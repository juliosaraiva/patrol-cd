package applicationsets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/appset"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Kind and API version of ApplicationSets and Applications (responses carry them).
const (
	kindApplicationSet = "ApplicationSet"
	kindApplication    = "Application"
	// maxContinueLen bounds the continue token accepted from clients.
	maxContinueLen = 1024
)

var apiVersion = v1alpha1.GroupVersion.String()

// forbidden is the uninformative denial of reads of existing ApplicationSets: the same
// answer for "does not exist" and "may not see it".
func forbidden() error { return rest.PermissionDenied("permission denied") }

func namespaceNotPermitted(ns string) error {
	return rest.PermissionDenied("namespace '%s' is not permitted", ns)
}

func (s *Service) controlNS() string { return s.d.Config.Namespace }

func (s *Service) nsOrDefault(ns string) string {
	if ns == "" {
		return s.controlNS()
	}
	return ns
}

// namespaceEnabled reports whether ApplicationSets may live in ns: the control-plane
// namespace or one matching Config.Namespaces (exact, glob or "/regexp/").
func (s *Service) namespaceEnabled(ns string) bool {
	if ns == "" {
		return false
	}
	if ns == s.controlNS() {
		return true
	}
	// The sourceNamespaces matcher implements exactly these pattern rules.
	return project.IsAppNamespacePermitted(&v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{SourceNamespaces: s.d.Config.Namespaces}}, ns, s.controlNS())
}

// objectFor returns the RBAC object of an ApplicationSet whose template project is proj.
func (s *Service) objectFor(proj, ns, name string) string {
	if proj == "" {
		proj = "default"
	}
	return rbac.AppObject(s.controlNS(), proj, s.nsOrDefault(ns), name)
}

// object returns the primary RBAC object of as (literal spec.template.spec.project, as
// upstream), recorded in audit entries.
func (s *Service) object(as *v1alpha1.ApplicationSet) string {
	return s.objectFor(as.Spec.Template.Spec.GetProject(), as.Namespace, as.Name)
}

// wildcardProject stands for a templated project in the RBAC objects of an existing
// ApplicationSet: only a grant that covers every project matches it.
const wildcardProject = "*"

// objects returns the RBAC objects of an existing ApplicationSet: one per project its
// Applications can be generated in (appset.Projects: spec.template.spec.project and the
// project of every generator-level template). Every action on it must be authorized on all
// of them, so a tenant cannot read, rewrite or delete an ApplicationSet that generates
// Applications in another tenant's project. A templated project (only objects created outside
// the API have one) fails closed: it is represented by the wildcard project.
func (s *Service) objects(as *v1alpha1.ApplicationSet) []string {
	projects, err := appset.Projects(as)
	if err != nil {
		return []string{s.objectFor(wildcardProject, as.Namespace, as.Name)}
	}
	out := make([]string, 0, len(projects))
	for _, p := range projects {
		out = append(out, s.objectFor(p, as.Namespace, as.Name))
	}
	return out
}

// can reports whether the caller may perform action on every RBAC object of as.
func (s *Service) can(ctx context.Context, action string, as *v1alpha1.ApplicationSet) bool {
	for _, o := range s.objects(as) {
		if !rest.Can(ctx, rbac.ResourceApplicationSets, action, o) {
			return false
		}
	}
	return true
}

// authorize authorizes action on every RBAC object of as (each denial is audited).
func (s *Service) authorize(ctx context.Context, action string, as *v1alpha1.ApplicationSet) error {
	for _, o := range s.objects(as) {
		if err := rest.Authorize(ctx, rbac.ResourceApplicationSets, action, o); err != nil {
			return err
		}
	}
	return nil
}

// objectsFor returns the RBAC objects of an ApplicationSet whose projects are projects.
func (s *Service) objectsFor(projects []string, ns, name string) []string {
	out := make([]string, 0, len(projects))
	for _, p := range projects {
		out = append(out, s.objectFor(p, ns, name))
	}
	return out
}

