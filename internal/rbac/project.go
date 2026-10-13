package rbac

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

const projectSubjectPrefix = "proj:"

// Role is a project role as declared in AppProject.spec.roles. Callers adapt the CRD
// type field by field: Name, Policies (the 'p' lines) and Groups (OIDC groups bound to
// the role). JWT token metadata is not RBAC state; token validity is checked by the
// auth layer before a ProjectRole subject is built.
type Role struct {
	Name     string
	Policies []string
	Groups   []string
}

func cloneRoles(roles []Role) []Role {
	out := make([]Role, len(roles))
	for i, r := range roles {
		out[i] = Role{Name: r.Name, Policies: slices.Clone(r.Policies), Groups: slices.Clone(r.Groups)}
	}
	return out
}

// ParseProjectSubject parses "proj:<project>:<role>". Like upstream, any string with
// exactly three ':'-separated parts whose first part is "proj" has this form.
func ParseProjectSubject(subject string) (ProjectRole, bool) {
	parts := strings.Split(subject, ":")
	if len(parts) != 3 || parts[0] != "proj" {
		return ProjectRole{}, false
	}
	return ProjectRole{Project: parts[1], Role: parts[2]}, true
}

// ProjectFromObject returns the project a request belongs to: the first '/'-separated
// segment of the object for project-scoped resources when the object has at least two
// segments, and the object itself for the "projects" resource.
func ProjectFromObject(resource, object string) (string, bool) {
	if resource == ResourceProjects {
		return object, object != ""
	}
	if !IsProjectScoped(resource) {
		return "", false
	}
	project, _, ok := strings.Cut(object, "/")
	if !ok {
		return "", false
	}
	return project, true
}

var (
	roleNameRegexp     = regexp.MustCompile(`^[a-zA-Z0-9]([-_a-zA-Z0-9]*[a-zA-Z0-9])?$`)
	groupInvalidChars  = regexp.MustCompile("[\"\n\r\t]")
	projectNameInvalid = ":,\"\n\r\t"
	validRoleActions   = map[string]bool{
		ActionGet: true, ActionCreate: true, ActionUpdate: true, ActionDelete: true,
		ActionSync: true, ActionOverride: true, ActionAll: true,
	}
)

// projectEntry is a compiled project. policy is nil when no role contributes rules.
type projectEntry struct {
	name     string
	roles    []Role
	policy   *Policy
	problems []ValidationError
}

func recompileProjects(projects map[string]*projectEntry, mode string) map[string]*projectEntry {
	out := make(map[string]*projectEntry, len(projects))
	for name, p := range projects {
		out[name] = compileProject(name, p.roles, mode)
	}
	return out
}

// compileProject validates and compiles a project's roles with the upstream AppProject
// rules: role names, 'p' lines whose subject is the role's own subject, a project-scoped
// resource, a valid action, an object inside the project and an allow/deny effect, and
// well-formed group names. Each role also gets the implicit rule
// "p, proj:<project>:<role>, projects, get, <project>, allow".
func compileProject(project string, roles []Role, mode string) *projectEntry {
	entry := &projectEntry{name: project, roles: roles}
	if project == "" || strings.ContainsAny(project, projectNameInvalid) || strings.TrimSpace(project) != project {
		entry.problems = append(entry.problems, ValidationError{
			Source:   "project/" + project,
			Severity: SeverityError,
			Message:  fmt.Sprintf("invalid project name %q: project roles are ignored", project),
		})
		return entry
	}
	objectRegexp, err := regexp.Compile(`^` + regexp.QuoteMeta(project) + `(/[*\w-.]+)?/[*\w-.]+$`)
	if err != nil {
		entry.problems = append(entry.problems, ValidationError{
			Source:   "project/" + project,
			Severity: SeverityError,
			Message:  fmt.Sprintf("cannot build object pattern for project %q: %v; project roles are ignored", project, err),
		})
		return entry
	}
	b := newBuilder(mode)
	seen := map[string]bool{}
	for _, role := range roles {
		source := "project/" + project + "/role/" + role.Name
		fail := func(line int, text, format string, args ...any) {
			entry.problems = append(entry.problems, ValidationError{Source: source, Line: line, Text: text, Severity: SeverityError, Message: fmt.Sprintf(format, args...)})
		}
		if !roleNameRegexp.MatchString(role.Name) {
			fail(0, "", "invalid role name %q: it must consist of alphanumeric characters, '-' or '_', and start and end with an alphanumeric character; the role is ignored", role.Name)
			continue
		}
		if seen[role.Name] {
			fail(0, "", "role %q is defined more than once; the duplicate is ignored", role.Name)
			continue
		}
		seen[role.Name] = true
		subject := ProjectRole{Project: project, Role: role.Name}.Subject()

		type pending struct {
			tokens []string
			loc    location
		}
		var allows, denies []pending
		roleValid := true
		var lineProblems []ValidationError
		for i, raw := range role.Policies {
			lineNo := i + 1
			pl, err := tokenizeLine(raw, DefaultMaxLineBytes)
			if err == nil && pl.kind == lineSkip {
				continue
			}
			if err == nil {
				err = validateRolePolicy(pl, subject, objectRegexp)
			}
			if err != nil {
				roleValid = false
				fail(lineNo, pl.text, "%v; the line is ignored", err)
				continue
			}
			checkFieldWhitespace(pl.tokens, []string{"subject", "resource", "action", "object", "effect"}, func(format string, args ...any) {
				lineProblems = append(lineProblems, ValidationError{Source: source, Line: lineNo, Text: pl.text, Severity: SeverityWarning, Message: fmt.Sprintf(format, args...)})
			})
			p := pending{tokens: pl.tokens, loc: location{source: source, line: lineNo, text: pl.text}}
			if strings.TrimSpace(pl.tokens[5]) == EffectDeny {
				denies = append(denies, p)
			} else {
				allows = append(allows, p)
			}
		}
		entry.problems = append(entry.problems, lineProblems...)
		if !roleValid {
			fail(0, "", "role %q has invalid policies: its allow rules are ignored until they are fixed", role.Name)
		} else {
			implicit := fmt.Sprintf("p, %s, %s, %s, %s, %s", subject, ResourceProjects, ActionGet, project, EffectAllow)
			b.addRule(subject, ResourceProjects, ActionGet, project, effectAllow, location{source: source, text: implicit})
			for _, p := range allows {
				b.addRule(p.tokens[1], p.tokens[2], p.tokens[3], p.tokens[4], parseEffect(p.tokens[5]), p.loc)
			}
		}
		for _, p := range denies {
			b.addRule(p.tokens[1], p.tokens[2], p.tokens[3], p.tokens[4], parseEffect(p.tokens[5]), p.loc)
		}
		for _, group := range role.Groups {
			if err := validateGroupName(group); err != nil {
				fail(0, group, "%v; the group binding is ignored", err)
				continue
			}
			// Build the binding exactly as upstream does (a CSV 'g' line), so quoted
			// group names are unquoted the same way.
			line := fmt.Sprintf("g, %s, %s", group, subject)
			pl, err := tokenizeLine(line, DefaultMaxLineBytes)
			if err != nil || pl.kind != lineGroup {
				fail(0, group, "group %q cannot be bound: %v; the group binding is ignored", group, err)
				continue
			}
			b.addLink(pl.tokens[1], pl.tokens[2], location{source: source, text: pl.text})
		}
	}
	if pol := b.build(); pol.NumRules() > 0 || pol.NumLinks() > 0 {
		entry.policy = pol
	}
	return entry
}

