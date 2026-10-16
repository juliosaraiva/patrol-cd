// Package config reads and writes the local configuration file of the patrol CLI
// ($XDG_CONFIG_HOME/patrol/config by default): contexts, servers, users and the
// prompts-enabled preference, with the layout of Argo CD's local config (API-052).
//
// The file holds session tokens, so it is written with mode 0600 (atomically, through a
// temporary file in the same directory) and refused on read when group or other
// permission bits are set.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// File names and environment variables of the configuration.
const (
	// DirName is the directory below $XDG_CONFIG_HOME (or ~/.config).
	DirName = "patrol"
	// FileName is the configuration file name.
	FileName = "config"
	// PrevContextFile records the previous context for `patrol context -`.
	PrevContextFile = ".prev-ctx"
	// EnvConfigDir overrides the configuration directory.
	EnvConfigDir = "PATROL_CONFIG_DIR"
	// EnvConfig overrides the configuration file path (default of --config).
	EnvConfig = "PATROL_CONFIG"
	// MaxFileBytes bounds the configuration file size.
	MaxFileBytes = 4 << 20
)

// redacted replaces token values in String, GoString and LogValue output.
const redacted = "[redacted]"

// LocalConfig is the content of the configuration file.
type LocalConfig struct {
	CurrentContext string       `json:"current-context"`
	Contexts       []ContextRef `json:"contexts"`
	Servers        []Server     `json:"servers"`
	Users          []User       `json:"users"`
	PromptsEnabled bool         `json:"prompts-enabled"`
}

// ContextRef names a server and the user (credentials) to use with it.
type ContextRef struct {
	Name   string `json:"name"`
	Server string `json:"server"`
	User   string `json:"user"`
}

// Context is a resolved context.
type Context struct {
	Name   string
	Server Server
	User   User
}

// Server holds the connection settings of a Patrol server.
type Server struct {
	// Server is the address ("host[:port]" or a URL); "port-forward" for port-forward
	// contexts and "kubernetes" for core-mode contexts.
	Server string `json:"server"`
	// Insecure skips server certificate verification.
	Insecure bool `json:"insecure,omitempty"`
	// GRPCWeb is accepted for compatibility with Argo CD configurations and ignored.
	GRPCWeb bool `json:"grpc-web,omitempty"`
	// GRPCWebRootPath is the server's root path (Argo CD name kept for compatibility).
	GRPCWebRootPath string `json:"grpc-web-root-path,omitempty"`
	// CACertificateAuthorityData is a base64 PEM bundle trusted for the server.
	CACertificateAuthorityData string `json:"certificate-authority-data,omitempty"`
	// ClientCertificateData and ClientCertificateKeyData are a base64 PEM client
	// certificate and key (mutual TLS).
	ClientCertificateData    string `json:"client-certificate-data,omitempty"`
	ClientCertificateKeyData string `json:"client-certificate-key-data,omitempty"`
	// PlainText disables TLS.
	PlainText bool `json:"plain-text,omitempty"`
	// Core talks to Kubernetes directly (core mode).
	Core bool `json:"core,omitempty"`
}

// User holds the credentials of a context.
type User struct {
	Name string `json:"name"`
	// AuthToken is the Patrol session or API token.
	AuthToken string `json:"auth-token,omitempty"`
	// RefreshToken is the identity provider's refresh token of an SSO login.
	RefreshToken string `json:"refresh-token,omitempty"`
}

// String redacts the tokens.
func (u User) String() string {
	return fmt.Sprintf("User{Name:%s, AuthToken:%s, RefreshToken:%s}", u.Name, mask(u.AuthToken), mask(u.RefreshToken))
}

// GoString redacts the tokens.
func (u User) GoString() string { return "config." + u.String() }

// LogValue redacts the tokens.
func (u User) LogValue() slog.Value {
	return slog.GroupValue(slog.String("name", u.Name), slog.Bool("hasAuthToken", u.AuthToken != ""), slog.Bool("hasRefreshToken", u.RefreshToken != ""))
}

func mask(s string) string {
	if s == "" {
		return ""
	}
	return redacted
}

// ErrInsecurePermissions is returned for a configuration file readable by others.
var ErrInsecurePermissions = errors.New("config file has incorrect permission flags")

