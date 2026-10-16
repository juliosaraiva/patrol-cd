package applications

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	pubapi "github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// appFilter is the item filter of the list and watch routes.
type appFilter struct {
	name      string
	namespace string // only items of this namespace ("" = any enabled namespace)
	projects  map[string]bool
	repo      string
	selector  labels.Selector
	minRV     int64
}

func (s *Service) newAppFilter(q *pubapi.ApplicationQuery, watch bool) (*appFilter, error) {
	sel, err := labels.Parse(q.Selector)
	if err != nil {
		return nil, rest.InvalidArgument("error parsing the selector: %v", err)
	}
	f := &appFilter{name: q.Name, namespace: q.AppNamespace, repo: q.Repo, selector: sel}
	// Upstream: watching one Application by name defaults its namespace to the control
	// plane's.
	if watch && q.Name != "" && f.namespace == "" {
		f.namespace = s.controlNS()
	}
	projects := q.Project
	if len(projects) == 0 {
		projects = q.Projects
	}
	if len(projects) > 0 {
		f.projects = map[string]bool{}
		for _, p := range projects {
			f.projects[p] = true
		}
	}
	if q.ResourceVersion != "" {
		if v, err := strconv.ParseInt(q.ResourceVersion, 10, 64); err == nil {
			f.minRV = v
		}
	}
	return f, nil
}

// matches applies every filter except RBAC.
func (s *Service) filterMatches(f *appFilter, a *v1alpha1.Application) bool {
	if !s.namespaceEnabled(a.Namespace) {
		return false
	}
	if f.name != "" && a.Name != f.name {
		return false
	}
	if f.namespace != "" && a.Namespace != f.namespace {
		return false
	}
	if len(f.projects) > 0 && !f.projects[a.Spec.GetProject()] {
		return false
	}
	if f.repo != "" && !slices.ContainsFunc(a.Spec.GetSources(), func(src v1alpha1.ApplicationSource) bool { return src.RepoURL == f.repo }) {
		return false
	}
	if f.minRV > 0 {
		if v, err := strconv.ParseInt(a.ResourceVersion, 10, 64); err == nil && v < f.minRV {
			return false
		}
	}
	return f.selector.Matches(labels.Set(a.Labels))
}

// listScope returns the list options selecting the namespaces to read.
func (s *Service) listScope(appNamespace string) (opts []client.ListOption, ok bool) {
	switch {
	case appNamespace != "":
		if !s.namespaceEnabled(appNamespace) {
			return nil, false
		}
		return []client.ListOption{client.InNamespace(appNamespace)}, true
	case !s.ns.clusterWide():
		return []client.ListOption{client.InNamespace(s.controlNS())}, true
	}
	return nil, true
}

// listToken is the decoded continue token of the application list (the last item sent).
type listToken struct {
	Name      string `json:"n"`
	Namespace string `json:"ns"`
}

func encodeToken(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeToken(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) > 4096 || json.Unmarshal(b, v) != nil {
		return rest.InvalidArgument("invalid continue token")
	}
	return nil
}

func compareApps(a, b *v1alpha1.Application) int {
	return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Namespace, b.Namespace))
}

// listApplications serves GET /applications: the enabled-namespace Applications the
// caller may get, filtered, sorted by name (then namespace), optionally paginated.
func (s *Service) listApplications(ctx context.Context, q *pubapi.ApplicationQuery) (*v1alpha1.ApplicationList, error) {
	if s.d.Reader == nil {
		return nil, unavailable("the Kubernetes API")
	}
	if q.Limit < 0 || q.Limit > MaxListLimit {
		return nil, rest.InvalidArgument("limit must be between 0 and %d", MaxListLimit)
	}
	f, err := s.newAppFilter(q, false)
	if err != nil {
		return nil, err
	}
	var after *listToken
	if q.Continue != "" {
		after = &listToken{}
		if err := decodeToken(q.Continue, after); err != nil {
			return nil, err
		}
	}
	out := &v1alpha1.ApplicationList{Items: []v1alpha1.Application{}}
	opts, ok := s.listScope(q.AppNamespace)
	if !ok {
		return out, nil
	}
	list := &v1alpha1.ApplicationList{}
	opts = append(opts, client.MatchingLabelsSelector{Selector: f.selector})
	if err := s.d.Reader.List(ctx, list, opts...); err != nil {
		return nil, err
	}
	out.ResourceVersion = list.ResourceVersion
	items := make([]*v1alpha1.Application, 0, len(list.Items))
	for i := range list.Items {
		a := &list.Items[i]
		if !s.filterMatches(f, a) || !s.canApp(ctx, rbac.ActionGet, a) {
			continue
		}
		items = append(items, a)
	}
	slices.SortFunc(items, compareApps)
	if after != nil {
		marker := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: after.Name, Namespace: after.Namespace}}
		i, _ := slices.BinarySearchFunc(items, marker, compareApps)
		for i < len(items) && compareApps(items[i], marker) <= 0 {
			i++
		}
		items = items[i:]
	}
	if q.Limit > 0 && int64(len(items)) > q.Limit {
		items = items[:q.Limit]
		last := items[len(items)-1]
		out.Continue = encodeToken(listToken{Name: last.Name, Namespace: last.Namespace})
	}
	for _, a := range items {
		out.Items = append(out.Items, *a.DeepCopy())
	}
	return out, nil
}

