package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/juliosaraiva/patrol-cd/internal/envtestx"
)

// One envtest API server per test binary, started when KUBEBUILDER_ASSETS is set (required
// when PATROL_REQUIRE_ENVTEST=1). Besides the Patrol CRDs it serves a copy of them in the
// API group oldGroup, used by the migrate-group tests.

const oldGroup = "old.patrol.example"

var (
	envCfg  *rest.Config
	envDyn  dynamic.Interface
	envKube kubernetes.Interface
	nsSeq   atomic.Int64
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("PATROL_REQUIRE_ENVTEST") == "1" {
			fmt.Fprintln(os.Stderr, "PATROL_REQUIRE_ENVTEST=1 but KUBEBUILDER_ASSETS is not set")
			return 1
		}
		return m.Run()
	}
	crdDir := filepath.Join("..", "..", "..", "deploy", "crds")
	oldDir, err := os.MkdirTemp("", "patrol-admin-crds-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "temp dir: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(oldDir) }()
	if err := copyCRDsToGroup(crdDir, oldDir, currentGroup, oldGroup); err != nil {
		fmt.Fprintf(os.Stderr, "copy CRDs: %v\n", err)
		return 1
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{crdDir, oldDir}, ErrorIfCRDPathMissing: true}
	cfg, err := envtestx.Start(env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\n", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
		}
	}()
	envCfg = cfg
	envDyn = dynamic.NewForConfigOrDie(cfg)
	envKube = kubernetes.NewForConfigOrDie(cfg)
	return m.Run()
}

// copyCRDsToGroup writes the CRDs of dir with every occurrence of from replaced by to.
func copyCRDsToGroup(dir, outDir, from, to string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), from+"_") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		out := strings.ReplaceAll(string(data), from, to)
		if err := os.WriteFile(filepath.Join(outDir, strings.Replace(e.Name(), from, to, 1)), []byte(out), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// requireEnv skips the test without an envtest API server.
func requireEnv(t *testing.T) context.Context {
	t.Helper()
	if envCfg == nil {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping envtest-backed test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// newNamespace creates a namespace for one test.
func newNamespace(t *testing.T, ctx context.Context, prefix string) string {
	t.Helper()
	name := fmt.Sprintf("%s-%d", prefix, nsSeq.Add(1))
	_, err := envKube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	require.NoError(t, err)
	return name
}

// testIO is a factory with captured streams, bound to the envtest API server.
type testIO struct {
	f      *Factory
	out    *bytes.Buffer
	errOut *bytes.Buffer
}

func newTestIO(ns string) *testIO {
	tio := &testIO{out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	tio.f = &Factory{IOStreams: IOStreams{In: strings.NewReader(""), Out: tio.out, ErrOut: tio.errOut}}
	if envCfg != nil {
		tio.f.RESTConfig = func() (*rest.Config, string, error) { return rest.CopyConfig(envCfg), ns, nil }
	}
	return tio
}

// run executes "patrol <args>" with a fresh command tree.
func (tio *testIO) run(ctx context.Context, args ...string) error {
	tio.out.Reset()
	tio.errOut.Reset()
	root := &cobra.Command{Use: "patrol", SilenceUsage: true, SilenceErrors: true}
	Register(root, tio.f)
	root.SetArgs(args)
	root.SetOut(tio.out)
	root.SetErr(tio.errOut)
	return root.ExecuteContext(ctx)
}

// applyFile creates the objects of a YAML file in ns (metadata.namespace "patrol" or empty
// becomes ns).
func applyFile(t *testing.T, ctx context.Context, ns, path string) []*unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	objs, err := splitObjects(data)
	require.NoError(t, err, path)
	var out []*unstructured.Unstructured
	for _, obj := range objs {
		out = append(out, createObject(t, ctx, ns, obj))
	}
	return out
}

func createObject(t *testing.T, ctx context.Context, ns string, obj *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	if obj.GetNamespace() == "" || obj.GetNamespace() == "patrol" {
		obj.SetNamespace(ns)
	}
	dc, err := resourceFor(envDyn, obj.GroupVersionKind().GroupKind(), obj.GetNamespace())
	require.NoError(t, err)
	created, err := dc.Create(ctx, obj, metav1.CreateOptions{})
	require.NoError(t, err, "create %s", objectRef(obj))
	return created
}

type contextT = context.Context

func schemaGK(k resourceKey) schema.GroupKind { return schema.GroupKind{Group: k.Group, Kind: k.Kind} }

func groupOf(apiVersion string) string {
	if g, _, ok := strings.Cut(apiVersion, "/"); ok {
		return g
	}
	return ""
}

// removeFinalizers clears the finalizers of obj (no controller runs in envtest).
func removeFinalizers(t *testing.T, ctx context.Context, obj *unstructured.Unstructured) {
	t.Helper()
	dc, err := resourceFor(envDyn, obj.GroupVersionKind().GroupKind(), obj.GetNamespace())
	require.NoError(t, err)
	_, _ = dc.Patch(ctx, obj.GetName(), types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{})
}

func runtimeFromUnstructured(u *unstructured.Unstructured, into any) error {
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, into)
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
