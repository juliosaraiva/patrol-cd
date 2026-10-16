package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/render/cmpwire"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// pluginConn is a client of one plugin sidecar socket.
type pluginConn struct {
	r      *Renderer
	name   string // socket file name
	client *http.Client
	tr     *http.Transport
	config cmpwire.ConfigResponse
	// maxResponse bounds response bodies (the call's output limit, at most
	// cmpwire.MaxResponseBytes).
	maxResponse int64
}

func (p *pluginConn) close() { p.tr.CloseIdleConnections() }

// ListPlugins returns the plugins served in the socket directory: one name per socket
// file, without the ".sock" suffix, in lexical order (SRC-090).
func (r *Renderer) ListPlugins() ([]string, error) {
	entries, err := os.ReadDir(r.cfg.PluginSocketDir)
	if err != nil {
		return nil, fmt.Errorf("failed to get plugins from dir %s, error=%w", r.cfg.PluginSocketDir, err)
	}
	var out []string
	for _, e := range entries {
		if e.Type()&fs.ModeSocket != 0 && strings.HasSuffix(e.Name(), cmpwire.SocketSuffix) {
			out = append(out, strings.TrimSuffix(e.Name(), cmpwire.SocketSuffix))
		}
	}
	return out, nil
}

// dialPlugin connects to the plugin socket file and fetches its configuration. The
// socket must be inside the socket directory. maxOutput is the call's output limit.
func (r *Renderer) dialPlugin(ctx context.Context, file string, maxOutput int64) (*pluginConn, error) {
	dir, err := filepath.Abs(r.cfg.PluginSocketDir)
	if err != nil {
		return nil, fmt.Errorf("error getting absolute path for plugin socket dir %q: %w", r.cfg.PluginSocketDir, err)
	}
	address := filepath.Join(dir, file)
	if rel, err := filepath.Rel(dir, address); err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || strings.ContainsRune(rel, filepath.Separator) {
		return nil, fmt.Errorf("invalid socket file path, %q is outside plugin socket dir %q", file, r.cfg.PluginSocketDir)
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", address)
		},
		DisableCompression:    true,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 0,
	}
	maxResponse := int64(cmpwire.MaxResponseBytes)
	if maxOutput > 0 && maxOutput < maxResponse {
		maxResponse = maxOutput
	}
	p := &pluginConn{r: r, name: file, tr: tr, client: &http.Client{Transport: tr}, maxResponse: maxResponse}
	var lastErr error
	for attempt := 0; attempt < r.cfg.CMPRetryAttempts; attempt++ {
		if attempt > 0 {
			t := time.NewTimer(time.Duration(attempt) * r.cfg.CMPRetryBackoff)
			select {
			case <-ctx.Done():
				t.Stop()
				p.close()
				return nil, ctx.Err()
			case <-t.C:
			}
		}
		var cfg cmpwire.ConfigResponse
		err := p.do(ctx, http.MethodGet, cmpwire.PathConfig, nil, 0, &cfg)
		if err == nil {
			p.config = cfg
			r.metrics.observeCMP("config", nil)
			return p, nil
		}
		lastErr = err
		if !isDialError(err) || ctx.Err() != nil {
			break
		}
	}
	r.metrics.observeCMP("config", lastErr)
	p.close()
	if isDialError(lastErr) {
		return nil, fmt.Errorf("error dialing to cmp-server for plugin %s: %w", file, lastErr)
	}
	return nil, fmt.Errorf("error checking plugin configuration %s: %w", file, lastErr)
}

func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// do performs one request. body, when non-nil, is sent with Content-Length size and
// the stream content type. The JSON response is decoded into out, bounded by the
// output limit.
func (p *pluginConn) do(ctx context.Context, method, path string, body io.Reader, size int64, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://cmp"+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.ContentLength = size
		req.Header.Set("Content-Type", cmpwire.ContentTypeStream)
	}
	req.Header.Set("Accept", cmpwire.ContentTypeJSON)
	if dl, ok := ctx.Deadline(); ok {
		req.Header.Set(cmpwire.HeaderDeadline, dl.UTC().Format(time.RFC3339Nano))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	limit := p.maxResponse
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("error reading cmp-server response: %w", err)
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("%w: cmp-server response exceeds %d bytes", ErrOutputTooLarge, limit)
	}
	if resp.StatusCode != http.StatusOK {
		var e cmpwire.Error
		if json.Unmarshal(data, &e) == nil && e.Message != "" {
			return &e
		}
		return fmt.Errorf("cmp-server returned HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("error decoding cmp-server response: %w", err)
	}
	return nil
}

