package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
)

const projManifest = `apiVersion: cd.patrol.example/v1alpha1
kind: AppProject
metadata:
  name: from-file
spec:
  description: from a manifest
  sourceRepos: ["https://git.example.com/*"]
  destinations:
  - server: https://kubernetes.default.svc
    namespace: apps
`

func TestProjectCreateGetListDelete(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.LoginAs("admin")

	e.Run("proj", "create", "demo",
		"--description", "Demo project",
		"-d", "https://kubernetes.default.svc,default", "--dest", "https://kubernetes.default.svc,apps",
		"-s", "https://git.example.com/*",
		"--allow-cluster-resource", "rbac.authorization.k8s.io/ClusterRole",
		"--allow-cluster-resource", "/Namespace/team-*",
		"--deny-namespaced-resource", "/ResourceQuota",
		"--source-namespaces", "ns1,ns2",
		"--orphaned-resources", "--orphaned-resources-warn",
		"--dest-service-accounts", "https://kubernetes.default.svc,apps,deployer",
		"--signature-keys", "4AEE18F83AFDEB23",
	).Succeeded(t)
	p := e.GetProject("demo")
	if p.Spec.Description != "Demo project" || len(p.Spec.Destinations) != 2 || !slices.Equal(p.Spec.SourceRepos, []string{"https://git.example.com/*"}) ||
		len(p.Spec.ClusterResourceWhitelist) != 2 || p.Spec.ClusterResourceWhitelist[1].Name != "team-*" ||
		len(p.Spec.NamespaceResourceBlacklist) != 1 || !slices.Equal(p.Spec.SourceNamespaces, []string{"ns1", "ns2"}) ||
		p.Spec.OrphanedResources == nil || p.Spec.OrphanedResources.Warn == nil || !*p.Spec.OrphanedResources.Warn ||
		len(p.Spec.DestinationServiceAccounts) != 1 || len(p.Spec.SignatureKeys) != 1 { //nolint:staticcheck // SA1019: proj create --signature-keys
		t.Fatalf("spec not applied: %+v", p.Spec)
	}

	// get.
	g := e.Run("proj", "get", "demo").Succeeded(t)
	g.Contains(t, "Name:                        demo", "Description:                 Demo project", "Destinations:", "https://kubernetes.default.svc,default",
		"Repositories:                https://git.example.com/*", "Source Namespaces:           ns1", "Allowed Cluster Resources:", "rbac.authorization.k8s.io/ClusterRole",
		"Denied Namespaced Resources: /ResourceQuota", "Signature keys:              4AEE18F83AFDEB23", "Orphaned Resources:          enabled (warn=true)")
	var pj v1alpha1.AppProject
	if err := json.Unmarshal([]byte(e.Run("proj", "get", "demo", "-o", "json").Succeeded(t).Stdout), &pj); err != nil || pj.Name != "demo" {
		t.Fatalf("json: %v %+v", err, pj)
	}
	e.Run("proj", "get", "demo", "-o", "yaml").Succeeded(t).Contains(t, "name: demo", "sourceRepos:")
	e.Run("proj", "get", "missing").Failed(t, 20, "not found")

	// Existing project: same spec is fine, a different one needs --upsert.
	e.Run("proj", "create", "demo", "--description", "other").Failed(t, 20, "existing project spec is different")
	e.Run("proj", "create", "demo", "--description", "other", "--upsert").Succeeded(t)
	if d := e.GetProject("demo").Spec.Description; d != "other" {
		t.Fatalf("upsert: %q", d)
	}

	// From a file, stdin and a URL.
	dir := t.TempDir()
	file := writeFile(t, dir, "proj.yaml", []byte(projManifest))
	e.Run("proj", "create", "-f", file).Succeeded(t)
	if p := e.GetProject("from-file"); p.Spec.Description != "from a manifest" {
		t.Fatalf("from file: %+v", p.Spec)
	}
	e.Run("proj", "create", "other-name", "-f", file).Failed(t, 20, "project name 'other-name' does not match project spec metadata.name 'from-file'")
	e.RunWith(clitest.RunOptions{Stdin: strings.ReplaceAll(projManifest, "from-file", "from-stdin")}, "proj", "create", "-f", "-").Succeeded(t)
	if !e.ProjectExists("from-stdin") {
		t.Fatal("from stdin not created")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.ReplaceAll(projManifest, "from-file", "from-url")))
	}))
	t.Cleanup(srv.Close)
	e.Run("proj", "create", "-f", srv.URL+"/proj.yaml").Succeeded(t)
	if !e.ProjectExists("from-url") {
		t.Fatal("from URL not created")
	}
	e.Run("proj", "create", "-f", filepath.Join(dir, "missing.yaml")).Failed(t, 20, "error reading proj from uri")
	e.Run("proj", "create", "bad", "-d", "no-comma").Failed(t, 1, "Expected destination of the form: server,namespace. Received: no-comma")
	e.Run("proj", "create", "bad", "--signature-keys", "xyz").Failed(t, 1, "'xyz' is not a valid GnuPG key ID")
	e.Run("proj", "create").Failed(t, 1)

	// list.
	e.Run("proj", "list").Succeeded(t).Contains(t, "NAME", "DESCRIPTION", "DESTINATIONS", "demo", "from-file", "https://kubernetes.default.svc,apps  https://git.example.com/*")
	names := e.Run("proj", "list", "-o", "name").Succeeded(t).Stdout
	for _, n := range []string{"demo", "from-file", "from-stdin", "from-url"} {
		if !strings.Contains(names, n+"\n") {
			t.Fatalf("%s missing from %q", n, names)
		}
	}
	var items []v1alpha1.AppProject
	if err := json.Unmarshal([]byte(e.Run("proj", "list", "-o", "json").Succeeded(t).Stdout), &items); err != nil || len(items) != 4 {
		t.Fatalf("list json: %v", err)
	}

	// delete (optional prompt).
	e.Run("--prompts-enabled", "proj", "delete", "from-url").Failed(t, 20, "no input available")
	e.RunWith(clitest.RunOptions{Stdin: "n\n"}, "--prompts-enabled", "proj", "delete", "from-url").Succeeded(t).Contains(t, "The command to delete from-url was cancelled.")
	if !e.ProjectExists("from-url") {
		t.Fatal("cancelled delete removed the project")
	}
	e.Run("proj", "delete", "from-url", "from-stdin").Succeeded(t)
	if e.ProjectExists("from-url") || e.ProjectExists("from-stdin") {
		t.Fatal("projects not deleted")
	}
	e.Run("proj", "delete", "missing").Failed(t, 20, "not found")
}

