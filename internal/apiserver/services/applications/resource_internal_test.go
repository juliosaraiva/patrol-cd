package applications

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/openapi"
	apirest "github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
	"github.com/juliosaraiva/patrol-cd/internal/lua"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	pubapi "github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// resourceContractRoutes are the resource-level application routes of api-contract.md.
var resourceContractRoutes = []string{
	"GET /api/v1/applications/{name}/resource-tree",
	"GET /api/v1/stream/applications/{name}/resource-tree",
	"GET /api/v1/applications/{name}/managed-resources",
	"POST /api/v1/applications/{name}/server-side-diff",
	"GET /api/v1/applications/{name}/events",
	"GET /api/v1/applications/{name}/resource",
	"POST /api/v1/applications/{name}/resource",
	"DELETE /api/v1/applications/{name}/resource",
	"GET /api/v1/applications/{name}/resource/actions",
	"POST /api/v1/applications/{name}/resource/actions",
	"GET /api/v1/applications/{name}/logs",
	"GET /api/v1/applications/{name}/pods/{pod}/logs",
	"GET /terminal",
}

func TestResourceRouteTable(t *testing.T) {
	t.Parallel()
	tbl := apirest.NewTable()
	New(Deps{}).Register(tbl)
	require.NoError(t, tbl.Err())
	got := map[string]apirest.Route{}
	for _, r := range tbl.Routes() {
		got[r.Pattern()] = r
	}
	tests := []struct {
		route, resource, action, opID string
	}{
		{"GET /api/v1/applications/{name}/resource-tree", rbac.ResourceApplications, rbac.ActionGet, "getResourceTree"},
		{"GET /api/v1/stream/applications/{name}/resource-tree", rbac.ResourceApplications, rbac.ActionGet, "watchResourceTree"},
		{"GET /api/v1/applications/{name}/managed-resources", rbac.ResourceApplications, rbac.ActionGet, "getManagedResources"},
		{"POST /api/v1/applications/{name}/server-side-diff", rbac.ResourceApplications, rbac.ActionGet, "serverSideDiff"},
		{"GET /api/v1/applications/{name}/events", rbac.ResourceApplications, rbac.ActionGet, "listApplicationEvents"},
		{"GET /api/v1/applications/{name}/resource", rbac.ResourceApplications, rbac.ActionGet, "getResource"},
		{"POST /api/v1/applications/{name}/resource", rbac.ResourceApplications, "update/<group>/<kind>/<namespace>/<name>", "patchResource"},
		{"DELETE /api/v1/applications/{name}/resource", rbac.ResourceApplications, "delete/<group>/<kind>/<namespace>/<name>", "deleteResource"},
		{"GET /api/v1/applications/{name}/resource/actions", rbac.ResourceApplications, rbac.ActionGet, "listResourceActions"},
		{"POST /api/v1/applications/{name}/resource/actions", rbac.ResourceApplications, "action/<group>/<kind>/<action>", "runResourceAction"},
		{"GET /api/v1/applications/{name}/logs", rbac.ResourceLogs, rbac.ActionGet, "getApplicationLogs"},
		{"GET /api/v1/applications/{name}/pods/{pod}/logs", rbac.ResourceLogs, rbac.ActionGet, "getPodLogs"},
		{"GET /terminal", rbac.ResourceExec, rbac.ActionCreate, "openTerminal"},
	}
	require.Len(t, tests, len(resourceContractRoutes))
	for _, tc := range tests {
		r, ok := got[tc.route]
		require.True(t, ok, "missing route %s", tc.route)
		assert.Equal(t, apirest.AccessRBAC, r.Access.Mode, tc.route)
		assert.True(t, r.Access.InHandler, tc.route)
		assert.Equal(t, tc.resource, r.Access.Resource, tc.route)
		assert.Equal(t, tc.action, r.Access.Action, tc.route)
		assert.Equal(t, rbacObjectDoc, r.Access.Object, tc.route)
		assert.Equal(t, tc.opID, r.OperationID, tc.route)
	}
	assert.True(t, got["GET /api/v1/stream/applications/{name}/resource-tree"].Stream)
	assert.True(t, got["GET /api/v1/applications/{name}/logs"].Stream)
	assert.Equal(t, int64(serverSideDiffMaxBodyBytes), got["POST /api/v1/applications/{name}/server-side-diff"].MaxBodyBytes)
	assert.Equal(t, int64(actionMaxBodyBytes), got["POST /api/v1/applications/{name}/resource/actions"].MaxBodyBytes)
}

