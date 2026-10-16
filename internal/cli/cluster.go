package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Cluster endpoints of `cluster add --cluster-endpoint`.
const (
	endpointKubeconfig = "kubeconfig"
	endpointKubePublic = "kube-public"
	endpointInternal   = "internal"
	allNamespaces      = "*"
)

// NewClusterCommand returns `patrol cluster` (API-072, REPO-106, REPO-108).
func NewClusterCommand(f *cmdutil.Factory) *cobra.Command {
	c := &cobra.Command{
		Use:   "cluster",
		Short: "Manage cluster credentials",
		RunE:  func(c *cobra.Command, args []string) error { return cmdutil.HelpExit(c, args) },
		Example: `  # List all known clusters in JSON format:
  patrol cluster list -o json

  # Add a target cluster configuration to Patrol CD. The context must exist in your kubectl config:
  patrol cluster add example-cluster

  # Get specific details about a cluster in plain text (wide) format:
  patrol cluster get example-cluster -o wide

  # Remove a target cluster context from Patrol CD
  patrol cluster rm example-cluster

  # Set a target cluster context from Patrol CD
  patrol cluster set CLUSTER_NAME --name new-cluster-name --namespace '*'
  patrol cluster set CLUSTER_NAME --name new-cluster-name --namespace namespace-one --namespace namespace-two`,
	}
	c.AddCommand(newClusterAddCommand(f), newClusterGetCommand(f), newClusterListCommand(f),
		newClusterRemoveCommand(f), newClusterRotateAuthCommand(f), newClusterSetCommand(f))
	return c
}

type clusterOptions struct {
	inCluster           bool
	upsert              bool
	serviceAccount      string
	awsRoleARN          string
	awsProfile          string
	awsClusterName      string
	systemNamespace     string
	namespaces          []string
	clusterResources    bool
	name                string
	project             string
	shard               int64
	execCommand         string
	execArgs            []string
	execEnv             map[string]string
	execAPIVersion      string
	execInstallHint     string
	clusterEndpoint     string
	disableCompression  bool
	proxyURL            string
	kubeconfig          string
	yes                 bool
	labels, annotations []string
}

func (o *clusterOptions) inClusterEndpoint() bool {
	return o.inCluster || o.clusterEndpoint == endpointInternal
}

// kubeconfigRules returns the loading rules of --kubeconfig, else KUBECONFIG, else
// ~/.kube/config.
func kubeconfigRules(f *cmdutil.Factory, explicit string) *clientcmd.ClientConfigLoadingRules {
	if explicit != "" {
		return &clientcmd.ClientConfigLoadingRules{ExplicitPath: explicit}
	}
	return &clientcmd.ClientConfigLoadingRules{Precedence: f.KubeconfigPaths("")}
}

func loadKubeconfig(f *cmdutil.Factory, explicit string) (*clientcmdapi.Config, error) {
	cfg, err := kubeconfigRules(f, explicit).Load()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return cfg, nil
}

// restConfigForContext returns the client configuration of a kubeconfig context.
func restConfigForContext(raw *clientcmdapi.Config, name string) (*rest.Config, error) {
	kctx := raw.Contexts[name]
	if kctx == nil {
		return nil, fmt.Errorf("context %s does not exist in kubeconfig", name)
	}
	return clientcmd.NewDefaultClientConfig(*raw, &clientcmd.ConfigOverrides{Context: *kctx}).ClientConfig()
}

func printKubeContexts(f *cmdutil.Factory, raw *clientcmdapi.Config) error {
	t := output.NewTable(f.Out())
	t.Row("CURRENT", "NAME", "CLUSTER", "SERVER")
	names := make([]string, 0, len(raw.Contexts))
	for n := range raw.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		kctx := raw.Contexts[n]
		if kctx == nil {
			continue
		}
		cl := raw.Clusters[kctx.Cluster]
		if cl == nil {
			continue
		}
		cur := " "
		if raw.CurrentContext == n {
			cur = "*"
		}
		t.Row(cur, n, kctx.Cluster, cl.Server)
	}
	return t.Flush()
}

