package compare

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

// TreeInput is a resource tree request: the managed resources of the latest comparison.
type TreeInput struct {
	App     *v1alpha1.Application
	Project *v1alpha1.AppProject
	// Cluster is the destination cluster (resolved from the Application when nil).
	Cluster *v1alpha1.Cluster
	// Managed are Result.Managed of the latest comparison.
	Managed []ManagedResource
}

// TreeResult is a resource tree and what is derived from it.
type TreeResult struct {
	Tree *ApplicationTree
	// Summary is status.summary.
	Summary v1alpha1.ApplicationSummary
	// OrphanedResources is len(Tree.OrphanedNodes).
	OrphanedResources int
	// Conditions holds OrphanedResourceWarning when the project warns about orphans.
	Conditions []Condition
	// EvaluatedConditionTypes is [OrphanedResourceWarning].
	EvaluatedConditionTypes []string
}

// ResourceTree builds the resource tree of an Application from the live-state cache
// (upstream getResourceTree): managed resources missing live are Missing nodes; existing ones
// are traversed with their descendants (owner references and synthesized relations),
// pruning descendants the project does not permit; with orphaned-resource monitoring, the
// top-level resources of the destination namespace that are not managed, are permitted and
// are not excluded are traversed too, skipping those attributed to another existing
// Application. It is used after each comparison and for tree-only refreshes.
func (m *Manager) ResourceTree(ctx context.Context, in TreeInput) (*TreeResult, error) {
	if in.App == nil || in.Project == nil {
		return nil, errors.Join(ErrInvalidInput, errors.New("application and project are required"))
	}
	cluster := in.Cluster
	if cluster == nil {
		c, err := m.d.Clusters.DestinationCluster(in.App.Spec.Destination)
		if err != nil {
			return nil, err
		}
		cluster = c
	}
	cc, err := m.d.LiveState.ClusterCache(ctx, cluster)
	if err != nil {
		return nil, err
	}
	var allowedNodeLabels []string
	if st := m.d.Settings.Current(); st != nil {
		allowedNodeLabels = st.AllowedNodeLabels()
	}
	return m.buildTree(ctx, cc, in.App, in.Project, cluster, in.Managed, allowedNodeLabels), nil
}

func (m *Manager) buildTree(ctx context.Context, cc cache.ClusterCache, app *v1alpha1.Application, proj *v1alpha1.AppProject, cluster *v1alpha1.Cluster, managed []ManagedResource, allowedNodeLabels []string) *TreeResult {
	permitted := func(group, kind, name, namespace string) bool {
		ok, _ := project.IsResourcePermitted(ctx, proj, schema.GroupKind{Group: group, Kind: kind}, name, namespace, cluster, m.d.Clusters)
		return ok
	}

	var orphanCandidates map[ResourceKey]*cache.Resource
	warn := false
	if project.OrphanedResourcesEnabled(proj) {
		orphanCandidates = cc.FindResources(app.Spec.Destination.Namespace, cache.TopLevelResource)
		warn = project.OrphanedResourcesWarn(proj)
	}

	nodes := []ResourceNode{}
	var liveKeys []ResourceKey
	for _, mr := range managed {
		delete(orphanCandidates, mr.Key)
		if mr.Live == nil {
			if mr.Target == nil {
				continue
			}
			nodes = append(nodes, ResourceNode{
				ResourceRef: ResourceRef{
					Group: mr.Key.Group, Version: mr.Target.GroupVersionKind().Version, Kind: mr.Key.Kind,
					Namespace: mr.Key.Namespace, Name: mr.Key.Name,
				},
				Health: &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusMissing},
			})
			continue
		}
		liveKeys = append(liveKeys, cache.KeyOf(mr.Live))
	}
	cc.IterateHierarchy(liveKeys, func(r *cache.Resource, ns map[ResourceKey]*cache.Resource) bool {
		node := asResourceNode(r, ns)
		if !permitted(node.Group, node.Kind, node.Name, node.Namespace) {
			return false
		}
		nodes = append(nodes, node)
		return true
	})

	orphans := []ResourceNode{}
	var orphanKeys []ResourceKey
	for k := range orphanCandidates {
		vk := v1alpha1.ResourceKey{Group: k.Group, Kind: k.Kind, Namespace: k.Namespace, Name: k.Name}
		if k.Namespace != "" && proj.Spec.OrphanedResources != nil && project.IsOrphanCandidate(proj, vk) {
			orphanKeys = append(orphanKeys, k)
		}
	}
	slices.SortFunc(orphanKeys, func(a, b ResourceKey) int { return strings.Compare(a.String(), b.String()) })
	appExists := m.d.ApplicationExists
	cc.IterateHierarchy(orphanKeys, func(r *cache.Resource, ns map[ResourceKey]*cache.Resource) bool {
		if name := AppOf(r, ns); name != "" && (appExists == nil || appExists(name)) {
			return false
		}
		node := asResourceNode(r, ns)
		if !permitted(node.Group, node.Kind, node.Name, node.Namespace) {
			return false
		}
		orphans = append(orphans, node)
		return true
	})

	byFullName := func(a, b ResourceNode) int { return strings.Compare(a.FullName(), b.FullName()) }
	slices.SortStableFunc(nodes, byFullName)
	slices.SortStableFunc(orphans, byFullName)
	tree := &ApplicationTree{Nodes: nodes, OrphanedNodes: orphans, Hosts: appHosts(cc, nodes, allowedNodeLabels)}

	res := &TreeResult{
		Tree:                    tree,
		Summary:                 treeSummary(tree, app),
		OrphanedResources:       len(orphans),
		EvaluatedConditionTypes: []string{v1alpha1.ApplicationConditionOrphanedResourceWarning},
	}
	if len(orphans) > 0 && warn {
		res.Conditions = []Condition{newCondition(v1alpha1.ApplicationConditionOrphanedResourceWarning, v1alpha1.ReasonOrphanedResources,
			project.OrphanedResourceWarningMessage(len(orphans)))}
	}
	return res
}

