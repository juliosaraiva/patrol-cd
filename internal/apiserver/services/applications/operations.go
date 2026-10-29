package applications

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	pubapi "github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Event reasons of operation requests (upstream argo.EventReasonOperationStarted).
const reasonOperationStarted = "OperationStarted"

// operationInProgress is returned when the Application already has a non-terminal
// Operation (upstream FailedPrecondition "another operation is already in progress";
// 409 because the controller admits at most one, ADR-0011).
func operationInProgress() error {
	return &rest.Error{Status: 409, Code: pubapi.CodeFailedPrecondition, Detail: "another operation is already in progress"}
}

func failedPrecondition(format string, args ...any) error {
	return rest.FailedPrecondition(format, args...)
}

// operationsOf returns the Operations of app (spec.application names it and no controller
// owner reference points to another incarnation), read from the API server page by page.
func (s *Service) operationsOf(ctx context.Context, app *v1alpha1.Application) ([]v1alpha1.Operation, error) {
	c, err := s.kube()
	if err != nil {
		return nil, err
	}
	var out []v1alpha1.Operation
	cont := ""
	for {
		list := &v1alpha1.OperationList{}
		opts := []client.ListOption{client.InNamespace(app.Namespace), client.Limit(500)}
		if cont != "" {
			opts = append(opts, client.Continue(cont))
		}
		if err := c.List(ctx, list, opts...); err != nil {
			return nil, err
		}
		for i := range list.Items {
			if belongsTo(&list.Items[i], app) {
				out = append(out, list.Items[i])
			}
		}
		if list.Continue == "" {
			return out, nil
		}
		cont = list.Continue
	}
}

func belongsTo(op *v1alpha1.Operation, app *v1alpha1.Application) bool {
	if op.Spec.Application != app.Name {
		return false
	}
	if ref := metav1.GetControllerOfNoCopy(op); ref != nil && ref.Kind == "Application" && app.UID != "" && ref.UID != app.UID {
		return false
	}
	return true
}

// activeOperations returns the non-terminal Operations of app.
func (s *Service) activeOperations(ctx context.Context, app *v1alpha1.Application) ([]v1alpha1.Operation, error) {
	ops, err := s.operationsOf(ctx, app)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(ops, func(op v1alpha1.Operation) bool { return op.IsTerminal() }), nil
}

// newOperation builds an Operation for app: generated name, the application label (when
// the name is a valid label value) and a controller owner reference, so Operations are
// garbage-collected with their Application.
func newOperation(app *v1alpha1.Application, kind string, spec v1alpha1.OperationSpec) *v1alpha1.Operation {
	prefix := app.Name
	if len(prefix) > 200 {
		prefix = strings.TrimRight(prefix[:200], "-.")
	}
	op := &v1alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: prefix + "-" + kind + "-",
			Namespace:    app.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1.GroupVersion.String(), Kind: "Application",
				Name: app.Name, UID: app.UID, Controller: new(true),
			}},
		},
		Spec: spec,
	}
	if len(validation.IsValidLabelValue(app.Name)) == 0 {
		op.Labels = map[string]string{v1alpha1.LabelKeyOperationApplication: app.Name}
	}
	return op
}

// maskOperation returns a copy of op whose local manifests have Secret data masked.
func maskOperation(op *v1alpha1.Operation, hide map[string]bool) *v1alpha1.Operation {
	out := op.DeepCopy()
	if out.Spec.Sync != nil && len(out.Spec.Sync.Manifests) > 0 {
		out.Spec.Sync.Manifests = maskManifests(out.Spec.Sync.Manifests, hide)
	}
	return out
}

var commitSHA = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// resolveRevision resolves the revision of source to an immutable one through the
// repo-server without its revision cache (upstream resolveRevision). A Git commit SHA is
// used as is. Without a repo-server the revision is left for the controller, which
// resolves and persists it before mutating anything.
func (s *Service) resolveRevision(ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject, source v1alpha1.ApplicationSource, index int, ambiguous string) (string, string, error) {
	if source.Chart == "" && commitSHA.MatchString(ambiguous) {
		return ambiguous, ambiguous, nil
	}
	if s.d.RepoServer == nil {
		return ambiguous, ambiguous, nil
	}
	repos, err := s.repositories()
	if err != nil {
		return "", "", err
	}
	repo, err := repos.GetRepository(ctx, source.RepoURL, proj.Name)
	if err != nil {
		return "", "", repositoryError(err)
	}
	resp, err := s.d.RepoServer.ResolveRevision(ctx, &apiclient.ResolveRevisionRequest{
		Repo: *repo, App: app.Name, Revision: ambiguous, Source: source, SourceIndex: max(index, 0), ForceRefresh: true,
	})
	if err != nil {
		if e := repoServerError("error resolving repo revision", err); rest.ProblemFor(e).Status == 400 {
			return "", "", failedPrecondition("%s", rest.ProblemFor(e).Detail)
		} else {
			return "", "", e
		}
	}
	return resp.Revision, firstNonEmpty(resp.AmbiguousRevision, ambiguous), nil
}

