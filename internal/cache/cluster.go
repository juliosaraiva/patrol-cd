package cache

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	authzclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

const (
	// queueSize bounds watch events waiting to be applied; watchers block when it is full.
	queueSize = 1024
	// maxBatch bounds the number of queued items applied under one write lock.
	maxBatch = 1000
	// discoveryTimeout bounds discovery/OpenAPI/SSAR requests when the REST config sets no
	// timeout (watch requests never get a client timeout).
	discoveryTimeout = 32 * time.Second
	// defaultQPS and defaultBurst replace client-go's 5/10 when the REST config leaves them
	// unset; a full sync issues one list per API (upstream Argo CD uses the same values).
	defaultQPS   = 50
	defaultBurst = 100
	// maxSyncAttempts bounds full-sync retries when an Invalidate supersedes a sync.
	maxSyncAttempts = 3
)

// ErrNotSynced is returned by operations that need discovery data before the first
// successful sync.
var ErrNotSynced = errors.New("cluster cache not synced")

var endpointsGK = schema.GroupKind{Kind: kindEndpoints}

// Option configures a Cluster.
type Option func(*options)

type options struct {
	logger       *slog.Logger
	wrapLW       func(api APIResourceInfo, namespace string, lw ListerWatcher) ListerWatcher
	watchBackoff wait.Backoff
	minWatch     time.Duration
}

// WithLogger sets the logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.logger = l
		}
	}
}

// WithListerWatcherWrapper wraps every ListerWatcher the cache creates. It exists for fault
// injection in tests.
func WithListerWatcherWrapper(fn func(api APIResourceInfo, namespace string, lw ListerWatcher) ListerWatcher) Option {
	return func(o *options) { o.wrapLW = fn }
}

type clients struct {
	disco discovery.DiscoveryInterface
	dyn   dynamic.Interface
	ssar  authzclient.SelfSubjectAccessReviewInterface
}

func newClients(cfg *rest.Config) (*clients, error) {
	cfg = rest.CopyConfig(cfg)
	cfg.WarningHandler = rest.NoWarnings{}
	if cfg.QPS == 0 && cfg.RateLimiter == nil {
		cfg.QPS, cfg.Burst = defaultQPS, defaultBurst
	}
	bounded := rest.CopyConfig(cfg)
	if bounded.Timeout == 0 {
		bounded.Timeout = discoveryTimeout
	}
	disco, err := discovery.NewDiscoveryClientForConfig(bounded)
	if err != nil {
		return nil, fmt.Errorf("create discovery client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}
	authz, err := authzclient.NewForConfig(bounded)
	if err != nil {
		return nil, fmt.Errorf("create authorization client: %w", err)
	}
	return &clients{disco: disco, dyn: dyn, ssar: authz.SelfSubjectAccessReviews()}, nil
}

// apiWatch is the registration of one watched API (all of its scopes).
type apiWatch struct {
	info   APIResourceInfo
	token  uint64
	cancel context.CancelFunc
	dead   bool
}

type queueKind int

const (
	qEvent queueKind = iota
	qReplace
	qRemoveAPI
	qNotify
)

// queueItem is one ordered mutation for the processor.
type queueItem struct {
	kind      queueKind
	token     uint64
	gk        schema.GroupKind
	eventType watch.EventType
	key       ResourceKey
	res       *Resource
	namespace string
	items     []*Resource
	changes   []change
}

type deniedKey struct {
	gk        schema.GroupKind
	namespace string
}

type syncStatus struct {
	attempted bool
	syncTime  time.Time
	syncErr   error
	epoch     uint64
	bgResync  bool
	closed    bool
	// flight is the foreground sync in progress, shared by every EnsureSynced caller.
	flight *syncFlight
}

// syncFlight is one foreground full sync. It runs under the cache's root context, so a
// caller that stops waiting does not cancel it.
type syncFlight struct {
	done chan struct{}
	// err and epoch (the Invalidate epoch the result is valid for) are written before done
	// is closed.
	err   error
	epoch uint64
}

// syncDecision is what the recorded sync status implies for EnsureSynced.
type syncDecision int

const (
	syncNeeded    syncDecision = iota // never synced, invalidated, or failed beyond the retry window
	syncCurrent                       // synced for the current epoch
	syncFailed                        // failed within the retry window: report the recorded error
	syncResyncDue                     // synced, and the periodic resync is due
)

// Cluster is the live-state cache of one cluster. Create it with New; it implements
// ClusterCache.
type Cluster struct {
	log  *slog.Logger
	opts options

	rootCtx    context.Context
	rootCancel context.CancelFunc
	wg         sync.WaitGroup
	closeOnce  sync.Once

	// syncSem serializes full syncs and API refreshes.
	syncSem chan struct{}
	// epoch increments on Invalidate; a sync result is valid only for its epoch.
	epoch atomic.Uint64

	mu         sync.RWMutex
	cfg        *rest.Config
	settings   Settings
	clients    *clients
	clientsErr error
	genCtx     context.Context
	genCancel  context.CancelFunc
	graph      *graph
	hasState   bool
	disc       *discoveryResult
	apis       map[schema.GroupKind]*apiWatch
	nextToken  uint64
	denied     map[deniedKey]UnavailableAPI
	pending    []queueItem
	lister     *lister
	closed     bool

	statusMu sync.Mutex
	status   syncStatus

	queue     chan queueItem
	wake      chan struct{}
	refreshCh chan struct{}

	refreshMu     sync.Mutex
	refreshExpect map[schema.GroupKind]bool

	hmu             sync.Mutex
	nextHandlerID   uint64
	updatedHandlers map[uint64]ResourceUpdatedHandler
	eventHandlers   map[uint64]EventHandler
	processHandlers map[uint64]ProcessEventsHandler

	activeWatches atomic.Int64
	oapi          openAPICache
}

var _ ClusterCache = (*Cluster)(nil)

// New creates a cluster cache. It does not contact the cluster until EnsureSynced. Close
// releases its goroutines.
func New(cfg *rest.Config, settings Settings, opts ...Option) (*Cluster, error) {
	if cfg == nil {
		return nil, errors.New("cluster cache: nil REST config")
	}
	o := options{
		logger:       slog.Default(),
		watchBackoff: wait.Backoff{Duration: time.Second, Factor: 2, Jitter: 0.5, Steps: 1 << 30, Cap: 30 * time.Second},
		minWatch:     time.Second,
	}
	for _, opt := range opts {
		opt(&o)
	}
	cl, err := newClients(cfg)
	if err != nil {
		return nil, err
	}
	c := &Cluster{
		log:             o.logger.With("component", "cluster-cache", "server", cfg.Host),
		opts:            o,
		syncSem:         make(chan struct{}, 1),
		cfg:             rest.CopyConfig(cfg),
		settings:        settings.normalized(),
		clients:         cl,
		graph:           newGraph(),
		apis:            map[schema.GroupKind]*apiWatch{},
		denied:          map[deniedKey]UnavailableAPI{},
		queue:           make(chan queueItem, queueSize),
		wake:            make(chan struct{}, 1),
		refreshCh:       make(chan struct{}, 1),
		refreshExpect:   map[schema.GroupKind]bool{},
		updatedHandlers: map[uint64]ResourceUpdatedHandler{},
		eventHandlers:   map[uint64]EventHandler{},
		processHandlers: map[uint64]ProcessEventsHandler{},
	}
	c.rootCtx, c.rootCancel = context.WithCancel(context.Background())
	c.genCtx, c.genCancel = context.WithCancel(c.rootCtx)
	c.wg.Add(2)
	go c.processLoop()
	go c.refreshLoop()
	return c, nil
}

// Close stops all watches and background work and waits for them to exit. It must not be
// called from a handler registered on this cache.
func (c *Cluster) Close() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.statusMu.Lock()
		c.status.closed = true
		c.statusMu.Unlock()
		c.rootCancel()
		c.wg.Wait()
	})
}

