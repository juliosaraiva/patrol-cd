package hydrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Controller names registered with the manager.
const (
	AppControllerName      = "app-hydration"
	ManifestControllerName = "manifest-hydration"
)

// Hydrator implements the source hydrator. Create it with New (unit tests, custom wiring) or
// Setup (registers both controllers with a manager).
type Hydrator struct {
	d       Deps
	log     *slog.Logger
	metrics *metrics
	queue   *keyQueue

	// forced records hydration keys whose next processing must call the commit-server even
	// when every Application already reports the dry SHA as hydrated (hard hydrate
	// requests). In memory only: after a restart a forced hydration degrades to a normal one.
	forced sync.Map

	// keys tracks, per hydration key, the requests made since its processing last started,
	// and what the latest completed processing handled. A request made from an Application
	// version that processing read or wrote, with the spec it hydrated, for a reason that
	// depends only on the Application (and for the dry revision it rendered), is covered
	// and not processed again: typically an evaluation racing with the processing of its
	// group. In memory only.
	keysMu sync.Mutex
	keys   map[Key]*keyState
}

type keyState struct {
	// requests maps the requesting Applications to their latest request.
	requests map[types.NamespacedName]keyRequest
	// seen are the resourceVersions handled by the latest completed processing, specs the
	// digests of the Application specs it hydrated (validated and rendered), and drySHA the
	// dry revision it rendered ("" when it failed before rendering). A version it wrote may
	// carry a spec changed while it ran, which it did not hydrate: seen alone does not tell.
	seen   map[types.NamespacedName]map[string]struct{}
	specs  map[types.NamespacedName]string
	drySHA string
}

// keyRequest is one request of a hydration key.
type keyRequest struct {
	// rv is the resourceVersion the Application was evaluated at, and spec the digest of
	// its spec at that version (specDigest; "" is never covered).
	rv   string
	spec string
	// drySHA is the dry revision the evaluation resolved ("" when it did not resolve one).
	drySHA string
	// coverable is false when the decision depended on something a completed processing
	// cannot have taken into account (elapsed time, an explicit hard request, a request
	// without an Application).
	coverable bool
}

func (h *Hydrator) state(k Key) *keyState {
	st := h.keys[k]
	if st == nil {
		st = &keyState{requests: map[types.NamespacedName]keyRequest{}}
		h.keys[k] = st
	}
	return st
}

// request enqueues k on behalf of an Application evaluation.
func (h *Hydrator) request(k Key, app types.NamespacedName, r keyRequest) {
	h.keysMu.Lock()
	h.state(k).requests[app] = r
	h.keysMu.Unlock()
	h.queue.add(k)
}

// takeRequests returns and clears the pending requests of k, and whether all of them are
// covered by the latest completed processing: each was made from an Application version
// that processing read or wrote, with the spec that processing hydrated for the
// Application, for a reason that depends on the Application only, and for the dry revision
// it rendered when the evaluation resolved one.
func (h *Hydrator) takeRequests(k Key) (map[types.NamespacedName]keyRequest, bool) {
	h.keysMu.Lock()
	defer h.keysMu.Unlock()
	st := h.state(k)
	reqs := st.requests
	st.requests = map[types.NamespacedName]keyRequest{}
	if len(reqs) == 0 || st.seen == nil {
		return reqs, false
	}
	for app, r := range reqs {
		if !r.coverable || r.rv == "" {
			return reqs, false
		}
		if _, ok := st.seen[app][r.rv]; !ok {
			return reqs, false
		}
		if r.spec == "" || r.spec != st.specs[app] {
			// Typically the spec changed while the processing ran: the version it wrote
			// carries the new spec, but it hydrated the previous one.
			return reqs, false
		}
		if r.drySHA != "" && r.drySHA != st.drySHA {
			return reqs, false
		}
	}
	return reqs, true
}

// restoreRequests puts back requests of a processing that did not complete; newer requests
// made meanwhile win.
func (h *Hydrator) restoreRequests(k Key, reqs map[types.NamespacedName]keyRequest) {
	h.keysMu.Lock()
	defer h.keysMu.Unlock()
	st := h.state(k)
	for app, r := range reqs {
		if _, ok := st.requests[app]; !ok {
			st.requests[app] = r
		}
	}
}

// completed records the versions a completed processing of k read and wrote, the specs it
// hydrated and the dry revision it rendered.
func (h *Hydrator) completed(k Key, seen versions, specs map[types.NamespacedName]string, drySHA string) {
	h.keysMu.Lock()
	defer h.keysMu.Unlock()
	st := h.state(k)
	st.seen, st.specs, st.drySHA = seen, specs, drySHA
}

// forget drops the state of a key no Application uses any more.
func (h *Hydrator) forget(k Key) {
	h.keysMu.Lock()
	defer h.keysMu.Unlock()
	if st := h.keys[k]; st != nil && len(st.requests) == 0 {
		delete(h.keys, k)
	}
}

