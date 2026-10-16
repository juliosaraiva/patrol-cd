package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"unicode"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
)

// serviceAccountDisallowedChars may not appear in a destination service account name.
const serviceAccountDisallowedChars = "!*[]{}\\/"

// maxWindowDescription is the maximum length of a sync window description.
const maxWindowDescription = 255

var (
	roleNameRegexp     = regexp.MustCompile(`^[a-zA-Z0-9]([-_a-zA-Z0-9]*[a-zA-Z0-9])?$`)
	groupInvalidChars  = regexp.MustCompile("[\"\n\r\t]")
	validPolicyActions = map[string]bool{
		rbac.ActionGet: true, rbac.ActionCreate: true, rbac.ActionUpdate: true, rbac.ActionDelete: true,
		rbac.ActionSync: true, rbac.ActionOverride: true, rbac.ActionAll: true,
	}
	// Unanchored, as upstream: any action containing one of these prefixes is accepted.
	validPolicyActionPatterns = []*regexp.Regexp{
		regexp.MustCompile("action/.*"),
		regexp.MustCompile("update/.*"),
		regexp.MustCompile("delete/.*"),
	}
)

// ValidateProject validates an AppProject the way the upstream API server does on project
// create/update and before token create/delete (AppProject.ValidateProject followed by the
// policy syntax check). It returns the first problem as a *ValidationError with the upstream
// message and code, or nil. It does not modify proj; call NormalizePolicies before and
// NormalizeSyncWindowTimeZones after it to reproduce the upstream write path.
func ValidateProject(proj *v1alpha1.AppProject) error {
	if proj == nil {
		return invalidf(CodeInvalidArgument, "missing payload 'project' in request")
	}
	if err := validateProjectSpec(proj); err != nil {
		return err
	}
	if _, _, err := rbac.Parse(ProjectPoliciesString(proj), rbac.ParseOptions{Source: "project/" + proj.Name}); err != nil {
		return invalidf(CodeInvalidArgument, "policy syntax error: %s", err.Error())
	}
	return nil
}

