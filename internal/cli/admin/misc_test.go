package admin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAdminHelpListsCommands(t *testing.T) {
	t.Parallel()
	out, _, err := offline(t, "admin")
	require.NoError(t, err)
	for _, name := range []string{"export", "import", "settings", "notifications", "cluster", "app", "repo", "proj",
		"initial-password", "redis-initial-password", "dashboard", "migrate-group", "argocd-import"} {
		assert.Regexp(t, `(?m)^\s+`+name+`\s`, out, name)
	}
	// kubectl connection flags are available on every admin command.
	out, _, err = offline(t, "admin", "initial-password", "--help")
	require.NoError(t, err)
	for _, flag := range []string{"--kubeconfig", "--context", "--namespace", "--server", "--insecure-skip-tls-verify"} {
		assert.Contains(t, out, flag)
	}
}

func TestRedisInitialPassword(t *testing.T) {
	t.Parallel()
	out, errOut, err := offline(t, "admin", "redis-initial-password")
	require.NoError(t, err)
	assert.Equal(t, 0, ExitCode(err))
	assert.Equal(t, "Patrol CD does not use Redis (ADR-0004): there is no Redis password to create. Nothing to do.\n", out)
	assert.Empty(t, errOut)
}

func TestInitialPassword(t *testing.T) {
	ctx := requireEnv(t)
	ns := newNamespace(t, ctx, "initpw")
	tio := newTestIO(ns)
	err := tio.run(ctx, "admin", "initial-password")
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err), err)

	_, err = envKube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: initialAdminSecretName},
		Data:       map[string][]byte{"password": []byte("generated-initial-password")},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, tio.run(ctx, "admin", "initial-password"))
	assert.Equal(t, "generated-initial-password\n\n This password must be only used for first time login. We strongly recommend you update the password using `patrol account update-password`.\n", tio.out.String())
	assert.Empty(t, tio.errOut.String(), "the password is printed once, on stdout only")
}

func TestRewriteGroupKey(t *testing.T) {
	t.Parallel()
	const from, to = "cd.patrol.example", "patrolcd.io"
	for _, tc := range []struct{ in, want string }{
		{"cd.patrol.example/tracking-id", "patrolcd.io/tracking-id"},
		{"link.cd.patrol.example/docs", "link.patrolcd.io/docs"},
		{"notified.notifications.cd.patrol.example", "notified.notifications.patrolcd.io"},
		{"notifications.cd.patrol.example/subscribe.on-sync.slack", "notifications.patrolcd.io/subscribe.on-sync.slack"},
		{"resources-finalizer.cd.patrol.example/background", "resources-finalizer.patrolcd.io/background"},
		{"resources-finalizer.cd.patrol.example", "resources-finalizer.patrolcd.io"},
		{"cd.patrol.example", "patrolcd.io"},
		// Not a whole label of the group: unchanged.
		{"xcd.patrol.example/key", "xcd.patrol.example/key"},
		{"cd.patrol.examples/key", "cd.patrol.examples/key"},
		{"example.com/cd.patrol.example", "example.com/cd.patrol.example"},
		{"app.kubernetes.io/instance", "app.kubernetes.io/instance"},
	} {
		assert.Equal(t, tc.want, rewriteGroupKey(tc.in, from, to), tc.in)
	}
	assert.Equal(t, "cd.patrol.example/x", rewriteGroupKey("cd.patrol.example/x", from, from))
	assert.Equal(t, "cd.patrol.example/x", rewriteGroupKey("cd.patrol.example/x", "", to))
	assert.Equal(t, "a.cd.patrol.example.patrolcd.io/b", rewriteGroupKey("a.cd.patrol.example.cd.patrol.example/b", from, to), "only the label before / is the key's group")
}
