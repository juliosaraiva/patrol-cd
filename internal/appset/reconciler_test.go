package appset

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func TestReconcileCreatesApplications(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, tenantNS, "set", elem("a", "dev"), elem("b", "prod"))
	as.Spec.Template.Namespace = "elsewhere"
	c := newFakeClient(t, as, appProject("default", func(p *v1alpha1.AppProject) { p.Spec.SourceNamespaces = []string{"team-*"} }))
	r, rec := newTestReconciler(t, c, nil)
	reconcileOnce(t, r, as)

	apps := listApps(t, c, tenantNS)
	require.Len(t, apps, 2)
	a := apps["a"]
	assert.Equal(t, "dev", a.Labels["env"])
	assert.Equal(t, []string{v1alpha1.FinalizerResources}, a.Finalizers)
	owner := metav1.GetControllerOf(&a)
	require.NotNil(t, owner)
	assert.Equal(t, "ApplicationSet", owner.Kind)
	assert.Equal(t, "set", owner.Name)
	assert.True(t, *owner.BlockOwnerDeletion)
	assert.Empty(t, listApps(t, c, "elsewhere"), "template namespace is ignored")

	got := getAppSet(t, c, tenantNS, "set")
	assert.Equal(t, []string{
		"ErrorOccurred|False|ApplicationSetUpToDate|All applications have been generated successfully",
		"ParametersGenerated|True|ParametersGenerated|Successfully generated parameters for all Applications",
		"ResourcesUpToDate|True|ApplicationSetUpToDate|All applications have been generated successfully",
	}, conditionSummary(got))
	assert.Equal(t, v1alpha1.HealthStatusHealthy, got.Status.Health.Status)
	assert.EqualValues(t, 2, got.Status.ResourcesCount)
	require.Len(t, got.Status.Resources, 2)
	assert.Equal(t, v1alpha1.ResourceStatus{Group: v1alpha1.GroupName, Version: "v1alpha1", Kind: "Application", Name: "a", Namespace: tenantNS,
		Health: &v1alpha1.HealthStatus{}}, got.Status.Resources[0])
	assert.ElementsMatch(t, []string{`Normal created Create created Application "a"`, `Normal created Create created Application "b"`}, rec.list())

	// A second reconcile without change writes nothing.
	before := listApps(t, c, tenantNS)["a"].ResourceVersion
	reconcileOnce(t, r, got)
	assert.Equal(t, before, listApps(t, c, tenantNS)["a"].ResourceVersion)
	assert.Len(t, rec.list(), 2, "no event for unchanged Applications")
	lt := findCondition(got, v1alpha1.ApplicationSetConditionResourcesUpToDate).LastTransitionTime
	again := getAppSet(t, c, tenantNS, "set")
	assert.True(t, lt.Equal(&findCondition(again, v1alpha1.ApplicationSetConditionResourcesUpToDate).LastTransitionTime))
}

func TestReconcilePolicies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		policy          v1alpha1.ApplicationsSyncPolicy
		updated, pruned bool
	}{
		{v1alpha1.ApplicationsSyncPolicyCreateOnly, false, false},
		{v1alpha1.ApplicationsSyncPolicyCreateUpdate, true, false},
		{v1alpha1.ApplicationsSyncPolicyCreateDelete, false, true},
		{v1alpha1.ApplicationsSyncPolicySync, true, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.policy), func(t *testing.T) {
			t.Parallel()
			as := listAppSet(t, controlNS, "set", elem("a", "dev"), elem("b", "dev"))
			as.Spec.SyncPolicy = &v1alpha1.ApplicationSetSyncPolicy{ApplicationsSync: ptr.To(tt.policy)}
			c := newFakeClient(t, as, appProject("default"))
			r, _ := newTestReconciler(t, c, func(cfg *Config) { cfg.EnablePolicyOverride = true })
			reconcileOnce(t, r, as)
			require.Len(t, listApps(t, c, controlNS), 2, "every policy creates")

			// Template change and element removal.
			as = getAppSet(t, c, controlNS, "set")
			as.Spec.Template.Spec.Source.TargetRevision = "v2"
			as.Spec.Generators[0].List.Elements = jsonList(t, elem("a", "dev"))
			require.NoError(t, c.Update(context.Background(), as))
			reconcileOnce(t, r, as)

			apps := listApps(t, c, controlNS)
			b, bExists := apps["b"]
			// The fake API server keeps an object with finalizers, marked for deletion.
			assert.Equal(t, !tt.pruned, bExists && b.DeletionTimestamp == nil, "deleted")
			wantRev := "HEAD"
			if tt.updated {
				wantRev = "v2"
			}
			assert.Equal(t, wantRev, apps["a"].Spec.Source.TargetRevision, "updated")
		})
	}
}

