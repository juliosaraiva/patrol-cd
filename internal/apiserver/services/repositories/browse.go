package repositories

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	reposerver "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// refKeyRE is upstream's rule for multi-source ref names.
var refKeyRE = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// browseObject returns the RBAC object of a repository-reading request: that of the
// Repository the repository library uses for url and appProject (one scoped to appProject,
// else an unscoped one), or the URL itself for an unregistered repository (upstream
// getRepo + createRBACObject).
func (s *Service) browseObject(ctx context.Context, kc KubeClient, url, appProject string) (string, error) {
	repos, err := s.reposForURL(ctx, kc, readPurpose, url)
	if err != nil {
		return "", err
	}
	if appProject != "" {
		for _, r := range repos {
			if r.Spec.Project == appProject {
				return rbacObject(r.Spec.Project, r.Spec.URL), nil
			}
		}
	}
	for _, r := range repos {
		if r.Spec.Project == "" {
			return rbacObject("", r.Spec.URL), nil
		}
	}
	return rbacObject("", url), nil
}

// authorizeRepoGet enforces repositories, get on the repository a request reads.
func (s *Service) authorizeRepoGet(ctx context.Context, kc KubeClient, url, appProject string) error {
	if err := validateRepoURL("repository URL", url, true); err != nil {
		return err
	}
	obj, err := s.browseObject(ctx, kc, url, appProject)
	if err != nil {
		return err
	}
	return authorize(ctx, rbac.ResourceRepositories, rbac.ActionGet, obj)
}

// resolve returns the access material for url, resolved server-side (registered
// repository, credential template, SCM provider, minted tokens). Call it after
// authorizeRepoGet.
func (s *Service) resolve(ctx context.Context, url, appProject string, typ v1alpha1.RepositoryType) (*reposerver.Repository, error) {
	if s.d.Resolver == nil || s.d.RepoServer == nil {
		return nil, rest.Unavailable("the repo-server is not configured").WithCause(errNotConfigured)
	}
	res, err := s.d.Resolver.Resolve(ctx, repository.Query{URL: url, Project: appProject, Type: typ})
	if err != nil {
		return nil, resolveError(err)
	}
	if res.Repository == nil {
		return nil, rest.Internal(errors.New("resolver returned no repository"))
	}
	// A registration that changed between the RBAC check and the resolution is checked
	// again on the object actually used.
	if p := res.Repository.Project; p != "" && p != appProject {
		if err := authorize(ctx, rbac.ResourceRepositories, rbac.ActionGet, rbacObject(p, url)); err != nil {
			return nil, err
		}
	}
	return res.Repository, nil
}

// resolveError maps a credential resolution failure. Messages name the failure class only
// (values never leave the server).
func resolveError(err error) error {
	switch {
	case errors.Is(err, repository.ErrNotSynced):
		return rest.Unavailable("repository configuration is not loaded yet; retry").WithCause(err)
	case errors.Is(err, repository.ErrTokenUnavailable):
		return rest.Unavailable("could not obtain repository credentials (%s)", repository.ConditionReason(err)).WithCause(err)
	case repository.IsConfigurationError(err):
		return rest.InvalidArgument("repository credentials are misconfigured (%s); please check the server logs", repository.ConditionReason(err)).WithCause(err)
	}
	return rest.Internal(err)
}

// repoServerError maps a repo-server error; secret values of the request are redacted
// from its message.
func repoServerError(err error, repo *reposerver.Repository) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	// The cause is logged for 5xx answers: it carries the redacted message only.
	cause := errors.New(redactSecrets(err.Error(), repo))
	var re *reposerver.Error
	if !errors.As(err, &re) {
		return rest.Unavailable("the repo-server is unavailable").WithCause(cause)
	}
	msg := redactSecrets(re.Message, repo)
	switch re.Code {
	case reposerver.CodeInvalidArgument, reposerver.CodePermissionDenied:
		return rest.InvalidArgument("%s", msg).WithCause(cause)
	case reposerver.CodeNotFound:
		return rest.NotFound("%s", msg).WithCause(cause)
	case reposerver.CodeResourceExhausted:
		return rest.ResourceExhausted("%s", msg).WithCause(cause)
	case reposerver.CodeUnavailable, reposerver.CodeUnauthenticated:
		return rest.Unavailable("the repo-server is unavailable").WithCause(cause)
	case reposerver.CodeDeadlineExceeded:
		return context.DeadlineExceeded
	}
	return rest.Internal(cause)
}

