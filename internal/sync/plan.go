package sync

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Action is what a planned task does.
type Action string

const (
	// ActionCreate creates a missing resource (apply or create).
	ActionCreate Action = "create"
	// ActionUpdate applies changes to an existing resource.
	ActionUpdate Action = "update"
	// ActionUnchanged: the diff reports no change; apply is a no-op.
	ActionUnchanged Action = "unchanged"
	// ActionReplace replaces an existing resource (Replace=true).
	ActionReplace Action = "replace"
	// ActionSkip: not applied because ApplyOutOfSyncOnly found no change.
	ActionSkip Action = "skip"
	// ActionPrune deletes a resource that is no longer desired.
	ActionPrune Action = "prune"
	// ActionPruneSkipped: the resource requires pruning but pruning is disabled for the
	// operation ("ignored (requires pruning)") or the resource ("ignored (no prune)").
	ActionPruneSkipped Action = "prune-skipped"
	// ActionRunHook creates and runs a hook.
	ActionRunHook Action = "run-hook"
)

// PlannedTask is one task of a Plan, in execution order.
type PlannedTask struct {
	// Key identifies the resource (namespace defaulted for namespaced kinds).
	Key     ResourceKey
	Version string
	// Phase and Wave the task runs in (after dependency and prune-order adjustments).
	Phase v1alpha1.SyncPhase
	Wave  int
	// Hook is true for hooks; HookType is the phase's hook type and DeletePolicies the
	// hook's delete policies.
	Hook           bool
	HookType       v1alpha1.HookType
	DeletePolicies []v1alpha1.HookDeletePolicy
	// Action is what the task does.
	Action Action
	// Message explains a skip ("ignored (requires pruning)", ...).
	Message string
	// SkipDryRun is true when the dry run is skipped: the kind is not served yet, or its
	// CRD is created or changed by this sync.
	SkipDryRun bool
	// RequiresConfirmation is true for a prune task waiting for Prune=confirm approval.
	RequiresConfirmation bool
	// Status, HookPhase and ResultMessage are the persisted result of the task, if any.
	Status        v1alpha1.ResultCode
	HookPhase     v1alpha1.OperationPhase
	ResultMessage string
	// Target is the object that will be applied (tracking, namespace and hook name
	// applied); Live is the live object. Both are copies the caller may keep.
	Target *unstructured.Unstructured
	Live   *unstructured.Unstructured
}

// Plan is the planned execution of an operation.
type Plan struct {
	// Tasks in execution order; SyncFail hooks (run only on failure) are included.
	Tasks []PlannedTask
	// Valid is false when a task failed validation (missing kind, project permission);
	// Errors holds the sorted distinct messages and Step would fail with "one or more
	// synchronization tasks are not valid".
	Valid  bool
	Errors []string
	// SharedResource is set when FailOnSharedResource=true and a target is part of another
	// Application; Step would fail with "Shared resource found: <SharedResource>".
	SharedResource string
	// Warnings describe destructive effects of the operation, for confirmation prompts and
	// status.plan.warnings.
	Warnings []string
	// PendingConfirmation lists prune tasks blocked until the Application's
	// cd.patrol.example/deletion-approved annotation is at or after the operation start.
	PendingConfirmation []ResourceKey
	// SkipHooks is true for the apply strategy and selective syncs.
	SkipHooks bool
	// MultiStep is true when the tasks span more than one (phase, wave).
	MultiStep bool
	// Digest is the sha256 of the sorted canonical target manifests (RenderedInputDigest).
	Digest string
	// TotalTasks counts tasks except SyncFail hooks.
	TotalTasks int
}

// Preview returns the tasks that would change the cluster (everything except unchanged
// and skipped resources), for `sync --dry-run` and `sync --preview-changes` output.
func (p *Plan) Preview() []PlannedTask {
	var out []PlannedTask
	for _, t := range p.Tasks {
		if t.Phase == v1alpha1.SyncPhaseSyncFail || t.Action == ActionUnchanged || t.Action == ActionSkip {
			continue
		}
		out = append(out, t)
	}
	return out
}

