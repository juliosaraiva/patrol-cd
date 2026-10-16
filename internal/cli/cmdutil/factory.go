// Package cmdutil is the shared foundation of the patrol CLI commands: global flags and
// their PATROL_OPTS / environment defaults, the local configuration, construction of the
// authenticated API client (transport selection, TLS, headers, retries, token renewal and
// SSO refresh), prompts, exit codes and logging.
//
// Command packages receive a *Factory and build commands as
//
//	func NewThingCommand(f *cmdutil.Factory) *cobra.Command
//
// calling f.NewClient(ctx) for an API client and writing to f.IO.Out / f.IO.ErrOut.
package cmdutil

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pkg/browser"
	"github.com/spf13/pflag"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/juliosaraiva/patrol-cd/internal/cli/config"
	"github.com/juliosaraiva/patrol-cd/internal/cli/transport"
)

// IOStreams are the standard streams of a command.
type IOStreams struct {
	In     io.Reader
	Out    io.Writer
	ErrOut io.Writer
}

// SystemIO returns the process streams.
func SystemIO() IOStreams { return IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr} }

// Factory carries the global options and collaborators of every command. A Factory is
// used for one command execution.
type Factory struct {
	IO IOStreams
	// Opts are the global flags (bound by BindFlags).
	Opts GlobalOptions
	// LookupEnv reads environment variables (os.LookupEnv by default).
	LookupEnv config.LookupEnv
	// Environ returns the environment passed to plugins (os.Environ by default).
	Environ func() []string
	// PortForward and Core are the transports of --port-forward and --core (Core nil:
	// "core mode not available in this build").
	PortForward transport.Provider
	Core        transport.Provider
	// OpenBrowser opens a URL for SSO logins.
	OpenBrowser func(url string) error
	// SSOTimeout bounds the wait for the SSO redirect (0: sso.DefaultTimeout).
	SSOTimeout time.Duration
	// Now is the clock (token expiry checks).
	Now func() time.Time
	// Version is the client build information.
	Version ClientVersion
	// KubeOverrides are the kubectl flags accepted by the command groups (core mode).
	KubeOverrides clientcmd.ConfigOverrides
	// Kubeconfig is the --kubeconfig flag of the command groups (core mode).
	Kubeconfig string

	envOpts   Opts
	flags     *pflag.FlagSet
	setupErrs []error
	notes     []string
	// argoServer and argoToken hold the ARGOCD_SERVER / ARGOCD_AUTH_TOKEN aliases.
	argoServer string
	argoToken  string
	logger     *slog.Logger
	prompter   *Prompter

	mu    sync.Mutex
	conns []*transport.Connection
}

// The browser launcher must not write to the terminal.
func init() { browser.Stdout, browser.Stderr = io.Discard, io.Discard }

// NewFactory returns a factory with production collaborators.
func NewFactory(streams IOStreams, lookupEnv config.LookupEnv) *Factory {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	return &Factory{
		IO:          streams,
		LookupEnv:   lookupEnv,
		Environ:     os.Environ,
		PortForward: &transport.PortForward{},
		OpenBrowser: browser.OpenURL,
		Now:         time.Now,
		Version:     BuildVersion(),
	}
}

// Getenv returns an environment variable ("" when unset).
func (f *Factory) Getenv(key string) string {
	v, _ := f.lookupEnv(key)
	return v
}

func (f *Factory) lookupEnv(key string) (string, bool) {
	if f.LookupEnv == nil {
		return os.LookupEnv(key)
	}
	return f.LookupEnv(key)
}

// BindFlags parses PATROL_OPTS and registers the global flags on fs (the root command's
// persistent flags).
func (f *Factory) BindFlags(fs *pflag.FlagSet) {
	f.flags = fs
	f.envOpts = Opts{}
	if raw, ok := f.lookupEnv(EnvOpts); ok && strings.TrimSpace(raw) != "" {
		o, err := ParseOpts(EnvOpts, raw)
		if err != nil {
			f.setupErrs = append(f.setupErrs, err)
		} else {
			f.envOpts = o
		}
	} else if raw, ok := f.lookupEnv(EnvArgoOpts); ok && strings.TrimSpace(raw) != "" {
		o, err := ParseOpts(EnvArgoOpts, raw)
		if err != nil {
			f.setupErrs = append(f.setupErrs, err)
		} else {
			f.notes = append(f.notes, fmt.Sprintf("%s is honored as a migration alias of %s; set %s instead", EnvArgoOpts, EnvOpts, EnvOpts))
			if _, ok := o[FlagAuthToken]; ok {
				delete(o, FlagAuthToken)
				f.notes = append(f.notes, fmt.Sprintf("--%s in %s is ignored; use %s or %s", FlagAuthToken, EnvArgoOpts, EnvOpts, EnvAuthToken))
			}
			f.envOpts = o
		}
	}
	if v, ok := f.lookupEnv(EnvArgoAuthToken); ok && v != "" {
		if t, ok := f.lookupEnv(EnvAuthToken); !ok || t == "" {
			f.argoToken = strings.TrimSpace(v)
		}
	}
	f.addGlobalFlags(fs)
}

