package sharding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	coordclient "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Lease metadata.
const (
	// AnnotationShardHandoff ("cd.patrol.example/shard-handoff-requested-by") is set on a shard
	// Lease by the replica that prefers the shard while another replica holds it. A holder for
	// which the shard is not its preferred one releases the Lease on its next heartbeat.
	// (Requested for api/v1alpha1/keys.go; declared here until then.)
	AnnotationShardHandoff = v1alpha1.Prefix + "shard-handoff-requested-by"
	// LeaseComponent is the app.kubernetes.io/component label value of shard Leases.
	LeaseComponent = "controller"
)

// LeaseClient is the Lease API used by the LeaseManager (NewLeaseClient for a clientset).
type LeaseClient interface {
	// List returns the Leases of the namespace. Shard Leases are recognized by name, so a
	// List that hides some of them (a label selector) makes them invisible to the manager;
	// other Leases are ignored.
	List(ctx context.Context) ([]coordinationv1.Lease, error)
	Get(ctx context.Context, name string) (*coordinationv1.Lease, error)
	Create(ctx context.Context, lease *coordinationv1.Lease) (*coordinationv1.Lease, error)
	// Update must fail with a Conflict error when lease.ResourceVersion is stale.
	Update(ctx context.Context, lease *coordinationv1.Lease) (*coordinationv1.Lease, error)
}

// NewLeaseClient returns a LeaseClient for the Leases of namespace. List returns every Lease
// of the namespace (no label selector): a shard Lease that was pre-created or lost its labels
// must stay visible, or the manager would try to create it on every heartbeat.
func NewLeaseClient(kube kubernetes.Interface, namespace string) LeaseClient {
	return kubeLeaseClient{c: kube.CoordinationV1().Leases(namespace)}
}

type kubeLeaseClient struct {
	c coordclient.LeaseInterface
}

func (k kubeLeaseClient) List(ctx context.Context) ([]coordinationv1.Lease, error) {
	l, err := k.c.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}

func (k kubeLeaseClient) Get(ctx context.Context, name string) (*coordinationv1.Lease, error) {
	return k.c.Get(ctx, name, metav1.GetOptions{})
}

func (k kubeLeaseClient) Create(ctx context.Context, l *coordinationv1.Lease) (*coordinationv1.Lease, error) {
	return k.c.Create(ctx, l, metav1.CreateOptions{})
}

func (k kubeLeaseClient) Update(ctx context.Context, l *coordinationv1.Lease) (*coordinationv1.Lease, error) {
	return k.c.Update(ctx, l, metav1.UpdateOptions{})
}

// LeaseManagerOptions configure a LeaseManager.
type LeaseManagerOptions struct {
	// Client accesses the shard Leases (required).
	Client LeaseClient
	// Namespace is the namespace of the Leases (control-plane namespace, required).
	Namespace string
	// Identity is the holder identity: the pod hostname (required). It is also the DNS label
	// patrol-server uses to reach the holder through the headless Service.
	Identity string
	// Config gives the preferred shard (Shard, Hostname, DynamicDistribution) and Heartbeat.
	Config Config
	// Replicas returns the shard count (required).
	Replicas ReplicaSource
	// LeaseDuration overrides Config.LeaseDuration() (3 × heartbeat).
	LeaseDuration time.Duration
	// CallTimeout bounds the API calls of one heartbeat (replica count read and Lease calls
	// together; default and maximum: half the heartbeat), so a hung call cannot hold renewals
	// past the renewal deadline.
	CallTimeout time.Duration
	// MaxShards caps the shards one replica holds (0: unlimited). The preferred shard is
	// always acquired.
	MaxShards int
	// DisableStandby stops the replica from taking over expired Leases of other shards.
	DisableStandby bool
	// OnAcquired runs after a shard Lease was acquired (ctx is the Run context); OnLost after
	// a shard was lost or is about to be released (before the Lease is cleared, so processing
	// stops first). Every period in which OwnsShard(n) is true starts with OnAcquired(n) and
	// ends with OnLost(n), possibly late: OnLost runs at the first heartbeat after the renewal
	// deadline passed. Both run on the manager goroutine, in order, and should return quickly.
	OnAcquired func(ctx context.Context, shard int)
	OnLost     func(shard int)
	// OnReplicasChanged runs when the shard count is first read and whenever it changes
	// (wire it to Assigner.SetReplicas). It never runs with a guessed count: until the first
	// successful read the replica claims no shard.
	OnReplicasChanged func(replicas int)
	// Clock is the time source (real clock when nil).
	Clock clock.Clock
	// Logger receives ownership changes (nil discards).
	Logger *slog.Logger
	// Registerer receives the ownership metrics (nil: not registered).
	Registerer prometheus.Registerer
}

