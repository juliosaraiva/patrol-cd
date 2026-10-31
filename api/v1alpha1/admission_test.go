package v1alpha1_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/envtestx"
)

// The admission tests run the generated CRDs and the Operation admission policy (deploy/crds) in a
// real kube-apiserver via envtest and check structural schemas, enums, defaults, CEL rules and the
// ValidatingAdmissionPolicy. They are skipped when KUBEBUILDER_ASSETS is unset, unless
// PATROL_REQUIRE_ENVTEST=1 (set by `make test`, together with KUBEBUILDER_ASSETS), in which case the
// test binary fails instead of silently testing nothing.

var (
	k8sClient client.Client
	testEnv   *envtest.Environment
)

// crdDir holds the generated CRDs and the hand-written admission policy.
var crdDir = filepath.Join("..", "..", "deploy", "crds")

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("PATROL_REQUIRE_ENVTEST") == "1" {
			fmt.Fprintln(os.Stderr, "PATROL_REQUIRE_ENVTEST=1 but KUBEBUILDER_ASSETS is not set: the admission tests cannot run")
			return 1
		}
		return m.Run()
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
	}
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

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "add client-go scheme: %v\n", err)
		return 1
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "add v1alpha1 scheme: %v\n", err)
		return 1
	}
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create client: %v\n", err)
		return 1
	}
	testEnv = env
	if err := installOperationAdmissionPolicy(); err != nil {
		fmt.Fprintf(os.Stderr, "install operation admission policy: %v\n", err)
		return 1
	}
	return m.Run()
}

// installOperationAdmissionPolicy applies the hand-written ValidatingAdmissionPolicy and binding from
// deploy/crds (envtest installs only CRDs) and waits until the API server enforces them.
func installOperationAdmissionPolicy() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	data, err := os.ReadFile(filepath.Join(crdDir, "operations-admission-policy.yaml"))
	if err != nil {
		return err
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(&obj.Object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("decode: %w", err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		if err := k8sClient.Create(ctx, obj); err != nil {
			return fmt.Errorf("create %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "admission-policy-probe-"}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		return err
	}
	// Policies take effect asynchronously: probe with a dry-run create the policy must reject.
	probe := &v1alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: "probe", Labels: map[string]string{v1alpha1.LabelKeyOperationApplication: "other"}},
		Spec:       v1alpha1.OperationSpec{Application: "web", Sync: &v1alpha1.SyncOperation{}},
	}
	for {
		err := k8sClient.Create(ctx, probe.DeepCopy(), client.DryRunAll)
		if err != nil && strings.Contains(err.Error(), "must equal spec.application") {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("policy %s not enforced; last probe result: %v", v1alpha1.OperationAdmissionPolicyName, err)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// newNamespace skips the test without envtest and otherwise returns a fresh namespace.
func newNamespace(t *testing.T) (context.Context, string) {
	t.Helper()
	if k8sClient == nil {
		t.Skip("KUBEBUILDER_ASSETS is not set; skipping envtest admission test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "admission-"}}
	require.NoError(t, k8sClient.Create(ctx, ns))
	return ctx, ns.Name
}

// requireInvalid asserts that err is an Invalid API error mentioning every fragment.
func requireInvalid(t *testing.T, err error, fragments ...string) {
	t.Helper()
	require.Error(t, err, "expected the API server to reject the object")
	require.Truef(t, apierrors.IsInvalid(err), "expected Invalid, got %v", err)
	for _, f := range fragments {
		assert.Contains(t, err.Error(), f)
	}
}

// requireForbidden asserts that err is a Forbidden API error mentioning every fragment.
func requireForbidden(t *testing.T, err error, fragments ...string) {
	t.Helper()
	require.Error(t, err, "expected the API server to reject the request")
	require.Truef(t, apierrors.IsForbidden(err), "expected Forbidden, got %v", err)
	for _, f := range fragments {
		assert.Contains(t, err.Error(), f)
	}
}

// userClient returns a client authenticated as a new envtest user without any permissions.
func userClient(t *testing.T, name string) client.Client {
	t.Helper()
	u, err := testEnv.AddUser(envtest.User{Name: name}, nil)
	require.NoError(t, err)
	c, err := client.New(u.Config(), client.Options{Scheme: k8sClient.Scheme()})
	require.NoError(t, err)
	return c
}

// grantOperations gives user the verbs on operations.cd.patrol.example in namespace ns.
func grantOperations(ctx context.Context, t *testing.T, ns, user string, verbs ...string) {
	t.Helper()
	role := &rbacv1.Role{
		ObjectMeta: meta(ns, "operations-"+user),
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{v1alpha1.GroupName}, Resources: []string{"operations"}, Verbs: verbs}},
	}
	require.NoError(t, k8sClient.Create(ctx, role))
	binding := &rbacv1.RoleBinding{
		ObjectMeta: meta(ns, "operations-"+user),
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
	}
	require.NoError(t, k8sClient.Create(ctx, binding))
}

// unstructuredOperation builds an Operation from raw spec fields, so tests can store explicit zero
// and empty values that typed Go clients would omit.
func unstructuredOperation(ns, name string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Operation"))
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetLabels(map[string]string{v1alpha1.LabelKeyOperationApplication: "web"})
	return u
}

func meta(ns, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: ns, Name: name}
}

func gitSource(path string) v1alpha1.ApplicationSource {
	return v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/org/repo.git", Path: path, TargetRevision: "main"}
}

func validApplication(ns, name string) *v1alpha1.Application {
	src := gitSource("apps/web")
	return &v1alpha1.Application{
		ObjectMeta: meta(ns, name),
		Spec: v1alpha1.ApplicationSpec{
			Source:      &src,
			Destination: v1alpha1.ApplicationDestination{Server: v1alpha1.KubernetesInternalAPIServerAddr, Namespace: "web"},
			Project:     "default",
		},
	}
}

func TestAdmissionApplicationValid(t *testing.T) {
	ctx, ns := newNamespace(t)

	single := validApplication(ns, "single")
	single.Annotations = map[string]string{v1alpha1.AnnotationKeyRefresh: "not-a-refresh-type"}
	single.Finalizers = []string{v1alpha1.FinalizerResourcesBackground}
	single.Spec.Source.Helm = &v1alpha1.ApplicationSourceHelm{
		ValueFiles:   []string{"values.yaml"},
		Parameters:   []v1alpha1.HelmParameter{{Name: "image.tag", Value: "1.0", ForceString: true}},
		ValuesObject: &runtime.RawExtension{Raw: []byte(`{"replicas":2,"nested":{"list":[1,"two"]}}`)},
	}
	single.Spec.SyncPolicy = &v1alpha1.SyncPolicy{
		Automated:   &v1alpha1.SyncPolicyAutomated{Prune: ptr.To(true), SelfHeal: ptr.To(true), Enabled: ptr.To(true)},
		SyncOptions: v1alpha1.SyncOptions{"CreateNamespace=true", "ServerSideApply=true"},
		Retry: &v1alpha1.RetryStrategy{Limit: 5, Refresh: true, Backoff: &v1alpha1.Backoff{
			Duration: "5s", Factor: ptr.To[int64](2), MaxDuration: "3m",
		}},
		ManagedNamespaceMetadata: &v1alpha1.ManagedNamespaceMetadata{Labels: map[string]string{"team": "web"}},
	}
	single.Spec.IgnoreDifferences = v1alpha1.IgnoreDifferences{{Group: "apps", Kind: "Deployment", JSONPointers: []string{"/spec/replicas"}}}
	single.Spec.Info = []v1alpha1.Info{{Name: "owner", Value: "team-web"}}
	require.NoError(t, k8sClient.Create(ctx, single), "single-source Application with an arbitrary refresh annotation value")

	got := &v1alpha1.Application{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(single), got))
	assert.JSONEq(t, `{"replicas":2,"nested":{"list":[1,"two"]}}`, string(got.Spec.Source.Helm.ValuesObject.Raw), "valuesObject keeps unknown fields")

	multi := validApplication(ns, "multi")
	multi.Spec.Source = nil
	multi.Spec.Destination = v1alpha1.ApplicationDestination{Name: "in-cluster", Namespace: "web"}
	multi.Spec.Sources = v1alpha1.ApplicationSources{
		{RepoURL: "https://charts.example.com", Chart: "web", TargetRevision: "1.2.*", Helm: &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"$values/web.yaml"}}},
		{RepoURL: "https://git.example.com/org/values.git", Ref: "values", TargetRevision: "main"},
		{
			RepoURL: "https://git.example.com/org/repo.git", Path: "kustomize",
			Kustomize: &v1alpha1.ApplicationSourceKustomize{
				Images:     v1alpha1.KustomizeImages{"nginx=nginx:1.27"},
				Replicas:   v1alpha1.KustomizeReplicas{{Name: "web", Count: intstr.FromInt32(3)}, {Name: "worker", Count: intstr.FromString("2")}},
				Patches:    v1alpha1.KustomizePatches{{Patch: "- op: add", Target: &v1alpha1.KustomizeSelector{KustomizeResID: v1alpha1.KustomizeResID{KustomizeGvk: v1alpha1.KustomizeGvk{Kind: "Deployment"}, Name: "web"}}}},
				Components: []string{"../components/monitoring"},
			},
		},
		{
			RepoURL: "https://git.example.com/org/repo.git", Path: "jsonnet",
			Directory: &v1alpha1.ApplicationSourceDirectory{Recurse: true, Jsonnet: v1alpha1.ApplicationSourceJsonnet{
				ExtVars: []v1alpha1.JsonnetVar{{Name: "env", Value: "prod"}}, TLAs: []v1alpha1.JsonnetVar{{Name: "n", Value: "1", Code: true}},
			}},
		},
		{
			RepoURL: "https://git.example.com/org/repo.git", Path: "cmp",
			Plugin: &v1alpha1.ApplicationSourcePlugin{
				Name: "my-plugin",
				Env:  v1alpha1.Env{{Name: "FOO", Value: "bar"}},
				Parameters: v1alpha1.ApplicationSourcePluginParameters{
					{Name: "s", String: ptr.To("v")},
					{Name: "m", OptionalMap: &v1alpha1.OptionalMap{}},
					{Name: "a", OptionalArray: &v1alpha1.OptionalArray{Array: []string{"x"}}},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, multi), "multi-source Application with a named destination")
	got = &v1alpha1.Application{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(multi), got))
	params := got.Spec.Sources[4].Plugin.Parameters
	require.Len(t, params, 3)
	assert.NotNil(t, params[1].OptionalMap, "present-but-empty map parameter survives the API server")

	hydrated := validApplication(ns, "hydrated")
	hydrated.Spec.Source = nil
	hydrated.Spec.SourceHydrator = &v1alpha1.SourceHydrator{
		DrySource:  v1alpha1.DrySource{RepoURL: "https://git.example.com/org/repo.git", TargetRevision: "main", Path: "env/prod"},
		SyncSource: v1alpha1.SyncSource{TargetBranch: "env/prod", Path: "./hydrated/prod"},
		HydrateTo:  &v1alpha1.HydrateTo{TargetBranch: "env/prod-next"},
	}
	require.NoError(t, k8sClient.Create(ctx, hydrated), "Application with a source hydrator")

	// The status subresource accepts a fully populated status.
	now := metav1.Now()
	single.Status = v1alpha1.ApplicationStatus{
		Resources: []v1alpha1.ResourceStatus{{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "web", Name: "web",
			Status: v1alpha1.SyncStatusCodeSynced, Health: &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy}}},
		Sync: v1alpha1.SyncStatus{Status: v1alpha1.SyncStatusCodeSynced, Revision: "0123456789abcdef", ShortRevision: "0123456",
			ComparedTo: v1alpha1.ComparedTo{Source: single.Spec.Source, Destination: single.Spec.Destination}},
		Health: v1alpha1.AppHealthStatus{Status: v1alpha1.HealthStatusHealthy, LastTransitionTime: &now},
		History: v1alpha1.RevisionHistories{{ID: 1, Revision: "0123456789abcdef", DeployedAt: now, Source: single.Spec.Source,
			InitiatedBy: v1alpha1.OperationInitiator{Automated: true}, OperationName: "single-1"}},
		Conditions: []metav1.Condition{
			{Type: v1alpha1.ApplicationConditionSynced, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonSynced, LastTransitionTime: now, ObservedGeneration: 1},
			{Type: v1alpha1.ApplicationConditionComparisonError, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonComparisonFailed, Message: "boom", LastTransitionTime: now},
		},
		ReconciledAt:       &now,
		ObservedGeneration: 1,
		OperationState: &v1alpha1.ApplicationOperationState{OperationName: "single-1", Phase: v1alpha1.OperationSucceeded, StartedAt: &now, FinishedAt: &now,
			SyncResult: &v1alpha1.SyncResultSummary{Revision: "0123456789abcdef", Source: single.Spec.Source}},
		SourceType: v1alpha1.ApplicationSourceTypeHelm,
		Summary:    v1alpha1.ApplicationSummary{Images: []string{"nginx:1.27"}},
	}
	require.NoError(t, k8sClient.Status().Update(ctx, single))
}

