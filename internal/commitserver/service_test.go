package commitserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// APP-120, APP-119, APP-128: a missing sync branch is bootstrapped as an orphan branch with
// an empty initial commit, then the hydrated commit lands on top of it.
func TestCommitBootstrapsOrphanSyncBranch(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.seed("main", map[string]string{"README": "dry repo content"})
	svc := newTestService(t, ServiceOptions{})

	req := newRequest(r.url(), "env/prod", drySHA1, pathOf("apps/a", configMap("a", "1")))
	resp, err := svc.CommitHydratedManifests(testCtx(t), req)
	require.NoError(t, err)

	assert.Equal(t, r.head("env/prod"), resp.HydratedSHA)
	assert.Equal(t, 2, r.commitCount("env/prod"))
	assert.Equal(t, "Initial commit for env/prod", r.git("log", "-1", "--format=%s", "env/prod~1"))
	assert.Empty(t, r.git("ls-tree", "--name-only", "env/prod~1"), "orphan initial commit is empty")
	assert.Equal(t, "Patrol CD <patrol-cd@example.com>", r.git("log", "-1", "--format=%an <%ae>", "env/prod"))
	assert.Equal(t, "Patrol CD <patrol-cd@example.com>", r.git("log", "-1", "--format=%cn <%ce>", "env/prod"))
	assert.Equal(t, "hydrate 1111111", r.git("log", "-1", "--format=%B", "env/prod"))
	assert.False(t, r.hasFile("env/prod", "README"), "nothing from main leaks into the orphan branch")
	for _, f := range []string{"apps/a/manifest.yaml", "apps/a/hydrator.metadata", "apps/a/README.md", "hydrator.metadata", ".gitattributes"} {
		assert.True(t, r.hasFile("env/prod", f), f)
	}
	assert.Equal(t, commitNote{DrySHA: drySHA1, ManifestsDigest: noteDigest(t, req)}, r.note(resp.HydratedSHA))
	assert.Equal(t, "100644", strings.Fields(r.git("ls-tree", "env/prod", "apps/a/manifest.yaml"))[0])
}

func noteDigest(t *testing.T, req *CommitHydratedManifestsRequest) string {
	t.Helper()
	p, err := prepare(req)
	require.NoError(t, err)
	return p.digest
}

// APP-120, APP-123: a missing hydrateTo branch is created from the sync branch; the sync
// branch is not modified.
func TestCommitCreatesTargetBranchFromSyncBranch(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	svc := newTestService(t, ServiceOptions{})
	ctx := testCtx(t)

	first, err := svc.CommitHydratedManifests(ctx, newRequest(r.url(), "env/prod", drySHA1, pathOf("apps/a", configMap("a", "1"))))
	require.NoError(t, err)

	staging := newRequest(r.url(), "env/prod", drySHA2, pathOf("apps/a", configMap("a", "2")))
	staging.TargetBranch = "env/prod-next"
	resp, err := svc.CommitHydratedManifests(ctx, staging)
	require.NoError(t, err)
	assert.Equal(t, r.head("env/prod-next"), resp.HydratedSHA)
	assert.Equal(t, first.HydratedSHA, r.git("rev-parse", "env/prod-next~1"), "staging branch starts at the sync branch tip")
	assert.Equal(t, first.HydratedSHA, r.head("env/prod"), "sync branch untouched")
	assert.Contains(t, r.file("env/prod-next", "apps/a/manifest.yaml"), "key: \"2\"")
	assert.Contains(t, r.file("env/prod", "apps/a/manifest.yaml"), "key: \"1\"")

	// Patrol deviation: a new target branch is pushed even when the manifests equal the sync
	// branch's (upstream leaves the branch uncreated in that case).
	same := newRequest(r.url(), "env/prod", drySHA3, pathOf("apps/a", configMap("a", "1")))
	same.TargetBranch = "env/other"
	resp, err = svc.CommitHydratedManifests(ctx, same)
	require.NoError(t, err)
	assert.Equal(t, first.HydratedSHA, resp.HydratedSHA)
	assert.Equal(t, first.HydratedSHA, r.head("env/other"))
}