// LeaseManager owns shards through Leases (ADR-0008). Each heartbeat it renews the Leases it
// holds, acquires its preferred shard (asking a borrowing holder to hand it back), and, as a
// standby, takes over at most one Lease that stayed free or unrenewed for a full lease
// duration (plus one heartbeat per shard it already holds, so replicas with fewer shards win).
// Expiry is judged by the local time at which a Lease record was last seen to change, never
// by comparing clocks across pods. A shard counts as owned only until the renewal deadline
// (lease duration − heartbeat) after the last successful renewal; a renewal is sent only while
// that deadline has not passed, and a lapsed shard is reported lost before it is re-acquired.
type LeaseManager struct {
	opts          LeaseManagerOptions
	clock         clock.Clock
	logger        *slog.Logger
	metrics       *leaseMetrics
	heartbeat     time.Duration
	leaseDuration time.Duration
	renewDeadline time.Duration
	callTimeout   time.Duration

	mu        sync.RWMutex
	held      map[int]*heldLease
	replicas  int
	preferred int
	lastErr   error

	// tick-goroutine state
	observed      map[int]observation
	lastPreferred *int
	// preferredFailure tracks consecutive failed acquisitions of the preferred shard.
	preferredFailure struct {
		shard int
		since time.Time
		err   error
	}
}

type heldLease struct {
	lastRenew time.Time
	lease     *coordinationv1.Lease
}

// observation is what was last seen of a shard Lease and when it last changed locally.
type observation struct {
	resourceVersion string
	holder          string
	at              time.Time
}

