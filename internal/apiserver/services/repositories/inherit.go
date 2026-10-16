package repositories

import (
	"context"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
)

// inheritanceKeys are the Secret keys that give a repository credentials of its own, so
// that no template is inherited (the repository library's hasCredentials: a Google
// service account key alone does not count, a username alone does).
var inheritanceKeys = []string{
	v1alpha1.RepositorySecretKeyUsername,
	v1alpha1.RepositorySecretKeyPassword,
	v1alpha1.RepositorySecretKeyBearerToken,
	v1alpha1.RepositorySecretKeySSHPrivateKey,
	v1alpha1.RepositorySecretKeyTLSClientCertData,
	v1alpha1.RepositorySecretKeyGitHubAppPrivateKey,
	v1alpha1.RepositorySecretKeyAzureServicePrincipalClientSecret,
}

// ownsCredentials reports whether a repository with access a and credential data has
// credentials of its own (the repository library then inherits no template): one of
// inheritanceKeys, Azure workload identity, or an SCMProvider that supplies credentials.
func (s *Service) ownsCredentials(ctx context.Context, kc KubeClient, a *v1alpha1.RepositoryAccess, data map[string][]byte) (bool, error) {
	if a.UseAzureWorkloadIdentity {
		return true, nil
	}
	for _, k := range inheritanceKeys {
		if len(data[k]) > 0 {
			return true, nil
		}
	}
	if ref := a.SCMProviderRef; ref != nil {
		var sp v1alpha1.SCMProvider
		if err := kc.Get(ctx, client.ObjectKey{Namespace: s.cfg.Namespace, Name: ref.Name}, &sp); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if auth := sp.Spec.Auth; auth != nil && (auth.GitHubApp != nil || auth.TokenSecretRef != nil || auth.BasicAuthSecretRef != nil) {
			return true, nil
		}
	}
	return false, nil
}

// inheritedTemplate returns the template the repository library applies to url: the
// longest normalized prefix, first by name on ties (templates are sorted by name).
func inheritedTemplate(templates []v1alpha1.RepositoryCredential, url string) *v1alpha1.RepositoryCredential {
	uk := project.NormalizeGitURL(url)
	var best *v1alpha1.RepositoryCredential
	bestLen := 0
	for i := range templates {
		pk := project.NormalizeGitURL(templates[i].Spec.URLPrefix)
		if uk == "" || pk == "" || !strings.HasPrefix(uk, pk) {
			continue
		}
		if len(pk) > bestLen {
			best, bestLen = &templates[i], len(pk)
		}
	}
	return best
}

// transportOverrides lists the settings of spec that would carry credentials inherited
// from t somewhere t does not send them: another proxy, or unverified TLS / host keys /
// plain HTTP (the repository library takes the receiving repository's proxy and insecure
// flags with a template's credentials).
func transportOverrides(spec *v1alpha1.RepositorySpec, t *v1alpha1.RepositoryCredential) []string {
	var out []string
	if spec.Proxy != "" && spec.Proxy != t.Spec.Proxy {
		out = append(out, "spec.proxy")
	}
	if spec.Insecure {
		out = append(out, "spec.insecure")
	}
	if spec.InsecureIgnoreHostKey { //nolint:staticcheck // deprecated upstream but honored by the library (IsInsecure)
		out = append(out, "spec.insecureIgnoreHostKey")
	}
	if spec.InsecureOCIForceHTTP && !t.Spec.InsecureOCIForceHTTP {
		out = append(out, "spec.insecureOCIForceHTTP")
	}
	return out
}

// checkInheritedTransport refuses a repository (to create, update or validate) that would
// inherit a credential template's credentials while sending them through its own proxy or
// with weaker TLS than the template: a caller allowed to create a repository under a
// template's prefix (for example a project role) must not be able to route an
// organization-wide password through a proxy it chose with TLS verification off. Callers
// allowed to update the template may (they could change the template itself). data are the
// credential keys the repository will have.
func (s *Service) checkInheritedTransport(ctx context.Context, kc KubeClient, p purpose, spec *v1alpha1.RepositorySpec, data map[string][]byte) error {
	if spec.Proxy == "" && !spec.Insecure && !spec.InsecureIgnoreHostKey && !spec.InsecureOCIForceHTTP { //nolint:staticcheck // see transportOverrides
		return nil
	}
	owns, err := s.ownsCredentials(ctx, kc, &spec.RepositoryAccess, data)
	if err != nil || owns {
		return err
	}
	templates, err := s.templatesOf(ctx, kc, p)
	if err != nil {
		return err
	}
	t := inheritedTemplate(templates, spec.URL)
	if t == nil {
		return nil
	}
	fields := transportOverrides(spec, t)
	if len(fields) == 0 || rest.Can(ctx, p.resource, rbac.ActionUpdate, t.Spec.URLPrefix) {
		return nil
	}
	a := rest.AuditFrom(ctx)
	a.SetCheck(p.resource, rbac.ActionUpdate, redactURLPassword(t.Spec.URLPrefix))
	a.SetReason("inherited credentials with overridden transport")
	a.Record()
	return rest.PermissionDenied("permission denied: the repository would use the credentials of a credential template with its own %s; "+
		"send credentials of its own or keep the template's proxy and TLS settings", strings.Join(fields, ", "))
}
