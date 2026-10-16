package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// enqueueRefresh adds a refresh request, after delay when positive. Identical pending
// requests collapse into one.
func (h *Handler) enqueueRefresh(req refreshRequest, delay time.Duration) {
	if delay > 0 {
		h.refreshQ.AddAfter(req, delay)
		return
	}
	h.refreshQ.Add(req)
}

func (h *Handler) refreshWorker(ctx context.Context) {
	for {
		req, shutdown := h.refreshQ.Get()
		if shutdown {
			return
		}
		h.applyRefresh(ctx, req)
		h.refreshQ.Done(req)
	}
}

// applyRefresh writes one refresh annotation, recovering from panics.
func (h *Handler) applyRefresh(ctx context.Context, req refreshRequest) {
	defer func() {
		if rec := recover(); rec != nil {
			h.metrics.refreshes.WithLabelValues(req.kind, refreshError).Inc()
			h.log.Error("webhook: panic while requesting refresh", "kind", req.kind, "namespace", req.namespace, "name", req.name, "panic", fmt.Sprint(rec))
		}
	}()
	var err error
	switch req.kind {
	case kindApplication:
		err = h.refreshApplication(ctx, req)
	case kindApplicationSet:
		err = h.patchAnnotations(ctx, &v1alpha1.ApplicationSet{ObjectMeta: metav1.ObjectMeta{Namespace: req.namespace, Name: req.name}},
			map[string]string{v1alpha1.AnnotationKeyApplicationSetRefresh: "true"})
	default:
		return
	}
	log := h.log.With("kind", req.kind, "namespace", req.namespace, "name", req.name)
	switch {
	case err == nil:
		h.metrics.refreshes.WithLabelValues(req.kind, refreshRequested).Inc()
		if req.hydrate {
			log.Info("webhook: requested refresh and hydration")
		} else {
			log.Info("webhook: requested refresh")
		}
	case apierrors.IsNotFound(err):
		h.metrics.refreshes.WithLabelValues(req.kind, refreshNotFound).Inc()
		log.Info("webhook: object deleted before its refresh was requested")
	default:
		h.metrics.refreshes.WithLabelValues(req.kind, refreshError).Inc()
		log.Warn("webhook: failed to request refresh", "error", err)
	}
}

// refreshApplication requests a normal refresh of an Application (and a normal
// hydration when req.hydrate). A pending "hard" request is never downgraded: the
// Application is read through Config.APIReader and patched with a JSON merge patch that
// carries the resourceVersion read, so the API server rejects it with 409 Conflict when
// the object changed in between (for example a user requesting a hard refresh, or the
// controller consuming the annotation). A conflict re-reads and re-decides, up to
// refreshAttempts times. The patch is sent even when the values are already set: the
// resourceVersion check then confirms that the value read was current (a stale "hard"
// that the controller already consumed would otherwise suppress the refresh).
func (h *Handler) refreshApplication(ctx context.Context, req refreshRequest) error {
	key := client.ObjectKey{Namespace: req.namespace, Name: req.name}
	return h.withRetries(ctx, func() error {
		var cur v1alpha1.Application
		cctx, cancel := context.WithTimeout(ctx, h.cfg.APITimeout)
		err := h.cfg.APIReader.Get(cctx, key, &cur)
		cancel()
		if err != nil {
			return err
		}
		meta := map[string]any{"annotations": refreshAnnotations(cur.Annotations, req.hydrate)}
		if rv := cur.ResourceVersion; rv != "" {
			meta["resourceVersion"] = rv
		}
		patch, err := json.Marshal(map[string]any{"metadata": meta})
		if err != nil {
			return fmt.Errorf("marshal refresh patch: %w", err)
		}
		cctx, cancel = context.WithTimeout(ctx, h.cfg.APITimeout)
		defer cancel()
		return h.cfg.Client.Patch(cctx, &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: req.namespace, Name: req.name}},
			client.RawPatch(types.MergePatchType, patch))
	})
}

// refreshAnnotations returns the annotations requesting a refresh (and a hydration when
// hydrate) given the current ones: "normal", or "hard" when a hard request is pending.
func refreshAnnotations(current map[string]string, hydrate bool) map[string]string {
	ann := map[string]string{v1alpha1.AnnotationKeyRefresh: string(v1alpha1.RefreshTypeNormal)}
	if current[v1alpha1.AnnotationKeyRefresh] == string(v1alpha1.RefreshTypeHard) {
		ann[v1alpha1.AnnotationKeyRefresh] = string(v1alpha1.RefreshTypeHard)
	}
	if hydrate {
		ann[v1alpha1.AnnotationKeyHydrate] = string(v1alpha1.HydrateTypeNormal)
		if current[v1alpha1.AnnotationKeyHydrate] == string(v1alpha1.HydrateTypeHard) {
			ann[v1alpha1.AnnotationKeyHydrate] = string(v1alpha1.HydrateTypeHard)
		}
	}
	return ann
}

// patchAnnotations applies an unconditional JSON merge patch of metadata.annotations
// (used for ApplicationSets, whose refresh annotation has a single value), retrying as
// withRetries.
func (h *Handler) patchAnnotations(ctx context.Context, obj client.Object, ann map[string]string) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": ann}})
	if err != nil {
		return fmt.Errorf("marshal refresh patch: %w", err)
	}
	return h.withRetries(ctx, func() error {
		cctx, cancel := context.WithTimeout(ctx, h.cfg.APITimeout)
		defer cancel()
		return h.cfg.Client.Patch(cctx, obj, client.RawPatch(types.MergePatchType, patch))
	})
}

// withRetries runs op until it succeeds, fails with an error other than a conflict,
// throttling (429) or server timeout, or has run refreshAttempts times,
// refreshRetryInterval apart (upstream: 5 attempts, 100 ms, conflicts only).
func (h *Handler) withRetries(ctx context.Context, op func() error) error {
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil {
			return nil
		}
		retriable := apierrors.IsConflict(err) || apierrors.IsTooManyRequests(err) || apierrors.IsServerTimeout(err)
		if !retriable || attempt >= refreshAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %w)", ctx.Err(), err)
		case <-time.After(refreshRetryInterval):
		}
	}
}
