package applications_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applications"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

var execCM = map[string]string{"exec.enabled": "true", "exec.shells": "bash,sh"}

func (re *resEnv) terminalReq(pod string) *apiclient.TerminalRequest {
	return &apiclient.TerminalRequest{AppName: "guestbook", ProjectName: "proj-a", Namespace: re.appNS, Pod: pod, Container: "app"}
}

func TestTerminalSession(t *testing.T) {
	re := newResEnv(t, resEnvOptions{cm: execCM})
	re.fixture("guestbook", "proj-a")
	ctx, cancel := context.WithTimeout(re.ctx, time.Minute)
	defer cancel()

	sess, err := re.rclient("execer").OpenTerminal(ctx, re.terminalReq("web-abc-1"))
	require.NoError(t, err)
	// bash is not in the container: the next configured shell is used.
	out, err := sess.ReadUntil(ctx, "sh$ ")
	require.NoError(t, err, out)
	require.NoError(t, sess.Resize(ctx, 40, 120))
	_, err = sess.ReadUntil(ctx, "resized 120x40")
	require.NoError(t, err)
	require.NoError(t, sess.SendStdin(ctx, "ls -la\r"))
	_, err = sess.ReadUntil(ctx, "echo:ls -la\r")
	require.NoError(t, err)
	require.NoError(t, sess.SendStdin(ctx, "exit\r"))
	_, err = sess.Recv(ctx)
	assert.ErrorIs(t, err, io.EOF, "the session closes normally when the shell exits")

	execs := func() []string {
		re.cluster.mu.Lock()
		defer re.cluster.mu.Unlock()
		var out []string
		for _, q := range re.cluster.execs {
			out = append(out, q.Get("command")+"/"+q.Get("container")+"/"+q.Get("tty"))
		}
		return out
	}
	assert.Equal(t, []string{"bash/app/true", "sh/app/true"}, execs())
	re.cluster.mu.Lock()
	assert.Equal(t, []string{"120x40"}, re.cluster.resizes)
	re.cluster.mu.Unlock()

	// Session audit: start and end, actor and target, no terminal content.
	entries := re.sessions.byOperation("openTerminal")
	require.Len(t, entries, 2)
	assert.Equal(t, applications.ReasonTerminalSessionStarted, entries[0].EventReason)
	assert.Equal(t, applications.ReasonTerminalSessionEnded, entries[1].EventReason)
	for _, e := range entries {
		assert.Equal(t, "execer", e.Actor)
		assert.Equal(t, "exec", e.Resource)
		assert.Equal(t, "proj-a/guestbook", e.Object)
		assert.Equal(t, "guestbook", e.Target.Name)
		assert.Contains(t, e.Message, "pod "+re.appNS+"/web-abc-1 container app")
		assert.NotContains(t, e.Message, "ls -la")
	}

	// A requested shell from exec.shells is used directly.
	req := re.terminalReq("web-abc-1")
	req.Shell = "sh"
	sess, err = re.rclient("alice").OpenTerminal(ctx, req)
	require.NoError(t, err)
	_, err = sess.ReadUntil(ctx, "sh$ ")
	require.NoError(t, err)
	require.NoError(t, sess.Close())
	assert.Equal(t, "sh/app/true", execs()[len(execs())-1])

	// The web terminal never impersonates (SYNC-123).
	for _, r := range re.cluster.seen(func(r seenRequest) bool { return strings.HasSuffix(r.path, "/exec") }) {
		assert.Empty(t, r.impersonate)
	}
}

func TestTerminalRefusals(t *testing.T) {
	re := newResEnv(t, resEnvOptions{cm: map[string]string{"exec.shells": "sh"}})
	re.fixture("guestbook", "proj-a")
	ctx, cancel := context.WithTimeout(re.ctx, time.Minute)
	defer cancel()
	open := func(c *apiclient.Client, req *apiclient.TerminalRequest) error {
		s, err := c.OpenTerminal(ctx, req)
		if err == nil {
			_ = s.Close()
		}
		return err
	}
	execer := re.rclient("execer")

	// exec.enabled unset → 404.
	requireProblem(t, open(execer, re.terminalReq("web-abc-1")), 404, "the web terminal is disabled")
	re.holder.setCM("exec.enabled", "true")

	requireProblem(t, open(re.rclient("reader"), re.terminalReq("web-abc-1")), 403, "permission denied: exec, create, proj-a/guestbook")
	requireProblem(t, open(re.rclient("logger"), re.terminalReq("web-abc-1")), 403, "permission denied: exec, create")
	requireProblem(t, open(re.rclient("bob"), re.terminalReq("web-abc-1")), 403, "")
	requireProblem(t, open(re.rclient(""), re.terminalReq("web-abc-1")), 401, "")
	requireProblem(t, open(execer, re.terminalReq("outsider")), 400, "Pod doesn't belong to specified app")
	requireProblem(t, open(execer, re.terminalReq("web-abc-2")), 400, `container "app" is not running`)
	wrongProject := re.terminalReq("web-abc-1")
	wrongProject.ProjectName = "proj-b"
	requireProblem(t, open(re.rclient("admin"), wrongProject), 404, "not found")
	missing := re.terminalReq("web-abc-1")
	missing.Container = ""
	requireProblem(t, open(execer, missing), 400, "Missing required parameters")
	bad := re.terminalReq("web-abc-1")
	bad.Pod = "../../etc"
	requireProblem(t, open(execer, bad), 400, "Pod name is not valid")
	otherNS := re.terminalReq("web-abc-1")
	otherNS.AppNamespace = re.nsB
	requireProblem(t, open(execer, otherNS), 403, "namespace '"+re.nsB+"' is not permitted")

	// Cross-site WebSocket hijacking: a forged Origin is refused.
	forged, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: re.url, AuthToken: execer.Token(), Headers: []string{"Origin: https://evil.example"}})
	require.NoError(t, err)
	requireProblem(t, open(forged, re.terminalReq("web-abc-1")), 403, "origin not allowed")
	sameOrigin, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: re.url, AuthToken: execer.Token(), Headers: []string{"Origin: " + re.url}})
	require.NoError(t, err)
	require.NoError(t, open(sameOrigin, re.terminalReq("web-abc-1")))
	configured, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: re.url, AuthToken: execer.Token(), Headers: []string{"Origin: https://patrol.example.com"}})
	require.NoError(t, err)
	require.NoError(t, open(configured, re.terminalReq("web-abc-1")), "the configured url is an allowed origin")

	assert.Empty(t, re.cluster.seen(func(r seenRequest) bool {
		return strings.HasSuffix(r.path, "/exec") && strings.Contains(r.query, "outsider")
	}))
	// Denials are audited by the framework.
	var denied int
	for _, e := range re.audit.byOperation("openTerminal") {
		if e.Result == rest.AuditDenied {
			denied++
		}
	}
	assert.Positive(t, denied)
}