// getApplication serves GET /applications/{name}, with an optional refresh.
func (s *Service) getApplication(ctx context.Context, req *pubapi.ApplicationGetRequest) (*v1alpha1.Application, error) {
	projects := req.Project
	if len(projects) == 0 {
		projects = req.Projects
	}
	if len(projects) > 1 {
		return nil, rest.InvalidArgument("multiple projects specified - the get endpoint accepts either zero or one project")
	}
	proj := ""
	if len(projects) == 1 {
		proj = projects[0]
	}
	app, p, err := s.getApp(ctx, rbac.ActionGet, appRequest{name: req.Name, namespace: req.AppNamespace, project: proj, resourceVersion: req.ResourceVersion})
	if err != nil {
		return nil, err
	}
	if req.Refresh == "" {
		return app, nil
	}
	refreshType := v1alpha1.RefreshTypeNormal
	if req.Refresh == string(v1alpha1.RefreshTypeHard) {
		refreshType = v1alpha1.RefreshTypeHard
	}
	return s.refresh(ctx, app, p, refreshType)
}

// refresh sets the refresh annotation (and, for hydrator Applications, the hydrate
// annotation), then waits until the controller reconciled the Application: an object
// with a newer resourceVersion and without the refresh annotation. The wait is bounded by
// Config.RefreshTimeout and ends when the client disconnects.
func (s *Service) refresh(ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject, refreshType v1alpha1.RefreshType) (*v1alpha1.Application, error) {
	c, err := s.kube()
	if err != nil {
		return nil, err
	}
	// A refresh writes the Application: always audited.
	if a := rest.AuditFrom(ctx); a != nil {
		a.SetCheck(rbac.ResourceApplications, rbac.ActionGet, s.rbacObject(app))
		a.SetMessage("%s requested a %s refresh", actorName(ctx), refreshType)
		a.Record()
	}
	var events <-chan Event
	if s.d.Events != nil {
		ch, cancel := s.d.Events.Subscribe()
		defer cancel()
		events = ch
	}
	annotations := map[string]string{v1alpha1.AnnotationKeyRefresh: string(refreshType)}
	if app.Spec.SourceHydrator != nil {
		hydrate := v1alpha1.HydrateTypeNormal
		if refreshType == v1alpha1.RefreshTypeHard {
			hydrate = v1alpha1.HydrateTypeHard
		}
		annotations[v1alpha1.AnnotationKeyHydrate] = string(hydrate)
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return nil, rest.Internal(err)
	}
	patched := app.DeepCopy()
	if err := c.Patch(ctx, patched, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return nil, err
	}
	if refreshType == v1alpha1.RefreshTypeHard {
		s.forceAppDetails(ctx, patched, proj)
	}

	wctx, cancel := context.WithTimeout(ctx, s.d.Config.RefreshTimeout)
	defer cancel()
	minRV := patched.ResourceVersion
	done := func(a *v1alpha1.Application) bool {
		if a == nil || a.Namespace != app.Namespace || a.Name != app.Name || !newerRV(a.ResourceVersion, minRV) {
			return false
		}
		_, pending := a.Annotations[v1alpha1.AnnotationKeyRefresh]
		return !pending
	}
	ticker := time.NewTicker(s.d.Config.RefreshPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-wctx.Done():
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, &rest.Error{Status: 504, Code: pubapi.CodeUnavailable, Detail: "application refresh deadline exceeded"}
		case ev, ok := <-events:
			if !ok {
				events = nil // fell behind: keep polling
				continue
			}
			if ev.Type == pubapi.EventDeleted && ev.Application.Namespace == app.Namespace && ev.Application.Name == app.Name {
				return nil, appNotFound(app.Name)
			}
			if done(ev.Application) {
				return ev.Application.DeepCopy(), nil
			}
		case <-ticker.C:
			cur := &v1alpha1.Application{}
			if err := c.Get(wctx, client.ObjectKeyFromObject(app), cur); err != nil {
				if apierrors.IsNotFound(err) {
					return nil, appNotFound(app.Name)
				}
				continue
			}
			if done(cur) {
				return cur, nil
			}
		}
	}
}

