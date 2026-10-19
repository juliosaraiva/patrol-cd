package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/reference"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applications"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	reposerver "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Support of the core-mode assembly (core.go): the settings snapshot, the in-process
// identity, lazily started informers, audit Events and request diagnostics.

// coreSettings is the settings snapshot read at startup; core-mode commands are short
// lived, so it is not followed.
type coreSettings struct{ st *settings.Settings }

// Current implements the services' SettingsSource.
func (s coreSettings) Current() *settings.Settings { return s.st }

// Subscribe implements apiserver.SettingsSource; the snapshot never changes.
func (s coreSettings) Subscribe(func(old, new *settings.Settings)) func() { return func() {} }

// loadCoreSettings reads the settings objects once with the caller's credentials. Missing
// and forbidden objects are treated as absent (defaults apply); an unreadable patrol-cm is
// reported through warn, since diffs and manifests may then differ from the controller's.
func loadCoreSettings(ctx context.Context, kube kubernetes.Interface, ns string, warn func(string), log *slog.Logger) (*settings.Settings, error) {
	in := settings.Input{Namespace: ns}
	for _, name := range []string{
		settings.DefaultConfigMapName, settings.DefaultRBACConfigMapName, settings.DefaultCmdParamsConfigMapName,
		settings.DefaultTLSCertsConfigMapName, settings.DefaultSSHKnownHostsConfigMapName, settings.DefaultGPGKeysConfigMapName,
		settings.DefaultNotificationsConfigMapName,
	} {
		cm, err := kube.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			in.ConfigMaps = append(in.ConfigMaps, cm)
		case apierrors.IsNotFound(err):
		case apierrors.IsForbidden(err):
			if name == settings.DefaultConfigMapName {
				warn(fmt.Sprintf("core mode: ConfigMap %s/%s is not readable with your Kubernetes credentials; default settings apply, so diffs and generated manifests may differ from the controller's", ns, name))
			} else {
				log.Debug("settings ConfigMap not readable; treated as absent", "namespace", ns, "name", name)
			}
		default:
			return nil, fmt.Errorf("read ConfigMap %s/%s: %w", ns, name, err)
		}
	}
	seen := map[string]bool{}
	for _, name := range []string{settings.DefaultSecretName, settings.DefaultNotificationsSecretName} {
		sec, err := kube.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			seen[name] = true
			in.Secrets = append(in.Secrets, sec)
		case apierrors.IsNotFound(err):
		case apierrors.IsForbidden(err):
			log.Debug("settings Secret not readable; treated as absent", "namespace", ns, "name", name)
		default:
			return nil, fmt.Errorf("read Secret %s/%s: %w", ns, name, err)
		}
	}
	sel := labels.SelectorFromSet(labels.Set{settings.LabelKeyPartOf: settings.LabelValuePartOf}).String()
	refs, err := kube.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
	switch {
	case err == nil:
		for i := range refs.Items {
			if !seen[refs.Items[i].Name] {
				in.Secrets = append(in.Secrets, &refs.Items[i])
			}
		}
	case apierrors.IsForbidden(err):
		log.Debug("referenced settings Secrets not readable; $<secret>:<key> references stay unresolved", "namespace", ns)
	default:
		return nil, fmt.Errorf("list Secrets of %s: %w", ns, err)
	}
	return settings.Parse(in), nil
}

// randomKeySet returns a throw-away key set for a token service that never signs tokens
// handed out (the in-process identity is verified as an external token).
func randomKeySet() (auth.KeySet, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return auth.KeySet{}, err
	}
	return auth.NewKeySet(b)
}

// newCoreIdentityToken returns a per-process JWT naming user; it is only ever compared
// with itself (coreVerifier) and never leaves the process.
func newCoreIdentityToken(user string) (string, error) {
	key := make([]byte, 32)
	jti := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": coreIdentityIssuer, "sub": user, "jti": hex.EncodeToString(jti),
	}).SignedString(key)
}

// coreVerifier accepts exactly the per-process identity token.
type coreVerifier struct {
	token     string
	principal auth.Principal
}

