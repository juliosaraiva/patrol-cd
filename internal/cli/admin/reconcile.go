package admin

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// reconcileApplications compares every selected Application again without the controller
// (upstream get-reconcile-results --refresh): settings, cluster registry, repository
// credentials, live-state caches and the comparison engine are built in-process from the
// kubeconfig credentials, and the repo-server renders the manifests.
func reconcileApplications(ctx context.Context, f *Factory, kc *kubeClients, repoClient apiclient.Client, selector string, serverSideDiff bool, jqTimeout time.Duration) ([]appReconcileResult, error) {
	log := f.logger()
	quiet := slog.New(slog.DiscardHandler)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	settingsMgr, err := settings.NewManager(kc.kube, settings.ManagerOptions{Namespace: kc.namespace, Logger: quiet})
	if err != nil {
		return nil, err
	}
	if err := settingsMgr.Start(ctx); err != nil {
		return nil, fmt.Errorf("error loading settings: %w", err)
	}
	st := settingsMgr.Current()
	inCluster := st.InClusterEnabled()
	registry, err := clusters.NewRegistry(kc.dynamic, kc.kube, clusters.RegistryOptions{Namespace: kc.namespace, InClusterEnabled: &inCluster, Logger: quiet})
	if err != nil {
		return nil, err
	}
	if err := registry.Start(ctx); err != nil {
		return nil, fmt.Errorf("error loading clusters: %w", err)
	}
	lister, err := repository.NewLister(kc.dynamic, kc.kube, repository.ListerOptions{Namespace: kc.namespace, Logger: quiet})
	if err != nil {
		return nil, err
	}
	if err := lister.Start(ctx); err != nil {
		return nil, fmt.Errorf("error loading repositories: %w", err)
	}
	resolver, err := repository.NewResolver(lister, repository.Options{Logger: quiet, RepoServer: repoClient})
	if err != nil {
		return nil, err
	}
	// The in-cluster destination is reached with the kubeconfig credentials, as from inside a
	// pod (upstream sets ARGOCD_FAKE_IN_CLUSTER).
	clusterOpts := clusters.Options{InClusterConfig: func() (*rest.Config, error) { return rest.CopyConfig(kc.config), nil }}
	cacheMgr := cache.NewManager(cache.ManagerOptions{Logger: quiet})
	defer cacheMgr.Close()
	base := cache.Settings{ClusterResyncPeriod: 12 * time.Hour, RetainManagedFields: serverSideDiff}
	live, err := compare.NewLiveStateCache(compare.LiveStateCacheOptions{
		Manager: cacheMgr,
		Config: func(ctx context.Context, c *v1alpha1.Cluster) (*rest.Config, error) {
			return registry.RestConfig(ctx, c, clusterOpts)
		},
		Settings: settingsMgr, ControlPlaneNamespace: kc.namespace, Base: base, Logger: quiet,
	})
	if err != nil {
		return nil, err
	}
	ssd, err := compare.NewKubeServerSideDiff(func(ctx context.Context, c *v1alpha1.Cluster) (*rest.Config, error) {
		return registry.RawRestConfig(ctx, c, clusterOpts)
	}, live, 0)
	if err != nil {
		return nil, err
	}
	cmp, err := compare.New(compare.Deps{
		RepoServer: repoClient, Repositories: resolver, Settings: settingsMgr, Clusters: registry, LiveState: live, ServerSideDiff: ssd,
		ControlPlaneNamespace: kc.namespace, ServerSideDiffEnabled: serverSideDiff, JQTimeout: jqTimeout, Logger: quiet,
	})
	if err != nil {
		return nil, err
	}
	var globals []project.GlobalProjectSetting
	for _, g := range st.GlobalProjects() {
		globals = append(globals, project.GlobalProjectSetting{ProjectName: g.ProjectName, LabelSelector: g.LabelSelector})
	}
	lookups := project.NewKubeLookup(kc.ctrl, kc.namespace, inCluster).Lookups(globals)

	apps, err := listApplications(ctx, kc, selector)
	if err != nil {
		return nil, err
	}
	type withServer struct {
		app    v1alpha1.Application
		server string
	}
	items := make([]withServer, 0, len(apps))
	for _, app := range apps {
		dest, err := registry.DestinationCluster(app.Spec.Destination)
		if err != nil {
			return nil, fmt.Errorf("error getting destination cluster: %w", err)
		}
		items = append(items, withServer{app: app, server: dest.Spec.Server})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].server < items[j].server })
	out := f.out()
	prev := ""
	var results []appReconcileResult
	for i := range items {
		app := &items[i].app
		if items[i].server != prev {
			_, _ = fmt.Fprintf(out, "Reconciling apps of %s\n", items[i].server)
			prev = items[i].server
		}
		_, _ = fmt.Fprintln(out, app.Name)
		proj, globalErr, err := project.ResolveAppProject(ctx, app, kc.namespace, lookups)
		if err != nil {
			return nil, fmt.Errorf("error getting namespaced project: %w", err)
		}
		if globalErr != nil {
			log.Warn("global projects", "error", globalErr)
		}
		res, err := cmp.Compare(ctx, compare.Input{App: app, Project: proj, NoCache: true, SkipTree: true})
		if err != nil {
			return nil, fmt.Errorf("error comparing app states: %w", err)
		}
		sync := res.SyncStatus
		// As upstream, the conditions are the stored ones updated by this comparison.
		conditions := compare.MergeConditions(app.Status.Conditions, res.Conditions, res.EvaluatedConditionTypes, app.Generation, metav1.NewTime(f.now()))
		results = append(results, appReconcileResult{Name: app.Name, Health: res.Health, Sync: &sync, Conditions: conditions})
	}
	return results, nil
}
