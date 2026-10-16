// Package ocitest provides an in-process OCI distribution registry for hermetic
// tests: pull side of the distribution API (manifests, blobs, paginated tag
// lists), optional basic or bearer-token authentication, request counters, and Go
// helpers to push content. It is test support code and must not be imported by
// production packages.
package ocitest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Options configures a Registry.
type Options struct {
	// Username and Password, when set, are required (basic auth, or the token
	// endpoint in bearer mode).
	Username, Password string
	// BearerToken switches to bearer-token mode: registry endpoints require
	// "Authorization: Bearer <BearerToken>" and the challenge points at /token,
	// which issues the token (checking Username/Password when set).
	BearerToken string
	// TLS serves HTTPS with a self-signed certificate (see CACertPEM).
	TLS bool
	// PageSize forces tag-list pagination with Link headers when positive and the
	// client did not ask for a smaller page.
	PageSize int
}

// Registry is an in-memory OCI registry served over httptest.
type Registry struct {
	opts   Options
	server *httptest.Server

	mu       sync.Mutex
	repos    map[string]*repository
	requests map[string]int
}

type repository struct {
	blobs     map[digest.Digest][]byte
	manifests map[digest.Digest]manifest
	tags      map[string]digest.Digest
}

type manifest struct {
	mediaType string
	raw       []byte
}

// New starts a registry that is closed when the test ends.
func New(t testing.TB, opts Options) *Registry {
	t.Helper()
	r := &Registry{opts: opts, repos: map[string]*repository{}, requests: map[string]int{}}
	r.server = httptest.NewUnstartedServer(http.HandlerFunc(r.serve))
	// Rejected handshakes from clients that do not trust the test CA are expected.
	r.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	if opts.TLS {
		r.server.StartTLS()
	} else {
		r.server.Start()
	}
	t.Cleanup(r.server.Close)
	return r
}

// Host returns host:port of the registry.
func (r *Registry) Host() string {
	return strings.TrimPrefix(strings.TrimPrefix(r.server.URL, "https://"), "http://")
}

// URL returns the base URL (http:// or https://).
func (r *Registry) URL() string { return r.server.URL }

// CACertPEM returns the PEM certificate of a TLS registry (nil otherwise).
func (r *Registry) CACertPEM() []byte {
	if r.server.Certificate() == nil {
		return nil
	}
	return pemCert(r.server.Certificate().Raw)
}

// Requests returns how many requests of kind ("manifest", "blob", "tags",
// "token", "ping") were served, or the count for "blob:<digest>".
func (r *Registry) Requests(kind string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[kind]
}

func (r *Registry) count(kinds ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range kinds {
		r.requests[k]++
	}
}

func (r *Registry) repo(name string) *repository {
	rp, ok := r.repos[name]
	if !ok {
		rp = &repository{
			blobs:     map[digest.Digest][]byte{},
			manifests: map[digest.Digest]manifest{},
			tags:      map[string]digest.Digest{},
		}
		r.repos[name] = rp
	}
	return rp
}

// PushBlob stores data in repository name and returns its descriptor.
func (r *Registry) PushBlob(name, mediaType string, data []byte) ocispec.Descriptor {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := digest.FromBytes(data)
	r.repo(name).blobs[d] = slices.Clone(data)
	return ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(data))}
}

// PutBlobRaw stores data under digest d without checking that they match, to
// simulate a registry serving tampered content.
func (r *Registry) PutBlobRaw(name string, d digest.Digest, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.repo(name).blobs[d] = slices.Clone(data)
}

// PushManifest stores raw as a manifest of mediaType, tagged tag when non-empty.
func (r *Registry) PushManifest(name, tag, mediaType string, raw []byte) ocispec.Descriptor {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := digest.FromBytes(raw)
	rp := r.repo(name)
	rp.manifests[d] = manifest{mediaType: mediaType, raw: slices.Clone(raw)}
	if tag != "" {
		rp.tags[tag] = d
	}
	return ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(raw))}
}

// Layer is a layer to push with PushArtifact.
type Layer struct {
	MediaType string
	Data      []byte
	// Descriptor, when its Digest is set, is written to the manifest instead of
	// the computed one (to forge sizes or digests). Data is still stored under
	// the real digest.
	Descriptor ocispec.Descriptor
}

// PushArtifact pushes config and layers and an OCI image manifest referencing
// them, tagged tag. It returns the manifest descriptor.
func (r *Registry) PushArtifact(name, tag, configMediaType string, config []byte, layers []Layer, annotations map[string]string) ocispec.Descriptor {
	if config == nil {
		config = []byte("{}")
	}
	m := ocispec.Manifest{
		MediaType:   ocispec.MediaTypeImageManifest,
		Config:      r.PushBlob(name, configMediaType, config),
		Annotations: annotations,
	}
	m.SchemaVersion = 2
	for _, l := range layers {
		desc := r.PushBlob(name, l.MediaType, l.Data)
		if l.Descriptor.Digest != "" {
			desc = l.Descriptor
		}
		m.Layers = append(m.Layers, desc)
	}
	raw, _ := json.Marshal(m) // a Manifest of strings and ints always marshals
	return r.PushManifest(name, tag, ocispec.MediaTypeImageManifest, raw)
}

