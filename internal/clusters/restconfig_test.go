package clusters

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func inClusterStub() (*rest.Config, error) {
	return &rest.Config{
		Host:            "https://10.0.0.1:443",
		BearerTokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token",
		TLSClientConfig: rest.TLSClientConfig{CAFile: "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"},
	}, nil
}

// TestRawRestConfigPrecedence covers REPO-091: the in-cluster cases, then AWS, exec and
// token/basic authentication, each with TLS settings.
func TestRawRestConfigPrecedence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	allAuth := func(c *v1alpha1.Cluster) {
		c.Spec.Config = v1alpha1.ClusterConfig{
			TLSClientConfig:    v1alpha1.TLSClientConfig{Insecure: true, ServerName: "sni.example"},
			AWSAuthConfig:      &v1alpha1.AWSAuthConfig{ClusterName: "eks-1", RoleARN: "arn:aws:iam::1:role/r", Profile: "p"},
			ExecProviderConfig: &v1alpha1.ExecProviderConfig{Command: "plugin", Args: []string{"a"}},
		}
	}
	secret := map[string][]byte{
		v1alpha1.ClusterSecretKeyBearerToken: []byte("tok"),
		v1alpha1.ClusterSecretKeyCAData:      []byte("ca"),
		v1alpha1.ClusterSecretKeyCertData:    []byte("cert"),
		v1alpha1.ClusterSecretKeyKeyData:     []byte("key"),
	}
	certAuth := map[string][]byte{
		v1alpha1.ClusterSecretKeyCAData:   []byte("ca"),
		v1alpha1.ClusterSecretKeyCertData: []byte("cert"),
		v1alpha1.ClusterSecretKeyKeyData:  []byte("key"),
	}
	opts := Options{InClusterConfig: inClusterStub, AWSAuthExec: true}
	tests := []struct {
		name   string
		c      *v1alpha1.Cluster
		data   map[string][]byte
		opts   Options
		assert func(t *testing.T, cfg *rest.Config)
	}{
		{
			name: "in-cluster without credentials uses the pod identity",
			c:    cluster("in", InClusterServer, allAuth),
			opts: opts,
			assert: func(t *testing.T, cfg *rest.Config) {
				assert.Equal(t, "https://10.0.0.1:443", cfg.Host)
				assert.Equal(t, "/var/run/secrets/kubernetes.io/serviceaccount/token", cfg.BearerTokenFile)
				assert.Empty(t, cfg.BearerToken)
				assert.Nil(t, cfg.ExecProvider, "AWS and exec settings are ignored in-cluster")
				assert.Nil(t, cfg.WrapTransport)
				assert.False(t, cfg.Insecure, "TLS settings are ignored in-cluster")
				assert.Empty(t, cfg.ServerName)
			},
		},
		{
			name: "in-cluster with a trailing slash is still in-cluster",
			c:    cluster("in", InClusterServer+"/"),
			opts: opts,
			assert: func(t *testing.T, cfg *rest.Config) {
				assert.Equal(t, "https://10.0.0.1:443", cfg.Host)
			},
		},
		{
			name: "in-cluster token overrides the service account token",
			c:    cluster("in", InClusterServer, allAuth),
			data: secret,
			opts: opts,
			assert: func(t *testing.T, cfg *rest.Config) {
				assert.Equal(t, "tok", cfg.BearerToken)
				assert.Empty(t, cfg.BearerTokenFile)
				assert.Empty(t, cfg.CAData, "secret CA is ignored in-cluster")
				assert.Equal(t, "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", cfg.CAFile)
			},
		},
		{
			name: "in-cluster basic auth overrides the service account token",
			c:    cluster("in", InClusterServer),
			data: map[string][]byte{"username": []byte("u"), "password": []byte("p")},
			opts: opts,
			assert: func(t *testing.T, cfg *rest.Config) {
				assert.Equal(t, "u", cfg.Username)
				assert.Equal(t, "p", cfg.Password)
				assert.Empty(t, cfg.BearerTokenFile)
			},
		},
		{
			name: "AWS takes precedence over exec and token",
			c:    cluster("eks", "https://eks.example", allAuth),
			data: secret,
			opts: opts,
			assert: func(t *testing.T, cfg *rest.Config) {
				require.NotNil(t, cfg.ExecProvider)
				assert.Equal(t, "patrolcd", cfg.ExecProvider.Command)
				assert.Equal(t, []string{"k8s-auth", "aws", "--cluster-name", "eks-1", "--role-arn", "arn:aws:iam::1:role/r", "--profile", "p"}, cfg.ExecProvider.Args)
				assert.Equal(t, ExecCredentialAPIVersion, cfg.ExecProvider.APIVersion)
				assert.Equal(t, clientcmdapi.NeverExecInteractiveMode, cfg.ExecProvider.InteractiveMode)
				assert.Empty(t, cfg.BearerToken)
				assert.True(t, cfg.Insecure)
				assert.Equal(t, "sni.example", cfg.ServerName)
				assert.Equal(t, []byte("ca"), cfg.CAData)
				assert.Equal(t, []byte("cert"), cfg.CertData)
				assert.Equal(t, []byte("key"), cfg.KeyData)
			},
		},
		{
			name: "AWS native mode uses an in-process token source",
			c:    cluster("eks", "https://eks.example", allAuth),
			opts: Options{Providers: mustProviderCache(t)},
			assert: func(t *testing.T, cfg *rest.Config) {
				assert.Nil(t, cfg.ExecProvider)
				assert.NotNil(t, cfg.WrapTransport)
			},
		},
		{
			name: "AWS native mode with a client certificate keeps the inert exec form",
			c:    cluster("eks", "https://eks.example", allAuth),
			data: certAuth,
			opts: Options{Providers: mustProviderCache(t)},
			assert: func(t *testing.T, cfg *rest.Config) {
				assert.Nil(t, cfg.WrapTransport, "client-go skips exec plugins with cert auth; no token is injected")
				require.NotNil(t, cfg.ExecProvider)
				assert.Equal(t, []string{"k8s-auth", "aws", "--cluster-name", "eks-1", "--role-arn", "arn:aws:iam::1:role/r", "--profile", "p"}, cfg.ExecProvider.Args)
				assert.Equal(t, []byte("cert"), cfg.CertData)
				assert.Equal(t, []byte("key"), cfg.KeyData)
			},
		},
		{
			name: "AWS native mode with a certificate but no key uses the token source",
			c:    cluster("eks", "https://eks.example", allAuth),
			data: map[string][]byte{v1alpha1.ClusterSecretKeyCertData: []byte("cert")},
			opts: Options{Providers: mustProviderCache(t)},
			assert: func(t *testing.T, cfg *rest.Config) {
				assert.NotNil(t, cfg.WrapTransport)
				assert.Nil(t, cfg.ExecProvider)
			},
		},
		{
			name: "k8s-auth helper exec with a client certificate keeps the exec form",
			c: cluster("gke", "https://gke.example", func(c *v1alpha1.Cluster) {
				c.Spec.Config.ExecProviderConfig = &v1alpha1.ExecProviderConfig{Command: "argocd-k8s-auth", Args: []string{"gcp"}}
			}),
			data: certAuth,
			opts: Options{Providers: mustProviderCache(t)},
			assert: func(t *testing.T, cfg *rest.Config) {
				assert.Nil(t, cfg.WrapTransport)
				require.NotNil(t, cfg.ExecProvider)
				assert.Equal(t, "argocd-k8s-auth", cfg.ExecProvider.Command)
				assert.Equal(t, []byte("cert"), cfg.CertData)
			},
		},
		{
			name: "exec takes precedence over token",
			c: cluster("exec", "https://exec.example", func(c *v1alpha1.Cluster) {
				c.Spec.Config.ExecProviderConfig = &v1alpha1.ExecProviderConfig{
					Command: "plugin", Args: []string{"x"}, Env: map[string]string{"B": "2", "A": "1"},
					APIVersion: "client.authentication.k8s.io/v1", InstallHint: "install it", ProvideClusterInfo: true,
					Config: &runtime.RawExtension{Raw: []byte(`{"clusterName":"c1"}`)},
				}
			}),
			data: secret,
			opts: opts,
			assert: func(t *testing.T, cfg *rest.Config) {
				require.NotNil(t, cfg.ExecProvider)
				e := cfg.ExecProvider
				assert.Equal(t, "plugin", e.Command)
				assert.Equal(t, []string{"x"}, e.Args)
				assert.Equal(t, []clientcmdapi.ExecEnvVar{{Name: "A", Value: "1"}, {Name: "B", Value: "2"}}, e.Env)
				assert.Equal(t, "client.authentication.k8s.io/v1", e.APIVersion)
				assert.Equal(t, "install it", e.InstallHint)
				assert.True(t, e.ProvideClusterInfo)
				assert.Equal(t, clientcmdapi.NeverExecInteractiveMode, e.InteractiveMode)
				require.IsType(t, &runtime.Unknown{}, e.Config)
				assert.JSONEq(t, `{"clusterName":"c1"}`, string(e.Config.(*runtime.Unknown).Raw))
				assert.Empty(t, cfg.BearerToken)
				assert.Equal(t, []byte("ca"), cfg.CAData)
			},
		},
		{
			name: "bearer token with TLS",
			c:    cluster("tok", "https://tok.example/"),
			data: secret,
			opts: opts,
			assert: func(t *testing.T, cfg *rest.Config) {
				assert.Equal(t, "https://tok.example", cfg.Host)
				assert.Equal(t, "tok", cfg.BearerToken)
				assert.Nil(t, cfg.ExecProvider)
				assert.Equal(t, []byte("ca"), cfg.CAData)
			},
		},
		{
			name: "basic auth",
			c:    cluster("basic", "https://basic.example"),
			data: map[string][]byte{"username": []byte("admin"), "password": []byte("pw")},
			opts: opts,
			assert: func(t *testing.T, cfg *rest.Config) {
				assert.Equal(t, "admin", cfg.Username)
				assert.Equal(t, "pw", cfg.Password)
			},
		},
		{
			name: "proxy, compression and tuning limits",
			c: cluster("p", "https://p.example", func(c *v1alpha1.Cluster) {
				c.Spec.Config.ProxyURL = "socks5://proxy.example:1080"
				c.Spec.Config.DisableCompression = true
			}),
			opts: Options{Tuning: &Tuning{QPS: 7, Burst: 9, ServerSideTimeout: 3 * time.Second}, UserAgent: "patrol-test"},
			assert: func(t *testing.T, cfg *rest.Config) {
				require.NotNil(t, cfg.Proxy)
				u, err := cfg.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "p.example"}})
				require.NoError(t, err)
				assert.Equal(t, "socks5://proxy.example:1080", u.String())
				assert.True(t, cfg.DisableCompression)
				assert.InDelta(t, 7, cfg.QPS, 0.001)
				assert.Equal(t, 9, cfg.Burst)
				assert.Equal(t, 3*time.Second, cfg.Timeout)
				assert.Equal(t, "patrol-test", cfg.UserAgent)
			},
		},
		{
			name: "default limits",
			c:    cluster("d", "https://d.example"),
			assert: func(t *testing.T, cfg *rest.Config) {
				assert.InDelta(t, 50, cfg.QPS, 0.001)
				assert.Equal(t, 100, cfg.Burst)
				assert.Zero(t, cfg.Timeout)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := RawRestConfig(ctx, tc.c, tc.data, tc.opts)
			require.NoError(t, err)
			tc.assert(t, cfg)
		})
	}
}

