package services

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func testRSAKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func TestGitHub(t *testing.T) {
	t.Parallel()
	keyPEM := testRSAKeyPEM(t)
	srv, rec := newServer(t, func(w http.ResponseWriter, c captured) {
		switch {
		case c.Method == http.MethodPost && c.Path == "/api/v3/app/installations/42/access_tokens":
			writeJSON(w, 201, map[string]any{"token": "ghs_installation_token", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
		case c.Method == http.MethodPost && c.Path == "/api/v3/repos/example/guestbook/statuses/abc123":
			writeJSON(w, 201, map[string]any{"id": 1})
		case c.Method == http.MethodGet && c.Path == "/api/v3/repos/example/guestbook/deployments":
			writeJSON(w, 200, []any{})
		case c.Method == http.MethodPost && c.Path == "/api/v3/repos/example/guestbook/deployments":
			writeJSON(w, 201, map[string]any{"id": 77})
		case c.Method == http.MethodPost && c.Path == "/api/v3/repos/example/guestbook/deployments/77/statuses":
			writeJSON(w, 201, map[string]any{"id": 1})
		case c.Method == http.MethodGet && c.Path == "/api/v3/repos/example/guestbook/commits/abc123/pulls":
			writeJSON(w, 200, []any{map[string]any{"number": 5}, map[string]any{"number": 6}})
		case c.Method == http.MethodGet && c.Path == "/api/v3/repos/example/guestbook/issues/5/comments":
			writeJSON(w, 200, []any{map[string]any{"id": 900, "body": "old\n<!-- argocd-notifications cd/guestbook -->"}})
		case c.Method == http.MethodGet && c.Path == "/api/v3/repos/example/guestbook/issues/6/comments":
			writeJSON(w, 200, []any{map[string]any{"id": 901, "body": "unrelated"}})
		case c.Method == http.MethodPatch && c.Path == "/api/v3/repos/example/guestbook/issues/comments/900":
			writeJSON(w, 200, map[string]any{"id": 900})
		case c.Method == http.MethodPost && c.Path == "/api/v3/repos/example/guestbook/issues/6/comments":
			writeJSON(w, 201, map[string]any{"id": 902})
		case c.Method == http.MethodPost && c.Path == "/api/v3/repos/example/guestbook/check-runs":
			writeJSON(w, 201, map[string]any{"id": 3})
		default:
			writeJSON(w, 404, map[string]any{"message": "Not Found " + c.Method + " " + c.Path})
		}
	})
	svc := mustNew(t, TypeGitHub, map[string]any{"appID": 1234, "installationID": "42", "privateKey": keyPEM, "enterpriseBaseURL": srv.URL + "/api/v3"}, testDeps())
	n := Notification{
		Message: strings.Repeat("m", 200),
		GitHub: &GitHubNotification{
			RepoURLPath:  "git@github.com:example/guestbook.git",
			RevisionPath: "abc123",
			Status:       &GitHubStatus{State: "success", Label: "continuous-delivery/guestbook", TargetURL: "https://cd.example.com/applications/guestbook"},
			Deployment: &GitHubDeployment{State: "success", Environment: "production", EnvironmentURL: "https://guestbook.example.com",
				LogURL: "https://cd.example.com/log", RequiredContexts: []string{}, AutoMerge: new(false)},
			PullRequestComment: &GitHubPullRequestComment{Content: "deployed", CommentTag: "cd/guestbook"},
			CheckRun: &GitHubCheckRun{Name: "cd/guestbook", Status: "completed", Conclusion: "success", DetailsURL: "https://cd.example.com",
				Output: &GitHubCheckRunOutput{Title: "Deployed", Summary: "ok"}},
		},
	}
	mustSend(t, svc, n, Destination{Service: "github"})

	byKey := map[string]captured{}
	for _, r := range rec.all() {
		byKey[r.Method+" "+r.Path] = r
		if !strings.Contains(r.Path, "access_tokens") {
			if got := r.Header.Get("Authorization"); got != "token ghs_installation_token" {
				t.Fatalf("%s %s Authorization = %q", r.Method, r.Path, got)
			}
			if r.Header.Get("X-GitHub-Api-Version") == "" {
				t.Fatalf("missing API version header")
			}
		} else if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Fatalf("token exchange must use the app JWT, got %q", r.Header.Get("Authorization"))
		}
	}
	status := byKey["POST /api/v3/repos/example/guestbook/statuses/abc123"].json(t)
	if status["state"] != "success" || status["context"] != "continuous-delivery/guestbook" || status["target_url"] != "https://cd.example.com/applications/guestbook" ||
		len([]rune(status["description"].(string))) != 140 {
		t.Fatalf("status %v", status)
	}
	if q := byKey["GET /api/v3/repos/example/guestbook/deployments"].RawQuery; !strings.Contains(q, "ref=abc123") || !strings.Contains(q, "environment=production") {
		t.Fatalf("deployment list query %q", q)
	}
	dep := byKey["POST /api/v3/repos/example/guestbook/deployments"].json(t)
	if dep["ref"] != "abc123" || dep["environment"] != "production" || dep["auto_merge"] != false {
		t.Fatalf("deployment %v", dep)
	}
	if rc, ok := dep["required_contexts"].([]any); !ok || len(rc) != 0 {
		t.Fatalf("required_contexts must be an empty array: %v", dep["required_contexts"])
	}
	ds := byKey["POST /api/v3/repos/example/guestbook/deployments/77/statuses"].json(t)
	if ds["state"] != "success" || ds["log_url"] != "https://cd.example.com/log" || ds["environment_url"] != "https://guestbook.example.com" ||
		len([]rune(ds["description"].(string))) != 140 {
		t.Fatalf("deployment status %v", ds)
	}
	patch := byKey["PATCH /api/v3/repos/example/guestbook/issues/comments/900"].json(t)
	if !strings.HasPrefix(patch["body"].(string), "deployed\n<!-- patrol-notifications cd/guestbook -->") {
		t.Fatalf("patched comment %v", patch)
	}
	if _, ok := byKey["POST /api/v3/repos/example/guestbook/issues/6/comments"]; !ok {
		t.Fatalf("missing comment creation on PR without tagged comment")
	}
	cr := byKey["POST /api/v3/repos/example/guestbook/check-runs"].json(t)
	if cr["name"] != "cd/guestbook" || cr["head_sha"] != "abc123" || cr["conclusion"] != "success" || cr["output"].(map[string]any)["title"] != "Deployed" {
		t.Fatalf("check run %v", cr)
	}

	// Repository URL without owner/repo.
	bad := n
	bad.GitHub = &GitHubNotification{RepoURLPath: "<no value>", RevisionPath: "abc123", Status: &GitHubStatus{State: "success"}}
	if err := send(t, svc, bad, Destination{}); !errors.Is(err, ErrInvalidNotification) || !strings.Contains(err.Error(), "does not have a /") {
		t.Fatalf("got %v", err)
	}
}

func TestGitHubStatusLimitIgnored(t *testing.T) {
	t.Parallel()
	keyPEM := testRSAKeyPEM(t)
	srv, _ := newServer(t, func(w http.ResponseWriter, c captured) {
		if strings.HasSuffix(c.Path, "access_tokens") {
			writeJSON(w, 201, map[string]any{"token": "ghs_x", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
			return
		}
		writeJSON(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"message": "This SHA and context has reached the maximum number of statuses."}}})
	})
	svc := mustNew(t, TypeGitHub, map[string]any{"appID": "1", "installationID": 2, "privateKey": keyPEM, "enterpriseBaseURL": srv.URL}, testDeps())
	n := Notification{GitHub: &GitHubNotification{RepoURLPath: "https://github.com/example/guestbook", RevisionPath: "abc", Status: &GitHubStatus{State: "pending", Label: "x"}}}
	mustSend(t, svc, n, Destination{})
}

