package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/pflag"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// Defaults of the common options.
const (
	// DefaultShutdownTimeout bounds the graceful drain after SIGTERM.
	DefaultShutdownTimeout = 30 * time.Second
	// cmdParamsReadTimeout bounds reading patrol-cmd-params-cm at startup.
	cmdParamsReadTimeout = 15 * time.Second
)

// CommonOptions are the options shared by the component commands.
type CommonOptions struct {
	// Kubeconfig, Context and Namespace select the control-plane cluster and namespace.
	// Without a kubeconfig the in-cluster configuration is used; without a namespace the
	// pod's namespace (POD_NAMESPACE or the service account namespace), else the kubeconfig
	// context's namespace.
	Kubeconfig string
	Context    string
	Namespace  string
	// MetricsAddr serves /metrics, the probes and the profiler ("-" disables).
	MetricsAddr string
	// HealthzAddr serves the probes on a separate listener ("" or "-": only on MetricsAddr).
	HealthzAddr string
	// TLS of the component's internal listener.
	TLSCert, TLSKey, TLSClientCA string
	// EnablePprof serves /debug/pprof/ on the metrics listener without the per-request file
	// check (off by default).
	EnablePprof bool
	// InternalTokenFile holds the shared internal token.
	InternalTokenFile string
	// ShutdownTimeout bounds the graceful drain after SIGTERM/SIGINT.
	ShutdownTimeout time.Duration
}

// commonFlags selects the common flags a command registers.
type commonFlags struct {
	kube bool
	// metrics registers --metrics-addr (with metricsDefault; "" means computed from the
	// component's address and port flags) and --enable-pprof.
	metrics        bool
	metricsDefault string
	// healthz registers --healthz-addr with healthzDefault.
	healthz        bool
	healthzDefault string
	// tls registers --tls-cert and --tls-key; clientCA also --tls-client-ca.
	tls, clientCA bool
	token         bool
}

// addCommonFlags registers the selected common flags on fs.
func addCommonFlags(fs *pflag.FlagSet, o *CommonOptions, c commonFlags) {
	if c.kube {
		fs.StringVar(&o.Kubeconfig, "kubeconfig", "", "Path to a kubeconfig file (default: in-cluster configuration, else $KUBECONFIG or ~/.kube/config)")
		fs.StringVar(&o.Context, "context", "", "Kubeconfig context to use")
		fs.StringVarP(&o.Namespace, "namespace", "n", "", "Control-plane namespace (default: the pod namespace, else the kubeconfig context namespace)")
	}
	if c.metrics {
		usage := `Address of the metrics, probe and profiler listener ("-" disables it)`
		if c.metricsDefault == "" {
			usage += " (default: --metrics-address and --metrics-port)"
		}
		fs.StringVar(&o.MetricsAddr, "metrics-addr", c.metricsDefault, usage)
		fs.BoolVar(&o.EnablePprof, "enable-pprof", false, "Serve /debug/pprof/ on the metrics listener (otherwise only while the file named by $"+EnvProfilerFile+" contains \"true\")")
	}
	if c.healthz {
		fs.StringVar(&o.HealthzAddr, "healthz-addr", c.healthzDefault, `Separate listener serving /healthz, /readyz and /livez ("" or "-": the metrics listener only)`)
	}
	if c.tls {
		fs.StringVar(&o.TLSCert, "tls-cert", "", "TLS certificate file of the listener (reloaded when it changes)")
		fs.StringVar(&o.TLSKey, "tls-key", "", "TLS private key file of the listener")
	}
	if c.clientCA {
		fs.StringVar(&o.TLSClientCA, "tls-client-ca", "", "CA bundle requiring and verifying client certificates (mutual TLS)")
	}
	if c.token {
		fs.StringVar(&o.InternalTokenFile, "internal-token-file", "", "File holding the shared internal token (default $"+EnvInternalTokenFile+" or "+DefaultInternalTokenFile+")")
	}
	fs.DurationVar(&o.ShutdownTimeout, "shutdown-timeout", DefaultShutdownTimeout, "Bound of the graceful drain after SIGTERM")
}

