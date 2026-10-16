package hydrator

import (
	"context"
	"sync"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// maxPendingKeys bounds the keys buffered before the manifest-hydration controller starts
// (distinct keys only; a key is a group of Applications, so this is generous).
const maxPendingKeys = 100000

// keyQueue is the source of the manifest-hydration controller: the app-hydration
// controller adds hydration keys to it. Keys added before the controller started are
// buffered (deduplicated) and flushed on Start. The workqueue never hands one key to two
// workers at once, which is what makes group status updates safe.
type keyQueue struct {
	mu      sync.Mutex
	q       workqueue.TypedRateLimitingInterface[Key]
	pending map[Key]struct{}
	dropped int
}

var _ source.TypedSource[Key] = (*keyQueue)(nil)

func newKeyQueue() *keyQueue { return &keyQueue{pending: map[Key]struct{}{}} }

// Start implements source.TypedSource. It is non-blocking.
func (s *keyQueue) Start(_ context.Context, q workqueue.TypedRateLimitingInterface[Key]) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.q = q
	for k := range s.pending {
		q.Add(k)
	}
	s.pending = nil
	return nil
}

// String implements fmt.Stringer (controller-runtime logs sources by name).
func (s *keyQueue) String() string { return "hydration-keys" }

func (s *keyQueue) add(k Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.q != nil {
		s.q.Add(k)
		return
	}
	if len(s.pending) >= maxPendingKeys {
		// Dropped keys are recovered by the next poll of their Applications.
		s.dropped++
		return
	}
	s.pending[k] = struct{}{}
}

// pendingKeys returns the buffered keys (tests).
func (s *keyQueue) pendingKeys() []Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Key, 0, len(s.pending))
	for k := range s.pending {
		out = append(out, k)
	}
	return out
}
