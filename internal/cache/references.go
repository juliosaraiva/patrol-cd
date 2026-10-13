package cache

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const (
	kindEndpoints             = "Endpoints"
	kindService               = "Service"
	kindSecret                = "Secret"
	kindServiceAccount        = "ServiceAccount"
	kindStatefulSet           = "StatefulSet"
	kindPersistentVolumeClaim = "PersistentVolumeClaim"
	kindCRD                   = "CustomResourceDefinition"
	kindAPIService            = "APIService"

	groupApps          = "apps"
	groupExtensions    = "extensions"
	groupOLM           = "operators.coreos.com"
	groupAPIExtensions = "apiextensions.k8s.io"
	groupAPIRegistry   = "apiregistration.k8s.io"

	annotationOLMOperatorGroup = "olm.operatorGroup"
	annotationSAName           = "kubernetes.io/service-account.name"
	annotationSAUID            = "kubernetes.io/service-account.uid"
	secretTypeSAToken          = "kubernetes.io/service-account-token"

	// AnnotationLastApplied is the kubectl client-side apply annotation.
	AnnotationLastApplied = "kubectl.kubernetes.io/last-applied-configuration"
)

// resolveOwnerRefs returns the object's owner references plus synthesized references, and,
// for StatefulSets, a predicate identifying PersistentVolumeClaims created from its
// volumeClaimTemplates. The inferred relations mirror upstream gitops-engine:
//
//   - core Endpoints without owners → Service of the same name (no UID);
//   - OLM ClusterServiceVersion with the olm.operatorGroup annotation → OperatorGroup (no UID);
//   - service-account token Secret → ServiceAccount (UID from annotation);
//   - StatefulSet → PersistentVolumeClaim named <template>-<statefulset>-<ordinal>.
func resolveOwnerRefs(un *unstructured.Unstructured) ([]metav1.OwnerReference, func(ResourceKey) bool) {
	refs := un.GetOwnerReferences()
	gvk := un.GroupVersionKind()
	switch {
	case gvk.Group == "" && gvk.Kind == kindEndpoints && len(refs) == 0:
		refs = append(refs, metav1.OwnerReference{APIVersion: "v1", Kind: kindService, Name: un.GetName()})
	case gvk.Group == groupOLM && gvk.Kind == "ClusterServiceVersion":
		if og := un.GetAnnotations()[annotationOLMOperatorGroup]; og != "" {
			refs = append(refs, metav1.OwnerReference{APIVersion: groupOLM + "/v1", Kind: "OperatorGroup", Name: og})
		}
	case gvk.Group == "" && gvk.Kind == kindSecret:
		if ref, ok := serviceAccountTokenOwner(un); ok && !hasOwnerUID(refs, ref.UID) {
			refs = append(refs, ref)
		}
	case (gvk.Group == groupApps || gvk.Group == groupExtensions) && gvk.Kind == kindStatefulSet:
		return refs, statefulSetClaims(un)
	}
	return refs, nil
}

func serviceAccountTokenOwner(un *unstructured.Unstructured) (metav1.OwnerReference, bool) {
	typ, _, _ := unstructured.NestedString(un.Object, "type")
	if typ != secretTypeSAToken {
		return metav1.OwnerReference{}, false
	}
	ann := un.GetAnnotations()
	name, uid := ann[annotationSAName], ann[annotationSAUID]
	if name == "" || uid == "" {
		return metav1.OwnerReference{}, false
	}
	return metav1.OwnerReference{APIVersion: "v1", Kind: kindServiceAccount, Name: name, UID: types.UID(uid)}, true
}

// statefulSetClaims returns a predicate matching PVC keys created from the StatefulSet's
// volumeClaimTemplates, or nil when it has none. A claim is named
// "<template>-<statefulset>-<ordinal>". Upstream uses an unanchored regular expression built
// from the names; this implementation matches the whole name exactly.
func statefulSetClaims(un *unstructured.Unstructured) func(ResourceKey) bool {
	templates, _, _ := unstructured.NestedSlice(un.Object, "spec", "volumeClaimTemplates")
	prefixes := make([]string, 0, len(templates))
	for _, t := range templates {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(m, "metadata", "name")
		if name != "" {
			prefixes = append(prefixes, name+"-"+un.GetName()+"-")
		}
	}
	if len(prefixes) == 0 {
		return nil
	}
	ns := un.GetNamespace()
	return func(k ResourceKey) bool {
		if k.Group != "" || k.Kind != kindPersistentVolumeClaim || k.Namespace != ns {
			return false
		}
		for _, p := range prefixes {
			if ord, ok := strings.CutPrefix(k.Name, p); ok && isDigits(ord) {
				return true
			}
		}
		return false
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func hasOwnerUID(refs []metav1.OwnerReference, uid types.UID) bool {
	for i := range refs {
		if refs[i].UID == uid {
			return true
		}
	}
	return false
}

func isPVCKey(k ResourceKey) bool { return k.Group == "" && k.Kind == kindPersistentVolumeClaim }
