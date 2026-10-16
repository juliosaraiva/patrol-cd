package reposerver

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/render/cmpwire"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// startFakePlugin serves a minimal cmpwire sidecar without discovery on
// <dir>/<name>.sock: generate returns one ConfigMap, parameters one announcement.
func startFakePlugin(t *testing.T, dir, name string) *atomic.Int32 {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(dir, name+cmpwire.SocketSuffix))
	require.NoError(t, err)
	var generated atomic.Int32
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", cmpwire.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET "+cmpwire.PathConfig, func(w http.ResponseWriter, _ *http.Request) {
		reply(w, cmpwire.ConfigResponse{})
	})
	mux.HandleFunc("POST "+cmpwire.PathGenerate, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		generated.Add(1)
		reply(w, cmpwire.ManifestResponse{Manifests: []string{configMap("from-plugin", nil)}})
	})
	mux.HandleFunc("POST "+cmpwire.PathParameters, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		reply(w, cmpwire.ParametersResponse{ParameterAnnouncements: []apiclient.ParameterAnnouncement{{Name: "replicas", String: "1", Title: "Replicas"}}})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &generated
}

func TestPluginSource(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t, "pcmp")
	generated := startFakePlugin(t, dir, "fake")
	r := newRemote(t)
	r.commit("init", map[string]string{"app/input.txt": "x"})
	s := newTestService(t, func(c *Config) { c.PluginSocketDir = dir })
	c := startServer(t, s)
	src := v1alpha1.ApplicationSource{RepoURL: r.url(), Path: "app", TargetRevision: "main", Plugin: &v1alpha1.ApplicationSourcePlugin{Name: "fake"}}

	resp, err := c.GenerateManifest(bg(), &apiclient.ManifestRequest{Repo: r.repo(), AppName: "app", Source: src})
	require.NoError(t, err)
	assert.Equal(t, "Plugin", resp.SourceType)
	assert.Equal(t, []string{"ConfigMap/from-plugin"}, names(objects(t, resp)))
	assert.Equal(t, int32(1), generated.Load())

	d, err := c.GetAppDetails(bg(), &apiclient.AppDetailsQuery{Repo: r.repo(), Source: src, AppName: "app"})
	require.NoError(t, err)
	assert.Equal(t, "Plugin", d.Type)
	require.NotNil(t, d.Plugin)
	assert.Equal(t, []apiclient.ParameterAnnouncement{{Name: "replicas", String: "1", Title: "Replicas"}}, d.Plugin.ParametersAnnouncement)

	list, err := c.ListPlugins(bg())
	require.NoError(t, err)
	assert.Equal(t, []apiclient.PluginInfo{{Name: "fake"}}, list.Items)
}
