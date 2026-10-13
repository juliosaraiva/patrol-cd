package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
)

var cmGR = schema.GroupResource{Resource: "configmaps"}

// scriptedLW serves scripted list and watch responses; once a script is exhausted it serves
// empty lists and watches that never emit.
type scriptedLW struct {
	mu      sync.Mutex
	lists   []func(metav1.ListOptions) (*unstructured.UnstructuredList, error)
	watches []func(metav1.ListOptions) (watch.Interface, error)
	listOps []metav1.ListOptions
	watchOp []metav1.ListOptions
}

func (s *scriptedLW) List(_ context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	s.mu.Lock()
	s.listOps = append(s.listOps, opts)
	var fn func(metav1.ListOptions) (*unstructured.UnstructuredList, error)
	if len(s.lists) > 0 {
		fn, s.lists = s.lists[0], s.lists[1:]
	}
	s.mu.Unlock()
	if fn == nil {
		l := &unstructured.UnstructuredList{}
		l.SetResourceVersion("100")
		return l, nil
	}
	return fn(opts)
}

func (s *scriptedLW) Watch(_ context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	s.mu.Lock()
	s.watchOp = append(s.watchOp, opts)
	var fn func(metav1.ListOptions) (watch.Interface, error)
	if len(s.watches) > 0 {
		fn, s.watches = s.watches[0], s.watches[1:]
	}
	s.mu.Unlock()
	if fn == nil {
		return watch.NewFake(), nil
	}
	return fn(opts)
}

func (s *scriptedLW) watchRVs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.watchOp))
	for _, o := range s.watchOp {
		out = append(out, o.ResourceVersion)
	}
	return out
}

func (s *scriptedLW) listCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.listOps)
}

func listOf(rv string, items ...*unstructured.Unstructured) func(metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	return func(metav1.ListOptions) (*unstructured.UnstructuredList, error) {
		l := &unstructured.UnstructuredList{}
		l.SetResourceVersion(rv)
		for _, it := range items {
			l.Items = append(l.Items, *it.DeepCopy())
		}
		return l, nil
	}
}

func watchErr(err error) func(metav1.ListOptions) (watch.Interface, error) {
	return func(metav1.ListOptions) (watch.Interface, error) { return nil, err }
}

// streamOf returns a watch that emits events and then, if closeAfter, ends the stream.
func streamOf(closeAfter bool, events ...watch.Event) func(metav1.ListOptions) (watch.Interface, error) {
	return func(metav1.ListOptions) (watch.Interface, error) {
		fw := watch.NewFakeWithChanSize(len(events)+1, false)
		for _, e := range events {
			fw.Action(e.Type, e.Object)
		}
		if closeAfter {
			fw.Stop()
		}
		return fw, nil
	}
}

type recordedEvent struct {
	typ watch.EventType
	key ResourceKey
	rv  string
	nil bool
}

// recState is what a recSink observed.
type recState struct {
	events     []recordedEvent
	raws       int
	replaces   [][]*Resource
	relists    []string
	goneCount  int
	denied     []string
	availCount int
}

type recSink struct {
	mu sync.Mutex
	st recState
}

func (s *recSink) raw(watch.EventType, *unstructured.Unstructured) {
	s.mu.Lock()
	s.st.raws++
	s.mu.Unlock()
}

func (s *recSink) event(_ context.Context, _ watchTarget, t watch.EventType, k ResourceKey, r *Resource, _ *unstructured.Unstructured) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := recordedEvent{typ: t, key: k, nil: r == nil}
	if r != nil {
		e.rv = r.ResourceVersion
	}
	s.st.events = append(s.st.events, e)
	return nil
}

func (s *recSink) replace(_ context.Context, _ watchTarget, items []*Resource) error {
	s.mu.Lock()
	s.st.replaces = append(s.st.replaces, items)
	s.mu.Unlock()
	return nil
}

func (s *recSink) relisted(_ watchTarget, reason string) {
	s.mu.Lock()
	s.st.relists = append(s.st.relists, reason)
	s.mu.Unlock()
}

