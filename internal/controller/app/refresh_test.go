package app

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	syncpkg "github.com/juliosaraiva/patrol-cd/internal/sync"
)

// comparedApp is an Application whose status matches its spec (nothing to refresh).
func comparedApp(reconciledAgo time.Duration, mutate ...func(*v1alpha1.Application)) *v1alpha1.Application {
	a := testApp()
	a.Generation = 3
	src := *a.Spec.Source
	t := metav1.NewTime(now.Add(-reconciledAgo))
	a.Status = v1alpha1.ApplicationStatus{
		ObservedGeneration: 3,
		ReconciledAt:       &t,
		Sync: v1alpha1.SyncStatus{Status: v1alpha1.SyncStatusCodeSynced, ComparedTo: v1alpha1.ComparedTo{
			Source: &src, Destination: a.Spec.Destination,
		}},
	}
	for _, fn := range mutate {
		fn(a)
	}
	return a
}

func TestNeedRefresh(t *testing.T) {
	t.Parallel()
	rec := settings.Reconciliation{Timeout: 3 * time.Minute, HardTimeout: 0}
	for _, tc := range []struct {
		name      string
		app       *v1alpha1.Application
		rec       settings.Reconciliation
		requested core.CompareLevel
		level     core.CompareLevel
		hard      bool
		reason    string
	}{
		{"nothing to do", comparedApp(time.Minute), rec, core.CompareNone, core.CompareNone, false, ""},
		{"normal refresh annotation", comparedApp(time.Minute, func(a *v1alpha1.Application) {
			a.Annotations = map[string]string{v1alpha1.AnnotationKeyRefresh: "normal"}
		}), rec, core.CompareNone, core.CompareForceResolve, false, "normal refresh requested"},
		{"hard refresh annotation", comparedApp(time.Minute, func(a *v1alpha1.Application) {
			a.Annotations = map[string]string{v1alpha1.AnnotationKeyRefresh: "hard"}
		}), rec, core.CompareNone, core.CompareForceResolve, true, "hard refresh requested"},
		{"never compared", testApp(), rec, core.CompareNone, core.CompareForceResolve, false, "spec.source differs"},
		{"source changed", comparedApp(time.Minute, func(a *v1alpha1.Application) { a.Spec.Source.TargetRevision = "v2" }), rec, core.CompareNone, core.CompareForceResolve, false, "spec.source differs"},
		{"soft expiry", comparedApp(4 * time.Minute), rec, core.CompareNone, core.CompareLatest, false, ""},
		{"timeout 0 disables expiry", comparedApp(time.Hour), settings.Reconciliation{}, core.CompareNone, core.CompareNone, false, ""},
		{"hard expiry", comparedApp(5 * time.Minute), settings.Reconciliation{Timeout: 3 * time.Minute, HardTimeout: 4 * time.Minute}, core.CompareNone, core.CompareLatest, true, ""},
		{"destination changed", comparedApp(time.Minute, func(a *v1alpha1.Application) { a.Spec.Destination.Namespace = "other" }), rec, core.CompareNone, core.CompareLatest, false, "spec.destination differs"},
		{"ignoreDifferences changed", comparedApp(time.Minute, func(a *v1alpha1.Application) {
			a.Spec.IgnoreDifferences = v1alpha1.IgnoreDifferences{{Kind: "ConfigMap", JSONPointers: []string{"/data"}}}
		}), rec, core.CompareNone, core.CompareLatest, false, "spec.ignoreDifferences differs"},
		{"generation changed", comparedApp(time.Minute, func(a *v1alpha1.Application) { a.Generation = 4 }), rec, core.CompareNone, core.CompareLatest, false, "spec changed"},
		{"manual operation completed", comparedApp(time.Minute, func(a *v1alpha1.Application) {
			f := metav1.NewTime(now)
			a.Status.OperationState = &v1alpha1.ApplicationOperationState{Phase: v1alpha1.OperationSucceeded, FinishedAt: &f}
		}), rec, core.CompareNone, core.CompareForceResolve, false, "operation completed"},
		{"automated operation completed", comparedApp(time.Minute, func(a *v1alpha1.Application) {
			f := metav1.NewTime(now)
			a.Status.OperationState = &v1alpha1.ApplicationOperationState{Phase: v1alpha1.OperationSucceeded, FinishedAt: &f, InitiatedBy: v1alpha1.OperationInitiator{Automated: true}}
		}), rec, core.CompareNone, core.CompareLatest, false, "operation completed"},
		{"operation completed before the comparison", comparedApp(time.Minute, func(a *v1alpha1.Application) {
			f := metav1.NewTime(now.Add(-2 * time.Minute))
			a.Status.OperationState = &v1alpha1.ApplicationOperationState{Phase: v1alpha1.OperationSucceeded, FinishedAt: &f}
		}), rec, core.CompareNone, core.CompareNone, false, ""},
		{"controller requested", comparedApp(time.Minute), rec, core.CompareRecent, core.CompareRecent, false, "controller refresh requested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := needRefresh(tc.app, tc.rec, tc.requested, now)
			assert.Equal(t, tc.level, d.level, d.reason)
			assert.Equal(t, tc.hard, d.hard)
			if tc.reason != "" {
				assert.Equal(t, tc.reason, d.reason)
			}
		})
	}
}