// NewLeaseManager validates opts and returns a manager; call Run to start it.
func NewLeaseManager(opts LeaseManagerOptions) (*LeaseManager, error) {
	switch {
	case opts.Client == nil:
		return nil, errors.New("sharding: lease client is required")
	case opts.Namespace == "":
		return nil, errors.New("sharding: lease namespace is required")
	case opts.Identity == "":
		return nil, errors.New("sharding: holder identity is required")
	case opts.Replicas == nil:
		return nil, errors.New("sharding: replica source is required")
	}
	m, err := newLeaseMetrics(opts.Registerer)
	if err != nil {
		return nil, err
	}
	hb := opts.Config.Heartbeat
	if hb <= 0 {
		hb = DefaultHeartbeat
	}
	ld := opts.LeaseDuration
	if ld <= 0 {
		ld = opts.Config.LeaseDuration()
	}
	if ld <= hb {
		return nil, fmt.Errorf("sharding: lease duration %s must be longer than the heartbeat %s", ld, hb)
	}
	ct := opts.CallTimeout
	switch {
	case ct == 0:
		ct = hb / 2
	case ct < 0 || ct > hb/2:
		return nil, fmt.Errorf("sharding: call timeout %s must be positive and at most half the heartbeat %s", ct, hb)
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.RealClock{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &LeaseManager{
		opts:          opts,
		clock:         clk,
		logger:        logger.With(slog.String("identity", opts.Identity)),
		metrics:       m,
		heartbeat:     hb,
		leaseDuration: ld,
		renewDeadline: ld - hb,
		callTimeout:   ct,
		held:          map[int]*heldLease{},
		replicas:      0,
		preferred:     -1,
		observed:      map[int]observation{},
	}, nil
}

// Run runs a heartbeat every heartbeat period (at a fixed rate: a slow heartbeat shortens the
// wait before the next one) until ctx ends, then releases the held Leases (OnLost first) so
// other replicas can take them over without waiting for expiry.
func (m *LeaseManager) Run(ctx context.Context) error {
	for {
		start := m.clock.Now()
		m.tick(ctx)
		wait := max(m.heartbeat-m.clock.Since(start), 0)
		select {
		case <-ctx.Done():
			m.releaseAll()
			return nil
		case <-m.clock.After(wait):
		}
	}
}

// OwnsShard reports whether this replica holds the Lease of shard and renewed it within the
// renewal deadline.
func (m *LeaseManager) OwnsShard(shard int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	h, ok := m.held[shard]
	return ok && m.clock.Since(h.lastRenew) < m.renewDeadline
}

// OwnedShards returns the shards this replica owns, sorted.
func (m *LeaseManager) OwnedShards() []int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []int
	for s, h := range m.held {
		if m.clock.Since(h.lastRenew) < m.renewDeadline {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// Replicas returns the last known shard count (0 until it was first read).
func (m *LeaseManager) Replicas() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.replicas
}

// PreferredShard returns the shard this replica tries to own (-1: standby only, or the shard
// count is not known yet).
func (m *LeaseManager) PreferredShard() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.preferred
}

// Healthz is a readiness check reflecting the last heartbeat. It fails when the replica count
// could not be read (dynamic distribution), when a shard Lease API call failed for a reason
// other than a lost update race (unreachable API, Forbidden, timeout), and when the preferred
// shard could not be acquired for a full lease duration although its Lease was absent, free,
// this replica's own or expired.
func (m *LeaseManager) Healthz() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastErr
}

func (m *LeaseManager) setErr(err error) {
	m.mu.Lock()
	m.lastErr = err
	m.mu.Unlock()
}

// tick runs one heartbeat. Its API calls share one deadline (callTimeout); callbacks get ctx.
func (m *LeaseManager) tick(ctx context.Context) {
	call, cancel := context.WithTimeout(ctx, m.callTimeout)
	defer cancel()

	prev := m.Replicas()
	replicas, replicasErr := m.opts.Replicas.Replicas(call)
	if replicasErr != nil {
		m.countErr("replicas", replicasErr)
		if prev == 0 {
			// No count was ever read. Guessing one (e.g. a single replica) would map the
			// clusters of the peers' shards to a shard this replica can own.
			m.logger.Warn("cannot read the controller replica count; claiming no shard until it is known", slog.Any("error", replicasErr))
			m.setErr(fmt.Errorf("cannot read the controller replica count; no shard is claimed until it is known: %w", replicasErr))
			return
		}
		m.logger.Warn("cannot read the controller replica count; keeping the last known count",
			slog.Int("replicas", prev), slog.Any("error", replicasErr))
		replicas = prev
	}
	replicas = max(replicas, 1)
	preferred := m.opts.Config.PreferredShard(replicas, m.preferredLogger(replicas))
	m.mu.Lock()
	m.replicas, m.preferred = replicas, preferred
	m.mu.Unlock()
	if replicas != prev && m.opts.OnReplicasChanged != nil {
		m.opts.OnReplicasChanged(replicas)
	}

	var leaseErr error
	if errs := m.heartbeatLeases(ctx, call, replicas, preferred); len(errs) > 0 {
		leaseErr = fmt.Errorf("error while updating the heartbeat for the shard leases: %w", errors.Join(errs...))
	}
	m.setErr(errors.Join(replicasErr, leaseErr))
}

// heartbeatLeases renews, releases and acquires the shard Leases and returns the errors that
// fail readiness.
func (m *LeaseManager) heartbeatLeases(ctx, call context.Context, replicas, preferred int) []error {
	// A late heartbeat reports lapsed shards lost before anything is renewed, so ownership
	// never lapses and comes back without callbacks.
	m.enforceDeadlines()
	leases, err := m.opts.Client.List(call)
	if err != nil {
		m.countErr("list", err)
		return []error{err}
	}
	byShard := map[int]*coordinationv1.Lease{}
	for i := range leases {
		if s, ok := ShardFromLeaseName(leases[i].Name); ok {
			byShard[s] = &leases[i]
		}
	}
	m.observe(byShard, replicas, m.clock.Now())

	errs := m.renewHeld(call, byShard, replicas, preferred)
	m.enforceDeadlines()
	if preferred >= 0 && preferred < replicas && !m.holds(preferred) {
		if err := m.acquirePreferred(ctx, call, preferred, byShard[preferred]); err != nil {
			errs = append(errs, err)
		}
	} else {
		m.preferredFailure.since = time.Time{}
	}
	if !m.opts.DisableStandby {
		errs = append(errs, m.acquireStandby(ctx, call, byShard, replicas, preferred)...)
	}
	return errs
}

// preferredLogger logs PreferredShard warnings only when the inputs change.
func (m *LeaseManager) preferredLogger(replicas int) *slog.Logger {
	if m.lastPreferred != nil && *m.lastPreferred == replicas {
		return nil
	}
	m.lastPreferred = ptr.To(replicas)
	return m.logger
}

func (m *LeaseManager) holds(shard int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.held[shard]
	return ok
}

func (m *LeaseManager) heldShards() []int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return slices.Sorted(maps.Keys(m.held))
}

