package scm_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// pager serves items in pages of the given size with integer cursors (0 = first page).
func pager(items []int, size int, calls *int) scm.PageFunc[int, int] {
	return func(_ context.Context, cursor int) ([]int, int, error) {
		*calls++
		start := cursor * size
		if start >= len(items) {
			return nil, 0, nil
		}
		end := min(start+size, len(items))
		next := 0
		if end < len(items) {
			next = cursor + 1
		}
		return items[start:end], next, nil
	}
}

func TestCollectPagesProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		items := rapid.SliceOf(rapid.Int()).Draw(t, "items")
		size := rapid.IntRange(1, 10).Draw(t, "size")
		maxPages := rapid.IntRange(1, 30).Draw(t, "maxPages")
		pages := max(1, (len(items)+size-1)/size)
		calls := 0
		got, err := scm.CollectPages(context.Background(), maxPages, pager(items, size, &calls))
		if pages > maxPages {
			if !errors.Is(err, scm.ErrPageLimit) || scm.KindOf(err) != scm.KindInvalid || got != nil {
				t.Fatalf("pages=%d cap=%d: got %v, %v; want page limit error and no partial result", pages, maxPages, got, err)
			}
			if calls != maxPages {
				t.Fatalf("fetched %d pages, want exactly the cap %d", calls, maxPages)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Equal(got, items) && (len(got) != 0 || len(items) != 0) {
			t.Fatalf("got %v, want %v", got, items)
		}
		if calls != pages {
			t.Fatalf("calls = %d, want %d", calls, pages)
		}
	})
}

func TestPagesLoopAndErrors(t *testing.T) {
	t.Parallel()
	loop := func(_ context.Context, cursor string) ([]int, string, error) {
		if cursor == "" {
			return []int{1}, "a", nil
		}
		return []int{2}, "a", nil // repeats cursor "a"
	}
	if _, err := scm.CollectPages(context.Background(), 10, loop); !errors.Is(err, scm.ErrPaginationLoop) {
		t.Fatalf("loop: %v", err)
	}
	boom := errors.New("boom")
	failing := func(_ context.Context, cursor int) ([]int, int, error) {
		if cursor == 1 {
			return nil, 0, boom
		}
		return []int{1}, 1, nil
	}
	if got, err := scm.CollectPages(context.Background(), 10, failing); !errors.Is(err, boom) || got != nil {
		t.Fatalf("error mid-way: %v %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := scm.CollectPages(ctx, 10, failing); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	// Early break from the iterator stops fetching.
	calls := 0
	for range scm.Pages(context.Background(), 100, pager([]int{1, 2, 3, 4, 5}, 1, &calls)) {
		break
	}
	if calls != 1 {
		t.Fatalf("calls after break = %d", calls)
	}
	// maxPages <= 0 selects the default cap.
	calls = 0
	if got, err := scm.CollectPages(context.Background(), 0, pager([]int{1, 2, 3}, 1, &calls)); err != nil || len(got) != 3 {
		t.Fatalf("default cap: %v %v", got, err)
	}
}