func TestNextExpiry(t *testing.T) {
	t.Parallel()
	ago := func(d time.Duration) *metav1.Time { t := metav1.NewTime(now.Add(-d)); return &t }
	noJitter := func(time.Duration) time.Duration { return 0 }
	maxJitter := func(m time.Duration) time.Duration { return m - time.Second }
	rec := settings.Reconciliation{Timeout: 3 * time.Minute, Jitter: time.Minute}
	assert.Equal(t, 2*time.Minute, nextExpiry(ago(time.Minute), rec, now, noJitter))
	assert.Equal(t, 2*time.Minute+59*time.Second, nextExpiry(ago(time.Minute), rec, now, maxJitter))
	assert.Equal(t, time.Second, nextExpiry(ago(time.Hour), rec, now, noJitter), "overdue")
	assert.Equal(t, time.Duration(0), nextExpiry(ago(time.Minute), settings.Reconciliation{}, now, noJitter), "disabled")
	assert.Equal(t, 30*time.Second, nextExpiry(ago(time.Minute), settings.Reconciliation{Timeout: 3 * time.Minute, HardTimeout: 90 * time.Second}, now, noJitter))
	assert.Equal(t, time.Duration(0), nextExpiry(nil, rec, now, noJitter))
	for range 100 {
		j := randomJitter(time.Minute)
		assert.True(t, j >= 0 && j < time.Minute)
	}
	assert.Equal(t, time.Duration(0), randomJitter(0))
}

func TestRefreshQueue(t *testing.T) {
	t.Parallel()
	clk := clocktesting.NewFakeClock(now)
	q := newRefreshQueue(clk)
	key := types.NamespacedName{Namespace: "ns", Name: "a"}
	lvl, wait := q.take(key)
	assert.Equal(t, core.CompareNone, lvl)
	assert.Zero(t, wait)

	q.RequestRefresh(key, core.CompareRecent, 0)
	q.RequestRefresh(key, core.CompareLatest, 0)
	q.RequestRefresh(key, core.CompareRecent, 0)
	lvl, _ = q.take(key)
	assert.Equal(t, core.CompareLatest, lvl, "requests coalesce to the highest level")
	lvl, _ = q.take(key)
	assert.Equal(t, core.CompareNone, lvl, "taken requests are removed")

	q.RequestRefresh(key, core.CompareLatest, 10*time.Second)
	lvl, wait = q.take(key)
	assert.Equal(t, core.CompareNone, lvl)
	assert.Equal(t, 10*time.Second, wait)
	clk.Step(10 * time.Second)
	lvl, _ = q.take(key)
	assert.Equal(t, core.CompareLatest, lvl)

	q.restore(key, core.CompareForceResolve)
	lvl, _ = q.take(key)
	assert.Equal(t, core.CompareForceResolve, lvl)
	q.RequestRefresh(key, core.CompareRecent, 0)
	q.forget(key)
	lvl, _ = q.take(key)
	assert.Equal(t, core.CompareNone, lvl)
	assert.GreaterOrEqual(t, len(q.events), 1, "requests notify the controller")

	q.RequestTreeRefresh(key)
	lvl, tree, _ := q.takeAll(key)
	assert.Equal(t, core.CompareNone, lvl)
	assert.True(t, tree)
	q.restoreAll(key, core.CompareNone, true)
	lvl, tree, _ = q.takeAll(key)
	assert.Equal(t, core.CompareNone, lvl)
	assert.True(t, tree, "a restored tree-only refresh")
	q.RequestTreeRefresh(key)
	q.restore(key, core.CompareRecent)
	lvl, tree, _ = q.takeAll(key)
	assert.Equal(t, core.CompareRecent, lvl)
	assert.True(t, tree, "a restored level keeps a pending tree-only refresh")
	q.RequestTreeRefresh(key)
	q.RequestRefresh(key, core.CompareRecent, 0)
	lvl, tree, _ = q.takeAll(key)
	assert.Equal(t, core.CompareRecent, lvl)
	assert.True(t, tree, "tree and comparison requests coalesce")
}

