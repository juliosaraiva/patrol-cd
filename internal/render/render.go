package render

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/render/cmpwire"
	"github.com/juliosaraiva/patrol-cd/internal/render/jsonnet"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

// Defaults applied when the corresponding Limits field is zero.
const (
	// DefaultMaxCombinedDirectoryManifestsSize is upstream's
	// reposerver.max.combined.directory.manifests.size default (10M).
	DefaultMaxCombinedDirectoryManifestsSize int64 = 10 * 1000 * 1000
	// DefaultMaxManifests caps the number of objects one source renders to.
	DefaultMaxManifests = 100000
	// DefaultMaxOutputBytes caps tool output (helm, kustomize, jsonnet), the CMP
	// response and the serialized size of all rendered objects.
	DefaultMaxOutputBytes int64 = 256 << 20
	// DefaultMaxScratchCopyBytes and DefaultMaxScratchCopyFiles bound the scratch copy
	// of a repository made for Kustomize overrides.
	DefaultMaxScratchCopyBytes int64 = 1 << 30
	DefaultMaxScratchCopyFiles       = 500000
	// DefaultMaxOverrideFileBytes caps one parameter override file.
	DefaultMaxOverrideFileBytes int64 = 1 << 20
	// DefaultMaxCMPTarBytes caps the source archive streamed to a plugin.
	DefaultMaxCMPTarBytes int64 = 1 << 30
)

// Limits bound one Generate call. Zero fields take the package defaults; a negative
// value disables the corresponding limit.
type Limits struct {
	// MaxCombinedDirectoryManifestsSize bounds the summed size of the YAML/JSON files
	// of a Directory source (Jsonnet files do not count). Upstream's value 0
	// ("unlimited") maps to a negative value here.
	MaxCombinedDirectoryManifestsSize int64
	// MaxManifests bounds the number of rendered objects (after List flattening).
	MaxManifests int
	// MaxOutputBytes bounds tool output and the serialized size of the result.
	MaxOutputBytes int64
	// MaxScratchCopyBytes and MaxScratchCopyFiles bound the scratch copy made for
	// Kustomize overrides.
	MaxScratchCopyBytes int64
	MaxScratchCopyFiles int
	// MaxOverrideFileBytes bounds each parameter override file.
	MaxOverrideFileBytes int64
	// MaxCMPTarBytes bounds the compressed source archive streamed to a plugin.
	MaxCMPTarBytes int64
}

func (l Limits) normalized() Limits {
	def := func(v, d int64) int64 {
		if v == 0 {
			return d
		}
		return v
	}
	l.MaxCombinedDirectoryManifestsSize = def(l.MaxCombinedDirectoryManifestsSize, DefaultMaxCombinedDirectoryManifestsSize)
	l.MaxOutputBytes = def(l.MaxOutputBytes, DefaultMaxOutputBytes)
	l.MaxScratchCopyBytes = def(l.MaxScratchCopyBytes, DefaultMaxScratchCopyBytes)
	l.MaxOverrideFileBytes = def(l.MaxOverrideFileBytes, DefaultMaxOverrideFileBytes)
	l.MaxCMPTarBytes = def(l.MaxCMPTarBytes, DefaultMaxCMPTarBytes)
	if l.MaxManifests == 0 {
		l.MaxManifests = DefaultMaxManifests
	}
	if l.MaxScratchCopyFiles == 0 {
		l.MaxScratchCopyFiles = DefaultMaxScratchCopyFiles
	}
	return l
}

