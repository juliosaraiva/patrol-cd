package settings

import (
	"crypto/tls"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/lua"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
)

// Defaults of patrol-cm settings (upstream values).
const (
	DefaultPasswordPattern               = "^.{8,32}$"
	DefaultUserSessionDuration           = 24 * time.Hour
	DefaultHelpChatText                  = "Chat now!"
	DefaultMaxPodLogsToRender            = int64(10)
	DefaultWebhookMaxPayloadSizeMB       = int64(50)
	DefaultWebhookRefreshJitterThreshold = 10
	DefaultReconciliationTimeout         = 120 * time.Second
	DefaultReconciliationJitter          = 60 * time.Second
	DefaultHardReconciliationTimeout     = time.Duration(0)
	DefaultUIBannerPosition              = "top"
)

// DefaultExecShells are the shells tried by the web terminal when exec.shells is unset.
var DefaultExecShells = []string{"bash", "sh", "powershell", "cmd"}

// DefaultHelmValuesFileSchemes are the remote value file schemes allowed when
// helm.valuesFileSchemes is absent.
var DefaultHelmValuesFileSchemes = []string{"https", "http"}

// Settings is an immutable snapshot of every settings source. All methods are safe for
// concurrent use and return copies, so callers cannot modify the snapshot.
type Settings struct {
	namespace string
	names     Names

	// input bookkeeping
	resourceVersions map[string]string
	fingerprint      string
	// accepted holds, per source, the raw value accepted for each key; it is consulted when
	// the next snapshot cannot parse a key.
	accepted map[Source]map[string]string
	issues   []Issue
	// states records how each named source was obtained; retained holds the data of the
	// retained sources as loaded (or carried over), for the next snapshot.
	states   map[Source]SourceState
	retained map[Source]retainedObject

	// patrol-cm
	url                     string
	additionalURLs          []string
	statusBadge             StatusBadge
	anonymousUserEnabled    bool
	userSessionDuration     time.Duration
	passwordPattern         string
	accounts                auth.Accounts
	ui                      UI
	help                    Help
	googleAnalytics         GoogleAnalytics
	dexConfig               string
	dexConnectors           []DexConnector
	dexConfigured           bool
	oidcConfig              *OIDCConfig
	oidcTLSInsecureSkip     bool
	resourceFilter          *ResourcesFilter
	resourceOverrides       map[string]lua.ResourceOverride
	compareOptions          diff.CompareOptions
	respectRBAC             string
	ignoreResourceUpdates   bool
	sensitiveMaskAnnots     []string
	customLabels            []string
	includeEventLabelKeys   []string
	excludeEventLabelKeys   []string
	enabledSourceTypes      map[v1alpha1.ApplicationSourceType]bool
	kustomize               Kustomize
	helmValuesFileSchemes   []string
	instanceLabelKey        string
	trackingMethod          v1alpha1.TrackingMethod
	allowedNodeLabels       []string
	installationID          string
	reconciliation          Reconciliation
	webhook                 Webhook
	inClusterEnabled        bool
	maxPodLogsToRender      int64
	exec                    Exec
	disableFineGrainedRBAC  bool
	deepLinks               map[string][]DeepLink
	extensions              []Extension
	extensionConfigs        map[string]string
	impersonation           Impersonation
	requireOverrideRevision bool
	hydrator                Hydrator
	globalProjects          []GlobalProject

	// patrol-secret and referenced Secrets
	serverSecretKey     Sensitive
	serverSecretKeyPrev Sensitive
	serverCertificate   *tls.Certificate
	secretData          map[string]string
	refSecrets          map[string]map[string]string

	// other sources
	rbac          RBAC
	tlsCerts      map[string][]string
	sshKnownHosts []SSHKnownHost
	gpgKeys       map[string]GPGKey
	notifications Notifications
	cmdParams     map[string]string
}

// StatusBadge holds statusbadge.* settings.
type StatusBadge struct {
	Enabled bool
	// RootURL overrides the URL used in badge links ("" = the request URL).
	RootURL string
}

// UI holds ui.* settings.
type UI struct {
	CSSURL          string
	BannerContent   string
	BannerURL       string
	BannerPermanent bool
	// BannerPosition is "top", "bottom", "both" or "" (unset; the UI uses top).
	BannerPosition string
}

// Help holds help.* settings.
type Help struct {
	ChatURL string
	// ChatText is "Chat now!" unless help.chatText is set; "" when help.chatUrl is absent.
	ChatText string
	// BinaryURLs maps "<os>-<arch>" to a download URL.
	BinaryURLs map[string]string
}