// Runtime is the bootstrapped process context of a component command.
type Runtime struct {
	// Name is the command name (server, controller, ...).
	Name string
	// Log is the process logger (also installed as slog.Default and controller-runtime's).
	Log *slog.Logger
	// Params are the resolved patrol-cmd-params-cm parameters (nil for commands without a
	// settings component).
	Params *settings.Params
	// Namespace is the control-plane namespace ("" without Kubernetes access).
	Namespace string
	// Rest and Kube reach the control-plane cluster (nil without Kubernetes access).
	Rest *rest.Config
	Kube kubernetes.Interface
	// Registry receives the component metrics (Go and process collectors included).
	Registry *prometheus.Registry
	// Common are the common options.
	Common CommonOptions
	// TokenExplicit reports that --internal-token-file was given.
	TokenExplicit bool
}

// bootstrapInput describes how to bootstrap one command.
type bootstrapInput struct {
	name      string
	component settings.Component // "" when the command has no settings component
	pf        *ParamFlags
	common    CommonOptions
	kube      bool
	// tokenExplicit reports --internal-token-file on the command line.
	tokenExplicit bool
	stderr        io.Writer
	// logOverride, when set, replaces the parameter-based log options.
	logOverride *LogOptions
	// lookupEnv defaults to os.LookupEnv.
	lookupEnv func(string) (string, bool)
}

// bootstrap resolves the parameters (flags and environment first, so the startup logger is
// configured), connects to the control-plane cluster, reads patrol-cmd-params-cm and
// resolves the parameters again with it.
func bootstrap(ctx context.Context, in bootstrapInput) (*Runtime, error) {
	if in.lookupEnv == nil {
		in.lookupEnv = os.LookupEnv
	}
	if in.stderr == nil {
		in.stderr = os.Stderr
	}
	rt := &Runtime{Name: in.name, Common: in.common, Registry: newRegistry(), TokenExplicit: in.tokenExplicit}
	if rt.Common.ShutdownTimeout <= 0 {
		rt.Common.ShutdownTimeout = DefaultShutdownTimeout
	}
	var issues []settings.Issue
	if in.pf != nil {
		p, is, err := in.pf.Resolve(nil, in.lookupEnv)
		if err != nil {
			return nil, err
		}
		rt.Params, issues = p, is
	}
	log, err := NewLogger(in.stderr, logOptionsFor(in, rt.Params))
	if err != nil {
		return nil, err
	}
	rt.Log = log.With("component", in.name)
	installLogger(rt.Log)

	if in.kube {
		cfg, ns, err := loadKubeConfig(in.common)
		if err != nil {
			return nil, err
		}
		rt.Rest, rt.Namespace = cfg, ns
		applyClientTuning(rt.Rest, rt.Params)
		if rt.Kube, err = kubernetes.NewForConfig(rt.Rest); err != nil {
			return nil, fmt.Errorf("create Kubernetes client: %w", err)
		}
		if in.pf != nil {
			cm, err := readCmdParams(ctx, rt.Kube, rt.Namespace, rt.Log)
			if err != nil {
				return nil, err
			}
			if cm != nil {
				p, is, err := in.pf.Resolve(cm, in.lookupEnv)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", settings.DefaultCmdParamsConfigMapName, err)
				}
				rt.Params, issues = p, is
				log, err := NewLogger(in.stderr, logOptionsFor(in, rt.Params))
				if err != nil {
					return nil, err
				}
				rt.Log = log.With("component", in.name)
				installLogger(rt.Log)
				applyClientTuning(rt.Rest, rt.Params)
				if rt.Kube, err = kubernetes.NewForConfig(rt.Rest); err != nil {
					return nil, fmt.Errorf("create Kubernetes client: %w", err)
				}
			}
		}
	}
	for _, is := range issues {
		rt.Log.Warn("patrol-cmd-params-cm: "+is.Message, "key", is.Key)
	}
	if in.pf != nil && rt.Params != nil {
		logParamSources(rt.Log, in.pf, rt.Params)
	}
	return rt, nil
}

