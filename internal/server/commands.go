package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/clusters/k8sauth"
	"github.com/juliosaraiva/patrol-cd/internal/cmp"
	"github.com/juliosaraiva/patrol-cd/internal/commitserver"
	"github.com/juliosaraiva/patrol-cd/internal/controller/sharding"
	notifcontroller "github.com/juliosaraiva/patrol-cd/internal/notifications/controller"
	"github.com/juliosaraiva/patrol-cd/internal/render/cmpwire"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// BinaryName is the name of the multi-call binary.
const BinaryName = "patrolcd"

// EnvBinaryName selects the subcommand like the binary name does (upstream
// ARGOCD_BINARY_NAME).
const EnvBinaryName = "PATROL_BINARY_NAME"

// binaryAliases maps executable names (symbolic links in the image) to subcommands
// (OPS-145, OPS-153).
var binaryAliases = map[string]string{
	"patrol-server":                    "server",
	"patrol-controller":                "controller",
	"patrol-application-controller":    "controller",
	"patrol-repo-server":               "repo-server",
	"patrol-applicationset-controller": "appset-controller",
	"patrol-appset-controller":         "appset-controller",
	"patrol-notifications":             "notifications-controller",
	"patrol-notifications-controller":  "notifications-controller",
	"patrol-cmp-server":                "cmp-server",
	"patrol-commit-server":             "commit-server",
	"patrol-k8s-auth":                  "k8s-auth",
}

// ResolveArgs returns the arguments of the command tree for a process started as argv0 with
// args: a known executable name (or $PATROL_BINARY_NAME) selects its subcommand.
func ResolveArgs(argv0 string, args []string, lookupEnv func(string) (string, bool)) []string {
	name := filepath.Base(argv0)
	if lookupEnv != nil {
		if v, ok := lookupEnv(EnvBinaryName); ok && v != "" {
			name = v
		}
	}
	if sub, ok := binaryAliases[name]; ok {
		return append([]string{sub}, args...)
	}
	return args
}

// Main runs the command tree and returns the process exit code. Helper modes must have been
// dispatched before (DispatchHelpers).
func Main(argv0 string, args []string, stdout, stderr io.Writer) int {
	root := NewRootCommand()
	root.SetArgs(ResolveArgs(argv0, args, os.LookupEnv))
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.Execute(); err != nil {
		_, _ = fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

// NewRootCommand returns the patrolcd command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   BinaryName,
		Short: "Patrol CD control-plane components",
		Long: "patrolcd runs the Patrol CD control-plane components. Each subcommand is one component;\n" +
			"the image also links the binary under each component name (patrol-server, patrol-repo-server, ...).\n\n" +
			"Configuration precedence for every patrol-cmd-params-cm parameter:\n" +
			"  command-line flag > environment variable (PATROL_*) > patrol-cmd-params-cm > default",
		SilenceUsage:      true,
		SilenceErrors:     true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help()
		},
	}
	root.AddCommand(
		newServerCommand(),
		newControllerCommand(),
		newRepoServerCommand(),
		newAppSetControllerCommand(),
		newNotificationsControllerCommand(),
		newCMPServerCommand(),
		newCommitServerCommand(),
		k8sauth.NewCommand(k8sauth.Config{}),
		newBootstrapCommand(),
		newVersionCommand(),
	)
	return root
}

func newVersionCommand() *cobra.Command {
	var short bool
	var output string
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return writeVersion(c.OutOrStdout(), short, output)
		},
	}
	cmd.Flags().BoolVar(&short, "short", false, "Print the version number only")
	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: json or wide")
	return cmd
}

// component describes one component command.
type component struct {
	use, short, long string
	settings         settings.Component
	flags            commonFlags
	common           CommonOptions
	pf               *ParamFlags
	fs               *pflag.FlagSet
	// extra registers the command's own flags (before the parameter flags, so they win a
	// name collision).
	extra func(fs *pflag.FlagSet)
	// logOverride returns fixed log options (commands without log parameters).
	logOverride func() *LogOptions
	// tracing names the OpenTelemetry service of components with otlp.* parameters.
	tracing string
	run     func(ctx context.Context, rt *Runtime) error
}

