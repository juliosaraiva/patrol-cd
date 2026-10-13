package oidc

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

func TestStateCodecRoundTripAndRotation(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	old, err := NewStateCodec(clock.Now, serverSecretPrev)
	if err != nil {
		t.Fatal(err)
	}
	st, err := NewLoginState(callbackURL, "https://patrol.example.com/x", true, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.State) < 43 || len(st.Nonce) < 43 || st.PKCEVerifier == "" || st.State == st.Nonce {
		t.Fatalf("weak state: %+v", st)
	}
	enc, err := old.Encode(st)
	if err != nil {
		t.Fatal(err)
	}
	// After rotating the server secret, in-flight states still decode.
	rotated, err := NewStateCodec(clock.Now, serverSecret, serverSecretPrev)
	if err != nil {
		t.Fatal(err)
	}
	got, err := rotated.Decode(enc)
	if err != nil || got != st {
		t.Fatalf("decode after rotation: %+v %v", got, err)
	}
	// Without the previous secret they do not.
	if _, err := newTestCodec(t, clock.Now).Decode(enc); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("decode with unrelated key: %v", err)
	}
	// Encryption hides the content.
	if containsAny(enc, st.State, st.Nonce, st.PKCEVerifier, "patrol.example.com") {
		t.Fatal("state cookie leaks plaintext")
	}
	// Expiry and clock skew.
	clock.Advance(StateTTL)
	if _, err := rotated.Decode(enc); err != nil {
		t.Fatalf("at TTL: %v", err)
	}
	clock.Advance(time.Second)
	if _, err := rotated.Decode(enc); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("after TTL: %v", err)
	}
	future, _ := NewLoginState(callbackURL, "", false, clock.Now().Add(time.Hour))
	fenc, _ := rotated.Encode(future)
	if _, err := rotated.Decode(fenc); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("future-dated state: %v", err)
	}
	for _, bad := range []string{"", "!!!", "QUJD", string(make([]byte, 5000))} {
		if _, err := rotated.Decode(bad); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("Decode(%q) = %v", bad, err)
		}
	}
	if _, err := NewStateCodec(nil, []byte("short")); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("short secret: %v", err)
	}
	// Incomplete states are rejected even when authentic.
	inc, _ := rotated.Encode(LoginState{State: "s", IssuedAt: clock.Now().Unix()})
	if _, err := rotated.Decode(inc); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("incomplete: %v", err)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// Property: flipping any bit of an encoded state makes it undecodable.
func TestStateTamperProperty(t *testing.T) {
	t.Parallel()
	codec := newTestCodec(t, nil)
	rapid.Check(t, func(rt *rapid.T) {
		st, err := NewLoginState(callbackURL, rapid.StringMatching(`(https://patrol\.example\.com/[a-z]{0,20})?`).Draw(rt, "ret"), rapid.Bool().Draw(rt, "pkce"), time.Now())
		if err != nil {
			rt.Fatal(err)
		}
		enc, err := codec.Encode(st)
		if err != nil {
			rt.Fatal(err)
		}
		got, err := codec.Decode(enc)
		if err != nil || got != st {
			rt.Fatalf("round trip: %v", err)
		}
		b := []byte(enc)
		i := rapid.IntRange(0, len(b)-1).Draw(rt, "pos")
		alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
		c := alphabet[rapid.IntRange(0, len(alphabet)-1).Draw(rt, "char")]
		if c == b[i] {
			rt.Skip("no change")
		}
		b[i] = c
		if _, err := codec.Decode(string(b)); err == nil {
			// The last character of unpadded base64 may carry unused bits; a change there
			// can decode to the same bytes. Anything else must fail.
			if i != len(b)-1 {
				rt.Fatalf("tampered state accepted (pos %d)", i)
			}
		}
	})
}

func TestStateCookies(t *testing.T) {
	t.Parallel()
	c := StateCookie("v", "/patrol/", true)
	if c.Name != StateCookieName || c.Value != "v" || c.Path != "/patrol" || !c.HttpOnly || !c.Secure ||
		c.SameSite != http.SameSiteLaxMode || c.MaxAge != int(StateTTL/time.Second) {
		t.Fatalf("state cookie = %+v", c)
	}
	cl := ClearStateCookie("", false)
	if cl.MaxAge >= 0 || cl.Path != "/" || cl.Secure || !cl.HttpOnly {
		t.Fatalf("clear cookie = %+v", cl)
	}
	r := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	if StateCookieValue(r) != "" {
		t.Fatal("value without cookie")
	}
	r.AddCookie(c)
	if StateCookieValue(r) != "v" {
		t.Fatal("cookie value not read")
	}
}
