package admin

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	appsettemplate "github.com/juliosaraiva/patrol-cd/internal/appset/template"
)

const (
	argoExportFixture   = "testdata/argocd/export.yaml"
	argoBlockingFixture = "testdata/argocd/blocking.yaml"
)

// argoFixtureSecrets are the credential values of the Argo CD fixtures; they must never be
// printed.
var argoFixtureSecrets = []string{
	"fake-repo-password", "fake-ssh-private-key-material", "fake-github-app-private-key", "fake-write-token",
	"fake-cluster-bearer-token", "fake-server-signing-key", "fake-github-webhook-secret", "fake-notifications-token",
	"fake-dex-client-secret", "fake-exec-env-secret", "FAKE-CA-CERTIFICATE-FOR-TESTS", "FAKEHASHFAKEHASH", "fake-token-id",
	"fake-blocking-svn-password", "fake-blocking-token", "fake-blocking-vault-token",
}

func assertNoArgoSecrets(t *testing.T, texts ...string) {
	t.Helper()
	all := strings.Join(texts, "\n")
	for _, s := range argoFixtureSecrets {
		assert.NotContains(t, all, s)
		assert.NotContains(t, all, base64.StdEncoding.EncodeToString([]byte(s)))
	}
}

func runArgoImport(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	tio := newTestIO("default")
	tio.f.RESTConfig = nil
	tio.f.In = strings.NewReader(stdin)
	err := tio.run(t.Context(), append([]string{"admin", "argocd-import"}, args...)...)
	return tio.out.String(), tio.errOut.String(), err
}

func TestArgoCDImportDryRunReport(t *testing.T) {
	t.Parallel()
	out, errOut, err := runArgoImport(t, "", argoExportFixture)
	require.NoError(t, err, out)
	assertNoArgoSecrets(t, out, errOut)
	assert.Contains(t, out, "Argo CD export: 25 object(s) read, 30 Patrol CD object(s) produced")
	assert.NotContains(t, out, "BLOCKING")
	assert.Contains(t, out, "Result: dry run, nothing was changed.")
	// One warning for each risky construct of the fixture.
	for _, want := range []string{
		// finalizers
		"WARNING: Application argocd/guestbook: finalizer resources-finalizer.argocd.argoproj.io is an Argo CD finalizer and is not honored by Patrol CD",
		"add resources-finalizer.cd.patrol.example if deletion should cascade",
		"WARNING: Application team-a/api: finalizer resources-finalizer.argocd.argoproj.io/background is an Argo CD finalizer",
		"WARNING: ApplicationSet argocd/cluster-addons template: finalizer resources-finalizer.argocd.argoproj.io is an Argo CD finalizer",
		"INFO: AppProject argocd/team-a: finalizer resources-finalizer.argocd.argoproj.io removed",
		// resource tracking and transfer procedure
		"WARNING: resource tracking: resources deployed by Argo CD carry argocd.argoproj.io/tracking-id (application.resourceTrackingMethod: annotation)",
		"Transfer procedure: (1) stop Argo CD",
		"INFO: Application team-a/api: annotation argocd.argoproj.io/tracking-id removed",
		// credentials
		"WARNING: Secret argocd/argocd-secret: server.secretkey is not copied",
		"WARNING: Secret argocd/argocd-secret: accounts.ci.tokens (API token records) is not copied",
		"WARNING: AppProject argocd/team-a: role ci has 1 JWT token(s) issued by Argo CD",
		"WARNING: Secret argocd/cluster-aks: execProviderConfig.env is copied into the Cluster spec",
		"WARNING: ApplicationSet argocd/team-repos: a generator references Secret github-token, which is not in the export",
		// destructive options
		"WARNING: Application argocd/guestbook: automated sync with prune and selfHeal was disabled (spec.syncPolicy.automated.enabled: false)",
		"WARNING: Application team-a/api: automated sync was disabled",
		"WARNING: ApplicationSet argocd/cluster-addons template: automated sync with prune was disabled",
		// generator-level templates win over spec.template: converted the same way
		"WARNING: ApplicationSet argocd/team-repos generators[0].scmProvider.template: finalizer resources-finalizer.argocd.argoproj.io is an Argo CD finalizer and is not honored by Patrol CD",
		"WARNING: ApplicationSet argocd/team-repos generators[0].scmProvider.template: automated sync with prune and selfHeal was disabled (spec.syncPolicy.automated.enabled: false)",
		// source hydrator: Patrol CD pushes hydrated commits as soon as the Application exists
		"WARNING: Application argocd/guestbook-hydrated: spec.sourceHydrator: when the source hydrator is enabled (hydrator.enabled in patrol-cmd-params-cm), Patrol CD hydrates each Application as soon as it is created, whatever its sync policy",
		`commits and pushes to branch "environments/prod" of https://github.com/example/hydrated.git with the write credentials of that repository. Stop Argo CD's hydrator before importing with --dry-run=false`,
		"WARNING: ConfigMap argocd/argocd-cmd-params-cm: hydrator.enabled is true",
		// notifications
		"WARNING: Application argocd/guestbook: notification subscription notifications.argoproj.io/subscribe.on-sync-succeeded.deployments mapped to notifications.cd.patrol.example/subscribe.on-sync-succeeded.deployments",
		"WARNING: Application argocd/guestbook: notification delivery state (notified.notifications.argoproj.io) is not transferred",
		// operations and settings
		"WARNING: Application argocd/guestbook: an in-flight Argo CD operation is not transferred",
		`WARNING: ConfigMap argocd/argocd-cm: url is "https://argocd.example.com"`,
		"WARNING: ConfigMap argocd/unrelated-config: not an Argo CD settings ConfigMap; skipped",
		"WARNING: Secret argocd/repo-private: key legacyKey is not used by Patrol CD; ignored",
		"WARNING: settings: patrol-cm: configManagementPlugins",
		"WARNING: settings: patrol-cmd-params-cm: redis.server",
		"INFO: ConfigMap argocd/argocd-cmd-params-cm: repo.server: \"argocd-repo-server:8081\" changed to \"patrol-repo-server:8081\"",
		"INFO: ApplicationSet argocd/cluster-addons: 1 generator label selector key(s) mapped",
	} {
		assert.Contains(t, out, want)
	}
	assert.Regexp(t, `Secret argocd/repo-apps\s+Secret repo-apps-creds \(credentials of Repository repo-apps\)\n`, out)
	// The namespace of team-a/api is enabled by the converted settings and project.
	assert.NotContains(t, out, "is not enabled by")
	assert.NotContains(t, out, "does not list namespace")
}