// APP-121, APP-122: re-hydrating the same dry SHA is a no-op; a new dry SHA with unchanged
// manifests creates no commit but records a note; changed manifests for the same dry SHA
// (spec change) are written.
func TestCommitIdempotence(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	svc := newTestService(t, ServiceOptions{})
	ctx := testCtx(t)

	req := newRequest(r.url(), "main", drySHA1, pathOf("apps/a", configMap("a", "1")), pathOf("apps/b", configMap("b", "1")))
	first, err := svc.CommitHydratedManifests(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, 2, r.commitCount("main"))

	again, err := svc.CommitHydratedManifests(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, first.HydratedSHA, again.HydratedSHA)
	assert.Equal(t, 2, r.commitCount("main"), "same dry SHA: no new commit")

	noop := newRequest(r.url(), "main", drySHA2, pathOf("apps/a", configMap("a", "1")), pathOf("apps/b", configMap("b", "1")))
	noop.DryCommitMetadata = &apiclient.RevisionMetadata{Message: "docs only"}
	resp, err := svc.CommitHydratedManifests(ctx, noop)
	require.NoError(t, err)
	assert.Equal(t, first.HydratedSHA, resp.HydratedSHA, "unchanged manifests: previous hydrated SHA returned")
	assert.Equal(t, 2, r.commitCount("main"), "no empty commit")
	assert.Equal(t, drySHA2, r.note(first.HydratedSHA).DrySHA, "note now maps the new dry SHA")
	assert.NotContains(t, r.file("main", "hydrator.metadata"), "docs only", "root metadata not committed without manifest changes")

	changed := newRequest(r.url(), "main", drySHA2, pathOf("apps/a", configMap("a", "1")), pathOf("apps/b", configMap("b", "2")))
	resp, err = svc.CommitHydratedManifests(ctx, changed)
	require.NoError(t, err)
	assert.NotEqual(t, first.HydratedSHA, resp.HydratedSHA, "same dry SHA but different manifests is written")
	assert.Equal(t, 3, r.commitCount("main"))
	assert.Equal(t, []string{"apps/b/README.md", "apps/b/hydrator.metadata", "apps/b/manifest.yaml", "hydrator.metadata"},
		strings.Fields(r.git("diff", "--name-only", first.HydratedSHA, resp.HydratedSHA)), "unchanged path a untouched")

	// A malformed note is ignored and replaced.
	r.git("notes", "--ref="+NoteNamespace, "add", "-f", "-m", "{not json", resp.HydratedSHA)
	resp2, err := svc.CommitHydratedManifests(ctx, changed)
	require.NoError(t, err)
	assert.Equal(t, resp.HydratedSHA, resp2.HydratedSHA)
	assert.Equal(t, drySHA2, r.note(resp.HydratedSHA).DrySHA)
}

// APP-119: only the files the hydrator owns are replaced; everything else in the path and
// paths whose manifests did not change are left alone.
func TestCommitPathCleaning(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	ctx := testCtx(t)
	svc := newTestService(t, ServiceOptions{})
	unchanged, err := encodeManifests([]json.RawMessage{configMap("same", "x")})
	require.NoError(t, err)
	r.seed("main", map[string]string{
		"apps/a/manifest.yaml":     "old: manifest\n",
		"apps/a/README.md":         "old readme\n",
		"apps/a/extra.txt":         "keep me\n",
		"apps/a/hydrator.metadata": "{}\n",
		"apps/same/manifest.yaml":  string(unchanged),
		"apps/same/README.md":      "custom readme\n",
		"apps/link/manifest.yaml":  "symlink:../../victim.txt",
		"victim.txt":               "victim\n",
		"other/file.txt":           "untouched\n",
	})

	resp, err := svc.CommitHydratedManifests(ctx, newRequest(r.url(), "main", drySHA1,
		pathOf("apps/a", configMap("a", "new")),
		pathOf("apps/same", configMap("same", "x")),
		pathOf("apps/link", configMap("link", "y")),
	))
	require.NoError(t, err)
	assert.Equal(t, r.head("main"), resp.HydratedSHA)
	assert.Equal(t, "keep me", r.file("main", "apps/a/extra.txt"))
	assert.Contains(t, r.file("main", "apps/a/manifest.yaml"), "key: new")
	assert.NotEqual(t, "old readme", r.file("main", "apps/a/README.md"))
	assert.Equal(t, "custom readme", r.file("main", "apps/same/README.md"), "unchanged path keeps its files")
	assert.False(t, r.hasFile("main", "apps/same/hydrator.metadata"))
	assert.Equal(t, "victim", r.file("main", "victim.txt"), "symlinked manifest.yaml replaced, target untouched")
	assert.Equal(t, "100644", strings.Fields(r.git("ls-tree", "main", "apps/link/manifest.yaml"))[0], "now a regular file")
	assert.Equal(t, "untouched", r.file("main", "other/file.txt"))
}