// parseBackoffDuration accepts seconds or a Go duration, as the controller does.
func parseBackoffDuration(field, v string) error {
	if v == "" {
		return nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return rest.InvalidArgument("%s must not be negative", field)
		}
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return rest.InvalidArgument("%s %q is not a duration", field, v)
	}
	return nil
}

func validateRetry(r *v1alpha1.RetryStrategy) error {
	if r == nil || r.Backoff == nil {
		return nil
	}
	if err := parseBackoffDuration("retryStrategy.backoff.duration", r.Backoff.Duration); err != nil {
		return err
	}
	if err := parseBackoffDuration("retryStrategy.backoff.maxDuration", r.Backoff.MaxDuration); err != nil {
		return err
	}
	if r.Backoff.Factor != nil && *r.Backoff.Factor < 1 {
		return rest.InvalidArgument("retryStrategy.backoff.factor must be at least 1")
	}
	return nil
}

func validateInfos(infos []v1alpha1.Info) error {
	if len(infos) > MaxOperationInfos {
		return rest.InvalidArgument("at most %d infos are accepted", MaxOperationInfos)
	}
	for _, i := range infos {
		if i.Name == "" {
			return rest.InvalidArgument("info entries need a name")
		}
	}
	return nil
}

func validateSyncResources(res []v1alpha1.SyncOperationResource) error {
	if len(res) > MaxSyncResources {
		return rest.InvalidArgument("at most %d resources can be synced selectively", MaxSyncResources)
	}
	for _, r := range res {
		if r.Kind == "" || r.Name == "" {
			return rest.InvalidArgument("resources to sync need a kind and a name")
		}
	}
	return nil
}

func validateLocalManifests(manifests []string) error {
	if len(manifests) > MaxLocalManifests {
		return rest.InvalidArgument("at most %d manifests are accepted", MaxLocalManifests)
	}
	total := 0
	for _, m := range manifests {
		total += len(m)
	}
	if total > MaxLocalManifestsBytes {
		return &rest.Error{Status: 413, Code: pubapi.CodeResourceExhausted, Detail: fmt.Sprintf("local manifests exceed %d bytes in total", MaxLocalManifestsBytes)}
	}
	return nil
}

// overrideSources validates the source(s) a sync request asks to render instead of the
// spec: the project must permit them like spec sources, and rendering anything else than
// the spec needs applications, override.
func (s *Service) overrideSources(ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject, req *pubapi.ApplicationSyncRequest) (v1alpha1.ApplicationSources, error) {
	if app.Spec.SourceHydrator != nil {
		return nil, rest.InvalidArgument("source overrides are not supported for applications using the source hydrator")
	}
	multi := app.Spec.HasMultipleSources()
	spec := app.Spec.DeepCopy()
	var sources v1alpha1.ApplicationSources
	if multi {
		if req.Source != nil || len(req.Sources) == 0 {
			return nil, rest.InvalidArgument("a multi-source application takes sources, not source")
		}
		sources = req.Sources.DeepCopy()
		spec.Sources = sources
	} else {
		if len(req.Sources) > 0 || req.Source == nil {
			return nil, rest.InvalidArgument("a single-source application takes source, not sources")
		}
		sources = v1alpha1.ApplicationSources{*req.Source.DeepCopy()}
		spec.Source = &sources[0]
	}
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	kl, _ := s.lookups(st)
	issues, err := project.ValidatePermissions(ctx, spec, proj, kl)
	if err != nil {
		return nil, rest.Unavailable("error validating project permissions").WithCause(err)
	}
	if len(issues) > 0 {
		parts := make([]string, 0, len(issues))
		for _, i := range issues {
			parts = append(parts, fmt.Sprintf("%s: %s", i.Type, i.Message))
		}
		return nil, rest.InvalidArgument("sync source override is invalid: %s", strings.Join(parts, ";"))
	}
	if multi {
		if err := project.ValidateRefSources(sources); err != nil {
			return nil, rest.InvalidArgument("sync source override is invalid: %v", err)
		}
	}
	if !jsonEqual(sources, app.Spec.GetSources()) {
		if err := s.authorizeApp(ctx, rbac.ActionOverride, app); err != nil {
			return nil, err
		}
		if app.Spec.IsAutomatedSyncEnabled() && !req.DryRun {
			return nil, failedPrecondition("cannot sync a source override when Automatic Sync Policy is enabled unless for dry run")
		}
	}
	return sources, nil
}

