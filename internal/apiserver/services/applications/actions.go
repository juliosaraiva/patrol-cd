package applications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	apirest "github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/lua"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	pubapi "github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// MaxActionImpactedResources caps the resources one action may patch or create.
const MaxActionImpactedResources = 100

// actionTarget is an authorized resource action request: the object the scripts see
// (a live object of the tree, or the Application itself) and where it lives.
type actionTarget struct {
	app     *v1alpha1.Application
	proj    *v1alpha1.AppProject
	st      *settings.Settings
	obj     *unstructured.Unstructured
	node    *compare.ResourceNode // nil for the Application itself
	cluster *clusterTarget
	action  string
}

func (t *actionTarget) self() bool { return t.node == nil }

// isAppSelf reports whether q targets the Application itself (group cd.patrol.example,
// kind Application, resourceName equal to the Application's name, namespace empty or the
// Application's).
func (s *Service) isAppSelf(q resourceQuery) bool {
	return q.group == v1alpha1.GroupName && q.kind == "Application" && q.resourceName == q.name &&
		(q.namespace == "" || q.namespace == s.nsOrDefault(q.appNamespace))
}

// resolveActionTarget authorizes action (applications, <action>) and loads the object of
// q: the Application itself (from the control plane; the cluster configuration is still
// resolved, so an enforced impersonation without a matching service account fails as
// upstream), or a live object of the tree read from the destination cluster with the
// Application's cluster configuration.
func (s *Service) resolveActionTarget(ctx context.Context, action string, q resourceQuery) (*actionTarget, error) {
	if s.isAppSelf(q) {
		st, err := s.settings()
		if err != nil {
			return nil, err
		}
		app, proj, err := s.getApp(ctx, action, q.appRequest())
		if err != nil {
			return nil, err
		}
		cc, err := s.appClusterConfig(ctx, app, proj, st, true)
		if err != nil {
			return nil, err
		}
		cp := app.DeepCopy()
		cp.APIVersion, cp.Kind = v1alpha1.GroupVersion.String(), "Application"
		cp.ManagedFields = nil
		obj, err := toUnstructured(cp)
		if err != nil {
			return nil, apirest.Internal(err)
		}
		return &actionTarget{app: app, proj: proj, st: st, obj: obj, cluster: cc, action: action}, nil
	}
	lt, err := s.resolveLiveTarget(ctx, action, q)
	if err != nil {
		return nil, err
	}
	n := lt.node
	obj, err := s.getLiveObject(ctx, lt.cluster.config, n.Group, lt.version, n.Kind, n.Namespace, n.Name)
	if err != nil {
		return nil, err
	}
	return &actionTarget{app: lt.app, proj: lt.proj, st: lt.st, obj: obj, node: n, cluster: lt.cluster, action: action}, nil
}

func (s *Service) actionRuntime(st *settings.Settings) (ActionRuntime, error) {
	rt, err := s.d.Resources.Lua.ActionRuntime(st)
	if err != nil {
		return nil, apirest.Unavailable("resource actions are not available").WithCause(err)
	}
	return rt, nil
}

// luaError maps a Lua discovery or action error: script errors are configuration errors
// reported with their (stack-free) message, as upstream.
func luaError(ctx context.Context, prefix string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, lua.ErrOverloaded) {
		return apirest.Unavailable("%s: %v", prefix, err).WithCause(err)
	}
	return apirest.InvalidArgument("%s: %v", prefix, err)
}

// listResourceActions serves GET /applications/{name}/resource/actions (applications,
// get): the actions offered by the configured and built-in discovery scripts for the live
// object (or the Application itself).
func (s *Service) listResourceActions(ctx context.Context, q *pubapi.ApplicationResourceRequest) (*pubapi.ResourceActionsListResponse, error) {
	t, err := s.resolveActionTarget(ctx, rbac.ActionGet, resourceQueryOf(q))
	if err != nil {
		return nil, err
	}
	rt, err := s.actionRuntime(t.st)
	if err != nil {
		return nil, err
	}
	actions, err := rt.ListActions(ctx, t.obj)
	if err != nil {
		return nil, luaError(ctx, "error executing Lua discovery script", err)
	}
	out := &pubapi.ResourceActionsListResponse{Actions: make([]pubapi.ResourceAction, 0, len(actions))}
	for _, a := range actions {
		out.Actions = append(out.Actions, publicAction(a))
	}
	return out, nil
}

func publicAction(a lua.Action) pubapi.ResourceAction {
	pa := pubapi.ResourceAction{Name: a.Name, Disabled: a.Disabled, IconClass: a.IconClass, DisplayName: a.DisplayName}
	for _, p := range a.Params {
		pa.Params = append(pa.Params, pubapi.ResourceActionParam{Name: p.Name})
	}
	return pa
}

