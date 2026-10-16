package applications

import (
	"net/http"
	"reflect"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	pubapi "github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// tag is the OpenAPI tag of the application routes.
const tag = "applications"

// rbacObjectDoc documents the RBAC object of application routes.
const rbacObjectDoc = "<project>/<app>"

// syncMaxBodyBytes bounds sync request bodies: up to 1 MiB of local manifests, JSON
// escaped.
const syncMaxBodyBytes = 3 << 20

// routeRegistrars are the route groups of the service, registered in order.
//
// EXTENSION POINT: the resource-level routes (resource tree and its stream, managed
// resources, server-side diff, events, live resource get/patch/delete, resource actions,
// logs, terminal) are added as another group: implement
// `func (s *Service) registerResourceRoutes(t *rest.Table)` in its own file of this
// package and append it here. Those handlers reuse getApp/getAppOnly (authorization with
// the existence-hiding rules), auditApp, Deps.LiveResources and further collaborators
// added to Deps.
var routeRegistrars = []func(*Service, *rest.Table){
	(*Service).registerCoreRoutes,
	(*Service).registerResourceRoutes, // resource_routes.go
}

// Register adds the application routes to t (it implements apiserver.Service).
func (s *Service) Register(t *rest.Table) {
	for _, r := range routeRegistrars {
		r(s, t)
	}
}

func appAccess(action, note string) rest.Access {
	a := rest.RBACInHandler(rbac.ResourceApplications, action, rbacObjectDoc)
	a.Note = note
	return a
}

func (s *Service) registerCoreRoutes(t *rest.Table) {
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplications, OperationID: "listApplications",
		Summary: "List applications", Tags: []string{tag},
		Access:      appAccess(rbac.ActionGet, "items filtered by applications, get, <project>/<app>"),
		Description: "Applications of the enabled namespaces the caller may get, sorted by name. `limit` and `continue` paginate.",
	}, s.listApplications)
	rest.RegisterStream(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathStreamApplications, OperationID: "watchApplications",
		Summary: "Watch applications (Server-Sent Events)", Tags: []string{tag},
		Access:      appAccess(rbac.ActionGet, "every event authorized with applications, get, <project>/<app>"),
		Description: "Events `{type: ADDED|MODIFIED|DELETED, object}`. An application the caller can no longer see is reported as DELETED with its last visible state.",
	}, rest.StreamOptions[pubapi.ApplicationWatchEvent]{}, s.watchApplications)
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: pubapi.PathApplications, OperationID: "createApplication",
		Summary: "Create an application", Tags: []string{tag},
		Access:      appAccess(rbac.ActionCreate, "update also required with upsert"),
		Description: "Idempotent when the existing application is identical; `upsert` updates a different one (needs update). `validate` (default true) renders every source.",
	}, s.createApplication)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplication, OperationID: "getApplication",
		Summary: "Get an application", Tags: []string{tag},
		Access:      appAccess(rbac.ActionGet, "403 for missing and forbidden applications unless a project is given (then 404)"),
		Description: "`refresh=normal|hard` sets the refresh annotation and waits (up to 15 s) until the controller reconciled the application.",
	}, s.getApplication)
	rest.Register(t, rest.Route{
		Method: http.MethodPut, Path: pubapi.PathApplication, OperationID: "updateApplication",
		Summary: "Update an application", Tags: []string{tag},
		Access:      appAccess(rbac.ActionUpdate, "moving to another project also needs create in the new and update in the old project"),
		Description: "Replaces spec, labels, annotations and finalizers; status is ignored. A `metadata.resourceVersion` makes the update conditional (409 when stale).",
	}, s.updateApplication)
	rest.Register(t, rest.Route{
		Method: http.MethodPut, Path: pubapi.PathApplicationSpec, OperationID: "updateApplicationSpec",
		Summary: "Update an application spec", Tags: []string{tag},
		Access: appAccess(rbac.ActionUpdate, ""),
	}, s.updateApplicationSpec)
	rest.Register(t, rest.Route{
		Method: http.MethodPatch, Path: pubapi.PathApplication, OperationID: "patchApplication",
		Summary: "Patch an application (JSON or merge patch)", Tags: []string{tag},
		Access: appAccess(rbac.ActionUpdate, "get then update"),
	}, s.patchApplication)
	rest.Register(t, rest.Route{
		Method: http.MethodDelete, Path: pubapi.PathApplication, OperationID: "deleteApplication",
		Summary: "Delete an application", Tags: []string{tag},
		Access:      appAccess(rbac.ActionDelete, "get then delete"),
		Description: "`cascade` (default true) adds the propagation finalizer of `propagationPolicy` (foreground default, background); `cascade=false` removes the propagation finalizers.",
	}, s.deleteApplication)
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: pubapi.PathApplicationSync, OperationID: "syncApplication",
		Summary: "Sync an application (creates an Operation)", Tags: []string{tag},
		Access:       appAccess(rbac.ActionSync, "applications, override also required for local manifests, source overrides and (with application.sync.requireOverridePrivilegeForRevisionSync) revision overrides"),
		MaxBodyBytes: syncMaxBodyBytes,
		Description:  "Refused while another operation is in progress (409), inside a sync window that blocks manual syncs (403) and for applications being deleted.",
	}, s.syncApplication)
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: pubapi.PathApplicationRollback, OperationID: "rollbackApplication",
		Summary: "Roll back an application to a history entry (creates an Operation)", Tags: []string{tag},
		Access: appAccess(rbac.ActionSync, ""),
	}, s.rollbackApplication)
	rest.Register(t, rest.Route{
		Method: http.MethodDelete, Path: pubapi.PathApplicationOperation, OperationID: "terminateApplicationOperation",
		Summary: "Terminate the running operation (sets spec.cancel)", Tags: []string{tag},
		Access: appAccess(rbac.ActionSync, ""),
	}, s.terminateOperation)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplicationOperations, OperationID: "listApplicationOperations",
		Summary: "List the operations of an application (newest first)", Tags: []string{tag},
		Access: appAccess(rbac.ActionGet, ""),
	}, s.listOperations)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplicationOperationByName, OperationID: "getApplicationOperation",
		Summary: "Get an operation of an application", Tags: []string{tag},
		Access: appAccess(rbac.ActionGet, ""),
	}, s.getOperation)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplicationSyncWindows, OperationID: "getApplicationSyncWindows",
		Summary: "Sync window state of an application", Tags: []string{tag},
		Access: appAccess(rbac.ActionGet, ""),
	}, s.syncWindows)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplicationRevisionMetadata, OperationID: "getApplicationRevisionMetadata",
		Summary: "Git commit metadata of a revision", Tags: []string{tag},
		Access: appAccess(rbac.ActionGet, ""),
	}, s.revisionMetadata)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplicationRevisionChart, OperationID: "getApplicationRevisionChartDetails",
		Summary: "Helm chart metadata of a revision", Tags: []string{tag},
		Access: appAccess(rbac.ActionGet, ""),
	}, s.revisionChartDetails)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplicationRevisionOCIMetadata, OperationID: "getApplicationRevisionOCIMetadata",
		Summary: "OCI metadata of a revision", Tags: []string{tag},
		Access: appAccess(rbac.ActionGet, ""),
	}, s.revisionOCIMetadata)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplicationManifests, OperationID: "getApplicationManifests",
		Summary: "Render the target manifests (Secret data masked)", Tags: []string{tag},
		Access: appAccess(rbac.ActionGet, ""),
	}, s.getManifests)
	t.Add(rest.Route{
		Method: http.MethodPost, Path: pubapi.PathApplicationManifestsWithFiles, OperationID: "getApplicationManifestsWithFiles",
		Summary: "Render a local source directory (gzipped tar body)", Tags: []string{tag},
		Access:             appAccess(rbac.ActionGet, "applications, override also required (local content)"),
		Request:            reflect.TypeFor[pubapi.ApplicationManifestsWithFilesQuery](),
		Response:           reflect.TypeFor[pubapi.ApplicationManifestResponse](),
		RequestContentType: pubapi.ContentTypeGzip,
		RequestDescription: "Gzipped tar archive (binary) of the local source directory; application/x-gzip and application/octet-stream are accepted too, other media types get 415.",
		MaxBodyBytes:       s.d.Config.MaxLocalFilesBytes,
		Audit:              rest.AuditAlways,
		Handler:            http.HandlerFunc(s.manifestsWithFiles),
	})
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplicationLinks, OperationID: "listApplicationLinks",
		Summary: "Deep links of an application (application.links)", Tags: []string{tag},
		Access: appAccess(rbac.ActionGet, ""),
	}, s.listLinks)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplicationResourceLinks, OperationID: "listApplicationResourceLinks",
		Summary: "Deep links of a managed resource (resource.links)", Tags: []string{tag},
		Access: appAccess(rbac.ActionGet, ""),
	}, s.listResourceLinks)
}
