package cmdutil

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/pflag"

	"github.com/juliosaraiva/patrol-cd/internal/cli/config"
)

// GlobalOptions are the persistent flags of the root command (upstream argocd root
// flags, with Patrol names for the context flag and the component names).
type GlobalOptions struct {
	ConfigPath     string
	Server         string
	PlainText      bool
	Insecure       bool
	ServerCrt      string
	ClientCrt      string
	ClientCrtKey   string
	AuthToken      string
	GRPCWeb        bool
	RootPath       string
	LogFormat      string
	LogLevel       string
	Headers        []string
	PortForward    bool
	PortForwardNS  string
	HTTPRetryMax   int
	Core           bool
	Context        string
	ServerName     string
	ControllerName string
	RepoServerName string
	PromptsEnabled bool
	KubeContext    string

	// Accepted for compatibility and ignored (Patrol has no Redis).
	RedisName        string
	RedisHAProxyName string
	RedisCompress    string
}

// Global flag names.
const (
	FlagConfig         = "config"
	FlagServer         = "server"
	FlagPlainText      = "plaintext"
	FlagInsecure       = "insecure"
	FlagServerCrt      = "server-crt"
	FlagClientCrt      = "client-crt"
	FlagClientCrtKey   = "client-crt-key"
	FlagAuthToken      = "auth-token"
	FlagGRPCWeb        = "grpc-web"
	FlagRootPath       = "grpc-web-root-path"
	FlagLogFormat      = "logformat"
	FlagLogLevel       = "loglevel"
	FlagHeader         = "header"
	FlagPortForward    = "port-forward"
	FlagPortForwardNS  = "port-forward-namespace"
	FlagHTTPRetryMax   = "http-retry-max"
	FlagCore           = "core"
	FlagContext        = "patrol-context"
	FlagArgoContext    = "argocd-context"
	FlagServerName     = "server-name"
	FlagControllerName = "controller-name"
	FlagRepoServerName = "repo-server-name"
	FlagPromptsEnabled = "prompts-enabled"
	FlagKubeContext    = "kube-context"
	FlagRedisName      = "redis-name"
	FlagRedisHAProxy   = "redis-haproxy-name"
	FlagRedisCompress  = "redis-compress"
)

const redisDeprecation = "Patrol CD has no Redis; the flag is accepted for compatibility with Argo CD scripts and ignored"