// validateActionRequest checks the action name and the parameter list shape.
func validateActionRequest(req *pubapi.ResourceActionRunRequest) error {
	if strings.TrimSpace(req.Action) == "" {
		return apirest.InvalidArgument("action is required")
	}
	if len(req.Action) > 253 {
		return apirest.InvalidArgument("action name is too long")
	}
	if len(req.ResourceActionParameters) > MaxActionParameters {
		return apirest.InvalidArgument("too many action parameters (at most %d)", MaxActionParameters)
	}
	seen := map[string]bool{}
	for _, p := range req.ResourceActionParameters {
		if p.Name == "" {
			return apirest.InvalidArgument("action parameter names must not be empty")
		}
		if seen[p.Name] {
			return apirest.InvalidArgument("duplicate action parameter %q", p.Name)
		}
		seen[p.Name] = true
		if len(p.Value) > MaxActionParameterBytes {
			return apirest.InvalidArgument("action parameter %q exceeds %d bytes", p.Name, MaxActionParameterBytes)
		}
	}
	return nil
}

// checkActionParameters validates the parameters against the discovery of the action:
// a disabled action is refused; parameters must be declared by the action's discovery
// entry (an action that discovery does not list, run through the upstream fallback
// lookup, takes no parameters).
func checkActionParameters(name string, discovered []lua.Action, params []pubapi.ResourceActionParameter) error {
	var def *lua.Action
	for i := range discovered {
		if discovered[i].Name == name {
			def = &discovered[i]
			break
		}
	}
	if def != nil && def.Disabled {
		return apirest.FailedPrecondition("action %q is disabled for this resource", name)
	}
	declared := map[string]bool{}
	if def != nil {
		for _, p := range def.Params {
			declared[p.Name] = true
		}
	}
	for _, p := range params {
		if !declared[p.Name] {
			return apirest.InvalidArgument("unknown parameter %q for action %q", p.Name, name)
		}
	}
	return nil
}

// runResourceAction serves POST /applications/{name}/resource/actions (applications,
// action/<group>/<kind>/<action>): runs the action script and applies its result. Every
// impacted resource must be permitted by the project for the destination; create
// operations are dry-run first, so a refused creation changes nothing; then the results
// are written in order (merge patches, with the status subresource first when the patch
// touches status; creates). Writes are not transactional (upstream).
func (s *Service) runResourceAction(ctx context.Context, req *pubapi.ResourceActionRunRequest) (*pubapi.ApplicationResponse, error) {
	if err := validateActionRequest(req); err != nil {
		return nil, err
	}
	q := resourceQuery{name: req.Name, appNamespace: req.AppNamespace, project: req.Project,
		group: req.Group, version: req.Version, kind: req.Kind, namespace: req.Namespace, resourceName: req.ResourceName}
	action := rbac.CustomAction(q.group, q.kind, req.Action)
	t, err := s.resolveActionTarget(ctx, action, q)
	if err != nil {
		return nil, err
	}
	rt, err := s.actionRuntime(t.st)
	if err != nil {
		return nil, err
	}
	discovered, err := rt.ListActions(ctx, t.obj)
	if err != nil {
		return nil, luaError(ctx, "error executing Lua discovery script", err)
	}
	if err := checkActionParameters(req.Action, discovered, req.ResourceActionParameters); err != nil {
		return nil, err
	}
	liveBytes, err := json.Marshal(t.obj.Object)
	if err != nil {
		return nil, apirest.Internal(err)
	}
	params := make([]lua.ActionParameter, 0, len(req.ResourceActionParameters))
	for _, p := range req.ResourceActionParameters {
		params = append(params, lua.ActionParameter{Name: p.Name, Value: p.Value})
	}
	impacted, err := rt.ExecuteAction(ctx, t.obj.DeepCopy(), req.Action, params)
	if err != nil {
		if errors.Is(err, lua.ErrActionNotFound) {
			return nil, luaError(ctx, "error getting Lua resource action", err)
		}
		return nil, luaError(ctx, "error executing Lua resource action", err)
	}
	if len(impacted) > MaxActionImpactedResources {
		return nil, apirest.InvalidArgument("action %q returned %d resources (at most %d)", req.Action, len(impacted), MaxActionImpactedResources)
	}

	// Permissions and dry runs first: a refused resource aborts before any change.
	kl, _ := s.lookups(t.st)
	for _, ir := range impacted {
		obj := ir.Object
		if obj == nil {
			return nil, apirest.Internal(errors.New("action returned a nil resource"))
		}
		if err := s.verifyResourcePermitted(ctx, t, obj, kl); err != nil {
			return nil, err
		}
		if ir.Operation == lua.OperationPatch && t.self() {
			if err := checkSelfPatch(t.app, obj); err != nil {
				return nil, err
			}
		}
		if ir.Operation == lua.OperationCreate {
			if err := s.createActionResource(ctx, t.cluster.config, obj, true); err != nil {
				return nil, err
			}
		}
	}
	for _, ir := range impacted {
		switch ir.Operation {
		case lua.OperationPatch:
			if t.self() {
				err = s.patchApplicationSelf(ctx, t, liveBytes, ir.Object)
			} else {
				err = s.patchActionResource(ctx, t.cluster.config, liveBytes, ir.Object)
			}
		case lua.OperationCreate:
			err = s.createActionResource(ctx, t.cluster.config, ir.Object, false)
		default:
			err = apirest.InvalidArgument("unsupported operation: %s", ir.Operation)
		}
		if err != nil {
			return nil, err
		}
	}
	if t.self() {
		auditApp(ctx, t.app, t.action, s.rbacObject(t.app), apirest.ReasonResourceAction, "ran action %s", req.Action)
	} else {
		auditApp(ctx, t.app, t.action, s.rbacObject(t.app), apirest.ReasonResourceAction, "ran action %s on resource %s/%s/%s", req.Action, t.node.Group, t.node.Kind, t.node.Name)
	}
	return &pubapi.ApplicationResponse{}, nil
}

