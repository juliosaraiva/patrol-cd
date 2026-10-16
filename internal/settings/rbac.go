package settings

import (
	"fmt"
	"maps"
	"slices"

	"github.com/juliosaraiva/patrol-cd/internal/rbac"
)

func defaultRBAC() RBAC {
	return RBAC{MatchMode: rbac.GlobMatchMode, Scopes: rbac.DefaultScopes()}
}

// parseRBAC reads patrol-rbac-cm. The policy documents are passed through unchanged even
// when invalid, so the enforcer fails closed (TEN-078); policy.default, policy.matchMode and
// scopes use per-key isolation.
func (p *parser) parseRBAC() {
	src := SourceRBACConfigMap
	data := p.cmData(src)
	r := defaultRBAC()
	if data == nil {
		p.s.rbac = r
		return
	}
	r.Present = true
	r.Data = maps.Clone(data)
	r.PolicyCSV = rbac.PolicyCSV(data)
	r.Sources = rbac.PolicySources(data)
	handled := map[string]bool{}
	for _, s := range r.Sources {
		handled[s.Name] = true
		p.accept(src, s.Name, data[s.Name])
	}

	if v, ok := data[RBACKeyPolicyDefault]; ok {
		handled[RBACKeyPolicyDefault] = true
		r.DefaultRole = v
		p.accept(src, RBACKeyPolicyDefault, v)
	}
	if v, ok := data[RBACKeyMatchMode]; ok {
		handled[RBACKeyMatchMode] = true
		switch v {
		case rbac.GlobMatchMode, rbac.RegexMatchMode:
			r.MatchMode = v
		case "":
		default:
			p.warn(src, RBACKeyMatchMode, "unknown match mode %q is treated as %q", v, rbac.GlobMatchMode)
		}
		p.accept(src, RBACKeyMatchMode, v)
	}
	if v, ok := data[RBACKeyScopes]; ok {
		handled[RBACKeyScopes] = true
		p.isolate(src, RBACKeyScopes, v, func(val string) error {
			scopes, err := rbac.ParseScopes(val)
			if err != nil {
				return fmt.Errorf("error unmarshalling scopes: %w", err)
			}
			r.Scopes = scopes
			return nil
		})
	}

	// Policy validation: errors make the enforcer deny everything; warnings are advisory.
	problems, err := rbac.Validate(r.Sources, rbac.ValidateOptions{MatchMode: r.MatchMode, Builtin: rbac.BuiltinPolicy()})
	for _, pr := range problems {
		if pr.Source == "builtin" {
			continue
		}
		sev := SeverityWarning
		if pr.Severity == rbac.SeverityError {
			sev = SeverityError
		}
		msg := pr.Message
		if pr.Line > 0 {
			msg = fmt.Sprintf("line %d: %s", pr.Line, msg)
		}
		p.addIssue(src, pr.Source, sev, msg)
	}
	if err != nil && !slices.ContainsFunc(problems, func(v rbac.ValidationError) bool { return v.Severity == rbac.SeverityError }) {
		p.errorf(src, RBACKeyPolicyCSV, "%v", err)
	}
	if err != nil {
		p.errorf(src, RBACKeyPolicyCSV, "the RBAC policy does not load; every request is denied until it is fixed")
	}

	for _, k := range slices.Sorted(maps.Keys(data)) {
		if !handled[k] {
			p.unknownKey(src, k)
		}
	}
	p.s.rbac = r
}
