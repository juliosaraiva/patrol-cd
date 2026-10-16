package rbac

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrPermissionDenied is matched (errors.Is) by errors returned from EnforceErr.
var ErrPermissionDenied = errors.New("permission denied")

// ProjectRole identifies a project role, the subject of a project-role token
// ("proj:<project>:<role>").
type ProjectRole struct {
	Project string
	Role    string
}

// Subject returns the policy subject "proj:<project>:<role>".
func (r ProjectRole) Subject() string { return projectSubjectPrefix + r.Project + ":" + r.Role }

func (r ProjectRole) valid() bool {
	return r.Project != "" && r.Role != "" && !strings.Contains(r.Project, ":") && !strings.Contains(r.Role, ":")
}

// Subjects is the identity a request is evaluated for.
type Subjects struct {
	// User is the user identifier (local account name, or the SSO user id / subject).
	// A value of the form "proj:<project>:<role>" is reserved for project-role tokens and
	// is ignored here; use ProjectRole.
	User string
	// Groups holds the values of the configured scope claims (see ScopeValues). A group
	// is evaluated only if it is the member of a 'g' line (global or of the request's
	// project), as upstream.
	Groups []string
	// ProjectRole is set only for verified project-role tokens. When set, User is
	// ignored and the subject is ProjectRole.Subject().
	ProjectRole *ProjectRole
}

// Decision is the result of an authorization check.
type Decision struct {
	// Allowed is true when the request is authorized.
	Allowed bool
	// MatchedPolicy is "<source>:<line>: <policy line>" of the rule that decided the
	// request (the allow that granted it or the deny that refused it); empty when no rule
	// matched.
	MatchedPolicy string
	// Reason explains the decision for logs and audit records.
	Reason string
}

// Option configures an Enforcer.
type Option func(*options)

type options struct {
	logger   *slog.Logger
	projects []projectInput
}

type projectInput struct {
	name  string
	roles []Role
}

// WithLogger sets the logger used for policy load events. Requests are never logged.
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.logger = l
		}
	}
}

// WithProjectPolicies registers the roles of a project at construction time. It is
// equivalent to calling SetProjectPolicies after NewEnforcer; problems are logged and
// available from ProjectProblems.
func WithProjectPolicies(project string, roles []Role) Option {
	return func(o *options) {
		o.projects = append(o.projects, projectInput{name: project, roles: cloneRoles(roles)})
	}
}

// Enforcer evaluates requests against the built-in policy, the user policy, the default
// role and per-project role policies. It is safe for concurrent use; Enforce never
// blocks on writers.
type Enforcer struct {
	mu     sync.Mutex // serializes writers
	state  atomic.Pointer[snapshot]
	logger *slog.Logger
}

// snapshot is an immutable enforcement state.
type snapshot struct {
	global      *Policy // built-in + user policy; empty (deny all) when nothing is loaded
	loadErr     error   // non-nil: the last policy failed to load; every request is denied
	defaultRole string
	matchMode   string
	disabled    bool
	projects    map[string]*projectEntry
}

// NewEnforcer returns an Enforcer with no policy: every request is denied until
// SetPolicy (or ApplyConfigMap) succeeds.
func NewEnforcer(opts ...Option) *Enforcer {
	o := options{logger: slog.Default()}
	for _, opt := range opts {
		opt(&o)
	}
	e := &Enforcer{logger: o.logger.With("component", "rbac")}
	st := &snapshot{
		global:    emptyPolicy(GlobMatchMode),
		matchMode: GlobMatchMode,
		projects:  map[string]*projectEntry{},
	}
	for _, p := range o.projects {
		entry := compileProject(p.name, p.roles, st.matchMode)
		e.logProjectProblems(entry)
		st.projects[p.name] = entry
	}
	e.state.Store(st)
	return e
}

func (e *Enforcer) load() *snapshot { return e.state.Load() }

// update applies fn to a copy of the current snapshot and publishes it. Writers are
// serialized; readers keep using the previous snapshot until the swap.
func (e *Enforcer) update(fn func(next *snapshot)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cur := e.load()
	next := *cur
	fn(&next)
	e.state.Store(&next)
}

// SetPolicy atomically replaces the built-in policy, the user policy, the default role
// and the match mode. If either policy has an error-severity problem, the enforcer
// fails closed (denies every request, as upstream cannot build its enforcer either) and
// the returned error is a *PolicyError; the next successful SetPolicy restores service.
// In the user policy, a pattern that cannot be evaluated in the match mode (for example
// '*' in regex mode) is such an error; in the built-in policy it is a warning (see
// Source.Builtin).
func (e *Enforcer) SetPolicy(builtin, user, defaultRole, matchMode string) error {
	return e.SetPolicySources(builtin, []Source{{Name: "user", CSV: user}}, defaultRole, matchMode)
}

