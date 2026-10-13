package oidc

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/juliosaraiva/patrol-cd/internal/auth"
)

const (
	// StateCookieName holds the encrypted login state between login and callback.
	StateCookieName = "patrol.oauthstate"
	// StateTTL is the lifetime of the login state (upstream: 5 minutes).
	StateTTL = 5 * time.Minute

	stateKeyInfo        = "patrol oidc state cookie v1"
	stateAAD            = "patrol.oauthstate.v1"
	maxStateCookieBytes = 4096
	minServerSecretLen  = 32
)

// LoginState is the per-login data kept in the state cookie.
type LoginState struct {
	State        string `json:"s"`
	Nonce        string `json:"n"`
	PKCEVerifier string `json:"v,omitempty"`
	ReturnURL    string `json:"r,omitempty"`
	RedirectURL  string `json:"cb"`
	IssuedAt     int64  `json:"t"`
}

// NewLoginState generates random state and nonce values (256 bits each) and, when pkce
// is true, a PKCE verifier.
func NewLoginState(redirectURL, returnURL string, pkce bool, now time.Time) (LoginState, error) {
	st := LoginState{RedirectURL: redirectURL, ReturnURL: returnURL, IssuedAt: now.Unix()}
	var err error
	if st.State, err = randomString(32); err != nil {
		return LoginState{}, err
	}
	if st.Nonce, err = randomString(32); err != nil {
		return LoginState{}, err
	}
	if pkce {
		st.PKCEVerifier = oauth2.GenerateVerifier()
	}
	return st, nil
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// StateCodec encrypts and authenticates LoginState values with AES-256-GCM under a key
// derived (HKDF-SHA256) from the server secret. Decoding also accepts keys derived from
// previous server secrets, so a key rotation does not break logins in flight.
type StateCodec struct {
	aeads []cipher.AEAD
	ttl   time.Duration
	now   func() time.Time
}

// NewStateCodec derives the state keys from the current and previous server secrets
// (the "server.secretkey" values). now may be nil.
func NewStateCodec(now func() time.Time, current []byte, previous ...[]byte) (*StateCodec, error) {
	if now == nil {
		now = time.Now
	}
	c := &StateCodec{ttl: StateTTL, now: now}
	for i, secret := range append([][]byte{current}, previous...) {
		if i > 0 && len(secret) == 0 {
			continue
		}
		if len(secret) < minServerSecretLen {
			return nil, fmt.Errorf("%w: server secret shorter than %d bytes", ErrInvalidConfig, minServerSecretLen)
		}
		key, err := hkdf.Key(sha256.New, secret, nil, stateKeyInfo, 32)
		if err != nil {
			return nil, fmt.Errorf("derive state key: %w", err)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("state cipher: %w", err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("state cipher: %w", err)
		}
		c.aeads = append(c.aeads, aead)
	}
	return c, nil
}

// Encode serializes and seals s.
func (c *StateCodec) Encode(s LoginState) (string, error) {
	plain, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode state: %w", err)
	}
	aead := c.aeads[0]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("encode state: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, plain, []byte(stateAAD))
	out := base64.RawURLEncoding.EncodeToString(sealed)
	if len(out) > maxStateCookieBytes {
		return "", fmt.Errorf("encode state: %d bytes exceeds cookie limit", len(out))
	}
	return out, nil
}

// Decode opens value and checks its age. Any failure wraps ErrInvalidState.
func (c *StateCodec) Decode(value string) (LoginState, error) {
	if value == "" || len(value) > maxStateCookieBytes {
		return LoginState{}, fmt.Errorf("%w: missing or oversized", ErrInvalidState)
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return LoginState{}, fmt.Errorf("%w: bad encoding", ErrInvalidState)
	}
	var plain []byte
	opened := false
	for _, aead := range c.aeads {
		ns := aead.NonceSize()
		if len(raw) < ns+aead.Overhead() {
			break
		}
		if p, err := aead.Open(nil, raw[:ns], raw[ns:], []byte(stateAAD)); err == nil {
			plain, opened = p, true
			break
		}
	}
	if !opened {
		return LoginState{}, fmt.Errorf("%w: authentication failed", ErrInvalidState)
	}
	var s LoginState
	if err := json.Unmarshal(plain, &s); err != nil {
		return LoginState{}, fmt.Errorf("%w: bad payload", ErrInvalidState)
	}
	age := c.now().Sub(time.Unix(s.IssuedAt, 0))
	if age > c.ttl || age < -time.Minute {
		return LoginState{}, fmt.Errorf("%w: expired", ErrInvalidState)
	}
	if s.State == "" || s.Nonce == "" || s.RedirectURL == "" {
		return LoginState{}, fmt.Errorf("%w: incomplete", ErrInvalidState)
	}
	return s, nil
}

// StateCookie returns the cookie carrying an encoded state: HttpOnly, SameSite=Lax (it
// must survive the top-level redirect back from the IdP), Secure when secure is true,
// expiring after StateTTL. path should be the server root path.
func StateCookie(value, path string, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     StateCookieName,
		Value:    value,
		Path:     auth.CookiePath(path),
		MaxAge:   int(StateTTL / time.Second),
		Expires:  time.Now().Add(StateTTL),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// ClearStateCookie returns a cookie that deletes the state cookie.
func ClearStateCookie(path string, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     StateCookieName,
		Path:     auth.CookiePath(path),
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// StateCookieValue returns the state cookie value from r, or "".
func StateCookieValue(r *http.Request) string {
	c, err := r.Cookie(StateCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}