func newClusterAddCommand(f *cmdutil.Factory) *cobra.Command {
	o := &clusterOptions{}
	c := &cobra.Command{
		Use:   "add CONTEXT",
		Short: CommandName + " cluster add CONTEXT",
		RunE: func(c *cobra.Command, args []string) error {
			raw, err := loadKubeconfig(f, o.kubeconfig)
			if err != nil {
				return err
			}
			if len(args) == 0 {
				f.Logger().Error("Choose a context name from:")
				if err := printKubeContexts(f, raw); err != nil {
					return err
				}
				return cmdutil.Silent(cmdutil.ExitFatal)
			}
			if o.inCluster && o.clusterEndpoint != "" {
				return cmdutil.Fatalf("Can only use one of --in-cluster or --cluster-endpoint")
			}
			switch o.clusterEndpoint {
			case "", endpointKubeconfig, endpointKubePublic, endpointInternal:
			default:
				return fmt.Errorf("unknown cluster endpoint %q: one of 'kubeconfig', 'kube-public' or 'internal'", o.clusterEndpoint)
			}
			contextName := args[0]
			conf, err := restConfigForContext(raw, contextName)
			if err != nil {
				return err
			}
			if o.proxyURL != "" {
				u, err := clusters.ParseProxyURL(o.proxyURL)
				if err != nil {
					return err
				}
				conf.Proxy = http.ProxyURL(u)
			}
			kube, err := kubernetes.NewForConfig(conf)
			if err != nil {
				return fmt.Errorf("create Kubernetes client: %w", err)
			}
			labels, err := parseLabels(o.labels)
			if err != nil {
				return err
			}
			annotations, err := parseLabels(o.annotations)
			if err != nil {
				return err
			}
			in := &apiclient.ClusterInput{
				Name: contextName, Namespaces: o.namespaces, ClusterResources: o.clusterResources, Project: o.project,
				Labels: labels, Annotations: annotations,
			}
			if o.name != "" {
				in.Name = o.name
			}
			if o.shard >= 0 {
				s := o.shard
				in.Shard = &s
			}
			if err := clusterConnection(f, c.Context(), conf, kube, o, contextName, in); err != nil {
				return err
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			if _, err := client.CreateCluster(c.Context(), in, o.upsert); err != nil {
				return err
			}
			output.Printf(f.Out(), "Cluster '%s' added\n", in.Server)
			return nil
		},
	}
	fs := c.Flags()
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "use a particular kubeconfig file")
	fs.BoolVar(&o.upsert, "upsert", false, "Override an existing cluster with the same name even if the spec differs")
	fs.StringVar(&o.serviceAccount, "service-account", "", fmt.Sprintf("System namespace service account to use for kubernetes resource management. If not set then default %q SA will be created", clusters.ManagerServiceAccount))
	fs.StringVar(&o.systemNamespace, "system-namespace", clusters.DefaultSystemNamespace, "Use different system namespace")
	fs.BoolVarP(&o.yes, "yes", "y", false, "Skip explicit confirmation")
	fs.StringArrayVar(&o.labels, "label", nil, "Set metadata labels (e.g. --label key=value)")
	fs.StringArrayVar(&o.annotations, "annotation", nil, "Set metadata annotations (e.g. --annotation key=value)")
	fs.StringVar(&o.proxyURL, "proxy-url", "", "use proxy to connect cluster")
	fs.BoolVar(&o.inCluster, "in-cluster", false, "Indicates Patrol CD resides inside this cluster and should connect using the internal k8s hostname (kubernetes.default.svc)")
	fs.StringVar(&o.awsClusterName, "aws-cluster-name", "", "AWS Cluster name if set then aws cli eks token command will be used to access cluster")
	fs.StringVar(&o.awsRoleARN, "aws-role-arn", "", "Optional AWS role arn. If set then AWS IAM Authenticator assumes a role to perform cluster operations instead of the default AWS credential provider chain.")
	fs.StringVar(&o.awsProfile, "aws-profile", "", "Optional AWS profile. If set then AWS IAM Authenticator uses this profile to perform cluster operations instead of the default AWS credential provider chain.")
	fs.StringArrayVar(&o.namespaces, "namespace", nil, "List of namespaces which are allowed to manage")
	fs.BoolVar(&o.clusterResources, "cluster-resources", false, "Indicates if cluster level resources should be managed. The setting is used only if list of managed namespaces is not empty.")
	fs.StringVar(&o.name, "name", "", "Overwrite the cluster name")
	fs.StringVar(&o.project, "project", "", "project of the cluster")
	fs.Int64Var(&o.shard, "shard", -1, "Cluster shard number; inferred from hostname if not set")
	fs.StringVar(&o.execCommand, "exec-command", "", "Command to run to provide client credentials to the cluster. You may need to build a custom Patrol CD image to ensure the command is available at runtime.")
	fs.StringArrayVar(&o.execArgs, "exec-command-args", nil, "Arguments to supply to the --exec-command executable")
	fs.StringToStringVar(&o.execEnv, "exec-command-env", nil, "Environment vars to set when running the --exec-command executable")
	fs.StringVar(&o.execAPIVersion, "exec-command-api-version", "", "Preferred input version of the ExecInfo for the --exec-command executable")
	fs.StringVar(&o.execInstallHint, "exec-command-install-hint", "", "Text shown to the user when the --exec-command executable doesn't seem to be present")
	fs.StringVar(&o.clusterEndpoint, "cluster-endpoint", "", "Cluster endpoint to use. Can be one of the following: 'kubeconfig', 'kube-public', or 'internal'.")
	fs.BoolVar(&o.disableCompression, "disable-compression", false, "Bypasses automatic GZip compression requests to the server")
	return c
}