// Without collaborators every resource route answers 503 (or 400 for invalid input),
// never panics.
func TestResourceRoutesWithoutDependencies(t *testing.T) {
	t.Parallel()
	tbl := apirest.NewTable()
	New(Deps{Config: Config{Namespace: "patrol"}}).Register(tbl)
	h, err := tbl.Handler(&apirest.Env{DisableAuth: true})
	require.NoError(t, err)
	for _, p := range resourceContractRoutes {
		method, path, _ := strings.Cut(p, " ")
		path = strings.NewReplacer("{name}", "app", "{pod}", "pod").Replace(path)
		body := strings.NewReader("")
		if method == http.MethodPost {
			body = strings.NewReader(`{"action":"x","patch":"{}"}`)
		}
		req := httptest.NewRequest(method, path+"?kind=Pod&resourceName=p", body)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if strings.HasPrefix(path, "/api/v1/stream/") {
			assert.Contains(t, rec.Body.String(), `"status":503`, p)
			continue
		}
		assert.Contains(t, []int{http.StatusServiceUnavailable, http.StatusBadRequest}, rec.Code, "%s: %s", p, rec.Body.String())
	}
}

func node(kind, name, uid string, parents ...compare.ResourceRef) compare.ResourceNode {
	g := ""
	switch kind {
	case "Deployment", "ReplicaSet", "StatefulSet":
		g = "apps"
	}
	return compare.ResourceNode{
		ResourceRef: compare.ResourceRef{Group: g, Version: "v1", Kind: kind, Namespace: "ns", Name: name, UID: uid},
		ParentRefs:  parents,
	}
}

func ref(kind, name string) compare.ResourceRef {
	n := node(kind, name, "")
	return n.ResourceRef
}

func TestSelectPods(t *testing.T) {
	t.Parallel()
	nodes := []compare.ResourceNode{
		node("Deployment", "web", "d1"),
		node("ReplicaSet", "web-1", "r1", ref("Deployment", "web")),
		node("Pod", "web-1-a", "p1", ref("ReplicaSet", "web-1")),
		node("Pod", "web-1-b", "p2", ref("ReplicaSet", "web-1")),
		node("StatefulSet", "db", "s1"),
		node("Pod", "db-0", "p3", ref("StatefulSet", "db")),
		node("Pod", "ghost", ""), // no live object
		// a parent cycle must not hang
		node("ReplicaSet", "loop-a", "la", ref("ReplicaSet", "loop-b")),
		node("ReplicaSet", "loop-b", "lb", ref("ReplicaSet", "loop-a")),
		node("Pod", "loop-pod", "lp", ref("ReplicaSet", "loop-a")),
	}
	names := func(f podFilter) []string {
		var out []string
		for _, p := range selectPods(nodes, f) {
			out = append(out, p.Name)
		}
		return out
	}
	tests := []struct {
		name string
		f    podFilter
		want []string
	}{
		{"all", podFilter{}, []string{"web-1-a", "web-1-b", "db-0", "loop-pod"}},
		{"deployment aggregates its pods", podFilter{kind: "Deployment", group: "apps", resourceName: "web"}, []string{"web-1-a", "web-1-b"}},
		{"replica set", podFilter{kind: "ReplicaSet", resourceName: "web-1"}, []string{"web-1-a", "web-1-b"}},
		{"one pod", podFilter{kind: "Pod", resourceName: "db-0"}, []string{"db-0"}},
		{"wrong group", podFilter{kind: "Deployment", group: "extensions", resourceName: "web"}, nil},
		{"namespace", podFilter{namespace: "other"}, nil},
		{"pod without uid never selected", podFilter{kind: "Pod", resourceName: "ghost"}, nil},
		{"cycle", podFilter{kind: "ReplicaSet", resourceName: "loop-b"}, []string{"loop-pod"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, names(tc.f))
		})
	}
}