func TestReconcilePolicyOverrideDisabled(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("a", "dev"))
	as.Spec.SyncPolicy = &v1alpha1.ApplicationSetSyncPolicy{ApplicationsSync: ptr.To(v1alpha1.ApplicationsSyncPolicySync)}
	existing := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: controlNS}}
	existing.Spec.Project = "default"
	existing.Spec.Source = &v1alpha1.ApplicationSource{RepoURL: "old"}
	c := newFakeClient(t, as, appProject("default"))
	r, _ := newTestReconciler(t, c, func(cfg *Config) {
		cfg.Policy = v1alpha1.ApplicationsSyncPolicyCreateOnly
		cfg.EnablePolicyOverride = false
	})
	reconcileOnce(t, r, as)
	as = getAppSet(t, c, controlNS, "set")
	as.Spec.Template.Spec.Source.Path = "changed"
	require.NoError(t, c.Update(context.Background(), as))
	reconcileOnce(t, r, as)
	assert.Equal(t, "apps/a", listApps(t, c, controlNS)["a"].Spec.Source.Path, "controller create-only wins without override")
}

func TestReconcileAdoptionAndConflict(t *testing.T) {
	t.Parallel()
	unowned := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: controlNS, Labels: map[string]string{"stale": "x"}}}
	unowned.Spec.Project = "default"
	first := listAppSet(t, controlNS, "first", elem("a", "dev"))
	second := listAppSet(t, controlNS, "second", elem("a", "prod"))
	c := newFakeClient(t, unowned, first, second, appProject("default"))
	r, _ := newTestReconciler(t, c, nil)
	reconcileOnce(t, r, first)
	app := listApps(t, c, controlNS)["a"]
	assert.Equal(t, "first", metav1.GetControllerOf(&app).Name, "unowned Application adopted")
	assert.Equal(t, map[string]string{"env": "dev"}, app.Labels)

	_, err := r.Reconcile(context.Background(), requestFor(second))
	require.Error(t, err)
	var owned *controllerutil.AlreadyOwnedError
	require.ErrorAs(t, err, &owned)
	got := getAppSet(t, c, controlNS, "second")
	cond := findCondition(got, v1alpha1.ApplicationSetConditionErrorOccurred)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, v1alpha1.ApplicationSetReasonUpdateApplicationError, cond.Reason)
	app = listApps(t, c, controlNS)["a"]
	assert.Equal(t, "dev", app.Labels["env"], "the second ApplicationSet does not modify the Application")
}

func TestReconcileValidation(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("good", "dev"), elem("good", "prod"), elem("c", "dev"), elem("d", "dev"))
	as.Spec.Template.Spec.Project = "{{env}}"
	c := newFakeClient(t, as, appProject("dev"))
	r, _ := newTestReconciler(t, c, nil)
	res, err := r.Reconcile(context.Background(), requestFor(as))
	require.NoError(t, err)
	assert.Equal(t, RequeueOnValidationError, res.RequeueAfter)
	apps := listApps(t, c, controlNS)
	assert.Contains(t, apps, "good")
	assert.Contains(t, apps, "c")
	assert.Equal(t, "dev", apps["good"].Labels["env"], "the first of duplicate names is applied")
	got := getAppSet(t, c, controlNS, "set")
	cond := findCondition(got, v1alpha1.ApplicationSetConditionErrorOccurred)
	require.NotNil(t, cond)
	assert.Equal(t, v1alpha1.ApplicationSetReasonApplicationValidationError, cond.Reason)
	assert.Equal(t, "ApplicationSet set contains applications with duplicate name: good", cond.Message)
	assert.Equal(t, metav1.ConditionFalse, findCondition(got, v1alpha1.ApplicationSetConditionResourcesUpToDate).Status)

	// Three invalid Applications: the message of the last one plus "(and 2 more)".
	as = listAppSet(t, controlNS, "three", elem("x", "nope"), elem("y", "nope"), elem("z", "nope"))
	as.Spec.Template.Spec.Project = "{{env}}"
	require.NoError(t, c.Create(context.Background(), as))
	_, err = r.Reconcile(context.Background(), requestFor(as))
	require.NoError(t, err)
	got = getAppSet(t, c, controlNS, "three")
	assert.Equal(t, "application references project nope which does not exist (and 2 more)",
		findCondition(got, v1alpha1.ApplicationSetConditionErrorOccurred).Message)
}

