package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// maxStreamFailures is the number of consecutive watch stream failures after which the
// watcher switches to polling for the rest of the wait.
const maxStreamFailures = 3

// appWatcher follows one Application: through the server's watch stream (Server-Sent
// Events), re-opened when it breaks, and through polling when the stream is not available
// (route missing, proxies that buffer event streams, repeated failures).
type appWatcher struct {
	e         *env
	cl        Client
	name      string
	namespace string
	// checkGone reads the Application once after each stream opened: a stream reports
	// nothing for an Application deleted before it opened, so a deletion wait would hang.
	checkGone bool
}

// appEventFunc handles one observed state. eventType is ADDED, MODIFIED or DELETED (for
// DELETED, app is the last known state). It returns done=true to stop watching.
type appEventFunc func(eventType string, app *v1alpha1.Application) (done bool, err error)

// run delivers states to fn until it is done, fn fails or ctx ends.
func (w *appWatcher) run(ctx context.Context, fn appEventFunc) error {
	failures := 0
	for failures < maxStreamFailures {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := w.stream(ctx, fn)
		if done {
			return err
		}
		if err != nil {
			if fatalStreamError(err) {
				return err
			}
			if streamUnsupported(err) {
				break
			}
		}
		failures++
		if err := sleepCtx(ctx, w.e.watchRetryDelay); err != nil {
			return err
		}
	}
	return w.poll(ctx, fn)
}

// stream reads one watch stream. done reports that fn finished (with err as its result);
// otherwise err describes why the stream ended (nil: closed by the server).
func (w *appWatcher) stream(ctx context.Context, fn appEventFunc) (done bool, err error) {
	s, err := w.cl.WatchApplications(ctx, &apiclient.ApplicationQuery{Name: w.name, AppNamespace: w.namespace}, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = s.Close() }()
	if w.checkGone {
		if _, err := getApp(ctx, w.cl, w.name, w.namespace, ""); err != nil && isNotFoundOrDenied(err) {
			gone := &v1alpha1.Application{}
			gone.Name, gone.Namespace = w.name, w.namespace
			if finished, ferr := fn(apiclient.EventDeleted, gone); finished || ferr != nil {
				return true, ferr
			}
		}
	}
	var last *v1alpha1.Application
	for {
		ev, err := s.Next()
		if err != nil {
			if isEOF(err) {
				return false, nil
			}
			return false, err
		}
		if ev.Object.Name != w.name || (w.namespace != "" && ev.Object.Namespace != w.namespace) {
			continue
		}
		obj := ev.Object
		if ev.Type == apiclient.EventDeleted && last != nil && obj.Name == "" {
			obj = *last
		}
		last = &obj
		finished, ferr := fn(ev.Type, &obj)
		if finished || ferr != nil {
			return true, ferr
		}
	}
}

// poll reads the Application periodically; a missing (or no longer visible) Application
// is reported as DELETED.
func (w *appWatcher) poll(ctx context.Context, fn appEventFunc) error {
	var last *v1alpha1.Application
	for {
		app, err := getApp(ctx, w.cl, w.name, w.namespace, "")
		switch {
		case err == nil:
			last = app
			if done, ferr := fn(apiclient.EventModified, app); done || ferr != nil {
				return ferr
			}
		case isNotFoundOrDenied(err):
			gone := last
			if gone == nil {
				gone = &v1alpha1.Application{}
				gone.Name, gone.Namespace = w.name, w.namespace
			}
			if done, ferr := fn(apiclient.EventDeleted, gone); done || ferr != nil {
				return ferr
			}
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			if apiclient.IsUnauthenticated(err) {
				return err
			}
		}
		if err := sleepCtx(ctx, w.e.pollInterval); err != nil {
			return err
		}
	}
}

// fatalStreamError reports errors that re-opening the stream cannot fix.
func fatalStreamError(err error) bool {
	return apiclient.IsUnauthenticated(err) || apiclient.IsPermissionDenied(err) || apiclient.IsInvalidArgument(err) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// streamUnsupported reports a server or proxy without the watch route.
func streamUnsupported(err error) bool {
	var ae *apiclient.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented, http.StatusNotAcceptable:
		return true
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// waitForAppDeletion blocks until the Application is gone.
func waitForAppDeletion(ctx context.Context, e *env, cl Client, name, namespace string) error {
	w := &appWatcher{e: e, cl: cl, name: name, namespace: namespace, checkGone: true}
	err := w.run(ctx, func(eventType string, _ *v1alpha1.Application) (bool, error) {
		return eventType == apiclient.EventDeleted, nil
	})
	if isNotFoundOrDenied(err) {
		return nil
	}
	return err
}