// forceAppDetails asks the repo-server for the primary source's details without its cache
// (upstream hard refresh); failures are only logged.
func (s *Service) forceAppDetails(ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject) {
	if s.d.RepoServer == nil || s.d.Repositories == nil || proj == nil {
		return
	}
	sources := app.Spec.GetSources()
	if len(sources) == 0 {
		return
	}
	st, err := s.settings()
	if err != nil {
		return
	}
	source := sources[0]
	repo, err := s.d.Repositories.GetRepository(ctx, source.RepoURL, proj.Name)
	if err != nil {
		s.log().DebugContext(ctx, "applications: hard refresh: repository not resolved", "error", err)
		return
	}
	repos, creds, err := s.d.Repositories.ListDependencyRepositories(ctx, proj, isOCI(source.RepoURL))
	if err != nil {
		s.log().DebugContext(ctx, "applications: hard refresh: dependency repositories not resolved", "error", err)
		return
	}
	_, err = s.d.RepoServer.GetAppDetails(ctx, &apiclient.AppDetailsQuery{
		Repo: *repo, Source: source, Repos: repos, HelmRepoCreds: creds,
		KustomizeOptions: kustomizeOptions(st), HelmOptions: apiclient.HelmOptions{ValuesFileSchemes: st.HelmValuesFileSchemes()},
		AppName: app.Name, NoCache: true, EnabledSourceTypes: enabledSourceTypes(st),
	})
	if err != nil {
		s.log().DebugContext(ctx, "applications: failed to force refresh application details", "error", err)
	}
}

// newerRV reports whether resource version a is newer than b (numeric comparison when both
// are numbers, as etcd versions are; otherwise any difference counts).
func newerRV(a, b string) bool {
	va, errA := strconv.ParseUint(a, 10, 64)
	vb, errB := strconv.ParseUint(b, 10, 64)
	if errA == nil && errB == nil {
		return va > vb
	}
	return a != b
}

// informerSyncTimeout bounds waitCached (upstream waitSync: 2 s).
const informerSyncTimeout = 2 * time.Second