// validateRolePolicy applies the upstream AppProject policy validation to a tokenized
// role policy line. Field values are compared with surrounding spaces trimmed, as
// upstream does.
func validateRolePolicy(pl parsedLine, subject string, objectRegexp *regexp.Regexp) error {
	if pl.kind != linePolicy {
		return fmt.Errorf("invalid policy rule %q: must be of the form 'p, sub, res, act, obj, eft'", pl.text)
	}
	field := func(i int) string { return strings.TrimSpace(pl.tokens[i]) }
	if field(1) != subject {
		return fmt.Errorf("invalid policy rule %q: policy subject must be %q, not %q", pl.text, subject, field(1))
	}
	if !IsProjectScoped(field(2)) {
		return fmt.Errorf("invalid policy rule %q: project resource must be one of 'applications', 'applicationsets', 'repositories', 'exec', 'logs' or 'clusters', not %q", pl.text, field(2))
	}
	if !isValidRoleAction(field(3)) {
		return fmt.Errorf("invalid policy rule %q: invalid action %q", pl.text, field(3))
	}
	if !objectRegexp.MatchString(field(4)) {
		return fmt.Errorf("invalid policy rule %q: object must be '<project>/*', '<project>[/<namespace>]/<name>', not %q", pl.text, field(4))
	}
	if e := field(5); e != EffectAllow && e != EffectDeny {
		return fmt.Errorf("invalid policy rule %q: effect must be 'allow' or 'deny'", pl.text)
	}
	return nil
}

// isValidRoleAction mirrors upstream: the plain verbs get/create/update/delete/sync/
// override, "*", or any action containing "action/", "update/" or "delete/" (upstream
// uses unanchored regular expressions for those).
func isValidRoleAction(action string) bool {
	if validRoleActions[action] {
		return true
	}
	for _, s := range []string{ActionAction + "/", ActionUpdate + "/", ActionDelete + "/"} {
		if strings.Contains(action, s) {
			return true
		}
	}
	return false
}

// validateGroupName mirrors the upstream group name rules for project roles.
func validateGroupName(name string) error {
	runes := []rune(name)
	trimmed := strings.TrimSpace(name)
	if len(trimmed) > 1 && strings.HasPrefix(trimmed, `"`) && strings.HasSuffix(trimmed, `"`) {
		trimmed = trimmed[1 : len(trimmed)-1]
	} else if strings.Contains(trimmed, ",") {
		return fmt.Errorf("group %q must be quoted", name)
	}
	if trimmed == "" {
		return fmt.Errorf("group %q is empty", name)
	}
	if groupInvalidChars.MatchString(trimmed) {
		return fmt.Errorf("group %q contains invalid characters", name)
	}
	if len(runes) > 1 && unicode.IsSpace(runes[0]) {
		return fmt.Errorf("group %q contains a leading space", name)
	}
	if len(runes) > 1 && unicode.IsSpace(runes[len(runes)-1]) {
		return fmt.Errorf("group %q contains a trailing space", name)
	}
	return nil
}
