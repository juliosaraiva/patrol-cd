package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	gosync "sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/jsonmergepatch"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/csaupgrade"
	"k8s.io/client-go/util/retry"
)

// maxPatchRetries bounds client-side apply retries on conflicts, like kubectl.
const maxPatchRetries = 5

var (
	gvrCRD       = schema.GroupVersionResource{Group: groupCRD, Version: "v1", Resource: "customresourcedefinitions"}
	gvrNamespace = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
)

// KubeClientOptions configure a KubeClient.
type KubeClientOptions struct {
	// Manager is the field manager of every write. Default DefaultManager.
	Manager string
	// DeleteWaitTimeout bounds the wait for an object to disappear before it is re-created
	// (force apply, force replace, RoleBinding roleRef changes). Default 60s.
	DeleteWaitTimeout time.Duration
	// Logger receives debug logs. Default discards.
	Logger *slog.Logger
}

// KubeClient implements Kube with a dynamic client and API discovery. API resources are
// cached per group version; a missing kind triggers one fresh discovery request, and
// WaitForCRDEstablished refreshes the CRD's group, so CRDs created during a sync are found.
type KubeClient struct {
	dyn        dynamic.Interface
	disco      discovery.DiscoveryInterface
	manager    string
	deleteWait time.Duration
	log        *slog.Logger

	mu      gosync.Mutex
	gvCache map[string]*metav1.APIResourceList
}

var _ Kube = (*KubeClient)(nil)

// NewKubeClient builds a KubeClient from a REST config.
func NewKubeClient(cfg *rest.Config, opts KubeClientOptions) (*KubeClient, error) {
	if cfg == nil {
		return nil, fmt.Errorf("%w: nil rest config", ErrInvalidInput)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create discovery client: %w", err)
	}
	return NewKubeClientFor(dyn, disco, opts), nil
}