func ref(as *v1alpha1.ApplicationSet) rest.ObjectRef {
	return rest.ObjectRef{APIVersion: apiVersion, Kind: kindApplicationSet, Namespace: as.Namespace, Name: as.Name, UID: as.UID}
}

func withTypeMeta(as *v1alpha1.ApplicationSet) *v1alpha1.ApplicationSet {
	as.APIVersion, as.Kind = apiVersion, kindApplicationSet
	return as
}

// getExisting reads an ApplicationSet and authorizes action on it. Missing and forbidden
// objects both yield forbidden() (the denial itself is audited with the full RBAC triple).
func (s *Service) getExisting(ctx context.Context, action, name, namespace string) (*v1alpha1.ApplicationSet, error) {
	if s.d.Reader == nil {
		return nil, unavailable("store")
	}
	ns := s.nsOrDefault(namespace)
	if !s.namespaceEnabled(ns) {
		return nil, namespaceNotPermitted(ns)
	}
	if name == "" {
		return nil, rest.InvalidArgument("name is required")
	}
	as := &v1alpha1.ApplicationSet{}
	if err := s.d.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, as); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, forbidden()
		}
		return nil, fmt.Errorf("error getting ApplicationSet: %w", err)
	}
	if err := s.authorize(ctx, action, as); err != nil {
		return nil, forbidden()
	}
	return as, nil
}

// get serves GET /api/v1/applicationsets/{name}.
func (s *Service) get(ctx context.Context, req *apiclient.ApplicationSetGetRequest) (*v1alpha1.ApplicationSet, error) {
	as, err := s.getExisting(ctx, rbac.ActionGet, req.Name, req.AppsetNamespace)
	if err != nil {
		return nil, err
	}
	return withTypeMeta(as), nil
}

// listKey is the sort (and continue) key of an ApplicationSet: name, then namespace.
type listKey struct {
	Name      string `json:"n"`
	Namespace string `json:"ns"`
}

func keyOf(as *v1alpha1.ApplicationSet) listKey {
	return listKey{Name: as.Name, Namespace: as.Namespace}
}

func (k listKey) compare(o listKey) int {
	if c := strings.Compare(k.Name, o.Name); c != 0 {
		return c
	}
	return strings.Compare(k.Namespace, o.Namespace)
}

func encodeContinue(k listKey) string {
	b, _ := json.Marshal(k)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeContinue(tok string) (listKey, error) {
	var k listKey
	if len(tok) > maxContinueLen {
		return k, errors.New("token too long")
	}
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return k, err
	}
	if err := json.Unmarshal(b, &k); err != nil {
		return k, err
	}
	if k.Name == "" {
		return k, errors.New("empty key")
	}
	return k, nil
}

// projectFilter reports whether as passes the projects filter (empty filter: all).
func projectFilter(projects []string, as *v1alpha1.ApplicationSet) bool {
	if len(projects) == 0 {
		return true
	}
	p := as.Spec.Template.Spec.GetProject()
	return slices.Contains(projects, p) || (as.Spec.Template.Spec.Project == "" && slices.Contains(projects, ""))
}

