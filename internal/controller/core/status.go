package core

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// maxConflictRetries bounds the re-read/re-apply loop of optimistic-concurrency writes.
const maxConflictRetries = 5

// ErrGone is returned by the write helpers when the object no longer exists.
var ErrGone = errors.New("object no longer exists")

// ErrConflict is returned by the strict write helpers when the object changed since it was
// read (HTTP 409). The caller abandons the step and recomputes it from the newer object.
var ErrConflict = errors.New("object was modified concurrently")

// writeKind selects what a write helper patches.
type writeKind int

const (
	writeStatus writeKind = iota
	writeObject
)

// PatchStatus applies mutate to a copy of obj and, when the copy differs, merge-patches the
// status subresource with optimistic concurrency (resourceVersion precondition). On a
// conflict the object is re-read through reader and mutate is applied again, at most
// maxConflictRetries times. obj is updated with the written (or re-read) object. changed
// reports whether a patch was sent. A deleted object yields ErrGone.
//
// Use it only when mutate derives every change from the object it is given; a mutate that
// writes values computed from an earlier read must use PatchStatusStrict.
func PatchStatus[T client.Object](ctx context.Context, c client.Client, reader client.Reader, obj T, mutate func(T) error) (changed bool, err error) {
	return write(ctx, c, reader, obj, mutate, writeStatus, maxConflictRetries)
}

// PatchStatusStrict is PatchStatus without the re-read: a conflict returns an error wrapping
// ErrConflict and obj is left unchanged, so a writer that decided on a stale object never
// overwrites a newer state (sync-state-machine.md: the stale writer's step is abandoned).
func PatchStatusStrict[T client.Object](ctx context.Context, c client.Client, obj T, mutate func(T) error) (changed bool, err error) {
	return write(ctx, c, nil, obj, mutate, writeStatus, 0)
}

// PatchObjectStrict is PatchObject without the re-read (see PatchStatusStrict).
func PatchObjectStrict[T client.Object](ctx context.Context, c client.Client, obj T, mutate func(T) error) (changed bool, err error) {
	return write(ctx, c, nil, obj, mutate, writeObject, 0)
}

// PatchObject is PatchStatus for the main resource (metadata and spec), e.g. finalizers,
// labels and annotations.
func PatchObject[T client.Object](ctx context.Context, c client.Client, reader client.Reader, obj T, mutate func(T) error) (changed bool, err error) {
	return write(ctx, c, reader, obj, mutate, writeObject, maxConflictRetries)
}

func write[T client.Object](ctx context.Context, c client.Client, reader client.Reader, obj T, mutate func(T) error, kind writeKind, retries int) (bool, error) {
	for attempt := 0; ; attempt++ {
		orig, ok := obj.DeepCopyObject().(T)
		if !ok {
			return false, fmt.Errorf("unexpected type %T", obj)
		}
		work, _ := obj.DeepCopyObject().(T)
		if err := mutate(work); err != nil {
			return false, err
		}
		if equality.Semantic.DeepEqual(orig, work) {
			return false, nil
		}
		patch := client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})
		var err error
		if kind == writeStatus {
			err = c.Status().Patch(ctx, work, patch)
		} else {
			err = c.Patch(ctx, work, patch)
		}
		switch {
		case err == nil:
			copyInto(obj, work)
			return true, nil
		case apierrors.IsNotFound(err):
			return false, fmt.Errorf("%s/%s: %w", obj.GetNamespace(), obj.GetName(), ErrGone)
		case apierrors.IsConflict(err) && (reader == nil || attempt >= retries):
			return false, fmt.Errorf("patch %s/%s: %w: %w", obj.GetNamespace(), obj.GetName(), ErrConflict, err)
		case apierrors.IsConflict(err):
			if gerr := reader.Get(ctx, client.ObjectKeyFromObject(obj), obj); gerr != nil {
				if apierrors.IsNotFound(gerr) {
					return false, fmt.Errorf("%s/%s: %w", obj.GetNamespace(), obj.GetName(), ErrGone)
				}
				return false, fmt.Errorf("re-read %s/%s after conflict: %w", obj.GetNamespace(), obj.GetName(), gerr)
			}
		default:
			return false, fmt.Errorf("patch %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
		}
	}
}

// copyInto overwrites dst with src (same concrete type).
func copyInto[T client.Object](dst, src T) {
	type deepCopier[X any] interface{ DeepCopyInto(X) }
	if d, ok := any(src).(deepCopier[T]); ok {
		d.DeepCopyInto(dst)
	}
}
