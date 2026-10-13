package scm

import (
	"context"
	"fmt"
	"iter"
)

// PageFunc fetches the page identified by cursor and returns its items and the cursor of
// the next page. The zero cursor value requests the first page; a zero next cursor ends
// pagination.
type PageFunc[T any, C comparable] func(ctx context.Context, cursor C) (items []T, next C, err error)

// Pages iterates pages produced by fetch. It stops with an error wrapping ErrPageLimit
// (KindInvalid) when more than maxPages pages would be fetched, and with
// ErrPaginationLoop (KindUnavailable) when a cursor repeats. maxPages <= 0 selects
// DefaultMaxPages.
func Pages[T any, C comparable](ctx context.Context, maxPages int, fetch PageFunc[T, C]) iter.Seq2[[]T, error] {
	if maxPages <= 0 {
		maxPages = DefaultMaxPages
	}
	return func(yield func([]T, error) bool) {
		var cursor, zero C
		seen := make(map[C]struct{})
		for page := 1; ; page++ {
			if page > maxPages {
				yield(nil, &Error{Kind: KindInvalid, Op: "Paginate", Err: fmt.Errorf("%w: more than %d pages", ErrPageLimit, maxPages)})
				return
			}
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			items, next, err := fetch(ctx, cursor)
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(items, nil) {
				return
			}
			if next == zero {
				return
			}
			if _, dup := seen[next]; dup || next == cursor {
				yield(nil, &Error{Kind: KindUnavailable, Op: "Paginate", Err: fmt.Errorf("%w: %v", ErrPaginationLoop, next)})
				return
			}
			seen[next] = struct{}{}
			cursor = next
		}
	}
}

// CollectPages fetches every page and concatenates the items. On error no partial result
// is returned, so callers never act on an incomplete listing.
func CollectPages[T any, C comparable](ctx context.Context, maxPages int, fetch PageFunc[T, C]) ([]T, error) {
	var out []T
	for items, err := range Pages(ctx, maxPages, fetch) {
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}