// Untrusted repository content: a symbolic link in a path component is refused (inside or
// outside the repository), and nothing is pushed.
func TestCommitRefusesSymlinkedPathComponents(t *testing.T) {
	t.Parallel()
	for name, target := range map[string]string{"inside": "../real", "outside": "../../../.."} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRemote(t)
			head := r.seed("main", map[string]string{"real/keep": "x", "apps/link": "symlink:" + target})
			svc := newTestService(t, ServiceOptions{})
			_, err := svc.CommitHydratedManifests(testCtx(t), newRequest(r.url(), "main", drySHA1, pathOf("apps/link/a", configMap("a", "1"))))
			require.ErrorContains(t, err, "is a symbolic link")
			assert.Equal(t, head, r.head("main"))
		})
	}
}

// Two paths of one request where one lies inside a file the other owns are rejected before
// any git work: writing both would erase one application's output (regression: the second
// path used to delete the first one's directory and the request succeeded).
func TestCommitRejectsPathsOverlappingOwnedFiles(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	head := r.seed("main", map[string]string{"keep.txt": "x"})
	svc := newTestService(t, ServiceOptions{})
	_, err := svc.CommitHydratedManifests(testCtx(t), newRequest(r.url(), "main", drySHA1,
		pathOf("x/manifest.yaml", configMap("a", "1")),
		pathOf("x", configMap("b", "1")),
	))
	require.ErrorIs(t, err, ErrInvalidRequest)
	require.ErrorContains(t, err, `path "x/manifest.yaml" overlaps the file manifest.yaml written for path "x"`)
	assert.Equal(t, head, r.head("main"), "nothing pushed")
}

// An owned file name occupied by a directory (another request's output, or user content) is
// never deleted: the request fails and nothing is pushed (regression: replaceFile used to
// remove the whole directory tree).
func TestCommitRefusesDirectoryAtOwnedFile(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		seed map[string]string
		path string
		want string
	}{
		"manifest.yaml written by an earlier request": {
			seed: map[string]string{
				"x/manifest.yaml/manifest.yaml":     "kind: ConfigMap\n",
				"x/manifest.yaml/hydrator.metadata": "{}\n",
				"x/manifest.yaml/user.txt":          "keep\n",
			},
			path: "x",
			want: `refusing to replace "x/manifest.yaml": it is a directory`,
		},
		"README.md directory": {
			seed: map[string]string{"apps/a/README.md/user.txt": "keep\n"},
			path: "apps/a",
			want: `refusing to replace "apps/a/README.md": it is a directory`,
		},
		"root hydrator.metadata directory": {
			seed: map[string]string{"hydrator.metadata/user.txt": "keep\n"},
			path: "apps/a",
			want: `refusing to replace "hydrator.metadata": it is a directory`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRemote(t)
			head := r.seed("main", tc.seed)
			svc := newTestService(t, ServiceOptions{})
			_, err := svc.CommitHydratedManifests(testCtx(t), newRequest(r.url(), "main", drySHA1, pathOf(tc.path, configMap("a", "1"))))
			require.ErrorContains(t, err, tc.want)
			assert.NotErrorIs(t, err, ErrInvalidRequest)
			assert.Equal(t, head, r.head("main"), "nothing pushed")
			for f, content := range tc.seed {
				assert.Equal(t, strings.TrimSpace(content), r.file("main", f), "%s untouched", f)
			}
		})
	}
}

