package render

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// TestCloneHelmClientCopiesEveryField fails when helm.Client gains an exported field
// that cloneHelmClient does not handle.
func TestCloneHelmClientCopiesEveryField(t *testing.T) {
	t.Parallel()
	handled := map[string]bool{
		"HelmPath": true, "WorkDir": true, "UserAgent": true, "Proxy": true, "NoProxy": true,
		"PluginsDir": true, "IndexTTL": true, "MaxIndexBytes": true, "MaxChartBytes": true,
		"MaxValuesBytes": true, "Archive": true, "Exec": true, "IndexCacheEntries": true,
	}
	typ := reflect.TypeFor[helm.Client]()
	for i := range typ.NumField() {
		if f := typ.Field(i); f.IsExported() && !handled[f.Name] {
			t.Errorf("helm.Client.%s is not handled by cloneHelmClient", f.Name)
		}
	}

	base := &helm.Client{
		HelmPath: "/bin/helm", WorkDir: "/base", UserAgent: "ua", Proxy: "http://base", NoProxy: "base.local",
		PluginsDir: "/plugins", IndexTTL: time.Minute, MaxIndexBytes: 1, MaxChartBytes: 2, MaxValuesBytes: 3,
		Archive: sandbox.ArchiveLimits{MaxFiles: 4, MaxTotalBytes: 5, MaxFileBytes: 6},
		Exec:    sandbox.Limits{Timeout: time.Hour, MaxStdout: 7}, IndexCacheEntries: 8,
	}
	got := cloneHelmClient(base, "/clone", "http://proxy", "no.proxy")
	gv, bv := reflect.ValueOf(got).Elem(), reflect.ValueOf(base).Elem()
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		switch f.Name {
		case "WorkDir":
			assertEqual(t, got.WorkDir, "/clone")
		case "Proxy":
			assertEqual(t, got.Proxy, "http://proxy")
		case "NoProxy":
			assertEqual(t, got.NoProxy, "no.proxy")
		default:
			if !reflect.DeepEqual(gv.Field(i).Interface(), bv.Field(i).Interface()) {
				t.Errorf("field %s not copied: %v", f.Name, gv.Field(i).Interface())
			}
		}
	}
}