// list serves GET /api/v1/applicationsets.
func (s *Service) list(ctx context.Context, req *apiclient.ApplicationSetListRequest) (*v1alpha1.ApplicationSetList, error) {
	if s.d.Reader == nil {
		return nil, unavailable("store")
	}
	sel, err := labels.Parse(req.Selector)
	if err != nil {
		return nil, rest.InvalidArgument("error parsing the selector: %v", err).WithCause(err)
	}
	if req.Limit < 0 {
		return nil, rest.InvalidArgument("limit must not be negative")
	}
	var after *listKey
	if req.Continue != "" {
		k, err := decodeContinue(req.Continue)
		if err != nil {
			return nil, rest.InvalidArgument("invalid continue token").WithCause(err)
		}
		after = &k
	}
	out := &v1alpha1.ApplicationSetList{Items: []v1alpha1.ApplicationSet{}}
	out.APIVersion, out.Kind = apiVersion, "ApplicationSetList"
	if req.AppsetNamespace != "" && !s.namespaceEnabled(req.AppsetNamespace) {
		return out, nil
	}
	opts := []client.ListOption{client.MatchingLabelsSelector{Selector: sel}}
	if req.AppsetNamespace != "" {
		opts = append(opts, client.InNamespace(req.AppsetNamespace))
	}
	var all v1alpha1.ApplicationSetList
	if err := s.d.Reader.List(ctx, &all, opts...); err != nil {
		return nil, fmt.Errorf("error listing ApplicationSets with selectors: %w", err)
	}
	out.ResourceVersion = all.ResourceVersion
	items := make([]v1alpha1.ApplicationSet, 0, len(all.Items))
	for i := range all.Items {
		as := &all.Items[i]
		if !s.namespaceEnabled(as.Namespace) || !projectFilter(req.Projects, as) {
			continue
		}
		if !s.can(ctx, rbac.ActionGet, as) {
			continue
		}
		items = append(items, *as)
	}
	slices.SortFunc(items, func(a, b v1alpha1.ApplicationSet) int { return keyOf(&a).compare(keyOf(&b)) })
	if after != nil {
		// Items strictly after the last key of the previous page.
		i := sort.Search(len(items), func(i int) bool { return keyOf(&items[i]).compare(*after) > 0 })
		items = items[i:]
	}
	if req.Limit > 0 && int64(len(items)) > req.Limit {
		items = items[:req.Limit]
		out.Continue = encodeContinue(keyOf(&items[len(items)-1]))
	}
	for i := range items {
		withTypeMeta(&items[i])
	}
	out.Items = items
	return out, nil
}

// validated checks an ApplicationSet from a create or generate request and returns its
// namespace and the projects its Applications can use, after authorizing create on each
// of them and checking that each exists and admits the namespace.
func (s *Service) validated(ctx context.Context, as *v1alpha1.ApplicationSet) (string, []string, error) {
	if as == nil {
		return "", nil, rest.InvalidArgument("error creating ApplicationSets: ApplicationSets is nil in request")
	}
	if as.Name == "" {
		return "", nil, rest.InvalidArgument("error validating ApplicationSets: metadata.name is required")
	}
	if err := appset.ValidateForAPI(as); err != nil {
		return "", nil, rest.InvalidArgument("error validating ApplicationSets: %v", err).WithCause(err)
	}
	ns := s.nsOrDefault(as.Namespace)
	if !s.namespaceEnabled(ns) {
		return "", nil, namespaceNotPermitted(ns)
	}
	as.Namespace = ns
	projects, err := appset.Projects(as)
	if err != nil {
		return "", nil, rest.InvalidArgument("error validating ApplicationSets: %v", err).WithCause(err)
	}
	// Authorize every project before revealing anything about any of them.
	for _, o := range s.objectsFor(projects, ns, as.Name) {
		if err := rest.Authorize(ctx, rbac.ResourceApplicationSets, rbac.ActionCreate, o); err != nil {
			return "", nil, err
		}
	}
	if s.d.Reader == nil {
		return "", nil, unavailable("store")
	}
	for _, p := range projects {
		proj := &v1alpha1.AppProject{}
		if err := s.d.Reader.Get(ctx, client.ObjectKey{Namespace: s.controlNS(), Name: p}, proj); err != nil {
			if apierrors.IsNotFound(err) {
				return "", nil, rest.InvalidArgument("ApplicationSet references project %s which does not exist", p)
			}
			return "", nil, fmt.Errorf("error getting ApplicationSet's project %q: %w", p, err)
		}
		if !project.IsAppNamespacePermitted(proj, ns, s.controlNS()) {
			return "", nil, rest.PermissionDenied("ApplicationSet namespace '%s' is not permitted in project '%s'", ns, p)
		}
	}
	return ns, projects, nil
}