// Config is the process-wide renderer configuration.
type Config struct {
	// Helm renders Helm sources and pulls chart sources. Required for Helm.
	Helm *helm.Client
	// KustomizePath is the default kustomize binary (absolute, or a bare name looked
	// up in ToolPATH). Empty means "kustomize".
	KustomizePath string
	// HelmPath is the helm binary kustomize --enable-helm uses (through a wrapper
	// that pins HELM_DATA_HOME/HELM_CONFIG_HOME/HELM_PLUGINS). Empty means "helm".
	HelmPath string
	// GitPath is the git binary kustomize remote bases use (through the
	// argument-injection guard wrapper). Empty means "git".
	GitPath string
	// ToolPATH is the PATH given to tools. Empty means the PATH of sandbox.BaseEnv.
	ToolPATH string
	// WorkDir holds per-call scratch directories (Kustomize copies, CMP archives,
	// tool wrappers). Empty means os.TempDir().
	WorkDir string
	// Exec bounds every kustomize invocation (timeout, rlimits). MaxStdout defaults
	// to Limits.MaxOutputBytes.
	Exec sandbox.Limits
	// Jsonnet runs Directory-source Jsonnet files in a child process.
	Jsonnet jsonnet.Runner
	// PluginSocketDir is the CMP socket directory. Empty means
	// cmpwire.DefaultSocketDir.
	PluginSocketDir string
	// PluginTarExclusions are filepath.Match globs (relative to the archive root)
	// excluded from archives streamed to plugins (reposerver.plugin.tar.exclusions).
	// ".git" is always excluded.
	PluginTarExclusions []string
	// PluginUseManifestGeneratePaths streams only the common root of the app path and
	// the manifest-generate-paths annotation paths to the plugin generate call
	// (reposerver.plugin.use.manifest.generate.paths).
	PluginUseManifestGeneratePaths bool
	// CMPRetryAttempts and CMPRetryBackoff configure retries of the plugin config call
	// on connection failures (upstream: 3 attempts, linear 1s backoff).
	CMPRetryAttempts int
	CMPRetryBackoff  time.Duration
	// CAData returns the PEM CA bundle configured for a repository host (Helm chart
	// pulls and dependency repositories), or nil.
	CAData func(host string) []byte
	// Limits are the default per-call limits.
	Limits Limits
	// Registerer receives the renderer metrics. Nil disables metrics.
	Registerer prometheus.Registerer
	// Logger receives structured logs. Nil means slog.Default().
	Logger *slog.Logger
}

// Input describes one source to render.
type Input struct {
	// RootDir is the absolute path of the obtained content: the repository checkout,
	// extracted OCI artifact or streamed directory. It is not modified (Kustomize
	// overrides are applied to a scratch copy). Ignored for Helm chart sources
	// (Source.Chart set), whose root is the pulled chart.
	RootDir string
	// AppPath is the application directory relative to RootDir. Empty means
	// Source.Path; both empty mean the root.
	AppPath string
	// Source is the application source. It is not modified; override files are
	// merged into a copy.
	Source v1alpha1.ApplicationSource
	// Revision is the resolved revision (commit SHA, chart version, OCI digest or
	// "streamed"), exported as ARGOCD_APP_REVISION. For Helm chart sources it is the
	// chart version to pull (Source.TargetRevision is resolved when empty).
	Revision string
	// Repo is the source repository (URL, credentials, proxy, TLS options).
	Repo apiclient.Repository
	// AppName is the Application instance name ("<name>", or "<namespace>_<name>"
	// for Applications outside the control-plane namespace).
	AppName string
	// ProjectName is the Application's project.
	ProjectName string
	// Namespace is the destination namespace.
	Namespace string
	// KubeVersion and APIVersions describe the destination cluster.
	KubeVersion string
	APIVersions []string
	// Repos and HelmRepoCreds are the repositories and credential templates visible to
	// the project, used for Helm dependency repositories.
	Repos         []apiclient.Repository
	HelmRepoCreds []apiclient.RepositoryCredentialTemplate
	// ProjectSourceRepos are the project's sourceRepos globs, used to explain failed
	// dependency builds (SRC-041).
	ProjectSourceRepos []string
	// HasMultipleSources enables `$ref` validation of value files and file
	// parameters.
	HasMultipleSources bool
	// RefSources maps "$<ref>" (the "$" is optional) to the root directory of the
	// referenced source's checkout, for `$<ref>/path` value files and file parameters.
	RefSources map[string]string
	// KustomizeOptions and HelmOptions carry patrol-cm settings.
	KustomizeOptions apiclient.KustomizeOptions
	HelmOptions      apiclient.HelmOptions
	// EnabledSourceTypes maps source type names ("Helm", "Kustomize", "Directory",
	// "Plugin") to false to disable them; missing keys are enabled.
	EnabledSourceTypes map[string]bool
	// ManifestGeneratePaths is the Application's manifest-generate-paths annotation
	// (used with Config.PluginUseManifestGeneratePaths).
	ManifestGeneratePaths string
	// GitCreds, when set, authenticates Kustomize remote bases with the application
	// repository's credentials (upstream behavior). Never forwarded to plugins.
	GitCreds git.Creds
	// Tracking, when set together with AppName, writes resource tracking metadata on
	// every rendered object except CRDs (upstream repo-server behavior).
	Tracking *Tracking
	// OnSourceArchived is called once a plugin's source archive has been written, so
	// the caller can release its repository lock early (upstream tarDoneCh).
	OnSourceArchived func()
	// Limits override Config.Limits field by field when non-zero.
	Limits Limits
}

