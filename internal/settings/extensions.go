package settings

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Defaults of proxy extension backends (upstream).
const (
	DefaultExtensionConnectionTimeout     = 2 * time.Second
	DefaultExtensionKeepAlive             = 15 * time.Second
	DefaultExtensionIdleConnectionTimeout = 60 * time.Second
	DefaultExtensionMaxIdleConnections    = 30
)

// Extension is one validated proxy extension (API-047), with "$" references resolved.
type Extension struct {
	Name    string
	Backend ExtensionBackend
}

// ExtensionBackend configures the connection to an extension's services.
type ExtensionBackend struct {
	ConnectionTimeout     time.Duration
	KeepAlive             time.Duration
	IdleConnectionTimeout time.Duration
	MaxIdleConnections    int
	Services              []ExtensionService
}

// ExtensionService is one backend service.
type ExtensionService struct {
	URL string
	// Cluster restricts the service to Applications whose destination matches.
	Cluster *ExtensionCluster
	Headers []ExtensionHeader
}

// ExtensionCluster selects a destination cluster by name or server URL.
type ExtensionCluster struct {
	Name   string
	Server string
}

// ExtensionHeader is added to every proxied request. Value may come from a Secret.
type ExtensionHeader struct {
	Name  string
	Value Sensitive
}

func (e Extension) clone() Extension {
	out := e
	out.Backend.Services = make([]ExtensionService, len(e.Backend.Services))
	for i, svc := range e.Backend.Services {
		svc.Headers = slices.Clone(svc.Headers)
		if svc.Cluster != nil {
			c := *svc.Cluster
			svc.Cluster = &c
		}
		out.Backend.Services[i] = svc
	}
	return out
}

type extensionsDoc struct {
	Extensions []extensionDoc `json:"extensions"`
}

type extensionDoc struct {
	Name    string     `json:"name"`
	Backend backendDoc `json:"backend"`
}

type backendDoc struct {
	ConnectionTimeout     string       `json:"connectionTimeout,omitempty"`
	KeepAlive             string       `json:"keepAlive,omitempty"`
	IdleConnectionTimeout string       `json:"idleConnectionTimeout,omitempty"`
	MaxIdleConnections    int          `json:"maxIdleConnections,omitempty"`
	Services              []serviceDoc `json:"services"`
}

type serviceDoc struct {
	URL     string `json:"url"`
	Cluster *struct {
		Server string `json:"server"`
		Name   string `json:"name"`
	} `json:"cluster,omitempty"`
	Headers []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
}

var extensionNameRE = regexp.MustCompile(`^[A-Za-z0-9-_]+$`)

// parseExtensions applies extension.config and extension.config.<name> with per-key
// isolation. Validation follows upstream validateConfigs; a duplicate name across keys is
// reported on the later key, which is dropped.
func (p *parser) parseExtensions(c *cmKeys) {
	src := SourceConfigMap
	var keys []string
	if _, ok := c.get(KeyExtensionConfig); ok {
		keys = append(keys, KeyExtensionConfig)
	}
	keys = append(keys, c.prefixed(KeyExtensionConfig+".")...)
	var all []Extension
	seen := map[string]string{}
	for _, k := range keys {
		var parsed []Extension
		p.isolate(src, k, c.data[k], func(v string) error {
			exts, err := p.parseExtensionKey(k, v)
			if err != nil {
				return err
			}
			for _, e := range exts {
				if prev, dup := seen[e.Name]; dup {
					return fmt.Errorf("validation error: duplicated extension found in the configs for %q (also defined by %s)", e.Name, prev)
				}
			}
			parsed = exts
			return nil
		})
		for _, e := range parsed {
			seen[e.Name] = k
		}
		all = append(all, parsed...)
		if v, ok := p.s.accepted[src][k]; ok {
			p.s.extensionConfigs[strings.TrimPrefix(strings.TrimPrefix(k, KeyExtensionConfig), ".")] = v
		}
	}
	p.s.extensions = all
}

