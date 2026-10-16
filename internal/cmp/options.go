package cmp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/juliosaraiva/patrol-cd/internal/render/cmpwire"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// Environment variables read by OptionsFromEnv. Patrol names win over the upstream
// (ARGOCD_*) names, which are accepted for compatibility.
const (
	EnvSocketDir        = "PATROL_PLUGINSOCKFILEPATH"
	EnvSocketDirArgo    = "ARGOCD_PLUGINSOCKFILEPATH"
	EnvWorkDir          = "PATROL_CMP_WORKDIR"
	EnvWorkDirArgo      = "ARGOCD_CMP_WORKDIR"
	EnvExecTimeout      = "PATROL_EXEC_TIMEOUT"
	EnvExecTimeoutArgo  = "ARGOCD_EXEC_TIMEOUT"
	EnvPassEnv          = "PATROL_CMP_PASS_ENV"
	EnvReceiveTimeout   = "PATROL_CMP_RECEIVE_TIMEOUT"
	EnvMaxArchiveSize   = "PATROL_CMP_MAX_ARCHIVE_SIZE"
	EnvMaxExtractedSize = "PATROL_CMP_MAX_EXTRACTED_SIZE"
	EnvMaxFiles         = "PATROL_CMP_MAX_FILES"
	EnvLogLevel         = "PATROL_CMP_SERVER_LOGLEVEL"
	EnvLogLevelArgo     = "ARGOCD_CMP_SERVER_LOGLEVEL"
	EnvLogFormat        = "PATROL_CMP_SERVER_LOGFORMAT"
	EnvLogFormatArgo    = "ARGOCD_CMP_SERVER_LOGFORMAT"
)

// Options are the cmp-server process settings (flags and environment).
type Options struct {
	// ConfigDir holds plugin.yaml (flag --config-dir-path).
	ConfigDir string
	// SocketDir is the plugin socket directory.
	SocketDir string
	// WorkDir is the parent of the "_cmp_server" work directory.
	WorkDir string
	// ExecTimeout bounds each plugin command.
	ExecTimeout time.Duration
	// PassEnv lists sidecar environment variables passed to plugins (see
	// Config.PassEnv).
	PassEnv []string
	// ReceiveTimeout bounds the reception of one request stream (see
	// Config.ReceiveTimeout); zero or negative leaves only the client deadline.
	ReceiveTimeout time.Duration
	// MaxArchiveBytes bounds the compressed archive of one request (negative:
	// unlimited).
	MaxArchiveBytes int64
	// MaxExtractedBytes bounds the extracted size of one request (negative:
	// unlimited).
	MaxExtractedBytes int64
	// MaxFiles bounds the number of archive entries of one request (negative:
	// unlimited).
	MaxFiles int
	// LogLevel (debug|info|warn|error) and LogFormat (json|text).
	LogLevel, LogFormat string
}

// DefaultOptions returns the defaults: config dir /home/patrol/cmp-server/config,
// socket dir /home/patrol/cmp-server/plugins, work dir parent os.TempDir(), 90 s exec
// timeout, 5 min receive timeout, 1 GiB archive and extraction limits, 500 000
// entries, info/json logs.
func DefaultOptions() Options {
	return Options{
		ConfigDir:         DefaultConfigDir,
		SocketDir:         cmpwire.DefaultSocketDir,
		WorkDir:           os.TempDir(),
		ExecTimeout:       DefaultExecTimeout,
		ReceiveTimeout:    DefaultReceiveTimeout,
		MaxArchiveBytes:   DefaultMaxArchiveBytes,
		MaxExtractedBytes: sandbox.DefaultArchiveMaxTotalBytes,
		MaxFiles:          sandbox.DefaultArchiveMaxFiles,
		LogLevel:          "info",
		LogFormat:         "json",
	}
}

