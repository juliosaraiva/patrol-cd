package lua

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	glua "github.com/yuin/gopher-lua"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The watchdog tests are not parallel: they assert process-wide counters.
//
// Only an evaluation that entered block() is sure to be abandoned. Under CPU starvation an
// evaluation goroutine can start after its deadline: the VM then stops before it calls block(),
// and exec returns ErrTimeout without counting a stalled evaluation (or counts one that ends as
// soon as it runs). So the tests count the evaluations that entered block() and assert on those,
// never on how many were started.

// blocker is a runtime whose scripts can call block(), a Go function that ignores the deadline
// until release is called: the stand-in for an uninterruptible library call.
type blocker struct {
	rt *Runtime
	// entered counts the evaluations that called block().
	entered atomic.Int64
	// enteredCh receives a value for each evaluation that called block() (dropped when full).
	enteredCh chan struct{}
	// release lets every blocked evaluation, and every later one, return from block().
	release func()
}

// newBlocker returns a blocker whose evaluations time out after timeout. Its evaluations are
// released when the test ends, and the test waits until none is stalled any more: a failed
// assertion must not leave blocked evaluations behind, because the stalled counter is
// process-wide and maxStalled of them refuse every later evaluation in the test binary.
func newBlocker(t *testing.T, timeout time.Duration) *blocker {
	t.Helper()
	released := make(chan struct{})
	b := &blocker{enteredCh: make(chan struct{}, maxStalled), release: sync.OnceFunc(func() { close(released) })}
	b.rt = New(nil, WithTimeout(timeout), withStateHook(func(L *glua.LState) {
		L.SetGlobal("block", L.NewFunction(func(*glua.LState) int {
			b.entered.Add(1)
			select {
			case b.enteredCh <- struct{}{}:
			default:
			}
			<-released
			return 0
		}))
	}))
	t.Cleanup(func() {
		b.release()
		assert.Eventually(t, func() bool { return stalledEvaluations.Load() == 0 }, 10*time.Second, 5*time.Millisecond,
			"released evaluations are still counted as stalled")
	})
	return b
}

const blockScript = `block() return {status = "Healthy"}`

// settle waits until the stalled count equals the evaluations of b that entered block(). An
// evaluation that entered block() is counted before its call returns and stays counted until it
// is released; one that was abandoned before it ran ends (or enters block()) as soon as it is
// scheduled. Callers start no evaluation while they settle.
func settle(t *testing.T, b *blocker) {
	t.Helper()
	require.Eventually(t, func() bool { return stalledEvaluations.Load() == b.entered.Load() }, 10*time.Second, time.Millisecond,
		"evaluations abandoned before they reached block() must end")
}

// stallExactly leaves exactly n evaluations of b blocked in block() and counted as stalled. Each
// round settles, then starts as many evaluations as are missing: since the stalled count equals
// the entered ones when a round starts, and each evaluation adds at most one, the count reaches n
// only with the round's last evaluation, so no evaluation is refused with ErrOverloaded early.
func stallExactly(t *testing.T, b *blocker, n int) {
	t.Helper()
	for range 100 {
		settle(t, b)
		missing := n - int(b.entered.Load())
		if missing == 0 {
			return
		}
		objs := make([]*unstructured.Unstructured, missing)
		for i := range objs {
			objs[i] = obj(t, rolloutYAML)
		}
		var wg sync.WaitGroup
		for _, o := range objs {
			wg.Go(func() {
				_, err := b.rt.ExecuteHealth(context.Background(), o, blockScript, false)
				assert.ErrorIs(t, err, ErrTimeout)
			})
		}
		wg.Wait()
	}
	t.Fatalf("only %d of %d evaluations reached block() in 100 rounds", b.entered.Load(), n)
}

// returnsWhileBlocked runs call and returns its error. Nothing releases the blocked evaluations
// before call returns, so a call that waited for its stuck evaluation would never return: the
// guard reports it (the test's cleanup then releases the evaluation).
func returnsWhileBlocked(t *testing.T, call func() error) error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- call() }()
	select {
	case err := <-errc:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("the caller waited for the evaluation stuck in block()")
		return nil
	}
}

