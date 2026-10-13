package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// FetchUserInfo queries the userinfo endpoint with accessToken and returns its claims.
// The response may be JSON or a signed JWT ("application/jwt", verified like an ID
// token except for expiry). The "sub" of the response must equal expectedSub
// (OIDC Core §5.3.2).
func (p *Provider) FetchUserInfo(ctx context.Context, accessToken, expectedSub string) (map[string]any, error) {
	endpoint, err := p.userInfoURL(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUserInfo, err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json, application/jwt")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUserInfo, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %w", ErrUserInfo, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: unexpected status %d", ErrUserInfo, resp.StatusCode)
	}
	claims := map[string]any{}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt == "application/jwt" {
		idt, err := p.verifySigned(ctx, strings.TrimSpace(string(body)), true)
		if err != nil {
			return nil, fmt.Errorf("%w: signed response: %w", ErrUserInfo, err)
		}
		if err := idt.Claims(&claims); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUserInfo, err)
		}
	} else if err := json.Unmarshal(body, &claims); err != nil {
		return nil, fmt.Errorf("%w: decode response: %w", ErrUserInfo, err)
	}
	if sub, _ := claims["sub"].(string); sub == "" || sub != expectedSub {
		return nil, fmt.Errorf("%w: subject does not match the ID token", ErrUserInfo)
	}
	return claims, nil
}

func (p *Provider) userInfoURL(ctx context.Context) (string, error) {
	if p.cfg.UserInfoPath != "" {
		base := p.cfg.UserInfoBaseURL
		if base == "" {
			base = p.cfg.Issuer
		}
		return strings.TrimRight(base, "/") + p.cfg.UserInfoPath, nil
	}
	prov, err := p.provider(ctx)
	if err != nil {
		return "", err
	}
	if ep := prov.UserInfoEndpoint(); ep != "" {
		return ep, nil
	}
	return "", fmt.Errorf("%w: provider advertises no userinfo endpoint and userInfoPath is unset", ErrUserInfo)
}

// fetchUserInfoGroups fetches groups from userinfo (bypassing the cache, so a new login
// sees current memberships) and caches them for VerifyExternal.
func (p *Provider) fetchUserInfoGroups(ctx context.Context, accessToken, sub string, tokenExpiry time.Time) ([]string, error) {
	claims, err := p.FetchUserInfo(ctx, accessToken, sub)
	if err != nil {
		return nil, err
	}
	groups, err := ExtractGroups(claims, p.cfg.groupsClaim())
	if err != nil {
		return nil, err
	}
	exp := tokenExpiry
	if ttl := p.cfg.UserInfoCacheExpiration; ttl > 0 && (exp.IsZero() || p.now().Add(ttl).Before(exp)) {
		exp = p.now().Add(ttl)
	}
	p.userInfo.put(sub, groups, exp)
	return groups, nil
}

// userInfoCache is a bounded map of subject -> groups with per-entry expiry.
type userInfoCache struct {
	mu  sync.Mutex
	m   map[string]userInfoEntry
	max int
	now func() time.Time
}

type userInfoEntry struct {
	groups []string
	exp    time.Time
}

func newUserInfoCache(maxEntries int, now func() time.Time) *userInfoCache {
	return &userInfoCache{m: map[string]userInfoEntry{}, max: maxEntries, now: now}
}

func (c *userInfoCache) get(sub string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[sub]
	if !ok {
		return nil, false
	}
	if !c.now().Before(e.exp) {
		delete(c.m, sub)
		return nil, false
	}
	return slices.Clone(e.groups), true
}

func (c *userInfoCache) put(sub string, groups []string, exp time.Time) {
	if exp.IsZero() || !c.now().Before(exp) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[sub]; !ok && len(c.m) >= c.max {
		now := c.now()
		for k, e := range c.m {
			if !now.Before(e.exp) {
				delete(c.m, k)
			}
		}
		for k := range c.m {
			if len(c.m) < c.max {
				break
			}
			delete(c.m, k)
		}
	}
	c.m[sub] = userInfoEntry{groups: slices.Clone(groups), exp: exp}
}
