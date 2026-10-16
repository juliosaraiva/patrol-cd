package app_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
)

const cmV1 = `apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
data:
  greeting: hello
`

const cmV2 = `apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
data:
  greeting: hello-v2
`

const cmExtra = `apiVersion: v1
kind: ConfigMap
metadata:
  name: extra
data:
  k: v
`

// TestE2EAppLifecycle drives create, list, get, set/unset, diff, sync, wait, history,
// rollback, manifests, resources and delete through the real CLI, API server and envtest.
func TestE2EAppLifecycle(t *testing.T) {
	x := newE2E(t)
	x.repo.set(t, "guestbook", "", cmV1, cmExtra)
	x.repo.set(t, "guestbook", "v2", cmV2, cmExtra)
	admin := x.token("admin")

	// create, idempotent create, conflicting create, upsert
	x.RunAs(admin, x.createArgs("guestbook", "default", "guestbook", "--label", "team=a", "--sync-option", "CreateNamespace=true")...).
		Succeeded(t).Contains(t, "application 'guestbook' created")
	a := x.app("guestbook")
	require.NotNil(t, a)
	assert.Equal(t, "a", a.Labels["team"])
	assert.Equal(t, v1alpha1.SyncOptions{"CreateNamespace=true"}, a.Spec.SyncPolicy.SyncOptions)
	x.RunAs(admin, x.createArgs("guestbook", "default", "guestbook", "--label", "team=a", "--sync-option", "CreateNamespace=true")...).
		Succeeded(t).Contains(t, "application 'guestbook' unchanged")
	x.RunAs(admin, x.createArgs("guestbook", "default", "other")...).
		Failed(t, 20, "existing application spec is different, use upsert flag to force update")
	x.RunAs(admin, x.createArgs("guestbook", "default", "guestbook", "--label", "team=a", "--upsert", "--revision", "main")...).
		Succeeded(t).Contains(t, "application 'guestbook' updated")
	assert.Equal(t, "main", x.app("guestbook").Spec.Source.TargetRevision)
	x.waitReconciled("guestbook")

	// list and filters
	x.RunAs(admin, "app", "list").Succeeded(t).Contains(t, "NAME", x.Namespace+"/guestbook", "default", "OutOfSync")
	x.RunAs(admin, "app", "list", "-o", "name").Succeeded(t).Contains(t, x.Namespace+"/guestbook")
	assert.NotContains(t, x.RunAs(admin, "app", "list", "-l", "team=b", "-o", "name").Succeeded(t).Stdout, "guestbook")
	assert.NotContains(t, x.RunAs(admin, "app", "list", "-p", "team-a", "-o", "name").Succeeded(t).Stdout, "guestbook")
	assert.Contains(t, x.RunAs(admin, "app", "list", "-r", repoURL, "-o", "name").Succeeded(t).Stdout, "guestbook")

	// get: summary, refresh, json
	x.RunAs(admin, "app", "get", "guestbook", "--refresh").Succeeded(t).
		Contains(t, "Name:", x.Namespace+"/guestbook", "Project:", "default", "Server:", inCluster, "Repo:", repoURL, "Path:", "guestbook",
			"Sync Status:", "OutOfSync", "Health Status:", "Missing", "ConfigMap", "cfg")
	got := decodeJSON[v1alpha1.Application](t, x.RunAs(admin, "app", "get", "guestbook", "-o", "json").Succeeded(t).Stdout)
	assert.Equal(t, "guestbook", got.Name)

	// set / unset
	x.RunAs(admin, "app", "set", "guestbook", "--directory-recurse", "--sync-retry-limit", "2", "--revision-history-limit", "5").Succeeded(t)
	a = x.app("guestbook")
	require.NotNil(t, a.Spec.Source.Directory)
	assert.True(t, a.Spec.Source.Directory.Recurse)
	require.NotNil(t, a.Spec.SyncPolicy.Retry)
	assert.EqualValues(t, 2, a.Spec.SyncPolicy.Retry.Limit)
	assert.EqualValues(t, 5, *a.Spec.RevisionHistoryLimit)
	x.RunAs(admin, "app", "set", "guestbook", "--directory-exclude", "*.bak").Succeeded(t)
	assert.Equal(t, "*.bak", x.app("guestbook").Spec.Source.Directory.Exclude)
	x.RunAs(admin, "app", "set", "guestbook", "--directory-recurse=false", "--directory-exclude", "").Succeeded(t)

	// diff before sync: both ConfigMaps missing (exit code 1), --exit-code=false
	r := x.RunAs(admin, "app", "diff", "guestbook")
	require.Equal(t, 1, r.Code, r.Stderr)
	assert.Contains(t, r.Stdout, "===== /ConfigMap "+x.dest+"/cfg ======")
	assert.Contains(t, r.Stdout, "greeting: hello")
	x.RunAs(admin, "app", "diff", "guestbook", "--exit-code=false").Succeeded(t)
	r = x.RunAs(admin, "app", "diff", "guestbook", "--diff-exit-code", "7")
	require.Equal(t, 7, r.Code, r.Stderr)

	// sync a single resource, then everything
	x.RunAs(admin, "app", "sync", "guestbook", "--resource", ":ConfigMap:cfg", "--timeout", "60").Succeeded(t).
		Contains(t, "Operation:", "Sync", "Phase:", "Succeeded", "configmap/cfg created")
	require.NotNil(t, x.configMap("cfg"))
	assert.Nil(t, x.configMap("extra"), "--resource selects one resource")
	x.RunAs(admin, "app", "sync", "guestbook", "--timeout", "60").Succeeded(t).Contains(t, "Succeeded", "configmap/extra created")
	require.NotNil(t, x.configMap("extra"))

	// diff after sync: no difference
	x.RunAs(admin, "app", "diff", "guestbook").Succeeded(t)
	x.RunAs(admin, "app", "diff", "guestbook", "--server-side-diff").Succeeded(t)
	// a revision with a different ConfigMap
	r = x.RunAs(admin, "app", "diff", "guestbook", "--revision", "v2")
	require.Equal(t, 1, r.Code, r.Stderr)
	assert.Contains(t, r.Stdout, "greeting: hello-v2")

	// wait
	x.RunAs(admin, "app", "wait", "guestbook", "--health", "--sync", "--timeout", "30").Succeeded(t).Contains(t, "Synced", "Healthy")

	// history: two syncs of revision main
	h := x.RunAs(admin, "app", "history", "guestbook").Succeeded(t)
	h.Contains(t, "SOURCE", repoURL, "ID", "DATE", "REVISION", "main ("+fakeSHA("main")[:7]+")")
	ids := strings.Fields(x.RunAs(admin, "app", "history", "guestbook", "-o", "id").Succeeded(t).Stdout)
	require.Equal(t, []string{"0", "1"}, ids)

	// sync revision v2, then roll back to the first deployment
	x.RunAs(admin, "app", "sync", "guestbook", "--revision", "v2", "--timeout", "60").Succeeded(t)
	assert.Equal(t, "hello-v2", x.configMap("cfg").Data["greeting"])
	x.RunAs(admin, "app", "rollback", "guestbook", "0", "--timeout", "60").Succeeded(t).Contains(t, "Succeeded")
	assert.Equal(t, "hello", x.configMap("cfg").Data["greeting"])
	assert.Len(t, x.app("guestbook").Status.History, 4)

	// manifests (git and live), resources
	x.RunAs(admin, "app", "manifests", "guestbook").Succeeded(t).Contains(t, "---", "kind: ConfigMap", "name: cfg", "greeting: hello")
	x.RunAs(admin, "app", "manifests", "guestbook", "--revision", "v2").Succeeded(t).Contains(t, "greeting: hello-v2")
	x.RunAs(admin, "app", "manifests", "guestbook", "--source", "live").Succeeded(t).Contains(t, "name: cfg", "uid:")
	x.RunAs(admin, "app", "resources", "guestbook").Succeeded(t).Contains(t, "GROUP", "KIND", "ConfigMap", "cfg", "extra")
	x.RunAs(admin, "app", "resources", "guestbook", "--output", "tree=detailed").Succeeded(t).Contains(t, "AGE", "HEALTH", "Healthy", "cfg")
	x.RunAs(admin, "app", "resources", "guestbook", "--orphaned").Succeeded(t).NotContains(t, "cfg")

	// delete with --wait: the Application disappears
	x.RunAs(admin, "app", "delete", "guestbook", "-y", "--wait").Succeeded(t)
	assert.Nil(t, x.app("guestbook"))
	x.RunAs(admin, "app", "get", "guestbook").Failed(t, 20, "permission denied")
}