func TestParseLogsRequest(t *testing.T) {
	t.Parallel()
	parse := func(target string) (*logsRequest, error) {
		mux := http.NewServeMux()
		var lr *logsRequest
		var err error
		h := func(_ http.ResponseWriter, r *http.Request) { lr, err = parseLogsRequest(r) }
		mux.HandleFunc("GET /api/v1/applications/{name}/logs", h)
		mux.HandleFunc("GET /api/v1/applications/{name}/pods/{pod}/logs", h)
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
		return lr, err
	}
	lr, err := parse("/api/v1/applications/app/logs?follow&tailLines=10&sinceSeconds=5&container=c&filter=!healthz&matchCase=true&kind=Deployment&group=apps&resourceName=web&appNamespace=ns&project=p")
	require.NoError(t, err)
	assert.Equal(t, appRequest{name: "app", namespace: "ns", project: "p"}, lr.app)
	assert.True(t, lr.follow)
	assert.Equal(t, int64(10), *lr.tailLines)
	assert.Equal(t, int64(5), *lr.sinceSeconds)
	assert.True(t, lr.hasFilter && lr.inverse && lr.matchCase)
	assert.Equal(t, "healthz", lr.filter)
	assert.Equal(t, "Deployment", lr.kind)

	lr, err = parse("/api/v1/applications/app/pods/web-0/logs?kind=Deployment&resourceName=x&tailLines=0")
	require.NoError(t, err)
	assert.Equal(t, "Pod", lr.kind, "the pod path implies kind=Pod")
	assert.Equal(t, "web-0", lr.resourceName)
	assert.Nil(t, lr.tailLines, "tailLines=0 means all lines")

	lr, err = parse("/api/v1/applications/app/logs?podName=p1&untilTime=2026-01-02T03:04:05.123456789Z&sinceTime=2026-01-01T00:00:00Z")
	require.NoError(t, err)
	assert.Equal(t, "Pod", lr.kind)
	assert.Equal(t, 123456789, lr.untilTime.Nanosecond())
	assert.NotNil(t, lr.sinceTime)

	for _, bad := range []struct{ q, msg string }{
		{"untilTime=yesterday", "invalid untilTime parameter value"},
		{"sinceTime=x", "invalid sinceTime parameter value"},
		{"tailLines=-1", "invalid tailLines"},
		{"sinceSeconds=abc", "invalid sinceSeconds"},
		{"follow=maybe", "invalid follow"},
		{"filter=" + strings.Repeat("a", maxLogFilterBytes+1), "filter exceeds"},
	} {
		_, err := parse("/api/v1/applications/app/logs?" + bad.q)
		require.Error(t, err, bad.q)
		assert.Contains(t, err.Error(), bad.msg)
	}
}

func TestLogFilter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		filter    string
		matchCase bool
		line      string
		want      bool
	}{
		{"", false, "anything", true},
		{"error", false, "an ERROR occurred", true},
		{"error", true, "an ERROR occurred", false},
		{"!healthz", false, "GET /healthz 200", false},
		{"!healthz", false, "GET /api 200", true},
		{"!", false, "excluded by the empty inverse filter", false},
	}
	for _, tc := range tests {
		lr := &logsRequest{matchCase: tc.matchCase}
		if tc.filter != "" {
			lr.hasFilter, lr.filter = true, tc.filter
			if tc.filter[0] == '!' {
				lr.filter, lr.inverse = tc.filter[1:], true
			}
		}
		assert.Equal(t, tc.want, lr.keep(tc.line), "%q %q", tc.filter, tc.line)
	}
}

