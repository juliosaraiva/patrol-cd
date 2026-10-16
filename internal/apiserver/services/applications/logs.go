package applications

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	apirest "github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	pubapi "github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

const (
	// maxLogFilterBytes caps the filter text.
	maxLogFilterBytes = 4096
	// maxMergeBuffered flushes the merge buffers when that many lines wait for a quiet pod.
	maxMergeBuffered = 10000
	// truncatedSuffix marks a log line cut at LogsConfig.MaxLineBytes.
	truncatedSuffix = " ...[truncated]"
)

// logsRequest is a parsed pod logs request.
type logsRequest struct {
	app          appRequest
	namespace    string
	podName      string
	container    string
	kind         string
	group        string
	resourceName string
	sinceSeconds *int64
	tailLines    *int64
	sinceTime    *metav1.Time
	untilTime    *time.Time
	follow       bool
	previous     bool
	matchCase    bool
	download     bool
	hasFilter    bool
	filter       string
	inverse      bool
}

func queryBool(q map[string][]string, name string) (bool, error) {
	vs, ok := q[name]
	if !ok || len(vs) == 0 {
		return false, nil
	}
	v := vs[len(vs)-1]
	if v == "" {
		return true, nil // a bare ?flag
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, apirest.InvalidArgument("invalid %s parameter value %q", name, v)
	}
	return b, nil
}

func queryInt(q map[string][]string, name string) (*int64, error) {
	vs, ok := q[name]
	if !ok || len(vs) == 0 || vs[len(vs)-1] == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(vs[len(vs)-1], 10, 64)
	if err != nil || n < 0 {
		return nil, apirest.InvalidArgument("invalid %s parameter value %q", name, vs[len(vs)-1])
	}
	return &n, nil
}

// parseLogsRequest reads the query of the logs routes (pod from the path of
// /pods/{pod}/logs, which implies kind=Pod and resourceName=pod, as podName does).
func parseLogsRequest(r *http.Request) (*logsRequest, error) {
	q := r.URL.Query()
	get := q.Get
	lr := &logsRequest{
		app:       appRequest{name: r.PathValue("name"), namespace: get("appNamespace"), project: get("project")},
		namespace: get("namespace"), container: get("container"),
		kind: get("kind"), group: get("group"), resourceName: get("resourceName"),
		podName: get("podName"),
	}
	if pod := r.PathValue("pod"); pod != "" {
		lr.podName = pod
	}
	if lr.podName != "" {
		lr.kind, lr.resourceName = "Pod", lr.podName
	}
	var err error
	if v := get("untilTime"); v != "" {
		t, perr := time.Parse(time.RFC3339Nano, v)
		if perr != nil {
			return nil, apirest.InvalidArgument("invalid untilTime parameter value: %v", perr)
		}
		lr.untilTime = &t
	}
	if v := get("sinceTime"); v != "" {
		t, perr := time.Parse(time.RFC3339Nano, v)
		if perr != nil {
			return nil, apirest.InvalidArgument("invalid sinceTime parameter value: %v", perr)
		}
		mt := metav1.NewTime(t)
		lr.sinceTime = &mt
	}
	if lr.sinceSeconds, err = queryInt(q, "sinceSeconds"); err != nil {
		return nil, err
	}
	if lr.sinceSeconds != nil && *lr.sinceSeconds == 0 {
		lr.sinceSeconds = nil
	}
	if lr.tailLines, err = queryInt(q, "tailLines"); err != nil {
		return nil, err
	}
	if lr.tailLines != nil && *lr.tailLines == 0 {
		lr.tailLines = nil
	}
	for name, dst := range map[string]*bool{"follow": &lr.follow, "previous": &lr.previous, "matchCase": &lr.matchCase, "download": &lr.download} {
		if *dst, err = queryBool(q, name); err != nil {
			return nil, err
		}
	}
	if f, ok := q["filter"]; ok && len(f) > 0 && f[len(f)-1] != "" {
		lit := f[len(f)-1]
		if len(lit) > maxLogFilterBytes {
			return nil, apirest.InvalidArgument("filter exceeds %d bytes", maxLogFilterBytes)
		}
		lr.hasFilter = true
		if lit[0] == '!' {
			lit, lr.inverse = lit[1:], true
		}
		lr.filter = lit
	}
	return lr, nil
}