func TestAdmissionApplicationRejected(t *testing.T) {
	ctx, ns := newNamespace(t)
	sources := v1alpha1.ApplicationSources{gitSource("a"), gitSource("b")}
	hydrator := &v1alpha1.SourceHydrator{
		DrySource:  v1alpha1.DrySource{RepoURL: "https://git.example.com/r.git", TargetRevision: "main", Path: "dry"},
		SyncSource: v1alpha1.SyncSource{TargetBranch: "hydrated", Path: "out"},
	}

	tests := []struct {
		name    string
		mutate  func(*v1alpha1.Application)
		message string
	}{
		{name: "source and sources", mutate: func(a *v1alpha1.Application) { a.Spec.Sources = sources }, message: "exactly one of spec.source, spec.sources or spec.sourceHydrator"},
		{name: "source and hydrator", mutate: func(a *v1alpha1.Application) { a.Spec.SourceHydrator = hydrator }, message: "exactly one of spec.source, spec.sources or spec.sourceHydrator"},
		{name: "no source", mutate: func(a *v1alpha1.Application) { a.Spec.Source = nil }, message: "exactly one of spec.source, spec.sources or spec.sourceHydrator"},
		{name: "server and name", mutate: func(a *v1alpha1.Application) { a.Spec.Destination.Name = "in-cluster" }, message: "exactly one of spec.destination.server or spec.destination.name"},
		{name: "neither server nor name", mutate: func(a *v1alpha1.Application) { a.Spec.Destination.Server = "" }, message: "exactly one of spec.destination.server or spec.destination.name"},
		{name: "hydrator sync path at repo root", mutate: func(a *v1alpha1.Application) {
			a.Spec.Source = nil
			h := hydrator.DeepCopy()
			h.SyncSource.Path = "."
			a.Spec.SourceHydrator = h
		}, message: "spec.sourceHydrator.syncSource.path in body should match"},
		{name: "hydrator sync path ./", mutate: func(a *v1alpha1.Application) {
			a.Spec.Source = nil
			h := hydrator.DeepCopy()
			h.SyncSource.Path = "./"
			a.Spec.SourceHydrator = h
		}, message: "spec.sourceHydrator.syncSource.path in body should match"},
		{name: "hydrator sync path //", mutate: func(a *v1alpha1.Application) {
			a.Spec.Source = nil
			h := hydrator.DeepCopy()
			h.SyncSource.Path = "//"
			a.Spec.SourceHydrator = h
		}, message: "spec.sourceHydrator.syncSource.path in body should match"},
		{name: "hydrator sync path ..", mutate: func(a *v1alpha1.Application) {
			a.Spec.Source = nil
			h := hydrator.DeepCopy()
			h.SyncSource.Path = ".."
			a.Spec.SourceHydrator = h
		}, message: "spec.sourceHydrator.syncSource.path in body should match"},
		{name: "sync option without value", mutate: func(a *v1alpha1.Application) {
			a.Spec.SyncPolicy = &v1alpha1.SyncPolicy{SyncOptions: v1alpha1.SyncOptions{"CreateNamespace=true", "PruneLast"}}
		}, message: "spec.syncPolicy.syncOptions entries must have the form Key=Value"},
		{name: "sync option with spaces", mutate: func(a *v1alpha1.Application) {
			a.Spec.SyncPolicy = &v1alpha1.SyncPolicy{SyncOptions: v1alpha1.SyncOptions{"Prune = false"}}
		}, message: "spec.syncPolicy.syncOptions entries must have the form Key=Value"},
		{name: "negative revision history limit", mutate: func(a *v1alpha1.Application) {
			a.Spec.RevisionHistoryLimit = ptr.To[int64](-1)
		}, message: "spec.revisionHistoryLimit"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := validApplication(ns, fmt.Sprintf("bad-%d", i))
			tt.mutate(app)
			requireInvalid(t, k8sClient.Create(ctx, app), tt.message)
		})
	}
}

