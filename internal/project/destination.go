package project

import (
	"context"
	"fmt"
	"strings"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// InClusterName is the default name of the in-cluster destination
// (v1alpha1.KubernetesInternalAPIServerAddr).
const InClusterName = "in-cluster"

// ProjectClusterLister returns the clusters scoped to a project (Cluster.spec.project).
type ProjectClusterLister interface {
	GetProjectClusters(ctx context.Context, project string) ([]*v1alpha1.Cluster, error)
}

// ClusterLookup resolves registered clusters. Implementations must follow the upstream
// registry rules (see ClusterIndex, which implements them over a list of Cluster objects):
//
//   - GetCluster returns the cluster whose spec.server equals server (trailing '/'
//     ignored), the synthesized in-cluster cluster for KubernetesInternalAPIServerAddr when
//     in-cluster is enabled and no Cluster object registers it, and an error wrapping
//     ErrClusterNotFound otherwise.
//   - GetClusterServersByName returns the servers of every cluster with the name (several
//     clusters may share one), [KubernetesInternalAPIServerAddr] for "in-cluster" when
//     no Cluster has that name and in-cluster is enabled, and never the in-cluster address
//     when in-cluster is disabled.
//   - GetProjectClusters returns the clusters whose spec.project is the project.
type ClusterLookup interface {
	ProjectClusterLister
	GetCluster(ctx context.Context, server string) (*v1alpha1.Cluster, error)
	GetClusterServersByName(ctx context.Context, name string) ([]string, error)
}

// Destination is a resolved destination: the server URL and name of the registered cluster
// an Application deploys to, and the target namespace ("" for cluster-scoped resources).
// The zero Server and Name mean "no cluster" and are never permitted.
type Destination struct {
	Server    string
	Name      string
	Namespace string
}

// DestinationOf returns the Destination of a resolved cluster and namespace.
func DestinationOf(cluster *v1alpha1.Cluster, namespace string) Destination {
	if cluster == nil {
		return Destination{Namespace: namespace}
	}
	return Destination{Server: normalizeServer(cluster.Spec.Server), Name: cluster.Spec.Name, Namespace: namespace}
}

// ResolveDestinationServer returns the server URL an Application destination designates
// (upstream GetDestinationServer): spec.destination.server with trailing '/' removed, or the
// single server registered under spec.destination.name.
func ResolveDestinationServer(ctx context.Context, dest v1alpha1.ApplicationDestination, lookup ClusterLookup) (string, error) {
	if dest.Name != "" && dest.Server != "" {
		return "", fmt.Errorf("application destination can't have both name and server defined: %s %s", dest.Name, dest.Server)
	}
	if dest.Server != "" {
		return strings.TrimRight(dest.Server, "/"), nil
	}
	if dest.Name == "" {
		return "", ErrDestinationMissing
	}
	if lookup == nil {
		return "", fmt.Errorf("error getting cluster by name %q: %w", dest.Name, errNoLookup)
	}
	servers, err := lookup.GetClusterServersByName(ctx, dest.Name)
	if err != nil {
		return "", fmt.Errorf("error getting cluster by name %q: %w", dest.Name, err)
	}
	switch len(servers) {
	case 0:
		return "", fmt.Errorf("there are no clusters with this name: %s", dest.Name)
	case 1:
		return servers[0], nil
	default:
		return "", fmt.Errorf("there are %d clusters with the same name: [%s]", len(servers), strings.Join(servers, " "))
	}
}

var errNoLookup = fmt.Errorf("no cluster lookup configured: %w", ErrClusterNotFound)

// ResolveDestination returns the registered cluster an Application destination designates
// (upstream GetDestinationCluster). Error messages are the upstream ones and are used
// verbatim as InvalidSpecError conditions.
func ResolveDestination(ctx context.Context, dest v1alpha1.ApplicationDestination, lookup ClusterLookup) (*v1alpha1.Cluster, error) {
	server, err := ResolveDestinationServer(ctx, dest, lookup)
	if err != nil {
		return nil, err
	}
	if lookup == nil {
		return nil, fmt.Errorf("error getting cluster by URL: %w", errNoLookup)
	}
	cluster, err := lookup.GetCluster(ctx, server)
	if err != nil {
		if dest.Server != "" {
			return nil, fmt.Errorf("error getting cluster by server %q: %w", dest.Server, err)
		}
		return nil, fmt.Errorf("error getting cluster by URL: %w", err)
	}
	if cluster == nil {
		return nil, fmt.Errorf("error getting cluster by URL: cluster %q not found: %w", server, ErrClusterNotFound)
	}
	return cluster, nil
}

// IsDestinationPermitted reports whether proj permits deploying to dest (upstream
// AppProject.IsDestinationPermitted). dest.Server and dest.Name must be those of the
// registered cluster (see ResolveDestination), so a project entry written with a server
// matches an Application addressing the cluster by name and vice versa.
//
// For each spec.destinations entry (server, name and namespace are globs; "!" negates; "*"
// matches anything including the empty namespace):
//   - the entry matches when (server or name matched) and the namespace matched; a server
//     or name only matches a non-empty value;
//   - the destination is rejected, whatever the other entries say, when the name did not
//     match and the entry name is negated, or when the server did not match, the entry server
//     is negated and the namespace matched, or when the namespace did not match, the entry
//     namespace is negated and the server matched.
//
// The destination is permitted when some entry matched and none rejected it. With
// spec.permitOnlyProjectScopedClusters, a matching destination must additionally be one of
// the project-scoped clusters (same name or same server); lister errors are returned as
// "could not retrieve project clusters: ...".
func IsDestinationPermitted(ctx context.Context, proj *v1alpha1.AppProject, dest Destination, lister ProjectClusterLister) (bool, error) {
	if proj == nil || (dest.Server == "" && dest.Name == "") {
		return false, nil
	}
	matched := destinationsMatch(proj.Spec.Destinations, dest)
	if !matched || !proj.Spec.PermitOnlyProjectScopedClusters {
		return matched, nil
	}
	if lister == nil {
		return false, fmt.Errorf("could not retrieve project clusters: %w", errNoLookup)
	}
	clusters, err := lister.GetProjectClusters(ctx, proj.Name)
	if err != nil {
		return false, fmt.Errorf("could not retrieve project clusters: %w", err)
	}
	for _, c := range clusters {
		if c == nil {
			continue
		}
		if c.Spec.Name == dest.Name || normalizeServer(c.Spec.Server) == normalizeServer(dest.Server) {
			return true, nil
		}
	}
	return false, nil
}

func destinationsMatch(entries []v1alpha1.ApplicationDestination, dst Destination) bool {
	anyMatched := false
	for _, item := range entries {
		nameMatched := dst.Name != "" && matchPattern(item.Name, dst.Name, true)
		serverMatched := dst.Server != "" && matchPattern(item.Server, dst.Server, true)
		namespaceMatched := matchPattern(item.Namespace, dst.Namespace, true)

		switch {
		case (serverMatched || nameMatched) && namespaceMatched:
			anyMatched = true
		// Upstream precedence: A || (B && C).
		case (!nameMatched && isDenyPattern(item.Name)) || (!serverMatched && isDenyPattern(item.Server) && namespaceMatched):
			return false
		case !namespaceMatched && isDenyPattern(item.Namespace) && serverMatched:
			return false
		}
	}
	return anyMatched
}

// IsAppDestinationPermitted resolves an Application destination and checks it against proj.
// The returned cluster is nil when resolution failed (err is then the resolution error).
func IsAppDestinationPermitted(ctx context.Context, proj *v1alpha1.AppProject, dest v1alpha1.ApplicationDestination, lookup ClusterLookup) (bool, *v1alpha1.Cluster, error) {
	cluster, err := ResolveDestination(ctx, dest, lookup)
	if err != nil {
		return false, nil, err
	}
	ok, err := IsDestinationPermitted(ctx, proj, DestinationOf(cluster, dest.Namespace), lookup)
	return ok, cluster, err
}
