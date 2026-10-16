package settings

import (
	"regexp"
	"slices"
	"strings"
)

// Keys of patrol-cm. Every key has the Argo CD argocd-cm name; none contains the "argocd"
// product name, so none is renamed.
const (
	KeyURL                     = "url"
	KeyAdditionalURLs          = "additionalUrls"
	KeyStatusBadgeEnabled      = "statusbadge.enabled"
	KeyStatusBadgeURL          = "statusbadge.url"
	KeyAnonymousUserEnabled    = "users.anonymous.enabled"
	KeyUserSessionDuration     = "users.session.duration"
	KeyPasswordPattern         = "passwordPattern"
	KeyAdminEnabled            = "admin.enabled"
	KeyAccountsPrefix          = "accounts."
	KeyGATrackingID            = "ga.trackingid"
	KeyGAAnonymizeUsers        = "ga.anonymizeusers"
	KeyHelpChatURL             = "help.chatUrl"
	KeyHelpChatText            = "help.chatText"
	KeyHelpDownloadPrefix      = "help.download."
	KeyDexConfig               = "dex.config"
	KeyOIDCConfig              = "oidc.config"
	KeyOIDCTLSInsecureSkip     = "oidc.tls.insecure.skip.verify"
	KeyResourceCustomizations  = "resource.customizations"
	KeyResourceExclusions      = "resource.exclusions"
	KeyResourceInclusions      = "resource.inclusions"
	KeyResourceCompareOptions  = "resource.compareoptions"
	KeyResourceRespectRBAC     = "resource.respectRBAC"
	KeyIgnoreResourceUpdates   = "resource.ignoreResourceUpdatesEnabled"
	KeySensitiveMaskAnnots     = "resource.sensitive.mask.annotations"
	KeyResourceCustomLabels    = "resource.customLabels"
	KeyIncludeEventLabelKeys   = "resource.includeEventLabelKeys"
	KeyExcludeEventLabelKeys   = "resource.excludeEventLabelKeys"
	KeyKustomizeEnable         = "kustomize.enable"
	KeyHelmEnable              = "helm.enable"
	KeyJsonnetEnable           = "jsonnet.enable"
	KeyKustomizeBuildOptions   = "kustomize.buildOptions"
	KeyKustomizePathPrefix     = "kustomize.path."
	KeyKustomizeVersionPrefix  = "kustomize.version."
	KeyHelmValuesFileSchemes   = "helm.valuesFileSchemes"
	KeyInstanceLabelKey        = "application.instanceLabelKey"
	KeyTrackingMethod          = "application.resourceTrackingMethod"
	KeyAllowedNodeLabels       = "application.allowedNodeLabels"
	KeyInstallationID          = "installationID"
	KeyUICSSURL                = "ui.cssurl"
	KeyUIBannerContent         = "ui.bannercontent"
	KeyUIBannerURL             = "ui.bannerurl"
	KeyUIBannerPermanent       = "ui.bannerpermanent"
	KeyUIBannerPosition        = "ui.bannerposition"
	KeyReconciliationTimeout   = "timeout.reconciliation"
	KeyReconciliationJitter    = "timeout.reconciliation.jitter"
	KeyHardReconciliation      = "timeout.hard.reconciliation"
	KeyWebhookMaxPayloadSizeMB = "webhook.maxPayloadSizeMB"
	KeyWebhookRefreshJitter    = "webhook.refresh.jitter"
	KeyWebhookJitterThreshold  = "webhook.refresh.jitter.threshold"
	KeyInClusterEnabled        = "cluster.inClusterEnabled"
	KeyMaxPodLogsToRender      = "server.maxPodLogsToRender"
	KeyExecEnabled             = "exec.enabled"
	KeyExecShells              = "exec.shells"
	KeyDisableFineGrainedRBAC  = "server.rbac.disableApplicationFineGrainedRBACInheritance"
	KeyRBACLogEnforceEnable    = "server.rbac.log.enforce.enable"
	KeyApplicationLinks        = "application.links"
	KeyResourceLinks           = "resource.links"
	KeyProjectLinks            = "project.links"
	KeyExtensionConfig         = "extension.config"
	KeyImpersonationEnabled    = "application.sync.impersonation.enabled"
	KeyImpersonationEnforced   = "application.sync.impersonation.enforced"
	KeyRequireOverrideRevision = "application.sync.requireOverridePrivilegeForRevisionSync"
	KeyCommitAuthorName        = "commit.author.name"
	KeyCommitAuthorEmail       = "commit.author.email"
	KeyCommitMessageTemplate   = "sourceHydrator.commitMessageTemplate"
	KeyReadmeMessageTemplate   = "sourceHydrator.readmeMessageTemplate"
	KeyGlobalProjects          = "globalProjects"
	KeyLegacyRepositories      = "repositories"
	KeyLegacyRepoCredentials   = "repository.credentials"
	KeyLegacyHelmRepositories  = "helm.repositories"
	KeyLegacyCMPs              = "configManagementPlugins"
)

