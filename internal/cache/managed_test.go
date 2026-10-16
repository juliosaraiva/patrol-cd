package cache

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Targets whose scope the cache does not cover (list denied in that namespace, or the API's
// registration reported gone) must be fetched with GET instead of being reported absent.
func TestGetManagedLiveObjsFetchesScopesTheCacheDoesNotCover(t *testing.T) {
	t.Parallel()
	cmPath := func(ns, name string) string { return "/api/v1/namespaces/" + ns + "/configmaps/" + name }
	tests := []struct {
		name        string
		target      *unstructured.Unstructured
		serverObj   bool
		getStatus   int
		markGone    bool
		wantPresent bool
		wantGets    int
		wantErr     func(error) bool
	}{
		{name: "list denied in target namespace: GET returns the object",
			target: mkObj("v1", "ConfigMap", "b", "x", ""), serverObj: true, wantPresent: true, wantGets: 1},
		{name: "list denied in target namespace: 403 on GET is an error",
			target: mkObj("v1", "ConfigMap", "b", "x", ""), getStatus: http.StatusForbidden, wantGets: 1, wantErr: apierrors.IsForbidden},
		{name: "list denied in target namespace: 404 on GET is absent",
			target: mkObj("v1", "ConfigMap", "b", "x", ""), wantGets: 1},
		{name: "watched namespace: an uncached object is absent without GET",
			target: mkObj("v1", "ConfigMap", "a", "x", ""), serverObj: true, wantGets: 0},
		{name: "registration reported gone: GET returns the object",
			target: mkObj("v1", "ConfigMap", "a", "x", ""), serverObj: true, markGone: true, wantPresent: true, wantGets: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fc := newFakeCluster(t, cmResource)
			fc.scope(cmResource.gvr(), "b").set(func(s *fakeScope) { s.listErr = apierrors.NewForbidden(cmGR, "", assert.AnError) })
			path := cmPath(tc.target.GetNamespace(), tc.target.GetName())
			var obj *unstructured.Unstructured
			if tc.serverObj {
				obj = mkObj("v1", "ConfigMap", tc.target.GetNamespace(), tc.target.GetName(), "uid-x")
			}
			fc.setObject(path, obj, tc.getStatus)

			c := fc.start(t, Settings{Namespaces: []string{"a", "b"}, RespectRBAC: RespectRBACNormal})
			denied := unavailable(c, ReasonListForbidden)
			require.Len(t, denied, 1)
			require.Equal(t, UnavailableAPI{GroupKind: cmGK, Namespace: "b", Reason: ReasonListForbidden, Message: denied[0].Message}, denied[0])
			_, registered := registration(c, cmGK)
			require.True(t, registered, "the kind stays watched in the namespaces it can be listed in")
			if tc.markGone {
				c.mu.Lock()
				c.apis[cmGK].dead = true
				c.mu.Unlock()
			}

			live, err := c.GetManagedLiveObjs(context.Background(), []*unstructured.Unstructured{tc.target}, nil)
			assert.Equal(t, tc.wantGets, fc.getCount(path))
			assert.Equal(t, tc.wantGets, fc.totalGets())
			if tc.wantErr != nil {
				require.Error(t, err)
				assert.True(t, tc.wantErr(err), "unexpected error: %v", err)
				return
			}
			require.NoError(t, err)
			got, present := live[KeyOf(tc.target)]
			require.Equal(t, tc.wantPresent, present)
			if tc.wantPresent {
				assert.Equal(t, "uid-x", string(got.GetUID()))
			}
		})
	}
}
