package project

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
)

// RoleSubject returns the RBAC subject of a project role, "proj:<project>:<role>".
func RoleSubject(project, role string) string {
	return fmt.Sprintf("proj:%s:%s", project, role)
}

// GetRoleByName returns a copy of the role named name and its index in spec.roles.
func GetRoleByName(proj *v1alpha1.AppProject, name string) (*v1alpha1.ProjectRole, int, error) {
	if proj == nil {
		return nil, -1, fmt.Errorf("role '%s' does not exist: no project", name)
	}
	for i := range proj.Spec.Roles {
		if proj.Spec.Roles[i].Name == name {
			role := proj.Spec.Roles[i]
			return &role, i, nil
		}
	}
	return nil, -1, fmt.Errorf("role '%s' does not exist in project '%s'", name, proj.Name)
}

// RoleNotFoundMessage is the NotFound message of token operations on an unknown role.
func RoleNotFoundMessage(project, role string) string {
	return fmt.Sprintf("project '%s' does not have role '%s'", project, role)
}

// ProjectPoliciesString returns the runtime policy of the project roles (TEN-020): per role,
// the implicit "p, proj:<p>:<r>, projects, get, <p>, allow", the role policies verbatim and a
// "g, <group>, proj:<p>:<r>" line per group, joined by newlines.
func ProjectPoliciesString(proj *v1alpha1.AppProject) string {
	if proj == nil {
		return ""
	}
	var lines []string
	for _, role := range proj.Spec.Roles {
		subject := RoleSubject(proj.Name, role.Name)
		lines = append(lines, fmt.Sprintf("p, %s, %s, %s, %s, allow", subject, rbac.ResourceProjects, rbac.ActionGet, proj.Name))
		lines = append(lines, role.Policies...)
		for _, g := range role.Groups {
			lines = append(lines, fmt.Sprintf("g, %s, %s", g, subject))
		}
	}
	return strings.Join(lines, "\n")
}

// RBACRoles adapts spec.roles for rbac.Enforcer.SetProjectPolicies.
func RBACRoles(proj *v1alpha1.AppProject) []rbac.Role {
	if proj == nil {
		return nil
	}
	out := make([]rbac.Role, 0, len(proj.Spec.Roles))
	for _, r := range proj.Spec.Roles {
		out = append(out, rbac.Role{Name: r.Name, Policies: slices.Clone(r.Policies), Groups: slices.Clone(r.Groups)})
	}
	return out
}

// NormalizePolicy rewrites a policy line to "a, b, c, ..." (fields after the first trimmed of
// spaces, joined by ", "), as the API server stores them.
func NormalizePolicy(policy string) string {
	parts := strings.Split(policy, ",")
	out := parts[0]
	for _, p := range parts[1:] {
		out += ", " + strings.Trim(p, " ")
	}
	return out
}

// NormalizePolicies normalizes every role policy of proj in place.
func NormalizePolicies(proj *v1alpha1.AppProject) {
	if proj == nil {
		return
	}
	for i := range proj.Spec.Roles {
		if proj.Spec.Roles[i].Policies == nil {
			continue
		}
		normalized := make([]string, 0, len(proj.Spec.Roles[i].Policies))
		for _, p := range proj.Spec.Roles[i].Policies {
			normalized = append(normalized, NormalizePolicy(p))
		}
		proj.Spec.Roles[i].Policies = normalized
	}
}

// AddGroupToRole adds an SSO group to a role. It reports false when already present.
func AddGroupToRole(proj *v1alpha1.AppProject, role, group string) (bool, error) {
	_, i, err := GetRoleByName(proj, role)
	if err != nil {
		return false, err
	}
	if slices.Contains(proj.Spec.Roles[i].Groups, group) {
		return false, nil
	}
	proj.Spec.Roles[i].Groups = append(proj.Spec.Roles[i].Groups, group)
	return true, nil
}

// RemoveGroupFromRole removes an SSO group from a role. It reports false when absent.
func RemoveGroupFromRole(proj *v1alpha1.AppProject, role, group string) (bool, error) {
	_, i, err := GetRoleByName(proj, role)
	if err != nil {
		return false, err
	}
	idx := slices.Index(proj.Spec.Roles[i].Groups, group)
	if idx < 0 {
		return false, nil
	}
	proj.Spec.Roles[i].Groups = slices.Delete(proj.Spec.Roles[i].Groups, idx, idx+1)
	return true, nil
}