func TestReconcileValidationDestination(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("unknown", "dev"), elem("denied", "dev"), elem("ok", "dev"))
	as.Spec.Template.Spec.Destination = v1alpha1.ApplicationDestination{Name: "{{name}}", Namespace: "apps"}
	proj := appProject("default", func(p *v1alpha1.AppProject) {
		p.Spec.Destinations = []v1alpha1.ApplicationDestination{{Name: "ok", Namespace: "*"}}
	})
	c := newFakeClient(t, as, proj, clusterObj("denied", "https://denied"), clusterObj("ok", "https://ok"))
	r, _ := newTestReconciler(t, c, nil)
	reconcileOnce(t, r, as)
	apps := listApps(t, c, controlNS)
	assert.Equal(t, []string{"ok"}, keys(apps))
	r2, _ := newTestReconciler(t, c, nil)
	invalid, validIdx, err := r2.validate(context.Background(), as, []v1alpha1.Application{
		appNamed(controlNS, "unknown", v1alpha1.ApplicationDestination{Name: "unknown", Namespace: "apps"}),
		appNamed(controlNS, "denied", v1alpha1.ApplicationDestination{Name: "denied", Namespace: "apps"}),
	})
	require.NoError(t, err)
	assert.Equal(t, []bool{false, false}, validIdx)
	assert.EqualError(t, invalid[controlNS+"/unknown"], "application destination spec is invalid: there are no clusters with this name: unknown")
	assert.EqualError(t, invalid[controlNS+"/denied"], "application destination server 'denied' and namespace 'apps' do not match any of the allowed destinations in project 'default'")
}

// A tenant ApplicationSet cannot generate Applications in a project that does not list its
// namespace in sourceNamespaces, even by templating the project.
func TestReconcileTemplatedProjectEscalationBlocked(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, tenantNS, "set", elem("a", "tenant"), elem("b", "admin"))
	as.Spec.Template.Spec.Project = "{{env}}"
	tenant := appProject("tenant", func(p *v1alpha1.AppProject) { p.Spec.SourceNamespaces = []string{tenantNS} })
	admin := appProject("admin")
	c := newFakeClient(t, as, tenant, admin)
	r, _ := newTestReconciler(t, c, nil)
	reconcileOnce(t, r, as)
	apps := listApps(t, c, tenantNS)
	assert.Equal(t, []string{"a"}, keys(apps))
	got := getAppSet(t, c, tenantNS, "set")
	cond := findCondition(got, v1alpha1.ApplicationSetConditionErrorOccurred)
	require.NotNil(t, cond)
	assert.Equal(t, v1alpha1.ApplicationSetReasonApplicationValidationError, cond.Reason)
	assert.Equal(t, "application 'b' in namespace 'team-a' is not permitted to use project 'admin'", cond.Message)

	// The same ApplicationSet in the control-plane namespace may use any project.
	cp := listAppSet(t, controlNS, "cp", elem("a", "tenant"), elem("b", "admin"))
	cp.Spec.Template.Spec.Project = "{{env}}"
	require.NoError(t, c.Create(context.Background(), cp))
	reconcileOnce(t, r, cp)
	assert.Len(t, listApps(t, c, controlNS), 2)
}

func TestReconcileIgnoredNamespace(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, "other", "set", elem("a", "dev"))
	c := newFakeClient(t, as, appProject("default"))
	r, _ := newTestReconciler(t, c, nil)
	reconcileOnce(t, r, as)
	assert.Empty(t, listApps(t, c, "other"))
	assert.Empty(t, getAppSet(t, c, "other", "set").Status.Conditions)
}

func TestReconcilePreservedFields(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("a", "dev"))
	as.Spec.PreservedFields = &v1alpha1.ApplicationPreservedFields{Annotations: []string{"keep-ann"}, Labels: []string{"env"}}
	as.Spec.Template.Annotations = map[string]string{"keep-ann": "template"}
	c := newFakeClient(t, as, appProject("default"))
	r, _ := newTestReconciler(t, c, func(cfg *Config) { cfg.GlobalPreservedLabels = []string{"global"} })
	reconcileOnce(t, r, as)

	app := listApps(t, c, controlNS)["a"]
	app.Annotations["keep-ann"] = "user"
	app.Annotations["drop-ann"] = "user"
	app.Annotations[v1alpha1.AnnotationKeyRefresh] = "hard"
	app.Labels["env"] = "user"
	app.Labels["global"] = "user"
	app.Labels["drop"] = "user"
	app.Finalizers = append(app.Finalizers, v1alpha1.FinalizerPostDeleteCleanup, "foo")
	require.NoError(t, c.Update(context.Background(), &app))
	reconcileOnce(t, r, as)

	app = listApps(t, c, controlNS)["a"]
	assert.Equal(t, map[string]string{"keep-ann": "user", v1alpha1.AnnotationKeyRefresh: "hard"}, app.Annotations)
	assert.Equal(t, map[string]string{"env": "user", "global": "user"}, app.Labels)
	assert.Equal(t, []string{v1alpha1.FinalizerResources, v1alpha1.FinalizerPostDeleteCleanup}, app.Finalizers)
}