func (s *recSink) gone(watchTarget) {
	s.mu.Lock()
	s.st.goneCount++
	s.mu.Unlock()
}

func (s *recSink) accessDenied(_ watchTarget, verb string, _ error) {
	s.mu.Lock()
	s.st.denied = append(s.st.denied, verb)
	s.mu.Unlock()
}

func (s *recSink) available(watchTarget) {
	s.mu.Lock()
	s.st.availCount++
	s.mu.Unlock()
}

func (s *recSink) snapshot() recState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.events = append([]recordedEvent(nil), s.st.events...)
	st.replaces = append([][]*Resource(nil), s.st.replaces...)
	st.relists = append([]string(nil), s.st.relists...)
	st.denied = append([]string(nil), s.st.denied...)
	return st
}

func newTestWatcher(lw ListerWatcher, sink watchSink) *watcher {
	s := Settings{}.normalized()
	return &watcher{
		target:           watchTarget{api: APIResourceInfo{GroupKind: schema.GroupKind{Kind: "ConfigMap"}}},
		lw:               lw,
		lister:           &lister{pageSize: 2, attempts: 2, backoff: wait.Backoff{Duration: time.Millisecond, Steps: 100}},
		convert:          func(un *unstructured.Unstructured) *Resource { return newResource(un, &s) },
		sink:             sink,
		log:              slog.New(slog.DiscardHandler),
		resync:           -1,
		deniedRetry:      10 * time.Millisecond,
		backoff:          wait.Backoff{Duration: time.Millisecond, Factor: 1, Steps: 1000},
		minWatchDuration: 0,
	}
}

// runWatcherFor runs w until cond holds, then stops it and returns the sink snapshot.
func runWatcherFor(t *testing.T, w *watcher, rv string, sink *recSink, cond func(recState) bool) recState {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.run(ctx, rv)
	}()
	eventually(t, 5*time.Second, func() bool { return cond(sink.snapshot()) }, "watcher condition")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not stop")
	}
	return sink.snapshot()
}

func cm(name, rv string) *unstructured.Unstructured {
	return mkObj("v1", "ConfigMap", "ns", name, "uid-"+name, withRV(rv))
}

func expiredStatus(asUnstructured bool) runtime.Object {
	st := &metav1.Status{Status: metav1.StatusFailure, Code: 410, Reason: metav1.StatusReasonExpired, Message: "too old resource version"}
	if !asUnstructured {
		return st
	}
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(st)
	if err != nil {
		panic(err)
	}
	obj := &unstructured.Unstructured{Object: u}
	obj.SetAPIVersion("v1")
	obj.SetKind("Status")
	return obj
}

func TestWatcherRelistOnExpiredWatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		first func(metav1.ListOptions) (watch.Interface, error)
	}{
		{"watch request returns 410", watchErr(apierrors.NewResourceExpired("too old"))},
		{"watch request returns Gone", watchErr(apierrors.NewGone("gone"))},
		{"error event with typed status", streamOf(false, watch.Event{Type: watch.Error, Object: expiredStatus(false)})},
		{"error event with unstructured status", streamOf(false, watch.Event{Type: watch.Error, Object: expiredStatus(true)})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lw := &scriptedLW{
				lists: []func(metav1.ListOptions) (*unstructured.UnstructuredList, error){listOf("20", cm("a", "18"), cm("b", "19"))},
				watches: []func(metav1.ListOptions) (watch.Interface, error){
					tc.first,
					streamOf(false, watch.Event{Type: watch.Added, Object: cm("c", "21")}),
				},
			}
			sink := &recSink{}
			got := runWatcherFor(t, newTestWatcher(lw, sink), "10", sink, func(s recState) bool { return len(s.events) == 1 })
			assert.Equal(t, []string{relistExpired}, got.relists)
			require.Len(t, got.replaces, 1)
			assert.Len(t, got.replaces[0], 2)
			assert.Equal(t, []string{"10", "20"}, lw.watchRVs()[:2], "watch resumes from the relist resourceVersion")
			assert.Equal(t, recordedEvent{typ: watch.Added, key: NewResourceKey("", "ConfigMap", "ns", "c"), rv: "21"}, got.events[0])
		})
	}
}