// render runs the generator with the configured bounds: the limits are enforced by the
// Generator while it generates (BoundedGenerator), and checked again here on its output.
func (s *Service) render(ctx context.Context, as *v1alpha1.ApplicationSet) ([]v1alpha1.Application, error) {
	if s.d.Generator == nil {
		return nil, unavailable("generator")
	}
	c := s.d.Config
	gctx, cancel := context.WithTimeout(ctx, c.GenerateTimeout)
	defer cancel()
	limits := GenerateLimits{MaxParameterSets: c.MaxGenerateParameterSets, MaxApplications: c.MaxGeneratedApplications, MaxBytes: c.MaxGenerateResponseBytes}
	apps, err := s.d.Generator.Generate(gctx, as.DeepCopy(), limits)
	if err != nil {
		if errors.Is(gctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, &rest.Error{Status: http.StatusGatewayTimeout, Code: apiclient.CodeUnavailable,
				Detail: fmt.Sprintf("generating the Applications of ApplicationSet %s timed out after %s", as.Name, c.GenerateTimeout), Cause: err}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if le := (*LimitError)(nil); errors.As(err, &le) {
			return nil, rest.InvalidArgument("%s", le.Error()).WithCause(err)
		}
		msg := err.Error()
		if !strings.HasPrefix(msg, "unable to generate Applications of ApplicationSet") {
			msg = "unable to generate Applications of ApplicationSet: " + msg
		}
		return nil, rest.InvalidArgument("%s", msg).WithCause(err)
	}
	if len(apps) > s.d.Config.MaxGeneratedApplications {
		return nil, rest.InvalidArgument("ApplicationSet %s generates %d Applications, more than the limit of %d", as.Name, len(apps), s.d.Config.MaxGeneratedApplications)
	}
	for i := range apps {
		apps[i].APIVersion, apps[i].Kind = apiVersion, kindApplication
	}
	return apps, nil
}

// generate serves POST /api/v1/applicationsets/generate.
func (s *Service) generate(ctx context.Context, req *apiclient.ApplicationSetGenerateRequest) (*apiclient.ApplicationSetGenerateResponse, error) {
	as := req.ApplicationSet
	if as != nil {
		as = as.DeepCopy()
	}
	if _, _, err := s.validated(ctx, as); err != nil {
		return nil, err
	}
	apps, err := s.render(ctx, as)
	if err != nil {
		return nil, err
	}
	out := &apiclient.ApplicationSetGenerateResponse{Applications: apps}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode generated Applications: %w", err)
	}
	if len(b) > s.d.Config.MaxGenerateResponseBytes {
		return nil, rest.InvalidArgument("the Applications generated by ApplicationSet %s exceed the response limit of %d bytes", as.Name, s.d.Config.MaxGenerateResponseBytes)
	}
	rest.AuditFrom(ctx).SetMessage("generated %d Applications of ApplicationSet %s/%s (dry run)", len(apps), as.Namespace, as.Name)
	return out, nil
}