// start waits for the Patrol CRDs (components that use the control-plane API only), then runs the
// component. The wait ends crdWaitTimeout after began, the start of the process, so that it counts
// the time bootstrap took. A stop during the wait is a clean stop, as it is during the run.
func (c *component) start(ctx context.Context, rt *Runtime, began time.Time) error {
	if c.flags.kube && rt.Rest != nil {
		wait := max(crdWaitTimeout-time.Since(began), crdWaitInterval)
		if err := waitForCRDs(ctx, rt.Rest, rt.Log, wait, crdWaitInterval); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
	return c.run(ctx, rt)
}

func (c *component) command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   c.use,
		Short: c.short,
		Long:  c.long,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			began := time.Now()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// A second signal terminates the process at once (default disposition).
			go func() { <-ctx.Done(); stop() }()
			in := bootstrapInput{
				name: c.use, component: c.settings, pf: c.pf, common: c.common, kube: c.flags.kube,
				tokenExplicit: c.fs.Changed("internal-token-file"), stderr: cmd.ErrOrStderr(),
			}
			if c.logOverride != nil {
				in.logOverride = c.logOverride()
			}
			rt, err := bootstrap(ctx, in)
			if err != nil {
				return err
			}
			if c.tracing != "" && rt.Params != nil {
				shutdown, err := setupTracing(ctx, TracingOptions{
					Address: rt.Params.String("otlp.address"), Insecure: rt.Params.Bool("otlp.insecure"),
					Headers: rt.Params.StringMap("otlp.headers"), Attrs: rt.Params.Strings("otlp.attrs"), ServiceName: c.tracing,
				}, rt.Log)
				if err != nil {
					return err
				}
				defer func() {
					sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
					defer cancel()
					_ = shutdown(sctx)
				}()
			}
			rt.Log.Info("starting", "version", VersionInfo().Version, "namespace", rt.Namespace)
			err = c.start(ctx, rt, began)
			if err == nil || errors.Is(err, context.Canceled) {
				rt.Log.Info("stopped")
				return nil
			}
			return err
		},
	}
	c.fs = cmd.Flags()
	c.fs.SortFlags = true
	addCommonFlags(c.fs, &c.common, c.flags)
	if c.extra != nil {
		c.extra(c.fs)
	}
	if c.settings != "" {
		c.pf = BindParamFlags(c.fs, c.settings)
	}
	return cmd
}

