package generators

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

func gitGen(dirs []v1alpha1.GitDirectoryGeneratorItem, files []v1alpha1.GitFileGeneratorItem) *v1alpha1.ApplicationSetGenerator {
	return &v1alpha1.ApplicationSetGenerator{Git: &v1alpha1.GitGenerator{
		RepoURL: "https://git.example.com/org/repo.git", Revision: "HEAD", Directories: dirs, Files: files,
	}}
}

func TestGitDirectories(t *testing.T) {
	t.Parallel()
	repos := &fakeRepos{dirs: []string{"apps", "apps/guestbook", "apps/helm", "apps/helm/templates", "d/e", "d/f", "d/g", "other"}}
	g := &GitGenerator{cfg: Config{Client: fakeClient(t, appProject("default")), Namespace: controlNS, Repos: repos}}

	tests := []struct {
		name       string
		items      []v1alpha1.GitDirectoryGeneratorItem
		goTemplate bool
		prefix     string
		values     map[string]string
		want       []map[string]any
	}{
		{
			name:  "fasttemplate apps/*",
			items: []v1alpha1.GitDirectoryGeneratorItem{{Path: "apps/*"}},
			want: []map[string]any{
				{"path": "apps/guestbook", "path.basename": "guestbook", "path.basenameNormalized": "guestbook", "path[0]": "apps", "path[1]": "guestbook"},
				{"path": "apps/helm", "path.basename": "helm", "path.basenameNormalized": "helm", "path[0]": "apps", "path[1]": "helm"},
			},
		},
		{
			name:  "exclude wins independent of order",
			items: []v1alpha1.GitDirectoryGeneratorItem{{Path: "d/[fg]", Exclude: true}, {Path: "d/*"}},
			want:  []map[string]any{{"path": "d/e", "path.basename": "e", "path.basenameNormalized": "e", "path[0]": "d", "path[1]": "e"}},
		},
		{name: "only excludes", items: []v1alpha1.GitDirectoryGeneratorItem{{Path: "d/*", Exclude: true}}, want: []map[string]any{}},
		{name: "invalid pattern ignored", items: []v1alpha1.GitDirectoryGeneratorItem{{Path: "["}, {Path: "other"}},
			want: []map[string]any{{"path": "other", "path.basename": "other", "path.basenameNormalized": "other", "path[0]": "other"}}},
		{
			name:       "goTemplate with prefix and values",
			items:      []v1alpha1.GitDirectoryGeneratorItem{{Path: "d/e"}},
			goTemplate: true,
			prefix:     "app",
			values:     map[string]string{"ns": "{{ .app.path.basename }}-ns"},
			want: []map[string]any{{
				"app":    map[string]any{"path": map[string]any{"path": "d/e", "basename": "e", "basenameNormalized": "e", "segments": []string{"d", "e"}}},
				"values": map[string]string{"ns": "e-ns"},
			}},
		},
		{
			name:   "fasttemplate prefix",
			items:  []v1alpha1.GitDirectoryGeneratorItem{{Path: "other"}},
			prefix: "p",
			want:   []map[string]any{{"p.path": "other", "p.path.basename": "other", "p.path.basenameNormalized": "other", "p.path[0]": "other"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gen := gitGen(tc.items, nil)
			gen.Git.PathParamPrefix = tc.prefix
			gen.Git.Values = tc.values
			got, err := g.GenerateParams(context.Background(), gen, appSet(tc.goTemplate))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("directories win over files", func(t *testing.T) {
		gen := gitGen([]v1alpha1.GitDirectoryGeneratorItem{{Path: "other"}}, []v1alpha1.GitFileGeneratorItem{{Path: "*.json"}})
		got, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.NoError(t, err)
		assert.Len(t, got, 1)
	})
	t.Run("neither", func(t *testing.T) {
		_, err := g.GenerateParams(context.Background(), gitGen(nil, nil), appSet(false))
		require.ErrorIs(t, err, ErrEmptyAppSetGenerator)
	})
}

func TestGitFiles(t *testing.T) {
	t.Parallel()
	repos := &fakeRepos{files: map[string]map[string][]byte{
		"cluster-config/**/config.json": {
			"cluster-config/production/config.json": []byte(`{"cluster":{"name":"prod","address":"https://prod"},"list":["a","b"],"n":3,"empty":{}}`),
			"cluster-config/staging/config.json":    []byte(`{"cluster":{"name":"staging","address":"https://staging"}}`),
			"cluster-config/dev/config.json":        []byte(`{"cluster":{"name":"dev"}}`),
		},
		"cluster-config/*/dev/config.json": {"cluster-config/dev/config.json": nil},
		"list.yaml":                        {"envs/list.yaml": []byte("- name: a\n- name: b\n")},
		"empty.yaml":                       {"envs/empty.yaml": []byte("")},
		"bad.yaml":                         {"envs/bad.yaml": []byte("a: [")},
		"scalar.yaml":                      {"envs/scalar.yaml": []byte("just a string")},
	}}
	g := &GitGenerator{cfg: Config{Client: fakeClient(t, appProject("default")), Namespace: controlNS, Repos: repos}}

	t.Run("fasttemplate flatten with exclude", func(t *testing.T) {
		gen := gitGen(nil, []v1alpha1.GitFileGeneratorItem{{Path: "cluster-config/**/config.json"}, {Path: "cluster-config/*/dev/config.json", Exclude: true}})
		got, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, map[string]any{
			"cluster.name": "prod", "cluster.address": "https://prod", "list.0": "a", "list.1": "b", "n": "3", "empty": "map[]",
			"path": "cluster-config/production", "path.basename": "production", "path.filename": "config.json",
			"path.basenameNormalized": "production", "path.filenameNormalized": "config.json",
			"path[0]": "cluster-config", "path[1]": "production",
		}, got[0])
		assert.Equal(t, "staging", got[1]["cluster.name"], "files are processed in sorted path order")
	})

	t.Run("goTemplate nested", func(t *testing.T) {
		gen := gitGen(nil, []v1alpha1.GitFileGeneratorItem{{Path: "cluster-config/*/dev/config.json"}})
		gen.Git.Files[0].Path = "cluster-config/**/config.json"
		gen.Git.Values = map[string]string{"base_dir": "{{ index .path.segments 0 }}"}
		got, err := g.GenerateParams(context.Background(), gen, appSet(true))
		require.NoError(t, err)
		require.Len(t, got, 3)
		assert.Equal(t, map[string]any{"name": "dev"}, got[0]["cluster"])
		assert.Equal(t, map[string]any{
			"path": "cluster-config/dev", "basename": "dev", "filename": "config.json",
			"basenameNormalized": "dev", "filenameNormalized": "config.json", "segments": []string{"cluster-config", "dev"},
		}, got[0]["path"])
		assert.Equal(t, map[string]string{"base_dir": "cluster-config"}, got[0]["values"])
	})

	t.Run("array file and empty file", func(t *testing.T) {
		gen := gitGen(nil, []v1alpha1.GitFileGeneratorItem{{Path: "list.yaml"}, {Path: "empty.yaml"}})
		got, err := g.GenerateParams(context.Background(), gen, appSet(false))
		require.NoError(t, err)
		require.Len(t, got, 3)
		assert.Equal(t, "envs", got[0]["path"])
		assert.NotContains(t, got[0], "name", "empty.yaml sorts first and has only path parameters")
		assert.Equal(t, "a", got[1]["name"])
		assert.Equal(t, "b", got[2]["name"])
	})

	t.Run("malformed file", func(t *testing.T) {
		for _, p := range []string{"bad.yaml", "scalar.yaml"} {
			_, err := g.GenerateParams(context.Background(), gitGen(nil, []v1alpha1.GitFileGeneratorItem{{Path: p}}), appSet(false))
			require.ErrorContains(t, err, "unable to process file 'envs/"+p+"' from repository 'https://git.example.com/org/repo.git': unable to parse file")
		}
	})
}

func TestGitProjectAndRefresh(t *testing.T) {
	t.Parallel()
	signed := appProject("signed")
	signed.Spec.SourceIntegrity = &v1alpha1.SourceIntegrity{Git: &v1alpha1.SourceIntegrityGit{Policies: []v1alpha1.SourceIntegrityGitPolicy{{
		Repos: []v1alpha1.SourceIntegrityGitPolicyRepo{{URL: "*"}},
		GPG:   &v1alpha1.SourceIntegrityGitPolicyGPG{Mode: v1alpha1.SourceIntegrityGitPolicyGPGModeHead, Keys: []string{"ABC"}},
	}}}}
	repos := &fakeRepos{dirs: []string{"a"}}
	g := &GitGenerator{cfg: Config{Client: fakeClient(t, appProject("default"), signed), Namespace: controlNS, Repos: repos}}
	gen := gitGen([]v1alpha1.GitDirectoryGeneratorItem{{Path: "*"}}, nil)

	as := appSet(false)
	as.Spec.Template.Spec.Project = "signed"
	as.Annotations = map[string]string{v1alpha1.AnnotationKeyApplicationSetRefresh: "true"}
	_, err := g.GenerateParams(context.Background(), gen, as)
	require.NoError(t, err)
	assert.Equal(t, RepoRequest{RepoURL: gen.Git.RepoURL, Revision: "HEAD", Project: "signed", NoRevisionCache: true, VerifyCommit: true}, repos.requests[0])

	as.Spec.Template.Spec.Project = "{{project}}"
	as.Annotations = nil
	_, err = g.GenerateParams(context.Background(), gen, as)
	require.NoError(t, err)
	assert.Equal(t, RepoRequest{RepoURL: gen.Git.RepoURL, Revision: "HEAD"}, repos.requests[1], "templated project: no verification, global credentials")

	as.Spec.Template.Spec.Project = "missing"
	_, err = g.GenerateParams(context.Background(), gen, as)
	require.ErrorContains(t, err, "error getting project missing")

	repos.err = errors.New("boom")
	as.Spec.Template.Spec.Project = "default"
	_, err = g.GenerateParams(context.Background(), gen, as)
	require.ErrorContains(t, err, "error generating params from git: error getting directories from repo: boom")
}

func TestGitRequeue(t *testing.T) {
	t.Parallel()
	g := &GitGenerator{cfg: Config{DefaultRequeueAfter: 0}}
	gen := gitGen(nil, nil)
	assert.Equal(t, DefaultRequeueAfter, g.RequeueAfter(gen))
	g.cfg.DefaultRequeueAfter = 60_000_000_000
	assert.Equal(t, g.cfg.DefaultRequeueAfter, g.RequeueAfter(gen))
	gen.Git.RequeueAfterSeconds = ptr.To[int64](10)
	assert.EqualValues(t, 10_000_000_000, g.RequeueAfter(gen))
	gen.Git.RequeueAfterSeconds = ptr.To[int64](0)
	assert.Equal(t, NoRequeueAfter, g.RequeueAfter(gen))
}

// fakeRepoServer implements the repo-server client methods used by RepoServerService.
type fakeRepoServer struct {
	apiclient.Client
	files *apiclient.GitFilesRequest
	dirs  *apiclient.GitDirectoriesRequest
}

func (f *fakeRepoServer) GetGitFiles(_ context.Context, req *apiclient.GitFilesRequest) (*apiclient.GitFilesResponse, error) {
	f.files = req
	return &apiclient.GitFilesResponse{Map: map[string][]byte{"a.json": []byte("{}")}}, nil
}

func (f *fakeRepoServer) GetGitDirectories(_ context.Context, req *apiclient.GitDirectoriesRequest) (*apiclient.GitDirectoriesResponse, error) {
	f.dirs = req
	return &apiclient.GitDirectoriesResponse{Paths: []string{"x"}}, nil
}

func TestRepoServerService(t *testing.T) {
	t.Parallel()
	rs := &fakeRepoServer{}
	svc := &RepoServerService{
		Client:             rs,
		SubmodulesEnabled:  true,
		NewGitFileGlobbing: true,
		Repositories: RepositoryResolverFunc(func(_ context.Context, url, project string) (apiclient.Repository, error) {
			return apiclient.Repository{URL: url, Project: project, Password: "secret"}, nil
		}),
	}
	req := RepoRequest{RepoURL: "https://r", Revision: "main", Project: "p", NoRevisionCache: true, VerifyCommit: true}
	files, err := svc.GetFiles(context.Background(), req, "*.json")
	require.NoError(t, err)
	assert.Contains(t, files, "a.json")
	assert.Equal(t, &apiclient.GitFilesRequest{
		Repo: apiclient.Repository{URL: "https://r", Project: "p", Password: "secret"}, SubmoduleEnabled: true, Revision: "main",
		Path: "*.json", NewGitFileGlobbingEnabled: true, NoRevisionCache: true, VerifyCommit: true,
	}, rs.files)
	dirs, err := svc.GetDirectories(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, []string{"x"}, dirs)
	assert.True(t, rs.dirs.SubmoduleEnabled)

	_, err = (&RepoServerService{}).GetDirectories(context.Background(), req)
	require.ErrorContains(t, err, "no repo-server client configured")
}

// TestGitProjectScopeTenantBoundary covers the credential scope of the Git generator: the
// project of the entry's effective template scopes credentials and integrity, and it must permit
// the ApplicationSet's namespace.
func TestGitProjectScopeTenantBoundary(t *testing.T) {
	t.Parallel()
	// team-b does not trust team-a; team-a-proj does.
	victim := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "team-b", Namespace: controlNS},
		Spec: v1alpha1.AppProjectSpec{SourceNamespaces: []string{"team-b"}}}
	own := appProject("team-a-proj")
	signed := appProject("signed")
	signed.Spec.SourceIntegrity = &v1alpha1.SourceIntegrity{Git: &v1alpha1.SourceIntegrityGit{Policies: []v1alpha1.SourceIntegrityGitPolicy{{
		Repos: []v1alpha1.SourceIntegrityGitPolicyRepo{{URL: "*"}},
		GPG:   &v1alpha1.SourceIntegrityGitPolicyGPG{Mode: v1alpha1.SourceIntegrityGitPolicyGPGModeHead, Keys: []string{"ABC"}},
	}}}}
	kube := fakeClient(t, appProject("default"), victim, own, signed)
	newSet := func() (Set, *fakeRepos) {
		repos := &fakeRepos{dirs: []string{"secret-dir-of-team-b"}}
		return New(Config{Client: kube, Namespace: controlNS, Repos: repos}), repos
	}
	gitEntry := func(project string) v1alpha1.ApplicationSetGenerator {
		e := *gitGen([]v1alpha1.GitDirectoryGeneratorItem{{Path: "*"}}, nil)
		e.Git.Template.Spec.Project = project
		return e
	}
	tenantSet := func(top string) *v1alpha1.ApplicationSet {
		as := appSet(false)
		as.Spec.Template.Spec.Project = top
		return as
	}

	t.Run("top-level project of another tenant with an own generator-level project", func(t *testing.T) {
		t.Parallel()
		set, repos := newSet()
		_, err := generate(t, set, gitEntry("team-a-proj"), tenantSet("team-b"))
		require.NoError(t, err)
		require.Len(t, repos.requests, 1)
		assert.Equal(t, "team-a-proj", repos.requests[0].Project, "the project the Applications land in scopes credentials, not the top-level one")
	})

	t.Run("project that does not permit the ApplicationSet namespace", func(t *testing.T) {
		t.Parallel()
		set, repos := newSet()
		_, err := generate(t, set, gitEntry(""), tenantSet("team-b"))
		var notPermitted *ProjectNotPermittedError
		require.ErrorAs(t, err, &notPermitted)
		assert.EqualError(t, notPermitted, "ApplicationSet namespace 'team-a' is not permitted to use project 'team-b'")
		assert.Empty(t, repos.requests, "no repository is read with the credentials of a project that does not trust the namespace")

		_, err = generate(t, set, gitEntry("team-b"), tenantSet("team-a-proj"))
		require.ErrorAs(t, err, &notPermitted, "a generator-level project is checked too")
		assert.Empty(t, repos.requests)
	})

	t.Run("matrix template project scopes a nested git child", func(t *testing.T) {
		t.Parallel()
		set, repos := newSet()
		child := v1alpha1.ApplicationSetNestedGenerator{Git: gitGen([]v1alpha1.GitDirectoryGeneratorItem{{Path: "*"}}, nil).Git}
		// A child template is never applied, so it cannot choose the scope either.
		child.Git.Template.Spec.Project = "team-a-proj"
		entry := v1alpha1.ApplicationSetGenerator{Matrix: &v1alpha1.MatrixGenerator{
			Generators: []v1alpha1.ApplicationSetNestedGenerator{listChild(t, map[string]any{"a": "1"}), child},
		}}
		entry.Matrix.Template.Spec.Project = "team-b"
		_, err := generate(t, set, entry, tenantSet("team-a-proj"))
		var notPermitted *ProjectNotPermittedError
		require.ErrorAs(t, err, &notPermitted)
		assert.Equal(t, "team-b", notPermitted.Project)
		assert.Empty(t, repos.requests)

		entry.Matrix.Template.Spec.Project = ""
		_, err = generate(t, set, entry, tenantSet("team-a-proj"))
		require.NoError(t, err)
		require.Len(t, repos.requests, 1)
		assert.Equal(t, "team-a-proj", repos.requests[0].Project)
	})

	t.Run("generator-level project decides source integrity", func(t *testing.T) {
		t.Parallel()
		set, repos := newSet()
		_, err := generate(t, set, gitEntry("signed"), tenantSet("default"))
		require.NoError(t, err)
		require.Len(t, repos.requests, 1)
		assert.True(t, repos.requests[0].VerifyCommit, "a lax top-level project cannot skip the verification the generated Applications' project requires")
		assert.Equal(t, "signed", repos.requests[0].Project)
	})

	t.Run("control-plane ApplicationSets may use any project", func(t *testing.T) {
		t.Parallel()
		set, repos := newSet()
		as := tenantSet("team-b")
		as.Namespace = controlNS
		_, err := generate(t, set, gitEntry(""), as)
		require.NoError(t, err)
		require.Len(t, repos.requests, 1)
		assert.Equal(t, "team-b", repos.requests[0].Project)
	})
}
