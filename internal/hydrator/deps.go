package hydrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/commitserver"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// Defaults.
const (
	// DefaultProcessors is the default number of manifest-hydration workers
	// (controller.hydration.processors).
	DefaultProcessors = 5
	// FailureCooldown is the fixed delay before an unattended retry of a failed hydration
	// (upstream hard-codes two minutes).
	FailureCooldown = 2 * time.Minute
	// maxStatusMessageRunes bounds the failure message stored in the status.
	maxStatusMessageRunes = 4096
	// renderParallelism bounds concurrent repo-server renders inside one group.
	renderParallelism = 8
)

// RepositoryResolver resolves read access to a repository for an AppProject (internal/
// repository implements it). project "" means only credentials that are not project-scoped.
type RepositoryResolver interface {
	GetRepository(ctx context.Context, repoURL, project string) (*apiclient.Repository, error)
}

// WriteCredentialResolver resolves write access (Repository purpose write, write credential
// templates) for the destination repository. A nil repository without error means no write
// credentials are configured; the push is then attempted anonymously.
type WriteCredentialResolver interface {
	GetWriteRepository(ctx context.Context, repoURL, project string) (*apiclient.Repository, error)
}

// HelmRepositoryLister lists the Helm repositories and credential templates visible to a
// project, forwarded to the repo-server for chart dependencies.
type HelmRepositoryLister interface {
	ListHelmRepositories(ctx context.Context, proj *v1alpha1.AppProject) ([]apiclient.Repository, []apiclient.RepositoryCredentialTemplate, error)
}

// ProjectResolver returns the effective AppProject of an Application, failing when the
// project does not exist or does not admit the Application's namespace.
type ProjectResolver interface {
	ResolveAppProject(ctx context.Context, app *v1alpha1.Application) (*v1alpha1.AppProject, error)
}

// ClusterInfo reports the Kubernetes version of an Application's destination cluster, sent
// to the repo-server when rendering the dry source.
type ClusterInfo interface {
	KubeVersion(ctx context.Context, app *v1alpha1.Application) (string, error)
}

// SettingsSource returns the current settings snapshot (*settings.Manager implements it).
type SettingsSource interface {
	Current() *settings.Settings
}

// StaticSettings is a SettingsSource returning a fixed snapshot.
type StaticSettings struct{ Settings *settings.Settings }

// Current implements SettingsSource.
func (s StaticSettings) Current() *settings.Settings { return s.Settings }

// Deps are the dependencies and parameters of the hydrator.
type Deps struct {
	// Enabled is the hydrator.enabled parameter. Setup registers nothing when false.
	Enabled bool
	// ControlPlaneNamespace is the namespace Patrol CD runs in (projects, instance names).
	ControlPlaneNamespace string

	// Client reads Applications (normally the manager's cached client) and writes their
	// metadata and status. Setup defaults it to mgr.GetClient().
	Client client.Client
	// APIReader re-reads an Application after an optimistic-concurrency conflict. Setup
	// defaults it to mgr.GetAPIReader(); New defaults it to Client.
	APIReader client.Reader

	// RepoServer renders the dry source and resolves its revision. Required.
	RepoServer apiclient.Client
	// CommitServer writes the hydrated manifests. Required.
	CommitServer commitserver.Client
	// Repositories resolves read access to the dry repository. Required.
	Repositories RepositoryResolver
	// WriteCredentials resolves write access to the destination repository. Required.
	WriteCredentials WriteCredentialResolver
	// HelmRepositories lists Helm dependency repositories (optional).
	HelmRepositories HelmRepositoryLister
	// Projects resolves effective projects. Default: KubeProjectResolver over Client.
	Projects ProjectResolver
	// Settings provides patrol-cm settings (templates, author, tracking). Required.
	Settings SettingsSource
	// ClusterInfo provides the destination Kubernetes version (optional; none is sent
	// without it).
	ClusterInfo ClusterInfo

	// Processors is the number of manifest-hydration workers (default DefaultProcessors;
	// values below 1 are coerced to 1 with a warning).
	Processors int
	// AppProcessors is the number of app-hydration workers (default 1, as upstream).
	AppProcessors int
	// PollInterval is how often the dry branch of each Application is re-resolved. Default:
	// timeout.reconciliation of the settings (0 there disables polling); negative disables
	// polling.
	PollInterval time.Duration
	// StaleTimeout is how long an Application may stay Hydrating before its hydration key is
	// re-enqueued (upstream statusRefreshTimeout, APP-131). Default: timeout.reconciliation
	// of the settings, or 120s when that is 0.
	StaleTimeout time.Duration
	// DisableGPG turns the project's GnuPG source integrity criteria off for the dry source,
	// mirroring the repo-server's switch (upstream ARGOCD_GPG_ENABLED=false). The zero
	// value enforces them, as upstream's default. A failing integrity result returned by
	// the repo-server fails the hydration whatever this setting.
	DisableGPG bool
	// AppFilter restricts the Applications this controller hydrates (sharding, application
	// namespaces). Nil accepts every Application.
	AppFilter func(*v1alpha1.Application) bool

	// Registry registers the hydrator metrics (nil: not registered).
	Registry prometheus.Registerer
	// Logger is the hydrator logger (default slog.Default()).
	Logger *slog.Logger
	// Now is the clock (default time.Now).
	Now func() time.Time
}

