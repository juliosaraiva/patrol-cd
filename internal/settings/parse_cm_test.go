package settings

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
)

// fullConfigMap sets every patrol-cm key family to a valid value.
func fullConfigMap() map[string]string {
	return map[string]string{
		KeyURL:                       "https://cd.example.com",
		KeyAdditionalURLs:            "- https://cd.internal.example.com\n- https://cd.other.example.com\n",
		KeyStatusBadgeEnabled:        "true",
		KeyStatusBadgeURL:            "https://badges.example.com/",
		KeyAnonymousUserEnabled:      "true",
		KeyUserSessionDuration:       "12h",
		KeyPasswordPattern:           "^.{12,64}$",
		KeyAdminEnabled:              "false",
		"accounts.alice":             "apiKey, login",
		"accounts.alice.enabled":     "true",
		"accounts.bob":               "apiKey",
		KeyGATrackingID:              "UA-12345-1",
		KeyGAAnonymizeUsers:          "false",
		KeyHelpChatURL:               "https://chat.example.com/cd",
		KeyHelpChatText:              "Ask us",
		"help.download.linux-amd64":  "https://dl.example.com/patrol-linux-amd64",
		"help.download.darwin-arm64": "https://dl.example.com/patrol-darwin-arm64",
		KeyDexConfig:                 "connectors:\n- type: github\n  id: github\n  name: GitHub\n  config:\n    clientID: abc\n    clientSecret: $dex.github.clientSecret\n",
		KeyOIDCTLSInsecureSkip:       "false",
		"resource.customizations.health.cert-manager.io_Certificate":                                          "hs = {}\nhs.status = \"Healthy\"\nreturn hs\n",
		"resource.customizations.useOpenLibs.cert-manager.io_Certificate":                                     "false",
		"resource.customizations.actions.apps_Deployment":                                                     "discovery.lua: |\n  return {}\n",
		"resource.customizations.ignoreDifferences.admissionregistration.k8s.io_MutatingWebhookConfiguration": "jsonPointers:\n- /webhooks/0/clientConfig/caBundle\n",
		"resource.customizations.ignoreDifferences.all":                                                       "managedFieldsManagers:\n- kube-controller-manager\n",
		"resource.customizations.ignoreResourceUpdates.all":                                                   "jsonPointers:\n- /status\n",
		"resource.customizations.knownTypeFields.argoproj.io_Rollout":                                         "- field: spec.template.spec\n  type: core/v1/PodSpec\n",
		KeyResourceCustomizations:                                                                             "ConfigMap:\n  ignoreDifferences: |\n    jsonPointers:\n    - /data/legacy\n",
		KeyResourceExclusions:                                                                                 "- apiGroups:\n  - repositories.stash.appscode.com\n  kinds:\n  - Snapshot\n  clusters:\n  - '*.local'\n",
		KeyResourceInclusions:                                                                                 "- apiGroups:\n  - '*'\n  kinds:\n  - Deployment\n  clusters:\n  - https://kubernetes.default.svc\n",
		KeyResourceCompareOptions:                                                                             "ignoreAggregatedRoles: true\nignoreResourceStatusField: crd\nignoreDifferencesOnResourceUpdates: false\n",
		KeyResourceRespectRBAC:                                                                                "strict",
		KeyIgnoreResourceUpdates:                                                                              "false",
		KeySensitiveMaskAnnots:                                                                                "openshift.io/token-secret.value, api-key",
		KeyResourceCustomLabels:                                                                               "tier,team",
		KeyIncludeEventLabelKeys:                                                                              "team, env*",
		KeyExcludeEventLabelKeys:                                                                              "environment,bu",
		KeyKustomizeEnable:                                                                                    "true",
		KeyHelmEnable:                                                                                         "false",
		KeyJsonnetEnable:                                                                                      "true",
		KeyKustomizeBuildOptions:                                                                              "--load-restrictor LoadRestrictionsNone",
		"kustomize.path.v5.4.3":                                                                               "/custom-tools/kustomize_5_4_3",
		"kustomize.buildOptions.v5.4.3":                                                                       "--enable-helm",
		KeyHelmValuesFileSchemes:                                                                              "https, s3",
		KeyInstanceLabelKey:                                                                                   "cd.patrol.example/instance",
		KeyTrackingMethod:                                                                                     "annotation+label",
		KeyAllowedNodeLabels:                                                                                  "topology.kubernetes.io/zone,node.kubernetes.io/instance-type",
		KeyInstallationID:                                                                                     "prod-1",
		KeyUICSSURL:                                                                                           "./custom/my-styles.css",
		KeyUIBannerContent:                                                                                    "Maintenance tonight",
		KeyUIBannerURL:                                                                                        "https://status.example.com",
		KeyUIBannerPermanent:                                                                                  "true",
		KeyUIBannerPosition:                                                                                   "both",
		KeyReconciliationTimeout:                                                                              "180s",
		KeyReconciliationJitter:                                                                               "30s",
		KeyHardReconciliation:                                                                                 "1h",
		KeyWebhookMaxPayloadSizeMB:                                                                            "20",
		KeyWebhookRefreshJitter:                                                                               "1m",
		KeyWebhookJitterThreshold:                                                                             "5",
		KeyInClusterEnabled:                                                                                   "false",
		KeyMaxPodLogsToRender:                                                                                 "25",
		KeyExecEnabled:                                                                                        "true",
		KeyExecShells:                                                                                         "bash,sh",
		KeyDisableFineGrainedRBAC:                                                                             "false",
		KeyApplicationLinks:                                                                                   "- url: https://splunk.example.com?search={{.spec.destination.namespace}}\n  title: Splunk\n  if: spec.project == \"default\"\n",
		KeyResourceLinks:                                                                                      "- url: https://logs.example.com/{{.metadata.namespace}}\n  title: Logs\n  icon.class: fa-book\n",
		KeyProjectLinks:                                                                                       "- url: https://audit.example.com?project={{.metadata.name}}\n  title: Audit\n  description: audit logs\n",
		KeyExtensionConfig:                                                                                    "extensions:\n- name: metrics\n  backend:\n    connectionTimeout: 5s\n    services:\n    - url: http://metrics.example.com\n      headers:\n      - name: Authorization\n        value: $ext.metrics.token\n",
		"extension.config.rollouts":                                                                           "services:\n- url: http://rollouts-a.example.com\n  cluster:\n    name: a\n- url: http://rollouts-b.example.com\n  cluster:\n    server: https://b.example.com\n",
		KeyImpersonationEnabled:                                                                               "true",
		KeyImpersonationEnforced:                                                                              "false",
		KeyRequireOverrideRevision:                                                                            "true",
		KeyCommitAuthorName:                                                                                   "Patrol CD",
		KeyCommitAuthorEmail:                                                                                  "patrol@example.com",
		KeyCommitMessageTemplate:                                                                              "{{ .metadata.drySha | trunc 7 }}: {{ .metadata.subject }}",
		KeyReadmeMessageTemplate:                                                                              "# Hydrated from {{ .RepoURL }}",
		KeyGlobalProjects:                                                                                     "- projectName: proj-global\n  labelSelector:\n    matchExpressions:\n    - key: opt\n      operator: In\n      values: [\"yes\"]\n",
	}
}

