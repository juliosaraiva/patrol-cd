package applications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	apirest "github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	pubapi "github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Event reasons of terminal session audit entries (Kubernetes Events on the Application).
const (
	ReasonTerminalSessionStarted = "TerminalSessionStarted"
	ReasonTerminalSessionEnded   = "TerminalSessionEnded"
)

// terminalWriteTimeout bounds one write to the terminal client.
const terminalWriteTimeout = 10 * time.Second

// errInvalidTerminalMessage ends a session whose client sent a frame that is not a JSON
// TerminalMessage text frame.
var errInvalidTerminalMessage = errors.New("invalid terminal message")

// terminalRequest is a validated /terminal query.
type terminalRequest struct {
	pod, container, app, appNamespace, project, namespace, shell string
}

// parseTerminalRequest validates the query with upstream's rules and messages.
func parseTerminalRequest(r *http.Request) (*terminalRequest, error) {
	q := r.URL.Query()
	t := &terminalRequest{
		pod: q.Get("pod"), container: q.Get("container"), app: q.Get("appName"), project: q.Get("projectName"),
		namespace: q.Get("namespace"), appNamespace: q.Get("appNamespace"), shell: q.Get("shell"),
	}
	if t.pod == "" || t.container == "" || t.app == "" || t.project == "" || t.namespace == "" {
		return nil, apirest.InvalidArgument("Missing required parameters")
	}
	checks := []struct {
		ok  bool
		msg string
	}{
		{len(validation.IsDNS1123Subdomain(t.pod)) == 0, "Pod name is not valid"},
		{len(validation.IsDNS1123Label(t.container)) == 0, "Container name is not valid"},
		{len(validation.IsDNS1123Subdomain(t.app)) == 0, "App name is not valid"},
		{len(validation.IsDNS1123Subdomain(t.project)) == 0, "Project name is not valid"},
		{len(validation.IsDNS1123Label(t.namespace)) == 0, "Namespace name is not valid"},
		{t.appNamespace == "" || len(validation.IsDNS1123Label(t.appNamespace)) == 0, "App namespace name is not valid"},
	}
	for _, c := range checks {
		if !c.ok {
			return nil, apirest.InvalidArgument("%s", c.msg)
		}
	}
	if len(t.shell) > 256 {
		return nil, apirest.InvalidArgument("Shell is not valid")
	}
	return t, nil
}

// terminalOriginAllowed implements the same-origin rule of the WebSocket handshake: a
// browser always sends Origin, which must name the request host or the host of the
// configured url / additionalUrls. Requests without Origin (non-browser clients, which are
// not exposed to cross-site WebSocket hijacking) are allowed; "null" origins are not.
func terminalOriginAllowed(r *http.Request, st *settings.Settings) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	allowed := []string{r.Host}
	if st != nil {
		for _, raw := range append([]string{st.URL()}, st.AdditionalURLs()...) {
			if p, err := url.Parse(raw); err == nil && p.Host != "" {
				allowed = append(allowed, p.Host)
			}
		}
	}
	for _, h := range allowed {
		if h != "" && strings.EqualFold(u.Host, h) {
			return true
		}
	}
	return false
}

// podInTree reports whether a Pod node with a live object of the managed tree has the name
// and namespace (upstream podExists).
func podInTree(nodes []compare.ResourceNode, pod, namespace string) bool {
	for i := range nodes {
		n := &nodes[i]
		if n.Kind == "Pod" && n.Group == "" && n.UID != "" && n.Name == pod && n.Namespace == namespace {
			return true
		}
	}
	return false
}

// containerRunning reports whether the (init) container is running.
func containerRunning(pod *corev1.Pod, container string) bool {
	for _, list := range [][]corev1.ContainerStatus{pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses, pod.Status.EphemeralContainerStatuses} {
		for i := range list {
			if list[i].Name == container {
				return list[i].State.Running != nil
			}
		}
	}
	return false
}

