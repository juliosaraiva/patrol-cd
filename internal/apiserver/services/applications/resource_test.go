package applications_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func TestApplicationEvents(t *testing.T) {
	re := newResEnv(t, resEnvOptions{})
	app := re.fixture("guestbook", "proj-a")
	pod := re.objs["Pod/web-abc-1"]
	mkEvent := func(ns, name string, involved corev1.ObjectReference, msg string) {
		require.NoError(t, re.kube.Create(re.ctx, &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, InvolvedObject: involved, Reason: "Test", Message: msg, Type: corev1.EventTypeNormal,
			FirstTimestamp: metav1.Now(), LastTimestamp: metav1.Now(),
		}))
	}
	mkEvent(app.Namespace, "app-ev", corev1.ObjectReference{Kind: "Application", Namespace: app.Namespace, Name: app.Name, UID: app.UID}, "app event")
	mkEvent(app.Namespace, "other-ev", corev1.ObjectReference{Kind: "Application", Namespace: app.Namespace, Name: "other", UID: "x"}, "other app")
	mkEvent(re.appNS, "pod-ev", corev1.ObjectReference{Kind: "Pod", Namespace: re.appNS, Name: pod.GetName(), UID: pod.GetUID()}, "pod event")
	outsider := &corev1.Pod{}
	require.NoError(t, re.kube.Get(re.ctx, client.ObjectKey{Namespace: re.appNS, Name: "outsider"}, outsider))
	mkEvent(re.appNS, "outsider-ev", corev1.ObjectReference{Kind: "Pod", Namespace: re.appNS, Name: "outsider", UID: outsider.UID}, "outsider event")
	reader := re.rclient("reader")

	list, err := reader.ListApplicationEvents(re.ctx, &apiclient.ApplicationEventsQuery{Name: "guestbook"})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	assert.Equal(t, "app event", list.Items[0].Message)

	list, err = reader.ListApplicationEvents(re.ctx, &apiclient.ApplicationEventsQuery{Name: "guestbook",
		ResourceNamespace: re.appNS, ResourceName: pod.GetName(), ResourceUID: string(pod.GetUID())})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	assert.Equal(t, "pod event", list.Items[0].Message)
	assert.NotEmpty(t, re.cluster.seen(func(r seenRequest) bool { return strings.HasSuffix(r.path, "/events") }), "resource events come from the destination cluster")

	// Only resources of the tree, with matching UID.
	_, err = reader.ListApplicationEvents(re.ctx, &apiclient.ApplicationEventsQuery{Name: "guestbook",
		ResourceNamespace: re.appNS, ResourceName: "outsider", ResourceUID: string(outsider.UID)})
	requireProblem(t, err, 400, "outsider not found as part of application guestbook")
	_, err = reader.ListApplicationEvents(re.ctx, &apiclient.ApplicationEventsQuery{Name: "guestbook",
		ResourceNamespace: re.appNS, ResourceName: pod.GetName(), ResourceUID: "forged"})
	requireProblem(t, err, 400, "not found as part of application")
	_, err = re.rclient("bob").ListApplicationEvents(re.ctx, &apiclient.ApplicationEventsQuery{Name: "guestbook"})
	requireProblem(t, err, 403, "permission denied")
}

