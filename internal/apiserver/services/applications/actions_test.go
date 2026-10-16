package applications_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func TestListResourceActions(t *testing.T) {
	re := newResEnv(t, resEnvOptions{})
	re.fixture("guestbook", "proj-a")
	reader := re.rclient("reader")

	res, err := reader.ListApplicationResourceActions(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook", Group: "apps", Kind: "Deployment", Namespace: re.appNS, ResourceName: "web"})
	require.NoError(t, err)
	byName := map[string]apiclient.ResourceAction{}
	for _, a := range res.Actions {
		byName[a.Name] = a
	}
	require.Contains(t, byName, "restart")
	assert.False(t, byName["pause"].Disabled)
	assert.True(t, byName["resume"].Disabled, "not paused")
	assert.Equal(t, []apiclient.ResourceActionParam{{Name: "replicas"}}, byName["scale"].Params)

	// The Application itself.
	res, err = reader.ListApplicationResourceActions(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook", Group: v1alpha1.GroupName, Kind: "Application", ResourceName: "guestbook"})
	require.NoError(t, err)
	require.Len(t, res.Actions, 1)
	assert.Equal(t, "toggle-auto-sync", res.Actions[0].Name)
	assert.Equal(t, "Enable Auto-Sync", res.Actions[0].DisplayName)

	// A kind without discovery has no actions; resources outside the tree are refused.
	res, err = reader.ListApplicationResourceActions(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook", Kind: "ConfigMap", Namespace: re.appNS, ResourceName: "cfg"})
	require.NoError(t, err)
	assert.Empty(t, res.Actions)
	_, err = reader.ListApplicationResourceActions(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook", Kind: "Pod", Namespace: re.appNS, ResourceName: "outsider"})
	requireProblem(t, err, 400, "not found as part of application guestbook")
	_, err = re.rclient("bob").ListApplicationResourceActions(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook", Group: "apps", Kind: "Deployment", Namespace: re.appNS, ResourceName: "web"})
	requireProblem(t, err, 403, "permission denied")

	// Custom actions replace the built-in ones unless mergeBuiltinActions.
	re.holder.setCM("resource.customizations.actions.apps_Deployment", `discovery.lua: |
  return {["say-hi"] = {}}
definitions:
- name: say-hi
  action.lua: |
    obj.metadata.labels = obj.metadata.labels or {}
    obj.metadata.labels["hi"] = "there"
    return obj
`)
	res, err = reader.ListApplicationResourceActions(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook", Group: "apps", Kind: "Deployment", Namespace: re.appNS, ResourceName: "web"})
	require.NoError(t, err)
	require.Len(t, res.Actions, 1)
	assert.Equal(t, "say-hi", res.Actions[0].Name)
}

func TestRunResourceAction(t *testing.T) {
	re := newResEnv(t, resEnvOptions{})
	re.fixture("guestbook", "proj-a")
	ns := re.appNS
	run := func(user, group, kind, name, action string, params ...apiclient.ResourceActionParameter) error {
		return re.rclient(user).RunApplicationResourceAction(re.ctx, &apiclient.ResourceActionRunRequest{
			Name: "guestbook", Group: group, Kind: kind, Namespace: ns, ResourceName: name, Action: action, ResourceActionParameters: params})
	}
	dep := func() *appsv1.Deployment {
		d := &appsv1.Deployment{}
		require.NoError(t, re.kube.Get(re.ctx, client.ObjectKey{Namespace: ns, Name: "web"}, d))
		return d
	}

	// action/apps/Deployment/* allows restart.
	require.NoError(t, run("actor", "apps", "Deployment", "web", "restart"))
	assert.NotEmpty(t, dep().Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"])
	assert.Contains(t, strings.Join(re.events(), "\n"), "ran action restart on resource apps/Deployment/web")

	// Parameters: declared ones are passed, others refused; scripts validate values.
	require.NoError(t, run("actor", "apps", "Deployment", "web", "scale", apiclient.ResourceActionParameter{Name: "replicas", Value: "5"}))
	assert.Equal(t, int32(5), *dep().Spec.Replicas)
	requireProblem(t, run("actor", "apps", "Deployment", "web", "scale", apiclient.ResourceActionParameter{Name: "replicas", Value: "abc"}), 400, "invalid number: abc")
	requireProblem(t, run("actor", "apps", "Deployment", "web", "restart", apiclient.ResourceActionParameter{Name: "x", Value: "1"}), 400, `unknown parameter "x" for action "restart"`)
	// A disabled action is refused.
	requireProblem(t, run("actor", "apps", "Deployment", "web", "resume"), 412, `action "resume" is disabled`)
	requireProblem(t, run("actor", "apps", "Deployment", "web", "self-destruct"), 400, "error getting Lua resource action")

	// RBAC: other kinds, readers, other tenants.
	requireProblem(t, run("actor", "batch", "CronJob", "nightly", "create-job"), 403, "permission denied")
	requireProblem(t, run("reader", "apps", "Deployment", "web", "restart"), 403, "permission denied")
	requireProblem(t, run("bob", "apps", "Deployment", "web", "restart"), 403, "permission denied")
	var deniedActions []string
	for _, e := range re.audit.byOperation("runResourceAction") {
		if e.Result == rest.AuditDenied {
			deniedActions = append(deniedActions, e.Action)
		}
	}
	assert.Contains(t, deniedActions, "action/batch/CronJob/create-job")
	requireProblem(t, run("actor", "apps", "Deployment", "missing", "restart"), 400, "not found as part of application")
}

// Create-type actions dry-run every creation after the project check; a refused creation
// changes nothing (SYNC-103, SYNC-112).
func TestRunCreateAction(t *testing.T) {
	re := newResEnv(t, resEnvOptions{})
	re.fixture("guestbook", "proj-a")
	ns := re.appNS
	jobs := func() []batchv1.Job {
		l := &batchv1.JobList{}
		require.NoError(t, re.kube.List(re.ctx, l, client.InNamespace(ns)))
		return l.Items
	}
	run := func() error {
		return re.rclient("alice").RunApplicationResourceAction(re.ctx, &apiclient.ResourceActionRunRequest{
			Name: "guestbook", Group: "batch", Kind: "CronJob", Namespace: ns, ResourceName: "nightly", Action: "create-job"})
	}

	re.updateProject("proj-a", func(p *v1alpha1.AppProject) {
		p.Spec.NamespaceResourceBlacklist = []metav1.GroupKind{{Group: "batch", Kind: "Job"}}
	})
	err := run()
	requireProblem(t, err, 403, "application is not permitted to manage batch/Job/nightly-")
	assert.Empty(t, jobs())
	assert.Empty(t, re.cluster.seen(func(r seenRequest) bool { return r.method == "POST" && strings.HasSuffix(r.path, "/jobs") }), "nothing sent")

	re.updateProject("proj-a", func(p *v1alpha1.AppProject) { p.Spec.NamespaceResourceBlacklist = nil })
	require.NoError(t, run())
	js := jobs()
	require.Len(t, js, 1)
	assert.Equal(t, "manual", js[0].Annotations["cronjob.kubernetes.io/instantiate"])
	posts := re.cluster.seen(func(r seenRequest) bool { return r.method == "POST" && strings.HasSuffix(r.path, "/jobs") })
	require.Len(t, posts, 2)
	assert.Contains(t, posts[0].query, "dryRun=All", "the creation is dry-run first")
	assert.NotContains(t, posts[1].query, "dryRun")
}

// Under impersonation the impersonated service account needs the verbs of the action
// (SYNC-121): a dry-run creation it may not do changes nothing.
func TestRunActionImpersonated(t *testing.T) {
	re := newResEnv(t, resEnvOptions{cm: map[string]string{"application.sync.impersonation.enabled": "true"}})
	re.fixture("guestbook", "proj-a")
	ns := re.appNS
	re.updateProject("proj-a", func(p *v1alpha1.AppProject) {
		p.Spec.DestinationServiceAccounts = []v1alpha1.ApplicationDestinationServiceAccount{{Server: inCluster, Namespace: "*", DefaultServiceAccount: "runner"}}
	})
	re.grantServiceAccount("runner", rbacv1.PolicyRule{APIGroups: []string{"batch"}, Resources: []string{"cronjobs"}, Verbs: []string{"get"}})
	run := func() error {
		return re.rclient("alice").RunApplicationResourceAction(re.ctx, &apiclient.ResourceActionRunRequest{
			Name: "guestbook", Group: "batch", Kind: "CronJob", Namespace: ns, ResourceName: "nightly", Action: "create-job"})
	}
	var err error
	eventually(t, func() bool {
		err = run()
		return err != nil && strings.Contains(err.Error(), "error creating resource (dry run)")
	}, "expected the dry-run creation to be forbidden")
	requireProblem(t, err, 403, "jobs.batch is forbidden")
	l := &batchv1.JobList{}
	require.NoError(t, re.kube.List(re.ctx, l, client.InNamespace(ns)))
	assert.Empty(t, l.Items)
	re.grantServiceAccount("runner", rbacv1.PolicyRule{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"create"}})
	eventually(t, func() bool { return run() == nil }, "the service account never got create on jobs")
	for _, r := range re.cluster.seen(func(r seenRequest) bool {
		return strings.Contains(r.path, "/cronjobs/") || strings.HasSuffix(r.path, "/jobs")
	}) {
		assert.Equal(t, "system:serviceaccount:"+ns+":runner", r.impersonate, r.method+" "+r.path)
	}
}

// toggle-auto-sync on the Application itself (SYNC-122): the Application is patched in the
// control plane; the project cannot be changed by an action.
func TestApplicationSelfAction(t *testing.T) {
	re := newResEnv(t, resEnvOptions{})
	app := re.fixture("guestbook", "proj-a")
	run := func(user string) error {
		return re.rclient(user).RunApplicationResourceAction(re.ctx, &apiclient.ResourceActionRunRequest{
			Name: "guestbook", Group: v1alpha1.GroupName, Kind: "Application", ResourceName: "guestbook", Action: "toggle-auto-sync"})
	}
	require.NoError(t, run("alice"))
	cur := re.getApp(app.Namespace, app.Name)
	require.NotNil(t, cur.Spec.SyncPolicy)
	require.NotNil(t, cur.Spec.SyncPolicy.Automated)
	assert.True(t, *cur.Spec.SyncPolicy.Automated.Enabled)
	assert.Contains(t, strings.Join(re.events(), "\n"), "ran action toggle-auto-sync")
	require.NoError(t, run("alice"))
	assert.False(t, *re.getApp(app.Namespace, app.Name).Spec.SyncPolicy.Automated.Enabled)
	requireProblem(t, run("actor"), 403, "permission denied")

	// A custom action may not move the Application to another project.
	re.holder.setCM("resource.customizations.actions."+v1alpha1.GroupName+"_Application", `discovery.lua: |
  return {["move"] = {}}
definitions:
- name: move
  action.lua: |
    obj.spec.project = "proj-b"
    return obj
`)
	err := re.rclient("alice").RunApplicationResourceAction(re.ctx, &apiclient.ResourceActionRunRequest{
		Name: "guestbook", Group: v1alpha1.GroupName, Kind: "Application", ResourceName: "guestbook", Action: "move"})
	requireProblem(t, err, 403, "cannot change the project")
	assert.Equal(t, "proj-a", re.getApp(app.Namespace, app.Name).Spec.Project)
}