// VerifyExternal implements auth.ExternalVerifier.
func (v *coreVerifier) VerifyExternal(_ context.Context, raw string) (*auth.Principal, error) {
	if v.token == "" || subtle.ConstantTimeCompare([]byte(raw), []byte(v.token)) != 1 {
		return nil, fmt.Errorf("%w: not the in-process identity of core mode", auth.ErrInvalidToken)
	}
	p := v.principal
	p.Groups = slices.Clone(p.Groups)
	return &p, nil
}

// coreProjectTokens issues project tokens only with the real session signing key
// (patrol-secret readable with the caller's credentials): a token signed with another key
// would be useless against patrol-server.
type coreProjectTokens struct {
	tokens    *auth.TokenService
	keyErr    error
	namespace string
}

// IssueProjectToken implements projects.TokenIssuer.
func (c coreProjectTokens) IssueProjectToken(ctx context.Context, proj, role, id string, expiresIn time.Duration) (auth.Issued, error) {
	if c.keyErr != nil {
		return auth.Issued{}, rest.FailedPrecondition("project tokens cannot be issued in core mode without the session signing key: Secret %s/%s (key server.secretkey) could not be loaded with your Kubernetes credentials", c.namespace, settings.DefaultSecretName).WithCause(c.keyErr)
	}
	return c.tokens.IssueProjectToken(ctx, proj, role, id, expiresIn)
}

// coreClusterLookup resolves Application destinations from the Cluster objects (shard
// routing of tree API calls).
type coreClusterLookup struct{ a *coreAssembly }

// DestinationCluster implements sharding.DestinationClusterLookup.
func (l coreClusterLookup) DestinationCluster(dest v1alpha1.ApplicationDestination) (*v1alpha1.Cluster, error) {
	ctx, cancel := context.WithTimeout(l.a.ctx, coreStartupTimeout)
	defer cancel()
	inCluster := true
	if st := l.a.settings.Current(); st != nil {
		inCluster = st.InClusterEnabled()
	}
	return project.ResolveDestination(ctx, dest, project.NewKubeLookup(l.a.direct, l.a.cfg.Namespace, inCluster))
}

// accessCheck asks the Kubernetes API whether the caller may use each verb on each
// resource of the control-plane namespace (SelfSubjectAccessReview) and returns the
// denied "verb resource" pairs.
func (a *coreAssembly) accessCheck(ctx context.Context, verbs []string, resources ...schemaResource) ([]string, error) {
	var denied []string
	for _, r := range resources {
		for _, v := range verbs {
			res, err := a.kube.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
				Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: r.namespace, Verb: v, Group: r.group, Resource: r.resource,
				}},
			}, metav1.CreateOptions{})
			if err != nil {
				return nil, fmt.Errorf("check access to %s: %w", r.resource, err)
			}
			if !res.Status.Allowed {
				denied = append(denied, v+" "+r.resource)
			}
		}
	}
	return denied, nil
}

type schemaResource struct{ group, resource, namespace string }

// coreResolver starts the repository lister (informers over Repositories,
// RepositoryCredentials, SCMProviders and labelled Secrets) on first use, after checking
// that the caller may list and watch them.
type coreResolver struct {
	a    *coreAssembly
	repo reposerver.Client

	mu   sync.Mutex
	res  *repository.Resolver
	stop context.CancelFunc
}

func (r *coreResolver) get(ctx context.Context) (*repository.Resolver, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.res != nil {
		return r.res, nil
	}
	a := r.a
	ns := a.cfg.Namespace
	g := v1alpha1.GroupVersion.Group
	denied, err := a.accessCheck(ctx, []string{"list", "watch"},
		schemaResource{g, "repositories", ns}, schemaResource{g, "repositorycredentials", ns},
		schemaResource{g, "scmproviders", ns}, schemaResource{"", "secrets", ns})
	if err != nil {
		return nil, err
	}
	if len(denied) > 0 {
		return nil, rest.PermissionDenied("core mode cannot read repository credentials with your Kubernetes credentials (denied in namespace %s: %s)", ns, strings.Join(denied, ", "))
	}
	lister, err := repository.NewLister(a.dyn, a.kube, repository.ListerOptions{Namespace: ns, Logger: a.log})
	if err != nil {
		return nil, err
	}
	// The informers run until the assembly closes (its context ends) or this attempt fails.
	lctx, stop := context.WithCancel(a.ctx)
	r.stop = stop
	errc := make(chan error, 1)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		errc <- lister.Start(lctx)
	}()
	timer := time.NewTimer(a.cfg.SyncTimeout)
	defer timer.Stop()
	select {
	case err = <-errc:
	case <-timer.C:
		stop()
		<-errc //nolint:errcheck // the informers stopped; their error is superseded by the timeout
		err = fmt.Errorf("repositories of namespace %s did not load within %s", ns, a.cfg.SyncTimeout)
	case <-ctx.Done():
		stop()
		<-errc //nolint:errcheck // the informers stopped; the caller's cancellation wins
		err = ctx.Err()
	}
	if err != nil {
		stop()
		return nil, err
	}
	res, err := repository.NewResolver(lister, repository.Options{Logger: a.log, RepoServer: r.repo})
	if err != nil {
		stop()
		return nil, err
	}
	r.res = res
	return res, nil
}