// syncApplication serves POST /applications/{name}/sync: it creates an Operation whose
// spec.sync mirrors the request (upstream ApplicationService.Sync). The returned
// Application carries the resource health kept in the controller's resource tree
// (withTreeHealth).
func (s *Service) syncApplication(ctx context.Context, req *pubapi.ApplicationSyncRequest) (*pubapi.ApplicationOperationResponse, error) {
	if err := checkBodyName(req.Name, req.BodyName); err != nil {
		return nil, err
	}
	ns, err := effectiveValue("appNamespace", req.AppNamespace, req.BodyAppNamespace)
	if err != nil {
		return nil, err
	}
	projectName, err := effectiveValue("project", req.Project, req.BodyProject)
	if err != nil {
		return nil, err
	}
	c, err := s.kube()
	if err != nil {
		return nil, err
	}
	app, proj, err := s.getApp(ctx, rbac.ActionGet, appRequest{name: req.Name, namespace: ns, project: projectName})
	if err != nil {
		return nil, err
	}
	// Unlike upstream, sync permission is checked before the windows, so a caller without
	// it learns nothing from the answer.
	if err := s.authorizeApp(ctx, rbac.ActionSync, app); err != nil {
		return nil, err
	}
	canSync, err := project.CanSync(proj, app, true, nil, s.now())
	if err != nil {
		return nil, rest.PermissionDenied("cannot sync: invalid sync window: %v", err)
	}
	if !canSync {
		return nil, rest.PermissionDenied("%s", project.SyncBlockedAPIMessage)
	}
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	automated := app.Spec.IsAutomatedSyncEnabled()
	overrides := req.Source != nil || len(req.Sources) > 0
	if req.Manifests != nil {
		if overrides {
			return nil, rest.InvalidArgument("manifests and source overrides are mutually exclusive")
		}
		if err := s.authorizeApp(ctx, rbac.ActionOverride, app); err != nil {
			return nil, err
		}
		if automated && !req.DryRun {
			return nil, failedPrecondition("cannot use local sync when Automatic Sync Policy is enabled unless for dry run")
		}
		if err := validateLocalManifests(req.Manifests); err != nil {
			return nil, err
		}
	}
	if app.DeletionTimestamp != nil {
		return nil, failedPrecondition("application is deleting")
	}
	if err := validateSyncResources(req.Resources); err != nil {
		return nil, err
	}
	if err := validateInfos(req.Infos); err != nil {
		return nil, err
	}
	if err := validateRetry(req.RetryStrategy); err != nil {
		return nil, err
	}

	sources := app.Spec.GetSources()
	if overrides {
		if sources, err = s.overrideSources(ctx, app, proj, req); err != nil {
			return nil, err
		}
	}
	if len(sources) == 0 {
		return nil, rest.InvalidArgument("application %s has no source", app.Name)
	}
	multi := app.Spec.HasMultipleSources()
	var revision, display string
	var revisions, displays []string
	switch {
	case multi:
		if req.Revision != "" {
			return nil, rest.InvalidArgument("revision cannot be used with a multi-source application; use revisions and sourcePositions")
		}
		if len(req.Revisions) != len(req.SourcePositions) {
			return nil, rest.InvalidArgument("revisions and sourcePositions must have the same number of values")
		}
		desired := make([]string, len(sources))
		for i, pos := range req.SourcePositions {
			if pos <= 0 || pos > int64(len(sources)) {
				return nil, rest.InvalidArgument("source position is out of range")
			}
			desired[pos-1] = req.Revisions[i]
		}
		if req.Manifests != nil {
			break
		}
		revisions, displays = make([]string, len(sources)), make([]string, len(sources))
		for i, src := range sources {
			if d := desired[i]; d != "" && d != firstNonEmpty(src.TargetRevision, "HEAD") {
				if err := s.revisionOverride(ctx, app, st); err != nil {
					return nil, err
				}
				if automated && !req.DryRun {
					return nil, failedPrecondition("Cannot sync source %s to %s: auto-sync currently set to %s", redactURL(src.RepoURL), d, src.TargetRevision)
				}
			}
			if revisions[i], displays[i], err = s.resolveRevision(ctx, app, proj, src, i, firstNonEmpty(desired[i], src.TargetRevision)); err != nil {
				return nil, err
			}
		}
	default:
		if len(req.Revisions) > 0 || len(req.SourcePositions) > 0 {
			return nil, rest.InvalidArgument("revisions and sourcePositions can only be used with a multi-source application")
		}
		if req.Manifests != nil {
			break
		}
		src := sources[0]
		if req.Revision != "" && req.Revision != firstNonEmpty(src.TargetRevision, "HEAD") {
			if err := s.revisionOverride(ctx, app, st); err != nil {
				return nil, err
			}
			if automated && !req.DryRun {
				return nil, failedPrecondition("Cannot sync to %s: auto-sync currently set to %s", req.Revision, src.TargetRevision)
			}
		}
		if revision, display, err = s.resolveRevision(ctx, app, proj, src, 0, firstNonEmpty(req.Revision, src.TargetRevision)); err != nil {
			return nil, err
		}
	}

	var retry *v1alpha1.RetryStrategy
	var syncOptions v1alpha1.SyncOptions
	if sp := app.Spec.SyncPolicy; sp != nil {
		retry, syncOptions = sp.Retry, sp.SyncOptions
	}
	if req.RetryStrategy != nil {
		retry = req.RetryStrategy
	}
	if req.SyncOptions != nil {
		syncOptions = req.SyncOptions.Items
	}
	if msgs := project.ValidateSyncOptions(syncOptions); len(msgs) > 0 {
		return nil, rest.InvalidArgument("invalid sync options: %s", strings.Join(msgs, "; "))
	}
	if syncOptions.HasOption(v1alpha1.SyncOptionKeyReplace+"="+v1alpha1.SyncOptionValueTrue) && s.d.Config.DisableSyncWithReplace {
		return nil, failedPrecondition("sync with replace was disabled on the API Server level via the server configuration")
	}
	if req.Manifests != nil {
		si, _ := project.EffectiveSourceIntegrity(proj)
		if project.HasSourceIntegrityCriteria(si, !s.d.Config.DisableGPG, app.Spec.GetSources()...) {
			return nil, failedPrecondition("Cannot use local manifests when source integrity is enforced")
		}
	}

	sync := &v1alpha1.SyncOperation{
		Revision:     revision,
		Revisions:    revisions,
		Prune:        req.Prune,
		DryRun:       req.DryRun,
		SyncStrategy: req.Strategy.DeepCopy(),
		Resources:    slices.Clone(req.Resources),
		Manifests:    slices.Clone(req.Manifests),
		SyncOptions:  slices.Clone(syncOptions),
	}
	if multi {
		sync.Sources = sources.DeepCopy()
	} else {
		src := sources[0]
		sync.Source = &src
	}
	op := newOperation(app, "sync", v1alpha1.OperationSpec{
		Application: app.Name,
		Sync:        sync,
		InitiatedBy: v1alpha1.OperationInitiator{Username: initiatorName(ctx)},
		Info:        slices.Clone(req.Infos),
		Retry:       retry.DeepCopy(),
	})
	if err := s.createOperation(ctx, c, app, op); err != nil {
		return nil, err
	}
	partial := ""
	if len(req.Resources) > 0 {
		partial = "partial "
	}
	target := display
	if multi {
		target = strings.Join(displays, ",")
	}
	if req.Manifests != nil {
		auditApp(ctx, app, rbac.ActionSync, s.rbacObject(app), reasonOperationStarted, "initiated %ssync locally", partial)
	} else {
		auditApp(ctx, app, rbac.ActionSync, s.rbacObject(app), reasonOperationStarted, "initiated %ssync to %s", partial, target)
	}
	filled, _ := s.withTreeHealth(ctx, app)
	return &pubapi.ApplicationOperationResponse{Application: filled, Operation: maskOperation(op, sensitiveAnnotations(st))}, nil
}