// EnsureSynced implements ClusterCache.
//
// The sync itself runs on a goroutine owned by the cache, under the cache's own context, and
// is shared by concurrent callers. ctx only bounds how long this caller waits: when it ends,
// EnsureSynced returns its error while the sync continues and records its outcome for later
// calls. A context that is already done when no sync result is available starts nothing.
func (c *Cluster) EnsureSynced(ctx context.Context) error {
	for {
		f, epoch, err := c.syncOrJoin(ctx)
		if f == nil {
			return err
		}
		select {
		case <-f.done:
			if f.err != nil || f.epoch >= epoch {
				return f.err
			}
			// The joined sync completed for an epoch older than this call (Invalidate ran
			// between its swap and its completion): evaluate again, which starts a new sync.
		case <-ctx.Done():
			return fmt.Errorf("wait for cluster cache sync: %w", ctx.Err())
		}
	}
}

// syncSettings returns the settings that drive syncDecision.
func (c *Cluster) syncSettings() (retry, period time.Duration) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.settings.SyncRetryTimeout, c.settings.ClusterResyncPeriod
}

// decideLocked evaluates the recorded status for epoch; call with statusMu held.
func (c *Cluster) decideLocked(epoch uint64, retry, period time.Duration) syncDecision {
	st := &c.status
	switch {
	case !st.attempted, st.epoch != epoch: // never synced, or invalidated since
		return syncNeeded
	case st.syncErr != nil:
		if retry > 0 && time.Since(st.syncTime) < retry {
			return syncFailed
		}
		return syncNeeded
	case period > 0 && time.Since(st.syncTime) >= period:
		return syncResyncDue
	}
	return syncCurrent
}

// syncOrJoin answers EnsureSynced from the recorded status when it can (starting a due
// periodic resync in the background). Otherwise it returns the running foreground sync,
// starting one if none runs, and the epoch the caller observed.
func (c *Cluster) syncOrJoin(ctx context.Context) (*syncFlight, uint64, error) {
	retry, period := c.syncSettings()
	epoch := c.epoch.Load()

	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	st := &c.status
	if st.closed {
		return nil, epoch, ErrClosed
	}
	switch c.decideLocked(epoch, retry, period) {
	case syncCurrent:
		return nil, epoch, nil
	case syncFailed:
		return nil, epoch, st.syncErr
	case syncResyncDue:
		if !st.bgResync {
			st.bgResync = true
			c.wg.Add(1) // ordered before Close's Wait: Close sets st.closed under statusMu first
			go c.backgroundResync()
		}
		return nil, epoch, nil
	}
	if st.flight != nil {
		return st.flight, epoch, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, epoch, fmt.Errorf("wait for cluster cache sync: %w", err)
	}
	f := &syncFlight{done: make(chan struct{})}
	st.flight = f
	c.wg.Add(1)
	go c.runFlight(f)
	return f, epoch, nil
}