// GetRepository implements applications.RepositoryResolver.
func (r *coreResolver) GetRepository(ctx context.Context, repoURL, proj string) (*reposerver.Repository, error) {
	res, err := r.get(ctx)
	if err != nil {
		return nil, err
	}
	return res.GetRepository(ctx, repoURL, proj)
}

// ListDependencyRepositories implements applications.RepositoryResolver and
// repositories.Resolver.
func (r *coreResolver) ListDependencyRepositories(ctx context.Context, proj *v1alpha1.AppProject, includeOCI bool) ([]reposerver.Repository, []reposerver.RepositoryCredentialTemplate, error) {
	res, err := r.get(ctx)
	if err != nil {
		return nil, nil, err
	}
	return res.ListDependencyRepositories(ctx, proj, includeOCI)
}

// Resolve implements repositories.Resolver.
func (r *coreResolver) Resolve(ctx context.Context, q repository.Query) (*repository.Resolution, error) {
	res, err := r.get(ctx)
	if err != nil {
		return nil, err
	}
	return res.Resolve(ctx, q)
}

// Validate implements repositories.Resolver.
func (r *coreResolver) Validate(ctx context.Context, repo *v1alpha1.Repository) (v1alpha1.ConnectionState, error) {
	res, err := r.get(ctx)
	if err != nil {
		return v1alpha1.ConnectionState{}, err
	}
	return res.Validate(ctx, repo)
}

// ValidateWithSecretData implements repositories.Resolver.
func (r *coreResolver) ValidateWithSecretData(ctx context.Context, repo *v1alpha1.Repository, data map[string][]byte) (v1alpha1.ConnectionState, error) {
	res, err := r.get(ctx)
	if err != nil {
		return v1alpha1.ConnectionState{}, err
	}
	return res.ValidateWithSecretData(ctx, repo, data)
}

// generatorResolver adapts the resolver for ApplicationSet generators.
func (r *coreResolver) generatorResolver() generators.RepositoryResolver {
	return generators.RepositoryResolverFunc(func(ctx context.Context, repoURL, proj string) (reposerver.Repository, error) {
		repo, err := r.GetRepository(ctx, repoURL, proj)
		if err != nil {
			return reposerver.Repository{}, err
		}
		if repo == nil {
			return reposerver.Repository{URL: repoURL, Project: proj}, nil
		}
		return *repo, nil
	})
}

// coreAppEvents feeds the Application watch stream from an informer started on the first
// subscription (only when the caller may list and watch Applications).
type coreAppEvents struct {
	a          *coreAssembly
	b          *applications.Broadcaster
	namespaces []string
	once       sync.Once
}

// Subscribe implements applications.Events.
func (e *coreAppEvents) Subscribe() (<-chan applications.Event, func()) {
	e.once.Do(e.start)
	return e.b.Subscribe()
}