// resourceStatuses converts generated Applications to status.resources entries (upstream
// BuildResourceStatus), sorted by name.
func resourceStatuses(apps []v1alpha1.Application) []v1alpha1.ResourceStatus {
	out := make([]v1alpha1.ResourceStatus, 0, len(apps))
	for _, a := range apps {
		out = append(out, v1alpha1.ResourceStatus{
			Group: v1alpha1.GroupVersion.Group, Version: v1alpha1.GroupVersion.Version, Kind: kindApplication,
			Name: a.Name, Namespace: a.Namespace,
			Status: a.Status.Sync.Status,
			Health: &v1alpha1.HealthStatus{Status: a.Status.Health.Status},
		})
	}
	slices.SortStableFunc(out, func(a, b v1alpha1.ResourceStatus) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// sanitizeForCreate drops server-managed metadata from a requested object.
func sanitizeForCreate(as *v1alpha1.ApplicationSet) {
	as.ResourceVersion = ""
	as.UID = ""
	as.Generation = 0
	as.CreationTimestamp = metav1.Time{}
	as.DeletionTimestamp = nil
	as.DeletionGracePeriodSeconds = nil
	as.ManagedFields = nil
	as.Status = v1alpha1.ApplicationSetStatus{}
}

func (s *Service) username(ctx context.Context) string {
	id := rest.IdentityFrom(ctx)
	if id != nil && id.Principal != nil {
		if u := id.Principal.Username(); u != "" {
			return u
		}
	}
	return "Unknown user"
}

// event records an API change on the ApplicationSet (upstream audit events).
func (s *Service) event(ctx context.Context, as *v1alpha1.ApplicationSet, reason, action string) {
	if s.d.Recorder == nil {
		return
	}
	r := ref(as)
	s.d.Recorder.Eventf(&corev1.ObjectReference{APIVersion: r.APIVersion, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name, UID: r.UID},
		corev1.EventTypeNormal, reason, "%s %s", s.username(ctx), action)
}

// create serves POST /api/v1/applicationsets.
func (s *Service) create(ctx context.Context, req *apiclient.ApplicationSetCreateRequest) (*v1alpha1.ApplicationSet, error) {
	if req.ApplicationSet == nil {
		return nil, rest.InvalidArgument("error creating ApplicationSets: ApplicationSets is nil in request")
	}
	as := req.ApplicationSet.DeepCopy()
	requestedRV := as.ResourceVersion
	ns, projects, err := s.validated(ctx, as)
	if err != nil {
		return nil, err
	}
	audit := rest.AuditFrom(ctx)
	audit.SetCheck(rbac.ResourceApplicationSets, rbac.ActionCreate, s.object(as))
	if req.DryRun {
		apps, err := s.render(ctx, as)
		if err != nil {
			return nil, err
		}
		as.Status.Resources = resourceStatuses(apps)
		as.Status.ResourcesCount = int64(len(apps))
		audit.SetMessage("dry run of ApplicationSet %s/%s: %d Applications", ns, as.Name, len(apps))
		return withTypeMeta(as), nil
	}
	if s.d.Writer == nil {
		return nil, unavailable("store")
	}
	sanitizeForCreate(as)
	created := as.DeepCopy()
	err = s.d.Writer.Create(ctx, created)
	if err == nil {
		audit.SetTarget(ref(created))
		audit.SetMessage("created ApplicationSet %s/%s", ns, created.Name)
		s.event(ctx, created, rest.ReasonResourceCreated, "created ApplicationSet")
		return withTypeMeta(created), nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("error creating ApplicationSet: %w", err)
	}
	existing := &v1alpha1.ApplicationSet{}
	if err := s.d.APIReader.Get(ctx, client.ObjectKeyFromObject(as), existing); err != nil {
		return nil, fmt.Errorf("unable to check existing ApplicationSet details: %w", err)
	}
	if sameDesiredState(existing, as) {
		return withTypeMeta(existing), nil
	}
	if !req.Upsert {
		return nil, rest.InvalidArgument("existing ApplicationSet spec is different, use upsert flag to force update")
	}
	// Upsert: update on every project of the new spec (create was checked above) and on every
	// project of the existing object, generator-level templates included: replacing an
	// ApplicationSet changes or deletes the Applications it generates in each of them.
	checked := map[string]bool{}
	for _, o := range append(s.objectsFor(projects, ns, as.Name), s.objects(existing)...) {
		if checked[o] {
			continue
		}
		checked[o] = true
		if err := rest.Authorize(ctx, rbac.ResourceApplicationSets, rbac.ActionUpdate, o); err != nil {
			return nil, err
		}
	}
	audit.SetCheck(rbac.ResourceApplicationSets, rbac.ActionUpdate, s.object(as))
	updated, err := s.update(ctx, existing, as, requestedRV)
	if err != nil {
		return nil, err
	}
	audit.SetTarget(ref(updated))
	audit.SetMessage("updated ApplicationSet %s/%s", ns, updated.Name)
	s.event(ctx, updated, rest.ReasonResourceUpdated, "updated ApplicationSets spec")
	return withTypeMeta(updated), nil
}

// controllerFinalizers are finalizers an upsert never removes: the controller relies on them
// to delete generated Applications in order.
var controllerFinalizers = []string{v1alpha1.FinalizerApplicationSet}

// update applies the desired spec, merged labels and annotations and the requested
// finalizers to existing. With rv (the request's metadata.resourceVersion) the update is
// conditional and a conflict is returned as 409; without it, conflicts are retried on a
// fresh read (upstream: 10 attempts).
func (s *Service) update(ctx context.Context, existing, desired *v1alpha1.ApplicationSet, rv string) (*v1alpha1.ApplicationSet, error) {
	cur := existing.DeepCopy()
	for attempt := 0; attempt < s.d.Config.UpdateRetries; attempt++ {
		cur.Spec = *desired.Spec.DeepCopy()
		cur.Labels = mergeMaps(cur.Labels, desired.Labels)
		cur.Annotations = mergeMaps(cur.Annotations, desired.Annotations)
		fins := slices.Clone(desired.Finalizers)
		for _, f := range controllerFinalizers {
			if slices.Contains(cur.Finalizers, f) && !slices.Contains(fins, f) {
				fins = append(fins, f)
			}
		}
		cur.Finalizers = fins
		if rv != "" {
			cur.ResourceVersion = rv
		}
		err := s.d.Writer.Update(ctx, cur)
		if err == nil {
			return cur, nil
		}
		if !apierrors.IsConflict(err) {
			return nil, err
		}
		if rv != "" {
			return nil, rest.Conflict("the ApplicationSet %s/%s has been modified (resourceVersion %s is stale); read it again and retry", cur.Namespace, cur.Name, rv).WithCause(err)
		}
		fresh := &v1alpha1.ApplicationSet{}
		if err := s.d.APIReader.Get(ctx, client.ObjectKeyFromObject(cur), fresh); err != nil {
			return nil, fmt.Errorf("error getting ApplicationSets: %w", err)
		}
		cur = fresh
	}
	return nil, rest.Conflict("Failed to update ApplicationSets. Too many conflicts")
}

func mergeMaps(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return base
	}
	out := maps.Clone(base)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, over)
	return out
}

