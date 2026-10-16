// Package operation implements the Operation reconciler: the sync operation state machine of
// docs/architecture/sync-state-machine.md (ADR-0011). Each reconcile performs at most one
// step of one Operation (admission, revision resolution, one sync wave, retry scheduling,
// termination or completion) and persists the result with optimistic concurrency before
// returning, so a restarted controller resumes from Operation.status.
package operation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	syncpkg "github.com/juliosaraiva/patrol-cd/internal/sync"
)

// ControllerName names the Operation controller (metrics, logs, events).
const ControllerName = "patrol-operation"

// Condition reasons and messages not declared in api/v1alpha1 (requested there).
const (
	ReasonApplicationDeleting = "ApplicationDeleting"
	ReasonRollbackNotAllowed  = "RollbackNotAllowed"
	ReasonTerminating         = "Terminating"

	msgOperationInProgress = "another operation is already in progress"
	msgApplicationDeleting = "application is deleting"
	msgRollbackAutoSync    = "rollback cannot be initiated when auto-sync is enabled"
	msgRollbackNoSource    = "cannot rollback to a revision recorded without its source; sync to the revision instead"
	msgTerminating         = "operation is terminating"
	msgTerminatingTimeout  = "operation is terminating due to timeout"
	msgTerminated          = "Operation terminated"
	msgSyncWindowBlocked   = "Sync operation blocked by sync window"
	causeSyncTimeout       = "controller sync timeout"
	// maxMessageLen bounds status messages.
	maxMessageLen = 32 * 1024
	// windowRecheck bounds the wait of an operation blocked by a sync window.
	windowRecheck = time.Minute
)

// Config configures the Operation reconciler.
type Config struct {
	core.Deps
	// Engines builds sync engines. Required (see NewKubeEngines).
	Engines EngineFactory
	// PollInterval is the requeue interval of transient errors (default
	// syncpkg.DefaultPollInterval).
	PollInterval time.Duration
}

// Reconciler reconciles Operations.
type Reconciler struct {
	cfg Config
	c   client.Client
	log *slog.Logger
}

// New returns a Reconciler.
func New(cfg Config) (*Reconciler, error) {
	if err := cfg.Complete(); err != nil {
		return nil, err
	}
	if cfg.Engines == nil {
		return nil, errors.New("operation: Engines is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = syncpkg.DefaultPollInterval
	}
	return &Reconciler{cfg: cfg, c: cfg.Client, log: cfg.Logger.With("controller", ControllerName)}, nil
}

// SetupWithManager registers the controller: Operations of allowed namespaces, and the
// non-terminal Operations of an Application when that Application changes (deletion,
// skip-reconcile removal, sync policy changes). core.IndexOperations must have been called.
func (r *Reconciler) SetupWithManager(mgr manager.Manager) error {
	allowed := predicate.NewPredicateFuncs(func(o client.Object) bool { return r.cfg.Namespaces().Allowed(o.GetNamespace()) })
	err := ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(&v1alpha1.Operation{}, builder.WithPredicates(allowed)).
		Watches(&v1alpha1.Application{}, handler.EnqueueRequestsFromMapFunc(r.operationsOfApplication),
			builder.WithPredicates(allowed, applicationChanged())).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.cfg.Params.OperationProcessors,
			SkipNameValidation:      ptr.To(true),
		}).
		Complete(r)
	if err != nil {
		return fmt.Errorf("create operation controller: %w", err)
	}
	return nil
}

// applicationChanged passes the Application changes that matter to pending and running
// Operations: spec (sync policy, rollback admission), annotations (skip-reconcile,
// deletion-approved) and deletion. Status-only updates, which the controllers write
// constantly, are filtered out.
func applicationChanged() predicate.Predicate {
	deletion := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return (e.ObjectOld.GetDeletionTimestamp() == nil) != (e.ObjectNew.GetDeletionTimestamp() == nil)
	}}
	return predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}, deletion)
}

func (r *Reconciler) operationsOfApplication(ctx context.Context, o client.Object) []reconcile.Request {
	ops, err := core.ListOperations(ctx, r.c, o.GetNamespace(), o.GetName())
	if err != nil {
		r.log.Warn("cannot list operations of application", "application", o.GetNamespace()+"/"+o.GetName(), "error", err)
		return nil
	}
	var out []reconcile.Request
	for i := range ops {
		if core.IsNonTerminal(&ops[i]) {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&ops[i])})
		}
	}
	return out
}

