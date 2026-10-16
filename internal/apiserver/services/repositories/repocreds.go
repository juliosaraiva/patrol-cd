package repositories

import (
	"context"
	"maps"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func credOwner(c *v1alpha1.RepositoryCredential, p purpose) secretOwner {
	return secretOwner{kind: "RepositoryCredential", name: c.Name, uid: c.UID, secretType: p.credSecretType()}
}

// samePrefix compares template URL prefixes (normalized; raw when they cannot be
// normalized).
func samePrefix(a, b string) bool { return sameURL(a, b) }

// exactTemplate returns the template of purpose p whose URL prefix is url, or nil. Unlike
// upstream (longest-prefix match), update and delete address exactly one template, so that
// the RBAC check on the URL of the request covers the object acted on.
func (s *Service) exactTemplate(ctx context.Context, kc KubeClient, p purpose, url string) (*v1alpha1.RepositoryCredential, error) {
	templates, err := s.templatesOf(ctx, kc, p)
	if err != nil {
		return nil, err
	}
	for i := range templates {
		if samePrefix(templates[i].Spec.URLPrefix, url) {
			return &templates[i], nil
		}
	}
	return nil, nil
}

// username returns the stored user name of a template (not a secret; upstream lists it).
func (s *Service) username(ctx context.Context, kc KubeClient, c *v1alpha1.RepositoryCredential, p purpose) string {
	data, err := s.storedCredentials(ctx, kc, credOwner(c, p), c.Spec.CredentialsRef)
	if err != nil {
		return ""
	}
	return string(data[v1alpha1.RepositorySecretKeyUsername])
}

func (s *Service) credInfo(ctx context.Context, kc KubeClient, p purpose, c *v1alpha1.RepositoryCredential) *apiclient.RepositoryCredentialInfo {
	out := credentialInfo(c, s.username(ctx, kc, c, p))
	return &out
}

func (s *Service) listCredentials(ctx context.Context, p purpose) (*apiclient.RepositoryCredentialInfoList, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	templates, err := s.templatesOf(ctx, kc, p)
	if err != nil {
		return nil, err
	}
	out := &apiclient.RepositoryCredentialInfoList{Items: []apiclient.RepositoryCredentialInfo{}}
	for i := range templates {
		c := &templates[i]
		if !rest.Can(ctx, p.resource, rbac.ActionGet, c.Spec.URLPrefix) {
			continue
		}
		out.Items = append(out.Items, *s.credInfo(ctx, kc, p, c))
	}
	return out, nil
}

// prepareCredentialWrite validates a template body and returns the normalized spec.
func (s *Service) prepareCredentialWrite(ctx context.Context, kc KubeClient, p purpose, body *apiclient.RepositoryCredentialWriteRequest, currentRef *v1alpha1.LocalSecretReference, currentSCM *v1alpha1.SCMProviderReference) (v1alpha1.RepositoryCredentialSpec, error) {
	if err := validateTypeMeta(body.APIVersion, body.Kind, "RepositoryCredential"); err != nil {
		return v1alpha1.RepositoryCredentialSpec{}, err
	}
	spec := *body.Spec.DeepCopy()
	if err := normalizeCredentialSpec(&spec, p); err != nil {
		return spec, err
	}
	if ref := spec.CredentialsRef; ref != nil && (currentRef == nil || ref.Name != currentRef.Name) {
		return spec, rest.InvalidArgument("spec.credentialsRef is managed by the server; send credentials instead")
	}
	spec.CredentialsRef = currentRef
	if err := validateCredentials(body.Credentials, &spec.RepositoryAccess); err != nil {
		return spec, err
	}
	if err := s.checkSCMProviderRef(ctx, kc, spec.SCMProviderRef, currentSCM); err != nil {
		return spec, err
	}
	return spec, nil
}

func (s *Service) createCredential(ctx context.Context, p purpose, req *apiclient.RepositoryCredentialCreateRequest) (*apiclient.RepositoryCredentialInfo, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSpace(req.Body.Spec.URLPrefix)
	if err := authorize(ctx, p.resource, rbac.ActionCreate, prefix); err != nil {
		return nil, err
	}
	spec, err := s.prepareCredentialWrite(ctx, kc, p, &req.Body, nil, nil)
	if err != nil {
		return nil, err
	}
	return s.storeNewTemplate(ctx, kc, p, spec, req.Body.Credentials, req.Upsert)
}

// storeNewTemplate creates a template, or handles an existing one with the same prefix
// (identical: unchanged; upsert: updated; otherwise "spec is different").
func (s *Service) storeNewTemplate(ctx context.Context, kc KubeClient, p purpose, spec v1alpha1.RepositoryCredentialSpec, creds *apiclient.RepositoryCredentials, upsert bool) (*apiclient.RepositoryCredentialInfo, error) {
	existing, err := s.exactTemplate(ctx, kc, p, spec.URLPrefix)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return s.createExistingTemplate(ctx, kc, p, existing, spec, creds, upsert)
	}
	if !p.write {
		// Upstream refuses a read template whose URL is registered as an unscoped read
		// repository.
		if repo, err := s.exactRepository(ctx, kc, p, spec.URLPrefix, ""); err != nil {
			return nil, err
		} else if repo != nil {
			return nil, rest.AlreadyExists("%s %q already exists", p.credNoun(), sanitizeURL(spec.URLPrefix))
		}
	}
	name := objectName(p.credPrefix(), spec.URLPrefix, "")
	obj := &v1alpha1.RepositoryCredential{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.cfg.Namespace},
		Spec:       spec,
	}
	data := credentialData(creds)
	if len(data) > 0 {
		obj.Spec.CredentialsRef = &v1alpha1.LocalSecretReference{Name: managedSecretName(name)}
	}
	if err := kc.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if existing, gerr := s.exactTemplate(ctx, kc, p, spec.URLPrefix); gerr == nil && existing != nil {
				return s.createExistingTemplate(ctx, kc, p, existing, spec, creds, upsert)
			}
			return nil, rest.AlreadyExists("%s %q already exists", p.credNoun(), sanitizeURL(spec.URLPrefix)).WithCause(err)
		}
		return nil, err
	}
	target(ctx, "RepositoryCredential", obj.Namespace, obj.Name, obj.UID)
	if len(data) > 0 {
		if err := s.storeNewCredentials(ctx, kc, credOwner(obj, p), creds, obj.Spec.CredentialsRef); err != nil {
			if derr := kc.Delete(ctx, obj, client.Preconditions{UID: &obj.UID}); derr != nil && !apierrors.IsNotFound(derr) {
				s.log.ErrorContext(ctx, "could not roll back credential template after a credentials failure", "template", obj.Name, "error", derr)
			}
			return nil, err
		}
	}
	rest.AuditFrom(ctx).SetMessage("created %s %s", p.credNoun(), sanitizeURL(spec.URLPrefix))
	return s.credInfo(ctx, kc, p, obj), nil
}