func TestHelmClientsForProxy(t *testing.T) {
	t.Parallel()
	if _, _, err := newHelmClients(nil, t.TempDir()).forProxy("http://p", ""); !errors.Is(err, ErrHelmNotConfigured) {
		t.Fatalf("error = %v, want ErrHelmNotConfigured", err)
	}
	base := &helm.Client{HelmPath: "helm", WorkDir: t.TempDir()}
	work := t.TempDir()
	h := newHelmClients(base, work)

	cl, release, err := h.forProxy("", "ignored")
	if err != nil || cl != base {
		t.Fatalf("no proxy: client %p (base %p), %v", cl, base, err)
	}
	release()

	a, release, err := h.forProxy("http://proxy-a:3128", "")
	if err != nil {
		t.Fatal(err)
	}
	release()
	assertEqual(t, a.Proxy, "http://proxy-a:3128")
	assertEqual(t, filepath.Dir(a.WorkDir), filepath.Join(work, "helm-proxy"))
	again, _, _ := h.forProxy("http://proxy-a:3128", "")
	if again != a {
		t.Fatal("same proxy returned a different client")
	}
	b, _, _ := h.forProxy("http://proxy-a:3128", "internal")
	if b == a || b.NoProxy != "internal" || b.WorkDir == a.WorkDir {
		t.Fatalf("noProxy not part of the client key: %+v", b)
	}

	// Concurrent first use of one proxy yields one client.
	var wg sync.WaitGroup
	clients := make([]*helm.Client, 8)
	for i := range clients {
		wg.Go(func() { clients[i], _, _ = h.forProxy("http://proxy-c", "") })
	}
	wg.Wait()
	for _, c := range clients {
		if c != clients[0] {
			t.Fatal("concurrent callers got different clients for one proxy")
		}
	}

	// Past the bound, each call gets a client of its own whose work directory is
	// removed on release.
	for i := len(h.byProxy); i < maxProxiedHelmClients; i++ {
		if _, _, err := h.forProxy(fmt.Sprintf("http://fill-%d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	extra, release, err := h.forProxy("http://one-too-many", "")
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, len(h.byProxy), maxProxiedHelmClients)
	assertEqual(t, extra.Proxy, "http://one-too-many")
	if err := os.WriteFile(filepath.Join(extra.WorkDir, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := os.Stat(extra.WorkDir); !os.IsNotExist(err) {
		t.Fatalf("per-call work dir not removed: %v", err)
	}
	sum := sha256.Sum256([]byte("http://proxy-a:3128\x00"))
	assertEqual(t, filepath.Base(a.WorkDir), hex.EncodeToString(sum[:16]))
}

// forwardProxy is an HTTP forward proxy that serves the requested origins itself:
// values.invalid (a remote values file) and charts.invalid (a chart repository).
// Neither name resolves, so a request succeeds only through the proxy.
type forwardProxy struct {
	*httptest.Server
	mu    sync.Mutex
	hosts []string
}

func (p *forwardProxy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.hosts)
}

func newForwardProxy(t testing.TB) *forwardProxy {
	t.Helper()
	tgz := packageChart(t, filepath.Join("testdata", "helm", "app"), "app")
	sum := sha256.Sum256(tgz)
	p := &forwardProxy{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p.mu.Lock()
		p.hosts = append(p.hosts, req.Host+req.URL.Path)
		p.mu.Unlock()
		if !req.URL.IsAbs() {
			http.Error(w, "not a proxy request", http.StatusBadRequest)
			return
		}
		switch req.Host + req.URL.Path {
		case "values.invalid/values.yaml":
			_, _ = w.Write([]byte("key: via-proxy\n"))
		case "charts.invalid/index.yaml":
			_, _ = fmt.Fprintf(w, "apiVersion: v1\nentries:\n  app:\n  - name: app\n    version: 0.1.0\n    urls: [http://charts.invalid/app-0.1.0.tgz]\n    digest: %s\n", hex.EncodeToString(sum[:]))
		case "charts.invalid/app-0.1.0.tgz":
			_, _ = w.Write(tgz)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

// TestHelmRepositoryProxy checks that `helm template` (remote value files) runs with
// the source repository's proxy and that chart pulls use the chart repository's
// proxy (upstream SRC-136), while other renders keep the configured client.
func TestHelmRepositoryProxy(t *testing.T) {
	t.Parallel()
	proxy := newForwardProxy(t)
	r := newTestRenderer(t, withHelm(t))

	t.Run("template", func(t *testing.T) {
		t.Parallel()
		in := helmInput(t, &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"http://values.invalid/values.yaml"}})
		in.HelmOptions.ValuesFileSchemes = []string{"http"}
		in.Repo = apiclient.Repository{URL: in.Source.RepoURL, Proxy: proxy.URL, NoProxy: "localhost"}
		assertEqual(t, cmData(t, generate(t, r, in), "test-app-cm")["key"].(string), "via-proxy")
		if !slices.Contains(proxy.seen(), "values.invalid/values.yaml") {
			t.Fatalf("value file not fetched through the repository proxy: %v", proxy.seen())
		}
	})
	t.Run("chart pull", func(t *testing.T) {
		t.Parallel()
		in := Input{
			AppName: "chart-app",
			Repo:    apiclient.Repository{URL: "http://charts.invalid", Type: v1alpha1.RepositoryTypeHelm, Proxy: proxy.URL},
			Source:  v1alpha1.ApplicationSource{RepoURL: "http://charts.invalid", Chart: "app", TargetRevision: "0.1.*"},
		}
		res := generate(t, r, in)
		findObj(t, res.Manifests, "ConfigMap", "chart-app-cm")
		seen := strings.Join(proxy.seen(), " ")
		assertContains(t, seen, "charts.invalid/index.yaml")
		assertContains(t, seen, "charts.invalid/app-0.1.0.tgz")
	})
	t.Run("no repository proxy keeps the configured client", func(t *testing.T) {
		t.Parallel()
		res := generate(t, r, helmInput(t, nil))
		findObj(t, res.Manifests, "ConfigMap", "test-app-cm")
	})
}
