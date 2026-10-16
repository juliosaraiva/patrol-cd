package core

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Environment variables of the Endpoints override (test and debugging aid, not a product
// setting).
const (
	// EnvEndpoints lists the endpoints: "repo-server=<url>,controller=<url>".
	EnvEndpoints = "PATROL_CORE_ENDPOINTS"
	// EnvAllowEndpoints must be "true" for EnvEndpoints to be honored.
	EnvAllowEndpoints = "PATROL_CORE_ALLOW_ENDPOINTS"
)

// Endpoints replace the Kubernetes port-forward to the repo-server and the controller
// with fixed URLs. They exist for tests (an in-process repo-server and tree API) and for
// debugging from inside the cluster; URLs may only name loopback addresses or
// cluster-internal host names (*.svc, *.svc.cluster.local), so the internal token is
// never sent to an arbitrary host.
type Endpoints struct {
	// RepoServer and Controller are http(s) base URLs.
	RepoServer string
	Controller string
	// Token is the internal token. Empty reads Secret patrol-internal-token with the
	// caller's credentials; it is never taken from the environment.
	Token string
}

// Validate checks both URLs.
func (e Endpoints) Validate() error {
	var errs []error
	for _, u := range []struct{ name, raw string }{{"repo-server", e.RepoServer}, {"controller", e.Controller}} {
		if err := validateEndpointURL(u.raw); err != nil {
			errs = append(errs, fmt.Errorf("%s endpoint: %w", u.name, err))
		}
	}
	return errors.Join(errs...)
}

func validateEndpointURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL %q must use http or https", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("URL %q must be scheme://host:port", raw)
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n <= 0 || n > 65535 {
			return fmt.Errorf("URL %q has an invalid port", raw)
		}
	}
	if !allowedEndpointHost(u.Hostname()) {
		return fmt.Errorf("host %q is neither a loopback address nor a cluster-internal name (*.svc, *.svc.cluster.local)", u.Hostname())
	}
	return nil
}

// allowedEndpointHost accepts loopback addresses and cluster-internal Service names.
func allowedEndpointHost(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if len(validation.IsDNS1123Subdomain(h)) > 0 {
		return false
	}
	for _, suffix := range []string{".svc", ".svc.cluster.local"} {
		if strings.HasSuffix(h, suffix) && len(h) > len(suffix) {
			return true
		}
	}
	return false
}

// ParseEndpoints parses the EnvEndpoints form "repo-server=<url>,controller=<url>".
func ParseEndpoints(s string) (Endpoints, error) {
	var e Endpoints
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return Endpoints{}, fmt.Errorf("%s: %q is not name=url", EnvEndpoints, part)
		}
		switch strings.TrimSpace(k) {
		case "repo-server":
			e.RepoServer = strings.TrimSpace(v)
		case "controller":
			e.Controller = strings.TrimSpace(v)
		default:
			return Endpoints{}, fmt.Errorf("%s: unknown endpoint %q (repo-server or controller)", EnvEndpoints, k)
		}
	}
	if err := e.Validate(); err != nil {
		return Endpoints{}, fmt.Errorf("%s: %w", EnvEndpoints, err)
	}
	return e, nil
}

// endpointsFromEnv returns the environment override: nil when EnvEndpoints is unset, an
// error when it is set without EnvAllowEndpoints=true or is invalid.
func endpointsFromEnv(lookup func(string) (string, bool)) (*Endpoints, error) {
	if lookup == nil {
		return nil, nil
	}
	raw, ok := lookup(EnvEndpoints)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	allow, _ := lookup(EnvAllowEndpoints)
	if ok, err := strconv.ParseBool(strings.TrimSpace(allow)); err != nil || !ok {
		return nil, fmt.Errorf("%s is a test and debugging setting; it is honored only with %s=true", EnvEndpoints, EnvAllowEndpoints)
	}
	e, err := ParseEndpoints(raw)
	if err != nil {
		return nil, err
	}
	return &e, nil
}