func TestArgoCDImportTrackingAndNamespaceVariants(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(argoExportFixture)
	require.NoError(t, err)
	export := string(data)
	export = strings.Replace(export, "application.resourceTrackingMethod: annotation", "application.resourceTrackingMethod: label", 1)
	export = strings.Replace(export, "application.namespaces: team-*", "application.namespaces: other-*", 1)
	export = strings.Replace(export, "  sourceNamespaces:\n  - team-a\n", "", 1)

	out, errOut, err := runArgoImport(t, export, "-")
	require.NoError(t, err, out)
	assertNoArgoSecrets(t, out, errOut)
	assert.Contains(t, out, "Argo CD tracks resources with the label app.kubernetes.io/instance (application.resourceTrackingMethod: label)")
	assert.Contains(t, out, "if both controllers run they overwrite each other")
	assert.Contains(t, out, "WARNING: Application team-a/api: namespace team-a is not enabled by application.namespaces of patrol-cmd-params-cm")
	assert.Contains(t, out, "WARNING: Application team-a/api: project team-a does not list namespace team-a in spec.sourceNamespaces")

	out, _, err = runArgoImport(t, "", argoExportFixture, "--keep-auto-sync")
	require.NoError(t, err)
	assert.Contains(t, out, "WARNING: Application argocd/guestbook: automated sync with prune and selfHeal is kept (--keep-auto-sync)")
	assert.NotContains(t, out, "was disabled")
}

func readConverted(t *testing.T, path string) map[string]*unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	objs, err := splitObjects(data)
	require.NoError(t, err)
	byName := map[string]*unstructured.Unstructured{}
	for _, o := range objs {
		key := o.GetKind() + "/" + o.GetName()
		require.NotContains(t, byName, key, "duplicate %s", key)
		byName[key] = o
	}
	return byName
}

