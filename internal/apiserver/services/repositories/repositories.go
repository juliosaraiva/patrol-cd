package repositories

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// maxListLimit bounds ?limit= of list requests.
const maxListLimit = 1000

func repoOwner(r *v1alpha1.Repository, p purpose) secretOwner {
	return secretOwner{kind: "Repository", name: r.Name, uid: r.UID, secretType: p.repoSecretType()}
}

// listOfPurpose lists the Repository objects of purpose p (all pages unless opts paginate).
func (s *Service) listOfPurpose(ctx context.Context, kc KubeClient, p purpose, opts ...client.ListOption) ([]v1alpha1.Repository, metav1.ListMeta, error) {
	var list v1alpha1.RepositoryList
	if err := kc.List(ctx, &list, append([]client.ListOption{client.InNamespace(s.cfg.Namespace)}, opts...)...); err != nil {
		return nil, metav1.ListMeta{}, err
	}
	out := list.Items[:0]
	for _, r := range list.Items {
		if p.matches(r.Spec.Purpose) {
			out = append(out, r)
		}
	}
	return out, list.ListMeta, nil
}

// reposForURL returns the Repository objects of purpose p registered for url, by name.
func (s *Service) reposForURL(ctx context.Context, kc KubeClient, p purpose, url string) ([]v1alpha1.Repository, error) {
	all, _, err := s.listOfPurpose(ctx, kc, p)
	if err != nil {
		return nil, err
	}
	var out []v1alpha1.Repository
	for _, r := range all {
		if sameURL(r.Spec.URL, url) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b v1alpha1.Repository) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// templatesOf lists the RepositoryCredential objects of purpose p.
func (s *Service) templatesOf(ctx context.Context, kc KubeClient, p purpose) ([]v1alpha1.RepositoryCredential, error) {
	var list v1alpha1.RepositoryCredentialList
	if err := kc.List(ctx, &list, client.InNamespace(s.cfg.Namespace)); err != nil {
		return nil, err
	}
	out := list.Items[:0]
	for _, c := range list.Items {
		if p.matches(c.Spec.Purpose) {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b v1alpha1.RepositoryCredential) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// lookupVisible finds the repository a request addresses, the way upstream getRepository
// does: among the repositories of the URL the caller may get, the only one when no project
// is given, else the one scoped to appProject. No visible repository is the generic 403
// (missing and forbidden look the same).
func (s *Service) lookupVisible(ctx context.Context, kc KubeClient, p purpose, action, url, appProject string) (*v1alpha1.Repository, error) {
	repos, err := s.reposForURL(ctx, kc, p, url)
	if err != nil {
		return nil, err
	}
	var visible []v1alpha1.Repository
	for _, r := range repos {
		if rest.Can(ctx, p.resource, rbac.ActionGet, rbacObject(r.Spec.Project, r.Spec.URL)) {
			visible = append(visible, r)
		}
	}
	if len(visible) == 0 {
		return nil, denyGeneric(ctx, p.resource, action, rbacObject(appProject, url))
	}
	if len(visible) == 1 && appProject == "" {
		return &visible[0], nil
	}
	for i := range visible {
		if visible[i].Spec.Project == appProject {
			return &visible[i], nil
		}
	}
	return nil, rest.NotFound("repository not found for url %q and project %q", sanitizeURL(url), appProject)
}

// needsCheck reports whether a repository's recorded connection state must be refreshed:
// none recorded, recorded for another generation, or older than the TTL.
func (s *Service) needsCheck(r *v1alpha1.Repository) bool {
	cs := r.Status.ConnectionState
	if cs == nil || cs.AttemptedAt == nil || r.Status.ObservedGeneration != r.Generation {
		return true
	}
	return s.now().Sub(cs.AttemptedAt.Time) > s.cfg.ConnectionStateTTL
}

// checkStored tests a stored repository (credentials resolved by the repository library
// from its Secret, template or SCM provider) and records the result. It returns the
// repository with the new state.
func (s *Service) checkStored(ctx context.Context, r *v1alpha1.Repository) *v1alpha1.Repository {
	if s.d.Resolver == nil {
		return r
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.ConnectionTimeout)
	state, err := s.d.Resolver.Validate(cctx, r)
	cancel()
	return s.record(ctx, r, state, err)
}

// checkWithData tests a repository with the given credential data (unsaved input, or the
// data just written to its managed Secret).
func (s *Service) checkWithData(ctx context.Context, r *v1alpha1.Repository, data map[string][]byte) (v1alpha1.ConnectionState, error) {
	if s.d.Resolver == nil {
		return v1alpha1.ConnectionState{}, rest.Unavailable("repository connection checks are not configured").WithCause(errNotConfigured)
	}
	if data == nil {
		data = map[string][]byte{}
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.ConnectionTimeout)
	defer cancel()
	return s.d.Resolver.ValidateWithSecretData(cctx, r, data)
}

// record writes a connection state to the repository's status (best effort).
func (s *Service) record(ctx context.Context, r *v1alpha1.Repository, state v1alpha1.ConnectionState, cause error) *v1alpha1.Repository {
	out := r.DeepCopy()
	if cause != nil && (errors.Is(cause, repository.ErrNotSynced) || errors.Is(cause, errNotConfigured) || ctx.Err() != nil) {
		// Nothing was tested: keep the recorded state.
		return out
	}
	if s.d.StatusWriter != nil {
		stored, err := s.d.StatusWriter.WriteConnectionState(ctx, r, state, cause)
		switch {
		case err == nil && stored != nil:
			return stored
		case errors.Is(err, repository.ErrStaleConnectionState) && stored != nil:
			return stored
		case err != nil:
			s.log.WarnContext(ctx, "could not record repository connection state", "repository", r.Name, "error", err)
		}
	}
	out.Status.ConnectionState = &state
	return out
}

// checkAll refreshes the connection state of the repositories that need it (all with
// force), with bounded parallelism.
func (s *Service) checkAll(ctx context.Context, repos []v1alpha1.Repository, force bool) {
	g := new(errgroup.Group)
	g.SetLimit(s.cfg.MaxParallelChecks)
	for i := range repos {
		if !force && !s.needsCheck(&repos[i]) {
			continue
		}
		g.Go(func() error {
			repos[i] = *s.checkStored(ctx, &repos[i])
			return nil
		})
	}
	_ = g.Wait()
}

func (s *Service) listRepositories(ctx context.Context, p purpose, req *apiclient.RepositoryListRequest) (*apiclient.RepositoryInfoList, error) {
	if err := s.gate(p); err != nil {
		return nil, err
	}
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	var opts []client.ListOption
	if req.Selector != "" {
		sel, err := labels.Parse(req.Selector)
		if err != nil {
			return nil, rest.InvalidArgument("invalid selector: %v", err)
		}
		opts = append(opts, client.MatchingLabelsSelector{Selector: sel})
	}
	if req.Limit < 0 || req.Limit > maxListLimit {
		return nil, rest.InvalidArgument("limit must be between 0 and %d", maxListLimit)
	}
	if req.Limit > 0 {
		opts = append(opts, client.Limit(req.Limit))
	}
	if req.Continue != "" {
		opts = append(opts, client.Continue(req.Continue))
	}
	repos, meta, err := s.listOfPurpose(ctx, kc, p, opts...)
	if err != nil {
		return nil, err
	}
	visible := repos[:0]
	for _, r := range repos {
		if req.AppProject != "" && r.Spec.Project != req.AppProject {
			continue
		}
		if rest.Can(ctx, p.resource, rbac.ActionGet, rbacObject(r.Spec.Project, r.Spec.URL)) {
			visible = append(visible, r)
		}
	}
	s.checkAll(ctx, visible, req.ForceRefresh)
	templates, err := s.templatesOf(ctx, kc, p)
	if err != nil {
		return nil, err
	}
	out := &apiclient.RepositoryInfoList{Items: make([]apiclient.RepositoryInfo, 0, len(visible)), Metadata: metav1.ListMeta{Continue: meta.Continue, ResourceVersion: meta.ResourceVersion}}
	for i := range visible {
		out.Items = append(out.Items, repositoryInfo(&visible[i], hasTemplate(templates, visible[i].Spec.URL)))
	}
	slices.SortStableFunc(out.Items, func(a, b apiclient.RepositoryInfo) int {
		return strings.Compare(a.Spec.Project+"/"+a.Spec.URL, b.Spec.Project+"/"+b.Spec.URL)
	})
	return out, nil
}

func (s *Service) info(ctx context.Context, kc KubeClient, p purpose, r *v1alpha1.Repository) (*apiclient.RepositoryInfo, error) {
	inherited := false
	if !accessHasCredentials(&r.Spec.RepositoryAccess) {
		templates, err := s.templatesOf(ctx, kc, p)
		if err != nil {
			return nil, err
		}
		inherited = hasTemplate(templates, r.Spec.URL)
	}
	out := repositoryInfo(r, inherited)
	return &out, nil
}

func (s *Service) getRepository(ctx context.Context, p purpose, req *apiclient.RepositoryQuery) (*apiclient.RepositoryInfo, error) {
	if err := s.gate(p); err != nil {
		return nil, err
	}
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	r, err := s.lookupVisible(ctx, kc, p, rbac.ActionGet, req.Repo, req.AppProject)
	if err != nil {
		return nil, err
	}
	if err := authorize(ctx, p.resource, rbac.ActionGet, rbacObject(r.Spec.Project, r.Spec.URL)); err != nil {
		return nil, err
	}
	if req.ForceRefresh || s.needsCheck(r) {
		r = s.checkStored(ctx, r)
	}
	return s.info(ctx, kc, p, r)
}

// prepareWrite validates the body of a create, update or validate request and returns the
// normalized spec. credentialsRef must be empty (or, on update, unchanged).
func (s *Service) prepareWrite(ctx context.Context, kc KubeClient, p purpose, body *apiclient.RepositoryWriteRequest, currentRef *v1alpha1.LocalSecretReference, currentSCM *v1alpha1.SCMProviderReference) (v1alpha1.RepositorySpec, error) {
	if err := validateTypeMeta(body.APIVersion, body.Kind, "Repository"); err != nil {
		return v1alpha1.RepositorySpec{}, err
	}
	spec := *body.Spec.DeepCopy()
	if err := normalizeRepositorySpec(&spec, p); err != nil {
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

// checkSCMProviderRef requires an existing SCMProvider the caller may get when a request
// sets or changes a reference: referencing one makes the object use its credentials.
func (s *Service) checkSCMProviderRef(ctx context.Context, kc KubeClient, ref, current *v1alpha1.SCMProviderReference) error {
	if ref == nil || (current != nil && current.Name == ref.Name) {
		return nil
	}
	if err := rest.Authorize(ctx, ResourceSCMProviders, rbac.ActionGet, ref.Name); err != nil {
		return err
	}
	var sp v1alpha1.SCMProvider
	if err := kc.Get(ctx, client.ObjectKey{Namespace: s.cfg.Namespace, Name: ref.Name}, &sp); err != nil {
		if apierrors.IsNotFound(err) {
			return rest.InvalidArgument("SCM provider %q does not exist", ref.Name)
		}
		return err
	}
	return nil
}

// requireProject checks that a project-scoped object names an existing project.
func (s *Service) requireProject(ctx context.Context, kc KubeClient, name string) error {
	if name == "" {
		return nil
	}
	var proj v1alpha1.AppProject
	if err := kc.Get(ctx, client.ObjectKey{Namespace: s.cfg.Namespace, Name: name}, &proj); err != nil {
		if apierrors.IsNotFound(err) {
			return rest.InvalidArgument("project %q does not exist", name)
		}
		return err
	}
	return nil
}

// connectionError is the error of a failed pre-save or validate check: the connection
// message (upstream "Unable to connect to repository: ..." or the configuration error
// message, completed with the condition reason).
func connectionError(state v1alpha1.ConnectionState, err error) error {
	msg := state.Message
	if msg == "" {
		msg = "connection check failed"
	}
	if repository.IsConfigurationError(err) {
		msg += " (" + repository.ConditionReason(err) + ")"
	}
	if errors.Is(err, repository.ErrNotSynced) {
		return rest.Unavailable("repository configuration is not loaded yet; retry").WithCause(err)
	}
	return rest.InvalidArgument("error testing repository connectivity: %s", msg).WithCause(err)
}

// candidate is an unsaved Repository for connection checks.
func (s *Service) candidate(spec v1alpha1.RepositorySpec, name string) *v1alpha1.Repository {
	return &v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.cfg.Namespace}, Spec: spec}
}

func (s *Service) createRepository(ctx context.Context, p purpose, req *apiclient.RepositoryCreateRequest) (*apiclient.RepositoryInfo, error) {
	if err := s.gate(p); err != nil {
		return nil, err
	}
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	body := &req.Body
	if strings.TrimSpace(body.Spec.URL) == "" {
		return nil, rest.InvalidArgument("missing payload in request")
	}
	// Authorize on the requested coordinates before anything else is looked up.
	spec := *body.Spec.DeepCopy()
	spec.URL = strings.TrimSpace(spec.URL)
	if err := authorize(ctx, p.resource, rbac.ActionCreate, rbacObject(spec.Project, spec.URL)); err != nil {
		return nil, err
	}
	if req.CredsOnly {
		return s.createTemplateFromRepository(ctx, kc, p, req)
	}
	spec, err = s.prepareWrite(ctx, kc, p, body, nil, nil)
	if err != nil {
		return nil, err
	}
	if err := s.requireProject(ctx, kc, spec.Project); err != nil {
		return nil, err
	}
	creds := credentialData(body.Credentials)
	if p.write && len(creds) == 0 && spec.SCMProviderRef == nil && !spec.UseAzureWorkloadIdentity {
		return nil, rest.InvalidArgument("missing credentials in request")
	}
	// Before the pre-save test, which would send inherited template credentials.
	if err := s.checkInheritedTransport(ctx, kc, p, &spec, creds); err != nil {
		return nil, err
	}
	name := objectName(p.repoPrefix(), spec.URL, spec.Project)
	// Unscoped read repositories and write repositories are tested before they are saved
	// (upstream); project-scoped read repositories are created even when unreachable and
	// their state is recorded.
	mustPass := p.write || spec.Project == ""
	var (
		state   v1alpha1.ConnectionState
		cerr    error
		checked bool
	)
	if s.d.Resolver != nil {
		state, cerr = s.checkWithData(ctx, s.candidate(spec, name), creds)
		checked = !errors.Is(cerr, repository.ErrNotSynced) && ctx.Err() == nil
		if cerr != nil && mustPass {
			return nil, connectionError(state, cerr)
		}
	} else if mustPass {
		return nil, rest.Unavailable("repository connection checks are not configured").WithCause(errNotConfigured)
	}
	if existing, err := s.exactRepository(ctx, kc, p, spec.URL, spec.Project); err != nil {
		return nil, err
	} else if existing != nil {
		return s.createExisting(ctx, kc, p, existing, spec, body.Credentials, req.Upsert)
	}
	repo := &v1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.cfg.Namespace},
		Spec:       spec,
	}
	if len(creds) > 0 {
		repo.Spec.CredentialsRef = &v1alpha1.LocalSecretReference{Name: managedSecretName(name)}
	}
	if err := kc.Create(ctx, repo); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// A concurrent create of the same URL and project, or a name collision.
			if existing, gerr := s.exactRepository(ctx, kc, p, spec.URL, spec.Project); gerr == nil && existing != nil {
				return s.createExisting(ctx, kc, p, existing, spec, body.Credentials, req.Upsert)
			}
			return nil, rest.AlreadyExists("%s %q already exists", p.repoNoun(), sanitizeURL(spec.URL)).WithCause(err)
		}
		return nil, err
	}
	target(ctx, "Repository", repo.Namespace, repo.Name, repo.UID)
	if len(creds) > 0 {
		if err := s.storeNewCredentials(ctx, kc, repoOwner(repo, p), body.Credentials, repo.Spec.CredentialsRef); err != nil {
			// Roll back: a Repository must not reference a Secret that was not written.
			if derr := kc.Delete(ctx, repo, client.Preconditions{UID: &repo.UID}); derr != nil && !apierrors.IsNotFound(derr) {
				s.log.ErrorContext(ctx, "could not roll back repository after a credentials failure", "repository", repo.Name, "error", derr)
			}
			return nil, err
		}
	}
	rest.AuditFrom(ctx).SetMessage("created %s %s", p.repoNoun(), sanitizeURL(spec.URL))
	if checked {
		repo = s.record(ctx, repo, state, cerr)
	}
	return s.info(ctx, kc, p, repo)
}

