package compare

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	syncpkg "github.com/juliosaraiva/patrol-cd/internal/sync"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

// timer records phase durations.
type timer struct {
	now   func() time.Time
	start time.Time
	last  time.Time
	out   []Timing
}

func newTimer(now func() time.Time) *timer {
	t := now()
	return &timer{now: now, start: t, last: t}
}

func (t *timer) mark(phase string) {
	n := t.now()
	t.out = append(t.out, Timing{Phase: phase, Duration: n.Sub(t.last)})
	t.last = n
}

// Compare compares the desired state of an Application (rendered by the repo-server, or local
// manifests) with its live state (upstream CompareAppState). It loads as much as it can and
// reports what failed as conditions: a Result with sync status Unknown and every resource
// Unknown when desired or live state could not be loaded or compared.
//
// It returns an error, and no Result, when the input is invalid, the destination cannot be
// resolved, ctx ended, or a manifest generation error is deferred by the repo error grace
// period (ErrRepoErrorDeferred).
func (m *Manager) Compare(ctx context.Context, in Input) (*Result, error) {
	if in.App == nil || in.Project == nil {
		return nil, fmt.Errorf("%w: application and project are required", ErrInvalidInput)
	}
	app, proj := in.App, in.Project
	tm := newTimer(m.d.Now)
	log := m.log.With("application", app.QualifiedName(), "project", app.Spec.GetProject())

	sources := in.Sources
	if len(sources) == 0 {
		sources = app.Spec.GetSources()
	}
	hasMultiple := in.HasMultipleSources || app.Spec.HasMultipleSources()
	revisions := make([]string, len(sources))
	if len(in.Revisions) == len(sources) {
		copy(revisions, in.Revisions)
	}
	for i := range revisions {
		if revisions[i] == "" {
			revisions[i] = sources[i].TargetRevision
		}
	}

	res := &Result{
		SyncStatus: v1alpha1.SyncStatus{
			Status: v1alpha1.SyncStatusCodeUnknown,
			ComparedTo: v1alpha1.ComparedTo{
				Destination:       app.Spec.Destination,
				IgnoreDifferences: app.Spec.IgnoreDifferences,
			},
		},
		Health:                  v1alpha1.HealthStatusUnknown,
		EvaluatedConditionTypes: slices.Clone(comparisonConditionTypes),
	}
	if hasMultiple {
		res.SyncStatus.ComparedTo.Sources = slices.Clone(sources)
		res.SyncStatus.Revisions = slices.Clone(revisions)
	} else {
		if len(sources) > 0 {
			src := sources[0]
			res.SyncStatus.ComparedTo.Source = &src
		} else {
			log.Warn("compare: the application has no source")
		}
		if len(revisions) > 0 {
			res.SyncStatus.Revision = revisions[0]
		}
	}
	res.ResourceHealthSource = v1alpha1.ResourceHealthLocationAppTree
	if m.d.PersistResourceHealth {
		res.ResourceHealthSource = v1alpha1.ResourceHealthLocationInline
	}

	st := m.d.Settings.Current()
	tm.mark(PhaseSettings)
	if st == nil {
		log.Info("comparison settings cannot be loaded, using unknown comparison")
		res.ReconciledAt = metav1.NewTime(m.d.Now())
		res.Timings = tm.out
		return res, nil
	}

	cluster, err := m.d.Clusters.DestinationCluster(app.Spec.Destination)
	if err != nil {
		return nil, fmt.Errorf("failed to get destination cluster: %w", err)
	}
	res.Cluster = cluster
	destNS := app.Spec.Destination.Namespace
	method := st.TrackingMethod()
	installationID := st.InstallationID()
	t := m.tracker(st)
	appID := tracking.AppOf(app)
	instance := t.InstanceName(appID)
	appKey := app.Namespace + "/" + app.Name + "/" + string(app.UID)
	log.Debug("comparing app state", "server", cluster.Spec.Server, "namespace", destNS)

	cc, ccErr := m.d.LiveState.ClusterCache(ctx, cluster)
	if ccErr != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("compare %s: %w", app.QualifiedName(), ctx.Err())
	}
	scope := func(gk schema.GroupKind) (bool, error) {
		if cc == nil {
			return false, cache.ErrNotSynced
		}
		return cc.IsNamespaced(gk)
	}

	var conds []Condition
	failed := false
	fail := func(kind string, c Condition) {
		conds = append(conds, c)
		failed = true
		m.metrics.failure(kind)
	}

	// Desired state.
	var targets []*unstructured.Unstructured
	if len(in.LocalManifests) == 0 {
		env := &renderEnv{
			st: st, app: app, proj: proj, instanceName: instance, hasMultiple: hasMultiple,
			cc: cc, ccErr: ccErr, server: cluster.Spec.Server,
			noCache: in.NoCache, noRevCache: in.NoRevisionCache, evaluate: !in.RollbackOrSyncOverride,
		}
		ro, err := m.getRepoObjs(ctx, env, sources, slices.Clone(revisions))
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, fmt.Errorf("compare %s: %w", app.QualifiedName(), ctx.Err())
		case err != nil:
			if !in.RollbackOrSyncOverride && m.repoErrorDeferred(appKey, in.NoRevisionCache) {
				m.metrics.failure(failureDeferred)
				log.Debug("ignoring repo error within the grace period", "error", err)
				return nil, fmt.Errorf("%w: %w", ErrRepoErrorDeferred, err)
			}
			fail(failureManifests, comparisonError(v1alpha1.ReasonManifestGenerationFail, "Failed to load target state: "+err.Error()))
		default:
			m.clearRepoError(appKey)
			targets = ro.targets
			res.ManifestResponses = ro.responses
			for _, r := range ro.responses {
				res.Revisions = append(res.Revisions, r.Revision)
			}
			res.RevisionsMayHaveChanges = ro.mayHaveChanges
		}
	} else {
		si, _ := project.EffectiveSourceIntegrity(proj)
		if project.HasSourceIntegrityCriteria(si, m.d.GPGEnabled, sources...) {
			fail(failureManifests, comparisonError(v1alpha1.ReasonComparisonFailed, "Cannot use local manifests when source integrity is enforced"))
		} else if len(in.LocalManifests) > maxTargetObjects {
			fail(failureManifests, comparisonError(v1alpha1.ReasonComparisonFailed,
				fmt.Sprintf("Failed to load local manifests: more than %d manifests", maxTargetObjects)))
		} else if objs, err := unmarshalManifests(in.LocalManifests); err != nil {
			fail(failureManifests, comparisonError(v1alpha1.ReasonComparisonFailed, "Failed to load local manifests: "+err.Error()))
		} else {
			targets = objs
		}
	}
	tm.mark(PhaseManifests)

	// Target normalization, de-duplication and exclusions.
	targets, dedup, err := normalizeTargets(destNS, targets, scope, func(u *unstructured.Unstructured) error {
		return t.SetAppInstance(u, instance, destNS, method, installationID)
	})
	if err != nil {
		// Upstream keeps comparing without targets; Patrol marks the comparison as failed so
		// nothing looks extraneous.
		fail(failureNormalize, comparisonError(v1alpha1.ReasonComparisonFailed, "Failed to normalize target state: "+err.Error()))
	}
	conds = append(conds, dedup...)
	filter := st.ResourceFilter()
	kept := targets[:0:0]
	targetNSExists := false
	for _, obj := range targets {
		if isManagedNamespace(obj, app) {
			targetNSExists = true
		}
		gvk := obj.GroupVersionKind()
		if filter.IsExcludedResource(gvk.Group, gvk.Kind, cluster.Spec.Server) {
			conds = append(conds, newCondition(v1alpha1.ApplicationConditionExcludedResourceWarning, v1alpha1.ReasonExcludedResource,
				fmt.Sprintf("Resource %s/%s %s is excluded in the settings", gvk.Group, gvk.Kind, obj.GetName())))
			continue
		}
		kept = append(kept, obj)
	}
	targets = kept
	tm.mark(PhaseDedup)

	// Live state.
	live := map[ResourceKey]*unstructured.Unstructured{}
	if ccErr != nil {
		fail(failureLive, comparisonError(v1alpha1.ReasonComparisonFailed,
			fmt.Sprintf("Failed to load live state: failed to get cluster info for %q: %s", cluster.Spec.Server, ccErr.Error())))
	} else {
		objs, err := cc.GetManagedLiveObjs(ctx, targets, func(r *cache.Resource) bool { return InfoOf(r).AppName == instance })
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, fmt.Errorf("compare %s: %w", app.QualifiedName(), ctx.Err())
		case err != nil:
			fail(failureLive, comparisonError(v1alpha1.ReasonComparisonFailed, "Failed to load live state: "+err.Error()))
		default:
			live = objs
		}
	}
	liveKeys := sortedKeys(live)
	for _, k := range liveKeys {
		ok, err := project.IsLiveResourcePermitted(ctx, proj, live[k], cluster, m.d.Clusters)
		if err != nil {
			fail(failureProject, comparisonError(v1alpha1.ReasonComparisonFailed, project.LiveResourceCheckErrorMessage(k.String(), app.Spec.Project, err)))
			continue
		}
		if !ok {
			delete(live, k)
		}
	}
	for _, k := range sortedKeys(live) {
		obj := live[k]
		if other, shared := t.DetectSharedResource(obj, appID, method, installationID); shared {
			conds = append(conds, newCondition(v1alpha1.ApplicationConditionSharedResourceWarning, v1alpha1.ReasonSharedResource,
				tracking.SharedResourceMessage(obj, appID, other)))
		}
		if isManagedNamespace(obj, app) && !targetNSExists {
			ns, err := managedNamespaceTarget(obj.GetName(), app.Spec.SyncPolicy.ManagedNamespaceMetadata)
			if err != nil {
				fail(failureNormalize, comparisonError(v1alpha1.ReasonComparisonFailed, err.Error()))
				continue
			}
			targets = append(targets, ns)
		}
	}
	syncTargets, hasPre, hasPost := syncpkg.PartitionTargets(targets)
	res.HasPreDeleteHooks, res.HasPostDeleteHooks = hasPre, hasPost
	res.Targets = syncTargets
	res.Live = maps.Clone(live)
	rec := reconcile(syncTargets, maps.Clone(live), destNS, scope)
	tm.mark(PhaseLive)

	// Diff.
	ds := m.diffSetup(ctx, st, app, cluster, t, method, rec.Target, log)
	conds = append(conds, ds.conditions...)
	res.Normalizer = ds.normalizer
	res.ServerSideDiff = ds.opts.ServerSide
	res.diffOpts = ds.opts
	diffs := make([]diff.Result, len(rec.Target))
	if ds.err != nil {
		fail(failureDiff, comparisonError(v1alpha1.ReasonComparisonFailed, "Failed to compare desired state to live state: "+ds.err.Error()))
	} else if len(rec.Target) > 0 {
		list, err := diff.DiffArray(ctx, rec.Target, rec.Live, ds.opts)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, fmt.Errorf("compare %s: %w", app.QualifiedName(), ctx.Err())
		case err != nil:
			fail(failureDiff, comparisonError(v1alpha1.ReasonComparisonFailed, "Failed to compare desired state to live state: failed to calculate diff: "+err.Error()))
		default:
			copy(diffs, list.Diffs)
		}
	}
	tm.mark(PhaseDiff)

	// Resource and sync status.
	syncCode := v1alpha1.SyncStatusCodeSynced
	managed := make([]ManagedResource, 0, len(rec.Target))
	statuses := make([]v1alpha1.ResourceStatus, 0, len(rec.Target))
	res.DiffResults = make(map[ResourceKey]bool, len(rec.Target))
	for i, target := range rec.Target {
		liveObj := rec.Live[i]
		obj := liveObj
		if obj == nil {
			obj = target
		}
		if obj == nil {
			continue
		}
		gvk := obj.GroupVersionKind()
		selfRef := t.IsSelfReferencedObject(liveObj, target, method, installationID)
		rs := v1alpha1.ResourceStatus{
			Group:                        gvk.Group,
			Version:                      gvk.Version,
			Kind:                         gvk.Kind,
			Namespace:                    obj.GetNamespace(),
			Name:                         obj.GetName(),
			Hook:                         isHookObject(obj),
			RequiresPruning:              target == nil && liveObj != nil && selfRef,
			RequiresDeletionConfirmation: requiresDeletionConfirmation(target, app) || requiresDeletionConfirmation(liveObj, app),
		}
		switch {
		case target != nil:
			rs.SyncWave = int64(syncpkg.Wave(target))
		case rs.Hook:
			for _, h := range rec.Hooks {
				if h.GetName() == liveObj.GetName() && h.GetKind() == liveObj.GetKind() && h.GetNamespace() == liveObj.GetNamespace() {
					rs.SyncWave = int64(syncpkg.Wave(h))
					break
				}
			}
		}
		dr := diffs[i]
		if dr.NormalizedLive == nil && dr.PredictedLive == nil {
			dr = diff.Result{NormalizedLive: []byte("{}"), PredictedLive: []byte("{}")}
		}
		managedNSPlaceholder := isManagedNamespace(target, app) && liveObj == nil
		switch {
		case rs.Hook || syncpkg.IsIgnored(obj) || (target != nil && syncpkg.IsSkip(target)) || !selfRef:
			// Hooks, skipped targets and objects carrying a copied tracking id have no
			// sync status and do not affect the application's.
		case !managedNSPlaceholder && (dr.Modified || target == nil || liveObj == nil):
			rs.Status = v1alpha1.SyncStatusCodeOutOfSync
			extraneous := target == nil && liveObj != nil
			if !extraneous || !diff.ParseCompareAnnotation(obj.GetAnnotations(), v1alpha1.AnnotationKeyCompareOptions).IgnoreExtraneous {
				syncCode = v1alpha1.SyncStatusCodeOutOfSync
			}
		default:
			rs.Status = v1alpha1.SyncStatusCodeSynced
		}
		namespaced, nsErr := scope(gvk.GroupKind())
		namespaced = namespaced && nsErr == nil
		if !project.IsGroupKindNamePermitted(proj, gvk.GroupKind(), obj.GetName(), namespaced) {
			rs.Status = v1alpha1.SyncStatusCodeUnknown
		}
		if namespaced && obj.GetNamespace() == "" {
			conds = append(conds, newCondition(v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonInvalidSpec,
				project.MissingNamespaceMessage(obj.GetName(), gvk)))
		}
		if failed {
			rs.Status = v1alpha1.SyncStatusCodeUnknown
		}
		key := ResourceKey{Group: rs.Group, Kind: rs.Kind, Namespace: rs.Namespace, Name: rs.Name}
		mr := ManagedResource{
			Key:                          key,
			Version:                      rs.Version,
			Target:                       target,
			Live:                         liveObj,
			Diff:                         dr,
			Hook:                         rs.Hook,
			SelfReferenced:               selfRef,
			RequiresPruning:              rs.RequiresPruning,
			RequiresDeletionConfirmation: rs.RequiresDeletionConfirmation,
			SyncWave:                     int(rs.SyncWave),
			Status:                       rs.Status,
		}
		if liveObj != nil {
			mr.ResourceVersion = liveObj.GetResourceVersion()
		}
		managed = append(managed, mr)
		statuses = append(statuses, rs)
		res.DiffResults[key] = dr.Modified
	}
	switch {
	case failed:
		syncCode = v1alpha1.SyncStatusCodeUnknown
	case changedManagedNamespaceMetadata(app, in.LastSyncResult):
		syncCode = v1alpha1.SyncStatusCodeOutOfSync
	}
	res.SyncStatus.Status = syncCode
	if len(res.ManifestResponses) > 0 || len(in.LocalManifests) > 0 {
		revs := make([]string, 0, len(res.ManifestResponses))
		for _, r := range res.ManifestResponses {
			revs = append(revs, r.Revision)
		}
		if hasMultiple {
			res.SyncStatus.Revisions = revs
		} else if len(revs) > 0 {
			res.SyncStatus.Revision = revs[0]
		}
	}
	res.SyncStatus.ShortRevision = shortRevision(res.SyncStatus)
	tm.mark(PhaseSync)

	// Health.
	h, herr := applicationHealth(app, managed, statuses, m.luaRuntime(st), m.d.PersistResourceHealth)
	res.Health = h
	if herr != nil {
		m.metrics.failure(failureHealth)
		log.Warn("resource health assessment failed", "error", herr)
		conds = append(conds, comparisonError(v1alpha1.ReasonComparisonFailed, "error setting app health: "+herr.Error()))
	}
	for _, r := range res.ManifestResponses {
		if r != nil && !r.SourceIntegrityResult.IsValid() {
			conds = append(conds, comparisonError(v1alpha1.ReasonComparisonFailed, integrityError(r.SourceIntegrityResult)))
		}
	}
	if hasMultiple {
		for _, r := range res.ManifestResponses {
			res.SourceTypes = append(res.SourceTypes, v1alpha1.ApplicationSourceType(r.SourceType))
		}
	} else if len(res.ManifestResponses) > 0 {
		res.SourceType = v1alpha1.ApplicationSourceType(res.ManifestResponses[0].SourceType)
	}
	res.Managed = managed
	res.Resources, res.ResourcesTruncated = boundResources(statuses, m.d.MaxResources)
	if res.ResourcesTruncated > 0 {
		conds = append(conds, newCondition(ConditionResourcesTruncated, ReasonResourcesTruncated,
			fmt.Sprintf("status.resources lists %d of %d resources", len(res.Resources), len(statuses))))
	}
	res.FailedToLoad = failed
	tm.mark(PhaseHealth)

	// Resource tree.
	if !in.SkipTree {
		if cc == nil {
			res.TreeError = fmt.Errorf("failed to get cluster info for %q: %w", cluster.Spec.Server, ccErr)
		} else {
			tr := m.buildTree(ctx, cc, app, proj, cluster, managed, st.AllowedNodeLabels())
			res.Tree, res.Summary, res.OrphanedResources = tr.Tree, tr.Summary, tr.OrphanedResources
			conds = append(conds, tr.Conditions...)
			res.EvaluatedConditionTypes = append(res.EvaluatedConditionTypes, tr.EvaluatedConditionTypes...)
		}
		if res.TreeError != nil {
			m.metrics.failure(failureTree)
			log.Warn("failed to build the resource tree", "error", res.TreeError)
		}
		tm.mark(PhaseTree)
	}

	if err := ctx.Err(); err != nil {
		// Late cancellation can leave a partial tree or health; report nothing.
		return nil, fmt.Errorf("compare %s: %w", app.QualifiedName(), err)
	}
	res.Conditions = conds
	res.ReconciledAt = metav1.NewTime(m.d.Now())
	res.Timings = tm.out
	m.observe(res, len(in.LocalManifests) > 0, tm, log)
	return res, nil
}