// addGlobalFlags registers the global flags on fs with defaults from PATROL_OPTS and the
// environment.
func (f *Factory) addGlobalFlags(fs *pflag.FlagSet) {
	o := &f.Opts
	opts := f.envOpts
	env := func(key, fallback string) string {
		if v, ok := f.lookupEnv(key); ok && v != "" {
			return v
		}
		return fallback
	}
	defaultPath, err := config.DefaultPath(f.lookupEnv)
	if err != nil {
		f.setupErrs = append(f.setupErrs, err)
	}
	retryMax, err := opts.Int(FlagHTTPRetryMax, 0)
	if err != nil {
		f.setupErrs = append(f.setupErrs, err)
	}
	headers, err := opts.StringSlice(FlagHeader, []string{})
	if err != nil {
		f.setupErrs = append(f.setupErrs, err)
	}
	fs.StringVar(&o.ConfigPath, FlagConfig, opts.String(FlagConfig, defaultPath), "Path to Patrol CD config")
	fs.StringVar(&o.Server, FlagServer, opts.String(FlagServer, f.serverFromEnv()), "Patrol CD server address")
	fs.BoolVar(&o.PlainText, FlagPlainText, opts.Bool(FlagPlainText), "Disable TLS")
	fs.BoolVar(&o.Insecure, FlagInsecure, opts.Bool(FlagInsecure), "Skip server certificate and domain verification")
	fs.StringVar(&o.ServerCrt, FlagServerCrt, opts.String(FlagServerCrt, ""), "Server certificate file")
	fs.StringVar(&o.ClientCrt, FlagClientCrt, opts.String(FlagClientCrt, ""), "Client certificate file")
	fs.StringVar(&o.ClientCrtKey, FlagClientCrtKey, opts.String(FlagClientCrtKey, ""), "Client certificate key file")
	fs.StringVar(&o.AuthToken, FlagAuthToken, opts.String(FlagAuthToken, env(EnvAuthToken, "")), fmt.Sprintf("Authentication token; set this or the %s environment variable", EnvAuthToken))
	fs.BoolVar(&o.GRPCWeb, FlagGRPCWeb, opts.Bool(FlagGRPCWeb), "Accepted for compatibility with Argo CD and ignored: Patrol CD serves JSON over HTTP, which works through any HTTP proxy.")
	fs.StringVar(&o.RootPath, FlagRootPath, opts.String(FlagRootPath, ""), "Root path of the Patrol CD server when it is served under a sub-path (Argo CD flag name kept for compatibility).")
	fs.StringVar(&o.LogFormat, FlagLogFormat, opts.String(FlagLogFormat, "json"), "Set the logging format. One of: json|text")
	fs.StringVar(&o.LogLevel, FlagLogLevel, opts.String(FlagLogLevel, "info"), "Set the logging level. One of: debug|info|warn|error")
	fs.StringSliceVarP(&o.Headers, FlagHeader, "H", headers, "Sets additional header to all requests made by Patrol CD CLI. (Can be repeated multiple times to add multiple headers, also supports comma separated headers)")
	fs.BoolVar(&o.PortForward, FlagPortForward, opts.Bool(FlagPortForward), "Connect to a random patrol-server port using port forwarding")
	fs.StringVar(&o.PortForwardNS, FlagPortForwardNS, opts.String(FlagPortForwardNS, ""), "Namespace name which should be used for port forwarding")
	fs.IntVar(&o.HTTPRetryMax, FlagHTTPRetryMax, retryMax, "Maximum number of retries to establish http connection to Patrol CD server")
	core, err := f.coreDefault(opts)
	if err != nil {
		f.setupErrs = append(f.setupErrs, err)
	}
	fs.BoolVar(&o.Core, FlagCore, core, "If set to true then CLI talks directly to Kubernetes instead of talking to Patrol CD API server")
	fs.StringVar(&o.Context, FlagContext, opts.String(FlagContext, opts.String(FlagArgoContext, "")), "The name of the Patrol CD server context to use")
	fs.StringVar(&o.Context, FlagArgoContext, opts.String(FlagContext, opts.String(FlagArgoContext, "")), "Alias of --"+FlagContext)
	_ = fs.MarkHidden(FlagArgoContext)
	fs.StringVar(&o.ServerName, FlagServerName, env(EnvServerName, DefaultServerName), fmt.Sprintf("Name of the Patrol CD API server; set this or the %s environment variable when the server's name label differs from the default, for example when installing via the Helm chart", EnvServerName))
	fs.StringVar(&o.ControllerName, FlagControllerName, env(EnvControllerName, DefaultControllerName), fmt.Sprintf("Name of the Patrol CD Application controller; set this or the %s environment variable when the controller's name label differs from the default, for example when installing via the Helm chart", EnvControllerName))
	fs.StringVar(&o.RepoServerName, FlagRepoServerName, env(EnvRepoServerName, DefaultRepoServerName), fmt.Sprintf("Name of the Patrol CD Repo server; set this or the %s environment variable when the server's name label differs from the default, for example when installing via the Helm chart", EnvRepoServerName))
	fs.BoolVar(&o.PromptsEnabled, FlagPromptsEnabled, opts.Bool(FlagPromptsEnabled), "Force optional interactive prompts to be enabled or disabled, overriding local configuration. If not specified, the local configuration value will be used, which is false by default.")
	fs.StringVar(&o.KubeContext, FlagKubeContext, opts.String(FlagKubeContext, ""), "Directs the command to the given kube-context")

	fs.StringVar(&o.RedisName, FlagRedisName, opts.String(FlagRedisName, ""), "Ignored (Patrol CD has no Redis)")
	fs.StringVar(&o.RedisHAProxyName, FlagRedisHAProxy, opts.String(FlagRedisHAProxy, ""), "Ignored (Patrol CD has no Redis)")
	fs.StringVar(&o.RedisCompress, FlagRedisCompress, opts.String(FlagRedisCompress, ""), "Ignored (Patrol CD has no Redis)")
	for _, n := range []string{FlagRedisName, FlagRedisHAProxy, FlagRedisCompress} {
		_ = fs.MarkDeprecated(n, redisDeprecation)
	}
}

// serverFromEnv returns PATROL_SERVER, else the ARGOCD_SERVER migration alias.
func (f *Factory) serverFromEnv() string {
	if v, ok := f.lookupEnv(EnvServer); ok && v != "" {
		return v
	}
	if v, ok := f.lookupEnv(EnvArgoServer); ok && v != "" {
		f.notes = append(f.notes, fmt.Sprintf("%s is honored as a migration alias of %s; set %s instead", EnvArgoServer, EnvServer, EnvServer))
		f.argoServer = v
		return v
	}
	return ""
}

// coreDefault returns the default of --core: PATROL_OPTS when it sets the flag, else
// PATROL_CORE.
func (f *Factory) coreDefault(opts Opts) (bool, error) {
	if _, ok := opts[FlagCore]; ok {
		return opts.Bool(FlagCore), nil
	}
	v, ok := f.lookupEnv(EnvCore)
	if !ok || strings.TrimSpace(v) == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return false, fmt.Errorf("%s: invalid boolean value %q", EnvCore, v)
	}
	return b, nil
}
