package admin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
)

// placeholderBearerToken is generate-spec's default token (upstream "bearer-token").
const placeholderBearerToken = "bearer-token"

func newClusterKubeconfigCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	var outputFile string
	cmd := &cobra.Command{
		Use:   "kubeconfig CLUSTER_URL [OUTPUT_PATH]",
		Short: "Generates kubeconfig for the specified cluster",
		Long: `Generates a kubeconfig reproducing how Patrol CD connects to a registered cluster
(credentials Secret, TLS, exec/AWS authentication), for example to troubleshoot
connectivity with kubectl from the controller pod.

The kubeconfig is written with mode 0600 to OUTPUT_PATH (or --output-file). Without an
output file it is printed with its credentials redacted.`,
		Example: `
#Generate a kubeconfig for a cluster and write it to a file
patrol admin cluster kubeconfig https://cluster-api-url:6443 /path/to/output/kubeconfig.yaml

#Show the kubeconfig without credentials
patrol admin cluster kubeconfig https://cluster-api-url:6443`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			path := outputFile
			if len(args) == 2 {
				if path != "" && path != args[1] {
					return errors.New("give the output path either as OUTPUT_PATH or with --output-file")
				}
				path = args[1]
			}
			kc, err := kube.get(nil)
			if err != nil {
				return err
			}
			cluster, err := findClusterByServer(ctx, kc, args[0])
			if err != nil {
				return err
			}
			data, err := clusterSecretData(ctx, kc, cluster)
			if err != nil {
				return err
			}
			cfg, err := clusters.RawRestConfig(ctx, cluster, data, clusters.Options{
				AWSAuthExec: true, DisableNativeK8sAuth: true, FakeInCluster: clusters.FakeInClusterFromEnv(),
			})
			if err != nil {
				return err
			}
			kcfg := kubeconfigFromREST(cfg, cluster, kc.namespace)
			if path == "" {
				redactKubeconfig(kcfg)
			}
			out, err := clientcmd.Write(*kcfg)
			if err != nil {
				return fmt.Errorf("error serializing kubeconfig: %w", err)
			}
			if path != "" {
				return writeFileSecure(path, out)
			}
			if _, err := f.out().Write(out); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(f.errOut(), "note: credentials are redacted; give OUTPUT_PATH to write a usable kubeconfig (mode 0600)")
			return nil
		},
	}
	cmd.Flags().StringVar(&outputFile, "output-file", "", "Write the kubeconfig with credentials to this file (mode 0600)")
	return cmd
}

// kubeconfigFromREST converts a cluster's REST configuration into a kubeconfig whose
// context uses namespace.
func kubeconfigFromREST(cfg *rest.Config, c *v1alpha1.Cluster, namespace string) *clientcmdapi.Config {
	name := c.Spec.Name
	if name == "" {
		name = c.Spec.Server
	}
	cl := clientcmdapi.NewCluster()
	cl.Server = cfg.Host
	cl.CertificateAuthorityData = cfg.CAData
	cl.CertificateAuthority = cfg.CAFile
	cl.InsecureSkipTLSVerify = cfg.Insecure
	cl.TLSServerName = cfg.ServerName
	cl.ProxyURL = c.Spec.Config.ProxyURL
	cl.DisableCompression = cfg.DisableCompression
	ai := clientcmdapi.NewAuthInfo()
	ai.Token = cfg.BearerToken
	ai.TokenFile = cfg.BearerTokenFile
	ai.Username = cfg.Username
	ai.Password = cfg.Password
	ai.ClientCertificateData = cfg.CertData
	ai.ClientCertificate = cfg.CertFile
	ai.ClientKeyData = cfg.KeyData
	ai.ClientKey = cfg.KeyFile
	if cfg.ExecProvider != nil {
		ai.Exec = cfg.ExecProvider.DeepCopy()
	}
	ctx := clientcmdapi.NewContext()
	ctx.Cluster, ctx.AuthInfo, ctx.Namespace = name, name, namespace
	out := clientcmdapi.NewConfig()
	out.Clusters[name] = cl
	out.AuthInfos[name] = ai
	out.Contexts[name] = ctx
	out.CurrentContext = name
	return out
}