func mustProviderCache(t *testing.T) *ProviderCache {
	t.Helper()
	c, err := NewProviderCache(16)
	require.NoError(t, err)
	return c
}

func TestAWSExecArgsEmptyClusterName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"aws", "--cluster-name", ""}, AWSExecArgs(v1alpha1.AWSAuthConfig{}))
}

func TestRawRestConfigErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, err := RawRestConfig(ctx, nil, nil, Options{})
	require.ErrorIs(t, err, ErrInvalidConfig)

	c := cluster("p", "https://p.example", func(c *v1alpha1.Cluster) { c.Spec.Config.ProxyURL = "socks5h://proxy:1080" })
	_, err = RawRestConfig(ctx, c, nil, Options{})
	require.EqualError(t, err, "unable to create K8s REST config, can`t parse proxy url: failed to parse proxy url, unsupported scheme \"socks5h\", must be http, https, or socks5")

	_, err = RawRestConfig(ctx, cluster("in", InClusterServer), nil, Options{InClusterConfig: func() (*rest.Config, error) {
		return nil, rest.ErrNotInCluster
	}})
	require.ErrorIs(t, err, rest.ErrNotInCluster)
	require.ErrorContains(t, err, "unable to create K8s REST config: ")

	_, err = RawRestConfig(ctx, cluster("in", InClusterServer), nil, Options{InClusterConfig: func() (*rest.Config, error) {
		return nil, nil // a misbehaving hook
	}})
	require.ErrorIs(t, err, ErrInvalidConfig)

	for _, scheme := range []string{"http", "https", "socks5"} {
		_, err := ParseProxyURL(scheme + "://proxy:3128")
		require.NoError(t, err, scheme)
	}
}