func validOperation(ns, name string) *v1alpha1.Operation {
	return &v1alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{v1alpha1.LabelKeyOperationApplication: "web"}},
		Spec: v1alpha1.OperationSpec{
			Application: "web",
			Sync: &v1alpha1.SyncOperation{
				Revision:     "main",
				Prune:        true,
				SyncStrategy: &v1alpha1.SyncStrategy{Hook: &v1alpha1.SyncStrategyHook{SyncStrategyApply: v1alpha1.SyncStrategyApply{Force: true}}},
				Resources:    []v1alpha1.SyncOperationResource{{Group: "apps", Kind: "Deployment", Name: "web"}},
				SyncOptions:  v1alpha1.SyncOptions{"PruneLast=true"},
			},
			InitiatedBy: v1alpha1.OperationInitiator{Username: "alice"},
			Info:        []v1alpha1.Info{{Name: "reason", Value: "release"}},
			Retry:       &v1alpha1.RetryStrategy{Limit: 2},
		},
	}
}

func TestAdmissionOperation(t *testing.T) {
	ctx, ns := newNamespace(t)

	t.Run("valid sync and status", func(t *testing.T) {
		op := validOperation(ns, "sync")
		require.NoError(t, k8sClient.Create(ctx, op))

		now := metav1.Now()
		op.Status = v1alpha1.OperationStatus{
			Phase: v1alpha1.OperationRunning, StartedAt: &now, ObservedGeneration: op.Generation,
			Conditions: []metav1.Condition{{Type: v1alpha1.OperationConditionAdmitted, Status: metav1.ConditionTrue, Reason: v1alpha1.OperationReasonAdmitted, LastTransitionTime: now}},
			SyncResult: &v1alpha1.SyncOperationResult{Revision: "0123abc", Resources: []v1alpha1.ResourceResult{
				{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "web", Name: "web", Status: v1alpha1.ResultCodeSynced, SyncPhase: v1alpha1.SyncPhaseSync},
				{Group: "batch", Version: "v1", Kind: "Job", Namespace: "web", Name: "migrate", HookType: v1alpha1.HookTypePreSync, HookPhase: v1alpha1.OperationSucceeded, SyncPhase: v1alpha1.SyncPhasePreSync},
			}},
			Progress: &v1alpha1.OperationProgress{SyncPhase: v1alpha1.SyncPhaseSync, Wave: -1, StartedAt: &now, CompletedTasks: 1, TotalTasks: 2},
			Plan:     &v1alpha1.OperationPlan{ResolvedRevisions: []string{"0123abc"}, RenderedInputDigest: "sha256:00", Destination: v1alpha1.ApplicationDestination{Server: "https://kubernetes.default.svc"}},
		}
		require.NoError(t, k8sClient.Status().Update(ctx, op))

		bad := op.DeepCopy()
		bad.Status.Phase = "Exploded"
		requireInvalid(t, k8sClient.Status().Update(ctx, bad), "status.phase", "Unsupported value")

		bad = op.DeepCopy()
		bad.Status.SyncResult.Resources[0].Status = "Maybe"
		requireInvalid(t, k8sClient.Status().Update(ctx, bad), "status.syncResult.resources[0].status")
	})

	t.Run("valid rollback with sync options", func(t *testing.T) {
		op := validOperation(ns, "rollback")
		op.Spec.Rollback = &v1alpha1.RollbackOperation{ID: 3}
		op.Spec.Sync = &v1alpha1.SyncOperation{Prune: true, DryRun: true}
		require.NoError(t, k8sClient.Create(ctx, op))
	})

	t.Run("spec immutable except cancel", func(t *testing.T) {
		op := validOperation(ns, "immutable")
		require.NoError(t, k8sClient.Create(ctx, op))

		mutations := []struct {
			name    string
			mutate  func(*v1alpha1.Operation)
			message string
		}{
			{name: "application", mutate: func(o *v1alpha1.Operation) { o.Spec.Application = "other" }, message: "spec.application is immutable"},
			{name: "sync field", mutate: func(o *v1alpha1.Operation) { o.Spec.Sync.Prune = false }, message: "spec.sync is immutable"},
			{name: "sync revision", mutate: func(o *v1alpha1.Operation) { o.Spec.Sync.Revision = "v2" }, message: "spec.sync is immutable"},
			{name: "add rollback", mutate: func(o *v1alpha1.Operation) { o.Spec.Rollback = &v1alpha1.RollbackOperation{ID: 1} }, message: "spec.rollback is immutable"},
			{name: "initiatedBy", mutate: func(o *v1alpha1.Operation) { o.Spec.InitiatedBy.Username = "mallory" }, message: "spec.initiatedBy is immutable"},
			{name: "info", mutate: func(o *v1alpha1.Operation) { o.Spec.Info = nil }, message: "spec.info is immutable"},
			{name: "retry", mutate: func(o *v1alpha1.Operation) { o.Spec.Retry.Limit = 10 }, message: "spec.retry is immutable"},
		}
		for _, m := range mutations {
			t.Run(m.name, func(t *testing.T) {
				cur := &v1alpha1.Operation{}
				require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(op), cur))
				m.mutate(cur)
				requireInvalid(t, k8sClient.Update(ctx, cur), m.message)
			})
		}

		cur := &v1alpha1.Operation{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(op), cur))
		cur.Labels[v1alpha1.LabelKeyOperationPhase] = string(v1alpha1.OperationRunning)
		require.NoError(t, k8sClient.Update(ctx, cur), "metadata changes are allowed")

		patch := client.MergeFrom(cur.DeepCopy())
		cur.Spec.Cancel = true
		require.NoError(t, k8sClient.Patch(ctx, cur, patch), "cancel may be set")

		patch = client.MergeFrom(cur.DeepCopy())
		cur.Spec.Cancel = false
		requireInvalid(t, k8sClient.Patch(ctx, cur, patch), "spec.cancel cannot be unset once true")
	})

	// Operations written as YAML may carry explicit zero values that typed Go clients omit when they
	// send a full update. Scalars of spec.sync are defaulted and spec.initiatedBy is compared field by
	// field, so such updates are accepted.
	t.Run("full update of an Operation with explicit zero scalars", func(t *testing.T) {
		u := unstructuredOperation(ns, "explicit-zero", map[string]any{
			"application": "web",
			"sync": map[string]any{
				"revision": "", "prune": false, "dryRun": false, "autoHealAttemptsCount": int64(0),
				"syncStrategy": map[string]any{"hook": map[string]any{"force": false}},
			},
			"initiatedBy": map[string]any{"username": "", "automated": false},
		})
		require.NoError(t, k8sClient.Create(ctx, u))

		cur := &v1alpha1.Operation{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(u), cur))
		cur.Labels[v1alpha1.LabelKeyOperationPhase] = string(v1alpha1.OperationPending)
		require.NoError(t, k8sClient.Update(ctx, cur), "typed full update after a YAML create with explicit zero scalars")

		// Omitted scalars are stored as their defaults, so both forms compare equal.
		omitted := validOperation(ns, "omitted-zero")
		omitted.Spec.Sync = &v1alpha1.SyncOperation{}
		omitted.Spec.InitiatedBy = v1alpha1.OperationInitiator{}
		require.NoError(t, k8sClient.Create(ctx, omitted))
		stored := &unstructured.Unstructured{}
		stored.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Operation"))
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(omitted), stored))
		sync, _, err := unstructured.NestedMap(stored.Object, "spec", "sync")
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"revision": "", "prune": false, "dryRun": false, "autoHealAttemptsCount": int64(0)}, sync)
	})

	// An explicit empty list or object is not defaulted: a typed full update omits it and is rejected
	// as a spec change. Clients must patch Operations; the message says so.
	t.Run("full update of an Operation with an explicit empty list needs a patch", func(t *testing.T) {
		u := unstructuredOperation(ns, "explicit-empty", map[string]any{
			"application": "web",
			"sync":        map[string]any{"prune": true, "syncOptions": []any{}},
		})
		require.NoError(t, k8sClient.Create(ctx, u))

		cur := &v1alpha1.Operation{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(u), cur))
		base := cur.DeepCopy()
		cur.Labels[v1alpha1.LabelKeyOperationPhase] = string(v1alpha1.OperationPending)
		requireInvalid(t, k8sClient.Update(ctx, cur), "spec.sync is immutable; change metadata and spec.cancel with a patch, not a full update")

		require.NoError(t, k8sClient.Patch(ctx, cur, client.MergeFrom(base)), "merge patch of a label")
		patch := client.MergeFrom(cur.DeepCopy())
		cur.Spec.Cancel = true
		require.NoError(t, k8sClient.Patch(ctx, cur, patch), "merge patch of spec.cancel")
		assert.Equal(t, string(v1alpha1.OperationPending), cur.Labels[v1alpha1.LabelKeyOperationPhase])
		assert.True(t, cur.Spec.Cancel)
	})

	rejected := []struct {
		name    string
		mutate  func(*v1alpha1.Operation)
		message string
	}{
		{name: "empty application", mutate: func(o *v1alpha1.Operation) {
			o.Spec.Application = ""
			o.Labels = nil
		}, message: "spec.application in body should be at least 1 chars long"},
		{name: "malformed sync option", mutate: func(o *v1alpha1.Operation) { o.Spec.Sync.SyncOptions = v1alpha1.SyncOptions{"PruneLast"} }, message: "spec.sync.syncOptions entries must have the form Key=Value"},
		{name: "neither sync nor rollback", mutate: func(o *v1alpha1.Operation) { o.Spec.Sync = nil }, message: "one of spec.sync or spec.rollback must be set"},
		{name: "rollback with explicit source", mutate: func(o *v1alpha1.Operation) {
			o.Spec.Rollback = &v1alpha1.RollbackOperation{ID: 1}
			src := gitSource("x")
			o.Spec.Sync.Source = &src
		}, message: "spec.rollback is mutually exclusive with spec.sync.source and spec.sync.sources"},
		{name: "rollback with explicit sources", mutate: func(o *v1alpha1.Operation) {
			o.Spec.Rollback = &v1alpha1.RollbackOperation{ID: 1}
			o.Spec.Sync.Sources = v1alpha1.ApplicationSources{gitSource("x")}
		}, message: "spec.rollback is mutually exclusive"},
		{name: "source and sources", mutate: func(o *v1alpha1.Operation) {
			src := gitSource("x")
			o.Spec.Sync.Source = &src
			o.Spec.Sync.Sources = v1alpha1.ApplicationSources{gitSource("y")}
		}, message: "spec.sync.source and spec.sync.sources are mutually exclusive"},
		{name: "manifests total over 1MiB", mutate: func(o *v1alpha1.Operation) {
			doc := strings.Repeat("a", 600*1024)
			o.Spec.Sync.Manifests = []string{doc, doc}
		}, message: "spec.sync.manifests must not exceed 1MiB in total"},
		{name: "manifest document over 1MiB", mutate: func(o *v1alpha1.Operation) {
			o.Spec.Sync.Manifests = []string{strings.Repeat("a", 1024*1024+1)}
		}, message: "spec.sync.manifests[0]"},
	}
	for i, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			op := validOperation(ns, fmt.Sprintf("bad-%d", i))
			tt.mutate(op)
			requireInvalid(t, k8sClient.Create(ctx, op), tt.message)
		})
	}

	t.Run("manifests within limits", func(t *testing.T) {
		op := validOperation(ns, "manifests")
		op.Spec.Sync.Manifests = []string{strings.Repeat("a", 400*1024), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"}
		require.NoError(t, k8sClient.Create(ctx, op))
	})
}

