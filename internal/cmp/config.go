package cmp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/render/cmpwire"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

const (
	// ConfigManagementPluginKind is the only accepted kind of plugin.yaml.
	ConfigManagementPluginKind = "ConfigManagementPlugin"
	// APIVersionArgo is the upstream apiVersion, accepted for compatibility.
	APIVersionArgo = "argoproj.io/v1alpha1"
	// ConfigFileName is the plugin configuration file name inside the config directory.
	ConfigFileName = "plugin.yaml"
	// DefaultConfigDir is the default plugin configuration directory
	// (upstream /home/argocd/cmp-server/config).
	DefaultConfigDir = "/home/patrol/cmp-server/config"
	// MaxConfigBytes bounds plugin.yaml.
	MaxConfigBytes = 1 << 20
)

// APIVersionPatrol is the Patrol apiVersion of plugin.yaml. plugin.yaml is not a
// Kubernetes resource; the group only namespaces the format.
var APIVersionPatrol = v1alpha1.GroupVersion.String()

// PluginConfig is plugin.yaml. It looks like a Kubernetes object but is not a custom
// resource.
type PluginConfig struct {
	APIVersion string         `json:"apiVersion,omitempty"`
	Kind       string         `json:"kind,omitempty"`
	Metadata   PluginMetadata `json:"metadata"`
	Spec       PluginSpec     `json:"spec"`

	warnings []string
}

// PluginMetadata holds the plugin name. Other ObjectMeta fields are accepted and
// ignored.
type PluginMetadata struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// PluginSpec is the plugin behavior.
type PluginSpec struct {
	// Version, when set, makes the socket (and the name Applications select)
	// "<name>-<version>".
	Version string `json:"version,omitempty"`
	// Init runs before Generate in the application directory; its output is ignored.
	Init Command `json:"init,omitempty"`
	// Generate prints the manifests (YAML or JSON object stream). Required.
	Generate Command `json:"generate"`
	// Discover decides whether the plugin supports an application directory.
	Discover Discover `json:"discover,omitempty"`
	// Parameters are the parameter announcements.
	Parameters Parameters `json:"parameters,omitempty"`
	// PreserveFileMode keeps the execute bits of received files.
	PreserveFileMode bool `json:"preserveFileMode,omitempty"`
	// ProvideGitCreds is reported to the repo-server, which never forwards repository
	// credentials to plugins (ADR-0007).
	ProvideGitCreds bool `json:"provideGitCreds,omitempty"`
	// LockRepo exists only in the removed v1 plugin format; it is accepted and ignored
	// (the repo-server releases its repository lock once the source is archived).
	LockRepo bool `json:"lockRepo,omitempty"`
}

