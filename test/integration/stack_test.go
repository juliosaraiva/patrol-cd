package integration

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
	"github.com/juliosaraiva/patrol-cd/test/integration/stack"
)

// TestStackStarts is the harness self-test: every component starts, the admin and a local
// account log in through the API, every destination is registered with its credentials,
// and the CLI runs in-process.
func TestStackStarts(t *testing.T) {
	s := stack.Start(t, stack.Options{
		Accounts:   map[string]string{"viewer": "viewer-password-1"},
		RBACPolicy: "g, viewer, role:readonly\n",
	})
	api := s.API(t)
	info, err := api.UserInfo(t.Context())
	require.NoError(t, err)
	assert.True(t, info.LoggedIn)
	assert.Equal(t, "admin", info.Username)

	viewer := s.AccountClient(t, "viewer")
	vinfo, err := viewer.UserInfo(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "viewer", vinfo.Username)
	_, err = viewer.CreateApplication(t.Context(), &apiclient.ApplicationCreateRequest{Application: &v1alpha1.Application{}})
	require.Error(t, err)
	// An account API token works as soon as it is returned (integration finding I-2).
	tokenClient := s.ClientWithToken(t, s.AccountToken(t, "viewer"))
	_, err = tokenClient.ListApplications(t.Context(), &apiclient.ApplicationQuery{})
	require.NoError(t, err, "first use of a new account API token")
	tinfo, err := tokenClient.UserInfo(t.Context())
	require.NoError(t, err)
	assert.True(t, tinfo.LoggedIn)
	assert.Equal(t, "viewer", tinfo.Username)

	// A project role token works as soon as it is returned.
	_, err = api.CreateProject(t.Context(), &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: "team"},
		Spec: v1alpha1.AppProjectSpec{
			SourceRepos: []string{"*"}, Destinations: []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}},
			Roles: []v1alpha1.ProjectRole{{Name: "ci", Policies: []string{"p, proj:team:ci, applications, get, team/*, allow"}}},
		},
	}, false)
	require.NoError(t, err)
	projClient := s.ClientWithToken(t, s.ProjectToken(t, "team", "ci"))
	_, err = projClient.ListApplications(t.Context(), &apiclient.ApplicationQuery{})
	require.NoError(t, err, "first use of a new project role token")

	// A changed password is accepted by the next login.
	require.NoError(t, viewer.UpdatePassword(t.Context(), &apiclient.UpdatePasswordRequest{CurrentPassword: "viewer-password-1", NewPassword: "viewer-password-2"}))
	s.Login(t, "viewer", "viewer-password-2")

	clusters, err := api.ListClusters(t.Context(), nil)
	require.NoError(t, err)
	var servers []string
	for _, c := range clusters.Items {
		servers = append(servers, c.Spec.Server)
	}
	assert.Contains(t, servers, s.Destination(0).Server, "in-cluster")
	assert.Contains(t, servers, s.Destination(1).Server, "dest-1")

	out := s.CLI(t, "version").Succeeded(t)
	assert.Contains(t, out.Stdout, "patrol")
}

// TestEveryDestinationKind deploys one Application to each kind of destination: the
// control cluster as "in-cluster" (local kubeconfig), the shared destination registered with
// a client certificate and a dedicated destination registered with a ServiceAccount bearer
// token. The CLI lists and reads them.
func TestEveryDestinationKind(t *testing.T) {
	s := stack.Start(t, stack.Options{DedicatedDestinations: 1})
	repo := s.Git().NewRepo(t, "everywhere")
	rev := repo.CommitFiles(t, "one config map", map[string]string{"app/cm.yaml": configMap("hello", map[string]string{"k": "v"}, nil)})
	for i, auth := range []stack.AuthMode{stack.AuthInCluster, stack.AuthClientCert, stack.AuthBearerToken} {
		d := s.Destination(i)
		require.Equal(t, auth, d.Auth)
		ns := s.NewNamespace(t, d, "everywhere")
		name := "everywhere-" + d.Name
		s.CreateApp(t, s.NewApp(name, repo, "app", d, ns))
		requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: name}))
		s.WaitSyncedHealthy(t, name, rev)
		assert.Equal(t, "v", liveData(t, d, ns, "hello", "k"), "%s (%s)", d.Name, auth)
	}
	list := s.CLI(t, "app", "list").Succeeded(t)
	for _, d := range []string{"in-cluster", "dest-1", "dest-2"} {
		assert.Contains(t, list.Stdout, "everywhere-"+d)
	}
	get := s.CLI(t, "app", "get", "everywhere-dest-2").Succeeded(t)
	assert.Contains(t, get.Stdout, "Synced")
}