// revisionOverride enforces applications, override for a revision override when
// application.sync.requireOverridePrivilegeForRevisionSync is set.
func (s *Service) revisionOverride(ctx context.Context, app *v1alpha1.Application, st interface {
	RequireOverridePrivilegeForRevisionSync() bool
}) error {
	if !st.RequireOverridePrivilegeForRevisionSync() {
		return nil
	}
	return s.authorizeApp(ctx, rbac.ActionOverride, app)
}

// createOperation refuses a second non-terminal Operation and creates op. The check is
// best effort: concurrent requests can both pass it, and the controller then admits the
// oldest and rejects the others (ADR-0011).
func (s *Service) createOperation(ctx context.Context, c client.Client, app *v1alpha1.Application, op *v1alpha1.Operation) error {
	active, err := s.activeOperations(ctx, app)
	if err != nil {
		return err
	}
	if len(active) > 0 {
		return operationInProgress()
	}
	if err := c.Create(ctx, op); err != nil {
		if apierrors.IsForbidden(err) {
			// The server's own identity was refused (RBAC, or the Operation admission policy
			// without the impersonate verb): a deployment error, not the caller's.
			return rest.Internal(fmt.Errorf("creating an Operation was refused; patrol-server needs create and %q on operations.%s: %w", v1alpha1.OperationInitiatorVerb, v1alpha1.GroupName, err))
		}
		return err
	}
	return nil
}

