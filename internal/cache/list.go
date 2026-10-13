package cache

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/semaphore"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
)

// ListerWatcher lists and watches one API resource in one scope (a namespace, or the whole
// cluster). dynamic.ResourceInterface satisfies it.
type ListerWatcher interface {
	List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error)
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
}

// maxListRestarts bounds restarts of a paginated list whose continue token expired.
const maxListRestarts = 3

// lister performs paginated lists with per-page retries.
type lister struct {
	pageSize int64
	attempts int
	backoff  wait.Backoff
	sem      *semaphore.Weighted
}

func newLister(s *Settings, sem *semaphore.Weighted) *lister {
	return &lister{
		pageSize: s.ListPageSize,
		attempts: s.ListRetryAttempts,
		backoff:  wait.Backoff{Duration: 200 * time.Millisecond, Factor: 2, Jitter: 0.5, Steps: 1 << 30, Cap: 5 * time.Second},
		sem:      sem,
	}
}

// list returns every object of the scope converted with convert, and the list's
// resourceVersion. Pages are converted as they arrive, so only one raw page is held in
// memory at a time.
func (l *lister) list(ctx context.Context, lw ListerWatcher, convert func(*unstructured.Unstructured) *Resource) ([]*Resource, string, error) {
	if l.sem != nil {
		if err := l.sem.Acquire(ctx, 1); err != nil {
			return nil, "", fmt.Errorf("acquire list semaphore: %w", err)
		}
		defer l.sem.Release(1)
	}
	for restart := 0; ; restart++ {
		items, rv, err := l.listOnce(ctx, lw, convert)
		if err != nil && isExpired(err) && restart < maxListRestarts && ctx.Err() == nil {
			continue // continue token expired between pages: start over
		}
		return items, rv, err
	}
}

func (l *lister) listOnce(ctx context.Context, lw ListerWatcher, convert func(*unstructured.Unstructured) *Resource) ([]*Resource, string, error) {
	opts := metav1.ListOptions{Limit: l.pageSize}
	var out []*Resource
	for {
		page, err := l.page(ctx, lw, opts)
		if err != nil {
			return nil, "", err
		}
		for i := range page.Items {
			obj := page.Items[i] // copy so the page's backing array is not retained
			out = append(out, convert(&obj))
		}
		cont := page.GetContinue()
		if cont == "" {
			return out, page.GetResourceVersion(), nil
		}
		opts.Continue = cont
	}
}

func (l *lister) page(ctx context.Context, lw ListerWatcher, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	bo := l.backoff
	for attempt := 1; ; attempt++ {
		list, err := lw.List(ctx, opts)
		if err == nil {
			if list == nil {
				return &unstructured.UnstructuredList{}, nil
			}
			return list, nil
		}
		if attempt >= l.attempts || !isRetryableListError(err) || ctx.Err() != nil {
			return nil, err
		}
		if !sleepCtx(ctx, bo.Step()) {
			return nil, ctx.Err()
		}
	}
}

// sleepCtx waits for d or until ctx is done; it reports whether the full duration elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