func (e *coreAppEvents) start() {
	a := e.a
	nss := cacheNamespaces(a.cfg.Namespace, e.namespaces)
	var res []schemaResource
	for ns := range nss {
		res = append(res, schemaResource{v1alpha1.GroupVersion.Group, "applications", ns})
	}
	ctx, cancel := context.WithTimeout(a.ctx, coreStartupTimeout)
	defer cancel()
	denied, err := a.accessCheck(ctx, []string{"list", "watch"}, res...)
	if err != nil || len(denied) > 0 {
		a.log.Debug("not watching Applications (the stream sends the initial list only)", "denied", denied, "error", err)
		return
	}
	// One plain informer per namespace ("" is every namespace): they stop, with their
	// handlers, when the assembly closes.
	for ns := range nss {
		lw := &toolscache.ListWatch{
			ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
				list := &v1alpha1.ApplicationList{}
				return list, a.direct.List(ctx, list, client.InNamespace(ns), &client.ListOptions{Raw: &o})
			},
			WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
				return a.direct.Watch(ctx, &v1alpha1.ApplicationList{}, client.InNamespace(ns), &client.ListOptions{Raw: &o})
			},
		}
		inf := toolscache.NewSharedIndexInformer(lw, &v1alpha1.Application{}, 0, toolscache.Indexers{})
		if _, err := inf.AddEventHandler(e.b); err != nil {
			a.log.Debug("cannot watch Applications", "namespace", ns, "error", err)
			continue
		}
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			inf.RunWithContext(a.ctx)
		}()
	}
}

// coreEventRecorder records the audit Events of Applications, AppProjects and
// ApplicationSets synchronously with the caller's credentials (best effort: a caller that
// may not create Events gets none).
type coreEventRecorder struct {
	kube      kubernetes.Interface
	scheme    *runtime.Scheme
	namespace string
	log       *slog.Logger
}

// Eventf implements rest.EventRecorder.
func (r *coreEventRecorder) Eventf(obj runtime.Object, eventType, reason, messageFmt string, args ...any) {
	ref, ok := obj.(*corev1.ObjectReference)
	if !ok {
		var err error
		if ref, err = reference.GetReference(r.scheme, obj); err != nil {
			r.log.Debug("cannot reference the event object", "error", err)
			return
		}
	}
	ns := ref.Namespace
	if ns == "" {
		ns = r.namespace
	}
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: fmt.Sprintf("%s.%x", ref.Name, now.UnixNano()), Namespace: ns},
		InvolvedObject: *ref, Reason: reason, Message: fmt.Sprintf(messageFmt, args...), Type: eventType,
		Source:         corev1.EventSource{Component: coreEventComponent},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), coreEventTimeout)
	defer cancel()
	if _, err := r.kube.CoreV1().Events(ns).Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		r.log.Debug("cannot record the audit event", "reason", reason, "error", err)
	}
}

// --- Request handling --------------------------------------------------------------------

// coreRefusedRoutes need Patrol CD sessions, which core mode does not have.
var coreRefusedRoutes = []struct{ pattern, what string }{
	{"POST " + apiclient.PathSession, "logging in"},
	{"DELETE " + apiclient.PathSession, "logging out"},
	{"POST " + apiclient.PathSessionOIDCExchange, "SSO login"},
	{"GET " + apiclient.PathAuthLogin, "SSO login"},
	{"GET " + apiclient.PathAuthCallback, "SSO login"},
	{"/api/dex/", "the Dex proxy"},
	{"PUT " + apiclient.PathAccountPassword, "updating account passwords"},
	{"POST " + apiclient.PathAccountToken, "generating account tokens"},
	{"DELETE " + apiclient.PathAccountTokenID, "deleting account tokens"},
}

// CoreUnsupportedMessage explains why an operation needing Patrol CD sessions is refused
// in core mode.
func CoreUnsupportedMessage(what string) string {
	return what + " is not available in core mode: there are no Patrol CD sessions; the Kubernetes API authenticates and authorizes every request with your kubeconfig credentials"
}

