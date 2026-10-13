package rbac

import (
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// isOverlayKey reports whether key is a "policy.<suffix>.csv" overlay key.
func isOverlayKey(key string) bool {
	return key != ConfigMapPolicyCSVKey && strings.HasPrefix(key, "policy.") && strings.HasSuffix(key, ".csv")
}

func overlayKeys(data map[string]string) []string {
	keys := make([]string, 0, len(data))
	for k := range data {
		if isOverlayKey(k) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// PolicyCSV composes the user policy from RBAC ConfigMap data exactly like upstream:
// policy.csv first, then every "policy.<suffix>.csv" value in ascending key order, each
// preceded by a newline.
func PolicyCSV(data map[string]string) string {
	var b strings.Builder
	if p, ok := data[ConfigMapPolicyCSVKey]; ok {
		b.WriteString(p)
	}
	for _, k := range overlayKeys(data) {
		b.WriteString("\n")
		b.WriteString(data[k])
	}
	return b.String()
}

// PolicySources returns the same composition as PolicyCSV as named sources, so problems
// are reported per ConfigMap key and line.
func PolicySources(data map[string]string) []Source {
	var out []Source
	if p, ok := data[ConfigMapPolicyCSVKey]; ok {
		out = append(out, Source{Name: ConfigMapPolicyCSVKey, CSV: p})
	}
	for _, k := range overlayKeys(data) {
		out = append(out, Source{Name: k, CSV: data[k]})
	}
	return out
}

// ParseScopes parses the "scopes" value of the RBAC ConfigMap, a YAML list such as
// "[groups]" or "[groups, email]". An empty value yields DefaultScopes. As documented
// upstream, a single scalar ("groups") is accepted as a one-element list. An explicit
// empty list ("[]") yields no scopes, so no group claim is evaluated.
func ParseScopes(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return DefaultScopes(), nil
	}
	var list []string
	if err := yaml.Unmarshal([]byte(value), &list); err != nil {
		var single string
		if err2 := yaml.Unmarshal([]byte(value), &single); err2 != nil || single == "" {
			return nil, fmt.Errorf("parse %s %q: %w", ConfigMapScopesKey, value, err)
		}
		return []string{single}, nil
	}
	if list == nil {
		return DefaultScopes(), nil
	}
	return list, nil
}

// ScopeValues collects the string values of the given claims, in scope order. A claim
// may be a string or a list of strings; other values are ignored.
func ScopeValues(claims map[string]any, scopes []string) []string {
	values := make([]string, 0)
	for _, scope := range scopes {
		switch v := claims[scope].(type) {
		case string:
			values = append(values, v)
		case []string:
			values = append(values, v...)
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					values = append(values, s)
				}
			}
		}
	}
	return values
}

// UserIdentifier returns federated_claims.user_id when present and non-empty (Dex), and
// the "sub" claim otherwise.
func UserIdentifier(claims map[string]any) string {
	sub, _ := claims["sub"].(string)
	federated, ok := claims["federated_claims"].(map[string]any)
	if !ok {
		return sub
	}
	if id, ok := federated["user_id"].(string); ok && id != "" {
		return id
	}
	return sub
}

// SubjectsFromClaims builds the Subjects of a user session from token claims: the user
// identifier and the values of the scope claims as groups. It never sets ProjectRole:
// the auth layer sets it only after verifying a project-role token (see
// ParseProjectSubject), so an SSO subject cannot impersonate a project role.
func SubjectsFromClaims(claims map[string]any, scopes []string) Subjects {
	return Subjects{User: UserIdentifier(claims), Groups: ScopeValues(claims, scopes)}
}
