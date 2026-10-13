package auth

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"pgregory.net/rapid"
)

func TestAccountsFromMaps(t *testing.T) {
	t.Parallel()
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tokens := `[{"id":"t1","iat":100},{"id":"t2","iat":200,"exp":300}]`
	tests := []struct {
		name    string
		cm      map[string]string
		secret  map[string]string
		want    Accounts
		wantErr bool
	}{
		{
			name: "defaults: only admin",
			want: Accounts{"admin": {Name: "admin", Enabled: true, Capabilities: []Capability{CapabilityLogin}}},
		},
		{
			name:   "admin password, mtime and tokens",
			secret: map[string]string{KeyAdminPassword: "hash", KeyAdminPasswordMtime: "2026-01-02T03:04:05Z", KeyAdminTokens: tokens},
			want: Accounts{"admin": {
				Name: "admin", Enabled: true, Capabilities: []Capability{CapabilityLogin},
				PasswordHash: "hash", PasswordMtime: mtime,
				Tokens: []TokenMeta{{ID: "t1", IssuedAt: 100}, {ID: "t2", IssuedAt: 200, ExpiresAt: 300}},
			}},
		},
		{
			name: "admin disabled",
			cm:   map[string]string{KeyAdminEnabled: "false"},
			want: Accounts{"admin": {Name: "admin", Enabled: false, Capabilities: []Capability{CapabilityLogin}}},
		},
		{
			name: "any false wins for admin",
			cm:   map[string]string{KeyAdminEnabled: "true", "accounts.admin.enabled": "false"},
			want: Accounts{"admin": {Name: "admin", Enabled: false, Capabilities: []Capability{CapabilityLogin}}},
		},
		{
			name: "accounts.admin adds capabilities without duplicates",
			cm:   map[string]string{"accounts.admin": "apiKey, login"},
			want: Accounts{"admin": {Name: "admin", Enabled: true, Capabilities: []Capability{CapabilityLogin, CapabilityAPIKey}}},
		},
		{
			name: "local account with secret data",
			cm:   map[string]string{"accounts.alice": "apiKey, login", "accounts.alice.enabled": "true"},
			secret: map[string]string{
				"accounts.alice.password":      "ahash",
				"accounts.alice.passwordMtime": "2026-01-02T03:04:05Z",
				"accounts.alice.tokens":        tokens,
				"accounts.ghost.password":      "ignored: not declared in cm",
			},
			want: Accounts{
				"admin": {Name: "admin", Enabled: true, Capabilities: []Capability{CapabilityLogin}},
				"alice": {
					Name: "alice", Enabled: true, Capabilities: []Capability{CapabilityAPIKey, CapabilityLogin},
					PasswordHash: "ahash", PasswordMtime: mtime,
					Tokens: []TokenMeta{{ID: "t1", IssuedAt: 100}, {ID: "t2", IssuedAt: 200, ExpiresAt: 300}},
				},
			},
		},
		{
			name: "enabled-only declaration",
			cm:   map[string]string{"accounts.bob.enabled": "false"},
			want: Accounts{
				"admin": {Name: "admin", Enabled: true, Capabilities: []Capability{CapabilityLogin}},
				"bob":   {Name: "bob", Enabled: false},
			},
		},
		{
			name:    "invalid enabled disables (fail closed)",
			cm:      map[string]string{"accounts.bob": "login", "accounts.bob.enabled": "yes please"},
			wantErr: true,
			want: Accounts{
				"admin": {Name: "admin", Enabled: true, Capabilities: []Capability{CapabilityLogin}},
				"bob":   {Name: "bob", Enabled: false, Capabilities: []Capability{CapabilityLogin}},
			},
		},
		{
			name:    "invalid mtime disables (fail closed)",
			cm:      map[string]string{"accounts.bob": "login"},
			secret:  map[string]string{"accounts.bob.passwordMtime": "yesterday"},
			wantErr: true,
			want: Accounts{
				"admin": {Name: "admin", Enabled: true, Capabilities: []Capability{CapabilityLogin}},
				"bob":   {Name: "bob", Enabled: false, Capabilities: []Capability{CapabilityLogin}},
			},
		},
		{
			name:    "invalid token JSON leaves no tokens",
			cm:      map[string]string{"accounts.bob": "apiKey"},
			secret:  map[string]string{"accounts.bob.tokens": "{not json"},
			wantErr: true,
			want: Accounts{
				"admin": {Name: "admin", Enabled: true, Capabilities: []Capability{CapabilityLogin}},
				"bob":   {Name: "bob", Enabled: true, Capabilities: []Capability{CapabilityAPIKey}},
			},
		},
		{
			name:    "unknown capability ignored and reported",
			cm:      map[string]string{"accounts.bob": "login, superuser"},
			wantErr: true,
			want: Accounts{
				"admin": {Name: "admin", Enabled: true, Capabilities: []Capability{CapabilityLogin}},
				"bob":   {Name: "bob", Enabled: true, Capabilities: []Capability{CapabilityLogin}},
			},
		},
		{
			name:    "malformed keys skipped",
			cm:      map[string]string{"accounts.a.b.c": "login", "accounts.": "login", "accounts.x:y": "login", "unrelated": "x"},
			wantErr: true,
			want:    Accounts{"admin": {Name: "admin", Enabled: true, Capabilities: []Capability{CapabilityLogin}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := AccountsFromMaps(tc.cm, tc.secret)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				var ce *AccountConfigError
				if !errors.As(err, &ce) {
					t.Fatalf("error is not an *AccountConfigError: %v", err)
				}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("accounts mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAccountsLookup(t *testing.T) {
	t.Parallel()
	as, err := AccountsFromMaps(map[string]string{"accounts.zed": "login", "accounts.amy": "login"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := as.LookupAccount(context.Background(), "nobody"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("err = %v", err)
	}
	if a, err := as.Get("amy"); err != nil || a.Name != "amy" {
		t.Fatalf("Get amy = %v, %v", a, err)
	}
	if got := as.Names(); !slices.Equal(got, []string{"admin", "amy", "zed"}) {
		t.Fatalf("Names = %v", got)
	}
}

func TestValidateAccountName(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"admin", "alice", "ci-bot_1", "UPPER", "üser"} {
		if err := ValidateAccountName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a:b", "a.b", "a b", "tab\t", "nl\n", string(make([]byte, 254))} {
		if err := ValidateAccountName(bad); !errors.Is(err, ErrInvalidAccountName) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSecretTokenMutations(t *testing.T) {
	t.Parallel()
	alice := Account{Name: "alice", Enabled: true, Capabilities: []Capability{CapabilityAPIKey}}
	orig := map[string]string{"other": "kept"}

	s1, err := AddToken(orig, alice, TokenMeta{ID: "t1", IssuedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(orig) != 1 {
		t.Fatal("input secret mutated")
	}
	s2, err := AddToken(s1, alice, TokenMeta{ID: "t2", IssuedAt: 2, ExpiresAt: 3})
	if err != nil {
		t.Fatal(err)
	}
	var stored []TokenMeta
	if err := json.Unmarshal([]byte(s2["accounts.alice.tokens"]), &stored); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]TokenMeta{{ID: "t1", IssuedAt: 1}, {ID: "t2", IssuedAt: 2, ExpiresAt: 3}}, stored); diff != "" {
		t.Fatal(diff)
	}
	if _, err := AddToken(s2, alice, TokenMeta{ID: "t1"}); !errors.Is(err, ErrTokenExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := DeleteToken(s2, alice, "nope"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
	s3, err := DeleteToken(s2, alice, "t1")
	if err != nil {
		t.Fatal(err)
	}
	s4, err := DeleteToken(s3, alice, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s4["accounts.alice.tokens"]; ok {
		t.Fatal("empty token list should remove the key")
	}
	if s4["other"] != "kept" {
		t.Fatal("unrelated key lost")
	}

	// Capability and state checks.
	if _, err := AddToken(orig, Account{Name: "bob", Enabled: true, Capabilities: []Capability{CapabilityLogin}}, TokenMeta{ID: "x"}); !errors.Is(err, ErrMissingCapability) {
		t.Fatalf("missing apiKey: %v", err)
	}
	if _, err := AddToken(orig, Account{Name: "bob", Capabilities: []Capability{CapabilityAPIKey}}, TokenMeta{ID: "x"}); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	if _, err := AddToken(map[string]string{"accounts.alice.tokens": "garbage"}, alice, TokenMeta{ID: "x"}); !errors.Is(err, ErrCorruptTokenList) {
		t.Fatalf("corrupt list: %v", err)
	}

	// Admin uses the admin.* keys.
	admin := Account{Name: AdminUsername, Enabled: true, Capabilities: []Capability{CapabilityLogin, CapabilityAPIKey}}
	sa, err := AddToken(nil, admin, TokenMeta{ID: "a1", IssuedAt: 5})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sa[KeyAdminTokens]; !ok {
		t.Fatalf("admin tokens not stored under %s: %v", KeyAdminTokens, sa)
	}
}

func TestChangePassword(t *testing.T) {
	t.Parallel()
	alice := Account{Name: "alice", Enabled: true, Capabilities: []Capability{CapabilityLogin}}
	policy, err := NewPasswordPolicy("")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 8, 30, 15, 999_000_000, time.UTC)
	if _, err := ChangePassword(nil, alice, "short", policy, now); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("policy: %v", err)
	}
	sec, err := ChangePassword(map[string]string{"x": "y"}, alice, "a-new-password", policy, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := sec["accounts.alice.passwordMtime"]; got != "2026-10-12T08:30:15Z" {
		t.Fatalf("mtime = %q", got)
	}
	as, err := AccountsFromMaps(map[string]string{"accounts.alice": "login"}, sec)
	if err != nil {
		t.Fatal(err)
	}
	got := as["alice"]
	if !VerifyPassword("a-new-password", got.PasswordHash) {
		t.Fatal("new password does not verify")
	}
	if !got.PasswordMtime.Equal(now.Truncate(time.Second)) {
		t.Fatalf("parsed mtime = %v", got.PasswordMtime)
	}
	// Removing the password removes both keys.
	cleared, err := SecretWithPassword(sec, "alice", "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cleared["accounts.alice.password"]; ok {
		t.Fatal("password key not removed")
	}
	if _, ok := cleared["accounts.alice.passwordMtime"]; ok {
		t.Fatal("mtime key not removed")
	}
}

func TestSecretData(t *testing.T) {
	t.Parallel()
	got := SecretData(map[string][]byte{"a": []byte("1"), "b": nil})
	if diff := cmp.Diff(map[string]string{"a": "1", "b": ""}, got); diff != "" {
		t.Fatal(diff)
	}
}

// Property: parsing never panics, admin always exists, every parsed account has a
// valid name, and enabled accounts never come with an unparsable mtime.
func TestAccountsFromMapsProperties(t *testing.T) {
	t.Parallel()
	keyGen := rapid.SampledFrom([]string{
		"accounts.alice", "accounts.alice.enabled", "accounts.alice.extra", "accounts.a.b.c",
		"accounts.", "accounts.:x", "admin.enabled", "accounts.admin", "accounts.admin.enabled", "other",
	})
	valGen := rapid.SampledFrom([]string{"", "true", "false", "login", "apiKey, login", "bogus", ",,", "TRUE", "0"})
	secretKeyGen := rapid.SampledFrom([]string{
		"admin.password", "admin.passwordMtime", "admin.tokens",
		"accounts.alice.password", "accounts.alice.passwordMtime", "accounts.alice.tokens",
	})
	secretValGen := rapid.SampledFrom([]string{"", "x", "2026-01-01T00:00:00Z", "[]", `[{"id":"a","iat":1}]`, `[{"iat":1}]`, "{"})
	rapid.Check(t, func(t *rapid.T) {
		cm := rapid.MapOf(keyGen, valGen).Draw(t, "cm")
		secret := rapid.MapOf(secretKeyGen, secretValGen).Draw(t, "secret")
		accounts, _ := AccountsFromMaps(cm, secret)
		admin, ok := accounts[AdminUsername]
		if !ok || !admin.HasCapability(CapabilityLogin) {
			t.Fatalf("admin missing or without login: %+v", admin)
		}
		for name, a := range accounts {
			if a.Name != name || ValidateAccountName(name) != nil {
				t.Fatalf("bad account entry %q: %+v", name, a)
			}
			for _, tok := range a.Tokens {
				if tok.ID == "" {
					t.Fatalf("token without id accepted: %+v", a.Tokens)
				}
			}
		}
	})
}

// Property: AddToken followed by DeleteToken restores the token list, and the order of
// remaining tokens is preserved.
func TestTokenAddDeleteProperty(t *testing.T) {
	t.Parallel()
	acct := Account{Name: "svc", Enabled: true, Capabilities: []Capability{CapabilityAPIKey}}
	rapid.Check(t, func(t *rapid.T) {
		ids := rapid.SliceOfNDistinct(rapid.StringMatching(`[a-z0-9-]{1,12}`), 0, 8, rapid.ID).Draw(t, "ids")
		secret := map[string]string{}
		var err error
		for i, id := range ids {
			if secret, err = AddToken(secret, acct, TokenMeta{ID: id, IssuedAt: int64(i)}); err != nil {
				t.Fatal(err)
			}
		}
		before := secret["accounts.svc.tokens"]
		extra := "extra-" + rapid.StringMatching(`[a-z]{3}`).Draw(t, "extra")
		withExtra, err := AddToken(secret, acct, TokenMeta{ID: extra, IssuedAt: 99})
		if err != nil {
			t.Fatal(err)
		}
		after, err := DeleteToken(withExtra, acct, extra)
		if err != nil {
			t.Fatal(err)
		}
		if after["accounts.svc.tokens"] != before {
			t.Fatalf("round trip changed tokens: %q -> %q", before, after["accounts.svc.tokens"])
		}
	})
}