// runFlight performs the foreground sync f. Its result is published under statusMu together
// with the recorded status, so a caller either joins f before it completes or sees the
// recorded status.
func (c *Cluster) runFlight(f *syncFlight) {
	defer c.wg.Done()
	defer close(f.done)
	select {
	case c.syncSem <- struct{}{}:
	case <-c.rootCtx.Done():
		c.finishFlight(f, ErrClosed)
		return
	}
	defer func() { <-c.syncSem }()

	// A background resync may have completed while this flight waited for syncSem.
	retry, period := c.syncSettings()
	epoch := c.epoch.Load()
	c.statusMu.Lock()
	switch c.decideLocked(epoch, retry, period) {
	case syncCurrent, syncResyncDue:
		f.err, f.epoch = nil, epoch
		c.status.flight = nil
		c.statusMu.Unlock()
		return
	case syncFailed:
		f.err, f.epoch = c.status.syncErr, epoch
		c.status.flight = nil
		c.statusMu.Unlock()
		return
	}
	c.statusMu.Unlock()
	c.syncLocked(f)
}

// finishFlight publishes err as the result of f without recording a sync.
func (c *Cluster) finishFlight(f *syncFlight, err error) {
	c.statusMu.Lock()
	f.err = err
	if c.status.flight == f {
		c.status.flight = nil
	}
	c.statusMu.Unlock()
}

func (c *Cluster) backgroundResync() {
	defer c.wg.Done()
	defer func() {
		c.statusMu.Lock()
		c.status.bgResync = false
		c.statusMu.Unlock()
	}()
	select {
	case c.syncSem <- struct{}{}:
	case <-c.rootCtx.Done():
		return
	}
	defer func() { <-c.syncSem }()
	c.log.Info("starting periodic cluster resync")
	c.syncLocked(nil)
}

// syncLocked runs a full sync under the cache's root context and records its outcome; when
// f is not nil, the outcome is also published as f's result. The caller holds syncSem.
func (c *Cluster) syncLocked(f *syncFlight) {
	start := time.Now()
	var (
		err   error
		epoch uint64
	)
	for range maxSyncAttempts {
		epoch, err = c.fullSync(c.rootCtx)
		if !errors.Is(err, errSuperseded) {
			break
		}
	}
	if c.rootCtx.Err() != nil {
		if f != nil {
			c.finishFlight(f, ErrClosed)
		}
		return
	}
	c.mu.RLock()
	server, metrics := c.cfg.Host, c.settings.Metrics
	c.mu.RUnlock()
	err = classifySyncError(server, err)
	c.statusMu.Lock()
	c.status.attempted = true
	c.status.syncTime = time.Now()
	c.status.syncErr = err
	c.status.epoch = epoch
	if f != nil {
		f.err, f.epoch = err, epoch
		if c.status.flight == f {
			c.status.flight = nil
		}
	}
	c.statusMu.Unlock()
	d := time.Since(start)
	if metrics.SyncCompleted != nil {
		metrics.SyncCompleted(d, err)
	}
	if err != nil {
		c.log.Warn("cluster cache sync failed", "duration", d, "error", err)
		return
	}
	c.log.Info("cluster cache synced", "duration", d, "resources", c.GetClusterInfo().ResourcesCount)
}

type scopeResult struct {
	api    APIResourceInfo
	ns     string
	items  []*Resource
	rv     string
	denied *UnavailableAPI
	gone   bool
}