func (m *LeaseManager) lastRenew(shard int) (time.Time, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	h, ok := m.held[shard]
	if !ok {
		return time.Time{}, false
	}
	return h.lastRenew, true
}

// countErr counts a failed heartbeat API call.
func (m *LeaseManager) countErr(op string, err error) {
	reason := reasonError
	if isRace(err) {
		reason = reasonConflict
	}
	m.metrics.errors.WithLabelValues(op, reason).Inc()
}

// isRace reports a lost optimistic-concurrency race: another writer changed or created the
// Lease first. The next heartbeat works from fresh data, so it does not fail readiness.
func isRace(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
}

// observe records when each shard Lease (or its absence) was last seen to change.
func (m *LeaseManager) observe(byShard map[int]*coordinationv1.Lease, replicas int, now time.Time) {
	for s, l := range byShard {
		m.noteObserved(s, l, now)
	}
	for s := range replicas {
		if _, ok := byShard[s]; ok {
			continue
		}
		if o, ok := m.observed[s]; !ok || o.resourceVersion != "" {
			m.observed[s] = observation{at: now}
		}
	}
	for s := range m.observed {
		if _, ok := byShard[s]; !ok && s >= replicas {
			delete(m.observed, s)
		}
	}
}

// noteObserved records l as seen at now when it changed since it was last seen.
func (m *LeaseManager) noteObserved(shard int, l *coordinationv1.Lease, now time.Time) {
	if o, ok := m.observed[shard]; !ok || o.resourceVersion != l.ResourceVersion {
		m.observed[shard] = observation{resourceVersion: l.ResourceVersion, holder: holderOf(l), at: now}
	}
}

// expired reports whether the Lease of shard has not changed for a full lease duration plus
// extra.
func (m *LeaseManager) expired(shard int, l *coordinationv1.Lease, now time.Time, extra time.Duration) bool {
	o, ok := m.observed[shard]
	if !ok {
		return false
	}
	d := m.leaseDuration
	if l != nil && l.Spec.LeaseDurationSeconds != nil && *l.Spec.LeaseDurationSeconds > 0 {
		d = time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second
	}
	return now.Sub(o.at) >= d+extra
}

// withShardLabels sets the shard Lease labels (restoring them on Leases that lost them).
func withShardLabels(l map[string]string) map[string]string {
	if l == nil {
		l = map[string]string{}
	}
	l[v1alpha1.LabelKeyPartOf] = v1alpha1.LabelValuePartOf
	l[v1alpha1.LabelKeyComponent] = LeaseComponent
	return l
}

// handoffRequestedByOther reports whether another replica asked for the shard of l.
func (m *LeaseManager) handoffRequestedByOther(l *coordinationv1.Lease) bool {
	by := l.Annotations[AnnotationShardHandoff]
	return by != "" && by != m.opts.Identity
}

