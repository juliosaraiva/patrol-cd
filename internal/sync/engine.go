package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	gosync "sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/clock"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/health"
)

// Config tunes an Engine. Use DefaultConfig and override fields.
type Config struct {
	// Manager is the field manager of server-side apply and the target of client-side
	// apply migration. Default DefaultManager.
	Manager string
	// WaveDelay is the pause after a non-final (phase, wave) group was applied before its
	// health is assessed (upstream ARGOCD_SYNC_WAVE_DELAY). The engine never sleeps: Step
	// returns RequeueAfter and a later Step that comes too early keeps waiting. Zero or
	// negative disables the delay. DefaultConfig reads EnvSyncWaveDelay.
	WaveDelay time.Duration
	// PollInterval is the RequeueAfter of a step that waits for health, hooks or deletions.
	// Default DefaultPollInterval.
	PollInterval time.Duration
	// CRDEstablishTimeout bounds the wait for an applied CRD to become Established.
	// Default DefaultCRDEstablishTimeout.
	CRDEstablishTimeout time.Duration
	// Parallelism bounds concurrent API calls within a step. Default 10.
	Parallelism int
	// Clock is the time source. Default the real clock.
	Clock clock.PassiveClock
	// Logger receives structured logs. Default discards. Manifests and Secret data are
	// never logged.
	Logger *slog.Logger
}

// DefaultConfig returns the default configuration with the wave delay read from the
// environment.
func DefaultConfig() Config {
	return Config{
		Manager:             DefaultManager,
		WaveDelay:           WaveDelayFromEnv(os.Getenv),
		PollInterval:        DefaultPollInterval,
		CRDEstablishTimeout: DefaultCRDEstablishTimeout,
		Parallelism:         10,
		Clock:               clock.RealClock{},
	}
}