// keep applies the line filter: the line must contain the text (case-insensitively unless
// matchCase), or must not with a leading "!".
func (lr *logsRequest) keep(line string) bool {
	if !lr.hasFilter {
		return true
	}
	var contains bool
	if lr.matchCase {
		contains = strings.Contains(line, lr.filter)
	} else {
		contains = strings.Contains(strings.ToLower(line), strings.ToLower(lr.filter))
	}
	return contains != lr.inverse
}

// podFilter selects pods of a tree.
type podFilter struct {
	kind, group, resourceName, namespace string
}

func (f podFilter) matches(n *compare.ResourceNode) bool {
	return (f.resourceName == "" || n.Name == f.resourceName) && (f.kind == "" || n.Kind == f.kind) &&
		(f.group == "" || n.Group == f.group) && (f.namespace == "" || n.Namespace == f.namespace)
}

// selectPods returns the Pod nodes (with a live object) of the managed tree that match f
// themselves or through an ancestor (parentRefs), as upstream getSelectedPods. Parent
// cycles are tolerated.
func selectPods(nodes []compare.ResourceNode, f podFilter) []compare.ResourceNode {
	byKey := make(map[compare.ResourceKey]*compare.ResourceNode, len(nodes))
	for i := range nodes {
		byKey[nodes[i].Key()] = &nodes[i]
	}
	memo := map[compare.ResourceKey]bool{}
	visiting := map[compare.ResourceKey]bool{}
	var selected func(n *compare.ResourceNode) bool
	selected = func(n *compare.ResourceNode) bool {
		k := n.Key()
		if v, ok := memo[k]; ok {
			return v
		}
		if visiting[k] {
			return false
		}
		visiting[k] = true
		defer delete(visiting, k)
		res := f.matches(n)
		for i := 0; !res && i < len(n.ParentRefs); i++ {
			if p, ok := byKey[n.ParentRefs[i].Key()]; ok {
				res = selected(p)
			}
		}
		memo[k] = res
		return res
	}
	var out []compare.ResourceNode
	for i := range nodes {
		n := &nodes[i]
		if n.Kind == "Pod" && n.Group == "" && n.UID != "" && selected(n) {
			out = append(out, *n)
		}
	}
	return out
}

// logsPlan is an authorized logs request ready to stream.
type logsPlan struct {
	req  *logsRequest
	app  *v1alpha1.Application
	kube kubernetes.Interface
	pods []compare.ResourceNode
}

// prepareLogs runs the checks of a logs request in upstream order: RBAC (applications,
// get with the existence-hiding rules, then logs, get), the tree, the destination cluster
// configuration (impersonated when configured), then the pods of the tree matching the
// query (at most server.maxPodLogsToRender; 0 allows none).
func (s *Service) prepareLogs(ctx context.Context, lr *logsRequest) (*logsPlan, error) {
	app, proj, err := s.getApp(ctx, rbac.ActionGet, lr.app)
	if err != nil {
		return nil, err
	}
	if err := apirest.Authorize(ctx, rbac.ResourceLogs, rbac.ActionGet, s.rbacObject(app)); err != nil {
		return nil, err
	}
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	tree, err := s.appTree(ctx, app, proj)
	if err != nil {
		return nil, err
	}
	cc, err := s.appClusterConfig(ctx, app, proj, st, true)
	if err != nil {
		return nil, err
	}
	cfg := cc.config
	cfg.Timeout = 0 // streams are bounded by the request context
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, apirest.Internal(fmt.Errorf("error creating kube client: %w", err))
	}
	pods := selectPods(tree.Nodes, podFilter{kind: lr.kind, group: lr.group, resourceName: lr.resourceName, namespace: lr.namespace})
	// Upstream order and semantics: no matching pod is an empty stream; otherwise more pods
	// than server.maxPodLogsToRender are refused, and 0 refuses every pod (it is not
	// "unlimited").
	if len(pods) > 0 && int64(len(pods)) > st.MaxPodLogsToRender() {
		return nil, apirest.InvalidArgument("max pods to view logs are reached. Please provide more granular query")
	}
	return &logsPlan{req: lr, app: app, kube: kc, pods: pods}, nil
}

