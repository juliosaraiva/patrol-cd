package reposerver

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/juliosaraiva/patrol-cd/internal/render/jsonnet"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// Defaults (upstream argocd-repo-server flag defaults where one exists).
const (
	DefaultListenAddress                    = ":8081"
	DefaultRequestTimeout                   = 10 * time.Minute
	DefaultMaxRequestBytes            int64 = 100 << 20 // reposerver.grpc.max.size (MiB)
	DefaultStreamedManifestMaxTarSize int64 = 100 * 1000 * 1000
	DefaultStreamedManifestMaxExtract int64 = 1000 * 1000 * 1000
	DefaultRepoCacheExpiration              = 24 * time.Hour
	DefaultRevisionCacheExpiration          = 3 * time.Minute
	DefaultManifestCacheEntries             = 4096
	DefaultManifestCacheBytes         int64 = 256 << 20
	DefaultAuxCacheEntries                  = 8192
	DefaultAuxCacheBytes              int64 = 64 << 20
	DefaultMaxWorkingCopies                 = 512
	DefaultPauseGenerationAttempts          = 3
	DefaultPauseGenerationMinutes           = 60
	DefaultMaxGitFilesBytes           int64 = 100 << 20
	DefaultExecTimeout                      = 90 * time.Second
	DefaultGitKillGrace                     = 10 * time.Second
	DefaultSSHKnownHostsPath                = "/app/config/ssh/ssh_known_hosts"
	DefaultTLSCertsDir                      = "/app/config/tls"
	DefaultGPGKeysDir                       = "/app/config/gpg/source"
	// DefaultMetricsAddress is the conventional probe and metrics address (upstream
	// metrics port 8084); MetricsAddress stays empty (disabled) unless set.
	DefaultMetricsAddress = ":8084"
	// TrustMaterialDisabled, as SSHKnownHostsPath, TLSCertsDir or GPGKeysDir, turns
	// that trust material off instead of using its default location.
	TrustMaterialDisabled = "-"
	symlinkMemoTTL        = 12 * time.Hour
)