func comparableCredSpec(spec v1alpha1.RepositoryCredentialSpec, p purpose) v1alpha1.RepositoryCredentialSpec {
	out := *spec.DeepCopy()
	out.CredentialsRef = nil
	if out.Type == "" {
		out.Type = v1alpha1.RepositoryTypeGit
	}
	if out.Purpose == "" {
		out.Purpose = p.value
	}
	return out
}

func (s *Service) createExistingTemplate(ctx context.Context, kc KubeClient, p purpose, existing *v1alpha1.RepositoryCredential, spec v1alpha1.RepositoryCredentialSpec, creds *apiclient.RepositoryCredentials, upsert bool) (*apiclient.RepositoryCredentialInfo, error) {
	target(ctx, "RepositoryCredential", existing.Namespace, existing.Name, existing.UID)
	stored, err := s.storedCredentials(ctx, kc, credOwner(existing, p), existing.Spec.CredentialsRef)
	if err != nil {
		return nil, err
	}
	diff := specDiff(comparableCredSpec(existing.Spec, p), comparableCredSpec(spec, p))
	if !maps.EqualFunc(stored, credentialData(creds), func(a, b []byte) bool { return string(a) == string(b) }) {
		diff = append(diff, "credentials")
	}
	switch {
	case len(diff) == 0:
		return s.credInfo(ctx, kc, p, existing), nil
	case upsert:
		if err := authorize(ctx, p.resource, rbac.ActionUpdate, existing.Spec.URLPrefix); err != nil {
			return nil, err
		}
		return s.applyTemplateUpdate(ctx, kc, p, existing, spec, creds, existing.ResourceVersion)
	default:
		return nil, specIsDifferent(p.credNoun(), diff)
	}
}

// applyTemplateUpdate stores spec (and creds when non-nil) on existing, in the order of
// applyUpdate: the template first, under resourceVersion, then its managed Secret.
func (s *Service) applyTemplateUpdate(ctx context.Context, kc KubeClient, p purpose, existing *v1alpha1.RepositoryCredential, spec v1alpha1.RepositoryCredentialSpec, creds *apiclient.RepositoryCredentials, resourceVersion string) (*apiclient.RepositoryCredentialInfo, error) {
	obj := existing.DeepCopy()
	owner := credOwner(obj, p)
	var plan *credentialPlan
	if creds != nil {
		var err error
		if plan, err = s.planCredentials(ctx, kc, owner, existing.Spec.CredentialsRef, creds); err != nil {
			return nil, err
		}
		spec.CredentialsRef = plan.ref()
	} else {
		spec.CredentialsRef = existing.Spec.CredentialsRef
	}
	obj.Spec = spec
	obj.ResourceVersion = resourceVersion
	if err := kc.Update(ctx, obj); err != nil {
		return nil, err
	}
	target(ctx, "RepositoryCredential", obj.Namespace, obj.Name, obj.UID)
	if plan != nil {
		if _, err := s.applyCredentials(ctx, kc, plan); err != nil {
			restored := obj.DeepCopy()
			restored.Spec = *existing.Spec.DeepCopy()
			if rerr := kc.Update(ctx, restored); rerr != nil {
				s.log.ErrorContext(ctx, "could not restore credential template after a credentials failure", "template", obj.Name, "error", rerr)
			}
			return nil, err
		}
	}
	if old := existing.Spec.CredentialsRef; old != nil && (spec.CredentialsRef == nil || spec.CredentialsRef.Name != old.Name) {
		if err := s.deleteManagedSecret(ctx, kc, owner, old); err != nil {
			s.log.WarnContext(ctx, "could not delete a credentials Secret that is no longer referenced", "template", obj.Name, "error", err)
		}
	}
	rest.AuditFrom(ctx).SetMessage("updated %s %s", p.credNoun(), sanitizeURL(spec.URLPrefix))
	return s.credInfo(ctx, kc, p, obj), nil
}

