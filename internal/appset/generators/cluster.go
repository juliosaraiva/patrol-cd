package generators

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/appset/template"
)

// InClusterName is the name of the synthetic local cluster.
const InClusterName = "in-cluster"

// ClusterGenerator produces one parameter set per registered Cluster (plus the synthetic local
// cluster when no selector is set and no Cluster registers the in-cluster server).
type ClusterGenerator struct {
	cfg Config
}

// RequeueAfter implements Generator: cluster changes trigger reconciles through watches.
func (g *ClusterGenerator) RequeueAfter(*v1alpha1.ApplicationSetGenerator) time.Duration {
	return NoRequeueAfter
}

// Template implements Generator.
func (g *ClusterGenerator) Template(gen *v1alpha1.ApplicationSetGenerator) *v1alpha1.ApplicationSetTemplate {
	return &gen.Clusters.Template
}

// GenerateParams implements Generator.
//
// Parameters: name, nameNormalized, server, project and the Cluster's labels and annotations
// (fasttemplate: metadata.labels.<k> / metadata.annotations.<k>; goTemplate: a metadata map with
// labels/annotations maps, each present only when non-empty), plus values. With flatList the
// result is the single parameter set {"clusters": [...]}.
func (g *ClusterGenerator) GenerateParams(ctx context.Context, gen *v1alpha1.ApplicationSetGenerator, appSet *v1alpha1.ApplicationSet) ([]map[string]any, error) {
	if gen == nil || gen.Clusters == nil {
		return nil, ErrEmptyAppSetGenerator
	}
	opts := TemplateOptions(appSet)
	sel := gen.Clusters.Selector
	ignoreLocal := len(sel.MatchLabels) > 0 || len(sel.MatchExpressions) > 0

	clusters, err := g.selectClusters(ctx, &sel)
	if err != nil {
		return nil, fmt.Errorf("error getting cluster secrets: %w", err)
	}

	var all []map[string]any
	hasInCluster := false
	for i := range clusters {
		c := &clusters[i]
		if strings.TrimRight(c.Spec.Server, "/") == v1alpha1.KubernetesInternalAPIServerAddr {
			hasInCluster = true
		}
		params := clusterParams(c, appSet.Spec.GoTemplate)
		if err := appendTemplatedValues(gen.Clusters.Values, params, opts); err != nil {
			return nil, fmt.Errorf("error appending templated values for cluster: %w", err)
		}
		all = append(all, params)
	}
	if !ignoreLocal && !hasInCluster {
		params := map[string]any{
			"name":           InClusterName,
			"nameNormalized": InClusterName,
			"server":         v1alpha1.KubernetesInternalAPIServerAddr,
			"project":        "",
		}
		if err := appendTemplatedValues(gen.Clusters.Values, params, opts); err != nil {
			return nil, fmt.Errorf("error appending templated values for local cluster: %w", err)
		}
		all = append(all, params)
	}
	if gen.Clusters.FlatList {
		return []map[string]any{{"clusters": all}}, nil
	}
	return all, nil
}

// selectClusters lists the Clusters of the control-plane namespace matching sel, keyed (and
// de-duplicated) by spec.name and sorted by name. Selectors are evaluated against the Cluster's
// labels plus cd.patrol.example/secret-type=cluster, so selectors migrated from Argo CD cluster
// Secrets keep matching every cluster.
func (g *ClusterGenerator) selectClusters(ctx context.Context, sel *metav1.LabelSelector) ([]v1alpha1.Cluster, error) {
	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return nil, fmt.Errorf("error converting label selector: %w", err)
	}
	var list v1alpha1.ClusterList
	if err := g.cfg.Client.List(ctx, &list, client.InNamespace(g.cfg.Namespace)); err != nil {
		return nil, err
	}
	byName := map[string]v1alpha1.Cluster{}
	for _, c := range list.Items {
		set := labels.Set{}
		for k, v := range c.Labels {
			set[k] = v
		}
		set[v1alpha1.LabelKeySecretType] = v1alpha1.LabelValueSecretTypeCluster
		if !selector.Matches(set) {
			continue
		}
		byName[c.Spec.Name] = c
	}
	out := make([]v1alpha1.Cluster, 0, len(byName))
	for _, name := range sortedKeys(byName) {
		out = append(out, byName[name])
	}
	return out, nil
}

func clusterParams(c *v1alpha1.Cluster, goTemplate bool) map[string]any {
	params := map[string]any{
		"name":           c.Spec.Name,
		"nameNormalized": template.SanitizeName(c.Spec.Name),
		"server":         c.Spec.Server,
		"project":        c.Spec.Project,
	}
	if goTemplate {
		meta := map[string]any{}
		if len(c.Annotations) > 0 {
			meta["annotations"] = copyStringMap(c.Annotations)
		}
		if len(c.Labels) > 0 {
			meta["labels"] = copyStringMap(c.Labels)
		}
		params["metadata"] = meta
		return params
	}
	for k, v := range c.Annotations {
		params["metadata.annotations."+k] = v
	}
	for k, v := range c.Labels {
		params["metadata.labels."+k] = v
	}
	return params
}

func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ClusterSpecifier is the name and server of a registered cluster.
type ClusterSpecifier struct {
	Name   string
	Server string
}

// ListClusters returns every Cluster of namespace plus the synthetic in-cluster cluster when no
// Cluster registers the in-cluster server.
func ListClusters(ctx context.Context, c client.Reader, namespace string) ([]ClusterSpecifier, error) {
	var list v1alpha1.ClusterList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("error listing clusters: %w", err)
	}
	out := make([]ClusterSpecifier, 0, len(list.Items)+1)
	hasInCluster := false
	for _, cl := range list.Items {
		server := strings.TrimRight(cl.Spec.Server, "/")
		out = append(out, ClusterSpecifier{Name: cl.Spec.Name, Server: server})
		if server == v1alpha1.KubernetesInternalAPIServerAddr {
			hasInCluster = true
		}
	}
	slices.SortFunc(out, func(a, b ClusterSpecifier) int { return strings.Compare(a.Name, b.Name) })
	if !hasInCluster {
		out = append(out, ClusterSpecifier{Name: InClusterName, Server: v1alpha1.KubernetesInternalAPIServerAddr})
	}
	return out, nil
}
