// Package applications implements the core Application routes of patrol-server
// (api-contract.md, "Applications"): CRUD with refresh, the watch stream, sync, rollback,
// operation termination and listing, sync window state, revision metadata, manifests and
// deep links. Resource-level routes (resource tree, managed resources, server-side diff,
// events, live resources, actions, logs, terminal) are added to the same Service by a later
// task through the extension point in routes.go.
//
// Handlers only read and write Kubernetes objects of the control plane (Application,
// Operation, AppProject, Cluster, Repository) and call the repo-server; the controller
// reconciles asynchronously. Authorization is enforced in the handlers with the
// Application's RBAC object ("<project>/<name>", or "<project>/<namespace>/<name>" outside
// the control-plane namespace), following the upstream existence-hiding rules.
package applications

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/gobwas/glob"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// Defaults of Config.
const (
	// DefaultRefreshTimeout bounds how long GET ?refresh= waits for the controller.
	DefaultRefreshTimeout = 15 * time.Second
	// DefaultMaxLocalFilesBytes caps the gzipped tar of manifests-with-files.
	DefaultMaxLocalFilesBytes int64 = 32 << 20
	// DefaultWatchBuffer is the per-subscriber event buffer of the Broadcaster (upstream
	// ARGOCD_WATCH_API_BUFFER_SIZE).
	DefaultWatchBuffer = 1000
	// MaxLocalManifestsBytes caps the local manifests of a sync request (the Operation CRD
	// admits at most 1 MiB in total).
	MaxLocalManifestsBytes = 1 << 20
	// MaxLocalManifests caps their number (Operation CRD MaxItems).
	MaxLocalManifests = 1024
	// MaxSyncResources caps the resources of a selective sync.
	MaxSyncResources = 5000
	// MaxOperationInfos caps spec.info of an Operation (CRD MaxItems).
	MaxOperationInfos = 64
	// MaxListLimit caps ?limit= of list routes.
	MaxListLimit = 5000
	// defaultOperationsLimit is the page size of the operations list.
	defaultOperationsLimit = 100
	maxOperationsLimit     = 1000
	// maxUpdateAttempts bounds optimistic-concurrency retries (upstream: 10).
	maxUpdateAttempts = 10
)

// Config is the static configuration of the handlers.
type Config struct {
	// Namespace is the control-plane namespace.
	Namespace string
	// ApplicationNamespaces are the additional namespaces Applications may live in
	// (application.namespaces: globs, or regular expressions wrapped in "/").
	ApplicationNamespaces []string
	// DisableSyncWithReplace refuses syncs whose options contain Replace=true
	// (server.sync.replace.allowed=false).
	DisableSyncWithReplace bool
	// DisableGPG mirrors PATROL_GPG_ENABLED=false: source integrity GPG policies are then
	// not criteria (upstream ARGOCD_GPG_ENABLED).
	DisableGPG bool
	// RefreshTimeout bounds GET ?refresh= (default DefaultRefreshTimeout).
	RefreshTimeout time.Duration
	// RefreshPollInterval is how often a refresh re-reads the Application besides watching
	// events (default 1s).
	RefreshPollInterval time.Duration
	// MaxLocalFilesBytes caps the body of manifests-with-files (default
	// DefaultMaxLocalFilesBytes).
	MaxLocalFilesBytes int64
}

// Parameter keys of patrol-cmd-params-cm read by ConfigFromParams.
const (
	ParamApplicationNamespaces = "application.namespaces"
	ParamSyncReplaceAllowed    = "server.sync.replace.allowed"
)

// ConfigFromParams returns the Config of the server component's parameters
// (settings.ResolveParams(settings.ComponentServer, ...)): application.namespaces and
// server.sync.replace.allowed. GPG and timing options keep their defaults.
func ConfigFromParams(namespace string, p *settings.Params) Config {
	c := Config{Namespace: namespace}
	if p == nil {
		return c
	}
	c.ApplicationNamespaces = p.Strings(ParamApplicationNamespaces)
	if _, ok := p.Lookup(ParamSyncReplaceAllowed); ok {
		c.DisableSyncWithReplace = !p.Bool(ParamSyncReplaceAllowed)
	}
	return c
}

// SettingsSource returns the current settings snapshot; *settings.Manager implements it.
type SettingsSource interface {
	Current() *settings.Settings
}

