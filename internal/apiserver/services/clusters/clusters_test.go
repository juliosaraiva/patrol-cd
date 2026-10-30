package clusters_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/clusters"
	clusterlib "github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

const (
	goodToken = "good-bearer-token-0123456789"
	inCluster = v1alpha1.KubernetesInternalAPIServerAddr
)

// input returns a cluster create request for the fake cluster authenticated with tok.
func input(f *fakeCluster, name, project, tok string) *apiclient.ClusterInput {
	return &apiclient.ClusterInput{
		Server: f.srv.URL, Name: name, Project: project,
		Labels: map[string]string{"env": "test"},
		Config: apiclient.ClusterConfigInput{BearerToken: tok, TLSClientConfig: apiclient.ClusterTLSClientConfigInput{CAData: f.caPEM}},
	}
}

func rawGet(t *testing.T, c *apiclient.Client, path string, req any) string {
	t.Helper()
	resp, err := c.DoRaw(context.Background(), http.MethodGet, path, req, nil, "")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

// TestClusterLifecycle covers REPO-098..102/104/096 through pkg/apiclient: create with a
// connection check and write-only credentials, idempotent create, upsert, list/get with
// sanitization and per-item RBAC, partial and full updates, cache invalidation and delete.
func TestClusterLifecycle(t *testing.T) {
	e := newEnv(t, envOptions{})
	f := newFakeCluster(t, e.cfg, e.clientset, "")
	f.addToken(goodToken)
	ctx := context.Background()
	admin := e.client("admin")

	created, err := admin.CreateCluster(ctx, input(f, "prod", "", goodToken), false)
	require.NoError(t, err)
	assert.Equal(t, f.srv.URL, created.Spec.Server)
	assert.Equal(t, "prod", created.Spec.Name)
	assert.Equal(t, apiclient.ClusterCredentialsInfo{HasBearerToken: true, HasCAData: true}, created.Credentials)
	assert.False(t, created.InCluster)
	require.NotNil(t, created.Status.ConnectionState, "the response carries the recorded connection state")
	assert.Equal(t, v1alpha1.ConnectionStatusSuccessful, created.Status.ConnectionState.Status)

	// The object holds no credentials; the Secret does, owned by the Cluster.
	obj := e.clusterObject(f.srv.URL)
	require.NotNil(t, obj)
	name, err := clusterlib.ObjectNameForServer(f.srv.URL)
	require.NoError(t, err)
	assert.Equal(t, name, obj.Name, "upstream naming")
	require.NotNil(t, obj.Spec.Config.CredentialsRef)
	sec := e.secret(obj.Spec.Config.CredentialsRef.Name)
	require.NotNil(t, sec)
	assert.Equal(t, goodToken, string(sec.Data[v1alpha1.ClusterSecretKeyBearerToken]))
	assert.Equal(t, v1alpha1.LabelValueSecretTypeCluster, sec.Labels[v1alpha1.LabelKeySecretType])
	assert.Equal(t, v1alpha1.AnnotationValueManagedByPatrol, sec.Annotations[v1alpha1.AnnotationKeyManagedBy])
	require.Len(t, sec.OwnerReferences, 1)
	assert.Equal(t, obj.UID, sec.OwnerReferences[0].UID)
	// The connection check is recorded in status (REPO-094).
	require.NotNil(t, obj.Status.ConnectionState)
	assert.Equal(t, v1alpha1.ConnectionStatusSuccessful, obj.Status.ConnectionState.Status)
	assert.True(t, strings.HasPrefix(obj.Status.ServerVersion, "v1."), obj.Status.ServerVersion)

	// No credential value appears in any response.
	for _, body := range []string{
		rawGet(t, admin, apiclient.PathClusters, nil),
		rawGet(t, admin, apiclient.PathCluster, &apiclient.ClusterIDRequest{Server: f.srv.URL}),
	} {
		assert.NotContains(t, body, goodToken)
		assert.NotContains(t, body, string(f.caPEM[30:60]))
	}

	// Identical create is idempotent; a different one is refused unless upsert.
	again, err := admin.CreateCluster(ctx, input(f, "prod", "", goodToken), false)
	require.NoError(t, err)
	assert.Equal(t, obj.UID, again.Metadata.UID)
	diff := input(f, "renamed", "", goodToken)
	ae := apiErr(t, func() error { _, err := admin.CreateCluster(ctx, diff, false); return err }())
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Equal(t, `existing cluster spec is different; use upsert flag to force update; difference in keys "Name"`, ae.Problem.Detail)
	upserted, err := admin.CreateCluster(ctx, diff, true)
	require.NoError(t, err)
	assert.Equal(t, "renamed", upserted.Spec.Name)
	assert.Equal(t, obj.UID, upserted.Metadata.UID)

	// Get by server, name and escaped name (API-009).
	got, err := admin.GetCluster(ctx, apiclient.ClusterByServer(f.srv.URL+"/"))
	require.NoError(t, err)
	assert.Equal(t, "renamed", got.Spec.Name)
	got, err = admin.GetCluster(ctx, apiclient.ClusterByName("renamed"))
	require.NoError(t, err)
	assert.Equal(t, f.srv.URL, got.Spec.Server)
	got, err = admin.GetCluster(ctx, apiclient.ClusterID{Value: url.QueryEscape("renamed"), Type: apiclient.ClusterIDTypeNameEscaped})
	require.NoError(t, err)
	assert.Equal(t, f.srv.URL, got.Spec.Server)
	_, err = admin.GetCluster(ctx, apiclient.ClusterID{Value: "x", Type: "bogus"})
	assert.Equal(t, http.StatusBadRequest, apiErr(t, err).StatusCode)

	// List includes the implicit in-cluster cluster (REPO-092) and filters.
	list, err := admin.ListClusters(ctx, nil)
	require.NoError(t, err)
	require.Len(t, list.Items, 2)
	assert.Equal(t, inCluster, list.Items[1].Spec.Server)
	assert.True(t, list.Items[1].InCluster)
	assert.Equal(t, "in-cluster", list.Items[1].Spec.Name)
	assert.NotEmpty(t, list.Metadata.ResourceVersion)
	byName, err := admin.ListClusters(ctx, &apiclient.ListClustersRequest{Name: "in-cluster"})
	require.NoError(t, err)
	require.Len(t, byName.Items, 1)
	assert.Equal(t, inCluster, byName.Items[0].Spec.Server)
	sel, err := admin.ListClusters(ctx, &apiclient.ListClustersRequest{Selector: "env=test"})
	require.NoError(t, err)
	require.Len(t, sel.Items, 1)
	page, err := admin.ListClusters(ctx, &apiclient.ListClustersRequest{Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.NotEmpty(t, page.Metadata.Continue)
	page2, err := admin.ListClusters(ctx, &apiclient.ListClustersRequest{Limit: 1, Continue: page.Metadata.Continue})
	require.NoError(t, err)
	require.Len(t, page2.Items, 1)
	assert.Equal(t, inCluster, page2.Items[0].Spec.Server)
	assert.Empty(t, page2.Metadata.Continue)

	// Partial update: namespaces only; credentials untouched.
	upd, err := admin.UpdateCluster(ctx, apiclient.ClusterByServer(f.srv.URL), &apiclient.ClusterInput{Namespaces: []string{"apps", " ", "web"}, Name: "ignored"}, "namespaces")
	require.NoError(t, err)
	assert.Equal(t, []string{"apps", "web"}, upd.Spec.Namespaces)
	assert.Equal(t, "renamed", upd.Spec.Name)
	assert.True(t, upd.Credentials.HasBearerToken)
	assert.Equal(t, goodToken, string(e.secret(obj.Spec.Config.CredentialsRef.Name).Data[v1alpha1.ClusterSecretKeyBearerToken]))

	// The server is immutable; a stale resourceVersion is a conflict.
	_, err = admin.UpdateCluster(ctx, apiclient.ClusterByServer(f.srv.URL), &apiclient.ClusterInput{Server: "https://other.example.com"}, "name")
	ae = apiErr(t, err)
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Contains(t, ae.Problem.Detail, "server is immutable")
	_, err = admin.UpdateCluster(ctx, apiclient.ClusterByServer(f.srv.URL), &apiclient.ClusterInput{Name: "x", ResourceVersion: "1"}, "name")
	assert.Equal(t, http.StatusConflict, apiErr(t, err).StatusCode)

	// A status write moves the resourceVersion, not what a partial update changes: without one
	// (the UI) the update goes through and the status stays.
	read := e.clusterObject(f.srv.URL)
	read.Status.Info.ApplicationsCount = 7
	require.NoError(t, e.kube.Status().Update(ctx, read))
	upd, err = admin.UpdateCluster(ctx, apiclient.ClusterByServer(f.srv.URL), &apiclient.ClusterInput{Namespaces: []string{"apps", "web", "ledger"}}, "namespaces")
	require.NoError(t, err)
	assert.Equal(t, []string{"apps", "web", "ledger"}, upd.Spec.Namespaces)
	assert.Equal(t, int64(7), upd.Status.Info.ApplicationsCount)

	// A config update replaces the credentials (unknown Secret keys dropped, REPO-104), and
	// the connection is checked with the new ones.
	_, err = admin.UpdateCluster(ctx, apiclient.ClusterByServer(f.srv.URL), input(f, "", "", "wrong-token"), "config")
	ae = apiErr(t, err)
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Contains(t, ae.Problem.Detail, "error getting server version: ")
	f.setBasic("operator", "s3cret")
	basic := input(f, "", "", "")
	basic.Config.Username, basic.Config.Password = "operator", "s3cret"
	upd, err = admin.UpdateCluster(ctx, apiclient.ClusterByServer(f.srv.URL), basic, "config")
	require.NoError(t, err)
	assert.Equal(t, apiclient.ClusterCredentialsInfo{HasBasicAuth: true, HasCAData: true}, upd.Credentials)
	sec = e.secret(obj.Spec.Config.CredentialsRef.Name)
	assert.NotContains(t, sec.Data, v1alpha1.ClusterSecretKeyBearerToken)
	assert.Equal(t, "s3cret", string(sec.Data[v1alpha1.ClusterSecretKeyPassword]))

	// Full replacement without updatedFields.
	full := input(f, "full", "", goodToken)
	full.Shard = new(int64(1))
	upd, err = admin.UpdateCluster(ctx, apiclient.ClusterByName("renamed"), full)
	require.NoError(t, err)
	assert.Equal(t, "full", upd.Spec.Name)
	assert.Empty(t, upd.Spec.Namespaces, "full replace")
	assert.Equal(t, int64(1), *upd.Spec.Shard)
	assert.True(t, upd.Credentials.HasBearerToken)

	// Cache invalidation sets the refresh annotation (REPO-096).
	inv, err := admin.InvalidateClusterCache(ctx, apiclient.ClusterByServer(f.srv.URL))
	require.NoError(t, err)
	ts, err := time.Parse(time.RFC3339, inv.Metadata.Annotations[v1alpha1.AnnotationKeyRefresh])
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), ts, time.Minute)
	// A later update keeps the pending invalidation.
	upd, err = admin.UpdateCluster(ctx, apiclient.ClusterByServer(f.srv.URL), &apiclient.ClusterInput{Annotations: map[string]string{"a": "b"}}, "annotations")
	require.NoError(t, err)
	assert.Equal(t, "b", upd.Metadata.Annotations["a"])
	assert.Contains(t, upd.Metadata.Annotations, v1alpha1.AnnotationKeyRefresh)

	// Audit records the target of mutations.
	creates := e.audit.byOperation("createCluster")
	require.NotEmpty(t, creates)
	assert.Equal(t, "Cluster", creates[0].Target.Kind)
	assert.Equal(t, "clusters", creates[0].Resource)

	// Delete removes the object and the managed Secret.
	require.NoError(t, admin.DeleteCluster(ctx, apiclient.ClusterByServer(f.srv.URL)))
	assert.Nil(t, e.clusterObject(f.srv.URL))
	assert.Nil(t, e.secret(obj.Spec.Config.CredentialsRef.Name))
	_, err = admin.GetCluster(ctx, apiclient.ClusterByServer(f.srv.URL))
	ae = apiErr(t, err)
	assert.Equal(t, http.StatusForbidden, ae.StatusCode)
	assert.Equal(t, "permission denied", ae.Problem.Detail)
	assert.Equal(t, http.StatusForbidden, apiErr(t, admin.DeleteCluster(ctx, apiclient.ClusterByServer(f.srv.URL))).StatusCode)

	// The implicit in-cluster cluster cannot be deleted (REPO-092).
	ae = apiErr(t, admin.DeleteCluster(ctx, apiclient.ClusterByServer(inCluster)))
	assert.Equal(t, http.StatusNotFound, ae.StatusCode)
	assert.Equal(t, `cluster "https://kubernetes.default.svc" not found`, ae.Problem.Detail)
}

// TestClusterCreateRejections covers the connection check and input guards of create.
func TestClusterCreateRejections(t *testing.T) {
	e := newEnv(t, envOptions{})
	f := newFakeCluster(t, e.cfg, e.clientset, "")
	f.addToken(goodToken)
	ctx := context.Background()
	admin := e.client("admin")

	// Unauthorized at the destination.
	_, err := admin.CreateCluster(ctx, input(f, "x", "", "bad-token"), false)
	ae := apiErr(t, err)
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Contains(t, ae.Problem.Detail, "error getting server version: ")
	assert.NotContains(t, ae.Problem.Detail, "bad-token")
	// Unknown CA.
	noCA := input(f, "x", "", goodToken)
	noCA.Config.TLSClientConfig.CAData = nil
	_, err = admin.CreateCluster(ctx, noCA, false)
	assert.Contains(t, apiErr(t, err).Problem.Detail, "certificate")
	// Unreachable.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	_, err = admin.CreateCluster(ctx, &apiclient.ClusterInput{Server: "https://" + addr, Config: apiclient.ClusterConfigInput{BearerToken: goodToken}}, false)
	assert.Contains(t, apiErr(t, err).Problem.Detail, "error getting server version: ")
	// SSRF guard.
	_, err = admin.CreateCluster(ctx, &apiclient.ClusterInput{Server: "http://169.254.169.254"}, false)
	assert.Contains(t, apiErr(t, err).Problem.Detail, "link-local, multicast or metadata address")
	// Unknown project.
	_, err = admin.CreateCluster(ctx, input(f, "x", "nope", goodToken), false)
	assert.Equal(t, `project "nope" does not exist`, apiErr(t, err).Problem.Detail)
	// Unknown body fields are rejected.
	_, err = admin.DoRaw(ctx, http.MethodPost, apiclient.PathClusters, nil, strings.NewReader(`{"server":"https://a","bogus":1}`), "application/json")
	ae = apiErr(t, err)
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Contains(t, ae.Problem.Detail, `unknown field "bogus"`)
	assert.Nil(t, e.clusterObject(f.srv.URL), "nothing stored")

	// cluster.inClusterEnabled=false: hidden, not gettable, not registrable (REPO-093).
	e.holder.setCM(clusterlib.SettingInClusterEnabled, "false")
	_, err = admin.CreateCluster(ctx, &apiclient.ClusterInput{Server: inCluster}, false)
	ae = apiErr(t, err)
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Equal(t, "cannot register cluster: in-cluster has been disabled", ae.Problem.Detail)
	list, err := admin.ListClusters(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, list.Items)
	_, err = admin.GetCluster(ctx, apiclient.ClusterByServer(inCluster))
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	e.holder.setCM(clusterlib.SettingInClusterEnabled, "no") // anything but "false" enables it
	list, err = admin.ListClusters(ctx, nil)
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
}

// TestClusterAuthorization covers RBAC objects (REPO-081: <project>/<server>), per-item
// list filtering, identical 403s for missing and forbidden clusters, tenant isolation and
// anonymous access.
func TestClusterAuthorization(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.createProject("team-a")
	e.createProject("team-b")
	fa := newFakeCluster(t, e.cfg, e.clientset, "")
	fb := newFakeCluster(t, e.cfg, e.clientset, "")
	fa.addToken(goodToken)
	fb.addToken(goodToken)
	ctx := context.Background()
	admin, bob, alice, carol := e.client("admin"), e.client("bob"), e.client("alice"), e.client("carol")

	// bob manages team-a clusters only.
	a, err := bob.CreateCluster(ctx, input(fa, "a", "team-a", goodToken), false)
	require.NoError(t, err)
	assert.Equal(t, "team-a", a.Spec.Project)
	_, err = bob.CreateCluster(ctx, input(fb, "b", "team-b", goodToken), false)
	ae := apiErr(t, err)
	assert.Equal(t, http.StatusForbidden, ae.StatusCode)
	assert.True(t, strings.HasPrefix(ae.Problem.Detail, "permission denied while creating cluster: permission denied: clusters, create, team-b/"+fb.srv.URL), ae.Problem.Detail)
	_, err = bob.CreateCluster(ctx, input(fb, "b", "", goodToken), false)
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode, "unscoped clusters need a global grant")
	_, err = admin.CreateCluster(ctx, input(fb, "b", "team-b", goodToken), false)
	require.NoError(t, err)
	// Registering an existing server the caller cannot see reveals nothing but its existence.
	_, err = bob.CreateCluster(ctx, input(fb, "b2", "team-a", goodToken), false)
	ae = apiErr(t, err)
	assert.Equal(t, http.StatusConflict, ae.StatusCode)
	assert.Equal(t, apiclient.CodeAlreadyExists, ae.Code())

	// Upsert is an update: frank may create team-a clusters but not update them.
	frank := e.client("frank")
	same, err := frank.CreateCluster(ctx, input(fa, "a", "team-a", goodToken), false)
	require.NoError(t, err, "identical create is idempotent")
	assert.Equal(t, a.Metadata.UID, same.Metadata.UID)
	_, err = frank.CreateCluster(ctx, input(fa, "a-renamed", "team-a", goodToken), true)
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	upserted, err := bob.CreateCluster(ctx, input(fa, "a", "team-a", goodToken), true)
	require.NoError(t, err)
	assert.Equal(t, "a", upserted.Spec.Name)

	// Lists are filtered per item.
	names := func(c *apiclient.Client) []string {
		l, err := c.ListClusters(ctx, nil)
		require.NoError(t, err)
		var out []string
		for _, it := range l.Items {
			out = append(out, it.Spec.Name)
		}
		return out
	}
	assert.ElementsMatch(t, []string{"a"}, names(bob))
	assert.ElementsMatch(t, []string{"a", "b", "in-cluster"}, names(alice))
	assert.Empty(t, names(carol))

	// Forbidden and missing clusters are indistinguishable.
	_, err = bob.GetCluster(ctx, apiclient.ClusterByServer(fb.srv.URL))
	forbidden := apiErr(t, err)
	_, err = bob.GetCluster(ctx, apiclient.ClusterByServer("https://missing.example.com"))
	missing := apiErr(t, err)
	assert.Equal(t, http.StatusForbidden, forbidden.StatusCode)
	assert.Equal(t, forbidden.StatusCode, missing.StatusCode)
	assert.Equal(t, forbidden.Problem.Detail, missing.Problem.Detail)
	assert.Equal(t, forbidden.Code(), missing.Code())

	// bob cannot move his cluster into team-b, update, rotate or delete team-b's cluster.
	_, err = bob.UpdateCluster(ctx, apiclient.ClusterByServer(fa.srv.URL), &apiclient.ClusterInput{Project: "team-b"}, "project")
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	_, err = bob.UpdateCluster(ctx, apiclient.ClusterByServer(fb.srv.URL), &apiclient.ClusterInput{Name: "x"}, "name")
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	assert.Equal(t, http.StatusForbidden, apiErr(t, bob.DeleteCluster(ctx, apiclient.ClusterByServer(fb.srv.URL))).StatusCode)
	assert.Equal(t, http.StatusForbidden, apiErr(t, bob.RotateClusterAuth(ctx, apiclient.ClusterByServer(fb.srv.URL))).StatusCode)
	_, err = bob.InvalidateClusterCache(ctx, apiclient.ClusterByServer(fb.srv.URL))
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	// Moving the cluster out of the project needs update on the unscoped object.
	_, err = bob.UpdateCluster(ctx, apiclient.ClusterByServer(fa.srv.URL), &apiclient.ClusterInput{}, "project")
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	// Read-only users cannot write.
	_, err = alice.UpdateCluster(ctx, apiclient.ClusterByServer(fa.srv.URL), &apiclient.ClusterInput{Name: "x"}, "name")
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	// bob can update and delete his own.
	_, err = bob.UpdateCluster(ctx, apiclient.ClusterByServer(fa.srv.URL), &apiclient.ClusterInput{Name: "a2"}, "name")
	require.NoError(t, err)
	require.NoError(t, bob.DeleteCluster(ctx, apiclient.ClusterByName("a2")))

	// Denials are audited with the RBAC triple.
	denied := 0
	for _, en := range e.audit.byOperation("deleteCluster") {
		if en.Result == "denied" {
			denied++
			assert.Equal(t, "team-b/"+fb.srv.URL, en.Object)
		}
	}
	assert.Equal(t, 1, denied)

	// Anonymous callers are rejected (users.anonymous.enabled is false).
	_, err = e.client("").ListClusters(ctx, nil)
	assert.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)
}