// shellsToTry returns the requested shell when it is allowed, otherwise the configured
// shells in order (upstream).
func shellsToTry(requested string, allowed []string) []string {
	if requested != "" && slices.Contains(allowed, requested) {
		return []string{requested}
	}
	return slices.Clone(allowed)
}

// serveTerminal serves the web terminal WebSocket (GET /terminal). Checks, in order:
// exec.enabled (404), the handshake Origin (403), the query (400), the Application
// namespace (403), applications, get with the existence-hiding rules and exec, create on
// the Application (403), the pod being a Pod of the Application's tree (400), the
// container running (400). The exec stream uses the destination cluster's own credentials
// (never impersonated, upstream SYNC-123). Sessions end on idle timeout, client or
// command exit, lost permissions (re-checked on every message and periodically, with the
// caller's session), and are audited at start and end (no payloads).
func (s *Service) serveTerminal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.settings()
	if err != nil {
		apirest.WriteError(w, r, err)
		return
	}
	ex := st.Exec()
	if !ex.Enabled {
		apirest.WriteError(w, r, apirest.NotFound("the web terminal is disabled"))
		return
	}
	if !terminalOriginAllowed(r, st) {
		apirest.WriteError(w, r, apirest.PermissionDenied("origin not allowed"))
		return
	}
	treq, err := parseTerminalRequest(r)
	if err != nil {
		apirest.WriteError(w, r, err)
		return
	}
	if ns := s.nsOrDefault(treq.appNamespace); !s.namespaceEnabled(ns) {
		apirest.WriteError(w, r, apirest.PermissionDenied("%s", namespaceNotPermitted(ns).Error()))
		return
	}
	app, proj, err := s.getApp(ctx, rbac.ActionGet, appRequest{name: treq.app, namespace: treq.appNamespace, project: treq.project})
	if err != nil {
		apirest.WriteError(w, r, err)
		return
	}
	object := s.rbacObject(app)
	if err := apirest.Authorize(ctx, rbac.ResourceExec, rbac.ActionCreate, object); err != nil {
		apirest.WriteError(w, r, err)
		return
	}
	tree, err := s.appTree(ctx, app, proj)
	if err != nil {
		apirest.WriteError(w, r, err)
		return
	}
	if !podInTree(tree.Nodes, treq.pod, treq.namespace) {
		apirest.WriteError(w, r, apirest.InvalidArgument("Pod doesn't belong to specified app"))
		return
	}
	cc, err := s.appClusterConfig(ctx, app, proj, st, false)
	if err != nil {
		apirest.WriteError(w, r, err)
		return
	}
	cfg := cc.config
	cfg.Timeout = 0
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		apirest.WriteError(w, r, apirest.Internal(fmt.Errorf("cannot initialize kube client: %w", err)))
		return
	}
	cctx, cancel := s.clusterCtx(ctx)
	pod, err := kc.CoreV1().Pods(treq.namespace).Get(cctx, treq.pod, metav1.GetOptions{})
	cancel()
	if err != nil {
		s.log().WarnContext(ctx, "applications: terminal: cannot read the pod", "application", app.QualifiedName(), "pod", treq.namespace+"/"+treq.pod, "error", err)
		apirest.WriteError(w, r, apirest.InvalidArgument("Cannot find pod"))
		return
	}
	if !containerRunning(pod, treq.container) {
		apirest.WriteError(w, r, apirest.InvalidArgument("container %q is not running", treq.container))
		return
	}
	shells := shellsToTry(treq.shell, ex.Shells)
	if len(shells) == 0 {
		apirest.WriteError(w, r, apirest.FailedPrecondition("no shells are configured (exec.shells)"))
		return
	}
	tc := s.d.Resources.Terminal
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // the Origin was checked above against the configured URLs
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		s.log().DebugContext(ctx, "applications: terminal: websocket upgrade failed", "error", err)
		return
	}
	conn.SetReadLimit(tc.MaxMessageBytes)
	sess := &terminalSession{
		s: s, conn: conn, app: app, object: object, cfg: cfg, tc: tc, shells: shells,
		exec: execRequest{namespace: treq.namespace, pod: treq.pod, container: treq.container},
	}
	sess.run(ctx)
}

