package v1alpha1

import (
	"encoding/json"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GetSources returns the sources the Application syncs from, normalizing the three source forms:
// the hydrated sync source when SourceHydrator is set, Sources for multi-source Applications, or a
// one-element list holding Source. It returns an empty list when no source is set.
func (s *ApplicationSpec) GetSources() ApplicationSources {
	switch {
	case s.SourceHydrator != nil:
		return ApplicationSources{s.SourceHydrator.GetSyncSource()}
	case s.HasMultipleSources():
		return s.Sources
	case s.Source != nil:
		return ApplicationSources{*s.Source}
	default:
		return ApplicationSources{}
	}
}

// HasMultipleSources reports whether the Application uses spec.sources.
func (s *ApplicationSpec) HasMultipleSources() bool {
	return s.SourceHydrator == nil && len(s.Sources) > 0
}

// GetProject returns the project name, defaulting to DefaultAppProjectName.
func (s *ApplicationSpec) GetProject() string {
	if s.Project == "" {
		return DefaultAppProjectName
	}
	return s.Project
}

// IsAutomatedSyncEnabled reports whether automated sync is configured and not disabled.
func (s *ApplicationSpec) IsAutomatedSyncEnabled() bool {
	return s.SyncPolicy.IsAutomatedSyncEnabled()
}

// IsAutomatedSyncEnabled reports whether automated sync is configured and not disabled through
// automated.enabled=false. It is safe to call on a nil policy.
func (p *SyncPolicy) IsAutomatedSyncEnabled() bool {
	return p != nil && p.Automated != nil && (p.Automated.Enabled == nil || *p.Automated.Enabled)
}

// GetPrune returns automated.prune, defaulting to false.
func (a *SyncPolicyAutomated) GetPrune() bool { return a != nil && a.Prune != nil && *a.Prune }

// GetSelfHeal returns automated.selfHeal, defaulting to false.
func (a *SyncPolicyAutomated) GetSelfHeal() bool { return a != nil && a.SelfHeal != nil && *a.SelfHeal }

// GetAllowEmpty returns automated.allowEmpty, defaulting to false.
func (a *SyncPolicyAutomated) GetAllowEmpty() bool {
	return a != nil && a.AllowEmpty != nil && *a.AllowEmpty
}

// GetSyncSource returns the source hydrated manifests are synced from. RepoURL defaults to the dry
// source's repository.
func (h SourceHydrator) GetSyncSource() ApplicationSource {
	repoURL := h.SyncSource.RepoURL
	if repoURL == "" {
		repoURL = h.DrySource.RepoURL
	}
	return ApplicationSource{
		RepoURL:        repoURL,
		Path:           h.SyncSource.Path,
		TargetRevision: h.SyncSource.TargetBranch,
	}
}

// GetDrySource returns the dry source as an ApplicationSource.
func (h SourceHydrator) GetDrySource() ApplicationSource {
	return ApplicationSource{
		RepoURL:        h.DrySource.RepoURL,
		Path:           h.DrySource.Path,
		TargetRevision: h.DrySource.TargetRevision,
		Helm:           h.DrySource.Helm,
		Kustomize:      h.DrySource.Kustomize,
		Directory:      h.DrySource.Directory,
		Plugin:         h.DrySource.Plugin,
	}
}

// Equals reports whether two destinations are identical. A destination given by Name and one given
// by Server are not considered equal; resolve names to servers before comparing.
func (d ApplicationDestination) Equals(other ApplicationDestination) bool {
	return d.Server == other.Server && d.Name == other.Name && d.Namespace == other.Namespace
}

// GetRevisions returns the compared revisions in source order (one entry for single-source).
func (s *ApplicationStatus) GetRevisions() []string {
	if len(s.Sync.Revisions) > 0 {
		return s.Sync.Revisions
	}
	if s.Sync.Revision != "" {
		return []string{s.Sync.Revision}
	}
	return []string{}
}

// FindResource returns the status of the resource with the given key.
func (s *ApplicationStatus) FindResource(key ResourceKey) (*ResourceStatus, bool) {
	for i := range s.Resources {
		if s.Resources[i].Key() == key {
			return &s.Resources[i], true
		}
	}
	return nil, false
}

// IsRefreshRequested returns the requested refresh type from the refresh annotation. Unknown values
// are treated as a normal refresh.
func (a *Application) IsRefreshRequested() (RefreshType, bool) {
	v, ok := a.Annotations[AnnotationKeyRefresh]
	if !ok {
		return "", false
	}
	if RefreshType(v) == RefreshTypeHard {
		return RefreshTypeHard, true
	}
	return RefreshTypeNormal, true
}

// QualifiedName returns "<namespace>/<name>".
func (a *Application) QualifiedName() string {
	if a.Namespace == "" {
		return a.Name
	}
	return a.Namespace + "/" + a.Name
}

// ResourceKey identifies a Kubernetes resource independently of its API version.
type ResourceKey struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

// String returns "group/kind/namespace/name".
func (k ResourceKey) String() string {
	return fmt.Sprintf("%s/%s/%s/%s", k.Group, k.Kind, k.Namespace, k.Name)
}

// Key returns the resource key of the status entry.
func (r ResourceStatus) Key() ResourceKey {
	return ResourceKey{Group: r.Group, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name}
}

// GroupVersionKind returns the GVK of the status entry.
func (r ResourceStatus) GroupVersionKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: r.Group, Version: r.Version, Kind: r.Kind}
}

