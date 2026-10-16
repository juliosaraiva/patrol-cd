package app

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// fakeClient implements Client with in-memory state for unit tests; methods a test does not
// set fail the test through the embedded nil interface (panic on call).
type fakeClient struct {
	Client
	mu sync.Mutex

	apps       map[string]*v1alpha1.Application // by name
	managed    []apiclient.ResourceDiff
	tree       *apiclient.ApplicationTree
	settings   *apiclient.Settings
	manifests  []string
	windows    *apiclient.ApplicationSyncWindowsResponse
	ssd        func(req *apiclient.ApplicationServerSideDiffRequest) (*apiclient.ServerSideDiffResponse, error)
	specs      []*apiclient.ApplicationUpdateSpecRequest
	updates    []*apiclient.ApplicationUpdateRequest
	deletes    []*apiclient.ApplicationDeleteRequest
	ssdCalls   int
	manifestQs []*apiclient.ApplicationManifestQuery
}

func newFakeClient(apps ...*v1alpha1.Application) *fakeClient {
	f := &fakeClient{apps: map[string]*v1alpha1.Application{}, settings: &apiclient.Settings{URL: "https://patrol.example.com"}}
	for _, a := range apps {
		f.apps[a.Name] = a.DeepCopy()
	}
	return f
}

func notFound() error {
	return &apiclient.APIError{StatusCode: http.StatusForbidden, Problem: apiclient.Problem{Status: http.StatusForbidden, Code: apiclient.CodePermissionDenied, Detail: "permission denied"}}
}

func (f *fakeClient) GetApplication(_ context.Context, req *apiclient.ApplicationGetRequest) (*v1alpha1.Application, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.apps[req.Name]
	if !ok {
		return nil, notFound()
	}
	return a.DeepCopy(), nil
}

func (f *fakeClient) ListApplications(_ context.Context, q *apiclient.ApplicationQuery) (*v1alpha1.ApplicationList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &v1alpha1.ApplicationList{}
	for _, a := range f.apps {
		out.Items = append(out.Items, *a.DeepCopy())
	}
	return out, nil
}

func (f *fakeClient) UpdateApplicationSpec(_ context.Context, req *apiclient.ApplicationUpdateSpecRequest) (*v1alpha1.ApplicationSpec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, req)
	if a, ok := f.apps[req.Name]; ok {
		a.Spec = *req.Spec.DeepCopy()
	}
	return req.Spec, nil
}

func (f *fakeClient) UpdateApplication(_ context.Context, req *apiclient.ApplicationUpdateRequest) (*v1alpha1.Application, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, req)
	f.apps[req.Application.Name] = req.Application.DeepCopy()
	return req.Application, nil
}

func (f *fakeClient) DeleteApplication(_ context.Context, req *apiclient.ApplicationDeleteRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, req)
	delete(f.apps, req.Name)
	return nil
}

func (f *fakeClient) GetApplicationManagedResources(context.Context, *apiclient.ApplicationManagedResourcesQuery) (*apiclient.ManagedResourcesResponse, error) {
	return &apiclient.ManagedResourcesResponse{Items: f.managed}, nil
}

func (f *fakeClient) GetApplicationResourceTree(context.Context, *apiclient.ApplicationTreeQuery) (*apiclient.ApplicationTree, error) {
	if f.tree == nil {
		return &apiclient.ApplicationTree{}, nil
	}
	return f.tree, nil
}

func (f *fakeClient) GetSettings(context.Context) (*apiclient.Settings, error) {
	return f.settings, nil
}

func (f *fakeClient) BaseURL() string { return "https://patrol.example.com" }

func (f *fakeClient) GetApplicationSyncWindows(context.Context, *apiclient.ApplicationRef) (*apiclient.ApplicationSyncWindowsResponse, error) {
	if f.windows != nil {
		return f.windows, nil
	}
	return &apiclient.ApplicationSyncWindowsResponse{CanSync: true, Status: "Sync Allowed"}, nil
}

func (f *fakeClient) GetApplicationOperation(context.Context, *apiclient.ApplicationOperationRequest) (*v1alpha1.Operation, error) {
	return nil, &apiclient.APIError{StatusCode: http.StatusNotFound, Problem: apiclient.Problem{Status: http.StatusNotFound, Code: apiclient.CodeNotFound}}
}

func (f *fakeClient) GetApplicationManifests(_ context.Context, q *apiclient.ApplicationManifestQuery) (*apiclient.ApplicationManifestResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.manifestQs = append(f.manifestQs, q)
	return &apiclient.ApplicationManifestResponse{Manifests: f.manifests}, nil
}

func (f *fakeClient) ApplicationServerSideDiff(_ context.Context, req *apiclient.ApplicationServerSideDiffRequest) (*apiclient.ServerSideDiffResponse, error) {
	f.mu.Lock()
	f.ssdCalls++
	fn := f.ssd
	f.mu.Unlock()
	return fn(req)
}

// testEnvWith returns an env using cl, with fast polling and no prompts.
func testEnvWith(cl Client) *env {
	e := defaultTestEnv()
	e.client = func(context.Context) (Client, error) { return cl, nil }
	e.pollInterval = 0
	e.watchRetryDelay = 0
	return e
}

// runCmd runs the app (or appset) command tree with args and returns stdout, stderr and
// the error.
func runCmd(t *testing.T, e *env, stdin string, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	e.prompter = func() *cmdutil.Prompter { return cmdutil.NewPrompter(bytes.NewBufferString(stdin), &errOut) }
	root := &cobra.Command{Use: "patrol", SilenceErrors: true, SilenceUsage: true}
	root.AddCommand(newAppCommand(e), newAppSetCommand(e))
	root.SetArgs(args)
	root.SetIn(bytes.NewBufferString(stdin))
	root.SetOut(&out)
	root.SetErr(&errOut)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func requireExitCode(t *testing.T, err error, code int) {
	t.Helper()
	require.Error(t, err)
	got, _ := cmdutil.ExitCodeOf(err)
	require.Equal(t, code, got, "error: %v", err)
}
