package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"
)

// object is one Kubernetes object of a rendered stream: its document node (a mapping, comments
// removed) and its identity.
type object struct {
	node                              *yamlv3.Node
	apiVersion, kind, namespace, name string
}

func (o object) id() string {
	return o.apiVersion + "/" + o.kind + "/" + o.namespace + "/" + o.name
}

// parseObjects splits a multi-document YAML stream into objects. Empty documents are skipped;
// duplicate keys, non-mapping documents and objects without apiVersion, kind or name are errors.
func parseObjects(data []byte) ([]object, error) {
	dec := yamlv3.NewDecoder(bytes.NewReader(data))
	var out []object
	seen := map[string]bool{}
	for {
		var doc yamlv3.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if len(doc.Content) == 0 {
			continue
		}
		root := doc.Content[0]
		if root.Kind == yamlv3.ScalarNode && root.Tag == "!!null" {
			continue
		}
		if root.Kind != yamlv3.MappingNode {
			return nil, fmt.Errorf("line %d: document is not a mapping", root.Line)
		}
		if len(root.Content) == 0 {
			continue
		}
		if err := checkDuplicateKeys(root, ""); err != nil {
			return nil, err
		}
		stripComments(root)
		o := object{node: root}
		o.apiVersion = scalarAt(root, "apiVersion")
		o.kind = scalarAt(root, "kind")
		o.name = scalarAt(root, "metadata", "name")
		o.namespace = scalarAt(root, "metadata", "namespace")
		if o.apiVersion == "" || o.kind == "" || o.name == "" {
			return nil, fmt.Errorf("line %d: object without apiVersion, kind or metadata.name", root.Line)
		}
		if seen[o.id()] {
			return nil, fmt.Errorf("duplicate object %s", o.id())
		}
		seen[o.id()] = true
		out = append(out, o)
	}
}