// ValidateJWTTokenID rejects a token id already used by a token of the role (checked against
// the legacy spec.roles[].jwtTokens, as upstream). An empty id is always accepted.
func ValidateJWTTokenID(proj *v1alpha1.AppProject, role, id string) error {
	r, _, err := GetRoleByName(proj, role)
	if err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	for _, t := range legacyTokens(r) {
		if t.ID == id {
			return invalidf(CodeInvalidArgument, "Token id '%s' has been used. ", id)
		}
	}
	return nil
}

// legacyTokens returns spec.roles[].jwtTokens, the deprecated token location still read
// and kept in sync for migration (TEN-034).
func legacyTokens(r *v1alpha1.ProjectRole) []v1alpha1.JWTToken {
	return r.JWTTokens //nolint:staticcheck // deprecated field, read for migration (TEN-034)
}

func setLegacyTokens(r *v1alpha1.ProjectRole, tokens []v1alpha1.JWTToken) {
	r.JWTTokens = tokens //nolint:staticcheck // deprecated field, kept in sync for migration (TEN-034)
}

// findToken locates a token by id when id is set, and by issued-at time only when id is
// empty (legacy tokens without a jti, and the CLI's delete-token, which passes only the iat;
// issuedAt -1 disables the iat lookup).
//
// Deliberate security difference from upstream, which falls back to the iat whenever the id
// is not found: there, a revoked token keeps authenticating while another token of the role
// has the same iat second, and deleting by an unknown id removes another token. Tokens issued
// with a jti always have their id recorded (TEN-032), and NormalizeJWTTokens gives legacy
// metadata id = strconv(iat), so the fallback is only needed when the caller has no id.
func findToken(tokens []v1alpha1.JWTToken, issuedAt int64, id string) int {
	if id != "" {
		return slices.IndexFunc(tokens, func(t v1alpha1.JWTToken) bool { return t.ID == id })
	}
	if issuedAt != -1 {
		return slices.IndexFunc(tokens, func(t v1alpha1.JWTToken) bool { return t.IssuedAt == issuedAt })
	}
	return -1
}

func tokenNotFound(project, role string, issuedAt int64) error {
	return fmt.Errorf("JWT token for role '%s' issued at '%d' does not exist in project '%s'", role, issuedAt, project)
}

// GetJWTToken finds a token of role in status.jwtTokensByRole: by id (the token's jti) when
// id is set, otherwise by issued-at time (pass -1 to disable it). This is the lookup token
// authentication uses (TEN-035). Unlike upstream there is no issued-at fallback for a token
// with an id, so a revoked token stops authenticating even when another token of the role
// was issued in the same second (see findToken).
func GetJWTToken(proj *v1alpha1.AppProject, role string, issuedAt int64, id string) (*v1alpha1.JWTToken, int, error) {
	if proj == nil {
		return nil, -1, tokenNotFound("", role, issuedAt)
	}
	items := proj.Status.JWTTokensByRole[role].Items
	if i := findToken(items, issuedAt, id); i >= 0 {
		t := items[i]
		return &t, i, nil
	}
	return nil, -1, tokenNotFound(proj.Name, role, issuedAt)
}

// GetJWTTokenFromSpec is GetJWTToken over the legacy spec.roles[].jwtTokens.
func GetJWTTokenFromSpec(proj *v1alpha1.AppProject, role string, issuedAt int64, id string) (*v1alpha1.JWTToken, int, error) {
	r, _, err := GetRoleByName(proj, role)
	if err != nil {
		return nil, -1, err
	}
	tokens := legacyTokens(r)
	if i := findToken(tokens, issuedAt, id); i >= 0 {
		t := tokens[i]
		return &t, i, nil
	}
	return nil, -1, tokenNotFound(proj.Name, r.Name, issuedAt)
}

// AddJWTToken records the metadata of a token issued for role (TEN-032): tokens are
// normalized, the token is appended to status.jwtTokensByRole[role], and spec and status are
// normalized again so both hold the merged list. The role must exist.
func AddJWTToken(proj *v1alpha1.AppProject, role string, token v1alpha1.JWTToken) error {
	if _, _, err := GetRoleByName(proj, role); err != nil {
		name := ""
		if proj != nil {
			name = proj.Name
		}
		return &ValidationError{Code: CodeNotFound, Message: RoleNotFoundMessage(name, role)}
	}
	NormalizeJWTTokens(proj)
	if proj.Status.JWTTokensByRole == nil {
		proj.Status.JWTTokensByRole = map[string]v1alpha1.JWTTokens{}
	}
	entry := proj.Status.JWTTokensByRole[role]
	entry.Items = append(slices.Clone(entry.Items), token)
	proj.Status.JWTTokensByRole[role] = entry
	NormalizeJWTTokens(proj)
	return nil
}