// TestRestConfigDoesNotMutateInClusterConfig: a shared configuration returned by
// Options.InClusterConfig is copied before the proxy, limits and tuned transport are applied,
// so repeated calls keep working.
func TestRestConfigDoesNotMutateInClusterConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := newTLSAPIServer(t)
	shared := &rest.Config{
		Host:            srv.URL,
		BearerToken:     "pod-token",
		QPS:             5,
		Burst:           10,
		UserAgent:       "manager",
		TLSClientConfig: rest.TLSClientConfig{CAData: srv.caPEM()},
	}
	want := rest.CopyConfig(shared)
	opts := Options{InClusterConfig: func() (*rest.Config, error) { return shared, nil }, UserAgent: "patrol-test"}

	proxied := cluster("in", InClusterServer, func(c *v1alpha1.Cluster) {
		c.Spec.Config.ProxyURL = "http://proxy.invalid:3128"
		c.Spec.Config.DisableCompression = true
	})
	for _, data := range []map[string][]byte{nil, {"bearerToken": []byte("override")}} {
		cfg, err := RawRestConfig(ctx, proxied, data, opts)
		require.NoError(t, err)
		assert.NotSame(t, shared, cfg)
		assert.InDelta(t, 50, cfg.QPS, 0.001)
		require.Equal(t, want, shared, "RawRestConfig modified the in-cluster configuration (secret data %v)", data != nil)
	}

	for i := range 2 {
		cfg, err := RestConfig(ctx, cluster("in", InClusterServer), nil, opts)
		require.NoError(t, err)
		ver, err := Probe(ctx, cfg)
		require.NoError(t, err, "call %d", i)
		assert.Equal(t, "v1.36.1", ver)
		assert.Equal(t, "Bearer pod-token", srv.lastHeader(t).Get("Authorization"))
		require.Equal(t, want, shared, "RestConfig modified the in-cluster configuration (call %d)", i)
	}
}

