package controller

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/juliosaraiva/patrol-cd/internal/notifications"
)

// stateDelta is the change processing made to a delivery state: keys recorded (with their
// timestamp) and keys cleared. Applying a delta to a newer copy of the annotation (after a
// conflict) keeps entries written concurrently by others.
type stateDelta struct {
	set map[string]int64
	del map[string]struct{}
}

func diffState(before, after notifications.DeliveryState) stateDelta {
	d := stateDelta{set: map[string]int64{}, del: map[string]struct{}{}}
	for k, v := range after {
		if old, ok := before[k]; !ok || old != v {
			d.set[k] = v
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			d.del[k] = struct{}{}
		}
	}
	return d
}

func (d stateDelta) empty() bool { return len(d.set) == 0 && len(d.del) == 0 }

// applyTo returns a copy of s with the delta applied, truncated like the engine does.
func (d stateDelta) applyTo(s notifications.DeliveryState) notifications.DeliveryState {
	out := s.Clone()
	for k := range d.del {
		delete(out, k)
	}
	for k, v := range d.set {
		out[k] = v
	}
	out.Truncate(notifications.MaxDeliveryStateEntries)
	return out
}

// memoTTL bounds how long the controller remembers its own writes and unpersisted deliveries.
const memoTTL = 5 * time.Minute

// memo remembers, per Application, what this process wrote and could not write:
//
//   - writtenRV: the resourceVersion of our last successful state patch. Until the cache shows
//     that version (or a live read shows the cache is current), the Application is read live, so
//     an event queued before our write cannot make us re-send what we just recorded.
//   - unpersisted: deliveries made whose state patch failed; merged into the next evaluation so
//     a transient API error does not cause duplicates.
//   - failures: consecutive evaluations with failed deliveries, for the retry backoff.
//
// Entries expire after memoTTL and are dropped when the Application disappears.
type memo struct {
	mu      sync.Mutex
	entries map[types.NamespacedName]*memoEntry
	now     func() time.Time
}

type memoEntry struct {
	writtenRV     string
	writtenAt     time.Time
	unpersisted   *stateDelta
	unpersistedAt time.Time
	failures      int
}

func newMemo(now func() time.Time) *memo {
	return &memo{entries: map[types.NamespacedName]*memoEntry{}, now: now}
}

func (m *memo) entry(key types.NamespacedName) *memoEntry {
	e, ok := m.entries[key]
	if !ok {
		e = &memoEntry{}
		m.entries[key] = e
	}
	return e
}

func (m *memo) gc(key types.NamespacedName, e *memoEntry) {
	now := m.now()
	if e.writtenRV != "" && now.Sub(e.writtenAt) > memoTTL {
		e.writtenRV = ""
	}
	if e.unpersisted != nil && now.Sub(e.unpersistedAt) > memoTTL {
		e.unpersisted = nil
	}
	if e.writtenRV == "" && e.unpersisted == nil && e.failures == 0 {
		delete(m.entries, key)
	}
}

// pendingWrite returns the resourceVersion of our last write not yet confirmed in the cache.
func (m *memo) pendingWrite(key types.NamespacedName) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return ""
	}
	m.gc(key, e)
	return e.writtenRV
}

func (m *memo) confirmWrite(key types.NamespacedName) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[key]; ok {
		e.writtenRV = ""
		m.gc(key, e)
	}
}

// written records a successful state patch (clearing unpersisted deliveries).
func (m *memo) written(key types.NamespacedName, rv string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entry(key)
	e.writtenRV, e.writtenAt = rv, m.now()
	e.unpersisted = nil
}

// unpersistedDelta returns deliveries whose state could not be written yet.
func (m *memo) unpersistedDelta(key types.NamespacedName) (stateDelta, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return stateDelta{}, false
	}
	m.gc(key, e)
	if e.unpersisted == nil {
		return stateDelta{}, false
	}
	return *e.unpersisted, true
}

// clearUnpersisted drops unpersisted deliveries once the stored state contains them.
func (m *memo) clearUnpersisted(key types.NamespacedName) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[key]; ok {
		e.unpersisted = nil
		m.gc(key, e)
	}
}

// failedWrite keeps d (which already includes previously unpersisted changes) for the next
// evaluation.
func (m *memo) failedWrite(key types.NamespacedName, d stateDelta) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entry(key)
	e.unpersisted, e.unpersistedAt = &d, m.now()
}

// recordOutcome updates the failure streak and returns it.
func (m *memo) recordOutcome(key types.NamespacedName, failed bool) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !failed {
		if e, ok := m.entries[key]; ok {
			e.failures = 0
			m.gc(key, e)
		}
		return 0
	}
	e := m.entry(key)
	e.failures++
	return e.failures
}

func (m *memo) forget(key types.NamespacedName) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
}
