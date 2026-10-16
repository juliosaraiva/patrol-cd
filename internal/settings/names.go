package settings

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Default object names in the control-plane namespace. Each is the Argo CD name with the
// "argocd" product prefix replaced by "patrol" (ADR-0003).
const (
	DefaultConfigMapName              = "patrol-cm"                   // argocd-cm
	DefaultRBACConfigMapName          = "patrol-rbac-cm"              // argocd-rbac-cm
	DefaultCmdParamsConfigMapName     = "patrol-cmd-params-cm"        // argocd-cmd-params-cm
	DefaultTLSCertsConfigMapName      = "patrol-tls-certs-cm"         // argocd-tls-certs-cm
	DefaultSSHKnownHostsConfigMapName = "patrol-ssh-known-hosts-cm"   // argocd-ssh-known-hosts-cm
	DefaultGPGKeysConfigMapName       = "patrol-gpg-keys-cm"          // argocd-gpg-keys-cm
	DefaultNotificationsConfigMapName = "patrol-notifications-cm"     // argocd-notifications-cm
	DefaultSecretName                 = "patrol-secret"               // argocd-secret
	DefaultNotificationsSecretName    = "patrol-notifications-secret" // argocd-notifications-secret
)

// Label that settings ConfigMaps and referenced Secrets must carry (Argo CD:
// app.kubernetes.io/part-of=argocd). See [Input.IgnorePartOfLabel].
const (
	LabelKeyPartOf   = v1alpha1.LabelKeyPartOf
	LabelValuePartOf = v1alpha1.LabelValuePartOf
)

// Source identifies one settings object independently of its configured name.
type Source string

// Settings sources.
const (
	SourceConfigMap              Source = "settings"
	SourceRBACConfigMap          Source = "rbac"
	SourceCmdParamsConfigMap     Source = "cmd-params"
	SourceTLSCertsConfigMap      Source = "tls-certs"
	SourceSSHKnownHostsConfigMap Source = "ssh-known-hosts"
	SourceGPGKeysConfigMap       Source = "gpg-keys"
	SourceNotificationsConfigMap Source = "notifications"
	SourceSecret                 Source = "secret"
	SourceNotificationsSecret    Source = "notifications-secret"
	// SourceReferencedSecret is any Secret labelled LabelKeyPartOf=LabelValuePartOf that
	// "$<secret>:<key>" references may point to.
	SourceReferencedSecret Source = "referenced-secret"
)

// configMapSources lists the ConfigMap sources in a stable order.
var configMapSources = []Source{
	SourceConfigMap, SourceRBACConfigMap, SourceCmdParamsConfigMap, SourceTLSCertsConfigMap,
	SourceSSHKnownHostsConfigMap, SourceGPGKeysConfigMap, SourceNotificationsConfigMap,
}

// secretSources lists the named Secret sources in a stable order.
var secretSources = []Source{SourceSecret, SourceNotificationsSecret}

// requiresPartOfLabel reports whether the ConfigMap source must carry the part-of label to be
// read. Upstream reads these through the settings manager's label-filtered informer (OPS-012);
// the RBAC, cmd-params and notifications ConfigMaps are read by name.
func requiresPartOfLabel(src Source) bool {
	switch src {
	case SourceConfigMap, SourceTLSCertsConfigMap, SourceSSHKnownHostsConfigMap, SourceGPGKeysConfigMap:
		return true
	}
	return false
}

// Names are the object names of the settings sources. Empty fields select the defaults.
type Names struct {
	ConfigMap              string
	RBACConfigMap          string
	CmdParamsConfigMap     string
	TLSCertsConfigMap      string
	SSHKnownHostsConfigMap string
	GPGKeysConfigMap       string
	NotificationsConfigMap string
	Secret                 string
	NotificationsSecret    string
}

// DefaultNames returns the default object names.
func DefaultNames() Names {
	return Names{
		ConfigMap:              DefaultConfigMapName,
		RBACConfigMap:          DefaultRBACConfigMapName,
		CmdParamsConfigMap:     DefaultCmdParamsConfigMapName,
		TLSCertsConfigMap:      DefaultTLSCertsConfigMapName,
		SSHKnownHostsConfigMap: DefaultSSHKnownHostsConfigMapName,
		GPGKeysConfigMap:       DefaultGPGKeysConfigMapName,
		NotificationsConfigMap: DefaultNotificationsConfigMapName,
		Secret:                 DefaultSecretName,
		NotificationsSecret:    DefaultNotificationsSecretName,
	}
}

// WithDefaults returns n with empty fields replaced by the defaults.
func (n Names) WithDefaults() Names {
	d := DefaultNames()
	set := func(v *string, def string) {
		if *v == "" {
			*v = def
		}
	}
	set(&n.ConfigMap, d.ConfigMap)
	set(&n.RBACConfigMap, d.RBACConfigMap)
	set(&n.CmdParamsConfigMap, d.CmdParamsConfigMap)
	set(&n.TLSCertsConfigMap, d.TLSCertsConfigMap)
	set(&n.SSHKnownHostsConfigMap, d.SSHKnownHostsConfigMap)
	set(&n.GPGKeysConfigMap, d.GPGKeysConfigMap)
	set(&n.NotificationsConfigMap, d.NotificationsConfigMap)
	set(&n.Secret, d.Secret)
	set(&n.NotificationsSecret, d.NotificationsSecret)
	return n
}

// Name returns the object name of a ConfigMap or named Secret source ("" for
// SourceReferencedSecret and unknown sources).
func (n Names) Name(src Source) string {
	n = n.WithDefaults()
	switch src {
	case SourceConfigMap:
		return n.ConfigMap
	case SourceRBACConfigMap:
		return n.RBACConfigMap
	case SourceCmdParamsConfigMap:
		return n.CmdParamsConfigMap
	case SourceTLSCertsConfigMap:
		return n.TLSCertsConfigMap
	case SourceSSHKnownHostsConfigMap:
		return n.SSHKnownHostsConfigMap
	case SourceGPGKeysConfigMap:
		return n.GPGKeysConfigMap
	case SourceNotificationsConfigMap:
		return n.NotificationsConfigMap
	case SourceSecret:
		return n.Secret
	case SourceNotificationsSecret:
		return n.NotificationsSecret
	}
	return ""
}

// Validate checks that every name is a valid object name and that no two ConfigMap sources
// (or the two Secret sources) share a name.
func (n Names) Validate() error {
	n = n.WithDefaults()
	var errs []error
	check := func(sources []Source) {
		seen := map[string]Source{}
		for _, src := range sources {
			name := n.Name(src)
			for _, msg := range validation.IsDNS1123Subdomain(name) {
				errs = append(errs, fmt.Errorf("%s object name %q: %s", src, name, msg))
			}
			if prev, ok := seen[name]; ok {
				errs = append(errs, fmt.Errorf("%s and %s use the same object name %q", prev, src, name))
			}
			seen[name] = src
		}
	}
	check(configMapSources)
	check(secretSources)
	return errors.Join(errs...)
}