// terminalSession bridges a client WebSocket and exec streams.
type terminalSession struct {
	s      *Service
	conn   *websocket.Conn
	app    *v1alpha1.Application
	object string
	cfg    *rest.Config
	tc     TerminalConfig
	shells []string
	exec   execRequest

	resized    bool
	rows, cols uint16
	sentInput  bool
	started    time.Time
	shell      string
}

// send writes a terminal message to the client (bounded).
func (t *terminalSession) send(ctx context.Context, op, data string) error {
	b, err := json.Marshal(pubapi.TerminalMessage{Operation: op, Data: data})
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, terminalWriteTimeout)
	defer cancel()
	return t.conn.Write(wctx, websocket.MessageText, b)
}

// audit records a session start or end (who, which Application, pod and container; never
// terminal content).
func (t *terminalSession) audit(ctx context.Context, reason, msg string) {
	id := apirest.IdentityFrom(ctx)
	e := apirest.AuditEntry{
		Time: time.Now().UTC(), RequestID: apirest.RequestID(ctx), Actor: id.Actor(),
		Operation: "openTerminal", Method: http.MethodGet, Route: http.MethodGet + " " + pubapi.PathTerminal,
		Resource: rbac.ResourceExec, Action: rbac.ActionCreate, Object: t.object,
		Result: apirest.AuditSucceeded, Status: http.StatusSwitchingProtocols,
		Target: &apirest.ObjectRef{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Application",
			Namespace: t.app.Namespace, Name: t.app.Name, UID: t.app.UID},
		Message: fmt.Sprintf("%s %s", actorName(ctx), msg), EventReason: reason,
	}
	if id != nil && id.Principal != nil {
		e.ActorKind = string(id.Principal.Kind)
	}
	t.s.d.Resources.Auditor.Audit(ctx, e)
}

// permitted re-checks applications, get and exec, create; full also re-verifies the
// caller's session and that the Application still exists in the same project.
func (t *terminalSession) permitted(ctx context.Context, full bool) bool {
	if !apirest.Can(ctx, rbac.ResourceApplications, rbac.ActionGet, t.object) ||
		!apirest.Can(ctx, rbac.ResourceExec, rbac.ActionCreate, t.object) {
		return false
	}
	if !full {
		return true
	}
	if err := apirest.Reverify(ctx); err != nil {
		return false
	}
	cur := &v1alpha1.Application{}
	if err := t.s.d.Reader.Get(ctx, client.ObjectKeyFromObject(t.app), cur); err != nil {
		return false
	}
	return cur.UID == t.app.UID && t.s.rbacObject(cur) == t.object
}

