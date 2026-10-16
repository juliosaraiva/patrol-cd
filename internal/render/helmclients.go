package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/juliosaraiva/patrol-cd/internal/helm"
)

// maxProxiedHelmClients bounds the helm clients kept for distinct repository proxies.
// A call for a further proxy gets a client of its own, discarded after the call.
const maxProxiedHelmClients = 16

// helmClients hands out the helm client for a repository proxy. internal/helm only
// has a client-wide Proxy/NoProxy, while upstream applies the proxy of the source
// repository to `helm template`/`dependency build` and the proxy of the chart
// repository to chart pulls (SRC-136). A repository without a proxy uses the
// configured client, which passes the repo-server's proxy variables through; a
// repository with one uses a client with identical settings, that proxy, and its own
// work directory (chart and index caches are per client).
type helmClients struct {
	base *helm.Client
	dir  string // parent of the per-proxy work directories

	mu      sync.Mutex
	byProxy map[helmProxyKey]*helm.Client
}

type helmProxyKey struct{ proxy, noProxy string }

func newHelmClients(base *helm.Client, workDir string) *helmClients {
	return &helmClients{base: base, dir: filepath.Join(workDir, "helm-proxy"), byProxy: map[helmProxyKey]*helm.Client{}}
}

// forProxy returns the client for proxy/noProxy and a release function the caller
// runs when done with it (after removing anything the client returned, such as a
// pulled chart directory). It returns ErrHelmNotConfigured without a base client.
func (h *helmClients) forProxy(proxy, noProxy string) (*helm.Client, func(), error) {
	if h == nil || h.base == nil {
		return nil, nil, ErrHelmNotConfigured
	}
	if proxy == "" {
		return h.base, func() {}, nil
	}
	key := helmProxyKey{proxy: proxy, noProxy: noProxy}
	h.mu.Lock()
	defer h.mu.Unlock()
	if cl, ok := h.byProxy[key]; ok {
		return cl, func() {}, nil
	}
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("render: create helm proxy work dir: %w", err)
	}
	if len(h.byProxy) < maxProxiedHelmClients {
		sum := sha256.Sum256([]byte(proxy + "\x00" + noProxy))
		cl := cloneHelmClient(h.base, filepath.Join(h.dir, hex.EncodeToString(sum[:16])), proxy, noProxy)
		h.byProxy[key] = cl
		return cl, func() {}, nil
	}
	dir, err := os.MkdirTemp(h.dir, "call-")
	if err != nil {
		return nil, nil, fmt.Errorf("render: create helm proxy work dir: %w", err)
	}
	return cloneHelmClient(h.base, dir, proxy, noProxy), func() { _ = os.RemoveAll(dir) }, nil
}

// cloneHelmClient returns a new client with the exported settings of base except the
// work directory and the proxy. base's WorkDir is neither read nor shared: it is
// written by base's lazy initialization, and caches must not be shared between
// clients whose locks are independent. TestCloneHelmClientCopiesEveryField fails when
// helm.Client gains a field this function does not handle.
func cloneHelmClient(base *helm.Client, workDir, proxy, noProxy string) *helm.Client {
	return &helm.Client{
		HelmPath:          base.HelmPath,
		WorkDir:           workDir,
		UserAgent:         base.UserAgent,
		Proxy:             proxy,
		NoProxy:           noProxy,
		PluginsDir:        base.PluginsDir,
		IndexTTL:          base.IndexTTL,
		MaxIndexBytes:     base.MaxIndexBytes,
		MaxChartBytes:     base.MaxChartBytes,
		MaxValuesBytes:    base.MaxValuesBytes,
		Archive:           base.Archive,
		Exec:              base.Exec,
		IndexCacheEntries: base.IndexCacheEntries,
	}
}