// redactSecrets removes the credential values of repo from msg.
func redactSecrets(msg string, repo *reposerver.Repository) string {
	if repo == nil {
		return msg
	}
	for _, v := range []string{repo.Password, repo.BearerToken, repo.SSHPrivateKey, repo.TLSClientCertKey} {
		if len(v) >= 4 {
			msg = strings.ReplaceAll(msg, v, "******")
		}
	}
	return msg
}

func (s *Service) browseContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.cfg.RepoServerTimeout)
}

func (s *Service) listRefs(ctx context.Context, req *apiclient.RepositoryQuery) (*apiclient.RepositoryRefs, error) {
	return s.refs(ctx, req, "", func(ctx context.Context, r *reposerver.Repository) (*reposerver.Refs, error) {
		return s.d.RepoServer.ListRefs(ctx, &reposerver.ListRefsRequest{Repo: *r})
	})
}

func (s *Service) listOCITags(ctx context.Context, req *apiclient.RepositoryQuery) (*apiclient.RepositoryRefs, error) {
	return s.refs(ctx, req, v1alpha1.RepositoryTypeOCI, func(ctx context.Context, r *reposerver.Repository) (*reposerver.Refs, error) {
		return s.d.RepoServer.ListOCITags(ctx, &reposerver.ListRefsRequest{Repo: *r})
	})
}

func (s *Service) refs(ctx context.Context, req *apiclient.RepositoryQuery, typ v1alpha1.RepositoryType, call func(context.Context, *reposerver.Repository) (*reposerver.Refs, error)) (*apiclient.RepositoryRefs, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	if err := s.authorizeRepoGet(ctx, kc, req.Repo, req.AppProject); err != nil {
		return nil, err
	}
	repo, err := s.resolve(ctx, req.Repo, req.AppProject, typ)
	if err != nil {
		return nil, err
	}
	cctx, cancel := s.browseContext(ctx)
	defer cancel()
	refs, err := call(cctx, repo)
	if err != nil {
		return nil, repoServerError(err, repo)
	}
	out := &apiclient.RepositoryRefs{}
	if refs != nil {
		out.Branches, out.Tags = refs.Branches, refs.Tags
	}
	return out, nil
}

func (s *Service) listHelmCharts(ctx context.Context, req *apiclient.RepositoryQuery) (*apiclient.RepositoryHelmChartList, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	if err := s.authorizeRepoGet(ctx, kc, req.Repo, req.AppProject); err != nil {
		return nil, err
	}
	repo, err := s.resolve(ctx, req.Repo, req.AppProject, v1alpha1.RepositoryTypeHelm)
	if err != nil {
		return nil, err
	}
	cctx, cancel := s.browseContext(ctx)
	defer cancel()
	charts, err := s.d.RepoServer.GetHelmCharts(cctx, &reposerver.HelmChartsRequest{Repo: *repo})
	if err != nil {
		return nil, repoServerError(err, repo)
	}
	out := &apiclient.RepositoryHelmChartList{Items: []apiclient.RepositoryHelmChart{}}
	if charts != nil {
		for _, c := range charts.Items {
			out.Items = append(out.Items, apiclient.RepositoryHelmChart{Name: c.Name, Versions: c.Versions})
		}
	}
	return out, nil
}

// appRef parses an application name ("name" or "namespace/name"; default namespace: the
// control plane's).
func (s *Service) appRef(appName string) (namespace, name string, err error) {
	namespace, name = s.cfg.Namespace, appName
	if ns, n, ok := strings.Cut(appName, "/"); ok {
		namespace, name = ns, n
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return "", "", rest.InvalidArgument("invalid application namespace %q", ns)
		}
	}
	if name != "" {
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			return "", "", rest.InvalidArgument("invalid application name %q", name)
		}
	}
	return namespace, name, nil
}