func (m *LeaseManager) renewHeld(call context.Context, byShard map[int]*coordinationv1.Lease, replicas, preferred int) []error {
	var errs []error
	for _, s := range m.heldShards() {
		l := byShard[s]
		switch {
		case s >= replicas:
			if err := m.release(call, s, l, "shard no longer exists after a replica count change"); err != nil {
				errs = append(errs, err)
			}
			continue
		case l == nil:
			m.lose(s, "shard lease was deleted")
			continue
		case holderOf(l) != m.opts.Identity:
			m.lose(s, "shard lease is held by "+holderOf(l))
			continue
		case s != preferred && m.handoffRequestedByOther(l):
			if err := m.release(call, s, l, "handing the shard back to "+l.Annotations[AnnotationShardHandoff]); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		// Renew only while the deadline has not passed (it may have passed during the List
		// call), so OwnsShard is true without interruption between renewals.
		sent := m.clock.Now()
		if last, ok := m.lastRenew(s); !ok || sent.Sub(last) >= m.renewDeadline {
			m.lose(s, "shard lease could not be renewed before the deadline")
			continue
		}
		next := l.DeepCopy()
		next.Labels = withShardLabels(next.Labels)
		next.Spec.RenewTime = &metav1.MicroTime{Time: sent}
		next.Spec.LeaseDurationSeconds = ptr.To(int32(m.leaseDuration / time.Second)) //nolint:gosec // bounded by MaxHeartbeatSeconds
		if next.Annotations[AnnotationShardHandoff] == m.opts.Identity {
			delete(next.Annotations, AnnotationShardHandoff)
		}
		updated, err := m.opts.Client.Update(call, next)
		if err != nil {
			m.countErr("renew", err)
			if isRace(err) {
				m.logger.Debug("shard lease changed concurrently; renewing at the next heartbeat", slog.Int("shard", s), slog.Any("error", err))
			} else {
				m.logger.Warn("cannot renew shard lease", slog.Int("shard", s), slog.Any("error", err))
				errs = append(errs, fmt.Errorf("renew shard %d: %w", s, err))
			}
			continue
		}
		m.mu.Lock()
		if h, ok := m.held[s]; ok {
			// The send time, not the response time: the deadline is counted from before the
			// write that other replicas measure expiry from.
			h.lastRenew, h.lease = sent, updated
		}
		m.mu.Unlock()
		m.observed[s] = observation{resourceVersion: updated.ResourceVersion, holder: m.opts.Identity, at: m.clock.Now()}
	}
	return errs
}

// enforceDeadlines drops shards whose renewal deadline passed.
func (m *LeaseManager) enforceDeadlines() {
	now := m.clock.Now()
	m.mu.RLock()
	var late []int
	for s, h := range m.held {
		if now.Sub(h.lastRenew) >= m.renewDeadline {
			late = append(late, s)
		}
	}
	m.mu.RUnlock()
	slices.Sort(late)
	for _, s := range late {
		m.lose(s, "shard lease could not be renewed before the deadline")
	}
}

// acquirePreferred acquires the preferred shard when its Lease is absent, free, this
// replica's own or expired, and otherwise asks the holder to hand it back. The returned error
// fails readiness: an API error other than a lost race, or attempts that kept failing for a
// lease duration (e.g. a Lease that exists but that List does not return).
func (m *LeaseManager) acquirePreferred(ctx, call context.Context, shard int, l *coordinationv1.Lease) error {
	if l == nil || holderOf(l) == "" || holderOf(l) == m.opts.Identity || m.expired(shard, l, m.clock.Now(), 0) {
		ok, err := m.acquire(ctx, call, shard, l)
		if ok {
			m.preferredFailure.since = time.Time{}
			return nil
		}
		return m.preferredAcquireFailed(shard, err)
	}
	// Held by another replica: not an acquisition failure.
	m.preferredFailure.since = time.Time{}
	if l.Annotations[AnnotationShardHandoff] == m.opts.Identity {
		return nil
	}
	next := l.DeepCopy()
	if next.Annotations == nil {
		next.Annotations = map[string]string{}
	}
	next.Annotations[AnnotationShardHandoff] = m.opts.Identity
	updated, err := m.opts.Client.Update(call, next)
	if err != nil {
		m.countErr("handoff", err)
		if isRace(err) {
			m.logger.Debug("cannot request shard handoff", slog.Int("shard", shard), slog.Any("error", err))
			return nil
		}
		m.logger.Warn("cannot request shard handoff", slog.Int("shard", shard), slog.Any("error", err))
		return fmt.Errorf("request handoff of shard %d: %w", shard, err)
	}
	m.observed[shard] = observation{resourceVersion: updated.ResourceVersion, holder: holderOf(updated), at: m.observed[shard].at}
	m.metrics.transitions.WithLabelValues(strconv.Itoa(shard), eventHandoff).Inc()
	m.logger.Info("requested shard handoff from its current holder", slog.Int("shard", shard), slog.String("holder", holderOf(l)))
	return nil
}

// preferredAcquireFailed tracks a failed acquisition of the preferred shard and returns the
// error that fails readiness, if any.
func (m *LeaseManager) preferredAcquireFailed(shard int, err error) error {
	now := m.clock.Now()
	f := &m.preferredFailure
	if f.since.IsZero() || f.shard != shard {
		f.shard, f.since = shard, now
	}
	if !isRace(err) {
		return err
	}
	if d := now.Sub(f.since); d >= m.leaseDuration {
		return fmt.Errorf("cannot acquire the preferred shard %d for %s: %w", shard, d, err)
	}
	return nil
}

// acquireStandby takes over at most one other shard whose Lease stayed absent, free or
// unchanged for a lease duration plus one heartbeat per shard already held, or that is this
// replica's own Lease dropped locally after a missed deadline (unless another replica asked
// for it). It returns the API errors other than lost races.
func (m *LeaseManager) acquireStandby(ctx, call context.Context, byShard map[int]*coordinationv1.Lease, replicas, preferred int) []error {
	held := m.heldShards()
	if m.opts.MaxShards > 0 && len(held) >= m.opts.MaxShards {
		return nil
	}
	extra := time.Duration(len(held)) * m.heartbeat
	start := max(preferred+1, 0)
	var errs []error
	for i := range replicas {
		s := (start + i) % replicas
		if s == preferred || slices.Contains(held, s) {
			continue
		}
		l := byShard[s]
		own := l != nil && holderOf(l) == m.opts.Identity && !m.handoffRequestedByOther(l)
		if !own && !m.expired(s, l, m.clock.Now(), extra) {
			continue
		}
		ok, err := m.acquire(ctx, call, s, l)
		if ok {
			return errs
		}
		if !isRace(err) {
			errs = append(errs, err)
		}
	}
	return errs
}

// acquire takes the Lease of shard (creating it when l is nil) and reports success; on
// failure err is non-nil (isRace tells a lost race from other failures).
func (m *LeaseManager) acquire(ctx, call context.Context, shard int, l *coordinationv1.Lease) (bool, error) {
	sent := m.clock.Now()
	micro := metav1.MicroTime{Time: sent}
	dur := ptr.To(int32(m.leaseDuration / time.Second)) //nolint:gosec // bounded by MaxHeartbeatSeconds
	var (
		updated *coordinationv1.Lease
		err     error
	)
	if l == nil {
		updated, err = m.opts.Client.Create(call, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      LeaseName(shard),
				Namespace: m.opts.Namespace,
				Labels:    withShardLabels(nil),
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr.To(m.opts.Identity),
				LeaseDurationSeconds: dur,
				AcquireTime:          &micro,
				RenewTime:            &micro,
				LeaseTransitions:     ptr.To[int32](0),
			},
		})
		if apierrors.IsAlreadyExists(err) {
			m.countErr("acquire", err)
			return m.acquireExisting(ctx, call, shard, err)
		}
	} else {
		next := l.DeepCopy()
		if holderOf(l) != m.opts.Identity {
			next.Spec.AcquireTime = &micro
			next.Spec.LeaseTransitions = ptr.To(ptr.Deref(l.Spec.LeaseTransitions, 0) + 1)
		}
		next.Spec.HolderIdentity = ptr.To(m.opts.Identity)
		next.Spec.LeaseDurationSeconds = dur
		next.Spec.RenewTime = &micro
		next.Labels = withShardLabels(next.Labels)
		delete(next.Annotations, AnnotationShardHandoff)
		updated, err = m.opts.Client.Update(call, next)
	}
	if err != nil {
		m.countErr("acquire", err)
		if isRace(err) {
			m.logger.Debug("shard lease changed concurrently; not acquired", slog.Int("shard", shard), slog.Any("error", err))
		} else {
			m.logger.Warn("cannot acquire shard lease", slog.Int("shard", shard), slog.Any("error", err))
		}
		return false, fmt.Errorf("acquire shard %d: %w", shard, err)
	}
	m.mu.Lock()
	m.held[shard] = &heldLease{lastRenew: sent, lease: updated}
	m.mu.Unlock()
	m.observed[shard] = observation{resourceVersion: updated.ResourceVersion, holder: m.opts.Identity, at: m.clock.Now()}
	m.metrics.owned.WithLabelValues(strconv.Itoa(shard)).Set(1)
	m.metrics.transitions.WithLabelValues(strconv.Itoa(shard), eventAcquired).Inc()
	prev := ""
	if l != nil {
		prev = holderOf(l)
	}
	m.logger.Info("acquired shard lease", slog.Int("shard", shard), slog.String("previousHolder", prev))
	if m.opts.OnAcquired != nil {
		m.opts.OnAcquired(ctx, shard)
	}
	return true, nil
}