// exactRepository returns the repository of purpose p with the same URL and exactly the
// given project, or nil.
func (s *Service) exactRepository(ctx context.Context, kc KubeClient, p purpose, url, projectName string) (*v1alpha1.Repository, error) {
	repos, err := s.reposForURL(ctx, kc, p, url)
	if err != nil {
		return nil, err
	}
	for i := range repos {
		if repos[i].Spec.Project == projectName {
			return &repos[i], nil
		}
	}
	return nil, nil
}

// comparableSpec is a spec without the server-managed credentialsRef, with defaults.
func comparableSpec(spec v1alpha1.RepositorySpec, p purpose) v1alpha1.RepositorySpec {
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

// createExisting handles a create whose URL and project are already registered: identical
// is idempotent, upsert updates, anything else is upstream's "spec is different" error.
func (s *Service) createExisting(ctx context.Context, kc KubeClient, p purpose, existing *v1alpha1.Repository, spec v1alpha1.RepositorySpec, creds *apiclient.RepositoryCredentials, upsert bool) (*apiclient.RepositoryInfo, error) {
	target(ctx, "Repository", existing.Namespace, existing.Name, existing.UID)
	stored, err := s.storedCredentials(ctx, kc, repoOwner(existing, p), existing.Spec.CredentialsRef)
	if err != nil {
		return nil, err
	}
	diff := specDiff(comparableSpec(existing.Spec, p), comparableSpec(spec, p))
	if !maps.EqualFunc(stored, credentialData(creds), func(a, b []byte) bool { return string(a) == string(b) }) {
		diff = append(diff, "credentials")
	}
	switch {
	case len(diff) == 0:
		return s.info(ctx, kc, p, existing)
	case upsert:
		// Upstream checks only create here; an upsert replaces the stored object, so update
		// is required too.
		if err := authorize(ctx, p.resource, rbac.ActionUpdate, rbacObject(existing.Spec.Project, existing.Spec.URL)); err != nil {
			return nil, err
		}
		updated, err := s.applyUpdate(ctx, kc, p, existing, spec, creds, existing.ResourceVersion)
		if err != nil {
			return nil, err
		}
		return s.info(ctx, kc, p, updated)
	default:
		return nil, specIsDifferent(p.repoNoun(), diff)
	}
}

// applyUpdate stores spec (and creds when non-nil) on existing, then checks and records
// the connection. The Repository is written first, under resourceVersion, with the
// credentialsRef the credentials will need; the managed Secret follows. A refused update
// (409 on a concurrent write, or any other failure) therefore changes no credentials, and a
// Secret that cannot be written after a successful update puts the previous spec back.
func (s *Service) applyUpdate(ctx context.Context, kc KubeClient, p purpose, existing *v1alpha1.Repository, spec v1alpha1.RepositorySpec, creds *apiclient.RepositoryCredentials, resourceVersion string) (*v1alpha1.Repository, error) {
	repo := existing.DeepCopy()
	owner := repoOwner(repo, p)
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
	repo.Spec = spec
	if resourceVersion != "" {
		repo.ResourceVersion = resourceVersion
	}
	if err := kc.Update(ctx, repo); err != nil {
		return nil, err
	}
	target(ctx, "Repository", repo.Namespace, repo.Name, repo.UID)
	var data map[string][]byte
	if plan != nil {
		stored, err := s.applyCredentials(ctx, kc, plan)
		if err != nil {
			restored := repo.DeepCopy()
			restored.Spec = *existing.Spec.DeepCopy()
			if rerr := kc.Update(ctx, restored); rerr != nil {
				s.log.ErrorContext(ctx, "could not restore repository after a credentials failure", "repository", repo.Name, "error", rerr)
			}
			return nil, err
		}
		data = stored
	}
	// A managed Secret that the update stopped referencing is removed.
	if old := existing.Spec.CredentialsRef; old != nil && (spec.CredentialsRef == nil || spec.CredentialsRef.Name != old.Name) {
		if err := s.deleteManagedSecret(ctx, kc, owner, old); err != nil {
			s.log.WarnContext(ctx, "could not delete a credentials Secret that is no longer referenced", "repository", repo.Name, "error", err)
		}
	}
	rest.AuditFrom(ctx).SetMessage("updated %s %s", p.repoNoun(), sanitizeURL(spec.URL))
	if creds != nil && s.d.Resolver != nil {
		state, cerr := s.checkWithData(ctx, repo, data)
		return s.record(ctx, repo, state, cerr), nil
	}
	return s.checkStored(ctx, repo), nil
}

func (s *Service) updateRepository(ctx context.Context, p purpose, req *apiclient.RepositoryUpdateRequest) (*apiclient.RepositoryInfo, error) {
	if err := s.gate(p); err != nil {
		return nil, err
	}
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	body := &req.Body
	if strings.TrimSpace(body.Spec.URL) == "" {
		body.Spec.URL = req.Repo
	}
	if !sameURL(body.Spec.URL, req.Repo) {
		return nil, rest.InvalidArgument("spec.url must name the repository of the path")
	}
	current := req.AppProject
	if current == "" {
		current = body.Spec.Project
	}
	existing, err := s.exactRepository(ctx, kc, p, req.Repo, current)
	if err != nil {
		return nil, err
	}
	if existing == nil && req.AppProject == "" && body.Spec.Project != "" {
		// Upstream getRepo falls back to the unscoped repository: this moves an unscoped
		// repository into a project.
		if existing, err = s.exactRepository(ctx, kc, p, req.Repo, ""); err != nil {
			return nil, err
		}
	}
	if existing == nil || !rest.Can(ctx, p.resource, rbac.ActionGet, rbacObject(existing.Spec.Project, existing.Spec.URL)) {
		return nil, denyGeneric(ctx, p.resource, rbac.ActionUpdate, rbacObject(current, req.Repo))
	}
	// Update permission where the repository is and where it will be.
	if err := authorize(ctx, p.resource, rbac.ActionUpdate, rbacObject(existing.Spec.Project, existing.Spec.URL)); err != nil {
		return nil, err
	}
	if err := authorize(ctx, p.resource, rbac.ActionUpdate, rbacObject(body.Spec.Project, body.Spec.URL)); err != nil {
		return nil, err
	}
	spec, err := s.prepareWrite(ctx, kc, p, body, existing.Spec.CredentialsRef, existing.Spec.SCMProviderRef)
	if err != nil {
		return nil, err
	}
	if spec.Project != existing.Spec.Project {
		if err := s.requireProject(ctx, kc, spec.Project); err != nil {
			return nil, err
		}
		if other, err := s.exactRepository(ctx, kc, p, spec.URL, spec.Project); err != nil {
			return nil, err
		} else if other != nil {
			return nil, rest.AlreadyExists("%s %q already exists in project %q", p.repoNoun(), sanitizeURL(spec.URL), spec.Project)
		}
	}
	if rv := body.Metadata.ResourceVersion; rv != "" && rv != existing.ResourceVersion {
		return nil, rest.Conflict("the repository has been modified (resourceVersion %s, request has %s); read it again and retry", existing.ResourceVersion, rv)
	}
	data := credentialData(body.Credentials)
	if body.Credentials == nil {
		if data, err = s.storedCredentials(ctx, kc, repoOwner(existing, p), existing.Spec.CredentialsRef); err != nil {
			return nil, err
		}
	}
	if err := s.checkInheritedTransport(ctx, kc, p, &spec, data); err != nil {
		return nil, err
	}
	updated, err := s.applyUpdate(ctx, kc, p, existing, spec, body.Credentials, existing.ResourceVersion)
	if err != nil {
		return nil, err
	}
	return s.info(ctx, kc, p, updated)
}

func (s *Service) deleteRepository(ctx context.Context, p purpose, req *apiclient.RepositoryQuery) (*apiclient.Empty, error) {
	if err := s.gate(p); err != nil {
		return nil, err
	}
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	r, err := s.lookupVisible(ctx, kc, p, rbac.ActionDelete, req.Repo, req.AppProject)
	if err != nil {
		return nil, err
	}
	if err := authorize(ctx, p.resource, rbac.ActionDelete, rbacObject(r.Spec.Project, r.Spec.URL)); err != nil {
		return nil, err
	}
	target(ctx, "Repository", r.Namespace, r.Name, r.UID)
	if err := kc.Delete(ctx, r, client.Preconditions{UID: &r.UID}); err != nil {
		return nil, err
	}
	if err := s.deleteManagedSecret(ctx, kc, repoOwner(r, p), r.Spec.CredentialsRef); err != nil {
		// The Secret is owned by the Repository; garbage collection removes it later.
		s.log.WarnContext(ctx, "could not delete the credentials Secret of a deleted repository", "repository", r.Name, "error", err)
	}
	rest.AuditFrom(ctx).SetMessage("deleted %s %s", p.repoNoun(), sanitizeURL(r.Spec.URL))
	return &apiclient.Empty{}, nil
}

func (s *Service) validateAccess(ctx context.Context, p purpose, req *apiclient.RepositoryValidateRequest) (*v1alpha1.ConnectionState, error) {
	if err := s.gate(p); err != nil {
		return nil, err
	}
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	body := &req.Body
	if strings.TrimSpace(body.Spec.URL) == "" {
		body.Spec.URL = req.Repo
	}
	if !sameURL(body.Spec.URL, req.Repo) {
		return nil, rest.InvalidArgument("spec.url must name the repository of the path")
	}
	if err := authorize(ctx, p.resource, rbac.ActionCreate, rbacObject(body.Spec.Project, strings.TrimSpace(body.Spec.URL))); err != nil {
		return nil, err
	}
	spec, err := s.prepareWrite(ctx, kc, p, body, nil, nil)
	if err != nil {
		return nil, err
	}
	if err := s.checkInheritedTransport(ctx, kc, p, &spec, credentialData(body.Credentials)); err != nil {
		return nil, err
	}
	state, cerr := s.checkWithData(ctx, s.candidate(spec, "validate"), credentialData(body.Credentials))
	if cerr != nil {
		return nil, connectionError(state, cerr)
	}
	return &state, nil
}