func TestReadLogLines(t *testing.T) {
	t.Parallel()
	in := "2026-01-01T00:00:01.000000001Z first line\n" +
		"2026-01-01T00:00:02Z a\rb\n" +
		"no timestamp here\n" +
		"2026-01-01T00:00:03Z " + strings.Repeat("x", 100) + "\n" +
		"2026-01-01T00:00:04Z last without newline"
	ch := make(chan logLine, 100)
	readLogLines(context.Background(), strings.NewReader(in), "p", 60, ch)
	close(ch)
	var got []logLine
	for l := range ch {
		got = append(got, l)
	}
	require.Len(t, got, 6)
	assert.Equal(t, "first line", got[0].text)
	assert.Equal(t, 1, got[0].ts.Nanosecond())
	assert.Equal(t, []string{"a", "b"}, []string{got[1].text, got[2].text})
	assert.Equal(t, "no timestamp here", got[3].text)
	assert.Equal(t, got[2].ts, got[3].ts, "a line without timestamp keeps the previous one")
	assert.True(t, strings.HasSuffix(got[4].text, truncatedSuffix), got[4].text)
	assert.LessOrEqual(t, len(got[4].text), 60+len(truncatedSuffix))
	assert.Equal(t, "last without newline", got[5].text)
	for _, l := range got {
		assert.Equal(t, "p", l.pod)
	}
}

func TestReadBoundedLine(t *testing.T) {
	t.Parallel()
	br := bufio.NewReaderSize(strings.NewReader(strings.Repeat("y", 100)+"\nshort\n"), 16)
	line, truncated, err := readBoundedLine(br, 10)
	require.NoError(t, err)
	assert.True(t, truncated)
	assert.Equal(t, strings.Repeat("y", 10), string(line))
	line, truncated, err = readBoundedLine(br, 10)
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Equal(t, "short", string(line))
}

func TestMergeLogs(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(pod string, sec int) logLine {
		return logLine{pod: pod, ts: base.Add(time.Duration(sec) * time.Second), text: pod}
	}
	a := make(chan logLine, 10)
	b := make(chan logLine, 10)
	for _, l := range []logLine{at("a", 1), at("a", 3), at("a", 5)} {
		a <- l
	}
	for _, l := range []logLine{at("b", 2), at("b", 4)} {
		b <- l
	}
	close(a)
	close(b)
	out := make(chan logLine, 10)
	mergeLogs(context.Background(), []<-chan logLine{a, b}, time.Hour, out)
	var secs []int
	for l := range out {
		secs = append(secs, int(l.ts.Sub(base)/time.Second))
	}
	assert.Equal(t, []int{1, 2, 3, 4, 5}, secs)

	// A quiet stream does not hold the other back beyond the window.
	quiet := make(chan logLine)
	busy := make(chan logLine, 1)
	busy <- at("busy", 1)
	out = make(chan logLine, 10)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { mergeLogs(ctx, []<-chan logLine{quiet, busy}, 20*time.Millisecond, out); close(done) }()
	select {
	case l := <-out:
		assert.Equal(t, "busy", l.pod)
	case <-time.After(5 * time.Second):
		t.Fatal("the buffered line was not flushed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("mergeLogs did not stop on cancellation")
	}
}

func TestSplitStatusPatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		patch, nonStatus, status string
	}{
		{`{"spec":{"x":1}}`, `{"spec":{"x":1}}`, ""},
		{`{"status":{"phase":"A"}}`, "", `{"status":{"phase":"A"}}`},
		{`{"spec":{"x":1},"status":{"phase":"A"}}`, `{"spec":{"x":1}}`, `{"status":{"phase":"A"}}`},
	}
	for _, tc := range tests {
		ns, st, err := splitStatusPatch([]byte(tc.patch))
		require.NoError(t, err)
		if tc.nonStatus == "" {
			assert.Nil(t, ns)
		} else {
			assert.JSONEq(t, tc.nonStatus, string(ns))
		}
		if tc.status == "" {
			assert.Nil(t, st)
		} else {
			assert.JSONEq(t, tc.status, string(st))
		}
	}
	_, _, err := splitStatusPatch([]byte("not json"))
	require.Error(t, err)
}

