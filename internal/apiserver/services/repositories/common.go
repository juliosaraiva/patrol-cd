package repositories

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// purpose distinguishes read repositories (resource repositories) from write
// repositories (resource write-repositories, source hydrator).
type purpose struct {
	value    v1alpha1.RepositoryPurpose
	resource string
	write    bool
}

var (
	readPurpose  = purpose{value: v1alpha1.RepositoryPurposeRead, resource: rbac.ResourceRepositories}
	writePurpose = purpose{value: v1alpha1.RepositoryPurposeWrite, resource: rbac.ResourceWriteRepositories, write: true}
)

func (p purpose) repoPrefix() string {
	if p.write {
		return "repo-write"
	}
	return "repo"
}

func (p purpose) credPrefix() string {
	if p.write {
		return "creds-write"
	}
	return "creds"
}

func (p purpose) repoSecretType() string {
	if p.write {
		return v1alpha1.LabelValueSecretTypeRepositoryWrite
	}
	return v1alpha1.LabelValueSecretTypeRepository
}

func (p purpose) credSecretType() string {
	if p.write {
		return v1alpha1.LabelValueSecretTypeRepoCredsWrite
	}
	return v1alpha1.LabelValueSecretTypeRepoCreds
}

// matches reports whether a stored purpose (CRD default read) is p.
func (p purpose) matches(v v1alpha1.RepositoryPurpose) bool {
	if v == "" {
		v = v1alpha1.RepositoryPurposeRead
	}
	return v == p.value
}

func (p purpose) repoNoun() string {
	if p.write {
		return "write repository"
	}
	return "repository"
}

func (p purpose) credNoun() string {
	if p.write {
		return "write repository credentials"
	}
	return "repository credentials"
}

// errHydratorDisabled is upstream's Unimplemented "hydrator is disabled".
var errHydratorDisabled = &rest.Error{Status: http.StatusNotImplemented, Code: apiclient.CodeFailedPrecondition, Detail: "hydrator is disabled"}

// gate refuses write repository routes unless the hydrator is enabled.
func (s *Service) gate(p purpose) error {
	if p.write && !s.cfg.HydratorEnabled {
		return errHydratorDisabled
	}
	return nil
}

// rbacObject is upstream createRBACObject: "<project>/<url>", or "<url>" when unscoped.
func rbacObject(projectName, url string) string {
	if projectName != "" {
		return projectName + "/" + url
	}
	return url
}

// urlKey is the identity of a repository URL (normalized, else trimmed).
func urlKey(raw string) string {
	if n := project.NormalizeGitURL(raw); n != "" {
		return n
	}
	return strings.TrimSpace(raw)
}

// sameURL is upstream git.SameURL as the repository library applies it.
func sameURL(a, b string) bool {
	ka, kb := urlKey(a), urlKey(b)
	return ka != "" && ka == kb
}

// objectName derives the deterministic object name of an API-created repository or
// template (upstream RepoURLToSecretName: "<prefix>-<hash>"; the formula is not stable
// and is never used for lookups).
func objectName(prefix, url, projectName string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(urlKey(url)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(projectName))
	return fmt.Sprintf("%s-%016x", prefix, h.Sum64())
}

// managedSecretName is the name of the credentials Secret the API manages for an object.
func managedSecretName(owner string) string {
	const suffix = "-creds"
	if len(owner)+len(suffix) <= 253 {
		return owner + suffix
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(owner))
	return fmt.Sprintf("creds-%016x", h.Sum64())
}

