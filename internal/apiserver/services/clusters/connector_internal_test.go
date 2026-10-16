package clusters

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	restclient "k8s.io/client-go/rest"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	clusterlib "github.com/juliosaraiva/patrol-cd/internal/clusters"
)

// TestConnectorNoAmbientIdentity: without AllowExec the connector never authenticates a
// probe with patrol-server's own cloud identity (in-process AWS/GCP/Azure tokens) or runs a
// command: such configurations are refused before any connection, so a caller-chosen server
// receives nothing (review finding: EKS token sent to a user-supplied URL). The in-cluster
// server ignores these settings and keeps its pod configuration.
func TestConnectorNoAmbientIdentity(t *testing.T) {
	// patrol-server's ambient AWS identity.
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDPATROLSERVER")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "patrol-server-secret")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))

	var hits atomic.Int32
	var lastAuth atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		lastAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"major":"1","minor":"30","gitVersion":"v1.30.2"}`))
	}))
	defer srv.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	data := map[string][]byte{v1alpha1.ClusterSecretKeyCAData: ca}
	exec := func(command string, args ...string) *v1alpha1.ExecProviderConfig {
		return &v1alpha1.ExecProviderConfig{Command: command, Args: args, APIVersion: "client.authentication.k8s.io/v1beta1"}
	}

	conn := NewConnector(ConnectorOptions{Timeout: 5 * time.Second})
	c, ok := conn.(*connector)
	require.True(t, ok)
	assert.True(t, c.o.Options.AWSAuthExec, "in-process AWS tokens disabled")
	assert.True(t, c.o.Options.DisableNativeK8sAuth, "in-process k8s-auth tokens disabled")

	ctx := context.Background()
	for name, cfg := range map[string]v1alpha1.ClusterConfig{
		"awsAuthConfig":          {AWSAuthConfig: &v1alpha1.AWSAuthConfig{ClusterName: "victim-prod-eks"}},
		"awsAuthConfig role":     {AWSAuthConfig: &v1alpha1.AWSAuthConfig{ClusterName: "victim-prod-eks", RoleARN: "arn:aws:iam::123456789012:role/admin"}},
		"argocd-k8s-auth aws":    {ExecProviderConfig: exec("argocd-k8s-auth", "aws", "--cluster-name", "victim-prod-eks")},
		"patrolcd k8s-auth gcp":  {ExecProviderConfig: exec("patrolcd", "k8s-auth", "gcp")},
		"argocd-k8s-auth azure":  {ExecProviderConfig: exec("argocd-k8s-auth", "azure")},
		"arbitrary exec command": {ExecProviderConfig: exec("/bin/sh", "-c", "id")},
	} {
		cl := &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: srv.URL, Name: "x", Config: cfg}}
		_, err := conn.Probe(ctx, cl, data)
		require.ErrorIs(t, err, ErrExecNotAllowed, name)
		_, err = conn.Kubernetes(ctx, cl, data)
		require.ErrorIs(t, err, ErrExecNotAllowed, name)
	}
	assert.Zero(t, hits.Load(), "nothing reached the caller-chosen server")

	// In-cluster: awsAuthConfig is ignored (internal/clusters), the pod configuration is used.
	inConn := NewConnector(ConnectorOptions{Timeout: 5 * time.Second, Options: clusterlib.Options{
		InClusterConfig: func() (*restclient.Config, error) {
			return &restclient.Config{Host: srv.URL, BearerToken: "pod-token", TLSClientConfig: restclient.TLSClientConfig{CAData: ca}}, nil
		},
	}})
	v, err := inConn.Probe(ctx, &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{
		Server: v1alpha1.KubernetesInternalAPIServerAddr, Name: "in-cluster",
		Config: v1alpha1.ClusterConfig{AWSAuthConfig: &v1alpha1.AWSAuthConfig{ClusterName: "x"}},
	}}, nil)
	require.NoError(t, err)
	assert.Equal(t, "v1.30.2", v)
	assert.Equal(t, "Bearer pod-token", lastAuth.Load())

	// AllowExec restores the upstream behavior (in-process tokens enabled).
	allowed, ok := NewConnector(ConnectorOptions{AllowExec: true}).(*connector)
	require.True(t, ok)
	assert.False(t, allowed.o.Options.AWSAuthExec)
	assert.False(t, allowed.o.Options.DisableNativeK8sAuth)
}

func TestUsesAmbientAuth(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		spec v1alpha1.ClusterSpec
		want bool
	}{
		{"bearer token", v1alpha1.ClusterSpec{Server: "https://kube.example.com"}, false},
		{"aws", v1alpha1.ClusterSpec{Server: "https://kube.example.com", Config: v1alpha1.ClusterConfig{AWSAuthConfig: &v1alpha1.AWSAuthConfig{}}}, true},
		{"exec", v1alpha1.ClusterSpec{Server: "https://kube.example.com", Config: v1alpha1.ClusterConfig{ExecProviderConfig: &v1alpha1.ExecProviderConfig{Command: "x"}}}, true},
		{"in-cluster ignores them", v1alpha1.ClusterSpec{Server: v1alpha1.KubernetesInternalAPIServerAddr + "/", Config: v1alpha1.ClusterConfig{AWSAuthConfig: &v1alpha1.AWSAuthConfig{}}}, false},
	} {
		assert.Equal(t, tc.want, usesAmbientAuth(&tc.spec), tc.name)
	}
}
