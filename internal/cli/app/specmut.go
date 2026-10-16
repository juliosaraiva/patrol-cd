package app

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Helpers that edit Application specs the way the upstream CLI does (v1alpha1 helper methods
// of Argo CD re-implemented on Patrol's types).

// sourceByPosition returns the source at the 1-based position (upstream
// GetSourcePtrByPosition): the hydrator's dry source for position 0, its sync source
// otherwise; the source at position-1 (or the first) of a multi-source Application; the
// single source. For hydrator Applications the returned source is a detached copy (edits are
// lost): editors use drySourceView/storeDrySource instead.
func sourceByPosition(spec *v1alpha1.ApplicationSpec, position int) *v1alpha1.ApplicationSource {
	index := position - 1
	if spec.SourceHydrator != nil {
		if index < 0 {
			s := spec.SourceHydrator.GetDrySource()
			return &s
		}
		s := spec.SourceHydrator.GetSyncSource()
		return &s
	}
	if spec.HasMultipleSources() {
		if index > 0 && index < len(spec.Sources) {
			return &spec.Sources[index]
		}
		return &spec.Sources[0]
	}
	return spec.Source
}

// drySourceView returns the dry source of h as a detached ApplicationSource (no pointer is
// shared with h), for editing with the plain source helpers; storeDrySource writes its tool
// options back. A hydrator's locations are not edited through a view.
func drySourceView(h *v1alpha1.SourceHydrator) v1alpha1.ApplicationSource {
	return h.DeepCopy().GetDrySource()
}

// storeDrySource stores the tool options of a view returned by drySourceView in h.
func storeDrySource(h *v1alpha1.SourceHydrator, view *v1alpha1.ApplicationSource) {
	h.DrySource.Helm = view.Helm
	h.DrySource.Kustomize = view.Kustomize
	h.DrySource.Directory = view.Directory
	h.DrySource.Plugin = view.Plugin
}

// firstSource returns the first source (or an empty one).
func firstSource(spec *v1alpha1.ApplicationSpec) v1alpha1.ApplicationSource {
	srcs := spec.GetSources()
	if len(srcs) == 0 {
		return v1alpha1.ApplicationSource{}
	}
	return srcs[0]
}

// isRefSource reports whether the source only provides files to other sources.
func isRefSource(s *v1alpha1.ApplicationSource) bool {
	return s.Ref != "" && s.Path == "" && s.Chart == ""
}

// explicitType returns the source type implied by a tool block, or "" when none or several.
func explicitType(s *v1alpha1.ApplicationSource) (v1alpha1.ApplicationSourceType, error) {
	var types []v1alpha1.ApplicationSourceType
	if s.Helm != nil {
		types = append(types, v1alpha1.ApplicationSourceTypeHelm)
	}
	if s.Kustomize != nil {
		types = append(types, v1alpha1.ApplicationSourceTypeKustomize)
	}
	if s.Directory != nil {
		types = append(types, v1alpha1.ApplicationSourceTypeDirectory)
	}
	if s.Plugin != nil {
		types = append(types, v1alpha1.ApplicationSourceTypePlugin)
	}
	switch len(types) {
	case 0:
		return "", nil
	case 1:
		return types[0], nil
	}
	return "", fmt.Errorf("multiple application sources defined: %v", types)
}

// newHelmParameter parses "name=value".
func newHelmParameter(text string, forceString bool) (v1alpha1.HelmParameter, error) {
	name, value, ok := strings.Cut(text, "=")
	if !ok {
		return v1alpha1.HelmParameter{}, fmt.Errorf("expected helm parameter of the form: param=value. Received: %s", text)
	}
	return v1alpha1.HelmParameter{Name: name, Value: value, ForceString: forceString}, nil
}

// addHelmParameter replaces the parameter of the same name or appends p.
func addHelmParameter(h *v1alpha1.ApplicationSourceHelm, p v1alpha1.HelmParameter) {
	for i := range h.Parameters {
		if h.Parameters[i].Name == p.Name {
			h.Parameters[i] = p
			return
		}
	}
	h.Parameters = append(h.Parameters, p)
}

// newHelmFileParameter parses "name=path".
func newHelmFileParameter(text string) (v1alpha1.HelmFileParameter, error) {
	name, path, ok := strings.Cut(text, "=")
	if !ok {
		return v1alpha1.HelmFileParameter{}, fmt.Errorf("expected helm file parameter of the form: param=path. Received: %s", text)
	}
	return v1alpha1.HelmFileParameter{Name: name, Path: path}, nil
}

func addHelmFileParameter(h *v1alpha1.ApplicationSourceHelm, p v1alpha1.HelmFileParameter) {
	for i := range h.FileParameters {
		if h.FileParameters[i].Name == p.Name {
			h.FileParameters[i] = p
			return
		}
	}
	h.FileParameters = append(h.FileParameters, p)
}

// setHelmValues stores a literal values block as valuesObject (upstream SetValuesString):
// "" clears both forms; the YAML must be an object (or a string).
func setHelmValues(h *v1alpha1.ApplicationSourceHelm, values string) error {
	if values == "" {
		h.ValuesObject = nil
		h.Values = ""
		return nil
	}
	data, err := yaml.YAMLToJSON([]byte(values))
	if err != nil {
		return fmt.Errorf("failed converting yaml to json: %w", err)
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("failed to unmarshal json: %w", err)
	}
	switch v.(type) {
	case string, map[string]any:
	default:
		return fmt.Errorf("invalid type %T for helm values: expected an object", v)
	}
	h.ValuesObject = &runtime.RawExtension{Raw: data}
	h.Values = ""
	return nil
}

// helmValuesEmpty reports whether neither values form holds content.
func helmValuesEmpty(h *v1alpha1.ApplicationSourceHelm) bool {
	if h.Values != "" {
		return false
	}
	if h.ValuesObject == nil {
		return true
	}
	raw := strings.TrimSpace(string(h.ValuesObject.Raw))
	return raw == "" || raw == "null" || raw == "{}"
}

func helmIsZero(h *v1alpha1.ApplicationSourceHelm) bool {
	return h == nil || h.Version == "" && h.ReleaseName == "" && len(h.ValueFiles) == 0 &&
		len(h.Parameters) == 0 && len(h.FileParameters) == 0 && helmValuesEmpty(h) &&
		!h.PassCredentials && !h.IgnoreMissingValueFiles && !h.SkipCrds && !h.SkipTests &&
		!h.SkipSchemaValidation && h.KubeVersion == "" && len(h.APIVersions) == 0 && h.Namespace == ""
}

func kustomizeIsZero(k *v1alpha1.ApplicationSourceKustomize) bool {
	return k == nil || k.NamePrefix == "" && k.NameSuffix == "" && k.Version == "" &&
		k.Namespace == "" && len(k.Images) == 0 && len(k.Replicas) == 0 &&
		len(k.CommonLabels) == 0 && len(k.CommonAnnotations) == 0 && len(k.Patches) == 0 &&
		len(k.Components) == 0 && k.KubeVersion == "" && len(k.APIVersions) == 0 &&
		!k.IgnoreMissingComponents && !k.ForceCommonLabels && !k.ForceCommonAnnotations &&
		!k.LabelWithoutSelector && !k.LabelIncludeTemplates && !k.CommonAnnotationsEnvsubst
}

// imageName returns the image name part of a Kustomize image override (before the first
// "=", ":" or "@", in that order of preference; upstream KustomizeImage.delim).
func imageName(image string) string {
	for _, d := range []string{"=", ":", "@"} {
		if strings.Contains(image, d) {
			name, _, _ := strings.Cut(image, d)
			return name
		}
	}
	return image
}

// mergeImage replaces the override of the same image or appends image.
func mergeImage(k *v1alpha1.ApplicationSourceKustomize, image string) {
	for i, existing := range k.Images {
		if imageName(string(existing)) == imageName(image) {
			k.Images[i] = v1alpha1.KustomizeImage(image)
			return
		}
	}
	k.Images = append(k.Images, v1alpha1.KustomizeImage(image))
}

// newKustomizeReplica parses "name=count".
func newKustomizeReplica(text string) (v1alpha1.KustomizeReplica, error) {
	name, count, ok := strings.Cut(text, "=")
	if !ok || name == "" {
		return v1alpha1.KustomizeReplica{}, fmt.Errorf("expected parameter of the form: name=count. Received: %s", text)
	}
	n, err := strconv.Atoi(count)
	if err != nil {
		return v1alpha1.KustomizeReplica{}, fmt.Errorf("expected integer value for count. Received: %s", count)
	}
	return v1alpha1.KustomizeReplica{Name: name, Count: intstr.FromInt(n)}, nil
}

func mergeReplica(k *v1alpha1.ApplicationSourceKustomize, r v1alpha1.KustomizeReplica) {
	for i := range k.Replicas {
		if k.Replicas[i].Name == r.Name {
			k.Replicas[i] = r
			return
		}
	}
	k.Replicas = append(k.Replicas, r)
}

// newEnvEntry parses "NAME=VALUE".
func newEnvEntry(text string) (v1alpha1.EnvEntry, error) {
	name, value, ok := strings.Cut(text, "=")
	if !ok {
		return v1alpha1.EnvEntry{}, fmt.Errorf("expected env entry of the form: param=value. Received: %s", text)
	}
	return v1alpha1.EnvEntry{Name: name, Value: value}, nil
}

func addEnvEntry(p *v1alpha1.ApplicationSourcePlugin, e v1alpha1.EnvEntry) {
	for i := range p.Env {
		if p.Env[i].Name == e.Name {
			p.Env[i] = e
			return
		}
	}
	p.Env = append(p.Env, e)
}

// removeEnvEntry removes the variable named key.
func removeEnvEntry(p *v1alpha1.ApplicationSourcePlugin, key string) error {
	i := slices.IndexFunc(p.Env, func(e v1alpha1.EnvEntry) bool { return e.Name == key })
	if i < 0 {
		return fmt.Errorf("unable to find env variable with key %q for plugin %q", key, p.Name)
	}
	p.Env = slices.Delete(p.Env, i, i+1)
	return nil
}

// newJsonnetVar parses "name=value" (a bare name has an empty value).
func newJsonnetVar(s string, code bool) v1alpha1.JsonnetVar {
	if name, value, ok := strings.Cut(s, "="); ok {
		return v1alpha1.JsonnetVar{Name: name, Value: value, Code: code}
	}
	return v1alpha1.JsonnetVar{Name: s, Code: code}
}

// syncPolicyIsZero reports an empty sync policy (normalized to nil).
func syncPolicyIsZero(p *v1alpha1.SyncPolicy) bool {
	return p == nil || p.Automated == nil && len(p.SyncOptions) == 0 && p.Retry == nil && p.ManagedNamespaceMetadata == nil
}

// parseKeyValues parses "key=value" items (upstream label.Parse: exactly one "=").
func parseKeyValues(items []string) (map[string]string, error) {
	if items == nil {
		return nil, nil
	}
	out := make(map[string]string, len(items))
	for _, it := range items {
		fields := strings.Split(it, "=")
		if len(fields) != 2 {
			return nil, fmt.Errorf("labels should have key=value, but instead got: %s", it)
		}
		out[fields[0]] = fields[1]
	}
	return out, nil
}