func TestCheckActionParameters(t *testing.T) {
	t.Parallel()
	discovered := []lua.Action{
		{Name: "scale", Params: []lua.ActionParam{{Name: "replicas"}}},
		{Name: "resume", Disabled: true},
		{Name: "restart"},
	}
	p := func(names ...string) []pubapi.ResourceActionParameter {
		var out []pubapi.ResourceActionParameter
		for _, n := range names {
			out = append(out, pubapi.ResourceActionParameter{Name: n, Value: "1"})
		}
		return out
	}
	assert.NoError(t, checkActionParameters("scale", discovered, p("replicas")))
	assert.NoError(t, checkActionParameters("restart", discovered, nil))
	assert.NoError(t, checkActionParameters("undiscovered", discovered, nil), "fallback lookup without parameters")
	assert.ErrorContains(t, checkActionParameters("scale", discovered, p("replicas", "evil")), `unknown parameter "evil"`)
	assert.ErrorContains(t, checkActionParameters("undiscovered", discovered, p("x")), `unknown parameter "x"`)
	assert.ErrorContains(t, checkActionParameters("resume", discovered, nil), "is disabled")

	for _, bad := range []pubapi.ResourceActionRunRequest{
		{Action: ""},
		{Action: strings.Repeat("a", 254)},
		{Action: "a", ResourceActionParameters: []pubapi.ResourceActionParameter{{Name: ""}}},
		{Action: "a", ResourceActionParameters: []pubapi.ResourceActionParameter{{Name: "x"}, {Name: "x"}}},
		{Action: "a", ResourceActionParameters: []pubapi.ResourceActionParameter{{Name: "x", Value: strings.Repeat("v", MaxActionParameterBytes+1)}}},
		{Action: "a", ResourceActionParameters: make([]pubapi.ResourceActionParameter, MaxActionParameters+1)},
	} {
		assert.Error(t, validateActionRequest(&bad))
	}
}

