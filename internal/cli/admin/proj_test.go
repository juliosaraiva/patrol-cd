package admin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func TestProjGenerateSpec(t *testing.T) {
	t.Parallel()
	out, _, err := offline(t, "admin", "proj", "generate-spec", "team-a", "--description", "Team A",
		"-d", "https://kubernetes.default.svc,team-a-*", "-s", "https://github.com/example/*",
		"--allow-cluster-resource", "/Namespace/team-a-*", "--deny-namespaced-resource", "ResourceQuota",
		"--allow-namespaced-resource", "apps/Deployment", "--source-namespaces", "team-a,team-b",
		"--dest-service-accounts", "https://kubernetes.default.svc,team-a,deployer", "--orphaned-resources-warn")
	require.NoError(t, err)
	proj := &v1alpha1.AppProject{}
	require.NoError(t, yaml.UnmarshalStrict([]byte(out), proj))
	assert.Equal(t, "AppProject", proj.Kind)
	assert.Equal(t, "team-a", proj.Name)
	assert.Equal(t, "Team A", proj.Spec.Description)
	assert.Equal(t, []v1alpha1.ApplicationDestination{{Server: "https://kubernetes.default.svc", Namespace: "team-a-*"}}, proj.Spec.Destinations)
	assert.Equal(t, []string{"https://github.com/example/*"}, proj.Spec.SourceRepos)
	assert.Equal(t, []v1alpha1.ClusterResourceRestrictionItem{{Group: "", Kind: "Namespace", Name: "team-a-*"}}, proj.Spec.ClusterResourceWhitelist)
	assert.Equal(t, []metav1.GroupKind{{Kind: "ResourceQuota"}}, proj.Spec.NamespaceResourceBlacklist)
	assert.Equal(t, []metav1.GroupKind{{Group: "apps", Kind: "Deployment"}}, proj.Spec.NamespaceResourceWhitelist)
	assert.Equal(t, []string{"team-a", "team-b"}, proj.Spec.SourceNamespaces)
	assert.Equal(t, "deployer", proj.Spec.DestinationServiceAccounts[0].DefaultServiceAccount)
	require.NotNil(t, proj.Spec.OrphanedResources)
	require.NotNil(t, proj.Spec.OrphanedResources.Warn)
	assert.True(t, *proj.Spec.OrphanedResources.Warn)

	// File input written back inline.
	path := filepath.Join(t.TempDir(), "project.yaml")
	orig, err := os.ReadFile("testdata/proj/project.yaml")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, orig, 0o644))
	_, _, err = offline(t, "admin", "proj", "generate-spec", "from-file", "-f", path, "-i", "--description", "updated")
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(data, proj))
	assert.Equal(t, "updated", proj.Spec.Description)
	assert.Equal(t, []string{"https://github.com/example/*"}, proj.Spec.SourceRepos)
	back, err := os.ReadFile(path + ".back")
	require.NoError(t, err)
	assert.Equal(t, orig, back)

	// JSON output.
	out, _, err = offline(t, "admin", "proj", "generate-spec", "p", "-o", "json")
	require.NoError(t, err)
	assert.Contains(t, out, `"kind": "AppProject"`)

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"other", "-f", "testdata/proj/project.yaml"}, "project name 'other' does not match project spec metadata.name 'from-file'"},
		{[]string{"p", "-d", "only-server"}, "Expected destination of the form: server,namespace. Received: only-server"},
		{[]string{"p", "--dest-service-accounts", "a,b"}, "Expected destination service account of the form"},
		{[]string{"p", "-o", "xml"}, "unknown output format: xml"},
	} {
		_, _, err := offline(t, append([]string{"admin", "proj", "generate-spec"}, tc.args...)...)
		require.ErrorContains(t, err, tc.want)
	}
	_, _, err = offline(t, "admin", "proj", "generate-spec")
	assert.Equal(t, 1, ExitCode(err))
}

func TestUpdateRolePolicies(t *testing.T) {
	t.Parallel()
	proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: v1alpha1.AppProjectSpec{Roles: []v1alpha1.ProjectRole{
		{Name: "app-deployer", Policies: []string{"p, proj:p:app-deployer, applications, sync, p/*, allow"}},
		{Name: "viewer", Policies: []string{"p, proj:p:viewer, applications, get, p/*, allow"}},
	}}}
	set, err := policyModification("set", "applications", "*", "allow")
	require.NoError(t, err)
	assert.True(t, updateRolePolicies(proj, "*deployer*", "action/*", set))
	assert.Equal(t, []string{"p, proj:p:app-deployer, applications, sync, p/*, allow", "p, proj:p:app-deployer, applications, action/*, p/*, allow"}, proj.Spec.Roles[0].Policies)
	assert.Len(t, proj.Spec.Roles[1].Policies, 1)
	deny, err := policyModification("set", "applications", "*", "deny")
	require.NoError(t, err)
	assert.True(t, updateRolePolicies(proj, "*deployer*", "sync", deny))
	assert.Equal(t, "p, proj:p:app-deployer, applications, sync, p/*, deny", proj.Spec.Roles[0].Policies[0])
	remove, err := policyModification("remove", "", "", "")
	require.NoError(t, err)
	assert.True(t, updateRolePolicies(proj, "*", "get", remove))
	assert.Empty(t, proj.Spec.Roles[1].Policies)
	assert.False(t, updateRolePolicies(proj, "*", "get", remove), "nothing left to remove")

	for _, tc := range []struct{ mod, scope, perm, want string }{
		{"set", "", "allow", "flag --group cannot be empty if permission should be set in role"},
		{"set", "*", "", "flag --permission cannot be empty if permission should be set in role"},
		{"rename", "*", "allow", "modification rename is not supported"},
	} {
		_, err := policyModification(tc.mod, "applications", tc.scope, tc.perm)
		require.EqualError(t, err, tc.want)
	}
}