func TestProjectModifiers(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.LoginAs("admin")
	e.Run("proj", "create", "p").Succeeded(t)
	spec := func() v1alpha1.AppProjectSpec { return e.GetProject("p").Spec }

	// set.
	e.Run("proj", "set", "p", "--description", "set by CLI", "-s", "https://a.example.com/*").Succeeded(t)
	if s := spec(); s.Description != "set by CLI" || !slices.Equal(s.SourceRepos, []string{"https://a.example.com/*"}) {
		t.Fatalf("set: %+v", s)
	}
	r := e.Run("proj", "set", "p")
	if r.Code != 1 || !strings.Contains(r.Stderr, "Please set at least one option to update") {
		t.Fatalf("set without options: %+v", r)
	}

	// destinations.
	e.Run("proj", "add-destination", "p", "https://kubernetes.default.svc", "web").Succeeded(t)
	e.Run("proj", "add-destination", "p", "in-cluster", "jobs", "--name").Succeeded(t)
	e.Run("proj", "add-destination", "p", "https://kubernetes.default.svc", "web").Failed(t, 1, "Specified destination is already defined in project")
	if d := spec().Destinations; len(d) != 2 || d[1].Name != "in-cluster" {
		t.Fatalf("destinations: %+v", d)
	}
	e.Run("proj", "remove-destination", "p", "https://kubernetes.default.svc", "web").Succeeded(t)
	e.Run("proj", "remove-destination", "p", "https://kubernetes.default.svc", "web").Failed(t, 1, "Specified destination does not exist in project")
	e.Run("proj", "add-destination", "p", "x").Failed(t, 1)

	// sources.
	e.Run("proj", "add-source", "p", "https://b.example.com/repo.git").Succeeded(t)
	e.Run("proj", "add-source", "p", "https://b.example.com/repo.git").Succeeded(t).Contains(t, "Source repository 'https://b.example.com/repo.git' already allowed in project")
	e.Run("proj", "remove-source", "p", "https://b.example.com/repo.git").Succeeded(t)
	e.Run("proj", "remove-source", "p", "https://b.example.com/repo.git").Succeeded(t).Contains(t, "does not exist in project")
	if s := spec().SourceRepos; slices.Contains(s, "https://b.example.com/repo.git") {
		t.Fatalf("sources: %v", s)
	}

	// source namespaces.
	e.Run("proj", "add-source-namespace", "p", "team-*").Succeeded(t)
	e.Run("proj", "add-source-namespace", "p", "team-*").Succeeded(t).Contains(t, "Source namespace 'team-*' already allowed in project")
	if s := spec().SourceNamespaces; !slices.Equal(s, []string{"team-*"}) {
		t.Fatalf("source namespaces: %v", s)
	}
	e.Run("proj", "remove-source-namespace", "p", "team-*").Succeeded(t)
	e.Run("proj", "remove-source-namespace", "p", "team-*").Succeeded(t).Contains(t, "Source namespace 'team-*' does not exist in project or cannot be removed")
	// The '*' wildcard cannot be removed with this command.
	e.Run("proj", "add-source-namespace", "p", "*").Succeeded(t)
	e.Run("proj", "remove-source-namespace", "p", "*").Succeeded(t).Contains(t, "Source namespace '*' does not exist in project or cannot be removed")
	if s := spec().SourceNamespaces; !slices.Equal(s, []string{"*"}) {
		t.Fatalf("wildcard source namespace removed: %v", s)
	}

	// resource allow/deny lists (messages on stderr).
	res := e.Run("proj", "allow-cluster-resource", "p", "rbac.authorization.k8s.io", "ClusterRole").Succeeded(t)
	if !strings.Contains(res.Stderr, "Group 'rbac.authorization.k8s.io', kind 'ClusterRole', and name '' is added to allowed cluster resources") {
		t.Fatalf("stderr: %q", res.Stderr)
	}
	e.Run("proj", "allow-cluster-resource", "p", "", "Namespace", "team-*").Succeeded(t)
	e.Run("proj", "deny-cluster-resource", "p", "rbac.authorization.k8s.io", "ClusterRole").Succeeded(t)
	if s := spec(); len(s.ClusterResourceWhitelist) != 1 || s.ClusterResourceWhitelist[0].Name != "team-*" {
		t.Fatalf("cluster allow list: %+v", s.ClusterResourceWhitelist)
	}
	e.Run("proj", "deny-cluster-resource", "p", "", "Node", "--list", "deny").Succeeded(t)
	e.Run("proj", "deny-namespace-resource", "p", "", "ResourceQuota").Succeeded(t)
	e.Run("proj", "allow-namespace-resource", "p", "apps", "Deployment", "--list", "allow").Succeeded(t)
	s := spec()
	if len(s.ClusterResourceBlacklist) != 1 || len(s.NamespaceResourceBlacklist) != 1 || len(s.NamespaceResourceWhitelist) != 1 {
		t.Fatalf("resource lists: %+v", s)
	}
	e.Run("proj", "allow-namespace-resource", "p", "", "ResourceQuota").Succeeded(t)
	if len(spec().NamespaceResourceBlacklist) != 0 {
		t.Fatal("allow-namespace-resource did not remove the deny entry")
	}
	e.Run("proj", "deny-namespace-resource", "p", "x").Failed(t, 1)

	// orphaned resources ignore list.
	e.Run("proj", "add-orphaned-ignore", "p", "", "ConfigMap", "--name", "kube-root-ca.crt").Succeeded(t)
	e.Run("proj", "add-orphaned-ignore", "p", "", "ConfigMap", "--name", "kube-root-ca.crt").Failed(t, 1, "already defined in the orphaned ignore list")
	if o := spec().OrphanedResources; o == nil || len(o.Ignore) != 1 || o.Ignore[0].Name != "kube-root-ca.crt" {
		t.Fatalf("orphaned ignore: %+v", o)
	}
	e.Run("proj", "remove-orphaned-ignore", "p", "", "ConfigMap", "--name", "kube-root-ca.crt").Succeeded(t)
	e.Run("proj", "remove-orphaned-ignore", "p", "", "ConfigMap").Failed(t, 1, "does not exist in the orphaned ignore")

	// signature keys (deprecated commands).
	r = e.Run("proj", "add-signature-key", "p", "4AEE18F83AFDEB23").Succeeded(t)
	if !strings.Contains(r.Stdout+r.Stderr, "deprecated") {
		t.Fatalf("no deprecation notice: %+v", r)
	}
	e.Run("proj", "add-signature-key", "p", "4AEE18F83AFDEB23").Failed(t, 1, "Specified signature key is already defined in project")
	e.Run("proj", "add-signature-key", "p", "nothex").Failed(t, 1, "'nothex' is not a valid GnuPG key ID")
	e.Run("proj", "remove-signature-key", "p", "4AEE18F83AFDEB23").Succeeded(t)
	e.Run("proj", "remove-signature-key", "p", "4AEE18F83AFDEB23").Failed(t, 1, "Specified signature key is not configured for project")
	// A fingerprint is stored as its short key ID.
	e.Run("proj", "add-signature-key", "p", "0123456789abcdef0123456789abcdef01234567").Succeeded(t)
	if k := spec().SignatureKeys; len(k) != 1 || k[0].KeyID != "89ABCDEF01234567" { //nolint:staticcheck // SA1019: deprecated command under test
		t.Fatalf("fingerprint key: %+v", k)
	}

	// destination service accounts.
	e.Run("proj", "add-destination-service-account", "p", "https://kubernetes.default.svc", "apps", "deployer", "--service-account-namespace", "ops").Succeeded(t)
	if d := spec().DestinationServiceAccounts; len(d) != 1 || d[0].DefaultServiceAccount != "ops:deployer" {
		t.Fatalf("dest service accounts: %+v", d)
	}
	e.Run("proj", "add-destination-service-account", "p", "https://kubernetes.default.svc", "apps", "deployer", "--service-account-namespace", "ops").Failed(t, 1, "already defined")
	e.Run("proj", "add-destination-service-account", "p", "https://kubernetes.default.svc", "apps", "dep*").Failed(t, 1, "ServiceAccount for DestinationServiceAccount must not contain wildcards")
	e.Run("proj", "add-destination-service-account", "p", "https://kubernetes.default.svc", "apps", "x", "--service-account-namespace", "o*").Failed(t, 1, "must not contain wildcards")
	e.Run("proj", "remove-destination-service-account", "p", "https://kubernetes.default.svc", "apps", "ops:deployer").Succeeded(t)
	e.Run("proj", "remove-destination-service-account", "p", "https://kubernetes.default.svc", "apps", "ops:deployer").Failed(t, 1, "does not exist")
}