// NewKubeClientFor builds a KubeClient from existing clients.
func NewKubeClientFor(dyn dynamic.Interface, disco discovery.DiscoveryInterface, opts KubeClientOptions) *KubeClient {
	if opts.Manager == "" {
		opts.Manager = DefaultManager
	}
	if opts.DeleteWaitTimeout <= 0 {
		opts.DeleteWaitTimeout = 60 * time.Second
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &KubeClient{
		dyn:        dyn,
		disco:      disco,
		manager:    opts.Manager,
		deleteWait: opts.DeleteWaitTimeout,
		log:        log,
		gvCache:    map[string]*metav1.APIResourceList{},
	}
}

// Resource implements Kube.
func (k *KubeClient) Resource(ctx context.Context, gvk schema.GroupVersionKind) (*metav1.APIResource, error) {
	if gvk.Version == "" {
		return k.resourceAnyVersion(ctx, gvk)
	}
	gv := gvk.GroupVersion().String()
	list, cached, err := k.resourceList(gv, false)
	if err != nil {
		return nil, fmt.Errorf("failed to discover server resources for group version %s: %w", gv, err)
	}
	if r := findKind(list, gvk); r != nil {
		return r, nil
	}
	if cached {
		if list, _, err = k.resourceList(gv, true); err != nil {
			return nil, fmt.Errorf("failed to discover server resources for group version %s: %w", gv, err)
		}
		if r := findKind(list, gvk); r != nil {
			return r, nil
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: gvk.Kind}, "")
}

func (k *KubeClient) resourceList(gv string, refresh bool) (*metav1.APIResourceList, bool, error) {
	if !refresh {
		k.mu.Lock()
		list, ok := k.gvCache[gv]
		k.mu.Unlock()
		if ok {
			return list, true, nil
		}
	}
	list, err := k.disco.ServerResourcesForGroupVersion(gv)
	if err != nil {
		k.mu.Lock()
		delete(k.gvCache, gv)
		k.mu.Unlock()
		return nil, false, err
	}
	k.mu.Lock()
	k.gvCache[gv] = list
	k.mu.Unlock()
	return list, false, nil
}

func (k *KubeClient) invalidateGroup(group string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for gv := range k.gvCache {
		g := ""
		if i := strings.Index(gv, "/"); i >= 0 {
			g = gv[:i]
		}
		if g == group {
			delete(k.gvCache, gv)
		}
	}
}

func findKind(list *metav1.APIResourceList, gvk schema.GroupVersionKind) *metav1.APIResource {
	if list == nil {
		return nil
	}
	for i := range list.APIResources {
		r := list.APIResources[i]
		if r.Kind == gvk.Kind && !strings.Contains(r.Name, "/") {
			if r.Group == "" {
				r.Group = gvk.Group
			}
			if r.Version == "" {
				r.Version = gvk.Version
			}
			return &r
		}
	}
	return nil
}

func (k *KubeClient) resourceAnyVersion(ctx context.Context, gvk schema.GroupVersionKind) (*metav1.APIResource, error) {
	groups, err := k.disco.ServerGroups()
	if err != nil {
		return nil, fmt.Errorf("failed to discover server groups: %w", err)
	}
	for _, g := range groups.Groups {
		if g.Name != gvk.Group {
			continue
		}
		versions := []string{g.PreferredVersion.Version}
		for _, v := range g.Versions {
			if v.Version != g.PreferredVersion.Version {
				versions = append(versions, v.Version)
			}
		}
		for _, v := range versions {
			if v == "" {
				continue
			}
			if r, err := k.Resource(ctx, schema.GroupVersionKind{Group: gvk.Group, Version: v, Kind: gvk.Kind}); err == nil {
				return r, nil
			}
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: gvk.Kind}, "")
}

// client returns the resource interface for gvk in namespace and the API resource.
func (k *KubeClient) client(ctx context.Context, gvk schema.GroupVersionKind, namespace string) (dynamic.ResourceInterface, *metav1.APIResource, error) {
	res, err := k.Resource(ctx, gvk)
	if err != nil {
		return nil, nil, err
	}
	gvr := gvk.GroupVersion().WithResource(res.Name)
	if res.Namespaced {
		if namespace == "" {
			return nil, nil, fmt.Errorf("namespace is required for %s", gvk.GroupKind())
		}
		return k.dyn.Resource(gvr).Namespace(namespace), res, nil
	}
	return k.dyn.Resource(gvr), res, nil
}

// prepare returns a copy of obj fit for writing: no namespace on cluster-scoped objects and
// no managed fields.
func prepare(obj *unstructured.Unstructured, res *metav1.APIResource) *unstructured.Unstructured {
	c := obj.DeepCopy()
	if !res.Namespaced {
		c.SetNamespace("")
	}
	c.SetManagedFields(nil)
	return c
}

func fieldValidation(validate bool) string {
	if validate {
		return metav1.FieldValidationStrict
	}
	return metav1.FieldValidationIgnore
}

func dryRunOpt(dryRun bool) []string {
	if dryRun {
		return []string{metav1.DryRunAll}
	}
	return nil
}

// Get implements Kube.
func (k *KubeClient) Get(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, error) {
	ri, _, err := k.client(ctx, gvk, namespace)
	if err != nil {
		return nil, err
	}
	return ri.Get(ctx, name, metav1.GetOptions{})
}

// ClientSideApply implements Kube.
func (k *KubeClient) ClientSideApply(ctx context.Context, obj *unstructured.Unstructured, opts ApplyOptions) (string, error) {
	ri, res, err := k.client(ctx, obj.GroupVersionKind(), obj.GetNamespace())
	if err != nil {
		return "", err
	}
	target := prepare(obj, res)
	name := target.GetName()

	if target.GetAPIVersion() == "rbac.authorization.k8s.io/v1" {
		recreate, err := k.reconcileRBAC(ctx, ri, target, opts.DryRun)
		if err != nil {
			return "", fmt.Errorf("error running kubectl auth reconcile: %w", err)
		}
		if recreate && opts.DryRun {
			// roleRef is immutable: a real run deletes and re-creates the binding, which a
			// dry run cannot simulate.
			return opMessage(target, "configured", true), nil
		}
	}

	modified, err := modifiedConfiguration(target)
	if err != nil {
		return "", err
	}
	for attempt := 0; ; attempt++ {
		live, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			createObj := &unstructured.Unstructured{}
			if err := createObj.UnmarshalJSON(modified); err != nil {
				return "", fmt.Errorf("decode configuration: %w", err)
			}
			_, err = ri.Create(ctx, createObj, metav1.CreateOptions{
				DryRun: dryRunOpt(opts.DryRun), FieldManager: k.manager, FieldValidation: fieldValidation(opts.Validate),
			})
			if apierrors.IsAlreadyExists(err) && attempt < maxPatchRetries {
				continue
			}
			if err != nil {
				return "", err
			}
			return opMessage(target, "created", opts.DryRun), nil
		}
		if err != nil {
			return "", err
		}
		patch, patchType, err := threeWayPatch(target.GroupVersionKind(), live, modified)
		if err != nil {
			return "", err
		}
		if string(patch) == "{}" {
			return opMessage(target, "unchanged", opts.DryRun), nil
		}
		_, err = ri.Patch(ctx, name, patchType, patch, metav1.PatchOptions{
			DryRun: dryRunOpt(opts.DryRun), FieldManager: k.manager, FieldValidation: fieldValidation(opts.Validate),
		})
		if err == nil {
			return opMessage(target, "configured", opts.DryRun), nil
		}
		if apierrors.IsConflict(err) && attempt < maxPatchRetries {
			continue
		}
		if (apierrors.IsConflict(err) || apierrors.IsInvalid(err)) && opts.Force {
			if opts.DryRun {
				// kubectl cannot simulate delete and re-create either.
				return opMessage(target, "configured", true), nil
			}
			if err := k.deleteAndCreate(ctx, ri, live, modified, opts.Validate); err != nil {
				return "", err
			}
			return opMessage(target, "configured", false), nil
		}
		return "", err
	}
}