// sourceArchive is a source tree archived for a plugin.
type sourceArchive struct {
	file *os.File
	meta cmpwire.Metadata
}

func (a *sourceArchive) close() {
	name := a.file.Name()
	_ = a.file.Close()
	_ = os.Remove(name)
}

// archiveSource writes the tree at root as tar.gz into a private temporary file,
// excluding ".git" and the configured exclusions, and builds the stream metadata
// (SRC-085, SRC-086).
func (r *Renderer) archiveSource(ctx context.Context, c *call, root string, env []string) (*sourceArchive, error) {
	f, err := os.CreateTemp(r.cfg.WorkDir, "patrol-cmp-*.tar.gz")
	if err != nil {
		return nil, fmt.Errorf("error compressing repo files: %w", err)
	}
	a := &sourceArchive{file: f}
	ok := false
	defer func() {
		if !ok {
			a.close()
		}
	}()
	excludes := append([]string{".git"}, r.cfg.PluginTarExclusions...)
	h := sha256.New()
	w := &limitedWriter{w: io.MultiWriter(f, h), limit: c.limits.MaxCMPTarBytes}
	n, err := sandbox.TarCountContext(ctx, root, w, excludes)
	if err != nil {
		return nil, fmt.Errorf("error compressing repo files: %w", err)
	}
	if n == 0 {
		return nil, fmt.Errorf("no files to send(%s)", root)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("error compressing repo files: %w", err)
	}
	rel, err := relToRoot(root, c.appPath)
	if err != nil {
		return nil, fmt.Errorf("error building app relative path: %w", err)
	}
	a.meta = cmpwire.Metadata{
		AppName:    filepath.Base(c.appPath),
		AppRelPath: filepath.ToSlash(rel),
		Checksum:   hex.EncodeToString(h.Sum(nil)),
		Size:       w.n,
		Env:        cmpwire.EnvEntries(env),
	}
	ok = true
	return a, nil
}

type limitedWriter struct {
	w     io.Writer
	limit int64
	n     int64
}

func (l *limitedWriter) Write(b []byte) (int, error) {
	if l.limit > 0 && l.n+int64(len(b)) > l.limit {
		return 0, fmt.Errorf("source archive exceeds %d bytes", l.limit)
	}
	n, err := l.w.Write(b)
	l.n += int64(n)
	return n, err
}

// stream sends an archived source to path and decodes the response into out.
func (p *pluginConn) stream(ctx context.Context, path string, a *sourceArchive, out any) error {
	frame, err := cmpwire.EncodeMetadata(a.meta)
	if err != nil {
		return err
	}
	if _, err := a.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	body := io.MultiReader(bytes.NewReader(frame), io.LimitReader(a.file, a.meta.Size))
	return p.do(ctx, http.MethodPost, path, body, int64(len(frame))+a.meta.Size, out)
}

