package projects

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// actor names the caller in audit messages (upstream: "Unknown user" without one).
func actor(ctx context.Context) string {
	if a := rest.IdentityFrom(ctx).Actor(); a != "" {
		return a
	}
	return "Unknown user"
}

// audit annotates the request's audit record with the project and the upstream Event
// reason and message ("<user> created project", ...).
func audit(ctx context.Context, p *v1alpha1.AppProject, reason, action string) {
	a := rest.AuditFrom(ctx)
	a.SetTarget(rest.ObjectRef{APIVersion: v1alpha1.GroupVersion.String(), Kind: "AppProject", Namespace: p.Namespace, Name: p.Name, UID: p.UID})
	a.SetEventReason(reason)
	a.SetMessage("%s %s", actor(ctx), action)
}

// forResponse returns a copy of p for a response: token metadata normalized (legacy
// spec tokens merged into status) and managed fields dropped.
func forResponse(p *v1alpha1.AppProject) *v1alpha1.AppProject {
	out := p.DeepCopy()
	out.ManagedFields = nil
	project.NormalizeJWTTokens(out)
	out.APIVersion, out.Kind = v1alpha1.GroupVersion.String(), "AppProject"
	return out
}

// legacyTokens returns the deprecated spec.roles[].jwtTokens of a role.
func legacyTokens(r *v1alpha1.ProjectRole) []v1alpha1.JWTToken {
	return r.JWTTokens //nolint:staticcheck // deprecated field, preserved for migration (TEN-034)
}

func setLegacyTokens(r *v1alpha1.ProjectRole, t []v1alpha1.JWTToken) {
	r.JWTTokens = t //nolint:staticcheck // deprecated field, preserved for migration (TEN-034)
}

// keepTokens gives the roles of next the token metadata stored for the role of the same
// name in old (none for new roles): token metadata is only written by the token routes, so
// a project editor can neither resurrect a revoked token nor forge a token record.
func keepTokens(next, old *v1alpha1.AppProject) {
	for i := range next.Spec.Roles {
		var tokens []v1alpha1.JWTToken
		if old != nil {
			if r, _, err := project.GetRoleByName(old, next.Spec.Roles[i].Name); err == nil {
				tokens = slices.Clone(legacyTokens(r))
			}
		}
		setLegacyTokens(&next.Spec.Roles[i], tokens)
	}
}

// prepare normalizes and validates a requested project the way the upstream API server
// does on create/update: policies normalized, ValidateProject (incl. policy syntax),
// sourceNamespaces entries checked, sync window time zones normalized.
func prepare(p *v1alpha1.AppProject, prefix string) error {
	project.NormalizePolicies(p)
	if err := project.ValidateProject(p); err != nil {
		return validationError(err, prefix)
	}
	if err := validateSourceNamespaces(p.Spec.SourceNamespaces); err != nil {
		return err
	}
	project.NormalizeSyncWindowTimeZones(&p.Spec)
	return nil
}

// list serves GET /api/v1/projects.
func (s *Service) list(ctx context.Context, req *apiclient.ProjectListRequest) (*v1alpha1.AppProjectList, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	opts := []client.ListOption{client.InNamespace(s.d.Config.Namespace)}
	if req.Selector != "" {
		sel, err := labels.Parse(req.Selector)
		if err != nil {
			return nil, rest.InvalidArgument("invalid selector: %v", err)
		}
		opts = append(opts, client.MatchingLabelsSelector{Selector: sel})
	}
	var list v1alpha1.AppProjectList
	if err := s.d.Client.List(ctx, &list, opts...); err != nil {
		return nil, err
	}
	visible := make([]*v1alpha1.AppProject, 0, len(list.Items))
	for i := range list.Items {
		if rest.Can(ctx, rbac.ResourceProjects, rbac.ActionGet, list.Items[i].Name) {
			visible = append(visible, &list.Items[i])
		}
	}
	slices.SortFunc(visible, func(a, b *v1alpha1.AppProject) int { return strings.Compare(a.Name, b.Name) })
	page, next, err := paginate(visible, func(p *v1alpha1.AppProject) string { return p.Name }, req.Limit, req.Continue)
	if err != nil {
		return nil, err
	}
	out := &v1alpha1.AppProjectList{Items: make([]v1alpha1.AppProject, 0, len(page))}
	out.APIVersion, out.Kind = v1alpha1.GroupVersion.String(), "AppProjectList"
	out.ResourceVersion, out.Continue = list.ResourceVersion, next
	for _, p := range page {
		out.Items = append(out.Items, *forResponse(p))
	}
	return out, nil
}