// serveLogs serves GET /applications/{name}/logs and /applications/{name}/pods/{pod}/logs:
// Server-Sent Events of pubapi.LogEntry (lines of every selected pod merged by timestamp,
// then a final entry with last=true), or with download=true a text attachment.
func (s *Service) serveLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lr, err := parseLogsRequest(r)
	if err != nil {
		apirest.WriteError(w, r, err)
		return
	}
	plan, err := s.prepareLogs(ctx, lr)
	if err != nil {
		apirest.WriteError(w, r, err)
		return
	}
	var out logSink
	if lr.download {
		out = newDownloadSink(w, lr, s.d.Resources.Logs.WriteTimeout)
	} else {
		out = newSSESink(w, s.d.Resources.Logs.WriteTimeout)
	}
	if err := out.start(); err != nil {
		s.log().DebugContext(ctx, "applications: logs: cannot start the response", "error", err)
		return
	}
	if len(plan.pods) == 0 {
		return // upstream: no pods, nothing sent
	}
	s.streamLogs(ctx, plan, out)
}

// logLine is one line of one pod.
type logLine struct {
	pod  string
	ts   time.Time
	text string
}

// streamLogs opens the pod log streams, merges them and writes them to out, applying the
// filter, untilTime, the byte and rate limits, heartbeats and periodic re-verification
// of the caller's session and permissions.
func (s *Service) streamLogs(ctx context.Context, plan *logsPlan, out logSink) {
	cfg := s.d.Resources.Logs
	lr := plan.req
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	streams := make([]<-chan logLine, 0, len(plan.pods))
	for _, pod := range plan.pods {
		ch := make(chan logLine, 64)
		streams = append(streams, ch)
		opts := &corev1.PodLogOptions{
			Container: lr.container, Follow: lr.follow, Timestamps: true, SinceSeconds: lr.sinceSeconds,
			SinceTime: lr.sinceTime, TailLines: lr.tailLines, Previous: lr.previous,
		}
		go readPodLogs(ctx, plan.kube, pod.Namespace, pod.Name, opts, cfg.MaxLineBytes, ch)
	}
	merged := make(chan logLine, 64)
	go mergeLogs(ctx, streams, cfg.MergeWindow, merged)

	limiter := rate.NewLimiter(rate.Limit(cfg.LinesPerSecond), cfg.Burst)
	heartbeat := time.NewTicker(cfg.Heartbeat)
	defer heartbeat.Stop()
	recheck := time.NewTicker(cfg.RecheckInterval)
	defer recheck.Stop()
	var sent int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if err := out.heartbeat(); err != nil {
				return
			}
		case <-recheck.C:
			if err := s.recheckLogs(ctx, plan.app); err != nil {
				out.fail(ctx, err)
				return
			}
		case l, ok := <-merged:
			if !ok {
				_ = out.entry(finalEntry(time.Now()))
				return
			}
			if !lr.keep(l.text) {
				continue
			}
			ts := metav1.NewTime(l.ts)
			e := pubapi.LogEntry{Content: l.text, PodName: l.pod, TimeStamp: &ts, TimeStampStr: l.ts.Format(time.RFC3339Nano)}
			if lr.untilTime != nil && l.ts.After(*lr.untilTime) {
				e.Last = true
				_ = out.entry(e)
				return
			}
			if err := limiter.Wait(ctx); err != nil {
				return
			}
			sent += int64(len(l.text))
			if sent > cfg.MaxBytes {
				out.fail(ctx, apirest.ResourceExhausted("the log stream exceeded %d bytes; narrow the query (tailLines, sinceSeconds, container)", cfg.MaxBytes))
				return
			}
			if err := out.entry(e); err != nil {
				return
			}
		}
	}
}

func finalEntry(now time.Time) pubapi.LogEntry {
	ts := metav1.NewTime(now)
	return pubapi.LogEntry{Last: true, TimeStamp: &ts, TimeStampStr: now.Format(time.RFC3339Nano)}
}

// recheckLogs ends a long-running stream when the caller's session is no longer valid, the
// Application was deleted or replaced, or the caller lost applications, get or logs, get.
func (s *Service) recheckLogs(ctx context.Context, app *v1alpha1.Application) error {
	if err := apirest.Reverify(ctx); err != nil {
		return err
	}
	cur := &v1alpha1.Application{}
	if err := s.d.Reader.Get(ctx, client.ObjectKeyFromObject(app), cur); err != nil {
		if apierrors.IsNotFound(err) {
			return forbidden()
		}
		return nil // transient read failure: keep streaming
	}
	if cur.UID != app.UID || !s.canApp(ctx, rbac.ActionGet, cur) ||
		!apirest.Can(ctx, rbac.ResourceLogs, rbac.ActionGet, s.rbacObject(cur)) {
		return forbidden()
	}
	return nil
}