// PushHelmChart pushes a packaged chart (.tgz) as a Helm OCI artifact tagged with
// version ('+' stored as '_', as Helm does).
func (r *Registry) PushHelmChart(name, version string, tgz []byte) ocispec.Descriptor {
	return r.PushArtifact(name, strings.ReplaceAll(version, "+", "_"),
		"application/vnd.cncf.helm.config.v1+json", []byte(`{"name":"chart","version":"`+version+`"}`),
		[]Layer{{MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip", Data: tgz}}, nil)
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	if path == "/token" {
		r.count("token")
		r.serveToken(w, req)
		return
	}
	if !strings.HasPrefix(path, "/v2/") {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	rest := strings.TrimPrefix(path, "/v2/")
	var name, kind, ref string
	switch {
	case rest == "":
		kind = "ping"
	case strings.HasSuffix(rest, "/tags/list"):
		name, kind = strings.TrimSuffix(rest, "/tags/list"), "tags"
	case strings.Contains(rest, "/manifests/"):
		i := strings.LastIndex(rest, "/manifests/")
		name, kind, ref = rest[:i], "manifest", rest[i+len("/manifests/"):]
	case strings.Contains(rest, "/blobs/"):
		i := strings.LastIndex(rest, "/blobs/")
		name, kind, ref = rest[:i], "blob", rest[i+len("/blobs/"):]
	default:
		writeError(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	if !r.authorized(w, req, name) {
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "read-only registry")
		return
	}
	switch kind {
	case "ping":
		r.count("ping")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	case "tags":
		r.count("tags")
		r.serveTags(w, req, name)
	case "manifest":
		r.count("manifest")
		r.serveManifest(w, req, name, ref)
	case "blob":
		r.count("blob", "blob:"+ref)
		r.serveBlob(w, req, name, ref)
	}
}

func (r *Registry) authorized(w http.ResponseWriter, req *http.Request, name string) bool {
	if r.opts.BearerToken != "" {
		if req.Header.Get("Authorization") == "Bearer "+r.opts.BearerToken {
			return true
		}
		scheme := "http"
		if r.opts.TLS {
			scheme = "https"
		}
		challenge := fmt.Sprintf(`Bearer realm="%s://%s/token",service="ocitest"`, scheme, req.Host)
		if name != "" {
			challenge += fmt.Sprintf(`,scope="repository:%s:pull"`, name)
		}
		w.Header().Set("WWW-Authenticate", challenge)
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return false
	}
	if r.opts.Username == "" {
		return true
	}
	if u, p, ok := req.BasicAuth(); ok && u == r.opts.Username && p == r.opts.Password {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="ocitest"`)
	writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
	return false
}

func (r *Registry) serveToken(w http.ResponseWriter, req *http.Request) {
	if r.opts.Username != "" {
		if u, p, ok := req.BasicAuth(); !ok || u != r.opts.Username || p != r.opts.Password {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "bad credentials")
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": r.opts.BearerToken})
}

func (r *Registry) serveTags(w http.ResponseWriter, req *http.Request, name string) {
	r.mu.Lock()
	rp, ok := r.repos[name]
	var tags []string
	if ok {
		for t := range rp.tags {
			tags = append(tags, t)
		}
	}
	r.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository name not known to registry")
		return
	}
	slices.Sort(tags)
	q := req.URL.Query()
	if last := q.Get("last"); last != "" {
		i, _ := slices.BinarySearch(tags, last)
		for i < len(tags) && tags[i] <= last {
			i++
		}
		tags = tags[i:]
	}
	n := r.opts.PageSize
	if v, err := strconv.Atoi(q.Get("n")); err == nil && v > 0 && (n <= 0 || v < n) {
		n = v
	}
	if n > 0 && len(tags) > n {
		tags = tags[:n]
		w.Header().Set("Link", fmt.Sprintf(`</v2/%s/tags/list?n=%d&last=%s>; rel="next"`, name, n, tags[len(tags)-1]))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "tags": tags})
}

func (r *Registry) serveManifest(w http.ResponseWriter, req *http.Request, name, ref string) {
	r.mu.Lock()
	var (
		m     manifest
		d     digest.Digest
		found bool
	)
	if rp, ok := r.repos[name]; ok {
		d = digest.Digest(ref)
		if d.Validate() != nil {
			d = rp.tags[ref]
		}
		m, found = rp.manifests[d]
	}
	r.mu.Unlock()
	if !found {
		writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}
	w.Header().Set("Content-Type", m.mediaType)
	w.Header().Set("Docker-Content-Digest", d.String())
	w.Header().Set("Content-Length", strconv.Itoa(len(m.raw)))
	if req.Method == http.MethodGet {
		_, _ = w.Write(m.raw)
	}
}

func (r *Registry) serveBlob(w http.ResponseWriter, req *http.Request, name, ref string) {
	r.mu.Lock()
	var (
		data  []byte
		found bool
	)
	if rp, ok := r.repos[name]; ok {
		data, found = rp.blobs[digest.Digest(ref)]
	}
	r.mu.Unlock()
	if !found {
		writeError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", ref)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if req.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// Digest returns the sha256 digest string of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