func stringMapsEqual(a, b map[string]string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return maps.Equal(a, b)
}

// sameDesiredState reports whether an existing ApplicationSet already has the requested
// spec, labels, annotations and finalizers (idempotent create).
func sameDesiredState(existing, want *v1alpha1.ApplicationSet) bool {
	return apiequality.Semantic.DeepEqual(existing.Spec, want.Spec) &&
		stringMapsEqual(existing.Labels, want.Labels) &&
		stringMapsEqual(existing.Annotations, want.Annotations) &&
		slices.Equal(existing.Finalizers, want.Finalizers)
}

// delete serves DELETE /api/v1/applicationsets/{name}.
func (s *Service) delete(ctx context.Context, req *apiclient.ApplicationSetDeleteRequest) (*apiclient.Empty, error) {
	cascade := req.Cascade == nil || *req.Cascade
	var policy metav1.DeletionPropagation
	switch pp := strings.ToLower(req.PropagationPolicy); {
	case !cascade && pp != "":
		return nil, rest.InvalidArgument("propagationPolicy cannot be combined with cascade=false")
	case !cascade:
		policy = metav1.DeletePropagationOrphan
	case pp == "" || pp == "background":
		policy = metav1.DeletePropagationBackground
	case pp == "foreground":
		policy = metav1.DeletePropagationForeground
	default:
		return nil, rest.InvalidArgument("invalid propagationPolicy %q: must be foreground or background", req.PropagationPolicy)
	}
	as, err := s.getExisting(ctx, rbac.ActionDelete, req.Name, req.AppsetNamespace)
	if err != nil {
		return nil, err
	}
	if s.d.Writer == nil {
		return nil, unavailable("store")
	}
	audit := rest.AuditFrom(ctx)
	audit.SetCheck(rbac.ResourceApplicationSets, rbac.ActionDelete, s.object(as))
	uid := as.UID
	if err := s.d.Writer.Delete(ctx, as, client.PropagationPolicy(policy), client.Preconditions{UID: &uid}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, rest.NotFound("ApplicationSet %s/%s was deleted concurrently", as.Namespace, as.Name).WithCause(err)
		}
		if apierrors.IsConflict(err) {
			return nil, rest.Conflict("ApplicationSet %s/%s was replaced concurrently", as.Namespace, as.Name).WithCause(err)
		}
		return nil, fmt.Errorf("error deleting ApplicationSets: %w", err)
	}
	audit.SetTarget(ref(as))
	audit.SetMessage("deleted ApplicationSet %s/%s (propagation %s)", as.Namespace, as.Name, policy)
	s.event(ctx, as, rest.ReasonResourceDeleted, "deleted ApplicationSets")
	return &apiclient.Empty{}, nil
}

