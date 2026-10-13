package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"pgregory.net/rapid"
)

// tokenEnv wires a TokenService to mutable settings maps, as a settings watcher would.
type tokenEnv struct {
	t         testing.TB
	clock     *fakeClock
	mu        sync.Mutex
	cm        map[string]string
	secret    map[string]string
	projects  map[string]bool // "<sub>|<jti>"
	lookupErr error
	revs      *MemoryRevocationStore
	svc       *TokenService
	keys      KeySet
}

func newTokenEnv(t testing.TB) *tokenEnv {
	t.Helper()
	e := &tokenEnv{
		t:     t,
		clock: newFakeClock(),
		cm: map[string]string{
			"accounts.alice": "apiKey, login",
			"accounts.bob":   "login",
			"accounts.ci":    "apiKey",
		},
		secret:   map[string]string{},
		projects: map[string]bool{},
	}
	e.revs = NewMemoryRevocationStore(0, e.clock.Now)
	ks, err := NewKeySet(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	e.keys = ks
	e.svc, err = NewTokenService(TokenServiceConfig{
		Keys:        ks,
		Accounts:    AccountLookupFunc(e.lookup),
		Revocations: e.revs,
		Projects:    TokenRegistryFunc(e.isKnown),
		Now:         e.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *tokenEnv) lookup(_ context.Context, name string) (Account, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lookupErr != nil {
		return Account{}, e.lookupErr
	}
	as, _ := AccountsFromMaps(e.cm, e.secret)
	return as.Get(name)
}

func (e *tokenEnv) isKnown(_ context.Context, sub, jti string, _ int64) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.projects[sub+"|"+jti], nil
}

func (e *tokenEnv) account(name string) Account {
	e.t.Helper()
	a, err := e.lookup(context.Background(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *tokenEnv) update(fn func(cm, secret map[string]string) (map[string]string, map[string]string)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cm, e.secret = fn(maps.Clone(e.cm), maps.Clone(e.secret))
}

func wantInvalid(t *testing.T, err error, also ...error) {
	t.Helper()
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
	for _, a := range also {
		if !errors.Is(err, a) {
			t.Fatalf("err = %v, want also %v", err, a)
		}
	}
}

func TestLoginSessionRoundTrip(t *testing.T) {
	t.Parallel()
	e := newTokenEnv(t)
	ctx := context.Background()
	iss, err := e.svc.IssueLoginSession(ctx, e.account("alice"), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if iss.ID == "" || !iss.ExpiresAt.Equal(iss.IssuedAt.Add(2*time.Hour)) {
		t.Fatalf("issued = %+v", iss)
	}
	// Header and claims layout.
	tok, _, err := jwt.NewParser().ParseUnverified(iss.Token, jwt.MapClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Header["alg"] != "HS256" || tok.Header["kid"] != e.keys.Current.ID() {
		t.Fatalf("header = %v", tok.Header)
	}
	c := tok.Claims.(jwt.MapClaims)
	for k, want := range map[string]any{"iss": "patrol", "sub": "alice:login", "kind": "login", "jti": iss.ID} {
		if c[k] != want {
			t.Fatalf("claim %s = %v, want %v", k, c[k], want)
		}
	}
	for _, k := range []string{"iat", "nbf", "exp"} {
		if _, ok := c[k]; !ok {
			t.Fatalf("claim %s missing", k)
		}
	}

	p, err := e.svc.Verify(ctx, iss.Token)
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "alice" || p.Kind != KindLogin || p.TokenID != iss.ID || p.Issuer != Issuer ||
		!p.ExpiresAt.Equal(iss.ExpiresAt) || !p.IssuedAt.Equal(iss.IssuedAt) || p.Claims["sub"] != "alice" {
		t.Fatalf("principal = %+v", p)
	}
	if p.Username() != "alice" {
		t.Fatalf("username = %q", p.Username())
	}
	// Default TTL.
	iss2, err := e.svc.IssueLoginSession(ctx, e.account("alice"), 0)
	if err != nil || iss2.ExpiresAt.Sub(iss2.IssuedAt) != DefaultSessionDuration {
		t.Fatalf("default ttl: %v %v", iss2, err)
	}
}

func TestAccountTokenLifecycle(t *testing.T) {
	t.Parallel()
	e := newTokenEnv(t)
	ctx := context.Background()
	alice := e.account("alice")

	// No expiry.
	iss, meta, err := e.svc.IssueAccountToken(ctx, alice, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != iss.ID || meta.ExpiresAt != 0 || !iss.ExpiresAt.IsZero() || meta.IssuedAt != iss.IssuedAt.Unix() {
		t.Fatalf("meta = %+v issued = %+v", meta, iss)
	}
	// Not yet persisted: rejected.
	_, err = e.svc.Verify(ctx, iss.Token)
	wantInvalid(t, err, ErrTokenRevoked)

	e.update(func(cm, s map[string]string) (map[string]string, map[string]string) {
		s, err := AddToken(s, alice, meta)
		if err != nil {
			t.Fatal(err)
		}
		return cm, s
	})
	p, err := e.svc.Verify(ctx, iss.Token)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindAPIKey || p.Subject != "alice" || !p.ExpiresAt.IsZero() {
		t.Fatalf("principal = %+v", p)
	}
	// Tokens without expiry cannot be put in the revocation store.
	if err := e.svc.Revoke(ctx, p); !errors.Is(err, ErrNotRevocable) {
		t.Fatalf("revoke: %v", err)
	}
	// Deleting the metadata revokes the token.
	e.update(func(cm, s map[string]string) (map[string]string, map[string]string) {
		s, err := DeleteToken(s, alice, meta.ID)
		if err != nil {
			t.Fatal(err)
		}
		return cm, s
	})
	_, err = e.svc.Verify(ctx, iss.Token)
	wantInvalid(t, err, ErrTokenRevoked)

	// With expiry and explicit id.
	iss2, meta2, err := e.svc.IssueAccountToken(ctx, e.account("alice"), "deploy-bot", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if meta2.ID != "deploy-bot" || meta2.ExpiresAt != iss2.ExpiresAt.Unix() || iss2.ExpiresAt.Sub(iss2.IssuedAt) != time.Hour {
		t.Fatalf("meta2 = %+v", meta2)
	}
	e.update(func(cm, s map[string]string) (map[string]string, map[string]string) {
		s, _ = AddToken(s, alice, meta2)
		return cm, s
	})
	if _, _, err := e.svc.IssueAccountToken(ctx, e.account("alice"), "deploy-bot", 0); !errors.Is(err, ErrTokenExists) {
		t.Fatalf("duplicate id: %v", err)
	}
	if _, err := e.svc.Verify(ctx, iss2.Token); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Hour + time.Minute)
	_, err = e.svc.Verify(ctx, iss2.Token)
	wantInvalid(t, err, ErrTokenExpired)
}

func TestCapabilityEnforcement(t *testing.T) {
	t.Parallel()
	e := newTokenEnv(t)
	ctx := context.Background()

	// bob: login only -> cannot mint API tokens. ci: apiKey only -> cannot log in.
	if _, _, err := e.svc.IssueAccountToken(ctx, e.account("bob"), "", 0); !errors.Is(err, ErrMissingCapability) {
		t.Fatalf("bob mint: %v", err)
	}
	if _, err := e.svc.IssueLoginSession(ctx, e.account("ci"), 0); !errors.Is(err, ErrMissingCapability) {
		t.Fatalf("ci login: %v", err)
	}
	// admin has only login by default.
	if _, _, err := e.svc.IssueAccountToken(ctx, e.account("admin"), "", 0); !errors.Is(err, ErrMissingCapability) {
		t.Fatalf("admin mint: %v", err)
	}

	// Sessions stop verifying when the account is disabled or loses the capability.
	bobSess, err := e.svc.IssueLoginSession(ctx, e.account("bob"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	aliceSess, err := e.svc.IssueLoginSession(ctx, e.account("alice"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	e.update(func(cm, s map[string]string) (map[string]string, map[string]string) {
		cm["accounts.bob.enabled"] = "false"
		cm["accounts.alice"] = "apiKey"
		return cm, s
	})
	_, err = e.svc.Verify(ctx, bobSess.Token)
	wantInvalid(t, err, ErrAccountDisabled)
	_, err = e.svc.Verify(ctx, aliceSess.Token)
	wantInvalid(t, err, ErrMissingCapability)
	if _, err := e.svc.IssueLoginSession(ctx, e.account("bob"), 0); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("disabled login: %v", err)
	}
	// Deleted account.
	e.update(func(cm, s map[string]string) (map[string]string, map[string]string) {
		delete(cm, "accounts.alice")
		return cm, s
	})
	_, err = e.svc.Verify(ctx, aliceSess.Token)
	wantInvalid(t, err, ErrAccountNotFound)
}

func TestPasswordChangeInvalidatesOlderTokens(t *testing.T) {
	t.Parallel()
	e := newTokenEnv(t)
	ctx := context.Background()
	alice := e.account("alice")
	old, err := e.svc.IssueLoginSession(ctx, alice, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	apiIss, apiMeta, err := e.svc.IssueAccountToken(ctx, alice, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	e.update(func(cm, s map[string]string) (map[string]string, map[string]string) {
		s, _ = AddToken(s, alice, apiMeta)
		return cm, s
	})
	e.clock.Advance(10 * time.Second)
	e.update(func(cm, s map[string]string) (map[string]string, map[string]string) {
		s, err := SecretWithPassword(s, "alice", mustHash(t, "new-password"), e.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		return cm, s
	})
	_, err = e.svc.Verify(ctx, old.Token)
	wantInvalid(t, err, ErrSessionInvalidated)
	// Upstream applies the mtime rule to API tokens as well.
	_, err = e.svc.Verify(ctx, apiIss.Token)
	wantInvalid(t, err, ErrSessionInvalidated)

	fresh, err := e.svc.IssueLoginSession(ctx, e.account("alice"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Verify(ctx, fresh.Token); err != nil {
		t.Fatalf("session issued after change: %v", err)
	}
}

func TestRevocation(t *testing.T) {
	t.Parallel()
	e := newTokenEnv(t)
	ctx := context.Background()
	iss, err := e.svc.IssueLoginSession(ctx, e.account("alice"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := e.svc.IssueLoginSession(ctx, e.account("alice"), time.Hour)
	p, err := e.svc.Verify(ctx, iss.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Revoke(ctx, p); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Verify(ctx, iss.Token)
	wantInvalid(t, err, ErrTokenRevoked)
	if _, err := e.svc.Verify(ctx, other.Token); err != nil {
		t.Fatalf("other session affected: %v", err)
	}
	if err := e.svc.Revoke(ctx, &Principal{}); err == nil {
		t.Fatal("revoking a principal without id succeeded")
	}
}

func TestExpiryAndLeeway(t *testing.T) {
	t.Parallel()
	e := newTokenEnv(t)
	ctx := context.Background()
	iss, err := e.svc.IssueLoginSession(ctx, e.account("alice"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute + DefaultLeeway - time.Second)
	if _, err := e.svc.Verify(ctx, iss.Token); err != nil {
		t.Fatalf("within leeway: %v", err)
	}
	e.clock.Advance(2 * time.Second)
	_, err = e.svc.Verify(ctx, iss.Token)
	wantInvalid(t, err, ErrTokenExpired)
}

func TestKeyRotation(t *testing.T) {
	t.Parallel()
	e := newTokenEnv(t)
	ctx := context.Background()
	old, err := e.svc.IssueLoginSession(ctx, e.account("alice"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Rotate: new current key, old key kept as previous.
	rotated, err := NewKeySet(testSecretPrev, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetKeys(rotated); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Verify(ctx, old.Token); err != nil {
		t.Fatalf("old token with previous key: %v", err)
	}
	fresh, err := e.svc.IssueLoginSession(ctx, e.account("alice"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _ := jwt.NewParser().ParseUnverified(fresh.Token, jwt.MapClaims{})
	if tok.Header["kid"] != rotated.Current.ID() {
		t.Fatalf("new token kid = %v, want current %v", tok.Header["kid"], rotated.Current.ID())
	}
	// Drop the previous key: old tokens stop verifying, new ones keep working.
	only, _ := NewKeySet(testSecretPrev)
	if err := e.svc.SetKeys(only); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Verify(ctx, old.Token)
	wantInvalid(t, err)
	if _, err := e.svc.Verify(ctx, fresh.Token); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetKeys(KeySet{}); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("empty key set: %v", err)
	}
}

func forge(t *testing.T, m jwt.SigningMethod, key any, header map[string]any, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(m, claims)
	for k, v := range header {
		if v == nil {
			delete(tok.Header, k)
			continue
		}
		tok.Header[k] = v
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAlgorithmConfusionAndForgery(t *testing.T) {
	t.Parallel()
	e := newTokenEnv(t)
	ctx := context.Background()
	now := e.clock.Now().Unix()
	kid := e.keys.Current.ID()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	base := func(mut func(c jwt.MapClaims)) jwt.MapClaims {
		c := jwt.MapClaims{"iss": "patrol", "sub": "alice:login", "jti": "j1", "iat": now, "nbf": now, "exp": now + 3600, "kind": "login"}
		if mut != nil {
			mut(c)
		}
		return c
	}
	hs := func(c jwt.MapClaims) string {
		return forge(t, jwt.SigningMethodHS256, testSecret, map[string]any{"kid": kid}, c)
	}

	// Positive control: the forging helper produces verifiable tokens with the real key.
	if _, err := e.svc.Verify(ctx, hs(base(nil))); err != nil {
		t.Fatalf("control token rejected: %v", err)
	}

	valid := hs(base(nil))
	parts := strings.Split(valid, ".")
	payload, _ := json.Marshal(base(func(c jwt.MapClaims) { c["sub"] = "admin:login" }))
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + parts[2]

	cases := map[string]string{
		"alg none":               forge(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, map[string]any{"kid": kid}, base(nil)),
		"HS384 with real secret": forge(t, jwt.SigningMethodHS384, testSecret, map[string]any{"kid": kid}, base(nil)),
		"HS512 with real secret": forge(t, jwt.SigningMethodHS512, testSecret, map[string]any{"kid": kid}, base(nil)),
		"RS256 attacker key":     forge(t, jwt.SigningMethodRS256, rsaKey, map[string]any{"kid": kid}, base(nil)),
		"ES256 attacker key":     forge(t, jwt.SigningMethodES256, ecKey, map[string]any{"kid": kid}, base(nil)),
		"HS256 wrong secret":     forge(t, jwt.SigningMethodHS256, []byte("an-attacker-chosen-secret-of-32-bytes!!"), map[string]any{"kid": kid}, base(nil)),
		"HS256 unknown kid":      forge(t, jwt.SigningMethodHS256, testSecret, map[string]any{"kid": "nope"}, base(nil)),
		"HS256 missing kid":      forge(t, jwt.SigningMethodHS256, testSecret, nil, base(nil)),
		"tampered payload":       tampered,
		"wrong issuer":           hs(base(func(c jwt.MapClaims) { c["iss"] = "argocd" })),
		"missing issuer":         hs(base(func(c jwt.MapClaims) { delete(c, "iss") })),
		"missing iat":            hs(base(func(c jwt.MapClaims) { delete(c, "iat") })),
		"iat in future":          hs(base(func(c jwt.MapClaims) { c["iat"] = now + 600 })),
		"nbf in future":          hs(base(func(c jwt.MapClaims) { c["nbf"] = now + 600 })),
		"missing kind":           hs(base(func(c jwt.MapClaims) { delete(c, "kind") })),
		"unknown kind":           hs(base(func(c jwt.MapClaims) { c["kind"] = "admin" })),
		"login sub without cap":  hs(base(func(c jwt.MapClaims) { c["sub"] = "alice" })),
		"kind/sub mismatch":      hs(base(func(c jwt.MapClaims) { c["sub"] = "alice:apiKey" })),
		"project sub as login":   hs(base(func(c jwt.MapClaims) { c["sub"] = "proj:p:r" })),
		"login without exp":      hs(base(func(c jwt.MapClaims) { delete(c, "exp") })),
		"missing jti":            hs(base(func(c jwt.MapClaims) { delete(c, "jti") })),
		"malformed project sub":  hs(base(func(c jwt.MapClaims) { c["kind"] = "project"; c["sub"] = "proj:only" })),
		"sso without exp":        hs(base(func(c jwt.MapClaims) { c["kind"] = "sso"; c["sub"] = "u"; delete(c, "exp") })),
		"sso empty subject":      hs(base(func(c jwt.MapClaims) { c["kind"] = "sso"; c["sub"] = "" })),
		"non-string exp":         hs(base(func(c jwt.MapClaims) { c["exp"] = "tomorrow" })),
		"garbage":                "not.a.jwt",
		"two segments":           "abc.def",
		"oversized":              strings.Repeat("a", MaxTokenLength+1),
		"empty":                  "",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, err := e.svc.Verify(ctx, raw)
			if p != nil {
				t.Fatalf("forged token accepted: %+v", p)
			}
			wantInvalid(t, err)
		})
	}
}

func TestProjectTokens(t *testing.T) {
	t.Parallel()
	e := newTokenEnv(t)
	ctx := context.Background()
	iss, err := e.svc.IssueProjectToken(ctx, "team-a", "deployer", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Verify(ctx, iss.Token)
	wantInvalid(t, err, ErrTokenRevoked)

	e.mu.Lock()
	e.projects["proj:team-a:deployer|"+iss.ID] = true
	e.mu.Unlock()
	p, err := e.svc.Verify(ctx, iss.Token)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindProject || p.Subject != "proj:team-a:deployer" || p.Project != "team-a" || p.Role != "deployer" {
		t.Fatalf("principal = %+v", p)
	}
	for _, bad := range [][2]string{{"", "r"}, {"p", ""}, {"a:b", "r"}, {"p", "r:x"}, {"p", "r w"}} {
		if _, err := e.svc.IssueProjectToken(ctx, bad[0], bad[1], "", 0); !errors.Is(err, ErrInvalidSubject) {
			t.Fatalf("IssueProjectToken(%q,%q) = %v", bad[0], bad[1], err)
		}
	}

	// Registry errors are infrastructure errors, and a nil registry rejects project tokens.
	reg := TokenRegistryFunc(func(context.Context, string, string, int64) (bool, error) {
		return false, errors.New("apiserver down")
	})
	svc, _ := NewTokenService(TokenServiceConfig{Keys: e.keys, Accounts: AccountLookupFunc(e.lookup), Revocations: e.revs, Projects: reg, Now: e.clock.Now})
	_, err = svc.Verify(ctx, iss.Token)
	if err == nil || errors.Is(err, ErrInvalidToken) {
		t.Fatalf("registry failure: %v", err)
	}
	noReg, _ := NewTokenService(TokenServiceConfig{Keys: e.keys, Accounts: AccountLookupFunc(e.lookup), Revocations: e.revs, Now: e.clock.Now})
	_, err = noReg.Verify(ctx, iss.Token)
	wantInvalid(t, err)
}

func TestSSOSession(t *testing.T) {
	t.Parallel()
	e := newTokenEnv(t)
	ctx := context.Background()
	ident := SSOIdentity{
		Subject: "admin:login", // an IdP subject that looks like a local session must stay SSO
		Email:   "jane@example.com",
		Name:    "Jane",
		Groups:  []string{"devs", "ops"},
		Extra:   map[string]any{"roles": []string{"r1"}},
	}
	iss, err := e.svc.IssueSSOSession(ctx, ident, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.Verify(ctx, iss.Token)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindSSO || p.Subject != "admin:login" || p.Email != "jane@example.com" || p.Name != "Jane" ||
		!slices.Equal(p.Groups, []string{"devs", "ops"}) || p.Username() != "jane@example.com" {
		t.Fatalf("principal = %+v", p)
	}
	if roles := stringList(p.Claims["roles"]); !slices.Equal(roles, []string{"r1"}) {
		t.Fatalf("extra claim roles = %v", p.Claims["roles"])
	}
	for _, reserved := range []string{"sub", "kind", "iss", "exp", "groups"} {
		bad := ident
		bad.Extra = map[string]any{reserved: "x"}
		if _, err := e.svc.IssueSSOSession(ctx, bad, 0); err == nil {
			t.Fatalf("reserved extra claim %q accepted", reserved)
		}
	}
	if _, err := e.svc.IssueSSOSession(ctx, SSOIdentity{}, 0); !errors.Is(err, ErrInvalidSubject) {
		t.Fatalf("empty subject: %v", err)
	}
	if _, err := e.svc.IssueSSOSession(ctx, SSOIdentity{Subject: "x", Groups: make([]string, MaxGroups+1)}, 0); err == nil {
		t.Fatal("too many groups accepted")
	}
	if _, err := e.svc.IssueSSOSession(ctx, SSOIdentity{Subject: "x\x00y"}, 0); !errors.Is(err, ErrInvalidSubject) {
		t.Fatalf("control char subject: %v", err)
	}
	// SSO sessions are revocable like login sessions.
	if err := e.svc.Revoke(ctx, p); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Verify(ctx, iss.Token)
	wantInvalid(t, err, ErrTokenRevoked)
}

func TestRenewIfExpiring(t *testing.T) {
	t.Parallel()
	e := newTokenEnv(t)
	ctx := context.Background()
	iss, err := e.svc.IssueLoginSession(ctx, e.account("alice"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := e.svc.Verify(ctx, iss.Token)
	if _, ok, err := e.svc.RenewIfExpiring(ctx, p); ok || err != nil {
		t.Fatalf("renewed too early: %v %v", ok, err)
	}
	e.clock.Advance(time.Hour - 4*time.Minute)
	p, err = e.svc.Verify(ctx, iss.Token)
	if err != nil {
		t.Fatal(err)
	}
	renewed, ok, err := e.svc.RenewIfExpiring(ctx, p)
	if err != nil || !ok {
		t.Fatalf("not renewed: %v %v", ok, err)
	}
	if renewed.ID == iss.ID || renewed.ExpiresAt.Sub(renewed.IssuedAt) != time.Hour {
		t.Fatalf("renewed = %+v", renewed)
	}
	if rp, err := e.svc.Verify(ctx, renewed.Token); err != nil || rp.Subject != "alice" || rp.Kind != KindLogin {
		t.Fatalf("renewed token: %+v %v", rp, err)
	}
	// API tokens and SSO sessions are not renewed.
	sso, _ := e.svc.IssueSSOSession(ctx, SSOIdentity{Subject: "u"}, time.Minute)
	sp, _ := e.svc.Verify(ctx, sso.Token)
	if _, ok, _ := e.svc.RenewIfExpiring(ctx, sp); ok {
		t.Fatal("sso session renewed")
	}
	if _, ok, _ := e.svc.RenewIfExpiring(ctx, nil); ok {
		t.Fatal("nil principal renewed")
	}
}

type fakeExternal struct {
	p   *Principal
	err error
}

func (f fakeExternal) VerifyExternal(context.Context, string) (*Principal, error) { return f.p, f.err }

func TestExternalVerifierRouting(t *testing.T) {
	t.Parallel()
	e := newTokenEnv(t)
	ctx := context.Background()
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	idpToken := forge(t, jwt.SigningMethodRS256, rsaKey, map[string]any{"kid": "idp"}, jwt.MapClaims{"iss": "https://idp.example.com", "sub": "u1"})

	// Without an external verifier, foreign issuers are rejected.
	_, err := e.svc.Verify(ctx, idpToken)
	wantInvalid(t, err)

	ext := &Principal{Subject: "u1", Kind: KindExternal, TokenID: "idp-jti", ExpiresAt: e.clock.Now().Add(time.Hour)}
	svc, _ := NewTokenService(TokenServiceConfig{Keys: e.keys, Accounts: AccountLookupFunc(e.lookup), Revocations: e.revs, External: fakeExternal{p: ext}, Now: e.clock.Now})
	p, err := svc.Verify(ctx, idpToken)
	if err != nil || p.Subject != "u1" {
		t.Fatalf("external: %+v %v", p, err)
	}
	if err := svc.Revoke(ctx, p); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Verify(ctx, idpToken)
	wantInvalid(t, err, ErrTokenRevoked)

	// Errors from the external verifier pass through unchanged.
	sentinel := errors.New("boom")
	svc2, _ := NewTokenService(TokenServiceConfig{Keys: e.keys, Accounts: AccountLookupFunc(e.lookup), Revocations: e.revs, External: fakeExternal{err: sentinel}, Now: e.clock.Now})
	if _, err := svc2.Verify(ctx, idpToken); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
	// A verifier returning neither principal nor error is treated as a failure.
	svc3, _ := NewTokenService(TokenServiceConfig{Keys: e.keys, Accounts: AccountLookupFunc(e.lookup), Revocations: e.revs, External: fakeExternal{}, Now: e.clock.Now})
	if p, err := svc3.Verify(ctx, idpToken); err == nil || p != nil {
		t.Fatalf("nil principal accepted: %+v %v", p, err)
	}
	// A Patrol-issued token never goes to the external verifier.
	iss, _ := svc2.IssueLoginSession(ctx, e.account("alice"), time.Hour)
	if _, err := svc2.Verify(ctx, iss.Token); err != nil {
		t.Fatalf("local token routed externally: %v", err)
	}
}

type failingRevocations struct{}

func (failingRevocations) Revoke(context.Context, string, time.Time) error { return errors.New("down") }
func (failingRevocations) IsRevoked(context.Context, string) (bool, error) {
	return false, errors.New("down")
}

func TestVerifyInfrastructureErrors(t *testing.T) {
	t.Parallel()
	e := newTokenEnv(t)
	ctx := context.Background()
	iss, _ := e.svc.IssueLoginSession(ctx, e.account("alice"), time.Hour)

	svc, _ := NewTokenService(TokenServiceConfig{Keys: e.keys, Accounts: AccountLookupFunc(e.lookup), Revocations: failingRevocations{}, Now: e.clock.Now})
	if _, err := svc.Verify(ctx, iss.Token); err == nil || errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revocation store failure: %v", err)
	}
	e.mu.Lock()
	e.lookupErr = errors.New("settings unavailable")
	e.mu.Unlock()
	if _, err := e.svc.Verify(ctx, iss.Token); err == nil || errors.Is(err, ErrInvalidToken) {
		t.Fatalf("lookup failure: %v", err)
	}
}

func TestNewTokenServiceValidation(t *testing.T) {
	t.Parallel()
	ks, _ := NewKeySet(testSecret)
	acc := Accounts{}
	revs := NewMemoryRevocationStore(0, nil)
	for name, cfg := range map[string]TokenServiceConfig{
		"no keys":        {Accounts: acc, Revocations: revs},
		"no accounts":    {Keys: ks, Revocations: revs},
		"no revocations": {Keys: ks, Accounts: acc},
	} {
		if _, err := NewTokenService(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMemoryRevocationStore(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	m := NewMemoryRevocationStore(2, clock.Now)
	ctx := context.Background()
	if err := m.Revoke(ctx, "a", clock.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke(ctx, "expired", clock.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := m.IsRevoked(ctx, "expired"); ok {
		t.Fatal("expired token stored")
	}
	if err := m.Revoke(ctx, "b", clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke(ctx, "c", clock.Now().Add(time.Hour)); !errors.Is(err, ErrRevocationStoreFull) {
		t.Fatalf("full: %v", err)
	}
	clock.Advance(2 * time.Minute) // "a" expires and can be pruned
	if err := m.Revoke(ctx, "c", clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke(ctx, "", clock.Now().Add(time.Hour)); err == nil {
		t.Fatal("empty id accepted")
	}
	if err := m.Revoke(ctx, "d", time.Time{}); !errors.Is(err, ErrNotRevocable) {
		t.Fatalf("zero exp: %v", err)
	}
}

// Property: SSO sessions round-trip subject, groups and email for arbitrary inputs.
func TestSSOSessionRoundTripProperty(t *testing.T) {
	t.Parallel()
	e := newTokenEnv(t)
	ctx := context.Background()
	rapid.Check(t, func(rt *rapid.T) {
		sub := rapid.StringMatching(`[\p{L}\p{N}:@._|/ -]{1,64}`).Draw(rt, "sub")
		groups := rapid.SliceOfN(rapid.StringMatching(`[\p{L}\p{N}:._/ -]{1,32}`), 0, 20).Draw(rt, "groups")
		email := rapid.StringMatching(`([a-z]{1,8}@[a-z]{1,8}\.com)?`).Draw(rt, "email")
		iss, err := e.svc.IssueSSOSession(ctx, SSOIdentity{Subject: sub, Groups: groups, Email: email}, time.Hour)
		if err != nil {
			rt.Fatal(err)
		}
		p, err := e.svc.Verify(ctx, iss.Token)
		if err != nil {
			rt.Fatal(err)
		}
		if p.Subject != sub || p.Email != email || p.Kind != KindSSO {
			rt.Fatalf("principal = %+v", p)
		}
		if len(groups) == 0 {
			groups = []string{}
		}
		if p.Groups == nil {
			p.Groups = []string{}
		}
		if !slices.Equal(p.Groups, groups) {
			rt.Fatalf("groups %q != %q", p.Groups, groups)
		}
	})
}
