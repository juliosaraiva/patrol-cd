package auth

import "errors"

// Token verification errors. Every verification failure caused by the token itself wraps
// ErrInvalidToken, and additionally one of the more specific sentinels below when it
// applies, so callers can map ErrInvalidToken to HTTP 401 and still branch on the cause.
// Errors that do not wrap ErrInvalidToken are infrastructure failures (e.g. the
// revocation store is unreachable) and should be reported as 5xx.
var (
	ErrInvalidToken       = errors.New("invalid token")
	ErrTokenExpired       = errors.New("token expired")
	ErrTokenRevoked       = errors.New("token revoked")
	ErrSessionInvalidated = errors.New("account password changed since token was issued")
	ErrNoToken            = errors.New("no token in request")
	ErrNotRevocable       = errors.New("token has no expiry and must be revoked by deleting its metadata")
)

// Account and login errors.
var (
	ErrAccountNotFound    = errors.New("account not found")
	ErrAccountDisabled    = errors.New("account disabled")
	ErrMissingCapability  = errors.New("account lacks capability")
	ErrInvalidAccountName = errors.New("invalid account name")
	// ErrInvalidCredentials is returned for every login failure that must not reveal
	// whether the user exists: unknown user, wrong password, lockout and concurrency limit.
	// Use errors.As with *LoginError to obtain the internal reason for logs and metrics.
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrNoCredentials      = errors.New("no credentials supplied")
	ErrUsernameTooLong    = errors.New("username is too long")
	ErrBlankPassword      = errors.New("blank passwords are not allowed")
	ErrPasswordPolicy     = errors.New("password does not satisfy policy")
	ErrTokenExists        = errors.New("account already has a token with this id")
	ErrTokenNotFound      = errors.New("token not found")
	ErrCorruptTokenList   = errors.New("stored token list is not valid JSON")
)

// Key, settings and URL errors.
var (
	ErrNoSigningKey       = errors.New("no signing key configured")
	ErrWeakSigningKey     = errors.New("signing key is too short")
	ErrInvalidRedirectURL = errors.New("invalid redirect URL")
	ErrInvalidCookie      = errors.New("invalid session cookie")
	ErrTokenTooLarge      = errors.New("token too large")
	ErrInvalidSubject     = errors.New("invalid token subject")
)