// Tracking configures resource tracking metadata.
type Tracking struct {
	Tracker        tracking.Tracker
	Method         tracking.Method
	InstallationID string
}

// Result is the rendered output.
type Result struct {
	// Manifests are the rendered objects, Lists flattened, in tool output order.
	Manifests []*unstructured.Unstructured
	// SourceType is the effective tool: Helm, Kustomize, Directory or Plugin.
	SourceType string
	// Commands are the executed helm/kustomize command lines with checkout paths
	// replaced by "." (never credentials).
	Commands []string
}

// Renderer renders sources. It is safe for concurrent use.
type Renderer struct {
	cfg     Config
	log     *slog.Logger
	metrics *metrics
	helm    *helmClients

	kustVersionsMu sync.Mutex
	kustVersions   map[string]kustVersion
}

// New validates cfg and returns a Renderer.
func New(cfg Config) (*Renderer, error) {
	if cfg.WorkDir == "" {
		cfg.WorkDir = os.TempDir()
	}
	if !filepath.IsAbs(cfg.WorkDir) {
		return nil, fmt.Errorf("render: work directory %q must be absolute", cfg.WorkDir)
	}
	if cfg.PluginSocketDir == "" {
		cfg.PluginSocketDir = cmpwire.DefaultSocketDir
	}
	if cfg.KustomizePath == "" {
		cfg.KustomizePath = "kustomize"
	}
	if cfg.HelmPath == "" {
		cfg.HelmPath = "helm"
	}
	if cfg.GitPath == "" {
		cfg.GitPath = "git"
	}
	if cfg.CMPRetryAttempts <= 0 {
		cfg.CMPRetryAttempts = 3
	}
	if cfg.CMPRetryBackoff <= 0 {
		cfg.CMPRetryBackoff = time.Second
	}
	for _, p := range cfg.PluginTarExclusions {
		if _, err := filepath.Match(p, ""); err != nil {
			return nil, fmt.Errorf("render: error verifying exclusion pattern %q: %w", p, err)
		}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	m, err := newMetrics(cfg.Registerer)
	if err != nil {
		return nil, err
	}
	return &Renderer{
		cfg: cfg, log: logger.With("component", "render"), metrics: m,
		helm: newHelmClients(cfg.Helm, cfg.WorkDir), kustVersions: map[string]kustVersion{},
	}, nil
}

// call is the state of one Generate call.
type call struct {
	in      Input
	src     *v1alpha1.ApplicationSource
	root    string // absolute, symlinks of components inside not resolved
	appPath string // absolute
	env     *buildEnv
	limits  Limits
	log     *slog.Logger
	// budget counts decoded objects against limits.MaxManifests across every decode
	// of the call (nil: unlimited).
	budget *manifestBudget
	// helm is the helm client for the repository proxy (see Renderer.helmClient);
	// release runs when the call ends.
	helm    *helm.Client
	release []func()
}

// helmClient returns the helm client for the call's repository proxy: the source
// repository's for `helm template` and `dependency build`, which for a chart source
// is also the chart repository used for the pull (upstream SRC-136).
func (r *Renderer) helmClient(c *call) (*helm.Client, error) {
	if c.helm != nil {
		return c.helm, nil
	}
	cl, release, err := r.helm.forProxy(c.in.Repo.Proxy, c.in.Repo.NoProxy)
	if err != nil {
		return nil, err
	}
	c.helm = cl
	c.release = append(c.release, release)
	return cl, nil
}

// end runs the release functions of the call, last acquired first.
func (c *call) end() {
	for i := len(c.release) - 1; i >= 0; i-- {
		c.release[i]()
	}
	c.release = nil
}

func (c *call) toolPATH(r *Renderer) string {
	if r.cfg.ToolPATH != "" {
		return r.cfg.ToolPATH
	}
	for _, kv := range sandbox.BaseEnv() {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			return v
		}
	}
	return sandbox.DefaultPath
}