func TestReconcileIgnoreApplicationDifferences(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("a", "dev"), elem("b", "dev"))
	as.Spec.Template.Spec.SyncPolicy = &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{Prune: ptr.To(true)}}
	as.Spec.IgnoreApplicationDifferences = v1alpha1.ApplicationSetIgnoreDifferences{{Name: "a", JSONPointers: []string{"/spec/syncPolicy"}}}
	c := newFakeClient(t, as, appProject("default"))
	r, _ := newTestReconciler(t, c, nil)
	reconcileOnce(t, r, as)
	for _, name := range []string{"a", "b"} {
		app := listApps(t, c, controlNS)[name]
		app.Spec.SyncPolicy = nil
		require.NoError(t, c.Update(context.Background(), &app))
	}
	reconcileOnce(t, r, as)
	apps := listApps(t, c, controlNS)
	assert.Nil(t, apps["a"].Spec.SyncPolicy, "ignored field keeps the live value")
	require.NotNil(t, apps["b"].Spec.SyncPolicy, "the named rule does not apply to b")

	// An invalid jq expression fails the reconcile.
	as = getAppSet(t, c, controlNS, "set")
	as.Spec.IgnoreApplicationDifferences = v1alpha1.ApplicationSetIgnoreDifferences{{JQPathExpressions: []string{"{{"}}}
	require.NoError(t, c.Update(context.Background(), as))
	_, err := r.Reconcile(context.Background(), requestFor(as))
	require.ErrorContains(t, err, "failed to build ignore diff config")
	got := getAppSet(t, c, controlNS, "set")
	assert.Equal(t, v1alpha1.HealthStatusDegraded, got.Status.Health.Status)
}

func TestReconcileRefreshAnnotationAndRequeue(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("a", "dev"))
	as.Annotations = map[string]string{v1alpha1.AnnotationKeyApplicationSetRefresh: "true"}
	c := newFakeClient(t, as, appProject("default"))
	r, _ := newTestReconciler(t, c, nil)
	res, err := r.Reconcile(context.Background(), requestFor(as))
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter, "list generators do not poll")
	got := getAppSet(t, c, controlNS, "set")
	assert.NotContains(t, got.Annotations, v1alpha1.AnnotationKeyApplicationSetRefresh)
}

// TestReconcileRefreshAnnotationConflict: when the ApplicationSet changed since the pass
// read it, the failed optimistic removal of the refresh annotation requeues without an error
// or an ErrorOccurred condition (integration finding I-4: a pass reading a stale cached
// object logged "Reconciler error" and flagged RefreshApplicationError).
func TestReconcileRefreshAnnotationConflict(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("a", "dev"))
	as.Annotations = map[string]string{v1alpha1.AnnotationKeyApplicationSetRefresh: "true"}
	base := newFakeClient(t, as, appProject("default"))
	conflicts := 0
	c := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*v1alpha1.ApplicationSet); ok && conflicts == 0 {
				conflicts++
				return apierrors.NewConflict(schema.GroupResource{Group: v1alpha1.GroupName, Resource: "applicationsets"}, obj.GetName(), errors.New("the object has been modified"))
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})
	r, _ := newTestReconciler(t, c, nil)
	res, err := r.Reconcile(context.Background(), requestFor(as))
	require.NoError(t, err)
	assert.Equal(t, conflictRequeueDelay, res.RequeueAfter, "a conflict requeues")
	assert.Equal(t, 1, conflicts)
	got := getAppSet(t, base, controlNS, "set")
	for _, cond := range got.Status.Conditions {
		if cond.Type == v1alpha1.ApplicationSetConditionErrorOccurred {
			assert.NotEqual(t, metav1.ConditionTrue, cond.Status, "no error condition for a conflict: %s", cond.Message)
		}
	}
	// The requeued pass removes the annotation.
	res, err = r.Reconcile(context.Background(), requestFor(as))
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)
	assert.NotContains(t, getAppSet(t, base, controlNS, "set").Annotations, v1alpha1.AnnotationKeyApplicationSetRefresh)
}