// run serves the session until it ends.
func (t *terminalSession) run(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if t.tc.MaxSessionDuration > 0 {
		var c context.CancelFunc
		ctx, c = context.WithTimeout(ctx, t.tc.MaxSessionDuration)
		defer c()
	}
	msgs := make(chan pubapi.TerminalMessage, 16)
	readErr := make(chan error, 1)
	go func() {
		for {
			typ, b, err := t.conn.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			var m pubapi.TerminalMessage
			if typ != websocket.MessageText || json.Unmarshal(b, &m) != nil {
				readErr <- errInvalidTerminalMessage
				return
			}
			select {
			case msgs <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		tk := time.NewTicker(t.tc.PingInterval)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				pctx, pc := context.WithTimeout(ctx, t.tc.PingInterval)
				err := t.conn.Ping(pctx)
				pc()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	reason := t.serve(ctx, msgs, readErr)
	if !t.started.IsZero() {
		t.audit(parent, ReasonTerminalSessionEnded, fmt.Sprintf("ended a terminal session in pod %s/%s container %s (%s, %s)",
			t.exec.namespace, t.exec.pod, t.exec.container, reason, time.Since(t.started).Round(time.Second)))
	}
	code := websocket.StatusNormalClosure
	if reason == "permission denied" {
		code = websocket.StatusPolicyViolation
	}
	_ = t.conn.Close(code, reason)
}

// serve tries the shells in order and pumps the session; it returns why it ended.
func (t *terminalSession) serve(ctx context.Context, msgs <-chan pubapi.TerminalMessage, readErr <-chan error) string {
	var lastErr error
	for i, shell := range t.shells {
		req := t.exec
		req.command = []string{shell}
		es, err := dialExec(ctx, t.cfg, req, t.tc.HandshakeTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return "client disconnected"
			}
			t.s.log().InfoContext(ctx, "applications: terminal: exec failed", "pod", req.namespace+"/"+req.pod, "shell", shell, "error", err)
			lastErr = err
			continue
		}
		if t.started.IsZero() {
			t.started = time.Now()
			t.audit(ctx, ReasonTerminalSessionStarted, fmt.Sprintf("started a terminal session in pod %s/%s container %s", req.namespace, req.pod, req.container))
		}
		t.shell = shell
		reason, retry := t.pump(ctx, es, msgs, readErr)
		es.close()
		if retry && !t.sentInput && i < len(t.shells)-1 {
			continue
		}
		return reason
	}
	if lastErr != nil {
		var he *execHandshakeError
		if errors.As(lastErr, &he) && he.Status == http.StatusForbidden {
			_ = t.send(ctx, pubapi.TerminalOpStdout, "Failed to exec container: permission denied on the destination cluster\r\n")
		} else {
			_ = t.send(ctx, pubapi.TerminalOpStdout, "Failed to exec container\r\n")
		}
	}
	return "exec failed"
}

// pump bridges one exec stream; retry is true when the shell could not be started.
func (t *terminalSession) pump(ctx context.Context, es *execStream, msgs <-chan pubapi.TerminalMessage, readErr <-chan error) (reason string, retry bool) {
	if t.resized {
		_ = es.resize(ctx, t.cols, t.rows)
	}
	done := make(chan error, 1)
	go func() {
		done <- es.run(ctx, func(p []byte) error { return t.send(ctx, pubapi.TerminalOpStdout, string(p)) })
	}()
	idle := time.NewTimer(t.tc.IdleTimeout)
	defer idle.Stop()
	recheck := time.NewTicker(t.tc.RecheckInterval)
	defer recheck.Stop()
	stop := func(r string) (string, bool) {
		es.close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		return r, false
	}
	denied := func() (string, bool) {
		_ = t.send(ctx, pubapi.TerminalOpStdout, "Permission denied")
		return stop("permission denied")
	}
	for {
		select {
		case <-ctx.Done():
			return stop("session ended")
		case err := <-readErr:
			if errors.Is(err, errInvalidTerminalMessage) {
				return stop("invalid client message")
			}
			return stop("client disconnected")
		case err := <-done:
			var se *execStatusError
			switch {
			case err == nil:
				return "command exited", false
			case errors.As(err, &se) && se.startFailure():
				if !t.sentInput {
					_ = t.send(ctx, pubapi.TerminalOpStdout, se.Error()+"\r\n")
				}
				return "shell failed to start", true
			case errors.As(err, &se):
				return "command exited", false
			default:
				return "exec stream failed", false
			}
		case <-idle.C:
			_ = t.send(ctx, pubapi.TerminalOpStdout, fmt.Sprintf("\r\nsession closed after %s without input\r\n", t.tc.IdleTimeout))
			return stop("idle timeout")
		case <-recheck.C:
			if !t.permitted(ctx, true) {
				return denied()
			}
		case m := <-msgs:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(t.tc.IdleTimeout)
			if !t.permitted(ctx, false) {
				return denied()
			}
			switch m.Operation {
			case pubapi.TerminalOpStdin:
				t.sentInput = true
				if err := es.stdin(ctx, []byte(m.Data)); err != nil {
					return stop("exec stream failed")
				}
			case pubapi.TerminalOpResize:
				t.resized, t.rows, t.cols = true, m.Rows, m.Cols
				if err := es.resize(ctx, m.Cols, m.Rows); err != nil {
					return stop("exec stream failed")
				}
			default:
				return stop("unknown message type")
			}
		}
	}
}
