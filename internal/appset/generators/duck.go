package generators

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// MaxClusterDecisionResources caps the number of duck-typed resources read per generation.
// More matching resources fail the generation instead of returning a partial list: the
// Applications of the missing decisions would otherwise be deleted as stale.
const MaxClusterDecisionResources = 1000

// clusterDecisionPageSize is the List page size used to read cluster decision resources.
const clusterDecisionPageSize = 250

// ErrTooManyClusterDecisionResources is returned when more than MaxClusterDecisionResources
// resources match a Cluster Decision Resource generator.
var ErrTooManyClusterDecisionResources = fmt.Errorf("more than %d cluster decision resources match", MaxClusterDecisionResources)

// DuckTypeGenerator is the Cluster Decision Resource generator: it reads the cluster decisions
// from the status of duck-typed resources (e.g. an OCM PlacementDecision) described by a
// ConfigMap.
type DuckTypeGenerator struct {
	cfg Config
}

// RequeueAfter implements Generator: requeueAfterSeconds, else the controller default (3m).
func (g *DuckTypeGenerator) RequeueAfter(gen *v1alpha1.ApplicationSetGenerator) time.Duration {
	return requeueSeconds(gen.ClusterDecisionResource.RequeueAfterSeconds, g.cfg.defaultRequeueAfter())
}

// Template implements Generator.
func (g *DuckTypeGenerator) Template(gen *v1alpha1.ApplicationSetGenerator) *v1alpha1.ApplicationSetTemplate {
	return &gen.ClusterDecisionResource.Template
}

// GenerateParams implements Generator.
//
// The ConfigMap (control-plane namespace) gives apiVersion, kind (used as the resource plural),
// statusListKey (default "clusters") and matchKey. Resources are selected by name or label
// selector (exactly one). Each decision whose matchKey value equals a registered cluster name
// yields name, server, every decision key formatted with %v, and the generator values copied
// verbatim (not templated).
func (g *DuckTypeGenerator) GenerateParams(ctx context.Context, gen *v1alpha1.ApplicationSetGenerator, appSet *v1alpha1.ApplicationSet) ([]map[string]any, error) {
	if gen == nil || gen.ClusterDecisionResource == nil {
		return nil, ErrEmptyAppSetGenerator
	}
	if g.cfg.Dynamic == nil {
		return nil, errors.New("cluster decision resource generator: no dynamic client configured")
	}
	cdr := gen.ClusterDecisionResource
	log := g.cfg.logger().With(appSetLogAttrs(appSet))

	clusters, err := ListClusters(ctx, g.cfg.Client, g.cfg.Namespace)
	if err != nil {
		return nil, err
	}
	cm := &corev1.ConfigMap{}
	if err := g.cfg.Client.Get(ctx, client.ObjectKey{Namespace: g.cfg.Namespace, Name: cdr.ConfigMapRef}, cm); err != nil {
		return nil, fmt.Errorf("error reading configMapRef: %w", err)
	}
	apiVersion := cm.Data["apiVersion"]
	kind := cm.Data["kind"]
	versionIdx := strings.Index(apiVersion, "/")
	if kind == "" || versionIdx < 1 {
		return nil, errors.New("there is a problem with the apiVersion, kind or resourceName provided")
	}
	hasSelector := cdr.LabelSelector.MatchLabels != nil || cdr.LabelSelector.MatchExpressions != nil
	if (cdr.Name == "" && !hasSelector) || (cdr.Name != "" && hasSelector) {
		return nil, errors.New("there is a problem with the definition of the ClusterDecisionResource generator")
	}
	gvr := schema.GroupVersionResource{Group: apiVersion[:versionIdx], Version: apiVersion[versionIdx+1:], Resource: kind}

	opts := metav1.ListOptions{Limit: clusterDecisionPageSize}
	if cdr.Name == "" {
		opts.LabelSelector = metav1.FormatLabelSelector(&cdr.LabelSelector)
	} else {
		opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", cdr.Name).String()
	}
	items, err := g.listDecisionResources(ctx, gvr, opts)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, errors.New("no clusterDecisionResources found")
	}
	statusListKey := cm.Data["statusListKey"]
	if statusListKey == "" {
		statusListKey = "clusters"
	}
	matchKey := cm.Data["matchKey"]
	if matchKey == "" {
		log.WarnContext(ctx, "matchKey not found in the cluster decision resource ConfigMap", slog.String("configMap", cm.Name))
		return nil, nil
	}

	var decisions []map[string]any
	for _, item := range items {
		status, ok := item.Object["status"].(map[string]any)
		if !ok || len(status) == 0 {
			log.WarnContext(ctx, "cluster decision resource has no status", slog.String("resource", item.GetName()))
			continue
		}
		raw, ok := status[statusListKey]
		if !ok || raw == nil {
			continue
		}
		items, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("cluster decision resource %s: status.%s is not a list", item.GetName(), statusListKey)
		}
		for _, d := range items {
			decision, ok := d.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("cluster decision resource %s: status.%s contains a non-object entry", item.GetName(), statusListKey)
			}
			decisions = append(decisions, decision)
		}
	}
	if len(decisions) == 0 {
		log.WarnContext(ctx, "cluster decision resource status list missing", slog.String("statusListKey", statusListKey))
		return nil, nil
	}

	res := []map[string]any{}
	for _, decision := range decisions {
		cluster := findCluster(clusters, decision, matchKey)
		if cluster == nil {
			continue
		}
		params := map[string]any{"name": cluster.Name, "server": cluster.Server}
		for k, v := range decision {
			params[k] = fmt.Sprintf("%v", v)
		}
		for k, v := range cdr.Values {
			if appSet.Spec.GoTemplate {
				if params["values"] == nil {
					params["values"] = map[string]string{}
				}
				params["values"].(map[string]string)[k] = v
			} else {
				params["values."+k] = v
			}
		}
		res = append(res, params)
	}
	return res, nil
}

func findCluster(clusters []ClusterSpecifier, decision map[string]any, matchKey string) *ClusterSpecifier {
	v, ok := decision[matchKey]
	if !ok || v == nil {
		return nil
	}
	s := fmt.Sprintf("%v", v)
	if s == "" {
		return nil
	}
	for i := range clusters {
		if clusters[i].Name == s {
			return &clusters[i]
		}
	}
	return nil
}

// listDecisionResources reads every matching resource, page by page. It fails rather than
// returning a partial list when more than MaxClusterDecisionResources match (a server that
// ignores the page size counts too).
func (g *DuckTypeGenerator) listDecisionResources(ctx context.Context, gvr schema.GroupVersionResource, opts metav1.ListOptions) ([]unstructured.Unstructured, error) {
	var items []unstructured.Unstructured
	for page := 0; ; page++ {
		if page > 2*MaxClusterDecisionResources/clusterDecisionPageSize+2 {
			return nil, fmt.Errorf("failed to get dynamic resources: the list did not complete after %d pages", page)
		}
		list, err := g.cfg.Dynamic.Resource(gvr).Namespace(g.cfg.Namespace).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to get dynamic resources: %w", err)
		}
		if len(list.Items) > MaxClusterDecisionResources-len(items) {
			return nil, ErrTooManyClusterDecisionResources
		}
		items = append(items, list.Items...)
		next := list.GetContinue()
		if next == "" {
			return items, nil
		}
		if len(items) >= MaxClusterDecisionResources {
			return nil, ErrTooManyClusterDecisionResources
		}
		opts.Continue = next
	}
}