func TestWatchdogAbandonsStuckEvaluation(t *testing.T) {
	require.Zero(t, stalledEvaluations.Load())
	b := newBlocker(t, 50*time.Millisecond)
	abandonedBefore := abandonedEvaluations.Load()

	// Evaluate until one enters block(); settling first means the last call is that one.
	var err error
	for round := 0; ; round++ {
		settle(t, b)
		if b.entered.Load() > 0 {
			break
		}
		require.Less(t, round, 100, "no evaluation reached block() in 100 attempts")
		o := obj(t, rolloutYAML)
		err = returnsWhileBlocked(t, func() error {
			_, err := b.rt.ExecuteHealth(context.Background(), o, blockScript, false)
			return err
		})
	}
	require.ErrorIs(t, err, ErrTimeout)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "exceeded execution timeout of 50ms")
	assert.Equal(t, int64(1), b.entered.Load())
	assert.Equal(t, int64(1), stalledEvaluations.Load())
	assert.GreaterOrEqual(t, abandonedEvaluations.Load()-abandonedBefore, int64(1))

	b.release()
	require.Eventually(t, func() bool { return stalledEvaluations.Load() == 0 }, 10*time.Second, 5*time.Millisecond)
}

func TestWatchdogCallerCancellation(t *testing.T) {
	require.Zero(t, stalledEvaluations.Load())
	b := newBlocker(t, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The caller gives up while the evaluation is stuck in block(), long before its deadline.
	go func() {
		select {
		case <-b.enteredCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	o := obj(t, rolloutYAML)
	err := returnsWhileBlocked(t, func() error {
		_, err := b.rt.ExecuteHealth(ctx, o, blockScript, false)
		return err
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrTimeout)
	assert.Contains(t, err.Error(), "lua script execution canceled")
	assert.Equal(t, int64(1), b.entered.Load())

	b.release()
	require.Eventually(t, func() bool { return stalledEvaluations.Load() == 0 }, 10*time.Second, 5*time.Millisecond)
}

func TestWatchdogOverload(t *testing.T) {
	require.Zero(t, stalledEvaluations.Load())
	b := newBlocker(t, 20*time.Millisecond)

	stallExactly(t, b, maxStalled)
	require.Equal(t, int64(maxStalled), stalledEvaluations.Load())

	_, err := b.rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), `return {status = "Healthy"}`, false)
	require.ErrorIs(t, err, ErrOverloaded)

	b.release()
	require.Eventually(t, func() bool { return stalledEvaluations.Load() == 0 }, 10*time.Second, 5*time.Millisecond)
	// The default timeout: a 20 ms one could expire before a starved goroutine runs.
	got, err := New(nil).ExecuteHealth(context.Background(), obj(t, rolloutYAML), `return {status = "Healthy"}`, false)
	require.NoError(t, err)
	assert.Equal(t, "Healthy", string(got.Status))
}

func TestRegisterMetrics(t *testing.T) {
	require.Zero(t, stalledEvaluations.Load())
	reg := prometheus.NewRegistry()
	require.NoError(t, RegisterMetrics(reg))
	require.NoError(t, RegisterMetrics(reg), "registering twice is harmless")

	b := newBlocker(t, 20*time.Millisecond)
	stallExactly(t, b, 1)

	values := func() map[string]float64 {
		families, err := reg.Gather()
		require.NoError(t, err)
		out := map[string]float64{}
		for _, f := range families {
			for _, m := range f.GetMetric() {
				switch {
				case m.GetGauge() != nil:
					out[f.GetName()] = m.GetGauge().GetValue()
				case m.GetCounter() != nil:
					out[f.GetName()] = m.GetCounter().GetValue()
				}
			}
		}
		return out
	}
	got := values()
	assert.Equal(t, float64(1), got["patrol_lua_stalled_evaluations"])
	assert.GreaterOrEqual(t, got["patrol_lua_abandoned_evaluations_total"], float64(1))

	b.release()
	require.Eventually(t, func() bool { return values()["patrol_lua_stalled_evaluations"] == 0 }, 10*time.Second, 5*time.Millisecond)
}