// fullSync discovers APIs, lists every in-scope API into a staging graph, swaps it in and
// restarts all watches. Readers keep using the previous state until the swap.
func (c *Cluster) fullSync(ctx context.Context) (uint64, error) {
	c.mu.RLock()
	settings, cl, clErr, server := c.settings, c.clients, c.clientsErr, c.cfg.Host
	c.mu.RUnlock()
	epoch := c.epoch.Load()
	if clErr != nil {
		return epoch, clErr
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.rootCtx, cancel)
	defer stop()

	disc, err := discoverAPIs(cl.disco, &settings, server)
	if err != nil {
		return epoch, err
	}
	sem := semaphore.NewWeighted(int64(settings.ListSemaphore))
	lst := newLister(&settings, sem)
	convert := func(un *unstructured.Unstructured) *Resource { return newResource(un, &settings) }

	var (
		resMu   sync.Mutex
		results []scopeResult
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(settings.ListSemaphore) // bounds goroutines as well as concurrent lists
	for _, api := range disc.watch {
		for _, ns := range settings.scopes(api.Meta.Namespaced) {
			g.Go(func() error {
				r := scopeResult{api: api, ns: ns}
				items, rv, err := lst.list(gctx, c.listerWatcher(cl, api, ns), convert)
				if err != nil {
					denied, gone, err := c.classifyListError(gctx, cl, &settings, api, ns, err)
					if err != nil {
						return fmt.Errorf("list %s: %w", scopeName(api, ns), err)
					}
					r.denied, r.gone = denied, gone
				} else {
					r.items, r.rv = items, rv
				}
				resMu.Lock()
				results = append(results, r)
				resMu.Unlock()
				return nil
			})
		}
	}
	if err := g.Wait(); err != nil {
		return epoch, err
	}
	slices.SortFunc(results, func(a, b scopeResult) int {
		return cmp.Or(compareAPIInfo(a.api, b.api), cmp.Compare(a.ns, b.ns))
	})

	staging := newGraph()
	denied := map[deniedKey]UnavailableAPI{}
	type plan struct {
		api    APIResourceInfo
		scopes []scopeResult
	}
	var plans []*plan
	for _, r := range results {
		switch {
		case r.gone:
			continue
		case r.denied != nil:
			denied[deniedKey{r.api.GroupKind, r.ns}] = *r.denied
			continue
		}
		for _, res := range r.items {
			staging.upsert(res)
		}
		if n := len(plans); n > 0 && plans[n-1].api.GroupKind == r.api.GroupKind {
			plans[n-1].scopes = append(plans[n-1].scopes, r)
		} else {
			plans = append(plans, &plan{api: r.api, scopes: []scopeResult{r}})
		}
	}
	staging.seal()

	type start struct {
		w   *watcher
		ctx context.Context
		rv  string
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return epoch, ErrClosed
	}
	if c.epoch.Load() != epoch {
		c.mu.Unlock()
		return epoch, errSuperseded
	}
	c.genCancel()
	c.genCtx, c.genCancel = context.WithCancel(c.rootCtx)
	var changes []change
	if c.hasState {
		changes = diffGraphs(c.graph, staging)
	}
	c.graph = staging
	c.hasState = true
	c.disc = disc
	c.denied = denied
	c.lister = lst
	c.pending = nil
	if len(changes) > 0 {
		c.pending = append(c.pending, queueItem{kind: qNotify, changes: changes})
	}
	c.apis = make(map[schema.GroupKind]*apiWatch, len(plans))
	var starts []start
	for _, p := range plans {
		w := c.registerAPI(p.api)
		var actx context.Context
		actx, w.cancel = context.WithCancel(c.genCtx)
		for _, s := range p.scopes {
			starts = append(starts, start{w: c.newWatcher(cl, &settings, lst, p.api, s.ns, w.token), ctx: actx, rv: s.rv})
		}
	}
	c.wg.Add(len(starts))
	c.mu.Unlock()

	c.wakeProcessor()
	for _, s := range starts {
		c.spawnWatcher(s.ctx, s.w, s.rv, settings.Metrics)
	}
	c.oapi.reset()
	return epoch, nil
}

// registerAPI allocates a watch registration; call with c.mu held.
func (c *Cluster) registerAPI(api APIResourceInfo) *apiWatch {
	c.nextToken++
	w := &apiWatch{info: api, token: c.nextToken}
	c.apis[api.GroupKind] = w
	return w
}

// classifyListError decides whether a failed list skips the scope (RBAC, API removed) or
// fails the operation.
func (c *Cluster) classifyListError(ctx context.Context, cl *clients, s *Settings, api APIResourceInfo, ns string, err error) (*UnavailableAPI, bool, error) {
	if apierrors.IsNotFound(err) {
		return nil, true, nil // the API disappeared after discovery
	}
	if !isAccessDenied(err) {
		return nil, false, err
	}
	switch s.RespectRBAC {
	case RespectRBACNone:
		return nil, false, err
	case RespectRBACStrict:
		allowed, serr := checkAccess(ctx, cl.ssar, api, ns, "list")
		if serr != nil {
			return nil, false, fmt.Errorf("%w (list error: %v)", serr, err)
		}
		if allowed {
			return nil, false, err
		}
	}
	c.log.Info("skipping API the credentials cannot list", "group", api.GroupKind.Group, "kind", api.GroupKind.Kind, "namespace", ns)
	return &UnavailableAPI{GroupKind: api.GroupKind, Namespace: ns, Reason: ReasonListForbidden, Message: err.Error()}, false, nil
}

func scopeName(api APIResourceInfo, ns string) string {
	if ns == "" {
		return api.GroupVersionResource.String()
	}
	return api.GroupVersionResource.String() + " in namespace " + ns
}

func (c *Cluster) listerWatcher(cl *clients, api APIResourceInfo, ns string) ListerWatcher {
	ri := cl.dyn.Resource(api.GroupVersionResource)
	var lw ListerWatcher = ri
	if ns != "" {
		lw = ri.Namespace(ns)
	}
	if c.opts.wrapLW != nil {
		lw = c.opts.wrapLW(api, ns, lw)
	}
	return lw
}

func (c *Cluster) newWatcher(cl *clients, s *Settings, lst *lister, api APIResourceInfo, ns string, token uint64) *watcher {
	denied := s.WatchResyncTimeout
	if denied <= 0 {
		denied = DefaultWatchResyncTimeout
	}
	return &watcher{
		target:           watchTarget{api: api, namespace: ns},
		lw:               c.listerWatcher(cl, api, ns),
		lister:           lst,
		convert:          func(un *unstructured.Unstructured) *Resource { return newResource(un, s) },
		sink:             &clusterSink{c: c, token: token, metrics: s.Metrics},
		log:              c.log.With("group", api.GroupKind.Group, "kind", api.GroupKind.Kind, "namespace", ns),
		resync:           s.WatchResyncTimeout,
		deniedRetry:      denied,
		backoff:          c.opts.watchBackoff,
		minWatchDuration: c.opts.minWatch,
	}
}

// spawnWatcher starts w in a goroutine; the caller has already done c.wg.Add(1). The active
// watch count is updated synchronously so that GetClusterInfo reflects it immediately.
func (c *Cluster) spawnWatcher(ctx context.Context, w *watcher, rv string, m Metrics) {
	n := c.activeWatches.Add(1)
	if m.ActiveWatches != nil {
		m.ActiveWatches(int(n))
	}
	go func() {
		defer c.wg.Done()
		defer func() {
			n := c.activeWatches.Add(-1)
			if m.ActiveWatches != nil {
				m.ActiveWatches(int(n))
			}
		}()
		w.run(ctx, rv)
	}()
}

// Invalidate implements ClusterCache.
func (c *Cluster) Invalidate(opts ...InvalidateOption) {
	var ic invalidateConfig
	for _, o := range opts {
		o(&ic)
	}
	c.mu.Lock()
	if ic.settings != nil {
		c.settings = ic.settings.normalized()
	}
	if ic.config != nil {
		c.cfg = rest.CopyConfig(ic.config)
		c.clients, c.clientsErr = newClients(c.cfg)
	}
	c.epoch.Add(1)
	c.genCancel()
	c.genCtx, c.genCancel = context.WithCancel(c.rootCtx)
	c.apis = map[schema.GroupKind]*apiWatch{}
	c.denied = map[deniedKey]UnavailableAPI{}
	c.pending = nil
	c.mu.Unlock()
	c.oapi.reset()
	c.log.Info("cluster cache invalidated")
}

// --- event processing -------------------------------------------------------------------

func (c *Cluster) wakeProcessor() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Cluster) enqueue(ctx context.Context, it queueItem) error {
	select {
	case c.queue <- it:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Cluster) batchInterval() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.settings.EventProcessingInterval
}