// waitCached waits (bounded) until the cached Reader serves app at its resourceVersion or
// later, so a list right after a write reflects it (upstream waitSync). It does nothing
// when reads are not cached.
func (s *Service) waitCached(ctx context.Context, app *v1alpha1.Application) {
	if !s.cachedReads || s.d.Reader == nil || app == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, informerSyncTimeout)
	defer cancel()
	for {
		cur := &v1alpha1.Application{}
		if err := s.d.Reader.Get(ctx, client.ObjectKeyFromObject(app), cur); err == nil &&
			(cur.ResourceVersion == app.ResourceVersion || newerRV(cur.ResourceVersion, app.ResourceVersion)) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// sanitizeNewApp clears the metadata a client cannot set on creation.
func sanitizeNewApp(a *v1alpha1.Application) {
	a.ResourceVersion = ""
	a.UID = ""
	a.Generation = 0
	a.CreationTimestamp = metav1.Time{}
	a.DeletionTimestamp = nil
	a.DeletionGracePeriodSeconds = nil
	a.ManagedFields = nil
	a.Status = v1alpha1.ApplicationStatus{}
}

// createApplication serves POST /applications.
func (s *Service) createApplication(ctx context.Context, req *pubapi.ApplicationCreateRequest) (*v1alpha1.Application, error) {
	c, err := s.kube()
	if err != nil {
		return nil, err
	}
	if req.Application == nil {
		return nil, rest.InvalidArgument("error creating application: application is nil in request")
	}
	a := req.Application.DeepCopy()
	ns, err := effectiveValue("metadata.namespace", req.AppNamespace, a.Namespace)
	if err != nil {
		return nil, err
	}
	a.Namespace = s.nsOrDefault(ns)
	sanitizeNewApp(a)
	if a.Name == "" {
		return nil, rest.InvalidArgument("resource name may not be empty")
	}
	if err := s.authorizeApp(ctx, rbac.ActionCreate, a); err != nil {
		return nil, err
	}
	if !s.namespaceEnabled(a.Namespace) {
		return nil, rest.PermissionDenied("%s", namespaceNotPermitted(a.Namespace).Error())
	}
	validate := req.Validate == nil || *req.Validate
	proj, err := s.appProject(ctx, a)
	if err != nil {
		return nil, err
	}
	existing := &v1alpha1.Application{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(a), existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
		existing = nil
	}
	if existing != nil && !s.canApp(ctx, rbac.ActionGet, existing) {
		// The name is taken by an Application the caller may not see: report the conflict
		// without revealing its content.
		return nil, rest.AlreadyExists("application %q already exists", a.Name)
	}
	if _, err := s.validateAndNormalize(ctx, a, proj, validate, existing); err != nil {
		return nil, err
	}
	if existing == nil {
		if err := c.Create(ctx, a); err == nil {
			auditApp(ctx, a, rbac.ActionCreate, s.rbacObject(a), rest.ReasonResourceCreated, "created application")
			s.waitCached(ctx, a)
			return a, nil
		} else if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		// Created concurrently after the first read: validateAndNormalize saw no
		// Application, so the project change rules (update on the old project) are applied
		// to the one that won the race. The spec was validated against a's project.
		existing = &v1alpha1.Application{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(a), existing); err != nil {
			return nil, err
		}
		if !s.canApp(ctx, rbac.ActionGet, existing) {
			return nil, rest.AlreadyExists("application %q already exists", a.Name)
		}
		if err := s.authorizeProjectChange(ctx, a, existing); err != nil {
			return nil, err
		}
	}
	if sameApp(existing, a) {
		return existing, nil
	}
	if req.Upsert == nil || !*req.Upsert {
		return nil, rest.InvalidArgument("existing application spec is different, use upsert flag to force update")
	}
	if err := s.authorizeApp(ctx, rbac.ActionUpdate, a); err != nil {
		return nil, err
	}
	// The project change rules ran against existing (validateAndNormalize, or above after
	// a lost create race); updateApp refuses an Application moved since.
	updated, err := s.updateApp(ctx, existing, a, metaMerge, "")
	if err != nil {
		return nil, err
	}
	auditApp(ctx, updated, rbac.ActionUpdate, s.rbacObject(updated), rest.ReasonResourceUpdated, "updated application spec")
	return updated, nil
}

// sameApp reports whether an existing Application already has the desired spec, labels,
// annotations and finalizers (idempotent create).
func sameApp(existing, desired *v1alpha1.Application) bool {
	return jsonEqual(existing.Spec, desired.Spec) && jsonEqual(existing.Labels, desired.Labels) &&
		jsonEqual(existing.Annotations, desired.Annotations) && jsonEqual(existing.Finalizers, desired.Finalizers)
}

func jsonEqual(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	norm := func(b []byte) []byte {
		switch string(b) {
		case "null", "{}", "[]":
			return nil
		}
		return b
	}
	return bytes.Equal(norm(ja), norm(jb))
}

// metaMode selects what updateApp does with the metadata of the new Application.
type metaMode int

const (
	// metaReplace replaces labels, annotations and finalizers (PUT /applications/{name}).
	metaReplace metaMode = iota
	// metaMerge merges labels and annotations and replaces finalizers (upsert).
	metaMerge
	// metaKeep writes the spec only: the stored labels, annotations and finalizers of the
	// object being updated are kept, including on a retry after a conflict
	// (PUT /applications/{name}/spec).
	metaKeep
)

// updateApp copies the spec of newApp onto the stored Application, and its labels,
// annotations and finalizers as mode says. With a resourceVersion the update is
// conditional (409 when stale); otherwise conflicts are retried on a fresh read, provided
// the Application stayed in the same project.
func (s *Service) updateApp(ctx context.Context, existing, newApp *v1alpha1.Application, mode metaMode, resourceVersion string) (*v1alpha1.Application, error) {
	c, err := s.kube()
	if err != nil {
		return nil, err
	}
	project := existing.Spec.GetProject()
	cur := existing.DeepCopy()
	for range maxUpdateAttempts {
		cur.Spec = *newApp.Spec.DeepCopy()
		switch mode {
		case metaReplace:
			cur.Labels = maps.Clone(newApp.Labels)
			cur.Annotations = maps.Clone(newApp.Annotations)
			cur.Finalizers = slices.Clone(newApp.Finalizers)
		case metaMerge:
			cur.Labels = mergeMaps(cur.Labels, newApp.Labels)
			cur.Annotations = mergeMaps(cur.Annotations, newApp.Annotations)
			cur.Finalizers = slices.Clone(newApp.Finalizers)
		case metaKeep:
		}
		if resourceVersion != "" {
			cur.ResourceVersion = resourceVersion
		}
		err := c.Update(ctx, cur)
		if err == nil {
			s.waitCached(ctx, cur)
			return cur, nil
		}
		if !apierrors.IsConflict(err) || resourceVersion != "" {
			return nil, err
		}
		next := &v1alpha1.Application{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(existing), next); err != nil {
			return nil, err
		}
		if next.Spec.GetProject() != project || next.UID != existing.UID {
			return nil, rest.Conflict("the application changed concurrently; retry the request")
		}
		cur = next
	}
	return nil, &rest.Error{Status: 500, Code: pubapi.CodeInternal, Detail: "Failed to update application. Too many conflicts"}
}

func mergeMaps(a, b map[string]string) map[string]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := maps.Clone(a)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, b)
	return out
}

