package project

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// ClusterIndex is an immutable ClusterLookup over a snapshot of Cluster objects with the
// upstream registry rules (util/db cluster.go):
//
//   - server URLs are compared with trailing '/' removed;
//   - when in-cluster is enabled, https://kubernetes.default.svc resolves to a Cluster
//     object registering it, or to a synthesized cluster named "in-cluster";
//   - when in-cluster is disabled, that address resolves to nothing ("cluster ... is
//     disabled") and is filtered out of name lookups and project clusters.
//
// Two Cluster objects registering the same server are a configuration error upstream cannot
// express (the server is the Secret key there); the index resolves the server to the object
// with the lexicographically smallest metadata.name so results are deterministic.
type ClusterIndex struct {
	inClusterEnabled bool
	byServer         map[string]*v1alpha1.Cluster
	serversByName    map[string][]string
	byProject        map[string][]*v1alpha1.Cluster
}

var _ ClusterLookup = (*ClusterIndex)(nil)

// NewClusterIndex indexes clusters. The objects are deep-copied, so later changes to the
// input do not affect the index.
func NewClusterIndex(clusters []v1alpha1.Cluster, inClusterEnabled bool) *ClusterIndex {
	sorted := make([]*v1alpha1.Cluster, 0, len(clusters))
	for i := range clusters {
		sorted = append(sorted, clusters[i].DeepCopy())
	}
	slices.SortFunc(sorted, func(a, b *v1alpha1.Cluster) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name))
	})
	idx := &ClusterIndex{
		inClusterEnabled: inClusterEnabled,
		byServer:         make(map[string]*v1alpha1.Cluster, len(sorted)),
		serversByName:    make(map[string][]string),
		byProject:        make(map[string][]*v1alpha1.Cluster),
	}
	for _, c := range sorted {
		server := normalizeServer(c.Spec.Server)
		if _, dup := idx.byServer[server]; !dup {
			idx.byServer[server] = c
		}
		idx.serversByName[c.Spec.Name] = append(idx.serversByName[c.Spec.Name], server)
		if c.Spec.Project != "" {
			idx.byProject[c.Spec.Project] = append(idx.byProject[c.Spec.Project], c)
		}
	}
	return idx
}

func normalizeServer(s string) string { return strings.TrimRight(s, "/") }

// InClusterCluster returns the synthesized in-cluster destination used when no Cluster object
// registers https://kubernetes.default.svc.
func InClusterCluster() *v1alpha1.Cluster {
	return &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: v1alpha1.KubernetesInternalAPIServerAddr, Name: InClusterName}}
}

// GetCluster implements ClusterLookup.
func (x *ClusterIndex) GetCluster(_ context.Context, server string) (*v1alpha1.Cluster, error) {
	server = normalizeServer(server)
	if x == nil {
		return nil, fmt.Errorf("cluster %q not found: %w", server, ErrClusterNotFound)
	}
	if server == v1alpha1.KubernetesInternalAPIServerAddr {
		if !x.inClusterEnabled {
			return nil, fmt.Errorf("cluster %q is disabled: %w", server, ErrClusterNotFound)
		}
		if c, ok := x.byServer[server]; ok {
			return c.DeepCopy(), nil
		}
		return InClusterCluster(), nil
	}
	if c, ok := x.byServer[server]; ok {
		return c.DeepCopy(), nil
	}
	return nil, fmt.Errorf("cluster %q not found: %w", server, ErrClusterNotFound)
}

// GetClusterServersByName implements ClusterLookup.
func (x *ClusterIndex) GetClusterServersByName(_ context.Context, name string) ([]string, error) {
	if x == nil {
		return nil, nil
	}
	servers := slices.Clone(x.serversByName[name])
	if len(servers) == 0 && name == InClusterName && x.inClusterEnabled {
		return []string{v1alpha1.KubernetesInternalAPIServerAddr}, nil
	}
	if !x.inClusterEnabled {
		servers = slices.DeleteFunc(servers, func(s string) bool { return s == v1alpha1.KubernetesInternalAPIServerAddr })
	}
	return servers, nil
}

// GetProjectClusters implements ClusterLookup.
func (x *ClusterIndex) GetProjectClusters(_ context.Context, project string) ([]*v1alpha1.Cluster, error) {
	if x == nil {
		return nil, nil
	}
	out := make([]*v1alpha1.Cluster, 0, len(x.byProject[project]))
	for _, c := range x.byProject[project] {
		if !x.inClusterEnabled && normalizeServer(c.Spec.Server) == v1alpha1.KubernetesInternalAPIServerAddr {
			continue
		}
		out = append(out, c.DeepCopy())
	}
	return out, nil
}
