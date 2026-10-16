package apiserver

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// maxIndexBytes bounds index.html.
const maxIndexBytes = 4 << 20

// Cache-Control values of static assets.
const (
	cacheImmutable = "public, max-age=31536000, immutable"
	cacheNoCache   = "no-cache"
)

// baseTagRE matches the <base href="..."> element of index.html.
var baseTagRE = regexp.MustCompile(`(?i)<base\s+href="[^"]*"\s*/?>`)

// UIHandler serves the single-page web UI from an fs.FS (typically the embedded ui/dist):
//   - existing files are served for GET and HEAD; files under assets/ (content-hashed by
//     the build) are cacheable forever, everything else must be revalidated;
//   - any other path whose request accepts text/html gets index.html (history routing)
//     with its <base href> set to the configured base path and no-cache headers;
//   - index.html requested explicitly also goes through that path;
//   - dot-files and directory listings are never served.
type UIHandler struct {
	fsys     fs.FS
	index    []byte
	modTime  time.Time
	notFound http.Handler
}

// NewUIHandler returns a handler for fsys; baseHRef is injected as <base href>. notFound
// serves requests that match nothing (nil: plain 404).
func NewUIHandler(fsys fs.FS, baseHRef string, notFound http.Handler) (*UIHandler, error) {
	if fsys == nil {
		return nil, errors.New("ui: nil filesystem")
	}
	f, err := fsys.Open("index.html")
	if err != nil {
		return nil, fmt.Errorf("ui: open index.html: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxIndexBytes+1))
	if err != nil {
		return nil, fmt.Errorf("ui: read index.html: %w", err)
	}
	if len(data) > maxIndexBytes {
		return nil, fmt.Errorf("ui: index.html exceeds %d bytes", maxIndexBytes)
	}
	var mod time.Time
	if st, err := f.Stat(); err == nil {
		mod = st.ModTime()
	}
	if notFound == nil {
		notFound = http.NotFoundHandler()
	}
	return &UIHandler{fsys: fsys, index: injectBaseHRef(data, baseHRef), modTime: mod, notFound: notFound}, nil
}

// injectBaseHRef replaces (or inserts) the <base href> of index.html.
func injectBaseHRef(index []byte, baseHRef string) []byte {
	href := normalizeBaseHRef(baseHRef)
	tag := []byte(`<base href="` + html.EscapeString(href) + `" />`)
	if baseTagRE.Match(index) {
		return baseTagRE.ReplaceAllLiteral(index, tag)
	}
	if i := bytes.Index(bytes.ToLower(index), []byte("<head>")); i >= 0 {
		out := append([]byte{}, index[:i+len("<head>")]...)
		out = append(out, tag...)
		return append(out, index[i+len("<head>"):]...)
	}
	return index
}

// normalizeBaseHRef returns "/" or "/x/y/".
func normalizeBaseHRef(b string) string {
	b = strings.Trim(strings.TrimSpace(b), "/")
	if b == "" {
		return "/"
	}
	return "/" + b + "/"
}

func (u *UIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name != "" && name != "index.html" && fs.ValidPath(name) && !hasDotSegment(name) {
		if u.serveFile(w, r, name) {
			return
		}
	}
	if name == "" || name == "index.html" || acceptsHTML(r) {
		u.serveIndex(w, r)
		return
	}
	u.notFound.ServeHTTP(w, r)
}

func hasDotSegment(name string) bool {
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

func acceptsHTML(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept") {
		for _, part := range strings.Split(v, ",") {
			mt, _, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err == nil && mt == "text/html" {
				return true
			}
		}
	}
	return false
}

// serveFile serves an existing regular file; it reports false when name is not one.
func (u *UIHandler) serveFile(w http.ResponseWriter, r *http.Request, name string) bool {
	f, err := u.fsys.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", cacheImmutable)
	} else {
		w.Header().Set("Cache-Control", cacheNoCache)
	}
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, st.Name(), st.ModTime(), rs)
		return true
	}
	data, err := io.ReadAll(io.LimitReader(f, st.Size()+1))
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return true
	}
	http.ServeContent(w, r, st.Name(), st.ModTime(), bytes.NewReader(data))
	return true
}

func (u *UIHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	h.Set("Content-Length", strconv.Itoa(len(u.index)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(u.index)
}

// OverlayFS serves files from upper when present, else from lower (upstream
// --staticassets directory over the embedded UI).
type OverlayFS struct {
	Upper, Lower fs.FS
}

// Open implements fs.FS.
func (o OverlayFS) Open(name string) (fs.File, error) {
	if o.Upper != nil {
		if f, err := o.Upper.Open(name); err == nil {
			return f, nil
		}
	}
	if o.Lower == nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return o.Lower.Open(name)
}
