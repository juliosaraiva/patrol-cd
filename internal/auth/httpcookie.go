package auth

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// SessionCookieName is the session cookie. Tokens larger than one cookie are split:
	// the first cookie holds "<n>:<chunk>" and chunks 1..n-1 are stored in
	// "patrol.token.<i>" (upstream uses "argocd.token" and "argocd.token-<i>").
	SessionCookieName = "patrol.token"
	// MaxCookieChunks bounds the number of cookies a token may be split into
	// (upstream default 20; browsers allow roughly 30-50 cookies per domain).
	MaxCookieChunks = 20
	// maxCookieLength is the per-cookie budget (name, value and attributes), below the
	// common 4096-byte browser limit.
	maxCookieLength = 4093
)

// CookieOptions controls session cookie attributes.
type CookieOptions struct {
	// Path is the server root path ("/" or e.g. "/patrol"). Normalized by CookiePath.
	Path string
	// Secure sets the Secure attribute; disable only for plain-HTTP development servers.
	Secure bool
	// Expires optionally makes the cookie persistent; zero yields a session cookie.
	Expires time.Time
}

// CookiePath normalizes a root path to a cookie path: "" and "/" yield "/", "x/" yields
// "/x".
func CookiePath(rootPath string) string {
	return "/" + strings.Trim(rootPath, "/")
}

func chunkName(i int) string {
	if i == 0 {
		return SessionCookieName
	}
	return SessionCookieName + "." + strconv.Itoa(i)
}

func (o CookieOptions) cookie(name, value string) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     CookiePath(o.Path),
		Expires:  o.Expires,
		Secure:   o.Secure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
}

// splitToken splits token into cookie values. It returns an error when the token needs
// more than MaxCookieChunks cookies.
func splitToken(token string, opts CookieOptions) ([]string, error) {
	if token == "" {
		return nil, fmt.Errorf("%w: empty token", ErrInvalidCookie)
	}
	for i := 0; i < len(token); i++ {
		if !isJWTChar(token[i]) {
			return nil, fmt.Errorf("%w: token contains characters outside the JWT alphabet", ErrInvalidCookie)
		}
	}
	// Budget computed for the longest chunk name plus the "<n>:" prefix.
	overhead := len(opts.cookie(chunkName(MaxCookieChunks-1), "").String()) + len(strconv.Itoa(MaxCookieChunks)) + 1
	size := maxCookieLength - overhead
	if size < 256 {
		return nil, fmt.Errorf("%w: cookie attributes too long", ErrInvalidCookie)
	}
	n := (len(token) + size - 1) / size
	if n > MaxCookieChunks {
		return nil, fmt.Errorf("%w: token of %d bytes needs %d cookies, max %d", ErrTokenTooLarge, len(token), n, MaxCookieChunks)
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		chunk := token[i*size : min((i+1)*size, len(token))]
		if i == 0 && n > 1 {
			chunk = strconv.Itoa(n) + ":" + chunk
		}
		out = append(out, chunk)
	}
	return out, nil
}

// SetSessionCookie writes token as one or more HttpOnly, SameSite=Strict cookies. When r
// is non-nil, chunk cookies left over from a previous, larger token are expired.
func SetSessionCookie(w http.ResponseWriter, r *http.Request, token string, opts CookieOptions) error {
	values, err := splitToken(token, opts)
	if err != nil {
		return err
	}
	for i, v := range values {
		http.SetCookie(w, opts.cookie(chunkName(i), v))
	}
	if r != nil {
		for _, c := range r.Cookies() {
			if i, ok := chunkIndex(c.Name); ok && i >= len(values) {
				http.SetCookie(w, expired(opts, c.Name))
			}
		}
	}
	return nil
}

// ClearSessionCookie expires the session cookie and every chunk cookie present in r
// (r may be nil, in which case only the first cookie is expired; orphaned chunks are
// ignored by TokenFromRequest).
func ClearSessionCookie(w http.ResponseWriter, r *http.Request, opts CookieOptions) {
	http.SetCookie(w, expired(opts, SessionCookieName))
	if r == nil {
		return
	}
	for _, c := range r.Cookies() {
		if i, ok := chunkIndex(c.Name); ok && i > 0 {
			http.SetCookie(w, expired(opts, c.Name))
		}
	}
}

func expired(opts CookieOptions, name string) *http.Cookie {
	c := opts.cookie(name, "")
	c.Expires = time.Unix(0, 0)
	c.MaxAge = -1
	return c
}

func chunkIndex(name string) (int, bool) {
	if name == SessionCookieName {
		return 0, true
	}
	rest, ok := strings.CutPrefix(name, SessionCookieName+".")
	if !ok {
		return 0, false
	}
	i, err := strconv.Atoi(rest)
	if err != nil || i < 1 || i >= MaxCookieChunks || strconv.Itoa(i) != rest {
		return 0, false
	}
	return i, true
}

// joinCookies reassembles a token split by SetSessionCookie.
func joinCookies(r *http.Request) (string, error) {
	first, err := r.Cookie(SessionCookieName)
	if err != nil {
		return "", ErrNoToken
	}
	n := 1
	value := first.Value
	if countStr, rest, found := strings.Cut(value, ":"); found {
		n, err = strconv.Atoi(countStr)
		if err != nil || n < 2 || n > MaxCookieChunks || strconv.Itoa(n) != countStr {
			return "", fmt.Errorf("%w: bad chunk count", ErrInvalidCookie)
		}
		value = rest
	}
	var sb strings.Builder
	sb.WriteString(value)
	for i := 1; i < n; i++ {
		c, err := r.Cookie(chunkName(i))
		if err != nil {
			return "", fmt.Errorf("%w: missing chunk %d of %d", ErrInvalidCookie, i, n)
		}
		if sb.Len()+len(c.Value) > MaxTokenLength {
			return "", ErrTokenTooLarge
		}
		sb.WriteString(c.Value)
	}
	if sb.Len() == 0 || sb.Len() > MaxTokenLength {
		return "", fmt.Errorf("%w: empty or oversized token", ErrInvalidCookie)
	}
	return sb.String(), nil
}

// TokenFromRequest returns the bearer token from "Authorization: Bearer <jwt>" (preferred,
// scheme matched case-insensitively) or from the session cookie(s). It only checks the
// token's shape (three dot-separated segments, size bound); verification is the caller's
// job. It returns an error wrapping ErrNoToken when no token is present.
func TokenFromRequest(r *http.Request) (string, error) {
	for _, h := range r.Header.Values("Authorization") {
		scheme, tok, ok := strings.Cut(strings.TrimSpace(h), " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			tok = strings.TrimSpace(tok)
			if looksLikeJWT(tok) {
				return tok, nil
			}
		}
	}
	tok, err := joinCookies(r)
	if err != nil {
		return "", err
	}
	if !looksLikeJWT(tok) {
		return "", fmt.Errorf("%w: not a JWT", ErrInvalidCookie)
	}
	return tok, nil
}

func looksLikeJWT(s string) bool {
	if s == "" || len(s) > MaxTokenLength {
		return false
	}
	parts := strings.Split(s, ".")
	return len(parts) == 3 && parts[0] != "" && parts[1] != ""
}

// isJWTChar reports whether c belongs to the base64url alphabet or is the JWS segment
// separator; such bytes are valid in cookie values without quoting.
func isJWTChar(c byte) bool {
	return c == '.' || c == '-' || c == '_' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}