// clusterConnection fills the server, TLS and authentication of in: AWS or exec
// configurations as given, otherwise the bearer token of --service-account or of the
// patrol-manager service account it installs (after confirmation on a terminal). The
// kubeconfig's own client certificate and key are never sent to Patrol CD.
func clusterConnection(f *cmdutil.Factory, ctx context.Context, conf *rest.Config, kube kubernetes.Interface, o *clusterOptions, contextName string, in *apiclient.ClusterInput) error {
	in.Server = conf.Host
	tls := apiclient.ClusterTLSClientConfigInput{Insecure: conf.Insecure, ServerName: conf.ServerName, CAData: conf.CAData}
	if len(tls.CAData) == 0 && conf.CAFile != "" {
		b, err := readInputFile(f, conf.CAFile)
		if err != nil {
			return fmt.Errorf("read the CA file of context %s: %w", contextName, err)
		}
		tls.CAData = b
	}
	in.Config = apiclient.ClusterConfigInput{TLSClientConfig: tls, DisableCompression: o.disableCompression || conf.DisableCompression, ProxyURL: o.proxyURL}
	switch {
	case o.awsClusterName != "":
		in.Config.AWSAuthConfig = &v1alpha1.AWSAuthConfig{ClusterName: o.awsClusterName, RoleARN: o.awsRoleARN, Profile: o.awsProfile}
	case o.execCommand != "":
		in.Config.ExecProviderConfig = &v1alpha1.ExecProviderConfig{Command: o.execCommand, Args: o.execArgs, Env: o.execEnv, APIVersion: o.execAPIVersion, InstallHint: o.execInstallHint}
	case o.serviceAccount != "":
		tok, err := clusters.ServiceAccountBearerToken(ctx, kube, o.systemNamespace, o.serviceAccount, &clusters.BootstrapOptions{Logger: f.Logger()})
		if err != nil {
			return err
		}
		in.Config.BearerToken = tok
	default:
		if isTerminal(f.IO.Out) && !o.yes {
			level := "cluster"
			if len(o.namespaces) > 0 {
				level = "namespace"
			}
			ok, err := f.Prompter().Confirm(fmt.Sprintf("WARNING: This will create a service account `%s` on the cluster referenced by context `%s` with full %s level privileges. Do you want to continue [y/N]? ", clusters.ManagerServiceAccount, contextName, level))
			if err != nil || !ok {
				return cmdutil.Silent(cmdutil.ExitFatal)
			}
		}
		tok, err := clusters.EnsureManagerServiceAccount(ctx, kube, o.systemNamespace, len(o.namespaces) == 0, o.namespaces, &clusters.BootstrapOptions{Logger: f.Logger()})
		if err != nil {
			return err
		}
		in.Config.BearerToken = tok
	}
	switch {
	case o.inClusterEndpoint():
		in.Server = v1alpha1.KubernetesInternalAPIServerAddr
	case o.clusterEndpoint == endpointKubePublic:
		endpoint, ca, err := kubePublicEndpoint(ctx, kube)
		if err != nil || endpoint == "" {
			f.Logger().Warn(fmt.Sprintf("Failed to find the cluster endpoint from kube-public data: %v", err))
			f.Logger().Info(fmt.Sprintf("Falling back to the endpoint '%s' as listed in the kubeconfig context", in.Server))
			return nil
		}
		in.Server = endpoint
		in.Config.TLSClientConfig.CAData = ca
	}
	return nil
}