// GoogleAnalytics holds ga.* settings.
type GoogleAnalytics struct {
	TrackingID     string
	AnonymizeUsers bool
}

// Kustomize holds kustomize.* settings.
type Kustomize struct {
	// BuildOptions are appended to every kustomize build (split on whitespace by callers).
	BuildOptions string
	// Versions are the additional versions, sorted by name.
	Versions []KustomizeVersion
}

// KustomizeVersion is one kustomize.path.<version> (or deprecated kustomize.version.<version>)
// entry.
type KustomizeVersion struct {
	Name         string
	Path         string
	BuildOptions string
}

// KustomizeVersionNotRegisteredError is returned by KustomizeBinaryPath for an unknown
// version.
type KustomizeVersionNotRegisteredError struct {
	Version string
}

func (e KustomizeVersionNotRegisteredError) Error() string {
	return fmt.Sprintf("kustomize version %s is not registered", e.Version)
}

// Reconciliation holds the timeout.* settings with defaults applied.
type Reconciliation struct {
	// Timeout is the periodic (soft) refresh interval; 0 disables.
	Timeout time.Duration
	// TimeoutSet is true when timeout.reconciliation is configured (the repo-server applies
	// its own default of 3m otherwise).
	TimeoutSet bool
	// Jitter is the maximum random delay added to periodic refreshes.
	Jitter time.Duration
	// HardTimeout is the periodic hard refresh interval; 0 disables.
	HardTimeout time.Duration
}

// Webhook holds the webhook settings of patrol-cm and the resolved webhook secrets of
// patrol-secret.
type Webhook struct {
	// MaxPayloadSize is webhook.maxPayloadSizeMB in bytes.
	MaxPayloadSize         int64
	RefreshJitter          time.Duration
	RefreshJitterThreshold int

	// SecretsLoaded is false while patrol-secret has never been loaded
	// (SourceState(SourceSecret) == SourceStateAbsent). The secrets below are then unknown
	// rather than unset, so handlers must reject payloads instead of skipping the signature
	// check. A Secret that disappears after loading is retained, keeping SecretsLoaded true.
	SecretsLoaded bool

	GitHubSecret          Sensitive
	GitLabSecret          Sensitive
	BitbucketUUID         Sensitive
	BitbucketServerSecret Sensitive
	GogsSecret            Sensitive
	AzureDevOpsUsername   Sensitive
	AzureDevOpsPassword   Sensitive
}

// Exec holds exec.* settings.
type Exec struct {
	Enabled bool
	Shells  []string
}

// Impersonation holds application.sync.impersonation.* settings.
type Impersonation struct {
	Enabled bool
	// Enforced is true unless application.sync.impersonation.enforced is exactly "false".
	Enforced bool
}

// Hydrator holds the source hydrator settings with default templates applied.
type Hydrator struct {
	// CommitAuthorName and CommitAuthorEmail are "" when unset (the commit server applies its
	// defaults).
	CommitAuthorName      string
	CommitAuthorEmail     string
	CommitMessageTemplate string
	ReadmeMessageTemplate string
}

// RBAC is the content of patrol-rbac-cm, ready for internal/rbac.
type RBAC struct {
	// Present is false when no patrol-rbac-cm was ever loaded (no user policy); one deleted
	// after loading is retained (see Parse and Settings.SourceState).
	Present bool
	// PolicyCSV is policy.csv followed by the policy.<name>.csv overlays in key order.
	PolicyCSV string
	// Sources are the same documents with their key names (rbac.ParseSources input).
	Sources []rbac.Source
	// DefaultRole is policy.default.
	DefaultRole string
	// MatchMode is "glob" or "regex".
	MatchMode string
	// Scopes are the claims evaluated as groups.
	Scopes []string
	// Data is a copy of the ConfigMap data (rbac.Enforcer.ApplyConfigMap input).
	Data map[string]string
}

// Notifications describes the notifications ConfigMap and Secret. The Secret's values are
// not kept in the snapshot; the notifications controller reads them itself.
type Notifications struct {
	// ConfigMap is a copy of the notifications ConfigMap data (nil when absent).
	ConfigMap map[string]string
	// SecretName is the notifications Secret's name.
	SecretName string
	// SecretPresent reports whether the Secret exists.
	SecretPresent bool
	// SecretKeys are the Secret's data keys, sorted.
	SecretKeys []string
}

// Namespace returns the control-plane namespace.
func (s *Settings) Namespace() string { return s.namespace }

// Names returns the object names the snapshot was built from.
func (s *Settings) Names() Names { return s.names }

