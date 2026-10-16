package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Routes outside the resource services: UI extension bundles, CLI downloads and SCM
// webhooks. They are raw routes of the route table so they share the process middleware
// (recovery, request IDs, access log, security headers, body cap), the root path and the
// 404/405 handling.
const (
	// PathExtensionsJS concatenates the UI extension bundles (upstream /extensions.js).
	PathExtensionsJS = "/extensions.js"
	// PathDownload serves CLI binaries.
	PathDownload = "/download/{file}"
	// PathWebhook receives SCM webhooks; a further segment names the provider.
	PathWebhook = "/api/webhook"
	// maxExtensionsBundleBytes bounds the concatenated extension bundle.
	maxExtensionsBundleBytes = 64 << 20
	// cliBinaryName is the CLI binary looked up in PATH when no download directory has it.
	cliBinaryName = "patrol"
)

// extensionFileRE matches the files concatenated into /extensions.js (upstream pattern).
var extensionFileRE = regexp.MustCompile(`^extension(.*)\.js$`)

// downloadFileRE matches downloadable CLI binaries: patrol-<os>-<arch>[.exe].
var downloadFileRE = regexp.MustCompile(`^patrol-[a-z0-9]+-[a-z0-9]+(\.exe)?$`)

// routesService registers the proxy extension routes (with static extension assets), the
// extension bundle, CLI downloads and the webhook endpoint.
type routesService struct {
	extensions    apiserver.Service
	webhook       http.Handler
	extensionsDir string
	downloadDir   string
	log           *slog.Logger
}

// Register implements apiserver.Service.
func (r *routesService) Register(t *rest.Table) {
	if r.extensions != nil {
		tmp := rest.NewTable()
		r.extensions.Register(tmp)
		if err := tmp.Err(); err != nil {
			r.log.Error("proxy extension routes are invalid; not registering them", "error", err)
		} else {
			for _, rt := range tmp.Routes() {
				if rt.Method == http.MethodGet && rt.Path == apiclient.PathExtension {
					rt.Handler = r.extensionAssets(rt.Handler)
				}
				t.Add(rt)
			}
		}
	}
	public := rest.Public()
	t.Add(rest.Route{
		Method: http.MethodGet, Path: PathExtensionsJS, OperationID: "getExtensionsBundle",
		Summary: "UI extension bundle", Access: public, SkipAuth: true, Hidden: true, Audit: rest.AuditNever,
		ResponseContentType: "text/javascript", Handler: http.HandlerFunc(r.extensionsJS),
	})
	t.Add(rest.Route{
		Method: http.MethodGet, Path: PathDownload, OperationID: "downloadCLI",
		Summary: "Download the CLI binary", Access: public, SkipAuth: true, Hidden: true, Audit: rest.AuditNever,
		ResponseContentType: "application/octet-stream", Handler: http.HandlerFunc(r.download),
	})
	if r.webhook != nil {
		for i, p := range []string{PathWebhook, PathWebhook + "/{provider...}"} {
			t.Add(rest.Route{
				Path: p, OperationID: "receiveWebhook" + strconv.Itoa(i), Summary: "SCM webhook",
				Access: public, SkipAuth: true, Hidden: true, Audit: rest.AuditNever, Handler: r.webhook,
			})
		}
	}
}