func TestWatcherBookmarkAndReconnect(t *testing.T) {
	t.Parallel()
	bookmark := mkObj("v1", "ConfigMap", "", "", "", withRV("30"))
	lw := &scriptedLW{watches: []func(metav1.ListOptions) (watch.Interface, error){
		streamOf(true,
			watch.Event{Type: watch.Added, Object: cm("a", "11")},
			watch.Event{Type: watch.Modified, Object: cm("a", "12")},
			watch.Event{Type: watch.Bookmark, Object: bookmark},
		),
		streamOf(true, watch.Event{Type: watch.Deleted, Object: cm("a", "31")}),
	}}
	sink := &recSink{}
	got := runWatcherFor(t, newTestWatcher(lw, sink), "10", sink, func(s recState) bool { return len(s.events) == 3 })
	assert.Empty(t, got.relists, "a closed stream resumes without relisting")
	assert.Equal(t, []string{"10", "30", "31"}, lw.watchRVs()[:3])
	assert.Equal(t, 3, got.raws)
	assert.Equal(t, []recordedEvent{
		{typ: watch.Added, key: NewResourceKey("", "ConfigMap", "ns", "a"), rv: "11"},
		{typ: watch.Modified, key: NewResourceKey("", "ConfigMap", "ns", "a"), rv: "12"},
		{typ: watch.Deleted, key: NewResourceKey("", "ConfigMap", "ns", "a"), nil: true},
	}, got.events)
	assert.Zero(t, lw.listCount())
}

func TestWatcherAccessDenied(t *testing.T) {
	t.Parallel()
	forbidden := apierrors.NewForbidden(cmGR, "", errors.New("no watch for you"))
	lw := &scriptedLW{watches: []func(metav1.ListOptions) (watch.Interface, error){watchErr(forbidden)}}
	sink := &recSink{}
	got := runWatcherFor(t, newTestWatcher(lw, sink), "10", sink, func(s recState) bool { return len(s.replaces) == 1 && s.availCount == 1 })
	assert.Equal(t, []string{"watch"}, got.denied)
	assert.Equal(t, []string{relistForbidden}, got.relists, "denied watches fall back to periodic relists")
}

func TestWatcherListDenied(t *testing.T) {
	t.Parallel()
	forbidden := apierrors.NewForbidden(cmGR, "", errors.New("no list"))
	lw := &scriptedLW{lists: []func(metav1.ListOptions) (*unstructured.UnstructuredList, error){
		func(metav1.ListOptions) (*unstructured.UnstructuredList, error) { return nil, forbidden },
	}}
	sink := &recSink{}
	got := runWatcherFor(t, newTestWatcher(lw, sink), "", sink, func(s recState) bool { return len(s.replaces) == 1 })
	assert.Equal(t, []string{"list"}, got.denied)
	assert.Equal(t, []string{relistInitial, relistForbidden}, got.relists)
}

func TestWatcherGone(t *testing.T) {
	t.Parallel()
	lw := &scriptedLW{watches: []func(metav1.ListOptions) (watch.Interface, error){watchErr(apierrors.NewNotFound(cmGR, ""))}}
	sink := &recSink{}
	w := newTestWatcher(lw, sink)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.run(context.Background(), "10")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher must exit when the API is gone")
	}
	assert.Equal(t, 1, sink.snapshot().goneCount)
}

func TestWatcherTransientErrorKeepsResourceVersion(t *testing.T) {
	t.Parallel()
	lw := &scriptedLW{watches: []func(metav1.ListOptions) (watch.Interface, error){
		watchErr(errors.New("connection reset")),
		streamOf(false, watch.Event{Type: watch.Error, Object: &metav1.Status{Code: 500, Reason: metav1.StatusReasonInternalError}}),
		streamOf(false, watch.Event{Type: watch.Added, Object: cm("a", "11")}),
	}}
	sink := &recSink{}
	got := runWatcherFor(t, newTestWatcher(lw, sink), "10", sink, func(s recState) bool { return len(s.events) == 1 })
	assert.Empty(t, got.relists)
	assert.Equal(t, []string{"10", "10", "10"}, lw.watchRVs()[:3])
}

