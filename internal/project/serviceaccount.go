package project

import (
	"fmt"
	"strings"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Settings keys (patrol-cm, Argo CD argocd-cm names) controlling sync impersonation.
const (
	SettingImpersonationEnabled  = "application.sync.impersonation.enabled"
	SettingImpersonationEnforced = "application.sync.impersonation.enforced"
)

// ImpersonationSettings controls whether syncs impersonate a destination service account
// (TEN-030, SYNC-093/094).
type ImpersonationSettings struct {
	// Enabled turns impersonation on.
	Enabled bool
	// Enforced fails the sync when no destination service account matches; when false the
	// controller falls back to its own identity.
	Enforced bool
}

// ImpersonationSettingsFromConfigMap reads the settings with the upstream rules: enabled only
// for the exact value "true"; enforced unless the value is exactly "false" (absent and any
// other value enforce).
func ImpersonationSettingsFromConfigMap(data map[string]string) ImpersonationSettings {
	return ImpersonationSettings{
		Enabled:  data[SettingImpersonationEnabled] == "true",
		Enforced: data[SettingImpersonationEnforced] != "false",
	}
}

// ServiceAccountUserName returns the Kubernetes user name of a service account.
func ServiceAccountUserName(namespace, name string) string {
	return fmt.Sprintf("system:serviceaccount:%s:%s", namespace, name)
}

// DeriveServiceAccount returns the user name to impersonate when syncing app to the cluster
// with URL destServer (upstream DeriveServiceAccountToImpersonate), or "" when no
// spec.destinationServiceAccounts entry matches.
//
// Entries are evaluated in order (including entries appended from global projects); the
// first whose server glob matches destServer and whose namespace glob matches
// spec.destination.namespace wins. A defaultServiceAccount "<ns>:<name>" is used as is;
// a plain "<name>" is qualified with the destination namespace, or with the Application's
// own namespace when the destination namespace is empty.
func DeriveServiceAccount(proj *v1alpha1.AppProject, app *v1alpha1.Application, destServer string) (string, error) {
	if proj == nil || app == nil {
		return "", nil
	}
	saNamespace := app.Spec.Destination.Namespace
	if saNamespace == "" {
		saNamespace = app.Namespace
	}
	for _, item := range proj.Spec.DestinationServiceAccounts {
		serverMatched, err := globMatchErr(item.Server, destServer)
		if err != nil {
			return "", fmt.Errorf("invalid glob pattern for destination server: %w", err)
		}
		namespaceMatched, err := globMatchErr(item.Namespace, app.Spec.Destination.Namespace)
		if err != nil {
			return "", fmt.Errorf("invalid glob pattern for destination namespace: %w", err)
		}
		if !serverMatched || !namespaceMatched {
			continue
		}
		sa := item.DefaultServiceAccount
		if !validServiceAccountName(sa) {
			return "", fmt.Errorf("default service account contains invalid chars '%s'", sa)
		}
		if strings.Contains(sa, ":") {
			return "system:serviceaccount:" + sa, nil
		}
		return ServiceAccountUserName(saNamespace, sa), nil
	}
	return "", nil
}

// Impersonation is the identity a sync runs with.
type Impersonation struct {
	// UserName is the Kubernetes user to impersonate; empty means the controller's own
	// identity.
	UserName string
}

// SelectServiceAccount applies the impersonation settings (TEN-029, TEN-030): disabled →
// controller identity; enabled → the derived service account; no match → a
// *NoServiceAccountError when enforced, else the controller identity. Errors from
// DeriveServiceAccount are returned as is; the controller reports them as
// "failed to derive service account to impersonate: <err>".
func SelectServiceAccount(proj *v1alpha1.AppProject, app *v1alpha1.Application, destServer string, settings ImpersonationSettings) (Impersonation, error) {
	if !settings.Enabled {
		return Impersonation{}, nil
	}
	sa, err := DeriveServiceAccount(proj, app, destServer)
	if err != nil {
		return Impersonation{}, err
	}
	if sa == "" {
		if settings.Enforced {
			ns := ""
			if app != nil {
				ns = app.Spec.Destination.Namespace
			}
			return Impersonation{}, &NoServiceAccountError{Server: destServer, Namespace: ns}
		}
		return Impersonation{}, nil
	}
	return Impersonation{UserName: sa}, nil
}
