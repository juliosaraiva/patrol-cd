// Package applicationsets implements the ApplicationSet API of patrol-server
// (api-contract.md, "ApplicationSets"; parity ASET-100, ASET-108..117, ASET-132, API-028):
// list, get, create (upsert, dry-run), delete, generate (dry-run rendering through
// internal/appset), resource tree, events and the Server-Sent Event watch.
//
// Authorization: resource "applicationsets", object "<project>/<name>" in the control-plane
// namespace and "<project>/<namespace>/<name>" elsewhere, for every project the
// ApplicationSet's Applications can be generated in (internal/appset Projects: the literal
// spec.template.spec.project, "default" when empty, and the project of every generator-level
// template). Every action, on new and existing objects, is authorized on all of them; the API
// rejects a templated project, and one on an existing object fails closed (wildcard project).
// Reads of existing ApplicationSets answer "permission denied" both for missing and forbidden
// objects, so the API is no existence oracle. Generation (generate, create dry run) is bounded
// while it runs (BoundedGenerator).
package applicationsets

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Defaults of Config.
const (
	DefaultGenerateTimeout          = 60 * time.Second
	DefaultMaxGeneratedApplications = 5000
	DefaultMaxGenerateParameterSets = generators.DefaultMaxParameterSets
	DefaultMaxGenerateResponseBytes = 32 << 20
	DefaultMaxEvents                = 1000
	DefaultUpdateRetries            = 10
)

// Config is the static configuration of the service.
type Config struct {
	// Namespace is the control-plane namespace (default namespace of every route).
	Namespace string
	// Namespaces are the additional namespaces ApplicationSets may live in
	// (applicationsetcontroller.namespaces: names, globs or "/regexp/"). Others are refused
	// (get, create, delete, generate, tree, events) or filtered (list, watch).
	Namespaces []string
	// GenerateTimeout bounds one generation (generate and create dry-run).
	GenerateTimeout time.Duration
	// MaxGeneratedApplications and MaxGenerateResponseBytes bound a generation's output, and
	// MaxGenerateParameterSets the parameter sets of all its generators together (before
	// post-selectors). They are passed to the Generator as GenerateLimits and enforced while
	// generating; the output is checked again before it is returned.
	MaxGeneratedApplications int
	MaxGenerateResponseBytes int
	MaxGenerateParameterSets int
	// MaxEvents bounds the events returned by the events route.
	MaxEvents int64
	// UpdateRetries bounds the conflict retries of an unconditional upsert.
	UpdateRetries int
}

// Watcher opens watches; controller-runtime's client.WithWatch implements it.
type Watcher interface {
	Watch(ctx context.Context, obj client.ObjectList, opts ...client.ListOption) (watch.Interface, error)
}

// Deps are the collaborators of the service. Routes whose collaborators are nil answer 503,
// so New(Deps{}) still registers every route (OpenAPI generation).
type Deps struct {
	Config Config
	// Reader reads ApplicationSets and AppProjects (may be a cache).
	Reader client.Reader
	// APIReader reads Events (field selectors on involvedObject.*), re-reads objects after
	// write conflicts and lists before watching; it must not be a cache. Nil uses Reader.
	APIReader client.Reader
	// Writer creates, updates and deletes ApplicationSets.
	Writer client.Writer
	// Watcher watches ApplicationSets for the stream route.
	Watcher Watcher
	// Generator renders ApplicationSets (generate, create dry-run) within GenerateLimits;
	// NewGenerator returns the implementation to wire.
	Generator Generator
	// Recorder records the Kubernetes Events of API changes on the ApplicationSet
	// (ResourceCreated, ResourceUpdated, ResourceDeleted); nil records none.
	Recorder rest.EventRecorder
	Logger   *slog.Logger
}

// Service serves the ApplicationSet routes; it implements apiserver.Service.
type Service struct {
	d Deps
}

