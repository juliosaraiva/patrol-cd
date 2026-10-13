package cache

import (
	"context"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
)

// watchTarget is one API resource in one scope ("" = cluster-wide / all namespaces).
type watchTarget struct {
	api       APIResourceInfo
	namespace string
}

// watchSink receives the output of a watcher. Implementations must be safe for concurrent
// use by many watchers.
type watchSink interface {
	// raw is called for every watch event before conversion.
	raw(eventType watch.EventType, un *unstructured.Unstructured)
	// event queues a converted event (res is nil for deletions). An error stops the watcher.
	event(ctx context.Context, t watchTarget, eventType watch.EventType, key ResourceKey, res *Resource, un *unstructured.Unstructured) error
	// replace queues the result of a relist. An error stops the watcher.
	replace(ctx context.Context, t watchTarget, items []*Resource) error
	// relisted reports that a relist starts, with its reason.
	relisted(t watchTarget, reason string)
	// gone reports that the API is no longer served; the watcher exits afterwards.
	gone(t watchTarget)
	// accessDenied reports a 401/403 for verb ("list" or "watch").
	accessDenied(t watchTarget, verb string, err error)
	// available reports that a watch was established.
	available(t watchTarget)
}

// Relist reasons reported to Metrics.Relisted.
const (
	relistInitial   = "initial"
	relistExpired   = "expired"
	relistResync    = "resync"
	relistForbidden = "forbidden-poll"
	relistError     = "error"
)

type watchOutcome int

const (
	outcomeStop      watchOutcome = iota // context done or sink refused
	outcomeReconnect                     // stream ended normally: resume from rv
	outcomeRetry                         // transient failure: back off, resume from rv
	outcomeRelist                        // rv unusable (410) or resync due
	outcomeDenied                        // 401/403
	outcomeGone                          // 404: API removed
)

// watcher keeps one watchTarget in sync: it relists when needed and streams watch events
// resumed from the last observed resourceVersion (including bookmarks).
type watcher struct {
	target  watchTarget
	lw      ListerWatcher
	lister  *lister
	convert func(*unstructured.Unstructured) *Resource
	sink    watchSink
	log     *slog.Logger

	// resync is the maximum time between relists (<= 0 disables periodic relists).
	resync time.Duration
	// deniedRetry is the wait before relisting after a 401/403.
	deniedRetry time.Duration
	// backoff spaces retries after transient failures.
	backoff wait.Backoff
	// minWatchDuration: streams closing sooner without error are treated as failures.
	minWatchDuration time.Duration
}

// run keeps the target synchronized until ctx is done or the API disappears. A non-empty rv
// resumes from an existing list; an empty rv starts with a relist.
func (w *watcher) run(ctx context.Context, rv string) {
	bo := w.backoff
	lastList := time.Now()
	reason := relistInitial
	for ctx.Err() == nil {
		if rv == "" {
			w.sink.relisted(w.target, reason)
			items, newRV, err := w.lister.list(ctx, w.lw, w.convert)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				switch {
				case apierrors.IsNotFound(err):
					w.sink.gone(w.target)
					return
				case isAccessDenied(err):
					w.sink.accessDenied(w.target, "list", err)
					reason = relistForbidden
					if !sleepCtx(ctx, w.deniedRetry) {
						return
					}
				default:
					w.log.Warn("relist failed", "error", err)
					reason = relistError
					if !sleepCtx(ctx, bo.Step()) {
						return
					}
				}
				continue
			}
			if err := w.sink.replace(ctx, w.target, items); err != nil {
				return
			}
			rv, lastList, bo = newRV, time.Now(), w.backoff
		}

		outcome, newRV, why, err := w.watchOnce(ctx, rv, lastList)
		rv = newRV
		switch outcome {
		case outcomeStop:
			return
		case outcomeGone:
			w.sink.gone(w.target)
			return
		case outcomeReconnect:
			bo = w.backoff
		case outcomeRetry:
			w.log.Debug("watch interrupted, retrying", "error", err)
			if !sleepCtx(ctx, bo.Step()) {
				return
			}
		case outcomeRelist:
			rv, reason = "", why
		case outcomeDenied:
			w.sink.accessDenied(w.target, "watch", err)
			if !sleepCtx(ctx, w.deniedRetry) {
				return
			}
			rv, reason = "", relistForbidden
		}
	}
}

// watchOnce runs one watch stream from rv. It returns the outcome, the last observed
// resourceVersion, the relist reason (for outcomeRelist) and the error that ended the stream.
func (w *watcher) watchOnce(ctx context.Context, rv string, lastList time.Time) (watchOutcome, string, string, error) {
	wi, err := w.lw.Watch(ctx, metav1.ListOptions{ResourceVersion: rv, AllowWatchBookmarks: true})
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return outcomeStop, rv, "", nil
		case isExpired(err):
			return outcomeRelist, rv, relistExpired, err
		case apierrors.IsNotFound(err):
			return outcomeGone, rv, "", err
		case isAccessDenied(err):
			return outcomeDenied, rv, "", err
		default:
			return outcomeRetry, rv, "", err
		}
	}
	defer wi.Stop()
	w.sink.available(w.target)
	started := time.Now()

	var resyncC <-chan time.Time
	if w.resync > 0 {
		d := time.Until(lastList.Add(wait.Jitter(w.resync, 0.1)))
		t := time.NewTimer(max(d, 0))
		defer t.Stop()
		resyncC = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return outcomeStop, rv, "", nil
		case <-resyncC:
			return outcomeRelist, rv, relistResync, nil
		case ev, ok := <-wi.ResultChan():
			if !ok {
				if time.Since(started) < w.minWatchDuration {
					return outcomeRetry, rv, "", nil
				}
				return outcomeReconnect, rv, "", nil
			}
			switch ev.Type {
			case watch.Bookmark:
				if m, err := meta.Accessor(ev.Object); err == nil && m.GetResourceVersion() != "" {
					rv = m.GetResourceVersion()
				}
			case watch.Error:
				err := apierrors.FromObject(ev.Object)
				switch {
				case isExpired(err):
					return outcomeRelist, rv, relistExpired, err
				case isAccessDenied(err):
					return outcomeDenied, rv, "", err
				default:
					return outcomeRetry, rv, "", err
				}
			case watch.Added, watch.Modified, watch.Deleted:
				un, ok := ev.Object.(*unstructured.Unstructured)
				if !ok {
					return outcomeRelist, rv, relistError, nil
				}
				newRV := un.GetResourceVersion()
				w.sink.raw(ev.Type, un)
				key := KeyOf(un)
				var res *Resource
				if ev.Type != watch.Deleted {
					res = w.convert(un)
				}
				if err := w.sink.event(ctx, w.target, ev.Type, key, res, un); err != nil {
					return outcomeStop, rv, "", nil
				}
				if newRV != "" {
					rv = newRV
				}
			}
		}
	}
}