// Changed reports whether a global flag was set on the command line.
func (f *Factory) Changed(name string) bool {
	if f.flags == nil {
		return false
	}
	fl := f.flags.Lookup(name)
	return fl != nil && fl.Changed
}

// Setup validates the global options after parsing and configures logging. It is called
// before every command runs.
func (f *Factory) Setup() error {
	if err := errors.Join(f.setupErrs...); err != nil {
		return &ExitError{Code: ExitFatal, Err: err}
	}
	lg, err := newLogger(f.IO.ErrOut, f.Opts.LogLevel, f.Opts.LogFormat)
	if err != nil {
		return &UsageError{Err: err}
	}
	f.logger = lg
	for _, n := range f.notes {
		lg.Warn(n)
	}
	if f.Opts.GRPCWeb {
		lg.Debug("--grpc-web is ignored: Patrol CD serves JSON over HTTP")
	}
	return nil
}

// Logger returns the command logger (writing to the error stream).
func (f *Factory) Logger() *slog.Logger {
	if f.logger == nil {
		lg, _ := newLogger(f.IO.ErrOut, "info", "json")
		f.logger = lg
	}
	return f.logger
}

func newLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "info", "":
		lv = slog.LevelInfo
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q (one of: debug|info|warn|error)", level)
	}
	if w == nil {
		w = io.Discard
	}
	opts := &slog.HandlerOptions{Level: lv, ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}
	switch strings.ToLower(format) {
	case "text", "":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("unknown log format %q (one of: json|text)", format)
}

// ConfigPath returns the local configuration path (--config).
func (f *Factory) ConfigPath() string {
	if f.Opts.ConfigPath != "" {
		return f.Opts.ConfigPath
	}
	p, err := config.DefaultPath(f.lookupEnv)
	if err != nil {
		return config.FileName
	}
	return p
}

// ReadConfig loads the local configuration (nil when absent).
func (f *Factory) ReadConfig() (*config.LocalConfig, error) { return config.Read(f.ConfigPath()) }

// WriteConfig stores the local configuration (mode 0600).
func (f *Factory) WriteConfig(cfg *config.LocalConfig) error {
	return config.Write(cfg, f.ConfigPath())
}

// PromptsEnabled reports whether optional confirmation prompts are enabled:
// --prompts-enabled (command line or PATROL_OPTS) wins, else the local configuration.
func (f *Factory) PromptsEnabled() bool {
	if f.Changed(FlagPromptsEnabled) {
		return f.Opts.PromptsEnabled
	}
	if _, ok := f.envOpts[FlagPromptsEnabled]; ok {
		return f.envOpts.Bool(FlagPromptsEnabled)
	}
	cfg, err := f.ReadConfig()
	if err != nil || cfg == nil {
		return false
	}
	return cfg.PromptsEnabled
}

// Prompter returns the prompter of the command's streams (mandatory prompts).
func (f *Factory) Prompter() *Prompter {
	if f.prompter == nil {
		f.prompter = NewPrompter(f.IO.In, f.IO.ErrOut)
	}
	return f.prompter
}

// OptionalPrompt returns the prompter for optional confirmations.
func (f *Factory) OptionalPrompt() OptionalPrompt {
	return OptionalPrompt{Enabled: f.PromptsEnabled(), P: f.Prompter()}
}

// track records a connection to close when the command finishes.
func (f *Factory) track(c *transport.Connection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns = append(f.conns, c)
}

// Close releases the connections opened by the command (port-forward listeners,
// in-process servers).
func (f *Factory) Close() error {
	f.mu.Lock()
	conns := f.conns
	f.conns = nil
	f.mu.Unlock()
	var errs []error
	for _, c := range conns {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Out returns the standard output.
func (f *Factory) Out() io.Writer {
	if f.IO.Out == nil {
		return io.Discard
	}
	return f.IO.Out
}

// ErrOut returns the error output.
func (f *Factory) ErrOut() io.Writer {
	if f.IO.ErrOut == nil {
		return io.Discard
	}
	return f.IO.ErrOut
}