// logOptionsFor returns the log options of a command: the override, or the component's
// <prefix>.log.level / <prefix>.log.format parameters (format default: json in a pod,
// text elsewhere) and log.format.timestamp.
func logOptionsFor(in bootstrapInput, p *settings.Params) LogOptions {
	if in.logOverride != nil {
		o := *in.logOverride
		if o.Format == "" {
			o.Format = defaultLogFormat()
		}
		if o.Level == "" {
			o.Level = "info"
		}
		return o
	}
	o := LogOptions{Level: "info", Format: defaultLogFormat()}
	if p == nil {
		return o
	}
	debug := false
	for _, v := range p.Values() {
		switch {
		case strings.HasSuffix(v.Spec.Key, ".log.level"):
			if s := p.String(v.Spec.Key); s != "" {
				o.Level = s
			}
		case strings.HasSuffix(v.Spec.Key, ".log.format"):
			if v.Source != settings.ParamFromDefault || in.pf.paramSource(p, v.Spec.Key) == "flag" {
				o.Format = p.String(v.Spec.Key)
			}
		case v.Spec.Key == "log.format.timestamp":
			o.TimestampLayout = p.String(v.Spec.Key)
		case v.Spec.Key == "applicationsetcontroller.debug":
			debug = p.Bool(v.Spec.Key)
		}
	}
	if debug {
		o.Level = "debug"
	}
	return o
}

// logParamSources logs, at debug level, which parameters were configured and where (never
// their values: otlp.headers may carry credentials).
func logParamSources(log *slog.Logger, pf *ParamFlags, p *settings.Params) {
	if !log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	for _, v := range p.Values() {
		if src := pf.paramSource(p, v.Spec.Key); src != "default" {
			log.Debug("parameter configured", "key", v.Spec.Key, "source", src)
		}
	}
}

// loadKubeConfig returns the control-plane REST configuration and namespace.
func loadKubeConfig(o CommonOptions) (*rest.Config, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.Kubeconfig != "" {
		rules.ExplicitPath = o.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: o.Context}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("load Kubernetes client configuration: %w", err)
	}
	ns := o.Namespace
	if ns == "" {
		if ns, _, err = cc.Namespace(); err != nil {
			return nil, "", fmt.Errorf("determine namespace: %w", err)
		}
	}
	if ns == "" {
		return nil, "", errors.New("no control-plane namespace: pass --namespace")
	}
	cfg = rest.CopyConfig(cfg)
	if cfg.UserAgent == "" {
		cfg.UserAgent = rest.DefaultKubernetesUserAgent()
	}
	return cfg, ns, nil
}

// applyClientTuning applies the <prefix>.k8s.client.qps / .burst parameters (OPS-021) to
// the control-plane client configuration.
func applyClientTuning(cfg *rest.Config, p *settings.Params) {
	if cfg == nil || p == nil {
		return
	}
	for _, v := range p.Values() {
		switch {
		case strings.HasSuffix(v.Spec.Key, ".k8s.client.qps"):
			cfg.QPS = float32(p.Float(v.Spec.Key))
		case strings.HasSuffix(v.Spec.Key, ".k8s.client.burst"):
			cfg.Burst = int(p.Int(v.Spec.Key))
		}
	}
}

// readCmdParams reads patrol-cmd-params-cm; a missing ConfigMap is nil, and a ConfigMap the
// component may not read is reported and ignored (the environment projection remains).
func readCmdParams(ctx context.Context, kube kubernetes.Interface, namespace string, log *slog.Logger) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, cmdParamsReadTimeout)
	defer cancel()
	cm, err := kube.CoreV1().ConfigMaps(namespace).Get(ctx, settings.DefaultCmdParamsConfigMapName, metav1.GetOptions{})
	switch {
	case err == nil:
		if cm.Data == nil {
			return map[string]string{}, nil
		}
		return cm.Data, nil
	case apierrors.IsNotFound(err):
		log.Debug("patrol-cmd-params-cm not found; using flags, environment and defaults", "namespace", namespace)
		return nil, nil
	case apierrors.IsForbidden(err):
		log.Warn("cannot read patrol-cmd-params-cm; using flags, environment and defaults", "namespace", namespace, "error", err)
		return nil, nil
	}
	return nil, fmt.Errorf("read %s/%s: %w", namespace, settings.DefaultCmdParamsConfigMapName, err)
}