// TestAdmissionOperationPolicy covers the ValidatingAdmissionPolicy in deploy/crds: CRD rules cannot
// see the requesting user or labels.
func TestAdmissionOperationPolicy(t *testing.T) {
	ctx, ns := newNamespace(t)

	// alice is an ordinary user allowed to request syncs; patrol-server may also create Operations on
	// behalf of others.
	alice := userClient(t, "alice")
	server := userClient(t, "patrol-server")
	grantOperations(ctx, t, ns, "alice", "create", "get", "patch", "update")
	grantOperations(ctx, t, ns, "patrol-server", "create", "get", v1alpha1.OperationInitiatorVerb)

	op := func(name, username string, automated bool) *v1alpha1.Operation {
		return &v1alpha1.Operation{
			ObjectMeta: meta(ns, name),
			Spec: v1alpha1.OperationSpec{
				Application: "web",
				Sync:        &v1alpha1.SyncOperation{Prune: true},
				InitiatedBy: v1alpha1.OperationInitiator{Username: username, Automated: automated},
			},
		}
	}
	// RBAC bindings take effect asynchronously.
	require.Eventually(t, func() bool { return alice.Create(ctx, op("rbac-alice", "alice", false), client.DryRunAll) == nil },
		30*time.Second, 100*time.Millisecond, "alice's RBAC binding never took effect")
	require.Eventually(t, func() bool { return server.Create(ctx, op("rbac-server", "bob", true), client.DryRunAll) == nil },
		30*time.Second, 100*time.Millisecond, "patrol-server's RBAC binding never took effect")

	const initiatorMessage = "spec.initiatedBy.username must be empty or the requesting user"
	t.Run("user may leave initiator empty", func(t *testing.T) {
		o := op("alice-empty", "", false)
		require.NoError(t, alice.Create(ctx, o))
		assert.Equal(t, v1alpha1.OperationInitiator{}, o.Spec.InitiatedBy)
	})
	t.Run("user may name themselves", func(t *testing.T) {
		require.NoError(t, alice.Create(ctx, op("alice-self", "alice", false)))
	})
	t.Run("user cannot name someone else", func(t *testing.T) {
		requireForbidden(t, alice.Create(ctx, op("alice-forged", "admin", false)), v1alpha1.OperationAdmissionPolicyName, initiatorMessage)
	})
	t.Run("user cannot claim an automated sync", func(t *testing.T) {
		requireForbidden(t, alice.Create(ctx, op("alice-automated", "", true)), initiatorMessage)
		requireForbidden(t, alice.Create(ctx, op("alice-automated-self", "alice", true)), initiatorMessage)
	})
	t.Run("impersonate verb allows initiating for others", func(t *testing.T) {
		o := op("server-for-bob", "bob", true)
		require.NoError(t, server.Create(ctx, o))

		// The initiator check applies to creation only: alice may still change metadata.
		cur := &v1alpha1.Operation{}
		require.NoError(t, alice.Get(ctx, client.ObjectKeyFromObject(o), cur))
		patch := client.MergeFrom(cur.DeepCopy())
		cur.Labels = map[string]string{"team": "web"}
		require.NoError(t, alice.Patch(ctx, cur, patch))
	})
	t.Run("application label must match spec.application", func(t *testing.T) {
		o := op("label-mismatch", "", false)
		o.Labels = map[string]string{v1alpha1.LabelKeyOperationApplication: "other"}
		requireInvalid(t, k8sClient.Create(ctx, o), "label cd.patrol.example/application must equal spec.application")

		o = op("label-match", "", false)
		o.Labels = map[string]string{v1alpha1.LabelKeyOperationApplication: "web"}
		require.NoError(t, k8sClient.Create(ctx, o))
		patch := client.MergeFrom(o.DeepCopy())
		o.Labels[v1alpha1.LabelKeyOperationApplication] = "other"
		requireInvalid(t, k8sClient.Patch(ctx, o, patch), "label cd.patrol.example/application must equal spec.application")
	})
}

