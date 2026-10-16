package rbac

import (
	"errors"
	"fmt"
	"strings"
)

// Errors returned by ValidateResourceAction and Can in strict mode.
var (
	ErrUnknownResource = errors.New("unknown RBAC resource")
	ErrInvalidAction   = errors.New("invalid RBAC action for resource")
)

// resourceAliases maps CLI shorthands to RBAC resource names (upstream
// "argocd admin settings rbac can").
var resourceAliases = map[string]string{
	"account":         ResourceAccounts,
	"app":             ResourceApplications,
	"apps":            ResourceApplications,
	"application":     ResourceApplications,
	"applicationsets": ResourceApplicationSets,
	"cert":            ResourceCertificates,
	"certs":           ResourceCertificates,
	"certificate":     ResourceCertificates,
	"cluster":         ResourceClusters,
	"extension":       ResourceExtensions,
	"gpgkey":          ResourceGPGKeys,
	"key":             ResourceGPGKeys,
	"log":             ResourceLogs,
	"logs":            ResourceLogs,
	"exec":            ResourceExec,
	"proj":            ResourceProjects,
	"projs":           ResourceProjects,
	"project":         ResourceProjects,
	"repo":            ResourceRepositories,
	"repos":           ResourceRepositories,
	"repository":      ResourceRepositories,
	"scm":             ResourceSCMProviders,
	"scmprovider":     ResourceSCMProviders,
}

// actionSet maps each valid action of a resource to whether it accepts a "/<path>"
// suffix (application fine-grained and custom resource actions).
type actionSet map[string]bool

var (
	crudActions = actionSet{ActionCreate: false, ActionGet: false, ActionUpdate: false, ActionDelete: false}
	crdActions  = actionSet{ActionCreate: false, ActionGet: false, ActionDelete: false}

	// validResourceActions is the strict-mode table. It follows upstream, plus
	// write-repositories (absent upstream although it is a baseline resource) and
	// accounts "get" (granted by role:readonly but missing from the upstream table).
	validResourceActions = map[string]actionSet{
		ResourceAccounts:          {ActionGet: false, ActionCreate: false, ActionUpdate: false},
		ResourceApplications:      {ActionCreate: false, ActionGet: false, ActionUpdate: true, ActionDelete: true, ActionAction: true, ActionOverride: false, ActionSync: false},
		ResourceApplicationSets:   crudActions,
		ResourceCertificates:      crdActions,
		ResourceClusters:          crudActions,
		ResourceExtensions:        {ActionInvoke: false},
		ResourceGPGKeys:           crdActions,
		ResourceLogs:              {ActionGet: false},
		ResourceExec:              {ActionCreate: false},
		ResourceProjects:          crudActions,
		ResourceRepositories:      crudActions,
		ResourceSCMProviders:      crudActions,
		ResourceWriteRepositories: crudActions,
	}
)

// ResolveResourceName maps a shorthand ("app", "repo", "cert", ...) to its RBAC resource
// name; other values are returned unchanged.
func ResolveResourceName(name string) string {
	if r, ok := resourceAliases[name]; ok {
		return r
	}
	return name
}

// ValidateResourceAction checks that resource is a baseline resource and action one of
// its actions; "update", "delete" and "action" on applications accept a "/<path>"
// suffix. Errors match ErrUnknownResource or ErrInvalidAction.
func ValidateResourceAction(resource, action string) error {
	valid, ok := validResourceActions[resource]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownResource, resource)
	}
	verb, _, hasPath := strings.Cut(action, "/")
	allowPath, ok := valid[verb]
	if !ok || (hasPath && !allowPath) {
		return fmt.Errorf("%w: %q is not a valid action for %s", ErrInvalidAction, action, resource)
	}
	return nil
}

// CanRequest is a question for Can, the engine of "patrol admin settings rbac can".
type CanRequest struct {
	// Subject is a role, user or group name, evaluated literally.
	Subject string
	// Action, for example "get" or "update/apps/Deployment/default/web".
	Action string
	// Resource, or one of its shorthands (see ResolveResourceName).
	Resource string
	// Object is the optional object. For project-scoped resources an empty object or
	// "*" means "*/*".
	Object string
	// Strict rejects unknown resources and invalid actions (upstream --strict, which
	// defaults to true).
	Strict bool
}

// Can answers whether req.Subject may perform req.Action, the way the upstream CLI does:
// the default role first, then the subject literally, against the built-in and user
// policies only (no project policies, no groups). Objects are matched literally, so "*"
// in the object is just a value that wildcard patterns match: a deny rule for a specific
// object ("*/guestbook") does not apply to the object "*/*", while a deny that matches
// the requested object always wins over allows. Errors are returned only for invalid
// requests in strict mode.
func (e *Enforcer) Can(req CanRequest) (Decision, error) {
	resource := ResolveResourceName(req.Resource)
	if req.Strict {
		if err := ValidateResourceAction(resource, req.Action); err != nil {
			return Decision{}, err
		}
	}
	object := req.Object
	if IsProjectScoped(resource) && (object == "" || object == "*") {
		object = "*/*"
	}
	st := e.load()
	if st.disabled {
		return Decision{Allowed: true, Reason: "RBAC enforcement is disabled"}, nil
	}
	if st.loadErr != nil {
		return Decision{Reason: "RBAC policy failed to load; all requests are denied: " + st.loadErr.Error()}, nil
	}
	global := []*Policy{st.global}
	if st.defaultRole != "" {
		if r := evaluate(st.defaultRole, global, resource, req.Action, object); r.outcome == outcomeAllow {
			return r.decision(st.defaultRole, kindDefaultRole), nil
		}
	}
	if req.Subject == "" {
		return Decision{Reason: "no subject"}, nil
	}
	return evaluate(req.Subject, global, resource, req.Action, object).decision(req.Subject, kindSubject), nil
}
