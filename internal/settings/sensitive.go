package settings

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
)

const redacted = "[redacted]"

// Sensitive holds a secret value (server key, webhook secret, client secret, private key).
// Its formatting, logging and marshalling methods never reveal the value; use Reveal to read
// it.
type Sensitive struct {
	v string
}

// NewSensitive wraps v.
func NewSensitive(v string) Sensitive { return Sensitive{v: v} }

// Reveal returns the secret value.
func (s Sensitive) Reveal() string { return s.v }

// Bytes returns a copy of the secret value as bytes.
func (s Sensitive) Bytes() []byte { return []byte(s.v) }

// IsZero reports whether the value is empty.
func (s Sensitive) IsZero() bool { return s.v == "" }

// Len returns the length of the value in bytes.
func (s Sensitive) Len() int { return len(s.v) }

// Equal compares two values in constant time.
func (s Sensitive) Equal(o Sensitive) bool {
	return subtle.ConstantTimeCompare([]byte(s.v), []byte(o.v)) == 1
}

// String returns "" for an empty value and "[redacted]" otherwise.
func (s Sensitive) String() string {
	if s.v == "" {
		return ""
	}
	return redacted
}

// GoString implements fmt.GoStringer without revealing the value.
func (s Sensitive) GoString() string { return "settings.Sensitive(" + s.String() + ")" }

// Format implements fmt.Formatter so every verb (including %v, %+v, %q, %x) is redacted.
func (s Sensitive) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		_, _ = fmt.Fprint(f, s.GoString())
		return
	}
	_, _ = fmt.Fprint(f, s.String())
}

// LogValue implements slog.LogValuer.
func (s Sensitive) LogValue() slog.Value { return slog.StringValue(s.String()) }

// MarshalText implements encoding.TextMarshaler (used by JSON and YAML encoders).
func (s Sensitive) MarshalText() ([]byte, error) { return []byte(s.String()), nil }