// resourceTree serves GET /api/v1/applicationsets/{name}/resource-tree: the generated
// Applications of status.resources as nodes parented by the ApplicationSet.
func (s *Service) resourceTree(ctx context.Context, req *apiclient.ApplicationSetGetRequest) (*apiclient.ApplicationSetTree, error) {
	as, err := s.getExisting(ctx, rbac.ActionGet, req.Name, req.AppsetNamespace)
	if err != nil {
		return nil, err
	}
	return buildTree(as), nil
}

func buildTree(as *v1alpha1.ApplicationSet) *apiclient.ApplicationSetTree {
	parent := []apiclient.ApplicationSetTreeRef{{
		Group: v1alpha1.GroupVersion.Group, Version: v1alpha1.GroupVersion.Version, Kind: kindApplicationSet,
		Namespace: as.Namespace, Name: as.Name, UID: string(as.UID),
	}}
	tree := &apiclient.ApplicationSetTree{Nodes: make([]apiclient.ApplicationSetTreeNode, 0, len(as.Status.Resources))}
	for _, r := range as.Status.Resources {
		n := apiclient.ApplicationSetTreeNode{
			Group: r.Group, Version: r.Version, Kind: r.Kind, Name: r.Name, Namespace: as.Namespace,
			ParentRefs: slices.Clone(parent),
		}
		if n.Kind == "" {
			n.Group, n.Version, n.Kind = v1alpha1.GroupVersion.Group, v1alpha1.GroupVersion.Version, kindApplication
		}
		if r.Health != nil {
			h := *r.Health
			n.Health = &h
		}
		if r.Status != "" {
			n.Info = []apiclient.ApplicationSetTreeInfo{{Name: "Sync Status", Value: string(r.Status)}}
		}
		tree.Nodes = append(tree.Nodes, n)
	}
	slices.SortStableFunc(tree.Nodes, func(a, b apiclient.ApplicationSetTreeNode) int {
		return strings.Compare(a.FullName(), b.FullName())
	})
	return tree
}

// events serves GET /api/v1/applicationsets/{name}/events: the Events whose involved
// object is this ApplicationSet (same name, namespace and UID).
func (s *Service) events(ctx context.Context, req *apiclient.ApplicationSetGetRequest) (*corev1.EventList, error) {
	as, err := s.getExisting(ctx, rbac.ActionGet, req.Name, req.AppsetNamespace)
	if err != nil {
		return nil, err
	}
	var list corev1.EventList
	err = s.d.APIReader.List(ctx, &list, client.InNamespace(as.Namespace),
		client.MatchingFields{"involvedObject.name": as.Name, "involvedObject.namespace": as.Namespace, "involvedObject.uid": string(as.UID)},
		client.Limit(s.d.Config.MaxEvents))
	if err != nil {
		return nil, fmt.Errorf("error listing resource events: %w", err)
	}
	out := &corev1.EventList{Items: make([]corev1.Event, 0, len(list.Items))}
	out.APIVersion, out.Kind = "v1", "EventList"
	out.ResourceVersion = list.ResourceVersion
	for _, e := range list.Items {
		io := e.InvolvedObject
		if io.Name != as.Name || io.Namespace != as.Namespace || io.UID != as.UID {
			continue
		}
		out.Items = append(out.Items, e)
	}
	return out, nil
}
