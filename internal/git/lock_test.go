package git

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initRecorder records init calls and their clean flag.
type initRecorder struct {
	mu     sync.Mutex
	cleans []bool
	closed atomic.Int32
}

func (r *initRecorder) init(clean bool) (io.Closer, error) {
	r.mu.Lock()
	r.cleans = append(r.cleans, clean)
	r.mu.Unlock()
	return closerFunc(func() error { r.closed.Add(1); return nil }), nil
}

func (r *initRecorder) calls() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.cleans...)
}

func TestRepoLockSameRevisionConcurrent(t *testing.T) {
	t.Parallel()
	var l RepoLock
	rec := &initRecorder{}
	ctx := testCtx(t)
	c1, err := l.Lock(ctx, "/repo", "rev1", true, rec.init)
	require.NoError(t, err)
	c2, err := l.Lock(ctx, "/repo", "rev1", true, rec.init)
	require.NoError(t, err, "same revision, both concurrent: shared")
	assert.Equal(t, []bool{true}, rec.calls(), "init runs once, clean on first use")
	require.NoError(t, c1.Close())
	assert.Equal(t, int32(0), rec.closed.Load(), "init closer waits for the last holder")
	require.NoError(t, c2.Close())
	require.NoError(t, c2.Close(), "release is idempotent")
	assert.Equal(t, int32(1), rec.closed.Load())

	// Same revision again after a concurrent hold: no clean needed.
	c3, err := l.Lock(ctx, "/repo", "rev1", true, rec.init)
	require.NoError(t, err)
	require.NoError(t, c3.Close())
	assert.Equal(t, []bool{true, false}, rec.calls())
}

func TestRepoLockCleanRules(t *testing.T) {
	t.Parallel()
	var l RepoLock
	rec := &initRecorder{}
	ctx := testCtx(t)
	steps := []struct {
		rev        string
		concurrent bool
		wantClean  bool
	}{
		{"a", true, true},   // first use
		{"a", true, false},  // same revision, previous concurrent
		{"b", true, true},   // revision changed
		{"b", false, false}, // same revision, previous concurrent
		{"b", true, true},   // previous was exclusive
	}
	for i, s := range steps {
		c, err := l.Lock(ctx, "/repo", s.rev, s.concurrent, rec.init)
		require.NoError(t, err)
		require.NoError(t, c.Close())
		assert.Equal(t, s.wantClean, rec.calls()[i], "step %d", i)
	}
}

func TestRepoLockDifferentRevisionWaits(t *testing.T) {
	t.Parallel()
	var l RepoLock
	rec := &initRecorder{}
	ctx := testCtx(t)
	c1, err := l.Lock(ctx, "/repo", "a", true, rec.init)
	require.NoError(t, err)

	acquired := make(chan io.Closer)
	go func() {
		c, err := l.Lock(ctx, "/repo", "b", true, rec.init)
		assert.NoError(t, err)
		acquired <- c
	}()
	select {
	case <-acquired:
		t.Fatal("different revision must wait")
	case <-time.After(100 * time.Millisecond):
	}
	// Exclusive request for the same revision waits too.
	exclusive := make(chan error, 1)
	shortCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	go func() {
		_, err := l.Lock(shortCtx, "/repo", "a", false, rec.init)
		exclusive <- err
	}()
	assert.ErrorIs(t, <-exclusive, context.DeadlineExceeded)

	// Other paths are independent.
	other, err := l.Lock(ctx, "/other", "z", false, rec.init)
	require.NoError(t, err)
	require.NoError(t, other.Close())

	require.NoError(t, c1.Close())
	select {
	case c := <-acquired:
		require.NoError(t, c.Close())
	case <-time.After(5 * time.Second):
		t.Fatal("waiter not released")
	}
}

func TestRepoLockInitFailure(t *testing.T) {
	t.Parallel()
	var l RepoLock
	ctx := testCtx(t)
	boom := errors.New("boom")
	_, err := l.Lock(ctx, "/repo", "a", true, func(bool) (io.Closer, error) { return nil, boom })
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "failed to initialize repository resources")

	rec := &initRecorder{}
	c, err := l.Lock(ctx, "/repo", "a", true, rec.init)
	require.NoError(t, err)
	require.NoError(t, c.Close())
	assert.Equal(t, []bool{true}, rec.calls(), "a failed init forces a clean")

	_, err = l.Lock(ctx, "/repo", "a", true, nil)
	assert.Error(t, err)
}

func TestRepoLockSerializesExclusiveHolders(t *testing.T) {
	t.Parallel()
	var l RepoLock
	ctx := testCtx(t)
	var active, maxActive atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			rev := []string{"a", "b"}[i%2]
			concurrent := i%3 != 0
			c, err := l.Lock(ctx, "/repo", rev, concurrent, func(bool) (io.Closer, error) { return NopCloser, nil })
			if !assert.NoError(t, err) {
				return
			}
			n := active.Add(1)
			for {
				m := maxActive.Load()
				if n <= m || maxActive.CompareAndSwap(m, n) {
					break
				}
			}
			if !concurrent {
				assert.Equal(t, int32(1), n, "exclusive holder must be alone")
			}
			time.Sleep(2 * time.Millisecond)
			active.Add(-1)
			assert.NoError(t, c.Close())
		})
	}
	wg.Wait()
}

func TestRepoLockInitBlocksJoiners(t *testing.T) {
	t.Parallel()
	var l RepoLock
	ctx := testCtx(t)
	release := make(chan struct{})
	started := make(chan struct{})
	var initDone atomic.Bool
	go func() {
		c, err := l.Lock(ctx, "/repo", "a", true, func(bool) (io.Closer, error) {
			close(started)
			<-release
			initDone.Store(true)
			return NopCloser, nil
		})
		assert.NoError(t, err)
		<-time.After(50 * time.Millisecond)
		assert.NoError(t, c.Close())
	}()
	<-started
	joined := make(chan struct{})
	go func() {
		c, err := l.Lock(ctx, "/repo", "a", true, func(bool) (io.Closer, error) { return NopCloser, nil })
		assert.NoError(t, err)
		assert.True(t, initDone.Load(), "a joiner must not proceed before the checkout is ready")
		assert.NoError(t, c.Close())
		close(joined)
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-joined
}
