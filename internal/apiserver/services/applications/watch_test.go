package applications_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

type watchResult struct {
	ev  apiclient.ApplicationWatchEvent
	err error
}

// watchEvents reads decoded watch events from s into a channel.
func watchEvents(t *testing.T, s *apiclient.EventStream) <-chan watchResult {
	t.Helper()
	ch := make(chan watchResult, 100)
	go func() {
		defer close(ch)
		for {
			ev, err := s.Next()
			if err != nil {
				ch <- watchResult{err: err}
				return
			}
			var we apiclient.ApplicationWatchEvent
			if err := ev.Decode(&we); err != nil {
				ch <- watchResult{err: err}
				return
			}
			ch <- watchResult{ev: we}
		}
	}()
	return ch
}

func next(t *testing.T, ch <-chan watchResult) apiclient.ApplicationWatchEvent {
	t.Helper()
	select {
	case r, ok := <-ch:
		require.True(t, ok, "stream closed")
		require.NoError(t, r.err)
		return r.ev
	case <-time.After(10 * time.Second):
		t.Fatal("no watch event within 10s")
	}
	return apiclient.ApplicationWatchEvent{}
}

// noEvent asserts nothing arrives for a while.
func noEvent(t *testing.T, ch <-chan watchResult, d time.Duration) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("unexpected event %s %s/%s (err %v)", r.ev.Type, r.ev.Object.Namespace, r.ev.Object.Name, r.err)
	case <-time.After(d):
	}
}

func TestWatchApplications(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.createApp(e.newApp("a1", "proj-a"))
	e.createApp(e.newApp("a0", "proj-a"))
	e.createApp(e.newApp("b1", "proj-b"))

	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	s, err := e.client("alice").WatchApplications(ctx, &apiclient.ApplicationQuery{}, nil)
	require.NoError(t, err)
	ch := watchEvents(t, s)

	// Initial state: alice's Applications only, sorted.
	ev := next(t, ch)
	assert.Equal(t, apiclient.EventAdded, ev.Type)
	assert.Equal(t, "a0", ev.Object.Name)
	ev = next(t, ch)
	assert.Equal(t, "a1", ev.Object.Name)

	// Another tenant's changes are never sent.
	e.createApp(e.newApp("b2", "proj-b"))
	e.updateApp("b1", func(a *v1alpha1.Application) { a.Labels = map[string]string{"x": "y"} })
	noEvent(t, ch, 500*time.Millisecond)

	// Own changes are.
	e.updateApp("a1", func(a *v1alpha1.Application) { a.Labels = map[string]string{"x": "y"} })
	ev = next(t, ch)
	assert.Equal(t, apiclient.EventModified, ev.Type)
	assert.Equal(t, "a1", ev.Object.Name)
	assert.Equal(t, "y", ev.Object.Labels["x"])

	// Moving an Application to a project alice cannot see is a DELETED event carrying the
	// state she was allowed to see.
	e.updateApp("a1", func(a *v1alpha1.Application) {
		a.Spec.Project = "proj-b"
		a.Annotations = map[string]string{"secret-plan": "moved"}
	})
	ev = next(t, ch)
	assert.Equal(t, apiclient.EventDeleted, ev.Type)
	assert.Equal(t, "a1", ev.Object.Name)
	assert.Equal(t, "proj-a", ev.Object.Spec.Project)
	assert.NotContains(t, ev.Object.Annotations, "secret-plan")
	// Further changes of it stay invisible.
	e.updateApp("a1", func(a *v1alpha1.Application) { a.Labels = map[string]string{"x": "z"} })
	noEvent(t, ch, 300*time.Millisecond)
	// Moving back makes it visible again.
	e.updateApp("a1", func(a *v1alpha1.Application) { a.Spec.Project = "proj-a" })
	ev = next(t, ch)
	assert.Equal(t, "a1", ev.Object.Name)
	assert.Equal(t, "proj-a", ev.Object.Spec.Project)

	// Deletion.
	app := e.getApp(e.ns, "a0")
	require.NoError(t, e.kube.Delete(e.ctx, app))
	ev = next(t, ch)
	assert.Equal(t, apiclient.EventDeleted, ev.Type)
	assert.Equal(t, "a0", ev.Object.Name)
}

func TestWatchApplicationFilters(t *testing.T) {
	e := newEnv(t, envOptions{})
	web := e.newApp("web", "proj-a")
	web.Labels = map[string]string{"tier": "web"}
	stored := e.createApp(web)
	e.createApp(e.newApp("db", "proj-a"))

	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	// A single named Application: initial ADDED even with a resourceVersion.
	s, err := e.client("admin").WatchApplications(ctx, &apiclient.ApplicationQuery{Name: "web", ResourceVersion: stored.ResourceVersion}, nil)
	require.NoError(t, err)
	ch := watchEvents(t, s)
	ev := next(t, ch)
	assert.Equal(t, apiclient.EventAdded, ev.Type)
	assert.Equal(t, "web", ev.Object.Name)
	e.updateApp("db", func(a *v1alpha1.Application) { a.Labels = map[string]string{"k": "v"} })
	noEvent(t, ch, 300*time.Millisecond)

	// A selector; a resourceVersion without a name skips the initial state. Labels that stop
	// matching end the visibility with a DELETED event.
	s2, err := e.client("admin").WatchApplications(ctx, &apiclient.ApplicationQuery{Selector: "tier=web", ResourceVersion: stored.ResourceVersion}, nil)
	require.NoError(t, err)
	ch2 := watchEvents(t, s2)
	noEvent(t, ch2, 300*time.Millisecond)
	e.updateApp("web", func(a *v1alpha1.Application) { a.Annotations = map[string]string{"n": "1"} })
	ev = next(t, ch2)
	assert.Equal(t, apiclient.EventModified, ev.Type)
	e.updateApp("web", func(a *v1alpha1.Application) { a.Labels = map[string]string{"tier": "db"} })
	ev = next(t, ch2)
	assert.Equal(t, apiclient.EventDeleted, ev.Type)
	assert.Equal(t, "web", ev.Object.Labels["tier"], "the last visible state")

	// Errors: before the stream starts, and as an error event.
	_, err = e.client("").WatchApplications(ctx, &apiclient.ApplicationQuery{}, nil)
	requireProblem(t, err, 401, "")
	s3, err := e.client("admin").WatchApplications(ctx, &apiclient.ApplicationQuery{Selector: "a in (("}, nil)
	require.NoError(t, err)
	_, err = s3.Next()
	requireProblem(t, err, 400, "error parsing the selector")
}

func TestWatchApplicationsUnavailable(t *testing.T) {
	e := newEnv(t, envOptions{noEvents: true})
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	s, err := e.client("admin").WatchApplications(ctx, &apiclient.ApplicationQuery{}, nil)
	require.NoError(t, err)
	_, err = s.Next()
	requireProblem(t, err, 503, "the application event stream is not available")
}