func TestReconcileGenerationError(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("a", "dev"))
	as.Spec.Generators = append(as.Spec.Generators, v1alpha1.ApplicationSetGenerator{Git: &v1alpha1.GitGenerator{RepoURL: "https://x", Revision: "HEAD"}})
	as.Annotations = map[string]string{v1alpha1.AnnotationKeyApplicationSetRefresh: "true"}
	c := newFakeClient(t, as, appProject("default"))
	r, _ := newTestReconciler(t, c, nil)
	res, err := r.Reconcile(context.Background(), requestFor(as))
	require.NoError(t, err)
	assert.Equal(t, RequeueOnValidationError, res.RequeueAfter)
	assert.Empty(t, listApps(t, c, controlNS), "a failing generator prevents every write")
	got := getAppSet(t, c, controlNS, "set")
	assert.Contains(t, got.Annotations, v1alpha1.AnnotationKeyApplicationSetRefresh, "kept until a successful pass")
	assert.Equal(t, metav1.ConditionFalse, findCondition(got, v1alpha1.ApplicationSetConditionParametersGenerated).Status)
	assert.Equal(t, v1alpha1.ApplicationSetReasonApplicationParamsGenerationError, findCondition(got, v1alpha1.ApplicationSetConditionErrorOccurred).Reason)
}

func TestReconcileResourcesStatusLimit(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("a", "dev"), elem("b", "dev"), elem("c", "dev"))
	c := newFakeClient(t, as, appProject("default"))
	r, _ := newTestReconciler(t, c, func(cfg *Config) { cfg.MaxResourcesStatusCount = 2 })
	reconcileOnce(t, r, as)
	got := getAppSet(t, c, controlNS, "set")
	assert.EqualValues(t, 3, got.Status.ResourcesCount)
	require.Len(t, got.Status.Resources, 2)
	assert.Equal(t, "b", got.Status.Resources[1].Name)
}

func TestReconcileDeleteInvalidDestination(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("a", "dev"), elem("gone", "dev"))
	as.Spec.Template.Spec.Destination = v1alpha1.ApplicationDestination{Name: "{{name}}", Namespace: "x"}
	c := newFakeClient(t, as, appProject("default"), clusterObj("a", "https://a"), clusterObj("gone", "https://gone"))
	r, rec := newTestReconciler(t, c, nil)
	reconcileOnce(t, r, as)
	require.Len(t, listApps(t, c, controlNS), 2)

	// The cluster disappears and the element is removed: the Application is deleted without
	// waiting for its resources finalizer.
	gone := clusterObj("gone", "https://gone")
	require.NoError(t, c.Delete(context.Background(), gone))
	as = getAppSet(t, c, controlNS, "set")
	as.Spec.Generators[0].List.Elements = jsonList(t, elem("a", "dev"))
	require.NoError(t, c.Update(context.Background(), as))
	reconcileOnce(t, r, as)
	_, exists := listApps(t, c, controlNS)["gone"]
	assert.False(t, exists)
	assert.Contains(t, rec.list(), `Normal Updated Update Updated Application "gone" finalizer before deletion, because application has an invalid destination`)
	assert.Contains(t, rec.list(), `Normal Deleted Delete Deleted Application "gone"`)
	got := getAppSet(t, c, controlNS, "set")
	assert.EqualValues(t, 1, got.Status.ResourcesCount)
}

