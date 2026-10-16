package core_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
)

// editKubeconfig loads the kubeconfig at path, applies edit and writes it to a new file.
func editKubeconfig(t *testing.T, path string, edit func(*clientcmdapi.Config)) string {
	t.Helper()
	kc, err := clientcmd.LoadFromFile(path)
	require.NoError(t, err)
	edit(kc)
	out := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, clientcmd.WriteToFile(*kc, out))
	require.NoError(t, os.Chmod(out, 0o600))
	return out
}

// TestCoreNamespaceFlag: -n/--namespace, --context and --kubeconfig select the Patrol
// namespace and the kubeconfig on any subcommand, placed before or after the group name,
// over a kube context whose namespace is another one.
func TestCoreNamespaceFlag(t *testing.T) {
	e := newCoreEnv(t)
	require.NoError(t, e.admin.Create(e.ctx, &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Namespace: e.ns, Name: "existing"},
		Spec: v1alpha1.ApplicationSpec{
			Project:     "default",
			Source:      &v1alpha1.ApplicationSource{RepoURL: repoURL, Path: "guestbook"},
			Destination: v1alpha1.ApplicationDestination{Server: inCluster, Namespace: e.dest},
		},
	}))
	kc, user := e.user("operator", operatorRules()...)
	// The current context's namespace is e.dest, where the user has no rights; context
	// "patrol" names the Patrol namespace.
	other := editKubeconfig(t, kc, func(c *clientcmdapi.Config) {
		cur := c.Contexts[c.CurrentContext]
		patrol := *cur
		cur.Namespace = e.dest
		c.Contexts["patrol"] = &patrol
	})

	e.run(other, "app", "list", "-o", "name").
		Failed(t, 20, `User "`+user+`" cannot list resource "applications"`, `in the namespace "`+e.dest+`"`)
	e.run(other, "app", "list", "-n", e.ns, "-o", "name").Succeeded(t).Contains(t, e.ns+"/existing")
	e.run(other, "-n", e.ns, "app", "get", "existing").Succeeded(t).Contains(t, "Name:", e.ns+"/existing")
	e.run(other, "proj", "list", "--namespace", e.ns).Succeeded(t).Contains(t, "default")
	e.run(other, "appset", "list", "-n", e.ns).Succeeded(t)
	e.run(other, "repo", "list", "--context", "patrol").Succeeded(t)
	e.run(other, "app", "get", "existing", "--context", "patrol").Succeeded(t).Contains(t, e.ns+"/existing")

	// --kubeconfig wins over KUBECONFIG.
	missing := filepath.Join(t.TempDir(), "missing-kubeconfig")
	e.runner.RunWith(clitest.RunOptions{Env: map[string]string{"KUBECONFIG": missing}},
		"--core", "app", "list", "--kubeconfig", other, "-n", e.ns, "-o", "name").Succeeded(t).Contains(t, e.ns+"/existing")
}

// clusterStandIn is a TLS stand-in for a destination cluster that records the
// credentials of every request; it answers /version and 404 for anything else.
type clusterStandIn struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func newClusterStandIn(t *testing.T) *clusterStandIn {
	t.Helper()
	s := &clusterStandIn{}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, fmt.Sprintf("%s %s auth=%s", r.Method, r.URL.Path, r.Header.Get("Authorization")))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/version" {
			_, _ = w.Write([]byte(`{"major":"1","minor":"31","gitVersion":"v1.31.2"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404,"message":"not found"}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *clusterStandIn) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.seen
	s.seen = nil
	return out
}

// TestCoreClusterCommandsUseCallerCredentials: `cluster set` and `cluster rotate-auth` in
// core mode never send the credentials stored for Patrol CD in the Cluster Secret to the
// destination cluster: with a kubeconfig context for its server the caller's credentials
// are used, without one nothing is sent (the update is not connection-checked and
// rotate-auth is refused).
func TestCoreClusterCommandsUseCallerCredentials(t *testing.T) {
	e := newCoreEnv(t)
	remote := newClusterStandIn(t)
	stored, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss":                                    "kubernetes/serviceaccount",
		"kubernetes.io/serviceaccount/namespace": "kube-system",
		"kubernetes.io/serviceaccount/secret.name":          "patrol-manager-token-abcde",
		"kubernetes.io/serviceaccount/service-account.name": "patrol-manager",
	}).SignedString([]byte("0123456789abcdef0123456789abcdef"))
	require.NoError(t, err)
	_, err = e.kube.CoreV1().Secrets(e.ns).Create(e.ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "remote-creds", Labels: map[string]string{v1alpha1.LabelKeySecretType: v1alpha1.LabelValueSecretTypeCluster}},
		Data:       map[string][]byte{v1alpha1.ClusterSecretKeyBearerToken: []byte(stored)},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, e.admin.Create(e.ctx, &v1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: e.ns, Name: "remote"},
		Spec: v1alpha1.ClusterSpec{Server: remote.URL, Name: "remote", Config: v1alpha1.ClusterConfig{
			TLSClientConfig: v1alpha1.TLSClientConfig{Insecure: true},
			CredentialsRef:  &v1alpha1.LocalSecretReference{Name: "remote-creds"},
		}},
	}))
	clusterName := func() string {
		var c v1alpha1.Cluster
		require.NoError(t, e.admin.Get(e.ctx, client.ObjectKey{Namespace: e.ns, Name: "remote"}, &c))
		return c.Spec.Name
	}
	kc, _ := e.user("operator", operatorRules()...)

	// No kubeconfig context for the cluster's server.
	e.run(kc, "cluster", "set", "remote", "--name", "renamed").Succeeded(t).Contains(t, "Cluster 'remote' updated.")
	assert.Equal(t, "renamed", clusterName())
	e.run(kc, "cluster", "rotate-auth", remote.URL).Failed(t, 20, "only with your own kubeconfig credentials")
	assert.Empty(t, remote.take(), "nothing may reach the cluster without the caller's credentials for it")

	// A context of the caller for the server (the current context stays the control plane).
	const callerToken = "caller-remote-token"
	withRemote := editKubeconfig(t, kc, func(c *clientcmdapi.Config) {
		c.Clusters["remote"] = &clientcmdapi.Cluster{Server: remote.URL, InsecureSkipTLSVerify: true}
		c.AuthInfos["remote-user"] = &clientcmdapi.AuthInfo{Token: callerToken}
		c.Contexts["remote"] = &clientcmdapi.Context{Cluster: "remote", AuthInfo: "remote-user"}
	})
	e.run(withRemote, "cluster", "set", "renamed", "--name", "renamed-again").Succeeded(t)
	assert.Equal(t, "renamed-again", clusterName())
	assert.Equal(t, []string{"GET /version auth=Bearer " + callerToken}, remote.take())
	e.run(withRemote, "cluster", "rotate-auth", remote.URL).Failed(t, 20, "patrol-manager-token-abcde")
	seen := remote.take()
	require.NotEmpty(t, seen)
	for _, s := range seen {
		assert.True(t, strings.HasSuffix(s, "auth=Bearer "+callerToken), s)
	}
	sec, err := e.kube.CoreV1().Secrets(e.ns).Get(e.ctx, "remote-creds", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, stored, string(sec.Data[v1alpha1.ClusterSecretKeyBearerToken]))
}