// TestRefreshQueueBoundsNotifications: requests made while nobody drains the channel (a
// standby replica, a burst) never start per-request timers: a key has at most one event
// outstanding, overflowing keys share one queue timer, and delayed requests of a key share
// its earliest timer. Draining delivers every key once.
func TestRefreshQueueBoundsNotifications(t *testing.T) {
	t.Parallel()
	clk := clocktesting.NewFakeClock(now)
	var timers []func()
	q := newRefreshQueueWith(clk, func(_ time.Duration, f func()) { timers = append(timers, f) }, 4)
	keys := make([]types.NamespacedName, 100)
	for i := range keys {
		keys[i] = types.NamespacedName{Namespace: "ns", Name: fmt.Sprintf("app-%d", i)}
	}
	for range 3 {
		for _, k := range keys {
			q.RequestRefresh(k, core.CompareLatest, 0)
			q.RequestTreeRefresh(k)
		}
	}
	pending, overflow, armed := q.stats()
	assert.Equal(t, 100, pending)
	assert.Equal(t, 96, overflow)
	assert.True(t, armed)
	assert.Len(t, timers, 1, "one queue timer, not one per request")
	assert.Len(t, q.events, 4)

	// The controller drains: every key is delivered exactly once.
	seen := map[types.NamespacedName]int{}
	for len(seen) < len(keys) {
		for len(q.events) > 0 {
			ev := <-q.events
			key := types.NamespacedName{Namespace: ev.Object.Namespace, Name: ev.Object.Name}
			seen[key]++
			q.received(key)
			lvl, tree, _ := q.takeAll(key)
			assert.Equal(t, core.CompareLatest, lvl)
			assert.True(t, tree)
		}
		if len(seen) == len(keys) {
			break
		}
		require.NotEmpty(t, timers, "an overflow timer is armed while keys wait")
		fire := timers[0]
		timers = timers[1:]
		fire()
	}
	for k, n := range seen {
		assert.Equal(t, 1, n, k.String())
	}
	pending, overflow, armed = q.stats()
	assert.Zero(t, pending)
	assert.Zero(t, overflow)
	assert.False(t, armed)

	// An outstanding event is not duplicated; after the reconcile received it, a new request
	// notifies again.
	k := keys[0]
	q.RequestRefresh(k, core.CompareRecent, 0)
	q.RequestRefresh(k, core.CompareLatest, 0)
	assert.Len(t, q.events, 1)
	<-q.events
	q.received(k)
	q.RequestRefresh(k, core.CompareRecent, 0)
	assert.Len(t, q.events, 1)
	<-q.events
	lvl, _ := q.take(k)
	assert.Equal(t, core.CompareLatest, lvl)

	// Delayed requests of one key share the earliest timer.
	timers = nil
	for range 10 {
		q.RequestRefresh(k, core.CompareLatest, 10*time.Second)
	}
	q.RequestRefresh(k, core.CompareLatest, 20*time.Second)
	assert.Len(t, timers, 1)
	q.RequestRefresh(k, core.CompareLatest, 5*time.Second)
	assert.Len(t, timers, 2, "an earlier due time arms an earlier timer")
	clk.Step(5 * time.Second)
	timers[1]()
	assert.Len(t, q.events, 1)
	timers[0]()
	assert.Len(t, q.events, 1, "the later timer does not duplicate the outstanding event")
}