// RepositoryResolver resolves repository access (credentials) for a project;
// *repository.Resolver implements it.
type RepositoryResolver interface {
	GetRepository(ctx context.Context, repoURL, project string) (*apiclient.Repository, error)
	ListDependencyRepositories(ctx context.Context, proj *v1alpha1.AppProject, includeOCI bool) ([]apiclient.Repository, []apiclient.RepositoryCredentialTemplate, error)
}

// RepoServer is the part of the repo-server API the handlers use; the repo-server
// apiclient.Client implements it.
type RepoServer interface {
	GenerateManifest(ctx context.Context, req *apiclient.ManifestRequest) (*apiclient.ManifestResponse, error)
	GenerateManifestWithFiles(ctx context.Context, req *apiclient.ManifestRequest, files io.Reader) (*apiclient.ManifestResponse, error)
	ResolveRevision(ctx context.Context, req *apiclient.ResolveRevisionRequest) (*apiclient.ResolveRevisionResponse, error)
	GetAppDetails(ctx context.Context, req *apiclient.AppDetailsQuery) (*apiclient.AppDetails, error)
	GetRevisionMetadata(ctx context.Context, req *apiclient.RevisionMetadataRequest) (*apiclient.RevisionMetadata, error)
	GetRevisionChartDetails(ctx context.Context, req *apiclient.ChartDetailsRequest) (*apiclient.ChartDetails, error)
	GetOCIMetadata(ctx context.Context, req *apiclient.ChartDetailsRequest) (*apiclient.OCIMetadata, error)
}

// ResourceRef identifies a resource managed by an Application.
type ResourceRef struct {
	Group, Version, Kind, Namespace, Name string
}

// ErrResourceNotInTree is returned by LiveResources when the resource is not part of the
// Application's resource tree.
var ErrResourceNotInTree = errors.New("resource not found as part of the application")

// LiveResources reads the live objects managed by an Application from its destination
// cluster. It is implemented over the controller tree API by the resource routes; nil
// makes the routes that need live objects answer 503.
type LiveResources interface {
	// GetManagedResource returns the live object ref of app's resource tree (proj is the
	// Application's effective project), or an error wrapping ErrResourceNotInTree.
	GetManagedResource(ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject, ref ResourceRef) (*unstructured.Unstructured, error)
}

// Deps are the collaborators of the handlers. Nil collaborators make the routes that need
// them answer 503, so New(Deps{}) can register the routes for the OpenAPI document.
type Deps struct {
	Config Config
	// Client reads (uncached, read-after-write) and writes Applications and Operations. The
	// server's identity needs the "impersonate" verb on operations.cd.patrol.example to set
	// spec.initiatedBy (v1alpha1.OperationInitiatorVerb).
	Client client.Client
	// Reader serves lists and project/cluster/repository lookups (normally the informer
	// cache); nil uses Client.
	Reader client.Reader
	// Settings provides patrol-cm settings (deep links, tracking, Helm/Kustomize options,
	// override privilege, global projects).
	Settings SettingsSource
	// Events feeds the watch stream and refresh waits (a Broadcaster attached to the
	// Application informer). Nil disables the stream (503); refreshes then poll.
	Events Events
	// Repositories resolves repository credentials (*repository.Resolver).
	Repositories RepositoryResolver
	// RepoServer renders manifests and reads revision metadata.
	RepoServer RepoServer
	// LiveResources reads managed live objects (resource links); optional. When nil and
	// Resources.Tree and Resources.Clusters are set, New uses the tree API and the
	// destination cluster (resource_deps.go).
	LiveResources LiveResources
	// Resources are the collaborators of the resource-level routes (resource tree,
	// managed resources, server-side diff, events, live resources, actions, logs,
	// terminal); see ResourceDeps.
	Resources ResourceDeps
	Logger    *slog.Logger
	Now       func() time.Time
}

// Service serves the application routes; it implements apiserver.Service.
type Service struct {
	d  Deps
	ns *namespaceMatcher
	// cachedReads is true when a separate (cached) Reader was given.
	cachedReads bool
}

// New returns the application service.
func New(d Deps) *Service {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	cached := d.Reader != nil
	if d.Reader == nil && d.Client != nil {
		d.Reader = d.Client
	}
	if d.Config.RefreshTimeout <= 0 {
		d.Config.RefreshTimeout = DefaultRefreshTimeout
	}
	if d.Config.RefreshPollInterval <= 0 {
		d.Config.RefreshPollInterval = time.Second
	}
	if d.Config.MaxLocalFilesBytes <= 0 {
		d.Config.MaxLocalFilesBytes = DefaultMaxLocalFilesBytes
	}
	m, warnings := newNamespaceMatcher(d.Config.Namespace, d.Config.ApplicationNamespaces)
	for _, w := range warnings {
		d.Logger.Warn("applications: ignoring application namespace pattern", "problem", w)
	}
	s := &Service{d: d, ns: m, cachedReads: cached}
	s.initResources()
	return s
}

