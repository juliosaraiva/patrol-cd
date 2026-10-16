package app

import (
	"fmt"
	"math/rand/v2"
	gosync "sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// refreshQueue holds controller-requested refreshes (live-state changes, completed
// operations, self-heal backoff, project/cluster/repository/settings changes) and feeds the
// Application controller through a channel source. Requests coalesce to the highest level
// and the earliest due time. It implements core.RefreshRequester.
//
// Notifications are bounded: a key has at most one event in the channel until the controller
// reconciles it (received/takeAll), at most one timer for its earliest delayed request, and
// keys that do not fit in a full channel wait in an overflow set drained by a single queue
// timer. Requests made while the channel is not drained (a standby replica under leader
// election) therefore cost one map entry per Application, not timers or goroutines.
type refreshQueue struct {
	clock     clock.PassiveClock
	afterFunc func(time.Duration, func())
	events    chan event.TypedGenericEvent[*v1alpha1.Application]

	mu       gosync.Mutex
	pending  map[types.NamespacedName]pendingRefresh
	overflow map[types.NamespacedName]struct{}
	// retryArmed reports the overflow timer.
	retryArmed bool
}

type pendingRefresh struct {
	level core.CompareLevel
	due   time.Time
	// tree requests a tree-only refresh (a descendant of a managed resource changed).
	tree bool
	// notified reports an event in the channel (or in the workqueue) not reconciled yet.
	notified bool
	// timerAt is the due time of the armed delayed-request timer (zero: none).
	timerAt time.Time
}

// refreshQueueSize bounds the buffered refresh events; keys that find it full wait in the
// overflow set, the pending map keeps the request.
const refreshQueueSize = 4096

// overflowRetry is the delay of the overflow timer.
const overflowRetry = 100 * time.Millisecond

func newRefreshQueue(c clock.Clock) *refreshQueue {
	after := func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	if de, ok := c.(clock.WithDelayedExecution); ok {
		after = func(d time.Duration, f func()) { de.AfterFunc(d, f) }
	}
	return newRefreshQueueWith(c, after, refreshQueueSize)
}

func newRefreshQueueWith(c clock.PassiveClock, after func(time.Duration, func()), size int) *refreshQueue {
	return &refreshQueue{
		clock:     c,
		afterFunc: after,
		events:    make(chan event.TypedGenericEvent[*v1alpha1.Application], size),
		pending:   map[types.NamespacedName]pendingRefresh{},
		overflow:  map[types.NamespacedName]struct{}{},
	}
}

// RequestRefresh implements core.RefreshRequester.
func (q *refreshQueue) RequestRefresh(key types.NamespacedName, level core.CompareLevel, after time.Duration) {
	if level == core.CompareNone {
		return
	}
	due := q.clock.Now().Add(after)
	q.mu.Lock()
	defer q.mu.Unlock()
	p, ok := q.pending[key]
	if level > p.level {
		p.level = level
	}
	if !ok || due.Before(p.due) {
		p.due = due
	}
	arm := after > 0 && (p.timerAt.IsZero() || due.Before(p.timerAt))
	if arm {
		p.timerAt = due
	}
	q.pending[key] = p
	if arm {
		q.afterFunc(after, func() { q.timerFired(key, due) })
		return
	}
	if after <= 0 {
		q.notifyLocked(key)
	}
}

// timerFired notifies key when its delayed request is due.
func (q *refreshQueue) timerFired(key types.NamespacedName, at time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	p, ok := q.pending[key]
	if !ok {
		return
	}
	if p.timerAt.Equal(at) {
		p.timerAt = time.Time{}
		q.pending[key] = p
	}
	q.notifyLocked(key)
}

// notifyLocked sends key's event unless one is already outstanding; a full channel puts the
// key in the overflow set. q.mu is held.
func (q *refreshQueue) notifyLocked(key types.NamespacedName) {
	p, ok := q.pending[key]
	if !ok || p.notified {
		return
	}
	ev := event.TypedGenericEvent[*v1alpha1.Application]{Object: &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}}
	select {
	case q.events <- ev:
		p.notified = true
		q.pending[key] = p
		delete(q.overflow, key)
	default:
		q.overflow[key] = struct{}{}
		if !q.retryArmed {
			q.retryArmed = true
			q.afterFunc(overflowRetry, q.drainOverflow)
		}
	}
}