func (s *Service) updateCredential(ctx context.Context, p purpose, req *apiclient.RepositoryCredentialUpdateRequest) (*apiclient.RepositoryCredentialInfo, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	body := &req.Body
	if strings.TrimSpace(body.Spec.URLPrefix) == "" {
		body.Spec.URLPrefix = req.URL
	}
	if !samePrefix(body.Spec.URLPrefix, req.URL) {
		return nil, rest.InvalidArgument("spec.urlPrefix must equal the URL of the path")
	}
	existing, err := s.exactTemplate(ctx, kc, p, req.URL)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, rest.NotFound("%s '%s' not found", p.credNoun(), sanitizeURL(req.URL))
	}
	spec, err := s.prepareCredentialWrite(ctx, kc, p, body, existing.Spec.CredentialsRef, existing.Spec.SCMProviderRef)
	if err != nil {
		return nil, err
	}
	if rv := body.Metadata.ResourceVersion; rv != "" && rv != existing.ResourceVersion {
		return nil, rest.Conflict("the credential template has been modified (resourceVersion %s, request has %s); read it again and retry", existing.ResourceVersion, rv)
	}
	return s.applyTemplateUpdate(ctx, kc, p, existing, spec, body.Credentials, existing.ResourceVersion)
}

func (s *Service) deleteCredential(ctx context.Context, p purpose, req *apiclient.RepositoryCredentialQuery) (*apiclient.Empty, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	existing, err := s.exactTemplate(ctx, kc, p, req.URL)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, rest.NotFound("%s '%s' not found", p.credNoun(), sanitizeURL(req.URL))
	}
	target(ctx, "RepositoryCredential", existing.Namespace, existing.Name, existing.UID)
	if err := kc.Delete(ctx, existing, client.Preconditions{UID: &existing.UID}); err != nil {
		return nil, err
	}
	if err := s.deleteManagedSecret(ctx, kc, credOwner(existing, p), existing.Spec.CredentialsRef); err != nil {
		s.log.WarnContext(ctx, "could not delete the credentials Secret of a deleted template", "template", existing.Name, "error", err)
	}
	rest.AuditFrom(ctx).SetMessage("deleted %s %s", p.credNoun(), sanitizeURL(existing.Spec.URLPrefix))
	return &apiclient.Empty{}, nil
}

// createTemplateFromRepository serves POST /repositories?credsOnly=true: the repository
// body becomes a credential template whose urlPrefix is spec.url. The response describes
// the template in the repository shape (metadata of the RepositoryCredential, spec.url =
// urlPrefix).
func (s *Service) createTemplateFromRepository(ctx context.Context, kc KubeClient, p purpose, req *apiclient.RepositoryCreateRequest) (*apiclient.RepositoryInfo, error) {
	b := &req.Body
	if b.Spec.Project != "" {
		return nil, rest.InvalidArgument("credential templates cannot be scoped to a project (credsOnly with spec.project)")
	}
	if err := validateTypeMeta(b.APIVersion, b.Kind, "Repository"); err != nil {
		return nil, err
	}
	body := apiclient.RepositoryCredentialWriteRequest{
		Spec: v1alpha1.RepositoryCredentialSpec{
			URLPrefix: b.Spec.URL, Type: b.Spec.Type, Purpose: b.Spec.Purpose, RepositoryAccess: *b.Spec.RepositoryAccess.DeepCopy(),
		},
		Credentials: b.Credentials,
	}
	spec, err := s.prepareCredentialWrite(ctx, kc, p, &body, nil, nil)
	if err != nil {
		return nil, err
	}
	c, err := s.storeNewTemplate(ctx, kc, p, spec, b.Credentials, req.Upsert)
	if err != nil {
		return nil, err
	}
	return &apiclient.RepositoryInfo{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "RepositoryCredential"},
		Metadata: c.Metadata,
		Spec: v1alpha1.RepositorySpec{
			URL: c.Spec.URLPrefix, Type: c.Spec.Type, Purpose: c.Spec.Purpose, RepositoryAccess: c.Spec.RepositoryAccess,
		},
		Status:          c.Status,
		ConnectionState: v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusUnknown},
		HasCredentials:  c.HasCredentials,
	}, nil
}