// effectiveProject returns the project an Application of name is evaluated against
// (scoped repositories and global projects merged).
func (s *Service) effectiveProject(ctx context.Context, kc KubeClient, name string) (*v1alpha1.AppProject, error) {
	inCluster := true
	var globals []project.GlobalProjectSetting
	if st := s.settings(); st != nil {
		inCluster = st.InClusterEnabled()
		for _, g := range st.GlobalProjects() {
			globals = append(globals, project.GlobalProjectSetting{ProjectName: g.ProjectName, LabelSelector: g.LabelSelector})
		}
	}
	lookup := project.NewKubeLookup(kc, s.cfg.Namespace, inCluster)
	proj, err := lookup.GetProject(ctx, name)
	if err != nil {
		return nil, err
	}
	l := lookup.Lookups(globals)
	l.Clusters = nil
	eff, gerr, err := project.EffectiveProject(ctx, proj, l)
	if gerr != nil {
		s.log.WarnContext(ctx, "global projects could not be evaluated", "project", name, "error", gerr)
	}
	return eff, err
}

// repoPermitted enforces the project's sourceRepos (upstream isRepoPermittedInProject). A
// missing project is reported like a refused repository (no project existence oracle).
func (s *Service) repoPermitted(ctx context.Context, kc KubeClient, url, projName string) (*v1alpha1.AppProject, error) {
	eff, err := s.effectiveProject(ctx, kc, projName)
	if err != nil && !project.IsNotFound(err) {
		return nil, err
	}
	if eff == nil || !project.IsSourcePermitted(eff, url) {
		return nil, rest.PermissionDenied("repository '%s' not permitted in project '%s'", sanitizeURL(url), projName)
	}
	return eff, nil
}