var errNotConfigured = errors.New("not configured")

func unavailable(what string) error {
	return rest.Unavailable("%s is not available", what).WithCause(errNotConfigured)
}

func (s *Service) kube() (client.Client, error) {
	if s.d.Client == nil {
		return nil, unavailable("the Kubernetes API")
	}
	return s.d.Client, nil
}

func (s *Service) settings() (*settings.Settings, error) {
	if s.d.Settings == nil {
		return nil, unavailable("settings")
	}
	st := s.d.Settings.Current()
	if st == nil {
		return nil, rest.Unavailable("settings are not loaded yet")
	}
	return st, nil
}

func (s *Service) repoServer() (RepoServer, error) {
	if s.d.RepoServer == nil {
		return nil, unavailable("the repo-server")
	}
	return s.d.RepoServer, nil
}

func (s *Service) repositories() (RepositoryResolver, error) {
	if s.d.Repositories == nil {
		return nil, unavailable("repository resolution")
	}
	return s.d.Repositories, nil
}

func (s *Service) log() *slog.Logger { return s.d.Logger }

func (s *Service) now() time.Time { return s.d.Now() }

// controlNS is the control-plane namespace.
func (s *Service) controlNS() string { return s.d.Config.Namespace }

// nsOrDefault returns ns, or the control-plane namespace when empty.
func (s *Service) nsOrDefault(ns string) string {
	if ns == "" {
		return s.d.Config.Namespace
	}
	return ns
}

// namespaceEnabled reports whether Applications in ns are served.
func (s *Service) namespaceEnabled(ns string) bool { return s.ns.allowed(ns) }

// lookups returns the project/cluster/repository lookups over the cached reader.
func (s *Service) lookups(st *settings.Settings) (*project.KubeLookup, project.Lookups) {
	inCluster := true
	var globals []project.GlobalProjectSetting
	if st != nil {
		inCluster = st.InClusterEnabled()
		for _, g := range st.GlobalProjects() {
			globals = append(globals, project.GlobalProjectSetting{ProjectName: g.ProjectName, LabelSelector: g.LabelSelector})
		}
	}
	kl := project.NewKubeLookup(s.d.Reader, s.controlNS(), inCluster)
	return kl, kl.Lookups(globals)
}

// namespaceMatcher decides which namespaces may hold Applications: the control-plane
// namespace and those matching application.namespaces (upstream glob.MatchStringInList
// with REGEXP: "/re/" entries are RE2 regular expressions, others globs).
type namespaceMatcher struct {
	controlPlane string
	globs        []*glob.Pattern
	regexps      []*regexp.Regexp
}

func newNamespaceMatcher(controlPlane string, patterns []string) (*namespaceMatcher, []string) {
	m := &namespaceMatcher{controlPlane: controlPlane}
	var warnings []string
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(p) >= 2 && strings.HasPrefix(p, "/") && strings.HasSuffix(p, "/") {
			re, err := regexp.Compile(p[1 : len(p)-1])
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("namespace pattern %q is not a valid RE2 expression: %v", p, err))
				continue
			}
			m.regexps = append(m.regexps, re)
			continue
		}
		g, err := glob.Compile(p)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("namespace pattern %q never matches: %v", p, err))
			continue
		}
		m.globs = append(m.globs, g)
	}
	return m, warnings
}

// clusterWide reports whether Applications outside the control-plane namespace exist.
func (m *namespaceMatcher) clusterWide() bool { return len(m.globs)+len(m.regexps) > 0 }

func (m *namespaceMatcher) allowed(ns string) bool {
	if ns == m.controlPlane {
		return true
	}
	for _, re := range m.regexps {
		if re.MatchString(ns) {
			return true
		}
	}
	for _, g := range m.globs {
		if g.Match(ns) {
			return true
		}
	}
	return false
}

// namespaceNotPermitted is the upstream error for a namespace outside
// application.namespaces.
func namespaceNotPermitted(ns string) error {
	return fmt.Errorf("namespace '%s' is not permitted", ns)
}