// modifiedConfiguration returns obj's JSON with the last-applied-configuration annotation
// set to obj's JSON without that annotation, like kubectl.
func modifiedConfiguration(obj *unstructured.Unstructured) ([]byte, error) {
	c := obj.DeepCopy()
	ann := c.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	delete(ann, corev1.LastAppliedConfigAnnotation)
	c.SetAnnotations(ann)
	without, err := json.Marshal(c.Object)
	if err != nil {
		return nil, fmt.Errorf("encode configuration: %w", err)
	}
	ann[corev1.LastAppliedConfigAnnotation] = string(without) + "\n"
	c.SetAnnotations(ann)
	out, err := json.Marshal(c.Object)
	if err != nil {
		return nil, fmt.Errorf("encode configuration: %w", err)
	}
	return out, nil
}

// threeWayPatch computes kubectl's apply patch: a strategic merge patch for kinds known to
// the client-go scheme, a JSON merge patch otherwise (CRDs and custom resources).
func threeWayPatch(gvk schema.GroupVersionKind, live *unstructured.Unstructured, modified []byte) ([]byte, types.PatchType, error) {
	current, err := json.Marshal(live.Object)
	if err != nil {
		return nil, "", fmt.Errorf("encode live object: %w", err)
	}
	var original []byte
	if v := live.GetAnnotations()[corev1.LastAppliedConfigAnnotation]; v != "" {
		original = []byte(v)
	}
	versioned, err := scheme.Scheme.New(gvk)
	switch {
	case err == nil:
		meta, err := strategicpatch.NewPatchMetaFromStruct(versioned)
		if err != nil {
			return nil, "", fmt.Errorf("patch metadata for %s: %w", gvk, err)
		}
		patch, err := strategicpatch.CreateThreeWayMergePatch(original, modified, current, meta, true)
		if err != nil {
			return nil, "", fmt.Errorf("creating patch: %w", err)
		}
		return patch, types.StrategicMergePatchType, nil
	case runtime.IsNotRegisteredError(err):
		patch, err := jsonmergepatch.CreateThreeWayJSONMergePatch(original, modified, current,
			mergepatch.RequireKeyUnchanged("apiVersion"),
			mergepatch.RequireKeyUnchanged("kind"),
			mergepatch.RequireMetadataKeyUnchanged("name"))
		if err != nil {
			return nil, "", fmt.Errorf("creating patch: %w", err)
		}
		return patch, types.MergePatchType, nil
	default:
		return nil, "", fmt.Errorf("getting instance of versioned object for %v: %w", gvk, err)
	}
}

// deleteAndCreate replaces live by the modified configuration (kubectl apply --force). If
// the creation fails the original object is restored.
func (k *KubeClient) deleteAndCreate(ctx context.Context, ri dynamic.ResourceInterface, live *unstructured.Unstructured, modified []byte, validate bool) error {
	if err := k.deleteAndWait(ctx, ri, live.GetName()); err != nil {
		return err
	}
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(modified); err != nil {
		return fmt.Errorf("decode configuration: %w", err)
	}
	_, err := ri.Create(ctx, obj, metav1.CreateOptions{FieldManager: k.manager, FieldValidation: fieldValidation(validate)})
	if err == nil {
		return nil
	}
	if _, rerr := ri.Create(ctx, restorable(live), metav1.CreateOptions{FieldManager: k.manager}); rerr != nil {
		return fmt.Errorf("an error occurred force-replacing the existing object with the newly provided one:\n\n%v.\n\nAdditionally, an error occurred attempting to restore the original object:\n\n%v", err, rerr)
	}
	return err
}