// rollbackApplication serves POST /applications/{name}/rollback: it creates a rollback
// Operation for a status.history entry (apply strategy, no hooks, the Application's sync
// options, no retry). The returned Application carries the resource health kept in the
// controller's resource tree (withTreeHealth).
func (s *Service) rollbackApplication(ctx context.Context, req *pubapi.ApplicationRollbackRequest) (*pubapi.ApplicationOperationResponse, error) {
	if err := checkBodyName(req.Name, req.BodyName); err != nil {
		return nil, err
	}
	ns, err := effectiveValue("appNamespace", req.AppNamespace, req.BodyAppNamespace)
	if err != nil {
		return nil, err
	}
	projectName, err := effectiveValue("project", req.Project, req.BodyProject)
	if err != nil {
		return nil, err
	}
	c, err := s.kube()
	if err != nil {
		return nil, err
	}
	app, _, err := s.getApp(ctx, rbac.ActionSync, appRequest{name: req.Name, namespace: ns, project: projectName})
	if err != nil {
		return nil, err
	}
	if app.DeletionTimestamp != nil {
		return nil, failedPrecondition("application is deleting")
	}
	if app.Spec.IsAutomatedSyncEnabled() {
		return nil, failedPrecondition("rollback cannot be initiated when auto-sync is enabled")
	}
	idx := slices.IndexFunc(app.Status.History, func(h v1alpha1.RevisionHistory) bool { return h.ID == req.ID })
	if idx < 0 {
		return nil, rest.InvalidArgument("application %s does not have deployment with id %v", app.QualifiedName(), req.ID)
	}
	entry := app.Status.History[idx]
	if entry.Source == nil && len(entry.Sources) == 0 {
		return nil, failedPrecondition("cannot rollback to a deployment without recorded sources. sync to revision instead.")
	}
	if app.Spec.HasMultipleSources() && len(entry.Sources) == 0 {
		return nil, failedPrecondition("cannot rollback a multi-source application to a single-source deployment. sync to revisions instead.")
	}
	var syncOptions v1alpha1.SyncOptions
	if app.Spec.SyncPolicy != nil {
		syncOptions = slices.Clone(app.Spec.SyncPolicy.SyncOptions)
	}
	op := newOperation(app, "rollback", v1alpha1.OperationSpec{
		Application: app.Name,
		Rollback:    &v1alpha1.RollbackOperation{ID: req.ID},
		Sync: &v1alpha1.SyncOperation{
			Prune:        req.Prune,
			DryRun:       req.DryRun,
			SyncStrategy: &v1alpha1.SyncStrategy{Apply: &v1alpha1.SyncStrategyApply{}},
			SyncOptions:  syncOptions,
		},
		InitiatedBy: v1alpha1.OperationInitiator{Username: initiatorName(ctx)},
	})
	if err := s.createOperation(ctx, c, app, op); err != nil {
		return nil, err
	}
	auditApp(ctx, app, rbac.ActionSync, s.rbacObject(app), reasonOperationStarted, "initiated rollback to %d", req.ID)
	filled, _ := s.withTreeHealth(ctx, app)
	return &pubapi.ApplicationOperationResponse{Application: filled, Operation: op}, nil
}

