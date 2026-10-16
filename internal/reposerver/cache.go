package reposerver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// renderSpec is every request input that can change rendered output, apart from the
// resolved revisions (which complete the cache key). It never contains credentials.
type renderSpec struct {
	Repo               string                     `json:"repo"`
	RepoType           string                     `json:"repoType,omitempty"`
	Source             v1alpha1.ApplicationSource `json:"source"`
	AppName            string                     `json:"appName,omitempty"`
	AppNamespace       string                     `json:"appNamespace,omitempty"`
	Namespace          string                     `json:"namespace,omitempty"`
	KubeVersion        string                     `json:"kubeVersion,omitempty"`
	APIVersions        []string                   `json:"apiVersions,omitempty"`
	KustomizeOptions   apiclient.KustomizeOptions `json:"kustomizeOptions"`
	HelmOptions        apiclient.HelmOptions      `json:"helmOptions"`
	EnabledSourceTypes map[string]bool            `json:"enabledSourceTypes,omitempty"`
	InstanceLabelKey   string                     `json:"instanceLabelKey,omitempty"`
	TrackingMethod     string                     `json:"trackingMethod,omitempty"`
	InstallationID     string                     `json:"installationID,omitempty"`
	HasMultipleSources bool                       `json:"hasMultipleSources,omitempty"`
	RefSources         map[string]refSpec         `json:"refSources,omitempty"`
	ProjectName        string                     `json:"projectName,omitempty"`
	ManifestGenPaths   string                     `json:"manifestGeneratePaths,omitempty"`
	SourceIntegrity    *v1alpha1.SourceIntegrity  `json:"sourceIntegrity,omitempty"`
	Keyring            string                     `json:"keyring,omitempty"`
	IntegrityRevision  string                     `json:"integrityRevision,omitempty"`
	Plugins            []string                   `json:"plugins,omitempty"`
	Server             string                     `json:"server"`
	Extra              map[string]json.RawMessage `json:"extra,omitempty"`
}

// refSpec is the credential-free part of a ref source.
type refSpec struct {
	Repo           string `json:"repo"`
	TargetRevision string `json:"targetRevision"`
	Chart          string `json:"chart,omitempty"`
}

// sourceIdentity is the part of a request that identifies one application source
// independently of revisions and cluster settings; UpdateRevisionForPaths moves every
// cached rendering of an identity from one revision to another.
type sourceIdentity struct {
	Repo               string                     `json:"repo"`
	AppName            string                     `json:"appName,omitempty"`
	Source             v1alpha1.ApplicationSource `json:"source"`
	HasMultipleSources bool                       `json:"hasMultipleSources,omitempty"`
}

func digestJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		// Every field is JSON-serializable; a failure here would be a programming
		// error. Fall back to a key that never matches anything cached.
		data = []byte("unserializable:" + err.Error())
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// keySource returns the source as it enters cache keys: for git and OCI sources the
// target revision is dropped (the resolved revision supersedes it), as upstream.
func keySource(src v1alpha1.ApplicationSource, kind sourceKind) v1alpha1.ApplicationSource {
	out := *src.DeepCopy()
	if kind != kindHelm {
		out.TargetRevision = ""
	}
	out.RepoURL = ""
	return out
}