// Reconcile performs one step of the Operation's state machine.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (res reconcile.Result, err error) {
	start := r.cfg.Clock.Now()
	op := &v1alpha1.Operation{}
	if err := r.c.Get(ctx, req.NamespacedName, op); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !r.cfg.Namespaces().Allowed(op.Namespace) {
		return reconcile.Result{}, nil
	}
	log := r.log.With("operation", req.String(), "application", op.Namespace+"/"+op.Spec.Application)
	defer func() {
		if r.cfg.Metrics != nil {
			r.cfg.Metrics.OperationStepDuration.WithLabelValues(string(op.Status.Phase)).Observe(r.cfg.Clock.Since(start).Seconds())
		}
		switch {
		case errors.Is(err, core.ErrGone):
			res, err = reconcile.Result{}, nil
		case errors.Is(err, core.ErrConflict):
			// Another writer (a user setting spec.cancel, another replica after a lease
			// handover) changed the Operation since this step read it: the step is abandoned
			// and recomputed from the newer object (sync-state-machine.md). Its watch event
			// normally triggers that reconcile first.
			log.Info("operation changed concurrently; step abandoned", "error", err)
			res, err = reconcile.Result{RequeueAfter: r.cfg.PollInterval}, nil
		}
	}()
	s := &step{r: r, op: op, log: log}
	if op.Status.Phase.IsTerminal() {
		// Terminal bookkeeping (phase label, Application summary and history repair,
		// retention) writes the Operation and its Application: only the shard owning the
		// destination may do it, or two replicas race on the same objects after every
		// completion (ADR-0008).
		if r.cfg.OwnsCluster != nil {
			owned, err := r.ownsTerminal(ctx, op)
			if err != nil || !owned {
				return reconcile.Result{}, err
			}
		}
		return s.reconcileTerminal(ctx)
	}

	app := &v1alpha1.Application{}
	if err := r.cfg.APIReader.Get(ctx, types.NamespacedName{Namespace: op.Namespace, Name: op.Spec.Application}, app); err != nil {
		if !apierrors.IsNotFound(err) {
			return reconcile.Result{}, fmt.Errorf("get application: %w", err)
		}
		if r.cfg.OwnsCluster != nil && !r.cfg.OwnsCluster(nil) {
			return reconcile.Result{}, nil
		}
		msg := fmt.Sprintf("application %s/%s not found", op.Namespace, op.Spec.Application)
		if op.Status.Phase == "" || op.Status.Phase == v1alpha1.OperationPending {
			return s.reject(ctx, v1alpha1.OperationReasonApplicationNotFound, msg)
		}
		return s.complete(ctx, v1alpha1.OperationError, msg, v1alpha1.OperationReasonApplicationNotFound)
	}
	s.app = app
	if core.SkipReconcile(app.Annotations) {
		log.Debug("skipping operation of an application with skip-reconcile")
		return reconcile.Result{}, nil
	}
	if r.cfg.OwnsCluster != nil && !r.ownsDestination(ctx, app) {
		return reconcile.Result{}, nil
	}
	if err := s.ensureMetadata(ctx); err != nil {
		return reconcile.Result{}, err
	}

	switch op.Status.Phase {
	case "", v1alpha1.OperationPending:
		return s.admit(ctx)
	case v1alpha1.OperationTerminating:
		return s.terminate(ctx, "")
	default:
		return s.run(ctx)
	}
}

// step is the state of one reconcile of one Operation.
type step struct {
	r   *Reconciler
	op  *v1alpha1.Operation
	app *v1alpha1.Application
	log *slog.Logger
}

func (s *step) now() metav1.Time { return metav1.NewTime(s.r.cfg.Clock.Now()) }