// processLoop applies queued changes in batches and dispatches handlers.
func (c *Cluster) processLoop() {
	defer c.wg.Done()
	ctx := c.rootCtx
	batch := make([]queueItem, 0, 64)
	for {
		select {
		case <-ctx.Done():
			return
		case it := <-c.queue:
			batch = append(batch, it)
		case <-c.wake:
		}
		if interval := c.batchInterval(); interval > 0 && len(batch) > 0 {
			t := time.NewTimer(interval)
		collect:
			for len(batch) < maxBatch {
				select {
				case it := <-c.queue:
					batch = append(batch, it)
				case <-t.C:
					break collect
				case <-ctx.Done():
					t.Stop()
					return
				}
			}
			t.Stop()
		} else {
		drain:
			for len(batch) < maxBatch {
				select {
				case it := <-c.queue:
					batch = append(batch, it)
				default:
					break drain
				}
			}
		}
		c.flush(batch)
		clear(batch)
		batch = batch[:0]
	}
}

// flush applies pending items and batch under the write lock, then calls handlers.
func (c *Cluster) flush(batch []queueItem) {
	start := time.Now()
	var (
		changes  []change
		applied  int
		fillFrom []int // indexes of changes whose namespace snapshot must be filled
	)
	c.mu.Lock()
	pending := c.pending
	c.pending = nil
	metrics := c.settings.Metrics
	add := func(ch ...change) {
		for _, x := range ch {
			fillFrom = append(fillFrom, len(changes))
			changes = append(changes, x)
		}
	}
	apply := func(it queueItem) {
		switch it.kind {
		case qNotify:
			changes = append(changes, it.changes...)
		case qRemoveAPI:
			applied++
			add(c.graph.removeGK(it.gk)...)
		case qReplace:
			if !c.tokenValid(it) {
				return
			}
			applied++
			add(c.graph.replaceScope(it.gk, it.namespace, it.items)...)
		case qEvent:
			if !c.tokenValid(it) {
				return
			}
			applied++
			if it.eventType == watch.Deleted {
				if old := c.graph.remove(it.key); old != nil {
					add(change{oldRes: old})
				}
				return
			}
			old := c.graph.upsert(it.res)
			add(change{newRes: c.graph.get(it.key), oldRes: old})
		}
	}
	for _, it := range pending {
		apply(it)
	}
	for _, it := range batch {
		apply(it)
	}
	c.graph.seal()
	for _, i := range fillFrom {
		r := changes[i].newRes
		if r == nil {
			r = changes[i].oldRes
		}
		changes[i].ns = c.graph.ns[r.Ref.Namespace]
	}
	c.mu.Unlock()

	if len(changes) > 0 {
		handlers := c.resourceUpdatedHandlers()
		for _, ch := range changes {
			for _, h := range handlers {
				c.safeCall("resource updated handler", func() { h(ch.newRes, ch.oldRes, ch.ns) })
			}
		}
	}
	if applied == 0 && len(changes) == 0 {
		return
	}
	d := time.Since(start)
	for _, h := range c.processEventsHandlers() {
		c.safeCall("process events handler", func() { h(d, applied) })
	}
	if metrics.EventsProcessed != nil {
		metrics.EventsProcessed(applied, d)
	}
}

// tokenValid reports whether it comes from the current, live registration of its API. Items
// of a registration whose API was reported gone are dropped: its objects were removed.
func (c *Cluster) tokenValid(it queueItem) bool {
	w := c.apis[it.gk]
	return w != nil && w.token == it.token && !w.dead
}

// safeCall runs a caller-provided handler, converting a panic into an error log so that the
// processor keeps running.
func (c *Cluster) safeCall(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("handler panicked", "handler", what, "panic", fmt.Sprint(r))
		}
	}()
	fn()
}

// clusterSink connects watchers of one API registration to the cluster.
type clusterSink struct {
	c       *Cluster
	token   uint64
	metrics Metrics
}

func (s *clusterSink) raw(t watch.EventType, un *unstructured.Unstructured) {
	for _, h := range s.c.eventHandlerList() {
		s.c.safeCall("event handler", func() { h(t, un) })
	}
}

func (s *clusterSink) event(ctx context.Context, t watchTarget, et watch.EventType, key ResourceKey, res *Resource, un *unstructured.Unstructured) error {
	gk := t.api.GroupKind
	// Endpoints churn constantly and their updates never matter to applications; like
	// upstream, modifications are not applied (additions and deletions are).
	if et == watch.Modified && gk == endpointsGK {
		return nil
	}
	switch {
	case gk.Group == groupAPIExtensions && gk.Kind == kindCRD:
		group, _, _ := unstructured.NestedString(un.Object, "spec", "group")
		kind, _, _ := unstructured.NestedString(un.Object, "spec", "names", "kind")
		if kind != "" {
			s.c.requestRefresh(map[schema.GroupKind]bool{{Group: group, Kind: kind}: et != watch.Deleted})
		}
	case gk.Group == groupAPIRegistry && gk.Kind == kindAPIService:
		// Like upstream: react to deletions and to the APIService becoming Available, and
		// retry until discovery serves its group (aggregated discovery can lag).
		switch {
		case et == watch.Deleted:
			s.c.requestRefresh(nil)
		case apiServiceAvailable(un):
			group, _, _ := unstructured.NestedString(un.Object, "spec", "group")
			s.c.requestRefresh(map[schema.GroupKind]bool{{Group: group}: true})
		}
	}
	return s.c.enqueue(ctx, queueItem{kind: qEvent, token: s.token, gk: gk, eventType: et, key: key, res: res})
}

