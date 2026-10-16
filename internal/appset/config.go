package appset

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/gobwas/glob"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

// Controller defaults.
const (
	// ControllerName names the controller (events source, controller-runtime controller name).
	ControllerName = "applicationset-controller"
	// SyncInitiator is the Operation spec.initiatedBy.username of RollingSync syncs.
	SyncInitiator = "applicationset-controller"
	// DefaultConcurrentReconciliations is the default number of parallel reconciles.
	DefaultConcurrentReconciliations = 10
	// MaxConcurrentApplicationUpdates caps parallel Application writes within one reconcile.
	MaxConcurrentApplicationUpdates = 200
	// DefaultMaxResourcesStatusCount is the default cap of status.resources entries.
	DefaultMaxResourcesStatusCount = 5000
	// RequeueOnValidationError is the requeue interval after a generation or validation error
	// (upstream ReconcileRequeueOnValidationError).
	RequeueOnValidationError = 3 * time.Minute
	// ReverseDeletionRequeue is the polling interval of ordered (Reverse) deletion.
	ReverseDeletionRequeue = 10 * time.Second
	// StaleTerminatingThreshold is how long an Application may appear terminating in the cache
	// during Reverse deletion before the controller verifies with an uncached read.
	StaleTerminatingThreshold = 2 * time.Minute
	// DefaultLeaderElectionID is the leader election lease name.
	DefaultLeaderElectionID = "58ac56fa.applicationsets." + v1alpha1.GroupName
	// maxConditionMessage bounds condition messages (metav1.Condition allows 32768 bytes).
	maxConditionMessage = 32 * 1024
)

// ErrSCMProvidersRequireAllowList is returned by Config validation when ApplicationSets in other
// namespaces are enabled together with SCM providers but without an allow-list (upstream
// startup check).
var ErrSCMProvidersRequireAllowList = errors.New("when enabling applicationset in any namespace using applicationset-namespaces, you must either set --enable-scm-providers=false or specify --allowed-scm-providers")

// Config configures the ApplicationSet controller.
type Config struct {
	// Namespace is the control-plane namespace (required). AppProjects, Clusters, plugin and
	// cluster decision resource ConfigMaps live there; ApplicationSets in it are always
	// reconciled.
	Namespace string
	// ApplicationSetNamespaces are additional namespaces whose ApplicationSets are reconciled
	// (applicationsetcontroller.namespaces): exact names, globs, or regular expressions wrapped
	// in "/". Generated Applications live in their ApplicationSet's namespace.
	ApplicationSetNamespaces []string
	// Policy is the controller-wide Application sync policy (applicationsetcontroller.policy);
	// empty means sync.
	Policy v1alpha1.ApplicationsSyncPolicy
	// EnablePolicyOverride lets spec.syncPolicy.applicationsSync override Policy
	// (applicationsetcontroller.enable.policy.override). Use DefaultEnablePolicyOverride for
	// the upstream default.
	EnablePolicyOverride bool
	// EnableProgressiveSyncs enables spec.strategy (applicationsetcontroller.enable.progressive.syncs).
	EnableProgressiveSyncs bool
	// GlobalPreservedAnnotations and GlobalPreservedLabels are preserved on every generated
	// Application (applicationsetcontroller.global.preserved.annotations / .labels).
	GlobalPreservedAnnotations []string
	GlobalPreservedLabels      []string
	// MaxResourcesStatusCount caps status.resources (applicationsetcontroller.status.max.resources.count);
	// 0 means unlimited. Callers pass DefaultMaxResourcesStatusCount for the default.
	MaxResourcesStatusCount int
	// ConcurrentApplicationUpdates bounds parallel Application writes within one reconcile
	// (≤0: 1; capped at MaxConcurrentApplicationUpdates).
	ConcurrentApplicationUpdates int
	// ConcurrentReconciliations bounds parallel reconciles (≤0: DefaultConcurrentReconciliations).
	ConcurrentReconciliations int
	// DryRun makes every write a server-side dry run (applicationsetcontroller.dryrun).
	DryRun bool
	// DisableInCluster mirrors cluster.inClusterEnabled=false: the in-cluster destination is
	// not a valid destination unless a Cluster registers it.
	DisableInCluster bool
	// GlobalProjects are the parsed globalProjects settings applied to generated Applications'
	// effective projects during validation.
	GlobalProjects []project.GlobalProjectSetting
	// Generators configures the generators. Client and Namespace default to the controller's.
	Generators generators.Config
	// MetricsApplicationSetLabels lists ApplicationSet label keys exported by the
	// patrol_appset_labels metric (empty: the metric is not exported).
	MetricsApplicationSetLabels []string
	// Registry receives the controller metrics; nil disables them.
	Registry prometheus.Registerer
	// Logger receives controller logs; nil discards them.
	Logger *slog.Logger
	// Now returns the current time (tests); nil uses time.Now.
	Now func() time.Time
}

// DefaultEnablePolicyOverride returns the upstream default of the policy override flag: true
// when no controller policy was configured through the environment/settings.
func DefaultEnablePolicyOverride(configuredPolicy string) bool {
	return configuredPolicy == ""
}