// New returns the service.
func New(d Deps) *Service {
	c := &d.Config
	if c.GenerateTimeout <= 0 {
		c.GenerateTimeout = DefaultGenerateTimeout
	}
	if c.MaxGeneratedApplications <= 0 {
		c.MaxGeneratedApplications = DefaultMaxGeneratedApplications
	}
	if c.MaxGenerateResponseBytes <= 0 {
		c.MaxGenerateResponseBytes = DefaultMaxGenerateResponseBytes
	}
	if c.MaxGenerateParameterSets <= 0 {
		c.MaxGenerateParameterSets = DefaultMaxGenerateParameterSets
	}
	if c.MaxEvents <= 0 {
		c.MaxEvents = DefaultMaxEvents
	}
	if c.UpdateRetries <= 0 {
		c.UpdateRetries = DefaultUpdateRetries
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.APIReader == nil {
		d.APIReader = d.Reader
	}
	return &Service{d: d}
}

const tag = "applicationsets"

// objectDoc documents the RBAC object of the routes.
const objectDoc = "<project>/<name>"

func access(action, note string) rest.Access {
	a := rest.RBACInHandler(rbac.ResourceApplicationSets, action, objectDoc)
	a.Note = note
	return a
}

// Register adds the ApplicationSet routes to t.
func (s *Service) Register(t *rest.Table) {
	const nsNote = "; on every project the ApplicationSet generates into (template and generator-level templates); <project>/<namespace>/<name> outside the control-plane namespace"
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathApplicationSets, OperationID: "listApplicationSets",
		Summary: "List ApplicationSets", Tags: []string{tag},
		Access:      access(rbac.ActionGet, "items filtered per ApplicationSet"+nsNote),
		Description: "ApplicationSets in enabled namespaces the caller may get, sorted by name; `projects`, `selector` and `appsetNamespace` filter, `limit`/`continue` paginate.",
	}, s.list)
	t.Add(rest.Route{
		Method: http.MethodGet, Path: apiclient.PathApplicationSetsStream, OperationID: "watchApplicationSets",
		Summary: "Watch ApplicationSets (Server-Sent Events)", Tags: []string{tag},
		Access:   access(rbac.ActionGet, "every event authorized when it is sent"+nsNote),
		Request:  reflect.TypeFor[apiclient.ApplicationSetWatchRequest](),
		Response: reflect.TypeFor[apiclient.ApplicationSetWatchEvent](),
		Stream:   true,
		Audit:    rest.AuditNever,
		Handler:  http.HandlerFunc(s.watch),
		Description: "Events `{type: ADDED|MODIFIED|DELETED, object}`. Without `resourceVersion`, the stream starts now and, with `name`, sends the current state first (ADDED). " +
			"A `resourceVersion` (e.g. of a previous list) or `Last-Event-ID` starts the watch there, so no change is missed; an expired version ends the stream with a 410 error event.",
	})
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: apiclient.PathApplicationSets, OperationID: "createApplicationSet",
		Summary: "Create ApplicationSet", Tags: []string{tag},
		Access:      access(rbac.ActionCreate, "upsert of a different spec also needs update on the projects of the new and the existing object"+nsNote),
		Description: "Creates the ApplicationSet; an identical existing one is returned unchanged, a different one needs `upsert=true`. `dryRun=true` renders the generated Applications into `status.resources` without persisting.",
	}, s.create)
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: apiclient.PathApplicationSetGenerate, OperationID: "generateApplicationSet",
		Summary: "Dry-run render of generated Applications", Tags: []string{tag},
		Access:             access(rbac.ActionCreate, "dry run"+nsNote),
		AllowUnknownFields: true, // the embedded ApplicationSet may carry fields of newer API versions
		Description:        "Renders the Applications the ApplicationSet would generate (generators, templates, templatePatch) without validating destinations and without writing anything.",
	}, s.generate)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathApplicationSet, OperationID: "getApplicationSet",
		Summary: "Get ApplicationSet", Tags: []string{tag},
		Access: access(rbac.ActionGet, "missing and forbidden ApplicationSets are both 403"+nsNote),
	}, s.get)
	rest.Register(t, rest.Route{
		Method: http.MethodDelete, Path: apiclient.PathApplicationSet, OperationID: "deleteApplicationSet",
		Summary: "Delete ApplicationSet", Tags: []string{tag},
		Access:      access(rbac.ActionDelete, "missing and forbidden ApplicationSets are both 403"+nsNote),
		Description: "`cascade=false` orphans the generated Applications; `propagationPolicy` is `foreground` or `background` (default).",
	}, s.delete)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathApplicationSetResourceTree, OperationID: "getApplicationSetResourceTree",
		Summary: "ApplicationSet resource tree", Tags: []string{tag},
		Access:      access(rbac.ActionGet, "missing and forbidden ApplicationSets are both 403"+nsNote),
		Description: "One node per generated Application of `status.resources`, with health and sync status, parented by the ApplicationSet.",
	}, s.resourceTree)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathApplicationSetEvents, OperationID: "listApplicationSetEvents",
		Summary: "ApplicationSet events", Tags: []string{tag},
		Access: access(rbac.ActionGet, "missing and forbidden ApplicationSets are both 403"+nsNote),
	}, s.events)
}

var errNotConfigured = errors.New("not configured")

func unavailable(what string) error {
	return rest.Unavailable("the ApplicationSet %s is not available", what).WithCause(errNotConfigured)
}
