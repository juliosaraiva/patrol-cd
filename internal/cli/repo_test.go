package cli_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
)

func sshPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(blk)
}

func repoByURL(e *clitest.Env, url string) *v1alpha1.Repository {
	for _, r := range e.Repositories() {
		if r.Spec.URL == url {
			return &r
		}
	}
	return nil
}

func TestRepoCommands(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.LoginAs("admin")
	const url = "https://git.example.com/org/app.git"
	const secret = "repo-pass-987"

	e.Run("repo", "add", url, "--username", "git", "--password", secret, "--name", "app").Succeeded(t).Contains(t, "Repository '"+url+"' added")
	r := repoByURL(e, url)
	if r == nil || r.Spec.Name != "app" {
		t.Fatalf("repository not stored: %+v", e.Repositories())
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), secret) {
		t.Fatal("the Repository resource holds the password; it must reference a Secret")
	}
	tested := e.RepoServer.Tested()
	if len(tested) == 0 || tested[len(tested)-1].URL != url || tested[len(tested)-1].Username != "git" {
		t.Fatalf("connection test not run with the credentials: %+v", tested)
	}

	// Listing and getting.
	e.Run("repo", "list").Succeeded(t).Contains(t, "TYPE", "NAME", "REPO", "CREDS", "STATUS", "git", "app", url, "true").NotContains(t, secret)
	e.Run("repo", "list", "-o", "url").Succeeded(t).Contains(t, url+"\n")
	e.Run("repo", "list", "--refresh", "hard").Succeeded(t).Contains(t, "Successful")
	e.Run("repo", "list", "--refresh", "soft").Failed(t, 20, "unknown refresh value: soft. Supported values: hard")
	e.Run("repo", "list", "-o", "toml").Failed(t, 20, "toml")
	var got map[string]any
	if err := json.Unmarshal([]byte(e.Run("repo", "get", url, "-o", "json").Succeeded(t).Stdout), &got); err != nil {
		t.Fatal(err)
	}
	e.Run("repo", "get", url, "-o", "yaml").Succeeded(t).Contains(t, "url: "+url).NotContains(t, secret)
	// Unknown repositories are reported like forbidden ones (no existence oracle).
	e.Run("repo", "get", "https://nope.example.com/x.git").Failed(t, 20, "permission denied")

	// Interactive password when only the username is given.
	const url2 = "https://git.example.com/org/other.git"
	e.RunWith(clitest.RunOptions{Stdin: "prompted-pass\n"}, "repo", "add", url2, "--username", "git").Succeeded(t).NotContains(t, "prompted-pass")
	if tested := e.RepoServer.Tested(); tested[len(tested)-1].Password != "prompted-pass" {
		t.Fatal("prompted password not used")
	}

	// Validation errors (client side).
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"repo", "add", "https://charts.example.com", "--type", "helm"}, "Must specify --name for repos of type 'helm'"},
		{[]string{"repo", "add", url, "--bearer-token", "t", "--password", "p", "--username", "u"}, "only --bearer-token or --password is allowed, not both"},
		{[]string{"repo", "add", url, "--ssh-private-key-path", "/dev/null"}, "--ssh-private-key-path is only supported for SSH repositories."},
		{[]string{"repo", "add", url, "--tls-client-cert-path", "/dev/null"}, "--tls-client-cert-path and --tls-client-cert-key-path must be specified together"},
		{[]string{"repo", "add", url, "--insecure-oci-force-http"}, "--insecure-oci-force-http requires --type oci or --enable-oci"},
		{[]string{"repo", "add"}, ""},
	}
	for _, tc := range cases {
		res := e.Run(tc.args...)
		if res.Code == 0 || !strings.Contains(res.Stderr, tc.want) {
			t.Fatalf("%v: %+v", tc.args, res)
		}
	}

	// Connection test failures are reported and nothing is stored.
	e.Run("repo", "add", "https://unreachable.example.com/x.git").Failed(t, 20, "connection refused")
	if repoByURL(e, "https://unreachable.example.com/x.git") != nil {
		t.Fatal("unreachable repository stored")
	}

	// SSH repository with a key file.
	dir := t.TempDir()
	key := writeFile(t, dir, "id_ed25519", sshPrivateKeyPEM(t))
	e.Run("repo", "add", "git@git.example.com:org/ssh.git", "--ssh-private-key-path", key, "--insecure-ignore-host-key").Succeeded(t)
	if tested := e.RepoServer.Tested(); !strings.Contains(tested[len(tested)-1].SSHPrivateKey, "OPENSSH PRIVATE KEY") {
		t.Fatal("ssh key not sent to the connection test")
	}

	// Upsert.
	e.Run("repo", "add", url, "--username", "git", "--password", "changed-pass", "--name", "app2").Failed(t, 20)
	e.Run("repo", "add", url, "--username", "git", "--password", "changed-pass", "--name", "app2", "--upsert").Succeeded(t)
	if r := repoByURL(e, url); r == nil || r.Spec.Name != "app2" {
		t.Fatalf("upsert not applied: %+v", r)
	}

	// Removal.
	e.Run("repo", "rm", url, url2).Succeeded(t).Contains(t, "Repository '"+url+"' removed", "Repository '"+url2+"' removed")
	if repoByURL(e, url) != nil || repoByURL(e, url2) != nil {
		t.Fatal("repositories not removed")
	}
	e.Run("repo", "rm").Failed(t, 1)
}