// drainOverflow retries the keys that found the channel full.
func (q *refreshQueue) drainOverflow() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.retryArmed = false
	for key := range q.overflow {
		delete(q.overflow, key)
		q.notifyLocked(key)
		if q.retryArmed {
			// Still full; notifyLocked put the key back and re-armed the timer.
			return
		}
	}
}

// RequestTreeRefresh asks for a tree-only refresh of key (no comparison).
func (q *refreshQueue) RequestTreeRefresh(key types.NamespacedName) {
	q.mu.Lock()
	defer q.mu.Unlock()
	p, ok := q.pending[key]
	if !ok {
		p.due = q.clock.Now()
	}
	p.tree = true
	q.pending[key] = p
	q.notifyLocked(key)
}

// received records that the controller is reconciling key: later requests notify again.
func (q *refreshQueue) received(key types.NamespacedName) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if p, ok := q.pending[key]; ok && p.notified {
		p.notified = false
		q.pending[key] = p
	}
}

// take returns the due requested level of key and removes it; a request that is not due yet
// is kept and its remaining delay returned.
func (q *refreshQueue) take(key types.NamespacedName) (core.CompareLevel, time.Duration) {
	level, _, wait := q.takeAll(key)
	return level, wait
}

// takeAll is take that also reports a requested tree-only refresh.
func (q *refreshQueue) takeAll(key types.NamespacedName) (core.CompareLevel, bool, time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	p, ok := q.pending[key]
	if !ok {
		return core.CompareNone, false, 0
	}
	if wait := p.due.Sub(q.clock.Now()); wait > 0 {
		p.notified = false
		q.pending[key] = p
		return core.CompareNone, false, wait
	}
	delete(q.pending, key)
	delete(q.overflow, key)
	return p.level, p.tree, 0
}

// restore puts back a taken level after a failed reconcile.
func (q *refreshQueue) restore(key types.NamespacedName, level core.CompareLevel) {
	if level == core.CompareNone {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	p, ok := q.pending[key]
	if !ok || level > p.level {
		p.level = level
	}
	if !ok {
		p.due = q.clock.Now()
	}
	q.pending[key] = p
}

func (q *refreshQueue) forget(key types.NamespacedName) {
	q.mu.Lock()
	delete(q.pending, key)
	delete(q.overflow, key)
	q.mu.Unlock()
}

// stats returns the pending keys, the overflow keys and whether the overflow timer is armed
// (tests).
func (q *refreshQueue) stats() (pending, overflow int, retryArmed bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending), len(q.overflow), q.retryArmed
}

// refreshDecision is the outcome of needRefresh.
type refreshDecision struct {
	level  core.CompareLevel
	hard   bool
	reason string
	// annotation reports a refresh requested through the refresh annotation, which is
	// removed after the status is persisted.
	annotation bool
}

