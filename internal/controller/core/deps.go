package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// Comparer compares an Application with its live state; *compare.Manager implements it.
type Comparer interface {
	Compare(ctx context.Context, in compare.Input) (*compare.Result, error)
}

// ClusterConfigFunc returns the REST config used to reach a destination cluster with the
// controller's own identity (impersonation is applied by the caller).
type ClusterConfigFunc func(ctx context.Context, cluster *v1alpha1.Cluster) (*rest.Config, error)

// Deps are the dependencies shared by the Application and Operation reconcilers.
type Deps struct {
	// Client reads Patrol objects from the manager cache and writes to the API server.
	// Required.
	Client client.Client
	// APIReader reads from the API server, bypassing the cache (fresh reads before writes,
	// admission). Required.
	APIReader client.Reader
	// Recorder records Kubernetes Events (nil: discarded).
	Recorder EventRecorder
	// Comparer compares Applications. Required.
	Comparer Comparer
	// Settings is the current patrol-cm snapshot. Required.
	Settings compare.SettingsSource
	// LiveState provides the live-state caches of destination clusters. Required.
	LiveState compare.LiveStateProvider
	// ClusterConfig returns destination REST configs. Required.
	ClusterConfig ClusterConfigFunc
	// Clusters resolves destinations and project-scoped clusters (*clusters.Registry). Nil
	// reads Cluster objects of the control-plane namespace through Client.
	Clusters project.ClusterLookup
	// Namespace is the control-plane namespace. Required.
	Namespace string
	// Params are the controller parameters (zero value: DefaultParams is not applied; use
	// DefaultParams or ParamsFrom).
	Params Params
	// Metrics are the Prometheus collectors (nil: not recorded).
	Metrics *Metrics
	// Refresher lets the Operation reconciler request Application refreshes (nil: none).
	Refresher RefreshRequester
	// OwnsCluster reports whether this controller shard processes Applications (and their
	// Operations) of a destination cluster; it is called with nil for a destination that does
	// not resolve (shard 0 in ADR-0008). Nil processes every cluster.
	OwnsCluster func(cluster *v1alpha1.Cluster) bool
	// Logger receives structured logs (nil: slog.Default()).
	Logger *slog.Logger
	// Clock is the time source (nil: the real clock).
	Clock clock.Clock

	namespaces *NamespaceMatcher
}

// Complete validates d and fills defaults.
func (d *Deps) Complete() error {
	var errs []error
	for name, ok := range map[string]bool{
		"Client":        d.Client != nil,
		"APIReader":     d.APIReader != nil,
		"Comparer":      d.Comparer != nil,
		"Settings":      d.Settings != nil,
		"LiveState":     d.LiveState != nil,
		"ClusterConfig": d.ClusterConfig != nil,
		"Namespace":     d.Namespace != "",
	} {
		if !ok {
			errs = append(errs, fmt.Errorf("controller: %s is required", name))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if d.Recorder == nil {
		d.Recorder = NopRecorder{}
	}
	if d.Refresher == nil {
		d.Refresher = NopRefresher{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = clock.RealClock{}
	}
	if d.Params.StatusProcessors <= 0 {
		d.Params.StatusProcessors = 1
	}
	if d.Params.OperationProcessors <= 0 {
		d.Params.OperationProcessors = 1
	}
	d.namespaces = NewNamespaceMatcher(d.Namespace, d.Params.ApplicationNamespaces)
	return nil
}

// Namespaces returns the Application namespace matcher.
func (d *Deps) Namespaces() *NamespaceMatcher {
	if d.namespaces == nil {
		d.namespaces = NewNamespaceMatcher(d.Namespace, d.Params.ApplicationNamespaces)
	}
	return d.namespaces
}

// CurrentSettings returns the current settings snapshot or an error when none is loaded.
func (d *Deps) CurrentSettings() (*settings.Settings, error) {
	st := d.Settings.Current()
	if st == nil {
		return nil, errors.New("settings are not loaded")
	}
	return st, nil
}

// ClusterLookup returns the destination cluster lookup.
func (d *Deps) ClusterLookup(st *settings.Settings) project.ClusterLookup {
	if d.Clusters != nil {
		return d.Clusters
	}
	inCluster := true
	if st != nil {
		inCluster = st.InClusterEnabled()
	}
	return project.NewKubeLookup(d.Client, d.Namespace, inCluster)
}

// ProjectLookups returns the lookups of project resolution under settings st.
func (d *Deps) ProjectLookups(st *settings.Settings) project.Lookups {
	inCluster := true
	var globals []project.GlobalProjectSetting
	if st != nil {
		inCluster = st.InClusterEnabled()
		for _, g := range st.GlobalProjects() {
			globals = append(globals, project.GlobalProjectSetting{ProjectName: g.ProjectName, LabelSelector: g.LabelSelector})
		}
	}
	kl := project.NewKubeLookup(d.Client, d.Namespace, inCluster)
	return project.Lookups{Projects: kl, Repositories: kl, Clusters: d.ClusterLookup(st), GlobalProjects: globals}
}

// ResolveProject returns the effective project of app (global projects merged, scoped
// repositories and clusters added) and checks the app's namespace may use it. A non-fatal
// global-project problem is logged.
func (d *Deps) ResolveProject(ctx context.Context, app *v1alpha1.Application, st *settings.Settings) (*v1alpha1.AppProject, error) {
	proj, gerr, err := project.ResolveAppProject(ctx, app, d.Namespace, d.ProjectLookups(st))
	if gerr != nil {
		d.Logger.Warn("global project problem", "application", app.QualifiedName(), "error", gerr)
	}
	return proj, err
}

// DestinationCluster resolves an Application destination to its Cluster.
func (d *Deps) DestinationCluster(ctx context.Context, dest v1alpha1.ApplicationDestination, st *settings.Settings) (*v1alpha1.Cluster, error) {
	return project.ResolveDestination(ctx, dest, d.ClusterLookup(st))
}

// SkipReconcile reports whether annotations carry cd.patrol.example/skip-reconcile set to a
// value strconv.ParseBool accepts as true (APP-088, REPO-097).
func SkipReconcile(annotations map[string]string) bool {
	v, ok := annotations[v1alpha1.AnnotationKeySkipReconcile]
	if !ok {
		return false
	}
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

// LookupClusterResolver adapts a project.ClusterLookup to compare.ClusterResolver.
type LookupClusterResolver struct {
	Lookup project.ClusterLookup
}

// DestinationCluster implements compare.ClusterResolver.
func (r LookupClusterResolver) DestinationCluster(dest v1alpha1.ApplicationDestination) (*v1alpha1.Cluster, error) {
	return project.ResolveDestination(context.Background(), dest, r.Lookup)
}

// GetProjectClusters implements compare.ClusterResolver.
func (r LookupClusterResolver) GetProjectClusters(ctx context.Context, proj string) ([]*v1alpha1.Cluster, error) {
	return r.Lookup.GetProjectClusters(ctx, proj)
}