func TestAdmissionAppProject(t *testing.T) {
	ctx, ns := newNamespace(t)
	valid := &v1alpha1.AppProject{
		ObjectMeta: meta(ns, "team-a"),
		Spec: v1alpha1.AppProjectSpec{
			SourceRepos:  []string{"https://git.example.com/team-a/*", "!https://git.example.com/team-a/secret"},
			Destinations: []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "team-a-*"}, {Name: "in-cluster", Namespace: "shared"}},
			Description:  "Team A",
			Roles: []v1alpha1.ProjectRole{{
				Name: "ci", Policies: []string{"p, proj:team-a:ci, applications, sync, team-a/*, allow"},
				JWTTokens: []v1alpha1.JWTToken{{IssuedAt: 1700000000, ID: "tok-1"}}, Groups: []string{"team-a-admins"},
			}},
			ClusterResourceWhitelist:   []v1alpha1.ClusterResourceRestrictionItem{{Group: "", Kind: "Namespace", Name: "team-a-*"}},
			NamespaceResourceBlacklist: []metav1.GroupKind{{Group: "", Kind: "ResourceQuota"}},
			OrphanedResources:          &v1alpha1.OrphanedResourcesMonitorSettings{Warn: ptr.To(true), Ignore: []v1alpha1.OrphanedResourceKey{{Kind: "ConfigMap", Name: "kube-root-ca.crt"}}},
			SyncWindows: v1alpha1.SyncWindows{
				{Kind: "allow", Schedule: "0 22 * * *", Duration: "1h", Applications: []string{"*"}, TimeZone: "Europe/Berlin"},
				{Kind: "deny", Schedule: "@daily", Duration: "8h", Namespaces: []string{"prod-*"}, ManualSync: true, UseAndOperator: true, SyncOverrun: true},
			},
			SignatureKeys:              []v1alpha1.SignatureKey{{KeyID: "4AEE18F83AFDEB23"}},
			SourceNamespaces:           []string{"team-a-*"},
			DestinationServiceAccounts: []v1alpha1.ApplicationDestinationServiceAccount{{Server: "*", Namespace: "team-a-*", DefaultServiceAccount: "team-a:deployer"}},
			SourceIntegrity: &v1alpha1.SourceIntegrity{Git: &v1alpha1.SourceIntegrityGit{Policies: []v1alpha1.SourceIntegrityGitPolicy{{
				Repos: []v1alpha1.SourceIntegrityGitPolicyRepo{{URL: "https://git.example.com/team-a/*"}},
				GPG:   &v1alpha1.SourceIntegrityGitPolicyGPG{Mode: v1alpha1.SourceIntegrityGitPolicyGPGModeStrict, Keys: []string{"4AEE18F83AFDEB23"}},
			}}}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, valid))

	valid.Status.JWTTokensByRole = map[string]v1alpha1.JWTTokens{"ci": {Items: []v1alpha1.JWTToken{{IssuedAt: 1700000000, ExpiresAt: 1800000000, ID: "tok-1"}}}}
	require.NoError(t, k8sClient.Status().Update(ctx, valid))

	tests := []struct {
		name    string
		mutate  func(*v1alpha1.AppProject)
		message string
	}{
		{name: "sync window kind", mutate: func(p *v1alpha1.AppProject) { p.Spec.SyncWindows[0].Kind = "maybe" }, message: "spec.syncWindows[0].kind"},
		{name: "sync window schedule", mutate: func(p *v1alpha1.AppProject) { p.Spec.SyncWindows[0].Schedule = "every night" }, message: "spec.syncWindows[0].schedule"},
		{name: "gpg mode", mutate: func(p *v1alpha1.AppProject) { p.Spec.SourceIntegrity.Git.Policies[0].GPG.Mode = "lenient" }, message: "spec.sourceIntegrity.git.policies[0].gpg.mode: Unsupported value"},
		{name: "description length", mutate: func(p *v1alpha1.AppProject) { p.Spec.Description = strings.Repeat("d", 256) }, message: "spec.description"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := valid.DeepCopy()
			p.ObjectMeta = meta(ns, fmt.Sprintf("bad-%d", i))
			p.Status = v1alpha1.AppProjectStatus{}
			tt.mutate(p)
			requireInvalid(t, k8sClient.Create(ctx, p), tt.message)
		})
	}
}

func appSetTemplate() v1alpha1.ApplicationSetTemplate {
	src := gitSource("apps/{{.name}}")
	return v1alpha1.ApplicationSetTemplate{
		ApplicationSetTemplateMeta: v1alpha1.ApplicationSetTemplateMeta{Name: "{{.name}}", Labels: map[string]string{"env": "{{.env}}"}},
		Spec: v1alpha1.ApplicationSpec{
			Source:      &src,
			Destination: v1alpha1.ApplicationDestination{Server: "{{.server}}", Namespace: "{{.name}}"},
			Project:     "default",
		},
	}
}

func validApplicationSet(ns, name string) *v1alpha1.ApplicationSet {
	nested := `{"generators":[{"list":{"elements":[{"name":"a"}]}},{"git":{"repoURL":"https://git.example.com/r.git","revision":"HEAD","directories":[{"path":"apps/*"}]}}]}`
	return &v1alpha1.ApplicationSet{
		ObjectMeta: meta(ns, name),
		Spec: v1alpha1.ApplicationSetSpec{
			GoTemplate:        true,
			GoTemplateOptions: []string{"missingkey=error"},
			Generators: []v1alpha1.ApplicationSetGenerator{
				{List: &v1alpha1.ListGenerator{Elements: []apiextensionsv1.JSON{{Raw: []byte(`{"name":"web","env":"prod","server":"https://kubernetes.default.svc"}`)}}}},
				{Matrix: &v1alpha1.MatrixGenerator{Generators: []v1alpha1.ApplicationSetNestedGenerator{
					{Clusters: &v1alpha1.ClusterGenerator{Selector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}}}},
					{Matrix: &apiextensionsv1.JSON{Raw: []byte(nested)}},
				}}},
				{Merge: &v1alpha1.MergeGenerator{MergeKeys: []string{"name"}, Generators: []v1alpha1.ApplicationSetNestedGenerator{
					{SCMProvider: &v1alpha1.SCMProviderGenerator{Github: &v1alpha1.SCMProviderGeneratorGithub{Organization: "org", ProviderRef: &v1alpha1.SCMProviderReference{Name: "github"}}}},
					{Merge: &apiextensionsv1.JSON{Raw: []byte(`{"mergeKeys":["name"],"generators":[{"list":{"elements":[]}},{"clusters":{}}]}`)}},
				}}},
				{SCMProvider: &v1alpha1.SCMProviderGenerator{
					Gitlab:  &v1alpha1.SCMProviderGeneratorGitlab{Group: "42", TokenRef: &v1alpha1.SecretRef{SecretName: "gitlab", Key: "token"}},
					Filters: []v1alpha1.SCMProviderGeneratorFilter{{RepositoryMatch: ptr.To("^web-"), PathsExist: []string{"deploy"}}},
				}},
				{SCMProvider: &v1alpha1.SCMProviderGenerator{AzureDevOps: &v1alpha1.SCMProviderGeneratorAzureDevOps{Organization: "o", TeamProject: "p", ProviderRef: &v1alpha1.SCMProviderReference{Name: "ado"}}}},
				{SCMProvider: &v1alpha1.SCMProviderGenerator{Bitbucket: &v1alpha1.SCMProviderGeneratorBitbucket{Owner: "ws", User: "bot", AppPasswordRef: &v1alpha1.SecretRef{SecretName: "bb", Key: "password"}}}},
				{PullRequest: &v1alpha1.PullRequestGenerator{
					Github:                      &v1alpha1.PullRequestGeneratorGithub{Owner: "org", Repo: "web", ProviderRef: &v1alpha1.SCMProviderReference{Name: "github"}, Labels: []string{"preview"}},
					Filters:                     []v1alpha1.PullRequestGeneratorFilter{{BranchMatch: ptr.To("^feature/")}},
					RequeueAfterSeconds:         ptr.To[int64](60),
					ContinueOnRepoNotFoundError: true,
				}},
				{PullRequest: &v1alpha1.PullRequestGenerator{BitbucketServer: &v1alpha1.PullRequestGeneratorBitbucketServer{
					Project: "P", Repo: "r", API: "https://bitbucket.example.com/rest",
					BasicAuth: &v1alpha1.BasicAuthBitbucketServer{Username: "bot", PasswordRef: &v1alpha1.SecretRef{SecretName: "bbs", Key: "password"}},
				}}},
				{ClusterDecisionResource: &v1alpha1.DuckTypeGenerator{ConfigMapRef: "ocm-placement", Name: "placement"}},
				{Plugin: &v1alpha1.PluginGenerator{ConfigMapRef: v1alpha1.PluginConfigMapRef{Name: "plugin"},
					Input: v1alpha1.PluginInput{Parameters: v1alpha1.PluginParameters{"k": {Raw: []byte(`{"any":["thing"]}`)}}}}},
			},
			Template: appSetTemplate(),
			SyncPolicy: &v1alpha1.ApplicationSetSyncPolicy{
				PreserveResourcesOnDeletion: true,
				ApplicationsSync:            ptr.To(v1alpha1.ApplicationsSyncPolicyCreateUpdate),
			},
			Strategy: &v1alpha1.ApplicationSetStrategy{
				Type: v1alpha1.ApplicationSetStrategyRollingSync,
				RollingSync: &v1alpha1.ApplicationSetRolloutStrategy{Steps: []v1alpha1.ApplicationSetRolloutStep{
					{MatchExpressions: []v1alpha1.ApplicationMatchExpression{{Key: "env", Operator: "In", Values: []string{"staging"}}}},
					{MatchExpressions: []v1alpha1.ApplicationMatchExpression{{Key: "env", Operator: "In", Values: []string{"prod"}}}, MaxUpdate: ptr.To(intstr.FromString("25%"))},
				}},
				DeletionOrder: v1alpha1.ApplicationSetDeletionOrderReverse,
			},
			PreservedFields:              &v1alpha1.ApplicationPreservedFields{Annotations: []string{"notes"}},
			IgnoreApplicationDifferences: v1alpha1.ApplicationSetIgnoreDifferences{{JSONPointers: []string{"/spec/source/targetRevision"}}},
			TemplatePatch:                ptr.To(`spec: {}`),
		},
	}
}

