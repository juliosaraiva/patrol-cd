package applications_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applications"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func readAllLogs(t *testing.T, s *apiclient.EventStream) ([]apiclient.LogEntry, error) {
	t.Helper()
	var out []apiclient.LogEntry
	err := apiclient.ReadLogs(s, func(e apiclient.LogEntry) error { out = append(out, e); return nil })
	return out, err
}

func contents(entries []apiclient.LogEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.PodName+": "+e.Content)
	}
	return out
}

func TestApplicationLogs(t *testing.T) {
	re := newResEnv(t, resEnvOptions{})
	re.fixture("guestbook", "proj-a")
	ns := re.appNS
	logger := re.rclient("logger")

	// Deployment-level aggregation: the pods of its ReplicaSet, merged by timestamp.
	s, err := logger.StreamApplicationLogs(re.ctx, &apiclient.ApplicationLogsQuery{Name: "guestbook", Kind: "Deployment", Group: "apps", ResourceName: "web", TailLines: ptrInt64(100)})
	require.NoError(t, err)
	entries, err := readAllLogs(t, s)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"web-abc-1: web1 started", "web-abc-2: web2 started", "web-abc-1: GET /healthz 200",
		"web-abc-2: GET /healthz 200", "web-abc-1: web1 ERROR boom",
	}, contents(entries))
	assert.Equal(t, "2026-01-01T00:00:01Z", entries[0].TimeStampStr)
	logReqs := re.cluster.seen(func(r seenRequest) bool { return strings.HasSuffix(r.path, "/log") })
	require.Len(t, logReqs, 2)
	assert.Contains(t, logReqs[0].query, "tailLines=100")
	assert.Contains(t, logReqs[0].query, "timestamps=true")
	assert.Empty(t, logReqs[0].impersonate)

	// Filters: inverse substring, case sensitivity.
	s, err = logger.StreamApplicationLogs(re.ctx, &apiclient.ApplicationLogsQuery{Name: "guestbook", Filter: "!healthz"})
	require.NoError(t, err)
	entries, err = readAllLogs(t, s)
	require.NoError(t, err)
	assert.Equal(t, []string{"web-abc-1: web1 started", "web-abc-2: web2 started", "web-abc-1: web1 ERROR boom"}, contents(entries))
	s, err = logger.StreamApplicationLogs(re.ctx, &apiclient.ApplicationLogsQuery{Name: "guestbook", Filter: "error", MatchCase: ptrBool(true)})
	require.NoError(t, err)
	entries, err = readAllLogs(t, s)
	require.NoError(t, err)
	assert.Empty(t, entries)

	// One pod, with container/previous/sinceSeconds passed through.
	s, err = logger.StreamPodLogs(re.ctx, &apiclient.ApplicationPodLogsQuery{Name: "guestbook", Pod: "web-abc-2", Container: "app", Previous: ptrBool(true), SinceSeconds: ptrInt64(60)})
	require.NoError(t, err)
	entries, err = readAllLogs(t, s)
	require.NoError(t, err)
	assert.Equal(t, []string{"web-abc-2: web2 started", "web-abc-2: GET /healthz 200"}, contents(entries))
	logReqs = re.cluster.seen(func(r seenRequest) bool { return strings.HasSuffix(r.path, "/web-abc-2/log") })
	last := logReqs[len(logReqs)-1].query
	assert.Contains(t, last, "container=app")
	assert.Contains(t, last, "previous=true")
	assert.Contains(t, last, "sinceSeconds=60")

	// untilTime: the first line after it is the last entry.
	s, err = logger.StreamApplicationLogs(re.ctx, &apiclient.ApplicationLogsQuery{Name: "guestbook", UntilTime: "2026-01-01T00:00:02.5Z"})
	require.NoError(t, err)
	var got []apiclient.LogEntry
	for {
		ev, err := s.Next()
		require.NoError(t, err)
		var e apiclient.LogEntry
		require.NoError(t, ev.Decode(&e))
		got = append(got, e)
		if e.Last {
			break
		}
	}
	_ = s.Close()
	require.Len(t, got, 3)
	assert.Equal(t, "GET /healthz 200", got[2].Content)

	// A pod that is not part of the tree yields nothing; a pod whose log cannot be read
	// yields its error as a line.
	s, err = logger.StreamPodLogs(re.ctx, &apiclient.ApplicationPodLogsQuery{Name: "guestbook", Pod: "outsider"})
	require.NoError(t, err)
	entries, err = readAllLogs(t, s)
	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.Empty(t, re.cluster.seen(func(r seenRequest) bool { return strings.HasSuffix(r.path, "/outsider/log") }), "never read")
	re.cluster.setLog(ns+"/web-abc-2", fakePodLog{status: 400, message: `container "app" in pod "web-abc-2" is waiting to start: ContainerCreating`})
	s, err = logger.StreamPodLogs(re.ctx, &apiclient.ApplicationPodLogsQuery{Name: "guestbook", Pod: "web-abc-2"})
	require.NoError(t, err)
	entries, err = readAllLogs(t, s)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Contains(t, entries[0].Content, "is waiting to start")

	// Download: a text attachment.
	body, err := logger.DownloadApplicationLogs(re.ctx, &apiclient.ApplicationLogsQuery{Name: "guestbook", PodName: "web-abc-1", Namespace: ns, Container: "app"})
	require.NoError(t, err)
	b, err := io.ReadAll(body)
	require.NoError(t, err)
	_ = body.Close()
	assert.Equal(t, "web1 started\nGET /healthz 200\nweb1 ERROR boom\n", string(b))

	// Invalid input.
	_, err = logger.StreamApplicationLogs(re.ctx, &apiclient.ApplicationLogsQuery{Name: "guestbook", UntilTime: "tomorrow"})
	requireProblem(t, err, 400, "invalid untilTime parameter value")
}