// extensionsJS concatenates every extension*.js file below the extensions directory (no
// symbolic links followed), each wrapped so a runtime error in one bundle does not stop the
// others (upstream API-050). A missing directory yields an empty script.
func (r *routesService) extensionsJS(w http.ResponseWriter, req *http.Request) {
	var buf bytes.Buffer
	if r.extensionsDir != "" {
		err := filepath.WalkDir(r.extensionsDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 || d.IsDir() || !d.Type().IsRegular() || !extensionFileRE.MatchString(d.Name()) {
				return nil
			}
			rel, err := filepath.Rel(r.extensionsDir, p)
			if err != nil {
				return err
			}
			data, err := readBounded(p, maxExtensionsBundleBytes-int64(buf.Len()))
			if err != nil {
				return err
			}
			fmt.Fprintf(&buf, "// source: %s\ntry {\n", filepath.ToSlash(rel))
			buf.Write(data)
			fmt.Fprintf(&buf, "\n} catch(e) { console.error('Extension %s failed to load:', e); }\n", jsSingleQuoted(d.Name()))
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			r.log.LogAttrs(req.Context(), slog.LevelError, "cannot build the extension bundle", slog.Any("error", err))
			rest.WriteError(w, req, rest.Internal(err))
			return
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/javascript; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	http.ServeContent(w, req, "extensions.js", time.Time{}, bytes.NewReader(buf.Bytes()))
}

// jsSingleQuoted escapes s for a single-quoted JavaScript string literal.
func jsSingleQuoted(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`, "\r", `\r`, "<", `\x3c`).Replace(s)
}

// readBounded reads a file of at most limit bytes.
func readBounded(p string, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("extension bundle exceeds %d bytes", maxExtensionsBundleBytes)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("extension bundle exceeds %d bytes", maxExtensionsBundleBytes)
	}
	return data, nil
}

// extensionAssets serves static files of the extensions directory for GET requests of the
// proxy extension route that are not proxy calls (proxy calls always carry the
// Patrol-Application-Name header), falling back to the proxy handler. The route requires an
// authenticated session like the proxy.
func (r *routesService) extensionAssets(proxy http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.extensionsDir != "" && req.Header.Get(apiclient.HeaderExtensionApplication) == "" {
			name := req.PathValue("extension") + "/" + req.PathValue("path")
			if serveRootFile(w, req, r.extensionsDir, name, false) {
				return
			}
		}
		proxy.ServeHTTP(w, req)
	})
}

// serveRootFile serves dir/name when name is a valid, dot-free relative path to a regular
// file inside dir (opened through os.Root, so neither ".." nor symbolic links can leave the
// directory). It reports false when there is no such file.
func serveRootFile(w http.ResponseWriter, req *http.Request, dir, name string, attachment bool) bool {
	if !fs.ValidPath(name) || name == "." {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") {
			return false
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	h := w.Header()
	ct := mime.TypeByExtension(path.Ext(name))
	if attachment || ct == "" {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", "no-cache")
	if attachment {
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(name)}))
	}
	http.ServeContent(w, req, path.Base(name), st.ModTime(), f)
	return true
}

// download serves CLI binaries (API-014): patrol-<os>-<arch>[.exe] from the download
// directory; patrol-linux is an alias of patrol-linux-<server arch>. Without a matching
// file, patrol-linux and patrol-linux-<server arch> on a Linux server fall back to the
// patrol binary found in PATH (upstream behavior).
func (r *routesService) download(w http.ResponseWriter, req *http.Request) {
	file := req.PathValue("file")
	candidates := []string{file}
	if file == "patrol-linux" {
		candidates = []string{"patrol-linux-" + runtime.GOARCH, file}
	}
	if !downloadFileRE.MatchString(file) && file != "patrol-linux" {
		rest.WriteError(w, req, rest.NotFound("no such download"))
		return
	}
	if r.downloadDir != "" {
		for _, c := range candidates {
			if serveRootFile(w, req, r.downloadDir, c, true) {
				return
			}
		}
	}
	if runtime.GOOS == "linux" && (file == "patrol-linux" || file == "patrol-linux-"+runtime.GOARCH) {
		if p, err := exec.LookPath(cliBinaryName); err == nil {
			if f, err := os.Open(p); err == nil {
				defer func() { _ = f.Close() }()
				if st, err := f.Stat(); err == nil && st.Mode().IsRegular() {
					h := w.Header()
					h.Set("Content-Type", "application/octet-stream")
					h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file}))
					http.ServeContent(w, req, file, st.ModTime(), f)
					return
				}
			}
		}
	}
	rest.WriteError(w, req, rest.NotFound("no such download"))
}
