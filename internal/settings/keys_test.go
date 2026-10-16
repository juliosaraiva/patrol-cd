package settings

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// upstreamConfigMapKeys are argocd-cm keys read by Argo CD v3.5.5 (reference document,
// shipped manifest, util/settings code), with placeholders instantiated.
var upstreamConfigMapKeys = []string{
	"url", "additionalUrls", "statusbadge.enabled", "statusbadge.url",
	"users.anonymous.enabled", "users.session.duration", "passwordPattern", "admin.enabled",
	"accounts.alice", "accounts.alice.enabled",
	"ga.trackingid", "ga.anonymizeusers", "help.chatUrl", "help.chatText",
	"help.download.linux-amd64", "help.download.linux-arm64", "help.download.linux-ppc64le",
	"help.download.linux-s390x", "help.download.darwin-amd64", "help.download.darwin-arm64",
	"help.download.windows-amd64",
	"dex.config", "oidc.config", "oidc.tls.insecure.skip.verify",
	"resource.customizations",
	"resource.customizations.health.cert-manager.io_Certificate",
	"resource.customizations.useOpenLibs.cert-manager.io_Certificate",
	"resource.customizations.actions.apps_Deployment",
	"resource.customizations.ignoreDifferences.all",
	"resource.customizations.ignoreDifferences.admissionregistration.k8s.io_MutatingWebhookConfiguration",
	"resource.customizations.ignoreResourceUpdates.all",
	"resource.customizations.ignoreResourceUpdates.ConfigMap",
	"resource.customizations.knownTypeFields.argoproj.io_Rollout",
	"resource.exclusions", "resource.inclusions", "resource.compareoptions", "resource.respectRBAC",
	"resource.ignoreResourceUpdatesEnabled", "resource.sensitive.mask.annotations",
	"resource.customLabels", "resource.includeEventLabelKeys", "resource.excludeEventLabelKeys",
	"kustomize.enable", "jsonnet.enable", "helm.enable", "kustomize.buildOptions",
	"kustomize.path.v3.9.1", "kustomize.buildOptions.v3.9.1", "kustomize.version.v3.5.1",
	"helm.valuesFileSchemes",
	"application.instanceLabelKey", "application.allowedNodeLabels", "application.resourceTrackingMethod",
	"installationID",
	"ui.cssurl", "ui.bannercontent", "ui.bannerurl", "ui.bannerpermanent", "ui.bannerposition",
	"timeout.reconciliation", "timeout.reconciliation.jitter", "timeout.hard.reconciliation",
	"webhook.refresh.jitter", "webhook.refresh.jitter.threshold", "webhook.maxPayloadSizeMB",
	"cluster.inClusterEnabled", "server.maxPodLogsToRender", "exec.enabled", "exec.shells",
	"project.links", "application.links", "resource.links",
	"extension.config", "extension.config.some-extension",
	"application.sync.impersonation.enabled", "application.sync.impersonation.enforced",
	"application.sync.requireOverridePrivilegeForRevisionSync",
	"commit.author.name", "commit.author.email",
	"sourceHydrator.commitMessageTemplate", "sourceHydrator.readmeMessageTemplate",
	"globalProjects", "server.rbac.disableApplicationFineGrainedRBACInheritance",
}

// TestConfigMapKeyRegister: every upstream argocd-cm key is accepted and interpreted by the
// settings model (OPS-029); none is reported as unknown or left uninterpreted.
func TestConfigMapKeyRegister(t *testing.T) {
	t.Parallel()
	data := map[string]string{}
	for _, k := range upstreamConfigMapKeys {
		spec, ok := LookupKey(SourceConfigMap, k)
		require.True(t, ok, k)
		assert.Empty(t, spec.UpstreamKey, "no argocd-cm key contains the product name, so none is renamed: %s", k)
		data[k] = ""
	}
	s := Parse(in(data))
	for _, is := range s.Validate() {
		assert.NotContains(t, is.Message, "unknown key", is.String())
		assert.NotContains(t, is.Message, "not interpreted", is.String())
	}
}

func TestSecretAndRBACKeyRegister(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"server.secretkey", "tls.crt", "tls.key", "admin.password", "admin.passwordMtime", "admin.tokens",
		"accounts.alice.password", "accounts.alice.passwordMtime", "accounts.alice.tokens",
		"webhook.github.secret", "webhook.gitlab.secret", "webhook.bitbucket.uuid", "webhook.bitbucketserver.secret",
		"webhook.gogs.secret", "webhook.azuredevops.username", "webhook.azuredevops.password", "oidc.okta.clientSecret"} {
		_, ok := LookupKey(SourceSecret, k)
		assert.True(t, ok, k)
	}
	for _, k := range []string{"policy.csv", "policy.overlay.csv", "policy.default", "scopes", "policy.matchMode"} {
		_, ok := LookupKey(SourceRBACConfigMap, k)
		assert.True(t, ok, k)
	}
	exact, ok := LookupKey(SourceSecret, "server.secretkey")
	require.True(t, ok)
	assert.Equal(t, "server.secretkey", exact.Key, "exact keys win over the catch-all pattern")
}

func TestAcceptedKeysSortedAndDescribed(t *testing.T) {
	t.Parallel()
	keys := AcceptedKeys()
	require.NotEmpty(t, keys)
	for i, k := range keys {
		assert.NotEmpty(t, k.Description, k.Key)
		if i > 0 {
			prev := keys[i-1]
			assert.True(t, prev.Source < k.Source || (prev.Source == k.Source && prev.Key <= k.Key), "%v before %v", prev, k)
		}
	}
	_, ok := LookupKey(SourceConfigMap, "help.download.plan9-386")
	assert.False(t, ok)
	_, ok = LookupKey(SourceConfigMap, "resource.customizations.bogus.apps_Deployment")
	assert.False(t, ok)
}

func TestNamesValidate(t *testing.T) {
	t.Parallel()
	require.NoError(t, Names{}.Validate())
	err := Names{ConfigMap: "same", RBACConfigMap: "same"}.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "use the same object name")
	err = Names{Secret: "Not_Valid"}.Validate()
	require.Error(t, err)
	assert.Equal(t, "custom-cm", Names{ConfigMap: "custom-cm"}.WithDefaults().ConfigMap)
	assert.Equal(t, DefaultSecretName, Names{}.Name(SourceSecret))
	assert.Equal(t, "", Names{}.Name(SourceReferencedSecret))
	for _, src := range append(append([]Source{}, configMapSources...), secretSources...) {
		assert.True(t, strings.HasPrefix(DefaultNames().Name(src), "patrol-"), src)
	}
}

func TestCustomNames(t *testing.T) {
	t.Parallel()
	names := Names{ConfigMap: "my-cm", Secret: "my-secret"}
	s := Parse(Input{
		Namespace: testNamespace,
		Names:     names,
		ConfigMaps: []*corev1.ConfigMap{
			newCM("my-cm", map[string]string{KeyURL: "https://custom"}),
			newCM(DefaultConfigMapName, map[string]string{KeyURL: "https://ignored"}),
		},
		Secrets: []*corev1.Secret{newSecret("my-secret", baseSecretData(), false)},
	})
	assert.Equal(t, "https://custom", s.URL())
	assert.Equal(t, testServerKey, s.ServerSecretKey().Reveal())
	assert.Equal(t, "my-cm", s.Names().ConfigMap)
	assert.Equal(t, map[string]string{"configmap/my-cm": "1", "secret/my-secret": "1"}, s.ResourceVersions())
}
