package auth

import (
	"fmt"
	"net/url"
	"path"
	"strings"
	"unicode"
)

const maxRedirectURLLength = 2048

// ValidateRedirectURL checks that redirectURL may be used as a post-login/logout
// redirect target. An empty URL is valid (callers fall back to the base URL).
//
// Semantics follow upstream isValidRedirectURL: the URL must be absolute, its scheme and
// host (including port) must equal those of one of the allowed base URLs, and its cleaned
// path must lie within that base URL's path. Hardening beyond upstream: the path must be
// equal to the base path or below it at a segment boundary ("/patrol" does not allow
// "/patrolx"), host comparison is case-insensitive, and URLs with user info, opaque
// parts, backslashes, whitespace or control characters are rejected.
func ValidateRedirectURL(redirectURL string, allowedBaseURLs []string) error {
	if redirectURL == "" {
		return nil
	}
	if len(redirectURL) > maxRedirectURLLength {
		return fmt.Errorf("%w: too long", ErrInvalidRedirectURL)
	}
	if strings.ContainsRune(redirectURL, '\\') || strings.IndexFunc(redirectURL, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return fmt.Errorf("%w: forbidden character", ErrInvalidRedirectURL)
	}
	r, err := url.Parse(redirectURL)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRedirectURL, err)
	}
	if r.User != nil || r.Opaque != "" || r.Host == "" || (r.Scheme != "https" && r.Scheme != "http") {
		return fmt.Errorf("%w: must be an absolute http(s) URL without user info", ErrInvalidRedirectURL)
	}
	if strings.ContainsAny(r.Path, "\r\n") {
		return fmt.Errorf("%w: forbidden character", ErrInvalidRedirectURL)
	}
	rp := r.Path
	if rp == "" {
		rp = "/"
	}
	rp = path.Clean(rp)
	for _, base := range allowedBaseURLs {
		b, err := url.Parse(base)
		if err != nil || b.Host == "" {
			continue
		}
		if !strings.EqualFold(b.Scheme, r.Scheme) || !strings.EqualFold(b.Host, r.Host) {
			continue
		}
		bp := strings.TrimSuffix(path.Clean("/"+b.Path), "/")
		if bp == "" || rp == bp || strings.HasPrefix(rp, bp+"/") {
			return nil
		}
	}
	return fmt.Errorf("%w: the protocol and host (including port) must match and the path must be within an allowed URL", ErrInvalidRedirectURL)
}

// IsValidRedirectURL is the boolean form of ValidateRedirectURL.
func IsValidRedirectURL(redirectURL string, allowedBaseURLs []string) bool {
	return ValidateRedirectURL(redirectURL, allowedBaseURLs) == nil
}
