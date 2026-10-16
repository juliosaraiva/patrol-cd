package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Helm hook values mapped to sync hook types (SYNC-060).
var helmHookTypes = map[string]v1alpha1.HookType{
	"pre-install":  v1alpha1.HookTypePreSync,
	"pre-upgrade":  v1alpha1.HookTypePreSync,
	"post-install": v1alpha1.HookTypePostSync,
	"post-upgrade": v1alpha1.HookTypePostSync,
}

// Helm delete policies mapped to hook delete policies (SYNC-061).
var helmDeletePolicies = map[string]v1alpha1.HookDeletePolicy{
	"before-hook-creation": v1alpha1.HookDeletePolicyBeforeHookCreation,
	"hook-succeeded":       v1alpha1.HookDeletePolicyHookSucceeded,
	"hook-failed":          v1alpha1.HookDeletePolicyHookFailed,
}

const helmCRDInstall = "crd-install"

// validSyncHookType reports whether t is a hook type the sync engine runs (PreSync, Sync,
// PostSync, SyncFail) or Skip. PreDelete and PostDelete are handled by the Application
// deletion flow, not by the engine.
func validSyncHookType(t string) bool {
	switch v1alpha1.HookType(t) {
	case v1alpha1.HookTypePreSync, v1alpha1.HookTypeSync, v1alpha1.HookTypePostSync,
		v1alpha1.HookTypeSyncFail, v1alpha1.HookTypeSkip:
		return true
	}
	return false
}

// HookTypes returns the sync hook types of obj (SYNC-053). Values of the
// cd.patrol.example/hook annotation are CSV (trimmed, de-duplicated); unknown values are
// dropped. Only when the object has no valid Patrol hook type are its helm.sh/hook values
// mapped (pre-install/pre-upgrade → PreSync, post-install/post-upgrade → PostSync).
func HookTypes(obj *unstructured.Unstructured) []v1alpha1.HookType {
	var types []v1alpha1.HookType
	for _, v := range AnnotationOptions(obj, v1alpha1.AnnotationKeyHook) {
		if validSyncHookType(v) {
			types = append(types, v1alpha1.HookType(v))
		}
	}
	if len(types) > 0 {
		return types
	}
	for _, v := range AnnotationOptions(obj, AnnotationKeyHelmHook) {
		if t, ok := helmHookTypes[v]; ok {
			types = append(types, t)
		}
	}
	return types
}

// IsSkip reports whether obj is annotated with Skip as its only hook type (SYNC-050): it is
// never applied and is not a hook.
func IsSkip(obj *unstructured.Unstructured) bool {
	types := HookTypes(obj)
	return len(types) == 1 && types[0] == v1alpha1.HookTypeSkip
}

// IsHook reports whether obj is a sync hook: it carries the cd.patrol.example/hook
// annotation (with any value except a lone Skip), or a helm.sh/hook annotation other than
// crd-install.
func IsHook(obj *unstructured.Unstructured) bool {
	if obj == nil {
		return false
	}
	ann := obj.GetAnnotations()
	if _, ok := ann[v1alpha1.AnnotationKeyHook]; ok {
		return !IsSkip(obj)
	}
	v, ok := ann[AnnotationKeyHelmHook]
	return ok && v != helmCRDInstall
}

// IsIgnored reports whether obj is a hook without any valid type (for example
// "cd.patrol.example/hook: Foo" or "helm.sh/hook: test"). Such objects are dropped
// entirely: neither applied, pruned nor tracked.
func IsIgnored(obj *unstructured.Unstructured) bool {
	return IsHook(obj) && len(HookTypes(obj)) == 0
}

// SyncPhases returns the sync phases an object runs in: none for Skip, every listed phase
// for hooks (in phase order), Sync for other resources (SYNC-048).
func SyncPhases(obj *unstructured.Unstructured) []v1alpha1.SyncPhase {
	if IsSkip(obj) {
		return nil
	}
	if !IsHook(obj) {
		return []v1alpha1.SyncPhase{v1alpha1.SyncPhaseSync}
	}
	seen := map[v1alpha1.SyncPhase]bool{}
	var phases []v1alpha1.SyncPhase
	for _, t := range HookTypes(obj) {
		switch t {
		case v1alpha1.HookTypePreSync, v1alpha1.HookTypeSync, v1alpha1.HookTypePostSync, v1alpha1.HookTypeSyncFail:
			p := v1alpha1.SyncPhase(t)
			if !seen[p] {
				seen[p] = true
				phases = append(phases, p)
			}
		}
	}
	slices.SortFunc(phases, func(a, b v1alpha1.SyncPhase) int { return phaseOrder(a) - phaseOrder(b) })
	return phases
}