// detectPlugin selects the plugin for c (SRC-071..SRC-075): with a name, that socket
// (used unconditionally without discovery rules, else when it matches); without, the
// first socket in lexical order whose discovery matches. env is sent with discovery.
func (r *Renderer) detectPlugin(ctx context.Context, c *call, name string, env []string) (*pluginConn, error) {
	var archive *sourceArchive
	defer func() {
		if archive != nil {
			archive.close()
		}
	}()
	match := func(p *pluginConn) (bool, bool, error) {
		if archive == nil {
			a, err := r.archiveSource(ctx, c, c.root, env)
			if err != nil {
				return false, false, err
			}
			archive = a
		}
		var resp cmpwire.MatchResponse
		err := p.stream(ctx, cmpwire.PathMatch, archive, &resp)
		r.metrics.observeCMP("match", err)
		if err != nil {
			return false, false, err
		}
		return resp.IsSupported, resp.IsDiscoveryEnabled, nil
	}
	supports := func(file string, named bool) (*pluginConn, bool, error) {
		p, err := r.dialPlugin(ctx, file, c.limits.MaxOutputBytes)
		if err != nil {
			return nil, false, err
		}
		if !p.config.IsDiscoveryConfigured {
			if named {
				return p, true, nil
			}
			p.close()
			return nil, false, nil
		}
		supported, discoveryEnabled, err := match(p)
		if err != nil {
			p.close()
			return nil, false, fmt.Errorf("error checking repository support for plugin %s: %w", file, err)
		}
		if !supported {
			if !discoveryEnabled && named {
				return p, true, nil
			}
			p.close()
			return nil, false, nil
		}
		return p, true, nil
	}

	if name != "" {
		p, found, err := supports(name+cmpwire.SocketSuffix, true)
		if !found {
			if err != nil {
				return nil, fmt.Errorf("could not find cmp-server plugin with name %q supporting the given repository: %w", name, err)
			}
			return nil, fmt.Errorf("could not find cmp-server plugin with name %q supporting the given repository", name)
		}
		return p, nil
	}
	entries, err := os.ReadDir(r.cfg.PluginSocketDir)
	if err != nil {
		return nil, fmt.Errorf("failed to list all plugins in dir: %w", err)
	}
	// As upstream, lastErr is the error of the last probed socket (nil when that
	// plugin answered but did not match).
	var lastErr error
	for _, e := range entries {
		if e.Type()&fs.ModeSocket == 0 {
			continue
		}
		p, found, err := supports(e.Name(), false)
		lastErr = err
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if found {
			return p, nil
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("could not find plugin supporting the given repository: %w", lastErr)
	}
	return nil, errors.New("could not find plugin supporting the given repository")
}

// pluginEnv returns the generate environment (SRC-078..SRC-080): the build
// environment, KUBE_VERSION and KUBE_API_VERSIONS (cluster values), the plugin env as
// ARGOCD_ENV_<name> (values substituted with the former) and the parameter variables.
func (c *call) pluginEnv() ([]string, error) {
	kubeVersion, err := helm.NormalizeKubeVersion(c.in.KubeVersion)
	if err != nil {
		return nil, err
	}
	env := c.env.Environ()
	env = append(env, "KUBE_VERSION="+kubeVersion, "KUBE_API_VERSIONS="+strings.Join(c.in.APIVersions, ","))
	return pluginParamEnv(env, c.src.Plugin)
}

// pluginParamEnv appends the plugin env and parameter variables to env.
func pluginParamEnv(env []string, plugin *v1alpha1.ApplicationSourcePlugin) ([]string, error) {
	if plugin == nil {
		return env, nil
	}
	base := newEnvFromList(env)
	out := slices.Clone(env)
	for _, e := range plugin.Env {
		out = append(out, "ARGOCD_ENV_"+e.Name+"="+base.Envsubst(e.Value))
	}
	params, err := pluginParametersEnv(plugin.Parameters)
	if err != nil {
		return nil, fmt.Errorf("failed to generate env vars from parameters: %w", err)
	}
	return append(out, params...), nil
}

// pluginParametersEnv renders plugin parameters as ARGOCD_APP_PARAMETERS (JSON) and
// PARAM_<NAME>[_<KEY>|_<i>] variables (upstream ApplicationSourcePluginParameters.Environ).
// Map keys are emitted in sorted order.
func pluginParametersEnv(params v1alpha1.ApplicationSourcePluginParameters) ([]string, error) {
	data, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	env := []string{"ARGOCD_APP_PARAMETERS=" + string(data)}
	for _, p := range params {
		base := "PARAM_" + escapeParamName(p.Name)
		if p.String != nil {
			env = append(env, base+"="+*p.String)
		}
		if p.OptionalMap != nil {
			keys := make([]string, 0, len(p.Map))
			for k := range p.Map {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				env = append(env, base+"_"+escapeParamName(k)+"="+p.Map[k])
			}
		}
		if p.OptionalArray != nil {
			for i, v := range p.Array {
				env = append(env, fmt.Sprintf("%s_%d=%s", base, i, v))
			}
		}
	}
	return env, nil
}

// escapeParamName upper-cases name (Unicode case mapping, as strings.ToUpper) and then
// replaces every rune outside [A-Z0-9_] with '_' (upstream escaped()).
func escapeParamName(name string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return '_'
	}, strings.ToUpper(name))
}