func secretValue(t *testing.T, obj *unstructured.Unstructured, key string) string {
	t.Helper()
	sec := &corev1.Secret{}
	require.NoError(t, runtimeFromUnstructured(obj, sec))
	return string(sec.Data[key])
}

func TestArgoCDImportOutputFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "patrol.yaml")
	require.NoError(t, os.WriteFile(path, []byte("stale"), 0o644))

	out, errOut, err := runArgoImport(t, "", argoExportFixture, "-o", path)
	require.NoError(t, err, out)
	assertNoArgoSecrets(t, out, errOut)
	assert.Contains(t, out, "Wrote 30 object(s) to "+path+" (mode 0600).")
	assert.Contains(t, errOut, "contains 9 Secret(s) with credentials")
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	objs := readConverted(t, path)
	for _, o := range objs {
		assert.NotEqual(t, argoGroup, objectGroup(o), "%s kept the Argo CD group", objectRef(o))
		for _, f := range o.GetFinalizers() {
			assert.NotContains(t, f, "argoproj.io")
		}
		for k := range o.GetAnnotations() {
			assert.NotContains(t, k, "argoproj.io", objectRef(o))
		}
		for k := range o.GetLabels() {
			assert.NotContains(t, k, "argoproj.io", objectRef(o))
		}
	}

	// Settings.
	cm := objs["ConfigMap/patrol-cm"]
	require.NotNil(t, cm)
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Contains(t, data, "resource.customizations.health.cd.patrol.example_Application")
	assert.NotContains(t, data, "resource.customizations.health.argoproj.io_Application")
	assert.Equal(t, "patrol-cm", cm.GetLabels()[labelKeyAppName])
	assert.Equal(t, v1alpha1.LabelValuePartOf, cm.GetLabels()[v1alpha1.LabelKeyPartOf])
	params, _, _ := unstructured.NestedStringMap(objs["ConfigMap/patrol-cmd-params-cm"].Object, "data")
	assert.Equal(t, "patrol-repo-server:8081", params["repo.server"])
	assert.Nil(t, objs["ConfigMap/unrelated-config"])
	sec := objs["Secret/patrol-secret"]
	require.NotNil(t, sec)
	secData, _, _ := unstructured.NestedStringMap(sec.Object, "data")
	assert.NotContains(t, secData, "server.secretkey")
	assert.NotContains(t, secData, "accounts.ci.tokens")
	assert.Contains(t, secData, "admin.password")
	assert.Equal(t, "fake-github-webhook-secret", secretValue(t, sec, "webhook.github.secret"))
	assert.Equal(t, v1alpha1.LabelValuePartOf, objs["Secret/dex-github-secret"].GetLabels()[v1alpha1.LabelKeyPartOf])

	// Repositories, credential templates and clusters with their credentials Secrets.
	repo := &v1alpha1.Repository{}
	require.NoError(t, runtimeFromUnstructured(objs["Repository/repo-apps"], repo))
	assert.Equal(t, "https://github.com/example/apps.git", repo.Spec.URL)
	assert.Equal(t, "default", repo.Spec.Project)
	require.NotNil(t, repo.Spec.CredentialsRef)
	creds := objs["Secret/"+repo.Spec.CredentialsRef.Name]
	require.NotNil(t, creds)
	assert.Equal(t, v1alpha1.LabelValueSecretTypeRepository, creds.GetLabels()[v1alpha1.LabelKeySecretType])
	assert.Equal(t, "fake-repo-password", secretValue(t, creds, "password"))
	assert.Equal(t, "deploy-bot", secretValue(t, creds, "username"))

	require.NoError(t, runtimeFromUnstructured(objs["Repository/repo-private"], repo))
	assert.True(t, repo.Spec.EnableLFS)
	assert.Equal(t, "fake-ssh-private-key-material", secretValue(t, objs["Secret/repo-private-creds"], "sshPrivateKey"))

	require.NoError(t, runtimeFromUnstructured(objs["Repository/repo-charts"], repo))
	assert.Equal(t, v1alpha1.RepositoryTypeHelm, repo.Spec.Type)
	assert.Nil(t, repo.Spec.CredentialsRef)

	require.NoError(t, runtimeFromUnstructured(objs["Repository/repo-hydrated-write"], repo))
	assert.Equal(t, v1alpha1.RepositoryPurposeWrite, repo.Spec.Purpose)
	assert.Equal(t, v1alpha1.LabelValueSecretTypeRepositoryWrite, objs["Secret/repo-hydrated-write-creds"].GetLabels()[v1alpha1.LabelKeySecretType])

	rc := &v1alpha1.RepositoryCredential{}
	require.NoError(t, runtimeFromUnstructured(objs["RepositoryCredential/creds-github-app"], rc))
	assert.Equal(t, "https://github.com/example", rc.Spec.URLPrefix)
	require.NotNil(t, rc.Spec.GitHubApp)
	assert.Equal(t, int64(12345), rc.Spec.GitHubApp.AppID)
	require.NotNil(t, rc.Spec.GitHubApp.InstallationID)
	assert.Equal(t, int64(678), *rc.Spec.GitHubApp.InstallationID)
	assert.Equal(t, "fake-github-app-private-key", secretValue(t, objs["Secret/creds-github-app-creds"], "githubAppPrivateKey"))

	cl := &v1alpha1.Cluster{}
	require.NoError(t, runtimeFromUnstructured(objs["Cluster/cluster-prod"], cl))
	assert.Equal(t, "prod", cl.Spec.Name)
	assert.Equal(t, []string{"apps", "monitoring"}, cl.Spec.Namespaces)
	assert.Equal(t, "prod", objs["Cluster/cluster-prod"].GetLabels()["env"])
	clusterCreds := objs["Secret/"+cl.Spec.Config.CredentialsRef.Name]
	assert.Equal(t, "fake-cluster-bearer-token", secretValue(t, clusterCreds, v1alpha1.ClusterSecretKeyBearerToken))
	assert.Equal(t, "FAKE-CA-CERTIFICATE-FOR-TESTS", secretValue(t, clusterCreds, v1alpha1.ClusterSecretKeyCAData))
	require.NoError(t, runtimeFromUnstructured(objs["Cluster/cluster-eks"], cl))
	require.NotNil(t, cl.Spec.Config.AWSAuthConfig)
	assert.Equal(t, "eks-prod", cl.Spec.Config.AWSAuthConfig.ClusterName)
	require.NotNil(t, cl.Spec.Shard)
	assert.Equal(t, int64(1), *cl.Spec.Shard)
	require.NoError(t, runtimeFromUnstructured(objs["Cluster/cluster-aks"], cl))
	assert.True(t, cl.Spec.ClusterResources)
	assert.Equal(t, "team-a", cl.Spec.Project)
	require.NotNil(t, cl.Spec.Config.ExecProviderConfig)
	assert.Equal(t, "kubelogin", cl.Spec.Config.ExecProviderConfig.Command)

	// Projects: Argo CD tokens are dropped.
	proj := &v1alpha1.AppProject{}
	require.NoError(t, runtimeFromUnstructured(objs["AppProject/team-a"], proj))
	require.Len(t, proj.Spec.Roles, 1)
	assert.Empty(t, proj.Spec.Roles[0].JWTTokens) //nolint:staticcheck // checks that Argo CD tokens are dropped
	assert.Equal(t, []string{"team-a"}, proj.Spec.SourceNamespaces)

	// Applications: automated sync disabled, annotations mapped, transient state dropped.
	app := &v1alpha1.Application{}
	require.NoError(t, runtimeFromUnstructured(objs["Application/guestbook"], app))
	assert.Empty(t, app.Namespace)
	require.NotNil(t, app.Spec.SyncPolicy.Automated)
	require.NotNil(t, app.Spec.SyncPolicy.Automated.Enabled)
	assert.False(t, *app.Spec.SyncPolicy.Automated.Enabled)
	assert.True(t, app.Spec.SyncPolicy.Automated.GetPrune(), "prune setting is kept for when sync is enabled again")
	assert.Equal(t, map[string]string{
		"notifications.cd.patrol.example/subscribe.on-sync-succeeded.deployments": "",
		"cd.patrol.example/manifest-generate-paths":                               ".",
	}, app.Annotations)
	assert.Equal(t, v1alpha1.GroupName, app.Spec.IgnoreDifferences[0].Group)
	_, hasOperation := objs["Application/guestbook"].Object["operation"]
	assert.False(t, hasOperation)
	_, hasStatus := objs["Application/guestbook"].Object["status"]
	assert.False(t, hasStatus)
	require.NoError(t, runtimeFromUnstructured(objs["Application/api"], app))
	assert.Equal(t, "team-a", app.Namespace)
	assert.Len(t, app.Spec.Sources, 2)
	assert.Empty(t, app.Annotations)

	// ApplicationSets: template finalizers and automated sync, selector keys.
	set := &v1alpha1.ApplicationSet{}
	require.NoError(t, runtimeFromUnstructured(objs["ApplicationSet/cluster-addons"], set))
	assert.Empty(t, set.Spec.Template.Finalizers)
	require.NotNil(t, set.Spec.Template.Spec.SyncPolicy.Automated.Enabled)
	assert.False(t, *set.Spec.Template.Spec.SyncPolicy.Automated.Enabled)
	assert.Equal(t, map[string]string{v1alpha1.LabelKeySecretType: "cluster", "env": "prod"}, set.Spec.Generators[0].Clusters.Selector.MatchLabels)
	assert.Contains(t, set.Spec.Template.Annotations, "notifications.cd.patrol.example/subscribe.on-sync-succeeded.deployments")
	// The generator-level template is converted too: the effective template of the
	// generated Applications (generator fields win) has no Argo CD finalizer and no
	// automated sync.
	require.NoError(t, runtimeFromUnstructured(objs["ApplicationSet/team-repos"], set))
	genTmpl := set.Spec.Generators[0].SCMProvider.Template
	assert.Empty(t, genTmpl.Finalizers)
	assert.Equal(t, map[string]string{"cd.patrol.example/sync-wave": "1"}, genTmpl.Annotations)
	effective := appsettemplate.MergeTemplate(&genTmpl, set.Spec.Template)
	require.NotNil(t, effective.Spec.SyncPolicy.Automated)
	require.NotNil(t, effective.Spec.SyncPolicy.Automated.Enabled)
	assert.False(t, *effective.Spec.SyncPolicy.Automated.Enabled)
	assert.True(t, effective.Spec.SyncPolicy.Automated.GetPrune(), "prune and selfHeal are kept for when sync is enabled again")
	assert.Empty(t, effective.Finalizers)

	// Without Secrets.
	out, errOut, err = runArgoImport(t, "", argoExportFixture, "-o", path, "--include-secrets=false")
	require.NoError(t, err)
	assert.Contains(t, out, "Wrote 21 object(s)")
	assert.Contains(t, errOut, "9 Secret(s) were left out")
	for _, o := range readConverted(t, path) {
		assert.NotEqual(t, "Secret", o.GetKind())
	}
}