// wrap adds, around the API handler: the refusal of session routes, the in-process
// identity, and request diagnostics (Kubernetes denials and internal connection failures
// appended to error documents, since in core mode they describe the caller's own access).
func (a *coreAssembly) wrap(api http.Handler) http.Handler {
	refused := http.NewServeMux()
	for _, r := range coreRefusedRoutes {
		what := r.what
		refused.HandleFunc(r.pattern, func(w http.ResponseWriter, req *http.Request) {
			rest.WriteError(w, req, rest.FailedPrecondition("%s", CoreUnsupportedMessage(what)))
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, pattern := refused.Handler(r); pattern != "" {
			h.ServeHTTP(w, r)
			return
		}
		r.Header.Del("Cookie")
		r.Header.Del("Authorization")
		if a.identityToken != "" {
			r.Header.Set("Authorization", "Bearer "+a.identityToken)
		}
		d := &coreDiagnostics{}
		r = r.WithContext(context.WithValue(r.Context(), coreDiagnosticsKey{}, d))
		ew := &diagnosticWriter{ResponseWriter: w, diag: d}
		api.ServeHTTP(ew, r)
		ew.finish()
	})
}

type coreDiagnosticsKey struct{}

// coreDiagnostics collects what went wrong below a request (bounded, deduplicated).
type coreDiagnostics struct {
	mu   sync.Mutex
	msgs []string
}

func (d *coreDiagnostics) add(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	if len(msg) > maxCoreDiagnosticBytes {
		msg = msg[:maxCoreDiagnosticBytes] + "…"
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.msgs) >= maxCoreDiagnostics || slices.Contains(d.msgs, msg) {
		return
	}
	d.msgs = append(d.msgs, msg)
}

func (d *coreDiagnostics) list() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.msgs)
}

// noteDiagnostic records msg for the request of ctx (no-op outside core mode requests).
func noteDiagnostic(ctx context.Context, msg string) {
	if d, ok := ctx.Value(coreDiagnosticsKey{}).(*coreDiagnostics); ok {
		d.add(msg)
	}
}

// coreStatusRecorder records the Status message of 401 and 403 answers of the Kubernetes
// API for the request that caused them.
type coreStatusRecorder struct{ next http.RoundTripper }

// RoundTrip implements http.RoundTripper.
func (c *coreStatusRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(req)
	if err != nil || resp == nil || (resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden) {
		return resp, err
	}
	if _, ok := req.Context().Value(coreDiagnosticsKey{}).(*coreDiagnostics); !ok {
		return resp, nil
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxCoreStatusBytes))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	msg := http.StatusText(resp.StatusCode)
	var st metav1.Status
	if rerr == nil && json.Unmarshal(body, &st) == nil && st.Message != "" {
		msg = st.Message
	}
	if resp.StatusCode == http.StatusUnauthorized {
		msg = "the Kubernetes API rejected your kubeconfig credentials: " + msg
	}
	noteDiagnostic(req.Context(), msg)
	return resp, nil
}

// WrappedRoundTripper lets client-go and utilnet reach the wrapped transport.
func (c *coreStatusRecorder) WrappedRoundTripper() http.RoundTripper { return c.next }

// diagnosticWriter holds back error documents (application/problem+json) so the request's
// diagnostics can be appended to their detail; every other response streams through.
type diagnosticWriter struct {
	http.ResponseWriter
	diag   *coreDiagnostics
	held   bool
	status int
	buf    bytes.Buffer
}

// WriteHeader implements http.ResponseWriter.
func (w *diagnosticWriter) WriteHeader(code int) {
	if code >= 400 && strings.HasPrefix(w.Header().Get("Content-Type"), apiclient.ProblemContentType) && len(w.diag.list()) > 0 {
		w.held, w.status = true, code
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write implements http.ResponseWriter.
func (w *diagnosticWriter) Write(b []byte) (int, error) {
	if !w.held {
		return w.ResponseWriter.Write(b)
	}
	if w.buf.Len()+len(b) > maxCoreProblemBytes {
		// Too large to rewrite: release what was held and stream the rest unchanged.
		w.held = false
		w.ResponseWriter.WriteHeader(w.status)
		if _, err := w.ResponseWriter.Write(w.buf.Bytes()); err != nil {
			return 0, err
		}
		w.buf.Reset()
		return w.ResponseWriter.Write(b)
	}
	w.buf.Write(b)
	return len(b), nil
}

// Flush implements http.Flusher.
func (w *diagnosticWriter) Flush() {
	if w.held {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer (http.ResponseController, WebSocket upgrades).
func (w *diagnosticWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *diagnosticWriter) finish() {
	if !w.held {
		return
	}
	body := w.buf.Bytes()
	var doc map[string]any
	if json.Unmarshal(body, &doc) == nil {
		detail, _ := doc["detail"].(string)
		extra := strings.Join(w.diag.list(), "; ")
		if detail == "" {
			doc["detail"] = extra
		} else {
			doc["detail"] = detail + ": " + extra
		}
		if b, err := json.Marshal(doc); err == nil {
			body = b
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(body)
}
