package repositories

import (
	"context"
	"maps"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

var scmProviderTypes = []v1alpha1.SCMProviderType{
	v1alpha1.SCMProviderTypeGitHub, v1alpha1.SCMProviderTypeGitLab, v1alpha1.SCMProviderTypeGitea,
	v1alpha1.SCMProviderTypeBitbucketCloud, v1alpha1.SCMProviderTypeBitbucketServer,
	v1alpha1.SCMProviderTypeAzureDevOps, v1alpha1.SCMProviderTypeAWSCodeCommit, v1alpha1.SCMProviderTypeFake,
}

func validateSecretKeyRef(field string, ref *v1alpha1.SecretKeyReference) error {
	if ref == nil {
		return nil
	}
	if err := validateDNSName(field+".name", ref.Name); err != nil {
		return err
	}
	if errs := validation.IsConfigMapKey(ref.Key); len(errs) > 0 {
		return invalid("%s.key %q is invalid: %s", field, ref.Key, strings.Join(errs, "; "))
	}
	return nil
}

// validateSCMProviderSpec checks a provider spec against the CRD rules (before the API
// server's admission, for 400 answers with field names).
func validateSCMProviderSpec(spec *v1alpha1.SCMProviderSpec) error {
	if !slices.Contains(scmProviderTypes, spec.Type) {
		return invalid("spec.type %q is invalid", spec.Type)
	}
	if err := validateHTTPURL("spec.apiURL", spec.APIURL); err != nil {
		return err
	}
	if err := validateHTTPURL("spec.webURL", spec.WebURL); err != nil {
		return err
	}
	if err := validateSecretKeyRef("spec.caSecretRef", spec.CASecretRef); err != nil {
		return err
	}
	if a := spec.Auth; a != nil {
		n := 0
		if a.GitHubApp != nil {
			n++
			if a.GitHubApp.AppID < 1 {
				return invalid("spec.auth.githubApp.appID must be at least 1")
			}
			if a.GitHubApp.InstallationID != nil && *a.GitHubApp.InstallationID < 1 {
				return invalid("spec.auth.githubApp.installationID must be at least 1 when set")
			}
			if err := validateSecretKeyRef("spec.auth.githubApp.privateKeySecretRef", &a.GitHubApp.PrivateKeySecretRef); err != nil {
				return err
			}
		}
		if a.TokenSecretRef != nil {
			n++
			if err := validateSecretKeyRef("spec.auth.tokenSecretRef", a.TokenSecretRef); err != nil {
				return err
			}
		}
		if a.BasicAuthSecretRef != nil {
			n++
			if err := validateDNSName("spec.auth.basicAuthSecretRef.name", a.BasicAuthSecretRef.Name); err != nil {
				return err
			}
		}
		if n > 1 {
			return invalid("at most one of githubApp, tokenSecretRef or basicAuthSecretRef may be set")
		}
	}
	if w := spec.Webhook; w != nil {
		if err := validateSecretKeyRef("spec.webhook.secretRef", &w.SecretRef); err != nil {
			return err
		}
	}
	if r := spec.StatusReporting; r != nil {
		switch r.Mode {
		case "", v1alpha1.StatusReportingModeStatuses:
		case v1alpha1.StatusReportingModeChecks:
			if spec.Type != v1alpha1.SCMProviderTypeGitHub || spec.Auth == nil || spec.Auth.GitHubApp == nil {
				return invalid("statusReporting.mode checks requires type github with auth.githubApp")
			}
		default:
			return invalid("spec.statusReporting.mode %q is invalid (checks or statuses)", r.Mode)
		}
		if err := checkText("spec.statusReporting.contextPrefix", r.ContextPrefix, 100, false); err != nil {
			return err
		}
	}
	if rl := spec.RateLimit; rl != nil {
		if rl.RequestsPerSecond < 1 {
			return invalid("spec.rateLimit.requestsPerSecond must be at least 1")
		}
		if rl.Burst < 0 {
			return invalid("spec.rateLimit.burst must not be negative")
		}
	}
	return nil
}

func validateObjectLabels(m metav1.ObjectMeta) error {
	for k, v := range m.Labels {
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			return invalid("label key %q is invalid", k)
		}
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			return invalid("label value of %q is invalid", k)
		}
	}
	for k := range m.Annotations {
		if errs := validation.IsQualifiedName(strings.ToLower(k)); len(errs) > 0 {
			return invalid("annotation key %q is invalid", k)
		}
	}
	return nil
}

func scmProviderView(sp *v1alpha1.SCMProvider) v1alpha1.SCMProvider {
	out := *sp.DeepCopy()
	out.TypeMeta = metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "SCMProvider"}
	out.ObjectMeta = cleanMeta(out.ObjectMeta)
	out.Spec.APIURL = redactURLPassword(out.Spec.APIURL)
	out.Spec.WebURL = redactURLPassword(out.Spec.WebURL)
	return out
}

