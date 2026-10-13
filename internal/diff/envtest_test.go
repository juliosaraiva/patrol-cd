package diff

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	openapiproto "k8s.io/kube-openapi/pkg/util/proto"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// kubeDryRunner is a DryRunner backed by a real API server (server-side apply, dryRun=All,
// force=true), as the controller implements it.
type kubeDryRunner struct {
	client dynamic.Interface
	mapper *restmapper.DeferredDiscoveryRESTMapper
}

func (k *kubeDryRunner) resource(u *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	gvk := u.GroupVersionKind()
	m, err := k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}
	if m.Scope.Name() == "namespace" {
		return k.client.Resource(m.Resource).Namespace(u.GetNamespace()), nil
	}
	return k.client.Resource(m.Resource), nil
}

func (k *kubeDryRunner) apply(ctx context.Context, u *unstructured.Unstructured, manager string, dryRun bool) (*unstructured.Unstructured, error) {
	ri, err := k.resource(u)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(u.Object)
	if err != nil {
		return nil, err
	}
	opts := metav1.PatchOptions{FieldManager: manager, Force: ptr.To(true)}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	return ri.Patch(ctx, u.GetName(), types.ApplyPatchType, data, opts)
}

func (k *kubeDryRunner) ServerSideApplyDryRun(ctx context.Context, u *unstructured.Unstructured, manager string) (*unstructured.Unstructured, error) {
	return k.apply(ctx, u, manager, true)
}

func (k *kubeDryRunner) get(ctx context.Context, u *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	ri, err := k.resource(u)
	if err != nil {
		return nil, err
	}
	return ri.Get(ctx, u.GetName(), metav1.GetOptions{})
}

func (k *kubeDryRunner) create(ctx context.Context, u *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	ri, err := k.resource(u)
	if err != nil {
		return nil, err
	}
	return ri.Create(ctx, u, metav1.CreateOptions{FieldManager: "kubectl-client-side-apply"})
}

// Shared envtest control plane: started lazily by the first test that needs it and stopped
// in TestMain, so several envtest-backed tests pay the start-up cost once.
var (
	envOnce   sync.Once
	testEnv   *envtest.Environment
	envClient *kubeDryRunner
	envParser *managedfields.GvkParser
	envErr    error
)

// envtestClients returns a client for the shared envtest API server and a GvkParser built
// from its OpenAPI document. The test is skipped when KUBEBUILDER_ASSETS is not set.
func envtestClients(t *testing.T) (*kubeDryRunner, *managedfields.GvkParser) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping envtest-backed test")
	}
	envOnce.Do(func() {
		testEnv = &envtest.Environment{}
		cfg, err := testEnv.Start()
		if err != nil {
			envErr = err
			return
		}
		dc, err := discovery.NewDiscoveryClientForConfig(cfg)
		if err != nil {
			envErr = err
			return
		}
		dyn, err := dynamic.NewForConfig(cfg)
		if err != nil {
			envErr = err
			return
		}
		envClient = &kubeDryRunner{client: dyn, mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))}
		doc, err := dc.OpenAPISchema()
		if err != nil {
			envErr = err
			return
		}
		models, err := openapiproto.NewOpenAPIData(doc)
		if err != nil {
			envErr = err
			return
		}
		envParser, envErr = managedfields.NewGVKParser(models, false)
	})
	require.NoError(t, envErr)
	return envClient, envParser
}

func TestMain(m *testing.M) {
	code := m.Run()
	if testEnv != nil {
		_ = testEnv.Stop()
	}
	os.Exit(code)
}

// TestEnvtestStrategiesAgree applies manifests to a real kube-apiserver and checks that the
// three strategies report no drift for freshly applied objects (with real server defaults)
// and report drift for real changes.
func TestEnvtestStrategiesAgree(t *testing.T) {
	k, p := envtestClients(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	manifests := []string{deployConfig, serviceConfig, stsConfig, webhookConfig}
	for i, m := range manifests {
		t.Run(fmt.Sprintf("manifest-%d", i), func(t *testing.T) {
			config := obj(t, m)
			config.SetNamespace("default")
			if config.GetKind() == "MutatingWebhookConfiguration" {
				config.SetNamespace("")
			}
			config.SetName(fmt.Sprintf("%s-%d", config.GetName(), i))

			// Server-side applied object: structured-merge and server-side diffs.
			applied, err := k.apply(ctx, config, DefaultManager, false)
			require.NoError(t, err)
			live, err := k.get(ctx, applied)
			require.NoError(t, err)

			legacy := mustDiff(t, config, live, Options{})
			assert.False(t, legacy.Modified, "legacy two-way\nlive: %s\npredicted: %s", legacy.NormalizedLive, legacy.PredictedLive)

			smd := mustDiff(t, config, live, Options{StructuredMerge: true, GVKParser: p})
			if config.GetKind() == "StatefulSet" {
				// Known baseline limitation of the structured-merge strategy: volumeClaimTemplates
				// is an atomic list and the server adds apiVersion/kind to each template, so the
				// prediction (config's templates) differs. Server-side diff handles it.
				assert.True(t, smd.Modified)
			} else {
				assert.False(t, smd.Modified, "structured merge\nlive: %s\npredicted: %s", smd.NormalizedLive, smd.PredictedLive)
			}

			o := DefaultOptions()
			o.ServerSide, o.GVKParser, o.DryRunner = true, p, k
			ssd := mustDiff(t, config, live, o)
			assert.False(t, ssd.Modified, "server-side\nlive: %s\npredicted: %s", ssd.NormalizedLive, ssd.PredictedLive)

			// A real change is detected by every strategy.
			changed := config.DeepCopy()
			changed.SetLabels(map[string]string{"changed": "true"})
			assert.True(t, mustDiff(t, changed, live, Options{}).Modified)
			assert.True(t, mustDiff(t, changed, live, Options{StructuredMerge: true, GVKParser: p}).Modified)
			res := mustDiff(t, changed, live, o)
			assert.True(t, res.Modified)
			labels, _ := nested(t, res.PredictedLive, "metadata", "labels")
			assert.Equal(t, map[string]any{"changed": "true"}, labels, "previously applied labels are pruned by the server")

			// Client-side applied object with last-applied-configuration: three-way diff.
			csa := withLastApplied(t, config, config)
			csa.SetName(config.GetName() + "-csa")
			created, err := k.create(ctx, csa)
			require.NoError(t, err)
			cfg2 := config.DeepCopy()
			cfg2.SetName(csa.GetName())
			three := mustDiff(t, cfg2, created, Options{})
			assert.False(t, three.Modified, "three-way\nlive: %s\npredicted: %s", three.NormalizedLive, three.PredictedLive)
		})
	}
}