// Config configures the repo-server. Zero values take the documented defaults.
type Config struct {
	// ListenAddress is the API address (reposerver.listen.address + port).
	ListenAddress string
	// MetricsAddress, when set, is a plain-HTTP listener for kubelet probes and
	// Prometheus (upstream metrics port 8084): GET /healthz (with ?full=true it also
	// checks the API listener, using an ephemeral client certificate under mTLS),
	// GET /readyz and, when Gatherer is set, GET /metrics. It never requires client
	// certificates, so probes work with mTLS enabled.
	MetricsAddress string
	// Token is the shared internal token (Secret patrol-internal-token, ADR-0006);
	// TokenFile is read when Token is empty. One of them is required by Run and Handler.
	Token     string
	TokenFile string
	// TLS: TLSConfig wins over the files. With ClientCAFile set, client certificates are
	// required and verified (mTLS). DisableTLS serves plain HTTP (reposerver.disable.tls).
	TLSConfig    *tls.Config
	TLSCertFile  string
	TLSKeyFile   string
	ClientCAFile string
	DisableTLS   bool

	// RequestTimeout bounds every request (server side).
	RequestTimeout time.Duration
	// MaxRequestBytes caps JSON request bodies and serialized responses
	// (reposerver.grpc.max.size).
	MaxRequestBytes int64
	// ParallelismLimit bounds concurrent manifest generations after a cache miss
	// (reposerver.parallelism.limit; 0 = unlimited).
	ParallelismLimit int
	// StreamedManifestMaxTarSize / StreamedManifestMaxExtractedSize bound
	// GenerateManifestWithFiles uploads (reposerver.streamed.manifest.max.*).
	StreamedManifestMaxTarSize       int64
	StreamedManifestMaxExtractedSize int64
	// MaxCombinedDirectoryManifestsSize bounds Directory sources
	// (reposerver.max.combined.directory.manifests.size; negative = unlimited).
	MaxCombinedDirectoryManifestsSize int64
	// Helm chart and OCI artifact extraction limits (negative or Disable* = unlimited).
	HelmManifestMaxExtractedSize        int64
	DisableHelmManifestMaxExtractedSize bool
	OCIManifestMaxExtractedSize         int64
	DisableOCIManifestMaxExtractedSize  bool
	// OCILayerMediaTypes is the OCI content-layer allow-list (reposerver.oci.layer.media.types).
	OCILayerMediaTypes []string
	// MaxGitFilesBytes bounds the total content returned by GetGitFiles.
	MaxGitFilesBytes int64

	// RootDir holds working copies, extracted artifacts, uploads and tool scratch
	// space. Default: $TMPDIR/_patrol-repo.
	RootDir string
	// MaxWorkingCopies bounds the persistent working copies; idle ones are evicted
	// least recently used first.
	MaxWorkingCopies int
	// CredentialsDir holds per-command credential files (tmpfs recommended). Default:
	// the process temp dir.
	CredentialsDir string

	// Cache sizing and expirations (reposerver.repo.cache.expiration,
	// --revision-cache-expiration).
	RepoCacheExpiration     time.Duration
	RevisionCacheExpiration time.Duration
	ManifestCacheEntries    int
	ManifestCacheBytes      int64
	AuxCacheEntries         int
	AuxCacheBytes           int64
	// Manifest generation error caching (ARGOCD_PAUSE_GEN_*). Attempts 0 means the
	// default (3); negative disables error caching. Minutes 0 means 60; Requests 0
	// disables the request-count reset.
	PauseGenerationAfterFailedAttempts int
	PauseGenerationMinutes             int
	PauseGenerationRequests            int

	// Tools. Paths are absolute or bare names looked up in ToolPATH.
	GitPath       string
	HelmPath      string
	KustomizePath string
	GPGPath       string
	ToolPATH      string
	// ExecTimeout bounds every tool invocation (ARGOCD_EXEC_TIMEOUT); ExecKillGrace is the
	// SIGTERM grace of git (ARGOCD_EXEC_FATAL_TIMEOUT).
	ExecTimeout   time.Duration
	ExecKillGrace time.Duration
	// Git retries of transient transport failures (ARGOCD_GIT_ATTEMPTS_COUNT, ...).
	GitRetryAttempts int
	GitRetryBase     time.Duration
	GitRetryMax      time.Duration
	// DisableSubmodules turns off submodule checkout (reposerver.enable.git.submodule=false).
	DisableSubmodules bool
	// DisableBuiltinGitConfig (reposerver.enable.builtin.git.config=false).
	DisableBuiltinGitConfig bool
	// IncludeHiddenDirectories lists dot-directories in GetGitDirectories.
	IncludeHiddenDirectories bool
	// AllowOutOfBoundsSymlinks skips the repository symlink scan (reposerver.allow.oob.symlinks).
	AllowOutOfBoundsSymlinks bool
	// AllowLocalRepositories permits file:// and local-path repositories (tests only).
	AllowLocalRepositories bool
	// HelmUserAgent is the User-Agent of Helm/OCI requests (reposerver.helm.user.agent).
	HelmUserAgent string
	// HelmPluginsDir enables operator-installed Helm plugins.
	HelmPluginsDir string
	// Jsonnet runs Directory-source Jsonnet files (default: re-exec of this binary).
	Jsonnet jsonnet.Runner
	// CMP settings (reposerver.plugin.*).
	PluginSocketDir                string
	PluginTarExclusions            []string
	PluginUseManifestGeneratePaths bool

	// Trust material mounted from ConfigMaps (never secrets): SSH known hosts file, TLS
	// certificates directory (one PEM file per host name) and GnuPG public keys
	// directory (one armored key per file named by key ID). Empty means the default
	// location (DefaultSSHKnownHostsPath, DefaultTLSCertsDir, DefaultGPGKeysDir);
	// TrustMaterialDisabled ("-") turns one off. Missing files mean "none".
	SSHKnownHostsPath string
	TLSCertsDir       string
	GPGKeysDir        string
	// GnuPGHomeParent is where the private GnuPG keyring is built (short paths are
	// preferred). Default: the process temp dir.
	GnuPGHomeParent string
	// DisableGPG turns GnuPG verification off (upstream ARGOCD_GPG_ENABLED=false).
	DisableGPG bool

	// Registerer receives metrics (nil disables); Gatherer serves MetricsAddress.
	Registerer prometheus.Registerer
	Gatherer   prometheus.Gatherer
	// Logger receives structured logs (default slog.Default()).
	Logger *slog.Logger
	// Now is the clock (tests).
	Now func() time.Time

	// rootFS replaces the file system view of the root directory (tests): a volume mount root
	// owned by another user cannot be created by a non-root test process.
	rootFS *rootFS
}