// RemoveJWTToken deletes the metadata of a token of role, located by id when id is set and
// otherwise by issued-at time (see findToken), from both spec.roles[].jwtTokens and
// status.jwtTokensByRole (TEN-033).
// Deleting the metadata revokes the token. It reports whether a token was found; an unknown
// role or token is not an error for the API (it answers success without updating).
func RemoveJWTToken(proj *v1alpha1.AppProject, role string, issuedAt int64, id string) (bool, error) {
	_, roleIdx, err := GetRoleByName(proj, role)
	if err != nil {
		return false, err
	}
	found := false
	specTokens := legacyTokens(&proj.Spec.Roles[roleIdx])
	if i := findToken(specTokens, issuedAt, id); i >= 0 {
		setLegacyTokens(&proj.Spec.Roles[roleIdx], slices.Delete(slices.Clone(specTokens), i, i+1))
		found = true
	}
	if entry, ok := proj.Status.JWTTokensByRole[role]; ok {
		if i := findToken(entry.Items, issuedAt, id); i >= 0 {
			entry.Items = slices.Delete(slices.Clone(entry.Items), i, i+1)
			proj.Status.JWTTokensByRole[role] = entry
			found = true
		}
	}
	if !found {
		return false, tokenNotFound(proj.Name, role, issuedAt)
	}
	return true, nil
}

// NormalizeJWTTokens migrates and reconciles token metadata (TEN-034): tokens without an id
// get id = strconv(iat); spec.roles[].jwtTokens and status.jwtTokensByRole are merged by id,
// sorted by iat descending (then id) and written to both; status entries for roles that no
// longer exist are dropped (revoking their tokens). It reports whether anything changed.
func NormalizeJWTTokens(proj *v1alpha1.AppProject) bool {
	if proj == nil {
		return false
	}
	changed := false
	for i := range proj.Spec.Roles {
		tokens := legacyTokens(&proj.Spec.Roles[i])
		for j, t := range tokens {
			if t.ID == "" {
				tokens[j].ID = strconv.FormatInt(t.IssuedAt, 10)
				changed = true
			}
		}
	}
	for role, entry := range proj.Status.JWTTokensByRole {
		for j, t := range entry.Items {
			if t.ID == "" {
				entry.Items[j].ID = strconv.FormatInt(t.IssuedAt, 10)
				changed = true
			}
		}
		proj.Status.JWTTokensByRole[role] = entry
	}

	existing := make(map[string]bool, len(proj.Spec.Roles))
	for i := range proj.Spec.Roles {
		role := &proj.Spec.Roles[i]
		existing[role.Name] = true
		if proj.Status.JWTTokensByRole == nil {
			proj.Status.JWTTokensByRole = map[string]v1alpha1.JWTTokens{}
		}
		statusTokens := proj.Status.JWTTokensByRole[role.Name].Items
		specTokens := legacyTokens(role)
		merged := mergeTokens(statusTokens, specTokens)
		if !tokensEqual(merged, specTokens) || !tokensEqual(merged, statusTokens) {
			changed = true
		}
		setLegacyTokens(role, merged)
		proj.Status.JWTTokensByRole[role.Name] = v1alpha1.JWTTokens{Items: slices.Clone(merged)}
	}
	for role := range proj.Status.JWTTokensByRole {
		if !existing[role] {
			delete(proj.Status.JWTTokensByRole, role)
			changed = true
		}
	}
	return changed
}

// mergeTokens merges two lists by id; on an id collision the entry from b wins (upstream:
// spec entries override status entries).
func mergeTokens(a, b []v1alpha1.JWTToken) []v1alpha1.JWTToken {
	byID := make(map[string]v1alpha1.JWTToken, len(a)+len(b))
	for _, t := range a {
		byID[t.ID] = t
	}
	for _, t := range b {
		byID[t.ID] = t
	}
	if len(byID) == 0 {
		return nil
	}
	out := make([]v1alpha1.JWTToken, 0, len(byID))
	for _, t := range byID {
		out = append(out, t)
	}
	sortTokens(out)
	return out
}

func sortTokens(tokens []v1alpha1.JWTToken) {
	slices.SortFunc(tokens, func(x, y v1alpha1.JWTToken) int {
		return cmp.Or(cmp.Compare(y.IssuedAt, x.IssuedAt), strings.Compare(x.ID, y.ID))
	})
}

// tokensEqual compares two token lists, treating nil and empty alike and ignoring order among
// tokens with the same iat and id ordering rules (lists are compared after sorting).
func tokensEqual(a, b []v1alpha1.JWTToken) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := slices.Clone(a), slices.Clone(b)
	sortTokens(as)
	sortTokens(bs)
	return slices.Equal(as, bs)
}
