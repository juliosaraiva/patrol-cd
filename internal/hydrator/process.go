package hydrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"golang.org/x/sync/errgroup"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/commitserver"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// LogValue describes the key without credentials (URLs are sanitized).
func (k Key) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("sourceRepoURL", git.SanitizeURL(k.SourceRepoURL)),
		slog.String("sourceTargetRevision", k.SourceTargetRevision),
		slog.String("destinationRepoURL", git.SanitizeURL(k.DestinationRepoURL)),
		slog.String("destinationBranch", k.DestinationBranch),
	)
}

// ReconcileKey is the manifest-hydration reconciler: it hydrates every Application sharing
// the key in one commit (APP-116). The workqueue never processes one key concurrently, so
// the group's statuses are owned by this call. Hydration failures are recorded in the
// Applications' status and are not returned (the app-hydration controller retries after
// the cooldown); errors returned are transient (listing, status persistence) and are
// retried with the controller's exponential backoff.
func (h *Hydrator) ReconcileKey(ctx context.Context, k Key) (reconcile.Result, error) {
	start := h.d.Now()
	log := h.log.With(slog.Any("hydrationKey", k))
	_, forced := h.forced.LoadAndDelete(k)
	reqs, covered := h.takeRequests(k)
	if covered && !forced {
		// Every request came from a state the latest processing already handled (typically
		// an evaluation racing with, or triggered by, that processing's status writes).
		log.Debug("hydrator: hydration key already processed")
		return reconcile.Result{}, nil
	}
	done := false
	defer func() {
		if !done {
			h.restoreRequests(k, reqs)
			if forced {
				h.forced.Store(k, struct{}{})
			}
		}
	}()

	apps, err := h.appsForKey(ctx, k)
	if err != nil {
		// We cannot tell which Applications to update; retry with backoff.
		return reconcile.Result{}, fmt.Errorf("failed to get apps for hydration: %w", err)
	}
	if len(apps) == 0 {
		log.Debug("hydrator: no application uses the hydration key")
		done = true
		h.forget(k)
		return reconcile.Result{}, nil
	}
	if h.d.Settings.Current() == nil {
		return reconcile.Result{}, errSettingsNotLoaded
	}
	log = log.With(slog.Int("appCount", len(apps)))
	seen := versions{}
	for _, app := range apps {
		seen.add(app)
	}
	drySHA := ""
	// specs are the specs this processing hydrates: those read by markHydrating, used for
	// validation and rendering. The status writes after the render may return a newer spec.
	specs := map[types.NamespacedName]string{}
	complete := func(err error) (reconcile.Result, error) {
		if err != nil {
			return reconcile.Result{}, err
		}
		for _, app := range apps {
			seen.add(app)
		}
		done = true
		h.completed(k, seen, specs, drySHA)
		return reconcile.Result{}, nil
	}

	apps, err = h.markHydrating(ctx, apps)
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(apps) == 0 {
		return complete(nil)
	}
	for _, app := range apps {
		seen.add(app)
		specs[types.NamespacedName{Namespace: app.Namespace, Name: app.Name}] = specDigest(app)
	}

	projects, verrs := h.validate(ctx, apps)
	if len(verrs) > 0 {
		for name, err := range verrs {
			log.Error("hydrator: failed to validate hydration app", slog.String("application", name), slog.Any("error", err))
		}
		err := h.fail(ctx, apps, verrs, "", seen)
		h.metrics.observeHydration(ResultFailed, len(apps), h.d.Now().Sub(start))
		return complete(err)
	}

	res := h.hydrate(ctx, log, apps, projects, forced)
	drySHA = res.drySHA
	if res.drySHA != "" {
		log = log.With(slog.String("drySha", res.drySHA))
	}
	if len(res.errs) > 0 {
		for name, err := range res.errs {
			log.Error("hydrator: failed to hydrate app", slog.String("application", name), slog.Any("error", err))
		}
		err := h.fail(ctx, apps, res.errs, res.drySHA, seen)
		h.metrics.observeHydration(ResultFailed, len(apps), h.d.Now().Sub(start))
		return complete(err)
	}
	result := ResultHydrated
	if res.skipped {
		result = ResultSkipped
	}
	err = h.succeed(ctx, log, apps, res.drySHA, res.hydratedSHA, seen)
	h.metrics.observeHydration(result, len(apps), h.d.Now().Sub(start))
	if err == nil {
		log.Info("hydrator: hydrated apps", slog.String("hydratedSha", res.hydratedSHA), slog.Bool("commitSkipped", res.skipped))
	}
	return complete(err)
}