func validateProjectSpec(proj *v1alpha1.AppProject) error {
	spec := &proj.Spec

	destKeys := make(map[string]bool, len(spec.Destinations))
	for _, dest := range spec.Destinations {
		if dest.Name == "!*" {
			return invalidf(CodeInvalidArgument, "name has an invalid format, '!*'")
		}
		if dest.Server == "!*" {
			return invalidf(CodeInvalidArgument, "server has an invalid format, '!*'")
		}
		if dest.Namespace == "!*" {
			return invalidf(CodeInvalidArgument, "namespace has an invalid format, '!*'")
		}
		key := dest.Server + "/" + dest.Namespace
		if dest.Server == "" && dest.Name != "" {
			key = dest.Name + "/" + dest.Namespace
		}
		if destKeys[key] {
			return invalidf(CodeInvalidArgument, "destination '%s' already added", key)
		}
		destKeys[key] = true
	}

	srcNamespaces := make(map[string]bool, len(spec.SourceNamespaces))
	for _, ns := range spec.SourceNamespaces {
		if srcNamespaces[ns] {
			return invalidf(CodeInvalidArgument, "source namespace '%s' already added", ns)
		}
		srcNamespaces[ns] = true
	}

	srcRepos := make(map[string]bool, len(spec.SourceRepos))
	for _, src := range spec.SourceRepos {
		if src == "!*" {
			return invalidf(CodeInvalidArgument, "source repository has an invalid format, '!*'")
		}
		if srcRepos[src] {
			return invalidf(CodeInvalidArgument, "source repository '%s' already added", src)
		}
		srcRepos[src] = true
	}

	roleNames := make(map[string]bool, len(spec.Roles))
	for _, role := range spec.Roles {
		if roleNames[role.Name] {
			return invalidf(CodeAlreadyExists, "role '%s' already exists", role.Name)
		}
		if err := ValidateRoleName(role.Name); err != nil {
			return err
		}
		policies := make(map[string]bool, len(role.Policies))
		for _, policy := range role.Policies {
			if policies[policy] {
				return invalidf(CodeAlreadyExists, "policy '%s' already exists for role '%s'", policy, role.Name)
			}
			if err := ValidateRolePolicy(proj.Name, role.Name, policy); err != nil {
				return err
			}
			policies[policy] = true
		}
		groups := make(map[string]bool, len(role.Groups))
		for _, group := range role.Groups {
			if groups[group] {
				return invalidf(CodeAlreadyExists, "group '%s' already exists for role '%s'", group, role.Name)
			}
			if err := ValidateGroupName(group); err != nil {
				return err
			}
			groups[group] = true
		}
		roleNames[role.Name] = true
	}

	windows := make(map[uint64]bool, len(spec.SyncWindows))
	for i := range spec.SyncWindows {
		w := &spec.SyncWindows[i]
		hash, err := SyncWindowIdentityHash(*w)
		if err != nil {
			return invalidf(CodeInternal, "failed to generate hash for sync window with kind '%s', schedule '%s', and duration '%s': %v", w.Kind, w.Schedule, w.Duration, err)
		}
		if windows[hash] {
			return invalidf(CodeAlreadyExists, "sync window with kind '%s', schedule '%s', and duration '%s' already exists (hash=%d, duplicate detected)", w.Kind, w.Schedule, w.Duration, hash)
		}
		if err := ValidateSyncWindow(*w); err != nil {
			return err
		}
		if len(w.Applications) == 0 && len(w.Namespaces) == 0 && len(w.Clusters) == 0 {
			return invalidf(CodeOutOfRange, "window '%s':'%s':'%s' requires one of application, cluster or namespace", w.Kind, w.Schedule, w.Duration)
		}
		windows[hash] = true
	}

	dsaKeys := make(map[string]bool, len(spec.DestinationServiceAccounts))
	for _, dsa := range spec.DestinationServiceAccounts {
		if strings.Contains(dsa.Server, "!") {
			return invalidf(CodeInvalidArgument, "server has an invalid format, '%s'", dsa.Server)
		}
		if strings.Contains(dsa.Namespace, "!") {
			return invalidf(CodeInvalidArgument, "namespace has an invalid format, '%s'", dsa.Namespace)
		}
		if !validServiceAccountName(dsa.DefaultServiceAccount) {
			return invalidf(CodeInvalidArgument, "defaultServiceAccount has an invalid format, '%s'", dsa.DefaultServiceAccount)
		}
		if _, err := compileGlob(dsa.Server); err != nil {
			return invalidf(CodeInvalidArgument, "server has an invalid format, '%s'", dsa.Server)
		}
		if _, err := compileGlob(dsa.Namespace); err != nil {
			return invalidf(CodeInvalidArgument, "namespace has an invalid format, '%s'", dsa.Namespace)
		}
		key := dsa.Server + "/" + dsa.Namespace
		if dsaKeys[key] {
			return invalidf(CodeInvalidArgument, "destinationServiceAccount '%s' already added", key)
		}
		dsaKeys[key] = true
	}
	return nil
}

func validServiceAccountName(sa string) bool {
	return strings.Trim(sa, " ") != "" && !strings.ContainsAny(sa, serviceAccountDisallowedChars)
}

// ValidateRoleName checks a project role name.
func ValidateRoleName(name string) error {
	if !roleNameRegexp.MatchString(name) {
		return invalidf(CodeInvalidArgument, "invalid role name '%s'. Must consist of alphanumeric characters, '-' or '_', and must start and end with an alphanumeric character", name)
	}
	return nil
}

// ValidateRolePolicy checks one project role policy line (TEN-018): it must be
// "p, proj:<project>:<role>, <resource>, <action>, <object>, <allow|deny>" with a
// project-scoped resource, a known action and an object inside the project
// ("<project>/<name>" or "<project>/<namespace>/<name>", '*' allowed).
func ValidateRolePolicy(project, role, policy string) error {
	parts := strings.Split(policy, ",")
	if len(parts) != 6 || strings.Trim(parts[0], " ") != "p" {
		return invalidf(CodeInvalidArgument, "invalid policy rule '%s': must be of the form: 'p, sub, res, act, obj, eft'", policy)
	}
	subject := strings.Trim(parts[1], " ")
	expected := fmt.Sprintf("proj:%s:%s", project, role)
	if subject != expected {
		return invalidf(CodeInvalidArgument, "invalid policy rule '%s': policy subject must be: '%s', not '%s'", policy, expected, subject)
	}
	resource := strings.Trim(parts[2], " ")
	if !rbac.IsProjectScoped(resource) {
		return invalidf(CodeInvalidArgument, "invalid policy rule '%s': project resource must be: 'applications', 'applicationsets', 'repositories', 'exec', 'logs' or 'clusters', not '%s'", policy, resource)
	}
	action := strings.Trim(parts[3], " ")
	if !isValidPolicyAction(action) {
		return invalidf(CodeInvalidArgument, "invalid policy rule '%s': invalid action '%s'", policy, action)
	}
	object := strings.Trim(parts[4], " ")
	if !isValidPolicyObject(project, object) {
		return invalidf(CodeInvalidArgument, "invalid policy rule '%s': object must be of form '%s/*', '%s[/<NAMESPACE>]/<APPNAME>' or '%s/<APPNAME>', not '%s'", policy, project, project, project, object)
	}
	effect := strings.Trim(parts[5], " ")
	if effect != "allow" && effect != "deny" {
		return invalidf(CodeInvalidArgument, "invalid policy rule '%s': effect must be: 'allow' or 'deny'", policy)
	}
	return nil
}