func TestDeletionHelpers(t *testing.T) {
	t.Parallel()
	app := testApp()
	cm := func(name string, wave int, annotations map[string]string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": name, "namespace": "default"}}}
		a := map[string]string{v1alpha1.AnnotationKeySyncWave: strconv.Itoa(wave)}
		for k, v := range annotations {
			a[k] = v
		}
		u.SetAnnotations(a)
		return u
	}
	objs := []*unstructured.Unstructured{cm("b", 1, nil), cm("a", 5, nil), cm("c", 5, nil), cm("d", -2, nil)}
	top := highestWave(objs)
	assert.Equal(t, []string{"a", "c"}, []string{top[0].GetName(), top[1].GetName()})
	assert.Nil(t, highestWave(nil))

	opts := syncpkg.ParseOptions(nil)
	assert.True(t, shouldBeDeleted(app, opts, cm("x", 0, nil)))
	assert.False(t, shouldBeDeleted(app, opts, cm("x", 0, map[string]string{v1alpha1.AnnotationKeySyncOptions: "Delete=false"})))
	assert.False(t, shouldBeDeleted(app, opts, cm("x", 0, map[string]string{"helm.sh/resource-policy": "keep"})))
	assert.False(t, shouldBeDeleted(app, syncpkg.ParseOptions(v1alpha1.SyncOptions{"Delete=false"}), cm("x", 0, nil)), "application-level Delete=false")
	assert.True(t, shouldBeDeleted(app, syncpkg.ParseOptions(v1alpha1.SyncOptions{"Delete=false"}), cm("x", 0, map[string]string{v1alpha1.AnnotationKeySyncOptions: "Delete=true"})), "resource annotation wins")
	crd := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]any{"name": "x.example.com"}}}
	assert.False(t, shouldBeDeleted(app, opts, crd))
	self := &unstructured.Unstructured{Object: map[string]any{"apiVersion": v1alpha1.GroupVersion.String(), "kind": "Application", "metadata": map[string]any{"name": app.Name, "namespace": app.Namespace}}}
	assert.False(t, shouldBeDeleted(app, opts, self), "a self-managed Application never deletes itself")

	assert.Equal(t, metav1.DeletePropagationForeground, propagationPolicy([]string{v1alpha1.FinalizerResources}))
	assert.Equal(t, metav1.DeletePropagationForeground, propagationPolicy([]string{v1alpha1.FinalizerResourcesForeground, v1alpha1.FinalizerResourcesBackground}))
	assert.Equal(t, metav1.DeletePropagationBackground, propagationPolicy([]string{"other", v1alpha1.FinalizerResourcesBackground, v1alpha1.FinalizerResources}))

	assert.Equal(t, []string{"a", "x", "y"}, setFinalizers([]string{"a"}, true, "x", "y"))
	assert.Equal(t, []string{"a"}, setFinalizers([]string{"x", "a", "y"}, false, "x", "y"))

	none := cm("h", 0, map[string]string{v1alpha1.AnnotationKeyHook: "PostDelete"})
	assert.Nil(t, explicitDeletePolicies(none))
	succeeded := cm("h", 0, map[string]string{v1alpha1.AnnotationKeyHook: "PostDelete", v1alpha1.AnnotationKeyHookDeletePolicy: "HookSucceeded"})
	assert.Equal(t, []v1alpha1.HookDeletePolicy{v1alpha1.HookDeletePolicyHookSucceeded}, explicitDeletePolicies(succeeded))
}