// Command is an executable with arguments: Command[0] runs with arguments
// Command[1:] followed by Args.
type Command struct {
	Command []string `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
}

// IsSet reports whether the command has an executable.
func (c Command) IsSet() bool { return len(c.Command) > 0 }

// argv returns the full argument vector.
func (c Command) argv() []string {
	out := make([]string, 0, len(c.Command)+len(c.Args))
	out = append(out, c.Command...)
	return append(out, c.Args...)
}

// Discover holds the discovery rules, evaluated in the order FileName, Find.Glob,
// Find.Command; the first one set decides.
type Discover struct {
	// FileName is a filepath.Glob pattern relative to the application directory.
	FileName string `json:"fileName,omitempty"`
	Find     Find   `json:"find,omitempty"`
}

// IsDefined reports whether any discovery rule is set.
func (d Discover) IsDefined() bool {
	return d.FileName != "" || d.Find.Glob != "" || d.Find.IsSet()
}

// Find is a glob with '**' support or a command (exit 0 and non-empty output match).
type Find struct {
	Command
	Glob string `json:"glob,omitempty"`
}

// Parameters are the static announcements and the dynamic announcement command.
type Parameters struct {
	Static  []apiclient.ParameterAnnouncement `json:"static,omitempty"`
	Dynamic Command                           `json:"dynamic,omitempty"`
}

// ReadPluginConfig reads and validates <dir>/plugin.yaml.
func ReadPluginConfig(dir string) (*PluginConfig, error) {
	path := filepath.Join(dir, ConfigFileName)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("error reading plugin configuration file: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("error reading plugin configuration file %s: %w", path, err)
	}
	if len(data) > MaxConfigBytes {
		return nil, fmt.Errorf("invalid plugin configuration file. %s exceeds %d bytes", path, MaxConfigBytes)
	}
	return ParsePluginConfig(data)
}

// ParsePluginConfig parses and validates plugin.yaml content. Unknown fields and
// unexpected apiVersions are accepted (as upstream) and reported by Warnings.
func ParsePluginConfig(data []byte) (*PluginConfig, error) {
	var cfg PluginConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("error unmarshalling plugin configuration file: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var strict PluginConfig
	if err := yaml.UnmarshalStrict(data, &strict); err != nil {
		cfg.warnings = append(cfg.warnings, "plugin configuration file has fields Patrol ignores: "+err.Error())
	}
	switch cfg.APIVersion {
	case APIVersionArgo, APIVersionPatrol:
	default:
		cfg.warnings = append(cfg.warnings, fmt.Sprintf("plugin configuration file has unexpected apiVersion %q (expected %s or %s)", cfg.APIVersion, APIVersionArgo, APIVersionPatrol))
	}
	if cfg.Spec.LockRepo {
		cfg.warnings = append(cfg.warnings, "spec.lockRepo is ignored: the repository lock is released once the source tree is archived")
	}
	if cfg.Spec.ProvideGitCreds {
		cfg.warnings = append(cfg.warnings, "spec.provideGitCreds is set, but Patrol never forwards repository credentials to plugins")
	}
	return &cfg, nil
}

// Validate checks the fields the server relies on. The first three messages are
// upstream's.
func (c *PluginConfig) Validate() error {
	if c.Metadata.Name == "" {
		return errors.New("invalid plugin configuration file. metadata.name should be non-empty")
	}
	if c.Kind != ConfigManagementPluginKind {
		return fmt.Errorf("invalid plugin configuration file. kind should be %s, found %s", ConfigManagementPluginKind, c.Kind)
	}
	if len(c.Spec.Generate.Command) == 0 {
		return errors.New("invalid plugin configuration file. spec.generate command should be non-empty")
	}
	if err := checkNameComponent("metadata.name", c.Metadata.Name); err != nil {
		return err
	}
	if c.Spec.Version != "" {
		if err := checkNameComponent("spec.version", c.Spec.Version); err != nil {
			return err
		}
	}
	for _, fc := range []struct {
		field string
		cmd   Command
	}{
		{"spec.init", c.Spec.Init},
		{"spec.generate", c.Spec.Generate},
		{"spec.discover.find", c.Spec.Discover.Find.Command},
		{"spec.parameters.dynamic", c.Spec.Parameters.Dynamic},
	} {
		if len(fc.cmd.Command) > 0 && fc.cmd.Command[0] == "" {
			return fmt.Errorf("invalid plugin configuration file. %s command[0] should be non-empty", fc.field)
		}
		for _, a := range fc.cmd.argv() {
			if strings.ContainsRune(a, 0) {
				return fmt.Errorf("invalid plugin configuration file. %s contains a NUL byte", fc.field)
			}
		}
	}
	return nil
}

// checkNameComponent rejects values that would place the socket outside the socket
// directory or produce an unusable file name.
func checkNameComponent(field, v string) error {
	if v == "." || v == ".." || strings.ContainsAny(v, "/\\\x00") {
		return fmt.Errorf("invalid plugin configuration file. %s %q must be a valid file name component", field, v)
	}
	return nil
}

// Warnings lists accepted-but-ignored configuration details found while parsing.
func (c *PluginConfig) Warnings() []string { return append([]string(nil), c.warnings...) }

// PluginName is the name Applications select: "<name>-<version>" or "<name>".
func (c *PluginConfig) PluginName() string {
	if c.Spec.Version != "" {
		return c.Metadata.Name + "-" + c.Spec.Version
	}
	return c.Metadata.Name
}

// SocketName is the socket file name: PluginName() + ".sock".
func (c *PluginConfig) SocketName() string { return c.PluginName() + cmpwire.SocketSuffix }

// IsDiscoveryConfigured reports whether any discovery rule is set.
func (c *PluginConfig) IsDiscoveryConfigured() bool { return c.Spec.Discover.IsDefined() }
