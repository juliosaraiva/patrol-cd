package rest

import (
	"net/http"
	"net/url"
	"strings"
)

// CSRFHeader and CSRFHeaderValue are the header every cookie-authenticated state-changing
// request must send (ADR-0009). Browsers cannot attach custom headers to cross-site
// requests without a CORS preflight, which patrol-server never approves.
const (
	CSRFHeader      = "X-Requested-With"
	CSRFHeaderValue = "patrol"
)

// CSRFConfig configures CSRF protection of cookie-authenticated mutations.
type CSRFConfig struct {
	// TrustedOrigins returns additional origins ("https://patrol.example.com") accepted in
	// Origin/Referer, typically the settings url and additionalUrls. The request's own
	// Host is always accepted.
	TrustedOrigins func() []string
}

// isSafeMethod reports whether method never changes state.
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// checkCSRF enforces, for a cookie-authenticated state-changing request: the
// X-Requested-With: patrol header, and that the Origin header (or, without Origin, the
// Referer) when present names the request's host or a trusted origin. Requests that
// carry neither header (non-browser clients, privacy-stripping proxies) rely on the
// custom header alone.
func (c CSRFConfig) check(r *http.Request) error {
	if r.Header.Get(CSRFHeader) != CSRFHeaderValue {
		return PermissionDenied("cookie-authenticated requests that change state must send the header %s: %s", CSRFHeader, CSRFHeaderValue)
	}
	var trusted []string
	if c.TrustedOrigins != nil {
		trusted = c.TrustedOrigins()
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if !originAllowed(origin, r.Host, trusted) {
			return PermissionDenied("cross-origin request rejected")
		}
		return nil
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		if err != nil || u.Host == "" || !originAllowed(u.Scheme+"://"+u.Host, r.Host, trusted) {
			return PermissionDenied("cross-origin request rejected")
		}
	}
	return nil
}

func originAllowed(origin, host string, trusted []string) bool {
	if origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if strings.EqualFold(u.Host, host) {
		return true
	}
	for _, t := range trusted {
		tu, err := url.Parse(t)
		if err != nil || tu.Host == "" {
			continue
		}
		if strings.EqualFold(tu.Scheme, u.Scheme) && strings.EqualFold(tu.Host, u.Host) {
			return true
		}
	}
	return false
}