func (s *clusterSink) replace(ctx context.Context, t watchTarget, items []*Resource) error {
	return s.c.enqueue(ctx, queueItem{kind: qReplace, token: s.token, gk: t.api.GroupKind, namespace: t.namespace, items: items})
}

func (s *clusterSink) relisted(t watchTarget, reason string) {
	if s.metrics.Relisted != nil {
		s.metrics.Relisted(t.api.GroupKind, t.namespace, reason)
	}
}

func (s *clusterSink) gone(t watchTarget) {
	c := s.c
	gk := t.api.GroupKind
	c.mu.Lock()
	if w := c.apis[gk]; w != nil && w.token == s.token && !w.dead {
		// Like upstream, drop the API's objects as soon as the server reports it gone. Later
		// items of this registration are dropped (tokenValid), and the refresh below lists
		// the API again under a new registration if it is still served.
		w.dead = true
		c.pending = append(c.pending, queueItem{kind: qRemoveAPI, gk: gk})
	}
	c.mu.Unlock()
	c.wakeProcessor()
	c.log.Info("API no longer served", "group", t.api.GroupKind.Group, "kind", t.api.GroupKind.Kind)
	c.requestRefresh(map[schema.GroupKind]bool{t.api.GroupKind: false})
}

func (s *clusterSink) accessDenied(t watchTarget, verb string, err error) {
	reason := ReasonWatchForbidden
	if verb == "list" {
		reason = ReasonListForbidden
	}
	c := s.c
	c.mu.Lock()
	if w := c.apis[t.api.GroupKind]; w != nil && w.token == s.token {
		c.denied[deniedKey{t.api.GroupKind, t.namespace}] = UnavailableAPI{
			GroupKind: t.api.GroupKind, Namespace: t.namespace, Reason: reason, Message: err.Error(),
		}
	}
	c.mu.Unlock()
	c.log.Warn("API access denied; falling back to periodic relists", "verb", verb,
		"group", t.api.GroupKind.Group, "kind", t.api.GroupKind.Kind, "namespace", t.namespace)
}

func (s *clusterSink) available(t watchTarget) {
	c := s.c
	k := deniedKey{t.api.GroupKind, t.namespace}
	c.mu.Lock()
	if w := c.apis[t.api.GroupKind]; w != nil && w.token == s.token {
		delete(c.denied, k)
	}
	c.mu.Unlock()
}

// --- API refresh (CRD / APIService changes) ----------------------------------------------

// apiServiceAvailable reports whether an APIService has condition Available=True.
func apiServiceAvailable(un *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(un.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if ok && m["type"] == "Available" {
			return m["status"] == "True"
		}
	}
	return false
}

// requestRefresh schedules a discovery refresh. expect maps group/kinds (or whole groups,
// with an empty Kind) to whether they should be served; the refresh is retried with backoff
// until discovery agrees (aggregated discovery can lag behind CRD establishment and
// APIService availability).
func (c *Cluster) requestRefresh(expect map[schema.GroupKind]bool) {
	c.refreshMu.Lock()
	for gk, present := range expect {
		c.refreshExpect[gk] = present
	}
	c.refreshMu.Unlock()
	select {
	case c.refreshCh <- struct{}{}:
	default:
	}
}

func (c *Cluster) refreshLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.rootCtx.Done():
			return
		case <-c.refreshCh:
		}
		c.runRefresh()
	}
}

func (c *Cluster) runRefresh() {
	// Same budget as upstream's APIService reconciliation: 9 attempts, 500ms growing by 1.5.
	bo := wait.Backoff{Duration: 500 * time.Millisecond, Factor: 1.5, Jitter: 0.1, Steps: 9}
	for attempt := 1; ; attempt++ {
		c.refreshMu.Lock()
		expect := make(map[schema.GroupKind]bool, len(c.refreshExpect))
		for k, v := range c.refreshExpect {
			expect[k] = v
		}
		c.refreshMu.Unlock()

		satisfied, err := c.refreshAPIs(c.rootCtx, expect)
		if err != nil && c.rootCtx.Err() == nil {
			c.log.Warn("API refresh failed", "error", err)
		}
		if satisfied || attempt >= bo.Steps || c.rootCtx.Err() != nil {
			if !satisfied && c.rootCtx.Err() == nil {
				c.log.Info("discovery did not converge after API change; will retry on next change or resync")
			}
			c.refreshMu.Lock()
			for gk, v := range expect {
				if cur, ok := c.refreshExpect[gk]; ok && cur == v {
					delete(c.refreshExpect, gk)
				}
			}
			c.refreshMu.Unlock()
			return
		}
		if !sleepCtx(c.rootCtx, bo.Step()) {
			return
		}
	}
}

