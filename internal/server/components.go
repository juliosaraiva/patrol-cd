package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver"
	"github.com/juliosaraiva/patrol-cd/internal/cmp"
	"github.com/juliosaraiva/patrol-cd/internal/commitserver"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// Default ports of the repo-server and the commit-server (OPS-009).
const (
	DefaultRepoServerPort          = 8081
	DefaultRepoServerMetricsPort   = 8084
	DefaultCommitServerPort        = 8086
	DefaultCommitServerMetricsPort = 8087
	// DefaultRepoServerTLSDir is where the install manifests mount Secret
	// patrol-repo-server-tls (tls.crt, tls.key); used when present and no files are given.
	DefaultRepoServerTLSDir = "/app/config/reposerver/tls"
)

// RepoServerConfig is the configuration of the repo-server.
type RepoServerConfig struct {
	// Server is the repo-server configuration (Token required).
	Server reposerver.Config
	// TLS selects the certificate pair, client CA and versions (ignored with
	// Server.DisableTLS). Without a pair a self-signed certificate is generated.
	TLS TLSServerOptions
}

// RunRepoServer runs the repo-server until ctx ends. Its probe and metrics listener is
// Server.MetricsAddress (GET /healthz[?full=true], /readyz, /metrics).
func RunRepoServer(ctx context.Context, cfg RepoServerConfig) error {
	c := cfg.Server
	if c.Token == "" && c.TokenFile == "" {
		return errors.New("invalid repo-server configuration: the internal token is required")
	}
	if !c.DisableTLS {
		tc, err := repoServerTLS(cfg.TLS)
		if err != nil {
			return fmt.Errorf("repo-server TLS: %w", err)
		}
		c.TLSConfig = tc
		c.ClientCAFile = cfg.TLS.ClientCAFile
	}
	return reposerver.Run(ctx, c)
}

// repoServerTLS builds the listener configuration: the given pair (reloaded on change), else
// the mounted patrol-repo-server-tls pair when present, else a self-signed certificate;
// versions and ciphers apply to all three. Mutual TLS is added by the repo-server from
// ClientCAFile.
func repoServerTLS(o TLSServerOptions) (*tls.Config, error) {
	if o.CertFile == "" && o.KeyFile == "" {
		crt, key := filepath.Join(DefaultRepoServerTLSDir, "tls.crt"), filepath.Join(DefaultRepoServerTLSDir, "tls.key")
		if fileExists(crt) && fileExists(key) {
			o.CertFile, o.KeyFile = crt, key
		}
	}
	// Mutual TLS is configured by the repo-server itself (with its health-check certificate).
	o.ClientCAFile = ""
	tc, err := serverTLSConfig(o)
	if err != nil {
		return nil, err
	}
	if tc.GetCertificate == nil {
		cert, err := apiserver.GenerateSelfSigned([]string{"localhost", "patrol-repo-server", "127.0.0.1", "::1"}, 365*24*time.Hour)
		if err != nil {
			return nil, err
		}
		tc.Certificates = []tls.Certificate{*cert}
	}
	return tc, nil
}