// Path validation happens before any git work.
func TestCommitRejectsInvalidRequests(t *testing.T) {
	t.Parallel()
	svc := newTestService(t, ServiceOptions{})
	url := "file:///nonexistent/repo.git"
	base := func() *CommitHydratedManifestsRequest {
		return newRequest(url, "main", drySHA1, pathOf("apps/a", configMap("a", "1")))
	}
	cases := map[string]func(*CommitHydratedManifestsRequest){
		"no repo URL":       func(r *CommitHydratedManifestsRequest) { r.Repo.URL = "" },
		"helm repository":   func(r *CommitHydratedManifestsRequest) { r.Repo.Type = "helm" },
		"no target branch":  func(r *CommitHydratedManifestsRequest) { r.TargetBranch = "" },
		"no sync branch":    func(r *CommitHydratedManifestsRequest) { r.SyncBranch = "" },
		"dry SHA not a SHA": func(r *CommitHydratedManifestsRequest) { r.DrySHA = "main" },
		"no paths":          func(r *CommitHydratedManifestsRequest) { r.Paths = nil },
		"duplicate paths": func(r *CommitHydratedManifestsRequest) {
			r.Paths = append(r.Paths, pathOf("apps/a/", configMap("b", "1")))
		},
		"path inside another path's manifest.yaml": func(r *CommitHydratedManifestsRequest) {
			r.Paths = append(r.Paths, pathOf("apps/a/manifest.yaml", configMap("b", "1")))
		},
		"path inside another path's README.md, listed first": func(r *CommitHydratedManifestsRequest) {
			r.Paths = append([]PathDetails{pathOf("apps/a/README.md/x", configMap("b", "1"))}, r.Paths...)
		},
		"path inside another path's hydrator.metadata, other case": func(r *CommitHydratedManifestsRequest) {
			r.Paths = append(r.Paths, pathOf("APPS/A/Hydrator.Metadata", configMap("b", "1")))
		},
		"manifest not an object": func(r *CommitHydratedManifestsRequest) {
			r.Paths[0].Manifests = []json.RawMessage{json.RawMessage(`[1,2]`)}
		},
		"invalid readme template": func(r *CommitHydratedManifestsRequest) { r.ReadmeTemplate = "{{ .Nope" },
		"author with brackets":    func(r *CommitHydratedManifestsRequest) { r.Author.Email = "<x@example.com>" },
		"author with newline":     func(r *CommitHydratedManifestsRequest) { r.Author.Name = "a\nb" },
		"commit message too big": func(r *CommitHydratedManifestsRequest) {
			r.CommitMessage = strings.Repeat("x", MaxCommitMessageBytes+1)
		},
	}
	for _, p := range []string{"", ".", "./", "/", "/abs", "..", "../x", "a/../../x", ".git", ".git/hooks", "a/.GIT/x",
		"hydrator.metadata", ".gitattributes/x", "a\x00b", "a\nb", `a\b`, strings.Repeat("a", MaxPathLength+1)} {
		cases[fmt.Sprintf("path %q", truncate(p, 20))] = func(r *CommitHydratedManifestsRequest) { r.Paths[0].Path = p }
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := base()
			mutate(req)
			_, err := svc.CommitHydratedManifests(testCtx(t), req)
			require.ErrorIs(t, err, ErrInvalidRequest)
		})
	}
	_, err := svc.CommitHydratedManifests(testCtx(t), nil)
	require.ErrorIs(t, err, ErrInvalidRequest)
}