// refreshAPIs re-runs discovery and starts/stops watches to match it without a full resync.
// It reports whether discovery agreed with expect (and with the API server).
//
// Group versions whose discovery failed are unknown, not absent: registrations of their
// groups are kept as they are, and their previous discovery data is carried over. A stopped
// registration always has its objects removed; when its API is still served it is listed
// again from scratch, so nothing it cached survives without a live watch behind it.
func (c *Cluster) refreshAPIs(ctx context.Context, expect map[schema.GroupKind]bool) (bool, error) {
	select {
	case c.syncSem <- struct{}{}:
	case <-ctx.Done():
		return true, ctx.Err()
	}
	defer func() { <-c.syncSem }()

	epoch := c.epoch.Load()
	if !c.validFor(epoch) {
		return true, nil // the next full sync discovers everything anyway
	}
	c.mu.RLock()
	settings, cl, lst, server := c.settings, c.clients, c.lister, c.cfg.Host
	c.mu.RUnlock()
	if cl == nil || lst == nil {
		return true, nil
	}
	disc, err := discoverAPIs(cl.disco, &settings, server)
	if err != nil {
		return false, err
	}
	failedGroups := disc.failedGroups()
	satisfied := true
	for gk, present := range expect {
		// An expectation about a group whose discovery failed cannot be confirmed: retry.
		if failedGroups[gk.Group] || disc.served(gk) != present {
			satisfied = false
		}
	}
	want := make(map[schema.GroupKind]APIResourceInfo, len(disc.watch))
	for _, api := range disc.watch {
		want[api.GroupKind] = api
	}

	var toStart []APIResourceInfo
	c.mu.Lock()
	if c.closed || c.epoch.Load() != epoch {
		c.mu.Unlock()
		return true, nil
	}
	disc.inheritFailed(c.disc)
	for gk, w := range c.apis {
		nw, ok := want[gk]
		unchanged := ok && nw.GroupVersionResource == w.info.GroupVersionResource && nw.Meta.Namespaced == w.info.Meta.Namespaced
		if !w.dead && (unchanged || failedGroups[gk.Group]) {
			continue
		}
		w.cancel()
		delete(c.apis, gk)
		for k := range c.denied {
			if k.gk == gk {
				delete(c.denied, k)
			}
		}
		// Drop what the stopped registration cached. If the API is still served it is
		// listed again below; if that list fails, nothing stale remains without a watch.
		c.pending = append(c.pending, queueItem{kind: qRemoveAPI, gk: gk})
		c.log.Info("stopping watch", "group", gk.Group, "kind", gk.Kind)
	}
	for gk, api := range want {
		if _, ok := c.apis[gk]; ok || c.listDenied(gk) {
			continue
		}
		toStart = append(toStart, api)
	}
	c.disc = disc
	c.mu.Unlock()
	c.wakeProcessor()
	slices.SortFunc(toStart, compareAPIInfo)

	convert := func(un *unstructured.Unstructured) *Resource { return newResource(un, &settings) }
	for _, api := range toStart {
		var results []scopeResult
		failed := false
		for _, ns := range settings.scopes(api.Meta.Namespaced) {
			items, rv, err := lst.list(ctx, c.listerWatcher(cl, api, ns), convert)
			if err != nil {
				if ctx.Err() != nil {
					return true, ctx.Err()
				}
				denied, gone, cerr := c.classifyListError(ctx, cl, &settings, api, ns, err)
				switch {
				case gone:
					// Discovery serves the API but the server does not: retry until they agree.
					satisfied, failed = false, true
				case denied != nil:
					c.mu.Lock()
					c.denied[deniedKey{api.GroupKind, ns}] = *denied
					c.mu.Unlock()
					continue
				default:
					c.log.Warn("listing new API failed", "group", api.GroupKind.Group, "kind", api.GroupKind.Kind, "error", cerr)
					satisfied, failed = false, true
				}
				if failed {
					break
				}
			}
			results = append(results, scopeResult{api: api, ns: ns, items: items, rv: rv})
		}
		if failed || len(results) == 0 {
			continue
		}
		c.mu.Lock()
		if c.closed || c.epoch.Load() != epoch {
			c.mu.Unlock()
			return true, nil
		}
		w := c.registerAPI(api)
		var actx context.Context
		actx, w.cancel = context.WithCancel(c.genCtx)
		watchers := make([]*watcher, 0, len(results))
		for _, r := range results {
			c.pending = append(c.pending, queueItem{kind: qReplace, token: w.token, gk: api.GroupKind, namespace: r.ns, items: r.items})
			watchers = append(watchers, c.newWatcher(cl, &settings, lst, api, r.ns, w.token))
		}
		c.wg.Add(len(watchers))
		c.mu.Unlock()
		c.wakeProcessor()
		c.log.Info("started watch", "group", api.GroupKind.Group, "kind", api.GroupKind.Kind)
		for i, wt := range watchers {
			c.spawnWatcher(actx, wt, results[i].rv, settings.Metrics)
		}
	}
	c.oapi.reset()
	return satisfied, nil
}

// listDenied reports an API skipped for lack of list permission; call with c.mu held.
func (c *Cluster) listDenied(gk schema.GroupKind) bool {
	for k, v := range c.denied {
		if k.gk == gk && v.Reason == ReasonListForbidden {
			return true
		}
	}
	return false
}

// validFor reports whether the last sync succeeded for epoch.
func (c *Cluster) validFor(epoch uint64) bool {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	return c.status.attempted && c.status.syncErr == nil && c.status.epoch == epoch
}

// --- queries -----------------------------------------------------------------------------

// GetServerVersion implements ClusterCache.
func (c *Cluster) GetServerVersion() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.disc == nil {
		return ""
	}
	return c.disc.serverVersion
}

// GetAPIResources implements ClusterCache.
func (c *Cluster) GetAPIResources() []APIResourceInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.disc == nil {
		return nil
	}
	return slices.Clone(c.disc.all)
}

