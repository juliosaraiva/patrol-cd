package controller

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/internal/notifications"
)

func TestDiffStateAndApply(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		before, after notifications.DeliveryState
		current       notifications.DeliveryState
		want          notifications.DeliveryState
		empty         bool
	}{
		{name: "no change", before: notifications.DeliveryState{"a": 1}, after: notifications.DeliveryState{"a": 1},
			current: notifications.DeliveryState{"a": 1, "x": 9}, want: notifications.DeliveryState{"a": 1, "x": 9}, empty: true},
		{name: "added", before: notifications.DeliveryState{"a": 1}, after: notifications.DeliveryState{"a": 1, "b": 2},
			current: notifications.DeliveryState{"a": 1, "x": 9}, want: notifications.DeliveryState{"a": 1, "b": 2, "x": 9}},
		{name: "removed", before: notifications.DeliveryState{"a": 1, "b": 2}, after: notifications.DeliveryState{"a": 1},
			current: notifications.DeliveryState{"a": 1, "b": 2, "x": 9}, want: notifications.DeliveryState{"a": 1, "x": 9}},
		{name: "re-recorded with a new timestamp", before: notifications.DeliveryState{"a": 1}, after: notifications.DeliveryState{"a": 5},
			current: notifications.DeliveryState{"a": 3}, want: notifications.DeliveryState{"a": 5}},
		{name: "from empty", before: nil, after: notifications.DeliveryState{"a": 1},
			current: nil, want: notifications.DeliveryState{"a": 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := diffState(tc.before, tc.after)
			assert.Equal(t, tc.empty, d.empty())
			cur := tc.current.Clone()
			assert.Equal(t, tc.want, d.applyTo(tc.current))
			if tc.current != nil {
				assert.Equal(t, cur, tc.current, "applyTo must not modify its input")
			}
		})
	}
}

func TestDeltaApplyTruncates(t *testing.T) {
	t.Parallel()
	after := notifications.DeliveryState{}
	for i := range notifications.MaxDeliveryStateEntries + 20 {
		after[fmt.Sprintf("k%03d", i)] = int64(i)
	}
	got := diffState(nil, after).applyTo(nil)
	assert.Len(t, got, notifications.MaxDeliveryStateEntries)
	_, newest := got[fmt.Sprintf("k%03d", notifications.MaxDeliveryStateEntries+19)]
	assert.True(t, newest, "the newest entries are kept")
}

// Applying the delta between two states to the first yields the second (when no truncation is
// involved), and entries the delta does not touch survive.
func TestDeltaRoundTripProperty(t *testing.T) {
	t.Parallel()
	gen := rapid.MapOfN(rapid.StringMatching(`[a-e]{1,2}`), rapid.Int64Range(1, 50), 0, 20)
	rapid.Check(t, func(t *rapid.T) {
		before := notifications.DeliveryState(gen.Draw(t, "before"))
		after := notifications.DeliveryState(gen.Draw(t, "after"))
		foreign := notifications.DeliveryState(rapid.MapOfN(rapid.StringMatching(`[x-z]{3}`), rapid.Int64Range(1, 50), 0, 5).Draw(t, "foreign"))
		d := diffState(before, after)
		got := d.applyTo(before)
		assert.Equal(t, len(after), len(got))
		for k, v := range after {
			assert.Equal(t, v, got[k])
		}
		current := before.Clone()
		for k, v := range foreign {
			current[k] = v
		}
		merged := d.applyTo(current)
		for k, v := range foreign {
			assert.Equal(t, v, merged[k], "concurrent entry %s kept", k)
		}
		for k, v := range after {
			assert.Equal(t, v, merged[k])
		}
	})
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestMemoLifecycle(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	m := newMemo(clock.Now)
	key := types.NamespacedName{Namespace: "ns", Name: "app"}

	assert.Empty(t, m.pendingWrite(key))
	_, ok := m.unpersistedDelta(key)
	assert.False(t, ok)

	// A failed write is remembered and merged into the next evaluation.
	d := diffState(nil, notifications.DeliveryState{"k": 1})
	m.failedWrite(key, d)
	got, ok := m.unpersistedDelta(key)
	require.True(t, ok)
	assert.Equal(t, d, got)

	// A successful write clears it and remembers the written resourceVersion.
	m.written(key, "42")
	_, ok = m.unpersistedDelta(key)
	assert.False(t, ok)
	assert.Equal(t, "42", m.pendingWrite(key))
	m.confirmWrite(key)
	assert.Empty(t, m.pendingWrite(key))
	assert.Empty(t, m.entries, "an entry without information is dropped")

	// Entries expire.
	m.written(key, "43")
	m.failedWrite(key, d)
	clock.Advance(memoTTL + time.Second)
	assert.Empty(t, m.pendingWrite(key))
	_, ok = m.unpersistedDelta(key)
	assert.False(t, ok)
	assert.Empty(t, m.entries)

	// Failure streaks.
	assert.Equal(t, 1, m.recordOutcome(key, true))
	assert.Equal(t, 2, m.recordOutcome(key, true))
	assert.Equal(t, 0, m.recordOutcome(key, false))
	assert.Empty(t, m.entries)

	// clearUnpersisted and forget.
	m.failedWrite(key, d)
	m.clearUnpersisted(key)
	_, ok = m.unpersistedDelta(key)
	assert.False(t, ok)
	m.written(key, "44")
	m.forget(key)
	assert.Empty(t, m.pendingWrite(key))
}

func TestMemoConcurrentUse(t *testing.T) {
	t.Parallel()
	m := newMemo(time.Now)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			key := types.NamespacedName{Namespace: "ns", Name: fmt.Sprintf("app-%d", i%4)}
			for j := range 200 {
				m.written(key, fmt.Sprint(j))
				m.failedWrite(key, diffState(nil, notifications.DeliveryState{"k": int64(j)}))
				_ = m.pendingWrite(key)
				_, _ = m.unpersistedDelta(key)
				m.recordOutcome(key, j%2 == 0)
				m.confirmWrite(key)
				m.clearUnpersisted(key)
			}
		})
	}
	wg.Wait()
}