func TestRepoAuthorization(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.CreateProject(&v1alpha1.AppProject{ObjectMeta: metaName("team-a"), Spec: v1alpha1.AppProjectSpec{SourceRepos: []string{"*"}}})
	e.CreateProject(&v1alpha1.AppProject{ObjectMeta: metaName("team-b"), Spec: v1alpha1.AppProjectSpec{SourceRepos: []string{"*"}}})
	bob := e.LoginToken("bob")
	alice := e.LoginToken("alice")
	// bob may manage repositories of team-a only.
	e.RunAs(bob, "repo", "add", "https://git.example.com/a.git", "--project", "team-a").Succeeded(t)
	e.RunAs(bob, "repo", "add", "https://git.example.com/b.git", "--project", "team-b").Failed(t, 20, "permission denied")
	e.RunAs(bob, "repo", "add", "https://git.example.com/c.git").Failed(t, 20, "permission denied")
	// alice can read but not write.
	e.RunAs(alice, "repo", "list").Succeeded(t).Contains(t, "https://git.example.com/a.git")
	e.RunAs(alice, "repo", "rm", "https://git.example.com/a.git").Failed(t, 20, "permission denied")
	if repoByURL(e, "https://git.example.com/a.git") == nil {
		t.Fatal("repository removed without permission")
	}
}

func TestRepoCredsCommands(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.LoginAs("admin")
	const prefix = "https://git.example.com/org"
	e.Run("repocreds", "add", prefix, "--username", "bot", "--password", "tmpl-pass-1").Succeeded(t).
		Contains(t, "Repository credentials for '"+prefix+"' added")
	e.Run("repocreds", "list").Succeeded(t).Contains(t, "URL PATTERN", "USERNAME", prefix, "bot").NotContains(t, "tmpl-pass-1")
	e.Run("repocreds", "list", "-o", "url").Succeeded(t).Contains(t, prefix+"\n")
	e.Run("repocreds", "list", "-o", "json").Succeeded(t).Contains(t, `"urlPrefix": "`+prefix+`"`).NotContains(t, "tmpl-pass-1")

	// A repository below the prefix inherits the template's credentials.
	e.Run("repo", "add", prefix+"/app.git").Succeeded(t)
	e.Run("repo", "list").Succeeded(t).Contains(t, "inherited")
	if tested := e.RepoServer.Tested(); tested[len(tested)-1].Username != "bot" {
		t.Fatalf("template credentials not used for the connection test: %+v", tested[len(tested)-1].LogValue())
	}

	dir := t.TempDir()
	key := writeFile(t, dir, "id", sshPrivateKeyPEM(t))
	e.Run("repocreds", "add", "git@git.example.com:org", "--ssh-private-key-path", key).Succeeded(t)
	e.Run("repocreds", "add", prefix, "--username", "bot2", "--password", "x").Failed(t, 20)
	e.Run("repocreds", "add", prefix, "--username", "bot2", "--password", "x", "--upsert").Succeeded(t)
	e.Run("repocreds", "list").Succeeded(t).Contains(t, "bot2", "git@git.example.com:org")

	alice := e.LoginToken("alice")
	e.RunAs(alice, "repocreds", "rm", prefix).Failed(t, 20, "permission denied")

	e.Run("repocreds", "rm", prefix).Succeeded(t).Contains(t, "Repository credentials for '"+prefix+"' removed")
	e.Run("repocreds", "list").Succeeded(t).NotContains(t, "bot2")
}
