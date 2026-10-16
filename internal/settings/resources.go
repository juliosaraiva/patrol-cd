package settings

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/gobwas/glob"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/lua"
)

type luaOverride = lua.ResourceOverride

// FilteredResource is one entry of resource.exclusions or resource.inclusions. Empty lists
// match everything. APIGroups and Clusters are gobwas globs; Kinds are exact names or "*".
type FilteredResource struct {
	APIGroups []string `json:"apiGroups,omitempty"`
	Kinds     []string `json:"kinds,omitempty"`
	Clusters  []string `json:"clusters,omitempty"`

	groupGlobs   []*glob.Pattern
	clusterGlobs []*glob.Pattern
}

// compile prepares the globs. An invalid pattern never matches (upstream logs the error and
// treats the pattern as not matching); the returned strings describe them.
func (r *FilteredResource) compile() []string {
	var bad []string
	r.groupGlobs, bad = compileGlobs(r.APIGroups, bad)
	r.clusterGlobs, bad = compileGlobs(r.Clusters, bad)
	return bad
}

func compileGlobs(patterns []string, bad []string) ([]*glob.Pattern, []string) {
	out := make([]*glob.Pattern, len(patterns))
	for i, p := range patterns {
		g, err := glob.Compile(p)
		if err != nil {
			bad = append(bad, fmt.Sprintf("invalid glob pattern %q: %v", p, err))
			continue
		}
		out[i] = g
	}
	return out, bad
}

func matchAny(globs []*glob.Pattern, patterns []string, value string) bool {
	if len(patterns) == 0 {
		return true
	}
	compiled := len(globs) == len(patterns)
	for i, p := range patterns {
		var g *glob.Pattern
		if compiled {
			g = globs[i]
		} else {
			// Entry built without NewFilteredResource: compile on demand.
			g, _ = glob.Compile(p)
		}
		if g != nil && g.Match(value) {
			return true
		}
	}
	return false
}

func (r FilteredResource) matchGroup(group string) bool {
	return matchAny(r.groupGlobs, r.APIGroups, group)
}

func (r FilteredResource) matchKind(kind string) bool {
	if len(r.Kinds) == 0 {
		return true
	}
	for _, k := range r.Kinds {
		if k == "*" || k == kind {
			return true
		}
	}
	return false
}

// MatchCluster reports whether the entry applies to the cluster with the given API server URL.
func (r FilteredResource) MatchCluster(cluster string) bool {
	return matchAny(r.clusterGlobs, r.Clusters, cluster)
}

// Match reports whether the entry matches group, kind and cluster.
func (r FilteredResource) Match(group, kind, cluster string) bool {
	return r.matchGroup(group) && r.matchKind(kind) && r.MatchCluster(cluster)
}

// NewFilteredResource returns an entry with compiled globs.
func NewFilteredResource(apiGroups, kinds, clusters []string) FilteredResource {
	r := FilteredResource{APIGroups: slices.Clone(apiGroups), Kinds: slices.Clone(kinds), Clusters: slices.Clone(clusters)}
	r.compile()
	return r
}

func (r FilteredResource) clone() FilteredResource {
	return NewFilteredResource(r.APIGroups, r.Kinds, r.Clusters)
}

// CoreExcludedResources returns the group/kinds that are excluded on every cluster
// regardless of configuration (upstream coreExcludedResources, DIFF-069): every kind of
// events.k8s.io and metrics.k8s.io, core Event, and coordination.k8s.io Lease.
func CoreExcludedResources() []FilteredResource {
	return []FilteredResource{
		NewFilteredResource([]string{"events.k8s.io", "metrics.k8s.io"}, nil, nil),
		NewFilteredResource([]string{""}, []string{"Event"}, nil),
		NewFilteredResource([]string{"coordination.k8s.io"}, []string{"Lease"}, nil),
	}
}

// DefaultResourceExclusionsYAML is the resource.exclusions value shipped in the install
// manifests' patrol-cm, identical to upstream manifests/base/config/argocd-cm.yaml.
const DefaultResourceExclusionsYAML = `### Network resources created by the Kubernetes control plane and excluded to reduce the number of watched events and UI clutter
- apiGroups:
  - ''
  - discovery.k8s.io
  kinds:
  - Endpoints
  - EndpointSlice
### Internal Kubernetes resources excluded reduce the number of watched events
- apiGroups:
  - coordination.k8s.io
  kinds:
  - Lease
### Internal Kubernetes Authz/Authn resources excluded reduce the number of watched events
- apiGroups:
  - authentication.k8s.io
  - authorization.k8s.io
  kinds:
  - SelfSubjectReview
  - TokenReview
  - LocalSubjectAccessReview
  - SelfSubjectAccessReview
  - SelfSubjectRulesReview
  - SubjectAccessReview
### Intermediate Certificate Request excluded reduce the number of watched events
- apiGroups:
  - certificates.k8s.io
  kinds:
  - CertificateSigningRequest
- apiGroups:
  - cert-manager.io
  kinds:
  - CertificateRequest
### Cilium internal resources excluded reduce the number of watched events and UI Clutter
- apiGroups:
  - cilium.io
  kinds:
  - CiliumIdentity
  - CiliumEndpoint
  - CiliumEndpointSlice
### Kyverno intermediate and reporting resources excluded reduce the number of watched events and improve performance
- apiGroups:
  - kyverno.io
  - reports.kyverno.io
  - wgpolicyk8s.io
  kinds:
  - PolicyReport
  - ClusterPolicyReport
  - EphemeralReport
  - ClusterEphemeralReport
  - AdmissionReport
  - ClusterAdmissionReport
  - BackgroundScanReport
  - ClusterBackgroundScanReport
  - UpdateRequest
`

