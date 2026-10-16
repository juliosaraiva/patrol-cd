package lua

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	glua "github.com/yuin/gopher-lua"
)

// blockingRuntime returns a runtime whose scripts can call block(), a Go function that
// ignores the deadline until release is closed: the stand-in for an uninterruptible
// library call.
func blockingRuntime(timeout time.Duration, release <-chan struct{}) *Runtime {
	return New(nil, WithTimeout(timeout), withStateHook(func(L *glua.LState) {
		L.SetGlobal("block", L.NewFunction(func(*glua.LState) int {
			<-release
			return 0
		}))
	}))
}

const blockScript = `block() return {status = "Healthy"}`

// The watchdog tests are not parallel: they assert process-wide counters.

func TestWatchdogAbandonsStuckEvaluation(t *testing.T) {
	require.Zero(t, stalledEvaluations.Load())
	release := make(chan struct{})
	rt := blockingRuntime(50*time.Millisecond, release)
	abandonedBefore := abandonedEvaluations.Load()

	start := time.Now()
	_, err := rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), blockScript, false)
	elapsed := time.Since(start)
	require.ErrorIs(t, err, ErrTimeout)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "exceeded execution timeout of 50ms")
	assert.Less(t, elapsed, 2*time.Second, "the caller must not wait for the stuck call")
	assert.Equal(t, int64(1), stalledEvaluations.Load())
	assert.Equal(t, abandonedBefore+1, abandonedEvaluations.Load())

	close(release)
	require.Eventually(t, func() bool { return stalledEvaluations.Load() == 0 }, 5*time.Second, 5*time.Millisecond)
}

func TestWatchdogCallerCancellation(t *testing.T) {
	require.Zero(t, stalledEvaluations.Load())
	release := make(chan struct{})
	rt := blockingRuntime(10*time.Second, release)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)

	start := time.Now()
	_, err := rt.ExecuteHealth(ctx, obj(t, rolloutYAML), blockScript, false)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrTimeout)
	assert.Contains(t, err.Error(), "lua script execution canceled")
	assert.Less(t, time.Since(start), 2*time.Second)

	close(release)
	require.Eventually(t, func() bool { return stalledEvaluations.Load() == 0 }, 5*time.Second, 5*time.Millisecond)
}

func TestWatchdogOverload(t *testing.T) {
	require.Zero(t, stalledEvaluations.Load())
	release := make(chan struct{})
	rt := blockingRuntime(20*time.Millisecond, release)

	var wg sync.WaitGroup
	for range maxStalled {
		wg.Go(func() {
			_, err := rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), blockScript, false)
			assert.ErrorIs(t, err, ErrTimeout)
		})
	}
	wg.Wait()
	require.Equal(t, int64(maxStalled), stalledEvaluations.Load())

	_, err := rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), `return {status = "Healthy"}`, false)
	require.ErrorIs(t, err, ErrOverloaded)

	close(release)
	require.Eventually(t, func() bool { return stalledEvaluations.Load() == 0 }, 5*time.Second, 5*time.Millisecond)
	got, err := rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), `return {status = "Healthy"}`, false)
	require.NoError(t, err)
	assert.Equal(t, "Healthy", string(got.Status))
}

func TestRegisterMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	require.NoError(t, RegisterMetrics(reg))
	require.NoError(t, RegisterMetrics(reg), "registering twice is harmless")

	release := make(chan struct{})
	rt := blockingRuntime(20*time.Millisecond, release)
	_, err := rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), blockScript, false)
	require.ErrorIs(t, err, ErrTimeout)

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

	close(release)
	require.Eventually(t, func() bool { return values()["patrol_lua_stalled_evaluations"] == 0 }, 5*time.Second, 5*time.Millisecond)
}