// SetPolicySources is SetPolicy with the user policy split into named sources (for
// example "policy.csv" and its overlays, see PolicySources), so problems carry
// per-source line numbers. The Builtin field of the user sources is ignored: they are
// always validated as user policy.
func (e *Enforcer) SetPolicySources(builtin string, user []Source, defaultRole, matchMode string) error {
	sources := make([]Source, 0, len(user)+1)
	sources = append(sources, Source{Name: "builtin", CSV: builtin, Builtin: true})
	for _, s := range user {
		s.Builtin = false
		sources = append(sources, s)
	}
	mode, _ := normalizeMatchMode(matchMode)
	pol, problems, err := ParseSources(sources, ParseOptions{MatchMode: matchMode})

	var recompiled map[string]*projectEntry
	e.update(func(next *snapshot) {
		if mode != next.matchMode {
			// Project role lines are revalidated for the new mode: a line that was a
			// valid glob may be an invalid regular expression ('*'), and the reverse.
			next.projects = recompileProjects(next.projects, mode)
			recompiled = next.projects
		}
		next.matchMode = mode
		next.defaultRole = defaultRole
		if err != nil {
			next.global = emptyPolicy(mode)
			next.loadErr = err
			return
		}
		next.global = pol
		next.loadErr = nil
	})

	for _, name := range slices.Sorted(maps.Keys(recompiled)) {
		e.logProjectProblems(recompiled[name])
	}
	if err != nil {
		e.logger.Error("RBAC policy failed to load; denying all requests until it is fixed", "error", err)
		return fmt.Errorf("load RBAC policy: %w", err)
	}
	warnings := 0
	for _, p := range problems {
		if p.Severity == SeverityWarning {
			warnings++
		}
	}
	if warnings > 0 {
		e.logger.Warn("RBAC policy loaded with warnings", "warnings", warnings, "first", firstWarning(problems))
	}
	e.logger.Info("RBAC policy loaded", "rules", pol.NumRules(), "links", pol.NumLinks(), "matchMode", mode, "defaultRole", defaultRole)
	return nil
}

func firstWarning(problems []ValidationError) string {
	for _, p := range problems {
		if p.Severity == SeverityWarning {
			return p.Error()
		}
	}
	return ""
}

// ApplyConfigMap loads the RBAC ConfigMap data (policy.csv, policy.<suffix>.csv
// overlays in key order, policy.default, policy.matchMode) together with the given
// built-in policy. The "scopes" key is not enforcer state; parse it with ParseScopes.
func (e *Enforcer) ApplyConfigMap(builtin string, data map[string]string) error {
	return e.SetPolicySources(builtin, PolicySources(data), data[ConfigMapPolicyDefaultKey], data[ConfigMapMatchModeKey])
}

// SetEnforcementEnabled turns enforcement on or off. While off, every request is
// allowed (upstream --disable-auth).
func (e *Enforcer) SetEnforcementEnabled(enabled bool) {
	e.update(func(next *snapshot) { next.disabled = !enabled })
}

// SetProjectPolicies registers or replaces the roles of a project. The returned error
// (a *PolicyError) lists invalid roles, policies and groups; the project is installed
// anyway with every invalid item removed, and a role with an invalid policy keeps only
// its deny rules, so a broken project can never grant more than a fixed one. A policy
// line whose resource, action or object does not compile in the active match mode is
// invalid, so no project rule can fail when it is evaluated.
func (e *Enforcer) SetProjectPolicies(project string, roles []Role) error {
	roles = cloneRoles(roles)
	var entry *projectEntry
	e.update(func(next *snapshot) {
		entry = compileProject(project, roles, next.matchMode)
		projects := maps.Clone(next.projects)
		projects[project] = entry
		next.projects = projects
	})
	e.logProjectProblems(entry)
	return policyErrorFrom(entry.problems)
}

// DeleteProjectPolicies unregisters a project.
func (e *Enforcer) DeleteProjectPolicies(project string) {
	e.update(func(next *snapshot) {
		if _, ok := next.projects[project]; !ok {
			return
		}
		projects := maps.Clone(next.projects)
		delete(projects, project)
		next.projects = projects
	})
}

// ProjectProblems returns the problems found when the project was last registered.
func (e *Enforcer) ProjectProblems(project string) []ValidationError {
	entry := e.load().projects[project]
	if entry == nil {
		return nil
	}
	return append([]ValidationError(nil), entry.problems...)
}

// DefaultRole returns the configured default role.
func (e *Enforcer) DefaultRole() string { return e.load().defaultRole }

// MatchMode returns the active match mode.
func (e *Enforcer) MatchMode() string { return e.load().matchMode }

// LoadError returns the error of the last failed SetPolicy, or nil.
func (e *Enforcer) LoadError() error { return e.load().loadErr }

func (e *Enforcer) logProjectProblems(entry *projectEntry) {
	if HasErrors(entry.problems) {
		e.logger.Warn("project RBAC policies are invalid; invalid items are ignored", "project", entry.name, "error", policyErrorFrom(entry.problems))
	}
}

