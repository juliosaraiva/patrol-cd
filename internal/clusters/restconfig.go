package clusters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Options configures RestConfig and RawRestConfig. The zero value selects upstream defaults:
// default tuning, the pod's in-cluster configuration and in-process cloud authentication.
type Options struct {
	// Tuning holds QPS, burst and transport settings. Nil means DefaultTuning(); use
	// TuningFromEnv to honor the PATROL_K8S* variables.
	Tuning *Tuning
	// UserAgent is set on the returned config when not empty.
	UserAgent string

	// InClusterConfig returns the pod's own configuration for the in-cluster server.
	// Nil means rest.InClusterConfig.
	InClusterConfig func() (*rest.Config, error)
	// FakeInCluster serves the in-cluster server from the local kubeconfig (KUBECONFIG or
	// ~/.kube/config) instead of the pod configuration. For development only; see
	// EnvFakeInCluster and FakeInClusterFromEnv.
	FakeInCluster bool

	// AWSAuthExec makes awsAuthConfig produce the upstream exec form
	// (`<K8sAuthCommand> k8s-auth aws --cluster-name ...`) instead of in-process tokens. The
	// exec form is serializable to a kubeconfig; it needs the binary in PATH.
	AWSAuthExec bool
	// K8sAuthCommand is the command of generated exec configurations. Default "patrolcd".
	K8sAuthCommand string
	// DisableNativeK8sAuth runs exec configurations that invoke `patrolcd k8s-auth` or
	// `argocd-k8s-auth` as subprocesses instead of producing their tokens in-process.
	DisableNativeK8sAuth bool
	// AWS holds settings for in-process EKS tokens that are not part of the Cluster API.
	AWS AWSTokenRequest
	// GCP and Azure hold settings for in-process GKE and AKS tokens. Their Lookup fields
	// are ignored: variables come from execProviderConfig.env, then from the process.
	GCP   GCPOptions
	Azure AzureOptions
	// Providers caches token sources across calls. Nil uses a process-wide cache. Entries are
	// keyed by the Cluster settings plus AWS.Region and AWS.STSEndpoint; callers that vary
	// AWS.LoadOptions or AWS.HTTPClient, or the GCP or Azure transports, must use separate
	// caches.
	Providers *ProviderCache
	// Now is the clock of cached token sources. Nil means time.Now.
	Now func() time.Time
}

// FakeInClusterFromEnv reports whether PATROL_FAKE_IN_CLUSTER is "true" (strconv.ParseBool).
func FakeInClusterFromEnv() bool {
	v, err := strconv.ParseBool(os.Getenv(EnvFakeInCluster))
	return err == nil && v
}

func (o *Options) tuning() Tuning {
	if o.Tuning != nil {
		return *o.Tuning
	}
	return DefaultTuning()
}

// RestConfig returns the configuration used to talk to cluster's API server: RawRestConfig
// followed by ApplyTuning (upstream Cluster.RESTConfig). secretData is the data of the Secret
// named by spec.config.credentialsRef (nil when there is none).
func RestConfig(ctx context.Context, cluster *v1alpha1.Cluster, secretData map[string][]byte, opts Options) (*rest.Config, error) {
	cfg, err := RawRestConfig(ctx, cluster, secretData, opts)
	if err != nil {
		return nil, fmt.Errorf("unable to get K8s RAW REST config: %w", err)
	}
	if err := ApplyTuning(cfg, opts.tuning()); err != nil {
		return nil, fmt.Errorf("unable to apply K8s REST config defaults: %w", err)
	}
	return cfg, nil
}