func TestGetResource(t *testing.T) {
	re := newResEnv(t, resEnvOptions{cm: map[string]string{"resource.sensitive.mask.annotations": "token"}})
	re.fixture("guestbook", "proj-a")
	reader := re.rclient("reader")
	ns := re.appNS

	res, err := reader.GetApplicationResource(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook", Group: "apps", Kind: "Deployment", Namespace: ns, ResourceName: "web"})
	require.NoError(t, err)
	var dep appsv1.Deployment
	require.NoError(t, json.Unmarshal([]byte(res.Manifest), &dep))
	assert.Equal(t, "web", dep.Name)
	assert.Equal(t, int32(2), *dep.Spec.Replicas)

	// Secrets are masked (data and sensitive annotations).
	res, err = reader.GetApplicationResource(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook", Kind: "Secret", Namespace: ns, ResourceName: "creds"})
	require.NoError(t, err)
	assert.NotContains(t, res.Manifest, "aHVudGVyMg==")
	assert.NotContains(t, res.Manifest, "t0p-secret")
	assert.Contains(t, res.Manifest, `"password":"++++++++"`)

	// Orphaned resources of the tree are readable; other objects are not.
	_, err = reader.GetApplicationResource(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook", Kind: "ConfigMap", Namespace: ns, ResourceName: "stray"})
	require.NoError(t, err)
	_, err = reader.GetApplicationResource(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook", Kind: "Pod", Namespace: ns, ResourceName: "outsider"})
	requireProblem(t, err, 400, "Pod  outsider not found as part of application guestbook")
	_, err = reader.GetApplicationResource(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook", Kind: "Secret", Namespace: "kube-system", ResourceName: "creds"})
	requireProblem(t, err, 400, "not found as part of application")
	_, err = reader.GetApplicationResource(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook"})
	requireProblem(t, err, 400, "kind and resourceName are required")
	_, err = re.rclient("bob").GetApplicationResource(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook", Kind: "Secret", Namespace: ns, ResourceName: "creds"})
	requireProblem(t, err, 403, "permission denied")
	// A version that the cluster does not serve.
	_, err = reader.GetApplicationResource(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook", Group: "apps", Version: "v9", Kind: "Deployment", Namespace: ns, ResourceName: "web"})
	requireProblem(t, err, 404, "error getting resource")

	// No impersonation by default.
	for _, r := range re.cluster.seen(func(r seenRequest) bool { return r.method == "GET" }) {
		assert.Empty(t, r.impersonate, r.path)
	}

	// Deep links of a managed resource use the same live read (LiveResources adapter).
	re.holder.setCM("resource.links", "- url: https://logs.example.com/{{ .resource.metadata.name }}/{{ .resource.data.password }}\n  title: Logs\n  if: resource.kind == \"Secret\"")
	links, err := reader.ListApplicationResourceLinks(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook", Kind: "Secret", Namespace: ns, ResourceName: "creds"})
	require.NoError(t, err)
	require.Len(t, links.Items, 1)
	assert.Equal(t, "https://logs.example.com/creds/++++++++", links.Items[0].URL)
	_, err = reader.ListApplicationResourceLinks(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook", Kind: "Pod", Namespace: ns, ResourceName: "outsider"})
	requireProblem(t, err, 400, "not found as part of application guestbook")
}

func TestPatchResource(t *testing.T) {
	re := newResEnv(t, resEnvOptions{})
	re.fixture("guestbook", "proj-a")
	ns := re.appNS
	patchDep := func(c *apiclient.Client, replicas int) (*apiclient.ApplicationResourceResponse, error) {
		return c.PatchApplicationResource(re.ctx, &apiclient.ApplicationResourcePatchRequest{
			Name: "guestbook", Group: "apps", Kind: "Deployment", Namespace: ns, ResourceName: "web",
			Patch: `{"spec":{"replicas":` + string(rune('0'+replicas)) + `}}`, PatchType: apiclient.PatchTypeMerge,
		})
	}

	// Fine-grained update/apps/Deployment/... allows patching the Deployment.
	res, err := patchDep(re.rclient("patcher"), 3)
	require.NoError(t, err)
	assert.Contains(t, res.Manifest, `"replicas":3`)
	dep := &appsv1.Deployment{}
	require.NoError(t, re.kube.Get(re.ctx, client.ObjectKey{Namespace: ns, Name: "web"}, dep))
	assert.Equal(t, int32(3), *dep.Spec.Replicas)
	assert.Contains(t, strings.Join(re.events(), "\n"), "patched resource apps/Deployment 'web'")

	// The same user may not patch other kinds; plain update does not cover resources by
	// default (v3), a reader may not patch at all, another tenant gets the hiding answer.
	_, err = re.rclient("patcher").PatchApplicationResource(re.ctx, &apiclient.ApplicationResourcePatchRequest{
		Name: "guestbook", Kind: "ConfigMap", Namespace: ns, ResourceName: "cfg", Patch: `{"data":{"k":"x"}}`})
	requireProblem(t, err, 403, "permission denied")
	_, err = patchDep(re.rclient("updater"), 4)
	requireProblem(t, err, 403, "permission denied")
	_, err = patchDep(re.rclient("reader"), 4)
	requireProblem(t, err, 403, "permission denied")
	_, err = patchDep(re.rclient("bob"), 4)
	requireProblem(t, err, 403, "permission denied")
	var deniedActions []string
	for _, e := range re.audit.byOperation("patchResource") {
		if e.Result == rest.AuditDenied {
			deniedActions = append(deniedActions, e.Action)
		}
	}
	assert.Contains(t, deniedActions, "update/apps/Deployment/"+ns+"/web")

	// server.rbac.disableApplicationFineGrainedRBACInheritance=false: plain update suffices.
	re.holder.setCM("server.rbac.disableApplicationFineGrainedRBACInheritance", "false")
	_, err = patchDep(re.rclient("updater"), 4)
	require.NoError(t, err)
	_, err = patchDep(re.rclient("patcher"), 5)
	require.NoError(t, err, "the fine-grained permission still works")

	// Resources outside the tree are refused before any cluster call.
	before := len(re.cluster.seen(func(r seenRequest) bool { return r.method == "PATCH" }))
	_, err = re.rclient("alice").PatchApplicationResource(re.ctx, &apiclient.ApplicationResourcePatchRequest{
		Name: "guestbook", Kind: "Pod", Namespace: ns, ResourceName: "outsider", Patch: `{"metadata":{"labels":{"x":"y"}}}`})
	requireProblem(t, err, 400, "Pod  outsider not found as part of application guestbook")
	assert.Len(t, re.cluster.seen(func(r seenRequest) bool { return r.method == "PATCH" }), before)

	// Invalid input.
	_, err = re.rclient("alice").PatchApplicationResource(re.ctx, &apiclient.ApplicationResourcePatchRequest{
		Name: "guestbook", Group: "apps", Kind: "Deployment", Namespace: ns, ResourceName: "web", Patch: `not json`})
	requireProblem(t, err, 400, "patch must be a JSON document")
	_, err = re.rclient("alice").PatchApplicationResource(re.ctx, &apiclient.ApplicationResourcePatchRequest{
		Name: "guestbook", Group: "apps", Kind: "Deployment", Namespace: ns, ResourceName: "web", Patch: `{}`, PatchType: "apply"})
	requireProblem(t, err, 400, "unsupported patch type")

	// JSON patch, and the masked result of a Secret patch.
	_, err = re.rclient("alice").PatchApplicationResource(re.ctx, &apiclient.ApplicationResourcePatchRequest{
		Name: "guestbook", Group: "apps", Kind: "Deployment", Namespace: ns, ResourceName: "web",
		Patch: `[{"op":"replace","path":"/spec/replicas","value":1}]`, PatchType: apiclient.PatchTypeJSON})
	require.NoError(t, err)
	res, err = re.rclient("alice").PatchApplicationResource(re.ctx, &apiclient.ApplicationResourcePatchRequest{
		Name: "guestbook", Kind: "Secret", Namespace: ns, ResourceName: "creds", Patch: `{"stringData":{"password":"n3w-s3cret"}}`})
	require.NoError(t, err)
	assert.NotContains(t, res.Manifest, "n3w-s3cret")
	assert.NotContains(t, res.Manifest, "bjN3LXMzY3JldA==")
	// A failing Secret patch reveals nothing of the request.
	_, err = re.rclient("alice").PatchApplicationResource(re.ctx, &apiclient.ApplicationResourcePatchRequest{
		Name: "guestbook", Kind: "Secret", Namespace: ns, ResourceName: "creds", Patch: `{"data":{"password":"not base64 l34k"}}`})
	ae := apiErr(t, err)
	assert.Equal(t, 422, ae.StatusCode)
	assert.Equal(t, "failed to patch Secret "+ns+"/creds", ae.Problem.Detail)
}

func TestDeleteResource(t *testing.T) {
	re := newResEnv(t, resEnvOptions{})
	re.fixture("guestbook", "proj-a")
	ns := re.appNS
	del := func(user, group, kind, name string, force, orphan *bool) error {
		return re.rclient(user).DeleteApplicationResource(re.ctx, &apiclient.ApplicationResourceDeleteRequest{
			Name: "guestbook", Group: group, Kind: kind, Namespace: ns, ResourceName: name, Force: force, Orphan: orphan})
	}
	// delete/*/Pod/*/* allows Pods only.
	requireProblem(t, del("podkiller", "apps", "Deployment", "web", nil, nil), 403, "permission denied")
	requireProblem(t, del("reader", "", "Pod", "web-abc-2", nil, nil), 403, "permission denied")
	requireProblem(t, del("podkiller", "", "Pod", "outsider", nil, nil), 400, "not found as part of application")
	require.NoError(t, del("podkiller", "", "Pod", "web-abc-2", ptrBool(true), nil))
	err := re.kube.Get(re.ctx, client.ObjectKey{Namespace: ns, Name: "web-abc-2"}, &corev1.Pod{})
	assert.True(t, apierrors.IsNotFound(err), "the unscheduled pod is gone: %v", err)
	assert.Contains(t, strings.Join(re.events(), "\n"), "deleted resource /Pod 'web-abc-2'")

	// Propagation: force → Background with grace 0; orphan wins; default Foreground.
	require.NoError(t, del("alice", "", "ConfigMap", "stray", nil, ptrBool(true)))
	require.NoError(t, del("alice", "", "ConfigMap", "cfg", nil, nil))
	deletes := re.cluster.seen(func(r seenRequest) bool { return r.method == "DELETE" })
	require.Len(t, deletes, 3)
	assert.Contains(t, deletes[0].body, `"propagationPolicy":"Background"`)
	assert.Contains(t, deletes[0].body, `"gracePeriodSeconds":0`)
	assert.Contains(t, deletes[1].body, `"propagationPolicy":"Orphan"`)
	assert.Contains(t, deletes[2].body, `"propagationPolicy":"Foreground"`)
}

// API-server live operations impersonate the project's destination service account when
// application.sync.impersonation.enabled (upstream getApplicationClusterConfig).
func TestResourceImpersonation(t *testing.T) {
	re := newResEnv(t, resEnvOptions{cm: map[string]string{"application.sync.impersonation.enabled": "true"}})
	re.fixture("guestbook", "proj-a")
	ns := re.appNS
	alice := re.rclient("alice")
	get := func() error {
		_, err := alice.GetApplicationResource(re.ctx, &apiclient.ApplicationResourceRequest{Name: "guestbook", Group: "apps", Kind: "Deployment", Namespace: ns, ResourceName: "web"})
		return err
	}

	// Enforced (default) and no matching service account: refused, nothing sent.
	before := len(re.cluster.seen(func(seenRequest) bool { return true }))
	requireProblem(t, get(), 412, "error getting application cluster config: no matching service account found for destination server "+inCluster+" and namespace "+ns)
	assert.Len(t, re.cluster.seen(func(seenRequest) bool { return true }), before)
	// Application events never need the destination cluster.
	_, err := alice.ListApplicationEvents(re.ctx, &apiclient.ApplicationEventsQuery{Name: "guestbook"})
	require.NoError(t, err)

	// A matching entry: the service account is impersonated (user only) and Kubernetes RBAC
	// applies to it.
	re.updateProject("proj-a", func(p *v1alpha1.AppProject) {
		p.Spec.DestinationServiceAccounts = []v1alpha1.ApplicationDestinationServiceAccount{{Server: inCluster, Namespace: ns, DefaultServiceAccount: "deployer"}}
	})
	sa := "system:serviceaccount:" + ns + ":deployer"
	err = get()
	requireProblem(t, err, 403, "error getting resource: ")
	assert.Contains(t, apiErr(t, err).Problem.Detail, sa)
	re.grantServiceAccount("deployer", rbacv1.PolicyRule{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"get", "patch"}})
	eventually(t, func() bool { return get() == nil }, "the impersonated service account never got access")
	gets := re.cluster.seen(func(r seenRequest) bool {
		return r.method == "GET" && strings.Contains(r.path, "/deployments/web")
	})
	require.NotEmpty(t, gets)
	last := gets[len(gets)-1]
	assert.Equal(t, sa, last.impersonate)
	assert.False(t, last.impersonateGroup, "only the user is impersonated")
	// A patch is impersonated too (fine-grained RBAC passed first).
	_, err = alice.PatchApplicationResource(re.ctx, &apiclient.ApplicationResourcePatchRequest{
		Name: "guestbook", Group: "apps", Kind: "Deployment", Namespace: ns, ResourceName: "web", Patch: `{"spec":{"replicas":1}}`})
	require.NoError(t, err)
	// The service account may not delete: the Kubernetes denial is reported.
	err = alice.DeleteApplicationResource(re.ctx, &apiclient.ApplicationResourceDeleteRequest{Name: "guestbook", Group: "apps", Kind: "Deployment", Namespace: ns, ResourceName: "web"})
	requireProblem(t, err, 403, "error deleting resource: ")

	// Not enforced and no match: the cluster credentials are used.
	re.updateProject("proj-a", func(p *v1alpha1.AppProject) { p.Spec.DestinationServiceAccounts = nil })
	re.holder.setCM("application.sync.impersonation.enforced", "false")
	require.NoError(t, get())
	gets = re.cluster.seen(func(r seenRequest) bool { return r.method == "GET" && strings.Contains(r.path, "/deployments/web") })
	assert.Empty(t, gets[len(gets)-1].impersonate)

	// Disabled again without a restart.
	re.holder.setCM("application.sync.impersonation.enforced", "true")
	re.holder.setCM("application.sync.impersonation.enabled", "false")
	require.NoError(t, get())
}
