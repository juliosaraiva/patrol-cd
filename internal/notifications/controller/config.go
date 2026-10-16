package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"

	"github.com/juliosaraiva/patrol-cd/internal/notifications"
	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// Defaults.
const (
	// DefaultConfigMapName is the notifications ConfigMap (argocd-notifications-cm upstream).
	DefaultConfigMapName = "patrol-notifications-cm"
	// DefaultSecretName is the notifications Secret (argocd-notifications-secret upstream).
	DefaultSecretName = "patrol-notifications-secret"
	// DefaultResyncPeriod is how often every in-scope Application is re-evaluated (time based
	// conditions, retries of failed deliveries); upstream's informer resync is 60s.
	DefaultResyncPeriod = 60 * time.Second
	// DefaultProcessors is the number of concurrent reconciles (upstream default 1).
	DefaultProcessors = 1
	// DefaultLeaderElectionID names the Lease used when Config.LeaderElection is set.
	DefaultLeaderElectionID = "patrol-notifications-controller"
	// ControllerName is the controller and event reporting component name.
	ControllerName = "patrol-notifications-controller"
	// DefaultUserAgent is set on the REST config when the caller set none (NOTIF-009).
	DefaultUserAgent = "patrol-notifications-controller"
	// DefaultQPS and DefaultBurst are applied when the REST config leaves them zero, like the
	// upstream client defaults, so the per-application annotation patches are not throttled.
	DefaultQPS   = 50
	DefaultBurst = 100

	// minResyncPeriod bounds how often every application can be re-evaluated.
	minResyncPeriod = time.Second
	// maxProcessors bounds concurrent reconciles.
	maxProcessors = 1024
	// maxNamespacePatterns bounds the application namespace list.
	maxNamespacePatterns = 1024
)

// RepositoryResolver resolves the repository access material (credentials included) an
// application's project may use, for the repo.GetCommitMetadata and repo.GetAppDetails helpers.
// Returned credentials are only forwarded to the repo-server and never logged.
type RepositoryResolver interface {
	ResolveRepository(ctx context.Context, repoURL, project string) (*apiclient.Repository, error)
}

// Config configures Run.
type Config struct {
	// RestConfig is the Kubernetes client configuration (required). It is copied; a missing user
	// agent, QPS or burst gets the defaults above.
	RestConfig *rest.Config
	// Namespace is the control-plane namespace (required): AppProjects and the controller-level
	// configuration live here and its Applications are always in scope.
	Namespace string
	// ApplicationNamespaces are additional namespaces Applications may live in
	// (application.namespaces): globs, or regular expressions wrapped in "/". Empty watches only
	// Namespace. When every entry is a literal namespace name only those namespaces are cached
	// (namespaced RBAC suffices); otherwise Applications are watched cluster-wide and out-of-scope
	// ones are ignored (upstream).
	ApplicationNamespaces []string
	// AppLabelSelector restricts the watched Applications (--app-label-selector).
	AppLabelSelector string
	// ConfigMapName and SecretName name the configuration objects (defaults above).
	ConfigMapName string
	SecretName    string
	// SelfServiceEnabled reads, in addition, the configuration objects of each application's
	// namespace (notificationscontroller.selfservice.enabled). It also hides .secrets from every
	// template, as upstream.
	SelfServiceEnabled bool
	// DisableDefaultCatalog stops merging the built-in trigger/template catalog under every
	// configuration (ConfigMap keys always win over the catalog).
	DisableDefaultCatalog bool
	// Processors is the number of concurrent reconciles (notificationscontroller.processors.count).
	Processors int
	// ResyncPeriod is the periodic re-evaluation interval of every Application (default 60s).
	ResyncPeriod time.Duration

	// Registry receives patrol_notifications_deliveries_total and
	// patrol_notifications_trigger_eval_total. Nil keeps the metrics unregistered.
	Registry prometheus.Registerer
	// Logger is the structured logger (default: discard).
	Logger *slog.Logger

	// RepoClient serves the repo.* helpers. Nil makes them fail with
	// notifications.ErrRepoServiceUnavailable.
	RepoClient apiclient.Client
	// RepoResolver resolves repository credentials for RepoClient requests. Nil sends requests
	// with the repository URL only (public repositories).
	RepoResolver RepositoryResolver

	// Network configures outbound delivery (dialer, per-host CAs, URL validation, timeouts).
	// Its DialContext applies to the control-plane configuration only; the other fields apply
	// to namespace (self-service) configurations too.
	Network services.Network
	// AllowPrivateEgress disables the default SSRF guard of the control-plane configuration.
	// Unless it is set or Network.DialContext is provided, deliveries cannot reach loopback,
	// private, link-local (cloud metadata), CGNAT or other non-public addresses, except
	// EgressAllowedPrefixes.
	AllowPrivateEgress bool
	// EgressAllowedPrefixes are reachable by the control-plane configuration despite the guard
	// (e.g. an in-cluster Alertmanager).
	EgressAllowedPrefixes []netip.Prefix
	// TenantEgressAllowedPrefixes are the only non-public ranges namespace (self-service)
	// configurations may reach (default none). Namespace configurations always use the
	// SSRF-guarded dialer: AllowPrivateEgress, EgressAllowedPrefixes and Network.DialContext
	// never apply to them, so opening a range for the control plane does not open it to
	// every tenant ConfigMap.
	TenantEgressAllowedPrefixes []netip.Prefix
	// TemplateLimits bounds each render (zero fields keep the engine defaults).
	TemplateLimits notifications.TemplateLimits
	// SendTimeout bounds one delivery including retries (default 2m).
	SendTimeout time.Duration

	// LeaderElection enables leader election (single active replica, as upstream runs one).
	LeaderElection bool
	// LeaderElectionNamespace defaults to Namespace; LeaderElectionID to DefaultLeaderElectionID.
	LeaderElectionNamespace string
	LeaderElectionID        string

	// Now is the clock of the engine and the controller (tests). Default time.Now.
	Now func() time.Time
}