func fullSecret() map[string]string {
	d := baseSecretData()
	d["dex.github.clientSecret"] = "dex-secret-value"
	d["ext.metrics.token"] = "  Bearer xyz  "
	d[SecretKeyWebhookGitHub] = "gh-secret"
	d[SecretKeyWebhookGitLab] = "$webhooks:gitlab"
	return d
}

func TestParseEveryConfigMapKeyFamily(t *testing.T) {
	t.Parallel()
	s := Parse(in(fullConfigMap(),
		newSecret(DefaultSecretName, fullSecret(), true),
		newSecret("webhooks", map[string]string{"gitlab": "gl-secret"}, true),
	))
	requireNoIssues(t, s, DefaultConfigMapName)
	requireNoIssues(t, s, DefaultSecretName)

	assert.Equal(t, "https://cd.example.com", s.URL())
	assert.Equal(t, []string{"https://cd.internal.example.com", "https://cd.other.example.com"}, s.AdditionalURLs())
	assert.Equal(t, StatusBadge{Enabled: true, RootURL: "https://badges.example.com/"}, s.StatusBadge())
	assert.True(t, s.AnonymousUserEnabled())
	assert.Equal(t, 12*time.Hour, s.UserSessionDuration())
	assert.Equal(t, "^.{12,64}$", s.PasswordPattern())

	accts := s.Accounts()
	assert.False(t, accts["admin"].Enabled)
	assert.True(t, accts["alice"].Enabled)
	assert.ElementsMatch(t, []string{"apiKey", "login"}, capabilities(accts["alice"].Capabilities))
	assert.ElementsMatch(t, []string{"apiKey"}, capabilities(accts["bob"].Capabilities))
	assert.False(t, s.UserLoginsDisabled(), "alice can log in")

	assert.Equal(t, GoogleAnalytics{TrackingID: "UA-12345-1", AnonymizeUsers: false}, s.GoogleAnalytics())
	assert.Equal(t, Help{ChatURL: "https://chat.example.com/cd", ChatText: "Ask us", BinaryURLs: map[string]string{
		"linux-amd64": "https://dl.example.com/patrol-linux-amd64", "darwin-arm64": "https://dl.example.com/patrol-darwin-arm64",
	}}, s.Help())

	assert.True(t, s.IsDexConfigured())
	assert.Equal(t, []DexConnector{{ID: "github", Name: "GitHub", Type: "github"}}, s.DexConnectors())
	dex, err := s.DexConfigResolved()
	require.NoError(t, err)
	conn := dex["connectors"].([]any)[0].(map[string]any)["config"].(map[string]any)
	assert.Equal(t, "dex-secret-value", conn["clientSecret"])
	assert.True(t, s.IsSSOConfigured())
	assert.False(t, s.OIDCTLSInsecureSkipVerify())

	ov := s.ResourceOverrides()
	require.Contains(t, ov, "cert-manager.io/Certificate")
	assert.Contains(t, ov["cert-manager.io/Certificate"].HealthLua, "Healthy")
	assert.Contains(t, ov["apps/Deployment"].Actions, "discovery.lua")
	assert.Contains(t, ov["*/*"].IgnoreResourceUpdates, "/status")
	assert.Contains(t, ov["ConfigMap"].IgnoreDifferences, "/data/legacy")
	assert.Len(t, ov["argoproj.io/Rollout"].KnownTypeFields, 1)

	assert.True(t, s.IsExcludedResource("repositories.stash.appscode.com", "Snapshot", "https://x.local"))
	assert.False(t, s.IsExcludedResource("repositories.stash.appscode.com", "Snapshot", "https://x.example.com"))
	assert.False(t, s.IsExcludedResource("apps", "Deployment", "https://kubernetes.default.svc"))
	assert.True(t, s.IsExcludedResource("", "Service", "https://kubernetes.default.svc"), "inclusions turn the cluster into allow-list mode")

	assert.Equal(t, diff.CompareOptions{IgnoreAggregatedRoles: true, IgnoreResourceStatusField: diff.IgnoreStatusCRD, IgnoreDifferencesOnResourceUpdates: false}, s.CompareOptions())
	assert.Equal(t, "strict", s.RespectRBAC())
	assert.Equal(t, cache.RespectRBACStrict, s.CacheRespectRBAC())
	assert.False(t, s.IgnoreResourceUpdatesEnabled())
	assert.Equal(t, []string{"openshift.io/token-secret.value", "api-key"}, s.SensitiveMaskAnnotations())
	assert.Equal(t, []string{"tier", "team"}, s.CustomLabels())
	assert.Equal(t, []string{"team", "env*"}, s.IncludeEventLabelKeys())
	assert.Equal(t, []string{"environment", "bu"}, s.ExcludeEventLabelKeys())

	assert.True(t, s.SourceTypeEnabled(v1alpha1.ApplicationSourceTypeKustomize))
	assert.False(t, s.SourceTypeEnabled(v1alpha1.ApplicationSourceTypeHelm))
	assert.True(t, s.SourceTypeEnabled(v1alpha1.ApplicationSourceTypeDirectory))
	assert.True(t, s.SourceTypeEnabled(v1alpha1.ApplicationSourceTypePlugin))
	assert.Equal(t, Kustomize{BuildOptions: "--load-restrictor LoadRestrictionsNone", Versions: []KustomizeVersion{{Name: "v5.4.3", Path: "/custom-tools/kustomize_5_4_3", BuildOptions: "--enable-helm"}}}, s.Kustomize())
	path, err := s.KustomizeBinaryPath("v5.4.3")
	require.NoError(t, err)
	assert.Equal(t, "/custom-tools/kustomize_5_4_3", path)
	_, err = s.KustomizeBinaryPath("v9")
	assert.EqualError(t, err, "kustomize version v9 is not registered")
	assert.Equal(t, []string{"https", "s3"}, s.HelmValuesFileSchemes())

	assert.Equal(t, "cd.patrol.example/instance", s.InstanceLabelKey())
	assert.Equal(t, v1alpha1.TrackingMethodAnnotationAndLabel, s.TrackingMethod())
	assert.Equal(t, []string{"topology.kubernetes.io/zone", "node.kubernetes.io/instance-type"}, s.AllowedNodeLabels())
	assert.Equal(t, "prod-1", s.InstallationID())

	assert.Equal(t, UI{CSSURL: "./custom/my-styles.css", BannerContent: "Maintenance tonight", BannerURL: "https://status.example.com", BannerPermanent: true, BannerPosition: "both"}, s.UI())
	assert.Equal(t, Reconciliation{Timeout: 180 * time.Second, TimeoutSet: true, Jitter: 30 * time.Second, HardTimeout: time.Hour}, s.Reconciliation())

	wh := s.Webhook()
	assert.Equal(t, int64(20*1024*1024), wh.MaxPayloadSize)
	assert.Equal(t, time.Minute, wh.RefreshJitter)
	assert.Equal(t, 5, wh.RefreshJitterThreshold)
	assert.Equal(t, "gh-secret", wh.GitHubSecret.Reveal())
	assert.Equal(t, "gl-secret", wh.GitLabSecret.Reveal(), "$<secret>:<key> reference resolved")

	assert.False(t, s.InClusterEnabled())
	assert.Equal(t, int64(25), s.MaxPodLogsToRender())
	assert.Equal(t, Exec{Enabled: true, Shells: []string{"bash", "sh"}}, s.Exec())
	assert.False(t, s.ApplicationFineGrainedRBACInheritanceDisabled())

	app := s.DeepLinks(KeyApplicationLinks)
	require.Len(t, app, 1)
	assert.Equal(t, "Splunk", app[0].Title)
	require.NotNil(t, app[0].Condition)
	assert.Equal(t, `spec.project == "default"`, *app[0].Condition)
	res := s.DeepLinks(KeyResourceLinks)
	require.Len(t, res, 1)
	assert.Equal(t, "fa-book", *res[0].IconClass)
	assert.Equal(t, "audit logs", *s.DeepLinks(KeyProjectLinks)[0].Description)

	exts := s.Extensions()
	require.Len(t, exts, 2)
	assert.Equal(t, "metrics", exts[0].Name)
	assert.Equal(t, 5*time.Second, exts[0].Backend.ConnectionTimeout)
	assert.Equal(t, DefaultExtensionKeepAlive, exts[0].Backend.KeepAlive)
	assert.Equal(t, "Bearer xyz", exts[0].Backend.Services[0].Headers[0].Value.Reveal(), "resolved and trimmed")
	assert.Equal(t, "rollouts", exts[1].Name)
	assert.Equal(t, "https://b.example.com", exts[1].Backend.Services[1].Cluster.Server)
	assert.Contains(t, s.ExtensionConfigs(), "")
	assert.Contains(t, s.ExtensionConfigs(), "rollouts")

	assert.Equal(t, Impersonation{Enabled: true, Enforced: false}, s.Impersonation())
	assert.True(t, s.RequireOverridePrivilegeForRevisionSync())
	assert.Equal(t, Hydrator{
		CommitAuthorName: "Patrol CD", CommitAuthorEmail: "patrol@example.com",
		CommitMessageTemplate: "{{ .metadata.drySha | trunc 7 }}: {{ .metadata.subject }}",
		ReadmeMessageTemplate: "# Hydrated from {{ .RepoURL }}",
	}, s.Hydrator())
	gps := s.GlobalProjects()
	require.Len(t, gps, 1)
	assert.Equal(t, "proj-global", gps[0].ProjectName)
	assert.True(t, gps[0].Selector.Matches(labelSet{"opt": "yes"}))
	assert.False(t, gps[0].Selector.Matches(labelSet{"opt": "no"}))
}

