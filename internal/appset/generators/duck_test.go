package generators

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

var placementGVR = schema.GroupVersionResource{Group: "cluster.open-cluster-management.io", Version: "v1beta1", Resource: "placementdecisions"}

func placementDecision(name string, labels map[string]string, decisions ...map[string]any) *unstructured.Unstructured {
	list := make([]any, 0, len(decisions))
	for _, d := range decisions {
		list = append(list, d)
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cluster.open-cluster-management.io/v1beta1",
		"kind":       "PlacementDecision",
		"metadata":   map[string]any{"name": name, "namespace": controlNS},
		"status":     map[string]any{"decisions": list},
	}}
	u.SetLabels(labels)
	return u
}

func duckGenerator(t *testing.T, objs []runtime.Object, cm map[string]string) *DuckTypeGenerator {
	t.Helper()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{placementGVR: "PlacementDecisionList"}, objs...)
	kube := fakeClient(t,
		cluster("c1", "c1", "https://c1.example", nil, nil),
		cluster("c2", "c2", "https://c2.example", nil, nil),
		configMap(controlNS, "ocm", cm),
	)
	return &DuckTypeGenerator{cfg: Config{Client: kube, Namespace: controlNS, Dynamic: dyn}}
}

var ocmConfigMap = map[string]string{
	"apiVersion":    "cluster.open-cluster-management.io/v1beta1",
	"kind":          "placementdecisions",
	"statusListKey": "decisions",
	"matchKey":      "clusterName",
}

func TestDuckTypeGenerator(t *testing.T) {
	t.Parallel()
	objs := []runtime.Object{
		placementDecision("pd-prod", map[string]string{"env": "prod"},
			map[string]any{"clusterName": "c1", "reason": "fit"},
			map[string]any{"clusterName": "unknown"},
		),
		placementDecision("pd-dev", map[string]string{"env": "dev"}, map[string]any{"clusterName": "c2"}),
	}
	g := duckGenerator(t, objs, ocmConfigMap)
	gen := &v1alpha1.ApplicationSetGenerator{ClusterDecisionResource: &v1alpha1.DuckTypeGenerator{
		ConfigMapRef:  "ocm",
		LabelSelector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
		Values:        map[string]string{"tier": "{{name}}"},
	}}
	got, err := g.GenerateParams(context.Background(), gen, appSet(false))
	require.NoError(t, err)
	assert.Equal(t, []map[string]any{{
		"name": "c1", "server": "https://c1.example", "clusterName": "c1", "reason": "fit",
		"values.tier": "{{name}}",
	}}, got, "decisions without a registered cluster are dropped; values are not templated")

	got, err = g.GenerateParams(context.Background(), gen, appSet(true))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"tier": "{{name}}"}, got[0]["values"])

	assert.Equal(t, DefaultRequeueAfter, g.RequeueAfter(gen))
	g.cfg.DefaultRequeueAfter = time.Minute
	assert.Equal(t, time.Minute, g.RequeueAfter(gen))
	gen.ClusterDecisionResource.RequeueAfterSeconds = ptr.To[int64](10)
	assert.Equal(t, 10*time.Second, g.RequeueAfter(gen))
}

func TestDuckTypeGeneratorByName(t *testing.T) {
	t.Parallel()
	g := duckGenerator(t, []runtime.Object{placementDecision("pd", nil, map[string]any{"clusterName": "c2"})}, ocmConfigMap)
	got, err := g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{ClusterDecisionResource: &v1alpha1.DuckTypeGenerator{
		ConfigMapRef: "ocm", Name: "pd",
	}}, appSet(false))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "https://c2.example", got[0]["server"])
}

func TestDuckTypeGeneratorErrors(t *testing.T) {
	t.Parallel()
	objs := []runtime.Object{placementDecision("pd", nil, map[string]any{"clusterName": "c2"})}
	sel := metav1.LabelSelector{MatchLabels: map[string]string{"x": "y"}}
	tests := []struct {
		name    string
		cm      map[string]string
		gen     v1alpha1.DuckTypeGenerator
		wantErr string
		wantNil bool
	}{
		{name: "missing configmap", cm: ocmConfigMap, gen: v1alpha1.DuckTypeGenerator{ConfigMapRef: "nope", Name: "pd"}, wantErr: "error reading configMapRef"},
		{name: "bad apiVersion", cm: map[string]string{"apiVersion": "v1", "kind": "x"}, gen: v1alpha1.DuckTypeGenerator{ConfigMapRef: "ocm", Name: "pd"},
			wantErr: "there is a problem with the apiVersion, kind or resourceName provided"},
		{name: "both name and selector", cm: ocmConfigMap, gen: v1alpha1.DuckTypeGenerator{ConfigMapRef: "ocm", Name: "pd", LabelSelector: sel},
			wantErr: "there is a problem with the definition of the ClusterDecisionResource generator"},
		{name: "neither", cm: ocmConfigMap, gen: v1alpha1.DuckTypeGenerator{ConfigMapRef: "ocm"},
			wantErr: "there is a problem with the definition of the ClusterDecisionResource generator"},
		{name: "no resources", cm: ocmConfigMap, gen: v1alpha1.DuckTypeGenerator{ConfigMapRef: "ocm", LabelSelector: sel}, wantErr: "no clusterDecisionResources found"},
		{name: "no matchKey", cm: map[string]string{"apiVersion": ocmConfigMap["apiVersion"], "kind": "placementdecisions", "statusListKey": "decisions"},
			gen: v1alpha1.DuckTypeGenerator{ConfigMapRef: "ocm", Name: "pd"}, wantNil: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := duckGenerator(t, objs, tt.cm)
			gen := tt.gen
			got, err := g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{ClusterDecisionResource: &gen}, appSet(false))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, got)
			}
		})
	}
	g := &DuckTypeGenerator{cfg: Config{Client: fakeClient(t), Namespace: controlNS}}
	_, err := g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{ClusterDecisionResource: &v1alpha1.DuckTypeGenerator{ConfigMapRef: "x"}}, appSet(false))
	require.ErrorContains(t, err, "no dynamic client configured")
}