// repoServerConfigFromParams maps the reposerver.* parameters and the PATROL_* tuning
// variables of the repo-server (upstream ARGOCD_EXEC_TIMEOUT, ARGOCD_GIT_*, ARGOCD_PAUSE_GEN_*,
// ARGOCD_GPG_ENABLED) to its configuration.
func repoServerConfigFromParams(p *settings.Params, lookupEnv func(string) (string, bool), log *slog.Logger) reposerver.Config {
	c := reposerver.Config{}
	if p != nil {
		c.ParallelismLimit = int(p.Int("reposerver.parallelism.limit"))
		c.DisableTLS = p.Bool("reposerver.disable.tls")
		c.RepoCacheExpiration = p.Duration("reposerver.repo.cache.expiration")
		if n := p.Quantity("reposerver.max.combined.directory.manifests.size"); n > 0 {
			c.MaxCombinedDirectoryManifestsSize = n
		} else {
			c.MaxCombinedDirectoryManifestsSize = -1
		}
		c.PluginTarExclusions = p.Strings("reposerver.plugin.tar.exclusions")
		c.PluginUseManifestGeneratePaths = p.Bool("reposerver.plugin.use.manifest.generate.paths")
		c.AllowOutOfBoundsSymlinks = p.Bool("reposerver.allow.oob.symlinks")
		c.StreamedManifestMaxTarSize = p.Quantity("reposerver.streamed.manifest.max.tar.size")
		c.StreamedManifestMaxExtractedSize = p.Quantity("reposerver.streamed.manifest.max.extracted.size")
		c.HelmManifestMaxExtractedSize = p.Quantity("reposerver.helm.manifest.max.extracted.size")
		c.DisableHelmManifestMaxExtractedSize = p.Bool("reposerver.disable.helm.manifest.max.extracted.size")
		c.OCIManifestMaxExtractedSize = p.Quantity("reposerver.oci.manifest.max.extracted.size")
		c.DisableOCIManifestMaxExtractedSize = p.Bool("reposerver.disable.oci.manifest.max.extracted.size")
		c.OCILayerMediaTypes = p.Strings("reposerver.oci.layer.media.types")
		c.DisableSubmodules = !p.Bool("reposerver.enable.git.submodule")
		c.DisableBuiltinGitConfig = !p.Bool("reposerver.enable.builtin.git.config")
		c.IncludeHiddenDirectories = p.Bool("reposerver.include.hidden.directories")
		if mb := p.Int("reposerver.grpc.max.size"); mb > 0 {
			c.MaxRequestBytes = mb << 20
		}
		c.HelmUserAgent = p.String("reposerver.helm.user.agent")
	}
	env := envReader{lookup: lookupEnv, log: log}
	c.ExecTimeout = env.duration("PATROL_EXEC_TIMEOUT")
	c.ExecKillGrace = env.duration("PATROL_EXEC_FATAL_TIMEOUT")
	c.GitRetryAttempts = env.int("PATROL_GIT_ATTEMPTS_COUNT")
	c.GitRetryBase = env.duration("PATROL_GIT_RETRY_DURATION")
	c.GitRetryMax = env.duration("PATROL_GIT_RETRY_MAX_DURATION")
	c.PauseGenerationAfterFailedAttempts = env.int("PATROL_PAUSE_GEN_AFTER_FAILED_ATTEMPTS")
	c.PauseGenerationMinutes = env.int("PATROL_PAUSE_GEN_MINUTES")
	c.PauseGenerationRequests = env.int("PATROL_PAUSE_GEN_REQUESTS")
	if v, ok := lookupEnv("PATROL_GPG_ENABLED"); ok && v == "false" {
		c.DisableGPG = true
	}
	return c
}

// envReader parses optional tuning variables; invalid values are ignored with a warning
// (the library default applies), as upstream's env helpers.
type envReader struct {
	lookup func(string) (string, bool)
	log    *slog.Logger
}

func (e envReader) raw(name string) (string, bool) {
	if e.lookup == nil {
		return "", false
	}
	v, ok := e.lookup(name)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

func (e envReader) duration(name string) time.Duration {
	v, ok := e.raw(name)
	if !ok {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		e.log.Warn("ignoring invalid duration", "variable", name, "value", v)
		return 0
	}
	return d
}

func (e envReader) int(name string) int {
	v, ok := e.raw(name)
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.log.Warn("ignoring invalid number", "variable", name, "value", v)
		return 0
	}
	return n
}

// CommitServerConfig is the configuration of the commit-server.
type CommitServerConfig struct {
	// ListenAddress serves the API (host:port).
	ListenAddress string
	// MetricsAddress serves /metrics and the probes; HealthzAddress optionally the probes.
	MetricsAddress, HealthzAddress string
	EnablePprof                    bool
	PprofFile                      string
	// Token is the internal token. Required.
	Token string
	// TLS enables TLS (and mutual TLS) on the API listener when a pair is configured.
	TLS TLSServerOptions
	// Service configures the commit service (work and credentials directories, git, trust
	// material: see CommitTrust).
	Service         commitserver.ServiceOptions
	ShutdownTimeout time.Duration
	Registry        *prometheus.Registry
	Logger          *slog.Logger
	// OnListening, when set, receives the bound API and metrics addresses.
	OnListening func(api, metrics net.Addr)
}