// cleanMeta returns metadata safe to return: managed fields and the kubectl
// last-applied-configuration annotation are removed.
func cleanMeta(m metav1.ObjectMeta) metav1.ObjectMeta {
	out := *m.DeepCopy()
	out.ManagedFields = nil
	if _, ok := out.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; ok {
		delete(out.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
		if len(out.Annotations) == 0 {
			out.Annotations = nil
		}
	}
	return out
}

// accessHasCredentials reports whether an object has credentials of its own.
func accessHasCredentials(a *v1alpha1.RepositoryAccess) bool {
	return a.CredentialsRef != nil || a.SCMProviderRef != nil || a.UseAzureWorkloadIdentity
}

// redactURLPassword masks the password of the user information of a URL ("******"),
// keeping every other byte as stored. It works on the authority text (after "://", up to
// the first "/", "?" or "#", user information ending at its last "@") rather than a parsed
// URL, so values url.Parse rejects are masked too and nothing else is re-encoded; an RBAC
// object "<project>/<url>" is masked the same way. The API refuses such URLs on writes, but
// objects created declaratively may carry them.
func redactURLPassword(s string) string {
	i := strings.Index(s, "://")
	if i < 0 {
		return s
	}
	authStart := i + 3
	tail := s[authStart:]
	end := strings.IndexAny(tail, "/?#")
	if end < 0 {
		end = len(tail)
	}
	at := strings.LastIndex(tail[:end], "@")
	if at < 0 {
		return s
	}
	colon := strings.Index(tail[:at], ":")
	if colon < 0 {
		return s
	}
	return s[:authStart] + tail[:colon+1] + "******" + tail[at:]
}

// redactAccess masks URL passwords in the URL-valued settings of a response.
func redactAccess(a *v1alpha1.RepositoryAccess) {
	a.Proxy = redactURLPassword(a.Proxy)
	if a.GitHubApp != nil {
		a.GitHubApp.EnterpriseBaseURL = redactURLPassword(a.GitHubApp.EnterpriseBaseURL)
	}
	if a.AzureServicePrincipal != nil {
		a.AzureServicePrincipal.ActiveDirectoryEndpoint = redactURLPassword(a.AzureServicePrincipal.ActiveDirectoryEndpoint)
	}
}

// repositoryInfo converts a stored Repository. URL passwords of declaratively created
// objects are masked.
func repositoryInfo(r *v1alpha1.Repository, inherited bool) apiclient.RepositoryInfo {
	out := apiclient.RepositoryInfo{
		TypeMeta:       metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Repository"},
		Metadata:       cleanMeta(r.ObjectMeta),
		Spec:           *r.Spec.DeepCopy(),
		Status:         *r.Status.DeepCopy(),
		HasCredentials: accessHasCredentials(&r.Spec.RepositoryAccess),
	}
	out.Spec.URL = redactURLPassword(out.Spec.URL)
	redactAccess(&out.Spec.RepositoryAccess)
	out.InheritedCredentials = !out.HasCredentials && inherited
	if cs := r.Status.ConnectionState; cs != nil {
		out.ConnectionState = *cs.DeepCopy()
	} else {
		out.ConnectionState = v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusUnknown}
	}
	return out
}

// credentialInfo converts a stored RepositoryCredential (URL passwords masked).
func credentialInfo(c *v1alpha1.RepositoryCredential, username string) apiclient.RepositoryCredentialInfo {
	out := apiclient.RepositoryCredentialInfo{
		TypeMeta:       metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "RepositoryCredential"},
		Metadata:       cleanMeta(c.ObjectMeta),
		Spec:           *c.Spec.DeepCopy(),
		Status:         *c.Status.DeepCopy(),
		HasCredentials: accessHasCredentials(&c.Spec.RepositoryAccess),
		Username:       username,
	}
	out.Spec.URLPrefix = redactURLPassword(out.Spec.URLPrefix)
	redactAccess(&out.Spec.RepositoryAccess)
	return out
}

// templateMatches reports whether a template prefix applies to url (the repository
// library's longest-prefix rule on normalized forms).
func templateMatches(prefix, url string) bool {
	pk, uk := project.NormalizeGitURL(prefix), project.NormalizeGitURL(url)
	return pk != "" && uk != "" && strings.HasPrefix(uk, pk)
}

func hasTemplate(creds []v1alpha1.RepositoryCredential, url string) bool {
	return slices.ContainsFunc(creds, func(c v1alpha1.RepositoryCredential) bool { return templateMatches(c.Spec.URLPrefix, url) })
}

// target records the object a mutation acted on in the audit record.
func target(ctx context.Context, kind, namespace, name string, uid types.UID) {
	rest.AuditFrom(ctx).SetTarget(rest.ObjectRef{
		APIVersion: v1alpha1.GroupVersion.String(), Kind: kind, Namespace: namespace, Name: name, UID: uid,
	})
}

// denyGeneric answers a request for an object that does not exist or that the caller may
// not see with the same 403 (no existence oracle; upstream PermissionDeniedAPIError). The
// audit record names the request's own coordinates.
func denyGeneric(ctx context.Context, resource, action, object string) error {
	a := rest.AuditFrom(ctx)
	a.SetCheck(resource, action, redactURLPassword(object))
	a.SetReason("not found or permission denied")
	a.Record()
	return rest.PermissionDenied("permission denied")
}

// authorize is rest.Authorize for RBAC objects built from stored URLs: policies are
// matched against the object as stored, while a URL password (declarative objects) is
// masked in the 403 detail and the audit record.
func authorize(ctx context.Context, resource, action, object string) error {
	safe := redactURLPassword(object)
	if safe == object {
		return rest.Authorize(ctx, resource, action, object)
	}
	if rest.Can(ctx, resource, action, object) {
		return nil
	}
	a := rest.AuditFrom(ctx)
	a.SetCheck(resource, action, safe)
	a.SetReason("permission denied")
	a.Record()
	return rest.PermissionDenied("permission denied: %s, %s, %s", resource, action, safe)
}

// sanitizeURL removes credentials from a URL for logs and messages.
func sanitizeURL(u string) string { return git.SanitizeURL(u) }