// TestClusterDeleteByName: by name every cluster with the name is deleted; unmanaged
// credential Secrets of declarative clusters are kept, unlabeled ones never modified.
func TestClusterDeleteByName(t *testing.T) {
	e := newEnv(t, envOptions{})
	f1 := newFakeCluster(t, e.cfg, e.clientset, "")
	f2 := newFakeCluster(t, e.cfg, e.clientset, "")
	f1.addToken(goodToken)
	f2.addToken(goodToken)
	ctx := context.Background()
	admin := e.client("admin")
	_, err := admin.CreateCluster(ctx, input(f1, "dup", "", goodToken), false)
	require.NoError(t, err)

	// A declarative cluster (kubectl) with a user-provided, unlabeled Secret.
	userSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "user-creds", Namespace: e.ns}, Data: map[string][]byte{"bearerToken": []byte(goodToken), "other": []byte("keep")}}
	require.NoError(t, e.kube.Create(ctx, userSecret))
	decl := &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "declarative", Namespace: e.ns}, Spec: v1alpha1.ClusterSpec{
		Server: f2.srv.URL + "/", Name: "dup", Config: v1alpha1.ClusterConfig{CredentialsRef: &v1alpha1.LocalSecretReference{Name: "user-creds"}},
	}}
	require.NoError(t, e.kube.Create(ctx, decl))

	// The trailing slash is normalized (REPO-077); the unlabeled Secret is not credentials.
	got, err := admin.GetCluster(ctx, apiclient.ClusterByServer(f2.srv.URL))
	require.NoError(t, err)
	assert.Equal(t, f2.srv.URL, got.Spec.Server)
	assert.False(t, got.Credentials.HasBearerToken)
	// Writing credentials into an unlabeled Secret is refused.
	cfgOnly := input(f2, "", "", goodToken)
	_, err = admin.UpdateCluster(ctx, apiclient.ClusterByServer(f2.srv.URL), cfgOnly, "config")
	ae := apiErr(t, err)
	assert.Equal(t, http.StatusPreconditionFailed, ae.StatusCode)
	assert.Equal(t, "keep", string(e.secret("user-creds").Data["other"]))

	require.NoError(t, admin.DeleteCluster(ctx, apiclient.ClusterByName("dup")))
	assert.Nil(t, e.clusterObject(f1.srv.URL))
	assert.Nil(t, e.clusterObject(f2.srv.URL+"/"))
	assert.NotNil(t, e.secret("user-creds"), "unmanaged Secret kept")
}