// acquireExisting handles a Create rejected because the Lease exists although List did not
// return it (created concurrently, or hidden from List): the Lease is read and taken when it
// is free or names this replica; otherwise its observation starts now.
func (m *LeaseManager) acquireExisting(ctx, call context.Context, shard int, createErr error) (bool, error) {
	l, err := m.opts.Client.Get(call, LeaseName(shard))
	switch {
	case apierrors.IsNotFound(err):
		return false, fmt.Errorf("acquire shard %d: %w", shard, createErr)
	case err != nil:
		m.countErr("get", err)
		m.logger.Warn("cannot read shard lease", slog.Int("shard", shard), slog.Any("error", err))
		return false, fmt.Errorf("read the lease of shard %d after it already existed: %w", shard, err)
	}
	m.noteObserved(shard, l, m.clock.Now())
	if h := holderOf(l); h == "" || h == m.opts.Identity {
		return m.acquire(ctx, call, shard, l)
	}
	return false, fmt.Errorf("acquire shard %d: %w", shard, createErr)
}

// lose drops shard locally and runs OnLost.
func (m *LeaseManager) lose(shard int, reason string) {
	m.mu.Lock()
	_, ok := m.held[shard]
	delete(m.held, shard)
	m.mu.Unlock()
	if !ok {
		return
	}
	m.metrics.owned.WithLabelValues(strconv.Itoa(shard)).Set(0)
	m.metrics.transitions.WithLabelValues(strconv.Itoa(shard), eventLost).Inc()
	m.logger.Warn("lost shard lease", slog.Int("shard", shard), slog.String("reason", reason))
	if m.opts.OnLost != nil {
		m.opts.OnLost(shard)
	}
}

