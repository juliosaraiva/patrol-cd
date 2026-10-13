package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/juliosaraiva/patrol-cd/internal/auth"
)

func TestAuthorizationCodeFlowWithPKCE(t *testing.T) {
	t.Parallel()
	f := newFakeIdP(t)
	p := newTestProvider(t, f, func(c *Config) {
		c.EnablePKCE = true
		c.RequestedScopes = []string{"profile", "groups"}
		c.RequestedIDTokenClaims = map[string]*Claim{"groups": {Essential: true}}
		c.DomainHint = "  example.com "
	})
	codec := newTestCodec(t, nil)
	ctx := context.Background()

	authURL, cookie, err := p.StartLogin(ctx, codec, LoginRequest{
		RedirectURL:           callbackURL,
		ReturnURL:             "https://patrol.example.com/applications?x=1",
		AllowedReturnBaseURLs: allowedBases,
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	st, err := codec.Decode(cookie)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"client_id":             f.clientID,
		"redirect_uri":          callbackURL,
		"response_type":         "code",
		"scope":                 "openid profile groups",
		"state":                 st.State,
		"nonce":                 st.Nonce,
		"code_challenge_method": "S256",
		"domain_hint":           "example.com",
		"claims":                `{"id_token":{"groups":{"essential":true}}}`,
	} {
		if got := q.Get(k); got != want {
			t.Errorf("auth param %s = %q, want %q", k, got, want)
		}
	}
	if q.Get("code_challenge") == "" || st.PKCEVerifier == "" || strings.Contains(authURL, st.PKCEVerifier) {
		t.Fatal("PKCE challenge missing or verifier leaked into the URL")
	}

	cb := f.follow(authURL)
	res, err := p.HandleCallback(ctx, cb, cookie, codec)
	if err != nil {
		t.Fatal(err)
	}
	c := res.Claims
	if c.Sub != "user-123" || c.Email != "jane@example.com" || c.Name != "Jane Doe" || c.Issuer != f.issuer() ||
		!slices.Equal(c.Groups, []string{"devs", "ops"}) || c.Expiry.IsZero() {
		t.Fatalf("claims = %+v", c)
	}
	if res.ReturnURL != "https://patrol.example.com/applications?x=1" || res.RawIDToken == "" {
		t.Fatalf("result = %+v", res)
	}
	f.mu.Lock()
	verifier := f.tokenRequests[0].Get("code_verifier")
	f.mu.Unlock()
	if verifier != st.PKCEVerifier {
		t.Fatalf("token request verifier = %q, want state verifier", verifier)
	}

	// The authorization code is single-use.
	if _, err := p.HandleCallback(ctx, cb, cookie, codec); !errors.Is(err, ErrExchange) {
		t.Fatalf("code replay: %v", err)
	}
}

func TestPKCEVerifierMismatchRejectedByIdP(t *testing.T) {
	t.Parallel()
	f := newFakeIdP(t)
	p := newTestProvider(t, f, func(c *Config) { c.EnablePKCE = true })
	codec := newTestCodec(t, nil)
	ctx := context.Background()
	authURL, cookie, err := p.StartLogin(ctx, codec, LoginRequest{RedirectURL: callbackURL})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := codec.Decode(cookie)
	st.PKCEVerifier = "attacker-chosen-verifier-attacker-chosen-verifier"
	forged, err := codec.Encode(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.HandleCallback(ctx, f.follow(authURL), forged, codec); !errors.Is(err, ErrExchange) {
		t.Fatalf("err = %v, want ErrExchange", err)
	}
	// A state without verifier is rejected locally when PKCE is enabled.
	authURL2, cookie2, _ := p.StartLogin(ctx, codec, LoginRequest{RedirectURL: callbackURL})
	st2, _ := codec.Decode(cookie2)
	st2.PKCEVerifier = ""
	noVerifier, _ := codec.Encode(st2)
	if _, err := p.HandleCallback(ctx, f.follow(authURL2), noVerifier, codec); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("missing verifier: %v", err)
	}
}

func TestFlowWithoutPKCE(t *testing.T) {
	t.Parallel()
	f := newFakeIdP(t)
	p := newTestProvider(t, f, nil)
	codec := newTestCodec(t, nil)
	ctx := context.Background()
	authURL, cookie, err := p.StartLogin(ctx, codec, LoginRequest{RedirectURL: callbackURL})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	if u.Query().Get("code_challenge") != "" || u.Query().Get("scope") != "openid profile email groups" {
		t.Fatalf("auth URL = %s", authURL)
	}
	res, err := p.HandleCallback(ctx, f.follow(authURL), cookie, codec)
	if err != nil {
		t.Fatal(err)
	}
	if res.ReturnURL != "" || res.Claims.Sub != "user-123" {
		t.Fatalf("result = %+v", res)
	}
}

func TestCallbackStateAndProviderErrors(t *testing.T) {
	t.Parallel()
	f := newFakeIdP(t)
	clock := newTestClock()
	p := newTestProvider(t, f, nil, WithClock(clock.Now))
	codec := newTestCodec(t, clock.Now)
	otherCodec, _ := NewStateCodec(clock.Now, serverSecretPrev)
	ctx := context.Background()

	start := func() (Callback, string) {
		authURL, cookie, err := p.StartLogin(ctx, codec, LoginRequest{RedirectURL: callbackURL})
		if err != nil {
			t.Fatal(err)
		}
		return f.follow(authURL), cookie
	}

	cb, cookie := start()
	bad := cb
	bad.State = "not-the-state"
	if _, err := p.HandleCallback(ctx, bad, cookie, codec); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("wrong state: %v", err)
	}
	bad.State = ""
	if _, err := p.HandleCallback(ctx, bad, cookie, codec); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("empty state: %v", err)
	}
	if _, err := p.HandleCallback(ctx, cb, "", codec); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("missing cookie: %v", err)
	}
	tampered := []byte(cookie)
	tampered[len(tampered)/2] ^= 0x01
	if _, err := p.HandleCallback(ctx, cb, string(tampered), codec); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("tampered cookie: %v", err)
	}
	if _, err := p.HandleCallback(ctx, cb, cookie, otherCodec); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("cookie from another server secret: %v", err)
	}
	noCode := cb
	noCode.Code = ""
	if _, err := p.HandleCallback(ctx, noCode, cookie, codec); !errors.Is(err, ErrMissingCode) {
		t.Fatalf("missing code: %v", err)
	}
	pe := Callback{Error: "access_denied", ErrorDescription: strings.Repeat("<script>", 100)}
	_, err := p.HandleCallback(ctx, pe, cookie, codec)
	var perr *ProviderError
	if !errors.As(err, &perr) || !errors.Is(err, ErrProviderError) || perr.Code != "access_denied" || len(perr.Description) > maxProviderErrorText {
		t.Fatalf("provider error: %v", err)
	}
	// The original callback still works (none of the failures above consumed the code).
	if _, err := p.HandleCallback(ctx, cb, cookie, codec); err != nil {
		t.Fatalf("valid callback: %v", err)
	}
	// Expired state.
	cb2, cookie2 := start()
	clock.Advance(StateTTL + time.Second)
	if _, err := p.HandleCallback(ctx, cb2, cookie2, codec); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expired state: %v", err)
	}
}

func TestCallbackIDTokenFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	run := func(t *testing.T, knob func(f *fakeIdP)) error {
		f := newFakeIdP(t)
		f.set(knob)
		p := newTestProvider(t, f, nil)
		codec := newTestCodec(t, nil)
		authURL, cookie, err := p.StartLogin(ctx, codec, LoginRequest{RedirectURL: callbackURL})
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.HandleCallback(ctx, f.follow(authURL), cookie, codec)
		return err
	}
	t.Run("wrong nonce", func(t *testing.T) {
		t.Parallel()
		err := run(t, func(f *fakeIdP) { n := "replayed-nonce"; f.nonceOverride = &n })
		if !errors.Is(err, ErrNonceMismatch) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("expired id token", func(t *testing.T) {
		t.Parallel()
		err := run(t, func(f *fakeIdP) { f.idTokenTTL = -time.Minute })
		if !errors.Is(err, ErrIDTokenInvalid) {
			t.Fatalf("err = %v", err)
		}
		if _, ok := errors.AsType[*gooidc.TokenExpiredError](err); !ok {
			t.Fatalf("expiry error type lost: %v", err)
		}
	})
	t.Run("audience mismatch", func(t *testing.T) {
		t.Parallel()
		err := run(t, func(f *fakeIdP) { f.audience = "another-client" })
		if !errors.Is(err, ErrAudience) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing id_token", func(t *testing.T) {
		t.Parallel()
		if err := run(t, func(f *fakeIdP) { f.omitIDToken = true }); !errors.Is(err, ErrMissingIDToken) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("too many groups", func(t *testing.T) {
		t.Parallel()
		groups := make([]any, auth.MaxGroups+1)
		for i := range groups {
			groups[i] = fmt.Sprintf("g%d", i)
		}
		if err := run(t, func(f *fakeIdP) { f.extraClaims["groups"] = groups }); !errors.Is(err, ErrTooManyGroups) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestStartLoginRejectsOpenRedirect(t *testing.T) {
	t.Parallel()
	f := newFakeIdP(t)
	p := newTestProvider(t, f, nil)
	_, _, err := p.StartLogin(context.Background(), newTestCodec(t, nil), LoginRequest{
		RedirectURL: callbackURL, ReturnURL: "https://evil.example.com/", AllowedReturnBaseURLs: allowedBases,
	})
	if !errors.Is(err, auth.ErrInvalidRedirectURL) {
		t.Fatalf("err = %v", err)
	}
	f.mu.Lock()
	hits := f.discoveryHits
	f.mu.Unlock()
	if hits != 0 {
		t.Fatal("open redirect check should happen before contacting the IdP")
	}
}

func TestBuildAuthURLValidation(t *testing.T) {
	t.Parallel()
	f := newFakeIdP(t)
	p := newTestProvider(t, f, nil)
	pk := newTestProvider(t, f, func(c *Config) { c.EnablePKCE = true })
	ctx := context.Background()
	if _, err := p.BuildAuthURL(ctx, "", "n", "", callbackURL); err == nil {
		t.Fatal("empty state accepted")
	}
	if _, err := p.BuildAuthURL(ctx, "s", "", "", callbackURL); err == nil {
		t.Fatal("empty nonce accepted")
	}
	if _, err := p.BuildAuthURL(ctx, "s", "n", "", "/relative"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("relative redirect: %v", err)
	}
	if _, err := pk.BuildAuthURL(ctx, "s", "n", "", callbackURL); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("PKCE without verifier: %v", err)
	}
	u, err := pk.BuildAuthURL(ctx, "s", "n", "verifier-verifier-verifier-verifier-verifier-1", callbackURL)
	if err != nil || !strings.Contains(u, "code_challenge=") {
		t.Fatalf("PKCE url: %s %v", u, err)
	}
}

func TestUserInfoGroups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name      string
		path      string
		base      bool
		knob      func(f *fakeIdP)
		want      []string
		wantError error
	}{
		{name: "json via userInfoPath", path: "/userinfo", knob: func(f *fakeIdP) { f.userinfo = map[string]any{"groups": []any{"from-userinfo"}} }, want: []string{"from-userinfo"}},
		{name: "json via discovery", knob: func(f *fakeIdP) { f.userinfo = map[string]any{"groups": "single"} }, want: []string{"single"}},
		{name: "json via userInfoBaseURL", path: "/userinfo", base: true, knob: func(f *fakeIdP) { f.userinfo = map[string]any{"groups": []any{"b"}} }, want: []string{"b"}},
		{name: "signed jwt", path: "/userinfo", knob: func(f *fakeIdP) {
			f.userinfo = map[string]any{"groups": []any{"jwt-group"}}
			f.userinfoJWT = true
		}, want: []string{"jwt-group"}},
		{name: "no groups in userinfo", knob: func(f *fakeIdP) { f.userinfo = map[string]any{} }, want: nil},
		{name: "subject mismatch", knob: func(f *fakeIdP) { f.userinfo = map[string]any{"sub": "someone-else"} }, wantError: ErrUserInfo},
		{name: "server error", knob: func(f *fakeIdP) { f.userinfoStatus = 500 }, wantError: ErrUserInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeIdP(t)
			f.set(tc.knob)
			var baseHits atomic.Int32
			p := newTestProvider(t, f, func(c *Config) {
				c.EnableUserInfoGroups = true
				c.UserInfoPath = tc.path
				if tc.base {
					// A separate server proves the base URL (not the issuer) is used.
					ui := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						baseHits.Add(1)
						f.userinfoHandler(w, r)
					}))
					t.Cleanup(ui.Close)
					c.UserInfoBaseURL = ui.URL
				}
			})
			codec := newTestCodec(t, nil)
			authURL, cookie, err := p.StartLogin(ctx, codec, LoginRequest{RedirectURL: callbackURL})
			if err != nil {
				t.Fatal(err)
			}
			res, err := p.HandleCallback(ctx, f.follow(authURL), cookie, codec)
			if tc.wantError != nil {
				if !errors.Is(err, tc.wantError) {
					t.Fatalf("err = %v, want %v", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(res.Claims.Groups, tc.want) {
				t.Fatalf("groups = %v, want %v (ID token groups must be replaced)", res.Claims.Groups, tc.want)
			}
			if tc.base && baseHits.Load() != 1 {
				t.Fatalf("userInfoBaseURL server hits = %d", baseHits.Load())
			}
			if got := stringsOf(res.Claims.Raw["groups"]); !slices.Equal(got, tc.want) {
				t.Fatalf("raw groups = %v", res.Claims.Raw["groups"])
			}
		})
	}
}

func stringsOf(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case nil:
		return nil
	}
	return []string{"unexpected"}
}

// End to end: OIDC login -> Patrol SSO session -> verification.
func TestOIDCLoginToPatrolSession(t *testing.T) {
	t.Parallel()
	f := newFakeIdP(t)
	f.set(func(f *fakeIdP) {
		f.extraClaims["roles"] = []any{"approver"}
		f.extraClaims["federated_claims"] = map[string]any{"connector_id": "github", "user_id": "gh-42"}
	})
	p := newTestProvider(t, f, func(c *Config) { c.EnablePKCE = true })
	codec := newTestCodec(t, nil)
	ctx := context.Background()
	authURL, cookie, err := p.StartLogin(ctx, codec, LoginRequest{RedirectURL: callbackURL})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.HandleCallback(ctx, f.follow(authURL), cookie, codec)
	if err != nil {
		t.Fatal(err)
	}
	ks, _ := auth.NewKeySet(serverSecret)
	svc, err := auth.NewTokenService(auth.TokenServiceConfig{
		Keys: ks, Accounts: auth.Accounts{}, Revocations: auth.NewMemoryRevocationStore(0, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	ident := res.Claims.SSOIdentity("roles", "sub", "missing")
	if ident.Subject != "gh-42" || len(ident.Extra) != 1 {
		t.Fatalf("identity = %+v", ident)
	}
	iss, err := svc.IssueSSOSession(ctx, ident, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pr, err := svc.Verify(ctx, iss.Token)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Subject != "gh-42" || pr.Kind != auth.KindSSO || !slices.Equal(pr.Groups, []string{"devs", "ops"}) || pr.Email != "jane@example.com" {
		t.Fatalf("principal = %+v", pr)
	}
	roles, _ := json.Marshal(pr.Claims["roles"])
	if string(roles) != `["approver"]` {
		t.Fatalf("roles claim = %s", roles)
	}
}

func TestConcurrentLogins(t *testing.T) {
	t.Parallel()
	f := newFakeIdP(t)
	p := newTestProvider(t, f, func(c *Config) { c.EnablePKCE = true })
	codec := newTestCodec(t, nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			authURL, cookie, err := p.StartLogin(ctx, codec, LoginRequest{RedirectURL: callbackURL})
			if err != nil {
				errs <- err
				return
			}
			if _, err := p.HandleCallback(ctx, f.follow(authURL), cookie, codec); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.discoveryHits != 1 {
		t.Fatalf("discovery ran %d times, want 1", f.discoveryHits)
	}
}