// Keys of patrol-secret.
const (
	SecretKeyServerSecretKey         = "server.secretkey"
	SecretKeyServerSecretKeyPrevious = "server.secretkey.previous"
	SecretKeyTLSCert                 = "tls.crt"
	SecretKeyTLSKey                  = "tls.key"
	SecretKeyAdminPassword           = "admin.password"
	SecretKeyAdminPasswordMtime      = "admin.passwordMtime"
	SecretKeyAdminTokens             = "admin.tokens"
	SecretKeyWebhookGitHub           = "webhook.github.secret"
	SecretKeyWebhookGitLab           = "webhook.gitlab.secret"
	SecretKeyWebhookBitbucketUUID    = "webhook.bitbucket.uuid"
	SecretKeyWebhookBitbucketServer  = "webhook.bitbucketserver.secret"
	SecretKeyWebhookGogs             = "webhook.gogs.secret"
	SecretKeyWebhookAzureDevOpsUser  = "webhook.azuredevops.username"
	SecretKeyWebhookAzureDevOpsPass  = "webhook.azuredevops.password"
)

// Keys of patrol-rbac-cm (the same constants as internal/rbac).
const (
	RBACKeyPolicyCSV     = "policy.csv"
	RBACKeyPolicyDefault = "policy.default"
	RBACKeyMatchMode     = "policy.matchMode"
	RBACKeyScopes        = "scopes"
)

// SSHKnownHostsKey is the only key of patrol-ssh-known-hosts-cm.
const SSHKnownHostsKey = "ssh_known_hosts"

// helpDownloadPlatforms are the help.download.<os>-<arch> suffixes upstream reads.
var helpDownloadPlatforms = []string{"darwin-amd64", "darwin-arm64", "windows-amd64", "linux-amd64", "linux-arm64", "linux-ppc64le", "linux-s390x"}

// customizationTypes are the <type> segments of resource.customizations.<type>.<group_kind>.
var customizationTypes = []string{"health", "useOpenLibs", "actions", "ignoreDifferences", "ignoreResourceUpdates", "knownTypeFields"}

// KeySpec describes one accepted key or key pattern, for "patrol admin settings validate"
// and documentation.
type KeySpec struct {
	// Source is the settings object holding the key.
	Source Source `json:"source"`
	// Key is the key, or a pattern with <placeholders> (for example "accounts.<name>").
	Key string `json:"key"`
	// Description is a one-line summary.
	Description string `json:"description"`
	// Deprecated is non-empty when the key is deprecated or has no effect, and says why.
	Deprecated string `json:"deprecated,omitempty"`
	// UpstreamKey is the Argo CD key when it differs from Key ("" when identical).
	UpstreamKey string `json:"upstreamKey,omitempty"`

	re *regexp.Regexp
}

// Matches reports whether key matches the spec.
func (k KeySpec) Matches(key string) bool {
	if k.re != nil {
		return k.re.MatchString(key)
	}
	return k.Key == key
}

func exact(src Source, key, desc string) KeySpec {
	return KeySpec{Source: src, Key: key, Description: desc}
}

func deprecated(src Source, key, desc, why string) KeySpec {
	return KeySpec{Source: src, Key: key, Description: desc, Deprecated: why}
}

func pattern(src Source, key, re, desc string) KeySpec {
	return KeySpec{Source: src, Key: key, Description: desc, re: regexp.MustCompile(re)}
}

var acceptedKeys = buildAcceptedKeys()