// appsForKey lists the Applications sharing k, sorted by qualified name (the first one
// provides the dry revision and the sync branch, as upstream's first app).
func (h *Hydrator) appsForKey(ctx context.Context, k Key) ([]*v1alpha1.Application, error) {
	var list v1alpha1.ApplicationList
	if err := h.d.Client.List(ctx, &list, client.MatchingFields{keyIndexField: k.indexValue()}); err != nil {
		return nil, fmt.Errorf("failed to list apps: %w", err)
	}
	var out []*v1alpha1.Application
	for i := range list.Items {
		app := &list.Items[i]
		if !h.accept(app) {
			continue
		}
		if ak, ok := KeyOf(app); !ok || ak != k {
			continue
		}
		out = append(out, app.DeepCopy())
	}
	slices.SortFunc(out, func(a, b *v1alpha1.Application) int { return strings.Compare(a.QualifiedName(), b.QualifiedName()) })
	return out, nil
}

// markHydrating sets a Hydrating current operation (start time, spec snapshot) on every
// Application of the group that is not already Hydrating; Applications already Hydrating
// (stale recovery, duplicate enqueue) keep their start time and get the snapshot of their
// current spec, which is what is rendered now. Deleted Applications are dropped from the
// group.
func (h *Hydrator) markHydrating(ctx context.Context, apps []*v1alpha1.Application) ([]*v1alpha1.Application, error) {
	now := metav1.NewTime(h.d.Now())
	out := apps[:0]
	for _, app := range apps {
		err := h.patchSourceHydrator(ctx, app, func(a *v1alpha1.Application) {
			if a.Spec.SourceHydrator == nil {
				return
			}
			if cur := a.Status.SourceHydrator.CurrentOperation; cur != nil && cur.Phase == v1alpha1.HydrateOperationPhaseHydrating {
				// Nothing is written when the snapshot is already current.
				cur.SourceHydrator = a.Spec.SourceHydrator.DeepCopy()
				return
			}
			a.Status.SourceHydrator.CurrentOperation = &v1alpha1.HydrateOperation{
				StartedAt:      now.DeepCopy(),
				Phase:          v1alpha1.HydrateOperationPhaseHydrating,
				SourceHydrator: a.Spec.SourceHydrator.DeepCopy(),
			}
		})
		if errors.Is(err, errAppGone) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if app.Spec.SourceHydrator == nil {
			// The source hydrator was removed concurrently; the app left the group.
			continue
		}
		out = append(out, app)
	}
	return out, nil
}