// restorable strips server-populated metadata so a deleted object can be created again.
func restorable(live *unstructured.Unstructured) *unstructured.Unstructured {
	c := live.DeepCopy()
	c.SetResourceVersion("")
	c.SetUID("")
	c.SetCreationTimestamp(metav1.Time{})
	c.SetDeletionTimestamp(nil)
	c.SetGeneration(0)
	c.SetManagedFields(nil)
	unstructured.RemoveNestedField(c.Object, "status")
	return c
}

// deleteAndWait deletes name in background mode and waits until it is gone.
func (k *KubeClient) deleteAndWait(ctx context.Context, ri dynamic.ResourceInterface, name string) error {
	policy := metav1.DeletePropagationBackground
	if err := ri.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, k.deleteWait, true, func(ctx context.Context) (bool, error) {
		_, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

// reconcileRBAC is the part of `kubectl auth reconcile` the sync relies on (SYNC-074): the
// namespace of a namespaced RBAC object must exist (auth reconcile would create it), and a
// binding whose immutable roleRef changed is deleted so the apply re-creates it. It reports
// whether the binding needs re-creation.
func (k *KubeClient) reconcileRBAC(ctx context.Context, ri dynamic.ResourceInterface, obj *unstructured.Unstructured, dryRun bool) (bool, error) {
	kind := obj.GetKind()
	clusterScoped := kind == "ClusterRole" || kind == "ClusterRoleBinding"
	if !dryRun && obj.GetNamespace() != "" && !clusterScoped {
		if _, err := k.dyn.Resource(gvrNamespace).Get(ctx, obj.GetNamespace(), metav1.GetOptions{}); err != nil {
			return false, fmt.Errorf("error getting namespace %s: %w", obj.GetNamespace(), err)
		}
	}
	if kind != "RoleBinding" && kind != "ClusterRoleBinding" {
		return false, nil
	}
	live, err := ri.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	liveRef, _, _ := unstructured.NestedMap(live.Object, "roleRef")
	targetRef, _, _ := unstructured.NestedMap(obj.Object, "roleRef")
	if reflect.DeepEqual(liveRef, targetRef) {
		return false, nil
	}
	if dryRun {
		return true, nil
	}
	k.log.Debug("roleRef changed, re-creating binding", "kind", kind, "namespace", obj.GetNamespace(), "name", obj.GetName())
	return true, k.deleteAndWait(ctx, ri, obj.GetName())
}

// ServerSideApply implements Kube.
func (k *KubeClient) ServerSideApply(ctx context.Context, obj *unstructured.Unstructured, opts ApplyOptions) (string, error) {
	ri, res, err := k.client(ctx, obj.GroupVersionKind(), obj.GetNamespace())
	if err != nil {
		return "", err
	}
	target := prepare(obj, res)
	data, err := json.Marshal(target.Object)
	if err != nil {
		return "", fmt.Errorf("encode configuration: %w", err)
	}
	force := true
	_, err = ri.Patch(ctx, target.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{
		DryRun: dryRunOpt(opts.DryRun), FieldManager: k.manager, Force: &force, FieldValidation: fieldValidation(opts.Validate),
	})
	if err != nil {
		return "", err
	}
	return opMessage(target, "serverside-applied", opts.DryRun), nil
}

// MigrateClientSideApply implements Kube with k8s.io/client-go/util/csaupgrade, retrying
// on conflicts with a fresh object.
func (k *KubeClient) MigrateClientSideApply(ctx context.Context, live *unstructured.Unstructured, csaManager string) error {
	ri, _, err := k.client(ctx, live.GroupVersionKind(), live.GetNamespace())
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh, err := ri.Get(ctx, live.GetName(), metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get fresh object for CSA migration: %w", err)
		}
		if !needsClientSideApplyMigration(fresh, csaManager) {
			return nil
		}
		patch, err := csaupgrade.UpgradeManagedFieldsPatch(fresh, sets.New(csaManager), k.manager)
		if err != nil {
			return fmt.Errorf("failed to generate csaupgrade migration patch: %w", err)
		}
		if patch == nil {
			return nil
		}
		_, err = ri.Patch(ctx, live.GetName(), types.JSONPatchType, patch, metav1.PatchOptions{})
		return err
	})
}

