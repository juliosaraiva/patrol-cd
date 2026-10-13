package v1alpha1

import (
	"encoding/json"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// ApplicationSource is the location of an application's manifests or chart and the options used to
// render them.
type ApplicationSource struct {
	// RepoURL is the URL of the repository (Git, Helm or OCI) that contains the application manifests.
	RepoURL string `json:"repoURL"`
	// Path is a directory path within the repository. Only valid for Git and OCI sources.
	// +optional
	Path string `json:"path,omitempty"`
	// TargetRevision is the revision to sync to: a commit, tag or branch for Git (HEAD if omitted), a
	// semver constraint or version for Helm charts.
	// +optional
	TargetRevision string `json:"targetRevision,omitempty"`
	// Helm holds Helm-specific options.
	// +optional
	Helm *ApplicationSourceHelm `json:"helm,omitempty"`
	// Kustomize holds Kustomize-specific options.
	// +optional
	Kustomize *ApplicationSourceKustomize `json:"kustomize,omitempty"`
	// Directory holds options for plain YAML/JSON or Jsonnet directories.
	// +optional
	Directory *ApplicationSourceDirectory `json:"directory,omitempty"`
	// Plugin holds Config Management Plugin options.
	// +optional
	Plugin *ApplicationSourcePlugin `json:"plugin,omitempty"`
	// Chart is the Helm chart name; required for sources in a Helm repository.
	// +optional
	Chart string `json:"chart,omitempty"`
	// Ref names this source so other sources of a multi-source Application can reference its files
	// as "$<ref>/<path>". Ignored for single-source Applications.
	// +optional
	Ref string `json:"ref,omitempty"`
	// Name identifies the source in the UI and CLI of multi-source Applications.
	// +optional
	Name string `json:"name,omitempty"`
	// TagPrefix restricts git tags to those with this prefix before targetRevision is evaluated as a
	// semver constraint. The prefix is stripped before comparison and re-added to the resolved tag.
	// +optional
	TagPrefix string `json:"tagPrefix,omitempty"`
}

// ApplicationSources is an ordered list of sources.
type ApplicationSources []ApplicationSource

// ApplicationSourceType is the detected rendering tool of a source.
type ApplicationSourceType string

const (
	ApplicationSourceTypeHelm      ApplicationSourceType = "Helm"
	ApplicationSourceTypeKustomize ApplicationSourceType = "Kustomize"
	ApplicationSourceTypeDirectory ApplicationSourceType = "Directory"
	ApplicationSourceTypePlugin    ApplicationSourceType = "Plugin"
)

// ApplicationSourceHelm holds Helm-specific options.
type ApplicationSourceHelm struct {
	// ValueFiles is a list of Helm value files to use when generating a template.
	// +optional
	ValueFiles []string `json:"valueFiles,omitempty"`
	// Parameters are passed to helm template as --set values.
	// +optional
	Parameters []HelmParameter `json:"parameters,omitempty"`
	// ReleaseName is the Helm release name. Defaults to the Application name.
	// +optional
	ReleaseName string `json:"releaseName,omitempty"`
	// Values is a YAML block of Helm values. ValuesObject takes precedence when both are set.
	// +optional
	Values string `json:"values,omitempty"`
	// FileParameters are passed to helm template as --set-file values.
	// +optional
	FileParameters []HelmFileParameter `json:"fileParameters,omitempty"`
	// Version is the Helm major version to use for templating ("3").
	// +optional
	Version string `json:"version,omitempty"`
	// PassCredentials passes credentials to all domains (helm --pass-credentials).
	// +optional
	PassCredentials bool `json:"passCredentials,omitempty"`
	// IgnoreMissingValueFiles skips value files that do not exist instead of failing.
	// +optional
	IgnoreMissingValueFiles bool `json:"ignoreMissingValueFiles,omitempty"`
	// SkipCrds skips installing CRDs from the chart (helm --skip-crds).
	// +optional
	SkipCrds bool `json:"skipCrds,omitempty"`
	// ValuesObject holds Helm values as a structured object. Takes precedence over Values.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	ValuesObject *runtime.RawExtension `json:"valuesObject,omitempty"`
	// Namespace is the namespace to template with. Defaults to the destination namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// KubeVersion is the Kubernetes version passed to Helm. Defaults to the destination's version.
	// +optional
	KubeVersion string `json:"kubeVersion,omitempty"`
	// APIVersions are the API versions passed to Helm ([group/]version/kind). Defaults to the
	// destination's API versions.
	// +optional
	APIVersions []string `json:"apiVersions,omitempty"`
	// SkipTests skips test manifests (helm --skip-tests).
	// +optional
	SkipTests bool `json:"skipTests,omitempty"`
	// SkipSchemaValidation skips JSON schema validation (helm --skip-schema-validation).
	// +optional
	SkipSchemaValidation bool `json:"skipSchemaValidation,omitempty"`
}

// HelmParameter is a Helm --set parameter.
type HelmParameter struct {
	// Name of the parameter.
	// +optional
	Name string `json:"name,omitempty"`
	// Value of the parameter.
	// +optional
	Value string `json:"value,omitempty"`
	// ForceString passes the value with --set-string.
	// +optional
	ForceString bool `json:"forceString,omitempty"`
}

// HelmFileParameter is a Helm --set-file parameter.
type HelmFileParameter struct {
	// Name of the parameter.
	// +optional
	Name string `json:"name,omitempty"`
	// Path of the file holding the value, relative to the source path.
	// +optional
	Path string `json:"path,omitempty"`
}

// KustomizeImage is an image override in the form [old_image_name=]<image_name>:<image_tag>.
type KustomizeImage string

// KustomizeImages is a list of image overrides.
type KustomizeImages []KustomizeImage

// ApplicationSourceKustomize holds Kustomize-specific options.
type ApplicationSourceKustomize struct {
	// NamePrefix overrides the kustomization namePrefix.
	// +optional
	NamePrefix string `json:"namePrefix,omitempty"`
	// NameSuffix overrides the kustomization nameSuffix.
	// +optional
	NameSuffix string `json:"nameSuffix,omitempty"`
	// Images are image overrides.
	// +optional
	Images KustomizeImages `json:"images,omitempty"`
	// CommonLabels are added to all rendered resources.
	// +optional
	CommonLabels map[string]string `json:"commonLabels,omitempty"`
	// Version selects a configured Kustomize version.
	// +optional
	Version string `json:"version,omitempty"`
	// CommonAnnotations are added to all rendered resources.
	// +optional
	CommonAnnotations map[string]string `json:"commonAnnotations,omitempty"`
	// ForceCommonLabels overwrites existing labels with CommonLabels.
	// +optional
	ForceCommonLabels bool `json:"forceCommonLabels,omitempty"`
	// ForceCommonAnnotations overwrites existing annotations with CommonAnnotations.
	// +optional
	ForceCommonAnnotations bool `json:"forceCommonAnnotations,omitempty"`
	// Namespace sets the namespace of all rendered resources.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// CommonAnnotationsEnvsubst substitutes environment variables in CommonAnnotations values.
	// +optional
	CommonAnnotationsEnvsubst bool `json:"commonAnnotationsEnvsubst,omitempty"`
	// Replicas are replica count overrides.
	// +optional
	Replicas KustomizeReplicas `json:"replicas,omitempty"`
	// Patches are Kustomize patches added to the kustomization.
	// +optional
	Patches KustomizePatches `json:"patches,omitempty"`
	// Components are Kustomize components added to the kustomization.
	// +optional
	Components []string `json:"components,omitempty"`
	// IgnoreMissingComponents skips components that do not exist instead of failing.
	// +optional
	IgnoreMissingComponents bool `json:"ignoreMissingComponents,omitempty"`
	// LabelWithoutSelector applies CommonLabels without adding them to selectors.
	// +optional
	LabelWithoutSelector bool `json:"labelWithoutSelector,omitempty"`
	// KubeVersion is the Kubernetes version passed to Helm charts inflated by Kustomize.
	// +optional
	KubeVersion string `json:"kubeVersion,omitempty"`
	// APIVersions are the API versions passed to Helm charts inflated by Kustomize.
	// +optional
	APIVersions []string `json:"apiVersions,omitempty"`
	// LabelIncludeTemplates applies CommonLabels to resource templates (requires LabelWithoutSelector).
	// +optional
	LabelIncludeTemplates bool `json:"labelIncludeTemplates,omitempty"`
}

// KustomizeReplica overrides the replica count of a Deployment or StatefulSet.
type KustomizeReplica struct {
	// Name of the Deployment or StatefulSet.
	Name string `json:"name"`
	// Count is the number of replicas.
	Count intstr.IntOrString `json:"count"`
}

// KustomizeReplicas is a list of replica overrides.
type KustomizeReplicas []KustomizeReplica

// KustomizePatches is a list of Kustomize patches.
type KustomizePatches []KustomizePatch

// KustomizePatch is a Kustomize patch (strategic merge or JSON 6902).
type KustomizePatch struct {
	// Path of a patch file, relative to the source path.
	// +optional
	Path string `json:"path,omitempty"`
	// Patch is an inline patch.
	// +optional
	Patch string `json:"patch,omitempty"`
	// Target selects the resources to patch.
	// +optional
	Target *KustomizeSelector `json:"target,omitempty"`
	// Options are Kustomize patch options (allowNameChange, allowKindChange).
	// +optional
	Options map[string]bool `json:"options,omitempty"`
}

// KustomizeSelector selects resources for a Kustomize patch.
type KustomizeSelector struct {
	KustomizeResID `json:",inline"`
	// AnnotationSelector selects resources by annotation.
	// +optional
	AnnotationSelector string `json:"annotationSelector,omitempty"`
	// LabelSelector selects resources by label.
	// +optional
	LabelSelector string `json:"labelSelector,omitempty"`
}

// KustomizeResID identifies resources by group, version, kind, name and namespace.
type KustomizeResID struct {
	KustomizeGvk `json:",inline"`
	// Name of the resource.
	// +optional
	Name string `json:"name,omitempty"`
	// Namespace of the resource.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// KustomizeGvk is a group, version and kind.
type KustomizeGvk struct {
	// +optional
	Group string `json:"group,omitempty"`
	// +optional
	Version string `json:"version,omitempty"`
	// +optional
	Kind string `json:"kind,omitempty"`
}

// ApplicationSourceDirectory holds options for plain YAML/JSON or Jsonnet directories.
type ApplicationSourceDirectory struct {
	// Recurse scans subdirectories for manifests.
	// +optional
	Recurse bool `json:"recurse,omitempty"`
	// Jsonnet holds Jsonnet-specific options.
	// +optional
	Jsonnet ApplicationSourceJsonnet `json:"jsonnet,omitempty,omitzero"`
	// Exclude is a glob of paths excluded from manifest generation.
	// +optional
	Exclude string `json:"exclude,omitempty"`
	// Include is a glob of paths included in manifest generation.
	// +optional
	Include string `json:"include,omitempty"`
}

// ApplicationSourceJsonnet holds Jsonnet-specific options.
type ApplicationSourceJsonnet struct {
	// ExtVars are Jsonnet external variables.
	// +optional
	ExtVars []JsonnetVar `json:"extVars,omitempty"`
	// TLAs are Jsonnet top-level arguments.
	// +optional
	TLAs []JsonnetVar `json:"tlas,omitempty"`
	// Libs are additional library search directories.
	// +optional
	Libs []string `json:"libs,omitempty"`
}

// JsonnetVar is a Jsonnet external variable or top-level argument.
type JsonnetVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	// Code evaluates Value as Jsonnet code instead of a string.
	// +optional
	Code bool `json:"code,omitempty"`
}

// ApplicationSourcePlugin holds Config Management Plugin options.
type ApplicationSourcePlugin struct {
	// Name of the plugin. If omitted the plugin is selected by discovery.
	// +optional
	Name string `json:"name,omitempty"`
	// Env are environment variables passed to the plugin.
	// +optional
	Env Env `json:"env,omitempty"`
	// Parameters are passed to the plugin.
	// +optional
	Parameters ApplicationSourcePluginParameters `json:"parameters,omitempty"`
}

// EnvEntry is an environment variable passed to a plugin.
type EnvEntry struct {
	// Name of the variable, usually upper case.
	Name string `json:"name"`
	// Value of the variable.
	Value string `json:"value"`
}

// Env is a list of environment variables.
type Env []EnvEntry

// ApplicationSourcePluginParameters is a list of plugin parameters.
type ApplicationSourcePluginParameters []ApplicationSourcePluginParameter

// ApplicationSourcePluginParameter is a plugin parameter of string, map or array type. Map and array
// are wrapped in pointers so "present but empty" can be distinguished from "absent".
type ApplicationSourcePluginParameter struct {
	// Name of the parameter.
	// +optional
	Name string `json:"name,omitempty"`
	// String is the value of a string parameter.
	// +optional
	String *string `json:"string,omitempty"`
	// OptionalMap holds the value of a map parameter.
	*OptionalMap `json:",omitempty"`
	// OptionalArray holds the value of an array parameter.
	*OptionalArray `json:",omitempty"`
}

// OptionalMap wraps the value of a map plugin parameter.
type OptionalMap struct {
	// Map is the value of a map parameter.
	// +optional
	Map map[string]string `json:"map"`
}

// OptionalArray wraps the value of an array plugin parameter.
type OptionalArray struct {
	// Array is the value of an array parameter.
	// +optional
	Array []string `json:"array"`
}

// MarshalJSON emits a present-but-empty map or array as {} or [] instead of null, which the API
// server would prune, so the "present" signal survives a round trip.
func (p ApplicationSourcePluginParameter) MarshalJSON() ([]byte, error) {
	out := map[string]any{"name": p.Name}
	if p.String != nil {
		out["string"] = *p.String
	}
	if p.OptionalMap != nil {
		if p.Map == nil {
			out["map"] = map[string]string{}
		} else {
			out["map"] = p.Map
		}
	}
	if p.OptionalArray != nil {
		if p.Array == nil {
			out["array"] = []string{}
		} else {
			out["array"] = p.Array
		}
	}
	return json.Marshal(out)
}