func (d *Deps) normalize() error {
	var missing []error
	for name, ok := range map[string]bool{
		"Client":           d.Client != nil,
		"RepoServer":       d.RepoServer != nil,
		"CommitServer":     d.CommitServer != nil,
		"Repositories":     d.Repositories != nil,
		"WriteCredentials": d.WriteCredentials != nil,
		"Settings":         d.Settings != nil,
	} {
		if !ok {
			missing = append(missing, fmt.Errorf("hydrator: %s is required", name))
		}
	}
	if err := errors.Join(missing...); err != nil {
		return err
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.APIReader == nil {
		d.APIReader = d.Client
	}
	if d.Projects == nil {
		d.Projects = &KubeProjectResolver{Reader: d.Client, Namespace: d.ControlPlaneNamespace, Settings: d.Settings, Logger: d.Logger}
	}
	switch {
	case d.Processors == 0:
		d.Processors = DefaultProcessors
	case d.Processors < 1:
		d.Logger.Warn(fmt.Sprintf("hydration-processors was set to %d; hydration requires at least one worker, using 1 instead", d.Processors))
		d.Processors = 1
	}
	if d.AppProcessors < 1 {
		d.AppProcessors = 1
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return nil
}

// KubeProjectResolver resolves effective projects (scoped repositories and clusters, global
// projects, source namespaces) from objects in the control-plane namespace.
type KubeProjectResolver struct {
	Reader    client.Reader
	Namespace string
	Settings  SettingsSource
	Logger    *slog.Logger
}

// ResolveAppProject implements ProjectResolver.
func (r *KubeProjectResolver) ResolveAppProject(ctx context.Context, app *v1alpha1.Application) (*v1alpha1.AppProject, error) {
	st := r.Settings.Current()
	if st == nil {
		return nil, errSettingsNotLoaded
	}
	var globals []project.GlobalProjectSetting
	for _, g := range st.GlobalProjects() {
		globals = append(globals, project.GlobalProjectSetting{ProjectName: g.ProjectName, LabelSelector: g.LabelSelector})
	}
	lookup := project.NewKubeLookup(r.Reader, r.Namespace, st.InClusterEnabled())
	proj, globalErr, err := project.ResolveAppProject(ctx, app, r.Namespace, lookup.Lookups(globals))
	if globalErr != nil && r.Logger != nil {
		r.Logger.Warn("hydrator: global projects", slog.String("app", app.QualifiedName()), slog.Any("error", globalErr))
	}
	return proj, err
}

var errSettingsNotLoaded = errors.New("settings are not loaded yet")