// parseExtensionKey parses one key: extension.config holds {extensions: [...]}, and
// extension.config.<name> holds the backend of extension <name>.
func (p *parser) parseExtensionKey(key, v string) ([]Extension, error) {
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(v), &raw); err != nil {
		return nil, fmt.Errorf("invalid extension config: %w", err)
	}
	b, err := yaml.Marshal(p.replaceMapSecrets(SourceConfigMap, key, raw))
	if err != nil {
		return nil, fmt.Errorf("invalid extension config: %w", err)
	}
	var docs []extensionDoc
	if key == KeyExtensionConfig {
		var d extensionsDoc
		if err := yaml.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("invalid parsed extension config: %w", err)
		}
		docs = d.Extensions
	} else {
		var bd backendDoc
		name := strings.TrimPrefix(key, KeyExtensionConfig+".")
		if err := yaml.Unmarshal(b, &bd); err != nil {
			return nil, fmt.Errorf("invalid parsed backend extension config for extension %s: %w", name, err)
		}
		docs = []extensionDoc{{Name: name, Backend: bd}}
	}
	names := map[string]bool{}
	out := make([]Extension, 0, len(docs))
	for _, d := range docs {
		e, err := buildExtension(d)
		if err != nil {
			return nil, fmt.Errorf("validation error: %w", err)
		}
		if names[e.Name] {
			return nil, fmt.Errorf("validation error: duplicated extension found in the configs for %q", e.Name)
		}
		names[e.Name] = true
		out = append(out, e)
	}
	return out, nil
}

func buildExtension(d extensionDoc) (Extension, error) {
	if d.Name == "" {
		return Extension{}, errors.New("extensions.name must be configured")
	}
	if !extensionNameRE.MatchString(d.Name) {
		return Extension{}, errors.New("invalid extensions.name: only alphanumeric characters, hyphens, and underscores are allowed")
	}
	if len(d.Backend.Services) == 0 {
		return Extension{}, fmt.Errorf("no backend service configured for extension %s", d.Name)
	}
	be := ExtensionBackend{
		ConnectionTimeout:     DefaultExtensionConnectionTimeout,
		KeepAlive:             DefaultExtensionKeepAlive,
		IdleConnectionTimeout: DefaultExtensionIdleConnectionTimeout,
		MaxIdleConnections:    DefaultExtensionMaxIdleConnections,
	}
	for _, f := range []struct {
		name string
		raw  string
		dst  *time.Duration
	}{
		{"connectionTimeout", d.Backend.ConnectionTimeout, &be.ConnectionTimeout},
		{"keepAlive", d.Backend.KeepAlive, &be.KeepAlive},
		{"idleConnectionTimeout", d.Backend.IdleConnectionTimeout, &be.IdleConnectionTimeout},
	} {
		if f.raw == "" {
			continue
		}
		dur, err := time.ParseDuration(f.raw)
		if err != nil || dur < 0 {
			return Extension{}, fmt.Errorf("extension %s: invalid backend.%s %q", d.Name, f.name, f.raw)
		}
		*f.dst = dur
	}
	if d.Backend.MaxIdleConnections < 0 {
		return Extension{}, fmt.Errorf("extension %s: backend.maxIdleConnections must not be negative", d.Name)
	}
	if d.Backend.MaxIdleConnections > 0 {
		be.MaxIdleConnections = d.Backend.MaxIdleConnections
	}
	total := len(d.Backend.Services)
	for _, svc := range d.Backend.Services {
		if svc.URL == "" {
			return Extension{}, errors.New("extensions.backend.services.url must be configured")
		}
		if total > 1 && svc.Cluster == nil {
			return Extension{}, errors.New("extensions.backend.services.cluster must be configured when defining more than one service per extension")
		}
		out := ExtensionService{URL: svc.URL}
		if svc.Cluster != nil {
			if svc.Cluster.Name == "" && svc.Cluster.Server == "" {
				return Extension{}, errors.New("cluster.name or cluster.server must be defined when cluster is provided in the configuration")
			}
			out.Cluster = &ExtensionCluster{Name: svc.Cluster.Name, Server: svc.Cluster.Server}
		}
		for _, h := range svc.Headers {
			if h.Name == "" {
				return Extension{}, errors.New("header.name must be defined when providing service headers in the configuration")
			}
			if h.Value == "" {
				return Extension{}, errors.New("header.value must be defined when providing service headers in the configuration")
			}
			out.Headers = append(out.Headers, ExtensionHeader{Name: h.Name, Value: NewSensitive(h.Value)})
		}
		be.Services = append(be.Services, out)
	}
	return Extension{Name: d.Name, Backend: be}, nil
}