// validate checks every Application of the group (APP-117). All must pass for the group to
// be hydrated. The returned projects are keyed by spec.project (nil when anything failed).
func (h *Hydrator) validate(ctx context.Context, apps []*v1alpha1.Application) (map[string]*v1alpha1.AppProject, map[string]error) {
	projects := map[string]*v1alpha1.AppProject{}
	errs := map[string]error{}
	type destKey struct{ repoURL, path string }
	seen := make(map[destKey]string, len(apps))
	for _, app := range apps {
		name := app.QualifiedName()
		proj, err := h.d.Projects.ResolveAppProject(ctx, app)
		if err != nil {
			errs[name] = fmt.Errorf("failed to get project %q: %w", app.Spec.Project, err)
			continue
		}
		dry := app.Spec.SourceHydrator.GetDrySource()
		if !project.IsSourcePermitted(proj, dry.RepoURL) {
			errs[name] = fmt.Errorf("application repo %s is not permitted in project '%s'", dry.RepoURL, proj.Name)
			continue
		}
		projects[app.Spec.Project] = proj
		dest := hydrateToSource(app.Spec.SourceHydrator)
		if isRootPath(dest.Path) {
			errs[name] = fmt.Errorf("app is configured to hydrate to the repository root (branch %q, path %q) which is not allowed", dest.TargetRevision, dest.Path)
			continue
		}
		if isOutsideRepository(dest.Path) {
			errs[name] = fmt.Errorf("app is configured to hydrate outside the repository (branch %q, path %q) which is not allowed", dest.TargetRevision, dest.Path)
			continue
		}
		if !project.IsSourcePermitted(proj, dest.RepoURL) {
			errs[name] = fmt.Errorf("destination repo %s is not permitted in project '%s'", dest.RepoURL, proj.Name)
			continue
		}
		dk := destKey{repoURL: dest.RepoURL, path: dest.Path}
		if other, ok := seen[dk]; ok {
			errs[name] = fmt.Errorf("app %s hydrator uses the same destination: repo=%s, path=%s", other, dk.repoURL, dk.path)
			errs[other] = fmt.Errorf("app %s hydrator uses the same destination: repo=%s, path=%s", name, dk.repoURL, dk.path)
			continue
		}
		seen[dk] = name
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return projects, nil
}

// hydrateResult is the outcome of hydrate.
type hydrateResult struct {
	drySHA      string
	hydratedSHA string
	// skipped is true when every Application already reported drySHA as hydrated with its
	// current configuration and the commit-server was not called.
	skipped bool
	// errs maps qualified names to errors; any entry fails the whole group.
	errs map[string]error
}

// hydrate renders every Application of the group at one dry SHA and commits the result.
func (h *Hydrator) hydrate(ctx context.Context, log *slog.Logger, apps []*v1alpha1.Application, projects map[string]*v1alpha1.AppProject, forced bool) hydrateResult {
	res := hydrateResult{errs: map[string]error{}}
	st := h.d.Settings.Current()
	first := apps[0]
	dest := hydrateToSource(first.Spec.SourceHydrator)
	// As upstream, the first app's sync branch is the base of a missing target branch; a
	// group shares the target branch, not necessarily the sync branch.
	syncBranch := first.Spec.SourceHydrator.SyncSource.TargetBranch
	dryRepoURL := first.Spec.SourceHydrator.DrySource.RepoURL

	// The first app is rendered at the dry targetRevision; its resolved SHA is used for
	// every other app, so the group is hydrated from one commit.
	firstPath, drySHA, err := h.render(ctx, st, first, "", projects[first.Spec.Project])
	if err != nil {
		res.errs[first.QualifiedName()] = fmt.Errorf("failed to get manifests: %w", err)
		return res
	}
	if !git.IsCommitSHA(drySHA) {
		res.errs[first.QualifiedName()] = fmt.Errorf("failed to get manifests: the repo-server resolved the dry revision to %q, which is not a commit SHA", drySHA)
		return res
	}
	res.drySHA = drySHA

	if !forced {
		if hydratedSHA, ok := alreadyHydrated(apps, drySHA); ok {
			log.Debug("hydrator: skipping hydration since the dry commit was already hydrated", slog.String("drySha", drySHA))
			res.hydratedSHA, res.skipped = hydratedSHA, true
			return res
		}
	}

	paths := make([]commitserver.PathDetails, len(apps))
	paths[0] = firstPath
	var mu sync.Mutex
	var g errgroup.Group
	g.SetLimit(renderParallelism)
	for i, app := range apps[1:] {
		g.Go(func() error {
			pd, _, err := h.render(ctx, st, app, drySHA, projects[app.Spec.Project])
			if err != nil {
				mu.Lock()
				res.errs[app.QualifiedName()] = fmt.Errorf("failed to get manifests: %w", err)
				mu.Unlock()
				return nil
			}
			paths[i+1] = pd
			return nil
		})
	}
	_ = g.Wait()
	if len(res.errs) > 0 {
		return res
	}

	// Project-scoped credentials only when the whole group belongs to one project;
	// otherwise only global credentials may be used (APP-125).
	projName := ""
	if len(projects) == 1 {
		for _, p := range projects {
			projName = p.Name
		}
	}
	groupErr := func(err error) hydrateResult {
		for _, app := range apps {
			res.errs[app.QualifiedName()] = err
		}
		return res
	}

	md, err := h.revisionMetadata(ctx, dryRepoURL, projName, drySHA)
	if err != nil {
		return groupErr(fmt.Errorf("failed to get revision metadata for %q: %w", drySHA, err))
	}
	repo, err := h.d.WriteCredentials.GetWriteRepository(ctx, dest.RepoURL, projName)
	if err != nil {
		return groupErr(fmt.Errorf("failed to get hydrator credentials: %w", err))
	}
	if repo == nil {
		log.Warn("hydrator: no credentials found for repo, continuing without credentials", slog.String("repo", git.SanitizeURL(dest.RepoURL)))
		repo = &apiclient.Repository{URL: dest.RepoURL}
	}
	if repo.URL == "" {
		repo.URL = dest.RepoURL
	}
	hs := st.Hydrator()
	commitMetadata := commitserver.NewCommitMetadata(log, dryRepoURL, drySHA, md)
	message, err := commitserver.RenderCommitMessage(hs.CommitMessageTemplate, commitMetadata)
	if err != nil {
		return groupErr(fmt.Errorf("failed to get hydrator commit templated message: %w", err))
	}
	resp, err := h.d.CommitServer.CommitHydratedManifests(ctx, &commitserver.CommitHydratedManifestsRequest{
		Repo:              *repo,
		SyncBranch:        syncBranch,
		TargetBranch:      dest.TargetRevision,
		DrySHA:            drySHA,
		CommitMessage:     message,
		Author:            commitserver.Author{Name: hs.CommitAuthorName, Email: hs.CommitAuthorEmail},
		ReadmeTemplate:    hs.ReadmeMessageTemplate,
		Paths:             paths,
		DryCommitMetadata: md,
		DryRepoURL:        dryRepoURL,
	})
	if err != nil {
		return groupErr(fmt.Errorf("failed to commit hydrated manifests: %w", err))
	}
	res.hydratedSHA = resp.HydratedSHA
	return res
}

// alreadyHydrated reports whether every Application's last successful hydration used drySHA
// with the configuration being hydrated now, all into the same commit. Upstream inspects
// only the first app; checking all of them means an added Application or a changed
// syncSource.path is written without waiting for a new dry commit.
func alreadyHydrated(apps []*v1alpha1.Application, drySHA string) (string, bool) {
	hydrated := ""
	for _, app := range apps {
		last := app.Status.SourceHydrator.LastSuccessfulOperation
		cur := app.Status.SourceHydrator.CurrentOperation
		if last == nil || last.DrySHA != drySHA || last.HydratedSHA == "" || last.SourceHydrator == nil || cur == nil || cur.SourceHydrator == nil {
			return "", false
		}
		if !apiequality.Semantic.DeepEqual(*last.SourceHydrator, *cur.SourceHydrator) {
			return "", false
		}
		if hydrated != "" && hydrated != last.HydratedSHA {
			return "", false
		}
		hydrated = last.HydratedSHA
	}
	return hydrated, hydrated != ""
}

// revisionMetadata reads the dry commit's metadata with read credentials.
func (h *Hydrator) revisionMetadata(ctx context.Context, repoURL, projName, revision string) (*apiclient.RevisionMetadata, error) {
	repo, err := h.d.Repositories.GetRepository(ctx, repoURL, projName)
	if err != nil {
		return nil, fmt.Errorf("failed to get repository %q: %w", git.SanitizeURL(repoURL), err)
	}
	if repo == nil {
		repo = &apiclient.Repository{URL: repoURL}
	}
	md, err := h.d.RepoServer.GetRevisionMetadata(ctx, &apiclient.RevisionMetadataRequest{Repo: *repo, Revision: revision})
	if err != nil {
		return nil, fmt.Errorf("failed to get revision metadata: %w", err)
	}
	return md, nil
}

// fail records a failed hydration on every Application of the group (APP-116, APP-130):
// Applications with their own error get it, the others a generic error naming the first
// failing Application. When the dry SHA was resolved it is recorded too.
func (h *Hydrator) fail(ctx context.Context, apps []*v1alpha1.Application, errs map[string]error, drySHA string, seen versions) error {
	generic := genericHydrationError(errs)
	now := metav1.NewTime(h.d.Now())
	var persistErrs []error
	for _, app := range apps {
		cause, ok := errs[app.QualifiedName()]
		if !ok {
			cause = generic
		}
		msg := statusMessage("Failed to hydrate: " + cause.Error())
		err := h.patchSourceHydrator(ctx, app, func(a *v1alpha1.Application) {
			s := &a.Status.SourceHydrator
			if s.CurrentOperation == nil {
				s.CurrentOperation = &v1alpha1.HydrateOperation{StartedAt: now.DeepCopy()}
				if a.Spec.SourceHydrator != nil {
					s.CurrentOperation.SourceHydrator = a.Spec.SourceHydrator.DeepCopy()
				}
			}
			s.CurrentOperation.Phase = v1alpha1.HydrateOperationPhaseFailed
			s.CurrentOperation.FinishedAt = now.DeepCopy()
			s.CurrentOperation.Message = msg
			s.CurrentOperation.HydratedSHA = ""
			if drySHA != "" {
				s.CurrentOperation.DrySHA = drySHA
				s.LastComparedDryRevision = drySHA
			}
		})
		if err != nil && !errors.Is(err, errAppGone) {
			persistErrs = append(persistErrs, err)
		}
		seen.add(app)
	}
	return errors.Join(persistErrs...)
}

// succeed records a successful hydration and requests a refresh of every Application so it
// compares and syncs the hydrated commit (APP-132).
func (h *Hydrator) succeed(ctx context.Context, log *slog.Logger, apps []*v1alpha1.Application, drySHA, hydratedSHA string, seen versions) error {
	now := metav1.NewTime(h.d.Now())
	var persistErrs []error
	for _, app := range apps {
		err := h.patchSourceHydrator(ctx, app, func(a *v1alpha1.Application) {
			s := &a.Status.SourceHydrator
			op := &v1alpha1.HydrateOperation{
				FinishedAt:  now.DeepCopy(),
				Phase:       v1alpha1.HydrateOperationPhaseHydrated,
				DrySHA:      drySHA,
				HydratedSHA: hydratedSHA,
			}
			if cur := s.CurrentOperation; cur != nil {
				op.StartedAt = cur.StartedAt.DeepCopy()
				op.SourceHydrator = cur.SourceHydrator.DeepCopy()
			}
			if op.SourceHydrator == nil && a.Spec.SourceHydrator != nil {
				op.SourceHydrator = a.Spec.SourceHydrator.DeepCopy()
			}
			s.CurrentOperation = op
			s.LastComparedDryRevision = drySHA
			s.LastSuccessfulOperation = &v1alpha1.SuccessfulHydrateOperation{
				DrySHA:         drySHA,
				HydratedSHA:    hydratedSHA,
				SourceHydrator: op.SourceHydrator.DeepCopy(),
			}
		})
		if errors.Is(err, errAppGone) {
			continue
		}
		if err != nil {
			persistErrs = append(persistErrs, err)
			continue
		}
		seen.add(app)
		if err := h.requestRefresh(ctx, app); err != nil {
			log.Error("hydrator: failed to request app refresh after hydration", slog.String("application", app.QualifiedName()), slog.Any("error", err))
		}
	}
	return errors.Join(persistErrs...)
}

// genericHydrationError summarizes the errors of a group for the Applications without an
// error of their own (upstream wording).
func genericHydrationError(errs map[string]error) error {
	if len(errs) == 0 {
		return nil
	}
	keys := slices.Sorted(maps.Keys(errs))
	remainder := "has an error"
	if len(keys) > 1 {
		remainder = fmt.Sprintf("and %d more have errors", len(keys)-1)
	}
	return fmt.Errorf("cannot hydrate because application %s %s", keys[0], remainder)
}
