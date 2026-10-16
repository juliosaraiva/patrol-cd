package admin

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"

	"github.com/gobwas/glob"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/controller/sharding"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// inClusterName is the name of the synthetic cluster for the in-cluster server.
const inClusterName = "in-cluster"

func newClusterCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Manage clusters configuration",
		Example: `
#Generate declarative config for a cluster
patrol admin cluster generate-spec my-cluster -o yaml

#Generate a kubeconfig for a cluster and write it to a file
patrol admin cluster kubeconfig https://cluster-api-url:6443 /path/to/output/kubeconfig.yaml

#Print information namespaces which Patrol CD manages in each cluster
patrol admin cluster namespaces`,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	ns := newClusterNamespacesCommand(f, kube)
	ns.AddCommand(newEnableNamespacedModeCommand(f, kube), newDisableNamespacedModeCommand(f, kube))
	cmd.AddCommand(
		newClusterKubeconfigCommand(f, kube),
		newClusterGenerateSpecCommand(f, kube),
		newClusterStatsCommand(f, kube),
		newClusterShardsCommand(f, kube),
		ns,
	)
	return cmd
}

// clusterWithInfo is a cluster with its computed shard and the namespaces its
// Applications use.
type clusterWithInfo struct {
	cluster    v1alpha1.Cluster
	synthetic  bool
	shard      int
	namespaces []string
}

// clusterInventory is the clusters and Applications of the control-plane namespace.
type clusterInventory struct {
	clusters []v1alpha1.Cluster // includes the synthetic in-cluster cluster when enabled
	apps     []v1alpha1.Application
}

// loadInventory lists the Clusters and Applications of the control-plane namespace. The
// synthetic in-cluster cluster is added when no Cluster serves the in-cluster server and
// cluster.inClusterEnabled is not "false" (upstream ListClusters).
func loadInventory(ctx context.Context, kc *kubeClients) (*clusterInventory, error) {
	var list v1alpha1.ClusterList
	if err := kc.ctrl.List(ctx, &list, client.InNamespace(kc.namespace)); err != nil {
		return nil, fmt.Errorf("error listing clusters: %w", err)
	}
	var apps v1alpha1.ApplicationList
	if err := kc.ctrl.List(ctx, &apps, client.InNamespace(kc.namespace)); err != nil {
		return nil, fmt.Errorf("error listing application: %w", err)
	}
	inv := &clusterInventory{apps: apps.Items}
	slices.SortFunc(list.Items, func(a, b v1alpha1.Cluster) int {
		return cmp.Or(cmp.Compare(clusters.NormalizeServer(a.Spec.Server), clusters.NormalizeServer(b.Spec.Server)), cmp.Compare(a.Name, b.Name))
	})
	hasInCluster := false
	seen := map[string]bool{}
	for _, c := range list.Items {
		server := clusters.NormalizeServer(c.Spec.Server)
		if seen[server] {
			continue // the object with the smallest name serves a server (registry rule)
		}
		seen[server] = true
		if server == v1alpha1.KubernetesInternalAPIServerAddr {
			hasInCluster = true
		}
		inv.clusters = append(inv.clusters, c)
	}
	if !hasInCluster && inClusterEnabled(ctx, kc) {
		inv.clusters = append([]v1alpha1.Cluster{{Spec: v1alpha1.ClusterSpec{Server: v1alpha1.KubernetesInternalAPIServerAddr, Name: inClusterName}}}, inv.clusters...)
	}
	return inv, nil
}

func inClusterEnabled(ctx context.Context, kc *kubeClients) bool {
	cm, err := kc.kube.CoreV1().ConfigMaps(kc.namespace).Get(ctx, settings.DefaultConfigMapName, metav1.GetOptions{})
	if err != nil {
		return true
	}
	return cm.Data["cluster.inClusterEnabled"] != "false"
}