func TestAdmissionApplicationSet(t *testing.T) {
	ctx, ns := newNamespace(t)

	valid := validApplicationSet(ns, "valid")
	require.NoError(t, k8sClient.Create(ctx, valid))
	got := &v1alpha1.ApplicationSet{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(valid), got))
	nested, err := v1alpha1.ToNestedMatrixGenerator(got.Spec.Generators[1].Matrix.Generators[1].Matrix)
	require.NoError(t, err)
	require.Len(t, nested.Generators, 2, "nested matrix generators are preserved as raw JSON")
	assert.Equal(t, "apps/*", nested.Generators[1].Git.Directories[0].Path)

	now := metav1.Now()
	got.Status = v1alpha1.ApplicationSetStatus{
		Conditions:        []metav1.Condition{{Type: v1alpha1.ApplicationSetConditionResourcesUpToDate, Status: metav1.ConditionTrue, Reason: v1alpha1.ApplicationSetReasonApplicationSetUpToDate, LastTransitionTime: now}},
		ApplicationStatus: []v1alpha1.ApplicationSetApplicationStatus{{Application: "web", Status: v1alpha1.ProgressiveSyncWaiting, Step: "1", TargetRevisions: []string{"abc"}, LastTransitionTime: &now}},
		Resources:         []v1alpha1.ResourceStatus{{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Kind: "Application", Namespace: ns, Name: "web"}},
		ResourcesCount:    1,
		Health:            v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy},
	}
	require.NoError(t, k8sClient.Status().Update(ctx, got))

	tests := []struct {
		name    string
		mutate  func(*v1alpha1.ApplicationSet)
		message string
	}{
		{name: "strategy type", mutate: func(a *v1alpha1.ApplicationSet) { a.Spec.Strategy.Type = "Canary" }, message: "spec.strategy.type"},
		{name: "rolling sync without steps", mutate: func(a *v1alpha1.ApplicationSet) { a.Spec.Strategy.RollingSync = nil }, message: "strategy.rollingSync is required"},
		{name: "deletion order", mutate: func(a *v1alpha1.ApplicationSet) { a.Spec.Strategy.DeletionOrder = "Random" }, message: "spec.strategy.deletionOrder"},
		{name: "applications sync policy", mutate: func(a *v1alpha1.ApplicationSet) {
			a.Spec.SyncPolicy.ApplicationsSync = ptr.To(v1alpha1.ApplicationsSyncPolicy("delete-only"))
		}, message: "spec.syncPolicy.applicationsSync"},
		{name: "github providerRef with tokenRef", mutate: func(a *v1alpha1.ApplicationSet) {
			a.Spec.Generators[2].Merge.Generators[0].SCMProvider.Github.TokenRef = &v1alpha1.SecretRef{SecretName: "s", Key: "k"}
		}, message: "providerRef is mutually exclusive with tokenRef and appSecretName"},
		{name: "azure devops without credentials", mutate: func(a *v1alpha1.ApplicationSet) {
			a.Spec.Generators[4].SCMProvider.AzureDevOps.ProviderRef = nil
		}, message: "set exactly one of providerRef or accessTokenRef"},
		{name: "bitbucket user without password", mutate: func(a *v1alpha1.ApplicationSet) {
			a.Spec.Generators[5].SCMProvider.Bitbucket.AppPasswordRef = nil
		}, message: "user and appPasswordRef must be set together"},
		{name: "pull request providerRef with basic auth", mutate: func(a *v1alpha1.ApplicationSet) {
			a.Spec.Generators[7].PullRequest.BitbucketServer.ProviderRef = &v1alpha1.SCMProviderReference{Name: "bbs"}
		}, message: "providerRef is mutually exclusive with basicAuth and bearerToken"},
		{name: "missing generators", mutate: func(a *v1alpha1.ApplicationSet) { a.Spec.Generators = nil }, message: "spec.generators: Required value"},
		{name: "too many generators", mutate: func(a *v1alpha1.ApplicationSet) {
			for range 129 - len(a.Spec.Generators) {
				a.Spec.Generators = append(a.Spec.Generators, v1alpha1.ApplicationSetGenerator{List: &v1alpha1.ListGenerator{}})
			}
		}, message: "spec.generators: Too many: 129: must have at most 128 items"},
		{name: "matrix with one generator", mutate: func(a *v1alpha1.ApplicationSet) {
			a.Spec.Generators[1].Matrix.Generators = a.Spec.Generators[1].Matrix.Generators[:1]
		}, message: "spec.generators[1].matrix.generators in body should have at least 2 items"},
		{name: "matrix with three generators", mutate: func(a *v1alpha1.ApplicationSet) {
			m := a.Spec.Generators[1].Matrix
			m.Generators = append(m.Generators, m.Generators[0])
		}, message: "spec.generators[1].matrix.generators: Too many: 3: must have at most 2 items"},
		{name: "merge with one generator", mutate: func(a *v1alpha1.ApplicationSet) {
			a.Spec.Generators[2].Merge.Generators = a.Spec.Generators[2].Merge.Generators[:1]
		}, message: "spec.generators[2].merge.generators in body should have at least 2 items"},
		{name: "deletion order case", mutate: func(a *v1alpha1.ApplicationSet) { a.Spec.Strategy.DeletionOrder = "reverse" }, message: "spec.strategy.deletionOrder: Unsupported value"},
		{name: "reverse deletion without rolling sync", mutate: func(a *v1alpha1.ApplicationSet) {
			a.Spec.Strategy.Type = v1alpha1.ApplicationSetStrategyAllAtOnce
		}, message: "strategy.deletionOrder Reverse requires strategy.type RollingSync"},
		{name: "rolling sync operator", mutate: func(a *v1alpha1.ApplicationSet) {
			a.Spec.Strategy.RollingSync.Steps[0].MatchExpressions[0].Operator = "Exists"
		}, message: "spec.strategy.rollingSync.steps[0].matchExpressions[0].operator: Unsupported value"},
		{name: "scm gitea providerRef with tokenRef", mutate: func(a *v1alpha1.ApplicationSet) {
			a.Spec.Generators = append(a.Spec.Generators, v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{Gitea: &v1alpha1.SCMProviderGeneratorGitea{
				Owner: "o", API: "https://gitea.example.com", TokenRef: &v1alpha1.SecretRef{SecretName: "s", Key: "k"}, ProviderRef: &v1alpha1.SCMProviderReference{Name: "gitea"},
			}}})
		}, message: "providerRef and tokenRef are mutually exclusive"},
		{name: "scm gitlab providerRef with tokenRef", mutate: func(a *v1alpha1.ApplicationSet) {
			a.Spec.Generators[3].SCMProvider.Gitlab.ProviderRef = &v1alpha1.SCMProviderReference{Name: "gitlab"}
		}, message: "spec.generators[3].scmProvider.gitlab: Invalid value: providerRef and tokenRef are mutually exclusive"},
		{name: "pull request gitea providerRef with tokenRef", mutate: func(a *v1alpha1.ApplicationSet) {
			a.Spec.Generators = append(a.Spec.Generators, v1alpha1.ApplicationSetGenerator{PullRequest: &v1alpha1.PullRequestGenerator{Gitea: &v1alpha1.PullRequestGeneratorGitea{
				Owner: "o", Repo: "r", API: "https://gitea.example.com", TokenRef: &v1alpha1.SecretRef{SecretName: "s", Key: "k"}, ProviderRef: &v1alpha1.SCMProviderReference{Name: "gitea"},
			}}})
		}, message: "spec.generators[10].pullRequest.gitea: Invalid value: providerRef and tokenRef are mutually exclusive"},
		{name: "pull request gitlab providerRef with tokenRef", mutate: func(a *v1alpha1.ApplicationSet) {
			a.Spec.Generators = append(a.Spec.Generators, v1alpha1.ApplicationSetGenerator{PullRequest: &v1alpha1.PullRequestGenerator{GitLab: &v1alpha1.PullRequestGeneratorGitLab{
				Project: "42", TokenRef: &v1alpha1.SecretRef{SecretName: "s", Key: "k"}, ProviderRef: &v1alpha1.SCMProviderReference{Name: "gitlab"},
			}}})
		}, message: "spec.generators[10].pullRequest.gitlab: Invalid value: providerRef and tokenRef are mutually exclusive"},
		{name: "pull request azure devops providerRef with tokenRef", mutate: func(a *v1alpha1.ApplicationSet) {
			a.Spec.Generators = append(a.Spec.Generators, v1alpha1.ApplicationSetGenerator{PullRequest: &v1alpha1.PullRequestGenerator{AzureDevOps: &v1alpha1.PullRequestGeneratorAzureDevOps{
				Organization: "o", Project: "p", Repo: "r", TokenRef: &v1alpha1.SecretRef{SecretName: "s", Key: "k"}, ProviderRef: &v1alpha1.SCMProviderReference{Name: "ado"},
			}}})
		}, message: "spec.generators[10].pullRequest.azuredevops: Invalid value: providerRef and tokenRef are mutually exclusive"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := validApplicationSet(ns, fmt.Sprintf("bad-%d", i))
			tt.mutate(a)
			requireInvalid(t, k8sClient.Create(ctx, a), tt.message)
		})
	}
}