// verifyResourcePermitted checks an impacted resource against the project (upstream
// verifyResourcePermitted: kind, name and destination namespace on the destination
// cluster).
func (s *Service) verifyResourcePermitted(ctx context.Context, t *actionTarget, obj *unstructured.Unstructured, lister project.ProjectClusterLister) error {
	gvk := obj.GroupVersionKind()
	ok, err := project.IsResourcePermitted(ctx, t.proj, gvk.GroupKind(), obj.GetName(), obj.GetNamespace(), t.cluster.cluster, lister)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return apirest.Unavailable("error checking resource permissions").WithCause(err)
	}
	if !ok {
		return apirest.PermissionDenied("application is not permitted to manage %s/%s/%s in %s", gvk.Group, gvk.Kind, obj.GetName(), obj.GetNamespace())
	}
	return nil
}

// checkSelfPatch refuses action results that would move the Application to another
// project (that needs create in the new and update in the old project, which an action
// permission does not grant).
func checkSelfPatch(app *v1alpha1.Application, obj *unstructured.Unstructured) error {
	p, _, _ := unstructured.NestedString(obj.Object, "spec", "project")
	if p != app.Spec.Project {
		return apirest.PermissionDenied("a resource action cannot change the project of an application")
	}
	return nil
}

// splitStatusPatch splits a merge patch into its non-status part (nil when only status
// changes) and its status-only part (nil when status is unchanged).
func splitStatusPatch(patch []byte) (nonStatus, status []byte, err error) {
	var obj map[string]any
	if err := json.Unmarshal(patch, &obj); err != nil {
		return nil, nil, err
	}
	statusVal, ok := obj["status"]
	if !ok {
		return patch, nil, nil
	}
	if status, err = json.Marshal(map[string]any{"status": statusVal}); err != nil {
		return nil, nil, err
	}
	delete(obj, "status")
	if len(obj) > 0 {
		if nonStatus, err = json.Marshal(obj); err != nil {
			return nil, nil, err
		}
	}
	return nonStatus, status, nil
}

// actionMergePatch computes the merge patch from the live object to the action result
// ("{}" when nothing changed).
func actionMergePatch(liveBytes []byte, newObj *unstructured.Unstructured) ([]byte, error) {
	newBytes, err := json.Marshal(newObj.Object)
	if err != nil {
		return nil, err
	}
	patch, err := jsonpatch.CreateMergePatch(liveBytes, newBytes)
	if err != nil {
		return nil, fmt.Errorf("error calculating merge patch: %w", err)
	}
	return patch, nil
}