// IsNamespaced implements ClusterCache.
func (c *Cluster) IsNamespaced(gk schema.GroupKind) (bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.disc != nil {
		if ns, ok := c.disc.namespaced[gk]; ok {
			return ns, nil
		}
	}
	return false, fmt.Errorf("%w: %s", ErrUnknownGroupKind, gk.String())
}

// namespaceMaps returns the published namespace maps (all, or just ns). The maps are
// immutable and may be read without locks.
func (c *Cluster) namespaceMaps(ns string) []nsMap {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if ns != "" {
		if m := c.graph.ns[ns]; m != nil {
			return []nsMap{m}
		}
		return nil
	}
	out := make([]nsMap, 0, len(c.graph.ns))
	for _, m := range c.graph.ns {
		out = append(out, m)
	}
	return out
}

// FindResources implements ClusterCache.
func (c *Cluster) FindResources(namespace string, predicates ...func(*Resource) bool) map[ResourceKey]*Resource {
	result := map[ResourceKey]*Resource{}
	for _, m := range c.namespaceMaps(namespace) {
	next:
		for k, r := range m {
			for _, p := range predicates {
				if !p(r) {
					continue next
				}
			}
			result[k] = r
		}
	}
	return result
}

// IterateHierarchy implements ClusterCache.
func (c *Cluster) IterateHierarchy(keys []ResourceKey, fn func(r *Resource, namespaceResources map[ResourceKey]*Resource) bool) {
	c.mu.RLock()
	snap := c.graph.captureHierarchy(keys)
	c.mu.RUnlock()
	snap.walk(fn)
}

// GetClusterInfo implements ClusterCache.
func (c *Cluster) GetClusterInfo() ClusterInfo {
	c.mu.RLock()
	info := ClusterInfo{
		Server:         c.cfg.Host,
		APIsCount:      len(c.apis),
		ResourcesCount: c.graph.count,
	}
	for _, d := range c.denied {
		info.UnavailableAPIs = append(info.UnavailableAPIs, d)
	}
	if c.disc != nil {
		info.K8SVersion = c.disc.serverVersion
		info.UnavailableAPIs = append(info.UnavailableAPIs, c.disc.failures...)
	}
	c.mu.RUnlock()
	slices.SortFunc(info.UnavailableAPIs, func(a, b UnavailableAPI) int {
		return cmp.Or(cmp.Compare(a.GroupKind.Group, b.GroupKind.Group), cmp.Compare(a.GroupKind.Kind, b.GroupKind.Kind),
			cmp.Compare(a.Version, b.Version), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Reason, b.Reason))
	})
	epoch := c.epoch.Load()
	c.statusMu.Lock()
	if c.status.attempted {
		t := c.status.syncTime
		info.LastCacheSyncTime = &t
		info.SyncError = c.status.syncErr
		info.Synced = c.status.syncErr == nil && c.status.epoch == epoch
	}
	c.statusMu.Unlock()
	info.ActiveWatches = int(c.activeWatches.Load())
	return info
}

// GetOpenAPISchema implements ClusterCache.
func (c *Cluster) GetOpenAPISchema(ctx context.Context) (map[string]*spec.Schema, error) {
	s, _, err := c.loadOpenAPI(ctx)
	return s, err
}

// GetGVKParser implements ClusterCache.
func (c *Cluster) GetGVKParser(ctx context.Context) (*GVKParser, error) {
	_, p, err := c.loadOpenAPI(ctx)
	return p, err
}

func (c *Cluster) loadOpenAPI(ctx context.Context) (map[string]*spec.Schema, *GVKParser, error) {
	c.mu.RLock()
	cl, clErr := c.clients, c.clientsErr
	c.mu.RUnlock()
	if clErr != nil {
		return nil, nil, clErr
	}
	return c.oapi.get(ctx, func() (map[string]*spec.Schema, *GVKParser, error) {
		return loadOpenAPIV3(cl.disco.OpenAPIV3())
	})
}

// --- handler registration -----------------------------------------------------------------

// OnResourceUpdated implements ClusterCache. Handlers run sequentially on the processor
// goroutine, without cache locks; a slow handler delays event processing.
func (c *Cluster) OnResourceUpdated(h ResourceUpdatedHandler) Unsubscribe {
	return register(c, c.updatedHandlers, h)
}

// OnEvent implements ClusterCache.
func (c *Cluster) OnEvent(h EventHandler) Unsubscribe { return register(c, c.eventHandlers, h) }

// OnProcessEvents implements ClusterCache.
func (c *Cluster) OnProcessEvents(h ProcessEventsHandler) Unsubscribe {
	return register(c, c.processHandlers, h)
}

func register[H any](c *Cluster, m map[uint64]H, h H) Unsubscribe {
	c.hmu.Lock()
	defer c.hmu.Unlock()
	c.nextHandlerID++
	id := c.nextHandlerID
	m[id] = h
	return func() {
		c.hmu.Lock()
		defer c.hmu.Unlock()
		delete(m, id)
	}
}

func snapshotHandlers[H any](c *Cluster, m map[uint64]H) []H {
	c.hmu.Lock()
	defer c.hmu.Unlock()
	ids := make([]uint64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]H, 0, len(ids))
	for _, id := range ids {
		out = append(out, m[id])
	}
	return out
}

func (c *Cluster) resourceUpdatedHandlers() []ResourceUpdatedHandler {
	return snapshotHandlers(c, c.updatedHandlers)
}
func (c *Cluster) eventHandlerList() []EventHandler { return snapshotHandlers(c, c.eventHandlers) }
func (c *Cluster) processEventsHandlers() []ProcessEventsHandler {
	return snapshotHandlers(c, c.processHandlers)
}