// Regression: generator-level templates (nested matrix/merge children included) kept Argo CD
// finalizers, Argo CD keys and automated prune/selfHeal, and spec.sourceHydrator gave no
// warning.
func TestArgoCDImportGeneratorTemplatesAndHydrator(t *testing.T) {
	t.Parallel()
	const export = `apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: nested
spec:
  goTemplate: true
  generators:
  - matrix:
      template:
        metadata:
          finalizers: [resources-finalizer.argocd.argoproj.io]
        spec:
          project: default
          destination: {}
          syncPolicy:
            automated: {selfHeal: true}
      generators:
      - list:
          elements: [{env: dev}]
          template:
            metadata: {}
            spec:
              project: default
              destination: {}
              syncPolicy:
                automated: {prune: true}
      - merge:
          mergeKeys: [env]
          generators:
          - list:
              elements: [{env: dev}]
              template:
                metadata:
                  labels:
                    argocd.argoproj.io/team: x
                  finalizers: [resources-finalizer.argocd.argoproj.io/foreground]
                spec:
                  project: default
                  destination: {}
                  syncPolicy:
                    automated: {}
          - list:
              elements: [{env: dev}]
  template:
    metadata:
      name: "app-{{.env}}"
    spec:
      project: default
      sourceHydrator:
        drySource: {repoURL: "https://github.com/example/apps.git", path: "{{.env}}", targetRevision: HEAD}
        syncSource: {targetBranch: "env/{{.env}}", path: "{{.env}}"}
        hydrateTo: {targetBranch: "env/{{.env}}-next"}
      destination:
        server: https://kubernetes.default.svc
        namespace: "{{.env}}"
`
	path := filepath.Join(t.TempDir(), "out.yaml")
	out, _, err := runArgoImport(t, export, "-", "-o", path)
	require.NoError(t, err, out)
	assert.NotContains(t, out, "BLOCKING")
	for _, want := range []string{
		"WARNING: ApplicationSet argocd/nested generators[0].matrix.template: finalizer resources-finalizer.argocd.argoproj.io is an Argo CD finalizer",
		"WARNING: ApplicationSet argocd/nested generators[0].matrix.template: automated sync with selfHeal was disabled",
		"WARNING: ApplicationSet argocd/nested generators[0].matrix.generators[0].list.template: automated sync with prune was disabled",
		"WARNING: ApplicationSet argocd/nested generators[0].matrix.generators[1].merge.generators[0].list.template: finalizer resources-finalizer.argocd.argoproj.io/foreground is an Argo CD finalizer",
		"WARNING: ApplicationSet argocd/nested generators[0].matrix.generators[1].merge.generators[0].list.template: automated sync was disabled",
		`WARNING: ApplicationSet argocd/nested template: spec.sourceHydrator: when the source hydrator is enabled (hydrator.enabled in patrol-cmd-params-cm), Patrol CD hydrates each generated Application as soon as it is created, whatever its sync policy`,
		`commits and pushes to branch "env/{{.env}}-next" of https://github.com/example/apps.git`,
	} {
		assert.Contains(t, out, want)
	}

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "argocd.argoproj.io", "no Argo CD key or finalizer is left, at any depth")
	set := &v1alpha1.ApplicationSet{}
	require.NoError(t, runtimeFromUnstructured(readConverted(t, path)["ApplicationSet/nested"], set))
	matrix := set.Spec.Generators[0].Matrix
	require.NotNil(t, matrix)
	effective := appsettemplate.MergeTemplate(&matrix.Template, set.Spec.Template)
	assert.Empty(t, effective.Finalizers)
	require.NotNil(t, effective.Spec.SyncPolicy.Automated.Enabled)
	assert.False(t, *effective.Spec.SyncPolicy.Automated.Enabled)
	assert.True(t, effective.Spec.SyncPolicy.Automated.GetSelfHeal())
	require.NotNil(t, matrix.Generators[0].List.Template.Spec.SyncPolicy.Automated.Enabled)
	assert.False(t, *matrix.Generators[0].List.Template.Spec.SyncPolicy.Automated.Enabled)
	merge, err := v1alpha1.ToNestedMergeGenerator(matrix.Generators[1].Merge)
	require.NoError(t, err)
	leaf := merge.Generators[0].List.Template
	assert.Equal(t, map[string]string{"cd.patrol.example/team": "x"}, leaf.Labels)
	assert.Empty(t, leaf.Finalizers)
	require.NotNil(t, leaf.Spec.SyncPolicy.Automated.Enabled)
	assert.False(t, *leaf.Spec.SyncPolicy.Automated.Enabled)
	assert.Equal(t, []string{"env"}, merge.MergeKeys, "other fields of raw nested generators are kept")
}

