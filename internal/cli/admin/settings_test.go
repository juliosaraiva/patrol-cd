package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	exampleSettings = "../../../examples/platform/settings/"
	overridesCM     = "testdata/settings/patrol-cm-overrides.yaml"
)

// offline runs an admin command that needs no cluster.
func offline(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	tio := newTestIO("default")
	tio.f.RESTConfig = nil
	err := tio.run(t.Context(), args...)
	return tio.out.String(), tio.errOut.String(), err
}

func TestSettingsValidateExamples(t *testing.T) {
	t.Parallel()
	args := []string{"admin", "settings", "validate", "--patrol-cm-path", exampleSettings + "patrol-cm.yaml", "--patrol-secret-path", exampleSettings + "patrol-secret.yaml"}
	for _, f := range []string{"patrol-rbac-cm.yaml", "patrol-cmd-params-cm.yaml", "patrol-gpg-keys-cm.yaml", "patrol-notifications-cm.yaml",
		"patrol-notifications-secret.yaml", "patrol-ssh-known-hosts-cm.yaml", "patrol-tls-certs-cm.yaml"} {
		args = append(args, "--settings-file", exampleSettings+f)
	}
	out, errOut, err := offline(t, args...)
	require.NoError(t, err)
	assert.NotContains(t, out, "❌", out)
	for _, g := range []string{"accounts", "certificates", "cmd-params", "general", "gpg-keys", "kustomize", "notifications", "rbac", "resource-overrides"} {
		assert.Contains(t, out, "✅ "+g+"\n")
	}
	assert.Contains(t, out, "OIDC is configured\n")
	assert.Contains(t, out, "2 accounts\n")
	assert.Contains(t, out, "--enable-helm (1 versions)\n")
	assert.Contains(t, out, `2 policy documents, default role "role:readonly", match mode glob`)
	assert.Contains(t, errOut, "note: "+exampleSettings+"patrol-secret.yaml has no server.secretkey")
	// Groups are printed in sorted order, separated by a blank line (upstream layout).
	assert.True(t, strings.Index(out, "✅ accounts") < strings.Index(out, "✅ general"))
	assert.Contains(t, out, "\n\n✅ certificates")
}

func TestSettingsValidateBadFixture(t *testing.T) {
	t.Parallel()
	out, _, err := offline(t, "admin", "settings", "validate", "--argocd-cm-path", "testdata/settings/bad-patrol-cm.yaml")
	require.NoError(t, err, "upstream exits 0 even when a group fails")
	for _, want := range []string{
		"❌ general\npatrol-cm: dex.config: error: invalid dex.config:",
		"❌ kustomize\n",
		"found duplicate kustomize version: v1.0.0",
		"❌ accounts\n",
		`unsupported capability "notacapability"`,
		"❌ resource-overrides\n",
		"resource.customizations.ignoreDifferences.apps_Deployment",
		"✅ rbac\n",
	} {
		assert.Contains(t, out, want)
	}

	out, _, err = offline(t, "admin", "settings", "validate", "--patrol-cm-path", "testdata/settings/bad-patrol-cm.yaml", "--group", "rbac", "--group", "accounts")
	require.NoError(t, err)
	assert.Equal(t, "✅ rbac\nno RBAC ConfigMap (built-in policy only)\n\n❌ accounts\npatrol-cm: accounts.robot: error: account \"robot\": settings key \"accounts.robot\": unsupported capability \"notacapability\"\n", out)

	_, _, err = offline(t, "admin", "settings", "validate", "--patrol-cm-path", "testdata/settings/bad-patrol-cm.yaml", "--group", "plugins")
	require.Error(t, err, "unknown groups are an error (upstream crashes)")
	assert.Contains(t, err.Error(), `unknown validation group "plugins"`)

	_, _, err = offline(t, "admin", "settings", "validate")
	require.EqualError(t, err, "either --patrol-cm-path must be provided or --load-cluster-settings must be set to true")
}

func TestSettingsValidateClusterSettings(t *testing.T) {
	ctx := requireEnv(t)
	ns := newNamespace(t, ctx, "settings")
	for _, f := range []string{"patrol-cm.yaml", "patrol-rbac-cm.yaml", "patrol-secret.yaml"} {
		applyFile(t, ctx, ns, exampleSettings+f)
	}
	_, err := envKube.CoreV1().Secrets(ns).Patch(ctx, "patrol-secret", types.MergePatchType, []byte(`{"stringData":{"server.secretkey":"0123456789abcdef0123456789abcdef"}}`), metav1.PatchOptions{})
	require.NoError(t, err)
	tio := newTestIO(ns)
	require.NoError(t, tio.run(ctx, "admin", "settings", "validate", "--load-cluster-settings"))
	out := tio.out.String()
	assert.Contains(t, out, "✅ general\nOIDC is configured\n")
	assert.NotContains(t, out, "unresolved secret reference", "patrol-oidc (labelled) resolves from the cluster")
	assert.Contains(t, out, `✅ rbac`+"\n"+`2 policy documents`)

	// Components ignore an unlabelled patrol-cm; validation from the cluster says so.
	_, err = envKube.CoreV1().ConfigMaps(ns).Patch(ctx, "patrol-cm", types.MergePatchType, []byte(`{"metadata":{"labels":{"app.kubernetes.io/part-of":null}}}`), metav1.PatchOptions{})
	require.NoError(t, err)
	require.NoError(t, tio.run(ctx, "admin", "settings", "validate", "--load-cluster-settings", "--group", "general"))
	assert.Contains(t, tio.out.String(), "❌ general\n")
	assert.Contains(t, tio.out.String(), "not labelled app.kubernetes.io/part-of=patrol-cd")
}