// get serves GET /api/v1/projects/{name} (RBAC checked by the route before existence).
func (s *Service) get(ctx context.Context, req *apiclient.ProjectRequest) (*v1alpha1.AppProject, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	p, err := s.getProject(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	return forResponse(p), nil
}

// newProject returns the object to create from a request: metadata reduced to name,
// labels, annotations and finalizers, status and token metadata dropped.
func (s *Service) newProject(in *v1alpha1.AppProject) *v1alpha1.AppProject {
	p := &v1alpha1.AppProject{}
	p.Name = in.Name
	p.Namespace = s.d.Config.Namespace
	p.Labels = maps.Clone(in.Labels)
	p.Annotations = maps.Clone(in.Annotations)
	for _, f := range in.Finalizers {
		if f == v1alpha1.FinalizerResources { // deletion held while Applications reference it (TEN-042)
			p.Finalizers = append(p.Finalizers, f)
		}
	}
	p.Spec = *in.Spec.DeepCopy()
	keepTokens(p, nil)
	return p
}

// create serves POST /api/v1/projects (upstream ProjectService.Create).
func (s *Service) create(ctx context.Context, req *apiclient.ProjectCreateRequest) (*v1alpha1.AppProject, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if req.Project == nil {
		return nil, rest.InvalidArgument("missing payload 'project' in request")
	}
	if err := validateName(req.Project.Name); err != nil {
		return nil, err
	}
	name := req.Project.Name
	rest.AuditFrom(ctx).SetCheck(rbac.ResourceProjects, rbac.ActionCreate, name)
	if err := rest.Authorize(ctx, rbac.ResourceProjects, rbac.ActionCreate, name); err != nil {
		return nil, err
	}
	p := s.newProject(req.Project)
	if err := prepare(p, "error validating project: "); err != nil {
		return nil, err
	}
	unlock := s.locks.Lock(name)
	defer unlock()
	existing, err := s.getProject(ctx, name)
	switch {
	case err == nil:
		return s.createExisting(ctx, existing, p, req.Upsert)
	case !apierrors.IsNotFound(err):
		return nil, err
	}
	// Creating a project grants its destinations and sources (own and inherited from the
	// global projects its labels select): the caller needs the same permissions as for
	// adding them by update to a bare project.
	bare := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace}}
	if err := s.checkEscalation(ctx, bare, p); err != nil {
		return nil, err
	}
	err = s.d.Client.Create(ctx, p)
	switch {
	case err == nil:
		audit(ctx, p, rest.ReasonResourceCreated, "created project")
		s.syncPolicies(p)
		return forResponse(p), nil
	case !apierrors.IsAlreadyExists(err):
		return nil, err
	}
	// Created concurrently by another writer.
	if existing, err = s.getProject(ctx, name); err != nil {
		return nil, rest.Internal(fmt.Errorf("unable to check existing project details: %w", err))
	}
	return s.createExisting(ctx, existing, p, req.Upsert)
}

// createExisting answers a create of an existing project (upstream): an identical spec
// returns the project; a different one is an error without upsert; an upsert needs
// projects, update and is held to the update checks (privilege escalation, application
// impact), unlike upstream. Callers that may not get the project only learn that it
// exists (409).
func (s *Service) createExisting(ctx context.Context, existing, p *v1alpha1.AppProject, upsert bool) (*v1alpha1.AppProject, error) {
	// A project the caller may not get is reported as existing, never shown or compared.
	if !rest.Can(ctx, rbac.ResourceProjects, rbac.ActionGet, p.Name) {
		return nil, rest.AlreadyExists("project '%s' already exists", p.Name)
	}
	cmp := p.DeepCopy()
	keepTokens(cmp, existing)
	if !upsert {
		if equality.Semantic.DeepEqual(existing.Spec, cmp.Spec) {
			return forResponse(existing), nil
		}
		msg := "existing project spec is different; use upsert flag to force update"
		if diff := diffPaths(existing.Spec, cmp.Spec); len(diff) > 0 {
			msg += fmt.Sprintf("; difference in keys %q", strings.Join(diff, ","))
		}
		return nil, rest.InvalidArgument("%s", msg)
	}
	if err := rest.Authorize(ctx, rbac.ResourceProjects, rbac.ActionUpdate, p.Name); err != nil {
		return nil, err
	}
	out, err := s.write(ctx, p.Name, p, "", false)
	if err != nil {
		return nil, err
	}
	audit(ctx, out, rest.ReasonResourceCreated, "created project")
	return forResponse(out), nil
}

