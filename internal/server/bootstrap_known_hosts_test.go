package server

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// TestDefaultSSHKnownHostsFingerprints pins every default entry to the SHA256 fingerprint its
// provider publishes (GitHub: api.github.com/meta and the "GitHub's SSH key fingerprints"
// page; Bitbucket: bitbucket.org/site/ssh; GitLab.com: docs.gitlab.com SSH host keys
// fingerprints; Azure DevOps: learn.microsoft.com, "Use SSH key authentication"), as checked
// on 2026-10-17 (docs/verification/runs/2026-10-17-chart-static.md).
func TestDefaultSSHKnownHostsFingerprints(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"[ssh.github.com]:443 ecdsa-sha2-nistp256": "SHA256:p2QAMXNIC1TJYWeIOttrVc98/R1BUFWu3/LiyKgUfQM",
		"[ssh.github.com]:443 ssh-ed25519":         "SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU",
		"[ssh.github.com]:443 ssh-rsa":             "SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s",
		"github.com ecdsa-sha2-nistp256":           "SHA256:p2QAMXNIC1TJYWeIOttrVc98/R1BUFWu3/LiyKgUfQM",
		"github.com ssh-ed25519":                   "SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU",
		"github.com ssh-rsa":                       "SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s",
		"bitbucket.org ecdsa-sha2-nistp256":        "SHA256:FC73VB6C4OQLSCrjEayhMp9UMxS97caD/Yyi2bhW/J0",
		"bitbucket.org ssh-ed25519":                "SHA256:ybgmFkzwOSotHTHLJgHO0QN8L0xErw6vd0VhFA9m3SM",
		"bitbucket.org ssh-rsa":                    "SHA256:46OSHA1Rmj8E8ERTC6xkNcmGOw9oFxYr0WF6zWW8l1E",
		"gitlab.com ecdsa-sha2-nistp256":           "SHA256:HbW3g8zUjNSksFbqTiUWPWg2Bq1x8xdGUrliXFzSnUw",
		"gitlab.com ssh-ed25519":                   "SHA256:eUXGGm1YGsMAS7vkcx6JOJdOGHPem5gQp4taiCfCLB8",
		"gitlab.com ssh-rsa":                       "SHA256:ROQFvPThGrW4RuWLoL9tq9I9zJ42fK4XywyRtbOz/EQ",
		"ssh.dev.azure.com ssh-rsa":                "SHA256:ohD8VZEXGWo6Ez8GSEJQ9WpafgLFsOfLOtGGQCQo6Og",
		"vs-ssh.visualstudio.com ssh-rsa":          "SHA256:ohD8VZEXGWo6Ez8GSEJQ9WpafgLFsOfLOtGGQCQo6Og",
	}
	got := map[string]string{}
	rest := []byte(DefaultSSHKnownHosts)
	for len(strings.TrimSpace(string(rest))) > 0 {
		marker, hosts, key, _, next, err := ssh.ParseKnownHosts(rest)
		require.NoError(t, err)
		assert.Empty(t, marker)
		require.Len(t, hosts, 1)
		id := hosts[0] + " " + key.Type()
		assert.NotContains(t, got, id, "duplicate entry")
		got[id] = ssh.FingerprintSHA256(key)
		rest = next
	}
	assert.Equal(t, want, got)
	assert.True(t, strings.HasSuffix(DefaultSSHKnownHosts, "\n"))
	assert.Equal(t, len(want), strings.Count(DefaultSSHKnownHosts, "\n"), "one entry per line")
}

// createKnownHostsCM creates patrol-ssh-known-hosts-cm as the install manifests ship it
// (labelled, data as given).
func createKnownHostsCM(t *testing.T, kube kubernetes.Interface, ns string, labels, data map[string]string) *corev1.ConfigMap {
	t.Helper()
	cm, err := kube.CoreV1().ConfigMaps(ns).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultSSHKnownHostsConfigMapName, Namespace: ns, Labels: labels},
		Data:       data,
	}, metav1.CreateOptions{FieldManager: "install"})
	require.NoError(t, err)
	return cm
}