type labelSet map[string]string

func (l labelSet) Has(k string) bool              { _, ok := l[k]; return ok }
func (l labelSet) Get(k string) string            { return l[k] }
func (l labelSet) Lookup(k string) (string, bool) { v, ok := l[k]; return v, ok }

func capabilities[T ~string](cs []T) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}

func TestParseDefaults(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{}))
	requireNoIssues(t, s, DefaultConfigMapName)
	assert.Equal(t, "", s.URL())
	assert.Equal(t, DefaultUserSessionDuration, s.UserSessionDuration())
	assert.Equal(t, DefaultPasswordPattern, s.PasswordPattern())
	assert.True(t, s.Accounts()["admin"].Enabled)
	assert.True(t, s.GoogleAnalytics().AnonymizeUsers)
	assert.Equal(t, Help{BinaryURLs: map[string]string{}}, s.Help(), "no chat text without help.chatUrl")
	assert.False(t, s.IsSSOConfigured())
	assert.Equal(t, diff.DefaultCompareOptions(), s.CompareOptions())
	assert.Equal(t, cache.RespectRBACNone, s.CacheRespectRBAC())
	assert.True(t, s.IgnoreResourceUpdatesEnabled())
	assert.Equal(t, DefaultHelmValuesFileSchemes, s.HelmValuesFileSchemes())
	assert.Equal(t, v1alpha1.LabelKeyAppInstanceDefault, s.InstanceLabelKey())
	assert.Equal(t, v1alpha1.TrackingMethodAnnotation, s.TrackingMethod())
	assert.Equal(t, Reconciliation{Timeout: 120 * time.Second, Jitter: 60 * time.Second}, s.Reconciliation())
	assert.Equal(t, int64(50*1024*1024), s.Webhook().MaxPayloadSize)
	assert.Equal(t, 10, s.Webhook().RefreshJitterThreshold)
	assert.True(t, s.InClusterEnabled())
	assert.Equal(t, int64(10), s.MaxPodLogsToRender())
	assert.Equal(t, Exec{Shells: []string{"bash", "sh", "powershell", "cmd"}}, s.Exec())
	assert.True(t, s.ApplicationFineGrainedRBACInheritanceDisabled())
	assert.Equal(t, Impersonation{Enforced: true}, s.Impersonation())
	assert.False(t, s.RequireOverridePrivilegeForRevisionSync())
	assert.Equal(t, DefaultCommitMessageTemplate, s.Hydrator().CommitMessageTemplate)
	assert.Equal(t, DefaultReadmeMessageTemplate, s.Hydrator().ReadmeMessageTemplate)
	assert.Empty(t, s.GlobalProjects())
	assert.Empty(t, s.Extensions())
	assert.Empty(t, s.DeepLinks(KeyApplicationLinks))
	for _, typ := range []v1alpha1.ApplicationSourceType{v1alpha1.ApplicationSourceTypeHelm, v1alpha1.ApplicationSourceTypeKustomize, v1alpha1.ApplicationSourceTypeDirectory, v1alpha1.ApplicationSourceTypePlugin} {
		assert.True(t, s.SourceTypeEnabled(typ), typ)
	}
}