// Plan computes the task plan of an operation without changing the cluster. It performs
// read-only calls only: API discovery, the GET of the destination namespace when
// CreateNamespace=true, and the Permission/IsNamespaced/SetTracking callbacks on copies.
// Results persisted in in.Previous are reflected in each task's status.
func (e *Engine) Plan(ctx context.Context, in *Input) (*Plan, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	r := e.newRun(ctx, in)
	tasks, out, err := r.buildTasks()
	if err != nil {
		return nil, err
	}
	if out != nil {
		return nil, fmt.Errorf("plan: %s", out.message)
	}
	p := &Plan{
		Valid:          r.valid,
		SharedResource: r.sharedMessage,
		SkipHooks:      in.skipHooks(),
		Digest:         Digest(in.Targets),
	}
	if !r.valid {
		p.Errors = r.results.failedMessages()
	}
	nonSyncFail := tasks.filter(func(t *task) bool { return t.phase != v1alpha1.SyncPhaseSyncFail })
	p.MultiStep = nonSyncFail.multiStep()
	p.TotalTasks = len(nonSyncFail)
	for _, t := range tasks {
		pt := PlannedTask{
			Key:           t.key(),
			Version:       t.version(),
			Phase:         t.phase,
			Wave:          t.wave(),
			Hook:          t.hook,
			HookType:      t.hookType(),
			SkipDryRun:    t.skipDryRun,
			Status:        t.status,
			HookPhase:     t.opState,
			ResultMessage: t.message,
		}
		if t.hook {
			pt.DeletePolicies = DeletePolicies(t.obj())
		}
		if t.target != nil {
			pt.Target = t.target.DeepCopy()
		}
		if t.live != nil {
			pt.Live = t.live.DeepCopy()
		}
		pt.Action, pt.Message = r.action(t)
		// Like upstream, confirmation is requested even when the sync does not prune.
		if t.isPrune() && t.pending() && !r.pruneConfirmed && r.opts.RequiresPruneConfirmation(t.live) {
			pt.RequiresConfirmation = true
			p.PendingConfirmation = append(p.PendingConfirmation, pt.Key)
		}
		p.Tasks = append(p.Tasks, pt)
	}
	p.Warnings = r.warnings(tasks)
	return p, nil
}

func (r *run) action(t *task) (Action, string) {
	switch {
	case t.isPrune():
		if !r.in.Prune {
			return ActionPruneSkipped, "ignored (requires pruning)"
		}
		if r.opts.PruneDisabled(t.live) {
			return ActionPruneSkipped, "ignored (no prune)"
		}
		return ActionPrune, ""
	case t.hook:
		return ActionRunHook, ""
	case t.outOfSyncOnlySkip:
		return ActionSkip, ""
	case t.live == nil:
		return ActionCreate, ""
	case r.opts.ReplaceFor(t.target, t.live):
		return ActionReplace, ""
	}
	if modified, ok := r.in.DiffResults[t.resourceKey()]; ok && !modified {
		return ActionUnchanged, ""
	}
	return ActionUpdate, ""
}

// warnings lists destructive effects in a stable order.
func (r *run) warnings(tasks syncTasks) []string {
	var w []string
	prunes := tasks.filter(func(t *task) bool {
		return t.isPrune() && r.in.Prune && !r.opts.PruneDisabled(t.live)
	})
	if n := len(prunes); n > 0 {
		applies := tasks.filter(func(t *task) bool { return !t.isPrune() && !t.hook })
		if len(applies) == 0 {
			w = append(w, fmt.Sprintf("all %d resources of the application will be pruned", n))
		} else {
			w = append(w, fmt.Sprintf("%d resources will be pruned", n))
		}
	}
	if r.in.strategyForce() {
		w = append(w, "force is enabled: resources that cannot be patched or replaced are deleted and re-created")
	} else if tasks.any(func(t *task) bool { return t.target != nil && ForceFor(false, t.target, t.live) }) {
		w = append(w, "some resources use Force=true: they may be deleted and re-created")
	}
	if r.opts.Replace {
		w = append(w, "Replace=true: existing resources are replaced instead of patched")
	} else if tasks.any(func(t *task) bool { return t.target != nil && t.live != nil && r.opts.ReplaceFor(t.target, t.live) }) {
		w = append(w, "some resources use Replace=true: they are replaced instead of patched")
	}
	if r.opts.PrunePropagationPolicy == metav1.DeletePropagationOrphan && len(prunes) > 0 {
		w = append(w, "PrunePropagationPolicy=orphan: dependents of pruned resources are left in the cluster")
	}
	return w
}