func TestGitHubInvalidOptions(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string]map[string]any{
		"missing app id": {"installationID": 1, "privateKey": "x"},
		"bad key":        {"appID": 1, "installationID": 1, "privateKey": "-----BEGIN RSA PRIVATE KEY-----\nnot a key\n-----END RSA PRIVATE KEY-----"},
	} {
		raw := mustJSON(t, opts)
		if _, err := New(TypeGitHub, raw, testDeps()); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("%s: got %v", name, err)
		}
	}
}

func TestGitURLHelpers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, https, full string
	}{
		{"https://github.com/example/guestbook.git", "https://github.com/example/guestbook.git", "example/guestbook"},
		{"git@github.com:example/guestbook.git", "https://github.com/example/guestbook.git", "example/guestbook"},
		{"ssh://git@gitlab.com/group/sub/repo.git", "https://gitlab.com/group/sub/repo.git", "group/sub"},
		{"https://user:pass@bitbucket.org/team/repo", "https://bitbucket.org/team/repo", "team/repo"},
		{"https://github.com/example/guestbook/", "https://github.com/example/guestbook/", "example/guestbook"},
	}
	for _, tc := range tests {
		got, err := RepoURLToHTTPS(tc.in)
		if err != nil || got != tc.https {
			t.Fatalf("RepoURLToHTTPS(%q) = %q, %v; want %q", tc.in, got, err, tc.https)
		}
		if got := FullNameByRepoURL(tc.in); got != tc.full {
			t.Fatalf("FullNameByRepoURL(%q) = %q; want %q", tc.in, got, tc.full)
		}
	}
}