func TestWatcherPeriodicResync(t *testing.T) {
	t.Parallel()
	lw := &scriptedLW{}
	sink := &recSink{}
	w := newTestWatcher(lw, sink)
	w.resync = 30 * time.Millisecond
	got := runWatcherFor(t, w, "10", sink, func(s recState) bool { return len(s.replaces) >= 2 })
	for _, r := range got.relists {
		assert.Equal(t, relistResync, r)
	}
}

func TestListerPaginationAndRetry(t *testing.T) {
	t.Parallel()
	page := func(rv, cont string, items ...*unstructured.Unstructured) func(metav1.ListOptions) (*unstructured.UnstructuredList, error) {
		return func(opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
			l, _ := listOf(rv, items...)(opts)
			l.SetContinue(cont)
			return l, nil
		}
	}
	fail := func(err error) func(metav1.ListOptions) (*unstructured.UnstructuredList, error) {
		return func(metav1.ListOptions) (*unstructured.UnstructuredList, error) { return nil, err }
	}
	tests := []struct {
		name      string
		script    []func(metav1.ListOptions) (*unstructured.UnstructuredList, error)
		attempts  int
		wantNames []string
		wantRV    string
		wantErr   func(error) bool
		wantCalls int
	}{
		{
			name:      "two pages",
			script:    []func(metav1.ListOptions) (*unstructured.UnstructuredList, error){page("7", "next", cm("a", "1"), cm("b", "2")), page("7", "", cm("c", "3"))},
			attempts:  1,
			wantNames: []string{"a", "b", "c"},
			wantRV:    "7",
			wantCalls: 2,
		},
		{
			name:      "retryable error is retried",
			script:    []func(metav1.ListOptions) (*unstructured.UnstructuredList, error){fail(apierrors.NewTooManyRequests("slow down", 0)), page("8", "", cm("a", "1"))},
			attempts:  3,
			wantNames: []string{"a"},
			wantRV:    "8",
			wantCalls: 2,
		},
		{
			name:      "retries are bounded",
			script:    []func(metav1.ListOptions) (*unstructured.UnstructuredList, error){fail(apierrors.NewServiceUnavailable("x")), fail(apierrors.NewServiceUnavailable("x"))},
			attempts:  2,
			wantErr:   apierrors.IsServiceUnavailable,
			wantCalls: 2,
		},
		{
			name:      "forbidden is not retried",
			script:    []func(metav1.ListOptions) (*unstructured.UnstructuredList, error){fail(apierrors.NewForbidden(cmGR, "", errors.New("no")))},
			attempts:  5,
			wantErr:   apierrors.IsForbidden,
			wantCalls: 1,
		},
		{
			name: "expired continue token restarts the list",
			script: []func(metav1.ListOptions) (*unstructured.UnstructuredList, error){
				page("7", "next", cm("a", "1")), fail(apierrors.NewResourceExpired("continue expired")),
				page("9", "", cm("a", "1"), cm("b", "2")),
			},
			attempts:  1,
			wantNames: []string{"a", "b"},
			wantRV:    "9",
			wantCalls: 3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lw := &scriptedLW{lists: tc.script}
			l := &lister{pageSize: 2, attempts: tc.attempts, backoff: wait.Backoff{Duration: time.Millisecond, Steps: 10}}
			s := Settings{}
			items, rv, err := l.list(context.Background(), lw, func(un *unstructured.Unstructured) *Resource { return newResource(un, &s) })
			assert.Equal(t, tc.wantCalls, lw.listCount())
			if tc.wantErr != nil {
				require.Error(t, err)
				assert.True(t, tc.wantErr(err), err.Error())
				return
			}
			require.NoError(t, err)
			var names []string
			for _, r := range items {
				names = append(names, r.Ref.Name)
			}
			assert.Equal(t, tc.wantNames, names)
			assert.Equal(t, tc.wantRV, rv)
			for _, op := range lw.listOps {
				assert.Equal(t, int64(2), op.Limit, fmt.Sprintf("page size: %+v", op))
			}
		})
	}
}