// OptionsFromEnv returns DefaultOptions overridden by the environment (lookup is
// typically os.LookupEnv). Durations use Go syntax ("90s", "2m"); sizes use
// Kubernetes quantities ("1Gi", "500M").
func OptionsFromEnv(lookup func(string) (string, bool)) (Options, error) {
	o := DefaultOptions()
	get := func(names ...string) (string, string, bool) {
		for _, n := range names {
			if v, ok := lookup(n); ok && strings.TrimSpace(v) != "" {
				return n, strings.TrimSpace(v), true
			}
		}
		return "", "", false
	}
	if _, v, ok := get(EnvSocketDir, EnvSocketDirArgo); ok {
		o.SocketDir = v
	}
	if _, v, ok := get(EnvWorkDir, EnvWorkDirArgo); ok {
		o.WorkDir = v
	}
	if n, v, ok := get(EnvExecTimeout, EnvExecTimeoutArgo); ok {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Options{}, fmt.Errorf("cmp: invalid %s %q: expected a positive duration such as 90s", n, v)
		}
		o.ExecTimeout = d
	}
	if _, v, ok := get(EnvPassEnv); ok {
		o.PassEnv = splitList(v)
	}
	if _, v, ok := get(EnvReceiveTimeout); ok {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return Options{}, fmt.Errorf("cmp: invalid %s %q: expected a duration such as 5m (0 disables)", EnvReceiveTimeout, v)
		}
		o.ReceiveTimeout = d
	}
	for _, q := range []struct {
		name string
		dst  *int64
	}{{EnvMaxArchiveSize, &o.MaxArchiveBytes}, {EnvMaxExtractedSize, &o.MaxExtractedBytes}} {
		if _, v, ok := get(q.name); ok {
			n, err := parseSize(v)
			if err != nil {
				return Options{}, fmt.Errorf("cmp: invalid %s %q: %w", q.name, v, err)
			}
			*q.dst = n
		}
	}
	if _, v, ok := get(EnvMaxFiles); ok {
		n, err := parseSize(v)
		if err != nil || n > int64(^uint(0)>>1) {
			return Options{}, fmt.Errorf("cmp: invalid %s %q", EnvMaxFiles, v)
		}
		o.MaxFiles = int(n)
	}
	if _, v, ok := get(EnvLogLevel, EnvLogLevelArgo); ok {
		o.LogLevel = v
	}
	if _, v, ok := get(EnvLogFormat, EnvLogFormatArgo); ok {
		o.LogFormat = v
	}
	return o, o.Validate()
}

// parseSize parses a Kubernetes quantity ("0" disables the limit, as upstream's
// size settings; a negative value too).
func parseSize(v string) (int64, error) {
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return 0, err
	}
	n, ok := q.AsInt64()
	if !ok {
		return 0, fmt.Errorf("quantity out of range")
	}
	if n <= 0 {
		return -1, nil
	}
	return n, nil
}

func splitList(v string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// AddFlags binds o to fs (flags override the environment when set).
func (o *Options) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.ConfigDir, "config-dir-path", o.ConfigDir, "Directory of the config management plugin configuration file (plugin.yaml)")
	fs.StringVar(&o.SocketDir, "socket-dir", o.SocketDir, "Plugin socket directory shared with the repo-server (env "+EnvSocketDir+")")
	fs.StringVar(&o.WorkDir, "work-dir", o.WorkDir, "Parent of the _cmp_server work directory, wiped at startup (env "+EnvWorkDir+")")
	fs.DurationVar(&o.ExecTimeout, "exec-timeout", o.ExecTimeout, "Timeout of each plugin command (env "+EnvExecTimeout+")")
	fs.StringSliceVar(&o.PassEnv, "pass-env", o.PassEnv, "Sidecar environment variables passed to plugin commands: names, PREFIX* or * (env "+EnvPassEnv+"); plugins get only PATH, HOME, TMPDIR and LANG otherwise")
	fs.DurationVar(&o.ReceiveTimeout, "receive-timeout", o.ReceiveTimeout, "Maximum time to receive one request stream; 0 leaves only the client deadline (env "+EnvReceiveTimeout+")")
	fs.Int64Var(&o.MaxArchiveBytes, "max-archive-bytes", o.MaxArchiveBytes, "Maximum compressed source archive size per request; negative disables")
	fs.Int64Var(&o.MaxExtractedBytes, "max-extracted-bytes", o.MaxExtractedBytes, "Maximum extracted source size per request; negative disables")
	fs.IntVar(&o.MaxFiles, "max-files", o.MaxFiles, "Maximum number of archive entries per request; negative disables")
	fs.StringVar(&o.LogLevel, "loglevel", o.LogLevel, "Log level: debug|info|warn|error (env "+EnvLogLevel+")")
	fs.StringVar(&o.LogFormat, "logformat", o.LogFormat, "Log format: json|text (env "+EnvLogFormat+")")
}