func TestParseHelpChatDefaults(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{KeyHelpChatURL: "https://chat"}))
	assert.Equal(t, "Chat now!", s.Help().ChatText, "default text when only the URL is set")
	s = Parse(in(map[string]string{KeyHelpChatText: "Hi"}))
	assert.Equal(t, "", s.Help().ChatText, "no text without URL (upstream)")
}

func TestParseHelmValuesFileSchemesEmptyDisablesRemote(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{KeyHelmValuesFileSchemes: ""}))
	assert.Empty(t, s.HelmValuesFileSchemes())
}

func TestParseInvalidValuesAreReportedWithKeyNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key, value string
		sev        Severity
		msg        string
		check      func(t *testing.T, s *Settings)
	}{
		{KeyURL, "cd.example.com", SeverityWarning, "Failed to validate URL in configmap: URL must include http or https protocol", func(t *testing.T, s *Settings) { assert.Equal(t, "cd.example.com", s.URL(), "kept, as upstream") }},
		{KeyAdditionalURLs, "{not a list", SeverityError, "Failed to decode all additional URLs", func(t *testing.T, s *Settings) { assert.Empty(t, s.AdditionalURLs()) }},
		{KeyAdditionalURLs, "- ftp://x", SeverityWarning, "Failed to validate external URL", nil},
		{KeyStatusBadgeEnabled, "yes", SeverityWarning, `value "yes" is treated as false`, func(t *testing.T, s *Settings) { assert.False(t, s.StatusBadge().Enabled) }},
		{KeyAnonymousUserEnabled, "True", SeverityWarning, `treated as false`, func(t *testing.T, s *Settings) { assert.False(t, s.AnonymousUserEnabled()) }},
		{KeyUserSessionDuration, "forever", SeverityError, "Failed to parse 'users.session.duration' key", func(t *testing.T, s *Settings) { assert.Equal(t, 24*time.Hour, s.UserSessionDuration()) }},
		{KeyPasswordPattern, "([", SeverityError, "failed to compile password regex", func(t *testing.T, s *Settings) { assert.Equal(t, DefaultPasswordPattern, s.PasswordPattern()) }},
		{KeyGAAnonymizeUsers, "no", SeverityWarning, `treated as true; only "false" disables it`, func(t *testing.T, s *Settings) { assert.True(t, s.GoogleAnalytics().AnonymizeUsers) }},
		{"help.download.plan9-386", "x", SeverityWarning, `unknown platform "plan9-386"`, nil},
		{KeyDexConfig, "connectors: [", SeverityError, "invalid dex.config", func(t *testing.T, s *Settings) { assert.False(t, s.IsDexConfigured()) }},
		{KeyDexConfig, "connectors: []", SeverityWarning, "url is empty", func(t *testing.T, s *Settings) { assert.False(t, s.IsDexConfigured()) }},
		{KeyOIDCConfig, "issuer: [", SeverityError, "invalid oidc config", func(t *testing.T, s *Settings) { assert.Nil(t, s.OIDCConfig()) }},
		{KeyResourceExclusions, "- apiGroups: x", SeverityError, "error unmarshalling excluded resources", nil},
		{KeyResourceInclusions, "kinds: [a]", SeverityError, "error unmarshalling included resources", nil},
		{KeyResourceExclusions, "- apiGroups: ['[']", SeverityWarning, "invalid glob pattern", nil},
		{KeyResourceCompareOptions, "ignoreAggregatedRoles: [", SeverityError, "Could not get compare options", nil},
		{KeyResourceCompareOptions, "ignoreResourceStatusField: maybe", SeverityWarning, "Unrecognized value for ignoreResourceStatusField - maybe, ignore status for all resources", func(t *testing.T, s *Settings) {
			assert.Equal(t, diff.IgnoreStatusAll, s.CompareOptions().IgnoreResourceStatusField)
		}},
		{KeyResourceRespectRBAC, "always", SeverityError, "invalid value for resource.respectRBAC: always", func(t *testing.T, s *Settings) { assert.Equal(t, "", s.RespectRBAC()) }},
		{KeyIgnoreResourceUpdates, "nope", SeverityError, "error parsing resource.ignoreResourceUpdatesEnabled", func(t *testing.T, s *Settings) { assert.True(t, s.IgnoreResourceUpdatesEnabled()) }},
		{KeyResourceCustomLabels, "a, b", SeverityWarning, `item " b" contains surrounding whitespace`, func(t *testing.T, s *Settings) { assert.Equal(t, []string{"a", " b"}, s.CustomLabels()) }},
		{KeyIncludeEventLabelKeys, "[", SeverityWarning, "invalid glob pattern", nil},
		{KeyHelmEnable, "no", SeverityWarning, "treated as false", func(t *testing.T, s *Settings) {
			assert.False(t, s.SourceTypeEnabled(v1alpha1.ApplicationSourceTypeHelm))
		}},
		{"kustomize.version.v1", "/bin/k1", SeverityWarning, "deprecated", func(t *testing.T, s *Settings) { assert.Len(t, s.Kustomize().Versions, 1) }},
		{"kustomize.buildOptions.v7", "--x", SeverityWarning, `kustomize version "v7" are ignored`, nil},
		{KeyInstanceLabelKey, "bad key!", SeverityError, "invalid label key", func(t *testing.T, s *Settings) {
			assert.Equal(t, v1alpha1.LabelKeyAppInstanceDefault, s.InstanceLabelKey())
		}},
		{KeyTrackingMethod, "magic", SeverityError, `invalid tracking method "magic"`, func(t *testing.T, s *Settings) {
			assert.Equal(t, v1alpha1.TrackingMethodAnnotation, s.TrackingMethod())
		}},
		{KeyAllowedNodeLabels, "good, bad/label/x", SeverityWarning, "Invalid node label key 'bad/label/x'", func(t *testing.T, s *Settings) { assert.Equal(t, []string{"good"}, s.AllowedNodeLabels()) }},
		{KeyUIBannerURL, "status", SeverityWarning, "Failed to validate UI banner URL", nil},
		{KeyUIBannerPosition, "left", SeverityWarning, `unknown banner position "left"`, nil},
		{KeyReconciliationTimeout, "soon", SeverityError, "invalid duration", func(t *testing.T, s *Settings) {
			assert.Equal(t, DefaultReconciliationTimeout, s.Reconciliation().Timeout)
		}},
		{KeyReconciliationJitter, "-5s", SeverityError, "must not be negative", nil},
		{KeyHardReconciliation, "x", SeverityError, "invalid duration", nil},
		{KeyWebhookMaxPayloadSizeMB, "fifty", SeverityError, "Failed to parse 'webhook.maxPayloadSizeMB' key", func(t *testing.T, s *Settings) { assert.Equal(t, int64(50*1024*1024), s.Webhook().MaxPayloadSize) }},
		{KeyWebhookRefreshJitter, "abc", SeverityError, "Failed to parse 'webhook.refresh.jitter' key", nil},
		{KeyWebhookJitterThreshold, "-1", SeverityError, "Invalid 'webhook.refresh.jitter.threshold' value -1, must be >= 0", func(t *testing.T, s *Settings) { assert.Equal(t, 10, s.Webhook().RefreshJitterThreshold) }},
		{KeyInClusterEnabled, "no", SeverityWarning, "treated as true", func(t *testing.T, s *Settings) {
			assert.True(t, s.InClusterEnabled(), `"no" still means enabled (REPO-093)`)
		}},
		{KeyMaxPodLogsToRender, "ten", SeverityError, "Failed to parse 'server.maxPodLogsToRender' key", func(t *testing.T, s *Settings) { assert.Equal(t, int64(10), s.MaxPodLogsToRender()) }},
		{KeyExecShells, "bash, sh", SeverityWarning, "surrounding whitespace", nil},
		{KeyDisableFineGrainedRBAC, "sometimes", SeverityError, "error parsing server.rbac.disableApplicationFineGrainedRBACInheritance", func(t *testing.T, s *Settings) { assert.True(t, s.ApplicationFineGrainedRBACInheritanceDisabled()) }},
		{KeyRBACLogEnforceEnable, "true", SeverityWarning, "removed in Argo CD 3.0", nil},
		{KeyApplicationLinks, "- url: [", SeverityError, "error unmarshalling deep links", nil},
		{KeyApplicationLinks, "- url: 'https://x/{{ env \"HOME\" }}'\n  title: x", SeverityWarning, "url template does not parse", nil},
		{KeyResourceLinks, "- url: https://x\n  title: x\n  if: 'kind =='", SeverityWarning, "condition does not compile", nil},
		{KeyExtensionConfig, "extensions:\n- name: bad name\n  backend:\n    services:\n    - url: http://x", SeverityError, "invalid extensions.name: only alphanumeric characters, hyphens, and underscores are allowed", func(t *testing.T, s *Settings) { assert.Empty(t, s.Extensions()) }},
		{KeyExtensionConfig, "extensions:\n- name: a\n  backend:\n    services:\n    - url: http://x\n    - url: http://y", SeverityError, "extensions.backend.services.cluster must be configured when defining more than one service per extension", nil},
		{KeyExtensionConfig, "extensions:\n- name: a\n  backend:\n    services:\n    - cluster:\n        name: c", SeverityError, "extensions.backend.services.url must be configured", nil},
		{KeyExtensionConfig, "extensions:\n- name: a\n  backend:\n    services: []", SeverityError, "no backend service configured for extension a", nil},
		{KeyExtensionConfig, "extensions:\n- name: a\n  backend:\n    services:\n    - url: http://x\n      headers:\n      - name: X", SeverityError, "header.value must be defined", nil},
		{KeyImpersonationEnforced, "maybe", SeverityWarning, "treated as true", func(t *testing.T, s *Settings) { assert.True(t, s.Impersonation().Enforced) }},
		{KeyRequireOverrideRevision, "yes", SeverityError, "error parsing application.sync.requireOverridePrivilegeForRevisionSync value: strconv.ParseBool: parsing \"yes\": invalid syntax, expected true or false", nil},
		{KeyCommitAuthorEmail, "patrol", SeverityWarning, "does not look like an email address", nil},
		{KeyCommitMessageTemplate, "{{ .x ", SeverityError, "invalid commit message template", func(t *testing.T, s *Settings) {
			assert.Equal(t, DefaultCommitMessageTemplate, s.Hydrator().CommitMessageTemplate)
		}},
		{KeyGlobalProjects, "- projectName: [", SeverityError, "error unmarshalling global project settings", nil},
		{KeyLegacyRepositories, "- url: https://github.com/x/y", SeverityWarning, "removed in Argo CD 2.1; use Repository objects", nil},
		{KeyLegacyRepoCredentials, "- url: https://github.com", SeverityWarning, "use RepositoryCredential objects", nil},
		{"users.passwordPattern", ".*", SeverityWarning, `did you mean "passwordPattern"?`, nil},
		{"controller.status.processors", "5", SeverityWarning, `is a patrol-cmd-params-cm key`, nil},
		{"completely.unknown", "x", SeverityWarning, "unknown key is ignored", nil},
		{"resource.customizations.health", "x", SeverityWarning, "expected resource.customizations.<type>.<group_kind>", nil},
		{"resource.customizations.bogus.apps_Deployment", "x", SeverityError, "resource customization type bogus not supported", nil},
		{"resource.customizations.useOpenLibs.apps_Deployment", "perhaps", SeverityError, "invalid resource customization", nil},
		{"resource.customizations.knownTypeFields.apps_Foo", "- field: spec\n  type: core/v1/Nope", SeverityWarning, `unsupported type "core/v1/Nope"`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Parallel()
			s := Parse(in(map[string]string{tc.key: tc.value}))
			key := tc.key
			if slices.Contains([]string{"resource.customizations.knownTypeFields.apps_Foo"}, key) {
				key = KeyResourceCustomizations
			}
			requireIssue(t, s, DefaultConfigMapName, key, tc.sev, tc.msg)
			if tc.check != nil {
				tc.check(t, s)
			}
		})
	}
}