func buildAcceptedKeys() []KeySpec {
	cm := SourceConfigMap
	types := strings.Join(customizationTypes, "|")
	platforms := strings.Join(helpDownloadPlatforms, "|")
	specs := []KeySpec{
		exact(cm, KeyURL, "external URL of the API server and UI; required for SSO"),
		exact(cm, KeyAdditionalURLs, "YAML list of additional external URLs (SSO redirects)"),
		exact(cm, KeyStatusBadgeEnabled, `"true" enables the status badge endpoint`),
		exact(cm, KeyStatusBadgeURL, "root URL override for status badge links"),
		exact(cm, KeyAnonymousUserEnabled, `"true" lets unauthenticated users act with policy.default`),
		exact(cm, KeyUserSessionDuration, "login session lifetime (default 24h)"),
		exact(cm, KeyPasswordPattern, "regular expression new local passwords must match (default ^.{8,32}$)"),
		exact(cm, KeyAdminEnabled, `"false" disables the built-in admin account`),
		pattern(cm, "accounts.<name>", `^accounts\.[^.]+$`, "local account capabilities (login, apiKey)"),
		pattern(cm, "accounts.<name>.enabled", `^accounts\.[^.]+\.enabled$`, `"false" disables a local account`),
		exact(cm, KeyGATrackingID, "Google Analytics tracking ID used by the UI"),
		exact(cm, KeyGAAnonymizeUsers, `user IDs are hashed unless "false"`),
		exact(cm, KeyHelpChatURL, "URL of the UI help chat button"),
		exact(cm, KeyHelpChatText, `text of the UI help chat button (default "Chat now!")`),
		pattern(cm, "help.download.<os>-<arch>", `^help\.download\.(`+platforms+`)$`, "CLI download link per platform"),
		exact(cm, KeyDexConfig, "Dex connector configuration (YAML; $secret references allowed)"),
		exact(cm, KeyOIDCConfig, "OIDC provider configuration (YAML; $secret references allowed)"),
		exact(cm, KeyOIDCTLSInsecureSkip, `"true" skips TLS verification towards the OIDC provider`),
		exact(cm, KeyResourceCustomizations, "legacy YAML map of resource customizations"),
		pattern(cm, "resource.customizations.<type>.<group_kind>", `^resource\.customizations\.(`+types+`)\..+$`, "resource customization (health, actions, ignoreDifferences, ignoreResourceUpdates, knownTypeFields, useOpenLibs)"),
		exact(cm, KeyResourceExclusions, "YAML list of resource filters excluded from discovery"),
		exact(cm, KeyResourceInclusions, "YAML list of resource filters included in discovery"),
		exact(cm, KeyResourceCompareOptions, "YAML compare options (ignoreAggregatedRoles, ignoreResourceStatusField, ignoreDifferencesOnResourceUpdates)"),
		exact(cm, KeyResourceRespectRBAC, `"", "normal" or "strict": skip APIs the controller cannot list`),
		exact(cm, KeyIgnoreResourceUpdates, `"false" disables ignoreResourceUpdates rules (default true)`),
		exact(cm, KeySensitiveMaskAnnots, "comma-separated Secret annotation keys masked in diffs"),
		exact(cm, KeyResourceCustomLabels, "comma-separated labels shown as resource info"),
		exact(cm, KeyIncludeEventLabelKeys, "comma-separated label key globs copied onto Application events"),
		exact(cm, KeyExcludeEventLabelKeys, "comma-separated label key globs never copied onto Application events"),
		exact(cm, KeyKustomizeEnable, `anything but "true" disables Kustomize`),
		exact(cm, KeyHelmEnable, `anything but "true" disables Helm`),
		exact(cm, KeyJsonnetEnable, `anything but "true" disables Jsonnet in directory sources`),
		exact(cm, KeyKustomizeBuildOptions, "options appended to kustomize build"),
		pattern(cm, "kustomize.path.<version>", `^kustomize\.path\..+$`, "binary path of an additional Kustomize version"),
		pattern(cm, "kustomize.buildOptions.<version>", `^kustomize\.buildOptions\..+$`, "build options of an additional Kustomize version"),
		{Source: cm, Key: "kustomize.version.<version>", Description: "binary path of an additional Kustomize version", Deprecated: "use kustomize.path.<version>", re: regexp.MustCompile(`^kustomize\.version\..+$`)},
		exact(cm, KeyHelmValuesFileSchemes, "allowed URL schemes of remote Helm value files (default https, http)"),
		exact(cm, KeyInstanceLabelKey, "label used for label-based resource tracking (default app.kubernetes.io/instance)"),
		exact(cm, KeyTrackingMethod, "annotation (default), label or annotation+label"),
		exact(cm, KeyAllowedNodeLabels, "comma-separated node labels shown in the pod view"),
		exact(cm, KeyInstallationID, "installation ID written on managed resources"),
		exact(cm, KeyUICSSURL, "URL or path of custom UI CSS"),
		exact(cm, KeyUIBannerContent, "UI banner text"),
		exact(cm, KeyUIBannerURL, "UI banner link"),
		exact(cm, KeyUIBannerPermanent, `"true" makes the UI banner permanent`),
		exact(cm, KeyUIBannerPosition, "top (default), bottom or both"),
		exact(cm, KeyReconciliationTimeout, "periodic refresh interval (default 120s; 0 disables)"),
		exact(cm, KeyReconciliationJitter, "maximum random delay added to periodic refreshes (default 60s)"),
		exact(cm, KeyHardReconciliation, "periodic hard refresh interval (default 0, disabled)"),
		exact(cm, KeyWebhookMaxPayloadSizeMB, "maximum webhook payload in MB (default 50)"),
		exact(cm, KeyWebhookRefreshJitter, "maximum delay of webhook-triggered refreshes (default 0)"),
		exact(cm, KeyWebhookJitterThreshold, "affected applications before webhook jitter applies (default 10)"),
		exact(cm, KeyInClusterEnabled, `"false" disables the in-cluster destination`),
		exact(cm, KeyMaxPodLogsToRender, "maximum pods whose logs the UI renders (default 10)"),
		exact(cm, KeyExecEnabled, `"true" enables the web terminal`),
		exact(cm, KeyExecShells, "comma-separated shells tried by the web terminal (default bash,sh,powershell,cmd)"),
		exact(cm, KeyDisableFineGrainedRBAC, "true (default): resource update/delete checks only fine-grained actions"),
		deprecated(cm, KeyRBACLogEnforceEnable, "RBAC enforcement on pod logs", "removed in Argo CD 3.0; logs RBAC is always enforced"),
		exact(cm, KeyApplicationLinks, "YAML list of Application deep links"),
		exact(cm, KeyResourceLinks, "YAML list of resource deep links"),
		exact(cm, KeyProjectLinks, "YAML list of project deep links"),
		exact(cm, KeyExtensionConfig, "proxy extensions (YAML extensions list; $secret references allowed)"),
		pattern(cm, "extension.config.<name>", `^extension\.config\.[^.]+$`, "backend of one proxy extension"),
		exact(cm, KeyImpersonationEnabled, `"true" syncs with project destination service accounts`),
		exact(cm, KeyImpersonationEnforced, `"false" falls back to the controller service account`),
		exact(cm, KeyRequireOverrideRevision, "true requires the override action to sync another revision"),
		exact(cm, KeyCommitAuthorName, "source hydrator commit author name"),
		exact(cm, KeyCommitAuthorEmail, "source hydrator commit author email"),
		exact(cm, KeyCommitMessageTemplate, "source hydrator commit message template"),
		exact(cm, KeyReadmeMessageTemplate, "source hydrator README template"),
		exact(cm, KeyGlobalProjects, "YAML list of {projectName, labelSelector} global projects"),
		deprecated(cm, KeyLegacyRepositories, "legacy repository list", "removed in Argo CD 2.1; use Repository objects"),
		deprecated(cm, KeyLegacyRepoCredentials, "legacy repository credential templates", "removed in Argo CD 2.1; use RepositoryCredential objects"),
		deprecated(cm, KeyLegacyHelmRepositories, "legacy Helm repository list", "removed in Argo CD 2.1; use Repository objects"),
		deprecated(cm, KeyLegacyCMPs, "legacy config management plugins", "removed in Argo CD 2.8; use sidecar plugins"),
	}

	sec := SourceSecret
	specs = append(specs,
		exact(sec, SecretKeyServerSecretKey, "HS256 session signing key (>= 32 bytes; generated when missing)"),
		exact(sec, SecretKeyServerSecretKeyPrevious, "previous signing key, accepted for verification only"),
		exact(sec, SecretKeyTLSCert, "API server certificate (PEM)"),
		exact(sec, SecretKeyTLSKey, "API server private key (PEM)"),
		exact(sec, SecretKeyAdminPassword, "bcrypt hash of the admin password"),
		exact(sec, SecretKeyAdminPasswordMtime, "RFC 3339 time of the last admin password change"),
		exact(sec, SecretKeyAdminTokens, "JSON list of admin API token metadata"),
		pattern(sec, "accounts.<name>.password", `^accounts\.[^.]+\.password$`, "bcrypt hash of a local account password"),
		pattern(sec, "accounts.<name>.passwordMtime", `^accounts\.[^.]+\.passwordMtime$`, "RFC 3339 time of the last password change"),
		pattern(sec, "accounts.<name>.tokens", `^accounts\.[^.]+\.tokens$`, "JSON list of API token metadata"),
		exact(sec, SecretKeyWebhookGitHub, "GitHub webhook secret (value or $<secret>:<key>)"),
		exact(sec, SecretKeyWebhookGitLab, "GitLab webhook secret"),
		exact(sec, SecretKeyWebhookBitbucketUUID, "Bitbucket Cloud webhook UUID"),
		exact(sec, SecretKeyWebhookBitbucketServer, "Bitbucket Server webhook secret"),
		exact(sec, SecretKeyWebhookGogs, "Gogs webhook secret"),
		exact(sec, SecretKeyWebhookAzureDevOpsUser, "Azure DevOps webhook basic-auth username"),
		exact(sec, SecretKeyWebhookAzureDevOpsPass, "Azure DevOps webhook basic-auth password"),
		pattern(sec, "<any>", `^.+$`, "any other key, referenced as $<key> from oidc.config, dex.config and extension.config"),
	)

	rb := SourceRBACConfigMap
	specs = append(specs,
		exact(rb, RBACKeyPolicyCSV, "user RBAC policy (CSV)"),
		pattern(rb, "policy.<name>.csv", `^policy\..+\.csv$`, "policy overlays appended in key order"),
		exact(rb, RBACKeyPolicyDefault, "default role of every request"),
		exact(rb, RBACKeyMatchMode, "glob (default) or regex"),
		exact(rb, RBACKeyScopes, "YAML list of OIDC claims evaluated as groups (default [groups])"),
	)

	specs = append(specs,
		pattern(SourceTLSCertsConfigMap, "<hostname>", `^.+$`, "PEM certificates trusted for a repository server"),
		exact(SourceSSHKnownHostsConfigMap, SSHKnownHostsKey, "known_hosts entries for SSH repositories"),
		pattern(SourceGPGKeysConfigMap, "<key-id>", `^.+$`, "ASCII-armored GnuPG public key, keyed by its 16-hex-digit key ID"),
		pattern(SourceNotificationsConfigMap, "<notifications key>", `^.+$`, "notifications services, templates, triggers and subscriptions (validated by internal/notifications)"),
		pattern(SourceNotificationsSecret, "<any>", `^.+$`, "notification service credentials referenced as $<key>"),
	)

	for _, p := range paramDefs {
		spec := KeySpec{Source: SourceCmdParamsConfigMap, Key: p.key, Description: p.desc, Deprecated: p.deprecated}
		if p.na != "" {
			spec.Deprecated = "no effect in Patrol CD: " + p.na
		}
		specs = append(specs, spec)
	}
	return specs
}