// TestE2EAppResourceCommands covers get-resource, patch-resource, delete-resource, actions
// and terminate-op / confirm-deletion against live objects in envtest.
func TestE2EAppResourceCommands(t *testing.T) {
	x := newE2E(t)
	deployment := func(name string) string {
		return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: ` + name + `
spec:
  replicas: 1
  selector:
    matchLabels: {app: web}
  template:
    metadata:
      labels: {app: web}
    spec:
      containers:
      - name: app
        image: nginx
`
	}
	x.repo.set(t, "web", "", cmV1, deployment("web"), deployment("web2"))
	admin := x.token("admin")
	x.RunAs(admin, x.createArgs("web", "default", "web")...).Succeeded(t)
	x.RunAs(admin, "app", "sync", "web", "--timeout", "60").Succeeded(t)

	x.RunAs(admin, "app", "get-resource", "web", "--kind", "ConfigMap", "--resource-name", "cfg", "-o", "yaml").Succeeded(t).
		Contains(t, "kind: ConfigMap", "greeting: hello")
	x.RunAs(admin, "app", "get-resource", "web", "--kind", "ConfigMap", "--filter-fields", "data.greeting").Succeeded(t).
		Contains(t, "greeting", "hello")

	x.RunAs(admin, "app", "patch-resource", "web", "--kind", "ConfigMap", "--resource-name", "cfg", "--patch", `{"data":{"greeting":"patched"}}`).Succeeded(t)
	assert.Equal(t, "patched", x.configMap("cfg").Data["greeting"])
	x.RunAs(admin, "app", "patch-resource", "web", "--kind", "Secret", "--patch", `{}`).Failed(t, 20, "no matching resource found")

	// actions on the Deployment (built-in Lua scripts executed by the server)
	x.RunAs(admin, "app", "actions", "list", "web", "--kind", "Deployment").Succeeded(t).Contains(t, "restart", "scale", "pause", "resume")
	x.RunAs(admin, "app", "actions", "run", "web", "scale", "--kind", "Deployment", "--param", "replicas=3").
		Failed(t, 20, "multiple resources match inputs, use the --all flag to patch multiple resources")
	x.RunAs(admin, "app", "actions", "run", "web", "scale", "--kind", "Deployment", "--resource-name", "web", "--param", "replicas=3").Succeeded(t)
	dep, err := x.Clientset.AppsV1().Deployments(x.dest).Get(x.Context(), "web", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, dep.Spec.Replicas)
	assert.EqualValues(t, 3, *dep.Spec.Replicas)
	x.RunAs(admin, "app", "actions", "run", "web", "restart", "--kind", "Deployment", "--all").Succeeded(t)
	for _, n := range []string{"web", "web2"} {
		d, err := x.Clientset.AppsV1().Deployments(x.dest).Get(x.Context(), n, metav1.GetOptions{})
		require.NoError(t, err)
		assert.NotEmpty(t, d.Spec.Template.Annotations, "%s restarted", n)
	}
	x.RunAs(admin, "app", "actions", "run", "web", "nope", "--kind", "ConfigMap").Failed(t, 20)

	// default foreground propagation: envtest runs no garbage collector, so the ConfigMap
	// keeps the foregroundDeletion finalizer; --force deletes in the background
	x.RunAs(admin, "app", "delete-resource", "web", "--kind", "ConfigMap", "--resource-name", "cfg").Succeeded(t)
	cm := x.configMap("cfg")
	require.NotNil(t, cm)
	assert.NotNil(t, cm.DeletionTimestamp)
	assert.Contains(t, cm.Finalizers, metav1.FinalizerDeleteDependents)
	x.RunAs(admin, "app", "delete-resource", "web", "--kind", "Deployment", "--resource-name", "web", "--force").Succeeded(t)
	x.WaitFor("Deployment deleted", func() bool {
		_, err := x.Clientset.AppsV1().Deployments(x.dest).Get(x.Context(), "web", metav1.GetOptions{})
		return err != nil
	})

	// confirm-deletion sets the approval annotation
	x.RunAs(admin, "app", "confirm-deletion", "web").Succeeded(t)
	assert.NotEmpty(t, x.app("web").Annotations[v1alpha1.AnnotationKeyDeletionApproved])

	// terminate-op cancels a running operation
	a := x.app("web")
	if a.Annotations == nil {
		a.Annotations = map[string]string{}
	}
	a.Annotations[holdAnnotation] = "true"
	require.NoError(t, x.Kube.Update(x.Context(), a))
	x.RunAs(admin, "app", "sync", "web", "--async").Succeeded(t)
	x.WaitFor("operation running", func() bool {
		st := x.app("web").Status.OperationState
		return st != nil && st.Phase == v1alpha1.OperationRunning
	})
	x.RunAs(admin, "app", "terminate-op", "web").Succeeded(t).Contains(t, "Application 'web' operation terminating")
	x.RunAs(admin, "app", "wait", "web", "--operation", "--timeout", "30").Succeeded(t)
	assert.Equal(t, v1alpha1.OperationCancelled, x.app("web").Status.OperationState.Phase)
	x.RunAs(admin, "app", "terminate-op", "web").Failed(t, 20, "No operation is in progress")
}

// TestE2EAppLocal covers --local sync (client-side rendering), diff --local with
// server-side generation (uploaded archive honoring ignore rules) and edit/patch.
func TestE2EAppLocal(t *testing.T) {
	x := newE2E(t)
	x.repo.set(t, "guestbook", "", cmV1)
	admin := x.token("admin")
	x.RunAs(admin, x.createArgs("guestbook", "default", "guestbook")...).Succeeded(t)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cm.yaml"), []byte(cmV2), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ignored.yaml"), []byte(cmExtra), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.yaml\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a manifest"), 0o600))

	// diff --local, client-side render: the local ConfigMap differs from the (missing) live one
	r := x.RunAs(admin, "app", "diff", "guestbook", "--local", dir)
	require.Equal(t, 1, r.Code, r.Stderr)
	assert.Contains(t, r.Stdout, "greeting: hello-v2")
	assert.Contains(t, r.Stdout, "name: extra", "client-side rendering reads every manifest of the directory")

	// diff --local --server-side-generate: the archive leaves out ignored and non-manifest files
	r = x.RunAs(admin, "app", "diff", "guestbook", "--local", dir, "--server-side-generate")
	require.Equal(t, 1, r.Code, r.Stderr)
	assert.Contains(t, r.Stdout, "greeting: hello-v2")
	assert.NotContains(t, r.Stdout, "name: extra")
	assert.Equal(t, []string{"cm.yaml"}, x.repo.lastUpload())

	// sync --local applies the locally rendered manifests
	x.RunAs(admin, "app", "sync", "guestbook", "--local", dir, "--timeout", "60").Succeeded(t).Contains(t, "Succeeded")
	require.NotNil(t, x.configMap("cfg"))
	assert.Equal(t, "hello-v2", x.configMap("cfg").Data["greeting"])

	// patch (json and merge) and edit
	x.RunAs(admin, "app", "patch", "guestbook", "--type", "merge", "--patch", `{"spec":{"source":{"targetRevision":"release"}}}`).Succeeded(t).
		Contains(t, "targetRevision: release")
	assert.Equal(t, "release", x.app("guestbook").Spec.Source.TargetRevision)
	x.RunAs(admin, "app", "patch", "guestbook", "--patch", `[{"op":"replace","path":"/spec/source/path","value":"guestbook2"}]`).Succeeded(t)
	assert.Equal(t, "guestbook2", x.app("guestbook").Spec.Source.Path)

	editor := filepath.Join(t.TempDir(), "editor.sh")
	require.NoError(t, os.WriteFile(editor, []byte("#!/bin/sh\nsed -i.bak 's/guestbook2/guestbook3/' \"$1\"\n"), 0o700))
	x.RunWith(clitest.RunOptions{NoServerFlags: true, Env: map[string]string{"EDITOR": editor, "PATH": "/usr/bin:/bin"}},
		append(append(x.ServerFlags(), "--auth-token", admin), "app", "edit", "guestbook")...).Succeeded(t)
	assert.Equal(t, "guestbook3", x.app("guestbook").Spec.Source.Path)
}

// TestE2EAppMultiSource covers add-source / remove-source and source positions.
func TestE2EAppMultiSource(t *testing.T) {
	x := newE2E(t)
	admin := x.token("admin")
	spec := `apiVersion: cd.patrol.example/v1alpha1
kind: Application
metadata:
  name: multi
spec:
  project: default
  destination:
    server: ` + inCluster + `
    namespace: ` + x.dest + `
  sources:
  - repoURL: ` + repoURL + `
    path: one
    name: first
  - repoURL: ` + repoURL + `
    path: two
    name: second
`
	file := filepath.Join(t.TempDir(), "app.yaml")
	require.NoError(t, os.WriteFile(file, []byte(spec), 0o600))
	x.RunAs(admin, "app", "create", "-f", file).Succeeded(t).Contains(t, "application 'multi' created")

	x.RunAs(admin, "app", "add-source", "multi", "--repo", repoURL, "--path", "three").Succeeded(t)
	require.Len(t, x.app("multi").Spec.Sources, 3)
	x.RunAs(admin, "app", "set", "multi", "--source-position", "2", "--path", "two-b").Succeeded(t)
	assert.Equal(t, "two-b", x.app("multi").Spec.Sources[1].Path)
	x.RunAs(admin, "app", "set", "multi", "--source-position", "4", "--path", "x").Failed(t, 20, "Source position should be less than the number of sources in the application")
	x.RunAs(admin, "app", "set", "multi", "--source-position", "2", "--helm-set", "image=nginx", "--helm-set", "tag=1").Succeeded(t)
	require.NotNil(t, x.app("multi").Spec.Sources[1].Helm)
	assert.Len(t, x.app("multi").Spec.Sources[1].Helm.Parameters, 2)
	x.RunAs(admin, "app", "unset", "multi", "--source-position", "2", "-p", "image").Succeeded(t)
	assert.Equal(t, []v1alpha1.HelmParameter{{Name: "tag", Value: "1"}}, x.app("multi").Spec.Sources[1].Helm.Parameters)
	x.RunAs(admin, "app", "set", "multi", "--source-name", "nope", "--path", "x").Failed(t, 1, "Unknown source name 'nope'")
	x.RunAs(admin, "app", "set", "multi", "--source-name", "second", "--source-position", "2", "--path", "x").
		Failed(t, 20, "Only one of source-position and source-name can be specified.")
	x.RunAs(admin, "app", "set", "multi", "--path", "x").
		Failed(t, 20, "Source position should be specified and must be greater than 0 for applications with multiple sources")
	x.RunAs(admin, "app", "remove-source", "multi", "--source-name", "first").Succeeded(t)
	srcs := x.app("multi").Spec.Sources
	require.Len(t, srcs, 2)
	assert.Equal(t, "two-b", srcs[0].Path)
	x.RunAs(admin, "app", "get", "multi", "--source-position", "2", "--show-params").Succeeded(t)
	x.RunAs(admin, "app", "sync", "multi", "--local", t.TempDir()).Failed(t, 1, "does not work on multi-source app with --local flag")
	x.RunAs(admin, "app", "remove-source", "multi", "--source-position", "2").Succeeded(t)
	x.RunAs(admin, "app", "remove-source", "multi", "--source-position", "1").Failed(t, 20, "Cannot remove the only source remaining in the app")

	x.RunAs(admin, x.createArgs("single", "default", "one")...).Succeeded(t)
	x.RunAs(admin, "app", "add-source", "single", "--repo", repoURL, "--path", "two").
		Failed(t, 20, "Cannot add source: application single does not have spec.sources defined")
	x.RunAs(admin, "app", "remove-source", "single", "--source-position", "1").Failed(t, 20, "Application does not have multiple sources configured")
}

// TestE2EAppAuthorization checks that RBAC denials and hidden existence reach the user.
func TestE2EAppAuthorization(t *testing.T) {
	x := newE2E(t)
	x.repo.set(t, "guestbook", "", cmV1)
	admin, alice, bob, nobody := x.token("admin"), x.token("alice"), x.token("bob"), x.token("nobody")
	x.RunAs(admin, x.createArgs("guestbook", "default", "guestbook")...).Succeeded(t)

	// alice (read-only): reads, but cannot change, sync or delete
	x.RunAs(alice, "app", "get", "guestbook").Succeeded(t).Contains(t, "guestbook")
	x.RunAs(alice, "app", "list", "-o", "name").Succeeded(t).Contains(t, "guestbook")
	x.RunAs(alice, x.createArgs("other", "default", "guestbook")...).Failed(t, 20, "permission denied")
	x.RunAs(alice, "app", "set", "guestbook", "--revision", "x").Failed(t, 20, "permission denied")
	x.RunAs(alice, "app", "sync", "guestbook").Failed(t, 20, "permission denied")
	x.RunAs(alice, "app", "delete", "guestbook", "-y").Failed(t, 20, "permission denied")
	require.NotNil(t, x.app("guestbook"))

	// nobody: the Application is invisible
	x.RunAs(nobody, "app", "get", "guestbook").Failed(t, 20, "permission denied")
	assert.NotContains(t, x.RunAs(nobody, "app", "list", "-o", "name").Succeeded(t).Stdout, "guestbook")

	// bob: full control of team-a only
	x.RunAs(bob, x.createArgs("bobs", "team-a", "guestbook")...).Succeeded(t)
	x.RunAs(bob, x.createArgs("bobs-default", "default", "guestbook")...).Failed(t, 20, "permission denied")
	x.RunAs(bob, "app", "sync", "bobs", "--timeout", "60").Succeeded(t)
	x.RunAs(bob, "app", "sync", "guestbook").Failed(t, 20, "permission denied")
	out := x.RunAs(bob, "app", "list", "-o", "name").Succeeded(t).Stdout
	assert.Contains(t, out, "bobs")
	assert.NotContains(t, out, "/guestbook")
	x.RunAs(bob, "app", "delete", "bobs", "-y", "--wait").Succeeded(t)
	assert.Nil(t, x.app("bobs"))

	// no credentials at all
	x.RunWith(clitest.RunOptions{NoServerFlags: true}, append(x.ServerFlags(), "app", "list")...).Failed(t, 20)
}

// TestE2EAppSet covers appset create (upsert, dry-run, wait), get, list, generate and
// delete, including authorization.
func TestE2EAppSet(t *testing.T) {
	x := newE2E(t)
	admin, alice := x.token("admin"), x.token("alice")
	appset := func(name, proj, envs string) string {
		var elems []string
		for _, e := range strings.Split(envs, ",") {
			elems = append(elems, "      - env: "+e)
		}
		return `apiVersion: cd.patrol.example/v1alpha1
kind: ApplicationSet
metadata:
  name: ` + name + `
spec:
  generators:
  - list:
      elements:
` + strings.Join(elems, "\n") + `
  template:
    metadata:
      name: '` + name + `-{{env}}'
    spec:
      project: ` + proj + `
      source:
        repoURL: ` + repoURL + `
        path: 'envs/{{env}}'
      destination:
        server: ` + inCluster + `
        namespace: ` + x.dest + `
`
	}
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
		return p
	}
	f1 := write("envs.yaml", appset("envs", "default", "dev,prod"))

	x.RunAs(admin, "appset", "create", f1, "--dry-run", "-o", "wide").Succeeded(t)
	assert.Empty(t, appSets(t, x), "dry-run creates nothing")
	r := x.RunAs(admin, "appset", "create", f1, "--wait").Succeeded(t)
	assert.Contains(t, r.Stderr, "ApplicationSet 'envs' created")
	r = x.RunAs(admin, "appset", "create", f1).Succeeded(t)
	assert.Contains(t, r.Stderr, "ApplicationSet 'envs' unchanged")
	f2 := write("envs2.yaml", appset("envs", "default", "dev,prod,qa"))
	x.RunAs(admin, "appset", "create", f2).Failed(t, 20, "existing ApplicationSet spec is different, use upsert flag to force update")
	r = x.RunAs(admin, "appset", "create", f2, "--upsert").Succeeded(t)
	assert.Contains(t, r.Stderr, "ApplicationSet 'envs' updated")

	x.RunAs(admin, "appset", "get", "envs").Succeeded(t).Contains(t, "Name:", "envs", "Project:", "default", "Source:", "Repo:", repoURL)
	x.RunAs(admin, "appset", "list").Succeeded(t).Contains(t, "NAME", "envs", "default")
	x.RunAs(admin, "appset", "list", "-o", "name").Succeeded(t).Contains(t, "envs")
	gen := x.RunAs(admin, "appset", "generate", f2, "-o", "yaml").Succeeded(t).Stdout
	for _, n := range []string{"envs-dev", "envs-prod", "envs-qa"} {
		assert.Contains(t, gen, "name: "+n)
	}
	assert.Contains(t, gen, "path: envs/qa")
	r = x.RunAs(admin, "appset", "generate", write("two.yaml", appset("a1", "default", "dev")+"---\n"+appset("a2", "default", "dev")))
	require.Equal(t, 1, r.Code)
	assert.Contains(t, r.Stdout, "Input file must contain one ApplicationSet")

	// read-only alice: get works, create and delete are denied
	x.RunAs(alice, "appset", "get", "envs").Succeeded(t)
	x.RunAs(alice, "appset", "create", write("a.yaml", appset("alices", "default", "dev"))).Failed(t, 20, "permission denied")
	x.RunAs(alice, "appset", "delete", "envs", "-y").Failed(t, 20, "permission denied")

	x.RunAs(admin, "appset", "delete", "envs", "-y", "--wait").Succeeded(t)
	assert.Empty(t, appSets(t, x))
	x.RunAs(admin, "appset", "get", "envs").Failed(t, 20)
}

func appSets(t *testing.T, x *e2eEnv) []string {
	t.Helper()
	var l v1alpha1.ApplicationSetList
	require.NoError(t, x.Kube.List(x.Context(), &l, client.InNamespace(x.Namespace)))
	var out []string
	for _, a := range l.Items {
		out = append(out, a.Name)
	}
	return out
}

// TestE2EAppCreateFromFiles covers create -f with several documents and stdin, --set-finalizer
// and the --validate=false path.
func TestE2EAppCreateFromFiles(t *testing.T) {
	x := newE2E(t)
	admin := x.token("admin")
	doc := func(name string) string {
		return `apiVersion: cd.patrol.example/v1alpha1
kind: Application
metadata:
  name: ` + name + `
spec:
  project: default
  source: {repoURL: ` + repoURL + `, path: ` + name + `}
  destination: {server: ` + inCluster + `, namespace: ` + x.dest + `}
`
	}
	r := x.RunWith(clitest.RunOptions{NoServerFlags: true, Stdin: doc("one") + "---\n" + doc("two")},
		append(append(x.ServerFlags(), "--auth-token", admin), "app", "create", "-f", "-", "--set-finalizer")...).Succeeded(t)
	r.Contains(t, "application 'one' created", "application 'two' created")
	for _, n := range []string{"one", "two"} {
		a := x.app(n)
		require.NotNil(t, a, n)
		assert.Contains(t, a.Finalizers, v1alpha1.FinalizerResources, n)
	}
	// strict decoding of the file
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	require.NoError(t, os.WriteFile(bad, []byte(doc("three")+"  unknownField: 1\n"), 0o600))
	x.RunAs(admin, "app", "create", "-f", bad).Failed(t, 20, "unknown field")
	assert.Nil(t, x.app("three"))

	// deleting with the resources finalizer completes once the controller removed it
	x.RunAs(admin, "app", "delete", "one", "-y", "--wait").Succeeded(t)
	assert.Nil(t, x.app("one"))
	// a non-cascading delete of several apps by selector
	x.RunAs(admin, "app", "delete", "two", "--cascade=false", "-y", "--wait").Succeeded(t)
	assert.Nil(t, x.app("two"))
}

// operations returns the Operations of Application name by name.
func (x *e2eEnv) operations(name string) map[string]*v1alpha1.Operation {
	x.T.Helper()
	var l v1alpha1.OperationList
	require.NoError(x.T, x.Kube.List(x.Context(), &l, client.InNamespace(x.Namespace), client.MatchingLabels{v1alpha1.LabelKeyOperationApplication: name}))
	out := map[string]*v1alpha1.Operation{}
	for i := range l.Items {
		out[l.Items[i].Name] = &l.Items[i]
	}
	return out
}

// newOperation returns the single Operation of Application name created since before.
func (x *e2eEnv) newOperation(name string, before map[string]*v1alpha1.Operation) *v1alpha1.Operation {
	x.T.Helper()
	var found []*v1alpha1.Operation
	for n, op := range x.operations(name) {
		if _, ok := before[n]; !ok {
			found = append(found, op)
		}
	}
	require.Len(x.T, found, 1, "new operations of %s", name)
	return found[0]
}

// TestE2EAppSyncFlags checks how sync flags reach the Operation, selector syncs and waits,
// previews and get --show-operation.
func TestE2EAppSyncFlags(t *testing.T) {
	x := newE2E(t)
	x.repo.set(t, "guestbook", "", cmV1, cmExtra)
	admin := x.token("admin")
	x.RunAs(admin, x.createArgs("s1", "default", "guestbook", "--label", "group=sync")...).Succeeded(t)
	x.waitReconciled("s1")

	// dry-run: nothing applied, no history
	ops := x.operations("s1")
	x.RunAs(admin, "app", "sync", "s1", "--dry-run", "--timeout", "60").Succeeded(t).Contains(t, "(dry run)")
	assert.Nil(t, x.configMap("cfg"))
	assert.Empty(t, x.app("s1").Status.History)
	assert.True(t, x.newOperation("s1", ops).Spec.Sync.DryRun)

	// operation options; an exclusion selector expands to the remaining resources
	ops = x.operations("s1")
	x.RunAs(admin, "app", "sync", "s1", "--prune", "--strategy", "apply", "--force", "--server-side", "--apply-out-of-sync-only",
		"--retry-limit", "3", "--retry-backoff-duration", "10s", "--info", "reason=e2e", "--resource", "!:ConfigMap:extra", "--timeout", "60").Succeeded(t)
	op := x.newOperation("s1", ops)
	s := op.Spec.Sync
	assert.True(t, s.Prune)
	require.NotNil(t, s.SyncStrategy)
	require.NotNil(t, s.SyncStrategy.Apply)
	assert.True(t, s.SyncStrategy.Apply.Force)
	assert.Contains(t, s.SyncOptions, "ServerSideApply=true")
	assert.Contains(t, s.SyncOptions, "ApplyOutOfSyncOnly=true")
	assert.Equal(t, []v1alpha1.SyncOperationResource{{Kind: "ConfigMap", Namespace: x.dest, Name: "cfg"}}, s.Resources)
	require.NotNil(t, op.Spec.Retry)
	assert.EqualValues(t, 3, op.Spec.Retry.Limit)
	require.NotNil(t, op.Spec.Retry.Backoff)
	assert.Equal(t, "10s", op.Spec.Retry.Backoff.Duration)
	assert.Equal(t, []v1alpha1.Info{{Name: "reason", Value: "e2e"}}, op.Spec.Info)
	assert.NotNil(t, x.configMap("cfg"))
	assert.Nil(t, x.configMap("extra"))
	ops = x.operations("s1")
	x.RunAs(admin, "app", "sync", "s1", "--replace", "--dry-run", "--strategy", "hook", "--timeout", "60").Succeeded(t)
	op = x.newOperation("s1", ops)
	assert.Contains(t, op.Spec.Sync.SyncOptions, "Replace=true")
	require.NotNil(t, op.Spec.Sync.SyncStrategy.Hook)
	x.RunAs(admin, "app", "sync", "s1", "--strategy", "bogus").Failed(t, 1, "Unknown sync strategy: 'bogus'")
	x.RunAs(admin, "app", "sync", "s1", "--info", "novalue").Failed(t, 20, "labels should have key=value, but instead got: novalue")

	// selector sync of several Applications, asynchronously, then wait by selector
	x.RunAs(admin, x.createArgs("s2", "default", "guestbook", "--label", "group=sync")...).Succeeded(t)
	x.waitReconciled("s2")
	x.RunAs(admin, "app", "sync", "-l", "group=sync", "--async").Succeeded(t)
	x.RunAs(admin, "app", "wait", "-l", "group=sync", "--operation", "--sync", "--health", "--timeout", "60").Succeeded(t)
	for _, n := range []string{"s1", "s2"} {
		assert.Equal(t, v1alpha1.SyncStatusCodeSynced, x.app(n).Status.Sync.Status, n)
	}
	x.RunAs(admin, "app", "sync", "-l", "group=none").Failed(t, 1, "No matching apps found for filter: selector group=none")

	// preview: no differences ends without syncing; a declined preview syncs nothing
	before := len(x.app("s1").Status.History)
	x.RunAs(admin, "app", "sync", "s1", "--preview-changes").Succeeded(t).Contains(t, "====== No Differences found ======")
	x.repo.set(t, "guestbook", "", cmV2, cmExtra)
	r := x.RunWith(clitest.RunOptions{NoServerFlags: true, Stdin: "n\n"}, append(append(x.ServerFlags(), "--auth-token", admin),
		"app", "sync", "s1", "--preview-changes")...).Succeeded(t)
	r.Contains(t, "====== Previewing differences between live and desired state of application "+x.Namespace+"/s1 ======", "hello-v2")
	assert.Len(t, x.app("s1").Status.History, before)
	x.RunAs(admin, "app", "sync", "s1", "--preview-changes", "--assumeYes", "--timeout", "60").Succeeded(t)
	assert.Equal(t, "hello-v2", x.configMap("cfg").Data["greeting"])

	// get variants
	x.RunAs(admin, "app", "get", "s1", "--show-operation").Succeeded(t).Contains(t, "Operation:", "Sync", "Phase:", "Succeeded")
	x.RunAs(admin, "app", "get", "s1", "-o", "tree").Succeeded(t).Contains(t, "ConfigMap", "cfg")
	x.RunAs(admin, "app", "get", "s1", "--hard-refresh", "-o", "wide").Succeeded(t).Contains(t, "Name:")

	// wait --delete
	x.RunAs(admin, "app", "delete", "s2", "-y").Succeeded(t)
	x.RunAs(admin, "app", "wait", "s2", "--delete", "--timeout", "30").Succeeded(t)
	assert.Nil(t, x.app("s2"))
	x.RunAs(admin, "app", "wait", "s1", "--suspended", "--timeout", "2").
		Failed(t, 1, `timed out (2s) waiting for app "s1" to match the expected conditions`).
		Contains(t, "This is the state of the app after wait timed out:", "The command timed out waiting for the conditions to be met.")
}

// TestE2EAppHydratorFlags creates a source hydrator Application from flags and clears its
// hydrateTo branch (APP-138).
func TestE2EAppHydratorFlags(t *testing.T) {
	x := newE2E(t)
	admin := x.token("admin")
	x.RunAs(admin, "app", "create", "hyd", "--dry-source-repo", repoURL, "--dry-source-revision", "main", "--dry-source-path", "base",
		"--sync-source-branch", "env/dev", "--sync-source-path", "dev", "--hydrate-to-branch", "env/dev-next",
		"--dest-server", inCluster, "--dest-namespace", x.dest, "--project", "default", "--validate=false").Succeeded(t)
	a := x.app("hyd")
	require.NotNil(t, a.Spec.SourceHydrator)
	assert.Nil(t, a.Spec.Source)
	h := a.Spec.SourceHydrator
	assert.Equal(t, v1alpha1.DrySource{RepoURL: repoURL, TargetRevision: "main", Path: "base"}, h.DrySource)
	assert.Equal(t, "env/dev", h.SyncSource.TargetBranch)
	assert.Equal(t, "dev", h.SyncSource.Path)
	require.NotNil(t, h.HydrateTo)
	assert.Equal(t, "env/dev-next", h.HydrateTo.TargetBranch)
	x.RunAs(admin, "app", "set", "hyd", "--hydrate-to-branch", "", "--validate=false").Succeeded(t)
	assert.Nil(t, x.app("hyd").Spec.SourceHydrator.HydrateTo)
	// get shows the sync source (upstream GetSource of a hydrator Application)
	x.RunAs(admin, "app", "get", "hyd").Succeeded(t).Contains(t, "Repo:             "+repoURL, "Target:           env/dev", "Path:             dev")

	// Source options of a hydrator Application are stored in its dry source (they are not
	// dropped while the command reports success).
	before := x.app("hyd").Spec
	x.RunAs(admin, "app", "set", "hyd", "--path", "new-path", "--helm-set", "a=b", "--validate=false").
		Failed(t, 20, "--path cannot be used with an application that uses a source hydrator")
	assert.Equal(t, before, x.app("hyd").Spec, "a rejected set changes nothing")
	x.RunAs(admin, "app", "set", "hyd", "--helm-set", "image.tag=v2", "-p", "replicas=3", "--validate=false").Succeeded(t)
	dry := x.app("hyd").Spec.SourceHydrator.DrySource
	require.NotNil(t, dry.Helm)
	assert.Equal(t, []v1alpha1.HelmParameter{{Name: "image.tag", Value: "v2"}, {Name: "replicas", Value: "3"}}, dry.Helm.Parameters)
	assert.Equal(t, "base", dry.Path)
	x.RunAs(admin, "app", "unset", "hyd", "-p", "image.tag").Succeeded(t)
	assert.Equal(t, []v1alpha1.HelmParameter{{Name: "replicas", Value: "3"}}, x.app("hyd").Spec.SourceHydrator.DrySource.Helm.Parameters)

	// create -f FILE with a hydrator spec applies tool options to the dry source (flags are
	// not applied to stdin input, as upstream).
	doc := `apiVersion: cd.patrol.example/v1alpha1
kind: Application
metadata:
  name: hyd-file
spec:
  project: default
  sourceHydrator:
    drySource: {repoURL: ` + repoURL + `, targetRevision: main, path: base}
    syncSource: {targetBranch: env/dev, path: dev}
  destination: {server: ` + inCluster + `, namespace: ` + x.dest + `}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "hyd.yaml")
	require.NoError(t, os.WriteFile(file, []byte(doc), 0o600))
	x.RunAs(admin, "app", "create", "-f", file, "--nameprefix", "dev-", "--validate=false").
		Succeeded(t).Contains(t, "application 'hyd-file' created")
	f := x.app("hyd-file")
	require.NotNil(t, f)
	assert.Nil(t, f.Spec.Source)
	assert.Equal(t, &v1alpha1.ApplicationSourceKustomize{NamePrefix: "dev-"}, f.Spec.SourceHydrator.DrySource.Kustomize)
	file2 := filepath.Join(dir, "hyd2.yaml")
	require.NoError(t, os.WriteFile(file2, []byte(strings.ReplaceAll(doc, "hyd-file", "hyd-file2")), 0o600))
	x.RunAs(admin, "app", "create", "-f", file2, "--repo", repoURL, "--validate=false").
		Failed(t, 20, "--repo cannot be used with an application that uses a source hydrator")
	assert.Nil(t, x.app("hyd-file2"))
}