// TestClusterRotateAuth covers REPO-103 end to end: the destination accepts only tokens of
// existing service-account token Secrets; rotation creates a new Secret, verifies and
// stores the new token, and deletes the old Secret, after which the old token is refused.
func TestClusterRotateAuth(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	sysNS := createNamespace(t, e.clientset, "kube-system")
	startTokenController(t, e.clientset, sysNS)
	f := newFakeCluster(t, e.cfg, e.clientset, sysNS)
	_, err := e.clientset.CoreV1().ServiceAccounts(sysNS).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: clusterlib.ManagerServiceAccount}}, metav1.CreateOptions{})
	require.NoError(t, err)
	oldToken, err := clusterlib.ServiceAccountBearerToken(ctx, e.clientset, sysNS, clusterlib.ManagerServiceAccount, &clusterlib.BootstrapOptions{TokenTimeout: 10 * time.Second, PollInterval: 50 * time.Millisecond})
	require.NoError(t, err)
	oldClaims, err := clusterlib.ParseServiceAccountToken(oldToken)
	require.NoError(t, err)

	admin := e.client("admin")
	in := input(f, "rotating", "", oldToken)
	in.Config.TLSClientConfig.CertData, in.Config.TLSClientConfig.KeyData = []byte("cert"), []byte("key")
	_, err = admin.CreateCluster(ctx, in, false)
	ae := apiErr(t, err)
	assert.Contains(t, ae.Problem.Detail, "error getting server version", "an invalid client certificate fails the check")
	in.Config.TLSClientConfig.CertData, in.Config.TLSClientConfig.KeyData = nil, nil
	_, err = admin.CreateCluster(ctx, in, false)
	require.NoError(t, err)

	require.NoError(t, admin.RotateClusterAuth(ctx, apiclient.ClusterByName("rotating")))

	obj := e.clusterObject(f.srv.URL)
	require.NotNil(t, obj)
	stored := string(e.secret(obj.Spec.Config.CredentialsRef.Name).Data[v1alpha1.ClusterSecretKeyBearerToken])
	require.NotEqual(t, oldToken, stored)
	newClaims, err := clusterlib.ParseServiceAccountToken(stored)
	require.NoError(t, err)
	assert.NotEqual(t, oldClaims.SecretName, newClaims.SecretName)
	_, err = e.clientset.CoreV1().Secrets(sysNS).Get(ctx, oldClaims.SecretName, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "old token Secret deleted: %v", err)
	// The destination refuses the old token now and accepts the new one.
	probe := func(tok string) error {
		_, err := clusters.NewConnector(clusters.ConnectorOptions{}).Probe(ctx, &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: f.srv.URL, Name: "p"}},
			map[string][]byte{v1alpha1.ClusterSecretKeyBearerToken: []byte(tok), v1alpha1.ClusterSecretKeyCAData: f.caPEM})
		return err
	}
	require.Error(t, probe(oldToken))
	require.NoError(t, probe(stored))

	// Clusters without a bearer token cannot be rotated.
	f2 := newFakeCluster(t, e.cfg, e.clientset, "")
	f2.setBasic("u", "p")
	basic := input(f2, "basic", "", "")
	basic.Config.Username, basic.Config.Password = "u", "p"
	_, err = admin.CreateCluster(ctx, basic, false)
	require.NoError(t, err)
	ae = apiErr(t, admin.RotateClusterAuth(ctx, apiclient.ClusterByName("basic")))
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Equal(t, "Cluster '"+f2.srv.URL+"' does not use bearer token authentication", ae.Problem.Detail)
	ae = apiErr(t, admin.RotateClusterAuth(ctx, apiclient.ClusterByServer(inCluster)))
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	// A bearer token that is not a legacy service-account token cannot be rotated.
	f3 := newFakeCluster(t, e.cfg, e.clientset, "")
	f3.addToken(goodToken)
	_, err = admin.CreateCluster(ctx, input(f3, "opaque", "", goodToken), false)
	require.NoError(t, err)
	ae = apiErr(t, admin.RotateClusterAuth(ctx, apiclient.ClusterByName("opaque")))
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Contains(t, ae.Problem.Detail, "legacy service-account token")
}