func TestParseKustomizeDuplicateVersion(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{
		"kustomize.path.v3.5.4":    "/tools/a",
		"kustomize.version.v3.5.4": "/tools/b",
	}))
	requireIssue(t, s, DefaultConfigMapName, "kustomize.version.v3.5.4", SeverityError, `failed to add kustomize version from "kustomize.version.v3.5.4": found duplicate kustomize version: v3.5.4`)
	assert.Equal(t, []KustomizeVersion{{Name: "v3.5.4", Path: "/tools/a"}}, s.Kustomize().Versions)
}

func TestParseExtensionsDuplicateAcrossKeys(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{
		KeyExtensionConfig:     "extensions:\n- name: dup\n  backend:\n    services:\n    - url: http://a\n",
		"extension.config.dup": "services:\n- url: http://b\n",
		"extension.config.ok":  "services:\n- url: http://c\n",
	}))
	requireIssue(t, s, DefaultConfigMapName, "extension.config.dup", SeverityError, `duplicated extension found in the configs for "dup"`)
	names := []string{}
	for _, e := range s.Extensions() {
		names = append(names, e.Name)
	}
	assert.Equal(t, []string{"dup", "ok"}, names, "the valid keys stay usable")
}

func TestParseGlobalProjectsStopAtInvalidSelector(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{KeyGlobalProjects: `
- projectName: a
  labelSelector:
    matchLabels: {x: "1"}
- projectName: b
  labelSelector:
    matchExpressions:
    - {key: x, operator: Bogus}
- projectName: c
  labelSelector:
    matchLabels: {y: "1"}
`}))
	requireIssue(t, s, DefaultConfigMapName, KeyGlobalProjects, SeverityError, `entry 1 ("b"): invalid labelSelector`)
	gps := s.GlobalProjects()
	require.Len(t, gps, 1, "later entries are dropped, as upstream")
	assert.Equal(t, "a", gps[0].ProjectName)
}

