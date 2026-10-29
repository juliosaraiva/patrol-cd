package applications

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
)

func healthNode(group, kind, name string, h *v1alpha1.HealthStatus) compare.ResourceNode {
	return compare.ResourceNode{ResourceRef: compare.ResourceRef{Group: group, Version: "v1", Kind: kind, Namespace: "ns", Name: name}, Health: h}
}

func healthKey(group, kind, name string) compare.ResourceKey {
	return compare.ResourceKey{Group: group, Kind: kind, Namespace: "ns", Name: name}
}

func TestTreeHealthByKey(t *testing.T) {
	t.Parallel()
	notCreated := &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusMissing, Message: resourceNotCreatedMessage}
	now := metav1.Now()
	withUID := healthNode("", "ConfigMap", "uid", nil)
	withUID.UID = "u-1"
	withRV := healthNode("", "ConfigMap", "rv", nil)
	withRV.ResourceVersion = "42"
	withCreated := healthNode("", "ConfigMap", "created", nil)
	withCreated.CreatedAt = &now

	tests := []struct {
		name string
		tree *compare.ApplicationTree
		want map[compare.ResourceKey]*v1alpha1.HealthStatus
	}{
		{name: "nil tree", tree: nil, want: map[compare.ResourceKey]*v1alpha1.HealthStatus{}},
		{
			name: "node health",
			tree: &compare.ApplicationTree{Nodes: []compare.ResourceNode{
				healthNode("apps", "Deployment", "web", &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusDegraded, Message: "bad"}),
			}},
			want: map[compare.ResourceKey]*v1alpha1.HealthStatus{
				healthKey("apps", "Deployment", "web"): {Status: v1alpha1.HealthStatusDegraded, Message: "bad"},
			},
		},
		{
			name: "no health and no live object",
			tree: &compare.ApplicationTree{Nodes: []compare.ResourceNode{healthNode("", "ConfigMap", "pending", nil)}},
			want: map[compare.ResourceKey]*v1alpha1.HealthStatus{healthKey("", "ConfigMap", "pending"): notCreated},
		},
		{
			name: "no health with a UID, a resourceVersion or a creation time",
			tree: &compare.ApplicationTree{Nodes: []compare.ResourceNode{withUID, withRV, withCreated}},
			want: map[compare.ResourceKey]*v1alpha1.HealthStatus{},
		},
		{
			name: "orphaned nodes do not count",
			tree: &compare.ApplicationTree{OrphanedNodes: []compare.ResourceNode{
				healthNode("", "ConfigMap", "stray", &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy}),
				healthNode("", "ConfigMap", "stray-pending", nil),
			}},
			want: map[compare.ResourceKey]*v1alpha1.HealthStatus{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, treeHealthByKey(tc.tree))
		})
	}

	t.Run("values are copies", func(t *testing.T) {
		t.Parallel()
		h := &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy}
		tree := &compare.ApplicationTree{Nodes: []compare.ResourceNode{healthNode("apps", "Deployment", "web", h)}}
		got := treeHealthByKey(tree)[healthKey("apps", "Deployment", "web")]
		require.NotNil(t, got)
		got.Status = v1alpha1.HealthStatusDegraded
		assert.Equal(t, v1alpha1.HealthStatusHealthy, tree.Nodes[0].Health.Status)
	})
}

// countingTree is a TreeClient whose Tree answers tree or err, or blocks until its context
// ends; it counts Tree calls.
type countingTree struct {
	tree  *compare.ApplicationTree
	err   error
	block bool
	calls atomic.Int64
}

func (f *countingTree) Tree(ctx context.Context, _ *v1alpha1.Application) (*compare.ApplicationTree, error) {
	f.calls.Add(1)
	if f.block {
		<-ctx.Done()
		return nil, fmt.Errorf("treeapi client: %w", ctx.Err())
	}
	return f.tree, f.err
}

func (f *countingTree) TreeIfChanged(context.Context, *v1alpha1.Application, string) (*compare.ApplicationTree, string, bool, error) {
	return nil, "", false, errors.New("not used")
}

func (f *countingTree) ManagedResources(context.Context, *v1alpha1.Application, treeapi.ManagedResourcesQuery) ([]treeapi.ResourceDiff, error) {
	return nil, errors.New("not used")
}

func (f *countingTree) ServerSideDiff(context.Context, *v1alpha1.Application, []treeapi.ResourceDiff, []string) (*treeapi.ServerSideDiffResponse, error) {
	return nil, errors.New("not used")
}