// TestE2EAppDiffSanitizesRenderedNames renders resources whose names carry terminal escapes
// (rendered repository content is not validated by Kubernetes before it is applied) and checks
// that app diff, its warnings and sync --preview-changes print them without control
// characters.
func TestE2EAppDiffSanitizesRenderedNames(t *testing.T) {
	x := newE2E(t)
	const evil = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: \"x\\e]0;pwned\\a\\e[2J\"\ndata:\n  k: v\n"
	const evilSecret = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: \"s\\e[2J\"\nstringData:\n  k: v\n"
	x.repo.set(t, "evil", "", evil)
	x.repo.set(t, "evil", "v2", evil, evil, evilSecret)
	admin := x.token("admin")
	x.RunAs(admin, x.createArgs("evil", "default", "evil", "--validate=false")...).Succeeded(t)
	clean := func(t *testing.T, r clitest.Result) {
		t.Helper()
		for _, u := range []string{"\x1b", "\x07"} {
			assert.NotContains(t, r.Stdout, u)
			assert.NotContains(t, r.Stderr, u)
		}
	}
	header := "===== /ConfigMap " + x.dest + "/x ]0;pwned  [2J ======"

	r := x.RunAs(admin, "app", "diff", "evil")
	require.Equal(t, 1, r.Code, r.Stderr)
	assert.Contains(t, r.Stdout, header)
	clean(t, r)

	r = x.RunAs(admin, "app", "diff", "evil", "--revision", "v2")
	require.Equal(t, 1, r.Code, r.Stderr)
	assert.Contains(t, r.Stdout, header)
	assert.Contains(t, r.Stderr, "Resource /ConfigMap/"+x.dest+"/x ]0;pwned  [2J appeared 2 times among application resources.")
	// (no live Secret: its scope is unknown to the client, so no namespace is defaulted)
	assert.Contains(t, r.Stderr, "Warning: Secret /s [2J is not supported in local diff and will be ignored")
	clean(t, r)

	r = x.RunWith(clitest.RunOptions{NoServerFlags: true, Stdin: "n\n"},
		append(append(x.ServerFlags(), "--auth-token", admin), "app", "sync", "evil", "--preview-changes")...).Succeeded(t)
	assert.Contains(t, r.Stdout, header)
	clean(t, r)
}