// redactKubeconfig replaces credentials (tokens, passwords, private keys, exec
// environment values) with a placeholder.
func redactKubeconfig(cfg *clientcmdapi.Config) {
	for _, ai := range cfg.AuthInfos {
		if ai.Token != "" {
			ai.Token = redactedValue
		}
		if ai.Password != "" {
			ai.Password = redactedValue
		}
		if len(ai.ClientKeyData) > 0 {
			ai.ClientKeyData = []byte(redactedValue)
		}
		if ai.Exec != nil {
			for i := range ai.Exec.Env {
				ai.Exec.Env[i].Value = redactedValue
			}
		}
	}
}

// clusterSpecOptions are the flags of "cluster generate-spec" (upstream ClusterOptions).
type clusterSpecOptions struct {
	name               string
	inCluster          bool
	clusterEndpoint    string
	namespaces         []string
	clusterResources   bool
	project            string
	shard              int64
	awsClusterName     string
	awsRoleARN         string
	awsProfile         string
	execCommand        string
	execArgs           []string
	execEnv            map[string]string
	execAPIVersion     string
	execInstallHint    string
	disableCompression bool
	bearerToken        string
	generateToken      bool
	serviceAccount     string
	systemNamespace    string
	labels             []string
	annotations        []string
	output             string
	outputFile         string
}

func newClusterGenerateSpecCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	o := &clusterSpecOptions{}
	cmd := &cobra.Command{
		Use:   "generate-spec CONTEXT",
		Short: "Generate declarative config for a cluster",
		Long: `Generate the declarative configuration of a cluster registration from a kubeconfig context:
a Cluster object and the credentials Secret it references. Nothing is sent to the cluster
unless --generate-bearer-token is given.

Printed credentials (bearer token, client key) are redacted; write the complete manifests
with --output-file (mode 0600).`,
		Example: `
#Generate declarative config for the cluster of the "prod" kubeconfig context
patrol admin cluster generate-spec prod --generate-bearer-token --output-file prod-cluster.yaml`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runClusterGenerateSpec(c.Context(), f, kube, o, args)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&o.bearerToken, "bearer-token", "", "Authentication token that should be used to access K8S API server")
	fl.BoolVar(&o.generateToken, "generate-bearer-token", false, "Generate authentication token that should be used to access K8S API server")
	fl.StringVar(&o.serviceAccount, "service-account", clusters.ManagerServiceAccount, fmt.Sprintf("System namespace service account to use for kubernetes resource management. If not set then default %q SA will be used", clusters.ManagerServiceAccount))
	fl.StringVar(&o.systemNamespace, "system-namespace", "kube-system", "Use different system namespace")
	fl.StringVarP(&o.output, "output", "o", formatYAML, "Output format. One of: json|yaml")
	fl.StringVar(&o.outputFile, "output-file", "", "Write the manifests, including credentials, to this file (mode 0600)")
	fl.StringArrayVar(&o.labels, "label", nil, "Set metadata labels (e.g. --label key=value)")
	fl.StringArrayVar(&o.annotations, "annotation", nil, "Set metadata annotations (e.g. --annotation key=value)")
	fl.StringVar(&o.name, "name", "", "Overwrite the cluster name")
	fl.BoolVar(&o.inCluster, "in-cluster", false, "Indicates Patrol CD resides inside this cluster and should connect using the internal k8s hostname (kubernetes.default.svc)")
	fl.StringVar(&o.clusterEndpoint, "cluster-endpoint", "", "Cluster endpoint to use. Can be one of the following: 'kubeconfig', 'kube-public', or 'internal'.")
	fl.StringArrayVar(&o.namespaces, "namespace", nil, "List of namespaces which are allowed to manage")
	fl.BoolVar(&o.clusterResources, "cluster-resources", false, "Indicates if cluster level resources should be managed. The setting is used only if list of managed namespaces is not empty.")
	fl.StringVar(&o.project, "project", "", "project of the cluster")
	fl.Int64Var(&o.shard, "shard", -1, "Cluster shard number")
	fl.StringVar(&o.awsClusterName, "aws-cluster-name", "", "AWS Cluster name if set then aws cli eks token command will be used to access cluster")
	fl.StringVar(&o.awsRoleARN, "aws-role-arn", "", "Optional AWS role arn. If set then AWS IAM Authenticator assumes a role to perform cluster operations instead of the default AWS credential provider chain.")
	fl.StringVar(&o.awsProfile, "aws-profile", "", "Optional AWS profile. If set then AWS IAM Authenticator uses this profile to perform cluster operations instead of the default AWS credential provider chain.")
	fl.StringVar(&o.execCommand, "exec-command", "", "Command to run to provide client credentials to the cluster. You may need to build a custom Patrol CD image to ensure the command is available at runtime.")
	fl.StringArrayVar(&o.execArgs, "exec-command-args", nil, "Arguments to supply to the --exec-command executable")
	fl.StringToStringVar(&o.execEnv, "exec-command-env", nil, "Environment vars to set when running the --exec-command executable")
	fl.StringVar(&o.execAPIVersion, "exec-command-api-version", "", "Preferred input version of the ExecInfo for the --exec-command executable")
	fl.StringVar(&o.execInstallHint, "exec-command-install-hint", "", "Text shown to the user when the --exec-command executable doesn't seem to be present")
	fl.BoolVar(&o.disableCompression, "disable-compression", false, "Bypasses automatic GZip compression requests to the server")
	return cmd
}

