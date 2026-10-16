package core

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/transport"
)

func TestEndpointValidation(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{
		"http://127.0.0.1:8081", "https://[::1]:8090", "http://localhost:1", "http://127.9.9.9",
		"https://patrol-repo-server.patrol.svc:8081", "http://patrol-controller-0.patrol-controller.patrol.svc.cluster.local:8090/",
	} {
		assert.NoError(t, validateEndpointURL(ok), ok)
	}
	for raw, msg := range map[string]string{
		"":                                "required",
		"ftp://127.0.0.1":                 "http or https",
		"http://10.0.0.1:8081":            "neither a loopback address",
		"http://evil.example.com":         "neither a loopback address",
		"http://svc":                      "neither a loopback address",
		"http://.svc":                     "neither a loopback address",
		"http://user:pw@127.0.0.1":        "scheme://host:port",
		"http://127.0.0.1/path":           "scheme://host:port",
		"http://127.0.0.1?x=1":            "scheme://host:port",
		"http://127.0.0.1:99999":          "invalid port",
		"http://patrol.svc.evil.example":  "neither a loopback address",
		"http://169.254.169.254/":         "neither a loopback address",
		"http://localhost.example.com:80": "neither a loopback address",
	} {
		err := validateEndpointURL(raw)
		require.Error(t, err, raw)
		assert.Contains(t, err.Error(), msg, raw)
	}

	e, err := ParseEndpoints("repo-server=http://127.0.0.1:1, controller=http://127.0.0.1:2")
	require.NoError(t, err)
	assert.Equal(t, Endpoints{RepoServer: "http://127.0.0.1:1", Controller: "http://127.0.0.1:2"}, e)
	for _, bad := range []string{"repo-server=http://127.0.0.1:1", "x=y", "nope", "repo-server=http://example.com,controller=http://127.0.0.1:2"} {
		_, err := ParseEndpoints(bad)
		assert.Error(t, err, bad)
	}
}

func TestEndpointsFromEnvNeedsTheGate(t *testing.T) {
	t.Parallel()
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	e, err := endpointsFromEnv(env(nil))
	require.NoError(t, err)
	assert.Nil(t, e)
	_, err = endpointsFromEnv(env(map[string]string{EnvEndpoints: "repo-server=http://127.0.0.1:1,controller=http://127.0.0.1:2"}))
	require.ErrorContains(t, err, "honored only with PATROL_CORE_ALLOW_ENDPOINTS=true")
	_, err = endpointsFromEnv(env(map[string]string{EnvEndpoints: "repo-server=http://example.com:1,controller=http://127.0.0.1:2", EnvAllowEndpoints: "true"}))
	require.ErrorContains(t, err, "neither a loopback address")
	e, err = endpointsFromEnv(env(map[string]string{EnvEndpoints: "repo-server=http://127.0.0.1:1,controller=http://127.0.0.1:2", EnvAllowEndpoints: "true"}))
	require.NoError(t, err)
	assert.Empty(t, e.Token, "the token never comes from the environment")

	// The provider refuses the ungated override before contacting anything.
	kc := writeKubeconfig(t, "https://127.0.0.1:1", "ctx", "team")
	p := NewProvider(Options{LookupEnv: env(map[string]string{EnvEndpoints: "repo-server=http://127.0.0.1:1,controller=http://127.0.0.1:2"})})
	_, err = p.Connect(t.Context(), transport.Request{Kube: transport.KubeOptions{KubeconfigPaths: []string{kc}}, Logger: slog.New(slog.DiscardHandler)})
	require.ErrorContains(t, err, "PATROL_CORE_ALLOW_ENDPOINTS")
	p = NewProvider(Options{Endpoints: &Endpoints{RepoServer: "http://10.1.1.1:1", Controller: "http://127.0.0.1:2"}})
	_, err = p.Connect(t.Context(), transport.Request{Kube: transport.KubeOptions{KubeconfigPaths: []string{kc}}})
	require.ErrorContains(t, err, "repo-server endpoint")
}

func writeKubeconfig(t *testing.T, server, current, namespace string) string {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["main"] = &clientcmdapi.Cluster{Server: server}
	cfg.Clusters["remote"] = &clientcmdapi.Cluster{Server: "https://remote.example.com:6443/"}
	cfg.AuthInfos["me"] = &clientcmdapi.AuthInfo{Token: "t"}
	cfg.AuthInfos["remote-me"] = &clientcmdapi.AuthInfo{Token: "remote-token"}
	cfg.Contexts["ctx"] = &clientcmdapi.Context{Cluster: "main", AuthInfo: "me", Namespace: namespace}
	cfg.Contexts["plain"] = &clientcmdapi.Context{Cluster: "main", AuthInfo: "me"}
	cfg.Contexts["z-remote"] = &clientcmdapi.Context{Cluster: "remote", AuthInfo: "remote-me"}
	cfg.CurrentContext = current
	p := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, clientcmd.WriteToFile(*cfg, p))
	return p
}

func TestNamespaceAndDestinations(t *testing.T) {
	t.Parallel()
	kc := writeKubeconfig(t, "https://127.0.0.1:6443", "ctx", "team-ns")
	opts := func(o clientcmd.ConfigOverrides) transport.KubeOptions {
		return transport.KubeOptions{KubeconfigPaths: []string{kc}, Overrides: o}
	}
	ko := opts(clientcmd.ConfigOverrides{})
	assert.Equal(t, "team-ns", resolveNamespace(ko.ClientConfig(), ko.Overrides), "kube context namespace")
	ko = opts(clientcmd.ConfigOverrides{Context: clientcmdapi.Context{Namespace: "flag-ns"}})
	assert.Equal(t, "flag-ns", resolveNamespace(ko.ClientConfig(), ko.Overrides), "-n wins")
	ko = opts(clientcmd.ConfigOverrides{CurrentContext: "plain"})
	assert.Equal(t, DefaultNamespace, resolveNamespace(ko.ClientConfig(), ko.Overrides), "default patrol")

	ko = opts(clientcmd.ConfigOverrides{})
	cc := ko.ClientConfig()
	cur, err := cc.ClientConfig()
	require.NoError(t, err)
	d := newDestinationConfigs(cc, cur)
	for _, server := range []string{v1alpha1.KubernetesInternalAPIServerAddr, "https://kubernetes.default.svc/", "https://127.0.0.1:6443"} {
		got, err := d.RESTConfig(t.Context(), &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: server}})
		require.NoError(t, err, server)
		assert.Equal(t, "t", got.BearerToken, server)
		assert.NotSame(t, cur, got)
	}
	got, err := d.RESTConfig(t.Context(), &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: "https://remote.example.com:6443"}})
	require.NoError(t, err)
	assert.Equal(t, "remote-token", got.BearerToken, "the caller's own context for that cluster")
	_, err = d.RESTConfig(t.Context(), &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: "https://unknown.example.com"}})
	require.ErrorContains(t, err, "no kubeconfig context for destination cluster https://unknown.example.com")
	_, err = d.RESTConfig(t.Context(), nil)
	require.Error(t, err)
}
