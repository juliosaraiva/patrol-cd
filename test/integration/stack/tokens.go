package stack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	krand "k8s.io/apimachinery/pkg/util/rand"

	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// AccountToken creates an API token of a local account (as admin) and returns it.
func (s *Stack) AccountToken(t testing.TB, account string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), Timeout(30*time.Second))
	defer cancel()
	tok, err := s.API(t).CreateAccountToken(ctx, &apiclient.CreateAccountTokenRequest{Name: account})
	if err != nil {
		t.Fatalf("create an API token for %s: %v", account, err)
	}
	return tok.Token
}

// ProjectToken creates a token of a project role (as admin) and returns it.
func (s *Stack) ProjectToken(t testing.TB, project, role string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), Timeout(30*time.Second))
	defer cancel()
	tok, err := s.API(t).CreateProjectToken(ctx, &apiclient.ProjectTokenCreateRequest{Project: project, Role: role})
	if err != nil {
		t.Fatalf("create a token for project %s role %s: %v", project, role, err)
	}
	return tok.Token
}

// InvalidateClusterCache asks patrol-server to invalidate the controller's live-state
// cache of a destination (POST /api/v1/clusters/{server}/invalidate-cache).
func (s *Stack) InvalidateClusterCache(t testing.TB, d *Cluster) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), Timeout(30*time.Second))
	defer cancel()
	if _, err := s.API(t).InvalidateClusterCache(ctx, apiclient.ClusterByServer(d.Server)); err != nil {
		t.Fatalf("invalidate the cache of %s: %v", d.Name, err)
	}
}

// DropCaches drops every cache of the installation: the repo-server restarts with an empty
// root directory and the controller's live-state cache of every destination is
// invalidated through the API.
func (s *Stack) DropCaches(t testing.TB) {
	t.Helper()
	s.DropRepoServerCache(t)
	for _, d := range s.dests {
		s.InvalidateClusterCache(t, d)
	}
}

// GitHubPushBody is the body of a GitHub push event for a branch of repo. modified lists
// changed paths (manifest-generate-paths filtering).
func GitHubPushBody(t testing.TB, repo *Repo, branch, sha string, modified ...string) []byte {
	t.Helper()
	webURL := strings.TrimSuffix(repo.URL, ".git")
	body, err := json.Marshal(map[string]any{
		"ref": "refs/heads/" + branch, "before": strings.Repeat("0", 40), "after": sha,
		"commits": []any{map[string]any{"id": sha, "added": []string{}, "removed": []string{}, "modified": modified}},
		"repository": map[string]any{
			"name": repo.Name, "full_name": "patrol/" + repo.Name, "html_url": webURL, "clone_url": repo.URL,
			"url": webURL, "default_branch": "main", "owner": map[string]any{"login": "patrol", "name": "patrol"},
		},
		"sender": map[string]any{"login": "patrol"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// GitHubSignature is the X-Hub-Signature-256 value of body with secret.
func GitHubSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// PostWebhook posts body to patrol-server's /api/webhook (path "" or "/<provider>") with
// headers and returns the HTTP status.
func (s *Stack) PostWebhook(t testing.TB, path string, body []byte, headers map[string]string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.ServerURL()+"/api/webhook"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		t.Fatalf("deliver the webhook: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// GitHubPush delivers a signed GitHub push event (X-Hub-Signature-256 with the Stack's
// webhook.github.secret) for a branch of repo to patrol-server's /api/webhook and returns
// the HTTP status. modified lists changed paths (manifest-generate-paths filtering).
func (s *Stack) GitHubPush(t testing.TB, repo *Repo, branch, sha string, modified ...string) int {
	t.Helper()
	body := GitHubPushBody(t, repo, branch, sha, modified...)
	return s.PostWebhook(t, "", body, map[string]string{
		"X-GitHub-Event": "push", "X-GitHub-Delivery": krand.String(16), "X-Hub-Signature-256": GitHubSignature(s.WebhookSecret, body),
	})
}