// parseKeyValues parses key=value entries (labels, annotations).
func parseKeyValues(entries []string, what string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, e := range entries {
		k, v, ok := strings.Cut(e, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid %s %q: expected key=value", what, e)
		}
		out[k] = v
	}
	return out, nil
}

func runClusterGenerateSpec(ctx context.Context, f *Factory, kube *kubeOptions, o *clusterSpecOptions, args []string) error {
	raw, err := kube.loadingRules.Load()
	if err != nil {
		return fmt.Errorf("error loading kubeconfig: %w", err)
	}
	if len(args) == 0 {
		names := slices.Sorted(maps.Keys(raw.Contexts))
		return exitWith(1, fmt.Errorf("Choose a context name from:\n%s", strings.Join(names, "\n"))) //nolint:staticcheck // upstream wording
	}
	contextName := args[0]
	if raw.Contexts[contextName] == nil {
		return fmt.Errorf("Context %s does not exist in kubeconfig", contextName) //nolint:staticcheck // upstream wording
	}
	if o.inCluster && o.clusterEndpoint != "" {
		return errors.New("Can only use one of --in-cluster or --cluster-endpoint") //nolint:staticcheck // upstream wording
	}
	switch o.clusterEndpoint {
	case "", "kubeconfig", "internal":
	case "kube-public":
		f.logger().Warn("kube-public cluster endpoints are not supported. Falling back to the endpoint listed in the kubeconfig context.")
	default:
		return fmt.Errorf("invalid --cluster-endpoint %q: one of kubeconfig, kube-public, internal", o.clusterEndpoint)
	}
	conf, err := clientcmd.NewDefaultClientConfig(*raw, &clientcmd.ConfigOverrides{CurrentContext: contextName}).ClientConfig()
	if err != nil {
		return err
	}
	labels, err := parseKeyValues(o.labels, "label")
	if err != nil {
		return err
	}
	annotations, err := parseKeyValues(o.annotations, "annotation")
	if err != nil {
		return err
	}
	name := contextName
	if o.name != "" {
		name = o.name
	}
	server := conf.Host
	if o.inCluster || o.clusterEndpoint == "internal" {
		server = v1alpha1.KubernetesInternalAPIServerAddr
	}
	cluster := &v1alpha1.Cluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Cluster"},
		ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
		Spec: v1alpha1.ClusterSpec{
			Server: server, Name: name, Namespaces: o.namespaces, Project: o.project,
			ClusterResources: o.clusterResources && len(o.namespaces) > 0,
			Config: v1alpha1.ClusterConfig{
				TLSClientConfig:    v1alpha1.TLSClientConfig{Insecure: conf.Insecure, ServerName: conf.ServerName},
				DisableCompression: o.disableCompression,
			},
		},
	}
	if o.shard >= 0 {
		cluster.Spec.Shard = &o.shard
	}
	if cluster.Name, err = clusters.ObjectNameForServer(server); err != nil {
		return err
	}
	data := map[string][]byte{}
	bearerToken := o.bearerToken
	switch {
	case o.awsClusterName != "":
		cluster.Spec.Config.AWSAuthConfig = &v1alpha1.AWSAuthConfig{ClusterName: o.awsClusterName, RoleARN: o.awsRoleARN, Profile: o.awsProfile}
		bearerToken = ""
	case o.execCommand != "":
		cluster.Spec.Config.ExecProviderConfig = &v1alpha1.ExecProviderConfig{
			Command: o.execCommand, Args: o.execArgs, Env: o.execEnv, APIVersion: o.execAPIVersion, InstallHint: o.execInstallHint,
		}
		bearerToken = ""
	case o.generateToken:
		target, err := kubernetes.NewForConfig(conf)
		if err != nil {
			return err
		}
		if bearerToken, err = clusters.ServiceAccountBearerToken(ctx, target, o.systemNamespace, o.serviceAccount, &clusters.BootstrapOptions{ServiceAccount: o.serviceAccount}); err != nil {
			return err
		}
	case bearerToken == "":
		bearerToken = placeholderBearerToken
	}
	if bearerToken != "" {
		data[v1alpha1.ClusterSecretKeyBearerToken] = []byte(bearerToken)
	}
	for key, pair := range map[string][2]string{
		v1alpha1.ClusterSecretKeyCAData:   {string(conf.CAData), conf.CAFile},
		v1alpha1.ClusterSecretKeyCertData: {string(conf.CertData), conf.CertFile},
		v1alpha1.ClusterSecretKeyKeyData:  {string(conf.KeyData), conf.KeyFile},
	} {
		value := []byte(pair[0])
		if len(value) == 0 && pair[1] != "" {
			if value, err = readFileLimited(pair[1]); err != nil {
				return err
			}
		}
		if len(value) > 0 {
			data[key] = value
		}
	}
	objs := []any{cluster}
	var secret *corev1.Secret
	if len(data) > 0 {
		cluster.Spec.Config.CredentialsRef = &v1alpha1.LocalSecretReference{Name: cluster.Name}
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Labels: map[string]string{v1alpha1.LabelKeySecretType: v1alpha1.LabelValueSecretTypeCluster}},
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}
	}
	return printSpecWithSecret(f, o.output, o.outputFile, objs, secret, func(k, v string) bool {
		return k == v1alpha1.ClusterSecretKeyKeyData || k == v1alpha1.ClusterSecretKeyPassword || (k == v1alpha1.ClusterSecretKeyBearerToken && v != placeholderBearerToken)
	})
}

// printSpecWithSecret prints generated objects followed by their credentials Secret. On
// stdout the values for which sensitive returns true are redacted; outputFile receives the
// complete manifests (mode 0600).
func printSpecWithSecret(f *Factory, format, outputFile string, objs []any, secret *corev1.Secret, sensitive func(key, value string) bool) error {
	full := append([]any(nil), objs...)
	shown := append([]any(nil), objs...)
	redacted := false
	if secret != nil {
		full = append(full, secretForOutput(secret, false))
		s := secretForOutput(secret, false)
		for k, v := range s.StringData {
			if sensitive(k, v) {
				s.StringData[k] = redactedValue
				redacted = true
			}
		}
		shown = append(shown, s)
	}
	if outputFile != "" {
		var buf bytes.Buffer
		if err := printResources(&buf, format, full...); err != nil {
			return err
		}
		if err := writeFileSecure(outputFile, buf.Bytes()); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(f.errOut(), "Wrote %s\n", outputFile)
		return nil
	}
	if err := printResources(f.out(), format, shown...); err != nil {
		return err
	}
	if redacted {
		_, _ = fmt.Fprintln(f.errOut(), "note: credentials are redacted; use --output-file to write complete manifests (mode 0600)")
	}
	return nil
}