func TestFakeInClusterUsesLocalKubeconfig(t *testing.T) {
	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "config")
	require.NoError(t, os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
- name: dev
  cluster: {server: "https://dev.local:6443", insecure-skip-tls-verify: true}
users:
- name: dev
  user: {token: dev-token}
contexts:
- name: dev
  context: {cluster: dev, user: dev}
current-context: dev
`), 0o600))
	t.Setenv("KUBECONFIG", kubeconfig)
	t.Setenv(EnvFakeInCluster, "true")
	require.True(t, FakeInClusterFromEnv())
	cfg, err := RawRestConfig(context.Background(), cluster("in", InClusterServer), nil, Options{FakeInCluster: FakeInClusterFromEnv()})
	require.NoError(t, err)
	assert.Equal(t, "https://dev.local:6443", cfg.Host)
	assert.Equal(t, "dev-token", cfg.BearerToken)
}

// probeWith builds a tuned config and probes the server.
func probeWith(t *testing.T, c *v1alpha1.Cluster, data map[string][]byte, opts Options) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg, err := RestConfig(ctx, c, data, opts)
	require.NoError(t, err)
	return Probe(ctx, cfg)
}

// TestTLSSettingsAgainstServer covers REPO-086: custom CA, insecure, serverName and mTLS.
func TestTLSSettingsAgainstServer(t *testing.T) {
	t.Parallel()
	srv := newTLSAPIServer(t)

	t.Run("custom CA", func(t *testing.T) {
		t.Parallel()
		ver, err := probeWith(t, cluster("ca", srv.URL), map[string][]byte{"caData": srv.caPEM()}, Options{})
		require.NoError(t, err)
		assert.Equal(t, "v1.36.1", ver)
	})
	t.Run("unknown CA fails", func(t *testing.T) {
		t.Parallel()
		_, err := probeWith(t, cluster("ca", srv.URL), nil, Options{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "certificate")
	})
	t.Run("insecure skips verification", func(t *testing.T) {
		t.Parallel()
		_, err := probeWith(t, cluster("insecure", srv.URL, func(c *v1alpha1.Cluster) { c.Spec.Config.TLSClientConfig.Insecure = true }), nil, Options{})
		require.NoError(t, err)
	})
	t.Run("serverName override", func(t *testing.T) {
		t.Parallel()
		ok := cluster("sni", srv.URL, func(c *v1alpha1.Cluster) { c.Spec.Config.TLSClientConfig.ServerName = "example.com" })
		_, err := probeWith(t, ok, map[string][]byte{"caData": srv.caPEM()}, Options{})
		require.NoError(t, err)
		bad := cluster("sni", srv.URL, func(c *v1alpha1.Cluster) { c.Spec.Config.TLSClientConfig.ServerName = "other.test" })
		_, err = probeWith(t, bad, map[string][]byte{"caData": srv.caPEM()}, Options{})
		require.Error(t, err)
	})
	t.Run("mutual TLS", func(t *testing.T) {
		t.Parallel()
		pki := newTestPKI(t)
		mtls := newMTLSAPIServer(t, pki)
		certPEM, keyPEM := pki.issue(t, "patrol", true, nil, nil)
		data := map[string][]byte{"caData": pki.caPEM, "certData": certPEM, "keyData": keyPEM}
		_, err := probeWith(t, cluster("mtls", mtls.URL), data, Options{})
		require.NoError(t, err)
		_, err = probeWith(t, cluster("mtls", mtls.URL), map[string][]byte{"caData": pki.caPEM}, Options{})
		require.Error(t, err, "the server requires a client certificate")
	})
}

// TestAuthHeaders covers REPO-084 and REPO-085 against a server.
func TestAuthHeaders(t *testing.T) {
	t.Parallel()
	srv := newTLSAPIServer(t)
	insecure := func(c *v1alpha1.Cluster) { c.Spec.Config.TLSClientConfig.Insecure = true }

	_, err := probeWith(t, cluster("tok", srv.URL, insecure), map[string][]byte{"bearerToken": []byte("s3cr3t")}, Options{})
	require.NoError(t, err)
	assert.Equal(t, "Bearer s3cr3t", srv.lastHeader(t).Get("Authorization"))

	_, err = probeWith(t, cluster("basic", srv.URL, insecure), map[string][]byte{"username": []byte("admin"), "password": []byte("pw")}, Options{})
	require.NoError(t, err)
	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("admin:pw")), srv.lastHeader(t).Get("Authorization"))
}

// TestDisableCompression covers REPO-089 for the tuned and the raw configuration.
func TestDisableCompression(t *testing.T) {
	t.Parallel()
	srv := newTLSAPIServer(t)
	data := map[string][]byte{"caData": srv.caPEM()}
	for _, disable := range []bool{false, true} {
		c := cluster("gz", srv.URL, func(c *v1alpha1.Cluster) { c.Spec.Config.DisableCompression = disable })
		want := "gzip"
		if disable {
			want = ""
		}
		_, err := probeWith(t, c, data, Options{})
		require.NoError(t, err)
		assert.Equal(t, want, srv.lastHeader(t).Get("Accept-Encoding"), "tuned, disable=%t", disable)

		cfg, err := RawRestConfig(context.Background(), c, data, Options{})
		require.NoError(t, err)
		_, err = Probe(context.Background(), cfg)
		require.NoError(t, err)
		assert.Equal(t, want, srv.lastHeader(t).Get("Accept-Encoding"), "raw, disable=%t", disable)
	}
}

// TestProxyURL covers REPO-090: requests go through the configured HTTP proxy.
func TestProxyURL(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.String())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"major":"1","minor":"36","gitVersion":"v1.36.0"}`))
	}))
	t.Cleanup(proxy.Close)
	c := cluster("proxied", "http://cluster.invalid:8080", func(c *v1alpha1.Cluster) { c.Spec.Config.ProxyURL = proxy.URL })
	ver, err := probeWith(t, c, nil, Options{})
	require.NoError(t, err)
	assert.Equal(t, "v1.36.0", ver)
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, seen)
	u, err := url.Parse(seen[len(seen)-1])
	require.NoError(t, err)
	assert.Equal(t, "cluster.invalid:8080", u.Host)
	assert.Equal(t, "/version", u.Path)
}