// APP-119, APP-127, APP-129: file contents written by the commit-server.
func TestCommitFileContents(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	svc := newTestService(t, ServiceOptions{})
	date := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)
	refDate := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	req := newRequest(r.url(), "main", drySHA1, PathDetails{
		Path:      "apps/web",
		Manifests: []json.RawMessage{configMap("web", "<v>&"), json.RawMessage(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web"},"spec":{"replicas":2,"ratio":0.5,"template":{"spec":{"containers":[{"name":"c","args":["a","b"]}]}}}}`)},
		Commands:  []string{"helm template . --name-template web", "kustomize build ."},
	})
	req.DryRepoURL = "https://user:secret@git.example.com/org/dry.git"
	req.Repo.URL = r.url()
	req.ReadmeTemplate = settings.DefaultReadmeMessageTemplate
	req.DryCommitMetadata = &apiclient.RevisionMetadata{
		Author:  "Dry Author <dry@example.com>",
		Date:    date,
		Message: "Update web\n\nMore details <b>\nArgocd-reference-commit-sha: abc1234\nSigned-off-by: Dev <dev@example.com>",
		References: []apiclient.RevisionReference{{
			RepoURL: "https://git.example.com/org/app.git", SHA: "abc1234", Author: "Code Author <code@example.com>",
			Date: refDate, Subject: "Fix bug",
		}},
	}
	_, err := svc.CommitHydratedManifests(testCtx(t), req)
	require.NoError(t, err)

	assert.Equal(t, `apiVersion: v1
data:
  key: <v>&
kind: ConfigMap
metadata:
  name: web
  namespace: default
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  ratio: 0.5
  replicas: 2
  template:
    spec:
      containers:
        - args:
            - a
            - b
          name: c
`, r.rawFile("main", "apps/web/manifest.yaml"))

	assert.Equal(t, `{
  "repoURL": "https://user@git.example.com/org/dry.git",
  "drySha": "1111111111111111111111111111111111111111",
  "author": "Dry Author <dry@example.com>",
  "date": "2026-10-01T12:30:00Z",
  "subject": "Update web",
  "body": "More details <b>\nSigned-off-by: Dev <dev@example.com>\n",
  "references": [
    {
      "commit": {
        "author": "Code Author <code@example.com>",
        "date": "2026-09-30T08:00:00Z",
        "subject": "Fix bug",
        "sha": "abc1234",
        "repoUrl": "https://git.example.com/org/app.git"
      }
    }
  ]
}
`, r.rawFile("main", "hydrator.metadata"))

	assert.Equal(t, `{
  "repoURL": "https://user@git.example.com/org/dry.git",
  "drySha": "1111111111111111111111111111111111111111",
  "commands": [
    "helm template . --name-template web",
    "kustomize build ."
  ]
}
`, r.rawFile("main", "apps/web/hydrator.metadata"))

	assert.Equal(t, "# Manifest Hydration\n\n"+
		"To hydrate the manifests in this repository, run the following commands:\n\n"+
		"```shell\n"+
		"git clone https://user@git.example.com/org/dry.git\n"+
		"# cd into the cloned directory\n"+
		"git checkout 1111111111111111111111111111111111111111\n"+
		"helm template . --name-template web\n"+
		"kustomize build .\n"+
		"```\n", r.rawFile("main", "apps/web/README.md"))

	assert.Equal(t, gitAttributesContent, r.rawFile("main", ".gitattributes"))
	assert.NotContains(t, r.git("log", "-p", "--all"), "secret", "credentials never reach git")
}

// APP-128: commit author from settings, each field defaulted independently.
func TestCommitAuthor(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	svc := newTestService(t, ServiceOptions{})
	req := newRequest(r.url(), "main", drySHA1, pathOf("a", configMap("a", "1")))
	req.Author = Author{Name: "Hydrator Bot"}
	_, err := svc.CommitHydratedManifests(testCtx(t), req)
	require.NoError(t, err)
	assert.Equal(t, "Hydrator Bot <patrol-cd@example.com>", r.git("log", "-1", "--format=%an <%ae>", "main"))
	assert.Equal(t, "Hydrator Bot <patrol-cd@example.com>", r.git("log", "-1", "--format=%an <%ae>", "main~1"), "orphan commit too")
}

// A push rejected because another writer updated the branch is retried on a fresh clone and
// the request is re-applied on top of the other writer's commit.
func TestCommitRetriesRejectedPush(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.seed("main", map[string]string{"base.txt": "base"})
	svc := newTestService(t, ServiceOptions{})
	var attempts atomic.Int32
	svc.beforePush = func(_ context.Context, attempt int) {
		attempts.Store(int32(attempt))
		if attempt == 1 {
			r.seed("main", map[string]string{"other/manifest.yaml": "from another writer\n"})
		}
	}
	resp, err := svc.CommitHydratedManifests(testCtx(t), newRequest(r.url(), "main", drySHA1, pathOf("apps/a", configMap("a", "1"))))
	require.NoError(t, err)
	assert.EqualValues(t, 2, attempts.Load())
	assert.Equal(t, r.head("main"), resp.HydratedSHA)
	assert.Equal(t, "from another writer", r.file("main", "other/manifest.yaml"))
	assert.True(t, r.hasFile("main", "apps/a/manifest.yaml"))
	assert.Equal(t, "seed main", r.git("log", "-1", "--format=%s", "main~1"), "linear history on top of the other commit")

	// Exhausted retries surface ErrConcurrentUpdate.
	svc2 := newTestService(t, ServiceOptions{MaxAttempts: 2})
	n := 0
	svc2.beforePush = func(context.Context, int) {
		n++
		r.seed("main", map[string]string{"other/manifest.yaml": fmt.Sprintf("writer %d\n", n)})
	}
	_, err = svc2.CommitHydratedManifests(testCtx(t), newRequest(r.url(), "main", drySHA2, pathOf("apps/a", configMap("a", "2"))))
	require.ErrorIs(t, err, ErrConcurrentUpdate)
	assert.Equal(t, 2, n)
}

// Several commit-server replicas (separate services, no shared lock) writing different
// paths of one branch concurrently all succeed.
func TestCommitConcurrentWriters(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.seed("main", map[string]string{"base.txt": "base"})
	const writers = 4
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		svc := newTestService(t, ServiceOptions{MaxAttempts: 20})
		wg.Go(func() {
			sha := strings.Repeat(fmt.Sprintf("%x", i+1), 40)
			_, errs[i] = svc.CommitHydratedManifests(testCtx(t), newRequest(r.url(), "main", sha, pathOf(fmt.Sprintf("apps/%d", i), configMap("a", fmt.Sprint(i)))))
		})
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "writer %d", i)
		assert.True(t, r.hasFile("main", fmt.Sprintf("apps/%d/manifest.yaml", i)))
	}
	assert.Equal(t, 1+writers, r.commitCount("main"))
}