func getKnownHostsCM(t *testing.T, kube kubernetes.Interface, ns string) *corev1.ConfigMap {
	t.Helper()
	cm, err := kube.CoreV1().ConfigMaps(ns).Get(context.Background(), settings.DefaultSSHKnownHostsConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	return cm
}

// TestBootstrapSeedsKnownHosts: the default known hosts are written once into a shipped
// ConfigMap without the key; a present key (managed through the API or by the operator, even
// empty), an unlabelled or a missing ConfigMap, and --skip-ssh-known-hosts are left alone.
// After seeding, an installer's server-side apply of the ConfigMap without data neither
// conflicts nor removes the key (the reason the install no longer renders it).
func TestBootstrapSeedsKnownHosts(t *testing.T) {
	t.Parallel()
	t.Run("shipped without the key", func(t *testing.T) {
		t.Parallel()
		kube, ns := bootstrapKube(t)
		createKnownHostsCM(t, kube, ns, partOf, nil)
		logs, err := runBootstrapLogged(t, ns, nil)
		require.NoError(t, err, logs)
		cm := getKnownHostsCM(t, kube, ns)
		assert.Equal(t, DefaultSSHKnownHosts, cm.Data[settings.SSHKnownHostsKey])
		assert.Contains(t, logs, `"msg":"seeded the default known hosts"`)
		owner := ""
		for _, mf := range cm.ManagedFields {
			if mf.FieldsV1 != nil && strings.Contains(mf.FieldsV1.GetRawString(), `"f:`+settings.SSHKnownHostsKey+`"`) {
				owner = mf.Manager + "/" + string(mf.Operation)
			}
		}
		assert.Equal(t, BootstrapFieldManager+"/Update", owner, "owner of data.ssh_known_hosts")

		// A re-run writes nothing.
		logs, err = runBootstrapLogged(t, ns, nil)
		require.NoError(t, err, logs)
		assert.Equal(t, cm.ResourceVersion, getKnownHostsCM(t, kube, ns).ResourceVersion)
		assert.Contains(t, logs, `"msg":"known hosts present; unchanged"`)

		// The installer re-applies the ConfigMap as rendered (labels, no data): no conflict
		// without force, and the seeded key stays.
		applied := []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"` + settings.DefaultSSHKnownHostsConfigMapName +
			`","labels":{"app.kubernetes.io/part-of":"patrol-cd","app.kubernetes.io/managed-by":"Helm"}}}`)
		_, err = kube.CoreV1().ConfigMaps(ns).Patch(context.Background(), settings.DefaultSSHKnownHostsConfigMapName,
			types.ApplyPatchType, applied, metav1.PatchOptions{FieldManager: "helm"})
		require.NoError(t, err)
		assert.Equal(t, DefaultSSHKnownHosts, getKnownHostsCM(t, kube, ns).Data[settings.SSHKnownHostsKey])
	})
	for name, data := range map[string]map[string]string{
		"key managed through the API": {settings.SSHKnownHostsKey: "git.example.test ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPlaceholderTestOnly\n"},
		"key emptied through the API": {settings.SSHKnownHostsKey: ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			kube, ns := bootstrapKube(t)
			before := createKnownHostsCM(t, kube, ns, partOf, data)
			logs, err := runBootstrapLogged(t, ns, nil)
			require.NoError(t, err, logs)
			after := getKnownHostsCM(t, kube, ns)
			assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
			assert.Equal(t, data, after.Data)
		})
	}
	t.Run("unlabelled", func(t *testing.T) {
		t.Parallel()
		kube, ns := bootstrapKube(t)
		before := createKnownHostsCM(t, kube, ns, nil, nil)
		logs, err := runBootstrapLogged(t, ns, nil)
		require.NoError(t, err, logs)
		assert.Equal(t, before.ResourceVersion, getKnownHostsCM(t, kube, ns).ResourceVersion)
		assert.Contains(t, logs, "not labelled")
	})
	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		kube, ns := bootstrapKube(t)
		logs, err := runBootstrapLogged(t, ns, nil)
		require.NoError(t, err, logs)
		_, err = kube.CoreV1().ConfigMaps(ns).Get(context.Background(), settings.DefaultSSHKnownHostsConfigMapName, metav1.GetOptions{})
		assert.True(t, apierrors.IsNotFound(err), "the bootstrap must not create the ConfigMap: %v", err)
		assert.Contains(t, logs, "known hosts ConfigMap not found")
	})
	t.Run("skip flag", func(t *testing.T) {
		t.Parallel()
		kube, ns := bootstrapKube(t)
		before := createKnownHostsCM(t, kube, ns, partOf, nil)
		logs, err := runBootstrapLogged(t, ns, func(c *BootstrapConfig) { c.SkipSSHKnownHosts = true })
		require.NoError(t, err, logs)
		assert.Equal(t, before.ResourceVersion, getKnownHostsCM(t, kube, ns).ResourceVersion)
		assert.Contains(t, logs, "--skip-ssh-known-hosts")
	})
}

// TestBootstrapKnownHostsRBAC: without update on the known hosts ConfigMap the run fails with
// Forbidden (the rule is part of the contract whenever the bootstrap seeds).
func TestBootstrapKnownHostsRBAC(t *testing.T) {
	t.Parallel()
	kube, ns := bootstrapKube(t)
	createSettingsCM(t, kube, ns, map[string]string{"admin.enabled": "false"})
	createKnownHostsCM(t, kube, ns, partOf, nil)
	rules := bootstrapPolicyRules()
	last := &rules[len(rules)-1]
	require.Equal(t, []string{settings.DefaultSSHKnownHostsConfigMapName}, last.ResourceNames)
	last.Verbs = []string{"get"}
	sa := serviceAccountClient(t, kube, ns, "patrol-server", rules)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := RunBootstrap(ctx, BootstrapConfig{Namespace: ns, Kube: sa, Logger: slog.New(slog.DiscardHandler)})
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err), err)
	_, has := getKnownHostsCM(t, kube, ns).Data[settings.SSHKnownHostsKey]
	assert.False(t, has)
}
