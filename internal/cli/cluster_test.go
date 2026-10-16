package cli_test

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
	clusterlib "github.com/juliosaraiva/patrol-cd/internal/clusters"
)

func clusterByServer(e *clitest.Env, server string) *v1alpha1.Cluster {
	for _, c := range e.Clusters() {
		if c.Spec.Server == server {
			return &c
		}
	}
	return nil
}

func clusterToken(t *testing.T, e *clitest.Env, c *v1alpha1.Cluster) string {
	t.Helper()
	if c.Spec.Config.CredentialsRef == nil {
		t.Fatalf("cluster %s has no credentials reference", c.Spec.Server)
	}
	s := e.Secret(c.Spec.Config.CredentialsRef.Name)
	if s == nil {
		t.Fatalf("credentials secret %s missing", c.Spec.Config.CredentialsRef.Name)
	}
	return clusterlib.CredentialsFromSecretData(s.Data).BearerToken
}

// TestClusterLifecycle is the only test that installs patrol-manager into kube-system of
// the shared envtest API server.
func TestClusterLifecycle(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.LoginAs("admin")
	fc := e.NewFakeCluster(clusterlib.DefaultSystemNamespace)
	dir := t.TempDir()
	kc := fc.WriteKubeconfig(dir, "target", "tgt")
	ctx := e.Context()

	// Without a context argument the kubeconfig contexts are listed.
	r := e.RunWith(clitest.RunOptions{Env: map[string]string{"KUBECONFIG": kc}}, "cluster", "add")
	if r.Code != 1 || !strings.Contains(r.Stdout, "CURRENT") || !strings.Contains(r.Stdout, "target") || !strings.Contains(r.Stderr, "Choose a context name from:") {
		t.Fatalf("context listing: %+v", r)
	}
	e.Run("cluster", "add", "nosuch", "--kubeconfig", kc).Failed(t, 20, "context nosuch does not exist in kubeconfig")
	e.Run("cluster", "add", "target", "--kubeconfig", kc, "--in-cluster", "--cluster-endpoint", "kubeconfig").Failed(t, 1, "Can only use one of --in-cluster or --cluster-endpoint")
	e.Run("cluster", "add", "target", "--kubeconfig", kc, "--cluster-endpoint", "moon").Failed(t, 20, `unknown cluster endpoint "moon"`)
	e.Run("cluster", "add", "target", "--kubeconfig", kc, "--label", "novalue").Failed(t, 20, "Label novalue is not in the format key=value")

	// add installs patrol-manager and registers the cluster with its token.
	e.Run("cluster", "add", "target", "--kubeconfig", kc, "--name", "tgt", "--label", "env=dev", "--annotation", "owner=team", "-y").
		Succeeded(t).Contains(t, "Cluster '"+fc.URL+"' added")
	c := clusterByServer(e, fc.URL)
	if c == nil || c.Spec.Name != "tgt" || c.Labels["env"] != "dev" || c.Annotations["owner"] != "team" {
		t.Fatalf("cluster object: %+v", c)
	}
	tok := clusterToken(t, e, c)
	if tok == "" || tok == fc.AdminToken {
		t.Fatal("the cluster must be registered with the patrol-manager token, never the kubeconfig credential")
	}
	if _, err := e.Clientset.CoreV1().ServiceAccounts(clusterlib.DefaultSystemNamespace).Get(ctx, clusterlib.ManagerServiceAccount, metav1.GetOptions{}); err != nil {
		t.Fatalf("patrol-manager service account: %v", err)
	}
	tc, err := clusterlib.ParseServiceAccountToken(tok)
	if err != nil || tc.ServiceAccountName != clusterlib.ManagerServiceAccount {
		t.Fatalf("registered token is not the patrol-manager token: %v %+v", err, tc)
	}

	// list / get.
	e.Run("cluster", "list").Succeeded(t).Contains(t, "SERVER", "NAME", "VERSION", "STATUS", "MESSAGE", "PROJECT", fc.URL, "tgt")
	e.Run("cluster", "list", "-o", "server").Succeeded(t).Contains(t, fc.URL+"\n")
	e.Run("cluster", "list", "-o", "json").Succeeded(t).Contains(t, `"server": "`+fc.URL+`"`).NotContains(t, tok)
	e.Run("cluster", "get", "tgt").Succeeded(t).Contains(t, "server: "+fc.URL, "name: tgt").NotContains(t, tok)
	e.Run("cluster", "get", fc.URL, "-o", "wide").Succeeded(t).Contains(t, "Cluster information", "Server URL:            "+fc.URL,
		"Server Name:           tgt", "Namespaces:            all namespaces", "oAuth authentication:  true", "Client cert:           false")
	e.Run("cluster", "get", "tgt", "-o", "server").Succeeded(t).Contains(t, fc.URL)
	e.Run("cluster", "get", "nope").Failed(t, 20)
	e.Run("cluster", "get", "tgt", "-o", "toml").Failed(t, 20, "toml")

	// set.
	e.Run("cluster", "set", "tgt").Succeeded(t).Contains(t, "Specify the cluster field to be updated.")
	e.Run("cluster", "set", "tgt", "--name", "tgt2", "--namespace", "apps", "--namespace", "web", "--label", "env=prod").Succeeded(t).Contains(t, "Cluster 'tgt' updated.")
	c = clusterByServer(e, fc.URL)
	if c.Spec.Name != "tgt2" || len(c.Spec.Namespaces) != 2 || c.Labels["env"] != "prod" {
		t.Fatalf("set: %+v", c)
	}
	e.Run("cluster", "list").Succeeded(t).Contains(t, fc.URL+" (2 namespaces)")
	e.Run("cluster", "set", "tgt2", "--namespace", "*").Succeeded(t)
	if c = clusterByServer(e, fc.URL); len(c.Spec.Namespaces) != 0 {
		t.Fatalf("namespaces not reset: %v", c.Spec.Namespaces)
	}

	// Existing server: same spec required unless --upsert; --service-account uses an
	// existing account's token.
	e.Run("cluster", "add", "target", "--kubeconfig", kc, "--name", "other", "-y").Failed(t, 20)
	e.Run("cluster", "add", "target", "--kubeconfig", kc, "--name", "tgt2", "--service-account", clusterlib.ManagerServiceAccount, "--upsert").Succeeded(t)

	// rotate-auth replaces the legacy token and deletes the old token Secret.
	oldTok := clusterToken(t, e, clusterByServer(e, fc.URL))
	claims, err := clusterlib.ParseServiceAccountToken(oldTok)
	if err != nil {
		t.Fatal(err)
	}
	e.Run("cluster", "rotate-auth", "tgt2").Succeeded(t).Contains(t, "Cluster 'tgt2' rotated auth")
	newTok := clusterToken(t, e, clusterByServer(e, fc.URL))
	if newTok == "" || newTok == oldTok {
		t.Fatal("token not rotated")
	}
	if _, err := e.Clientset.CoreV1().Secrets(clusterlib.DefaultSystemNamespace).Get(ctx, claims.SecretName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old token secret still present: %v", err)
	}
	e.Run("cluster", "rotate-auth", "nope").Failed(t, 20)

	// Authorization: alice is read-only.
	alice := e.LoginToken("alice")
	e.RunAs(alice, "cluster", "list").Succeeded(t).Contains(t, fc.URL)
	e.RunAs(alice, "cluster", "set", "tgt2", "--name", "x").Failed(t, 20, "permission denied")
	e.RunAs(alice, "cluster", "rotate-auth", "tgt2").Failed(t, 20, "permission denied")
	e.RunAs(alice, "cluster", "rm", "tgt2", "-y").Failed(t, 20)

	// rm: mandatory confirmation unless -y; the manager identity is removed through the
	// kubeconfig context named like the cluster.
	e.RunWith(clitest.RunOptions{Stdin: "n\n"}, "cluster", "rm", "tgt2", "--kubeconfig", kc).Succeeded(t).Contains(t, "The command to remove 'tgt2' was cancelled.")
	if clusterByServer(e, fc.URL) == nil {
		t.Fatal("cancelled rm removed the cluster")
	}
	fc2 := fc.WriteKubeconfig(t.TempDir(), "tgt2")
	e.RunWith(clitest.RunOptions{Stdin: "y\n"}, "cluster", "rm", "tgt2", "--kubeconfig", fc2).Succeeded(t).Contains(t, "Cluster 'tgt2' removed")
	if clusterByServer(e, fc.URL) != nil {
		t.Fatal("cluster not removed")
	}
	if _, err := e.Clientset.CoreV1().ServiceAccounts(clusterlib.DefaultSystemNamespace).Get(ctx, clusterlib.ManagerServiceAccount, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("patrol-manager not uninstalled: %v", err)
	}
}