func TestArgoCDImportBlocking(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "never-written.yaml")
	out, errOut, err := runArgoImport(t, "", argoBlockingFixture, "-o", path)
	require.Error(t, err)
	assert.Equal(t, exitBlocking, ExitCode(err))
	assertNoArgoSecrets(t, out, errOut, err.Error())
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "nothing is written when blocking incompatibilities are found")
	assert.Contains(t, out, "BLOCKING (9)")
	for _, want := range []string{
		"BLOCKING: Rollout argocd/canary: unsupported object argoproj.io/Rollout",
		`BLOCKING: Application argocd/unknown-field: spec cannot be converted: json: unknown field "futureOption"`,
		"BLOCKING: Application argocd/two-sources: exactly one of spec.source, spec.sources or spec.sourceHydrator must be set",
		"BLOCKING: AppProject argocd/bad-policy: project is not valid for Patrol CD",
		`BLOCKING: Secret argocd/repo-svn: unsupported repository type "svn"`,
		`BLOCKING: Secret argocd/repo-bad-bool: invalid repository Secret: insecure: invalid boolean "maybe"`,
		"BLOCKING: Secret argocd/cluster-bad-config: config is not valid JSON (contents not shown)",
		`BLOCKING: Secret argocd/cluster-bad-shard: invalid cluster Secret: shard: invalid shard "first"`,
		`BLOCKING: Secret argocd/mystery: unknown argocd.argoproj.io/secret-type "vault"`,
		"Result: 9 blocking incompatibilities; nothing was written.",
	} {
		assert.Contains(t, out, want)
	}

	assert.Regexp(t, `Application argocd/valid\s+Application valid\n`, out)

	// Apply mode refuses too.
	_, _, err = runArgoImport(t, "", argoBlockingFixture, "--dry-run=false")
	assert.Equal(t, exitBlocking, ExitCode(err))
}