// A pod whose log answers late is still merged by timestamp: no line of the other pod goes out
// before it (issue #223: under load web-abc-2's lines came first).
func TestApplicationLogsWaitsForADelayedPod(t *testing.T) {
	re := newResEnv(t, resEnvOptions{})
	re.fixture("guestbook", "proj-a")
	ns := re.appNS
	release := make(chan struct{})
	answer := sync.OnceFunc(func() { close(release) })
	t.Cleanup(answer) // runs before the fake cluster closes
	// web-abc-1 answers 200 ms (ten times the harness's former merge window) after web-abc-2's
	// lines were sent.
	re.cluster.updateLog(ns+"/web-abc-1", func(pl *fakePodLog) { pl.hold = release })
	re.cluster.updateLog(ns+"/web-abc-2", func(pl *fakePodLog) {
		pl.served = sync.OnceFunc(func() { time.AfterFunc(200*time.Millisecond, answer) })
	})

	s, err := re.rclient("logger").StreamApplicationLogs(re.ctx, &apiclient.ApplicationLogsQuery{Name: "guestbook", Kind: "Deployment", Group: "apps", ResourceName: "web", TailLines: ptrInt64(100)})
	require.NoError(t, err)
	entries, err := readAllLogs(t, s)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"web-abc-1: web1 started", "web-abc-2: web2 started", "web-abc-1: GET /healthz 200",
		"web-abc-2: GET /healthz 200", "web-abc-1: web1 ERROR boom",
	}, contents(entries))
	require.NotEmpty(t, entries)
	assert.Equal(t, "2026-01-01T00:00:01Z", entries[0].TimeStampStr)
	assert.Len(t, re.cluster.seen(func(r seenRequest) bool { return strings.HasSuffix(r.path, "/log") }), 2)
}

func TestApplicationLogsAuthorization(t *testing.T) {
	re := newResEnv(t, resEnvOptions{cm: map[string]string{"server.maxPodLogsToRender": "1"}})
	re.fixture("guestbook", "proj-a")
	q := &apiclient.ApplicationLogsQuery{Name: "guestbook", PodName: "web-abc-1"}

	// applications, get is not enough: logs, get is required (TEN-084).
	_, err := re.rclient("reader").StreamApplicationLogs(re.ctx, q)
	requireProblem(t, err, 403, "permission denied: logs, get, proj-a/guestbook")
	_, err = re.rclient("bob").StreamApplicationLogs(re.ctx, q)
	requireProblem(t, err, 403, "permission denied")
	_, err = re.rclient("bob").StreamApplicationLogs(re.ctx, &apiclient.ApplicationLogsQuery{Name: "guestbook", Project: "proj-a"})
	requireProblem(t, err, 403, "")
	_, err = re.rclient("").StreamApplicationLogs(re.ctx, q)
	requireProblem(t, err, 401, "")
	assert.Empty(t, re.cluster.seen(func(r seenRequest) bool { return strings.HasSuffix(r.path, "/log") }), "denied requests never reach the cluster")

	// server.maxPodLogsToRender.
	_, err = re.rclient("logger").StreamApplicationLogs(re.ctx, &apiclient.ApplicationLogsQuery{Name: "guestbook", Kind: "Deployment", ResourceName: "web"})
	requireProblem(t, err, 400, "max pods to view logs are reached. Please provide more granular query")
	s, err := re.rclient("logger").StreamApplicationLogs(re.ctx, q)
	require.NoError(t, err)
	_, err = readAllLogs(t, s)
	require.NoError(t, err)

	// server.maxPodLogsToRender "0" refuses every pod (upstream: not "unlimited"); a query
	// matching no pod is still an empty stream.
	re0 := newResEnv(t, resEnvOptions{cm: map[string]string{"server.maxPodLogsToRender": "0"}})
	re0.fixture("guestbook", "proj-a")
	_, err = re0.rclient("logger").StreamApplicationLogs(re0.ctx, q)
	requireProblem(t, err, 400, "max pods to view logs are reached. Please provide more granular query")
	_, err = re0.rclient("logger").StreamApplicationLogs(re0.ctx, &apiclient.ApplicationLogsQuery{Name: "guestbook", Kind: "Deployment", ResourceName: "web"})
	requireProblem(t, err, 400, "max pods to view logs are reached")
	s, err = re0.rclient("logger").StreamApplicationLogs(re0.ctx, &apiclient.ApplicationLogsQuery{Name: "guestbook", PodName: "no-such-pod"})
	require.NoError(t, err)
	entries, err := readAllLogs(t, s)
	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.Empty(t, re0.cluster.seen(func(r seenRequest) bool { return strings.HasSuffix(r.path, "/log") }), "refused requests never reach the cluster")
}