func TestWithTreeHealth(t *testing.T) {
	t.Parallel()
	unknown := func() *v1alpha1.HealthStatus { return &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusUnknown} }
	// newApp returns an Application whose stored resource health is Unknown (so a fill that
	// modified its argument would show).
	newApp := func(source v1alpha1.ResourceHealthLocation, resources ...v1alpha1.ResourceStatus) *v1alpha1.Application {
		a := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "app"}}
		a.Status.ResourceHealthSource = source
		a.Status.Resources = resources
		return a
	}
	res := func(group, kind, name string) v1alpha1.ResourceStatus {
		return v1alpha1.ResourceStatus{Group: group, Version: "v1", Kind: kind, Namespace: "ns", Name: name, Health: unknown()}
	}
	treeAll := &compare.ApplicationTree{
		Nodes: []compare.ResourceNode{
			healthNode("apps", "Deployment", "web", &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy}),
			healthNode("", "ConfigMap", "cfg", &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusDegraded, Message: "bad"}),
			healthNode("", "ConfigMap", "pending", nil),
		},
		OrphanedNodes: []compare.ResourceNode{healthNode("", "ConfigMap", "stray", &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy})},
	}
	appTree := func() *v1alpha1.Application {
		return newApp(v1alpha1.ResourceHealthLocationAppTree,
			res("apps", "Deployment", "web"), res("", "ConfigMap", "cfg"), res("", "ConfigMap", "pending"),
			res("", "ConfigMap", "stray"), res("", "Secret", "no-node"))
	}
	healthy := &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy}

	tests := []struct {
		name      string
		tree      *countingTree // nil: Deps without a tree client
		app       *v1alpha1.Application
		wantCalls int64
		// wantHealth is the health of each resource of the result, in order; nil: the
		// argument itself is returned.
		wantHealth []*v1alpha1.HealthStatus
		wantErr    error
		wantPause  bool
	}{
		{
			name: "appTree source is filled", tree: &countingTree{tree: treeAll}, app: appTree(), wantCalls: 1,
			wantHealth: []*v1alpha1.HealthStatus{
				healthy,
				{Status: v1alpha1.HealthStatusDegraded, Message: "bad"},
				{Status: v1alpha1.HealthStatusMissing, Message: resourceNotCreatedMessage},
				nil, // only an orphaned node
				nil, // no node
			},
		},
		{
			name: "empty tree clears every health", tree: &countingTree{tree: &compare.ApplicationTree{}}, app: appTree(), wantCalls: 1,
			wantHealth: []*v1alpha1.HealthStatus{nil, nil, nil, nil, nil},
		},
		{name: "inline source is not read", tree: &countingTree{tree: treeAll}, app: newApp(v1alpha1.ResourceHealthLocationInline, res("apps", "Deployment", "web"))},
		{name: "no resources", tree: &countingTree{tree: treeAll}, app: newApp(v1alpha1.ResourceHealthLocationAppTree)},
		{name: "no tree client", app: appTree()},
		{
			name: "controller answer", tree: &countingTree{err: &treeapi.Error{Status: 503, Code: treeapi.CodeUnavailable, Message: "not compared yet"}},
			app: appTree(), wantCalls: 1, wantErr: treeapi.ErrUnavailable,
		},
		{
			name: "controller does not answer", tree: &countingTree{block: true},
			app: appTree(), wantCalls: 1, wantErr: context.DeadlineExceeded, wantPause: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := Deps{Logger: slog.New(slog.DiscardHandler), Resources: ResourceDeps{ResourceHealthTimeout: 50 * time.Millisecond}}
			if tc.tree != nil {
				d.Resources.Tree = tc.tree
			}
			s := New(d)
			before := tc.app.DeepCopy()

			start := time.Now()
			got, err := s.withTreeHealth(context.Background(), tc.app)
			assert.Less(t, time.Since(start), 5*time.Second, "bounded by ResourceHealthTimeout")

			if tc.wantErr != nil {
				require.Error(t, err)
				assert.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantPause, pausesTreeHealth(err))
			if tc.tree != nil {
				assert.Equal(t, tc.wantCalls, tc.tree.calls.Load(), "tree reads")
			}
			assert.Equal(t, before, tc.app, "the argument is never modified")
			if tc.wantHealth == nil {
				assert.Same(t, tc.app, got)
				return
			}
			require.NotSame(t, tc.app, got)
			require.Len(t, got.Status.Resources, len(tc.wantHealth))
			for i, want := range tc.wantHealth {
				assert.Equal(t, want, got.Status.Resources[i].Health, got.Status.Resources[i].Name)
			}
		})
	}
}

func TestPausesTreeHealth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "not compared yet", err: fmt.Errorf("read: %w", &treeapi.Error{Code: treeapi.CodeUnavailable}), want: false},
		{name: "unknown application", err: &treeapi.Error{Code: treeapi.CodeNotFound}, want: false},
		{name: "misdirected", err: &treeapi.Error{Code: treeapi.CodeMisdirected}, want: false},
		{name: "transport failure", err: fmt.Errorf("treeapi client: %w: %w", treeapi.ErrUnavailable, io.EOF), want: true},
		{name: "timeout", err: fmt.Errorf("read: %w", fmt.Errorf("treeapi client: %w", context.DeadlineExceeded)), want: true},
		{name: "routing error", err: errors.New("no endpoint for shard 2"), want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, pausesTreeHealth(tc.err))
		})
	}
}