// release stops processing shard (OnLost) and then clears the Lease holder so another
// replica can take it at once. It returns an API error other than a lost race (the Lease then
// expires).
func (m *LeaseManager) release(call context.Context, shard int, l *coordinationv1.Lease, reason string) error {
	m.mu.Lock()
	h, ok := m.held[shard]
	delete(m.held, shard)
	m.mu.Unlock()
	if !ok {
		return nil
	}
	m.metrics.owned.WithLabelValues(strconv.Itoa(shard)).Set(0)
	m.metrics.transitions.WithLabelValues(strconv.Itoa(shard), eventReleased).Inc()
	m.logger.Info("releasing shard lease", slog.Int("shard", shard), slog.String("reason", reason))
	if m.opts.OnLost != nil {
		m.opts.OnLost(shard)
	}
	if l == nil {
		if fresh, err := m.opts.Client.Get(call, LeaseName(shard)); err == nil {
			l = fresh
		} else {
			l = h.lease
		}
	}
	if l == nil || holderOf(l) != m.opts.Identity {
		return nil
	}
	next := l.DeepCopy()
	next.Spec.HolderIdentity = nil
	next.Spec.RenewTime = &metav1.MicroTime{Time: m.clock.Now()}
	updated, err := m.opts.Client.Update(call, next)
	if err != nil {
		m.countErr("release", err)
		m.logger.Warn("cannot release shard lease; it will expire", slog.Int("shard", shard), slog.Any("error", err))
		if isRace(err) {
			return nil
		}
		return fmt.Errorf("release shard %d: %w", shard, err)
	}
	m.observed[shard] = observation{resourceVersion: updated.ResourceVersion, at: m.clock.Now()}
	return nil
}

// releaseAll releases every held Lease on shutdown.
func (m *LeaseManager) releaseAll() {
	ctx, cancel := context.WithTimeout(context.Background(), m.heartbeat)
	defer cancel()
	for _, s := range m.heldShards() {
		_ = m.release(ctx, s, nil, "controller is shutting down") // logged and counted; the Lease expires otherwise
	}
}

func holderOf(l *coordinationv1.Lease) string {
	if l == nil {
		return ""
	}
	return ptr.Deref(l.Spec.HolderIdentity, "")
}
