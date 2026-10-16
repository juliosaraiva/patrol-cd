package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/notifications"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// fakeRepoClient implements the two repo-server calls notifications use.
type fakeRepoClient struct {
	apiclient.Client
	metaReq    *apiclient.RevisionMetadataRequest
	detailsReq *apiclient.AppDetailsQuery
	meta       *apiclient.RevisionMetadata
	details    *apiclient.AppDetails
	err        error
}

func (f *fakeRepoClient) GetRevisionMetadata(_ context.Context, req *apiclient.RevisionMetadataRequest) (*apiclient.RevisionMetadata, error) {
	f.metaReq = req
	return f.meta, f.err
}

func (f *fakeRepoClient) GetAppDetails(_ context.Context, req *apiclient.AppDetailsQuery) (*apiclient.AppDetails, error) {
	f.detailsReq = req
	return f.details, f.err
}

type fakeResolver struct {
	repo *apiclient.Repository
	err  error
	got  [2]string
}

func (f *fakeResolver) ResolveRepository(_ context.Context, repoURL, project string) (*apiclient.Repository, error) {
	f.got = [2]string{repoURL, project}
	return f.repo, f.err
}

func TestRepoServiceGetCommitMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	date := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	c := &fakeRepoClient{meta: &apiclient.RevisionMetadata{Author: "a <a@example.com>", Message: "fix", Date: date, Tags: []string{"v1"}}}
	res := &fakeResolver{repo: &apiclient.Repository{Username: "u", Password: "p"}}
	svc := NewRepoService(c, res)

	md, err := svc.GetCommitMetadata(ctx, "https://git.example.com/r.git", "abc", "team")
	require.NoError(t, err)
	assert.Equal(t, &notifications.CommitMetadata{Message: "fix", Author: "a <a@example.com>", Date: date, Tags: []string{"v1"}}, md)
	assert.Equal(t, [2]string{"https://git.example.com/r.git", "team"}, res.got)
	assert.Equal(t, "https://git.example.com/r.git", c.metaReq.Repo.URL, "the URL is filled when the resolver leaves it empty")
	assert.Equal(t, "u", c.metaReq.Repo.Username, "resolved credentials are forwarded")
	assert.Equal(t, "abc", c.metaReq.Revision)

	// Without resolver: URL and project only.
	c2 := &fakeRepoClient{meta: &apiclient.RevisionMetadata{}}
	_, err = NewRepoService(c2, nil).GetCommitMetadata(ctx, "https://git.example.com/r.git", "abc", "team")
	require.NoError(t, err)
	assert.Equal(t, apiclient.Repository{URL: "https://git.example.com/r.git", Project: "team"}, c2.metaReq.Repo)

	// Errors.
	_, err = svc.GetCommitMetadata(ctx, " ", "abc", "team")
	require.ErrorContains(t, err, "no source repository URL")
	_, err = NewRepoService(&fakeRepoClient{err: errors.New("unavailable")}, nil).GetCommitMetadata(ctx, "https://x", "abc", "p")
	require.ErrorContains(t, err, "unavailable")
	_, err = NewRepoService(&fakeRepoClient{}, nil).GetCommitMetadata(ctx, "https://x", "abc", "p")
	require.ErrorContains(t, err, "no revision metadata")
	_, err = NewRepoService(&fakeRepoClient{}, &fakeResolver{err: errors.New("denied")}).GetCommitMetadata(ctx, "https://x", "abc", "p")
	require.ErrorContains(t, err, "denied")
	assert.Nil(t, NewRepoService(nil, nil), "no client, no service")
}

func TestRepoServiceGetAppDetails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := &fakeRepoClient{details: &apiclient.AppDetails{
		Type: "Helm",
		Helm: &apiclient.HelmAppSpec{
			Name: "chart", ValueFiles: []string{"values.yaml"}, Values: "a: 1",
			Parameters:     []v1alpha1.HelmParameter{{Name: "image.tag", Value: "1"}},
			FileParameters: []v1alpha1.HelmFileParameter{{Name: "f", Path: "p"}},
		},
		Kustomize: &apiclient.KustomizeAppSpec{Images: []string{"nginx:1"}},
	}}
	app := map[string]any{
		"metadata": map[string]any{"name": "guestbook"},
		"spec": map[string]any{
			"sources": []any{
				map[string]any{"repoURL": "https://charts.example.com", "chart": "chart", "targetRevision": "1.0.0",
					"helm": map[string]any{"parameters": []any{map[string]any{"name": "replicas", "value": "2"}}}},
				map[string]any{"repoURL": "https://git.example.com/values.git"},
			},
		},
	}
	d, err := NewRepoService(c, nil).GetAppDetails(ctx, app)
	require.NoError(t, err)
	assert.Equal(t, "Helm", d.Type)
	require.NotNil(t, d.Helm)
	assert.Equal(t, "chart", d.Helm.HelmAppSpec.Name)
	assert.Equal(t, []notifications.HelmParameter{{Name: "image.tag", Value: "1"}}, d.Helm.HelmAppSpec.Parameters)
	assert.Equal(t, []notifications.HelmParameter{{Name: "replicas", Value: "2"}}, d.Helm.HelmParameterOverrides)
	assert.Equal(t, []string{"nginx:1"}, d.Kustomize.Images)
	assert.Equal(t, "https://charts.example.com", c.detailsReq.Repo.URL, "the first of spec.sources")
	assert.Equal(t, "default", c.detailsReq.Repo.Project, "an empty project is default")
	assert.Equal(t, "guestbook", c.detailsReq.AppName)
}

func TestFirstSource(t *testing.T) {
	t.Parallel()
	src := func(url string) map[string]any { return map[string]any{"repoURL": url} }
	tests := []struct {
		name    string
		spec    any
		want    string
		wantErr string
	}{
		{name: "single source", spec: map[string]any{"source": src("a")}, want: "a"},
		{name: "sources win over source", spec: map[string]any{"source": src("a"), "sources": []any{src("b"), src("c")}}, want: "b"},
		{name: "empty sources", spec: map[string]any{"source": src("a"), "sources": []any{}}, want: "a"},
		{name: "hydrator sync source", spec: map[string]any{
			"source":         src("a"),
			"sourceHydrator": map[string]any{"drySource": map[string]any{"repoURL": "dry", "targetRevision": "main", "path": "p"}, "syncSource": map[string]any{"targetBranch": "env/prod", "path": "out"}},
		}, want: "dry"},
		{name: "no source", spec: map[string]any{}, wantErr: "no source"},
		{name: "no spec", spec: nil, wantErr: "no spec"},
		{name: "malformed", spec: map[string]any{"source": "x"}, wantErr: "decoding"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := map[string]any{"metadata": map[string]any{"name": "x"}}
			if tc.spec != nil {
				app["spec"] = tc.spec
			}
			got, err := firstSource(app)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.RepoURL)
		})
	}
}

func TestProjectGetter(t *testing.T) {
	t.Parallel()
	c := fakeClient(t, &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "patrol", Annotations: map[string]string{"a": "b"}}})
	g := NewProjectGetter(c, "patrol")
	p, err := g.GetAppProject(context.Background(), "team")
	require.NoError(t, err)
	assert.Equal(t, "team", p["metadata"].(map[string]any)["name"])
	_, err = g.GetAppProject(context.Background(), "missing")
	require.ErrorContains(t, err, `cannot get application project "missing"`)
	// Projects are looked up only in the control-plane namespace.
	_, err = NewProjectGetter(c, "other").GetAppProject(context.Background(), "team")
	require.Error(t, err)
}