func isTerminal(w any) bool {
	fh, ok := w.(*os.File)
	return ok && term.IsTerminal(int(fh.Fd()))
}

// kubePublicEndpoint reads the API server endpoint and CA from kube-public/cluster-info.
func kubePublicEndpoint(ctx context.Context, kube kubernetes.Interface) (string, []byte, error) {
	cm, err := kube.CoreV1().ConfigMaps("kube-public").Get(ctx, "cluster-info", metav1.GetOptions{})
	if err != nil {
		return "", nil, err
	}
	raw, ok := cm.Data["kubeconfig"]
	if !ok {
		return "", nil, errors.New("cluster-info does not contain a public kubeconfig")
	}
	cfg, err := clientcmd.Load([]byte(raw))
	if err != nil {
		return "", nil, fmt.Errorf("failed to parse cluster-info kubeconfig: %w", err)
	}
	names := make([]string, 0, len(cfg.Clusters))
	for n := range cfg.Clusters {
		names = append(names, n)
	}
	if len(names) == 0 {
		return "", nil, errors.New("cluster-info kubeconfig does not have any clusters")
	}
	sort.Strings(names)
	cl := cfg.Clusters[names[0]]
	return cl.Server, cl.CertificateAuthorityData, nil
}

var serverSelectorRE = regexp.MustCompile(`^https?://`)

// clusterID selects by server URL (http(s)://...) or by name.
func clusterID(selector string) apiclient.ClusterID {
	if serverSelectorRE.MatchString(selector) {
		return apiclient.ClusterByServer(selector)
	}
	return apiclient.ClusterByName(selector)
}