// completed is a validated Config with defaults applied.
type completed struct {
	Config
	selector labels.Selector
	scope    namespaceMatcher
	// tenantNetwork is the outbound network of namespace (self-service) configurations:
	// Network with the guarded dialer allowing only TenantEgressAllowedPrefixes.
	tenantNetwork services.Network
}

func (c Config) complete() (*completed, error) {
	var errs []error
	if c.RestConfig == nil {
		errs = append(errs, errors.New("rest config is required"))
	}
	if c.Namespace == "" {
		errs = append(errs, errors.New("namespace is required"))
	} else if msgs := validation.IsDNS1123Label(c.Namespace); len(msgs) > 0 {
		errs = append(errs, fmt.Errorf("invalid namespace %q: %v", c.Namespace, msgs))
	}
	if c.ConfigMapName == "" {
		c.ConfigMapName = DefaultConfigMapName
	}
	if c.SecretName == "" {
		c.SecretName = DefaultSecretName
	}
	for _, n := range []string{c.ConfigMapName, c.SecretName} {
		if msgs := validation.IsDNS1123Subdomain(n); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("invalid object name %q: %v", n, msgs))
		}
	}
	switch {
	case c.Processors <= 0:
		c.Processors = DefaultProcessors
	case c.Processors > maxProcessors:
		errs = append(errs, fmt.Errorf("processors must be at most %d", maxProcessors))
	}
	switch {
	case c.ResyncPeriod == 0:
		c.ResyncPeriod = DefaultResyncPeriod
	case c.ResyncPeriod < minResyncPeriod:
		errs = append(errs, fmt.Errorf("resync period must be at least %s", minResyncPeriod))
	}
	if c.SendTimeout < 0 {
		errs = append(errs, errors.New("send timeout must not be negative"))
	}
	if len(c.ApplicationNamespaces) > maxNamespacePatterns {
		errs = append(errs, fmt.Errorf("at most %d application namespace patterns are supported", maxNamespacePatterns))
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.LeaderElectionID == "" {
		c.LeaderElectionID = DefaultLeaderElectionID
	}
	if c.LeaderElectionNamespace == "" {
		c.LeaderElectionNamespace = c.Namespace
	}
	if c.Network.UserAgent == "" {
		c.Network.UserAgent = "patrol-notifications"
	}
	tenantNetwork := c.Network
	tenantNetwork.DialContext = services.NewGuardedDialer(services.GuardOptions{AllowedPrefixes: c.TenantEgressAllowedPrefixes})
	if c.Network.DialContext == nil && !c.AllowPrivateEgress {
		c.Network.DialContext = services.NewGuardedDialer(services.GuardOptions{AllowedPrefixes: c.EgressAllowedPrefixes})
	}
	selector, err := labels.Parse(c.AppLabelSelector)
	if err != nil {
		errs = append(errs, fmt.Errorf("invalid application label selector: %w", err))
	}
	scope, warnings := newNamespaceMatcher(c.Namespace, c.ApplicationNamespaces)
	if len(errs) > 0 {
		return nil, fmt.Errorf("notifications controller configuration: %w", errors.Join(errs...))
	}
	for _, w := range warnings {
		c.Logger.Warn("ignoring application namespace entry", "warning", w)
	}
	rc := rest.CopyConfig(c.RestConfig)
	if rc.UserAgent == "" {
		rc.UserAgent = DefaultUserAgent
	}
	if rc.QPS == 0 {
		rc.QPS = DefaultQPS
	}
	if rc.Burst == 0 {
		rc.Burst = DefaultBurst
	}
	c.RestConfig = rc
	return &completed{Config: c, selector: selector, scope: scope, tenantNetwork: tenantNetwork}, nil
}