func TestParseCompareOptionsYAMLOff(t *testing.T) {
	t.Parallel()
	// Unquoted YAML off decodes as boolean false and is accepted as "none" (DIFF-025).
	s := Parse(in(map[string]string{KeyResourceCompareOptions: "ignoreResourceStatusField: off"}))
	requireNoIssues(t, s, DefaultConfigMapName)
	assert.Equal(t, diff.IgnoreStatusNone, s.CompareOptions().IgnoreResourceStatusField)
}

func TestSystemIgnoreAndResourceUpdateRules(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{
		"resource.customizations.ignoreDifferences.apps_Deployment":   "jsonPointers:\n- /spec/replicas\n",
		"resource.customizations.ignoreResourceUpdates.batch_Job":     "jqPathExpressions:\n- .status\n",
		"resource.customizations.knownTypeFields.argoproj.io_Rollout": "- field: spec.template.spec\n  type: core/v1/PodSpec\n",
	}))
	requireNoIssues(t, s, DefaultConfigMapName)
	sys := s.SystemIgnoreRules()
	require.Len(t, sys, 2)
	assert.Equal(t, diff.IgnoreRule{Group: "apps", Kind: "Deployment", JSONPointers: []string{"/spec/replicas"}}, sys[0])
	assert.Equal(t, []string{"/status"}, sys[1].JSONPointers, "ignoreResourceStatusField=all by default")

	upd := s.ResourceUpdateIgnoreRules()
	// batch/Job update rule; with ignoreDifferencesOnResourceUpdates (default true) the
	// apps/Deployment diff rule and the /status rule; always the metadata noise rule (OPS-063).
	require.Len(t, upd, 4)
	assert.Equal(t, "batch", upd[0].Group)
	assert.Equal(t, "apps", upd[1].Group)
	assert.Equal(t, []string{"/status"}, upd[2].JSONPointers)
	assert.Equal(t, []string{"/metadata/resourceVersion", "/metadata/generation", "/metadata/managedFields"}, upd[3].JSONPointers)

	off := Parse(in(map[string]string{
		"resource.customizations.ignoreDifferences.apps_Deployment": "jsonPointers:\n- /spec/replicas\n",
		KeyResourceCompareOptions:                                   "ignoreDifferencesOnResourceUpdates: false",
	}))
	require.Len(t, off.ResourceUpdateIgnoreRules(), 1, "only the metadata noise rule")

	ktf := s.KnownTypeFields()
	require.Len(t, ktf, 1)
	for gk, fields := range ktf {
		assert.Equal(t, "argoproj.io", gk.Group)
		assert.Equal(t, []diff.KnownTypeField{{Field: "spec.template.spec", Type: "core/v1/PodSpec"}}, fields)
	}
}