func (s *Service) listApps(ctx context.Context, req *apiclient.RepositoryAppsRequest) (*apiclient.RepositoryAppList, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	if err := s.authorizeRepoGet(ctx, kc, req.Repo, req.AppProject); err != nil {
		return nil, err
	}
	if req.AppProject == "" {
		return nil, rest.InvalidArgument("appProject is required")
	}
	appNS, appName, err := s.appRef(req.AppName)
	if err != nil {
		return nil, err
	}
	// Discovery clones the repository and runs config management tools: only for callers
	// who may create or update the Application it is for.
	appObj := rbac.AppObject(s.cfg.Namespace, req.AppProject, appNS, appName)
	if !rest.Can(ctx, rbac.ResourceApplications, rbac.ActionCreate, appObj) && !rest.Can(ctx, rbac.ResourceApplications, rbac.ActionUpdate, appObj) {
		return nil, denyGeneric(ctx, rbac.ResourceApplications, rbac.ActionCreate, appObj)
	}
	if _, err := s.repoPermitted(ctx, kc, req.Repo, req.AppProject); err != nil {
		return nil, err
	}
	repo, err := s.resolve(ctx, req.Repo, req.AppProject, "")
	if err != nil {
		return nil, err
	}
	cctx, cancel := s.browseContext(ctx)
	defer cancel()
	apps, err := s.d.RepoServer.ListApps(cctx, &reposerver.ListAppsRequest{Repo: *repo, Revision: req.Revision, EnabledSourceTypes: s.enabledSourceTypes()})
	if err != nil {
		return nil, repoServerError(err, repo)
	}
	out := &apiclient.RepositoryAppList{Items: []apiclient.RepositoryApp{}}
	if apps != nil {
		for path, typ := range apps.Apps {
			out.Items = append(out.Items, apiclient.RepositoryApp{Path: path, Type: typ})
		}
	}
	slices.SortFunc(out.Items, func(a, b apiclient.RepositoryApp) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

func (s *Service) enabledSourceTypes() map[string]bool {
	st := s.settings()
	if st == nil {
		return nil
	}
	out := map[string]bool{}
	for k, v := range st.EnabledSourceTypes() {
		out[string(k)] = v
	}
	return out
}

func (s *Service) kustomizeOptions() reposerver.KustomizeOptions {
	st := s.settings()
	if st == nil {
		return reposerver.KustomizeOptions{}
	}
	k := st.Kustomize()
	out := reposerver.KustomizeOptions{BuildOptions: k.BuildOptions}
	for _, v := range k.Versions {
		if out.Versions == nil {
			out.Versions, out.VersionBuildOptions = map[string]string{}, map[string]string{}
		}
		out.Versions[v.Name] = v.Path
		if v.BuildOptions != "" {
			out.VersionBuildOptions[v.Name] = v.BuildOptions
		}
	}
	return out
}

func (s *Service) helmOptions() reposerver.HelmOptions {
	st := s.settings()
	if st == nil {
		return reposerver.HelmOptions{}
	}
	return reposerver.HelmOptions{ValuesFileSchemes: st.HelmValuesFileSchemes()}
}

func (s *Service) getAppDetails(ctx context.Context, req *apiclient.RepositoryAppDetailsRequest) (*apiclient.RepositoryAppDetails, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	q := &req.Body
	if q.Source == nil {
		return nil, rest.InvalidArgument("missing payload in request")
	}
	source := q.Source.DeepCopy()
	if strings.TrimSpace(source.RepoURL) == "" {
		source.RepoURL = req.Repo
	}
	if !sameURL(source.RepoURL, req.Repo) {
		return nil, rest.InvalidArgument("source.repoURL must name the repository of the path")
	}
	if err := s.authorizeRepoGet(ctx, kc, source.RepoURL, q.AppProject); err != nil {
		return nil, err
	}
	if q.AppProject == "" {
		return nil, rest.InvalidArgument("appProject is required")
	}
	appNS, appName, err := s.appRef(q.AppName)
	if err != nil {
		return nil, err
	}
	appObj := rbac.AppObject(s.cfg.Namespace, q.AppProject, appNS, appName)
	if err := rest.Authorize(ctx, rbac.ResourceApplications, rbac.ActionGet, appObj); err != nil {
		return nil, err
	}
	var app *v1alpha1.Application
	if appName != "" {
		a := &v1alpha1.Application{}
		switch err := kc.Get(ctx, client.ObjectKey{Namespace: appNS, Name: appName}, a); {
		case err == nil:
			app = a
		case !apierrors.IsNotFound(err):
			return nil, err
		}
	}
	if app == nil {
		// The Application is being formulated: details are revealed to callers who may
		// create it.
		if err := rest.Authorize(ctx, rbac.ResourceApplications, rbac.ActionCreate, appObj); err != nil {
			return nil, err
		}
	} else {
		// Details of an existing Application: only of its own project and of sources it
		// uses or used (not arbitrary values).
		if app.Spec.GetProject() != q.AppProject || !sourceInHistory(app, source, int(q.SourceIndex), int64(q.VersionID)) {
			return nil, denyGeneric(ctx, rbac.ResourceApplications, rbac.ActionGet, appObj)
		}
	}
	eff, err := s.repoPermitted(ctx, kc, source.RepoURL, q.AppProject)
	if err != nil {
		return nil, err
	}
	typ := v1alpha1.RepositoryType("")
	if source.Chart != "" {
		typ = v1alpha1.RepositoryTypeHelm
	}
	repo, err := s.resolve(ctx, source.RepoURL, q.AppProject, typ)
	if err != nil {
		return nil, err
	}
	isOCI := strings.HasPrefix(strings.ToLower(source.RepoURL), "oci://")
	repos, helmCreds, err := s.d.Resolver.ListDependencyRepositories(ctx, eff, isOCI)
	if err != nil {
		return nil, resolveError(err)
	}
	var refSources map[string]reposerver.RefTarget
	if app != nil && app.Spec.HasMultipleSources() {
		if refSources, err = s.refSources(ctx, app.Spec.Sources, q.AppProject); err != nil {
			return nil, err
		}
	}
	cctx, cancel := s.browseContext(ctx)
	defer cancel()
	d, err := s.d.RepoServer.GetAppDetails(cctx, &reposerver.AppDetailsQuery{
		Repo: *repo, Source: *source, Repos: repos, HelmRepoCreds: helmCreds,
		KustomizeOptions: s.kustomizeOptions(), HelmOptions: s.helmOptions(), AppName: appName,
		EnabledSourceTypes: s.enabledSourceTypes(), RefSources: refSources,
	})
	if err != nil {
		return nil, repoServerError(err, repo)
	}
	return appDetails(d), nil
}

// refSources resolves the `ref` sources of a multi-source Application (upstream
// GetRefSources).
func (s *Service) refSources(ctx context.Context, sources v1alpha1.ApplicationSources, projectName string) (map[string]reposerver.RefTarget, error) {
	out := map[string]reposerver.RefTarget{}
	for _, src := range sources {
		if src.Ref == "" {
			continue
		}
		if !refKeyRE.MatchString(src.Ref) {
			return nil, rest.InvalidArgument("sources.ref %s cannot contain any special characters except '_' and '-'", src.Ref)
		}
		key := "$" + src.Ref
		if _, dup := out[key]; dup {
			return nil, rest.InvalidArgument("invalid sources: multiple sources had the same `ref` key")
		}
		repo, err := s.resolve(ctx, src.RepoURL, projectName, "")
		if err != nil {
			return nil, err
		}
		out[key] = reposerver.RefTarget{Repo: *repo, TargetRevision: src.TargetRevision, Chart: src.Chart}
	}
	return out, nil
}

func appDetails(d *reposerver.AppDetails) *apiclient.RepositoryAppDetails {
	out := &apiclient.RepositoryAppDetails{}
	if d == nil {
		return out
	}
	out.Type = d.Type
	if h := d.Helm; h != nil {
		out.Helm = &apiclient.RepositoryHelmAppSpec{Name: h.Name, ValueFiles: h.ValueFiles, Parameters: h.Parameters, Values: h.Values, FileParameters: h.FileParameters}
	}
	if k := d.Kustomize; k != nil {
		out.Kustomize = &apiclient.RepositoryKustomizeAppSpec{Images: k.Images}
	}
	if d.Directory != nil {
		out.Directory = &apiclient.RepositoryDirectoryAppSpec{}
	}
	if p := d.Plugin; p != nil {
		out.Plugin = &apiclient.RepositoryPluginAppSpec{}
		for _, a := range p.ParametersAnnouncement {
			out.Plugin.ParametersAnnouncement = append(out.Plugin.ParametersAnnouncement, apiclient.RepositoryPluginParameter{
				Name: a.Name, Title: a.Title, Tooltip: a.Tooltip, Required: a.Required, ItemType: a.ItemType,
				CollectionType: a.CollectionType, String: a.String, Array: a.Array, Map: a.Map,
			})
		}
	}
	return out
}

// sourceInHistory reports whether source is the Application's current source (one of its
// sources, its hydrator dry or sync source) or one it synced before (upstream
// isSourceInHistory: history entries compare with the synced revision).
func sourceInHistory(app *v1alpha1.Application, source *v1alpha1.ApplicationSource, index int, versionID int64) bool {
	if h := app.Spec.SourceHydrator; h != nil {
		dry, sync := h.GetDrySource(), h.GetSyncSource()
		return sourceEquals(source, &dry) || sourceEquals(source, &sync)
	}
	if app.Spec.HasMultipleSources() {
		for i := range app.Spec.Sources {
			if sourceEquals(source, &app.Spec.Sources[i]) {
				return true
			}
		}
	} else if app.Spec.Source != nil && sourceEquals(source, app.Spec.Source) {
		return true
	}
	for _, h := range app.Status.History {
		if len(h.Sources) > 0 {
			if h.ID != versionID || index < 0 || index >= len(h.Sources) || index >= len(h.Revisions) {
				continue
			}
			src := h.Sources[index].DeepCopy()
			src.TargetRevision = h.Revisions[index]
			if sourceEquals(source, src) {
				return true
			}
			continue
		}
		if h.Source == nil {
			continue
		}
		src := h.Source.DeepCopy()
		src.TargetRevision = h.Revision
		if sourceEquals(source, src) {
			return true
		}
	}
	return false
}

// sourceEquals is upstream ApplicationSource.Equals: empty Helm and plugin blocks equal
// absent ones; everything else must be deeply equal.
func sourceEquals(a, b *v1alpha1.ApplicationSource) bool {
	if a == nil || b == nil {
		return a == b
	}
	ac, bc := a.DeepCopy(), b.DeepCopy()
	if isZero(ac.Helm) {
		ac.Helm = nil
	}
	if isZero(bc.Helm) {
		bc.Helm = nil
	}
	if isZero(ac.Plugin) {
		ac.Plugin = nil
	}
	if isZero(bc.Plugin) {
		bc.Plugin = nil
	}
	return reflect.DeepEqual(ac, bc)
}

func isZero[T any](p *T) bool { return p == nil || reflect.ValueOf(*p).IsZero() }