// update serves PUT /api/v1/projects/{name} (upstream ProjectService.Update).
func (s *Service) update(ctx context.Context, req *apiclient.ProjectUpdateRequest) (*v1alpha1.AppProject, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if req.Project == nil {
		return nil, rest.InvalidArgument("missing payload 'project' in request")
	}
	p := req.Project.DeepCopy()
	switch p.Name {
	case "":
		p.Name = req.Name
	case req.Name:
	default:
		return nil, rest.InvalidArgument("project name '%s' does not match the path name '%s'", p.Name, req.Name)
	}
	if err := prepare(p, ""); err != nil {
		return nil, err
	}
	unlock := s.locks.Lock(p.Name)
	defer unlock()
	out, err := s.write(ctx, p.Name, p, req.Project.ResourceVersion, true)
	if err != nil {
		return nil, err
	}
	audit(ctx, out, rest.ReasonResourceUpdated, "updated project")
	return forResponse(out), nil
}

// write replaces the spec (and, with meta, the labels and annotations) of the stored
// project with req's after the escalation and impact checks; token metadata is kept. The
// caller holds the project lock. With clientRV the stored project must still have that
// resourceVersion (409 otherwise); without it conflicts are retried with fresh checks.
func (s *Service) write(ctx context.Context, name string, req *v1alpha1.AppProject, clientRV string, meta bool) (*v1alpha1.AppProject, error) {
	var out *v1alpha1.AppProject
	conflict := func() error {
		return rest.Conflict("the project '%s' has been modified; reload it and retry", name)
	}
	attempt := func() error {
		old, err := s.getProject(ctx, name)
		if err != nil {
			return err
		}
		if clientRV != "" && old.ResourceVersion != clientRV {
			return conflict()
		}
		next := old.DeepCopy()
		next.Spec = *req.Spec.DeepCopy()
		keepTokens(next, old)
		if meta {
			next.Labels = maps.Clone(req.Labels)
			next.Annotations = maps.Clone(req.Annotations)
		}
		if err := s.checkEscalation(ctx, old, next); err != nil {
			return err
		}
		if err := s.checkImpact(ctx, old, next); err != nil {
			return err
		}
		if err := s.d.Client.Update(ctx, next); err != nil {
			if apierrors.IsConflict(err) && clientRV != "" {
				return conflict()
			}
			return err
		}
		out = next
		return nil
	}
	var err error
	if clientRV != "" {
		err = attempt()
	} else {
		err = retry.RetryOnConflict(retry.DefaultBackoff, attempt)
	}
	if err != nil {
		return nil, err
	}
	s.pruneRoleTokens(ctx, name)
	s.syncPolicies(out)
	return out, nil
}

// clusterLookup returns the destination lookup of the impact check.
func (s *Service) clusterLookup(ctx context.Context) (project.ClusterLookup, error) {
	if s.d.Clusters != nil {
		return s.d.Clusters, nil
	}
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	var list v1alpha1.ClusterList
	if err := s.d.Client.List(ctx, &list, client.InNamespace(s.d.Config.Namespace)); err != nil {
		return nil, err
	}
	return project.NewClusterIndex(list.Items, st.InClusterEnabled()), nil
}