func (s *Service) listSCMProviders(ctx context.Context, _ *apiclient.Empty) (*v1alpha1.SCMProviderList, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	var list v1alpha1.SCMProviderList
	if err := kc.List(ctx, &list, client.InNamespace(s.cfg.Namespace)); err != nil {
		return nil, err
	}
	out := &v1alpha1.SCMProviderList{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "SCMProviderList"},
		ListMeta: metav1.ListMeta{ResourceVersion: list.ResourceVersion},
		Items:    []v1alpha1.SCMProvider{},
	}
	for i := range list.Items {
		if rest.Can(ctx, ResourceSCMProviders, rbac.ActionGet, list.Items[i].Name) {
			out.Items = append(out.Items, scmProviderView(&list.Items[i]))
		}
	}
	slices.SortFunc(out.Items, func(a, b v1alpha1.SCMProvider) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (s *Service) getSCMProvider(ctx context.Context, req *apiclient.SCMProviderQuery) (*v1alpha1.SCMProvider, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	sp := &v1alpha1.SCMProvider{}
	if err := kc.Get(ctx, client.ObjectKey{Namespace: s.cfg.Namespace, Name: req.Name}, sp); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, rest.NotFound("SCM provider %q not found", req.Name)
		}
		return nil, err
	}
	out := scmProviderView(sp)
	return &out, nil
}

func (s *Service) createSCMProvider(ctx context.Context, req *apiclient.SCMProviderCreateRequest) (*v1alpha1.SCMProvider, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	b := &req.Body
	name := b.Metadata.Name
	if name == "" {
		return nil, rest.InvalidArgument("metadata.name is required")
	}
	if err := validateDNSName("metadata.name", name); err != nil {
		return nil, err
	}
	if err := rest.Authorize(ctx, ResourceSCMProviders, rbac.ActionCreate, name); err != nil {
		return nil, err
	}
	if err := validateTypeMeta(b.APIVersion, b.Kind, "SCMProvider"); err != nil {
		return nil, err
	}
	if b.Metadata.Namespace != "" && b.Metadata.Namespace != s.cfg.Namespace {
		return nil, rest.InvalidArgument("metadata.namespace must be empty or %q", s.cfg.Namespace)
	}
	if err := validateObjectLabels(b.Metadata); err != nil {
		return nil, err
	}
	if err := validateSCMProviderSpec(&b.Spec); err != nil {
		return nil, err
	}
	sp := &v1alpha1.SCMProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: s.cfg.Namespace,
			Labels: maps.Clone(b.Metadata.Labels), Annotations: maps.Clone(b.Metadata.Annotations),
		},
		Spec: *b.Spec.DeepCopy(),
	}
	if err := kc.Create(ctx, sp); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, rest.AlreadyExists("SCM provider %q already exists", name).WithCause(err)
		}
		return nil, err
	}
	target(ctx, "SCMProvider", sp.Namespace, sp.Name, sp.UID)
	out := scmProviderView(sp)
	return &out, nil
}

func (s *Service) updateSCMProvider(ctx context.Context, req *apiclient.SCMProviderUpdateRequest) (*v1alpha1.SCMProvider, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	b := &req.Body
	if b.Metadata.Name != "" && b.Metadata.Name != req.Name {
		return nil, rest.InvalidArgument("metadata.name must match the name of the path")
	}
	if err := validateTypeMeta(b.APIVersion, b.Kind, "SCMProvider"); err != nil {
		return nil, err
	}
	if err := validateObjectLabels(b.Metadata); err != nil {
		return nil, err
	}
	if err := validateSCMProviderSpec(&b.Spec); err != nil {
		return nil, err
	}
	sp := &v1alpha1.SCMProvider{}
	if err := kc.Get(ctx, client.ObjectKey{Namespace: s.cfg.Namespace, Name: req.Name}, sp); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, rest.NotFound("SCM provider %q not found", req.Name)
		}
		return nil, err
	}
	if rv := b.Metadata.ResourceVersion; rv != "" && rv != sp.ResourceVersion {
		return nil, rest.Conflict("the SCM provider has been modified (resourceVersion %s, request has %s); read it again and retry", sp.ResourceVersion, rv)
	}
	sp.Spec = *b.Spec.DeepCopy()
	sp.Labels = maps.Clone(b.Metadata.Labels)
	sp.Annotations = maps.Clone(b.Metadata.Annotations)
	if err := kc.Update(ctx, sp); err != nil {
		return nil, err
	}
	target(ctx, "SCMProvider", sp.Namespace, sp.Name, sp.UID)
	out := scmProviderView(sp)
	return &out, nil
}

func (s *Service) deleteSCMProvider(ctx context.Context, req *apiclient.SCMProviderQuery) (*apiclient.Empty, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	sp := &v1alpha1.SCMProvider{}
	if err := kc.Get(ctx, client.ObjectKey{Namespace: s.cfg.Namespace, Name: req.Name}, sp); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, rest.NotFound("SCM provider %q not found", req.Name)
		}
		return nil, err
	}
	target(ctx, "SCMProvider", sp.Namespace, sp.Name, sp.UID)
	if err := kc.Delete(ctx, sp, client.Preconditions{UID: &sp.UID}); err != nil {
		return nil, err
	}
	return &apiclient.Empty{}, nil
}