// Generate renders in. Errors carry the upstream messages where they exist; see the
// component documentation for the list.
func (r *Renderer) Generate(ctx context.Context, in Input) (*Result, error) {
	start := time.Now()
	res, err := r.generate(ctx, in)
	sourceType := ""
	if res != nil {
		sourceType = res.SourceType
	}
	r.metrics.observeRender(sourceType, err, time.Since(start))
	logArgs := []any{
		"app", in.AppName, "repo", redactURL(in.Repo.URL), "path", in.Source.Path,
		"chart", in.Source.Chart, "sourceType", sourceType, "duration", time.Since(start),
	}
	if err != nil {
		r.log.LogAttrs(ctx, slog.LevelDebug, "render failed", slog.Group("request", logArgs...), slog.String("error", err.Error()))
		return nil, err
	}
	r.log.LogAttrs(ctx, slog.LevelDebug, "rendered", slog.Group("request", logArgs...), slog.Int("manifests", len(res.Manifests)))
	return res, nil
}

func (r *Renderer) generate(ctx context.Context, in Input) (*Result, error) {
	c := &call{in: in, src: in.Source.DeepCopy(), limits: mergeLimits(r.cfg.Limits, in.Limits).normalized()}
	c.log = r.log.With("app", in.AppName)
	c.budget = newManifestBudget(c.limits.MaxManifests)
	defer c.end()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if in.Source.Chart != "" {
		chartDir, cleanup, err := r.pullChart(ctx, c)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		c.root, c.appPath = chartDir, chartDir
	} else {
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
	}
	c.env = newBuildEnv(c.in)

	sourceType, err := r.sourceType(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("error getting app source type: %w", err)
	}

	var (
		objs     []*unstructured.Unstructured
		commands []string
	)
	switch sourceType {
	case v1alpha1.ApplicationSourceTypeHelm:
		var cmd string
		objs, cmd, err = r.renderHelm(ctx, c)
		if cmd != "" {
			commands = append(commands, cmd)
		}
	case v1alpha1.ApplicationSourceTypeKustomize:
		objs, commands, err = r.renderKustomize(ctx, c)
	case v1alpha1.ApplicationSourceTypePlugin:
		objs, err = r.renderPlugin(ctx, c)
		if err != nil {
			err = fmt.Errorf("CMP processing failed for application %q: %w", in.AppName, err)
		}
	default:
		sourceType = v1alpha1.ApplicationSourceTypeDirectory
		objs, err = r.renderDirectory(ctx, c)
	}
	if err != nil {
		return nil, err
	}
	manifests, err := c.postProcess(objs)
	if err != nil {
		return nil, err
	}
	return &Result{Manifests: manifests, SourceType: string(sourceType), Commands: commands}, nil
}