// checkDuplicateKeys refuses mappings with a repeated key (decoding into a yaml.Node does not;
// kubectl would keep one of the values).
func checkDuplicateKeys(n *yamlv3.Node, path string) error {
	switch n.Kind {
	case yamlv3.MappingNode:
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			if seen[k] {
				return fmt.Errorf("line %d: duplicate key %s", n.Content[i].Line, join(path, k))
			}
			seen[k] = true
			if err := checkDuplicateKeys(n.Content[i+1], join(path, k)); err != nil {
				return err
			}
		}
	case yamlv3.SequenceNode:
		for i, c := range n.Content {
			if err := checkDuplicateKeys(c, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func stripComments(n *yamlv3.Node) {
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	for _, c := range n.Content {
		stripComments(c)
	}
}

// mapValue returns the value node of key in mapping m, or nil.
func mapValue(m *yamlv3.Node, key string) *yamlv3.Node {
	if m == nil || m.Kind != yamlv3.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// deleteKey removes key from mapping m.
func deleteKey(m *yamlv3.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = slices.Delete(m.Content, i, i+2)
			return
		}
	}
}

// scalarAt returns the scalar at a key path of mapping m, or "".
func scalarAt(m *yamlv3.Node, path ...string) string {
	n := m
	for _, k := range path {
		n = mapValue(n, k)
	}
	if n == nil || n.Kind != yamlv3.ScalarNode {
		return ""
	}
	return n.Value
}

// Helm-only metadata. The static manifests carry neither helm.sh/chart nor
// app.kubernetes.io/managed-by: they are applied by kubectl, kustomize, a GitOps tool or Patrol
// CD itself, so no single value would be true, and upstream's static manifests omit both.
const (
	labelChart     = "helm.sh/chart"
	labelManagedBy = "app.kubernetes.io/managed-by"
)

func isHelmAnnotation(key string) bool {
	return strings.HasPrefix(key, "helm.sh/") || strings.HasPrefix(key, "meta.helm.sh/")
}

// sanitizeAll removes the Helm-only metadata from every object and refuses what a static
// manifest must not contain: Helm hooks (including tests), Helm metadata left outside
// metadata.labels/annotations (selectors), unrendered release placeholders and Secrets with
// data.
func sanitizeAll(objs []object) error {
	var errs []error
	for i := range objs {
		if err := sanitize(&objs[i]); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", objs[i].id(), err))
		}
	}
	return errors.Join(errs...)
}

func sanitize(o *object) error {
	var errs []error
	walkMetadata(o.node, "", func(meta *yamlv3.Node, path string) {
		if labels := mapValue(meta, "labels"); labels != nil && labels.Kind == yamlv3.MappingNode {
			deleteKey(labels, labelChart)
			if v := mapValue(labels, labelManagedBy); v != nil {
				if v.Value != "Helm" {
					errs = append(errs, fmt.Errorf("%s.labels: unexpected %s %q", path, labelManagedBy, v.Value))
				}
				deleteKey(labels, labelManagedBy)
			}
			if len(labels.Content) == 0 {
				deleteKey(meta, "labels")
			}
		}
		if ann := mapValue(meta, "annotations"); ann != nil && ann.Kind == yamlv3.MappingNode {
			for i := 0; i+1 < len(ann.Content); {
				key := ann.Content[i].Value
				switch {
				case strings.HasPrefix(key, "helm.sh/hook"):
					errs = append(errs, fmt.Errorf("%s.annotations: Helm hook %s=%q (hooks and tests have no static equivalent)", path, key, ann.Content[i+1].Value))
					i += 2
				case isHelmAnnotation(key):
					ann.Content = slices.Delete(ann.Content, i, i+2)
				default:
					i += 2
				}
			}
			if len(ann.Content) == 0 {
				deleteKey(meta, "annotations")
			}
		}
	})
	walkKeys(o.node, "", func(key string, value *yamlv3.Node, path string) {
		if key == labelChart || key == labelManagedBy || isHelmAnnotation(key) {
			errs = append(errs, fmt.Errorf("%s: Helm metadata outside metadata.labels/annotations", path))
		}
		if value.Kind == yamlv3.ScalarNode && strings.Contains(value.Value, "RELEASE-NAME") {
			errs = append(errs, fmt.Errorf("%s: unrendered release name placeholder", path))
		}
	})
	if o.kind == "Secret" {
		for _, k := range []string{"data", "stringData"} {
			if v := mapValue(o.node, k); v != nil && len(v.Content) > 0 {
				errs = append(errs, fmt.Errorf("secret with %s: static manifests must not carry secret material", k))
			}
		}
	}
	return errors.Join(errs...)
}

// walkMetadata calls fn for every mapping found under a "metadata" key (object metadata, pod
// template metadata, volume claim template metadata, ...).
func walkMetadata(n *yamlv3.Node, path string, fn func(meta *yamlv3.Node, path string)) {
	switch n.Kind {
	case yamlv3.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i].Value, n.Content[i+1]
			p := join(path, k)
			if k == "metadata" && v.Kind == yamlv3.MappingNode {
				fn(v, p)
			}
			walkMetadata(v, p, fn)
		}
	case yamlv3.SequenceNode:
		for i, c := range n.Content {
			walkMetadata(c, fmt.Sprintf("%s[%d]", path, i), fn)
		}
	}
}