// needRefresh decides whether and how an Application is compared (upstream
// needRefreshAppStatus, APP-078..084), in order: the refresh annotation (force-resolve,
// hard for "hard"); spec sources differing from status.sync.comparedTo (force-resolve);
// hard/soft expiry of reconciledAt (latest, hard when the hard timeout expired); destination
// or ignoreDifferences differing from comparedTo, or a spec generation not yet observed
// (latest); an operation that completed after the last comparison (latest for automated,
// force-resolve for manual operations); a controller request (its level).
func needRefresh(app *v1alpha1.Application, rec settings.Reconciliation, requested core.CompareLevel, now time.Time) refreshDecision {
	if t, ok := app.IsRefreshRequested(); ok {
		return refreshDecision{level: core.CompareForceResolve, hard: t == v1alpha1.RefreshTypeHard, reason: fmt.Sprintf("%s refresh requested", t), annotation: true}
	}
	if !sourcesEqual(app) {
		reason := "spec.source differs"
		if app.Spec.HasMultipleSources() {
			reason = "at least one of the spec.sources differs"
		}
		return refreshDecision{level: core.CompareForceResolve, reason: reason}
	}
	soft, hard := expiry(app.Status.ReconciledAt, rec, now)
	if soft || hard {
		at := "never"
		if app.Status.ReconciledAt != nil {
			at = app.Status.ReconciledAt.UTC().Format(time.RFC3339)
		}
		if hard {
			return refreshDecision{level: core.CompareLatest, hard: true, reason: fmt.Sprintf("comparison expired, requesting hard refresh. reconciledAt: %s, expiry: %s", at, rec.HardTimeout)}
		}
		return refreshDecision{level: core.CompareLatest, reason: fmt.Sprintf("comparison expired, requesting refresh. reconciledAt: %s, expiry: %s", at, rec.Timeout)}
	}
	if !app.Spec.Destination.Equals(app.Status.Sync.ComparedTo.Destination) {
		return refreshDecision{level: core.CompareLatest, reason: "spec.destination differs"}
	}
	if !ignoreDifferencesEqual(app.Spec.IgnoreDifferences, app.Status.Sync.ComparedTo.IgnoreDifferences) {
		return refreshDecision{level: core.CompareLatest, reason: "spec.ignoreDifferences differs"}
	}
	if app.Generation != app.Status.ObservedGeneration {
		return refreshDecision{level: core.CompareLatest, reason: "spec changed"}
	}
	if os := app.Status.OperationState; os != nil && os.Phase.IsTerminal() && os.FinishedAt != nil &&
		(app.Status.ReconciledAt == nil || os.FinishedAt.After(app.Status.ReconciledAt.Time)) {
		level := core.CompareForceResolve
		if os.InitiatedBy.Automated {
			level = core.CompareLatest
		}
		return refreshDecision{level: max(level, requested), reason: "operation completed"}
	}
	if requested > core.CompareNone {
		return refreshDecision{level: requested, reason: "controller refresh requested"}
	}
	return refreshDecision{}
}

// expiry reports whether the soft and hard comparison windows expired. A nil reconciledAt is
// always soft-expired; a timeout <= 0 disables its expiry.
func expiry(reconciledAt *metav1.Time, rec settings.Reconciliation, now time.Time) (soft, hard bool) {
	if reconciledAt == nil {
		return true, rec.HardTimeout > 0
	}
	age := now.Sub(reconciledAt.Time)
	return rec.Timeout > 0 && age >= rec.Timeout, rec.HardTimeout > 0 && age >= rec.HardTimeout
}

// nextExpiry returns the delay until the next periodic refresh: reconciledAt + timeout plus a
// random jitter in [0, jitter) (APP-081/082), or the hard timeout when it is earlier. Zero means
// no periodic refresh.
func nextExpiry(reconciledAt *metav1.Time, rec settings.Reconciliation, now time.Time, jitter func(time.Duration) time.Duration) time.Duration {
	if reconciledAt == nil {
		return 0
	}
	var out time.Duration
	if rec.Timeout > 0 {
		out = reconciledAt.Add(rec.Timeout).Sub(now) + jitter(rec.Jitter)
	}
	if rec.HardTimeout > 0 {
		if h := reconciledAt.Add(rec.HardTimeout).Sub(now); out == 0 || h < out {
			out = h
		}
	}
	if out < 0 {
		out = time.Second
	}
	return out
}

func randomJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(max)))
}

func sourcesEqual(app *v1alpha1.Application) bool {
	ct := app.Status.Sync.ComparedTo
	if app.Spec.HasMultipleSources() {
		return equality.Semantic.DeepEqual(nilIfEmpty(app.Spec.Sources), nilIfEmpty(ct.Sources))
	}
	sources := app.Spec.GetSources()
	if len(sources) == 0 {
		return ct.Source == nil
	}
	return ct.Source != nil && equality.Semantic.DeepEqual(sources[0], *ct.Source)
}

func nilIfEmpty(s v1alpha1.ApplicationSources) v1alpha1.ApplicationSources {
	if len(s) == 0 {
		return nil
	}
	return s
}

func ignoreDifferencesEqual(a, b v1alpha1.IgnoreDifferences) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return equality.Semantic.DeepEqual(a, b)
}