func TestArgoCDImportInputErrors(t *testing.T) {
	t.Parallel()
	_, _, err := runArgoImport(t, "", filepath.Join(t.TempDir(), "missing.yaml"))
	require.ErrorContains(t, err, "error reading")
	_, _, err = runArgoImport(t, "# nothing\n", "-")
	require.ErrorContains(t, err, "- contains no objects")
	_, _, err = runArgoImport(t, "kind: [", "-")
	require.Error(t, err)
}

func TestArgoKeyToPatrol(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in, want string
		keep     bool
	}{
		{"argocd.argoproj.io/sync-wave", "cd.patrol.example/sync-wave", true},
		{"argocd.argoproj.io/compare-options", "cd.patrol.example/compare-options", true},
		{"argocd.argoproj.io/refresh", "", false},
		{"argocd.argoproj.io/tracking-id", "", false},
		{"notified.notifications.argoproj.io", "", false},
		{"notifications.argoproj.io/subscribe.on-deployed.slack", "notifications.cd.patrol.example/subscribe.on-deployed.slack", true},
		{"on-deployed.slack.recipients.argocd-notifications.argoproj.io", "on-deployed.slack.recipients.notifications.cd.patrol.example", true},
		{"pref.argocd.argoproj.io/default-view", "pref.cd.patrol.example/default-view", true},
		{"app.kubernetes.io/instance", "app.kubernetes.io/instance", true},
		{"kubectl.kubernetes.io/last-applied-configuration", "", false},
		{"example.com/argocd.argoproj.io", "example.com/argocd.argoproj.io", true},
	} {
		got, keep := argoKeyToPatrol(tc.in)
		assert.Equal(t, tc.keep, keep, tc.in)
		if keep {
			assert.Equal(t, tc.want, got, tc.in)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"resources-finalizer.argocd.argoproj.io", "resources-finalizer.cd.patrol.example"},
		{"resources-finalizer.argocd.argoproj.io/foreground", "resources-finalizer.cd.patrol.example/foreground"},
		{"post-delete-finalizer.argocd.argoproj.io/cleanup", "post-delete-finalizer.cd.patrol.example/cleanup"},
		{"example.com/finalizer", ""},
	} {
		assert.Equal(t, tc.want, argoFinalizerEquivalent(tc.in), tc.in)
	}
}