// withDefaults returns cfg with defaults applied and validates it.
func (cfg Config) withDefaults() (Config, error) {
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = DefaultListenAddress
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if cfg.ParallelismLimit < 0 {
		return cfg, errors.New("reposerver: parallelism limit must not be negative")
	}
	if cfg.StreamedManifestMaxTarSize <= 0 {
		cfg.StreamedManifestMaxTarSize = DefaultStreamedManifestMaxTarSize
	}
	if cfg.StreamedManifestMaxExtractedSize <= 0 {
		cfg.StreamedManifestMaxExtractedSize = DefaultStreamedManifestMaxExtract
	}
	if cfg.MaxGitFilesBytes <= 0 {
		cfg.MaxGitFilesBytes = DefaultMaxGitFilesBytes
	}
	if cfg.RootDir == "" {
		cfg.RootDir = filepath.Join(os.TempDir(), "_patrol-repo")
	}
	if !filepath.IsAbs(cfg.RootDir) {
		return cfg, fmt.Errorf("reposerver: root dir %q must be absolute", cfg.RootDir)
	}
	if cfg.MaxWorkingCopies <= 0 {
		cfg.MaxWorkingCopies = DefaultMaxWorkingCopies
	}
	if cfg.RepoCacheExpiration <= 0 {
		cfg.RepoCacheExpiration = DefaultRepoCacheExpiration
	}
	if cfg.RevisionCacheExpiration <= 0 {
		cfg.RevisionCacheExpiration = DefaultRevisionCacheExpiration
	}
	if cfg.ManifestCacheEntries <= 0 {
		cfg.ManifestCacheEntries = DefaultManifestCacheEntries
	}
	if cfg.ManifestCacheBytes <= 0 {
		cfg.ManifestCacheBytes = DefaultManifestCacheBytes
	}
	if cfg.AuxCacheEntries <= 0 {
		cfg.AuxCacheEntries = DefaultAuxCacheEntries
	}
	if cfg.AuxCacheBytes <= 0 {
		cfg.AuxCacheBytes = DefaultAuxCacheBytes
	}
	if cfg.PauseGenerationAfterFailedAttempts == 0 {
		cfg.PauseGenerationAfterFailedAttempts = DefaultPauseGenerationAttempts
	}
	if cfg.PauseGenerationMinutes <= 0 {
		cfg.PauseGenerationMinutes = DefaultPauseGenerationMinutes
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = DefaultExecTimeout
	}
	if cfg.ExecKillGrace <= 0 {
		cfg.ExecKillGrace = DefaultGitKillGrace
	}
	if cfg.GitPath == "" {
		cfg.GitPath = "git"
	}
	if cfg.HelmPath == "" {
		cfg.HelmPath = "helm"
	}
	if cfg.KustomizePath == "" {
		cfg.KustomizePath = "kustomize"
	}
	if cfg.GPGPath == "" {
		cfg.GPGPath = "gpg"
	}
	cfg.SSHKnownHostsPath = trustPath(cfg.SSHKnownHostsPath, DefaultSSHKnownHostsPath)
	cfg.TLSCertsDir = trustPath(cfg.TLSCertsDir, DefaultTLSCertsDir)
	cfg.GPGKeysDir = trustPath(cfg.GPGKeysDir, DefaultGPGKeysDir)
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return cfg, nil
}

// trustPath applies the default location of a trust material path; "-" disables it
// (empty result).
func trustPath(p, def string) string {
	switch p {
	case "":
		return def
	case TrustMaterialDisabled:
		return ""
	default:
		return p
	}
}

// helmExtractLimits returns the Helm chart extraction limits.
func (cfg Config) helmExtractLimits() sandbox.ArchiveLimits {
	if cfg.DisableHelmManifestMaxExtractedSize || cfg.HelmManifestMaxExtractedSize < 0 {
		return sandbox.ArchiveLimits{MaxTotalBytes: -1, MaxFileBytes: -1}
	}
	return sandbox.ArchiveLimits{MaxTotalBytes: cfg.HelmManifestMaxExtractedSize, MaxFileBytes: cfg.HelmManifestMaxExtractedSize}
}

// ociExtractLimits returns the OCI artifact extraction limits.
func (cfg Config) ociExtractLimits() sandbox.ArchiveLimits {
	if cfg.DisableOCIManifestMaxExtractedSize || cfg.OCIManifestMaxExtractedSize < 0 {
		return sandbox.ArchiveLimits{MaxTotalBytes: -1, MaxFileBytes: -1}
	}
	return sandbox.ArchiveLimits{MaxTotalBytes: cfg.OCIManifestMaxExtractedSize, MaxFileBytes: cfg.OCIManifestMaxExtractedSize}
}

// readToken returns the configured token.
func (cfg Config) readToken() (string, error) {
	if cfg.Token != "" {
		return cfg.Token, nil
	}
	if cfg.TokenFile == "" {
		return "", errors.New("reposerver: an internal token is required (Token or TokenFile)")
	}
	data, err := readFileLimited(cfg.TokenFile, 64<<10)
	if err != nil {
		return "", fmt.Errorf("reposerver: read token file: %w", err)
	}
	tok := trimSpace(string(data))
	if tok == "" {
		return "", errors.New("reposerver: token file is empty")
	}
	return tok, nil
}