// A followed stream ends cleanly when the client goes away or loses its permission, and is
// impersonated when configured.
func TestApplicationLogsFollow(t *testing.T) {
	re := newResEnv(t, resEnvOptions{
		cm: map[string]string{"application.sync.impersonation.enabled": "true", "application.sync.impersonation.enforced": "false"},
		resources: func(r *applications.ResourceDeps) {
			r.Logs.RecheckInterval = 100 * time.Millisecond
		},
	})
	re.fixture("guestbook", "proj-a")
	ns := re.appNS
	re.updateProject("proj-a", func(p *v1alpha1.AppProject) {
		p.Spec.DestinationServiceAccounts = []v1alpha1.ApplicationDestinationServiceAccount{{Server: inCluster, Namespace: ns, DefaultServiceAccount: "viewer"}}
	})
	re.cluster.setLog(ns+"/web-abc-1", fakePodLog{lines: []string{"2026-01-01T00:00:01Z hello"}, follow: true})

	ctx, cancel := context.WithCancel(re.ctx)
	s, err := re.rclient("logger").StreamPodLogs(ctx, &apiclient.ApplicationPodLogsQuery{Name: "guestbook", Pod: "web-abc-1", Follow: ptrBool(true)})
	require.NoError(t, err)
	n := 0
	err = apiclient.ReadLogs(s, func(e apiclient.LogEntry) error {
		n++
		if n >= 4 {
			return apiclient.ErrStopWatch
		}
		return nil
	})
	require.NoError(t, err)
	cancel()
	select {
	case pod := <-re.cluster.followEnd:
		assert.Equal(t, ns+"/web-abc-1", pod)
	case <-time.After(10 * time.Second):
		t.Fatal("the pod log stream was not closed after the client left")
	}
	reqs := re.cluster.seen(func(r seenRequest) bool { return strings.HasSuffix(r.path, "/web-abc-1/log") })
	require.NotEmpty(t, reqs)
	assert.Contains(t, reqs[0].query, "follow=true")
	assert.Equal(t, "system:serviceaccount:"+ns+":viewer", reqs[0].impersonate)

	// Losing logs, get mid-stream ends the stream with a 403 error event.
	s, err = re.rclient("logger").StreamPodLogs(re.ctx, &apiclient.ApplicationPodLogsQuery{Name: "guestbook", Pod: "web-abc-1", Follow: ptrBool(true)})
	require.NoError(t, err)
	first := true
	err = apiclient.ReadLogs(s, func(apiclient.LogEntry) error {
		if first {
			first = false
			re.setPolicy(strings.ReplaceAll(resourcePolicy, "p, role:a-logger, logs, get, proj-a/*, allow\n", ""))
		}
		return nil
	})
	assert.Equal(t, 403, apiErr(t, err).StatusCode)
	select {
	case <-re.cluster.followEnd:
	case <-time.After(10 * time.Second):
		t.Fatal("the pod log stream was not closed after the permission was revoked")
	}

	// A revoked session ends the stream with a 401 error event.
	re.setPolicy(resourcePolicy)
	c, token := re.rclientToken("logger")
	s, err = c.StreamPodLogs(re.ctx, &apiclient.ApplicationPodLogsQuery{Name: "guestbook", Pod: "web-abc-1", Follow: ptrBool(true)})
	require.NoError(t, err)
	first = true
	err = apiclient.ReadLogs(s, func(apiclient.LogEntry) error {
		if first {
			first = false
			p, err := re.tokens.Verify(re.ctx, token)
			require.NoError(t, err)
			require.NoError(t, re.tokens.Revoke(re.ctx, p))
		}
		return nil
	})
	assert.Equal(t, 401, apiErr(t, err).StatusCode)
}

func ptrInt64(v int64) *int64 { return &v }
