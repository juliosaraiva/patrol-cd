package projects

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func TestDiffPaths(t *testing.T) {
	t.Parallel()
	a := v1alpha1.AppProjectSpec{Description: "a", SourceRepos: []string{"x"}, Roles: []v1alpha1.ProjectRole{{Name: "r", Groups: []string{"g"}}}}
	b := v1alpha1.AppProjectSpec{Description: "b", SourceRepos: []string{"x", "y"}, Roles: []v1alpha1.ProjectRole{{Name: "r", Groups: []string{"h"}}}}
	assert.Equal(t, []string{"SourceRepos", "1", "Description", "Roles", "0", "Groups", "0"}, diffPaths(a, b))
	assert.Empty(t, diffPaths(v1alpha1.AppProjectSpec{SourceRepos: []string{}}, v1alpha1.AppProjectSpec{}), "nil and empty are equal")
	w := true
	assert.Equal(t, []string{"OrphanedResources"}, diffPaths(v1alpha1.AppProjectSpec{}, v1alpha1.AppProjectSpec{OrphanedResources: &v1alpha1.OrphanedResourcesMonitorSettings{Warn: &w}}))
	m1 := map[string]string{"a": "1"}
	m2 := map[string]string{"a": "2", "b": "3"}
	assert.Equal(t, []string{"a", "b"}, diffPaths(m1, m2))
}

func TestSymmetricDifference(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"a", "d"}, symmetricDifference([]string{"a", "b", "c"}, []string{"b", "c", "d"}))
	// Upstream counts occurrences: a value listed twice and removed escaped the check.
	assert.Equal(t, []string{"x"}, symmetricDifference([]string{"x", "x"}, nil))
	assert.Empty(t, symmetricDifference([]string{"x", "x"}, []string{"x"}))
	assert.Equal(t, []string{""}, symmetricDifference([]string{""}, nil), "name-only destinations contribute an empty server")
}

func TestValidateSourceNamespaces(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in      []string
		wantErr string
	}{
		{[]string{"team-*", "/^prod-[a-z]+$/", "exact"}, ""},
		{[]string{""}, "must not be empty"},
		{[]string{"a b"}, "whitespace"},
		{[]string{"/(/"}, "not a valid regular expression"},
		{[]string{"[a"}, "not a valid pattern"},
		{[]string{string(make([]byte, 300))}, "longer than"},
	} {
		err := validateSourceNamespaces(tc.in)
		if tc.wantErr == "" {
			assert.NoError(t, err, tc.in)
			continue
		}
		require.Error(t, err, tc.in)
		assert.Contains(t, err.Error(), tc.wantErr)
	}
}

func TestValidationErrorCodes(t *testing.T) {
	t.Parallel()
	for code, status := range map[project.Code]int{
		project.CodeInvalidArgument: http.StatusBadRequest,
		project.CodeOutOfRange:      http.StatusBadRequest,
		project.CodeAlreadyExists:   http.StatusConflict,
		project.CodeNotFound:        http.StatusNotFound,
		project.CodeInternal:        http.StatusInternalServerError,
	} {
		err := validationError(&project.ValidationError{Code: code, Message: "m"}, "p: ")
		assert.Equal(t, status, rest.ProblemFor(err).Status, code)
	}
	assert.Equal(t, "p: m", rest.ProblemFor(validationError(&project.ValidationError{Code: project.CodeInvalidArgument, Message: "m"}, "p: ")).Detail)
}

func TestKeyLock(t *testing.T) {
	t.Parallel()
	l := newKeyLock()
	var inside, maxInside atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			unlock := l.Lock("p")
			n := inside.Add(1)
			for {
				m := maxInside.Load()
				if n <= m || maxInside.CompareAndSwap(m, n) {
					break
				}
			}
			inside.Add(-1)
			unlock()
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), maxInside.Load())
	l.mu.Lock()
	assert.Empty(t, l.m, "entries are released")
	l.mu.Unlock()
	// Different keys do not block each other.
	u1 := l.Lock("a")
	u2 := l.Lock("b")
	u2()
	u1()
}

func TestKeepTokens(t *testing.T) {
	t.Parallel()
	old := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{Roles: []v1alpha1.ProjectRole{{Name: "r", JWTTokens: []v1alpha1.JWTToken{{ID: "1", IssuedAt: 1}}}}}} //nolint:staticcheck // deprecated field under test
	next := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{Roles: []v1alpha1.ProjectRole{
		{Name: "r", JWTTokens: []v1alpha1.JWTToken{{ID: "forged", IssuedAt: 2}}},   //nolint:staticcheck // deprecated field under test
		{Name: "new", JWTTokens: []v1alpha1.JWTToken{{ID: "forged", IssuedAt: 2}}}, //nolint:staticcheck // deprecated field under test
	}}}
	keepTokens(next, old)
	assert.Equal(t, []v1alpha1.JWTToken{{ID: "1", IssuedAt: 1}}, next.Spec.Roles[0].JWTTokens) //nolint:staticcheck // deprecated field under test
	assert.Empty(t, next.Spec.Roles[1].JWTTokens)                                              //nolint:staticcheck // deprecated field under test
}

func TestRoutes(t *testing.T) {
	t.Parallel()
	tbl := rest.NewTable()
	New(Deps{}).Register(tbl)
	require.NoError(t, tbl.Err())
	var got []string
	for _, r := range tbl.Routes() {
		got = append(got, r.Method+" "+r.Path+" "+r.Access.Resource+","+r.Access.Action)
	}
	assert.Equal(t, []string{
		"GET /api/v1/projects projects,get",
		"POST /api/v1/projects projects,create",
		"DELETE /api/v1/projects/{name} projects,delete",
		"GET /api/v1/projects/{name} projects,get",
		"PUT /api/v1/projects/{name} projects,update",
		"GET /api/v1/projects/{name}/detailed projects,get",
		"GET /api/v1/projects/{name}/events projects,get",
		"GET /api/v1/projects/{name}/globalprojects projects,get",
		"GET /api/v1/projects/{name}/links projects,get",
		"GET /api/v1/projects/{name}/syncwindows projects,get",
		"POST /api/v1/projects/{project}/roles/{role}/token projects,update",
		"DELETE /api/v1/projects/{project}/roles/{role}/token/{iat} projects,update",
	}, got)
}

func TestUnconfigured(t *testing.T) {
	t.Parallel()
	s := New(Deps{})
	_, err := s.get(context.Background(), &apiclient.ProjectRequest{Name: "x"})
	assert.Equal(t, http.StatusServiceUnavailable, rest.ProblemFor(err).Status)
	s = New(Deps{Config: Config{Namespace: "ns"}, Client: nil})
	_, err = s.createToken(context.Background(), &apiclient.ProjectTokenCreateRequest{Project: "x", Role: "r"})
	assert.Equal(t, http.StatusServiceUnavailable, rest.ProblemFor(err).Status)
}
