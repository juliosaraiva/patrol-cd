// Package badge implements the status badge of patrol-server (GET /api/badge; parity
// API-043): an SVG image with the health and sync status of an Application, or the aggregate
// of the Applications of projects, embeddable in READMEs without credentials.
//
// The badge is public only while statusbadge.enabled is true in patrol-cm; otherwise every
// request is answered 404 before any Application is read, so a disabled badge reveals
// nothing (upstream answers an "Unknown" badge instead; see docs/architecture/components/
// api-appsets-misc.md). When enabled, a missing Application, or one in a namespace that is
// not enabled for Applications, shows "Not Found". Names are validated (400) before any
// lookup, every text is XML-escaped, and responses are never cached.
package badge

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Request limits.
const (
	maxQueryBytes   = 4 << 10
	maxProjects     = 50
	DefaultPageSize = 500
	DefaultMaxApps  = 10000
	cacheControl    = "private, no-store"
	allowAnyOrigin  = "*"
	badgeRouteTag   = "badge"
	badgeOperation  = "getBadge"
	trueQueryString = "true"
)

// SettingsSource returns the current settings snapshot; *settings.Manager implements it.
type SettingsSource interface {
	Current() *settings.Settings
}

// Config is the static configuration of the badge.
type Config struct {
	// Namespace is the control-plane namespace (default Application namespace).
	Namespace string
	// ApplicationNamespaces are the other namespaces Applications may live in
	// (application.namespaces: names, globs or "/regexp/").
	ApplicationNamespaces []string
	// MaxApplications bounds the Applications aggregated for project badges
	// (default DefaultMaxApps); beyond it the badge shows Unknown.
	MaxApplications int
	// PageSize is the number of Applications read per List call for project badges
	// (default DefaultPageSize).
	PageSize int
}

// Deps are the collaborators of the badge.
type Deps struct {
	Config   Config
	Settings SettingsSource
	// Reader reads Applications. It may be a controller-runtime cache, which cannot paginate:
	// project badges then read at most MaxApplications+1 Applications in one List call.
	Reader client.Reader
	Logger *slog.Logger
}

// Service serves the badge route; it implements apiserver.Service.
type Service struct {
	d Deps
}

// New returns the service.
func New(d Deps) *Service {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Config.MaxApplications <= 0 {
		d.Config.MaxApplications = DefaultMaxApps
	}
	if d.Config.PageSize <= 0 {
		d.Config.PageSize = DefaultPageSize
	}
	return &Service{d: d}
}

// Register adds the badge route to t.
func (s *Service) Register(t *rest.Table) {
	t.Add(rest.Route{
		Method: http.MethodGet, Path: apiclient.PathBadge, OperationID: badgeOperation,
		Summary: "Status badge (SVG)", Tags: []string{badgeRouteTag},
		Access: rest.Access{Mode: rest.AccessPublic, Note: "public while statusbadge.enabled is true; 404 otherwise"},
		Description: "Health and sync status of an Application (`name`, `appNamespace`), or the aggregate of every Application of the `project`s, as an SVG image. " +
			"`revision=true` adds the last synced revision (7 characters unless `keepFullRevision=true`), `showAppName=true` a title row, `width` overrides the width.",
		Request:             reflect.TypeFor[apiclient.BadgeRequest](),
		ResponseContentType: apiclient.BadgeContentType,
		Audit:               rest.AuditNever,
		Handler:             http.HandlerFunc(s.serve),
		Responses: []rest.ResponseDoc{
			{Status: http.StatusOK, Description: "The SVG badge.", ContentType: apiclient.BadgeContentType},
			{Status: http.StatusNotFound, Description: "Status badges are disabled (statusbadge.enabled is not true)."},
		},
	})
}

func (s *Service) namespaceEnabled(ns string) bool {
	if ns == s.d.Config.Namespace {
		return true
	}
	return project.IsAppNamespacePermitted(&v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{SourceNamespaces: s.d.Config.ApplicationNamespaces}}, ns, s.d.Config.Namespace)
}

func isTrue(q url.Values, key string) bool {
	v, ok := q[key]
	return ok && len(v) > 0 && strings.EqualFold(v[0], trueQueryString)
}

// serve answers GET /api/badge.
func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	if s.d.Settings == nil {
		rest.WriteError(w, r, rest.Unavailable("settings are not available").WithCause(errNotConfigured))
		return
	}
	st := s.d.Settings.Current()
	if st == nil {
		rest.WriteError(w, r, rest.Unavailable("settings are not loaded yet"))
		return
	}
	if !st.StatusBadge().Enabled {
		rest.WriteError(w, r, rest.NotFound("status badges are disabled"))
		return
	}
	if len(r.URL.RawQuery) > maxQueryBytes {
		rest.WriteError(w, r, rest.InvalidArgument("query string too long"))
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		rest.WriteError(w, r, rest.InvalidArgument("invalid query string"))
		return
	}
	v, err := s.view(r.Context(), q)
	if err != nil {
		rest.WriteError(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", apiclient.BadgeContentType)
	// Never cached, so badges are never stale; fetchable from any origin (README badges,
	// dashboards), as upstream.
	h.Set("Cache-Control", cacheControl)
	h.Set("Access-Control-Allow-Origin", allowAnyOrigin)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(render(v)))
}