// Key returns the resource key of the result.
func (r ResourceResult) Key() ResourceKey {
	return ResourceKey{Group: r.Group, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name}
}

// GroupVersionKind returns the GVK of the result.
func (r ResourceResult) GroupVersionKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: r.Group, Version: r.Version, Kind: r.Kind}
}

// IsTerminal reports whether the phase is final: Succeeded, Failed, Error, Cancelled or Rejected.
func (p OperationPhase) IsTerminal() bool {
	switch p {
	case OperationSucceeded, OperationFailed, OperationError, OperationCancelled, OperationRejected:
		return true
	default:
		return false
	}
}

// IsSuccessful reports whether the phase is Succeeded.
func (p OperationPhase) IsSuccessful() bool { return p == OperationSucceeded }

// IsTerminal reports whether the Operation reached a terminal phase.
func (o *Operation) IsTerminal() bool { return o.Status.Phase.IsTerminal() }

// IsCancelRequested reports whether cancellation was requested.
func (o *Operation) IsCancelRequested() bool { return o.Spec.Cancel }

// IsDryRun reports whether the Operation is a dry-run sync.
func (o *Operation) IsDryRun() bool { return o.Spec.Sync != nil && o.Spec.Sync.DryRun }

// IsWarn reports whether orphaned-resource warnings are enabled.
func (s *OrphanedResourcesMonitorSettings) IsWarn() bool {
	return s != nil && s.Warn != nil && *s.Warn
}

// ToNestedMatrixGenerator decodes the raw JSON of a matrix generator nested inside a matrix or
// merge generator. It returns nil for a nil input, and an error when the JSON is malformed or does
// not hold exactly two generators (the count the CRD schema enforces for MatrixGenerator).
func ToNestedMatrixGenerator(j *apiextensionsv1.JSON) (*NestedMatrixGenerator, error) {
	if j == nil {
		return nil, nil
	}
	var g NestedMatrixGenerator
	if err := json.Unmarshal(j.Raw, &g); err != nil {
		return nil, fmt.Errorf("decode nested matrix generator: %w", err)
	}
	if len(g.Generators) != 2 {
		return nil, fmt.Errorf("nested matrix generator has %d generators, want exactly 2", len(g.Generators))
	}
	return &g, nil
}

// ToNestedMergeGenerator decodes the raw JSON of a merge generator nested inside a matrix or merge
// generator. It returns nil for a nil input, and an error when the JSON is malformed or holds fewer
// than two generators (the minimum the CRD schema enforces for MergeGenerator).
func ToNestedMergeGenerator(j *apiextensionsv1.JSON) (*NestedMergeGenerator, error) {
	if j == nil {
		return nil, nil
	}
	var g NestedMergeGenerator
	if err := json.Unmarshal(j.Raw, &g); err != nil {
		return nil, fmt.Errorf("decode nested merge generator: %w", err)
	}
	if len(g.Generators) < 2 {
		return nil, fmt.Errorf("nested merge generator has %d generators, want at least 2", len(g.Generators))
	}
	return &g, nil
}

// AllowUpdate reports whether the policy allows updating generated Applications.
func (p ApplicationsSyncPolicy) AllowUpdate() bool {
	return p == "" || p == ApplicationsSyncPolicyCreateUpdate || p == ApplicationsSyncPolicySync
}

// AllowDelete reports whether the policy allows deleting generated Applications.
func (p ApplicationsSyncPolicy) AllowDelete() bool {
	return p == "" || p == ApplicationsSyncPolicyCreateDelete || p == ApplicationsSyncPolicySync
}