func TestResourceOverrides(t *testing.T) {
	t.Parallel()
	ro := func(args ...string) (string, error) {
		out, _, err := offline(t, append(append([]string{"admin", "settings", "resource-overrides"}, args...), "--patrol-cm-path", overridesCM)...)
		return out, err
	}
	out, err := ro("ignore-differences", "testdata/resources/deployment.yaml")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "Following fields are ignored:\n\n--- live/web\n+++ target/web\n"), out)
	assert.Contains(t, out, "\n-  replicas: 3\n")

	out, err = ro("ignore-differences", "testdata/resources/statefulset.yaml")
	require.NoError(t, err)
	assert.Equal(t, "Ignore differences are not configured for 'apps/StatefulSet'\n", out)

	out, err = ro("ignore-resource-updates", "testdata/resources/externalsecret.yaml")
	require.NoError(t, err)
	assert.Contains(t, out, "Following fields are ignored:")
	assert.Contains(t, out, `-  refreshTime: "2026-10-16T10:00:00Z"`)
	out, err = ro("ignore-resource-updates", "testdata/resources/statefulset.yaml")
	require.NoError(t, err)
	assert.Equal(t, "No overrides configured for 'apps/StatefulSet'\n", out)

	out, err = ro("health", "testdata/resources/widget-broken.yaml")
	require.NoError(t, err)
	assert.Equal(t, "STATUS: Degraded\nMESSAGE: widget is broken\n", out)
	out, err = ro("health", "testdata/resources/deployment.yaml")
	require.NoError(t, err)
	assert.Equal(t, "STATUS: Healthy\nMESSAGE: \n", out, "built-in check without overrides")

	out, err = ro("list-actions", "testdata/resources/deployment.yaml")
	require.NoError(t, err)
	assert.Equal(t, "NAME      DISABLED\nannotate  false\npause     true\nscale-up  false\n", out)
	out, err = ro("list-actions", "testdata/resources/statefulset.yaml")
	require.NoError(t, err)
	assert.Equal(t, "Actions are not configured for 'apps/StatefulSet'\n", out)

	out, err = ro("run-action", "testdata/resources/deployment.yaml", "scale-up")
	require.NoError(t, err)
	assert.Contains(t, out, "Following fields have been changed:\n\n")
	assert.Contains(t, out, "-  replicas: 3\n+  replicas: 4\n")
	out, err = ro("action", "testdata/resources/deployment.yaml", "annotate", "--param", "note=hello")
	require.NoError(t, err)
	assert.Contains(t, out, "+    example.com/note: hello\n")

	_, err = ro("run-action", "testdata/resources/deployment.yaml", "annotate", "--param", "bad")
	require.EqualError(t, err, "Invalid parameter format: bad")
	_, err = ro("run-action", "testdata/resources/deployment.yaml", "nope")
	require.Error(t, err)
	_, err = ro("health")
	require.Error(t, err, "missing argument")
}