func execPluginPath(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	return exe
}

func decodePluginToken(t *testing.T, header string) pluginReport {
	t.Helper()
	tok, ok := strings.CutPrefix(header, "Bearer ")
	require.True(t, ok, "authorization header %q", header)
	b, err := base64.RawURLEncoding.DecodeString(tok)
	require.NoError(t, err)
	var rep pluginReport
	require.NoError(t, json.Unmarshal(b, &rep))
	return rep
}

// TestExecProviderPlugin covers REPO-088: the plugin receives args and env, and the cluster
// config when provideClusterInfo is set.
func TestExecProviderPlugin(t *testing.T) {
	t.Parallel()
	srv := newTLSAPIServer(t)
	c := cluster("exec", srv.URL, func(c *v1alpha1.Cluster) {
		c.Spec.Config.TLSClientConfig.Insecure = true
		c.Spec.Config.ExecProviderConfig = &v1alpha1.ExecProviderConfig{
			Command:            execPluginPath(t),
			Args:               []string{"--flag", "value"},
			Env:                map[string]string{envExecPlugin: "1", "FOO": "bar"},
			APIVersion:         ExecCredentialAPIVersion,
			ProvideClusterInfo: true,
			Config:             &runtime.RawExtension{Raw: []byte(`{"clusterName":"prod-1"}`)},
		}
	})
	_, err := probeWith(t, c, nil, Options{})
	require.NoError(t, err)
	rep := decodePluginToken(t, srv.lastHeader(t).Get("Authorization"))
	assert.Equal(t, []string{"--flag", "value"}, rep.Args)
	assert.Equal(t, "bar", rep.Foo)
	assert.Equal(t, srv.URL, rep.ClusterServer)
	assert.JSONEq(t, `{"clusterName":"prod-1"}`, string(rep.ClusterConfig))
}