// DefaultResourceExclusions returns the parsed DefaultResourceExclusionsYAML.
func DefaultResourceExclusions() []FilteredResource {
	out, err := parseFilteredResources(DefaultResourceExclusionsYAML)
	if err != nil {
		// The constant is covered by tests; an empty list keeps the core exclusions.
		return nil
	}
	return out
}

// DefaultIgnoreResourceUpdates returns the resource.customizations.ignoreResourceUpdates.*
// keys shipped in the install manifests' patrol-cm. They are upstream's
// (manifests/base/config/argocd-cm.yaml) except for the Application rule, which targets
// Patrol's group (cd.patrol.example_Application), uses Patrol's notified/refresh/hydrate
// annotation keys and drops ".operation" (Patrol Applications have no operation field). The
// Argo Rollouts rule keeps notified.notifications.argoproj.io, which Argo Rollouts writes.
func DefaultIgnoreResourceUpdates() map[string]string {
	return map[string]string{
		"resource.customizations.ignoreResourceUpdates.all": "jsonPointers:\n  - /status\n",
		"resource.customizations.ignoreResourceUpdates.cd.patrol.example_Application": "jqPathExpressions:\n" +
			"  - '.metadata.annotations.\"notified.notifications.cd.patrol.example\"'\n" +
			"  - '.metadata.annotations.\"cd.patrol.example/refresh\"'\n" +
			"  - '.metadata.annotations.\"cd.patrol.example/hydrate\"'\n",
		"resource.customizations.ignoreResourceUpdates.argoproj.io_Rollout": "jqPathExpressions:\n" +
			"  - '.metadata.annotations.\"notified.notifications.argoproj.io\"'\n",
		"resource.customizations.ignoreResourceUpdates.autoscaling_HorizontalPodAutoscaler": "jqPathExpressions:\n" +
			"  - '.metadata.annotations.\"autoscaling.alpha.kubernetes.io/behavior\"'\n" +
			"  - '.metadata.annotations.\"autoscaling.alpha.kubernetes.io/conditions\"'\n" +
			"  - '.metadata.annotations.\"autoscaling.alpha.kubernetes.io/metrics\"'\n" +
			"  - '.metadata.annotations.\"autoscaling.alpha.kubernetes.io/current-metrics\"'\n",
		"resource.customizations.ignoreResourceUpdates.ConfigMap": "jqPathExpressions:\n" +
			"  - '.metadata.annotations.\"cluster-autoscaler.kubernetes.io/last-updated\"'\n" +
			"  - '.metadata.annotations.\"control-plane.alpha.kubernetes.io/leader\"'\n",
		"resource.customizations.ignoreResourceUpdates.apps_ReplicaSet": "jqPathExpressions:\n" +
			"  - '.metadata.annotations.\"deployment.kubernetes.io/desired-replicas\"'\n" +
			"  - '.metadata.annotations.\"deployment.kubernetes.io/max-replicas\"'\n" +
			"  - '.metadata.annotations.\"rollout.argoproj.io/desired-replicas\"'\n",
		"resource.customizations.ignoreResourceUpdates.discovery.k8s.io_EndpointSlice": "jsonPointers:\n" +
			"  - /metadata\n  - /endpoints\n  - /ports\n",
		"resource.customizations.ignoreResourceUpdates.Endpoints": "jsonPointers:\n  - /metadata\n  - /subsets\n",
	}
}

func parseFilteredResources(v string) ([]FilteredResource, error) {
	out := make([]FilteredResource, 0)
	if err := yaml.Unmarshal([]byte(v), &out); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].compile()
	}
	return out, nil
}

// ResourcesFilter decides which group/kinds are discovered and compared on a cluster. It
// implements cache.ResourceFilter.
type ResourcesFilter struct {
	// ResourceExclusions are resource.exclusions (the core exclusions are always added).
	ResourceExclusions []FilteredResource
	// ResourceInclusions are resource.inclusions.
	ResourceInclusions []FilteredResource
}

var coreExcluded = CoreExcludedResources()

// IsExcluded implements the upstream truth table (resources_filter.go):
//
//	excluded (core list or exclusions)            → excluded
//	matched by an inclusion                       → included
//	some inclusion applies to the cluster          → excluded
//	otherwise                                     → included
func (rf *ResourcesFilter) IsExcluded(group, kind, cluster string) bool {
	for _, r := range coreExcluded {
		if r.Match(group, kind, cluster) {
			return true
		}
	}
	if rf == nil {
		return false
	}
	for _, r := range rf.ResourceExclusions {
		if r.Match(group, kind, cluster) {
			return true
		}
	}
	for _, r := range rf.ResourceInclusions {
		if r.Match(group, kind, cluster) {
			return false
		}
	}
	for _, r := range rf.ResourceInclusions {
		if r.MatchCluster(cluster) {
			return true
		}
	}
	return false
}