// metricsAddress returns --metrics-addr when given, else host:port of the component's
// metrics address parameter and port flag.
func (c *component) metricsAddress(p *settings.Params, hostKey string, port int) string {
	if c.fs.Changed("metrics-addr") || c.common.MetricsAddr != "" {
		return c.common.MetricsAddr
	}
	host := ""
	if p != nil && hostKey != "" {
		host = p.String(hostKey)
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// pprofFile is the file whose content "true" enables the profiler per request.
func pprofFile() string {
	if p := os.Getenv(EnvProfilerFile); p != "" {
		return p
	}
	return DefaultProfilerFile
}

// internalToken loads the internal token (file, or the Secret during development).
func internalToken(ctx context.Context, rt *Runtime) (string, error) {
	return loadInternalToken(ctx, rt.Common.InternalTokenFile, rt.TokenExplicit, rt.Kube, rt.Namespace)
}

// clusterTuning returns the destination cluster client tuning (PATROL_K8S* variables and
// the <component>.k8s.* parameters).
func clusterTuning(rt *Runtime) *clusters.Tuning {
	t := clusters.TuningFromLookup(paramEnvLookup(rt.Params, os.LookupEnv), rt.Log)
	return &t
}

// optionalString returns a pointer to the parameter value when it was configured anywhere
// (nil: the library default applies).
func optionalString(p *settings.Params, key string) *string {
	v, ok := p.Lookup(key)
	if !ok || v.Source == settings.ParamFromDefault {
		return nil
	}
	s := p.String(key)
	return &s
}

func newServerCommand() *cobra.Command {
	var (
		port, metricsPort                     int
		extensionsDir, downloadDir            string
		treeURL, treeService, treeSts, treeCA string
		treePort                              int
		treeTLS, allowClusterExec             bool
		trustedProxies                        []string
		dexCA, dexCert                        string
	)
	c := &component{
		use: "server", short: "Run patrol-server (API, web UI, webhooks)",
		settings: settings.ComponentServer, tracing: "patrol-server",
		flags: commonFlags{kube: true, metrics: true, healthz: true, tls: true, token: true},
		extra: func(fs *pflag.FlagSet) {
			fs.IntVar(&port, "port", DefaultServerPort, "Port of the API, UI and webhook listener")
			fs.IntVar(&metricsPort, "metrics-port", DefaultServerMetricsPort, "Port of the metrics listener")
			fs.StringVar(&extensionsDir, "extensions-dir", DefaultExtensionsDir, "Directory of UI extensions (extension*.js concatenated into /extensions.js; other files served under /extensions/)")
			fs.StringVar(&downloadDir, "download-dir", DefaultDownloadDir, "Directory of CLI binaries served under /download/ (patrol-<os>-<arch>)")
			fs.StringVar(&treeURL, "controller-tree-api-url", "", "Send every controller tree API call to this URL (single controller, development); default: route to the replica owning the shard")
			fs.StringVar(&treeService, "controller-service", "patrol-controller", "Headless Service of the controller StatefulSet (tree API routing)")
			fs.StringVar(&treeSts, "controller-statefulset", "patrol-controller", "Controller StatefulSet name (tree API routing)")
			fs.IntVar(&treePort, "controller-tree-api-port", 8090, "Port of the controller tree API")
			fs.BoolVar(&treeTLS, "controller-tree-api-tls", false, "Use TLS to reach the controller tree API")
			fs.StringVar(&treeCA, "controller-tree-api-ca-cert-path", "", "CA bundle verifying the controller tree API certificate (default: not verified; the internal token authenticates)")
			fs.BoolVar(&allowClusterExec, "allow-cluster-exec", false, "Let cluster connection checks run exec credential plugins and use the server's own cloud identity")
			fs.StringSliceVar(&trustedProxies, "trusted-proxies", nil, "CIDRs of reverse proxies whose X-Forwarded-For is trusted (rate limiting, logs, webhook source)")
			fs.StringVar(&dexCA, "dex-server-ca-cert-path", filepath.Join(DefaultDexTLSDir, "ca.crt"), "CA bundle verifying Dex's certificate with --dex-server-strict-tls")
			fs.StringVar(&dexCert, "dex-server-cert-path", filepath.Join(DefaultDexTLSDir, "tls.crt"), "Dex's certificate, pinned with --dex-server-strict-tls (Dex must present exactly this certificate)")
		},
	}
	c.run = func(ctx context.Context, rt *Runtime) error {
		p := rt.Params
		token, err := internalToken(ctx, rt)
		if err != nil {
			return err
		}
		proxies, err := parsePrefixes(trustedProxies)
		if err != nil {
			return fmt.Errorf("--trusted-proxies: %w", err)
		}
		healthz := rt.Common.HealthzAddr
		cfg := Config{
			Namespace: rt.Namespace, RestConfig: rt.Rest,
			ListenAddress:  net.JoinHostPort(p.String("server.listen.address"), strconv.Itoa(port)),
			MetricsAddress: c.metricsAddress(p, "server.metrics.listen.address", metricsPort),
			HealthzAddress: healthz, EnablePprof: rt.Common.EnablePprof, PprofFile: pprofFile(),
			Insecure:      p.Bool("server.insecure"),
			TLSMinVersion: p.String("server.tls.minversion"), TLSMaxVersion: p.String("server.tls.maxversion"),
			TLSCiphers:  p.String("server.tls.ciphers"),
			TLSCertFile: rt.Common.TLSCert, TLSKeyFile: rt.Common.TLSKey,
			RootPath: p.String("server.rootpath"), BaseHRef: p.String("server.basehref"),
			XFrameOptions:         optionalString(p, "server.x.frame.options"),
			ContentSecurityPolicy: optionalString(p, "server.content.security.policy"),
			DisableAuth:           p.Bool("server.disable.auth"),
			TrustedProxies:        proxies,
			StaticAssetsDir:       p.String("server.staticassets"),
			ExtensionsDir:         extensionsDir, DownloadDir: downloadDir,
			InternalToken: token,
			RepoServer:    repoServerOptionsFromParams(p, "server", ""),
			TreeAPI: TreeAPIClientOptions{
				URL: treeURL, Service: treeService, StatefulSet: treeSts, Port: treePort,
				TLS: TLSClientOptions{Plaintext: !treeTLS, CAFile: treeCA},
			},
			DexServer: p.String("server.dex.server"), DexPlaintext: p.Bool("server.dex.server.plaintext"),
			DexStrictTLS: p.Bool("server.dex.server.strict.tls"),
			DexCAFile:    dexCA, DexCertFile: dexCert,
			ApplicationNamespaces:    p.Strings("application.namespaces"),
			ApplicationSetNamespaces: nil,
			HydratorEnabled:          p.Bool("hydrator.enabled"),
			ConnectionStateTTL:       p.Duration("server.connection.status.cache.expiration"),
			SyncWithReplaceAllowed:   p.Bool("server.sync.replace.allowed"),
			EnableProxyExtension:     p.Bool("server.enable.proxy.extension"),
			WebhookParallelism:       int(p.Int("server.webhook.parallelism.limit")),
			WebhookRefreshWorkers:    int(p.Int("server.webhook.refresh.workers")),
			AppSet: AppSetAPIOptions{
				EnableSCMProviders:       p.Bool("applicationsetcontroller.enable.scm.providers"),
				AllowedSCMProviders:      p.Strings("applicationsetcontroller.allowed.scm.providers"),
				SCMRootCAPath:            p.String("applicationsetcontroller.scm.root.ca.path"),
				EnableNewGitFileGlobbing: p.Bool("applicationsetcontroller.enable.new.git.file.globbing"),
				EnableGitHubAPIMetrics:   p.Bool("applicationsetcontroller.enable.github.api.metrics"),
			},
			AllowClusterExec: allowClusterExec,
			ClusterTuning:    clusterTuning(rt),
			ShutdownTimeout:  rt.Common.ShutdownTimeout,
			Registry:         rt.Registry,
			Logger:           rt.Log,
		}
		if ns := os.Getenv("PATROL_APPLICATIONSET_CONTROLLER_NAMESPACES"); ns != "" {
			cfg.ApplicationSetNamespaces = splitList(ns)
		}
		warnNoEffect(rt, p, "server.enable.gzip", "server.api.content.types", "server.oidc.cache.expiration",
			"server.app.state.cache.expiration", "server.default.cache.expiration", "server.glob.cache.size")
		return Run(ctx, cfg)
	}
	return c.command()
}

// warnNoEffect logs the configured parameters this build accepts without using them.
func warnNoEffect(rt *Runtime, p *settings.Params, keys ...string) {
	for _, k := range keys {
		if v, ok := p.Lookup(k); ok && v.Source != settings.ParamFromDefault {
			rt.Log.Warn("parameter has no effect in this version of patrol-server", "key", k)
		}
	}
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func parsePrefixes(in []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			a, err := netip.ParseAddr(s)
			if err != nil {
				return nil, err
			}
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func newControllerCommand() *cobra.Command {
	var (
		metricsPort   int
		treeAddr      string
		dynamic       bool
		commitTimeout time.Duration
		commitTLS     TLSClientOptions
	)
	c := &component{
		use: "controller", short: "Run the application controller (reconcilers, sharding, tree API, source hydrator)",
		settings: settings.ComponentController, tracing: "patrol-controller",
		flags: commonFlags{kube: true, metrics: true, healthz: true, tls: true, clientCA: true, token: true},
		extra: func(fs *pflag.FlagSet) {
			fs.IntVar(&metricsPort, "metrics-port", DefaultControllerMetricsPort, "Port of the metrics listener")
			fs.StringVar(&treeAddr, "tree-api-addr", DefaultTreeAPIAddress, `Address of the tree API served to patrol-server ("-" disables it); TLS with --tls-cert/--tls-key`)
			fs.BoolVar(&dynamic, "dynamic-cluster-distribution-enabled", false, "Read the shard count from the controller workload (env "+sharding.EnvDynamicClusterDistribution+")")
			fs.DurationVar(&commitTimeout, "commit-server-timeout", commitserver.DefaultClientTimeout, "Bound of one source hydrator commit request (clone, write, push)")
			fs.StringVar(&commitTLS.CAFile, "commit-server-ca-cert-path", "", "CA bundle verifying the commit-server certificate (https commit.server; default: not verified, the internal token authenticates)")
			fs.StringVar(&commitTLS.ClientCertFile, "commit-server-client-cert-path", "", "Client certificate presented to a commit-server requiring mutual TLS (https commit.server)")
			fs.StringVar(&commitTLS.ClientKeyFile, "commit-server-client-cert-key-path", "", "Key of --commit-server-client-cert-path")
		},
	}
	c.run = func(ctx context.Context, rt *Runtime) error {
		p := rt.Params
		token, err := internalToken(ctx, rt)
		if err != nil {
			return err
		}
		host, _ := os.Hostname()
		shardCfg := sharding.ConfigFromEnv(paramEnvLookup(p, os.LookupEnv), host, rt.Log)
		if c.fs.Changed("dynamic-cluster-distribution-enabled") {
			shardCfg.DynamicDistribution = dynamic
		}
		return RunController(ctx, ControllerConfig{
			Namespace: rt.Namespace, RestConfig: rt.Rest, Params: p, InternalToken: token,
			RepoServer:      repoServerOptionsFromParams(p, "controller", ""),
			CommitServer:    CommitServerClientOptions{Address: p.String("commit.server"), Timeout: commitTimeout, TLS: commitTLS},
			HydratorEnabled: p.Bool("hydrator.enabled"),
			MetricsAddress:  c.metricsAddress(p, "", metricsPort), HealthzAddress: rt.Common.HealthzAddr,
			EnablePprof: rt.Common.EnablePprof, PprofFile: pprofFile(),
			TreeAPIAddress: treeAddr,
			TreeAPITLS:     TLSServerOptions{CertFile: rt.Common.TLSCert, KeyFile: rt.Common.TLSKey, ClientCAFile: rt.Common.TLSClientCA},
			Sharding:       shardCfg, Identity: host,
			ClusterTuning:       clusterTuning(rt),
			ClusterInfoInterval: clusters.ClusterInfoIntervalFromLookup(os.LookupEnv, rt.Log),
			ShutdownTimeout:     rt.Common.ShutdownTimeout, Registry: rt.Registry, Logger: rt.Log,
		})
	}
	return c.command()
}

func newRepoServerCommand() *cobra.Command {
	var (
		port, metricsPort, maxWorkingCopies                     int
		rootDir, socketDir, knownHosts, certsDir, gpgDir, gnupg string
		helmPlugins, credentialsDir                             string
		revisionCacheExpiration                                 time.Duration
	)
	c := &component{
		use: "repo-server", short: "Run the repo-server (sources and manifest rendering; no Kubernetes access)",
		settings: settings.ComponentRepoServer, tracing: "patrol-repo-server",
		flags: commonFlags{metrics: true, tls: true, clientCA: true, token: true},
		extra: func(fs *pflag.FlagSet) {
			fs.IntVar(&port, "port", DefaultRepoServerPort, "Port of the API listener")
			fs.IntVar(&metricsPort, "metrics-port", DefaultRepoServerMetricsPort, "Port of the probe and metrics listener")
			fs.StringVar(&rootDir, "root-dir", "", "Working directory of clones, extracted artifacts and uploads (default $TMPDIR/_patrol-repo)")
			fs.IntVar(&maxWorkingCopies, "max-working-copies", reposerver.DefaultMaxWorkingCopies, "Maximum persistent working copies (least recently used evicted)")
			fs.StringVar(&credentialsDir, "credentials-dir", "", "Directory of per-command credential files (tmpfs recommended; default $TMPDIR)")
			fs.StringVar(&socketDir, "plugin-socket-dir", cmpwire.DefaultSocketDir, "Directory of config management plugin sockets")
			fs.StringVar(&knownHosts, "ssh-known-hosts-path", reposerver.DefaultSSHKnownHostsPath, `SSH known hosts file mounted from patrol-ssh-known-hosts-cm ("-" disables)`)
			fs.StringVar(&certsDir, "tls-certs-dir", reposerver.DefaultTLSCertsDir, `Repository TLS certificates directory mounted from patrol-tls-certs-cm ("-" disables)`)
			fs.StringVar(&gpgDir, "gpg-keys-dir", reposerver.DefaultGPGKeysDir, `GnuPG public keys directory mounted from patrol-gpg-keys-cm ("-" disables)`)
			fs.StringVar(&gnupg, "gnupg-home-parent", "", "Parent directory of the private GnuPG keyring (default $TMPDIR)")
			fs.StringVar(&helmPlugins, "helm-plugins-dir", "", "Directory of operator-installed Helm plugins")
			fs.DurationVar(&revisionCacheExpiration, "revision-cache-expiration", reposerver.DefaultRevisionCacheExpiration, "Cache expiration of resolved revisions")
		},
	}
	c.run = func(ctx context.Context, rt *Runtime) error {
		p := rt.Params
		token, err := internalToken(ctx, rt)
		if err != nil {
			return err
		}
		rc := repoServerConfigFromParams(p, os.LookupEnv, rt.Log)
		rc.ListenAddress = net.JoinHostPort(p.String("reposerver.listen.address"), strconv.Itoa(port))
		rc.MetricsAddress = c.metricsAddress(p, "reposerver.metrics.listen.address", metricsPort)
		if rc.MetricsAddress == "-" {
			rc.MetricsAddress = ""
		}
		rc.Token = token
		rc.RootDir, rc.MaxWorkingCopies, rc.CredentialsDir = rootDir, maxWorkingCopies, credentialsDir
		rc.PluginSocketDir = socketDir
		rc.SSHKnownHostsPath, rc.TLSCertsDir, rc.GPGKeysDir = knownHosts, certsDir, gpgDir
		rc.GnuPGHomeParent, rc.HelmPluginsDir = gnupg, helmPlugins
		rc.RevisionCacheExpiration = revisionCacheExpiration
		rc.Registerer, rc.Gatherer, rc.Logger = rt.Registry, rt.Registry, rt.Log
		clientCA := p.String("reposerver.client.ca.path")
		if c.fs.Changed("tls-client-ca") {
			clientCA = rt.Common.TLSClientCA
		}
		return RunRepoServer(ctx, RepoServerConfig{Server: rc, TLS: TLSServerOptions{
			CertFile: rt.Common.TLSCert, KeyFile: rt.Common.TLSKey, ClientCAFile: clientCA,
			MinVersion: p.String("reposerver.tls.minversion"), MaxVersion: p.String("reposerver.tls.maxversion"),
			Ciphers: p.String("reposerver.tls.ciphers"),
		}})
	}
	return c.command()
}

func newAppSetControllerCommand() *cobra.Command {
	var (
		webhookAddr, scmProxy, scmNoProxy string
		concurrentUpdates                 int
		appSetLabels                      []string
		cacheSyncPeriod                   time.Duration
	)
	c := &component{
		use: "appset-controller", short: "Run the ApplicationSet controller",
		settings: settings.ComponentAppSetController,
		flags:    commonFlags{kube: true, metrics: true, metricsDefault: DefaultAppSetMetricsAddr, healthz: true, healthzDefault: DefaultAppSetProbeAddr, token: true},
		extra: func(fs *pflag.FlagSet) {
			fs.StringVar(&webhookAddr, "webhook-addr", "", "Address receiving SCM webhooks (upstream :7000); patrol-server's /api/webhook refreshes ApplicationSets too")
			fs.StringVar(&scmProxy, "scm-proxy-url", "", "Proxy of SCM provider API requests")
			fs.StringVar(&scmNoProxy, "scm-no-proxy", "", "Hosts reached without the SCM proxy")
			fs.IntVar(&concurrentUpdates, "concurrent-application-updates", 1, "Parallel Application writes within one reconcile")
			fs.StringSliceVar(&appSetLabels, "metrics-applicationset-labels", nil, "ApplicationSet label keys exported by patrol_appset_labels")
			fs.DurationVar(&cacheSyncPeriod, "cache-sync-period", 10*time.Hour, "Informer resync period")
		},
	}
	c.run = func(ctx context.Context, rt *Runtime) error {
		p := rt.Params
		token, err := internalToken(ctx, rt)
		if err != nil {
			return err
		}
		rc, err := appSetRunConfigFromParams(p, rt.Namespace, rt.Rest)
		if err != nil {
			return err
		}
		rc.Generators.SCM.ProxyURL, rc.Generators.SCM.NoProxy = scmProxy, scmNoProxy
		rc.ConcurrentApplicationUpdates = concurrentUpdates
		rc.MetricsApplicationSetLabels = appSetLabels
		rc.CacheSyncPeriod = cacheSyncPeriod
		if p.Bool("applicationsetcontroller.debug") {
			rt.Log.Debug("debug logging enabled by applicationsetcontroller.debug")
		}
		return RunAppSetController(ctx, AppSetConfig{
			Run: rc, InternalToken: token,
			RepoServer:         repoServerOptionsFromParams(p, "applicationsetcontroller", ""),
			SubmodulesEnabled:  p.Bool("applicationsetcontroller.enable.git.submodule"),
			NewGitFileGlobbing: p.Bool("applicationsetcontroller.enable.new.git.file.globbing"),
			MetricsAddress:     rt.Common.MetricsAddr, HealthzAddress: rt.Common.HealthzAddr, WebhookAddress: webhookAddr,
			EnablePprof: rt.Common.EnablePprof, PprofFile: pprofFile(),
			WebhookParallelism: int(p.Int("applicationsetcontroller.webhook.parallelism.limit")),
			ShutdownTimeout:    rt.Common.ShutdownTimeout, Registry: rt.Registry, Logger: rt.Log,
		})
	}
	return c.command()
}

func newNotificationsControllerCommand() *cobra.Command {
	var (
		repoServer, selector, cmName, secretName string
		leaderElection, allowPrivate             bool
		egress, tenantEgress                     []string
	)
	c := &component{
		use: "notifications-controller", short: "Run the notifications controller",
		settings: settings.ComponentNotificationsController,
		flags:    commonFlags{kube: true, metrics: true, metricsDefault: DefaultNotificationsMetricsAddr, healthz: true, token: true},
		extra: func(fs *pflag.FlagSet) {
			fs.StringVar(&repoServer, "repo-server", DefaultRepoServerAddress, "Repo-server address of the repo.* template helpers")
			fs.StringVar(&selector, "app-label-selector", "", "Label selector restricting the watched Applications")
			fs.StringVar(&cmName, "config-map-name", notifcontroller.DefaultConfigMapName, "Notifications ConfigMap")
			fs.StringVar(&secretName, "secret-name", notifcontroller.DefaultSecretName, "Notifications Secret")
			fs.BoolVar(&leaderElection, "enable-leader-election", false, "Run only on the replica holding the leader election Lease")
			fs.BoolVar(&allowPrivate, "allow-private-egress", false, "Let control-plane notification services reach private, loopback and link-local addresses")
			fs.StringSliceVar(&egress, "egress-allowed-cidrs", nil, "Non-public CIDRs control-plane notification services may reach")
			fs.StringSliceVar(&tenantEgress, "tenant-egress-allowed-cidrs", nil, "Non-public CIDRs self-service (namespace) configurations may reach")
		},
	}
	c.run = func(ctx context.Context, rt *Runtime) error {
		p := rt.Params
		token, err := internalToken(ctx, rt)
		if err != nil {
			rt.Log.Warn("internal token unavailable; the repo.* template helpers are disabled", "error", err)
			token = ""
		}
		eg, err := parsePrefixes(egress)
		if err != nil {
			return fmt.Errorf("--egress-allowed-cidrs: %w", err)
		}
		teg, err := parsePrefixes(tenantEgress)
		if err != nil {
			return fmt.Errorf("--tenant-egress-allowed-cidrs: %w", err)
		}
		return RunNotificationsController(ctx, NotificationsConfig{
			Controller: notifcontroller.Config{
				RestConfig: rt.Rest, Namespace: rt.Namespace,
				ApplicationNamespaces: p.Strings("application.namespaces"), AppLabelSelector: selector,
				ConfigMapName: cmName, SecretName: secretName,
				SelfServiceEnabled: p.Bool("notificationscontroller.selfservice.enabled"),
				Processors:         int(p.Int("notificationscontroller.processors.count")),
				AllowPrivateEgress: allowPrivate, EgressAllowedPrefixes: eg, TenantEgressAllowedPrefixes: teg,
				LeaderElection: leaderElection,
			},
			InternalToken: token, RepoServer: repoServerOptionsFromParams(p, "notificationscontroller", repoServer),
			MetricsAddress: rt.Common.MetricsAddr, HealthzAddress: rt.Common.HealthzAddr,
			EnablePprof: rt.Common.EnablePprof, PprofFile: pprofFile(),
			ShutdownTimeout: rt.Common.ShutdownTimeout, Registry: rt.Registry, Logger: rt.Log,
		})
	}
	return c.command()
}

func newCommitServerCommand() *cobra.Command {
	var (
		port, metricsPort, maxConcurrent             int
		workDir, credentialsDir, gitPath, knownHosts string
		certsDir                                     string
		gitTimeout                                   time.Duration
	)
	c := &component{
		use: "commit-server", short: "Run the commit-server (source hydrator writes; no Kubernetes access)",
		settings: settings.ComponentCommitServer,
		flags:    commonFlags{metrics: true, healthz: true, tls: true, clientCA: true, token: true},
		extra: func(fs *pflag.FlagSet) {
			fs.IntVar(&port, "port", DefaultCommitServerPort, "Port of the API listener")
			fs.IntVar(&metricsPort, "metrics-port", DefaultCommitServerMetricsPort, "Port of the metrics listener")
			fs.StringVar(&workDir, "work-dir", "", "Directory of per-request clones (an ephemeral volume; default $TMPDIR)")
			fs.StringVar(&credentialsDir, "credentials-dir", "", "Directory of per-command credential files (tmpfs recommended; default $TMPDIR)")
			fs.StringVar(&gitPath, "git-path", "git", "git executable")
			fs.DurationVar(&gitTimeout, "git-timeout", 0, "Timeout of each git command (0: the internal/git default)")
			fs.IntVar(&maxConcurrent, "max-concurrent-requests", 0, "Requests processed at once (0: unlimited)")
			fs.StringVar(&knownHosts, "ssh-known-hosts-path", reposerver.DefaultSSHKnownHostsPath, `SSH known hosts file mounted from patrol-ssh-known-hosts-cm ("-" disables)`)
			fs.StringVar(&certsDir, "tls-certs-dir", reposerver.DefaultTLSCertsDir, `Repository TLS certificates directory mounted from patrol-tls-certs-cm ("-" disables)`)
		},
	}
	c.run = func(ctx context.Context, rt *Runtime) error {
		p := rt.Params
		token, err := internalToken(ctx, rt)
		if err != nil {
			return err
		}
		hosts, certs, err := CommitTrust(knownHosts, certsDir, rt.Log)
		if err != nil {
			return err
		}
		return RunCommitServer(ctx, CommitServerConfig{
			Service: commitserver.ServiceOptions{
				WorkDir: workDir, CredentialsDir: credentialsDir, GitPath: gitPath, GitTimeout: gitTimeout,
				MaxConcurrentRequests: maxConcurrent, SSHKnownHosts: hosts, TLSCertificates: certs, Logger: rt.Log,
			},
			ListenAddress:  net.JoinHostPort(p.String("commitserver.listen.address"), strconv.Itoa(port)),
			MetricsAddress: c.metricsAddress(p, "commitserver.metrics.listen.address", metricsPort),
			HealthzAddress: rt.Common.HealthzAddr, EnablePprof: rt.Common.EnablePprof, PprofFile: pprofFile(),
			Token:           token,
			TLS:             TLSServerOptions{CertFile: rt.Common.TLSCert, KeyFile: rt.Common.TLSKey, ClientCAFile: rt.Common.TLSClientCA},
			ShutdownTimeout: rt.Common.ShutdownTimeout, Registry: rt.Registry, Logger: rt.Log,
		})
	}
	return c.command()
}

func newCMPServerCommand() *cobra.Command {
	opts := cmp.DefaultOptions()
	var envErr error
	if o, err := cmp.OptionsFromEnv(os.LookupEnv); err == nil {
		opts = o
	} else {
		envErr = err
	}
	var cmpFlags *pflag.FlagSet
	c := &component{
		use: "cmp-server", short: "Run a config management plugin sidecar server",
		flags: commonFlags{metrics: true, metricsDefault: "-"},
		extra: func(fs *pflag.FlagSet) { cmpFlags = fs; opts.AddFlags(fs) },
		logOverride: func() *LogOptions {
			o := &LogOptions{Level: opts.LogLevel, Format: opts.LogFormat}
			if !fsChanged(cmpFlags, "logformat") && !envSet(cmp.EnvLogFormat, cmp.EnvLogFormatArgo) {
				o.Format = defaultLogFormat()
			}
			return o
		},
	}
	c.run = func(ctx context.Context, rt *Runtime) error {
		if envErr != nil {
			return envErr
		}
		if !fsChanged(cmpFlags, "logformat") && !envSet(cmp.EnvLogFormat, cmp.EnvLogFormatArgo) {
			opts.LogFormat = defaultLogFormat()
		}
		return RunCMPServer(ctx, CMPServerConfig{
			Options: opts, MetricsAddress: rt.Common.MetricsAddr,
			ShutdownTimeout: rt.Common.ShutdownTimeout, Registry: rt.Registry, Logger: rt.Log,
		})
	}
	return c.command()
}

func fsChanged(fs *pflag.FlagSet, name string) bool { return fs != nil && fs.Changed(name) }

func envSet(names ...string) bool {
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok && strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}
