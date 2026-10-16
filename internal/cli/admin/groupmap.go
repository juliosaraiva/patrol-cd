package admin

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// rewriteGroupKey maps an annotation, label or finalizer key of API group from to the same
// key of group to (ADR-0002: every Patrol key embeds the group, e.g.
// "cd.patrol.example/tracking-id", "link.cd.patrol.example/x",
// "notified.notifications.cd.patrol.example", "resources-finalizer.cd.patrol.example/background").
// The group is replaced where it is a whole dot-separated label: preceded by the start of the
// key or ".", followed by the end of the key or "/". Other keys are returned unchanged.
func rewriteGroupKey(key, from, to string) string {
	if from == "" || from == to {
		return key
	}
	var sb strings.Builder
	i := 0
	for {
		j := strings.Index(key[i:], from)
		if j < 0 {
			sb.WriteString(key[i:])
			return sb.String()
		}
		start := i + j
		end := start + len(from)
		before := start == 0 || key[start-1] == '.'
		after := end == len(key) || key[end] == '/'
		sb.WriteString(key[i:start])
		if before && after {
			sb.WriteString(to)
		} else {
			sb.WriteString(from)
		}
		i = end
	}
}

func rewriteKeys(m map[string]string, from, to string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[rewriteGroupKey(k, from, to)] = v
	}
	return out
}

// rewriteMetaGroup rewrites the metadata keys (labels, annotations, finalizers) of obj.
func rewriteMetaGroup(obj *unstructured.Unstructured, from, to string) {
	if l := obj.GetLabels(); l != nil {
		obj.SetLabels(rewriteKeys(l, from, to))
	}
	if a := obj.GetAnnotations(); a != nil {
		obj.SetAnnotations(rewriteKeys(a, from, to))
	}
	if fs := obj.GetFinalizers(); fs != nil {
		out := make([]string, len(fs))
		for i, f := range fs {
			out[i] = rewriteGroupKey(f, from, to)
		}
		obj.SetFinalizers(out)
	}
	if refs := obj.GetOwnerReferences(); refs != nil {
		for i := range refs {
			refs[i].APIVersion = rewriteAPIVersion(refs[i].APIVersion, from, to)
		}
		obj.SetOwnerReferences(refs)
	}
}

// rewriteAPIVersion maps "<from>/<version>" to "<to>/<version>".
func rewriteAPIVersion(apiVersion, from, to string) string {
	if g, v, ok := strings.Cut(apiVersion, "/"); ok && g == from {
		return to + "/" + v
	}
	return apiVersion
}

// rewriteGroupFields maps group-valued fields of a spec subtree: every "group" string equal
// to from (resource allow/deny lists, ignoreDifferences, orphaned resource keys).
func rewriteGroupFields(v any, from, to string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "group" {
				if s, ok := val.(string); ok && s == from {
					t[k] = to
				}
				continue
			}
			rewriteGroupFields(val, from, to)
		}
	case []any:
		for _, e := range t {
			rewriteGroupFields(e, from, to)
		}
	}
}

// rewriteObjectGroup moves obj from API group from to group to: apiVersion, metadata keys,
// owner references, group-valued spec fields, and the metadata of ApplicationSet templates.
func rewriteObjectGroup(obj *unstructured.Unstructured, from, to string) {
	obj.SetAPIVersion(rewriteAPIVersion(obj.GetAPIVersion(), from, to))
	rewriteMetaGroup(obj, from, to)
	spec, ok := obj.Object["spec"].(map[string]any)
	if !ok {
		return
	}
	rewriteGroupFields(spec, from, to)
	if obj.GetKind() == kindApplicationSet.Kind {
		if tmpl, ok := spec["template"].(map[string]any); ok {
			if md, ok := tmpl["metadata"].(map[string]any); ok {
				t := &unstructured.Unstructured{Object: map[string]any{"metadata": md}}
				rewriteMetaGroup(t, from, to)
				tmpl["metadata"] = t.Object["metadata"]
			}
		}
	}
}