// Permissions are re-checked on every message: a revoked exec permission ends the session.
// Idle sessions are closed.
func TestTerminalRevocationAndIdle(t *testing.T) {
	re := newResEnv(t, resEnvOptions{cm: map[string]string{"exec.enabled": "true", "exec.shells": "sh"},
		resources: func(r *applications.ResourceDeps) {
			r.Terminal.IdleTimeout = 500 * time.Millisecond
			r.Terminal.RecheckInterval = 100 * time.Millisecond
		}})
	re.fixture("guestbook", "proj-a")
	ctx, cancel := context.WithTimeout(re.ctx, time.Minute)
	defer cancel()

	sess, err := re.rclient("execer").OpenTerminal(ctx, re.terminalReq("web-abc-1"))
	require.NoError(t, err)
	_, err = sess.ReadUntil(ctx, "sh$ ")
	require.NoError(t, err)
	re.setPolicy(strings.ReplaceAll(resourcePolicy, "p, role:a-exec, exec, create, proj-a/*, allow\n", ""))
	require.NoError(t, sess.SendStdin(ctx, "id\r"))
	out, err := sess.ReadUntil(ctx, "Permission denied")
	require.NoError(t, err, out)
	assert.NotContains(t, out, "echo:id", "the input was not forwarded")
	_, err = sess.Recv(ctx)
	code, reason := apiclient.CloseStatus(err)
	assert.Equal(t, 1008, code, "policy violation: %v", err)
	assert.Equal(t, "permission denied", reason)

	re.setPolicy(resourcePolicy)

	// A revoked session ends the terminal at the next periodic check, without input.
	c, token := re.rclientToken("execer")
	sess, err = c.OpenTerminal(ctx, re.terminalReq("web-abc-1"))
	require.NoError(t, err)
	_, err = sess.ReadUntil(ctx, "sh$ ")
	require.NoError(t, err)
	p, err := re.tokens.Verify(re.ctx, token)
	require.NoError(t, err)
	require.NoError(t, re.tokens.Revoke(re.ctx, p))
	out, err = sess.ReadUntil(ctx, "Permission denied")
	require.NoError(t, err, out)

	sess, err = re.rclient("execer").OpenTerminal(ctx, re.terminalReq("web-abc-1"))
	require.NoError(t, err)
	out, err = sess.ReadUntil(ctx, "without input")
	require.NoError(t, err, out)
	_, err = sess.Recv(ctx)
	assert.True(t, errors.Is(err, io.EOF), "%v", err)
	entries := re.sessions.byOperation("openTerminal")
	require.NotEmpty(t, entries)
	assert.Contains(t, entries[len(entries)-1].Message, "idle timeout")
}

// With impersonation enforced and no matching service account, logs and live reads fail
// but the terminal works with the cluster credentials (SYNC-123).
func TestTerminalNotImpersonated(t *testing.T) {
	re := newResEnv(t, resEnvOptions{cm: map[string]string{"exec.enabled": "true", "exec.shells": "sh", "application.sync.impersonation.enabled": "true"}})
	re.fixture("guestbook", "proj-a")
	ctx, cancel := context.WithTimeout(re.ctx, time.Minute)
	defer cancel()
	_, err := re.rclient("alice").StreamPodLogs(ctx, &apiclient.ApplicationPodLogsQuery{Name: "guestbook", Pod: "web-abc-1"})
	requireProblem(t, err, 412, "no matching service account found")
	sess, err := re.rclient("alice").OpenTerminal(ctx, re.terminalReq("web-abc-1"))
	require.NoError(t, err)
	_, err = sess.ReadUntil(ctx, "sh$ ")
	require.NoError(t, err)
	require.NoError(t, sess.Close())
	for _, r := range re.cluster.seen(func(r seenRequest) bool { return strings.Contains(r.path, "/pods/web-abc-1") }) {
		assert.Empty(t, r.impersonate, r.path)
	}
	_ = v1alpha1.GroupName
}
