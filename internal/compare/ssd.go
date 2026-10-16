package compare

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	openapiproto "k8s.io/kube-openapi/pkg/util/proto"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
)

// DefaultSchemaTTL is how long a cluster's OpenAPI type parser and dynamic client are reused.
const DefaultSchemaTTL = 10 * time.Minute

// KubeServerSideDiff is the ServerSideDiffProvider talking to destination clusters directly.
// Dry runs use the untuned cluster config without impersonation, as upstream
// (cluster.RawRestConfig); kinds are resolved through the live-state cache's discovery data.
type KubeServerSideDiff struct {
	config ClusterConfigFunc
	live   LiveStateProvider
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	parsers map[string]parserEntry
	clients map[string]clientEntry
}

type parserEntry struct {
	parser  *managedfields.GvkParser
	version string
	at      time.Time
}

type clientEntry struct {
	client dynamic.Interface
	at     time.Time
}

// NewKubeServerSideDiff returns a provider. config should return the raw cluster config
// (*clusters.Registry RawRestConfig); ttl <= 0 selects DefaultSchemaTTL.
func NewKubeServerSideDiff(config ClusterConfigFunc, live LiveStateProvider, ttl time.Duration) (*KubeServerSideDiff, error) {
	if config == nil || live == nil {
		return nil, errors.New("compare: server-side diff needs a cluster config function and a live state provider")
	}
	if ttl <= 0 {
		ttl = DefaultSchemaTTL
	}
	return &KubeServerSideDiff{
		config: config, live: live, ttl: ttl, now: time.Now,
		parsers: map[string]parserEntry{}, clients: map[string]clientEntry{},
	}, nil
}

// DryRunner implements ServerSideDiffProvider.
func (k *KubeServerSideDiff) DryRunner(ctx context.Context, cluster *v1alpha1.Cluster) (diff.DryRunner, error) {
	cc, err := k.live.ClusterCache(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("error getting cluster cache: %w", err)
	}
	dyn, err := k.dynamicClient(ctx, cluster)
	if err != nil {
		return nil, err
	}
	resources := map[schema.GroupKind]cache.APIResourceInfo{}
	for _, r := range cc.GetAPIResources() {
		resources[r.GroupKind] = r
	}
	return &kubeDryRunner{dyn: dyn, resources: resources}, nil
}

func (k *KubeServerSideDiff) dynamicClient(ctx context.Context, cluster *v1alpha1.Cluster) (dynamic.Interface, error) {
	key := ClusterKey(cluster)
	now := k.now()
	k.mu.Lock()
	if e, ok := k.clients[key]; ok && now.Sub(e.at) < k.ttl {
		k.mu.Unlock()
		return e.client, nil
	}
	k.mu.Unlock()
	cfg, err := k.config(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("error getting cluster REST config: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("error creating dynamic client: %w", err)
	}
	k.mu.Lock()
	k.clients[key] = clientEntry{client: dyn, at: now}
	k.mu.Unlock()
	return dyn, nil
}

// GVKParser implements ServerSideDiffProvider. The parser is rebuilt after the TTL or when
// the cluster's server version or API count changes.
func (k *KubeServerSideDiff) GVKParser(ctx context.Context, cluster *v1alpha1.Cluster) (*managedfields.GvkParser, error) {
	cc, err := k.live.ClusterCache(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("error getting cluster cache: %w", err)
	}
	info := cc.GetClusterInfo()
	version := fmt.Sprintf("%s/%d", info.K8SVersion, info.APIsCount)
	key := ClusterKey(cluster)
	now := k.now()
	k.mu.Lock()
	if e, ok := k.parsers[key]; ok && e.version == version && now.Sub(e.at) < k.ttl {
		k.mu.Unlock()
		return e.parser, nil
	}
	k.mu.Unlock()
	cfg, err := k.config(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("error getting cluster REST config: %w", err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("error creating discovery client: %w", err)
	}
	doc, err := dc.OpenAPISchema()
	if err != nil {
		return nil, fmt.Errorf("error loading OpenAPI schema of cluster %q: %w", key, err)
	}
	models, err := openapiproto.NewOpenAPIData(doc)
	if err != nil {
		return nil, fmt.Errorf("error parsing OpenAPI schema of cluster %q: %w", key, err)
	}
	parser, err := managedfields.NewGVKParser(models, false)
	if err != nil {
		return nil, fmt.Errorf("error building type parser of cluster %q: %w", key, err)
	}
	k.mu.Lock()
	k.parsers[key] = parserEntry{parser: parser, version: version, at: now}
	k.mu.Unlock()
	return parser, nil
}

// kubeDryRunner performs server-side apply dry runs (dryRun=All, force) with a dynamic client.
type kubeDryRunner struct {
	dyn       dynamic.Interface
	resources map[schema.GroupKind]cache.APIResourceInfo
}

// ServerSideApplyDryRun implements diff.DryRunner.
func (d *kubeDryRunner) ServerSideApplyDryRun(ctx context.Context, obj *unstructured.Unstructured, manager string) (*unstructured.Unstructured, error) {
	gvk := obj.GroupVersionKind()
	info, ok := d.resources[gvk.GroupKind()]
	if !ok {
		return nil, fmt.Errorf("server-side dry run: the server does not serve %s", gvk.String())
	}
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("server-side dry run: encode %s %s: %w", gvk.Kind, obj.GetName(), err)
	}
	gvr := schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: info.Meta.Name}
	var ri dynamic.ResourceInterface = d.dyn.Resource(gvr)
	if info.Meta.Namespaced {
		ri = d.dyn.Resource(gvr).Namespace(obj.GetNamespace())
	}
	return ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{
		DryRun:       []string{metav1.DryRunAll},
		FieldManager: manager,
		Force:        ptr.To(true),
	})
}
