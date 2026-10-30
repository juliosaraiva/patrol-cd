package server

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/app"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
)

// fixedState is a stateSource with one state, or none.
type fixedState struct {
	state *app.AppState
}

func (f fixedState) State(types.NamespacedName) (app.AppState, bool) {
	if f.state == nil {
		return app.AppState{}, false
	}
	return *f.state, true
}

// An Application with spec errors has an empty tree but no managed resources to report: they
// are unavailable, as for one never compared, so the API reports its conditions instead of an
// empty list that reads as "no diff".
func TestTreeStateOfApplicationState(t *testing.T) {
	t.Parallel()
	managed := []compare.ManagedResource{{Key: compare.ResourceKey{Kind: "ConfigMap", Name: "cm"}}}
	tree := &compare.ApplicationTree{Nodes: []compare.ResourceNode{{ResourceRef: compare.ResourceRef{Kind: "ConfigMap", Name: "cm"}}}}
	for _, tc := range []struct {
		name            string
		state           *app.AppState
		wantTree        *compare.ApplicationTree
		wantManaged     []compare.ManagedResource
		treeUnavailable bool
		managedErr      bool
	}{
		{name: "compared", state: &app.AppState{Tree: tree, Managed: managed}, wantTree: tree, wantManaged: managed},
		{name: "compared with no managed resources", state: &app.AppState{Tree: &compare.ApplicationTree{}}, wantTree: &compare.ApplicationTree{}},
		{name: "spec errors", state: &app.AppState{Tree: &compare.ApplicationTree{}, SpecInvalid: true}, wantTree: &compare.ApplicationTree{}, managedErr: true},
		{name: "never compared", treeUnavailable: true, managedErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sch := runtime.NewScheme()
			require.NoError(t, v1alpha1.AddToScheme(sch))
			a := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: "patrol", Name: "a"}}
			ts := &treeState{
				reader:  fake.NewClientBuilder().WithScheme(sch).WithObjects(a).Build(),
				app:     fixedState{state: tc.state},
				matcher: core.NewNamespaceMatcher("patrol", nil),
			}
			key := treeapi.AppKey{Namespace: "patrol", Name: "a"}

			gotTree, err := ts.Tree(t.Context(), key)
			if tc.treeUnavailable {
				assert.True(t, errors.Is(err, treeapi.ErrUnavailable), "%v", err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantTree, gotTree)
			}
			gotManaged, err := ts.ManagedResources(t.Context(), key)
			if tc.managedErr {
				assert.True(t, errors.Is(err, treeapi.ErrUnavailable), "%v", err)
				assert.Nil(t, gotManaged)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantManaged, gotManaged)
			}
		})
	}
}