// checkImpact refuses an update that makes Applications of the project (control-plane
// namespace, as upstream) lose a source or destination permitted before (TEN-047). Raw
// project specs are compared, without global projects or scoped resources.
func (s *Service) checkImpact(ctx context.Context, old, next *v1alpha1.AppProject) error {
	var apps v1alpha1.ApplicationList
	if err := s.d.Client.List(ctx, &apps, client.InNamespace(s.d.Config.Namespace)); err != nil {
		return err
	}
	var lookup project.ClusterLookup
	invalidSrc, invalidDst := 0, 0
	for i := range apps.Items {
		app := &apps.Items[i]
		if app.Spec.GetProject() != next.Name {
			continue
		}
		for _, src := range app.Spec.GetSources() {
			if project.IsSourcePermitted(old, src.RepoURL) && !project.IsSourcePermitted(next, src.RepoURL) {
				invalidSrc++
				break
			}
		}
		if lookup == nil {
			var err error
			if lookup, err = s.clusterLookup(ctx); err != nil {
				return err
			}
		}
		cluster, err := project.ResolveDestination(ctx, app.Spec.Destination, lookup)
		if err != nil {
			if errors.Is(err, project.ErrDestinationMissing) {
				invalidDst++
			}
			continue // an unresolvable destination is already in error
		}
		dest := project.DestinationOf(cluster, app.Spec.Destination.Namespace)
		before, err := project.IsDestinationPermitted(ctx, old, dest, lookup)
		if err != nil {
			return err
		}
		if !before {
			continue
		}
		after, err := project.IsDestinationPermitted(ctx, next, dest, lookup)
		if err != nil {
			return err
		}
		if !after {
			invalidDst++
		}
	}
	var parts []string
	if invalidSrc > 0 {
		parts = append(parts, fmt.Sprintf("%d applications source became invalid", invalidSrc))
	}
	if invalidDst > 0 {
		parts = append(parts, fmt.Sprintf("%d applications destination became invalid", invalidDst))
	}
	if len(parts) > 0 {
		return rest.InvalidArgument("as a result of project update %s", strings.Join(parts, " and "))
	}
	return nil
}

// pruneRoleTokens drops the status token entries of roles that no longer exist, so a role
// re-created later does not inherit (and revive) the tokens of the deleted one. Failures
// are logged: token verification already requires the role to exist.
func (s *Service) pruneRoleTokens(ctx context.Context, name string) {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		p, err := s.getProject(ctx, name)
		if err != nil {
			return err
		}
		changed := false
		for role := range p.Status.JWTTokensByRole {
			if _, _, err := project.GetRoleByName(p, role); err != nil {
				delete(p.Status.JWTTokensByRole, role)
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return s.d.Client.Status().Update(ctx, p)
	})
	if err != nil {
		s.d.Logger.Warn("failed to drop the token records of deleted project roles", "project", name, "error", err)
	}
}

// syncPolicies pushes the project's role policies to the RBAC enforcer right away.
func (s *Service) syncPolicies(p *v1alpha1.AppProject) {
	if s.d.Policies == nil || p == nil {
		return
	}
	if err := s.d.Policies.SetProjectPolicies(p.Name, project.RBACRoles(p)); err != nil {
		s.d.Logger.Warn("project role policies have problems; invalid items are ignored", "project", p.Name, "error", err)
	}
}

// delete serves DELETE /api/v1/projects/{name} (upstream ProjectService.Delete).
func (s *Service) delete(ctx context.Context, req *apiclient.ProjectRequest) (*apiclient.Empty, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if req.Name == v1alpha1.DefaultAppProjectName {
		return nil, rest.InvalidArgument("name '%s' is reserved and cannot be deleted", req.Name)
	}
	rest.AuditFrom(ctx).SetCheck(rbac.ResourceProjects, rbac.ActionDelete, req.Name)
	if err := rest.Authorize(ctx, rbac.ResourceProjects, rbac.ActionDelete, req.Name); err != nil {
		return nil, err
	}
	unlock := s.locks.Lock(req.Name)
	defer unlock()
	p, err := s.getProject(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	var apps v1alpha1.ApplicationList
	if err := s.d.Client.List(ctx, &apps, client.InNamespace(s.d.Config.Namespace)); err != nil {
		return nil, err
	}
	n := 0
	for i := range apps.Items {
		if apps.Items[i].Spec.GetProject() == req.Name {
			n++
		}
	}
	if n > 0 {
		return nil, rest.InvalidArgument("project is referenced by %d applications", n)
	}
	var appsets v1alpha1.ApplicationSetList
	if err := s.d.Client.List(ctx, &appsets, client.InNamespace(s.d.Config.Namespace)); err != nil {
		return nil, err
	}
	n = 0
	for i := range appsets.Items {
		if appsets.Items[i].Spec.Template.Spec.Project == req.Name {
			n++
		}
	}
	if n > 0 {
		return nil, rest.InvalidArgument("project is referenced by %d applicationsets", n)
	}
	if err := s.d.Client.Delete(ctx, p, client.Preconditions{UID: ptr.To(p.UID)}); err != nil {
		return nil, err
	}
	audit(ctx, p, rest.ReasonResourceDeleted, "deleted project")
	if s.d.Policies != nil {
		s.d.Policies.DeleteProjectPolicies(p.Name)
	}
	return &apiclient.Empty{}, nil
}