// ParsePolicy validates a controller policy value ("" means sync).
func ParsePolicy(s string) (v1alpha1.ApplicationsSyncPolicy, error) {
	switch v1alpha1.ApplicationsSyncPolicy(s) {
	case "", v1alpha1.ApplicationsSyncPolicySync:
		return v1alpha1.ApplicationsSyncPolicySync, nil
	case v1alpha1.ApplicationsSyncPolicyCreateOnly, v1alpha1.ApplicationsSyncPolicyCreateUpdate, v1alpha1.ApplicationsSyncPolicyCreateDelete:
		return v1alpha1.ApplicationsSyncPolicy(s), nil
	}
	return "", fmt.Errorf("invalid policy %q: Policy value can be: sync, create-only, create-update, create-delete, default value: sync", s)
}

// EffectivePolicy returns the policy that applies to appSet: its spec.syncPolicy.applicationsSync
// when set and override is enabled, the controller policy otherwise.
func EffectivePolicy(appSet *v1alpha1.ApplicationSet, controllerPolicy v1alpha1.ApplicationsSyncPolicy, override bool) v1alpha1.ApplicationsSyncPolicy {
	if controllerPolicy == "" {
		controllerPolicy = v1alpha1.ApplicationsSyncPolicySync
	}
	if appSet == nil || appSet.Spec.SyncPolicy == nil || appSet.Spec.SyncPolicy.ApplicationsSync == nil || !override {
		return controllerPolicy
	}
	if *appSet.Spec.SyncPolicy.ApplicationsSync == "" {
		return controllerPolicy
	}
	return *appSet.Spec.SyncPolicy.ApplicationsSync
}

// completed is a validated Config with defaults applied.
type completed struct {
	Config
	namespaces *NamespaceMatcher
	log        *slog.Logger
	now        func() time.Time
}

func (c Config) complete() (*completed, error) {
	if c.Namespace == "" {
		return nil, errors.New("appset: the control-plane namespace is required")
	}
	policy, err := ParsePolicy(string(c.Policy))
	if err != nil {
		return nil, err
	}
	c.Policy = policy
	matcher, err := NewNamespaceMatcher(c.Namespace, c.ApplicationSetNamespaces)
	if err != nil {
		return nil, err
	}
	if matcher.MultiNamespace() && c.Generators.SCM.Enabled && len(c.Generators.SCM.AllowedProviders) == 0 {
		return nil, ErrSCMProvidersRequireAllowList
	}
	if c.ConcurrentApplicationUpdates <= 0 {
		c.ConcurrentApplicationUpdates = 1
	}
	if c.ConcurrentApplicationUpdates > MaxConcurrentApplicationUpdates {
		c.ConcurrentApplicationUpdates = MaxConcurrentApplicationUpdates
	}
	if c.ConcurrentReconciliations <= 0 {
		c.ConcurrentReconciliations = DefaultConcurrentReconciliations
	}
	if c.MaxResourcesStatusCount < 0 {
		c.MaxResourcesStatusCount = 0
	}
	if c.Generators.Namespace == "" {
		c.Generators.Namespace = c.Namespace
	}
	log := c.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if c.Generators.Logger == nil {
		c.Generators.Logger = log
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	return &completed{Config: c, namespaces: matcher, log: log, now: now}, nil
}

// NamespaceMatcher decides which namespaces' ApplicationSets are reconciled.
type NamespaceMatcher struct {
	control  string
	literals map[string]bool
	globs    []*glob.Pattern
	regexps  []*regexp.Regexp
	all      bool
}

// NewNamespaceMatcher compiles patterns (exact names, gobwas globs, "/regexp/"). The control
// namespace always matches. Invalid patterns are errors.
func NewNamespaceMatcher(control string, patterns []string) (*NamespaceMatcher, error) {
	m := &NamespaceMatcher{control: control, literals: map[string]bool{control: true}}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(p) > 2 && strings.HasPrefix(p, "/") && strings.HasSuffix(p, "/") {
			re, err := regexp.Compile(p[1 : len(p)-1])
			if err != nil {
				return nil, fmt.Errorf("invalid applicationset namespace pattern %q: %w", p, err)
			}
			m.regexps = append(m.regexps, re)
			continue
		}
		if p == "*" {
			m.all = true
		}
		if !strings.ContainsAny(p, "*?[{\\") {
			m.literals[p] = true
			continue
		}
		g, err := glob.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid applicationset namespace pattern %q: %w", p, err)
		}
		m.globs = append(m.globs, g)
	}
	return m, nil
}

// Allowed reports whether ApplicationSets in namespace are reconciled.
func (m *NamespaceMatcher) Allowed(namespace string) bool {
	if m == nil {
		return false
	}
	if m.all || m.literals[namespace] {
		return true
	}
	for _, g := range m.globs {
		if g.Match(namespace) {
			return true
		}
	}
	for _, re := range m.regexps {
		if re.MatchString(namespace) {
			return true
		}
	}
	return false
}

// MultiNamespace reports whether namespaces other than the control namespace are enabled.
func (m *NamespaceMatcher) MultiNamespace() bool {
	return m != nil && (len(m.literals) > 1 || len(m.globs) > 0 || len(m.regexps) > 0 || m.all)
}

// LiteralNamespaces returns the namespaces to cache when every pattern is a literal name, and
// false when a glob or regexp requires a cluster-wide cache.
func (m *NamespaceMatcher) LiteralNamespaces() ([]string, bool) {
	if m == nil || m.all || len(m.globs) > 0 || len(m.regexps) > 0 {
		return nil, false
	}
	out := make([]string, 0, len(m.literals))
	for ns := range m.literals {
		out = append(out, ns)
	}
	return out, true
}