func newClusterGetCommand(f *cmdutil.Factory) *cobra.Command {
	var format string
	c := &cobra.Command{
		Use:   "get SERVER/NAME",
		Short: "Get cluster information",
		Example: `patrol cluster get https://12.34.567.89
patrol cluster get in-cluster`,
		Args:              cmdutil.MinimumArgs(1),
		ValidArgsFunction: CompleteClusters(f, 0),
		RunE: func(c *cobra.Command, args []string) error {
			if err := output.CheckFormat(format, output.JSON, output.YAML, output.Wide, "server", ""); err != nil {
				return output.UnknownFormat(format)
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			var list []apiclient.RegisteredCluster
			for _, sel := range args {
				cl, err := client.GetCluster(c.Context(), clusterID(sel))
				if err != nil {
					return err
				}
				list = append(list, *cl)
			}
			switch format {
			case output.JSON, output.YAML:
				return output.PrintResourceList(f.Out(), list, format, true)
			case "server":
				for _, cl := range list {
					output.Println(f.Out(), cl.Spec.Server)
				}
				return nil
			}
			for _, cl := range list {
				printClusterDetails(f, &cl)
			}
			return nil
		},
	}
	c.Flags().StringVarP(&format, "output", "o", output.YAML, "Output format. One of: json|yaml|wide|server")
	return c
}

func printClusterDetails(f *cmdutil.Factory, cl *apiclient.RegisteredCluster) {
	w := f.Out()
	ns := "all namespaces"
	if len(cl.Spec.Namespaces) > 0 {
		ns = strings.Join(cl.Spec.Namespaces, ", ")
	}
	_, _ = fmt.Fprint(w, "Cluster information\n\n")
	output.Printf(w, "  Server URL:            %s\n", cl.Spec.Server)
	output.Printf(w, "  Server Name:           %s\n", output.OrDash(cl.Spec.Name))
	output.Printf(w, "  Server Version:        %s\n", cl.Status.ServerVersion)
	output.Printf(w, "  Namespaces:            %s\n", ns)
	_, _ = fmt.Fprint(w, "\nTLS configuration\n\n")
	_, _ = fmt.Fprintf(w, "  Client cert:           %v\n", cl.Credentials.HasClientCertificate)
	_, _ = fmt.Fprintf(w, "  Cert validation:       %v\n", !cl.Spec.Config.TLSClientConfig.Insecure)
	_, _ = fmt.Fprint(w, "\nAuthentication\n\n")
	_, _ = fmt.Fprintf(w, "  Basic authentication:  %v\n", cl.Credentials.HasBasicAuth)
	_, _ = fmt.Fprintf(w, "  oAuth authentication:  %v\n", cl.Credentials.HasBearerToken)
	_, _ = fmt.Fprintf(w, "  AWS authentication:    %v\n", cl.Spec.Config.AWSAuthConfig != nil)
	_, _ = fmt.Fprintf(w, "\nDisable compression: %v\n", cl.Spec.Config.DisableCompression)
	_, _ = fmt.Fprintf(w, "\nUse proxy: %v\n", cl.Spec.Config.ProxyURL != "")
	_, _ = fmt.Fprintln(w)
}

func newClusterListCommand(f *cmdutil.Factory) *cobra.Command {
	var format string
	c := &cobra.Command{
		Use:   "list",
		Short: "List configured clusters",
		Example: `
# List Clusters in Default "Wide" Format
patrol cluster list

# List Cluster via specifying the server
patrol cluster list --server <PATROL_SERVER_ADDRESS>

# List Clusters in JSON Format
patrol cluster list -o json --server <PATROL_SERVER_ADDRESS>

# List Clusters in YAML Format
patrol cluster list -o yaml --server <PATROL_SERVER_ADDRESS>

# List Clusters that have been added to your Patrol CD
patrol cluster list -o server <PATROL_SERVER_ADDRESS>
`,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := output.CheckFormat(format, output.JSON, output.YAML, output.Wide, "server", ""); err != nil {
				return output.UnknownFormat(format)
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			l, err := client.ListClusters(c.Context(), nil)
			if err != nil {
				return err
			}
			switch format {
			case output.JSON, output.YAML:
				return output.PrintResourceList(f.Out(), l.Items, format, false)
			case "server":
				for _, cl := range l.Items {
					output.Println(f.Out(), cl.Spec.Server)
				}
				return nil
			}
			t := output.NewTable(f.Out())
			t.Row("SERVER", "NAME", "VERSION", "STATUS", "MESSAGE", "PROJECT")
			for _, cl := range l.Items {
				server := cl.Spec.Server
				if len(cl.Spec.Namespaces) > 0 {
					server = fmt.Sprintf("%s (%d namespaces)", server, len(cl.Spec.Namespaces))
				}
				status, msg := "", ""
				if cs := cl.Status.ConnectionState; cs != nil {
					status, msg = string(cs.Status), cs.Message
				}
				t.Row(server, cl.Spec.Name, cl.Status.ServerVersion, status, msg, cl.Spec.Project)
			}
			return t.Flush()
		},
	}
	c.Flags().StringVarP(&format, "output", "o", output.Wide, "Output format. One of: json|yaml|wide|server")
	return c
}

func newClusterSetCommand(f *cmdutil.Factory) *cobra.Command {
	var (
		name                string
		namespaces          []string
		labels, annotations []string
	)
	c := &cobra.Command{
		Use:   "set NAME",
		Short: "Set cluster information",
		Example: `  # Set cluster information
  patrol cluster set CLUSTER_NAME --name new-cluster-name --namespace '*'
  patrol cluster set CLUSTER_NAME --name new-cluster-name --namespace namespace-one --namespace namespace-two`,
		Args:              cmdutil.ExactArgs(1),
		ValidArgsFunction: CompleteClusters(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			var fields []string
			if name != "" {
				fields = append(fields, "name")
			}
			if namespaces != nil {
				fields = append(fields, "namespaces")
			}
			if labels != nil {
				fields = append(fields, "labels")
			}
			if annotations != nil {
				fields = append(fields, "annotations")
			}
			if fields == nil {
				output.Println(f.Out(), "Specify the cluster field to be updated.")
				return nil
			}
			if len(namespaces) == 1 && strings.EqualFold(namespaces[0], allNamespaces) {
				namespaces = []string{}
			}
			lm, err := parseLabels(labels)
			if err != nil {
				return err
			}
			am, err := parseLabels(annotations)
			if err != nil {
				return err
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			in := &apiclient.ClusterInput{Name: name, Namespaces: namespaces, Labels: lm, Annotations: am}
			if _, err := client.UpdateCluster(c.Context(), apiclient.ClusterByName(args[0]), in, fields...); err != nil {
				if apiclient.IsPermissionDenied(err) {
					f.Logger().Error("Ensure that the cluster is present and you have the necessary permissions to update the cluster")
				}
				return err
			}
			output.Printf(f.Out(), "Cluster '%s' updated.\n", args[0])
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "Overwrite the cluster name")
	c.Flags().StringArrayVar(&namespaces, "namespace", nil, "List of namespaces which are allowed to manage. Specify '*' to manage all namespaces")
	c.Flags().StringArrayVar(&labels, "label", nil, "Set metadata labels (e.g. --label key=value)")
	c.Flags().StringArrayVar(&annotations, "annotation", nil, "Set metadata annotations (e.g. --annotation key=value)")
	return c
}

func newClusterRemoveCommand(f *cmdutil.Factory) *cobra.Command {
	var noPrompt bool
	var kubeconfig string
	c := &cobra.Command{
		Use:   "rm SERVER/NAME",
		Short: "Remove cluster credentials",
		Example: `patrol cluster rm https://12.34.567.89
patrol cluster rm cluster-name`,
		Args:              cmdutil.MinimumArgs(1),
		ValidArgsFunction: CompleteClusters(f, 0),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			all := false
			for _, sel := range args {
				answer := "y"
				if !noPrompt && !all {
					var msg string
					if len(args) == 1 {
						msg = "Are you sure you want to remove '" + sel + "'? Any Apps deploying to this cluster will go to health status Unknown.[y/n] "
					} else {
						msg = "Are you sure you want to remove '" + sel + "'? Any Apps deploying to this cluster will go to health status Unknown.[y/n/A] where 'A' is to remove all specified clusters without prompting. Any Apps deploying to these clusters will go to health status Unknown. "
					}
					yes, a, err := f.Prompter().ConfirmAll(msg)
					if err != nil {
						return err
					}
					switch {
					case a && len(args) > 1:
						all = true
					case yes || a:
					default:
						answer = "n"
					}
				}
				if answer != "y" {
					output.Println(f.Out(), "The command to remove '"+sel+"' was cancelled.")
					continue
				}
				id := clusterID(sel)
				cl, err := client.GetCluster(c.Context(), id)
				if err != nil {
					return err
				}
				if err := client.DeleteCluster(c.Context(), id); err != nil {
					return err
				}
				output.Printf(f.Out(), "Cluster '%s' removed\n", sel)
				uninstallManager(c.Context(), f, kubeconfig, cl)
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&noPrompt, "yes", "y", false, "Turn off prompting to confirm remove of cluster resources")
	c.Flags().StringVar(&kubeconfig, "kubeconfig", "", "use a particular kubeconfig file (to remove the patrol-manager service account)")
	return c
}

// uninstallManager removes the patrol-manager identity from the cluster through the
// kubeconfig context named like the cluster (upstream). Without such a context nothing
// is removed.
func uninstallManager(ctx context.Context, f *cmdutil.Factory, kubeconfig string, cl *apiclient.RegisteredCluster) {
	raw, err := loadKubeconfig(f, kubeconfig)
	if err != nil || cl.Spec.Name == "" || raw.Contexts[cl.Spec.Name] == nil {
		f.Logger().Info(fmt.Sprintf("no kubeconfig context named %q: the %s service account was not removed from the cluster", cl.Spec.Name, clusters.ManagerServiceAccount))
		return
	}
	conf, err := restConfigForContext(raw, cl.Spec.Name)
	if err == nil {
		var kube kubernetes.Interface
		if kube, err = kubernetes.NewForConfig(conf); err == nil {
			err = clusters.UninstallManager(ctx, kube, clusters.DefaultSystemNamespace, cl.Spec.Namespaces, &clusters.BootstrapOptions{Logger: f.Logger()})
		}
	}
	if err != nil {
		f.Logger().Warn(fmt.Sprintf("failed to remove the %s service account: %v", clusters.ManagerServiceAccount, err))
	}
}

func newClusterRotateAuthCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "rotate-auth SERVER/NAME",
		Short: CommandName + " cluster rotate-auth SERVER/NAME",
		Example: `patrol cluster rotate-auth https://12.34.567.89
patrol cluster rotate-auth cluster-name`,
		Args:              cmdutil.ExactArgs(1),
		ValidArgsFunction: CompleteClusters(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			if err := client.RotateClusterAuth(c.Context(), clusterID(args[0])); err != nil {
				return err
			}
			output.Printf(f.Out(), "Cluster '%s' rotated auth\n", args[0])
			return nil
		},
	}
}
