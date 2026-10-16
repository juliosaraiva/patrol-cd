// Package projects implements the AppProject API of patrol-server (api-contract.md,
// "Projects"; upstream ProjectService): CRUD with internal/project validation and the
// upstream privilege-escalation and application-impact checks, the detailed (effective)
// project with its global projects and the scoped repositories and clusters the caller may
// see, project events, active sync windows, project deep links, and project role tokens
// recorded in AppProject status.
//
// Authorization uses RBAC resource "projects" with the project name as object. Token
// routes also admit members of the role's groups (upstream). Project role token metadata
// (status.jwtTokensByRole and the deprecated spec.roles[].jwtTokens) can only be changed by
// the token routes: create and update keep the stored values.
package projects

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Client reads and writes AppProjects (spec and status subresource) and reads
// Applications, ApplicationSets, Clusters, Repositories and cluster credential Secrets of
// the control-plane namespace; a controller-runtime client.Client implements it.
type Client interface {
	client.Reader
	client.Writer
	client.StatusClient
}

// SettingsSource returns the current settings snapshot; *settings.Manager implements it.
type SettingsSource interface {
	Current() *settings.Settings
}

// TokenIssuer mints project role tokens; *auth.TokenService implements it.
type TokenIssuer interface {
	IssueProjectToken(ctx context.Context, project, role, id string, expiresIn time.Duration) (auth.Issued, error)
}

// EventLister lists the Kubernetes Events of the control-plane namespace;
// kubernetes.Interface.CoreV1().Events(namespace) implements it.
type EventLister interface {
	List(ctx context.Context, opts metav1.ListOptions) (*corev1.EventList, error)
}

// PolicySink receives project role policies as soon as this API changes a project;
// *rbac.Enforcer implements it. The server must still follow AppProject changes made
// elsewhere (kubectl, other replicas) from a watch.
type PolicySink interface {
	SetProjectPolicies(project string, roles []rbac.Role) error
	DeleteProjectPolicies(project string)
}

// Config is the static configuration of the service.
type Config struct {
	// Namespace is the control-plane namespace holding AppProjects.
	Namespace string
}

// Deps are the collaborators of the service. With zero Deps the routes register (OpenAPI
// generation) and answer 503.
type Deps struct {
	Config   Config
	Client   Client
	Settings SettingsSource
	Tokens   TokenIssuer
	Events   EventLister
	// Clusters resolves Application destinations for the update impact check; nil builds a
	// lookup from the Cluster objects on each update (internal/project.ClusterIndex).
	Clusters project.ClusterLookup
	// Policies receives role policy changes (optional).
	Policies PolicySink
	Logger   *slog.Logger
	Now      func() time.Time
}

// Service serves the project routes.
type Service struct {
	d     Deps
	locks *keyLock
}

// New returns the project service.
func New(d Deps) *Service {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Service{d: d, locks: newKeyLock()}
}

const tag = "projects"

// Register adds the project routes to t.
func (s *Service) Register(t *rest.Table) {
	get := rest.RBACPathValue(rbac.ResourceProjects, rbac.ActionGet, "name")
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathProjects, OperationID: "listProjects",
		Summary: "List projects", Tags: []string{tag},
		Access: withNote(rest.RBACInHandler(rbac.ResourceProjects, rbac.ActionGet, "<name>"), "items filtered per project"),
	}, s.list)
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: apiclient.PathProjects, OperationID: "createProject",
		Summary: "Create a project", Tags: []string{tag}, AllowUnknownFields: true,
		Access:      withNote(rest.RBACInHandler(rbac.ResourceProjects, rbac.ActionCreate, "<name>"), "upsert also needs projects, update; destinations and sources need clusters/repositories update"),
		Description: "Validates the project (internal/project), requires clusters, update on each destination server and repositories, update on each source repository, and creates it. An identical existing project is returned; a different one is an error unless upsert is set.",
	}, s.create)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathProject, OperationID: "getProject",
		Summary: "Get a project", Tags: []string{tag}, Access: get,
	}, s.get)
	rest.Register(t, rest.Route{
		Method: http.MethodPut, Path: apiclient.PathProject, OperationID: "updateProject",
		Summary: "Update a project", Tags: []string{tag}, AllowUnknownFields: true,
		Access:      withNote(rest.RBACPathValue(rbac.ResourceProjects, rbac.ActionUpdate, "name"), "added or removed destinations and sources need clusters/repositories update; resource list changes need clusters update on every destination"),
		Description: "Replaces the spec, labels and annotations. Changes that would make existing Applications of the project invalid are refused.",
	}, s.update)
	rest.Register(t, rest.Route{
		Method: http.MethodDelete, Path: apiclient.PathProject, OperationID: "deleteProject",
		Summary: "Delete a project", Tags: []string{tag},
		Access:      rest.RBACInHandler(rbac.ResourceProjects, rbac.ActionDelete, "{name}"),
		Description: "The default project cannot be deleted, nor a project referenced by Applications or ApplicationSets of the control-plane namespace.",
	}, s.delete)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathProjectDetailed, OperationID: "getDetailedProject",
		Summary: "Get the effective project with global projects and scoped repositories and clusters", Tags: []string{tag}, Access: get,
	}, s.detailed)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathProjectGlobalProjects, OperationID: "getGlobalProjects",
		Summary: "Global projects a project inherits from", Tags: []string{tag}, Access: get,
	}, s.globalProjects)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathProjectEvents, OperationID: "listProjectEvents",
		Summary: "Kubernetes Events of a project", Tags: []string{tag}, Access: get,
	}, s.events)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathProjectSyncWindows, OperationID: "getProjectSyncWindows",
		Summary: "Sync windows of a project active now", Tags: []string{tag}, Access: get,
	}, s.syncWindows)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathProjectLinks, OperationID: "listProjectLinks",
		Summary: "Project deep links (project.links)", Tags: []string{tag},
		Access: rest.RBACInHandler(rbac.ResourceProjects, rbac.ActionGet, "{name}"),
	}, s.links)
	tokenAccess := withNote(rest.RBACInHandler(rbac.ResourceProjects, rbac.ActionUpdate, "{project}"), "members of the role's groups are also allowed")
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: apiclient.PathProjectRoleToken, OperationID: "createProjectToken",
		Summary: "Create a project role token", Tags: []string{tag}, Access: tokenAccess,
		Description: "Returns the token once; the project records its ID and times in status.jwtTokensByRole.",
	}, s.createToken)
	rest.Register(t, rest.Route{
		Method: http.MethodDelete, Path: apiclient.PathProjectRoleTokenIAT, OperationID: "deleteProjectToken",
		Summary: "Delete (revoke) a project role token", Tags: []string{tag}, Access: tokenAccess,
		Description: "Locates the token by ?id= when given, else by issued-at time. Unknown roles and tokens are not an error.",
	}, s.deleteToken)
}

func withNote(a rest.Access, note string) rest.Access {
	a.Note = note
	return a
}