// destinationServer resolves an Application destination to a server URL with upstream's
// messages.
func (inv *clusterInventory) destinationServer(d v1alpha1.ApplicationDestination) (string, error) {
	if d.Server != "" {
		return clusters.NormalizeServer(d.Server), nil
	}
	var servers []string
	for _, c := range inv.clusters {
		if c.Spec.Name == d.Name {
			servers = append(servers, clusters.NormalizeServer(c.Spec.Server))
		}
	}
	switch len(servers) {
	case 1:
		return servers[0], nil
	case 0:
		return "", fmt.Errorf("there are no clusters with this name: %s", d.Name)
	default:
		return "", fmt.Errorf("there are %d clusters with the same name: %v", len(servers), servers)
	}
}

// withInfo computes shards (replicas > 0) and the destination namespaces of each cluster.
// shardFilter >= 0 keeps only that shard. resourceNamespaces uses the namespaces of
// status.resources (cluster namespaces command) instead of the destination namespace.
func (inv *clusterInventory) withInfo(alg sharding.Algorithm, replicas, shardFilter int, resourceNamespaces bool, log *slog.Logger) ([]clusterWithInfo, error) {
	nsByServer := map[string]map[string]bool{}
	appsPerServer := map[string]int64{}
	for _, app := range inv.apps {
		server, err := inv.destinationServer(app.Spec.Destination)
		if err != nil {
			return nil, fmt.Errorf("error validating application destination: %w", err)
		}
		appsPerServer[server]++
		if nsByServer[server] == nil {
			nsByServer[server] = map[string]bool{}
		}
		if resourceNamespaces && len(app.Status.Resources) > 0 {
			for _, r := range app.Status.Resources {
				if r.Namespace != "" {
					nsByServer[server][r.Namespace] = true
				}
			}
		} else {
			nsByServer[server][app.Spec.Destination.Namespace] = true
		}
	}
	infos := make([]sharding.ClusterInfo, len(inv.clusters))
	for i := range inv.clusters {
		infos[i] = sharding.ClusterInfoFrom(&inv.clusters[i])
	}
	var dist map[string]int
	if replicas > 0 {
		dist = sharding.Distribute(alg, infos, appsPerServer, replicas, log)
	}
	var out []clusterWithInfo
	for i, c := range inv.clusters {
		server := clusters.NormalizeServer(c.Spec.Server)
		shard := 0
		if replicas > 0 {
			shard = dist[server]
		}
		if shardFilter >= 0 && shard != shardFilter {
			continue
		}
		var namespaces []string
		for ns := range nsByServer[server] {
			namespaces = append(namespaces, ns)
		}
		slices.SortFunc(namespaces, func(a, b string) int { return cmp.Or(cmp.Compare(len(a), len(b)), cmp.Compare(a, b)) })
		out = append(out, clusterWithInfo{cluster: c, synthetic: inv.clusters[i].Name == "", shard: shard, namespaces: namespaces})
	}
	return out, nil
}

// shardingFlags are the flags of "cluster stats" and "cluster shards".
type shardingFlags struct {
	shard          int
	replicas       int
	method         string
	controllerName string
	ignoredRedis   redisFlags
	cmd            *cobra.Command
}

// redisFlags are upstream's Redis cache flags. Patrol CD has no Redis (ADR-0004): cluster
// statistics are read from Cluster status, so they are accepted and ignored.
type redisFlags struct {
	portForward bool
	str         string
	strs        []string
	boolean     bool
	db          int
}

// bindIgnoredRedisFlags registers upstream's Redis flags (with their upstream types) as
// accepted and ignored; hidden hides them from the help.
func bindIgnoredRedisFlags(fl *pflag.FlagSet, r *redisFlags, usage string, hidden bool) {
	for _, name := range redisFlagNames {
		switch name {
		case "port-forward-redis":
			fl.BoolVar(&r.portForward, name, true, usage)
		case "sentinel":
			fl.StringArrayVar(&r.strs, name, nil, usage)
		case "redis-insecure-skip-tls-verify", "redis-use-tls":
			fl.BoolVar(&r.boolean, name, false, usage)
		case "redisdb":
			fl.IntVar(&r.db, name, 0, usage)
		default:
			fl.StringVar(&r.str, name, "", usage)
		}
		if hidden {
			_ = fl.MarkHidden(name)
		}
	}
}