// patchActionResource applies an action's patch result to a live object on the
// destination cluster: when the patch changes status, the status subresource is patched
// first (NotFound: the kind has no status subresource, the whole patch is applied to the
// object instead), then the rest.
func (s *Service) patchActionResource(ctx context.Context, cfg *rest.Config, liveBytes []byte, newObj *unstructured.Unstructured) error {
	patch, err := actionMergePatch(liveBytes, newObj)
	if err != nil {
		return apirest.Internal(err)
	}
	if string(patch) == "{}" {
		return nil
	}
	nonStatus, statusPatch, err := splitStatusPatch(patch)
	if err != nil {
		return apirest.Internal(fmt.Errorf("error splitting status patch: %w", err))
	}
	cctx, cancel := s.clusterCtx(ctx)
	defer cancel()
	gvk := newObj.GroupVersionKind()
	gvr, namespaced, _, err := apiResource(cctx, cfg, gvk.Group, gvk.Version, gvk.Kind, "patch")
	if err != nil {
		return s.destinationError(ctx, "error patching resource", err)
	}
	ri, err := resourceClient(cfg, gvr, namespaced, newObj.GetNamespace())
	if err != nil {
		return s.destinationError(ctx, "error patching resource", err)
	}
	if statusPatch != nil {
		_, err := ri.Patch(cctx, newObj.GetName(), types.MergePatchType, patch, metav1.PatchOptions{}, "status")
		switch {
		case err == nil:
			patch = nonStatus
		case apierrors.IsNotFound(err):
			// No status subresource: the full patch below updates status too.
		default:
			return s.destinationError(ctx, "error patching resource", err)
		}
	}
	if patch == nil {
		return nil
	}
	if _, err := ri.Patch(cctx, newObj.GetName(), types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return s.destinationError(ctx, "error patching resource", err)
	}
	return nil
}

// patchApplicationSelf applies an action's patch of the Application itself. The
// Application lives in the control plane, so the patch is written there (with the
// resourceVersion the action saw, so a concurrent change is a conflict instead of being
// overwritten).
func (s *Service) patchApplicationSelf(ctx context.Context, t *actionTarget, liveBytes []byte, newObj *unstructured.Unstructured) error {
	c, err := s.kube()
	if err != nil {
		return err
	}
	patch, err := actionMergePatch(liveBytes, newObj)
	if err != nil {
		return apirest.Internal(err)
	}
	if string(patch) == "{}" {
		return nil
	}
	nonStatus, statusPatch, err := splitStatusPatch(patch)
	if err != nil {
		return apirest.Internal(fmt.Errorf("error splitting status patch: %w", err))
	}
	withRV := func(p []byte) ([]byte, error) {
		var m map[string]any
		if err := json.Unmarshal(p, &m); err != nil {
			return nil, err
		}
		md, _ := m["metadata"].(map[string]any)
		if md == nil {
			md = map[string]any{}
		}
		md["resourceVersion"] = t.app.ResourceVersion
		m["metadata"] = md
		return json.Marshal(m)
	}
	target := &v1alpha1.Application{}
	target.Namespace, target.Name = t.app.Namespace, t.app.Name
	if statusPatch != nil {
		sp, err := withRV(statusPatch)
		if err != nil {
			return apirest.Internal(err)
		}
		if err := c.Status().Patch(ctx, target, client.RawPatch(types.MergePatchType, sp)); err != nil {
			return fmt.Errorf("error patching resource: %w", err)
		}
		if nonStatus == nil {
			return nil
		}
		// The status write changed the resourceVersion.
		t.app.ResourceVersion = target.ResourceVersion
		patch = nonStatus
	}
	p, err := withRV(patch)
	if err != nil {
		return apirest.Internal(err)
	}
	if err := c.Patch(ctx, target, client.RawPatch(types.MergePatchType, p)); err != nil {
		return fmt.Errorf("error patching resource: %w", err)
	}
	return nil
}

// createActionResource creates (or, with dryRun, dry-run creates) an object returned by an
// action on the destination cluster.
func (s *Service) createActionResource(ctx context.Context, cfg *rest.Config, obj *unstructured.Unstructured, dryRun bool) error {
	prefix := "error creating resource"
	opts := metav1.CreateOptions{}
	if dryRun {
		prefix = "error creating resource (dry run)"
		opts.DryRun = []string{metav1.DryRunAll}
	}
	cctx, cancel := s.clusterCtx(ctx)
	defer cancel()
	gvk := obj.GroupVersionKind()
	gvr, namespaced, _, err := apiResource(cctx, cfg, gvk.Group, gvk.Version, gvk.Kind, "create")
	if err != nil {
		return s.destinationError(ctx, prefix, err)
	}
	if namespaced && obj.GetNamespace() == "" {
		return apirest.InvalidArgument("%s: %s %s has no namespace", prefix, gvk.Kind, obj.GetName())
	}
	ri, err := resourceClient(cfg, gvr, namespaced, obj.GetNamespace())
	if err != nil {
		return s.destinationError(ctx, prefix, err)
	}
	if _, err := ri.Create(cctx, obj.DeepCopy(), opts); err != nil {
		return s.destinationError(ctx, prefix, err)
	}
	return nil
}
