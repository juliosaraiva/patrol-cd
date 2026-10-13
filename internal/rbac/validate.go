package rbac

import "fmt"

// ValidateOptions configures Validate.
type ValidateOptions struct {
	// MatchMode is the policy.matchMode value ("glob" or "regex").
	MatchMode string
	// Builtin, when non-empty, is parsed before the sources (as source "builtin") so that
	// references to built-in roles are not reported as dangling.
	Builtin string
}

// Validate checks a policy for "patrol admin settings rbac validate". It returns every
// problem: syntax errors (which make the policy unloadable; err is then a *PolicyError
// matching ErrInvalidPolicy) and warnings, including roles that are assigned by 'g'
// lines but grant nothing, directly or through inherited roles.
func Validate(sources []Source, opts ValidateOptions) ([]ValidationError, error) {
	all := sources
	if opts.Builtin != "" {
		all = append([]Source{{Name: "builtin", CSV: opts.Builtin}}, sources...)
	}
	pol, problems, err := ParseSources(all, ParseOptions{MatchMode: opts.MatchMode})
	if err != nil {
		return problems, err
	}
	for _, role := range pol.roles {
		if roleHasPermissions(pol, role) {
			continue
		}
		loc := pol.roleLoc[role]
		problems = append(problems, ValidationError{
			Source:   loc.source,
			Line:     loc.line,
			Text:     loc.text,
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("role %q is assigned but no policy grants it (or its inherited roles) any permission", role),
		})
	}
	return problems, nil
}

// roleHasPermissions reports whether role or a role it inherits (within the hierarchy
// limit) is the subject of at least one 'p' rule.
func roleHasPermissions(p *Policy, role string) bool {
	seen := visited{}
	seen.add(role)
	frontier := []string{role}
	for level := 0; ; level++ {
		for _, name := range frontier {
			if _, ok := p.subjects[name]; ok {
				return true
			}
		}
		if level == maxHierarchyLevel {
			return false
		}
		var next []string
		for _, name := range frontier {
			for _, r := range p.links[name] {
				if seen.add(r) {
					next = append(next, r)
				}
			}
		}
		if len(next) == 0 {
			return false
		}
		frontier = next
	}
}