func TestAdmissionRepository(t *testing.T) {
	ctx, ns := newNamespace(t)

	minimal := &v1alpha1.Repository{ObjectMeta: meta(ns, "minimal"), Spec: v1alpha1.RepositorySpec{URL: "https://git.example.com/org/repo.git"}}
	require.NoError(t, k8sClient.Create(ctx, minimal))
	assert.Equal(t, v1alpha1.RepositoryTypeGit, minimal.Spec.Type, "type defaults to git")
	assert.Equal(t, v1alpha1.RepositoryPurposeRead, minimal.Spec.Purpose, "purpose defaults to read")

	full := &v1alpha1.Repository{ObjectMeta: meta(ns, "full"), Spec: v1alpha1.RepositorySpec{
		URL: "git@github.com:org/repo.git", Type: v1alpha1.RepositoryTypeGit, Name: "repo", Project: "team-a",
		Purpose: v1alpha1.RepositoryPurposeWrite, Insecure: true, InsecureIgnoreHostKey: true, EnableLFS: true, Depth: 1,
		WebhookManifestCacheWarmDisabled: true,
		RepositoryAccess: v1alpha1.RepositoryAccess{
			ForceHTTPBasicAuth: true, Proxy: "http://proxy:3128", NoProxy: "localhost", UseAzureWorkloadIdentity: true,
			GitHubApp:      &v1alpha1.RepositoryGitHubApp{AppID: 1, InstallationID: ptr.To[int64](2), EnterpriseBaseURL: "https://ghe.example.com/api/v3"},
			CredentialsRef: &v1alpha1.LocalSecretReference{Name: "repo-creds"},
			SCMProviderRef: &v1alpha1.SCMProviderReference{Name: "github"},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, full))
	require.NotNil(t, full.Spec.GitHubApp.InstallationID)
	assert.Equal(t, int64(2), *full.Spec.GitHubApp.InstallationID)
	assert.True(t, full.Spec.WebhookManifestCacheWarmDisabled)

	now := metav1.Now()
	full.Status = v1alpha1.RepositoryStatus{
		ConnectionState: &v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusSuccessful, AttemptedAt: &now},
		Conditions:      []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonConnectionSuccessful, LastTransitionTime: now}},
	}
	require.NoError(t, k8sClient.Status().Update(ctx, full))
	bad := full.DeepCopy()
	bad.Status.ConnectionState.Status = "Flaky"
	requireInvalid(t, k8sClient.Status().Update(ctx, bad), "status.connectionState.status")

	oci := &v1alpha1.Repository{ObjectMeta: meta(ns, "oci"), Spec: v1alpha1.RepositorySpec{URL: "oci://registry.example.com/charts", Type: v1alpha1.RepositoryTypeOCI,
		RepositoryAccess: v1alpha1.RepositoryAccess{InsecureOCIForceHTTP: true}}}
	require.NoError(t, k8sClient.Create(ctx, oci))

	tests := []struct {
		name    string
		spec    v1alpha1.RepositorySpec
		message string
	}{
		{name: "type", spec: v1alpha1.RepositorySpec{URL: "https://x", Type: "svn"}, message: "spec.type"},
		{name: "purpose", spec: v1alpha1.RepositorySpec{URL: "https://x", Purpose: "admin"}, message: "spec.purpose"},
		{name: "write helm", spec: v1alpha1.RepositorySpec{URL: "https://x", Type: v1alpha1.RepositoryTypeHelm, Purpose: v1alpha1.RepositoryPurposeWrite}, message: "purpose write is only supported for git repositories"},
		{name: "lfs on helm", spec: v1alpha1.RepositorySpec{URL: "https://x", Type: v1alpha1.RepositoryTypeHelm, EnableLFS: true}, message: "enableLFS is only supported for git repositories"},
		{name: "empty url", spec: v1alpha1.RepositorySpec{}, message: "spec.url"},
		{name: "empty credentials ref", spec: v1alpha1.RepositorySpec{URL: "https://x", RepositoryAccess: v1alpha1.RepositoryAccess{CredentialsRef: &v1alpha1.LocalSecretReference{}}}, message: "spec.credentialsRef.name"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &v1alpha1.Repository{ObjectMeta: meta(ns, fmt.Sprintf("bad-%d", i)), Spec: tt.spec}
			requireInvalid(t, k8sClient.Create(ctx, r), tt.message)
		})
	}
}

func TestAdmissionRepositoryCredential(t *testing.T) {
	ctx, ns := newNamespace(t)

	cred := &v1alpha1.RepositoryCredential{ObjectMeta: meta(ns, "org"), Spec: v1alpha1.RepositoryCredentialSpec{
		URLPrefix:        "https://git.example.com/org/",
		RepositoryAccess: v1alpha1.RepositoryAccess{CredentialsRef: &v1alpha1.LocalSecretReference{Name: "org-creds"}},
	}}
	require.NoError(t, k8sClient.Create(ctx, cred))
	assert.Equal(t, v1alpha1.RepositoryTypeGit, cred.Spec.Type, "type defaults to git")
	assert.Equal(t, v1alpha1.RepositoryPurposeRead, cred.Spec.Purpose, "purpose defaults to read")

	// An org-wide GitHub App template without installationID: the installation is discovered from the
	// organization in each repository URL (as Argo CD does when githubAppInstallationID is 0).
	write := &v1alpha1.RepositoryCredential{ObjectMeta: meta(ns, "write"), Spec: v1alpha1.RepositoryCredentialSpec{
		URLPrefix: "https://github.com/org/", Purpose: v1alpha1.RepositoryPurposeWrite,
		RepositoryAccess: v1alpha1.RepositoryAccess{
			GitHubApp:             &v1alpha1.RepositoryGitHubApp{AppID: 10},
			AzureServicePrincipal: &v1alpha1.AzureServicePrincipal{ClientID: "c", TenantID: "t"},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, write))
	assert.Nil(t, write.Spec.GitHubApp.InstallationID)

	requireInvalid(t, k8sClient.Create(ctx, &v1alpha1.RepositoryCredential{ObjectMeta: meta(ns, "no-prefix")}), "spec.urlPrefix")
	requireInvalid(t, k8sClient.Create(ctx, &v1alpha1.RepositoryCredential{ObjectMeta: meta(ns, "bad-type"),
		Spec: v1alpha1.RepositoryCredentialSpec{URLPrefix: "https://x/", Type: "svn"}}), "spec.type")
	requireInvalid(t, k8sClient.Create(ctx, &v1alpha1.RepositoryCredential{ObjectMeta: meta(ns, "bad-app"),
		Spec: v1alpha1.RepositoryCredentialSpec{URLPrefix: "https://x/", RepositoryAccess: v1alpha1.RepositoryAccess{GitHubApp: &v1alpha1.RepositoryGitHubApp{}}}}), "spec.githubApp.appID")
	requireInvalid(t, k8sClient.Create(ctx, &v1alpha1.RepositoryCredential{ObjectMeta: meta(ns, "bad-installation"),
		Spec: v1alpha1.RepositoryCredentialSpec{URLPrefix: "https://x/", RepositoryAccess: v1alpha1.RepositoryAccess{GitHubApp: &v1alpha1.RepositoryGitHubApp{AppID: 1, InstallationID: ptr.To[int64](0)}}}}),
		"spec.githubApp.installationID")
}

func TestAdmissionCluster(t *testing.T) {
	ctx, ns := newNamespace(t)

	cluster := &v1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "prod", Labels: map[string]string{"env": "prod", v1alpha1.LabelKeyAutoLabelClusterInfo: "true"}},
		Spec: v1alpha1.ClusterSpec{
			Server: "https://prod.k8s.example.com:6443", Name: "prod", Namespaces: []string{"a", "b"}, ClusterResources: true,
			Project: "team-a", Shard: ptr.To[int64](1),
			Config: v1alpha1.ClusterConfig{
				TLSClientConfig:    v1alpha1.TLSClientConfig{ServerName: "prod.internal"},
				AWSAuthConfig:      &v1alpha1.AWSAuthConfig{ClusterName: "prod", RoleARN: "arn:aws:iam::123456789012:role/patrol"},
				ExecProviderConfig: &v1alpha1.ExecProviderConfig{Command: "kubelogin", Args: []string{"get-token"}, Env: map[string]string{"MODE": "workload"}, APIVersion: "client.authentication.k8s.io/v1", InstallHint: "install kubelogin"},
				DisableCompression: true,
				ProxyURL:           "http://proxy:3128",
				CredentialsRef:     &v1alpha1.LocalSecretReference{Name: "prod-cluster"},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cluster))

	now := metav1.Now()
	cluster.Status = v1alpha1.ClusterStatus{
		ConnectionState: &v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusFailed, Message: "dial tcp: timeout", AttemptedAt: &now},
		ServerVersion:   "v1.36.0",
		Info: v1alpha1.ClusterInfo{CacheInfo: v1alpha1.ClusterCacheInfo{ResourcesCount: 10, APIsCount: 5, LastCacheSyncTime: &now},
			ApplicationsCount: 3, APIVersions: []string{"v1", "apps/v1"}},
		AssignedShard: ptr.To[int64](1),
		Conditions:    []metav1.Condition{{Type: v1alpha1.ConditionConnectionFailed, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonConnectionFailed, LastTransitionTime: now}},
	}
	require.NoError(t, k8sClient.Status().Update(ctx, cluster))

	inCluster := &v1alpha1.Cluster{ObjectMeta: meta(ns, "in-cluster"), Spec: v1alpha1.ClusterSpec{Server: v1alpha1.KubernetesInternalAPIServerAddr, Name: "in-cluster"}}
	require.NoError(t, k8sClient.Create(ctx, inCluster))

	tests := []struct {
		name    string
		spec    v1alpha1.ClusterSpec
		message string
	}{
		{name: "not a url", spec: v1alpha1.ClusterSpec{Server: "not a url", Name: "x"}, message: "server must be an http(s) URL"},
		{name: "wrong scheme", spec: v1alpha1.ClusterSpec{Server: "ftp://cluster", Name: "x"}, message: "server must be an http(s) URL"},
		{name: "missing name", spec: v1alpha1.ClusterSpec{Server: "https://cluster"}, message: "spec.name"},
		{name: "negative shard", spec: v1alpha1.ClusterSpec{Server: "https://cluster", Name: "x", Shard: ptr.To[int64](-1)}, message: "spec.shard"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &v1alpha1.Cluster{ObjectMeta: meta(ns, fmt.Sprintf("bad-%d", i)), Spec: tt.spec}
			requireInvalid(t, k8sClient.Create(ctx, c), tt.message)
		})
	}

	t.Run("server is immutable", func(t *testing.T) {
		cur := &v1alpha1.Cluster{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cur))
		cur.Spec.Server = "https://other.k8s.example.com:6443"
		requireInvalid(t, k8sClient.Update(ctx, cur), "server is immutable")

		cur = &v1alpha1.Cluster{}
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cur))
		cur.Spec.Name = "renamed"
		require.NoError(t, k8sClient.Update(ctx, cur), "the rest of the spec may change")
	})
}

