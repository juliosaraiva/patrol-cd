package sync

import (
	"cmp"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// phaseOrder orders sync phases: PreSync < Sync < PostSync < SyncFail.
func phaseOrder(p v1alpha1.SyncPhase) int {
	switch p {
	case v1alpha1.SyncPhasePreSync:
		return -1
	case v1alpha1.SyncPhaseSync:
		return 0
	case v1alpha1.SyncPhasePostSync:
		return 1
	case v1alpha1.SyncPhaseSyncFail:
		return 2
	}
	return 3
}

// kindOrderList is the apply order of well-known kinds (SYNC-065), taken from Helm's kind
// sorter like upstream. Every other kind sorts after these.
var kindOrderList = []string{
	"Namespace",
	"NetworkPolicy",
	"ResourceQuota",
	"LimitRange",
	"PodSecurityPolicy",
	"PodDisruptionBudget",
	"ServiceAccount",
	"Secret",
	"SecretList",
	"ConfigMap",
	"StorageClass",
	"PersistentVolume",
	"PersistentVolumeClaim",
	"CustomResourceDefinition",
	"ClusterRole",
	"ClusterRoleList",
	"ClusterRoleBinding",
	"ClusterRoleBindingList",
	"Role",
	"RoleList",
	"RoleBinding",
	"RoleBindingList",
	"Service",
	"DaemonSet",
	"Pod",
	"ReplicationController",
	"ReplicaSet",
	"Deployment",
	"HorizontalPodAutoscaler",
	"StatefulSet",
	"Job",
	"CronJob",
	"IngressClass",
	"Ingress",
	"APIService",
}

var kindOrderIndex = func() map[string]int {
	m := make(map[string]int, len(kindOrderList))
	for i, k := range kindOrderList {
		m[k] = i - len(kindOrderList) // all negative; unknown kinds get 0 and sort last
	}
	return m
}()

// KindOrder returns the sort weight of a kind; lower applies first. The group is ignored,
// like upstream.
func KindOrder(kind string) int { return kindOrderIndex[kind] }

// task is one unit of work: apply a target (live may be nil), prune a live object (target
// nil) or run a hook in one phase.
type task struct {
	phase  v1alpha1.SyncPhase
	target *unstructured.Unstructured
	live   *unstructured.Unstructured

	hook         bool
	namespaced   *bool // resource scope when known
	waveOverride *int
	skipDryRun   bool

	// Execution state, seeded from persisted results.
	status  v1alpha1.ResultCode
	opState v1alpha1.OperationPhase
	message string

	// prevCompleted is true when the completion of this task was already persisted in a
	// previous step (Input.Previous).
	prevCompleted bool
	// adopted is true for a hook found in the cluster with this operation's marker.
	adopted bool
	// nsPending is true when the task's namespace is created by this sync and does not exist
	// yet, so a server-side dry run cannot validate it.
	nsPending bool
	// outOfSyncOnlySkip is true when ApplyOutOfSyncOnly skips the task.
	outOfSyncOnlySkip bool
	// planError is set for a task that failed planning (e.g. namespace auto creation).
	planError bool
	// order is the reserved position of the task's first result row (0 = none).
	order int
}

func (t *task) obj() *unstructured.Unstructured {
	if t.target != nil {
		return t.target
	}
	return t.live
}

func (t *task) isPrune() bool { return t.target == nil }

func (t *task) wave() int {
	if t.waveOverride != nil {
		return *t.waveOverride
	}
	return Wave(t.obj())
}

func (t *task) gvk() schema.GroupVersionKind { return t.obj().GroupVersionKind() }
func (t *task) group() string                { return t.gvk().Group }
func (t *task) kind() string                 { return t.gvk().Kind }
func (t *task) version() string              { return t.gvk().Version }
func (t *task) name() string                 { return t.obj().GetName() }
func (t *task) namespace() string            { return t.obj().GetNamespace() }

func (t *task) pending() bool { return t.opState == "" }
func (t *task) running() bool { return t.opState == v1alpha1.OperationRunning }
func (t *task) completed() bool {
	return isCompleted(t.opState)
}
func (t *task) successful() bool { return t.opState == v1alpha1.OperationSucceeded }
func (t *task) pruned() bool     { return t.status == v1alpha1.ResultCodePruned }

func isCompleted(p v1alpha1.OperationPhase) bool {
	switch p {
	case v1alpha1.OperationSucceeded, v1alpha1.OperationFailed, v1alpha1.OperationError:
		return true
	}
	return false
}

func (t *task) hookType() v1alpha1.HookType {
	if t.hook {
		return v1alpha1.HookType(t.phase)
	}
	return ""
}

func (t *task) hasDeletePolicy(p v1alpha1.HookDeletePolicy) bool {
	if !t.hook {
		return false
	}
	return slices.Contains(DeletePolicies(t.obj()), p)
}

// deleteBeforeCreation: a pending hook whose object already exists is deleted first.
func (t *task) deleteBeforeCreation() bool {
	return t.live != nil && t.pending() && t.hasDeletePolicy(v1alpha1.HookDeletePolicyBeforeHookCreation)
}

func (t *task) deleteOnPhaseSuccessful() bool {
	return t.live != nil && t.hasDeletePolicy(v1alpha1.HookDeletePolicyHookSucceeded)
}

func (t *task) deleteOnPhaseFailed() bool {
	return t.live != nil && t.hasDeletePolicy(v1alpha1.HookDeletePolicyHookFailed)
}

// key is the identity used for results: the target's (namespace-defaulted) key, or the
// live key for prune tasks.
func (t *task) key() ResourceKey { return keyOf(t.obj()) }

// resourceKey matches diff results: like key, but the live namespace wins when a live object
// exists (cluster-scoped targets may carry the destination namespace).
func (t *task) resourceKey() ResourceKey {
	k := keyOf(t.obj())
	if t.live != nil {
		k.Namespace = t.live.GetNamespace()
	}
	return k
}

func (t *task) resultKey() string { return resultKey(t.key(), t.phase) }

func (t *task) String() string {
	kind := "resource"
	if t.hook {
		kind = "hook"
	}
	return fmt.Sprintf("%s/%d %s %s/%s:%s/%s", t.phase, t.wave(), kind, t.group(), t.kind(), t.namespace(), t.name())
}

type syncTasks []*task

// compareTasks orders tasks by phase, wave, kind order and name like upstream, then by
// kind, group, namespace and apply-before-prune so that the order is total.
func compareTasks(a, b *task) int {
	if c := cmp.Compare(phaseOrder(a.phase), phaseOrder(b.phase)); c != 0 {
		return c
	}
	if c := cmp.Compare(a.wave(), b.wave()); c != 0 {
		return c
	}
	ao, bo := a.obj(), b.obj()
	if c := cmp.Compare(KindOrder(ao.GetKind()), KindOrder(bo.GetKind())); c != 0 {
		return c
	}
	if c := cmp.Compare(ao.GetName(), bo.GetName()); c != 0 {
		return c
	}
	if c := cmp.Compare(ao.GetKind(), bo.GetKind()); c != 0 {
		return c
	}
	if c := cmp.Compare(ao.GroupVersionKind().Group, bo.GroupVersionKind().Group); c != 0 {
		return c
	}
	if c := cmp.Compare(ao.GetNamespace(), bo.GetNamespace()); c != 0 {
		return c
	}
	return cmp.Compare(boolInt(a.isPrune()), boolInt(b.isPrune()))
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// sortTasks sorts tasks and then moves dependencies before their first dependent
// (SYNC-066): a core Namespace before the first task in that namespace, a CRD before the
// first resource of its group/kind. A moved dependency takes the phase and wave of that
// dependent.
func (s syncTasks) sortTasks() {
	slices.SortStableFunc(s, compareTasks)
	s.adjustDeps(func(obj *unstructured.Unstructured) (string, bool) {
		return obj.GetName(), isNamespaceKind(obj)
	}, func(obj *unstructured.Unstructured) (string, bool) {
		return obj.GetNamespace(), obj.GetNamespace() != ""
	})
	s.adjustDeps(crdGroupKind, func(obj *unstructured.Unstructured) (string, bool) {
		gvk := obj.GroupVersionKind()
		return gvk.Group + "/" + gvk.Kind, true
	})
}

func (s syncTasks) adjustDeps(isDep, refDep func(*unstructured.Unstructured) (string, bool)) {
	firstIndex := map[string]int{}
	for i, t := range s {
		if t.target == nil {
			continue
		}
		if depKey, ok := isDep(t.target); ok {
			if index, ok := firstIndex[depKey]; ok {
				wave := s[index].wave()
				t.waveOverride = &wave
				t.phase = s[index].phase
				copy(s[index+1:i+1], s[index:i])
				s[index] = t
				for k, v := range firstIndex {
					if v >= index {
						firstIndex[k] = v + 1
					}
				}
			}
		} else if depKey, ok := refDep(t.target); ok {
			if _, seen := firstIndex[depKey]; !seen {
				firstIndex[depKey] = i
			}
		}
	}
}

func (s syncTasks) filter(pred func(*task) bool) syncTasks {
	var out syncTasks
	for _, t := range s {
		if pred(t) {
			out = append(out, t)
		}
	}
	return out
}

func (s syncTasks) split(pred func(*task) bool) (yes, no syncTasks) {
	for _, t := range s {
		if pred(t) {
			yes = append(yes, t)
		} else {
			no = append(no, t)
		}
	}
	return yes, no
}

func (s syncTasks) any(pred func(*task) bool) bool { return slices.ContainsFunc(s, pred) }

func (s syncTasks) all(pred func(*task) bool) bool {
	for _, t := range s {
		if !pred(t) {
			return false
		}
	}
	return true
}

func (s syncTasks) phase() v1alpha1.SyncPhase {
	if len(s) > 0 {
		return s[0].phase
	}
	return ""
}

func (s syncTasks) wave() int {
	if len(s) > 0 {
		return s[0].wave()
	}
	return 0
}

func (s syncTasks) lastPhase() v1alpha1.SyncPhase {
	if len(s) > 0 {
		return s[len(s)-1].phase
	}
	return ""
}

func (s syncTasks) lastWave() int {
	if len(s) > 0 {
		return s[len(s)-1].wave()
	}
	return 0
}

// multiStep reports whether the tasks span more than one (phase, wave).
func (s syncTasks) multiStep() bool {
	return s.wave() != s.lastWave() || s.phase() != s.lastPhase()
}
