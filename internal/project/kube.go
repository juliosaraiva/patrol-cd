package project

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// KubeLookup reads AppProjects, Clusters and Repositories of the control-plane namespace
// through a controller-runtime client.Reader (normally the manager's cached client). It
// implements ProjectGetter, ClusterLookup and ScopedRepositoryLister. Cluster queries list
// the namespace's Clusters and apply the ClusterIndex rules.
type KubeLookup struct {
	reader           client.Reader
	namespace        string
	inClusterEnabled bool
}

var (
	_ ProjectGetter          = (*KubeLookup)(nil)
	_ ClusterLookup          = (*KubeLookup)(nil)
	_ ScopedRepositoryLister = (*KubeLookup)(nil)
)

// NewKubeLookup returns a lookup over namespace. inClusterEnabled mirrors the
// cluster.inClusterEnabled setting (default true).
func NewKubeLookup(reader client.Reader, namespace string, inClusterEnabled bool) *KubeLookup {
	return &KubeLookup{reader: reader, namespace: namespace, inClusterEnabled: inClusterEnabled}
}

// Lookups returns a Lookups bundle backed by k.
func (k *KubeLookup) Lookups(globals []GlobalProjectSetting) Lookups {
	return Lookups{Projects: k, Repositories: k, Clusters: k, GlobalProjects: globals}
}

// GetProject implements ProjectGetter. A missing project yields the API server's NotFound
// error (IsNotFound reports true).
func (k *KubeLookup) GetProject(ctx context.Context, name string) (*v1alpha1.AppProject, error) {
	proj := &v1alpha1.AppProject{}
	if err := k.reader.Get(ctx, client.ObjectKey{Namespace: k.namespace, Name: name}, proj); err != nil {
		return nil, err
	}
	return proj, nil
}

func (k *KubeLookup) index(ctx context.Context) (*ClusterIndex, error) {
	var list v1alpha1.ClusterList
	if err := k.reader.List(ctx, &list, client.InNamespace(k.namespace)); err != nil {
		return nil, fmt.Errorf("failed to list clusters: %w", err)
	}
	return NewClusterIndex(list.Items, k.inClusterEnabled), nil
}

// GetCluster implements ClusterLookup.
func (k *KubeLookup) GetCluster(ctx context.Context, server string) (*v1alpha1.Cluster, error) {
	idx, err := k.index(ctx)
	if err != nil {
		return nil, err
	}
	return idx.GetCluster(ctx, server)
}

// GetClusterServersByName implements ClusterLookup.
func (k *KubeLookup) GetClusterServersByName(ctx context.Context, name string) ([]string, error) {
	idx, err := k.index(ctx)
	if err != nil {
		return nil, err
	}
	return idx.GetClusterServersByName(ctx, name)
}

// GetProjectClusters implements ClusterLookup.
func (k *KubeLookup) GetProjectClusters(ctx context.Context, project string) ([]*v1alpha1.Cluster, error) {
	idx, err := k.index(ctx)
	if err != nil {
		return nil, err
	}
	return idx.GetProjectClusters(ctx, project)
}

// GetProjectRepositoryURLs implements ScopedRepositoryLister: the URLs of the read-purpose
// Repositories whose spec.project is project, in name order.
func (k *KubeLookup) GetProjectRepositoryURLs(ctx context.Context, project string) ([]string, error) {
	var list v1alpha1.RepositoryList
	if err := k.reader.List(ctx, &list, client.InNamespace(k.namespace)); err != nil {
		return nil, fmt.Errorf("failed to list repositories: %w", err)
	}
	var urls []string
	for _, r := range list.Items {
		if r.Spec.Project != project || r.Spec.Purpose == v1alpha1.RepositoryPurposeWrite {
			continue
		}
		urls = append(urls, r.Spec.URL)
	}
	return urls, nil
}