// versions collects the resourceVersions a processing handled.
type versions map[types.NamespacedName]map[string]struct{}

func (v versions) add(app *v1alpha1.Application) {
	nn := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}
	if v[nn] == nil {
		v[nn] = map[string]struct{}{}
	}
	v[nn][app.ResourceVersion] = struct{}{}
}

// specDigest identifies app's spec for request coverage ("" when it cannot be encoded,
// which is never covered).
func specDigest(app *v1alpha1.Application) string {
	data, err := json.Marshal(&app.Spec)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// accept reports whether this hydrator handles app.
func (h *Hydrator) accept(app *v1alpha1.Application) bool {
	if app == nil || app.Spec.SourceHydrator == nil {
		return false
	}
	return h.d.AppFilter == nil || h.d.AppFilter(app)
}

// pollInterval is the dry-branch polling period (0: polling disabled).
func (h *Hydrator) pollInterval() time.Duration {
	switch {
	case h.d.PollInterval > 0:
		return h.d.PollInterval
	case h.d.PollInterval < 0:
		return 0
	}
	if st := h.d.Settings.Current(); st != nil {
		return st.Reconciliation().Timeout
	}
	return defaultPollInterval
}

// staleTimeout is how long an Application may stay Hydrating before its key is re-enqueued
// (upstream statusRefreshTimeout).
func (h *Hydrator) staleTimeout() time.Duration {
	if h.d.StaleTimeout > 0 {
		return h.d.StaleTimeout
	}
	if st := h.d.Settings.Current(); st != nil && st.Reconciliation().Timeout > 0 {
		return st.Reconciliation().Timeout
	}
	return defaultPollInterval
}

// defaultPollInterval applies when neither the parameters nor the settings provide one
// (upstream default timeout.reconciliation).
const defaultPollInterval = 120 * time.Second

// New validates deps and returns a Hydrator that is not registered with any manager. Keys
// enqueued before a manager starts the manifest-hydration controller are buffered.
func New(deps Deps) (*Hydrator, error) {
	if err := deps.normalize(); err != nil {
		return nil, err
	}
	m, err := newMetrics(deps.Registry)
	if err != nil {
		return nil, err
	}
	return &Hydrator{d: deps, log: deps.Logger, metrics: m, queue: newKeyQueue(), keys: map[Key]*keyState{}}, nil
}

// Setup registers the hydrator with mgr: the Application field index by hydration key, the
// app-hydration controller (Applications with spec.sourceHydrator) and the
// manifest-hydration controller (hydration keys). Both controllers are leader-elected with
// the manager. Client and APIReader default to the manager's. When deps.Enabled is false
// (hydrator.enabled), Setup registers nothing and returns (nil, nil).
func Setup(ctx context.Context, mgr manager.Manager, deps Deps) (*Hydrator, error) {
	if !deps.Enabled {
		return nil, nil
	}
	if mgr == nil {
		return nil, errors.New("hydrator: manager is required")
	}
	if deps.Client == nil {
		deps.Client = mgr.GetClient()
	}
	if deps.APIReader == nil {
		deps.APIReader = mgr.GetAPIReader()
	}
	h, err := New(deps)
	if err != nil {
		return nil, err
	}
	if err := mgr.GetFieldIndexer().IndexField(ctx, &v1alpha1.Application{}, keyIndexField, indexByKey); err != nil {
		return nil, fmt.Errorf("hydrator: index applications by hydration key: %w", err)
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named(AppControllerName).
		For(&v1alpha1.Application{}, builder.WithPredicates(appPredicate(h.accept))).
		WithOptions(controller.Options{MaxConcurrentReconciles: h.d.AppProcessors}).
		Complete(reconcile.Func(h.ReconcileApp)); err != nil {
		return nil, fmt.Errorf("hydrator: create %s controller: %w", AppControllerName, err)
	}
	c, err := controller.NewTyped(ManifestControllerName, mgr, controller.TypedOptions[Key]{
		Reconciler:              reconcile.TypedFunc[Key](h.ReconcileKey),
		MaxConcurrentReconciles: h.d.Processors,
	})
	if err != nil {
		return nil, fmt.Errorf("hydrator: create %s controller: %w", ManifestControllerName, err)
	}
	if err := c.Watch(h.queue); err != nil {
		return nil, fmt.Errorf("hydrator: watch hydration keys: %w", err)
	}
	h.log.Info("hydrator: source hydrator enabled",
		slog.Int("hydrationProcessors", h.d.Processors), slog.Int("appProcessors", h.d.AppProcessors))
	return h, nil
}

// Enqueue requests processing of a hydration key (for callers such as webhooks that know
// the key; Applications are normally enqueued by the app-hydration controller).
func (h *Hydrator) Enqueue(k Key) { h.request(k, types.NamespacedName{}, keyRequest{}) }
