package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// RepoLock serializes use of persistent working copies, mirroring upstream's
// repositoryLock: per working-copy path, operations on the same revision that
// all allow concurrency share the checkout (reference counted); any other
// operation waits until the in-flight ones finish. Different paths never block
// each other. The zero value is ready to use.
//
// The first holder of an idle path runs init (typically CheckoutRevision) while
// other callers for that path wait; init receives clean=true on first use, when
// the previous holder was exclusive (or its init failed), or when the revision
// differs from the last completed one, so state left by a previous operation
// (vendored charts, kustomize edits) is removed. The closer returned by init runs
// when the last holder releases.
type RepoLock struct {
	mu     sync.Mutex
	states map[string]*lockState
}

type lockState struct {
	revision        string // revision of current holders, "" when idle
	lastRevision    string // revision of the last completed hold
	used            bool
	count           int
	allowConcurrent bool
	initializing    bool
	initCloser      io.Closer
	changed         chan struct{} // closed (and replaced) on every transition waiters care about
}

func (st *lockState) notify() {
	close(st.changed)
	st.changed = make(chan struct{})
}

// Lock acquires path for revision. It blocks until the path is idle, or until it
// is held for the same revision by holders that all allow concurrency (and
// allowConcurrent is true). It returns the context error when ctx ends while
// waiting. The returned closer releases the hold; it is idempotent.
func (l *RepoLock) Lock(ctx context.Context, path, revision string, allowConcurrent bool, init func(clean bool) (io.Closer, error)) (io.Closer, error) {
	if init == nil {
		return nil, errors.New("git: RepoLock.Lock requires an init function")
	}
	for {
		l.mu.Lock()
		if l.states == nil {
			l.states = map[string]*lockState{}
		}
		st, ok := l.states[path]
		if !ok {
			st = &lockState{changed: make(chan struct{})}
			l.states[path] = st
		}
		if !st.initializing {
			if st.count == 0 {
				return l.initialize(st, revision, allowConcurrent, init)
			}
			if st.revision == revision && st.allowConcurrent && allowConcurrent {
				st.count++
				l.mu.Unlock()
				return l.releaser(st), nil
			}
		}
		changed := st.changed
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

// initialize runs init for an idle path. It is entered with l.mu held and
// returns with it released.
func (l *RepoLock) initialize(st *lockState, revision string, allowConcurrent bool, init func(bool) (io.Closer, error)) (io.Closer, error) {
	clean := !st.used || !st.allowConcurrent || st.lastRevision != revision
	st.initializing = true
	st.count = 1
	st.revision = revision
	st.allowConcurrent = allowConcurrent
	l.mu.Unlock()

	closer, err := init(clean)

	l.mu.Lock()
	defer l.mu.Unlock()
	st.initializing = false
	st.used = true
	if err != nil {
		st.count = 0
		st.revision = ""
		st.allowConcurrent = false // the next holder must clean
		st.notify()
		return nil, fmt.Errorf("failed to initialize repository resources: %w", err)
	}
	st.initCloser = closer
	st.notify()
	return l.releaser(st), nil
}

func (l *RepoLock) releaser(st *lockState) io.Closer {
	var once sync.Once
	return closerFunc(func() error {
		var err error
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			st.count--
			if st.count > 0 {
				return
			}
			st.lastRevision = st.revision
			st.revision = ""
			if st.initCloser != nil {
				if cerr := st.initCloser.Close(); cerr != nil {
					err = fmt.Errorf("init closer failed: %w", cerr)
				}
				st.initCloser = nil
			}
			st.notify()
		})
		return err
	})
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// NopCloser is an io.Closer that does nothing, for init functions without
// resources to release.
var NopCloser io.Closer = closerFunc(func() error { return nil })
