package extensions

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// Transport timeouts not configurable per extension (upstream values).
const (
	tlsHandshakeTimeout   = 10 * time.Second
	expectContinueTimeout = time.Second
)

// clusterKey selects a backend service by destination cluster name and/or server URL.
type clusterKey struct{ name, server string }

// backend is one configured service of an extension.
type backend struct {
	target  *url.URL
	headers []settings.ExtensionHeader // values never logged
}

// extension is one registered proxy extension.
type extension struct {
	name string
	// single is set when the extension has exactly one service, used for every
	// Application whatever its destination (upstream).
	single    *backend
	byCluster map[clusterKey]*backend
	transport *http.Transport
}

// registry is the set of extensions built from one configuration.
type registry struct {
	fingerprint string
	extensions  map[string]*extension
}

func (r *registry) close() {
	if r == nil {
		return
	}
	for _, e := range r.extensions {
		e.transport.CloseIdleConnections()
	}
}

// fingerprintOf digests the extension configuration (header values included, so a rotated
// Secret rebuilds the registry; the digest never leaves the process).
func fingerprintOf(exts []settings.Extension) string {
	h := sha256.New()
	for _, e := range exts {
		_, _ = fmt.Fprintf(h, "ext %q %d %d %d %d\n", e.Name, e.Backend.ConnectionTimeout, e.Backend.KeepAlive,
			e.Backend.IdleConnectionTimeout, e.Backend.MaxIdleConnections)
		for _, svc := range e.Backend.Services {
			_, _ = fmt.Fprintf(h, "svc %q", svc.URL)
			if svc.Cluster != nil {
				_, _ = fmt.Fprintf(h, " cluster %q %q", svc.Cluster.Name, svc.Cluster.Server)
			}
			for _, hd := range svc.Headers {
				_, _ = fmt.Fprintf(h, " header %q %q", hd.Name, hd.Value.Reveal())
			}
			_, _ = h.Write([]byte("\n"))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// parseTarget validates a backend URL: http or https with a host. Credentials, a query or
// a fragment are refused; the path is ignored, as upstream (the request path is forwarded).
func parseTarget(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid backend URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("backend URL scheme must be http or https")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("backend URL has no host")
	}
	if u.User != nil {
		return nil, fmt.Errorf("backend URL must not contain credentials (use headers)")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("backend URL must not contain a query or fragment")
	}
	return &url.URL{Scheme: scheme, Host: u.Host}, nil
}

func normalizeServer(s string) string { return strings.TrimRight(s, "/") }

// newTransport builds the HTTP transport of one extension. Environment proxies are not
// used and redirects are never followed (the reverse proxy returns them to the caller).
func newTransport(b settings.ExtensionBackend, responseHeaderTimeout time.Duration) *http.Transport {
	maxIdle := b.MaxIdleConnections
	return &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   b.ConnectionTimeout,
			KeepAlive: b.KeepAlive,
		}).DialContext,
		MaxIdleConns:          maxIdle,
		MaxIdleConnsPerHost:   maxIdle,
		IdleConnTimeout:       b.IdleConnectionTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ForceAttemptHTTP2:     true,
	}
}

// buildRegistry registers every valid extension. An extension whose services cannot be
// registered (invalid URL, duplicated cluster selector) is skipped and logged, never
// partially registered; the others keep working.
func buildRegistry(exts []settings.Extension, responseHeaderTimeout time.Duration, log *slog.Logger) *registry {
	reg := &registry{fingerprint: fingerprintOf(exts), extensions: map[string]*extension{}}
	for _, e := range exts {
		ext, err := buildExtension(e, responseHeaderTimeout)
		if err != nil {
			log.Warn("proxy extension not registered", slog.String("extension", e.Name), slog.String("reason", err.Error()))
			continue
		}
		reg.extensions[e.Name] = ext
	}
	return reg
}

func buildExtension(e settings.Extension, responseHeaderTimeout time.Duration) (*extension, error) {
	if len(e.Backend.Services) == 0 {
		return nil, fmt.Errorf("no backend service configured")
	}
	ext := &extension{name: e.Name, byCluster: map[clusterKey]*backend{}, transport: newTransport(e.Backend, responseHeaderTimeout)}
	for i, svc := range e.Backend.Services {
		target, err := parseTarget(svc.URL)
		if err != nil {
			return nil, fmt.Errorf("service %d: %w", i, err)
		}
		be := &backend{target: target, headers: svc.Headers}
		if len(e.Backend.Services) == 1 {
			ext.single = be
			break
		}
		if svc.Cluster == nil {
			return nil, fmt.Errorf("service %d: cluster must be configured when defining more than one service", i)
		}
		name, server := svc.Cluster.Name, normalizeServer(svc.Cluster.Server)
		var keys []clusterKey
		if name != "" {
			keys = append(keys, clusterKey{name: name})
		}
		if server != "" {
			keys = append(keys, clusterKey{server: server})
		}
		if name != "" && server != "" {
			keys = append(keys, clusterKey{name: name, server: server})
		}
		for _, k := range keys {
			if _, dup := ext.byCluster[k]; dup {
				return nil, fmt.Errorf("duplicated proxy configuration found for cluster name %q server %q", k.name, k.server)
			}
			ext.byCluster[k] = be
		}
	}
	return ext, nil
}

// find selects the backend for an Application destination: the single service, else the
// service registered for the destination's name or server as written in the Application
// (upstream), else for the name or server of the resolved cluster (Patrol addition: an
// Application addressing its cluster by server still reaches a service selected by name).
func (e *extension) find(dest v1alpha1.ApplicationDestination, cluster *v1alpha1.Cluster) *backend {
	if e.single != nil {
		return e.single
	}
	keys := []clusterKey{{name: dest.Name, server: normalizeServer(dest.Server)}}
	if cluster != nil {
		keys = append(keys,
			clusterKey{name: cluster.Spec.Name, server: normalizeServer(cluster.Spec.Server)},
			clusterKey{name: cluster.Spec.Name},
			clusterKey{server: normalizeServer(cluster.Spec.Server)})
	}
	for _, k := range keys {
		if k.name == "" && k.server == "" {
			continue
		}
		if be, ok := e.byCluster[k]; ok {
			return be
		}
	}
	return nil
}