// RawRestConfig returns the cluster configuration without the tuned transport, as upstream
// Cluster.RawRestConfig. Authentication is chosen in this order:
//
//  1. in-cluster server with FakeInCluster: the local kubeconfig;
//  2. in-cluster server without username, password or bearer token: the pod's configuration;
//  3. in-cluster server with any of them: the pod's configuration with those credentials
//     replacing the service-account token (the token file is cleared);
//  4. otherwise awsAuthConfig, then execProviderConfig, then bearer token / basic auth, each
//     with the TLS settings (insecure, serverName, CA, client certificate and key).
//
// In case 4, AWS and recognized k8s-auth exec configurations get in-process tokens unless the
// Secret holds both a client certificate and key: client-go then ignores exec plugins, so the
// exec form is returned and the cluster is authenticated with mutual TLS only, as upstream.
//
// TLS, AWS and exec settings are ignored for the in-cluster server. Then proxyURL,
// disableCompression, the server-side timeout, QPS and burst are applied. The inputs, including
// the configuration returned by Options.InClusterConfig, are never modified.
func RawRestConfig(ctx context.Context, cluster *v1alpha1.Cluster, secretData map[string][]byte, opts Options) (*rest.Config, error) {
	if cluster == nil {
		return nil, fmt.Errorf("%w: nil cluster", ErrInvalidConfig)
	}
	creds := CredentialsFromSecretData(secretData)
	spec := &cluster.Spec
	server := NormalizeServer(spec.Server)

	var (
		cfg *rest.Config
		err error
	)
	switch {
	case server == InClusterServer && opts.FakeInCluster:
		cfg, err = localKubeconfig()
	case server == InClusterServer:
		inCluster := opts.InClusterConfig
		if inCluster == nil {
			inCluster = rest.InClusterConfig
		}
		cfg, err = inCluster()
		switch {
		case err != nil:
		case cfg == nil:
			err = fmt.Errorf("%w: in-cluster configuration is nil", ErrInvalidConfig)
		default:
			// The hook may hand out a shared configuration (the manager's own, say); everything
			// below and ApplyTuning modify cfg, so work on a copy.
			cfg = rest.CopyConfig(cfg)
			if creds.HasBasicOrToken() {
				cfg.Username = creds.Username
				cfg.Password = creds.Password
				cfg.BearerToken = creds.BearerToken
				cfg.BearerTokenFile = ""
			}
		}
	default:
		cfg, err = remoteConfig(ctx, server, spec.Config, creds, &opts)
	}
	if err != nil {
		return nil, fmt.Errorf("unable to create K8s REST config: %w", err)
	}
	if spec.Config.ProxyURL != "" {
		u, err := ParseProxyURL(spec.Config.ProxyURL)
		if err != nil {
			// Upstream wording, including its backquote.
			return nil, fmt.Errorf("unable to create K8s REST config, can`t parse proxy url: %w", err)
		}
		cfg.Proxy = http.ProxyURL(u)
	}
	cfg.DisableCompression = spec.Config.DisableCompression
	opts.tuning().applyLimits(cfg)
	if opts.UserAgent != "" {
		cfg.UserAgent = opts.UserAgent
	}
	return cfg, nil
}

// ParseProxyURL parses a cluster proxy URL. Only http, https and socks5 are accepted (socks5h
// is not, unlike repository proxies).
func ParseProxyURL(proxyURL string) (*url.URL, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http", "https", "socks5":
		return u, nil
	default:
		return nil, fmt.Errorf("failed to parse proxy url, unsupported scheme %q, must be http, https, or socks5", u.Scheme)
	}
}

func localKubeconfig() (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if os.Getenv(clientcmd.RecommendedConfigPathEnvVar) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = ""
		}
		rules.ExplicitPath = filepath.Join(home, clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName)
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load local kubeconfig: %w", err)
	}
	return cfg, nil
}

func remoteConfig(ctx context.Context, server string, cc v1alpha1.ClusterConfig, creds Credentials, opts *Options) (*rest.Config, error) {
	tlsConfig := rest.TLSClientConfig{
		Insecure:   cc.TLSClientConfig.Insecure,
		ServerName: cc.TLSClientConfig.ServerName,
		CAData:     creds.CAData,
		CertData:   creds.CertData,
		KeyData:    creds.KeyData,
	}
	cfg := &rest.Config{Host: server, TLSClientConfig: tlsConfig}
	// client-go never runs an exec plugin when a client certificate and key are configured
	// (exec.Authenticator.UpdateTransportConfig), so upstream authenticates such clusters with
	// mutual TLS alone. In-process cloud tokens would add a bearer token client-go never sends
	// (and fail when no cloud credentials exist), so they are only used without cert auth and
	// the inert exec form is kept otherwise.
	native := !creds.HasCertAuth()
	switch {
	case cc.AWSAuthConfig != nil:
		aws := *cc.AWSAuthConfig
		if opts.AWSAuthExec || !native {
			cfg.ExecProvider = &clientcmdapi.ExecConfig{
				APIVersion:      ExecCredentialAPIVersion,
				Command:         opts.k8sAuthCommand(),
				Args:            append([]string{K8sAuthSubcommand}, AWSExecArgs(aws)...),
				InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
			}
			return cfg, nil
		}
		src, err := opts.awsSource(aws)
		if err != nil {
			return nil, err
		}
		cfg.WrapTransport = wrapWithTokenSource(src)
	case cc.ExecProviderConfig != nil:
		exec := cc.ExecProviderConfig
		if native && !opts.DisableNativeK8sAuth {
			src, ok, err := opts.nativeExecSource(ctx, exec)
			if err != nil {
				return nil, err
			}
			if ok {
				cfg.WrapTransport = wrapWithTokenSource(src)
				return cfg, nil
			}
		}
		cfg.ExecProvider = ExecConfig(exec)
	default:
		cfg.Username = creds.Username
		cfg.Password = creds.Password
		cfg.BearerToken = creds.BearerToken
	}
	return cfg, nil
}

// ExecConfig converts execProviderConfig to a client-go exec configuration. Environment
// variables are sorted by name (upstream iterates a map, so its order is random). The plugin
// never runs interactively, and config is passed as ExecCredential.spec.cluster.config when
// provideClusterInfo is set.
func ExecConfig(e *v1alpha1.ExecProviderConfig) *clientcmdapi.ExecConfig {
	out := &clientcmdapi.ExecConfig{
		APIVersion:         e.APIVersion,
		Command:            e.Command,
		Args:               slices.Clone(e.Args),
		InstallHint:        e.InstallHint,
		ProvideClusterInfo: e.ProvideClusterInfo,
		InteractiveMode:    clientcmdapi.NeverExecInteractiveMode,
	}
	for _, k := range slices.Sorted(maps.Keys(e.Env)) {
		out.Env = append(out.Env, clientcmdapi.ExecEnvVar{Name: k, Value: e.Env[k]})
	}
	if e.Config != nil && len(e.Config.Raw) > 0 {
		out.Config = &runtime.Unknown{Raw: slices.Clone(e.Config.Raw), ContentType: runtime.ContentTypeJSON}
	}
	return out
}