// IsExcludedResource is IsExcluded under the upstream name.
func (rf *ResourcesFilter) IsExcludedResource(group, kind, cluster string) bool {
	return rf.IsExcluded(group, kind, cluster)
}

func (rf *ResourcesFilter) clone() *ResourcesFilter {
	out := &ResourcesFilter{}
	for _, r := range rf.ResourceExclusions {
		out.ResourceExclusions = append(out.ResourceExclusions, r.clone())
	}
	for _, r := range rf.ResourceInclusions {
		out.ResourceInclusions = append(out.ResourceInclusions, r.clone())
	}
	return out
}

// compareOptionsDoc is the YAML shape of resource.compareoptions.
type compareOptionsDoc struct {
	IgnoreAggregatedRoles              bool   `json:"ignoreAggregatedRoles,omitempty"`
	IgnoreResourceStatusField          string `json:"ignoreResourceStatusField,omitempty"`
	IgnoreDifferencesOnResourceUpdates *bool  `json:"ignoreDifferencesOnResourceUpdates,omitempty"`
}

func defaultCompareOptions() diff.CompareOptions { return diff.DefaultCompareOptions() }

// overrideGroupKind converts an override key ("group/Kind", "Kind", "*/*") to diff globs.
func overrideGroupKind(key string) (group, kind string) {
	if g, k, ok := strings.Cut(key, "/"); ok {
		return g, k
	}
	return "", key
}

func ignoreRule(key string, d lua.IgnoreDiff) diff.IgnoreRule {
	g, k := overrideGroupKind(key)
	return diff.IgnoreRule{
		Group:                 g,
		Kind:                  k,
		JSONPointers:          slices.Clone(d.JSONPointers),
		JQPathExpressions:     slices.Clone(d.JQPathExpressions),
		ManagedFieldsManagers: slices.Clone(d.ManagedFieldsManagers),
	}
}

// ignoreDifferencesRules converts the system-level ignoreDifferences overrides (sorted by key).
func (s *Settings) ignoreDifferencesRules() []diff.IgnoreRule {
	var out []diff.IgnoreRule
	for _, key := range slices.Sorted(maps.Keys(s.resourceOverrides)) {
		d, err := s.resourceOverrides[key].GetIgnoreDifferences()
		if err != nil || (len(d.JSONPointers) == 0 && len(d.JQPathExpressions) == 0 && len(d.ManagedFieldsManagers) == 0) {
			continue
		}
		out = append(out, ignoreRule(key, d))
	}
	return out
}

// SystemIgnoreRules returns the system-level ignore rules the diff applies after the
// application's own rules: resource.customizations.ignoreDifferences.* plus the /status rule
// selected by ignoreResourceStatusField (DIFF-022..025).
func (s *Settings) SystemIgnoreRules() []diff.IgnoreRule {
	return diff.SystemIgnoreRules(s.ignoreDifferencesRules(), s.compareOptions)
}

// ResourceUpdateIgnoreRules returns the rules deciding whether a live update is relevant
// (OPS-063): ignoreResourceUpdates overrides, plus the ignoreDifferences overrides and the
// ignoreResourceStatusField rule when ignoreDifferencesOnResourceUpdates is set, plus
// resourceVersion/generation/managedFields (composition by diff.ResourceUpdateIgnoreRules).
func (s *Settings) ResourceUpdateIgnoreRules() []diff.IgnoreRule {
	var updates []diff.IgnoreRule
	for _, key := range slices.Sorted(maps.Keys(s.resourceOverrides)) {
		d, err := s.resourceOverrides[key].GetIgnoreResourceUpdates()
		if err != nil || (len(d.JSONPointers) == 0 && len(d.JQPathExpressions) == 0 && len(d.ManagedFieldsManagers) == 0) {
			continue
		}
		updates = append(updates, ignoreRule(key, d))
	}
	return diff.ResourceUpdateIgnoreRules(updates, s.ignoreDifferencesRules(), s.compareOptions)
}

// KnownTypeFields returns resource.customizations.knownTypeFields.* keyed by group/kind.
// Wildcard keys are skipped (upstream applies knownTypeFields per exact group/kind).
func (s *Settings) KnownTypeFields() map[schema.GroupKind][]diff.KnownTypeField {
	out := map[schema.GroupKind][]diff.KnownTypeField{}
	for key, o := range s.resourceOverrides {
		if len(o.KnownTypeFields) == 0 || strings.Contains(key, "*") {
			continue
		}
		g, k := overrideGroupKind(key)
		fields := make([]diff.KnownTypeField, 0, len(o.KnownTypeFields))
		for _, f := range o.KnownTypeFields {
			fields = append(fields, diff.KnownTypeField{Field: f.Field, Type: f.Type})
		}
		out[schema.GroupKind{Group: g, Kind: k}] = fields
	}
	return out
}