// manifestKey combines the spec digest with the resolved revisions.
func manifestKey(specDigest, revision string, refRevs map[string]string) string {
	var b strings.Builder
	b.WriteString(specDigest)
	b.WriteByte(0)
	b.WriteString(revision)
	keys := slices.Sorted(maps.Keys(refRevs))
	for _, k := range keys {
		b.WriteByte(0)
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(refRevs[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "mfst|" + hex.EncodeToString(sum[:])
}

// manifestEntry is a cached generation result (or failure record, SRC-111).
type manifestEntry struct {
	key        string
	specDigest string
	identity   string
	revision   string
	refRevs    map[string]string

	resp *apiclient.ManifestResponse // nil for failure records

	failures        int
	firstFailure    time.Time
	lastError       string
	cachedResponses int
}

func (e *manifestEntry) size() int64 {
	n := int64(512 + len(e.lastError))
	if e.resp != nil {
		for _, m := range e.resp.Manifests {
			n += int64(len(m)) + 16
		}
		for _, c := range e.resp.Commands {
			n += int64(len(c)) + 16
		}
	}
	return n
}

// clone returns a deep copy (callers never share cached slices).
func (e *manifestEntry) clone() *manifestEntry {
	out := *e
	out.refRevs = maps.Clone(e.refRevs)
	if e.resp != nil {
		out.resp = cloneManifestResponse(e.resp)
	}
	return &out
}

func cloneManifestResponse(r *apiclient.ManifestResponse) *apiclient.ManifestResponse {
	if r == nil {
		return nil
	}
	out := *r
	out.Manifests = slices.Clone(r.Manifests)
	out.Commands = slices.Clone(r.Commands)
	if r.SourceIntegrityResult != nil {
		si := &apiclient.SourceIntegrityResult{}
		for _, c := range r.SourceIntegrityResult.Checks {
			si.Checks = append(si.Checks, apiclient.SourceIntegrityCheck{Name: c.Name, Problems: slices.Clone(c.Problems)})
		}
		out.SourceIntegrityResult = si
	}
	return &out
}

// manifestCache is the render cache: an LRU bounded by entries and bytes, plus an
// index from source identity to keys for UpdateRevisionForPaths.
type manifestCache struct {
	mu    sync.Mutex
	lru   *lru[*manifestEntry]
	index map[string]map[string]struct{}
	ttl   time.Duration
}

func newManifestCache(maxEntries int, maxBytes int64, ttl time.Duration, now func() time.Time) *manifestCache {
	c := &manifestCache{index: map[string]map[string]struct{}{}, ttl: ttl}
	c.lru = newLRU[*manifestEntry](maxEntries, maxBytes, now)
	c.lru.onEvict = func(key string, e *manifestEntry) {
		// Called with c.mu held (every lru access goes through manifestCache).
		if set := c.index[e.identity]; set != nil {
			delete(set, key)
			if len(set) == 0 {
				delete(c.index, e.identity)
			}
		}
	}
	return c
}

func (c *manifestCache) get(key string) (*manifestEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.lru.Get(key)
	if !ok {
		return nil, false
	}
	return e.clone(), true
}

func (c *manifestCache) set(e *manifestEntry) {
	e = e.clone()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lru.Add(e.key, e, e.size(), c.ttl) {
		set := c.index[e.identity]
		if set == nil {
			set = map[string]struct{}{}
			c.index[e.identity] = set
		}
		set[e.key] = struct{}{}
	}
}

func (c *manifestCache) remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lru.Remove(key)
}

// moveRevision copies every successful entry of identity generated at oldRev (with
// the given old ref revisions) to newRev / newRefs. matchRevision false copies entries
// regardless of their main revision (non-git main sources). It returns the number of
// entries copied.
func (c *manifestCache) moveRevision(identity string, matchRevision bool, oldRev, newRev string, oldRefs, newRefs map[string]string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := slices.Collect(maps.Keys(c.index[identity]))
	sort.Strings(keys)
	var copies []*manifestEntry
	for _, k := range keys {
		e, ok := c.lru.Get(k)
		// Failure records and results carrying a source integrity verdict (which is
		// specific to the verified commit) are never moved.
		if !ok || e.resp == nil || e.resp.SourceIntegrityResult != nil || e.resp.VerifyResult != "" {
			continue
		}
		if matchRevision && e.revision != oldRev {
			continue
		}
		match := true
		for url, rev := range oldRefs {
			if got, ok := e.refRevs[url]; !ok || got != rev {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		n := e.clone()
		if matchRevision {
			n.revision = newRev
			n.resp.Revision = newRev
		}
		maps.Copy(n.refRevs, newRefs)
		n.key = manifestKey(n.specDigest, n.revision, n.refRevs)
		if n.key == e.key {
			continue
		}
		copies = append(copies, n)
	}
	for _, n := range copies {
		if c.lru.Add(n.key, n, n.size(), c.ttl) {
			set := c.index[n.identity]
			if set == nil {
				set = map[string]struct{}{}
				c.index[n.identity] = set
			}
			set[n.key] = struct{}{}
		}
	}
	return len(copies)
}

func (c *manifestCache) stats() (int, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len(), c.lru.Bytes()
}

// auxCache holds the secondary caches (app details, listings, metadata, files,
// directories, changed files, OCI tags) as JSON documents, so every read returns a
// private copy. Keys are prefixed by kind and include a credential fingerprint where
// the value was read from a repository with credentials.
type auxCache struct {
	lru *lru[[]byte]
}

func newAuxCache(maxEntries int, maxBytes int64, now func() time.Time) *auxCache {
	return &auxCache{lru: newLRU[[]byte](maxEntries, maxBytes, now)}
}

// getAux returns a private copy of the cached value of key.
func getAux[T any](c *auxCache, key string) (*T, bool) {
	data, ok := c.lru.Get(key)
	if !ok {
		return nil, false
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		c.lru.Remove(key)
		return nil, false
	}
	return &out, true
}

// putAux stores v for ttl.
func putAux(c *auxCache, key string, v any, ttl time.Duration) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.lru.Add(key, data, int64(len(data)+len(key)+64), ttl)
}

// auxKey builds a cache key from parts.
func auxKey(kind string, parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return kind + "|" + hex.EncodeToString(h.Sum(nil))
}