// AWSExecArgs returns the k8s-auth aws arguments for an awsAuthConfig, as upstream builds them:
// --cluster-name is always present (possibly empty); --role-arn and --profile only when set.
func AWSExecArgs(a v1alpha1.AWSAuthConfig) []string {
	args := []string{"aws", "--cluster-name", a.ClusterName}
	if a.RoleARN != "" {
		args = append(args, "--role-arn", a.RoleARN)
	}
	if a.Profile != "" {
		args = append(args, "--profile", a.Profile)
	}
	return args
}

func (o *Options) k8sAuthCommand() string {
	if o.K8sAuthCommand != "" {
		return o.K8sAuthCommand
	}
	return DefaultK8sAuthCommand
}

func (o *Options) providers() *ProviderCache {
	if o.Providers != nil {
		return o.Providers
	}
	return sharedProviderCache()
}

// providerKey hashes the provider identity so cached token sources are shared only between
// identical settings. Secret values in the input never leave this function.
func providerKey(parts ...any) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	for _, p := range parts {
		_ = enc.Encode(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (o *Options) awsSource(a v1alpha1.AWSAuthConfig) (*cachingTokenSource, error) {
	req := o.AWS
	req.ClusterName, req.RoleARN, req.Profile = a.ClusterName, a.RoleARN, a.Profile
	key := providerKey("aws", a, req.Region, req.STSEndpoint)
	return o.providers().get(key, o.Now, func() (TokenSource, error) { return awsTokenSource(req), nil })
}

// nativeExecSource recognizes exec configurations that run the k8s-auth helper
// (`patrolcd k8s-auth <provider> ...` or upstream's `argocd-k8s-auth <provider> ...`) and
// returns an in-process token source for them. ok is false for any other command, or when the
// arguments are not understood, in which case the plugin is executed.
func (o *Options) nativeExecSource(_ context.Context, e *v1alpha1.ExecProviderConfig) (src *cachingTokenSource, ok bool, err error) {
	provider, args, ok := splitK8sAuthCommand(e.Command, e.Args)
	if !ok {
		return nil, false, nil
	}
	env := maps.Clone(e.Env)
	lookup := func(k string) (string, bool) {
		if v, found := env[k]; found {
			return v, true
		}
		return os.LookupEnv(k)
	}
	key := providerKey("exec", provider, args, env)
	switch provider {
	case "aws":
		a, ok := parseAWSArgs(args)
		if !ok {
			return nil, false, nil
		}
		req := o.AWS
		req.ClusterName, req.RoleARN, req.Profile = a.ClusterName, a.RoleARN, a.Profile
		if len(env) > 0 {
			// AWS configuration loading reads the process environment; per-cluster
			// variables need the subprocess.
			return nil, false, nil
		}
		src, err = o.providers().get(key, o.Now, func() (TokenSource, error) { return awsTokenSource(req), nil })
	case "gcp":
		if len(args) != 0 {
			return nil, false, nil
		}
		g := o.GCP
		g.Lookup = lookup
		src, err = o.providers().get(key, o.Now, func() (TokenSource, error) { return GCPTokenSource(g), nil })
	case "azure":
		if len(args) != 0 {
			return nil, false, nil
		}
		az := o.Azure
		az.Lookup = lookup
		src, err = o.providers().get(key, o.Now, func() (TokenSource, error) { return AzureTokenSource(az) })
	default:
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return src, true, nil
}

// splitK8sAuthCommand returns the provider and its arguments when command/args invoke the
// k8s-auth helper.
func splitK8sAuthCommand(command string, args []string) (string, []string, bool) {
	switch filepath.Base(command) {
	case upstreamK8sAuthCommand:
		if len(args) == 0 {
			return "", nil, false
		}
		return args[0], args[1:], true
	case DefaultK8sAuthCommand:
		if len(args) < 2 || args[0] != K8sAuthSubcommand {
			return "", nil, false
		}
		return args[1], args[2:], true
	default:
		return "", nil, false
	}
}

// parseAWSArgs parses the k8s-auth aws flags (--flag value or --flag=value).
func parseAWSArgs(args []string) (v1alpha1.AWSAuthConfig, bool) {
	var a v1alpha1.AWSAuthConfig
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(args[i], "=")
		if !hasValue {
			if i+1 >= len(args) {
				return a, false
			}
			i++
			value = args[i]
		}
		switch name {
		case "--cluster-name":
			a.ClusterName = value
		case "--role-arn":
			a.RoleARN = value
		case "--profile":
			a.Profile = value
		default:
			return a, false
		}
	}
	return a, true
}
