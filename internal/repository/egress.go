package repository

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"golang.org/x/oauth2"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// Token requests run in the controller, the API server and the commit-server, which hold
// Kubernetes credentials, and some of their endpoints are named by repository objects. The
// endpoint policy below bounds where those requests may go and what of their responses may
// be reflected into errors (status conditions, API responses).

// Built-in endpoint lists (see the Options fields that extend or replace them).
var (
	// googleTokenHosts are Google's OAuth 2.0 and IAM credentials endpoints.
	googleTokenHosts = []string{"oauth2.googleapis.com", "accounts.google.com", "www.googleapis.com", "iamcredentials.googleapis.com"}
	// azureAuthorityHosts are the Microsoft Entra authorities of the public, US Government
	// and China clouds.
	azureAuthorityHosts = []string{"login.microsoftonline.com", "login.microsoftonline.us", "login.chinacloudapi.cn", "login.partner.microsoftonline.cn"}
	// defaultACRHosts are the Azure Container Registry domains of the public, China and US
	// Government clouds.
	defaultACRHosts = []string{"*.azurecr.io", "*.azurecr.cn", "*.azurecr.us"}
	// defaultAzureDevOpsHosts are the Azure DevOps Services git hosts.
	defaultAzureDevOpsHosts = []string{"dev.azure.com", "*.visualstudio.com"}
)

// errEndpointNotAllowed marks a token request refused by the endpoint policy. Errors wrapping
// it are configuration errors.
var errEndpointNotAllowed = errors.New("endpoint not allowed by the token endpoint policy")

// maxIssuerMessage bounds the error text kept from a token issuer library.
const maxIssuerMessage = 512

// hostPatterns is a list of lower-cased host names ("host") and domain wildcards
// ("*.domain", any subdomain of domain but not domain itself).
type hostPatterns []string

