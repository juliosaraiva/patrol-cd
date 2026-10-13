package auth

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"

	"golang.org/x/crypto/bcrypt"
)

const (
	// DefaultPasswordPattern is the password policy used when the settings ConfigMap key
	// "passwordPattern" is unset or empty (upstream default).
	DefaultPasswordPattern = `^.{8,32}$`
	// DefaultBcryptCost is the bcrypt work factor used for new hashes. It is also the
	// minimum accepted by HashPasswordWithCost.
	DefaultBcryptCost = bcrypt.DefaultCost
	// MaxPasswordBytes is the bcrypt input limit. Longer passwords are rejected rather
	// than silently truncated.
	MaxPasswordBytes = 72
)

// minBcryptCost is the lowest cost HashPasswordWithCost accepts. It is a variable only so
// that tests in this package can lower it from TestMain (before any test runs).
var minBcryptCost = DefaultBcryptCost

// HashPassword hashes password with bcrypt at DefaultBcryptCost.
func HashPassword(password string) (string, error) {
	return HashPasswordWithCost(password, DefaultBcryptCost)
}

// HashPasswordWithCost hashes password with bcrypt. Costs below DefaultBcryptCost are
// raised to it, matching upstream behavior. Blank passwords and passwords longer than
// MaxPasswordBytes are rejected.
func HashPasswordWithCost(password string, cost int) (string, error) {
	if password == "" {
		return "", ErrBlankPassword
	}
	if len(password) > MaxPasswordBytes {
		return "", fmt.Errorf("%w: longer than %d bytes", ErrPasswordPolicy, MaxPasswordBytes)
	}
	cost = max(cost, minBcryptCost)
	if cost > bcrypt.MaxCost {
		return "", fmt.Errorf("bcrypt cost %d exceeds maximum %d", cost, bcrypt.MaxCost)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(h), nil
}

// VerifyPassword reports whether password matches the bcrypt hash. The comparison is
// constant-time with respect to the password content. Blank passwords and empty hashes
// never match.
func VerifyPassword(password, hash string) bool {
	if password == "" || hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// PasswordNeedsRehash reports whether hash was produced with a cost lower than
// DefaultBcryptCost (or is not a bcrypt hash at all).
func PasswordNeedsRehash(hash string) bool {
	c, err := bcrypt.Cost([]byte(hash))
	return err != nil || c < DefaultBcryptCost
}

// PasswordPolicy validates new passwords against the configured regular expression.
// Patterns use RE2 syntax, so matching is linear-time.
type PasswordPolicy struct {
	pattern string
	re      *regexp.Regexp
}

// PasswordPolicyError describes a password rejected by a PasswordPolicy. It wraps
// ErrPasswordPolicy.
type PasswordPolicyError struct {
	Pattern string
}

func (e *PasswordPolicyError) Error() string {
	return "new password does not match the following expression: " + e.Pattern
}

// Unwrap returns ErrPasswordPolicy.
func (e *PasswordPolicyError) Unwrap() error { return ErrPasswordPolicy }

// NewPasswordPolicy compiles pattern; an empty pattern selects DefaultPasswordPattern.
func NewPasswordPolicy(pattern string) (*PasswordPolicy, error) {
	if pattern == "" {
		pattern = DefaultPasswordPattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile password pattern: %w", err)
	}
	return &PasswordPolicy{pattern: pattern, re: re}, nil
}

// PasswordPolicyFromSettings builds the policy from the settings ConfigMap data
// (key "passwordPattern").
func PasswordPolicyFromSettings(cm map[string]string) (*PasswordPolicy, error) {
	return NewPasswordPolicy(cm[KeyPasswordPattern])
}

// Pattern returns the effective regular expression.
func (p *PasswordPolicy) Pattern() string { return p.pattern }

// Validate checks password against the policy and the bcrypt length limit.
func (p *PasswordPolicy) Validate(password string) error {
	if password == "" {
		return ErrBlankPassword
	}
	if len(password) > MaxPasswordBytes {
		return fmt.Errorf("%w: longer than %d bytes", ErrPasswordPolicy, MaxPasswordBytes)
	}
	if !p.re.MatchString(password) {
		return &PasswordPolicyError{Pattern: p.pattern}
	}
	return nil
}

// InitialPasswordLength is the length of generated initial admin passwords (upstream).
const InitialPasswordLength = 16

const initialPasswordAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-"

// GenerateInitialPassword returns a uniformly random InitialPasswordLength-character
// password over [0-9A-Za-z-] (the upstream initial admin password alphabet). Bootstrap
// code stores it in the initial-admin Secret and its hash with SecretWithPassword.
func GenerateInitialPassword() (string, error) {
	b := make([]byte, InitialPasswordLength)
	limit := big.NewInt(int64(len(initialPasswordAlphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("generate password: %w", err)
		}
		b[i] = initialPasswordAlphabet[n.Int64()]
	}
	return string(b), nil
}