// WaveDelayFromEnv parses EnvSyncWaveDelay (integer seconds): unset or not an integer →
// DefaultWaveDelay, zero or negative → no delay (SYNC-064).
func WaveDelayFromEnv(getenv func(string) string) time.Duration {
	v := getenv(EnvSyncWaveDelay)
	if v == "" {
		return DefaultWaveDelay
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return DefaultWaveDelay
	}
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// Engine plans and executes sync operations against one destination cluster. It holds no
// per-operation state and is safe for concurrent use.
type Engine struct {
	kube   Kube
	health HealthFunc
	cfg    Config
	log    *slog.Logger
}

// NewEngine returns an Engine. healthFn nil uses internal/health built-in checks only.
func NewEngine(kube Kube, healthFn HealthFunc, cfg Config) (*Engine, error) {
	if kube == nil {
		return nil, fmt.Errorf("%w: kube is required", ErrInvalidInput)
	}
	if healthFn == nil {
		healthFn = func(obj *unstructured.Unstructured) (*health.HealthStatus, error) {
			return health.Assess(obj, nil)
		}
	}
	if cfg.Manager == "" {
		cfg.Manager = DefaultManager
	}
	if cfg.WaveDelay < 0 {
		cfg.WaveDelay = 0
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.CRDEstablishTimeout <= 0 {
		cfg.CRDEstablishTimeout = DefaultCRDEstablishTimeout
	}
	if cfg.Parallelism <= 0 {
		cfg.Parallelism = 10
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.RealClock{}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Engine{kube: kube, health: healthFn, cfg: cfg, log: log}, nil
}

// StepResult is the outcome of one Step or Terminate call. The caller persists Phase,
// Message, Results (replacing status.syncResult.resources), Progress and
// ManagedNamespaceMetadata, and calls Step again after RequeueAfter while Phase is Running
// (zero means immediately; the engine returns zero only after a step that made progress:
// a wave applied without wave delay, or a hook completion to persist before its cleanup).
type StepResult struct {
	// Phase is Running, Succeeded, Failed or Error (Cancelled or Error for Terminate).
	Phase v1alpha1.OperationPhase
	// Message describes the phase, with upstream wording.
	Message string
	// Results are all result rows, one per (resource, sync phase), in first-recorded order.
	Results []v1alpha1.ResourceResult
	// Progress is the execution progress to persist in status.progress.
	Progress *v1alpha1.OperationProgress
	// RequeueAfter is when the caller must call Step again while Phase is Running.
	RequeueAfter time.Duration
	// ManagedNamespaceMetadata is the namespace metadata applied by this sync
	// (status.syncResult.managedNamespaceMetadata).
	ManagedNamespaceMetadata *v1alpha1.ManagedNamespaceMetadata
}

// runState is the tri-state outcome of running a group of tasks.
type runState int

const (
	stateSuccessful runState = iota
	statePending
	stateFailed
)

func mergeState(cur, next runState) runState {
	if next > cur {
		return next
	}
	return cur
}

// resultTransient is the internal result of an operation that failed transiently; it is
// never recorded.
const resultTransient v1alpha1.ResultCode = "transient"

type waveID struct {
	phase v1alpha1.SyncPhase
	wave  int
}

type resourceEntry struct {
	res *metav1.APIResource
	err error
}

// run is the state of one Plan, Step or Terminate call.
type run struct {
	e       *Engine
	ctx     context.Context
	in      *Input
	opts    Options
	log     *slog.Logger
	now     time.Time
	results *resultStore
	prev    map[string]v1alpha1.ResourceResult

	dryRun         bool
	pruneConfirmed bool
	csaManager     string
	terminating    bool

	phase   v1alpha1.OperationPhase
	message string

	resMu    gosync.Mutex
	resCache map[schema.GroupVersionKind]resourceEntry

	// crdKinds are "<group>/<kind>" of CRDs among the targets; crdScopes their scope.
	crdKinds  map[string]bool
	crdScopes map[string]bool

	// tasks of this call, after planning.
	tasks syncTasks
	// valid is false when a task failed validation.
	valid bool
	// sharedMessage is set when a target is owned by another Application.
	sharedMessage string
	// createdNamespaces are namespaces created before the dry run of this step.
	createdNamespaces map[string]bool
	// applied is the (phase, wave) applied by this step.
	applied *waveID

	transientMu  gosync.Mutex
	transientErr error
}

func (e *Engine) newRun(ctx context.Context, in *Input) *run {
	r := &run{
		e:                 e,
		ctx:               ctx,
		in:                in,
		opts:              ParseOptions(in.SyncOptions),
		now:               e.cfg.Clock.Now(),
		results:           newResultStore(in.Previous),
		prev:              make(map[string]v1alpha1.ResourceResult, len(in.Previous)),
		dryRun:            in.DryRun,
		csaManager:        DefaultClientSideApplyMigrationManager,
		resCache:          map[schema.GroupVersionKind]resourceEntry{},
		crdKinds:          map[string]bool{},
		crdScopes:         map[string]bool{},
		createdNamespaces: map[string]bool{},
		valid:             true,
		phase:             v1alpha1.OperationRunning,
	}
	r.log = e.log.With("application", in.App.QualifiedName(), "operation", in.OperationUID)
	for _, p := range in.Previous {
		r.prev[rowKey(p)] = p
	}
	r.pruneConfirmed = DeletionApproved(in.App.Annotations, in.StartedAt)
	if m := in.App.Annotations[v1alpha1.AnnotationKeyClientSideApplyMigrationManager]; m != "" {
		r.csaManager = m
	}
	return r
}

func (r *run) setPhase(phase v1alpha1.OperationPhase, message string) {
	if r.phase != phase || r.message != message {
		r.log.Debug("operation state", "phase", phase, "message", message)
	}
	r.phase, r.message = phase, message
}

func (r *run) set(t *task, status v1alpha1.ResultCode, phase v1alpha1.OperationPhase, message string) {
	r.results.set(t, status, phase, message)
}

func (r *run) started() bool { return r.results.len() > 0 }

// noteTransient remembers the first transient error of the step.
func (r *run) noteTransient(err error) {
	r.transientMu.Lock()
	defer r.transientMu.Unlock()
	if r.transientErr == nil {
		r.transientErr = err
	}
}

func (r *run) transient() error {
	r.transientMu.Lock()
	defer r.transientMu.Unlock()
	return r.transientErr
}

// resource looks up the API resource of gvk once per call, retrying Unauthorized like
// upstream.
func (r *run) resource(gvk schema.GroupVersionKind) (*metav1.APIResource, error) {
	r.resMu.Lock()
	if e, ok := r.resCache[gvk]; ok {
		r.resMu.Unlock()
		return e.res, e.err
	}
	r.resMu.Unlock()
	var res *metav1.APIResource
	err := retry.OnError(retry.DefaultRetry, apierrors.IsUnauthorized, func() error {
		var err error
		res, err = r.e.kube.Resource(r.ctx, gvk)
		return err
	})
	if err != nil && IsTransient(err) {
		return nil, err // not cached
	}
	r.resMu.Lock()
	r.resCache[gvk] = resourceEntry{res: res, err: err}
	r.resMu.Unlock()
	return res, err
}

// scope returns whether gvk is namespaced and whether that is known: the IsNamespaced
// callback, then API discovery, then the CRD manifests of this sync.
func (r *run) scope(gvk schema.GroupVersionKind) (namespaced, known bool) {
	if r.in.IsNamespaced != nil {
		if ns, err := r.in.IsNamespaced(gvk.GroupKind()); err == nil {
			return ns, true
		}
	}
	if res, err := r.resource(gvk); err == nil && res != nil {
		return res.Namespaced, true
	}
	if ns, ok := r.crdScopes[gvk.Group+"/"+gvk.Kind]; ok {
		return ns, true
	}
	return false, false
}

func (r *run) parallel(tasks syncTasks, state runState, fn func(*task) runState) runState {
	if len(tasks) == 0 {
		return state
	}
	var (
		mu  gosync.Mutex
		wg  gosync.WaitGroup
		sem = make(chan struct{}, r.e.cfg.Parallelism)
		out = state
	)
	for _, t := range tasks {
		r.results.reserve(t)
	}
	for _, t := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s := fn(t)
			mu.Lock()
			out = mergeState(out, s)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// result assembles the StepResult of this call.
func (r *run) result(requeue time.Duration) *StepResult {
	rows := r.results.snapshot()
	augmentMessages(r.ctx, r.e.kube, rows)
	res := &StepResult{
		Phase:        r.phase,
		Message:      r.message,
		Results:      rows,
		Progress:     r.progress(),
		RequeueAfter: requeue,
	}
	if r.phase != v1alpha1.OperationRunning {
		res.RequeueAfter = 0
	}
	if r.opts.CreateNamespace && r.in.ManagedNamespaceMetadata != nil {
		res.ManagedNamespaceMetadata = r.in.ManagedNamespaceMetadata.DeepCopy()
	}
	return res
}

func (r *run) progress() *v1alpha1.OperationProgress {
	p := &v1alpha1.OperationProgress{}
	if r.in.Progress != nil {
		p = r.in.Progress.DeepCopy()
	}
	switch {
	case r.applied != nil:
		p.SyncPhase = r.applied.phase
		p.Wave = int64(r.applied.wave)
		p.StartedAt = &metav1.Time{Time: r.now}
	case p.SyncPhase == "":
		for _, t := range r.tasks {
			if !t.completed() && t.phase != v1alpha1.SyncPhaseSyncFail {
				p.SyncPhase = t.phase
				p.Wave = int64(t.wave())
				break
			}
		}
	}
	var total, done int64
	for _, t := range r.tasks {
		if t.phase == v1alpha1.SyncPhaseSyncFail && t.pending() {
			continue
		}
		total++
		if t.completed() {
			done++
		}
	}
	p.TotalTasks, p.CompletedTasks = total, done
	return p
}

// abortFirstStep returns the result of a first step (namespace creation and dry run)
// interrupted by a transient API error. It reports none of the rows recorded by the step:
// persisted rows mean the dry run completed, so a later step would skip it and apply
// unvalidated tasks (SYNC-071). A destination namespace created by the step is then
// treated as pre-existing by the next step.
func (r *run) abortFirstStep(err error) (*StepResult, error) {
	r.results = newResultStore(r.in.Previous)
	for _, t := range r.tasks {
		if _, ok := r.results.get(t.resultKey()); !ok {
			t.status, t.opState, t.message, t.order = "", "", "", 0
		}
	}
	return r.abortTransient(err)
}

// abortTransient returns the result of a step interrupted by a transient API error.
func (r *run) abortTransient(err error) (*StepResult, error) {
	r.setPhase(v1alpha1.OperationRunning, fmt.Sprintf("retrying after transient error: %v", err))
	res := r.result(r.e.cfg.PollInterval)
	if errors.Is(err, ErrTransient) {
		return res, err
	}
	return res, fmt.Errorf("%w: %w", ErrTransient, err)
}
