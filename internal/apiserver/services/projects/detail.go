package projects

import (
	"context"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/links"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// globalProjectsOf returns the global projects of p (status removed: only their spec is
// inherited, and their token metadata belongs to them). Problems with the globalProjects
// setting are logged, as upstream.
func (s *Service) globalProjectsOf(ctx context.Context, st *settings.Settings, p *v1alpha1.AppProject) []*v1alpha1.AppProject {
	globals, err := project.GlobalProjects(ctx, p, globalSettings(st), s)
	if err != nil {
		s.d.Logger.Warn("global projects of a project could not all be resolved", "project", p.Name, "error", err)
	}
	return globals
}

func stripForGlobal(g *v1alpha1.AppProject) v1alpha1.AppProject {
	out := g.DeepCopy()
	out.Status = v1alpha1.AppProjectStatus{}
	out.ManagedFields = nil
	out.APIVersion, out.Kind = v1alpha1.GroupVersion.String(), "AppProject"
	for i := range out.Spec.Roles {
		setLegacyTokens(&out.Spec.Roles[i], nil)
	}
	return *out
}

// detailed serves GET /api/v1/projects/{name}/detailed (upstream GetDetailedProject): the
// effective project (global projects merged), the global projects, and the scoped
// repositories and clusters the caller may get (sanitized).
func (s *Service) detailed(ctx context.Context, req *apiclient.ProjectRequest) (*apiclient.ProjectDetailed, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	p, err := s.getProject(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	globals := s.globalProjectsOf(ctx, st, p)
	virtual := p.DeepCopy()
	out := &apiclient.ProjectDetailed{GlobalProjects: []v1alpha1.AppProject{}, Repositories: []v1alpha1.Repository{}, Clusters: []apiclient.RegisteredCluster{}}
	for _, g := range globals {
		virtual = project.MergeVirtualProject(virtual, g)
		out.GlobalProjects = append(out.GlobalProjects, stripForGlobal(g))
	}
	out.Project = forResponse(virtual)

	var repos v1alpha1.RepositoryList
	if err := s.d.Client.List(ctx, &repos, client.InNamespace(s.d.Config.Namespace)); err != nil {
		return nil, err
	}
	for i := range repos.Items {
		r := &repos.Items[i]
		if r.Spec.Project != p.Name || r.Spec.Purpose == v1alpha1.RepositoryPurposeWrite {
			continue
		}
		if !rest.Can(ctx, rbac.ResourceRepositories, rbac.ActionGet, p.Name+"/"+r.Spec.URL) {
			continue
		}
		out.Repositories = append(out.Repositories, sanitizeRepository(r))
	}

	var list v1alpha1.ClusterList
	if err := s.d.Client.List(ctx, &list, client.InNamespace(s.d.Config.Namespace)); err != nil {
		return nil, err
	}
	scoped, err := project.NewClusterIndex(list.Items, st.InClusterEnabled()).GetProjectClusters(ctx, p.Name)
	if err != nil {
		return nil, err
	}
	for _, c := range scoped {
		if !rest.Can(ctx, rbac.ResourceClusters, rbac.ActionGet, apiclient.ClusterRBACObject(c.Spec.Project, c.Spec.Server)) {
			continue
		}
		creds, err := s.clusterCredentials(ctx, c)
		if err != nil {
			return nil, err
		}
		out.Clusters = append(out.Clusters, clusters.Sanitize(c, creds, false))
	}
	return out, nil
}

// clusterCredentials reads the credentials Secret of a cluster (only labeled cluster
// Secrets count); used for the credential presence flags only.
func (s *Service) clusterCredentials(ctx context.Context, c *v1alpha1.Cluster) (map[string][]byte, error) {
	ref := c.Spec.Config.CredentialsRef
	if ref == nil || ref.Name == "" {
		return nil, nil
	}
	var sec corev1.Secret
	if err := s.d.Client.Get(ctx, client.ObjectKey{Namespace: s.d.Config.Namespace, Name: ref.Name}, &sec); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if sec.Labels[v1alpha1.LabelKeySecretType] != v1alpha1.LabelValueSecretTypeCluster {
		return nil, nil
	}
	return sec.Data, nil
}

// sanitizeRepository drops managed fields and a password embedded in the proxy URL.
func sanitizeRepository(r *v1alpha1.Repository) v1alpha1.Repository {
	out := r.DeepCopy()
	out.ManagedFields = nil
	out.APIVersion, out.Kind = v1alpha1.GroupVersion.String(), "Repository"
	if strings.Contains(out.Spec.Proxy, "@") {
		if u, err := url.Parse(out.Spec.Proxy); err == nil && u.User != nil {
			out.Spec.Proxy = u.Redacted()
		}
	}
	return *out
}

// globalProjects serves GET /api/v1/projects/{name}/globalprojects.
func (s *Service) globalProjects(ctx context.Context, req *apiclient.ProjectRequest) (*apiclient.ProjectGlobalProjectsResponse, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	p, err := s.getProject(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	out := &apiclient.ProjectGlobalProjectsResponse{Items: []v1alpha1.AppProject{}}
	for _, g := range s.globalProjectsOf(ctx, st, p) {
		out.Items = append(out.Items, stripForGlobal(g))
	}
	return out, nil
}

// events serves GET /api/v1/projects/{name}/events: the Events of the control-plane
// namespace whose involved object is this project (name, UID and namespace).
func (s *Service) events(ctx context.Context, req *apiclient.ProjectRequest) (*corev1.EventList, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if s.d.Events == nil {
		return nil, rest.Unavailable("project events are not available").WithCause(errNotConfigured)
	}
	p, err := s.getProject(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	sel := fields.SelectorFromSet(fields.Set{
		"involvedObject.name":      p.Name,
		"involvedObject.uid":       string(p.UID),
		"involvedObject.namespace": p.Namespace,
	}).String()
	list, err := s.d.Events.List(ctx, metav1.ListOptions{FieldSelector: sel})
	if err != nil {
		return nil, err
	}
	list.APIVersion, list.Kind = "v1", "EventList"
	for i := range list.Items {
		list.Items[i].ManagedFields = nil
	}
	if list.Items == nil {
		list.Items = []corev1.Event{}
	}
	return list, nil
}

// syncWindows serves GET /api/v1/projects/{name}/syncwindows (upstream
// GetSyncWindowsState): the project's own windows active now.
func (s *Service) syncWindows(ctx context.Context, req *apiclient.ProjectRequest) (*apiclient.ProjectSyncWindowsResponse, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	p, err := s.getProject(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	active, err := project.ActiveSyncWindows(p.Spec.SyncWindows, s.d.Now())
	if err != nil {
		return nil, rest.FailedPrecondition("%v", err)
	}
	out := &apiclient.ProjectSyncWindowsResponse{Windows: []v1alpha1.SyncWindow{}}
	out.Windows = append(out.Windows, active...)
	return out, nil
}

// links serves GET /api/v1/projects/{name}/links (upstream ListLinks): project.links
// evaluated with the project (status removed) as "project". A denial is reported with
// upstream's generic message.
func (s *Service) links(ctx context.Context, req *apiclient.ProjectRequest) (*apiclient.LinksResponse, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rest.AuditFrom(ctx).SetCheck(rbac.ResourceProjects, rbac.ActionGet, req.Name)
	if err := rest.Authorize(ctx, rbac.ResourceProjects, rbac.ActionGet, req.Name); err != nil {
		return nil, rest.PermissionDenied("unauthorized access to project %s", req.Name)
	}
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	p, err := s.getProject(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	lctx, err := links.NewContext(links.Input{Project: p})
	if err != nil {
		return nil, rest.Internal(err)
	}
	out, problems := links.Evaluate(lctx, p.Name, st.DeepLinks(settings.KeyProjectLinks))
	if len(problems) > 0 {
		s.d.Logger.Warn("errors while evaluating project deep links", "project", p.Name, "errors", strings.Join(problems, "; "))
	}
	return out, nil
}