// Digest returns the hex sha256 over the canonical JSON of the targets, sorted, for
// status.plan.renderedInputDigest.
func Digest(targets []*unstructured.Unstructured) string {
	docs := make([]string, 0, len(targets))
	for _, t := range targets {
		if t == nil {
			continue
		}
		b, err := json.Marshal(t.Object) // map keys are sorted by encoding/json
		if err != nil {
			continue
		}
		docs = append(docs, string(b))
	}
	sort.Strings(docs)
	h := sha256.New()
	for _, d := range docs {
		h.Write([]byte(d))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// reconciled pairs a target with its live object; target is nil for prune candidates.
type reconciled struct {
	key    ResourceKey
	target *unstructured.Unstructured
	live   *unstructured.Unstructured
}

// outcome is a terminal result decided before execution.
type outcome struct {
	phase   v1alpha1.OperationPhase
	message string
}

// buildTasks computes the tasks of this call, mirroring upstream getSyncTasks plus the
// controller-side preparation (tracking, RespectIgnoreDifferences, shared resources).
// It returns an error only for transient API failures; terminal problems are returned as
// an outcome.
func (r *run) buildTasks() (syncTasks, *outcome, error) {
	in := r.in
	syncObjs, _, _ := PartitionTargets(in.Targets)

	var resTargets, hooks []*unstructured.Unstructured
	for _, obj := range syncObjs {
		if IsIgnored(obj) {
			continue
		}
		c := obj.DeepCopy()
		if in.SetTracking != nil {
			if err := in.SetTracking(c); err != nil {
				return nil, &outcome{v1alpha1.OperationError, fmt.Sprintf("failed to set tracking metadata on %s: %v", keyOf(obj), err)}, nil
			}
		}
		if IsHook(c) {
			hooks = append(hooks, c)
		} else {
			resTargets = append(resTargets, c)
		}
	}
	for _, obj := range slices.Concat(resTargets, hooks) {
		if gk, ok := crdGroupKind(obj); ok {
			r.crdKinds[gk] = true
			if ns, ok := crdScopeNamespaced(obj); ok {
				r.crdScopes[gk] = ns
			}
		}
	}

	recs := r.reconcile(resTargets)

	if r.opts.RespectIgnoreDifferences && in.Normalizer != nil && !r.terminating {
		if err := r.normalizeTargets(recs); err != nil {
			return nil, &outcome{v1alpha1.OperationError, fmt.Sprintf("Failed to normalize target resources: %v", err)}, nil
		}
	}

	if r.opts.FailOnSharedResource && in.SharedWith != nil {
		for _, rec := range recs {
			if rec.target == nil || rec.live == nil {
				continue
			}
			if owner, shared := in.SharedWith(rec.live); shared {
				r.sharedMessage = fmt.Sprintf("%s/%s is part of applications %s and %s",
					rec.live.GetKind(), rec.live.GetName(), in.App.QualifiedName(), owner)
				break
			}
		}
	}

	var tasks syncTasks
	for _, rec := range recs {
		if !matchesResources(rec.key, in.Resources) {
			continue
		}
		if in.IsSelfReferenced != nil && rec.live != nil && !in.IsSelfReferenced(rec.live, rec.target) {
			continue
		}
		obj := rec.target
		if obj == nil {
			obj = rec.live
		}
		if IsHook(obj) {
			continue // live hooks are never pruned; target hooks are handled below
		}
		for _, phase := range SyncPhases(obj) {
			tasks = append(tasks, &task{phase: phase, target: rec.target, live: rec.live})
		}
	}

	if !in.skipHooks() {
		for idx, h := range hooks {
			for _, phase := range SyncPhases(h) {
				t := h.DeepCopy()
				if t.GetName() == "" {
					t.SetName(HookName(h.GetGenerateName(), in.OperationUID, phase, Wave(h), idx))
				}
				r.defaultNamespace(t)
				if !slices.Contains(t.GetFinalizers(), v1alpha1.FinalizerHook) {
					t.SetFinalizers(append(t.GetFinalizers(), v1alpha1.FinalizerHook))
				}
				ann := t.GetAnnotations()
				if ann == nil {
					ann = map[string]string{}
				}
				ann[AnnotationKeyOperation] = operationMarker(in.OperationUID, in.Attempt, phase)
				t.SetAnnotations(ann)
				tasks = append(tasks, &task{phase: phase, target: t, hook: true})
			}
		}
	}

	if r.opts.CreateNamespace && in.App.Destination.Namespace != "" {
		var err error
		tasks, err = r.autoCreateNamespace(tasks, recs)
		if err != nil {
			return nil, nil, err
		}
	}

	// Enrich target tasks with live objects (hooks find the objects of earlier syncs).
	byName := map[string][]*reconciled{}
	for _, rec := range recs {
		byName[rec.key.Group+"/"+rec.key.Kind+"/"+rec.key.Name] = append(byName[rec.key.Group+"/"+rec.key.Kind+"/"+rec.key.Name], rec)
	}
	for _, t := range tasks {
		if t.target == nil || t.live != nil {
			continue
		}
		gvk := t.target.GroupVersionKind()
		for _, rec := range byName[gvk.Group+"/"+gvk.Kind+"/"+t.target.GetName()] {
			if rec.live != nil && (rec.key.Namespace == "" || rec.key.Namespace == t.target.GetNamespace()) {
				t.live = rec.live
				break
			}
		}
	}

	if err := r.validateTasks(tasks); err != nil {
		return nil, nil, err
	}
	r.skipDryRunOfChangedKinds(tasks)

	// Prune tasks run in reverse wave order: mirror the distinct prune waves (SYNC-068).
	pruneWaves := map[int][]*task{}
	for _, t := range tasks {
		if t.isPrune() {
			pruneWaves[t.wave()] = append(pruneWaves[t.wave()], t)
		}
	}
	waves := slices.Sorted(maps.Keys(pruneWaves))
	for i, n := 0, len(waves); i < n/2; i++ {
		lo, hi := waves[i], waves[n-1-i]
		for _, t := range pruneWaves[lo] {
			w := hi
			t.waveOverride = &w
		}
		for _, t := range pruneWaves[hi] {
			w := lo
			t.waveOverride = &w
		}
	}
	// PruneLast tasks move to the last Sync-phase wave + 1 (SYNC-038).
	last := 0
	for _, t := range tasks {
		if t.phase == v1alpha1.SyncPhaseSync && t.wave() > last {
			last = t.wave()
		}
	}
	last++
	for _, t := range tasks {
		if t.isPrune() && r.opts.PruneLastFor(t.live) {
			w := last
			t.waveOverride = &w
		}
	}

	tasks.sortTasks()

	for _, t := range tasks {
		rk := t.resultKey()
		if res, ok := r.results.get(rk); ok {
			t.status, t.opState, t.message = res.Status, res.HookPhase, res.Message
		} else if t.hook && t.live != nil && !r.dryRun &&
			t.live.GetAnnotations()[AnnotationKeyOperation] == operationMarker(in.OperationUID, in.Attempt, t.phase) {
			// Created by this attempt before a restart, result not persisted: adopt it. Hooks
			// of an earlier attempt are re-run (BeforeHookCreation), like upstream retries.
			t.adopted = true
			r.set(t, v1alpha1.ResultCodeSynced, v1alpha1.OperationRunning, opMessage(t.target, "created", false))
		}
		if p, ok := r.prev[rk]; ok && isCompleted(p.HookPhase) {
			t.prevCompleted = true
		}
		if r.opts.ApplyOutOfSyncOnly && !t.hook && t.target != nil && t.live != nil {
			if modified, ok := in.DiffResults[t.resourceKey()]; ok && !modified {
				t.outOfSyncOnlySkip = true
			}
		}
	}
	r.tasks = tasks
	return tasks, nil, nil
}

// reconcile pairs targets with live objects like upstream Reconcile and sets the target
// namespace: the destination namespace for namespaced kinds without one, none for
// cluster-scoped kinds. Remaining live objects become prune candidates. The result is in
// target order followed by prune candidates sorted by key.
func (r *run) reconcile(targets []*unstructured.Unstructured) []*reconciled {
	live := make(map[ResourceKey]*unstructured.Unstructured, len(r.in.Live))
	for k, v := range r.in.Live {
		if v != nil {
			live[k] = v
		}
	}
	dedupLiveByUID(targets, live)
	destNS := r.in.App.Destination.Namespace

	recs := make([]*reconciled, 0, len(targets)+len(live))
	for _, t := range targets {
		gvk := t.GroupVersionKind()
		ns := t.GetNamespace()
		if ns == "" {
			ns = destNS
		}
		namespaced, known := r.scope(gvk)
		var keys []ResourceKey
		if namespaced || !known {
			keys = append(keys, ResourceKey{Group: gvk.Group, Kind: gvk.Kind, Namespace: ns, Name: t.GetName()})
		}
		if !namespaced || !known {
			keys = append(keys, ResourceKey{Group: gvk.Group, Kind: gvk.Kind, Name: t.GetName()})
		}
		var l *unstructured.Unstructured
		for _, k := range keys {
			if obj, ok := live[k]; ok {
				l = obj
				delete(live, k)
				break
			}
		}
		switch {
		case known && !namespaced:
			t.SetNamespace("")
		case t.GetNamespace() == "":
			t.SetNamespace(destNS)
		}
		key := keyOf(t)
		if l != nil {
			key = keyOf(l)
		}
		recs = append(recs, &reconciled{key: key, target: t, live: l})
	}
	for _, k := range slices.SortedFunc(maps.Keys(live), func(a, b ResourceKey) int { return cmp.Compare(a.String(), b.String()) }) {
		recs = append(recs, &reconciled{key: k, live: live[k]})
	}
	return recs
}

// dedupLiveByUID drops live duplicates of one object served by several API groups
// (e.g. events.k8s.io/Event and Event), keeping keys referenced by targets, like upstream.
func dedupLiveByUID(targets []*unstructured.Unstructured, live map[ResourceKey]*unstructured.Unstructured) {
	targetKeys := map[ResourceKey]bool{}
	for _, t := range targets {
		targetKeys[keyOf(t)] = true
	}
	byUID := map[types.UID][]ResourceKey{}
	for k, obj := range live {
		if uid := obj.GetUID(); uid != "" {
			byUID[uid] = append(byUID[uid], k)
		}
	}
	for _, keys := range byUID {
		if len(keys) < 2 {
			continue
		}
		slices.SortFunc(keys, func(a, b ResourceKey) int { return cmp.Compare(a.String(), b.String()) })
		left := len(keys)
		for _, k := range keys {
			if targetKeys[k] {
				continue
			}
			delete(live, k)
			left--
			if left == 1 {
				break
			}
		}
	}
}

// defaultNamespace sets the destination namespace on a namespaced (or unknown-scope)
// object without one and clears it on cluster-scoped objects.
func (r *run) defaultNamespace(obj *unstructured.Unstructured) {
	namespaced, known := r.scope(obj.GroupVersionKind())
	switch {
	case known && !namespaced:
		obj.SetNamespace("")
	case obj.GetNamespace() == "":
		obj.SetNamespace(r.in.App.Destination.Namespace)
	}
}

// autoCreateNamespace adds the PreSync task that creates (or, with managed namespace
// metadata, updates) the destination namespace (SYNC-036, SYNC-037).
func (r *run) autoCreateNamespace(tasks syncTasks, recs []*reconciled) (syncTasks, error) {
	in := r.in
	ns := in.App.Destination.Namespace
	for _, rec := range recs {
		if isNamespaceWithName(rec.target, ns) {
			return tasks, nil // a manifest for the namespace wins
		}
	}
	managed := &unstructured.Unstructured{}
	managed.SetAPIVersion("v1")
	managed.SetKind(kindNamespace)
	managed.SetName(ns)

	live, err := r.e.kube.Get(r.ctx, managed.GroupVersionKind(), "", ns)
	switch {
	case err == nil:
	case apierrors.IsNotFound(err):
		live = nil
	case IsTransient(err):
		return nil, err
	default:
		t := &task{phase: v1alpha1.SyncPhasePreSync, target: managed, planError: true}
		r.set(t, v1alpha1.ResultCodeSyncFailed, v1alpha1.OperationError, fmt.Sprintf("namespace auto creation failed: %v", err))
		return append(tasks, t), nil
	}
	md := in.ManagedNamespaceMetadata
	if live != nil && md == nil {
		return tasks, nil // exists and its metadata is not managed
	}
	if md != nil {
		managed.SetLabels(maps.Clone(md.Labels))
		ann := maps.Clone(md.Annotations)
		if ann == nil {
			ann = map[string]string{}
		}
		// Managed metadata relies on server-side apply so labels and annotations set by
		// others survive.
		ann[v1alpha1.AnnotationKeySyncOptions] = optServerSideApply
		managed.SetAnnotations(ann)
	}
	if in.TrackManagedNamespace && in.SetTracking != nil {
		if err := in.SetTracking(managed); err != nil {
			t := &task{phase: v1alpha1.SyncPhasePreSync, target: managed, planError: true}
			r.set(t, v1alpha1.ResultCodeSyncFailed, v1alpha1.OperationError, fmt.Sprintf("namespaceModifier error: %v", err))
			return append(tasks, t), nil
		}
	}
	return append(tasks, &task{phase: v1alpha1.SyncPhasePreSync, target: managed, live: live}), nil
}

// validateTasks checks every task's kind against API discovery and the project
// (SYNC-035, SYNC-072, SYNC-073).
func (r *run) validateTasks(tasks syncTasks) error {
	for _, t := range tasks {
		if t.planError {
			continue
		}
		gvk := t.gvk()
		res, err := r.resource(gvk)
		var namespaced bool
		if err != nil {
			if IsTransient(err) {
				return err
			}
			if apierrors.IsNotFound(err) && r.mayskipDryRun(t) {
				t.skipDryRun = true
				// Upstream skips the project check for such tasks; Patrol still checks it,
				// using the scope declared by the CRD of the same sync when known.
				if ns, ok := r.crdScopes[gvk.Group+"/"+gvk.Kind]; ok {
					namespaced = ns
				} else {
					namespaced = t.namespace() != ""
				}
			} else {
				r.set(t, v1alpha1.ResultCodeSyncFailed, "", err.Error())
				r.valid = false
				continue
			}
		} else {
			namespaced = res.Namespaced
			if !namespaced && t.target != nil && t.target.GetNamespace() != "" {
				t.target.SetNamespace("")
			}
		}
		t.namespaced = &namespaced
		if r.in.Permission != nil {
			if err := r.in.Permission(t.obj(), namespaced); err != nil {
				r.set(t, v1alpha1.ResultCodeSyncFailed, "", err.Error())
				r.valid = false
			}
		}
	}
	return nil
}

// skipDryRunOfChangedKinds skips the dry run of custom resources whose CRD this sync
// creates or changes: the server would validate them against the schema being replaced
// (e.g. reject a field the new schema adds). A CRD counts as unchanged only when the
// comparison reports it unmodified. Upstream's client dry run validates no schema at all.
func (r *run) skipDryRunOfChangedKinds(tasks syncTasks) {
	changing := map[string]bool{}
	for _, t := range tasks {
		gk, ok := crdGroupKind(t.target)
		if !ok {
			continue
		}
		if modified, known := r.in.DiffResults[t.resourceKey()]; t.live == nil || !known || modified {
			changing[gk] = true
		}
	}
	if len(changing) == 0 {
		return
	}
	for _, t := range tasks {
		if t.target == nil || isCRD(t.target) {
			continue
		}
		gvk := t.target.GroupVersionKind()
		if changing[gvk.Group+"/"+gvk.Kind] {
			t.skipDryRun = true
		}
	}
}

func (r *run) mayskipDryRun(t *task) bool {
	if r.opts.SkipDryRunFor(t.target) {
		return true
	}
	gvk := t.gvk()
	return r.crdKinds[gvk.Group+"/"+gvk.Kind]
}

// markPendingNamespaces flags tasks whose namespace is created by this sync and does not
// exist yet: a server-side dry run cannot validate them.
func (r *run) markPendingNamespaces(tasks syncTasks) {
	pending := map[string]bool{}
	for _, t := range tasks {
		if t.target != nil && t.live == nil && isNamespaceKind(t.target) && !r.createdNamespaces[t.target.GetName()] {
			pending[t.target.GetName()] = true
		}
	}
	for _, t := range tasks {
		t.nsPending = t.target != nil && t.target.GetNamespace() != "" && pending[t.target.GetNamespace()]
	}
}

// namespaceCreationTask returns the task creating the destination namespace before the
// dry run: a Namespace target (synthetic or from Git) for it without live object.
func (r *run) namespaceCreationTask(tasks syncTasks) *task {
	for _, t := range tasks {
		if t.live == nil && !t.planError && isNamespaceWithName(t.target, r.in.App.Destination.Namespace) && r.in.App.Destination.Namespace != "" {
			return t
		}
	}
	return nil
}