func TestAdmissionSCMProvider(t *testing.T) {
	ctx, ns := newNamespace(t)

	github := &v1alpha1.SCMProvider{ObjectMeta: meta(ns, "github"), Spec: v1alpha1.SCMProviderSpec{
		Type: v1alpha1.SCMProviderTypeGitHub,
		Auth: &v1alpha1.SCMProviderAuth{GitHubApp: &v1alpha1.SCMProviderGitHubApp{
			AppID: 123, PrivateKeySecretRef: v1alpha1.SecretKeyReference{Name: "gh-app", Key: "private-key.pem"},
		}},
		Webhook:         &v1alpha1.SCMProviderWebhook{SecretRef: v1alpha1.SecretKeyReference{Name: "gh-webhook", Key: "secret"}},
		StatusReporting: &v1alpha1.SCMProviderStatusReporting{Enabled: true, Mode: v1alpha1.StatusReportingModeChecks},
		RateLimit:       &v1alpha1.SCMProviderRateLimit{RequestsPerSecond: 10, Burst: 20},
	}}
	require.NoError(t, k8sClient.Create(ctx, github))
	assert.Equal(t, "patrol-cd", github.Spec.StatusReporting.ContextPrefix, "contextPrefix defaults to patrol-cd")

	gitlab := &v1alpha1.SCMProvider{ObjectMeta: meta(ns, "gitlab"), Spec: v1alpha1.SCMProviderSpec{
		Type: v1alpha1.SCMProviderTypeGitLab, APIURL: "https://gitlab.example.com/api/v4", WebURL: "https://gitlab.example.com",
		CASecretRef:     &v1alpha1.SecretKeyReference{Name: "ca", Key: "ca.crt"},
		Auth:            &v1alpha1.SCMProviderAuth{TokenSecretRef: &v1alpha1.SecretKeyReference{Name: "gitlab", Key: "token"}},
		StatusReporting: &v1alpha1.SCMProviderStatusReporting{Enabled: true},
	}}
	require.NoError(t, k8sClient.Create(ctx, gitlab))
	assert.Equal(t, v1alpha1.StatusReportingModeStatuses, gitlab.Spec.StatusReporting.Mode, "mode defaults to statuses")
	assert.Equal(t, "patrol-cd", gitlab.Spec.StatusReporting.ContextPrefix)

	fake := &v1alpha1.SCMProvider{ObjectMeta: meta(ns, "fake"), Spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeFake}}
	require.NoError(t, k8sClient.Create(ctx, fake))

	now := metav1.Now()
	github.Status = v1alpha1.SCMProviderStatus{
		Capabilities:  []string{"RepositoryLister", "CheckRunReporter"},
		Conditions:    []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonConnectionSuccessful, LastTransitionTime: now}},
		RateLimit:     &v1alpha1.SCMProviderRateLimitStatus{Limit: 5000, Remaining: 4999, ResetAt: &now},
		LastCheckedAt: &now,
	}
	require.NoError(t, k8sClient.Status().Update(ctx, github))

	tests := []struct {
		name    string
		spec    v1alpha1.SCMProviderSpec
		message string
	}{
		{name: "type", spec: v1alpha1.SCMProviderSpec{Type: "perforce"}, message: "spec.type"},
		{name: "mode", spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitHub, StatusReporting: &v1alpha1.SCMProviderStatusReporting{Mode: "comments"}}, message: "spec.statusReporting.mode"},
		{name: "checks without github app", spec: v1alpha1.SCMProviderSpec{
			Type: v1alpha1.SCMProviderTypeGitHub, Auth: &v1alpha1.SCMProviderAuth{TokenSecretRef: &v1alpha1.SecretKeyReference{Name: "t", Key: "k"}},
			StatusReporting: &v1alpha1.SCMProviderStatusReporting{Mode: v1alpha1.StatusReportingModeChecks},
		}, message: "statusReporting.mode checks requires type github with auth.githubApp"},
		{name: "checks on gitlab", spec: v1alpha1.SCMProviderSpec{
			Type: v1alpha1.SCMProviderTypeGitLab, Auth: &v1alpha1.SCMProviderAuth{GitHubApp: &v1alpha1.SCMProviderGitHubApp{AppID: 1, PrivateKeySecretRef: v1alpha1.SecretKeyReference{Name: "a", Key: "b"}}},
			StatusReporting: &v1alpha1.SCMProviderStatusReporting{Mode: v1alpha1.StatusReportingModeChecks},
		}, message: "statusReporting.mode checks requires type github"},
		{name: "two auth methods", spec: v1alpha1.SCMProviderSpec{
			Type: v1alpha1.SCMProviderTypeGitea,
			Auth: &v1alpha1.SCMProviderAuth{TokenSecretRef: &v1alpha1.SecretKeyReference{Name: "t", Key: "k"}, BasicAuthSecretRef: &v1alpha1.LocalSecretReference{Name: "b"}},
		}, message: "at most one of githubApp, tokenSecretRef or basicAuthSecretRef"},
		{name: "rate limit", spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitHub, RateLimit: &v1alpha1.SCMProviderRateLimit{RequestsPerSecond: 0}}, message: "spec.rateLimit.requestsPerSecond"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &v1alpha1.SCMProvider{ObjectMeta: meta(ns, fmt.Sprintf("bad-%d", i)), Spec: tt.spec}
			requireInvalid(t, k8sClient.Create(ctx, p), tt.message)
		})
	}
}
