package applicationsets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Stream limits.
const (
	maxWatchProjects   = 100
	maxWatchQueryBytes = 8 << 10
)

// watchQuery is the parsed query of the stream route.
type watchQuery struct {
	name       string
	namespace  string
	projects   []string
	selector   labels.Selector
	minVersion int64
	// resume is the resourceVersion the watch starts from (resourceVersion or
	// Last-Event-ID); no initial list is made then.
	resume string
}

func parseWatchQuery(r *http.Request) (*watchQuery, error) {
	if len(r.URL.RawQuery) > maxWatchQueryBytes {
		return nil, rest.InvalidArgument("query string too long")
	}
	v, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, rest.InvalidArgument("invalid query string: %v", err)
	}
	q := &watchQuery{name: v.Get("name"), namespace: v.Get("appsetNamespace")}
	if q.namespace == "" {
		q.namespace = v.Get("appSetNamespace")
	}
	q.projects = v["projects"]
	if len(q.projects) > maxWatchProjects {
		return nil, rest.InvalidArgument("at most %d projects may be given", maxWatchProjects)
	}
	if q.selector, err = labels.Parse(v.Get("selector")); err != nil {
		return nil, rest.InvalidArgument("error parsing labels with selectors: %v", err).WithCause(err)
	}
	if rv := v.Get("resourceVersion"); rv != "" {
		// A numeric version both filters older objects (upstream) and starts the watch
		// there, so list-then-watch clients miss no change; others are ignored, as upstream.
		if n, err := strconv.ParseInt(rv, 10, 64); err == nil && n > 0 {
			q.minVersion = n
			q.resume = rv
		}
	}
	if id := strings.TrimSpace(r.Header.Get("Last-Event-ID")); id != "" {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil && n > 0 {
			q.resume = id
		}
	}
	return q, nil
}

// matches applies the stream's filters (RBAC is checked separately, per event).
func (s *Service) matches(q *watchQuery, as *v1alpha1.ApplicationSet) bool {
	if q.name != "" && as.Name != q.name {
		return false
	}
	if q.namespace != "" && as.Namespace != q.namespace {
		return false
	}
	if !projectFilter(q.projects, as) || !q.selector.Matches(labels.Set(as.Labels)) {
		return false
	}
	if q.minVersion > 0 {
		if v, err := strconv.ParseInt(as.ResourceVersion, 10, 64); err == nil && v < q.minVersion {
			return false
		}
	}
	return s.namespaceEnabled(as.Namespace)
}

// watch serves GET /api/v1/stream/applicationsets.
func (s *Service) watch(w http.ResponseWriter, r *http.Request) {
	q, err := parseWatchQuery(r)
	if err != nil {
		rest.WriteError(w, r, err)
		return
	}
	if s.d.Watcher == nil || s.d.APIReader == nil {
		rest.WriteError(w, r, unavailable("event stream"))
		return
	}
	opts := rest.StreamOptions[apiclient.ApplicationSetWatchEvent]{
		// Authorization is evaluated when each event is sent, with the current policy.
		Authorize: func(ctx context.Context, ev apiclient.ApplicationSetWatchEvent) (bool, error) {
			return s.namespaceEnabled(ev.Object.Namespace) && s.can(ctx, rbac.ActionGet, &ev.Object), nil
		},
	}
	rest.ServeSSE(w, r, opts, func(ctx context.Context, sink *rest.Sink[apiclient.ApplicationSetWatchEvent]) error {
		return s.produce(ctx, q, sink)
	})
}

func (s *Service) listOptions(q *watchQuery) []client.ListOption {
	opts := []client.ListOption{client.MatchingLabelsSelector{Selector: q.selector}}
	if q.namespace != "" {
		opts = append(opts, client.InNamespace(q.namespace))
	}
	return opts
}

func (s *Service) send(sink *rest.Sink[apiclient.ApplicationSetWatchEvent], q *watchQuery, typ string, as *v1alpha1.ApplicationSet) error {
	if !s.matches(q, as) {
		return nil
	}
	return sink.SendWithID(as.ResourceVersion, apiclient.ApplicationSetWatchEvent{Type: typ, Object: *withTypeMeta(as.DeepCopy())})
}

// produce lists (sending the current state when a name is given), then watches from the
// list's resourceVersion, re-establishing the watch when the API server closes it. An
// expired resourceVersion ends the stream with 410 (the client lists again).
func (s *Service) produce(ctx context.Context, q *watchQuery, sink *rest.Sink[apiclient.ApplicationSetWatchEvent]) error {
	rv := q.resume
	if rv == "" {
		var list v1alpha1.ApplicationSetList
		if err := s.d.APIReader.List(ctx, &list, s.listOptions(q)...); err != nil {
			return fmt.Errorf("error listing ApplicationSets: %w", err)
		}
		rv = list.ResourceVersion
		if q.name != "" {
			items := list.Items
			slices.SortFunc(items, func(a, b v1alpha1.ApplicationSet) int {
				return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
			})
			for i := range items {
				if err := s.send(sink, q, apiclient.EventAdded, &items[i]); err != nil {
					return err
				}
			}
		}
	}
	for {
		next, err := s.watchOnce(ctx, q, rv, sink)
		if err != nil || ctx.Err() != nil {
			return err
		}
		rv = next
	}
}

// watchOnce consumes one API server watch and returns the last resourceVersion seen.
func (s *Service) watchOnce(ctx context.Context, q *watchQuery, rv string, sink *rest.Sink[apiclient.ApplicationSetWatchEvent]) (string, error) {
	opts := append(s.listOptions(q), &client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: rv, AllowWatchBookmarks: true}})
	w, err := s.d.Watcher.Watch(ctx, &v1alpha1.ApplicationSetList{}, opts...)
	if err != nil {
		if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
			return rv, expired()
		}
		return rv, fmt.Errorf("error watching ApplicationSets: %w", err)
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return rv, nil
		case ev, ok := <-w.ResultChan():
			if !ok {
				return rv, nil
			}
			switch ev.Type {
			case watch.Added, watch.Modified, watch.Deleted:
				as, isAS := ev.Object.(*v1alpha1.ApplicationSet)
				if !isAS {
					continue
				}
				rv = as.ResourceVersion
				if err := s.send(sink, q, string(ev.Type), as); err != nil {
					return rv, err
				}
			case watch.Bookmark:
				if as, isAS := ev.Object.(*v1alpha1.ApplicationSet); isAS && as.ResourceVersion != "" {
					rv = as.ResourceVersion
				}
			case watch.Error:
				err := apierrors.FromObject(ev.Object)
				if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
					return rv, expired()
				}
				var st apierrors.APIStatus
				if errors.As(err, &st) {
					return rv, fmt.Errorf("ApplicationSet watch failed: %w", err)
				}
				return rv, errors.New("ApplicationSet watch failed")
			}
		}
	}
}

func expired() error {
	return &rest.Error{Status: http.StatusGone, Code: apiclient.CodeFailedPrecondition,
		Detail: "the resource version of the watch expired; list the ApplicationSets again and reconnect"}
}
