package applications

import (
	"context"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	pubapi "github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func runtimeToMap(obj runtime.Object) (map[string]any, error) {
	return runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
}

// watchApplications serves GET /stream/applications (upstream Watch). Without a
// resourceVersion, or for a single named Application, the current matching Applications
// are sent first as ADDED (sorted by qualified name). Every event is authorized against
// the Application's state in that event (applications, get): an Application the caller
// saw but may no longer see (moved to another project, labels no longer matching) is
// reported as DELETED with the last state the caller was allowed to see, so nothing of its
// new state leaks.
func (s *Service) watchApplications(ctx context.Context, q *pubapi.ApplicationQuery, sink *rest.Sink[pubapi.ApplicationWatchEvent]) error {
	if s.d.Events == nil || s.d.Reader == nil {
		return unavailable("the application event stream")
	}
	f, err := s.newAppFilter(q, true)
	if err != nil {
		return err
	}
	events, cancel := s.d.Events.Subscribe()
	defer cancel()

	w := &appWatch{s: s, ctx: ctx, f: f, sink: sink, visible: map[types.NamespacedName]*v1alpha1.Application{}}
	if q.ResourceVersion == "" || q.Name != "" {
		opts, ok := s.listScope(f.namespace)
		if ok {
			list := &v1alpha1.ApplicationList{}
			opts = append(opts, client.MatchingLabelsSelector{Selector: f.selector})
			if err := s.d.Reader.List(ctx, list, opts...); err != nil {
				return err
			}
			items := make([]*v1alpha1.Application, 0, len(list.Items))
			for i := range list.Items {
				items = append(items, &list.Items[i])
			}
			slices.SortFunc(items, func(a, b *v1alpha1.Application) int {
				return compareQualified(a, b)
			})
			for _, a := range items {
				if err := w.handle(Event{Type: pubapi.EventAdded, Application: a}); err != nil {
					return err
				}
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return rest.ResourceExhausted("the application event stream fell behind; reconnect to resume")
			}
			if err := w.handle(ev); err != nil {
				return err
			}
		}
	}
}

func compareQualified(a, b *v1alpha1.Application) int {
	if a.Namespace != b.Namespace {
		if a.Namespace < b.Namespace {
			return -1
		}
		return 1
	}
	switch {
	case a.Name < b.Name:
		return -1
	case a.Name > b.Name:
		return 1
	}
	return 0
}

// appWatch is the per-stream state: the Applications sent and still visible to the caller.
type appWatch struct {
	s       *Service
	ctx     context.Context
	f       *appFilter
	sink    *rest.Sink[pubapi.ApplicationWatchEvent]
	visible map[types.NamespacedName]*v1alpha1.Application
	// healthPausedUntil: resource health is not read from the controller before this instant
	// (set for resourceHealthPause when the controller did not answer).
	healthPausedUntil time.Time
}

// send sends an event. ADDED and MODIFIED events carry the resource health kept in the
// controller's resource tree (upstream Watch, withTreeHealth); DELETED events are sent as
// they are (the controller drops the state of deleted Applications). a is never modified:
// it is shared with the other subscribers.
func (w *appWatch) send(typ string, a *v1alpha1.Application) error {
	out := a
	if typ != pubapi.EventDeleted && !w.s.now().Before(w.healthPausedUntil) {
		filled, err := w.s.withTreeHealth(w.ctx, a)
		if pausesTreeHealth(err) {
			w.healthPausedUntil = w.s.now().Add(resourceHealthPause)
		}
		out = filled
	}
	return w.sink.SendWithID(out.ResourceVersion, pubapi.ApplicationWatchEvent{Type: typ, Object: *out})
}

func (w *appWatch) handle(ev Event) error {
	a := ev.Application
	if a == nil {
		return nil
	}
	key := types.NamespacedName{Namespace: a.Namespace, Name: a.Name}
	prev, seen := w.visible[key]
	allowed := w.s.filterMatches(w.f, a) && w.s.canApp(w.ctx, rbac.ActionGet, a)
	if ev.Type == pubapi.EventDeleted {
		switch {
		case seen:
			delete(w.visible, key)
			last := prev
			if allowed {
				last = a
			}
			return w.send(pubapi.EventDeleted, last)
		case allowed:
			return w.send(pubapi.EventDeleted, a)
		}
		return nil
	}
	if !allowed {
		if seen {
			delete(w.visible, key)
			return w.send(pubapi.EventDeleted, prev)
		}
		return nil
	}
	if seen && prev.UID == a.UID && !newerRV(a.ResourceVersion, prev.ResourceVersion) {
		return nil // already sent this or a later state
	}
	w.visible[key] = a
	return w.send(ev.Type, a)
}