// compileHostPatterns validates and normalizes host patterns; field names the option in
// errors.
// The result is never nil, so an empty list allows nothing.
func compileHostPatterns(field string, in ...[]string) (hostPatterns, error) {
	out := hostPatterns{}
	for _, list := range in {
		for _, raw := range list {
			p := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
			rest := strings.TrimPrefix(p, "*.")
			if _, err := netip.ParseAddr(rest); err == nil && rest == p {
				out = append(out, p)
				continue
			}
			if rest == "" || strings.ContainsAny(rest, "*/:@?#[] \t") || strings.HasPrefix(rest, ".") || strings.Contains(rest, "..") {
				return nil, fmt.Errorf("repository resolver: invalid %s entry %q (want a host name or *.domain)", field, raw)
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// match reports whether the host name (no port) is allowed.
func (h hostPatterns) match(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	for _, p := range h {
		if suffix, ok := strings.CutPrefix(p, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		} else if host == p {
			return true
		}
	}
	return false
}

// checkEndpointURL validates an endpoint URL named by a repository object or credential:
// https with a host, and an allowed host name when allow is non-nil.
func checkEndpointURL(what, raw string, allow hostPatterns) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return configError(ErrInvalidConfiguration, "%s is not a valid URL", what)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%s must use https, got scheme %q: %w: %w", what, u.Scheme, ErrInvalidConfiguration, errEndpointNotAllowed)
	}
	if u.User != nil {
		return fmt.Errorf("%s must not contain user info: %w: %w", what, ErrInvalidConfiguration, errEndpointNotAllowed)
	}
	if allow != nil && !allow.match(u.Hostname()) {
		return fmt.Errorf("%s host %q is not allowed: %w: %w", what, u.Hostname(), ErrInvalidConfiguration, errEndpointNotAllowed)
	}
	return nil
}

// egressGuard refuses connections that token requests must never make: link-local
// addresses (cloud instance metadata services), unspecified and multicast addresses,
// loopback unless allowed, and the operator's denied networks. It checks the address
// actually dialed (after DNS resolution, so a rebinding name cannot evade it); with a proxy
// that is the proxy's address.
type egressGuard struct {
	allowLoopback bool
	denied        []netip.Prefix
}

func (g *egressGuard) allowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	switch {
	case !ip.IsValid(), ip.IsUnspecified(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return false
	case ip.IsLoopback() && !g.allowLoopback:
		return false
	}
	for _, p := range g.denied {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// control is a net.Dialer Control function.
func (g *egressGuard) control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !g.allowed(ip) {
		return fmt.Errorf("connection to %s refused: %w", host, errEndpointNotAllowed)
	}
	return nil
}

// dialer returns a dialer applying the guard (http.DefaultTransport's timeouts).
func (g *egressGuard) dialer() *net.Dialer {
	return &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: g.control}
}

// policyTransport enforces the endpoint policy on every request of a token client,
// redirects included: https only and, when allow is non-nil, an allowed host name.
type policyTransport struct {
	base  http.RoundTripper
	allow hostPatterns
	what  string
}

func (t *policyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var err error
	switch {
	case req.URL.Scheme != "https":
		err = fmt.Errorf("%s: scheme %q is not allowed: %w", t.what, req.URL.Scheme, errEndpointNotAllowed)
	case t.allow != nil && !t.allow.match(req.URL.Hostname()):
		err = fmt.Errorf("%s: host %q is not allowed: %w", t.what, req.URL.Hostname(), errEndpointNotAllowed)
	}
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	return t.base.RoundTrip(req)
}

// issuerErr replaces the message of an issuer library error. Unwrap keeps errors.Is/As.
type issuerErr struct {
	msg string
	err error
}

func (e *issuerErr) Error() string { return e.msg }
func (e *issuerErr) Unwrap() error { return e.err }

// safeCode matches OAuth error codes and Entra AADSTS identifiers worth reporting.
var safeCode = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// issuerError returns err without text an HTTP server sent. Token endpoints may be named by
// repository objects, and messages reach status conditions and API responses, so a
// response body is never reflected: only the status code and a short error code remain.
// Errors of other kinds are bounded to maxIssuerMessage bytes.
func issuerError(err error) error {
	if err == nil {
		return nil
	}
	var (
		re *oauth2.RetrieveError
		ae *azidentity.AuthenticationFailedError
		se *scm.Error
	)
	switch {
	case errors.As(err, &re):
		msg := "the token endpoint refused the request"
		if re.Response != nil {
			msg = fmt.Sprintf("the token endpoint returned HTTP %d", re.Response.StatusCode)
		}
		code := re.ErrorCode
		if code == "" {
			code = jsonErrorCode(re.Body)
		}
		if safeCode.MatchString(code) {
			msg += fmt.Sprintf(" (error %q)", code)
		}
		return &issuerErr{msg: msg, err: err}
	case errors.As(err, &ae):
		if ae.RawResponse == nil {
			return &issuerErr{msg: truncateMessage(ae.Error()), err: err}
		}
		msg := fmt.Sprintf("Microsoft Entra ID returned HTTP %d", ae.RawResponse.StatusCode)
		if body, perr := runtime.Payload(ae.RawResponse); perr == nil {
			if code := jsonErrorCode(body); safeCode.MatchString(code) {
				msg += fmt.Sprintf(" (error %q)", code)
			}
		}
		return &issuerErr{msg: msg, err: err}
	case errors.As(err, &se) && se.StatusCode != 0 && (se.Kind != scm.KindNotFound || se.Op != "FindInstallation"):
		// The FindInstallation "not found" message is composed locally (upstream wording);
		// every other HTTP failure may carry server text (GitHub's "message" field).
		return &issuerErr{msg: fmt.Sprintf("%s: %s: %s (HTTP %d)", se.Provider, se.Op, se.Kind, se.StatusCode), err: err}
	}
	if msg := err.Error(); len(msg) > maxIssuerMessage {
		return &issuerErr{msg: truncateMessage(msg), err: err}
	}
	return err
}

// jsonErrorCode returns the "error" member of a JSON object body ("" otherwise).
func jsonErrorCode(body []byte) string {
	var v struct {
		Error any `json:"error"`
	}
	if len(body) == 0 || json.Unmarshal(body, &v) != nil {
		return ""
	}
	s, _ := v.Error.(string)
	return s
}

func truncateMessage(s string) string {
	if len(s) <= maxIssuerMessage {
		return s
	}
	return strings.ToValidUTF8(s[:maxIssuerMessage], "") + "..."
}

// tokenFailure wraps a failed token request: refusals by the endpoint policy are
// configuration errors, the rest ErrTokenUnavailable with the issuer's text removed.
func tokenFailure(what string, err error) error {
	if errors.Is(err, errEndpointNotAllowed) {
		if errors.Is(err, ErrInvalidConfiguration) {
			return fmt.Errorf("%s: %w", what, err)
		}
		return fmt.Errorf("%s: %w: %w", what, ErrInvalidConfiguration, issuerError(err))
	}
	return fmt.Errorf("%s: %w: %w", what, ErrTokenUnavailable, issuerError(err))
}
