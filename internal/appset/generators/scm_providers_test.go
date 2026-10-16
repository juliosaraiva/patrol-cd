package generators

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/awscodecommit"
)

// TestSCMProviderBlocks checks the provider construction of the blocks not covered elsewhere.
func TestSCMProviderBlocks(t *testing.T) {
	t.Parallel()
	objs := []any{secret(appSetNS, "tok", nil, map[string]string{"token": "t0k"})}
	ref := &v1alpha1.SecretRef{SecretName: "tok", Key: "token"}

	t.Run("aws codecommit", func(t *testing.T) {
		t.Parallel()
		f := &recordingFactory{provider: seededSCM()}
		g := scmGenerator(t, f, SCMConfig{Enabled: true, AllowedProviders: []string{"https://only.example"}}, objs...)
		// The fake provider rejects the organization-less CodeCommit query; only the
		// construction is checked here (CodeCommit itself is tested in pkg/scm/awscodecommit).
		_, err := g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{
			AWSCodeCommit: &v1alpha1.SCMProviderGeneratorAWSCodeCommit{
				Region: "eu-west-1", Role: "arn:aws:iam::1:role/x", AllBranches: true,
				TagFilters: []v1alpha1.TagFilter{{Key: "team"}, {Key: "env", Value: "prod"}},
			},
		}}, appSet(false))
		require.ErrorContains(t, err, "error listing repos", "built despite the allow-list: CodeCommit has no API URL")
		req := f.last(t)
		assert.Equal(t, awscodecommit.Type, req.Type)
		assert.Equal(t, awscodecommit.Options{Region: "eu-west-1", Role: "arn:aws:iam::1:role/x",
			TagFilters: []awscodecommit.TagFilter{{Key: "team"}, {Key: "env", Value: "prod"}}}, req.CodeCommit)
	})

	t.Run("bitbucket cloud", func(t *testing.T) {
		t.Parallel()
		f := &recordingFactory{provider: seededSCM()}
		g := scmGenerator(t, f, SCMConfig{Enabled: true, AllowedProviders: []string{"https://only.example"}}, objs...)
		_, err := g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{
			Bitbucket: &v1alpha1.SCMProviderGeneratorBitbucket{Owner: "org", User: "me", AppPasswordRef: ref},
		}}, appSet(false))
		require.NoError(t, err)
		req := f.last(t)
		assert.Equal(t, "bitbucketcloud", req.Type)
		assert.Equal(t, "me", req.BitbucketCloud.Username)
		tok, err := req.Config.Token(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "t0k", tok)
	})

	t.Run("gitea insecure", func(t *testing.T) {
		t.Parallel()
		f := &recordingFactory{provider: seededSCM()}
		g := scmGenerator(t, f, SCMConfig{Enabled: true}, objs...)
		_, err := g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{
			Gitea: &v1alpha1.SCMProviderGeneratorGitea{Owner: "org", API: "https://gitea.example", Insecure: true, TokenRef: ref},
		}}, appSet(false))
		require.NoError(t, err)
		req := f.last(t)
		assert.Equal(t, "gitea", req.Type)
		assert.True(t, req.Config.Insecure)
		assert.Equal(t, "https://gitea.example", req.Config.APIURL)
	})

	t.Run("root CA appended", func(t *testing.T) {
		t.Parallel()
		f := &recordingFactory{provider: seededSCM()}
		path := t.TempDir() + "/ca.pem"
		require.NoError(t, writeFile(path, testCAPEM))
		g := scmGenerator(t, f, SCMConfig{Enabled: true, RootCAPath: path}, objs...)
		_, err := g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{
			BitbucketServer: &v1alpha1.SCMProviderGeneratorBitbucketServer{Project: "org", API: "https://bbs.example"},
		}}, appSet(false))
		require.NoError(t, err)
		assert.Contains(t, string(f.last(t).Config.CAData), "BEGIN CERTIFICATE")

		// A missing or invalid root CA file is ignored (system pool).
		g = scmGenerator(t, f, SCMConfig{Enabled: true, RootCAPath: t.TempDir() + "/missing.pem"}, objs...)
		_, err = g.GenerateParams(context.Background(), &v1alpha1.ApplicationSetGenerator{SCMProvider: &v1alpha1.SCMProviderGenerator{
			Gitlab: &v1alpha1.SCMProviderGeneratorGitlab{Group: "org"},
		}}, appSet(false))
		require.NoError(t, err)
		assert.Empty(t, f.last(t).Config.CAData)
	})
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