// DeletePolicies returns the hook delete policies of obj: the union of the Patrol
// annotation and the mapped Helm annotation, BeforeHookCreation when neither has a valid
// value (SYNC-054..SYNC-056, SYNC-061).
func DeletePolicies(obj *unstructured.Unstructured) []v1alpha1.HookDeletePolicy {
	var policies []v1alpha1.HookDeletePolicy
	add := func(p v1alpha1.HookDeletePolicy) {
		if !slices.Contains(policies, p) {
			policies = append(policies, p)
		}
	}
	for _, v := range AnnotationOptions(obj, v1alpha1.AnnotationKeyHookDeletePolicy) {
		switch p := v1alpha1.HookDeletePolicy(v); p {
		case v1alpha1.HookDeletePolicyHookSucceeded, v1alpha1.HookDeletePolicyHookFailed, v1alpha1.HookDeletePolicyBeforeHookCreation:
			add(p)
		}
	}
	for _, v := range AnnotationOptions(obj, AnnotationKeyHelmHookDeletePolicy) {
		if p, ok := helmDeletePolicies[v]; ok {
			add(p)
		}
	}
	if len(policies) == 0 {
		policies = append(policies, v1alpha1.HookDeletePolicyBeforeHookCreation)
	}
	return policies
}

// Wave returns the sync wave of obj: the integer cd.patrol.example/sync-wave annotation,
// else the integer helm.sh/hook-weight annotation (on any object), else 0 (SYNC-061,
// SYNC-062).
func Wave(obj *unstructured.Unstructured) int {
	ann := obj.GetAnnotations()
	if v, ok := ann[v1alpha1.AnnotationKeySyncWave]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	if v, ok := ann[AnnotationKeyHelmHookWeight]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// deleteHookValues are the annotation values marking Application deletion hooks.
var deleteHookValues = map[v1alpha1.HookType]map[string]string{
	v1alpha1.HookTypePreDelete: {
		v1alpha1.AnnotationKeyHook: string(v1alpha1.HookTypePreDelete),
		AnnotationKeyHelmHook:      "pre-delete",
	},
	v1alpha1.HookTypePostDelete: {
		v1alpha1.AnnotationKeyHook: string(v1alpha1.HookTypePostDelete),
		AnnotationKeyHelmHook:      "post-delete",
	},
}

func isDeleteHookOfType(obj *unstructured.Unstructured, t v1alpha1.HookType) bool {
	if obj == nil {
		return false
	}
	ann := obj.GetAnnotations()
	for key, want := range deleteHookValues[t] {
		raw, ok := ann[key]
		if !ok {
			continue
		}
		for _, item := range strings.Split(raw, ",") {
			if strings.TrimSpace(item) == want {
				return true
			}
		}
	}
	return false
}

// IsPreDeleteHook reports whether obj is a PreDelete hook ("PreDelete" in the Patrol hook
// annotation or "pre-delete" in helm.sh/hook) (SYNC-051).
func IsPreDeleteHook(obj *unstructured.Unstructured) bool {
	return isDeleteHookOfType(obj, v1alpha1.HookTypePreDelete)
}

// IsPostDeleteHook reports whether obj is a PostDelete hook ("PostDelete" or "post-delete")
// (SYNC-052).
func IsPostDeleteHook(obj *unstructured.Unstructured) bool {
	return isDeleteHookOfType(obj, v1alpha1.HookTypePostDelete)
}

// hasSyncPhaseHook reports whether obj is also a hook of a sync phase.
func hasSyncPhaseHook(obj *unstructured.Unstructured) bool {
	for _, t := range HookTypes(obj) {
		switch t {
		case v1alpha1.HookTypePreSync, v1alpha1.HookTypeSync, v1alpha1.HookTypePostSync, v1alpha1.HookTypeSyncFail:
			return true
		}
	}
	return false
}

// PartitionTargets removes Application deletion hooks that have no sync phase from the
// rendered manifests, like upstream's partitionTargetObjsForSync. It reports whether the
// full set declared PreDelete and/or PostDelete hooks, which drives the Application
// finalizers. Nil entries are dropped.
func PartitionTargets(targets []*unstructured.Unstructured) (syncObjs []*unstructured.Unstructured, hasPreDelete, hasPostDelete bool) {
	for _, obj := range targets {
		if obj == nil {
			continue
		}
		if IsPreDeleteHook(obj) {
			hasPreDelete = true
			if !hasSyncPhaseHook(obj) {
				continue
			}
		}
		if IsPostDeleteHook(obj) {
			hasPostDelete = true
			if !hasSyncPhaseHook(obj) {
				continue
			}
		}
		syncObjs = append(syncObjs, obj)
	}
	return syncObjs, hasPreDelete, hasPostDelete
}

// HookName returns the deterministic name of a hook that only has metadata.generateName:
// generateName followed by the first 8 hex characters of
// sha256("<operationUID>/<phase>/<wave>/<index>"), where index is the position of the hook
// among the hook manifests of the operation's rendered targets
// (docs/architecture/sync-state-machine.md). A resumed operation therefore finds the hook it
// created before a restart.
func HookName(generateName, operationUID string, phase v1alpha1.SyncPhase, wave, index int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%d/%d", operationUID, phase, wave, index)))
	return generateName + hex.EncodeToString(sum[:])[:8]
}

// operationMarker is the AnnotationKeyOperation value of a hook created by an attempt of an
// operation in a phase: "<operationUID>/<attempt>/<phase>".
func operationMarker(operationUID string, attempt int64, phase v1alpha1.SyncPhase) string {
	return fmt.Sprintf("%s/%d/%s", operationUID, attempt, phase)
}