func TestResourcePatchType(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]types.PatchType{
		"": types.MergePatchType, "merge": types.MergePatchType, "application/merge-patch+json": types.MergePatchType,
		"json": types.JSONPatchType, "application/json-patch+json": types.JSONPatchType,
		"strategic": types.StrategicMergePatchType, "application/strategic-merge-patch+json": types.StrategicMergePatchType,
	} {
		got, err := resourcePatchType(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	_, err := resourcePatchType("apply")
	assert.ErrorContains(t, err, "unsupported patch type")
}

func TestDeleteOptions(t *testing.T) {
	t.Parallel()
	o := deleteOptions(false, false)
	assert.Equal(t, metav1.DeletePropagationForeground, *o.PropagationPolicy)
	assert.Nil(t, o.GracePeriodSeconds)
	o = deleteOptions(false, true)
	assert.Equal(t, metav1.DeletePropagationBackground, *o.PropagationPolicy)
	assert.Equal(t, int64(0), *o.GracePeriodSeconds)
	o = deleteOptions(true, true)
	assert.Equal(t, metav1.DeletePropagationOrphan, *o.PropagationPolicy, "orphan wins over force")
}

func TestParseTerminalRequest(t *testing.T) {
	t.Parallel()
	ok := "pod=web-0&container=app&appName=guestbook&projectName=default&namespace=default"
	r := httptest.NewRequest(http.MethodGet, "/terminal?"+ok+"&appNamespace=team&shell=bash", nil)
	tr, err := parseTerminalRequest(r)
	require.NoError(t, err)
	assert.Equal(t, terminalRequest{pod: "web-0", container: "app", app: "guestbook", project: "default", namespace: "default", appNamespace: "team", shell: "bash"}, *tr)
	for q, msg := range map[string]string{
		"pod=web-0":                                                  "Missing required parameters",
		strings.Replace(ok, "web-0", "Web_0", 1):                     "Pod name is not valid",
		strings.Replace(ok, "=app", "=app.x", 1):                     "Container name is not valid",
		strings.Replace(ok, "guestbook", "Guest", 1):                 "App name is not valid",
		strings.Replace(ok, "default&", "Default&", 1):               "Project name is not valid",
		strings.Replace(ok, "namespace=default", "namespace=a.b", 1): "Namespace name is not valid",
		ok + "&appNamespace=A_B":                                     "App namespace name is not valid",
	} {
		_, err := parseTerminalRequest(httptest.NewRequest(http.MethodGet, "/terminal?"+q, nil))
		require.Error(t, err, q)
		assert.Contains(t, err.Error(), msg, q)
	}
}

func TestTerminalOriginAllowed(t *testing.T) {
	t.Parallel()
	req := func(host, origin string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/terminal", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}
	assert.True(t, terminalOriginAllowed(req("patrol.local:8080", ""), nil), "non-browser clients send no Origin")
	assert.True(t, terminalOriginAllowed(req("patrol.local:8080", "https://patrol.local:8080"), nil))
	assert.True(t, terminalOriginAllowed(req("patrol.local:8080", "https://PATROL.local:8080"), nil))
	assert.False(t, terminalOriginAllowed(req("patrol.local:8080", "https://evil.example"), nil))
	assert.False(t, terminalOriginAllowed(req("patrol.local:8080", "null"), nil))
	assert.False(t, terminalOriginAllowed(req("patrol.local:8080", "file://patrol.local:8080"), nil))
}

func TestContainerRunningAndShells(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses:     []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}, {Name: "side", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}}},
		InitContainerStatuses: []corev1.ContainerStatus{{Name: "init", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
	}}
	assert.True(t, containerRunning(pod, "app"))
	assert.True(t, containerRunning(pod, "init"))
	assert.False(t, containerRunning(pod, "side"))
	assert.False(t, containerRunning(pod, "missing"))
	assert.Equal(t, []string{"sh"}, shellsToTry("sh", []string{"bash", "sh"}))
	assert.Equal(t, []string{"bash", "sh"}, shellsToTry("zsh", []string{"bash", "sh"}), "a shell outside exec.shells is ignored")
	assert.Equal(t, []string{"bash", "sh"}, shellsToTry("", []string{"bash", "sh"}))
	nodes := []compare.ResourceNode{node("Pod", "web-0", "u"), node("Pod", "nouid", ""), {ResourceRef: compare.ResourceRef{Group: "metrics.k8s.io", Kind: "Pod", Namespace: "ns", Name: "fake", UID: "x"}}}
	assert.True(t, podInTree(nodes, "web-0", "ns"))
	assert.False(t, podInTree(nodes, "web-0", "other"))
	assert.False(t, podInTree(nodes, "nouid", "ns"))
	assert.False(t, podInTree(nodes, "fake", "ns"), "only core Pods")
}

