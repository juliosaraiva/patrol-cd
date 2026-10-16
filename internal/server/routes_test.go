package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// routesHandler mounts the routes service on a route table with authentication disabled
// (as with --disable-auth) and returns the handler.
func routesHandler(t *testing.T, r *routesService) http.Handler {
	t.Helper()
	tbl := rest.NewTable()
	r.Register(tbl)
	require.NoError(t, tbl.Err())
	h, err := tbl.Handler(&rest.Env{DisableAuth: true, Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	return h
}

func get(t *testing.T, h http.Handler, path string, hdr ...string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func body(t *testing.T, r *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	return string(b)
}

func TestExtensionsBundle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "b", "extension-b.js"), "console.log('b')")
	writeFile(t, filepath.Join(dir, "a", "extension-a.js"), "console.log('a')")
	writeFile(t, filepath.Join(dir, "a", "helper.js"), "not included")
	writeFile(t, filepath.Join(dir, "a", "extension.css"), "not included")
	outside := filepath.Join(t.TempDir(), "extension-evil.js")
	writeFile(t, outside, "evil()")
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "extension-link.js")))

	h := routesHandler(t, &routesService{extensionsDir: dir, log: slog.New(slog.DiscardHandler)})
	resp := get(t, h, "/extensions.js")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/javascript; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	got := body(t, resp)
	want := "// source: a/extension-a.js\ntry {\nconsole.log('a')\n} catch(e) { console.error('Extension extension-a.js failed to load:', e); }\n" +
		"// source: b/extension-b.js\ntry {\nconsole.log('b')\n} catch(e) { console.error('Extension extension-b.js failed to load:', e); }\n"
	assert.Equal(t, want, got)
	assert.NotContains(t, got, "evil", "symbolic links are not followed")

	missing := routesHandler(t, &routesService{extensionsDir: filepath.Join(dir, "nope"), log: slog.New(slog.DiscardHandler)})
	resp = get(t, missing, "/extensions.js")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, body(t, resp))
	assert.Equal(t, "text/javascript; charset=utf-8", resp.Header.Get("Content-Type"))
}

func TestExtensionStaticAssets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "metrics", "img", "logo.svg"), "<svg/>")
	writeFile(t, filepath.Join(dir, "metrics", "app.css"), "body{}")
	writeFile(t, filepath.Join(dir, "metrics", ".secret"), "hidden")
	secret := filepath.Join(t.TempDir(), "secret.txt")
	writeFile(t, secret, "outside")
	require.NoError(t, os.Symlink(secret, filepath.Join(dir, "metrics", "link.txt")))
	require.NoError(t, os.Symlink(filepath.Dir(secret), filepath.Join(dir, "metrics", "linkdir")))

	proxied := 0
	proxy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied++
		w.WriteHeader(http.StatusTeapot)
	})
	ext := apiserver.ServiceFunc(func(t *rest.Table) {
		t.Add(rest.Route{Method: http.MethodGet, Path: apiclient.PathExtension, OperationID: "invokeExtensionGet", Access: rest.Public(), Handler: proxy})
	})
	h := routesHandler(t, &routesService{extensions: ext, extensionsDir: dir, log: slog.New(slog.DiscardHandler)})

	resp := get(t, h, "/extensions/metrics/img/logo.svg")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "image/svg+xml", resp.Header.Get("Content-Type"))
	assert.Equal(t, "<svg/>", body(t, resp))
	resp = get(t, h, "/extensions/metrics/app.css")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css"))

	for _, p := range []string{
		"/extensions/metrics/.secret",
		"/extensions/metrics/link.txt",
		"/extensions/metrics/linkdir/secret.txt",
		"/extensions/metrics/%2e%2e/%2e%2e/etc/passwd",
		"/extensions/metrics/missing.js",
	} {
		before := proxied
		resp := get(t, h, p)
		assert.Equal(t, http.StatusTeapot, resp.StatusCode, "%s falls through to the proxy", p)
		assert.Equal(t, before+1, proxied, p)
	}
	// A proxy call (with the Application header) never serves files.
	resp = get(t, h, "/extensions/metrics/app.css", apiclient.HeaderExtensionApplication, "patrol:guestbook")
	assert.Equal(t, http.StatusTeapot, resp.StatusCode)
}

func TestDownloads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "patrol-linux-"+runtime.GOARCH), "linux-binary")
	writeFile(t, filepath.Join(dir, "patrol-darwin-arm64"), "darwin-binary")
	writeFile(t, filepath.Join(dir, "README"), "not a binary")
	h := routesHandler(t, &routesService{downloadDir: dir, log: slog.New(slog.DiscardHandler)})

	resp := get(t, h, "/download/patrol-darwin-arm64")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, `attachment; filename=patrol-darwin-arm64`, resp.Header.Get("Content-Disposition"))
	assert.Equal(t, "darwin-binary", body(t, resp))

	resp = get(t, h, "/download/patrol-linux")
	require.Equal(t, http.StatusOK, resp.StatusCode, "alias of the server architecture")
	assert.Equal(t, "linux-binary", body(t, resp))

	for _, p := range []string{"/download/README", "/download/patrol-windows-amd64.exe", "/download/..%2fREADME", "/download/patrol-linux-..%2f.."} {
		resp := get(t, h, p)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, p)
	}
}

func TestWebhookRoute(t *testing.T) {
	t.Parallel()
	var paths []string
	wh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
	})
	h := routesHandler(t, &routesService{webhook: wh, log: slog.New(slog.DiscardHandler)})
	for _, p := range []string{"/api/webhook", "/api/webhook/github"} {
		req := httptest.NewRequest(http.MethodPost, p, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusAccepted, rec.Code, p)
	}
	assert.Equal(t, []string{"POST /api/webhook", "POST /api/webhook/github"}, paths)
}