// asResourceNode converts a cached resource to a tree node (upstream asResourceNode). A
// parent reference gets the child's namespace when the parent is found there, else "" (a
// cluster-scoped owner).
func asResourceNode(r *cache.Resource, namespaceResources map[ResourceKey]*cache.Resource) ResourceNode {
	gv, err := schema.ParseGroupVersion(r.Ref.APIVersion)
	if err != nil {
		gv = schema.GroupVersion{}
	}
	parents := make([]ResourceRef, 0, len(r.OwnerRefs))
	for _, o := range r.OwnerRefs {
		ogv, err := schema.ParseGroupVersion(o.APIVersion)
		if err != nil {
			ogv = schema.GroupVersion{}
		}
		p := ResourceRef{Group: ogv.Group, Version: ogv.Version, Kind: o.Kind, Name: o.Name, UID: string(o.UID)}
		if parent, ok := namespaceResources[ResourceKey{Group: ogv.Group, Kind: o.Kind, Namespace: r.Ref.Namespace, Name: o.Name}]; ok {
			p.Namespace = parent.Ref.Namespace
		}
		parents = append(parents, p)
	}
	info := InfoOf(r)
	node := ResourceNode{
		ResourceRef: ResourceRef{
			Group: gv.Group, Version: gv.Version, Kind: r.Ref.Kind, Namespace: r.Ref.Namespace,
			Name: r.Ref.Name, UID: string(r.Ref.UID),
		},
		ParentRefs:      parents,
		Info:            slices.Clone(info.Info),
		ResourceVersion: r.ResourceVersion,
		Images:          slices.Clone(info.Images),
		NetworkingInfo:  cloneNetworkingInfo(info.NetworkingInfo),
	}
	if len(node.ParentRefs) == 0 {
		node.ParentRefs = nil
	}
	if info.Health != nil {
		node.Health = &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusCode(info.Health.Status), Message: info.Health.Message}
	}
	if r.CreationTimestamp != nil {
		t := *r.CreationTimestamp
		node.CreatedAt = &t
	}
	return node
}

func cloneNetworkingInfo(n *NetworkingInfo) *NetworkingInfo {
	if n == nil {
		return nil
	}
	return &NetworkingInfo{
		TargetLabels: maps.Clone(n.TargetLabels),
		TargetRefs:   slices.Clone(n.TargetRefs),
		Labels:       maps.Clone(n.Labels),
		Ingress:      slices.Clone(n.Ingress),
		ExternalURLs: slices.Clone(n.ExternalURLs),
	}
}

var hostResourceNames = map[corev1.ResourceName]bool{
	corev1.ResourceCPU:     true,
	corev1.ResourceStorage: true,
	corev1.ResourceMemory:  true,
}