// validateAndUpdate reads the stored Application (authorizing action on it with the
// existence-hiding rules), validates newApp against the effective project and updates it.
func (s *Service) validateAndUpdate(ctx context.Context, newApp *v1alpha1.Application, mode metaMode, validate bool, action, currentProject, resourceVersion string) (*v1alpha1.Application, error) {
	existing, proj, err := s.getApp(ctx, action, appRequest{name: newApp.Name, namespace: newApp.Namespace, project: currentProject})
	if err != nil {
		return nil, err
	}
	if _, err := s.validateAndNormalize(ctx, newApp, proj, validate, existing); err != nil {
		return nil, err
	}
	updated, err := s.updateApp(ctx, existing, newApp, mode, resourceVersion)
	if err != nil {
		return nil, err
	}
	auditApp(ctx, updated, rbac.ActionUpdate, s.rbacObject(updated), rest.ReasonResourceUpdated, "updated application spec")
	return updated, nil
}

// updateApplication serves PUT /applications/{name}.
func (s *Service) updateApplication(ctx context.Context, req *pubapi.ApplicationUpdateRequest) (*v1alpha1.Application, error) {
	if req.Application == nil {
		return nil, rest.InvalidArgument("error updating application: application is nil in request")
	}
	a := req.Application.DeepCopy()
	if a.Name == "" {
		a.Name = req.Name
	}
	if a.Name != req.Name {
		return nil, rest.InvalidArgument("metadata.name %q does not match the path name %q", a.Name, req.Name)
	}
	ns, err := effectiveValue("metadata.namespace", req.AppNamespace, a.Namespace)
	if err != nil {
		return nil, err
	}
	a.Namespace = s.nsOrDefault(ns)
	if err := s.authorizeApp(ctx, rbac.ActionUpdate, a); err != nil {
		return nil, err
	}
	validate := req.Validate == nil || *req.Validate
	return s.validateAndUpdate(ctx, a, metaReplace, validate, rbac.ActionUpdate, req.Project, a.ResourceVersion)
}

// updateApplicationSpec serves PUT /applications/{name}/spec. Only the spec is written:
// metadata changed concurrently (labels, annotations, a cascade finalizer added by a
// delete) is kept, conflicts re-apply the spec to a fresh read.
func (s *Service) updateApplicationSpec(ctx context.Context, req *pubapi.ApplicationUpdateSpecRequest) (*v1alpha1.ApplicationSpec, error) {
	if req.Spec == nil {
		return nil, rest.InvalidArgument("error updating application spec: spec is nil in request")
	}
	a, err := s.getAppOnly(ctx, rbac.ActionUpdate, appRequest{name: req.Name, namespace: req.AppNamespace, project: req.Project})
	if err != nil {
		return nil, err
	}
	a.Spec = *req.Spec.DeepCopy()
	validate := req.Validate == nil || *req.Validate
	updated, err := s.validateAndUpdate(ctx, a, metaKeep, validate, rbac.ActionUpdate, req.Project, "")
	if err != nil {
		return nil, err
	}
	return &updated.Spec, nil
}