// readPodLogs streams the log of one pod into ch (closed at the end). A stream that cannot
// be opened produces one line with the error (upstream: the reason, e.g. a container that
// is not running, is shown to the user).
func readPodLogs(ctx context.Context, kc kubernetes.Interface, namespace, pod string, opts *corev1.PodLogOptions, maxLine int, ch chan<- logLine) {
	defer close(ch)
	rc, err := kc.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		select {
		case ch <- logLine{pod: pod, ts: time.Now(), text: podLogError(err)}:
		case <-ctx.Done():
		}
		return
	}
	defer func() { _ = rc.Close() }()
	readLogLines(ctx, rc, pod, maxLine, ch)
}

// podLogError is the text shown for a pod whose log cannot be read: the Kubernetes status
// message, or a generic text for transport errors.
func podLogError(err error) string {
	var st apierrors.APIStatus
	if errors.As(err, &st) && st.Status().Message != "" {
		return st.Status().Message
	}
	return "error reading pod logs: " + err.Error()
}

// readLogLines parses "<RFC3339Nano timestamp> <text>" lines (bounded length; longer lines
// are truncated) into ch. Carriage returns split a line into several entries (upstream). A
// line without a parsable timestamp keeps the previous one.
func readLogLines(ctx context.Context, r io.Reader, pod string, maxLine int, ch chan<- logLine) {
	br := bufio.NewReaderSize(r, 32<<10)
	var last time.Time
	for {
		line, truncated, err := readBoundedLine(br, maxLine)
		if raw := strings.TrimSpace(string(line)); raw != "" {
			ts, text := last, raw
			if tsStr, rest, ok := strings.Cut(raw, " "); ok {
				if t, perr := time.Parse(time.RFC3339Nano, tsStr); perr == nil {
					ts, text = t, rest
				}
			} else if t, perr := time.Parse(time.RFC3339Nano, raw); perr == nil {
				ts, text = t, ""
			}
			if ts.IsZero() {
				ts = time.Now()
			}
			last = ts
			if truncated {
				text += truncatedSuffix
			}
			for part := range strings.SplitSeq(text, "\r") {
				select {
				case ch <- logLine{pod: pod, ts: ts, text: part}:
				case <-ctx.Done():
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// readBoundedLine reads one line (without the newline) of at most max bytes; the rest of
// a longer line is discarded and truncated is set.
func readBoundedLine(br *bufio.Reader, max int) (line []byte, truncated bool, err error) {
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line) < max {
			room := max - len(line)
			if len(chunk) > room {
				line = append(line, chunk[:room]...)
				truncated = true
			} else {
				line = append(line, chunk...)
			}
		} else if len(chunk) > 1 || (len(chunk) == 1 && chunk[0] != '\n') {
			truncated = true
		}
		switch {
		case err == nil:
			return trimNewline(line), truncated, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return trimNewline(line), truncated, err
		}
	}
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// mergeLogs merges per-pod line streams by timestamp into out (closed at the end): a line
// is emitted once every open stream has a buffered line (so the oldest is known); every
// window, or when too many lines wait, all buffered lines are emitted in timestamp order
// (a quiet pod does not hold the others back).
func mergeLogs(ctx context.Context, streams []<-chan logLine, window time.Duration, out chan<- logLine) {
	defer close(out)
	type tagged struct {
		idx  int
		line logLine
		done bool
	}
	in := make(chan tagged, 64)
	for i, ch := range streams {
		go func() {
			for l := range ch {
				select {
				case in <- tagged{idx: i, line: l}:
				case <-ctx.Done():
					for range ch { //nolint:revive // drain so the reader can exit
					}
					return
				}
			}
			select {
			case in <- tagged{idx: i, done: true}:
			case <-ctx.Done():
			}
		}()
	}
	n := len(streams)
	buf := make([][]logLine, n)
	closed := make([]bool, n)
	open, buffered := n, 0
	emit := func(flush bool) bool {
		for {
			oldest, waiting := -1, false
			for i := range buf {
				switch {
				case len(buf[i]) > 0:
					if oldest < 0 || buf[i][0].ts.Before(buf[oldest][0].ts) {
						oldest = i
					}
				case !closed[i]:
					waiting = true
				}
			}
			if oldest < 0 || (waiting && !flush) {
				return true
			}
			l := buf[oldest][0]
			buf[oldest] = buf[oldest][1:]
			buffered--
			select {
			case out <- l:
			case <-ctx.Done():
				return false
			}
		}
	}
	ticker := time.NewTicker(window)
	defer ticker.Stop()
	for open > 0 {
		select {
		case <-ctx.Done():
			return
		case t := <-in:
			if t.done {
				closed[t.idx] = true
				open--
			} else {
				buf[t.idx] = append(buf[t.idx], t.line)
				buffered++
			}
			if !emit(buffered >= maxMergeBuffered) {
				return
			}
		case <-ticker.C:
			if !emit(true) {
				return
			}
		}
	}
	emit(true)
}

// logSink writes a logs response.
type logSink interface {
	start() error
	entry(e pubapi.LogEntry) error
	heartbeat() error
	fail(ctx context.Context, err error)
}

// sseSink writes Server-Sent Events with blocking, deadline-bounded writes (the reader's
// pace throttles the pod log streams).
type sseSink struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
}

func newSSESink(w http.ResponseWriter, timeout time.Duration) *sseSink {
	return &sseSink{w: w, rc: http.NewResponseController(w), timeout: timeout}
}

func (s *sseSink) start() error {
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	return s.rc.Flush()
}

func (s *sseSink) write(p string) error {
	_ = s.rc.SetWriteDeadline(time.Now().Add(s.timeout))
	if _, err := io.WriteString(s.w, p); err != nil {
		return err
	}
	if err := s.rc.Flush(); err != nil {
		return err
	}
	_ = s.rc.SetWriteDeadline(time.Time{})
	return nil
}

func (s *sseSink) event(name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var sb strings.Builder
	if name != "" {
		sb.WriteString("event: " + name + "\n")
	}
	sb.WriteString("data: ")
	sb.Write(b)
	sb.WriteString("\n\n")
	return s.write(sb.String())
}

func (s *sseSink) entry(e pubapi.LogEntry) error { return s.event("", e) }

func (s *sseSink) heartbeat() error { return s.write(": heartbeat\n\n") }

func (s *sseSink) fail(ctx context.Context, err error) {
	_ = s.event("error", problemOf(ctx, err))
}

func problemOf(ctx context.Context, err error) pubapi.Problem {
	e := apirest.ProblemFor(err)
	return pubapi.Problem{
		Type: pubapi.ProblemType(e.Code), Title: http.StatusText(e.Status), Status: e.Status,
		Detail: e.Detail, Code: e.Code, RequestID: apirest.RequestID(ctx),
	}
}

// downloadSink writes the lines as a text attachment (upstream download=true).
type downloadSink struct {
	w        http.ResponseWriter
	rc       *http.ResponseController
	timeout  time.Duration
	filename string
}

// logFileName is "<namespace>-<pod>-<container>" when all are valid names, else "log".
func logFileName(namespace, pod, container string) string {
	for _, v := range []string{namespace, pod, container} {
		if v == "" || len(validation.IsDNS1123Subdomain(v)) > 0 {
			return "log"
		}
	}
	return namespace + "-" + pod + "-" + container
}

func newDownloadSink(w http.ResponseWriter, lr *logsRequest, timeout time.Duration) *downloadSink {
	return &downloadSink{w: w, rc: http.NewResponseController(w), timeout: timeout,
		filename: logFileName(lr.namespace, lr.podName, lr.container)}
}

func (d *downloadSink) start() error {
	h := d.w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment;filename="%s.log"`, d.filename))
	h.Set("Cache-Control", "no-store")
	d.w.WriteHeader(http.StatusOK)
	return d.rc.Flush()
}

func (d *downloadSink) entry(e pubapi.LogEntry) error {
	if e.Last {
		return nil
	}
	_ = d.rc.SetWriteDeadline(time.Now().Add(d.timeout))
	_, err := io.WriteString(d.w, e.Content+"\n")
	_ = d.rc.SetWriteDeadline(time.Time{})
	return err
}

func (d *downloadSink) heartbeat() error { return d.rc.Flush() }

func (d *downloadSink) fail(ctx context.Context, err error) {
	_, _ = io.WriteString(d.w, problemOf(ctx, err).Detail+"\n")
}