// TestClusterExecAndInCluster: exec-plugin clusters are stored without being probed by the
// API server and never return their exec configuration; the implicit in-cluster cluster is
// materialized as an object when updated (upstream creates its Secret).
func TestClusterExecAndInCluster(t *testing.T) {
	var local *fakeCluster
	e := newEnv(t, envOptions{connector: func(e *testEnv) clusters.Connector {
		return clusters.NewConnector(clusters.ConnectorOptions{Timeout: 5 * time.Second, Options: clusterlib.Options{
			InClusterConfig: func() (*rest.Config, error) {
				return &rest.Config{Host: local.srv.URL, BearerToken: goodToken, TLSClientConfig: rest.TLSClientConfig{CAData: local.caPEM}}, nil
			},
		}})
	}})
	local = newFakeCluster(t, e.cfg, e.clientset, "")
	local.addToken(goodToken)
	ctx := context.Background()
	admin := e.client("admin")

	execIn := &apiclient.ClusterInput{Server: "https://exec.example.com", Name: "exec", Config: apiclient.ClusterConfigInput{
		ExecProviderConfig: &v1alpha1.ExecProviderConfig{Command: "/usr/bin/false", Args: []string{"--token=SECRET-ARG"}, Env: map[string]string{"PASSWORD": "SECRET-ENV"}, APIVersion: "client.authentication.k8s.io/v1beta1"},
	}}
	created, err := admin.CreateCluster(ctx, execIn, false)
	require.NoError(t, err)
	assert.Nil(t, created.Spec.Config.ExecProviderConfig)
	assert.True(t, created.Credentials.HasExecProviderConfig)
	body := rawGet(t, admin, apiclient.PathClusters, nil)
	assert.NotContains(t, body, "SECRET-ARG")
	assert.NotContains(t, body, "SECRET-ENV")
	obj := e.clusterObject("https://exec.example.com")
	require.NotNil(t, obj)
	assert.Nil(t, obj.Status.ConnectionState, "not probed by the API server")

	// Updating the implicit in-cluster cluster creates its object.
	upd, err := admin.UpdateCluster(ctx, apiclient.ClusterByServer(inCluster), &apiclient.ClusterInput{Namespaces: []string{"apps"}}, "namespaces")
	require.NoError(t, err)
	assert.False(t, upd.InCluster)
	assert.Equal(t, "in-cluster", upd.Spec.Name)
	assert.Equal(t, []string{"apps"}, upd.Spec.Namespaces)
	o := e.clusterObject(inCluster)
	require.NotNil(t, o)
	require.NotNil(t, o.Status.ConnectionState)
	assert.Equal(t, v1alpha1.ConnectionStatusSuccessful, o.Status.ConnectionState.Status)
	list, err := admin.ListClusters(ctx, &apiclient.ListClustersRequest{Server: inCluster})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	assert.False(t, list.Items[0].InCluster, "the object overrides the implicit cluster")
	// Deleting the override restores the implicit cluster.
	require.NoError(t, admin.DeleteCluster(ctx, apiclient.ClusterByServer(inCluster)))
	list, err = admin.ListClusters(ctx, &apiclient.ListClustersRequest{Server: inCluster})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	assert.True(t, list.Items[0].InCluster)

	// The JSON of a cluster is the documented shape.
	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(rawGet(t, admin, apiclient.PathCluster, &apiclient.ClusterIDRequest{Server: "https://exec.example.com"})), &raw))
	assert.Equal(t, "Cluster", raw["kind"])
	assert.Contains(t, raw, "credentials")
	var stored v1alpha1.Cluster
	require.NoError(t, e.kube.Get(ctx, client.ObjectKeyFromObject(obj), &stored))
	assert.Equal(t, "SECRET-ARG", strings.TrimPrefix(stored.Spec.Config.ExecProviderConfig.Args[0], "--token="), "stored in the object (documented)")
}
