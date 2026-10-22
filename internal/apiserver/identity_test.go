package apiserver_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver"
)

func projectWithTokens(items ...v1alpha1.JWTToken) *v1alpha1.AppProject {
	return &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "patrol"},
		Spec:       v1alpha1.AppProjectSpec{Roles: []v1alpha1.ProjectRole{{Name: "ci"}}},
		Status:     v1alpha1.AppProjectStatus{JWTTokensByRole: map[string]v1alpha1.JWTTokens{"ci": {Items: items}}},
	}
}

// getter returns a ProjectGetter that answers with p or err and counts its calls.
func getter(p *v1alpha1.AppProject, err error) (apiserver.ProjectGetter, *atomic.Int32) {
	var n atomic.Int32
	return func(context.Context, string) (*v1alpha1.AppProject, error) {
		n.Add(1)
		return p, err
	}, &n
}

var notFound = apierrors.NewNotFound(v1alpha1.Resource("appprojects"), "team")

func TestProjectTokenRegistryWithFallback(t *testing.T) {
	t.Parallel()
	const subject = "proj:team:ci"
	fresh := projectWithTokens(v1alpha1.JWTToken{ID: "t1", IssuedAt: 100})
	stale := projectWithTokens()
	boom := errors.New("boom")

	for _, tc := range []struct {
		name        string
		cached      *v1alpha1.AppProject
		cachedErr   error
		live        *v1alpha1.AppProject
		liveErr     error
		jti         string
		want        bool
		wantErr     bool
		wantLiveGet int32
	}{
		{name: "cached lists the token: no live read", cached: fresh, jti: "t1", want: true, wantLiveGet: 0},
		{name: "cached view is stale: the live read finds it", cached: stale, live: fresh, jti: "t1", want: true, wantLiveGet: 1},
		{name: "cached view has not seen the project: the live read finds it", cachedErr: notFound, live: fresh, jti: "t1", want: true, wantLiveGet: 1},
		{name: "revoked: neither lists it", cached: stale, live: stale, jti: "t1", want: false, wantLiveGet: 1},
		{name: "another token id is not known", cached: fresh, live: fresh, jti: "t2", want: false, wantLiveGet: 1},
		{name: "project deleted", cachedErr: notFound, liveErr: notFound, jti: "t1", want: false, wantLiveGet: 1},
		{name: "cached read fails: the error is returned", cachedErr: boom, live: fresh, jti: "t1", wantErr: true, wantLiveGet: 0},
		{name: "live read fails: the error is returned", cached: stale, liveErr: boom, jti: "t1", wantErr: true, wantLiveGet: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cached, cachedCalls := getter(tc.cached, tc.cachedErr)
			live, liveCalls := getter(tc.live, tc.liveErr)
			known, err := apiserver.ProjectTokenRegistryWithFallback(cached, live).IsTokenKnown(t.Context(), subject, tc.jti, 100)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, known)
			assert.EqualValues(t, 1, cachedCalls.Load())
			assert.Equal(t, tc.wantLiveGet, liveCalls.Load())
		})
	}
}

func TestProjectTokenRegistryWithFallbackFindsTokensWithoutAnID(t *testing.T) {
	t.Parallel()
	legacy := projectWithTokens(v1alpha1.JWTToken{IssuedAt: 100})
	cached, _ := getter(projectWithTokens(), nil)
	live, _ := getter(legacy, nil)
	reg := apiserver.ProjectTokenRegistryWithFallback(cached, live)
	known, err := reg.IsTokenKnown(t.Context(), "proj:team:ci", "", 100)
	require.NoError(t, err)
	assert.True(t, known)
	known, err = reg.IsTokenKnown(t.Context(), "proj:team:ci", "", 101)
	require.NoError(t, err)
	assert.False(t, known, "a different issue time is another token")
}

func TestProjectTokenRegistryDoesNotFallBack(t *testing.T) {
	t.Parallel()
	get, calls := getter(projectWithTokens(), nil)
	known, err := apiserver.ProjectTokenRegistry(get).IsTokenKnown(t.Context(), "proj:team:ci", "t1", 100)
	require.NoError(t, err)
	assert.False(t, known)
	assert.EqualValues(t, 1, calls.Load())
	known, err = apiserver.ProjectTokenRegistry(get).IsTokenKnown(t.Context(), "not-a-project-subject", "t1", 100)
	require.NoError(t, err)
	assert.False(t, known)
}
