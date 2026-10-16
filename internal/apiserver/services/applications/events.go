package applications

import (
	"sync"

	"k8s.io/client-go/tools/cache"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Event is a change of an Application. Application is shared between subscribers and must
// not be modified.
type Event struct {
	// Type is apiclient.EventAdded, EventModified or EventDeleted.
	Type        string
	Application *v1alpha1.Application
}

// Events delivers Application changes to subscribers.
type Events interface {
	// Subscribe returns a channel receiving every Application event from now on, and a
	// function ending the subscription. The channel is closed after the subscription ends
	// or when the subscriber fell behind (its buffer was full); a subscriber whose channel
	// closed unexpectedly must resynchronize.
	Subscribe() (<-chan Event, func())
}

// Broadcaster fans Application events out to subscribers. It implements Events and
// client-go's cache.ResourceEventHandler, so the server wires it to the Application
// informer of its cache:
//
//	inf, _ := mgrCache.GetInformer(ctx, &v1alpha1.Application{})
//	_, _ = inf.AddEventHandler(broadcaster)
//
// Publishing never blocks: a subscriber whose buffer is full is disconnected.
type Broadcaster struct {
	mu     sync.Mutex
	subs   map[*subscriber]struct{}
	buffer int
}

type subscriber struct {
	ch     chan Event
	closed bool
}

var (
	_ Events                     = (*Broadcaster)(nil)
	_ cache.ResourceEventHandler = (*Broadcaster)(nil)
)

// NewBroadcaster returns a Broadcaster with a per-subscriber buffer of buffer events
// (default DefaultWatchBuffer when <= 0).
func NewBroadcaster(buffer int) *Broadcaster {
	if buffer <= 0 {
		buffer = DefaultWatchBuffer
	}
	return &Broadcaster{subs: map[*subscriber]struct{}{}, buffer: buffer}
}

// Subscribe implements Events.
func (b *Broadcaster) Subscribe() (<-chan Event, func()) {
	sub := &subscriber{ch: make(chan Event, b.buffer)}
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return sub.ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			delete(b.subs, sub)
			if !sub.closed {
				sub.closed = true
				close(sub.ch)
			}
		})
	}
}

// Subscribers returns the number of active subscribers.
func (b *Broadcaster) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Publish sends ev to every subscriber without blocking; subscribers whose buffer is full
// are disconnected (their channel is closed).
func (b *Broadcaster) Publish(ev Event) {
	if ev.Application == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs {
		if sub.closed {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			sub.closed = true
			close(sub.ch)
			delete(b.subs, sub)
		}
	}
}

// OnAdd implements cache.ResourceEventHandler.
func (b *Broadcaster) OnAdd(obj any, _ bool) { b.publishObj(apiclient.EventAdded, obj) }

// OnUpdate implements cache.ResourceEventHandler.
func (b *Broadcaster) OnUpdate(_, newObj any) { b.publishObj(apiclient.EventModified, newObj) }

// OnDelete implements cache.ResourceEventHandler.
func (b *Broadcaster) OnDelete(obj any) {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	b.publishObj(apiclient.EventDeleted, obj)
}

func (b *Broadcaster) publishObj(typ string, obj any) {
	app, ok := obj.(*v1alpha1.Application)
	if !ok || app == nil {
		return
	}
	b.Publish(Event{Type: typ, Application: app.DeepCopy()})
}