// view computes the badge content for the query. Every parameter is validated before any
// Application is read.
func (s *Service) view(ctx context.Context, q url.Values) (view, error) {
	v := view{health: v1alpha1.HealthStatusUnknown, sync: v1alpha1.SyncStatusCodeUnknown}
	nsParam := first(q, "appNamespace")
	if nsParam == "" {
		nsParam = first(q, "namespace") // upstream spelling
	}
	if nsParam != "" && len(validation.IsDNS1123Label(nsParam)) > 0 {
		return v, rest.InvalidArgument("invalid namespace")
	}
	names, hasName := q["name"]
	name := ""
	if hasName && len(names) > 0 {
		name = names[0]
		if len(validation.IsDNS1123Subdomain(name)) > 0 {
			return v, rest.InvalidArgument("invalid application name")
		}
	}
	projects, hasProjects := q["project"]
	if len(projects) > maxProjects {
		return v, rest.InvalidArgument("at most %d projects may be given", maxProjects)
	}
	for _, p := range projects {
		if p != "" && len(validation.IsDNS1123Label(strings.ToLower(p))) > 0 {
			return v, rest.InvalidArgument("invalid project name")
		}
	}

	ns := s.d.Config.Namespace
	if nsParam != "" {
		if s.namespaceEnabled(nsParam) {
			ns = nsParam
		} else {
			v.notFound = true
		}
	}
	appName, revision := "", ""
	if hasName && !v.notFound {
		if s.d.Reader == nil {
			return v, rest.Unavailable("applications are not available")
		}
		app := &v1alpha1.Application{}
		err := s.d.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, app)
		switch {
		case err == nil:
			v.health, v.sync = app.Status.Health.Status, app.Status.Sync.Status
			appName = name
			revision = syncedRevision(app)
		case apierrors.IsNotFound(err):
			v.notFound = true
		default:
			s.d.Logger.LogAttrs(ctx, slog.LevelWarn, "status badge: application lookup failed",
				slog.String("requestId", rest.RequestID(ctx)), slog.Any("error", err))
		}
	}
	if hasProjects && !v.notFound {
		if err := s.aggregate(ctx, ns, projects, &v); err != nil {
			s.d.Logger.LogAttrs(ctx, slog.LevelWarn, "status badge: application list failed",
				slog.String("requestId", rest.RequestID(ctx)), slog.Any("error", err))
		}
	}
	if isTrue(q, "revision") && revision != "" {
		v.revision = revision
		if !isTrue(q, "keepFullRevision") && len(revision) > shortRevisionLen {
			v.revision = revision[:shortRevisionLen]
		} else {
			v.fullRev = true
		}
	}
	if w := first(q, "width"); w != "" {
		if n, err := strconv.Atoi(w); err == nil && n > 0 && n <= maxWidth {
			v.width = n
		}
	}
	if isTrue(q, "showAppName") && appName != "" {
		v.title = appName
	}
	return v, nil
}

func first(q url.Values, key string) string {
	if vs := q[key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// syncedRevision returns the revision of the last sync (first source for multi-source).
func syncedRevision(app *v1alpha1.Application) string {
	op := app.Status.OperationState
	if op == nil || op.SyncResult == nil {
		return ""
	}
	if len(op.SyncResult.Revisions) > 0 {
		return op.SyncResult.Revisions[0]
	}
	return op.SyncResult.Revision
}

// cacheContinue is the continue token of controller-runtime's cache reader, which cannot
// paginate and truncates a List at its Limit.
const cacheContinue = "continue-not-supported"

var (
	errTooMany       = errors.New("too many applications to aggregate")
	errNotConfigured = errors.New("not configured")
)

// aggregate folds the status of every Application of projects in ns into v (upstream):
// any Application not Synced makes the badge OutOfSync, any not Healthy Degraded; with at
// least one Application and neither, Synced and Healthy.
func (s *Service) aggregate(ctx context.Context, ns string, projects []string, v *view) error {
	if s.d.Reader == nil {
		return errors.New("applications are not available")
	}
	want := map[string]bool{}
	for _, p := range projects {
		want[p] = true
	}
	matched, outOfSync, degraded := 0, false, false
	fold := func(items []v1alpha1.Application) {
		for i := range items {
			a := &items[i]
			if !want[a.Spec.GetProject()] {
				continue
			}
			matched++
			if a.Status.Sync.Status != v1alpha1.SyncStatusCodeSynced {
				outOfSync = true
			}
			if a.Status.Health.Status != v1alpha1.HealthStatusHealthy {
				degraded = true
			}
		}
	}
	maxApps, pageSize := s.d.Config.MaxApplications, s.d.Config.PageSize
	// The objects are only read: a cache may hand out its own copies.
	opts := []client.ListOption{client.InNamespace(ns), client.UnsafeDisableDeepCopy}
	cont := ""
	seen := 0
	for {
		var list v1alpha1.ApplicationList
		page := append(slices.Clip(opts), client.Limit(int64(pageSize)))
		if cont != "" {
			page = append(page, client.Continue(cont))
		}
		if err := s.d.Reader.List(ctx, &list, page...); err != nil {
			return err
		}
		if list.Continue == cacheContinue {
			// A controller-runtime cache: it truncates at Limit and cannot continue. A full
			// page is read again in one call bounded by the cap.
			if len(list.Items) >= pageSize {
				list = v1alpha1.ApplicationList{}
				if err := s.d.Reader.List(ctx, &list, append(slices.Clip(opts), client.Limit(int64(maxApps)+1))...); err != nil {
					return err
				}
			}
			if len(list.Items) > maxApps {
				return errTooMany
			}
			fold(list.Items)
			break
		}
		seen += len(list.Items)
		if seen > maxApps {
			return errTooMany
		}
		fold(list.Items)
		if cont = list.Continue; cont == "" {
			break
		}
	}
	switch {
	case outOfSync:
		v.sync = v1alpha1.SyncStatusCodeOutOfSync
	case matched > 0:
		v.sync = v1alpha1.SyncStatusCodeSynced
	}
	switch {
	case degraded:
		v.health = v1alpha1.HealthStatusDegraded
	case matched > 0:
		v.health = v1alpha1.HealthStatusHealthy
	}
	return nil
}