// AcceptedKeys returns every accepted key and key pattern, ordered by source and key.
func AcceptedKeys() []KeySpec {
	out := slices.Clone(acceptedKeys)
	slices.SortStableFunc(out, func(a, b KeySpec) int {
		if a.Source != b.Source {
			return strings.Compare(string(a.Source), string(b.Source))
		}
		return strings.Compare(a.Key, b.Key)
	})
	return out
}

// LookupKey returns the spec accepting key in source. Exact keys win over patterns.
func LookupKey(src Source, key string) (KeySpec, bool) {
	var match *KeySpec
	for i := range acceptedKeys {
		k := &acceptedKeys[i]
		if k.Source != src || !k.Matches(key) {
			continue
		}
		if k.re == nil {
			return *k, true
		}
		if match == nil {
			match = k
		}
	}
	if match == nil {
		return KeySpec{}, false
	}
	return *match, true
}

// keySuggestions maps common mistakes to the key that was probably meant.
var keySuggestions = map[string]string{
	"users.passwordPattern":     KeyPasswordPattern,
	"timeout.reconcilation":     KeyReconciliationTimeout,
	"resource.respectRbac":      KeyResourceRespectRBAC,
	"application.instanceLabel": KeyInstanceLabelKey,
	"statusbadge.enable":        KeyStatusBadgeEnabled,
	"exec.enable":               KeyExecEnabled,
	"oidc.configuration":        KeyOIDCConfig,
	"dex.configuration":         KeyDexConfig,
	"resource.exclusion":        KeyResourceExclusions,
	"resource.inclusion":        KeyResourceInclusions,
}