// Requests for one repository are serialized inside a service.
func TestCommitSerializesPerRepository(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	svc := newTestService(t, ServiceOptions{})
	var inPush, maxInPush atomic.Int32
	svc.beforePush = func(context.Context, int) {
		n := inPush.Add(1)
		for {
			m := maxInPush.Load()
			if n <= m || maxInPush.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inPush.Add(-1)
	}
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Go(func() {
			sha := strings.Repeat(fmt.Sprintf("%x", i+1), 40)
			_, err := svc.CommitHydratedManifests(testCtx(t), newRequest(r.url(), "main", sha, pathOf(fmt.Sprintf("p%d", i), configMap("a", "1"))))
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	assert.EqualValues(t, 1, maxInPush.Load())
}

// OPS-084: metrics for successful and failed requests.
func TestCommitMetrics(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	require.NoError(t, err)
	again, err := NewMetrics(reg)
	require.NoError(t, err, "re-registration reuses collectors")
	require.Same(t, m.commitRequests, again.commitRequests)
	svc := newTestService(t, ServiceOptions{Metrics: m})
	ctx := testCtx(t)

	_, err = svc.CommitHydratedManifests(ctx, newRequest(r.url(), "main", drySHA1, pathOf("a", configMap("a", "1"))))
	require.NoError(t, err)
	_, err = svc.CommitHydratedManifests(ctx, newRequest("file://"+r.dir+"/missing.git", "main", drySHA1, pathOf("a", configMap("a", "1"))))
	require.Error(t, err)

	label := r.url()
	assert.InDelta(t, 1, testutil.ToFloat64(m.commitRequests.WithLabelValues(label, ResponseTypeSuccess)), 0)
	assert.InDelta(t, 1, testutil.ToFloat64(m.commitRequests.WithLabelValues("file://"+r.dir+"/missing.git", ResponseTypeFailure)), 0)
	assert.GreaterOrEqual(t, testutil.ToFloat64(m.gitRequests.WithLabelValues(label, GitRequestTypePush)), 2.0, "branch and note pushes")
	assert.GreaterOrEqual(t, testutil.ToFloat64(m.gitRequests.WithLabelValues(label, GitRequestTypeFetch)), 1.0)
	assert.InDelta(t, 0, testutil.ToFloat64(m.pending.WithLabelValues(label)), 0)
	assert.Equal(t, 2, testutil.CollectAndCount(m.commitDuration, "patrol_commitserver_commit_request_duration_seconds"), "success and failure series")
}

func TestKeyedMutex(t *testing.T) {
	t.Parallel()
	var k keyedMutex
	unlock, err := k.lock(context.Background(), "a")
	require.NoError(t, err)
	other, err := k.lock(context.Background(), "b")
	require.NoError(t, err, "different keys do not block")
	other()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = k.lock(ctx, "a")
	require.ErrorIs(t, err, context.DeadlineExceeded)

	done := make(chan struct{})
	go func() {
		u, err := k.lock(context.Background(), "a")
		if assert.NoError(t, err) {
			u()
		}
		close(done)
	}()
	unlock()
	unlock() // idempotent
	<-done
	k.mu.Lock()
	assert.Empty(t, k.entries, "entries are released")
	k.mu.Unlock()
}

func TestIsConcurrentUpdate(t *testing.T) {
	t.Parallel()
	for msg, want := range map[string]bool{
		"! [rejected] main -> main (fetch first)":            true,
		"! [rejected] main -> main (non-fast-forward)":       true,
		"error: cannot lock ref 'refs/notes/x'":              true,
		"remote: error: failed to update ref":                true,
		"! [remote rejected] main -> main (hook declined)":   false,
		"fatal: Authentication failed for 'https://x/'":      false,
		"fatal: repository 'file:///missing' does not exist": false,
	} {
		assert.Equal(t, want, isConcurrentUpdate(errors.New(msg)), msg)
		assert.Equal(t, want, errors.Is(classify(errors.New(msg)), ErrConcurrentUpdate), msg)
	}
}