// appHosts lists the nodes running the Application's pods with capacity and requests of cpu,
// memory and storage (upstream getAppHosts). Neighbor pods in a terminal phase do not count.
func appHosts(cc cache.ClusterCache, appNodes []ResourceNode, allowedNodeLabels []string) []HostInfo {
	appPods := map[ResourceKey]bool{}
	for _, n := range appNodes {
		if n.Group == "" && n.Kind == "Pod" {
			appPods[n.Key()] = true
		}
	}
	if len(appPods) == 0 {
		return nil
	}
	nodeInfos := map[string]NodeInfo{}
	appPodsByNode := map[string][]PodInfo{}
	otherPodsByNode := map[string][]PodInfo{}
	cc.FindResources("", func(r *cache.Resource) bool {
		key := r.ResourceKey()
		info, ok := r.Info.(*ResourceInfo)
		if !ok || info == nil || key.Group != "" {
			return false
		}
		switch {
		case key.Kind == "Node" && info.NodeInfo != nil:
			nodeInfos[key.Name] = *info.NodeInfo
		case key.Kind == "Pod" && info.PodInfo != nil:
			if appPods[key] {
				appPodsByNode[info.PodInfo.NodeName] = append(appPodsByNode[info.PodInfo.NodeName], *info.PodInfo)
			} else {
				otherPodsByNode[info.PodInfo.NodeName] = append(otherPodsByNode[info.PodInfo.NodeName], *info.PodInfo)
			}
		}
		return false
	})
	var hosts []HostInfo
	for _, nodeName := range slices.Sorted(maps.Keys(appPodsByNode)) {
		node, ok := nodeInfos[nodeName]
		if !ok {
			continue
		}
		resources := map[corev1.ResourceName]*HostResourceInfo{}
		get := func(n corev1.ResourceName) *HostResourceInfo {
			if r, ok := resources[n]; ok {
				return r
			}
			r := &HostResourceInfo{ResourceName: n}
			resources[n] = r
			return r
		}
		for name, q := range node.Capacity {
			get(name).Capacity += q.MilliValue()
		}
		for _, pod := range appPodsByNode[nodeName] {
			for name, q := range pod.ResourceRequests {
				if hostResourceNames[name] {
					get(name).RequestedByApp += q.MilliValue()
				}
			}
		}
		for _, pod := range otherPodsByNode[nodeName] {
			if podTerminal(pod.Phase) {
				continue
			}
			for name, q := range pod.ResourceRequests {
				if hostResourceNames[name] {
					get(name).RequestedByNeighbors += q.MilliValue()
				}
			}
		}
		var infos []HostResourceInfo
		for _, name := range slices.Sorted(maps.Keys(resources)) {
			r := resources[name]
			if hostResourceNames[name] && r.Capacity > 0 {
				infos = append(infos, *r)
			}
		}
		labels := map[string]string{}
		for _, l := range allowedNodeLabels {
			if v, ok := node.Labels[l]; ok {
				labels[l] = v
			}
		}
		hosts = append(hosts, HostInfo{Name: nodeName, ResourcesInfo: infos, SystemInfo: node.SystemInfo, Labels: labels})
	}
	return hosts
}

// treeSummary computes status.summary from the managed nodes (upstream GetSummary): unique
// sorted external URLs (including the Application's own link annotations, not validated, as
// upstream), unique sorted images, and whether a child Application is managed.
func treeSummary(tree *ApplicationTree, app *v1alpha1.Application) v1alpha1.ApplicationSummary {
	urls := map[string]bool{}
	images := map[string]bool{}
	appOfApps := false
	for _, n := range tree.Nodes {
		if n.NetworkingInfo != nil {
			for _, u := range n.NetworkingInfo.ExternalURLs {
				urls[u] = true
			}
		}
		for _, img := range n.Images {
			images[img] = true
		}
		if n.Group == v1alpha1.GroupName && n.Kind == "Application" {
			appOfApps = true
		}
	}
	for k, v := range app.Annotations {
		if strings.HasPrefix(k, v1alpha1.AnnotationKeyLinkPrefix) {
			urls[v] = true
		}
	}
	s := v1alpha1.ApplicationSummary{IsAppOfApps: appOfApps}
	if len(urls) > 0 {
		s.ExternalURLs = slices.Sorted(maps.Keys(urls))
	}
	if len(images) > 0 {
		s.Images = slices.Sorted(maps.Keys(images))
	}
	return s
}