func TestMaskResourceDiff(t *testing.T) {
	t.Parallel()
	secret := func(v string) string {
		return `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s","annotations":{"token":"t"}},"data":{"password":"` + v + `"}}`
	}
	hide := map[string]bool{"token": true}
	d := pubapi.ResourceDiff{Kind: "Secret", TargetState: secret("cGFzcw=="), LiveState: secret("b3RoZXI="), NormalizedLiveState: secret("b3RoZXI="), PredictedLiveState: secret("cGFzcw==")}
	require.NoError(t, maskResourceDiff(&d, hide))
	for _, s := range []string{d.TargetState, d.LiveState, d.NormalizedLiveState, d.PredictedLiveState} {
		assert.NotContains(t, s, "cGFzcw==")
		assert.NotContains(t, s, "b3RoZXI=")
		assert.NotContains(t, s, `"t"`)
	}
	assert.NotEqual(t, d.TargetState, d.LiveState, "different values keep different masks")

	// A spoofed kind does not avoid masking: the parsed object decides.
	d = pubapi.ResourceDiff{Kind: "ConfigMap", TargetState: secret("cGFzcw=="), LiveState: "null"}
	require.NoError(t, maskResourceDiff(&d, nil))
	assert.NotContains(t, d.TargetState, "cGFzcw==")
	assert.Equal(t, "null", d.LiveState)

	cm := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"},"data":{"k":"v"}}`
	d = pubapi.ResourceDiff{Kind: "ConfigMap", TargetState: cm, LiveState: cm}
	require.NoError(t, maskResourceDiff(&d, nil))
	assert.Equal(t, cm, d.TargetState, "other kinds are untouched")

	d = pubapi.ResourceDiff{Kind: "Secret", TargetState: "{not json"}
	require.Error(t, maskResourceDiff(&d, nil), "unparsable states fail closed")
}

func TestAppStateErrorAndTreeErrors(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{}
	assert.NoError(t, appStateError(app))
	app.Status.Conditions = []metav1.Condition{
		{Type: v1alpha1.ApplicationConditionComparisonError, Status: metav1.ConditionTrue, Message: "boom"},
		{Type: "SyncError", Status: metav1.ConditionTrue, Message: "ignored"},
	}
	err := appStateError(app)
	require.Error(t, err)
	assert.Equal(t, "ComparisonError: boom", apirest.ProblemFor(err).Detail)

	s := New(Deps{})
	ctx := context.Background()
	tests := []struct {
		err    error
		status int
		detail string
	}{
		{&treeapi.Error{Status: 400, Code: treeapi.CodeInvalidArgument, Message: "name mismatch"}, 400, "name mismatch"},
		{&treeapi.Error{Status: 403, Code: treeapi.CodePermissionDenied, Message: "apps/Deployment x not found as part of application a"}, 403, "not found as part of application"},
		{&treeapi.Error{Status: 413, Code: treeapi.CodeTooLarge, Message: "too many"}, 413, "too many"},
		{&treeapi.Error{Status: 504, Code: treeapi.CodeDeadlineExceeded}, 504, "did not answer in time"},
		{&treeapi.Error{Status: 404, Code: treeapi.CodeNotFound}, 503, "not available yet"},
		{&treeapi.Error{Status: 500, Code: treeapi.CodeInternal, Message: "secret detail"}, 500, "internal error"},
		{&treeapi.Error{Status: 401, Code: treeapi.CodeUnauthenticated}, 503, "controller is unavailable"},
		{errors.Join(treeapi.ErrUnavailable, errors.New("dial tcp")), 503, "controller is unavailable"},
	}
	for _, tc := range tests {
		e := apirest.ProblemFor(s.treeError(ctx, "what", tc.err))
		assert.Equal(t, tc.status, e.Status, tc.err.Error())
		assert.Contains(t, e.Detail, tc.detail)
	}
	assert.True(t, isStateMissing(&treeapi.Error{Code: treeapi.CodeUnavailable}))
	assert.False(t, isStateMissing(errors.Join(treeapi.ErrUnavailable, errors.New("transport"))), "transport errors are not cache misses")
}

