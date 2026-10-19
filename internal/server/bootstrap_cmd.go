package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// serviceAccountNamespaceFile is the namespace file of a pod's mounted service account token.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// envPodNamespace is the downward-API variable naming the pod's namespace.
const envPodNamespace = "POD_NAMESPACE"

// bootstrapOptions are the flags of `patrolcd bootstrap`.
type bootstrapOptions struct {
	kubeconfig, context, namespace string
	skipAdmin, skipInternalToken   bool
	skipSSHKnownHosts              bool
	timeout                        time.Duration
	logLevel, logFormat            string
}

// newBootstrapCommand returns `patrolcd bootstrap`, the first-run step of an installation
// (an init container of patrol-server, or of the controller in a core install).
func newBootstrapCommand() *cobra.Command {
	var o bootstrapOptions
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Create the first-run Secrets and the default SSH known hosts when absent",
		Long: "Creates, in the control-plane namespace, what a fresh installation needs, and keeps existing\n" +
			"material (one exception: a stale initial admin Secret, below):\n" +
			"  - " + InternalTokenSecretName + " (key " + InternalTokenSecretKey + "): the shared token of the internal APIs\n" +
			"  - patrol-secret key server.secretkey: the session signing key\n" +
			"  - patrol-secret keys admin.password and admin.passwordMtime, and " + InitialAdminSecretName + "\n" +
			"    (key " + InitialAdminSecretKey + "): the initial admin password, unless patrol-cm sets admin.enabled to false\n" +
			"    or patrol-secret already holds admin.password. An initial admin Secret older than patrol-secret\n" +
			"    (left over from an earlier installation) is replaced by a new one\n" +
			"  - patrol-ssh-known-hosts-cm key ssh_known_hosts: the published host keys of GitHub, Bitbucket Cloud,\n" +
			"    GitLab.com and Azure DevOps, when the ConfigMap exists without that key (a present key is kept)\n\n" +
			"Runs are idempotent (a converged namespace is not written to) and safe to run concurrently. The\n" +
			"install manifests run it as an init container; it can also be run by hand with a kubeconfig.\n" +
			"Nothing secret is printed: read the initial password with `patrol admin initial-password`.\n" +
			"Exit status 0 when converged, 1 on failure.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runBootstrapCommand(ctx, o, c.ErrOrStderr(), os.LookupEnv, serviceAccountNamespaceFile)
		},
	}
	fs := cmd.Flags()
	fs.SortFlags = true
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "Path to a kubeconfig file (default: in-cluster configuration, else $KUBECONFIG or ~/.kube/config)")
	fs.StringVar(&o.context, "context", "", "Kubeconfig context to use")
	fs.StringVarP(&o.namespace, "namespace", "n", "", "Control-plane namespace (default: $"+envPodNamespace+", else the pod's service account namespace)")
	fs.BoolVar(&o.skipAdmin, "skip-admin", false, "Do not create the initial admin password (admin.password, "+InitialAdminSecretName+")")
	fs.BoolVar(&o.skipInternalToken, "skip-internal-token", false, "Do not create Secret "+InternalTokenSecretName)
	fs.BoolVar(&o.skipSSHKnownHosts, "skip-ssh-known-hosts", false, "Do not seed the default known hosts into ConfigMap "+settings.DefaultSSHKnownHostsConfigMapName)
	fs.DurationVar(&o.timeout, "timeout", DefaultBootstrapTimeout, "Bound of the whole run, retries of transient failures included")
	fs.StringVar(&o.logLevel, "loglevel", "info", "Log level: debug|info|warn|error")
	fs.StringVar(&o.logFormat, "logformat", "", "Log format: json|text (default: json in a pod, text elsewhere)")
	return cmd
}

// runBootstrapCommand configures logging and the Kubernetes client, then runs RunBootstrap
// within the timeout. Logs go to stderr; nothing is written to stdout.
func runBootstrapCommand(ctx context.Context, o bootstrapOptions, stderr io.Writer, lookupEnv func(string) (string, bool), saNamespaceFile string) error {
	format := o.logFormat
	if format == "" {
		format = defaultLogFormat()
	}
	log, err := NewLogger(stderr, LogOptions{Level: o.logLevel, Format: format})
	if err != nil {
		return err
	}
	log = log.With("component", "bootstrap")
	installLogger(log)
	if o.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive, got %s", o.timeout)
	}
	ns, err := bootstrapNamespace(o.namespace, lookupEnv, saNamespaceFile)
	if err != nil {
		return err
	}
	rc, _, err := loadKubeConfig(CommonOptions{Kubeconfig: o.kubeconfig, Context: o.context, Namespace: ns})
	if err != nil {
		return err
	}
	kube, err := newBootstrapClient(rc)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	start := time.Now()
	log.Info("starting", "version", VersionInfo().Version, "namespace", ns)
	if err := RunBootstrap(ctx, BootstrapConfig{
		Namespace: ns, Kube: kube, SkipAdmin: o.skipAdmin, SkipInternalToken: o.skipInternalToken,
		SkipSSHKnownHosts: o.skipSSHKnownHosts, Logger: log,
	}); err != nil {
		return err
	}
	log.Info("converged", "namespace", ns, "duration", time.Since(start).Round(time.Millisecond))
	return nil
}

// newBootstrapClient returns the bootstrap's Kubernetes client, without client-side
// throttling: an attempt makes about a dozen requests and the retry backoff spaces attempts,
// so client-go's token bucket only adds failure modes. Near the deadline it refuses a request
// whose wait would pass the deadline with a plain error ("rate: Wait(n=1) would exceed
// context deadline"), which would replace the failure that kept the run retrying (e.g.
// Forbidden), and long waits print unstructured klog lines. The API server's priority and
// fairness still applies.
func newBootstrapClient(rc *rest.Config) (kubernetes.Interface, error) {
	rc = rest.CopyConfig(rc)
	rc.RateLimiter = flowcontrol.NewFakeAlwaysRateLimiter()
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	return kube, nil
}

// bootstrapNamespace returns the control-plane namespace: the flag, else $POD_NAMESPACE,
// else the service account namespace file. Unlike the component commands it never falls
// back to a kubeconfig context namespace: writing Secrets into "default" by accident is
// worse than asking for --namespace.
func bootstrapNamespace(flag string, lookupEnv func(string) (string, bool), saNamespaceFile string) (string, error) {
	ns := strings.TrimSpace(flag)
	if ns == "" && lookupEnv != nil {
		if v, ok := lookupEnv(envPodNamespace); ok {
			ns = strings.TrimSpace(v)
		}
	}
	if ns == "" && saNamespaceFile != "" {
		if b, err := os.ReadFile(saNamespaceFile); err == nil {
			ns = strings.TrimSpace(string(b))
		}
	}
	if ns == "" {
		return "", errors.New("no control-plane namespace: pass --namespace (or set " + envPodNamespace + ")")
	}
	if msgs := validation.IsDNS1123Label(ns); len(msgs) > 0 {
		return "", fmt.Errorf("invalid namespace %q: %s", ns, strings.Join(msgs, "; "))
	}
	return ns, nil
}
