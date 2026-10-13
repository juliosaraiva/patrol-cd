package cache

import (
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// newResource converts a freshly decoded object into a Resource. It may mutate un (stripping
// managedFields and the last-applied annotation when the manifest is retained), so un must
// be owned by the caller and not used afterwards.
func newResource(un *unstructured.Unstructured, s *Settings) *Resource {
	refs, inferred := resolveOwnerRefs(un)
	gvk := un.GroupVersionKind()
	r := &Resource{
		Ref: ResourceRef{
			ResourceKey: ResourceKey{Group: gvk.Group, Kind: gvk.Kind, Namespace: un.GetNamespace(), Name: un.GetName()},
			UID:         un.GetUID(),
			APIVersion:  un.GetAPIVersion(),
		},
		OwnerRefs:        refs,
		ResourceVersion:  un.GetResourceVersion(),
		Labels:           pick(un.GetLabels(), s.TrackingLabelKeys),
		Annotations:      pick(un.GetAnnotations(), s.TrackingAnnotationKeys),
		inferredChildren: inferred,
	}
	if ct := un.GetCreationTimestamp(); !ct.IsZero() {
		r.CreationTimestamp = &ct
	}
	cacheManifest := false
	if s.PopulateResourceInfo != nil {
		r.Info, cacheManifest = s.PopulateResourceInfo(un, len(refs) == 0)
	}
	if !cacheManifest && s.IsManaged != nil {
		cacheManifest = s.IsManaged(un)
	}
	// CRDs carry no tracking metadata but are diffed (upstream caches every CRD manifest).
	cacheManifest = cacheManifest || (gvk.Group == groupAPIExtensions && gvk.Kind == kindCRD)
	if cacheManifest {
		if !s.RetainManagedFields {
			un.SetManagedFields(nil)
		}
		if ann := un.GetAnnotations(); ann != nil {
			if v, ok := ann[AnnotationLastApplied]; ok {
				r.LastAppliedConfiguration = v
				delete(ann, AnnotationLastApplied)
				if len(ann) == 0 {
					ann = nil
				}
				un.SetAnnotations(ann)
			}
		}
		r.Resource = un
	}
	return r
}

func pick(m map[string]string, keys []string) map[string]string {
	if len(m) == 0 || len(keys) == 0 {
		return nil
	}
	var out map[string]string
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if out == nil {
				out = make(map[string]string, len(keys))
			}
			out[k] = v
		}
	}
	return out
}

// liveCopy returns a deep copy of the retained manifest with the last-applied annotation
// restored.
func (r *Resource) liveCopy() *unstructured.Unstructured {
	u := r.Resource.DeepCopy()
	if r.LastAppliedConfiguration != "" {
		ann := u.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[AnnotationLastApplied] = r.LastAppliedConfiguration
		u.SetAnnotations(ann)
	}
	return u
}

func (r *Resource) ownerRef() metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: r.Ref.APIVersion, Kind: r.Ref.Kind, Name: r.Ref.Name, UID: r.Ref.UID}
}

// withInferredOwner returns a copy of r with an inferred owner reference added or removed.
// It returns r unchanged when nothing changes.
func (r *Resource) withInferredOwner(owner metav1.OwnerReference, add bool) *Resource {
	inferred := slices.Contains(r.inferredUIDs, owner.UID)
	switch {
	case add && (inferred || hasOwnerUID(r.OwnerRefs, owner.UID)):
		return r
	case !add && !inferred:
		return r
	}
	cp := *r
	if add {
		cp.OwnerRefs = append(slices.Clone(r.OwnerRefs), owner)
		cp.inferredUIDs = append(slices.Clone(r.inferredUIDs), owner.UID)
		return &cp
	}
	cp.OwnerRefs = slices.DeleteFunc(slices.Clone(r.OwnerRefs), func(o metav1.OwnerReference) bool { return o.UID == owner.UID })
	cp.inferredUIDs = slices.DeleteFunc(slices.Clone(r.inferredUIDs), func(u types.UID) bool { return u == owner.UID })
	return &cp
}