// TestDuckTypeGeneratorPaging: every page of decision resources is read; more resources than
// MaxClusterDecisionResources fail the generation instead of returning a partial list (whose
// missing decisions would have their Applications deleted).
func TestDuckTypeGeneratorPaging(t *testing.T) {
	t.Parallel()
	gen := &v1alpha1.ApplicationSetGenerator{ClusterDecisionResource: &v1alpha1.DuckTypeGenerator{
		ConfigMapRef:  "ocm",
		LabelSelector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
	}}
	// pager serves total resources in pages of the requested size; decide names the cluster
	// of each resource.
	pager := func(total int, decide func(i int) string, ignoreLimit bool, requests *[]metav1.ListOptions) *DuckTypeGenerator {
		g := duckGenerator(t, nil, ocmConfigMap)
		fake := g.cfg.Dynamic.(*dynfake.FakeDynamicClient)
		var mu sync.Mutex
		fake.PrependReactor("list", "placementdecisions", func(action k8stesting.Action) (bool, runtime.Object, error) {
			opts := action.(k8stesting.ListActionImpl).ListOptions
			mu.Lock()
			*requests = append(*requests, opts)
			mu.Unlock()
			start := 0
			if opts.Continue != "" {
				start, _ = strconv.Atoi(opts.Continue)
			}
			end := total
			if !ignoreLimit && opts.Limit > 0 && start+int(opts.Limit) < total {
				end = start + int(opts.Limit)
			}
			list := &unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "cluster.open-cluster-management.io/v1beta1", "kind": "PlacementDecisionList"}}
			for i := start; i < end; i++ {
				list.Items = append(list.Items, *placementDecision(fmt.Sprintf("pd-%d", i), map[string]string{"env": "prod"}, map[string]any{"clusterName": decide(i)}))
			}
			if end < total {
				list.SetContinue(strconv.Itoa(end))
			}
			return true, list, nil
		})
		return g
	}

	t.Run("decisions on later pages", func(t *testing.T) {
		t.Parallel()
		var requests []metav1.ListOptions
		g := pager(600, func(i int) string {
			switch i {
			case 0:
				return "c1"
			case 599:
				return "c2"
			}
			return "unknown"
		}, false, &requests)
		got, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.NoError(t, err)
		names := []any{}
		for _, p := range got {
			names = append(names, p["name"])
		}
		assert.Equal(t, []any{"c1", "c2"}, names, "the decision on the last page is kept")
		require.Len(t, requests, 3)
		assert.Equal(t, []string{"", "250", "500"}, []string{requests[0].Continue, requests[1].Continue, requests[2].Continue})
		for _, r := range requests {
			assert.EqualValues(t, 250, r.Limit)
			assert.Equal(t, "env=prod", r.LabelSelector)
		}
	})

	t.Run("more resources than the cap", func(t *testing.T) {
		t.Parallel()
		var requests []metav1.ListOptions
		g := pager(MaxClusterDecisionResources+1, func(int) string { return "c1" }, false, &requests)
		_, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.ErrorIs(t, err, ErrTooManyClusterDecisionResources)
	})

	t.Run("server ignoring the page size", func(t *testing.T) {
		t.Parallel()
		var requests []metav1.ListOptions
		g := pager(MaxClusterDecisionResources+1, func(int) string { return "c1" }, true, &requests)
		_, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.ErrorIs(t, err, ErrTooManyClusterDecisionResources)
		assert.Len(t, requests, 1)
	})

	t.Run("exactly the cap", func(t *testing.T) {
		t.Parallel()
		var requests []metav1.ListOptions
		g := pager(MaxClusterDecisionResources, func(int) string { return "c1" }, false, &requests)
		got, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.NoError(t, err)
		assert.Len(t, got, MaxClusterDecisionResources)
	})

	t.Run("endless empty pages", func(t *testing.T) {
		t.Parallel()
		g := duckGenerator(t, nil, ocmConfigMap)
		fake := g.cfg.Dynamic.(*dynfake.FakeDynamicClient)
		fake.PrependReactor("list", "placementdecisions", func(k8stesting.Action) (bool, runtime.Object, error) {
			list := &unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "cluster.open-cluster-management.io/v1beta1", "kind": "PlacementDecisionList"}}
			list.SetContinue("again")
			return true, list, nil
		})
		_, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.ErrorContains(t, err, "the list did not complete")
	})
}