func TestDestinationError(t *testing.T) {
	t.Parallel()
	s := New(Deps{})
	ctx := context.Background()
	gr := schema.GroupResource{Group: "apps", Resource: "deployments"}
	tests := []struct {
		err    error
		status int
		detail string
	}{
		{apierrors.NewNotFound(gr, "web"), 404, `error getting resource: deployments.apps "web" not found`},
		{apierrors.NewForbidden(gr, "web", errors.New("no")), 403, "error getting resource: deployments.apps \"web\" is forbidden"},
		{apierrors.NewUnauthorized("bad token"), 503, "rejected the cluster credentials"},
		{apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "p", nil), 422, "error getting resource"},
		{apierrors.NewConflict(gr, "web", errors.New("c")), 409, "error getting resource"},
		{apierrors.NewInternalError(errors.New("webhook failed")), 502, "webhook failed"},
		{errors.New("dial tcp: refused"), 503, "not reachable"},
	}
	for _, tc := range tests {
		e := apirest.ProblemFor(s.destinationError(ctx, "error getting resource", tc.err))
		assert.Equal(t, tc.status, e.Status, tc.err.Error())
		assert.Contains(t, e.Detail, tc.detail)
	}
}

func TestExecURLAndLogFileName(t *testing.T) {
	t.Parallel()
	u, err := execURL(&rest.Config{Host: "https://k8s.example:6443/prefix"}, execRequest{namespace: "ns", pod: "p", container: "c", command: []string{"sh"}})
	require.NoError(t, err)
	assert.Equal(t, "https://k8s.example:6443/prefix/api/v1/namespaces/ns/pods/p/exec?command=sh&container=c&stdin=true&stdout=true&tty=true", u)
	u, err = execURL(&rest.Config{Host: "127.0.0.1:8080"}, execRequest{namespace: "ns", pod: "p", container: "c", command: []string{"bash"}})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(u, "http://127.0.0.1:8080/api/v1/"), u)
	_, err = execURL(&rest.Config{}, execRequest{})
	require.Error(t, err)

	assert.Equal(t, "ns-pod-c", logFileName("ns", "pod", "c"))
	assert.Equal(t, "log", logFileName("ns", "", "c"))
	assert.Equal(t, "log", logFileName("ns", "../etc", "c"))
	assert.Equal(t, "log", logFileName("NS", "pod", "c"))
}

func TestExecStatusError(t *testing.T) {
	t.Parallel()
	exit := &execStatusError{Status: metav1.Status{Status: metav1.StatusFailure, Reason: statusReasonNonZeroExitCode, Message: "command terminated with non-zero exit code"}}
	assert.False(t, exit.startFailure())
	notFound := &execStatusError{Status: metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonInternalError, Message: `exec: "bash": executable file not found in $PATH`}}
	assert.True(t, notFound.startFailure())
	assert.Contains(t, notFound.Error(), "executable file not found")
}

// The resource routes produce a valid OpenAPI document (raw stream and WebSocket routes
// included).
func TestResourceRoutesOpenAPI(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "..", "..")
	crds, err := openapi.LoadCRDSchemas(filepath.Join(root, "deploy", "crds"))
	require.NoError(t, err)
	tbl := apirest.NewTable()
	New(Deps{}).Register(tbl)
	require.NoError(t, tbl.Err())
	doc, err := openapi.Generate(tbl.Routes(), openapi.Options{Title: "t", Version: "v1", CRDs: crds,
		CommentDirs: map[string]string{"github.com/juliosaraiva/patrol-cd/pkg/apiclient": filepath.Join(root, "pkg", "apiclient")}})
	require.NoError(t, err)
	for _, id := range []string{"getResourceTree", "watchResourceTree", "getManagedResources", "serverSideDiff", "listApplicationEvents",
		"getResource", "patchResource", "deleteResource", "listResourceActions", "runResourceAction", "getApplicationLogs", "getPodLogs", "openTerminal"} {
		assert.Contains(t, string(doc), `"operationId": "`+id+`"`)
	}
	assert.Contains(t, string(doc), `"/terminal"`)
}