// Replace implements Kube (kubectl replace [--force]).
func (k *KubeClient) Replace(ctx context.Context, obj *unstructured.Unstructured, opts ReplaceOptions) (string, error) {
	ri, res, err := k.client(ctx, obj.GroupVersionKind(), obj.GetNamespace())
	if err != nil {
		return "", err
	}
	target := prepare(obj, res)
	if opts.Force && opts.DryRun {
		// Delete and re-create cannot be simulated (kubectl replace --force --dry-run does
		// not contact the server either).
		return opMessage(target, "replaced", true), nil
	}
	if opts.Force {
		if err := k.deleteAndWait(ctx, ri, target.GetName()); err != nil {
			return "", err
		}
		target.SetResourceVersion("")
		if _, err := ri.Create(ctx, target, metav1.CreateOptions{FieldManager: k.manager}); err != nil {
			return "", err
		}
		return opMessage(target, "replaced", false), nil
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live, err := ri.Get(ctx, target.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		target.SetResourceVersion(live.GetResourceVersion())
		_, err = ri.Update(ctx, target, metav1.UpdateOptions{DryRun: dryRunOpt(opts.DryRun), FieldManager: k.manager})
		return err
	})
	if err != nil {
		return "", err
	}
	return opMessage(target, "replaced", opts.DryRun), nil
}

// Create implements Kube.
func (k *KubeClient) Create(ctx context.Context, obj *unstructured.Unstructured, opts CreateOptions) (string, error) {
	ri, res, err := k.client(ctx, obj.GroupVersionKind(), obj.GetNamespace())
	if err != nil {
		return "", err
	}
	target := prepare(obj, res)
	if _, err := ri.Create(ctx, target, metav1.CreateOptions{
		DryRun: dryRunOpt(opts.DryRun), FieldManager: k.manager, FieldValidation: fieldValidation(opts.Validate),
	}); err != nil {
		return "", err
	}
	return opMessage(target, "created", opts.DryRun), nil
}

// Update implements Kube. Conflicts are returned to the caller.
func (k *KubeClient) Update(ctx context.Context, obj *unstructured.Unstructured, opts UpdateOptions) (*unstructured.Unstructured, error) {
	ri, res, err := k.client(ctx, obj.GroupVersionKind(), obj.GetNamespace())
	if err != nil {
		return nil, err
	}
	return ri.Update(ctx, prepare(obj, res), metav1.UpdateOptions{DryRun: dryRunOpt(opts.DryRun), FieldManager: k.manager})
}

// Delete implements Kube.
func (k *KubeClient) Delete(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string, opts DeleteOptions) error {
	ri, _, err := k.client(ctx, gvk, namespace)
	if err != nil {
		return err
	}
	del := metav1.DeleteOptions{DryRun: dryRunOpt(opts.DryRun)}
	if opts.PropagationPolicy != "" {
		p := opts.PropagationPolicy
		del.PropagationPolicy = &p
	}
	return ri.Delete(ctx, name, del)
}

// WaitForCRDEstablished implements Kube. Beyond the Established condition it waits (within
// the same timeout) until discovery serves the CRD's kind, because the API server publishes
// discovery asynchronously and a custom resource applied right after the CRD would
// otherwise fail with "the server could not find the requested resource".
func (k *KubeClient) WaitForCRDEstablished(ctx context.Context, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var crd *unstructured.Unstructured
	err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		obj, err := k.dyn.Resource(gvrCRD).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		crd = obj
		conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		for _, c := range conds {
			m, ok := c.(map[string]any)
			if ok && m["type"] == "Established" {
				return m["status"] == "True", nil
			}
		}
		return false, nil
	})
	if crd == nil {
		return fmt.Errorf("failed to ensure CRD ready: %w", err)
	}
	group, _, _ := unstructured.NestedString(crd.Object, "spec", "group")
	k.invalidateGroup(group)
	if err != nil {
		return fmt.Errorf("failed to ensure CRD ready: %w", err)
	}
	kind, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "kind")
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for _, v := range versions {
		m, ok := v.(map[string]any)
		if !ok || m["served"] != true {
			continue
		}
		version, _ := m["name"].(string)
		gvk := schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
		err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(context.Context) (bool, error) {
			list, _, err := k.resourceList(gvk.GroupVersion().String(), true)
			if err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
			return findKind(list, gvk) != nil, nil
		})
		if err != nil {
			return fmt.Errorf("failed to ensure CRD ready: %s not served: %w", gvk, err)
		}
	}
	return nil
}
