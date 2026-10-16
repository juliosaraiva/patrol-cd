package applications

import (
	"net/http"
	"reflect"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	pubapi "github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// treeStreamBuffer bounds the queued tree events of one stream (trees can be large).
const treeStreamBuffer = 16

// registerResourceRoutes adds the resource-level routes (resource tree and its stream,
// managed resources, server-side diff, events, live resources, resource actions, logs,
// terminal).
func (s *Service) registerResourceRoutes(t *rest.Table) {
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplicationResourceTree, OperationID: "getResourceTree",
		Summary: "Resource tree of an application", Tags: []string{tag},
		Access:      appAccess(rbac.ActionGet, ""),
		Description: "The tree computed by the application controller (managed resources and their descendants, orphaned resources, hosts). A missing tree triggers a normal refresh first.",
	}, s.resourceTree)
	rest.RegisterStream(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathStreamApplicationResourceTree, OperationID: "watchResourceTree",
		Summary: "Watch the resource tree (Server-Sent Events)", Tags: []string{tag},
		Access:      appAccess(rbac.ActionGet, "re-evaluated for every event"),
		Description: "Events `{type, object: ApplicationTree}`: ADDED with the current tree, MODIFIED when it changes, DELETED when the application is deleted. Losing access ends the stream with a 403 error event.",
	}, rest.StreamOptions[pubapi.ApplicationTreeEvent]{Buffer: treeStreamBuffer}, s.watchResourceTree)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplicationManagedResources, OperationID: "getManagedResources",
		Summary: "Managed resources with diff data (Secret data masked)", Tags: []string{tag},
		Access:      appAccess(rbac.ActionGet, ""),
		Description: "Hooks are excluded; non-empty `group`, `kind`, `namespace`, `resourceName` filters match exactly.",
	}, s.managedResources)
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: pubapi.PathApplicationServerSideDiff, OperationID: "serverSideDiff",
		Summary: "Server-side (dry-run apply) diff", Tags: []string{tag},
		Access:       appAccess(rbac.ActionGet, "live resources must be managed resources of the application"),
		MaxBodyBytes: serverSideDiffMaxBodyBytes,
		Description:  "Index-aligned `liveResources` and `targetManifests` (at most 5000). Runs on the controller replica serving the application, with its credentials (not impersonated).",
	}, s.serverSideDiff)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplicationEvents, OperationID: "listApplicationEvents",
		Summary: "Kubernetes events of an application or of one of its resources", Tags: []string{tag},
		Access:      appAccess(rbac.ActionGet, ""),
		Description: "Without `resourceName`/`resourceUID`: the application's events. Otherwise the resource must be in the tree with that UID, name and namespace; its events are read from the destination cluster.",
	}, s.listEvents)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplicationResource, OperationID: "getResource",
		Summary: "Live manifest of a resource of the application tree (Secret data masked)", Tags: []string{tag},
		Access: appAccess(rbac.ActionGet, ""),
	}, s.getResource)
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: pubapi.PathApplicationResource, OperationID: "patchResource",
		Summary: "Patch a live resource of the application tree", Tags: []string{tag},
		Access:      appAccess("update/<group>/<kind>/<namespace>/<name>", "plain update suffices when server.rbac.disableApplicationFineGrainedRBACInheritance is false"),
		Description: "Body `{patch, patchType: json|merge|strategic}`; returns the patched manifest (Secret data masked).",
	}, s.patchResource)
	rest.Register(t, rest.Route{
		Method: http.MethodDelete, Path: pubapi.PathApplicationResource, OperationID: "deleteResource",
		Summary: "Delete a live resource of the application tree", Tags: []string{tag},
		Access:      appAccess("delete/<group>/<kind>/<namespace>/<name>", "plain delete suffices when server.rbac.disableApplicationFineGrainedRBACInheritance is false"),
		Description: "`orphan` deletes with propagation Orphan, `force` with Background and grace period 0, otherwise Foreground.",
	}, s.deleteResource)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplicationResourceActions, OperationID: "listResourceActions",
		Summary: "Actions of a resource (Lua discovery)", Tags: []string{tag},
		Access:      appAccess(rbac.ActionGet, ""),
		Description: "Group `cd.patrol.example`, kind `Application` and `resourceName` = the application's name target the application itself.",
	}, s.listResourceActions)
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: pubapi.PathApplicationResourceActions, OperationID: "runResourceAction",
		Summary: "Run a resource action", Tags: []string{tag},
		Access:       appAccess("action/<group>/<kind>/<action>", "created and patched resources must be permitted by the project"),
		MaxBodyBytes: actionMaxBodyBytes,
		Description:  "Body `{action, resourceActionParameters}`; parameters must be declared by the action's discovery; disabled actions are refused.",
	}, s.runResourceAction)
	logsAccess := rest.RBACInHandler(rbac.ResourceLogs, rbac.ActionGet, rbacObjectDoc)
	logsAccess.Note = "applications, get also required"
	logsResponses := []rest.ResponseDoc{{Status: http.StatusOK, Description: "With download=true: the lines as a text attachment.", ContentType: "application/octet-stream"}}
	t.Add(rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplicationLogs, OperationID: "getApplicationLogs",
		Summary: "Pod logs of an application (Server-Sent Events)", Tags: []string{tag},
		Access:      logsAccess,
		Description: "LogEntry events of the tree's pods matching `kind`/`group`/`resourceName`/`namespace` (directly or through an owner; at most server.maxPodLogsToRender pods), merged by timestamp, then an entry with `last: true`. Without a matching pod the stream ends without events.",
		Request:     reflect.TypeFor[pubapi.ApplicationLogsQuery](), Response: reflect.TypeFor[pubapi.LogEntry](), Stream: true,
		Responses: logsResponses, Handler: http.HandlerFunc(s.serveLogs),
	})
	t.Add(rest.Route{
		Method: http.MethodGet, Path: pubapi.PathApplicationPodLogs, OperationID: "getPodLogs",
		Summary: "Logs of one pod of an application (Server-Sent Events)", Tags: []string{tag},
		Access:      logsAccess,
		Description: "LogEntry events of the pod, which must be in the application's resource tree (otherwise the stream ends without events), then an entry with `last: true`.",
		Request:     reflect.TypeFor[pubapi.ApplicationPodLogsQuery](), Response: reflect.TypeFor[pubapi.LogEntry](), Stream: true,
		Responses: logsResponses, Handler: http.HandlerFunc(s.serveLogs),
	})
	execAccess := rest.RBACInHandler(rbac.ResourceExec, rbac.ActionCreate, rbacObjectDoc)
	execAccess.Note = "applications, get also required; exec.enabled must be true"
	t.Add(rest.Route{
		Method: http.MethodGet, Path: pubapi.PathTerminal, OperationID: "openTerminal",
		Summary: "Web terminal (WebSocket)", Tags: []string{tag},
		Access:      execAccess,
		Description: "WebSocket of TerminalMessage JSON text frames (stdin and resize from the client, stdout from the server) to a running container of a pod of the application's tree. Same-origin handshakes only.",
		Request:     reflect.TypeFor[pubapi.TerminalRequest](), Response: reflect.TypeFor[pubapi.TerminalMessage](),
		Responses: []rest.ResponseDoc{
			{Status: http.StatusSwitchingProtocols, Description: "WebSocket session."},
			{Status: http.StatusNotFound, Description: "exec.enabled is not true."},
		},
		Handler: http.HandlerFunc(s.serveTerminal),
	})
}