// Fingerprint is a digest of the input; equal fingerprints mean equal input.
func (s *Settings) Fingerprint() string { return s.fingerprint }

// ResourceVersions maps object names to the resourceVersion of the object that was read.
func (s *Settings) ResourceVersions() map[string]string { return maps.Clone(s.resourceVersions) }

// Validate returns every issue found while building the snapshot, sorted.
func (s *Settings) Validate() []Issue { return slices.Clone(s.issues) }

// URL returns the external URL ("url").
func (s *Settings) URL() string { return s.url }

// AdditionalURLs returns additionalUrls.
func (s *Settings) AdditionalURLs() []string { return slices.Clone(s.additionalURLs) }

// StatusBadge returns the status badge settings.
func (s *Settings) StatusBadge() StatusBadge { return s.statusBadge }

// AnonymousUserEnabled reports users.anonymous.enabled.
func (s *Settings) AnonymousUserEnabled() bool { return s.anonymousUserEnabled }

// UserSessionDuration returns users.session.duration (default 24h).
func (s *Settings) UserSessionDuration() time.Duration { return s.userSessionDuration }

// PasswordPattern returns passwordPattern (default ^.{8,32}$).
func (s *Settings) PasswordPattern() string { return s.passwordPattern }

// Accounts returns the local accounts (admin plus accounts.*), with password hashes, mtimes
// and token metadata from patrol-secret.
func (s *Settings) Accounts() auth.Accounts {
	out := make(auth.Accounts, len(s.accounts))
	for name, a := range s.accounts {
		a.Capabilities = slices.Clone(a.Capabilities)
		a.Tokens = slices.Clone(a.Tokens)
		out[name] = a
	}
	return out
}

// UserLoginsDisabled reports whether no enabled account has the login capability (API-041).
func (s *Settings) UserLoginsDisabled() bool {
	for _, a := range s.accounts {
		if a.Enabled && a.HasCapability(auth.CapabilityLogin) {
			return false
		}
	}
	return true
}

// UI returns the ui.* settings.
func (s *Settings) UI() UI { return s.ui }

// Help returns the help.* settings.
func (s *Settings) Help() Help {
	h := s.help
	h.BinaryURLs = maps.Clone(h.BinaryURLs)
	return h
}

// GoogleAnalytics returns the ga.* settings.
func (s *Settings) GoogleAnalytics() GoogleAnalytics { return s.googleAnalytics }

// RespectRBAC returns resource.respectRBAC: "", "normal" or "strict".
func (s *Settings) RespectRBAC() string { return s.respectRBAC }

// CacheRespectRBAC maps resource.respectRBAC to the cluster cache setting.
func (s *Settings) CacheRespectRBAC() cache.RespectRBAC {
	switch s.respectRBAC {
	case "normal":
		return cache.RespectRBACNormal
	case "strict":
		return cache.RespectRBACStrict
	}
	return cache.RespectRBACNone
}

// ResourceFilter returns a copy of the effective resource filter (core exclusions,
// resource.exclusions and resource.inclusions). It implements cache.ResourceFilter.
func (s *Settings) ResourceFilter() *ResourcesFilter { return s.resourceFilter.clone() }

// IsExcludedResource reports whether group/kind is excluded on the cluster with the given API
// server URL.
func (s *Settings) IsExcludedResource(group, kind, cluster string) bool {
	return s.resourceFilter.IsExcluded(group, kind, cluster)
}

// ResourceOverrides returns the resource customizations keyed by "group/Kind" ("Kind" for
// the core group, "*/*" for all), as parsed by lua.ParseResourceOverrides. The /status rule of
// ignoreResourceStatusField is not included; see SystemIgnoreRules.
func (s *Settings) ResourceOverrides() map[string]lua.ResourceOverride {
	out := make(map[string]lua.ResourceOverride, len(s.resourceOverrides))
	for k, v := range s.resourceOverrides {
		v.KnownTypeFields = slices.Clone(v.KnownTypeFields)
		out[k] = v
	}
	return out
}

// CompareOptions returns resource.compareoptions with defaults applied.
func (s *Settings) CompareOptions() diff.CompareOptions { return s.compareOptions }

// IgnoreResourceUpdatesEnabled reports resource.ignoreResourceUpdatesEnabled (default true).
func (s *Settings) IgnoreResourceUpdatesEnabled() bool { return s.ignoreResourceUpdates }

// SensitiveMaskAnnotations returns resource.sensitive.mask.annotations.
func (s *Settings) SensitiveMaskAnnotations() []string { return slices.Clone(s.sensitiveMaskAnnots) }