func TestParseMissingConfigMapAndLabel(t *testing.T) {
	t.Parallel()
	s := Parse(in(nil))
	requireIssue(t, s, DefaultConfigMapName, "", SeverityError, "ConfigMap not found; local accounts are disabled and every other key uses its default")

	unlabelled := newCM(DefaultConfigMapName, map[string]string{KeyURL: "https://x"})
	unlabelled.Labels = nil
	s = Parse(in(nil, unlabelled))
	requireIssue(t, s, DefaultConfigMapName, "", SeverityError, "ConfigMap is ignored because it is not labelled app.kubernetes.io/part-of=patrol-cd; local accounts are disabled")
	assert.Equal(t, "", s.URL(), "an unlabelled patrol-cm is ignored (OPS-012)")
	assert.Equal(t, SourceStateAbsent, s.SourceState(SourceConfigMap))

	// The label requirement of the TLS, SSH and GPG ConfigMaps is reported as before.
	tlsCM := newCM(DefaultTLSCertsConfigMapName, map[string]string{})
	tlsCM.Labels = nil
	s = Parse(in(map[string]string{}, tlsCM))
	requireIssue(t, s, DefaultTLSCertsConfigMapName, "", SeverityError, "ConfigMap is ignored because it is not labelled app.kubernetes.io/part-of=patrol-cd")

	input := in(nil, unlabelled)
	input.IgnorePartOfLabel = true
	s = Parse(input)
	assert.Equal(t, "https://x", s.URL())
}

func TestSnapshotIsImmutable(t *testing.T) {
	t.Parallel()
	s := Parse(in(fullConfigMap(), newSecret(DefaultSecretName, fullSecret(), true), newSecret("webhooks", map[string]string{"gitlab": "x"}, true)))
	urls := s.AdditionalURLs()
	urls[0] = "mutated"
	assert.NotEqual(t, "mutated", s.AdditionalURLs()[0])
	h := s.Help()
	h.BinaryURLs["linux-amd64"] = "mutated"
	assert.NotEqual(t, "mutated", s.Help().BinaryURLs["linux-amd64"])
	links := s.DeepLinks(KeyApplicationLinks)
	*links[0].Condition = "mutated"
	assert.NotEqual(t, "mutated", *s.DeepLinks(KeyApplicationLinks)[0].Condition)
	acc := s.Accounts()
	acc["alice"].Capabilities[0] = "mutated"
	assert.NotContains(t, capabilities(s.Accounts()["alice"].Capabilities), "mutated")
	ov := s.ResourceOverrides()
	delete(ov, "apps/Deployment")
	assert.Contains(t, s.ResourceOverrides(), "apps/Deployment")
}