func (s *shardingFlags) bind(cmd *cobra.Command) {
	s.cmd = cmd
	fl := cmd.Flags()
	fl.IntVar(&s.shard, "shard", -1, "Cluster shard filter")
	fl.IntVar(&s.replicas, "replicas", 0, "Application controller replicas count. Inferred from number of running controller pods if not specified")
	fl.StringVar(&s.method, "sharding-method", string(sharding.DefaultAlgorithm), "Sharding method. Defaults: legacy. Supported sharding methods are : [legacy, round-robin, consistent-hashing] ")
	fl.StringVar(&s.controllerName, "controller-name", sharding.DefaultApplicationControllerName, "Value of the app.kubernetes.io/name label of the application controller pods (to count replicas)")
	bindIgnoredRedisFlags(fl, &s.ignoredRedis, redisIgnoredMessage, false)
}

// redisIgnoredMessage explains why upstream's Redis flags have no effect.
const redisIgnoredMessage = "Ignored: Patrol CD has no Redis; cluster statistics are read from the Cluster status"

// redisFlagNames are upstream's Redis cache flags, accepted for compatibility.
var redisFlagNames = []string{
	"port-forward-redis", "redis", "redis-ca-certificate", "redis-client-certificate", "redis-client-key",
	"redis-compress", "sentinel", "sentinelmaster", "redis-insecure-skip-tls-verify", "redis-use-tls", "redisdb",
	"app-state-cache-expiration", "default-cache-expiration",
}

// warnIgnoredFlags reports given flags that have no effect.
func warnIgnoredFlags(cmd *cobra.Command, w io.Writer, names []string, why string) {
	for _, name := range names {
		if fl := cmd.Flags().Lookup(name); fl != nil && fl.Changed {
			_, _ = fmt.Fprintf(w, "warning: --%s: %s\n", name, why)
		}
	}
}

// controllerReplicas counts the controller pods (upstream getControllerReplicas).
func controllerReplicas(ctx context.Context, kc *kubeClients, name string) (int, error) {
	pods, err := kc.kube.CoreV1().Pods(kc.namespace).List(ctx, metav1.ListOptions{LabelSelector: v1alpha1.LabelKeyAppName + "=" + name})
	if err != nil {
		return 0, err
	}
	return len(pods.Items), nil
}

func (s *shardingFlags) load(ctx context.Context, f *Factory, kube *kubeOptions) ([]clusterWithInfo, int, error) {
	warnIgnoredFlags(s.cmd, f.errOut(), redisFlagNames, "Patrol CD has no Redis; cluster statistics are read from the Cluster status")
	kc, err := kube.get(nil)
	if err != nil {
		return nil, 0, err
	}
	replicas := s.replicas
	if replicas == 0 {
		if replicas, err = controllerReplicas(ctx, kc, s.controllerName); err != nil {
			return nil, 0, err
		}
	}
	inv, err := loadInventory(ctx, kc)
	if err != nil {
		return nil, 0, err
	}
	log := f.logger()
	list, err := inv.withInfo(sharding.ParseAlgorithm(s.method, log), replicas, s.shard, false, log)
	return list, replicas, err
}

func connectionStatus(c v1alpha1.Cluster) string {
	if c.Status.ConnectionState == nil {
		return ""
	}
	return string(c.Status.ConnectionState.Status)
}

func newClusterStatsCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	var s shardingFlags
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Prints information cluster statistics and inferred shard number",
		Example: `
#Display stats and shards for clusters
patrol admin cluster stats

#Display Cluster Statistics for a Specific Shard
patrol admin cluster stats --shard=1`,
		RunE: func(c *cobra.Command, _ []string) error {
			list, _, err := s.load(c.Context(), f, kube)
			if err != nil {
				return err
			}
			tw := newTable(f.out())
			_, _ = fmt.Fprint(tw, "SERVER\tSHARD\tCONNECTION\tNAMESPACES COUNT\tAPPS COUNT\tRESOURCES COUNT\n")
			for _, ci := range list {
				st := ci.cluster.Status
				_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%d\t%d\n", ci.cluster.Spec.Server, ci.shard, connectionStatus(ci.cluster), len(ci.namespaces), st.Info.ApplicationsCount, st.Info.CacheInfo.ResourcesCount)
			}
			return tw.Flush()
		},
	}
	s.bind(cmd)
	return cmd
}

func newClusterShardsCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	var s shardingFlags
	cmd := &cobra.Command{
		Use:   "shards",
		Short: "Print information about each controller shard and the estimated portion of Kubernetes resources it is responsible for.",
		RunE: func(c *cobra.Command, _ []string) error {
			list, replicas, err := s.load(c.Context(), f, kube)
			if err != nil {
				return err
			}
			if replicas == 0 || len(list) == 0 {
				return nil
			}
			printShardSummary(f.out(), list, replicas)
			return nil
		},
	}
	s.bind(cmd)
	return cmd
}

// printShardSummary prints each shard's cached resource count and its share of the
// per-shard average (upstream printStatsSummary; empty shards are listed too).
func printShardSummary(w io.Writer, list []clusterWithInfo, replicas int) {
	var total int64
	byShard := make([]int64, replicas)
	for _, ci := range list {
		n := ci.cluster.Status.Info.CacheInfo.ResourcesCount
		total += n
		if ci.shard >= 0 && ci.shard < replicas {
			byShard[ci.shard] += n
		}
	}
	avg := float64(total) / float64(replicas)
	tw := newTable(w)
	_, _ = fmt.Fprint(tw, "SHARD\tRESOURCES COUNT\n")
	for shard, n := range byShard {
		percent := 0.0
		if avg > 0 {
			percent = float64(n) / avg * 100
		}
		_, _ = fmt.Fprintf(tw, "%d\t%d (%.0f%%)\n", shard, n, percent)
	}
	_ = tw.Flush()
}

func newClusterNamespacesCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "namespaces",
		Short: "Print information namespaces which Patrol CD manages in each cluster.",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			list, _, err := namespacesInventory(c.Context(), f, kube)
			if err != nil {
				return err
			}
			tw := newTable(f.out())
			_, _ = fmt.Fprint(tw, "CLUSTER\tNAMESPACES\n")
			for _, ci := range list {
				ns := ci.namespaces
				s := strings.Join(ns, ",")
				if len(ns) > 4 {
					s = fmt.Sprintf("%s (total %d)", strings.Join(ns[:4], ","), len(ns))
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\n", ci.cluster.Spec.Server, s)
			}
			return tw.Flush()
		},
	}
}

func namespacesInventory(ctx context.Context, f *Factory, kube *kubeOptions) ([]clusterWithInfo, *kubeClients, error) {
	kc, err := kube.get(nil)
	if err != nil {
		return nil, nil, err
	}
	inv, err := loadInventory(ctx, kc)
	if err != nil {
		return nil, nil, err
	}
	list, err := inv.withInfo(sharding.DefaultAlgorithm, 0, -1, true, f.logger())
	return list, kc, err
}

// globMatch matches s against a glob pattern (upstream glob.Match: an invalid pattern
// matches nothing).
func globMatch(pattern, s string) bool {
	g, err := glob.Compile(pattern)
	return err == nil && g.Match(s)
}

func dryRunSuffix(dryRun bool) string {
	if dryRun {
		return "done (dry run)"
	}
	return "done"
}

func newEnableNamespacedModeCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	var (
		dryRun           bool
		clusterResources bool
		maxNamespaces    int
	)
	cmd := &cobra.Command{
		Use:   "enable-namespaced-mode PATTERN",
		Short: "Enable namespaced mode for clusters which name matches to the specified pattern.",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			list, kc, err := namespacesInventory(ctx, f, kube)
			if err != nil {
				return err
			}
			for _, ci := range list {
				if len(ci.namespaces) == 0 || len(ci.namespaces) > maxNamespaces || !globMatch(args[0], ci.cluster.Spec.Server) {
					continue
				}
				_, _ = fmt.Fprintf(f.out(), "Setting cluster %s namespaces to %v...", ci.cluster.Spec.Server, ci.namespaces)
				if ci.synthetic {
					_, _ = fmt.Fprintln(f.out(), "skipped (no Cluster object; create one for the in-cluster server first)")
					continue
				}
				if !dryRun {
					if err := updateClusterSpec(ctx, kc, ci.cluster.Name, func(spec *v1alpha1.ClusterSpec) {
						spec.Namespaces = slices.Clone(ci.namespaces)
						spec.ClusterResources = clusterResources
					}); err != nil {
						return fmt.Errorf("error updating cluster: %w", err)
					}
				}
				_, _ = fmt.Fprintln(f.out(), dryRunSuffix(dryRun))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", true, "Print what will be performed")
	cmd.Flags().BoolVar(&clusterResources, "cluster-resources", false, "Indicates if cluster level resources should be managed.")
	cmd.Flags().IntVar(&maxNamespaces, "max-namespace-count", 0, "Max number of namespaces that cluster should managed managed namespaces is less or equal to specified count")
	return cmd
}

func newDisableNamespacedModeCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "disable-namespaced-mode PATTERN",
		Short: "Disable namespaced mode for clusters which name matches to the specified pattern.",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			list, kc, err := namespacesInventory(ctx, f, kube)
			if err != nil {
				return err
			}
			for _, ci := range list {
				if !globMatch(args[0], ci.cluster.Spec.Server) || len(ci.cluster.Spec.Namespaces) == 0 || ci.synthetic {
					continue
				}
				_, _ = fmt.Fprintf(f.out(), "Disabling namespaced mode for cluster %s...", ci.cluster.Spec.Server)
				if !dryRun {
					if err := updateClusterSpec(ctx, kc, ci.cluster.Name, func(spec *v1alpha1.ClusterSpec) { spec.Namespaces = nil }); err != nil {
						return fmt.Errorf("error updating cluster: %w", err)
					}
				}
				_, _ = fmt.Fprintln(f.out(), dryRunSuffix(dryRun))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", true, "Print what will be performed")
	return cmd
}

// updateClusterSpec re-reads a Cluster and updates its spec with optimistic concurrency.
func updateClusterSpec(ctx context.Context, kc *kubeClients, name string, mutate func(*v1alpha1.ClusterSpec)) error {
	for attempt := 0; ; attempt++ {
		var c v1alpha1.Cluster
		if err := kc.ctrl.Get(ctx, client.ObjectKey{Namespace: kc.namespace, Name: name}, &c); err != nil {
			return err
		}
		mutate(&c.Spec)
		err := kc.ctrl.Update(ctx, &c)
		if err == nil || !apierrors.IsConflict(err) || attempt >= 4 {
			return err
		}
	}
}

// findClusterByServer returns the Cluster serving server (the object with the smallest
// name, as the registry chooses).
func findClusterByServer(ctx context.Context, kc *kubeClients, server string) (*v1alpha1.Cluster, error) {
	var list v1alpha1.ClusterList
	if err := kc.ctrl.List(ctx, &list, client.InNamespace(kc.namespace)); err != nil {
		return nil, fmt.Errorf("error listing clusters: %w", err)
	}
	server = clusters.NormalizeServer(server)
	var found *v1alpha1.Cluster
	for i := range list.Items {
		c := &list.Items[i]
		if clusters.NormalizeServer(c.Spec.Server) == server && (found == nil || c.Name < found.Name) {
			found = c
		}
	}
	if found == nil {
		return nil, &clusters.NotFoundError{Server: server}
	}
	return found, nil
}

// clusterSecretData returns the data of a Cluster's credentials Secret (nil without one).
func clusterSecretData(ctx context.Context, kc *kubeClients, c *v1alpha1.Cluster) (map[string][]byte, error) {
	ref := c.Spec.Config.CredentialsRef
	if ref == nil || ref.Name == "" {
		return nil, nil
	}
	var sec corev1.Secret
	if err := kc.ctrl.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: ref.Name}, &sec); err != nil {
		return nil, fmt.Errorf("error getting credentials Secret %s of cluster %s: %w", ref.Name, c.Spec.Server, err)
	}
	return sec.Data, nil
}