func isValidPolicyAction(action string) bool {
	if validPolicyActions[action] {
		return true
	}
	for _, re := range validPolicyActionPatterns {
		if re.MatchString(action) {
			return true
		}
	}
	return false
}

var objectRegexpCache = newCache[string, compiledRegexp](1024)

func isValidPolicyObject(project, object string) bool {
	c, ok := objectRegexpCache.Get(project)
	if !ok {
		re, err := regexp.Compile(`^` + regexp.QuoteMeta(project) + `(/[*\w-.]+)?/[*\w-.]+$`)
		c = compiledRegexp{re: re, err: err}
		objectRegexpCache.Add(project, c)
	}
	return c.err == nil && c.re.MatchString(object)
}

// ValidateGroupName checks an SSO group bound to a project role (TEN-019). Messages use the
// group name with surrounding spaces and quotes removed, as upstream.
func ValidateGroupName(name string) error {
	runes := []rune(name)
	trimmed := strings.TrimSpace(name)
	if len(trimmed) > 1 && strings.HasPrefix(trimmed, `"`) && strings.HasSuffix(trimmed, `"`) {
		trimmed = trimmed[1 : len(trimmed)-1]
	} else if strings.Contains(trimmed, ",") {
		return invalidf(CodeInvalidArgument, "group '%s' must be quoted", trimmed)
	}
	if trimmed == "" {
		return invalidf(CodeInvalidArgument, "group '%s' is empty", trimmed)
	}
	if groupInvalidChars.MatchString(trimmed) {
		return invalidf(CodeInvalidArgument, "group '%s' contains invalid characters", trimmed)
	}
	if len(runes) > 1 && unicode.IsSpace(runes[0]) {
		return invalidf(CodeInvalidArgument, "group '%s' contains a leading space", trimmed)
	}
	if len(runes) > 1 && unicode.IsSpace(runes[len(runes)-1]) {
		return invalidf(CodeInvalidArgument, "group '%s' contains a trailing space", trimmed)
	}
	return nil
}

// ValidateSyncWindow checks one window (upstream SyncWindow.Validate, without its time-zone
// rewrite; see NormalizeSyncWindowTimeZones): kind, schedule, duration and description
// length. Selectors are checked by ValidateProject.
func ValidateSyncWindow(w v1alpha1.SyncWindow) error {
	if w.Kind != SyncWindowKindAllow && w.Kind != SyncWindowKindDeny {
		return invalidf(CodeInvalidArgument, "kind '%s' mismatch: can only be allow or deny", w.Kind)
	}
	if _, _, err := parseWindow(&w); err != nil {
		return &ValidationError{Code: CodeInvalidArgument, Message: err.Error()}
	}
	if len(w.Description) > maxWindowDescription {
		return invalidf(CodeInvalidArgument, "description must not exceed 255 characters")
	}
	return nil
}

// syncWindowIdentity holds the fields that identify a window: manualSync, description and
// syncOverrun are behavior/metadata and excluded, so two windows differing only there are
// duplicates.
type syncWindowIdentity struct {
	Kind           string   `json:"k"`
	Schedule       string   `json:"s"`
	Duration       string   `json:"d"`
	Applications   []string `json:"a,omitempty"`
	Namespaces     []string `json:"n,omitempty"`
	Clusters       []string `json:"c,omitempty"`
	TimeZone       string   `json:"z"`
	UseAndOperator bool     `json:"o"`
}