// Validate checks the options.
func (o Options) Validate() error {
	if o.ConfigDir == "" || o.SocketDir == "" {
		return fmt.Errorf("cmp: config and socket directories are required")
	}
	if !filepath.IsAbs(o.WorkDir) {
		return fmt.Errorf("cmp: work directory %q must be absolute", o.WorkDir)
	}
	if o.ExecTimeout <= 0 {
		return fmt.Errorf("cmp: exec timeout must be positive")
	}
	if err := validatePassEnv(o.PassEnv); err != nil {
		return err
	}
	if _, err := parseLevel(o.LogLevel); err != nil {
		return err
	}
	switch o.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("cmp: unknown log format %q (json|text)", o.LogFormat)
	}
	return nil
}

func parseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if s == "trace" {
		return slog.LevelDebug, nil
	}
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("cmp: unknown log level %q: %w", s, err)
	}
	return l, nil
}

// NewLogger returns the logger described by LogLevel and LogFormat, writing to w.
func (o Options) NewLogger(w io.Writer) (*slog.Logger, error) {
	level, err := parseLevel(o.LogLevel)
	if err != nil {
		return nil, err
	}
	hopts := &slog.HandlerOptions{Level: level}
	if o.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, hopts)), nil
	}
	return slog.New(slog.NewJSONHandler(w, hopts)), nil
}

// Config returns the server configuration for plugin.
func (o Options) Config(plugin *PluginConfig, logger *slog.Logger, reg prometheus.Registerer) Config {
	receiveTimeout := o.ReceiveTimeout
	if receiveTimeout <= 0 {
		receiveTimeout = -1 // Config: negative disables, zero is the default
	}
	return Config{
		Plugin:          plugin,
		SocketDir:       o.SocketDir,
		WorkDir:         o.WorkDir,
		Exec:            sandbox.Limits{Timeout: o.ExecTimeout},
		PassEnv:         o.PassEnv,
		ReceiveTimeout:  receiveTimeout,
		MaxArchiveBytes: o.MaxArchiveBytes,
		Archive:         sandbox.ArchiveLimits{MaxFiles: o.MaxFiles, MaxTotalBytes: o.MaxExtractedBytes, MaxFileBytes: o.MaxExtractedBytes},
		Registerer:      reg,
		Logger:          logger,
	}
}

// Run is the cmp-server entry point: it validates o, reads and validates plugin.yaml
// (failing on the upstream validation errors), logs configuration warnings, and
// serves until ctx is done (the caller cancels ctx on SIGINT/SIGTERM; the socket is
// removed on return).
func Run(ctx context.Context, o Options, logger *slog.Logger, reg prometheus.Registerer) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if logger == nil {
		logger = slog.Default()
	}
	plugin, err := ReadPluginConfig(o.ConfigDir)
	if err != nil {
		return err
	}
	for _, w := range plugin.Warnings() {
		logger.Warn(w, "plugin", plugin.PluginName())
	}
	s, err := New(o.Config(plugin, logger, reg))
	if err != nil {
		return err
	}
	return s.ListenAndServe(ctx)
}