// RunCommitServer runs the commit-server API and its metrics/probe listener until ctx ends.
func RunCommitServer(ctx context.Context, cfg CommitServerConfig) error {
	if cfg.Token == "" {
		return errors.New("invalid commit-server configuration: the internal token is required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	reg := cfg.Registry
	if reg == nil {
		reg = newRegistry()
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = DefaultShutdownTimeout
	}
	var tc *tls.Config
	if cfg.TLS.Enabled() {
		var err error
		if tc, err = serverTLSConfig(cfg.TLS); err != nil {
			return fmt.Errorf("commit-server TLS: %w", err)
		}
	}
	l, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddress, err)
	}
	var metricsAddr net.Addr
	bound := make(chan struct{})
	return runGroup(ctx, cfg.ShutdownTimeout, log,
		task{name: "commit-server", run: func(ctx context.Context) error {
			select {
			case <-bound:
			case <-ctx.Done():
				_ = l.Close()
				return nil
			}
			if cfg.OnListening != nil {
				cfg.OnListening(l.Addr(), metricsAddr)
			}
			return commitserver.Run(ctx, commitserver.RunOptions{
				Listener: l, MetricsAddress: "-", Token: cfg.Token, TLSConfig: tc, Service: cfg.Service,
				Registry: reg, ShutdownTimeout: cfg.ShutdownTimeout, Logger: log,
			})
		}},
		task{name: "observability", run: func(ctx context.Context) error {
			return serveObservability(ctx, ObservabilityOptions{
				MetricsAddr: cfg.MetricsAddress, HealthzAddr: cfg.HealthzAddress, Gatherer: reg,
				PprofEnabled: cfg.EnablePprof, PprofFile: cfg.PprofFile, Logger: log, ShutdownTimeout: cfg.ShutdownTimeout,
			}, func(m, _ net.Addr) { metricsAddr = m; close(bound) })
		}},
	)
}

// CMPServerConfig is the configuration of the config management plugin sidecar server.
type CMPServerConfig struct {
	Options cmp.Options
	// MetricsAddress optionally serves /metrics and the probes ("-": none, the default; the
	// sidecar is reached through its unix socket).
	MetricsAddress  string
	ShutdownTimeout time.Duration
	Registry        *prometheus.Registry
	Logger          *slog.Logger
}

// RunCMPServer runs the CMP sidecar server until ctx ends (the socket is removed on return).
func RunCMPServer(ctx context.Context, cfg CMPServerConfig) error {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	reg := cfg.Registry
	if reg == nil {
		reg = newRegistry()
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = DefaultShutdownTimeout
	}
	if err := cfg.Options.Validate(); err != nil {
		return err
	}
	return runGroup(ctx, cfg.ShutdownTimeout, log,
		task{name: "cmp-server", run: func(ctx context.Context) error { return cmp.Run(ctx, cfg.Options, log, reg) }},
		task{name: "observability", run: func(ctx context.Context) error {
			return serveObservability(ctx, ObservabilityOptions{MetricsAddr: cfg.MetricsAddress, Gatherer: reg, Logger: log, ShutdownTimeout: cfg.ShutdownTimeout}, nil)
		}},
	)
}

// CommitTrust returns the trust material of the commit-server's git operations: the SSH
// known hosts file (missing: none) and a lookup of the per-host PEM bundle in certsDir (one
// file per host name, as mounted from patrol-tls-certs-cm; read per call so updates of the
// mounted ConfigMap apply). Empty paths or "-" disable them.
func CommitTrust(knownHostsPath, certsDir string, log *slog.Logger) ([]byte, func(serverName string) []byte, error) {
	var knownHosts []byte
	if knownHostsPath != "" && knownHostsPath != "-" {
		data, err := readBounded(knownHostsPath, maxPEMBytes)
		switch {
		case err == nil:
			knownHosts = data
		case errors.Is(err, os.ErrNotExist):
		default:
			return nil, nil, fmt.Errorf("read SSH known hosts: %w", err)
		}
	}
	if certsDir == "" || certsDir == "-" {
		return knownHosts, nil, nil
	}
	return knownHosts, func(serverName string) []byte {
		if serverName == "" || strings.ContainsAny(serverName, `/\`) || strings.HasPrefix(serverName, ".") {
			return nil
		}
		data, err := readBounded(filepath.Join(certsDir, serverName), maxPEMBytes)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				log.Warn("cannot read the TLS certificates of a repository host", "host", serverName, "error", err)
			}
			return nil
		}
		return data
	}, nil
}
