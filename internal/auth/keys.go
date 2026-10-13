package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MinSigningKeyLength is the minimum HS256 key size in bytes (RFC 7518 §3.2 requires a
// key at least as long as the hash output).
const MinSigningKeyLength = 32

// DefaultSessionDuration is the login session lifetime when "users.session.duration" is
// unset or invalid.
const DefaultSessionDuration = 24 * time.Hour

// SigningKey is an HS256 secret with a key ID derived from it. The secret is never
// exposed through formatting or logging.
type SigningKey struct {
	id     string
	secret []byte
}

// NewSigningKey copies secret and derives its key ID.
func NewSigningKey(secret []byte) (SigningKey, error) {
	if len(secret) == 0 {
		return SigningKey{}, ErrNoSigningKey
	}
	if len(secret) < MinSigningKeyLength {
		return SigningKey{}, fmt.Errorf("%w: %d bytes, need at least %d", ErrWeakSigningKey, len(secret), MinSigningKeyLength)
	}
	s := make([]byte, len(secret))
	copy(s, secret)
	m := hmac.New(sha256.New, s)
	m.Write([]byte("patrol.jwt.kid.v1"))
	return SigningKey{id: base64.RawURLEncoding.EncodeToString(m.Sum(nil))[:16], secret: s}, nil
}

// ID returns the key ID placed in the JWT "kid" header. It is derived with HMAC from the
// secret, so it identifies the key without revealing anything usable about it.
func (k SigningKey) ID() string { return k.id }

// String implements fmt.Stringer without exposing the secret.
func (k SigningKey) String() string { return "SigningKey(" + k.id + ")" }

// GoString implements fmt.GoStringer without exposing the secret.
func (k SigningKey) GoString() string { return k.String() }

// LogValue implements slog.LogValuer without exposing the secret.
func (k SigningKey) LogValue() slog.Value { return slog.StringValue(k.id) }

// KeySet is the current signing key plus keys still accepted for verification.
// Tokens are always signed with Current.
type KeySet struct {
	Current  SigningKey
	Previous []SigningKey
}

// NewKeySet builds a KeySet from raw secrets. Empty previous secrets are ignored.
func NewKeySet(current []byte, previous ...[]byte) (KeySet, error) {
	cur, err := NewSigningKey(current)
	if err != nil {
		return KeySet{}, fmt.Errorf("current signing key: %w", err)
	}
	ks := KeySet{Current: cur}
	for i, p := range previous {
		if len(p) == 0 {
			continue
		}
		k, err := NewSigningKey(p)
		if err != nil {
			return KeySet{}, fmt.Errorf("previous signing key %d: %w", i, err)
		}
		if k.id == cur.id {
			continue
		}
		ks.Previous = append(ks.Previous, k)
	}
	return ks, nil
}

// KeySetFromSecret reads "server.secretkey" and the optional "server.secretkey.previous"
// from the settings Secret data.
func KeySetFromSecret(secret map[string]string) (KeySet, error) {
	cur := secret[KeyServerSecretKey]
	if cur == "" {
		return KeySet{}, fmt.Errorf("%w: %s is missing", ErrNoSigningKey, KeyServerSecretKey)
	}
	return NewKeySet([]byte(cur), []byte(secret[KeyServerSecretKeyPrevious]))
}

func (ks KeySet) lookup(kid string) (SigningKey, bool) {
	if kid == "" {
		return SigningKey{}, false
	}
	if ks.Current.id == kid {
		return ks.Current, true
	}
	for _, k := range ks.Previous {
		if k.id == kid {
			return k, true
		}
	}
	return SigningKey{}, false
}

var simpleDurationRE = regexp.MustCompile(`^(\d+)([smhd])$`)

// ParseSessionDuration parses the "users.session.duration" setting. It accepts the
// upstream "<n><s|m|h|d>" form and any Go duration string. Empty input yields
// DefaultSessionDuration; invalid or non-positive input yields DefaultSessionDuration and
// a non-nil error describing the ignored value.
func ParseSessionDuration(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return DefaultSessionDuration, nil
	}
	var d time.Duration
	if m := simpleDurationRE.FindStringSubmatch(v); m != nil {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return DefaultSessionDuration, fmt.Errorf("invalid %s %q: %w", KeySessionDuration, v, err)
		}
		unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
		if n > int64(1<<62)/int64(unit) {
			return DefaultSessionDuration, fmt.Errorf("invalid %s %q: out of range", KeySessionDuration, v)
		}
		d = time.Duration(n) * unit
	} else {
		var err error
		if d, err = time.ParseDuration(v); err != nil {
			return DefaultSessionDuration, fmt.Errorf("invalid %s %q: %w", KeySessionDuration, v, err)
		}
	}
	if d <= 0 {
		return DefaultSessionDuration, fmt.Errorf("invalid %s %q: must be positive", KeySessionDuration, v)
	}
	return d, nil
}