func TestProjectEdit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell editor")
	}
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.LoginAs("admin")
	e.Run("proj", "create", "ed", "--description", "before").Succeeded(t)
	dir := t.TempDir()
	editor := filepath.Join(dir, "editor.sh")
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nPATH=/usr/bin:/bin\nsed -i.bak 's/description: before/description: after/' \"$1\"\n"), 0o755); err != nil { //nolint:gosec // test editor
		t.Fatal(err)
	}
	e.RunWith(clitest.RunOptions{Env: map[string]string{"EDITOR": editor}}, "proj", "edit", "ed").Succeeded(t)
	if d := e.GetProject("ed").Spec.Description; d != "after" {
		t.Fatalf("edit not saved: %q", d)
	}
	// An editor that changes nothing cancels the edit.
	noop := filepath.Join(dir, "noop.sh")
	if err := os.WriteFile(noop, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // test editor
		t.Fatal(err)
	}
	e.RunWith(clitest.RunOptions{Env: map[string]string{"EDITOR": noop}}, "proj", "edit", "ed").Failed(t, 20, "edit cancelled")
}

func TestProjectAuthorization(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.CreateProject(&v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}})
	e.CreateProject(&v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "team-b"}})
	alice := e.LoginToken("alice")
	bob := e.LoginToken("bob")
	nobody := e.LoginToken("nobody")

	e.RunAs(alice, "proj", "list").Succeeded(t).Contains(t, "team-a", "team-b")
	e.RunAs(alice, "proj", "create", "new").Failed(t, 20, "permission denied")
	e.RunAs(alice, "proj", "delete", "team-a").Failed(t, 20, "permission denied")
	e.RunAs(bob, "proj", "set", "team-a", "--description", "by bob").Succeeded(t)
	e.RunAs(bob, "proj", "set", "team-b", "--description", "by bob").Failed(t, 20, "permission denied")
	if d := e.GetProject("team-b").Spec.Description; d != "" {
		t.Fatalf("team-b modified: %q", d)
	}
	// Listing is filtered by permission.
	e.RunAs(bob, "proj", "list", "-o", "name").Succeeded(t).Contains(t, "team-a\n").NotContains(t, "team-b")
	e.RunAs(nobody, "proj", "list", "-o", "name").Succeeded(t).NotContains(t, "team-a")
	e.RunAs(nobody, "proj", "get", "team-a").Failed(t, 20, "permission denied")
}