// TestAWSExecModePlugin covers REPO-087's exec form: `patrolcd k8s-auth aws ...` is executed
// with exactly the upstream arguments.
func TestAWSExecModePlugin(t *testing.T) {
	t.Setenv(envExecPlugin, "1")
	srv := newTLSAPIServer(t)
	c := cluster("eks", srv.URL, func(c *v1alpha1.Cluster) {
		c.Spec.Config.TLSClientConfig.Insecure = true
		c.Spec.Config.AWSAuthConfig = &v1alpha1.AWSAuthConfig{ClusterName: "eks-prod", RoleARN: "arn:aws:iam::123:role/deployer"}
	})
	_, err := probeWith(t, c, nil, Options{AWSAuthExec: true, K8sAuthCommand: execPluginPath(t)})
	require.NoError(t, err)
	rep := decodePluginToken(t, srv.lastHeader(t).Get("Authorization"))
	assert.Equal(t, []string{"k8s-auth", "aws", "--cluster-name", "eks-prod", "--role-arn", "arn:aws:iam::123:role/deployer"}, rep.Args)
}

// TestNativeK8sAuthRecognition checks which exec configurations run in-process.
func TestNativeK8sAuthRecognition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		command string
		args    []string
		native  bool
	}{
		{"patrolcd", []string{"k8s-auth", "gcp"}, true},
		{"/usr/local/bin/patrolcd", []string{"k8s-auth", "azure"}, true},
		{"argocd-k8s-auth", []string{"gcp"}, true},
		{"argocd-k8s-auth", []string{"aws", "--cluster-name", "x", "--role-arn=arn"}, true},
		{"argocd-k8s-auth", []string{"aws", "--unknown", "x"}, false},
		{"argocd-k8s-auth", []string{"gcp", "--extra"}, false},
		{"argocd-k8s-auth", []string{"oci"}, false},
		{"patrolcd", []string{"controller"}, false},
		{"kubelogin", []string{"get-token"}, false},
	}
	for _, tc := range tests {
		c := cluster("x", "https://x.example", func(c *v1alpha1.Cluster) {
			c.Spec.Config.ExecProviderConfig = &v1alpha1.ExecProviderConfig{Command: tc.command, Args: tc.args}
		})
		cfg, err := RawRestConfig(context.Background(), c, nil, Options{Providers: mustProviderCache(t)})
		require.NoError(t, err, tc)
		assert.Equal(t, tc.native, cfg.WrapTransport != nil, "%v", tc)
		assert.Equal(t, !tc.native, cfg.ExecProvider != nil, "%v", tc)

		cfg, err = RawRestConfig(context.Background(), c, nil, Options{DisableNativeK8sAuth: true})
		require.NoError(t, err)
		assert.NotNil(t, cfg.ExecProvider, "disabled native auth always executes the plugin")
	}
}

func TestCredentialsDoNotLeak(t *testing.T) {
	t.Parallel()
	c := CredentialsFromSecretData(map[string][]byte{"bearerToken": []byte("top-secret"), "password": []byte("pw-secret"), "keyData": []byte("KEY")})
	for _, s := range []string{c.String(), c.GoString()} {
		assert.NotContains(t, s, "top-secret")
		assert.NotContains(t, s, "pw-secret")
		assert.NotContains(t, s, "KEY")
	}
	assert.Equal(t, c, CredentialsFromSecretData(c.SecretData()))
	tok := Token{Value: "secret-token", Expiry: time.Unix(0, 0)}
	assert.NotContains(t, tok.String(), "secret-token")
}