func (m *Manager) observe(res *Result, local bool, tm *timer, log *slog.Logger) {
	total := tm.last.Sub(tm.start)
	m.metrics.duration.WithLabelValues(strconv.FormatBool(local)).Observe(total.Seconds())
	m.metrics.results.WithLabelValues(string(res.SyncStatus.Status)).Inc()
	attrs := []any{"sync", res.SyncStatus.Status, "health", res.Health, "resources", len(res.Managed), "time_ms", total.Milliseconds()}
	for _, t := range res.Timings {
		m.metrics.phase.WithLabelValues(t.Phase).Observe(t.Duration.Seconds())
		attrs = append(attrs, t.Phase+"_ms", t.Duration.Milliseconds())
	}
	log.Debug("compared app state", attrs...)
}

func sortedKeys(m map[ResourceKey]*unstructured.Unstructured) []ResourceKey {
	return slices.SortedFunc(maps.Keys(m), func(a, b ResourceKey) int { return strings.Compare(a.String(), b.String()) })
}

// shortRevision abbreviates the compared revision(s) for the printer column: 7 characters of
// a hexadecimal commit SHA, other revisions (chart versions, digests) unchanged, " +N" for
// additional sources.
func shortRevision(s v1alpha1.SyncStatus) string {
	revs := s.Revisions
	if len(revs) == 0 && s.Revision != "" {
		revs = []string{s.Revision}
	}
	if len(revs) == 0 {
		return ""
	}
	short := revs[0]
	if isHexSHA(short) {
		short = short[:7]
	}
	if len(revs) > 1 {
		short += fmt.Sprintf(" +%d", len(revs)-1)
	}
	return short
}

func isHexSHA(s string) bool {
	if len(s) <= 7 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