// ensureMetadata adds the application and phase labels and the controller owner reference
// when missing, and corrects an application label that does not name spec.application (the
// API server lists Operations by that label, core.ListOperationsLive). The write is strict:
// a conflict abandons the step.
func (s *step) ensureMetadata(ctx context.Context) error {
	_, err := core.PatchObjectStrict(ctx, s.r.c, s.op, func(o *v1alpha1.Operation) error {
		if o.Labels == nil {
			o.Labels = map[string]string{}
		}
		cur, has := o.Labels[v1alpha1.LabelKeyOperationApplication]
		switch want := core.ApplicationLabelValue(o.Spec.Application); {
		case want == "" && has:
			delete(o.Labels, v1alpha1.LabelKeyOperationApplication)
		case want != "" && cur != want:
			o.Labels[v1alpha1.LabelKeyOperationApplication] = want
		}
		if o.Status.Phase != "" {
			o.Labels[v1alpha1.LabelKeyOperationPhase] = string(o.Status.Phase)
		}
		if s.app != nil && s.app.UID != "" && !core.HasOwnerReference(o, s.app) {
			ref := core.ApplicationOwnerReference(s.app)
			for _, existing := range o.OwnerReferences {
				if existing.Controller != nil && *existing.Controller {
					ref.Controller = ptr.To(false)
				}
			}
			o.OwnerReferences = append(o.OwnerReferences, ref)
		}
		return nil
	})
	return err
}

// setPhaseLabel mirrors status.phase into the phase label (strict write).
func (s *step) setPhaseLabel(ctx context.Context) error {
	if s.op.Labels[v1alpha1.LabelKeyOperationPhase] == string(s.op.Status.Phase) {
		return nil
	}
	_, err := core.PatchObjectStrict(ctx, s.r.c, s.op, func(o *v1alpha1.Operation) error {
		if o.Labels == nil {
			o.Labels = map[string]string{}
		}
		o.Labels[v1alpha1.LabelKeyOperationPhase] = string(o.Status.Phase)
		return nil
	})
	return err
}

// errTerminalOverwrite refuses a status write that would change a terminal phase.
var errTerminalOverwrite = errors.New("operation already reached a terminal phase")

// patch applies mutate to the Operation status and persists it when changed. The write is
// strict (resourceVersion precondition, no re-read): mutate's values were computed from the
// object this step read, so a conflict abandons the step (core.ErrConflict) instead of
// applying them over a newer state. A terminal phase is never changed.
func (s *step) patch(ctx context.Context, mutate func(st *v1alpha1.OperationStatus)) error {
	_, err := core.PatchStatusStrict(ctx, s.r.c, s.op, func(o *v1alpha1.Operation) error {
		before := o.Status.Phase
		mutate(&o.Status)
		if before.IsTerminal() && o.Status.Phase != before {
			return fmt.Errorf("%s/%s is %s: %w", o.Namespace, o.Name, before, errTerminalOverwrite)
		}
		o.Status.Message = truncate(o.Status.Message, maxMessageLen)
		o.Status.ObservedGeneration = o.Generation
		return nil
	})
	if err != nil {
		return err
	}
	return s.setPhaseLabel(ctx)
}

func (s *step) setCondition(st *v1alpha1.OperationStatus, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            truncate(message, maxMessageLen),
		ObservedGeneration: s.op.Generation,
		LastTransitionTime: s.now(),
	})
}

// isAutomated reports whether the Operation was initiated by automated sync.
func (s *step) isAutomated() bool { return s.op.Spec.InitiatedBy.Automated }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// ownsTerminal reports whether this shard processes a terminal Operation: the shard owning
// its Application's destination, or shard 0 when the Application is gone (reconcileTerminal
// then has nothing to do).
func (r *Reconciler) ownsTerminal(ctx context.Context, op *v1alpha1.Operation) (bool, error) {
	app := &v1alpha1.Application{}
	if err := r.c.Get(ctx, types.NamespacedName{Namespace: op.Namespace, Name: op.Spec.Application}, app); err != nil {
		if apierrors.IsNotFound(err) {
			return r.cfg.OwnsCluster(nil), nil
		}
		return false, fmt.Errorf("get application: %w", err)
	}
	return r.ownsDestination(ctx, app), nil
}

// ownsDestination reports whether this shard owns the Application's destination cluster (an
// unresolvable destination belongs to shard 0, ADR-0008).
func (r *Reconciler) ownsDestination(ctx context.Context, app *v1alpha1.Application) bool {
	var cluster *v1alpha1.Cluster
	if st, err := r.cfg.CurrentSettings(); err == nil {
		if c, err := r.cfg.DestinationCluster(ctx, app.Spec.Destination, st); err == nil {
			cluster = c
		}
	}
	return r.cfg.OwnsCluster(cluster)
}