// renderPlugin renders a source with a Config Management Plugin sidecar (SRC-077..
// SRC-087).
func (r *Renderer) renderPlugin(ctx context.Context, c *call) ([]*unstructured.Unstructured, error) {
	env, err := c.pluginEnv()
	if err != nil {
		return nil, err
	}
	name := ""
	if c.src.Plugin != nil {
		name = c.src.Plugin.Name
	}
	p, err := r.detectPlugin(ctx, c, name, env)
	if err != nil {
		return nil, err
	}
	defer p.close()
	if p.config.ProvideGitCreds {
		// ADR-0007: sidecars never receive repository credentials.
		c.log.Warn("plugin requests git credentials (provideGitCreds); Patrol does not forward repository credentials to plugins", "plugin", strings.TrimSuffix(p.name, cmpwire.SocketSuffix))
	}
	root := c.root
	if r.cfg.PluginUseManifestGeneratePaths {
		root = applicationRootPath(c.root, c.appPath, c.in.ManifestGeneratePaths, c.log)
	}
	archive, err := r.archiveSource(ctx, c, root, env)
	if err != nil {
		return nil, err
	}
	defer archive.close()
	if c.in.OnSourceArchived != nil {
		c.in.OnSourceArchived()
	}
	var resp cmpwire.ManifestResponse
	err = p.stream(ctx, cmpwire.PathGenerate, archive, &resp)
	r.metrics.observeCMP("generate", err)
	if err != nil {
		return nil, fmt.Errorf("error generating manifests in cmp: %w", err)
	}
	var objs []*unstructured.Unstructured
	for _, m := range resp.Manifests {
		parsed, err := splitYAML([]byte(m), c.budget)
		if err != nil {
			return nil, fmt.Errorf("failed to convert CMP manifests to unstructured objects: %w", err)
		}
		objs = append(objs, parsed...)
	}
	return objs, nil
}

// PluginParameters returns the parameter announcements of the plugin serving in
// (SRC-124): the plugin is selected as for generation, with the reduced environment
// upstream sends for announcements, and the whole repository is streamed.
func (r *Renderer) PluginParameters(ctx context.Context, in Input) ([]apiclient.ParameterAnnouncement, error) {
	c := &call{in: in, src: in.Source.DeepCopy(), limits: mergeLimits(r.cfg.Limits, in.Limits).normalized(), log: r.log.With("app", in.AppName)}
	if !filepath.IsAbs(in.RootDir) {
		return nil, fmt.Errorf("render: root directory %q must be absolute", in.RootDir)
	}
	rel := in.AppPath
	if rel == "" {
		rel = in.Source.Path
	}
	appPath, err := resolveAppPath(in.RootDir, rel)
	if err != nil {
		return nil, err
	}
	c.root, c.appPath = filepath.Clean(in.RootDir), appPath
	c.env = newBuildEnv(in).subset("APP_NAME", "APP_SOURCE_REPO_URL", "APP_SOURCE_PATH", "APP_SOURCE_TARGET_REVISION")
	env, err := pluginParamEnv(c.env.Environ(), c.src.Plugin)
	if err != nil {
		return nil, fmt.Errorf("failed to get env vars for plugin: %w", err)
	}
	name := ""
	if c.src.Plugin != nil {
		name = c.src.Plugin.Name
	}
	p, err := r.detectPlugin(ctx, c, name, env)
	if err != nil {
		return nil, fmt.Errorf("failed to detect CMP for app: %w", err)
	}
	defer p.close()
	archive, err := r.archiveSource(ctx, c, c.root, env)
	if err != nil {
		return nil, fmt.Errorf("error sending file to cmp-server: %w", err)
	}
	defer archive.close()
	var resp cmpwire.ParametersResponse
	err = p.stream(ctx, cmpwire.PathParameters, archive, &resp)
	r.metrics.observeCMP("parameters", err)
	if err != nil {
		return nil, fmt.Errorf("failed to get parameter announcement: %w", err)
	}
	return resp.ParameterAnnouncements, nil
}