func TestArgoCDImportApply(t *testing.T) {
	ctx := requireEnv(t)
	ns := newNamespace(t, ctx, "argoimport")
	_, err := envKube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	appGVR := kindApplication.gvr(currentGroup)
	_ = envDyn.Resource(appGVR).Namespace("team-a").Delete(ctx, "api", metav1.DeleteOptions{})
	// An existing settings ConfigMap is left unchanged unless --overwrite-settings.
	_, err = envKube.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "patrol-rbac-cm"}, Data: map[string]string{"policy.default": "role:admin", "extra": "kept"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	tio := newTestIO(ns)
	require.NoError(t, tio.run(ctx, "admin", "argocd-import", argoExportFixture, "--dry-run=false"), tio.out.String())
	out := tio.out.String()
	assertNoArgoSecrets(t, out, tio.errOut.String())
	assert.Contains(t, out, "Creating objects in namespace "+ns)
	assert.Contains(t, out, "ConfigMap "+ns+"/patrol-rbac-cm already exists; left unchanged (merge it with --overwrite-settings)")
	assert.Contains(t, out, "Application team-a/api created")
	assert.Contains(t, out, "Result: 29 object(s) created or updated, 1 left unchanged.")

	// Objects passed the CRD schemas; credentials Secrets are owned by their objects.
	for _, name := range []string{"repo-apps", "repo-private", "repo-charts", "repo-hydrated-write"} {
		_, err := envDyn.Resource(kindRepository.gvr(currentGroup)).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err, name)
	}
	repo, err := envDyn.Resource(kindRepository.gvr(currentGroup)).Namespace(ns).Get(ctx, "repo-apps", metav1.GetOptions{})
	require.NoError(t, err)
	creds, err := envKube.CoreV1().Secrets(ns).Get(ctx, "repo-apps-creds", metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, creds.OwnerReferences, 1)
	assert.Equal(t, repo.GetUID(), creds.OwnerReferences[0].UID)
	assert.Equal(t, "Repository", creds.OwnerReferences[0].Kind)
	assert.Equal(t, "fake-repo-password", string(creds.Data["password"]))
	cluster, err := envDyn.Resource(kindCluster.gvr(currentGroup)).Namespace(ns).Get(ctx, "cluster-prod", metav1.GetOptions{})
	require.NoError(t, err)
	clusterCreds, err := envKube.CoreV1().Secrets(ns).Get(ctx, "cluster-prod-creds", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, cluster.GetUID(), clusterCreds.OwnerReferences[0].UID)
	_, err = envDyn.Resource(kindRepositoryCredential.gvr(currentGroup)).Namespace(ns).Get(ctx, "creds-github-app", metav1.GetOptions{})
	require.NoError(t, err)
	for _, name := range []string{"team-a", "default"} {
		_, err := envDyn.Resource(kindAppProject.gvr(currentGroup)).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err, name)
	}
	for _, name := range []string{"cluster-addons", "team-repos"} {
		_, err := envDyn.Resource(kindApplicationSet.gvr(currentGroup)).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err, name)
	}
	guestbook, err := envDyn.Resource(appGVR).Namespace(ns).Get(ctx, "guestbook", metav1.GetOptions{})
	require.NoError(t, err)
	enabled, found, _ := unstructured.NestedBool(guestbook.Object, "spec", "syncPolicy", "automated", "enabled")
	assert.True(t, found)
	assert.False(t, enabled)
	assert.Empty(t, guestbook.GetFinalizers())
	api, err := envDyn.Resource(appGVR).Namespace("team-a").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "team-a", api.GetNamespace())
	t.Cleanup(func() { _ = envDyn.Resource(appGVR).Namespace("team-a").Delete(ctx, "api", metav1.DeleteOptions{}) })
	cm, err := envKube.CoreV1().ConfigMaps(ns).Get(ctx, "patrol-cm", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, cm.Data, "resource.customizations.health.cd.patrol.example_Application")
	sec, err := envKube.CoreV1().Secrets(ns).Get(ctx, "patrol-secret", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, sec.Data, "server.secretkey")

	// A second run changes nothing.
	require.NoError(t, tio.run(ctx, "admin", "argocd-import", argoExportFixture, "--dry-run=false"))
	assert.Contains(t, tio.out.String(), "Result: 0 object(s) created or updated, 30 left unchanged.")

	// --overwrite-settings merges converted keys into existing settings objects.
	require.NoError(t, tio.run(ctx, "admin", "argocd-import", argoExportFixture, "--dry-run=false", "--overwrite-settings"))
	assert.Contains(t, tio.out.String(), "ConfigMap "+ns+"/patrol-rbac-cm updated (3 key(s) merged)")
	rbacCM, err := envKube.CoreV1().ConfigMaps(ns).Get(ctx, "patrol-rbac-cm", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "role:readonly", rbacCM.Data["policy.default"])
	assert.Equal(t, "kept", rbacCM.Data["extra"])
	assertNoArgoSecrets(t, tio.out.String(), tio.errOut.String())
}