func TestReconcileDeletionPolicies(t *testing.T) {
	t.Parallel()
	t.Run("create-only releases Applications", func(t *testing.T) {
		t.Parallel()
		as := listAppSet(t, controlNS, "set", elem("a", "dev"))
		as.Finalizers = []string{v1alpha1.FinalizerApplicationSet}
		as.Spec.SyncPolicy = &v1alpha1.ApplicationSetSyncPolicy{ApplicationsSync: ptr.To(v1alpha1.ApplicationsSyncPolicyCreateOnly)}
		c := newFakeClient(t, as, appProject("default"))
		r, _ := newTestReconciler(t, c, func(cfg *Config) { cfg.EnablePolicyOverride = true })
		reconcileOnce(t, r, as)
		require.NoError(t, c.Delete(context.Background(), getAppSet(t, c, controlNS, "set")))
		reconcileOnce(t, r, as)
		app := listApps(t, c, controlNS)["a"]
		assert.Empty(t, app.OwnerReferences)
		assertGone(t, c, as)
	})
	t.Run("preserveResourcesOnDeletion strips finalizers", func(t *testing.T) {
		t.Parallel()
		as := listAppSet(t, controlNS, "set", elem("a", "dev"))
		as.Spec.Template.Finalizers = []string{v1alpha1.FinalizerResourcesBackground, v1alpha1.FinalizerPostDelete}
		as.Spec.SyncPolicy = &v1alpha1.ApplicationSetSyncPolicy{PreserveResourcesOnDeletion: true}
		c := newFakeClient(t, as, appProject("default"))
		r, _ := newTestReconciler(t, c, nil)
		reconcileOnce(t, r, as)
		got := getAppSet(t, c, controlNS, "set")
		assert.Contains(t, got.Finalizers, v1alpha1.FinalizerApplicationSet, "finalizer added to act before garbage collection")
		assert.Equal(t, []string{v1alpha1.FinalizerResourcesBackground, v1alpha1.FinalizerPostDelete}, listApps(t, c, controlNS)["a"].Finalizers)

		require.NoError(t, c.Delete(context.Background(), got))
		reconcileOnce(t, r, as)
		app := listApps(t, c, controlNS)["a"]
		assert.Equal(t, []string{v1alpha1.FinalizerPostDelete}, app.Finalizers, "resources finalizers removed, delete hooks kept")
		assertGone(t, c, as)
	})
}

func assertGone(t *testing.T, c client.Client, as *v1alpha1.ApplicationSet) {
	t.Helper()
	err := c.Get(context.Background(), client.ObjectKeyFromObject(as), &v1alpha1.ApplicationSet{})
	require.Error(t, err, "the ApplicationSet is gone once its finalizer is removed")
}

func TestReconcileDryRun(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("a", "dev"))
	c := newFakeClient(t, as, appProject("default"))
	r, _ := newTestReconciler(t, c, func(cfg *Config) { cfg.DryRun = true })
	reconcileOnce(t, r, as)
	assert.Empty(t, listApps(t, c, controlNS))
	assert.Empty(t, getAppSet(t, c, controlNS, "set").Status.Conditions)
}

func TestReconcileProgressiveSyncsDisabledClearsStatus(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("a", "dev"))
	as.Spec.Strategy = rollingSync(v1alpha1.ApplicationSetRolloutStep{})
	c := newFakeClient(t, as, appProject("default"))
	stored := getAppSet(t, c, controlNS, "set")
	stored.Status.ApplicationStatus = []v1alpha1.ApplicationSetApplicationStatus{{Application: "a", Status: v1alpha1.ProgressiveSyncWaiting}}
	require.NoError(t, c.Status().Update(context.Background(), stored))
	r, _ := newTestReconciler(t, c, nil)
	reconcileOnce(t, r, as)
	got := getAppSet(t, c, controlNS, "set")
	assert.Empty(t, got.Status.ApplicationStatus)
	app := listApps(t, c, controlNS)["a"]
	assert.NotContains(t, app.Annotations, v1alpha1.AnnotationKeyApplicationSetProgressiveStatus)
}

func TestReconcileMissingAppSet(t *testing.T) {
	t.Parallel()
	c := newFakeClient(t)
	r, _ := newTestReconciler(t, c, nil)
	_, err := r.Reconcile(context.Background(), requestFor(listAppSet(t, controlNS, "missing")))
	require.NoError(t, err)
}

func TestReconcileConcurrentUpdatesDeterministic(t *testing.T) {
	t.Parallel()
	var elems []map[string]any
	for i := 0; i < 40; i++ {
		elems = append(elems, elem("app-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "dev"))
	}
	results := map[int][]string{}
	for _, conc := range []int{1, 10} {
		as := listAppSet(t, controlNS, "set", elems...)
		c := newFakeClient(t, as, appProject("default"))
		r, _ := newTestReconciler(t, c, func(cfg *Config) { cfg.ConcurrentApplicationUpdates = conc })
		reconcileOnce(t, r, as)
		results[conc] = keys(listApps(t, c, controlNS))
	}
	assert.Equal(t, results[1], results[10])
	assert.Len(t, results[1], 40)
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func appNamed(ns, name string, dest v1alpha1.ApplicationDestination) v1alpha1.Application {
	app := v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	app.Spec.Project = "default"
	app.Spec.Destination = dest
	return app
}