// terminateOperation serves DELETE /applications/{name}/operation: spec.cancel=true is
// merge-patched on every non-terminal Operation of the Application.
func (s *Service) terminateOperation(ctx context.Context, req *pubapi.ApplicationRef) (*pubapi.ApplicationOperationResponse, error) {
	c, err := s.kube()
	if err != nil {
		return nil, err
	}
	app, _, err := s.getApp(ctx, rbac.ActionSync, appRequest{name: req.Name, namespace: req.AppNamespace, project: req.Project})
	if err != nil {
		return nil, err
	}
	active, err := s.activeOperations(ctx, app)
	if err != nil {
		return nil, err
	}
	if len(active) == 0 {
		return nil, rest.InvalidArgument("Unable to terminate operation. No operation is in progress")
	}
	sortOperations(active)
	patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"cancel":true}}`))
	for i := range active {
		if active[i].Spec.Cancel {
			continue
		}
		if err := c.Patch(ctx, &active[i], patch); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
	}
	auditApp(ctx, app, rbac.ActionSync, s.rbacObject(app), rest.ReasonResourceUpdated, "terminated running operation")
	st, _ := s.settings()
	return &pubapi.ApplicationOperationResponse{Application: app, Operation: maskOperation(&active[0], sensitiveAnnotations(st))}, nil
}

// sortOperations orders Operations newest first (creation time, then name).
func sortOperations(ops []v1alpha1.Operation) {
	slices.SortFunc(ops, compareOperations)
}

func compareOperations(a, b v1alpha1.Operation) int {
	return cmp.Or(b.CreationTimestamp.Compare(a.CreationTimestamp.Time), strings.Compare(b.Name, a.Name))
}

type operationsToken struct {
	Created time.Time `json:"t"`
	Name    string    `json:"n"`
}

// listOperations serves GET /applications/{name}/operations: newest first, bounded.
func (s *Service) listOperations(ctx context.Context, req *pubapi.ApplicationOperationsRequest) (*v1alpha1.OperationList, error) {
	app, _, err := s.getApp(ctx, rbac.ActionGet, appRequest{name: req.Name, namespace: req.AppNamespace, project: req.Project})
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	switch {
	case limit < 0 || limit > maxOperationsLimit:
		return nil, rest.InvalidArgument("limit must be between 0 and %d", maxOperationsLimit)
	case limit == 0:
		limit = defaultOperationsLimit
	}
	ops, err := s.operationsOf(ctx, app)
	if err != nil {
		return nil, err
	}
	sortOperations(ops)
	if req.Continue != "" {
		var tok operationsToken
		if err := decodeToken(req.Continue, &tok); err != nil {
			return nil, err
		}
		marker := v1alpha1.Operation{ObjectMeta: metav1.ObjectMeta{Name: tok.Name, CreationTimestamp: metav1.NewTime(tok.Created)}}
		i := 0
		for i < len(ops) && compareOperations(ops[i], marker) <= 0 {
			i++
		}
		ops = ops[i:]
	}
	out := &v1alpha1.OperationList{Items: []v1alpha1.Operation{}}
	if int64(len(ops)) > limit {
		ops = ops[:limit]
		last := ops[len(ops)-1]
		out.Continue = encodeToken(operationsToken{Created: last.CreationTimestamp.Time, Name: last.Name})
	}
	st, _ := s.settings()
	hide := sensitiveAnnotations(st)
	for i := range ops {
		out.Items = append(out.Items, *maskOperation(&ops[i], hide))
	}
	return out, nil
}

// getOperation serves GET /applications/{name}/operations/{op}.
func (s *Service) getOperation(ctx context.Context, req *pubapi.ApplicationOperationRequest) (*v1alpha1.Operation, error) {
	c, err := s.kube()
	if err != nil {
		return nil, err
	}
	app, _, err := s.getApp(ctx, rbac.ActionGet, appRequest{name: req.Name, namespace: req.AppNamespace, project: req.Project})
	if err != nil {
		return nil, err
	}
	notFound := rest.NotFound("operations.%s %q not found", v1alpha1.GroupName, req.Operation)
	op := &v1alpha1.Operation{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: app.Namespace, Name: req.Operation}, op); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, notFound
		}
		return nil, err
	}
	if !belongsTo(op, app) {
		return nil, notFound
	}
	st, _ := s.settings()
	return maskOperation(op, sensitiveAnnotations(st)), nil
}
