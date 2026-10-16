package compare

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// ResourceRef identifies a resource in the resource tree (upstream ResourceRef).
type ResourceRef struct {
	Group     string `json:"group,omitempty"`
	Version   string `json:"version,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	UID       string `json:"uid,omitempty"`
}

// FullName returns "group/kind/namespace/name", the sort key of tree nodes.
func (r ResourceRef) FullName() string {
	return r.Group + "/" + r.Kind + "/" + r.Namespace + "/" + r.Name
}

// Key returns the resource key of the reference.
func (r ResourceRef) Key() ResourceKey {
	return ResourceKey{Group: r.Group, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name}
}

// InfoItem is a name/value pair shown for a resource (pod status, revision, custom labels).
type InfoItem struct {
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
}

// LoadBalancerIngress is one load balancer ingress point (hostname preferred over IP).
type LoadBalancerIngress struct {
	Hostname string `json:"hostname,omitempty"`
	IP       string `json:"ip,omitempty"`
}

// NetworkingInfo is the networking metadata of Services, Ingresses, Pods, Gateway API and
// Istio resources, and the external URLs of any resource with link annotations.
type NetworkingInfo struct {
	TargetLabels map[string]string     `json:"targetLabels,omitempty"`
	TargetRefs   []ResourceRef         `json:"targetRefs,omitempty"`
	Labels       map[string]string     `json:"labels,omitempty"`
	Ingress      []LoadBalancerIngress `json:"ingress,omitempty"`
	ExternalURLs []string              `json:"externalURLs,omitempty"`
}

// ResourceNode is one node of the resource tree (upstream ResourceNode).
type ResourceNode struct {
	ResourceRef
	ParentRefs      []ResourceRef          `json:"parentRefs,omitempty"`
	Info            []InfoItem             `json:"info,omitempty"`
	NetworkingInfo  *NetworkingInfo        `json:"networkingInfo,omitempty"`
	ResourceVersion string                 `json:"resourceVersion,omitempty"`
	Images          []string               `json:"images,omitempty"`
	Health          *v1alpha1.HealthStatus `json:"health,omitempty"`
	CreatedAt       *metav1.Time           `json:"createdAt,omitempty"`
}

// HostResourceInfo is the capacity and the requests of one resource on a node.
type HostResourceInfo struct {
	ResourceName         corev1.ResourceName `json:"resourceName,omitempty"`
	RequestedByApp       int64               `json:"requestedByApp,omitempty"`
	RequestedByNeighbors int64               `json:"requestedByNeighbors,omitempty"`
	Capacity             int64               `json:"capacity,omitempty"`
}

// HostInfo describes a node running pods of the Application.
type HostInfo struct {
	Name          string                `json:"name,omitempty"`
	ResourcesInfo []HostResourceInfo    `json:"resourcesInfo,omitempty"`
	SystemInfo    corev1.NodeSystemInfo `json:"systemInfo,omitzero"`
	Labels        map[string]string     `json:"labels,omitempty"`
}

// ApplicationTree is the resource tree served by the controller tree API.
type ApplicationTree struct {
	// Nodes are the managed resources and their descendants, sorted by FullName.
	Nodes []ResourceNode `json:"nodes,omitempty"`
	// OrphanedNodes are the orphaned resources of the destination namespace and their
	// descendants, sorted by FullName.
	OrphanedNodes []ResourceNode `json:"orphanedNodes,omitempty"`
	// Hosts are the nodes running the Application's pods, sorted by name.
	Hosts []HostInfo `json:"hosts,omitempty"`
}

// FindNode returns the node (managed or orphaned) with the given identity, or nil.
func (t *ApplicationTree) FindNode(group, kind, namespace, name string) *ResourceNode {
	if t == nil {
		return nil
	}
	for _, list := range [][]ResourceNode{t.Nodes, t.OrphanedNodes} {
		for i := range list {
			n := &list[i]
			if n.Group == group && n.Kind == kind && n.Namespace == namespace && n.Name == name {
				return n
			}
		}
	}
	return nil
}