func TestProjUpdateRolePolicyCommand(t *testing.T) {
	ctx := requireEnv(t)
	ns := newNamespace(t, ctx, "projrole")
	for _, name := range []string{"team-a", "team-b", "other"} {
		createObject(t, ctx, ns, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": v1alpha1.GroupVersion.String(), "kind": "AppProject", "metadata": map[string]any{"name": name},
			"spec": map[string]any{"roles": []any{
				map[string]any{"name": "deployer", "policies": []any{"p, proj:" + name + ":deployer, applications, sync, " + name + "/*, allow"}},
				map[string]any{"name": "viewer", "policies": []any{"p, proj:" + name + ":viewer, applications, get, " + name + "/*, allow"}},
			}},
		}})
	}
	policies := func(name string) [][]string {
		obj, err := envDyn.Resource(kindAppProject.gvr(currentGroup)).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err)
		p := &v1alpha1.AppProject{}
		require.NoError(t, runtimeFromUnstructured(obj, p))
		var out [][]string
		for _, r := range p.Spec.Roles {
			out = append(out, r.Policies)
		}
		return out
	}
	tio := newTestIO(ns)
	args := []string{"admin", "proj", "update-role-policy", "team-*", "set", "action/*", "--role", "*deploy*", "--resource", "applications", "--scope", "*", "--permission", "allow"}

	// Dry run (default): diff printed, nothing changed.
	require.NoError(t, tio.run(ctx, args...))
	out := tio.out.String()
	assert.Contains(t, out, "===== team-a ======")
	assert.Contains(t, out, "===== team-b ======")
	assert.NotContains(t, out, "===== other ======")
	assert.Contains(t, out, "+    - p, proj:team-a:deployer, applications, action/*, team-a/*, allow")
	assert.Equal(t, [][]string{{"p, proj:team-a:deployer, applications, sync, team-a/*, allow"}, {"p, proj:team-a:viewer, applications, get, team-a/*, allow"}}, policies("team-a"))

	// Apply.
	require.NoError(t, tio.run(ctx, append(args, "--dry-run=false")...))
	assert.Equal(t, []string{"p, proj:team-a:deployer, applications, sync, team-a/*, allow", "p, proj:team-a:deployer, applications, action/*, team-a/*, allow"}, policies("team-a")[0])
	assert.Len(t, policies("other")[0], 1)
	// Applying again changes nothing and prints nothing.
	require.NoError(t, tio.run(ctx, append(args, "--dry-run=false")...))
	assert.Empty(t, tio.out.String())

	// Remove from every role of every project.
	require.NoError(t, tio.run(ctx, "admin", "proj", "update-role-policy", "*", "remove", "get", "--dry-run=false"))
	for _, name := range []string{"team-a", "team-b", "other"} {
		assert.Empty(t, policies(name)[1], name)
	}
	require.ErrorContains(t, tio.run(ctx, "admin", "proj", "update-role-policy", "*", "set", "get"), "flag --group cannot be empty")
}

func TestProjGenerateAllowList(t *testing.T) {
	ctx := requireEnv(t)
	tio := newTestIO("default")
	require.NoError(t, tio.run(ctx, "admin", "proj", "generate-allow-list", "testdata/proj/clusterrole.yaml", "deployers"))
	proj := &v1alpha1.AppProject{}
	require.NoError(t, yaml.UnmarshalStrict(tio.out.Bytes(), proj))
	assert.Equal(t, "deployers", proj.Name)
	assert.Equal(t, v1alpha1.GroupVersion.String(), proj.APIVersion)
	assert.ElementsMatch(t, []metav1.GroupKind{{Group: "", Kind: "ConfigMap"}, {Group: "", Kind: "Pod"}, {Group: "apps", Kind: "Deployment"}}, proj.Spec.NamespaceResourceWhitelist)

	path := filepath.Join(t.TempDir(), "allow.yaml")
	require.NoError(t, tio.run(ctx, "admin", "proj", "generate-allow-list", "testdata/proj/clusterrole.yaml", "deployers", "-o", path))
	assert.Empty(t, tio.out.String())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "kind: Deployment")

	require.ErrorContains(t, tio.run(ctx, "admin", "proj", "generate-allow-list", "testdata/proj/project.yaml", "x"), `unexpected kind "AppProject"`)
	require.ErrorContains(t, tio.run(ctx, "admin", "proj", "generate-allow-list", "testdata/proj/missing.yaml", "x"), "error reading cluster role file")
}
