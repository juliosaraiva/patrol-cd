package helm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	k8sversion "k8s.io/apimachinery/pkg/util/version"
)

// ExtraValuesPlaceholder replaces the path of the temporary file holding
// TemplateOpts.Values in returned command lines (upstream wording).
const ExtraValuesPlaceholder = "<temp file with values from source.helm.values/valuesObject>"

// KV is one --set, --set-string or --set-file parameter.
type KV struct {
	Name  string
	Value string
}

// TemplateOpts are the inputs of `helm template`. Paths in ValueFiles and
// SetFileParams must already be resolved (ResolveValueFilePathOrURL): absolute
// local paths or remote URLs.
type TemplateOpts struct {
	// ReleaseName is passed as --name-template.
	ReleaseName string
	// Namespace is passed as --namespace when non-empty.
	Namespace string
	// KubeVersion is normalized (NormalizeKubeVersion) and passed as
	// --kube-version when non-empty.
	KubeVersion string
	// APIVersions are passed as one --api-versions flag each.
	APIVersions []string
	// ValueFiles are passed as --values in order.
	ValueFiles []string
	// Values is inline YAML (helm.values / helm.valuesObject) written to a 0600
	// temp file and passed as the last --values.
	Values []byte
	// SetParams, SetStringParams and SetFileParams become --set, --set-string and
	// --set-file. Duplicate names keep the last value at the first position.
	// Unescaped commas are escaped unless the value is a {list}.
	SetParams, SetStringParams, SetFileParams []KV
	// SkipCRDs omits --include-crds (passed by default). IncludeCRDs forces it
	// even when SkipCRDs is set.
	SkipCRDs, IncludeCRDs bool
	// SkipSchemaValidation and SkipTests add the corresponding flags.
	SkipSchemaValidation, SkipTests bool
	// PassCredentials has no effect on template; it is accepted so a whole
	// helm source spec can be mapped onto TemplateOpts (see PullChart and
	// RepoCreds for where it applies).
	PassCredentials bool
	// Version is the deprecated helm.version: "", "v3" and "v4" are accepted and
	// all use the bundled Helm 4; anything else fails with ErrUnsupportedVersion.
	Version string
}

// CheckVersion validates helm.version.
func CheckVersion(v string) error {
	switch v {
	case "", "v3", "v4":
		return nil
	}
	return tag(ErrUnsupportedVersion, fmt.Errorf("helm version '%s' is not supported", v))
}

// NormalizeKubeVersion reduces a Kubernetes version to MAJOR.MINOR[.PATCH],
// dropping a leading "v" and vendor suffixes ("v1.30.11+IKS" -> "1.30.11"), as
// upstream does before passing --kube-version. Empty stays empty.
func NormalizeKubeVersion(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	parsed, err := k8sversion.ParseGeneric(v)
	if err != nil {
		return "", tag(ErrInvalidArgument, fmt.Errorf("could not parse kubernetes version %s: %w", v, err))
	}
	return parsed.String(), nil
}

// Template runs `helm template . ...` in chartDir and returns the rendered
// manifests (multi-document YAML).
func (c *Client) Template(ctx context.Context, chartDir string, opts TemplateOpts) ([]byte, error) {
	out, _, err := c.TemplateWithCommand(ctx, chartDir, opts)
	return out, err
}

// TemplateWithCommand is Template that also returns the executed command line
// for ManifestResponse.commands, with the inline values file replaced by
// ExtraValuesPlaceholder. Callers replace repository checkout paths themselves.
//
// The call uses a fresh helm home (config, caches, registry/repository config,
// plugins) that is removed afterwards. If Chart.lock did not exist before the
// call, a Chart.lock left by helm is removed (helm/helm#6870 workaround). On
// failure, consecutive "--api-versions X " sequences in the message are replaced
// by "<api versions removed> ".
func (c *Client) TemplateWithCommand(ctx context.Context, chartDir string, opts TemplateOpts) ([]byte, string, error) {
	if err := c.init(); err != nil {
		return nil, "", err
	}
	if err := CheckVersion(opts.Version); err != nil {
		return nil, "", err
	}
	if err := checkChartDir(chartDir); err != nil {
		return nil, "", err
	}
	kubeVersion, err := NormalizeKubeVersion(opts.KubeVersion)
	if err != nil {
		return nil, "", err
	}
	for _, f := range opts.ValueFiles {
		if err := checkResolved(f); err != nil {
			return nil, "", err
		}
	}
	for _, p := range opts.SetFileParams {
		if err := checkResolved(p.Value); err != nil {
			return nil, "", err
		}
	}

	h, err := c.newHome()
	if err != nil {
		return nil, "", err
	}
	defer h.close()
	var extraValues string
	if len(opts.Values) > 0 {
		if extraValues, err = h.writeSecret("values-*.yaml", opts.Values); err != nil {
			return nil, "", err
		}
	}
	restoreLock, err := preserveChartLock(chartDir)
	if err != nil {
		return nil, "", err
	}
	defer restoreLock()

	args := templateArgs(opts, kubeVersion, extraValues)
	out, cmdline, err := c.run(ctx, h, chartDir, args, nil)
	if extraValues != "" {
		cmdline = strings.ReplaceAll(cmdline, extraValues, ExtraValuesPlaceholder)
	}
	if err != nil {
		msg := err.Error()
		if extraValues != "" {
			msg = strings.ReplaceAll(msg, extraValues, ExtraValuesPlaceholder)
		}
		if strings.Contains(msg, "--api-versions") {
			msg = apiVersionsRemover.ReplaceAllString(msg, "<api versions removed> ")
		}
		return nil, cmdline, &templateError{msg: "failed to execute helm template command: " + msg, err: err}
	}
	return out, cmdline, nil
}