func TestRBACCanAndValidate(t *testing.T) {
	t.Parallel()
	policy := "testdata/rbac/policy.csv"
	cases := []struct {
		args []string
		out  string
		code int
	}{
		{[]string{"alice", "sync", "applications", "team-a/web"}, "Yes\n", 0},
		{[]string{"alice", "sync", "app", "team-a/web"}, "Yes\n", 0},
		{[]string{"alice", "delete", "applications", "team-a/web-prod"}, "No\n", 1},
		{[]string{"bob", "sync", "applications", "team-a/web"}, "No\n", 1},
		{[]string{"role:deployer", "get", "applications", "team-a/x"}, "Yes\n", 0},
		{[]string{"role:admin", "update", "clusters", "*"}, "Yes\n", 0},
		{[]string{"role:readonly", "get", "projects"}, "Yes\n", 0},
		{[]string{"role:readonly", "update", "projects"}, "No\n", 1},
	}
	for _, c := range cases {
		out, _, err := offline(t, append(append([]string{"admin", "settings", "rbac", "can"}, c.args...), "--policy-file", policy)...)
		assert.Equal(t, c.out, out, c.args)
		assert.Equal(t, c.code, ExitCode(err), c.args)
	}
	out, _, err := offline(t, "admin", "settings", "rbac", "can", "bob", "get", "applications", "team-a/x", "--policy-file", policy, "--default-role", "role:readonly")
	require.NoError(t, err)
	assert.Equal(t, "Yes\n", out)
	out, _, err = offline(t, "admin", "settings", "rbac", "can", "bob", "update", "applications", "team-a/x", "--policy-file", policy, "-q")
	assert.Equal(t, 1, ExitCode(err))
	assert.Empty(t, out, "quiet mode prints nothing")
	_, _, err = offline(t, "admin", "settings", "rbac", "can", "role:admin", "get", "admin", "x", "--policy-file", policy, "--use-builtin-policy=false")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error in RBAC request")
	out, _, err = offline(t, "admin", "settings", "rbac", "can", "role:admin", "get", "admin", "x", "--policy-file", policy, "--strict=false")
	assert.Equal(t, 1, ExitCode(err))
	assert.Equal(t, "No\n", out)
	out, _, err = offline(t, "admin", "settings", "rbac", "can", "role:admin", "get", "clusters", "x", "--policy-file", policy, "--use-builtin-policy=false")
	assert.Equal(t, 1, ExitCode(err), "built-in roles come from the built-in policy")
	assert.Equal(t, "No\n", out)

	// A ConfigMap manifest as policy file: overlays and policy.default are honored.
	out, _, err = offline(t, "admin", "settings", "rbac", "can", "ci-bot", "sync", "applications", "any/app", "--policy-file", exampleSettings+"patrol-rbac-cm.yaml")
	require.NoError(t, err)
	assert.Equal(t, "Yes\n", out)
	out, _, err = offline(t, "admin", "settings", "rbac", "can", "nobody", "get", "applications", "any/app", "--policy-file", exampleSettings+"patrol-rbac-cm.yaml")
	require.NoError(t, err, "policy.default role:readonly applies")
	assert.Equal(t, "Yes\n", out)

	_, _, err = offline(t, "admin", "settings", "rbac", "can", "a", "get", "applications", "--policy-file", "testdata/rbac/invalid-policy.csv")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid user policy")

	out, _, err = offline(t, "admin", "settings", "rbac", "validate", "--policy-file", policy)
	require.NoError(t, err)
	assert.Equal(t, "Policy is valid.\n", out)
	out, _, err = offline(t, "admin", "settings", "rbac", "validate", "--policy-file", "testdata/rbac/invalid-policy.csv")
	assert.Equal(t, 1, ExitCode(err))
	assert.True(t, strings.HasPrefix(out, "Policy is invalid: invalid RBAC policy: testdata/rbac/invalid-policy.csv:1:"), out)

	empty := filepath.Join(t.TempDir(), "empty.csv")
	require.NoError(t, os.WriteFile(empty, []byte(""), 0o600))
	_, _, err = offline(t, "admin", "settings", "rbac", "validate", "--policy-file", empty)
	require.EqualError(t, err, "Policy is empty or could not be loaded.")

	_, _, err = offline(t, "admin", "settings", "rbac", "validate")
	require.EqualError(t, err, "please provide exactly one of --policy-file or --namespace")
	_, _, err = offline(t, "admin", "settings", "rbac", "validate", "-n", "x", "--policy-file", policy)
	require.EqualError(t, err, "please provide exactly one of --policy-file or --namespace")
	_, _, err = offline(t, "admin", "settings", "rbac", "can", "a", "get", "applications")
	require.EqualError(t, err, "please provide exactly one of --policy-file or --namespace")
}

func TestRBACFromNamespace(t *testing.T) {
	ctx := requireEnv(t)
	ns := newNamespace(t, ctx, "rbac")
	applyFile(t, ctx, ns, exampleSettings+"patrol-rbac-cm.yaml")
	tio := newTestIO(ns)
	require.NoError(t, tio.run(ctx, "admin", "settings", "rbac", "validate", "--namespace", ns))
	assert.Equal(t, "Policy is valid.\n", tio.out.String())
	err := tio.run(ctx, "admin", "settings", "rbac", "can", "example-org:team-a-leads", "delete", "applications", "team-a/web-prod", "-n", ns)
	assert.Equal(t, 1, ExitCode(err))
	assert.Equal(t, "No\n", tio.out.String())
	require.NoError(t, tio.run(ctx, "admin", "settings", "rbac", "can", "role:team-a-lead", "delete", "applications", "team-a/web", "-n", ns))
	assert.Equal(t, "Yes\n", tio.out.String())
	other := newNamespace(t, ctx, "rbac-missing")
	err = tio.run(ctx, "admin", "settings", "rbac", "validate", "-n", other)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not get configmap")
}