// patchApplication serves PATCH /applications/{name}: the patch is applied to the stored
// Application JSON, then the result goes through the update path with validation. A patch
// that sets metadata.resourceVersion makes the update conditional; otherwise conflicts
// re-apply the patch to a fresh read.
func (s *Service) patchApplication(ctx context.Context, req *pubapi.ApplicationPatchRequest) (*v1alpha1.Application, error) {
	if err := checkBodyName(req.Name, req.BodyName); err != nil {
		return nil, err
	}
	ns, err := effectiveValue("appNamespace", req.AppNamespace, req.BodyAppNamespace)
	if err != nil {
		return nil, err
	}
	projectName, err := effectiveValue("project", req.Project, req.BodyProject)
	if err != nil {
		return nil, err
	}
	switch req.PatchType {
	case "", pubapi.PatchTypeJSON, pubapi.PatchTypeMerge:
	default:
		return nil, rest.InvalidArgument("Patch type '%s' is not supported", req.PatchType)
	}
	for range maxUpdateAttempts {
		app, err := s.getAppOnly(ctx, rbac.ActionGet, appRequest{name: req.Name, namespace: ns, project: projectName})
		if err != nil {
			return nil, err
		}
		if err := s.authorizeApp(ctx, rbac.ActionUpdate, app); err != nil {
			return nil, err
		}
		newApp, err := applyAppPatch(app, req.Patch, req.PatchType)
		if err != nil {
			return nil, err
		}
		if newApp.Name != app.Name || s.nsOrDefault(newApp.Namespace) != app.Namespace {
			return nil, rest.InvalidArgument("the patch must not change metadata.name or metadata.namespace")
		}
		newApp.Namespace = app.Namespace
		asserted := newApp.ResourceVersion != app.ResourceVersion
		rv := app.ResourceVersion
		if asserted {
			rv = newApp.ResourceVersion
		}
		updated, err := s.validateAndUpdate(ctx, newApp, metaReplace, true, rbac.ActionUpdate, projectName, rv)
		if err == nil {
			return updated, nil
		}
		if asserted || !apierrors.IsConflict(err) {
			return nil, err
		}
	}
	return nil, &rest.Error{Status: 500, Code: pubapi.CodeInternal, Detail: "Failed to update application. Too many conflicts"}
}

// Bounds of PATCH /applications/{name}: the body cap does not bound the effect of a JSON
// patch, whose "copy" operations can double the document each (Kubernetes applies the
// same kind of limits to its own JSON patches).
const (
	// maxPatchOperations caps the operations of a JSON patch.
	maxPatchOperations = 1000
	// maxPatchCopyBytes caps the bytes added by the "copy" operations of a JSON patch.
	maxPatchCopyBytes int64 = 1 << 20
	// maxPatchedBytes caps the patched Application document.
	maxPatchedBytes = rest.DefaultMaxBodyBytes
)

// patchTooLarge is the 413 of a patch whose effect exceeds the bounds.
func patchTooLarge(format string, args ...any) error {
	return &rest.Error{Status: http.StatusRequestEntityTooLarge, Code: pubapi.CodeResourceExhausted, Detail: fmt.Sprintf(format, args...)}
}

// applyAppPatch applies a JSON (RFC 6902) or merge (RFC 7386) patch to app, within the
// operation, copy and result size bounds.
func applyAppPatch(app *v1alpha1.Application, patch, patchType string) (*v1alpha1.Application, error) {
	orig := app.DeepCopy()
	orig.APIVersion, orig.Kind = v1alpha1.GroupVersion.String(), "Application"
	doc, err := json.Marshal(orig)
	if err != nil {
		return nil, rest.Internal(err)
	}
	var patched []byte
	switch patchType {
	case "", pubapi.PatchTypeJSON:
		p, err := jsonpatch.DecodePatch([]byte(patch))
		if err != nil {
			return nil, rest.InvalidArgument("error decoding json patch: %v", err)
		}
		if len(p) > maxPatchOperations {
			return nil, patchTooLarge("the allowed maximum operations in a JSON patch is %d, got %d", maxPatchOperations, len(p))
		}
		opts := jsonpatch.NewApplyOptions()
		opts.AccumulatedCopySizeLimit = maxPatchCopyBytes
		if patched, err = p.ApplyWithOptions(doc, opts); err != nil {
			if _, ok := errors.AsType[*jsonpatch.AccumulatedCopySizeError](err); ok {
				return nil, patchTooLarge("error applying patch: %v", err)
			}
			return nil, rest.InvalidArgument("error applying patch: %v", err)
		}
	case pubapi.PatchTypeMerge:
		if patched, err = jsonpatch.MergePatch(doc, []byte(patch)); err != nil {
			return nil, rest.InvalidArgument("error calculating merge patch: %v", err)
		}
	}
	if int64(len(patched)) > maxPatchedBytes {
		return nil, patchTooLarge("the patched application is %d bytes, exceeding the limit %d", len(patched), maxPatchedBytes)
	}
	out := &v1alpha1.Application{}
	if err := json.Unmarshal(patched, out); err != nil {
		return nil, rest.InvalidArgument("error unmarshaling patched app: %v", err)
	}
	return out, nil
}