// SyncWindowIdentityHash returns the identity hash used for duplicate detection. The value is
// stable within Patrol but differs from upstream's (gob + xxhash); only equality matters.
func SyncWindowIdentityHash(w v1alpha1.SyncWindow) (uint64, error) {
	b, err := json.Marshal(syncWindowIdentity{
		Kind: w.Kind, Schedule: w.Schedule, Duration: w.Duration,
		Applications: w.Applications, Namespaces: w.Namespaces, Clusters: w.Clusters,
		TimeZone: w.TimeZone, UseAndOperator: w.UseAndOperator,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to encode sync window for hashing: %w", err)
	}
	h := fnv.New64a()
	_, _ = h.Write(b)
	return h.Sum64(), nil
}

// NormalizeSyncWindowTimeZones rewrites empty and unknown window time zones to "UTC", as
// upstream validation does on project create/update. It reports whether anything changed.
func NormalizeSyncWindowTimeZones(spec *v1alpha1.AppProjectSpec) bool {
	changed := false
	if spec == nil {
		return false
	}
	for i := range spec.SyncWindows {
		tz := spec.SyncWindows[i].TimeZone
		if _, ok := loadLocation(tz); tz == "" || !ok {
			spec.SyncWindows[i].TimeZone = "UTC"
			changed = changed || tz != "UTC"
		}
	}
	return changed
}

// ErrWindowRequired is the error of AddSyncWindow for a missing kind, schedule or duration.
var ErrWindowRequired = errors.New("cannot create window: require kind, schedule, duration and one or more of applications, namespaces and clusters")

// AddSyncWindow appends a validated window to spec (upstream AppProjectSpec.AddWindow). The
// window's time zone is normalized like ValidateProject's write path. Selector presence and
// duplicates are checked by ValidateProject.
func AddSyncWindow(spec *v1alpha1.AppProjectSpec, w v1alpha1.SyncWindow) error {
	if spec == nil {
		return errors.New("cannot create window: no project spec")
	}
	if w.Kind == "" || w.Schedule == "" || w.Duration == "" {
		return ErrWindowRequired
	}
	if len(w.Applications) == 0 {
		w.Applications = nil
	}
	if len(w.Namespaces) == 0 {
		w.Namespaces = nil
	}
	if len(w.Clusters) == 0 {
		w.Clusters = nil
	}
	if _, ok := loadLocation(w.TimeZone); w.TimeZone == "" || !ok {
		w.TimeZone = "UTC"
	}
	if err := ValidateSyncWindow(w); err != nil {
		return err
	}
	spec.SyncWindows = append(spec.SyncWindows, w)
	return nil
}

// DeleteSyncWindow removes the window at index id (upstream AppProjectSpec.DeleteWindow).
func DeleteSyncWindow(spec *v1alpha1.AppProjectSpec, id int) error {
	if spec == nil || id < 0 || id >= len(spec.SyncWindows) {
		return fmt.Errorf("window with id '%d' not found", id)
	}
	spec.SyncWindows = append(spec.SyncWindows[:id], spec.SyncWindows[id+1:]...)
	return nil
}

// SyncWindowUpdate holds the fields `proj windows update` may change; empty values keep the
// current value, except TimeZone which defaults to "UTC" as upstream.
type SyncWindowUpdate struct {
	Schedule     string
	Duration     string
	Applications []string
	Namespaces   []string
	Clusters     []string
	TimeZone     string
	Description  string
}

// UpdateSyncWindow applies u to w (upstream SyncWindow.Update).
func UpdateSyncWindow(w *v1alpha1.SyncWindow, u SyncWindowUpdate) error {
	if w == nil {
		return errors.New("cannot update: no window")
	}
	if u.Schedule == "" && u.Duration == "" && len(u.Applications) == 0 && len(u.Namespaces) == 0 && len(u.Clusters) == 0 && u.Description == "" {
		return errors.New("cannot update: require one or more of schedule, duration, application, namespace, cluster or description")
	}
	if u.Schedule != "" {
		w.Schedule = u.Schedule
	}
	if u.Duration != "" {
		w.Duration = u.Duration
	}
	if len(u.Applications) > 0 {
		w.Applications = u.Applications
	}
	if len(u.Namespaces) > 0 {
		w.Namespaces = u.Namespaces
	}
	if len(u.Clusters) > 0 {
		w.Clusters = u.Clusters
	}
	if u.Description != "" {
		w.Description = u.Description
	}
	w.TimeZone = u.TimeZone
	if w.TimeZone == "" {
		w.TimeZone = "UTC"
	}
	return nil
}
