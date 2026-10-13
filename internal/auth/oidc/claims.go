package oidc

import (
	"fmt"
	"strings"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/auth"
)

// Claims are the verified identity claims of an OIDC login.
type Claims struct {
	Issuer string
	Sub    string
	Email  string
	Name   string
	Groups []string
	Expiry time.Time
	// Raw holds every claim of the ID token (with "groups" replaced by userinfo groups
	// when EnableUserInfoGroups is set).
	Raw map[string]any
}

// UserID returns the stable user identifier: Dex's federated_claims.user_id when present,
// otherwise "sub" (upstream GetUserIdentifier semantics).
func (c Claims) UserID() string {
	if fc, ok := c.Raw["federated_claims"].(map[string]any); ok {
		if id, ok := fc["user_id"].(string); ok && id != "" {
			return id
		}
	}
	return c.Sub
}

// SSOIdentity converts the claims to the input of auth.TokenService.IssueSSOSession.
// extraClaims names additional raw claims to carry into the session (e.g. the claims
// referenced by RBAC scopes); reserved names are skipped.
func (c Claims) SSOIdentity(extraClaims ...string) auth.SSOIdentity {
	id := auth.SSOIdentity{Subject: c.UserID(), Email: c.Email, Name: c.Name, Groups: c.Groups}
	for _, name := range extraClaims {
		switch name {
		case "", "iss", "sub", "aud", "exp", "nbf", "iat", "jti", "kind", "groups", "email", "name":
			continue
		}
		if v, ok := c.Raw[name]; ok {
			if id.Extra == nil {
				id.Extra = map[string]any{}
			}
			id.Extra[name] = v
		}
	}
	return id
}

// ExtractGroups returns the group names held by claim in raw. The claim may be a string
// or an array (non-string elements are ignored). A dotted claim name that is not present
// at the top level is resolved through nested objects. Duplicates are removed, order is
// preserved. More than auth.MaxGroups groups is an error.
func ExtractGroups(raw map[string]any, claim string) ([]string, error) {
	if claim == "" {
		claim = DefaultGroupsClaim
	}
	v, ok := raw[claim]
	if !ok && strings.Contains(claim, ".") {
		v, ok = nested(raw, strings.Split(claim, "."))
	}
	if !ok || v == nil {
		return nil, nil
	}
	var in []string
	switch t := v.(type) {
	case string:
		in = []string{t}
	case []string:
		in = t
	case []any:
		in = make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				in = append(in, s)
			}
		}
	default:
		return nil, nil
	}
	if len(in) > auth.MaxGroups {
		return nil, fmt.Errorf("%w: %d > %d", ErrTooManyGroups, len(in), auth.MaxGroups)
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, g := range in {
		if g == "" {
			continue
		}
		if _, dup := seen[g]; dup {
			continue
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}
	return out, nil
}

func nested(m map[string]any, path []string) (any, bool) {
	var cur any = m
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = obj[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func claimsFromRaw(raw map[string]any, groupsClaim string) (Claims, error) {
	c := Claims{Raw: raw}
	c.Issuer, _ = raw["iss"].(string)
	c.Sub, _ = raw["sub"].(string)
	c.Email, _ = raw["email"].(string)
	c.Name, _ = raw["name"].(string)
	if exp, ok := raw["exp"].(float64); ok {
		c.Expiry = time.Unix(int64(exp), 0)
	}
	groups, err := ExtractGroups(raw, groupsClaim)
	if err != nil {
		return Claims{}, err
	}
	c.Groups = groups
	return c, nil
}