// walkKeys calls fn for every mapping entry.
func walkKeys(n *yamlv3.Node, path string, fn func(key string, value *yamlv3.Node, path string)) {
	switch n.Kind {
	case yamlv3.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i].Value, n.Content[i+1]
			p := join(path, k)
			fn(k, v, p)
			walkKeys(v, p, fn)
		}
	case yamlv3.SequenceNode:
		for i, c := range n.Content {
			walkKeys(c, fmt.Sprintf("%s[%d]", path, i), fn)
		}
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// crdKinds are the objects of the shared crds/ base: one copy per cluster.
var crdKinds = []string{"CustomResourceDefinition", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"}

// splitCRDs separates the CRDs and the admission policy from the other objects.
func splitCRDs(objs []object) (crds, rest []object) {
	for _, o := range objs {
		if slices.Contains(crdKinds, o.kind) {
			crds = append(crds, o)
		} else {
			rest = append(rest, o)
		}
	}
	return crds, rest
}

// clusterScopedKinds are the cluster-scoped kinds the chart can render.
var clusterScopedKinds = []string{
	"Namespace", "CustomResourceDefinition", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding",
	"ClusterRole", "ClusterRoleBinding", "PriorityClass", "IngressClass", "StorageClass",
	"MutatingWebhookConfiguration", "ValidatingWebhookConfiguration", "APIService",
}

// checkScope enforces the variant's scope: a cluster-wide variant renders the CRDs and the
// admission policy; a namespaced one renders no cluster-scoped object and nothing outside the
// installation namespace. Every namespaced object is in the installation namespace (no
// application namespaces in the profiles).
func checkScope(v variant, crds, rest []object) error {
	var errs []error
	if v.cluster {
		var kinds []string
		for _, o := range crds {
			kinds = append(kinds, o.kind)
		}
		for _, k := range crdKinds {
			if !slices.Contains(kinds, k) {
				errs = append(errs, fmt.Errorf("cluster-wide variant without a %s", k))
			}
		}
	} else if len(crds) > 0 {
		errs = append(errs, fmt.Errorf("namespace-scoped variant renders %s", crds[0].id()))
	}
	for _, o := range rest {
		cluster := slices.Contains(clusterScopedKinds, o.kind)
		switch {
		case cluster && !v.cluster:
			errs = append(errs, fmt.Errorf("namespace-scoped variant renders the cluster-scoped %s", o.id()))
		case cluster && o.namespace != "":
			errs = append(errs, fmt.Errorf("cluster-scoped %s has a namespace", o.id()))
		case !cluster && o.namespace != namespace:
			errs = append(errs, fmt.Errorf("%s is not in namespace %q", o.id(), namespace))
		}
		if o.kind == "Namespace" {
			errs = append(errs, fmt.Errorf("the chart renders a Namespace (%s); the generator adds its own", o.name))
		}
	}
	return errors.Join(errs...)
}

// kindOrder is the apply order: the namespace, the CRDs and the admission policy, then
// NetworkPolicies and PDBs (in force before any pod starts), identities, configuration, RBAC,
// Services and finally the workloads. Kinds not listed follow, by name.
var kindOrder = []string{
	"Namespace",
	"CustomResourceDefinition",
	"ValidatingAdmissionPolicy",
	"ValidatingAdmissionPolicyBinding",
	"NetworkPolicy",
	"PodDisruptionBudget",
	"ServiceAccount",
	"Secret",
	"ConfigMap",
	"ClusterRole",
	"ClusterRoleBinding",
	"Role",
	"RoleBinding",
	"Service",
	"Deployment",
	"StatefulSet",
	"HorizontalPodAutoscaler",
}

func kindRank(kind string) int {
	if i := slices.Index(kindOrder, kind); i >= 0 {
		return i
	}
	return len(kindOrder)
}

// sortObjects orders objects by kind rank, then kind, namespace and name (deterministic).
func sortObjects(objs []object) {
	slices.SortStableFunc(objs, func(a, b object) int {
		if d := kindRank(a.kind) - kindRank(b.kind); d != 0 {
			return d
		}
		for _, p := range [][2]string{{a.kind, b.kind}, {a.namespace, b.namespace}, {a.name, b.name}, {a.apiVersion, b.apiVersion}} {
			if c := strings.Compare(p[0], p[1]); c != 0 {
				return c
			}
		}
		return 0
	})
}

// encodeObjects serializes objects as a multi-document YAML stream.
func encodeObjects(objs []object) ([]byte, error) {
	var b bytes.Buffer
	enc := yamlv3.NewEncoder(&b)
	enc.SetIndent(2)
	enc.CompactSeqIndent()
	for _, o := range objs {
		if err := enc.Encode(o.node); err != nil {
			return nil, fmt.Errorf("%s: %w", o.id(), err)
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Pod Security admission labels of the namespace. enforce is pinned to the oldest Kubernetes
// version the chart supports (1.30) so a cluster upgrade cannot start rejecting pods; audit and
// warn follow the cluster's latest policy to surface future violations without blocking.
const psaEnforceVersion = "v1.30"

// namespaceObject is the installation namespace of the cluster-wide variants (not rendered by
// the chart: Helm installs use --create-namespace).
func namespaceObject(appVersion string) (object, error) {
	doc := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    app.kubernetes.io/instance: %s
    app.kubernetes.io/part-of: patrol-cd
    app.kubernetes.io/version: %q
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/enforce-version: %s
    pod-security.kubernetes.io/audit: restricted
    pod-security.kubernetes.io/audit-version: latest
    pod-security.kubernetes.io/warn: restricted
    pod-security.kubernetes.io/warn-version: latest
`, namespace, releaseName, appVersion, psaEnforceVersion)
	objs, err := parseObjects([]byte(doc))
	if err != nil {
		return object{}, err
	}
	return objs[0], nil
}
