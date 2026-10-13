package rbac

import "slices"

// Keys of the RBAC ConfigMap (patrol-rbac-cm). Key names follow Argo CD (ADR-0003).
const (
	ConfigMapPolicyCSVKey     = "policy.csv"
	ConfigMapPolicyDefaultKey = "policy.default"
	ConfigMapScopesKey        = "scopes"
	ConfigMapMatchModeKey     = "policy.matchMode"
)

// Match modes for resource, action and object patterns.
const (
	GlobMatchMode  = "glob"
	RegexMatchMode = "regex"
)

// Built-in roles defined by [BuiltinPolicy].
const (
	RoleAdmin    = "role:admin"
	RoleReadOnly = "role:readonly"
)

// Resources of the baseline RBAC model.
const (
	ResourceClusters          = "clusters"
	ResourceProjects          = "projects"
	ResourceApplications      = "applications"
	ResourceApplicationSets   = "applicationsets"
	ResourceRepositories      = "repositories"
	ResourceWriteRepositories = "write-repositories"
	ResourceCertificates      = "certificates"
	ResourceAccounts          = "accounts"
	ResourceGPGKeys           = "gpgkeys"
	ResourceLogs              = "logs"
	ResourceExec              = "exec"
	ResourceExtensions        = "extensions"
)

// Actions of the baseline RBAC model. Applications additionally use the path forms
// "update/<group>/<kind>/<namespace>/<name>", "delete/<group>/<kind>/<namespace>/<name>"
// and "action/<group>/<kind>/<action-name>"; see [SubResourceAction] and [CustomAction].
const (
	ActionGet      = "get"
	ActionCreate   = "create"
	ActionUpdate   = "update"
	ActionDelete   = "delete"
	ActionSync     = "sync"
	ActionOverride = "override"
	ActionAction   = "action"
	ActionInvoke   = "invoke"
	ActionAll      = "*"
)

// Effects of a 'p' rule.
const (
	EffectAllow = "allow"
	EffectDeny  = "deny"
)

// maxHierarchyLevel bounds role inheritance depth: a subject reaches roles at most this
// many 'g' hops away (the Casbin default role manager limit used upstream).
const maxHierarchyLevel = 10

var (
	resources = []string{
		ResourceClusters,
		ResourceProjects,
		ResourceApplications,
		ResourceApplicationSets,
		ResourceRepositories,
		ResourceWriteRepositories,
		ResourceCertificates,
		ResourceAccounts,
		ResourceGPGKeys,
		ResourceLogs,
		ResourceExec,
		ResourceExtensions,
	}
	actions = []string{
		ActionGet,
		ActionCreate,
		ActionUpdate,
		ActionDelete,
		ActionSync,
		ActionOverride,
		ActionAction,
		ActionInvoke,
	}
	projectScoped = map[string]bool{
		ResourceApplications:    true,
		ResourceApplicationSets: true,
		ResourceLogs:            true,
		ResourceExec:            true,
		ResourceClusters:        true,
		ResourceRepositories:    true,
	}
	defaultScopes = []string{"groups"}
)

// Resources returns every baseline resource name.
func Resources() []string { return slices.Clone(resources) }

// Actions returns every baseline action verb (without path forms and without "*").
func Actions() []string { return slices.Clone(actions) }

// IsProjectScoped reports whether objects of resource are "<project>/..." names, which
// makes project role policies applicable to them. Matches upstream ProjectScoped.
func IsProjectScoped(resource string) bool { return projectScoped[resource] }

// DefaultScopes returns the OIDC claims used as group subjects when the "scopes" key is
// absent: ["groups"].
func DefaultScopes() []string { return slices.Clone(defaultScopes) }

func isKnownResource(r string) bool { return slices.Contains(resources, r) }