// templateError keeps the rewritten message while unwrapping to the original
// error (sandbox.ExitError, ErrTimeout, ...).
type templateError struct {
	msg string
	err error
}

func (e *templateError) Error() string { return e.msg }
func (e *templateError) Unwrap() error { return e.err }

var apiVersionsRemover = regexp.MustCompile(`(--api-versions [^ ]+ )+`)

func templateArgs(opts TemplateOpts, kubeVersion, extraValues string) []string {
	args := []string{"template", ".", "--name-template", opts.ReleaseName}
	if opts.Namespace != "" {
		args = append(args, "--namespace", opts.Namespace)
	}
	if kubeVersion != "" {
		args = append(args, "--kube-version", kubeVersion)
	}
	for _, p := range dedupKV(opts.SetParams) {
		args = append(args, "--set", p.Name+"="+cleanSetParameter(p.Value))
	}
	for _, p := range dedupKV(opts.SetStringParams) {
		args = append(args, "--set-string", p.Name+"="+cleanSetParameter(p.Value))
	}
	for _, p := range dedupKV(opts.SetFileParams) {
		args = append(args, "--set-file", p.Name+"="+cleanSetParameter(p.Value))
	}
	for _, f := range opts.ValueFiles {
		args = append(args, "--values", f)
	}
	if extraValues != "" {
		args = append(args, "--values", extraValues)
	}
	for _, v := range opts.APIVersions {
		args = append(args, "--api-versions", v)
	}
	if !opts.SkipCRDs || opts.IncludeCRDs {
		args = append(args, "--include-crds")
	}
	if opts.SkipSchemaValidation {
		args = append(args, "--skip-schema-validation")
	}
	if opts.SkipTests {
		args = append(args, "--skip-tests")
	}
	return args
}

// dedupKV keeps one entry per name: the last value, at the first position
// (upstream collects parameters in a map, so the last value wins).
func dedupKV(in []KV) []KV {
	if len(in) < 2 {
		return in
	}
	pos := make(map[string]int, len(in))
	out := make([]KV, 0, len(in))
	for _, kv := range in {
		if i, ok := pos[kv.Name]; ok {
			out[i].Value = kv.Value
			continue
		}
		pos[kv.Name] = len(out)
		out = append(out, kv)
	}
	return out
}

// cleanSetParameter escapes commas that are not already escaped, so helm treats
// them as part of the value, except for values wrapped in {} (helm list syntax).
func cleanSetParameter(v string) string {
	if strings.HasPrefix(v, "{") && strings.HasSuffix(v, "}") {
		return v
	}
	if !strings.Contains(v, ",") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v) + 4)
	var prev rune
	for _, r := range v {
		if r == ',' && prev != '\\' {
			b.WriteString(`\,`)
		} else {
			b.WriteRune(r)
		}
		prev = r
	}
	return b.String()
}

func checkChartDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: chart directory must be absolute", ErrInvalidArgument)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("%w: chart directory: %w", ErrInvalidArgument, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: chart directory is not a directory", ErrInvalidArgument)
	}
	return nil
}

// checkResolved accepts absolute paths and URLs with a scheme.
func checkResolved(p string) error {
	if filepath.IsAbs(p) {
		return nil
	}
	if u, err := url.Parse(p); err == nil && u.Scheme != "" {
		return nil
	}
	return tag(ErrInvalidValueFile, fmt.Errorf("value file %q is not resolved (absolute path or URL required)", p))
}

// preserveChartLock records whether Chart.lock exists and returns a function
// that removes a Chart.lock created in the meantime.
func preserveChartLock(chartDir string) (func(), error) {
	lock := filepath.Join(chartDir, "Chart.lock")
	_, err := os.Lstat(lock)
	switch {
	case err == nil:
		return func() {}, nil
	case errors.Is(err, fs.ErrNotExist):
		return func() { _ = os.Remove(lock) }, nil
	default:
		return nil, fmt.Errorf("failed to clean up chart lock file: failed to check lock file status: %w", err)
	}
}