// propagationFinalizer maps a propagation policy to its finalizer ("" for an invalid
// policy).
func propagationFinalizer(policy string) string {
	switch strings.ToLower(policy) {
	case "":
		return v1alpha1.FinalizerResources
	case "foreground":
		return v1alpha1.FinalizerResourcesForeground
	case "background":
		return v1alpha1.FinalizerResourcesBackground
	}
	return ""
}

var propagationFinalizers = []string{v1alpha1.FinalizerResources, v1alpha1.FinalizerResourcesForeground, v1alpha1.FinalizerResourcesBackground}

// deleteApplication serves DELETE /applications/{name}: cascading deletes add the
// propagation finalizer of the requested policy, non-cascading deletes remove every
// propagation finalizer, then the Application is deleted (UID precondition).
func (s *Service) deleteApplication(ctx context.Context, req *pubapi.ApplicationDeleteRequest) (*pubapi.ApplicationResponse, error) {
	c, err := s.kube()
	if err != nil {
		return nil, err
	}
	app, err := s.getAppOnly(ctx, rbac.ActionGet, appRequest{name: req.Name, namespace: req.AppNamespace, project: req.Project})
	if err != nil {
		return nil, err
	}
	if err := s.authorizeApp(ctx, rbac.ActionDelete, app); err != nil {
		return nil, err
	}
	cascade := req.Cascade == nil || *req.Cascade
	if !cascade && req.PropagationPolicy != "" {
		return nil, rest.InvalidArgument("cannot set propagation policy when cascading is disabled")
	}
	finalizer := ""
	if cascade {
		if finalizer = propagationFinalizer(req.PropagationPolicy); finalizer == "" {
			return nil, rest.InvalidArgument("invalid propagation policy: %s", req.PropagationPolicy)
		}
	}
	cur := app
	for attempt := 0; ; attempt++ {
		want := slices.Clone(cur.Finalizers)
		switch {
		case cascade && cur.DeletionTimestamp == nil && !slices.Contains(want, finalizer):
			// Kubernetes refuses new finalizers on objects being deleted.
			want = append(want, finalizer)
		case !cascade:
			want = slices.DeleteFunc(want, func(f string) bool { return slices.Contains(propagationFinalizers, f) })
		}
		if slices.Equal(want, cur.Finalizers) {
			break
		}
		next := cur.DeepCopy()
		next.Finalizers = want
		err := c.Patch(ctx, next, client.MergeFromWithOptions(cur, client.MergeFromWithOptimisticLock{}))
		if err == nil {
			cur = next
			break
		}
		if !apierrors.IsConflict(err) || attempt >= maxUpdateAttempts {
			return nil, err
		}
		fresh := &v1alpha1.Application{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(app), fresh); err != nil {
			return nil, err
		}
		if fresh.UID != app.UID || fresh.Spec.GetProject() != app.Spec.GetProject() {
			return nil, rest.Conflict("the application changed concurrently; retry the request")
		}
		cur = fresh
	}
	uid := app.UID
	if err := c.Delete(ctx, cur, client.Preconditions{UID: &uid}); err != nil {
		// Removing the last finalizer of an Application already being deleted removes it.
		if !apierrors.IsNotFound(err) || cur.DeletionTimestamp == nil {
			return nil, err
		}
	}
	auditApp(ctx, app, rbac.ActionDelete, s.rbacObject(app), rest.ReasonResourceDeleted, "deleted application")
	return &pubapi.ApplicationResponse{}, nil
}
