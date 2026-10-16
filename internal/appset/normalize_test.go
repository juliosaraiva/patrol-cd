package appset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func TestNormalizeApplicationSpec(t *testing.T) {
	t.Parallel()
	in := &v1alpha1.ApplicationSpec{
		SyncPolicy: &v1alpha1.SyncPolicy{},
		Sources: v1alpha1.ApplicationSources{
			{RepoURL: "a", Helm: &v1alpha1.ApplicationSourceHelm{}, Kustomize: &v1alpha1.ApplicationSourceKustomize{}},
			{RepoURL: "b", Directory: &v1alpha1.ApplicationSourceDirectory{Include: "*.yaml"}},
			{RepoURL: "c", Directory: &v1alpha1.ApplicationSourceDirectory{}},
			{RepoURL: "d", Directory: &v1alpha1.ApplicationSourceDirectory{Recurse: true}, Helm: &v1alpha1.ApplicationSourceHelm{ReleaseName: "r"}},
		},
	}
	out := NormalizeApplicationSpec(in)
	assert.Equal(t, "default", out.Project)
	assert.Nil(t, out.SyncPolicy)
	assert.Nil(t, out.Sources[0].Helm)
	assert.Nil(t, out.Sources[0].Kustomize)
	assert.Equal(t, &v1alpha1.ApplicationSourceDirectory{Include: "*.yaml"}, out.Sources[1].Directory)
	assert.Nil(t, out.Sources[2].Directory)
	assert.True(t, out.Sources[3].Directory.Recurse)
	assert.Equal(t, "r", out.Sources[3].Helm.ReleaseName)
	assert.NotNil(t, in.SyncPolicy, "the input is not modified")

	single := NormalizeApplicationSpec(&v1alpha1.ApplicationSpec{Project: "p", Source: &v1alpha1.ApplicationSource{Helm: &v1alpha1.ApplicationSourceHelm{}},
		SyncPolicy: &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{}}})
	assert.Equal(t, "p", single.Project)
	assert.Nil(t, single.Source.Helm)
	assert.NotNil(t, single.SyncPolicy)
}

func TestIgnoreDiffer(t *testing.T) {
	t.Parallel()
	_, err := newIgnoreDiffer(v1alpha1.ApplicationSetIgnoreDifferences{{JQPathExpressions: []string{"{{"}}}, nil)
	require.ErrorContains(t, err, "failed to build ignore diff config")

	none, err := newIgnoreDiffer(nil, nil)
	require.NoError(t, err)
	assert.Nil(t, none)
	live := &v1alpha1.Application{}
	require.NoError(t, none.apply(context.Background(), live, live))

	d, err := newIgnoreDiffer(v1alpha1.ApplicationSetIgnoreDifferences{
		{JSONPointers: []string{"/spec/syncPolicy"}},
		{Name: "only-b", JQPathExpressions: []string{".spec.sources[] | select(.repoURL == \"x\").targetRevision"}},
	}, nil)
	require.NoError(t, err)

	mk := func(name, rev string, automated bool) *v1alpha1.Application {
		app := &v1alpha1.Application{}
		app.Name, app.Namespace = name, "ns"
		app.Spec.Project = "default"
		app.Spec.Sources = v1alpha1.ApplicationSources{{RepoURL: "x", Path: "p", TargetRevision: rev}, {RepoURL: "y", Path: "q", TargetRevision: "main"}}
		if automated {
			app.Spec.SyncPolicy = &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{Prune: ptr.To(true)}}
		}
		return app
	}
	// The sync policy is ignored for every Application.
	eq, err := specsEquivalent(context.Background(), d, mk("a", "main", false), mk("a", "main", true))
	require.NoError(t, err)
	assert.True(t, eq)
	// The jq rule applies only to the named Application.
	eq, err = specsEquivalent(context.Background(), d, mk("only-b", "v1", false), mk("only-b", "v2", false))
	require.NoError(t, err)
	assert.True(t, eq)
	eq, err = specsEquivalent(context.Background(), d, mk("a", "v1", false), mk("a", "v2", false))
	require.NoError(t, err)
	assert.False(t, eq)

	// Rules cannot change the desired name or namespace.
	desired := mk("a", "v1", true)
	liveApp := mk("a", "v1", false)
	require.NoError(t, d.apply(context.Background(), liveApp, desired))
	assert.Equal(t, "a", desired.Name)
	assert.Equal(t, "ns", desired.Namespace)
	assert.Nil(t, desired.Spec.SyncPolicy)
}