func mergeLimits(base, over Limits) Limits {
	pick := func(a, b int64) int64 {
		if b != 0 {
			return b
		}
		return a
	}
	out := base
	out.MaxCombinedDirectoryManifestsSize = pick(base.MaxCombinedDirectoryManifestsSize, over.MaxCombinedDirectoryManifestsSize)
	out.MaxOutputBytes = pick(base.MaxOutputBytes, over.MaxOutputBytes)
	out.MaxScratchCopyBytes = pick(base.MaxScratchCopyBytes, over.MaxScratchCopyBytes)
	out.MaxOverrideFileBytes = pick(base.MaxOverrideFileBytes, over.MaxOverrideFileBytes)
	out.MaxCMPTarBytes = pick(base.MaxCMPTarBytes, over.MaxCMPTarBytes)
	if over.MaxManifests != 0 {
		out.MaxManifests = over.MaxManifests
	}
	if over.MaxScratchCopyFiles != 0 {
		out.MaxScratchCopyFiles = over.MaxScratchCopyFiles
	}
	return out
}

// Errors of output validation.
var (
	// ErrInvalidManifest reports a rendered object without apiVersion or kind.
	ErrInvalidManifest = errors.New("invalid manifest")
	// ErrTooManyManifests reports output over Limits.MaxManifests.
	ErrTooManyManifests = errors.New("too many manifests")
	// ErrOutputTooLarge reports output over Limits.MaxOutputBytes.
	ErrOutputTooLarge = errors.New("rendered manifests exceed the size limit")
)

// postProcess flattens Lists, drops nil objects and null lists, validates every
// object, applies tracking metadata and enforces count and size limits.
func (c *call) postProcess(objs []*unstructured.Unstructured) ([]*unstructured.Unstructured, error) {
	out := make([]*unstructured.Unstructured, 0, len(objs))
	var size int64
	add := func(o *unstructured.Unstructured) error {
		if err := validateObject(o); err != nil {
			return err
		}
		if c.limits.MaxManifests > 0 && len(out) >= c.limits.MaxManifests {
			return fmt.Errorf("%w: more than %d objects", ErrTooManyManifests, c.limits.MaxManifests)
		}
		if t := c.in.Tracking; t != nil && t.Tracker != nil && c.in.AppName != "" && !tracking.IsCRD(o) {
			if err := t.Tracker.SetAppInstance(o, c.in.AppName, c.in.Namespace, t.Method, t.InstallationID); err != nil {
				return fmt.Errorf("failed to set app instance tracking info on manifest: %w", err)
			}
		}
		if c.limits.MaxOutputBytes > 0 {
			data, err := json.Marshal(o.Object)
			if err != nil {
				return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
			}
			size += int64(len(data))
			if size > c.limits.MaxOutputBytes {
				return fmt.Errorf("%w (%d bytes)", ErrOutputTooLarge, c.limits.MaxOutputBytes)
			}
		}
		out = append(out, o)
		return nil
	}
	for _, obj := range objs {
		if obj == nil || obj.Object == nil {
			continue
		}
		switch {
		case obj.IsList():
			var items []*unstructured.Unstructured
			if err := obj.EachListItem(func(item runtime.Object) error {
				u, ok := item.(*unstructured.Unstructured)
				if !ok {
					return errors.New("resource list item has unexpected type")
				}
				items = append(items, u)
				return nil
			}); err != nil {
				return nil, err
			}
			for _, it := range items {
				if err := add(it); err != nil {
					return nil, err
				}
			}
		case isNullList(obj):
		default:
			if err := add(obj); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// isNullList reports a List kind whose items are null ("items: null" with neither spec
// nor status), which upstream renders as nothing.
func isNullList(obj *unstructured.Unstructured) bool {
	if _, ok := obj.Object["spec"]; ok {
		return false
	}
	if _, ok := obj.Object["status"]; ok {
		return false
	}
	field, ok := obj.Object["items"]
	return ok && field == nil
}

func validateObject(o *unstructured.Unstructured) error {
	apiVersion, _ := o.Object["apiVersion"].(string)
	kind, _ := o.Object["kind"].(string)
	if apiVersion == "" || kind == "" {
		name := o.GetName()
		return fmt.Errorf("%w: object %q has no apiVersion or kind", ErrInvalidManifest, name)
	}
	if md, ok := o.Object["metadata"]; ok && md != nil {
		if _, ok := md.(map[string]any); !ok {
			return fmt.Errorf("%w: %s %q has malformed metadata", ErrInvalidManifest, kind, o.GetName())
		}
	}
	return nil
}