// CustomLabels returns resource.customLabels (split on commas, not trimmed, as upstream).
func (s *Settings) CustomLabels() []string { return slices.Clone(s.customLabels) }

// IncludeEventLabelKeys returns resource.includeEventLabelKeys.
func (s *Settings) IncludeEventLabelKeys() []string { return slices.Clone(s.includeEventLabelKeys) }

// ExcludeEventLabelKeys returns resource.excludeEventLabelKeys.
func (s *Settings) ExcludeEventLabelKeys() []string { return slices.Clone(s.excludeEventLabelKeys) }

// SourceTypeEnabled reports whether a rendering tool is enabled (kustomize.enable,
// helm.enable, jsonnet.enable for Directory; Plugin is always enabled).
func (s *Settings) SourceTypeEnabled(t v1alpha1.ApplicationSourceType) bool {
	if v, ok := s.enabledSourceTypes[t]; ok {
		return v
	}
	return true
}

// EnabledSourceTypes returns the enabled map sent to the repo-server.
func (s *Settings) EnabledSourceTypes() map[v1alpha1.ApplicationSourceType]bool {
	return maps.Clone(s.enabledSourceTypes)
}

// Kustomize returns the kustomize.* settings.
func (s *Settings) Kustomize() Kustomize {
	k := s.kustomize
	k.Versions = slices.Clone(k.Versions)
	return k
}

// KustomizeBinaryPath returns the binary registered for version ("" selects the default
// binary). An unknown version yields KustomizeVersionNotRegisteredError.
func (s *Settings) KustomizeBinaryPath(version string) (string, error) {
	if version == "" {
		return "", nil
	}
	for _, v := range s.kustomize.Versions {
		if v.Name == version {
			return v.Path, nil
		}
	}
	return "", KustomizeVersionNotRegisteredError{Version: version}
}

// HelmValuesFileSchemes returns helm.valuesFileSchemes (default https, http; empty when the
// key is set to "").
func (s *Settings) HelmValuesFileSchemes() []string { return slices.Clone(s.helmValuesFileSchemes) }

// InstanceLabelKey returns application.instanceLabelKey (default app.kubernetes.io/instance).
func (s *Settings) InstanceLabelKey() string { return s.instanceLabelKey }

// TrackingMethod returns application.resourceTrackingMethod (default annotation).
func (s *Settings) TrackingMethod() v1alpha1.TrackingMethod { return s.trackingMethod }

// AllowedNodeLabels returns application.allowedNodeLabels (invalid keys dropped).
func (s *Settings) AllowedNodeLabels() []string { return slices.Clone(s.allowedNodeLabels) }

// InstallationID returns installationID.
func (s *Settings) InstallationID() string { return s.installationID }

// Reconciliation returns the timeout.* settings.
func (s *Settings) Reconciliation() Reconciliation { return s.reconciliation }

// Webhook returns the webhook settings and resolved webhook secrets.
func (s *Settings) Webhook() Webhook { return s.webhook }

// InClusterEnabled reports cluster.inClusterEnabled (true unless exactly "false").
func (s *Settings) InClusterEnabled() bool { return s.inClusterEnabled }

// MaxPodLogsToRender returns server.maxPodLogsToRender (default 10).
func (s *Settings) MaxPodLogsToRender() int64 { return s.maxPodLogsToRender }

// Exec returns the exec.* settings.
func (s *Settings) Exec() Exec {
	e := s.exec
	e.Shells = slices.Clone(e.Shells)
	return e
}

// ApplicationFineGrainedRBACInheritanceDisabled reports
// server.rbac.disableApplicationFineGrainedRBACInheritance (default true).
func (s *Settings) ApplicationFineGrainedRBACInheritanceDisabled() bool {
	return s.disableFineGrainedRBAC
}

// DeepLinks returns the deep links of key KeyApplicationLinks, KeyResourceLinks or
// KeyProjectLinks, in configuration order.
func (s *Settings) DeepLinks(key string) []DeepLink {
	links := s.deepLinks[key]
	out := make([]DeepLink, len(links))
	for i, l := range links {
		out[i] = l.clone()
	}
	return out
}

// Extensions returns the validated proxy extensions ordered by key ("extension.config"
// first, then "extension.config.<name>" keys in order).
func (s *Settings) Extensions() []Extension {
	out := make([]Extension, len(s.extensions))
	for i, e := range s.extensions {
		out[i] = e.clone()
	}
	return out
}