// Read loads the file at path. It returns nil, nil when the file does not exist.
func Read(path string) (*LocalConfig, error) {
	f, err := os.Open(path) //nolint:gosec // the path is the user's own configuration file
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open config file: %w", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat config file: %w", err)
	}
	if err := checkPermissions(fi); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("config file %s exceeds %d bytes", path, MaxFileBytes)
	}
	var cfg LocalConfig
	if len(bytes.TrimSpace(data)) > 0 {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("failed to parse config file: %w", err)
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks that the current context resolves.
func (l *LocalConfig) Validate() error {
	if l.CurrentContext == "" {
		return nil
	}
	if _, err := l.ResolveContext(l.CurrentContext); err != nil {
		return fmt.Errorf("local config invalid: %w", err)
	}
	return nil
}

// Write stores cfg at path with mode 0600, creating the directory (0700) when needed.
// The file is replaced atomically.
func Write(cfg *LocalConfig, path string) error {
	if cfg == nil {
		return errors.New("nil config")
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary config file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("set config file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write config file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync config file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close config file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("replace config file: %w", err)
	}
	return nil
}

// Delete removes the file at path.
func Delete(path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("delete config file: %w", err)
	}
	return nil
}

// ResolveContext resolves the named context (the current context when name is empty).
func (l *LocalConfig) ResolveContext(name string) (*Context, error) {
	if name == "" {
		if l.CurrentContext == "" {
			return nil, errors.New("local config: current-context unset")
		}
		name = l.CurrentContext
	}
	for _, c := range l.Contexts {
		if c.Name != name {
			continue
		}
		s, err := l.GetServer(c.Server)
		if err != nil {
			return nil, err
		}
		u, err := l.GetUser(c.User)
		if err != nil {
			return nil, err
		}
		return &Context{Name: c.Name, Server: *s, User: *u}, nil
	}
	return nil, fmt.Errorf("Context '%s' undefined", name) //nolint:staticcheck // upstream message
}

// GetServer returns the named server.
func (l *LocalConfig) GetServer(name string) (*Server, error) {
	for i := range l.Servers {
		if l.Servers[i].Server == name {
			s := l.Servers[i]
			return &s, nil
		}
	}
	return nil, fmt.Errorf("Server '%s' undefined", name) //nolint:staticcheck // upstream message
}

// UpsertServer adds or replaces a server.
func (l *LocalConfig) UpsertServer(s Server) {
	for i := range l.Servers {
		if l.Servers[i].Server == s.Server {
			l.Servers[i] = s
			return
		}
	}
	l.Servers = append(l.Servers, s)
}

// RemoveServer removes a server; it reports whether one was removed.
func (l *LocalConfig) RemoveServer(name string) bool {
	for i := range l.Servers {
		if l.Servers[i].Server == name {
			l.Servers = append(l.Servers[:i], l.Servers[i+1:]...)
			return true
		}
	}
	return false
}

// GetUser returns the named user.
func (l *LocalConfig) GetUser(name string) (*User, error) {
	for i := range l.Users {
		if l.Users[i].Name == name {
			u := l.Users[i]
			return &u, nil
		}
	}
	return nil, fmt.Errorf("User '%s' undefined", name) //nolint:staticcheck // upstream message
}

// UpsertUser adds or replaces a user.
func (l *LocalConfig) UpsertUser(u User) {
	for i := range l.Users {
		if l.Users[i].Name == u.Name {
			l.Users[i] = u
			return
		}
	}
	l.Users = append(l.Users, u)
}

// RemoveUser removes a user; it reports whether one was removed.
func (l *LocalConfig) RemoveUser(name string) bool {
	for i := range l.Users {
		if l.Users[i].Name == name {
			l.Users = append(l.Users[:i], l.Users[i+1:]...)
			return true
		}
	}
	return false
}

// GetToken returns the auth token of the named user.
func (l *LocalConfig) GetToken(name string) string {
	for _, u := range l.Users {
		if u.Name == name {
			return u.AuthToken
		}
	}
	return ""
}

// RemoveToken clears the tokens of the named user; it reports whether the user exists.
func (l *LocalConfig) RemoveToken(name string) bool {
	for i := range l.Users {
		if l.Users[i].Name == name {
			l.Users[i].AuthToken = ""
			l.Users[i].RefreshToken = ""
			return true
		}
	}
	return false
}

// UpsertContext adds or replaces a context.
func (l *LocalConfig) UpsertContext(c ContextRef) {
	for i := range l.Contexts {
		if l.Contexts[i].Name == c.Name {
			l.Contexts[i] = c
			return
		}
	}
	l.Contexts = append(l.Contexts, c)
}

// RemoveContext removes a context and returns its server name.
func (l *LocalConfig) RemoveContext(name string) (string, bool) {
	for i := range l.Contexts {
		if l.Contexts[i].Name == name {
			server := l.Contexts[i].Server
			l.Contexts = append(l.Contexts[:i], l.Contexts[i+1:]...)
			return server, true
		}
	}
	return "", false
}

// ServerInUse reports whether a context other than except refers to server.
func (l *LocalConfig) ServerInUse(server, except string) bool {
	for _, c := range l.Contexts {
		if c.Name != except && c.Server == server {
			return true
		}
	}
	return false
}

// IsEmpty reports whether no server is configured.
func (l *LocalConfig) IsEmpty() bool { return len(l.Servers) == 0 }

// LookupEnv reads an environment variable (os.LookupEnv in production).
type LookupEnv func(key string) (string, bool)

// DefaultDir returns the configuration directory: $PATROL_CONFIG_DIR, else
// $XDG_CONFIG_HOME/patrol, else ~/.config/patrol.
func DefaultDir(env LookupEnv) (string, error) {
	if env == nil {
		env = os.LookupEnv
	}
	if d, ok := env(EnvConfigDir); ok && strings.TrimSpace(d) != "" {
		return d, nil
	}
	if x, ok := env("XDG_CONFIG_HOME"); ok && strings.TrimSpace(x) != "" {
		return filepath.Join(x, DirName), nil
	}
	home, ok := env("HOME")
	if !ok || home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("determine the home directory: %w", err)
		}
		home = h
	}
	return filepath.Join(home, ".config", DirName), nil
}

// DefaultPath returns $PATROL_CONFIG, else <DefaultDir>/config.
func DefaultPath(env LookupEnv) (string, error) {
	if env == nil {
		env = os.LookupEnv
	}
	if p, ok := env(EnvConfig); ok && strings.TrimSpace(p) != "" {
		return p, nil
	}
	dir, err := DefaultDir(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}