// Enforce decides whether subjects may perform action on object of resource.
func (e *Enforcer) Enforce(s Subjects, resource, action, object string) Decision {
	st := e.load()
	if st.disabled {
		return Decision{Allowed: true, Reason: "RBAC enforcement is disabled"}
	}
	if st.loadErr != nil {
		return Decision{Reason: "RBAC policy failed to load; all requests are denied: " + st.loadErr.Error()}
	}
	global := []*Policy{st.global}

	// 1. The default role, against the global policy.
	if st.defaultRole != "" {
		if r := evaluate(st.defaultRole, global, resource, action, object); r.outcome == outcomeAllow {
			return r.decision(st.defaultRole, kindDefaultRole)
		}
	}

	// 2. The request's project, if it is registered.
	var proj *projectEntry
	if name, ok := ProjectFromObject(resource, object); ok {
		proj = st.projects[name]
	}
	pols := global
	if proj != nil && proj.policy != nil {
		pols = []*Policy{st.global, proj.policy}
	}

	// 3. Project-role tokens are confined to their own project.
	user := s.User
	if s.ProjectRole != nil {
		if !s.ProjectRole.valid() {
			return Decision{Reason: "invalid project role subject"}
		}
		user = s.ProjectRole.Subject()
		if proj != nil {
			if proj.name != s.ProjectRole.Project {
				return Decision{Reason: fmt.Sprintf("project role %q may only access project %q, not %q", user, s.ProjectRole.Project, proj.name)}
			}
			if r, ok := evalDefaultRoleInProject(st, pols, resource, action, object); ok {
				return r.decision(st.defaultRole, kindDefaultRole)
			}
			return evaluate(user, pols, resource, action, object).decision(user, kindSubject)
		}
	} else if _, reserved := ParseProjectSubject(user); reserved {
		user = ""
	}

	// The default role again, now with the project's policies (they can bind it).
	if r, ok := evalDefaultRoleInProject(st, pols, resource, action, object); ok {
		return r.decision(st.defaultRole, kindDefaultRole)
	}

	// 4. The user, then each group that is the member of a 'g' line. The first explicit
	// deny (or evaluation failure) is reported when nothing allows the request.
	var (
		denied     evalResult
		deniedSub  string
		deniedKind subjectKind
	)
	if user != "" {
		r := evaluate(user, pols, resource, action, object)
		switch r.outcome {
		case outcomeAllow:
			return r.decision(user, kindSubject)
		case outcomeDeny, outcomeFailed:
			denied, deniedSub, deniedKind = r, user, kindSubject
		}
	}
	for _, g := range s.Groups {
		if g == "" || !hasMember(g, pols) {
			continue
		}
		if _, reserved := ParseProjectSubject(g); reserved {
			continue
		}
		r := evaluate(g, pols, resource, action, object)
		switch r.outcome {
		case outcomeAllow:
			return r.decision(g, kindGroup)
		case outcomeDeny, outcomeFailed:
			if denied.outcome == outcomeNone {
				denied, deniedSub, deniedKind = r, g, kindGroup
			}
		}
	}
	if denied.outcome != outcomeNone {
		return denied.decision(deniedSub, deniedKind)
	}
	return Decision{Reason: "no policy allows " + resource + " " + action + " on " + strconv.Quote(object)}
}

// evalDefaultRoleInProject re-evaluates the default role with project policies when the
// request has some (upstream evaluates it against the project-augmented enforcer too).
// ok is true when that grants the request.
func evalDefaultRoleInProject(st *snapshot, pols []*Policy, resource, action, object string) (evalResult, bool) {
	if st.defaultRole == "" || len(pols) < 2 {
		return evalResult{}, false
	}
	r := evaluate(st.defaultRole, pols, resource, action, object)
	return r, r.outcome == outcomeAllow
}

// PermissionDeniedError is returned by EnforceErr. It matches ErrPermissionDenied.
type PermissionDeniedError struct {
	Resource, Action, Object string
	// Subject is the user or project-role subject of the request, if any.
	Subject  string
	Decision Decision
}

// Error renders the upstream message shape:
// "permission denied: <resource>, <action>, <object>[, sub: <subject>]".
func (e *PermissionDeniedError) Error() string {
	msg := fmt.Sprintf("%s: %s, %s, %s", ErrPermissionDenied, e.Resource, e.Action, e.Object)
	if e.Subject != "" {
		msg += ", sub: " + e.Subject
	}
	return msg
}

// Unwrap makes errors.Is(err, ErrPermissionDenied) true.
func (e *PermissionDeniedError) Unwrap() error { return ErrPermissionDenied }

// EnforceErr is Enforce returning nil when allowed and a *PermissionDeniedError otherwise.
func (e *Enforcer) EnforceErr(s Subjects, resource, action, object string) error {
	d := e.Enforce(s, resource, action, object)
	if d.Allowed {
		return nil
	}
	sub := s.User
	if s.ProjectRole != nil {
		sub = s.ProjectRole.Subject()
	}
	return &PermissionDeniedError{Resource: resource, Action: action, Object: object, Subject: sub, Decision: d}
}