// ExtensionConfigs returns the raw extension configuration keyed by extension name ("" for
// the extension.config key), as upstream's ArgoCDSettings.ExtensionConfig.
func (s *Settings) ExtensionConfigs() map[string]string { return maps.Clone(s.extensionConfigs) }

// Impersonation returns the application.sync.impersonation.* settings.
func (s *Settings) Impersonation() Impersonation { return s.impersonation }

// RequireOverridePrivilegeForRevisionSync reports
// application.sync.requireOverridePrivilegeForRevisionSync (default false).
func (s *Settings) RequireOverridePrivilegeForRevisionSync() bool {
	return s.requireOverrideRevision
}

// Hydrator returns the source hydrator settings.
func (s *Settings) Hydrator() Hydrator { return s.hydrator }

// GlobalProjects returns globalProjects in configuration order.
func (s *Settings) GlobalProjects() []GlobalProject {
	out := make([]GlobalProject, len(s.globalProjects))
	for i, g := range s.globalProjects {
		out[i] = g.clone()
	}
	return out
}

// ServerSecretKey returns server.secretkey (empty when missing).
func (s *Settings) ServerSecretKey() Sensitive { return s.serverSecretKey }

// SigningKeys builds the session signing key set (server.secretkey plus the optional
// server.secretkey.previous).
func (s *Settings) SigningKeys() (auth.KeySet, error) {
	var prev [][]byte
	if !s.serverSecretKeyPrev.IsZero() {
		prev = append(prev, s.serverSecretKeyPrev.Bytes())
	}
	ks, err := auth.NewKeySet(s.serverSecretKey.Bytes(), prev...)
	if err != nil {
		return auth.KeySet{}, fmt.Errorf("build signing keys from %s: %w", s.names.Secret, err)
	}
	return ks, nil
}

// ServerCertificate returns the API server certificate from tls.crt/tls.key, or nil.
func (s *Settings) ServerCertificate() *tls.Certificate {
	if s.serverCertificate == nil {
		return nil
	}
	c := *s.serverCertificate
	return &c
}

// RBAC returns the patrol-rbac-cm settings.
func (s *Settings) RBAC() RBAC {
	r := s.rbac
	r.Sources = slices.Clone(r.Sources)
	r.Scopes = slices.Clone(r.Scopes)
	r.Data = maps.Clone(r.Data)
	return r
}

// TLSCertificates returns the patrol-tls-certs-cm entries: hostname → PEM certificates.
func (s *Settings) TLSCertificates() map[string][]string {
	out := make(map[string][]string, len(s.tlsCerts))
	for k, v := range s.tlsCerts {
		out[k] = slices.Clone(v)
	}
	return out
}

// TLSCertificatesForServer returns the certificates configured for serverName; a port
// suffix is ignored (upstream ServerNameWithoutPort).
func (s *Settings) TLSCertificatesForServer(serverName string) []string {
	host, _, _ := strings.Cut(serverName, ":")
	return slices.Clone(s.tlsCerts[host])
}

// SSHKnownHosts returns the valid patrol-ssh-known-hosts-cm entries in file order.
func (s *Settings) SSHKnownHosts() []SSHKnownHost {
	out := make([]SSHKnownHost, len(s.sshKnownHosts))
	for i, h := range s.sshKnownHosts {
		h.Hosts = slices.Clone(h.Hosts)
		out[i] = h
	}
	return out
}

// SSHKnownHostsData renders the valid entries in known_hosts format (one per line).
func (s *Settings) SSHKnownHostsData() string {
	var b strings.Builder
	for _, h := range s.sshKnownHosts {
		b.WriteString(h.Line)
		b.WriteByte('\n')
	}
	return b.String()
}

// GPGKeys returns the valid patrol-gpg-keys-cm entries keyed by key ID.
func (s *Settings) GPGKeys() map[string]GPGKey { return maps.Clone(s.gpgKeys) }

// Notifications describes the notifications ConfigMap and Secret.
func (s *Settings) Notifications() Notifications {
	n := s.notifications
	n.ConfigMap = maps.Clone(n.ConfigMap)
	n.SecretKeys = slices.Clone(n.SecretKeys)
	return n
}

// CmdParams returns the patrol-cmd-params-cm data.
func (s *Settings) CmdParams() map[string]string { return maps.Clone(s.cmdParams) }

// Params resolves the typed parameters of a component from patrol-cmd-params-cm and the
// environment (see ResolveParams).
func (s *Settings) Params(c Component, lookupEnv func(string) (string, bool)) (*Params, []Issue) {
	return ResolveParams(c, s.cmdParams, lookupEnv)
}
