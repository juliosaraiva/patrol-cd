package generators

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

type pluginServer struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
	status   int
	body     string
	delay    time.Duration
}

func (s *pluginServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, r)
	s.bodies = append(s.bodies, string(b))
	status, body, delay := s.status, s.body, s.delay
	s.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func pluginGenerator(t *testing.T, srv *httptest.Server, cm map[string]string, cfg PluginConfig) *PluginGenerator {
	t.Helper()
	data := map[string]string{"baseUrl": srv.URL, "token": "$plugin.token"}
	for k, v := range cm {
		data[k] = v
	}
	kube := fakeClient(t,
		configMap(controlNS, "plugin", data),
		secret(controlNS, "patrol-secret", nil, map[string]string{"plugin.token": " s3cr3t \n"}),
		secret(controlNS, "custom", nil, map[string]string{"key": "other"}),
	)
	return &PluginGenerator{cfg: Config{Client: kube, Namespace: controlNS, Plugin: cfg}}
}

func pluginGen(params map[string]any) *v1alpha1.ApplicationSetGenerator {
	pp := v1alpha1.PluginParameters{}
	for k, v := range params {
		b, _ := json.Marshal(v)
		pp[k] = apiextensionsv1.JSON{Raw: b}
	}
	return &v1alpha1.ApplicationSetGenerator{Plugin: &v1alpha1.PluginGenerator{
		ConfigMapRef: v1alpha1.PluginConfigMapRef{Name: "plugin"},
		Input:        v1alpha1.PluginInput{Parameters: pp},
		Values:       map[string]string{"v": "x"},
	}}
}

func TestPluginGenerator(t *testing.T) {
	t.Parallel()
	ps := &pluginServer{body: `{"output":{"parameters":[{"name":"a","nested":{"k":"v"},"n":1}]}}`}
	srv := httptest.NewServer(ps)
	t.Cleanup(srv.Close)
	g := pluginGenerator(t, srv, nil, PluginConfig{})
	gen := pluginGen(map[string]any{"env": "prod", "list": []any{"x"}})

	got, err := g.GenerateParams(context.Background(), gen, appSet(false))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "a", got[0]["name"])
	assert.Equal(t, "v", got[0]["nested.k"], "fasttemplate parameters are flattened")
	assert.Equal(t, "1", got[0]["n"])
	assert.Equal(t, "x", got[0]["values.v"])
	assert.Equal(t, map[string]any{"input": map[string]any{"parameters": map[string]any{"env": "prod", "list": []any{"x"}}}}, got[0]["generator"])

	ps.mu.Lock()
	req := ps.requests[0]
	body := ps.bodies[0]
	ps.mu.Unlock()
	assert.Equal(t, http.MethodPost, req.Method)
	assert.Equal(t, "/api/v1/getparams.execute", req.URL.Path)
	assert.Equal(t, "Bearer s3cr3t", req.Header.Get("Authorization"), "the token is trimmed")
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
	assert.JSONEq(t, `{"applicationSetName":"set","input":{"parameters":{"env":"prod","list":["x"]}}}`, body)

	got, err = g.GenerateParams(context.Background(), gen, appSet(true))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"k": "v"}, got[0]["nested"], "goTemplate keeps objects")
	assert.Equal(t, map[string]string{"v": "x"}, got[0]["values"])

	assert.Equal(t, DefaultPluginRequeueAfter, g.RequeueAfter(gen))
	gen.Plugin.RequeueAfterSeconds = ptr.To[int64](60)
	assert.Equal(t, time.Minute, g.RequeueAfter(gen))
}

func TestPluginGeneratorTokenReferences(t *testing.T) {
	t.Parallel()
	ps := &pluginServer{body: `{"output":{"parameters":[]}}`}
	srv := httptest.NewServer(ps)
	t.Cleanup(srv.Close)

	g := pluginGenerator(t, srv, map[string]string{"token": "$custom:key"}, PluginConfig{})
	_, err := g.GenerateParams(context.Background(), pluginGen(nil), appSet(false))
	require.NoError(t, err)
	ps.mu.Lock()
	assert.Equal(t, "Bearer other", ps.requests[0].Header.Get("Authorization"))
	ps.mu.Unlock()

	g = pluginGenerator(t, srv, map[string]string{"token": "literal"}, PluginConfig{})
	_, err = g.GenerateParams(context.Background(), pluginGen(nil), appSet(false))
	require.ErrorContains(t, err, "does not reference a secret key starting with '$'")

	g = pluginGenerator(t, srv, map[string]string{"token": "$missing:key"}, PluginConfig{})
	_, err = g.GenerateParams(context.Background(), pluginGen(nil), appSet(false))
	require.ErrorContains(t, err, "error fetching Secret token")

	g = pluginGenerator(t, srv, map[string]string{"baseUrl": ""}, PluginConfig{})
	_, err = g.GenerateParams(context.Background(), pluginGen(nil), appSet(false))
	require.ErrorContains(t, err, "baseUrl not found in ConfigMap")
}

func TestPluginGeneratorFailures(t *testing.T) {
	t.Parallel()
	t.Run("error status", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(&pluginServer{status: http.StatusForbidden, body: `{"message":"denied"}`})
		t.Cleanup(srv.Close)
		_, err := pluginGenerator(t, srv, nil, PluginConfig{}).GenerateParams(context.Background(), pluginGen(nil), appSet(false))
		require.ErrorContains(t, err, "API error with status code 403: denied")
	})
	t.Run("invalid body", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(&pluginServer{body: `not json`})
		t.Cleanup(srv.Close)
		_, err := pluginGenerator(t, srv, nil, PluginConfig{}).GenerateParams(context.Background(), pluginGen(nil), appSet(false))
		require.ErrorContains(t, err, "error listing params")
	})
	t.Run("response too large", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(&pluginServer{body: `{"output":{"parameters":[{"a":"` + strings.Repeat("x", 200) + `"}]}}`})
		t.Cleanup(srv.Close)
		_, err := pluginGenerator(t, srv, nil, PluginConfig{MaxResponseBytes: 64}).GenerateParams(context.Background(), pluginGen(nil), appSet(false))
		require.ErrorContains(t, err, "plugin response exceeds 64 bytes")
	})
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		ps := &pluginServer{body: `{}`, delay: 2 * time.Second}
		srv := httptest.NewServer(ps)
		t.Cleanup(srv.Close)
		start := time.Now()
		_, err := pluginGenerator(t, srv, map[string]string{"requestTimeout": "1"}, PluginConfig{}).GenerateParams(context.Background(), pluginGen(nil), appSet(false))
		require.Error(t, err)
		assert.Less(t, time.Since(start), 1900*time.Millisecond)
	})
	t.Run("invalid timeout", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(&pluginServer{body: `{}`})
		t.Cleanup(srv.Close)
		_, err := pluginGenerator(t, srv, map[string]string{"requestTimeout": "soon"}, PluginConfig{}).GenerateParams(context.Background(), pluginGen(nil), appSet(false))
		require.ErrorContains(t, err, "error set requestTimeout")
	})
	t.Run("empty body", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(&pluginServer{body: ``})
		t.Cleanup(srv.Close)
		got, err := pluginGenerator(t, srv, nil, PluginConfig{}).GenerateParams(context.Background(), pluginGen(nil), appSet(false))
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}
