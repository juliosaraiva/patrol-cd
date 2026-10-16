package commitserver

import (
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// APP-126, APP-129: the default commit message template (upstream data) with references and
// trailers removed from the body.
func TestRenderCommitMessage(t *testing.T) {
	t.Parallel()
	md := NewCommitMetadata(discard, "https://github.com/test/apps", "3ff41cc5247197a6caf50216c4c76cc29d78a97d", &apiclient.RevisionMetadata{
		Author: "test test@test.com",
		Date:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Message: "testn\n\nArgocd-reference-commit-repourl: https://github.com/test/apps\n" +
			"Argocd-reference-commit-author: not an address\n" +
			"Argocd-reference-commit-subject: testhydratormd\n" +
			"Argocd-reference-commit-sha: ABCDEF1\n" +
			"Signed-off-by: testUser <test@gmail.com>",
		References: []apiclient.RevisionReference{{Author: "testAuthor", Subject: "test", RepoURL: "https://github.com/test/apps", SHA: "3ff41cc5247197a6caf50216c4c76cc29d78a97c"}},
	})
	assert.Equal(t, "testn", md.Subject)
	assert.Equal(t, "Argocd-reference-commit-author: not an address\nArgocd-reference-commit-sha: ABCDEF1\nSigned-off-by: testUser <test@gmail.com>\n", md.Body,
		"valid trailers removed; invalid ones (bad address, uppercase SHA) kept")
	assert.Equal(t, "2026-01-02T03:04:05Z", md.Date)

	got, err := RenderCommitMessage(settings.DefaultCommitMessageTemplate, md)
	require.NoError(t, err)
	assert.Equal(t, `3ff41cc: testn

Argocd-reference-commit-author: not an address
Argocd-reference-commit-sha: ABCDEF1
Signed-off-by: testUser <test@gmail.com>


Co-authored-by: testAuthor
Co-authored-by: test test@test.com
`, got)

	got, err = RenderCommitMessage(`{{ .metadata.repoURL }} {{ (index .metadata.references 0).commit.sha | trunc 7 }}`, md)
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/test/apps 3ff41cc", got)

	got, err = RenderCommitMessage("", md)
	require.NoError(t, err)
	assert.Empty(t, got)

	_, err = RenderCommitMessage("{{ .metadata.subject", md)
	require.ErrorContains(t, err, "failed to parse commit message template")
	_, err = RenderCommitMessage(`{{ env "HOME" }}`, md)
	require.Error(t, err, "env is not available")
	_, err = RenderCommitMessage(`{{ fail "nope" }}`, md)
	require.ErrorContains(t, err, "failed to execute commit message template")
	_, err = RenderCommitMessage(`{{ repeat 2000000 "x" }}`, md)
	require.ErrorIs(t, err, errTemplateOutputTooLarge)
}

// APP-127: README rendering with the default template; references never reach the README
// data (upstream behavior).
func TestRenderReadme(t *testing.T) {
	t.Parallel()
	tmpl, err := ParseTemplate("readme", settings.DefaultReadmeMessageTemplate)
	require.NoError(t, err)
	out, err := renderReadme(tmpl, HydratorCommitMetadata{RepoURL: "https://git.example.com/dry.git", DrySHA: drySHA1, Commands: []string{"helm template ."}})
	require.NoError(t, err)
	assert.Contains(t, string(out), "git clone https://git.example.com/dry.git\n")
	assert.Contains(t, string(out), "git checkout "+drySHA1+"\nhelm template .\n```")
	assert.NotContains(t, string(out), "References")

	out, err = renderReadme(tmpl, HydratorCommitMetadata{RepoURL: "r", DrySHA: "s", References: []RevisionReference{{Commit: &CommitMetadata{SHA: "abcdef123", RepoURL: "u", Subject: "subj", Author: "a"}}}})
	require.NoError(t, err)
	assert.Contains(t, string(out), "* [abcdef1](u): subj (a)", "custom data with references renders the section")
}

func TestNewCommitMetadataNil(t *testing.T) {
	t.Parallel()
	assert.Equal(t, HydratorCommitMetadata{RepoURL: "r", DrySHA: "s"}, NewCommitMetadata(nil, "r", "s", nil))
	md := NewCommitMetadata(nil, "r", "s", &apiclient.RevisionMetadata{Message: "only subject", References: []apiclient.RevisionReference{{}}})
	assert.Equal(t, "only subject", md.Subject)
	assert.Empty(t, md.Body)
	assert.Empty(t, md.References, "empty references dropped")
	raw, err := json.Marshal(md)
	require.NoError(t, err)
	assert.JSONEq(t, `{"repoURL":"r","drySha":"s","subject":"only subject"}`, string(raw))
}

func TestEncodeManifests(t *testing.T) {
	t.Parallel()
	out, err := encodeManifests(nil)
	require.NoError(t, err)
	assert.Empty(t, out)
	out, err = encodeManifests([]json.RawMessage{json.RawMessage(`{"b":1,"a":{"z":[1,2.5,"3"],"y":null},"c":"multi\nline"}`)})
	require.NoError(t, err)
	assert.Equal(t, "a:\n  \"y\": null\n  z:\n    - 1\n    - 2.5\n    - \"3\"\nb: 1\nc: |-\n  multi\n  line\n", string(out))
	_, err = encodeManifests([]json.RawMessage{json.RawMessage(`null`)})
	require.Error(t, err)
	_, err = encodeManifests([]json.RawMessage{json.RawMessage(`{`)})
	require.Error(t, err)
}

// Property: cleanPath accepts only local, non-root paths outside .git, and is idempotent.
func TestCleanPathProperty(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		parts := rapid.SliceOfN(rapid.SampledFrom([]string{"a", "b", ".", "..", "", ".git", ".GIT", "x.yaml", "hydrator.metadata", " ", "-"}), 1, 6).Draw(t, "parts")
		p := strings.Join(parts, "/")
		if rapid.Bool().Draw(t, "abs") {
			p = "/" + p
		}
		c, err := cleanPath(p)
		if err != nil {
			return
		}
		if !filepath.IsLocal(c) || c == "." || strings.HasPrefix(c, "/") {
			t.Fatalf("cleanPath(%q) = %q is not a local non-root path", p, c)
		}
		for _, part := range strings.Split(c, "/") {
			if strings.EqualFold(part, ".git") || part == ".." {
				t.Fatalf("cleanPath(%q) = %q contains %q", p, c, part)
			}
		}
		again, err := cleanPath(c)
		if err != nil || again != c {
			t.Fatalf("cleanPath not idempotent: %q -> %q -> %q (%v)", p, c, again, err)
		}
	})
}

func TestStripCredentials(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://user:pw@host/r.git":  "https://user@host/r.git",
		"https://user@host/r.git":     "https://user@host/r.git",
		"https://host/r.git":          "https://host/r.git",
		"git@host:org/r.git":          "git@host:org/r.git",
		"ssh://git:pw@host/org/r.git": "ssh://git@host/org/r.git",
	} {
		assert.Equal(t, want, stripCredentials(in), in)
	}
}

func TestRequestLogValueRedacts(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	l := slog.New(slog.NewJSONHandler(&buf, nil))
	req := newRequest("https://bot:pw@git.example.com/r.git", "main", drySHA1, pathOf("a", configMap("a", "1")))
	req.Repo.Password = "super-secret"
	req.Repo.SSHPrivateKey = "-----BEGIN KEY-----"
	l.Info("x", slog.Any("request", req))
	assert.NotContains(t, buf.String(), "super-secret")
	assert.NotContains(t, buf.String(), "BEGIN KEY")
	assert.NotContains(t, buf.String(), ":pw@")
	assert.Contains(t, buf.String(), `"hasCredentials":true`)
}
