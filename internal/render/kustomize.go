package render

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Masterminds/semver/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// PatchesCommandComment is appended to Result.Commands when patches were written into
// the kustomization (upstream wording).
const PatchesCommandComment = "# kustomization.yaml updated with patches. There is no `kustomize edit` command for adding patches. In order to generate the manifests in your local environment, you will need to copy the patches into kustomization.yaml manually."

var (
	kustomizeSemverRe = regexp.MustCompile(`v?([0-9]+)(\.[0-9]+)?(\.[0-9]+)?` +
		`(-([0-9A-Za-z\-]+(\.[0-9A-Za-z\-]+)*))?` +
		`(\+([0-9A-Za-z\-]+(\.[0-9A-Za-z\-]+)*))?`)
	unknownKustomizeVersion = semver.MustParse("v99.99.99")
	kustomizeV370           = semver.MustParse("v3.7.0")
	kustomizeV385           = semver.MustParse("v3.8.5")
	kustomizeV530           = semver.MustParse("v5.3.0")
)

type kustVersion struct {
	v *semver.Version
}

// kustomizeBinary selects the kustomize binary (SRC-064): the path registered for
// spec.source.kustomize.version, else the default binary. display is the program name
// shown in commands ("kustomize" for the default, as upstream).
func (r *Renderer) kustomizeBinary(c *call) (path, display string, err error) {
	if k := c.src.Kustomize; k != nil && k.Version != "" {
		p, ok := c.in.KustomizeOptions.Versions[k.Version]
		if !ok || p == "" {
			return "", "", fmt.Errorf("error getting kustomize binary path: kustomize version %s is not registered", k.Version)
		}
		return p, p, nil
	}
	return r.cfg.KustomizePath, "kustomize", nil
}

// kustomizeVersion returns the version of binary, detected once per binary with
// `kustomize version`; an undetectable version is assumed to be v99.99.99 (upstream).
func (r *Renderer) kustomizeVersion(ctx context.Context, c *call, binary, toolPATH string) *semver.Version {
	r.kustVersionsMu.Lock()
	kv, ok := r.kustVersions[binary]
	r.kustVersionsMu.Unlock()
	if ok {
		return kv.v
	}
	v := unknownKustomizeVersion
	res, err := sandbox.Run(ctx, sandbox.Cmd{
		Path:   binary,
		Args:   []string{"version"},
		Env:    sandbox.BaseEnv("PATH=" + toolPATH),
		Limits: sandbox.Limits{Timeout: 30 * time.Second, MaxStdout: 1 << 20, MaxStderr: 1 << 20},
	})
	if err != nil {
		c.log.Warn("failed to detect kustomize version", "binary", filepath.Base(binary), "error", err)
		if ctx.Err() != nil {
			return v // do not cache a cancellation
		}
	} else if m := kustomizeSemverRe.FindString(string(res.Stdout)); m != "" {
		if parsed, perr := semver.NewVersion(m); perr == nil {
			v = parsed
		} else {
			c.log.Warn("failed to parse kustomize version", "binary", filepath.Base(binary), "error", perr)
		}
	}
	r.kustVersionsMu.Lock()
	r.kustVersions[binary] = kustVersion{v: v}
	r.kustVersionsMu.Unlock()
	return v
}

// kustomizeMutates reports whether the overrides modify the kustomization (upstream
// AllowsConcurrentProcessing, plus replicas, which also run `kustomize edit`).
func kustomizeMutates(k *v1alpha1.ApplicationSourceKustomize) bool {
	if k == nil {
		return false
	}
	return k.NamePrefix != "" || k.NameSuffix != "" || len(k.Images) > 0 || len(k.Replicas) > 0 ||
		len(k.CommonLabels) > 0 || len(k.CommonAnnotations) > 0 || k.Namespace != "" ||
		len(k.Patches) > 0 || len(k.Components) > 0
}

// replicaCount returns the integer count of a replica override.
func replicaCount(rp v1alpha1.KustomizeReplica) (int, error) {
	if rp.Count.Type == intstr.String {
		n, err := strconv.Atoi(rp.Count.StrVal)
		if err != nil {
			return 0, fmt.Errorf("expected integer value for count. Received: %s", rp.Count.StrVal)
		}
		return n, nil
	}
	return rp.Count.IntValue(), nil
}

// kustomizeRun is one kustomize execution environment for a call.
type kustomizeRun struct {
	r        *Renderer
	c        *call
	binary   string
	display  string
	root     string // root kustomize sees (the scratch copy when copied)
	limits   sandbox.Limits
	editEnv  []string
	buildEnv []string
	commands []string
}

// commandLine formats argv like upstream GetCommandArgsToLog, with the root replaced
// by ".".
func (k *kustomizeRun) commandLine(args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, k.display)
	for _, a := range args {
		switch {
		case a == "":
			parts = append(parts, `""`)
		case strings.IndexFunc(a, unicode.IsSpace) >= 0:
			parts = append(parts, strconv.Quote(a))
		default:
			parts = append(parts, a)
		}
	}
	return strings.ReplaceAll(strings.Join(parts, " "), k.root, ".")
}

// run executes kustomize and records the command line.
func (k *kustomizeRun) run(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmdline := k.commandLine(args)
	k.commands = append(k.commands, cmdline)
	res, err := sandbox.Run(ctx, sandbox.Cmd{Path: k.binary, Args: args, Dir: dir, Env: env, Limits: k.limits})
	if err != nil {
		// Errors name the shared checkout, never the scratch copy.
		fix := func(s string) string { return strings.ReplaceAll(s, k.root, k.c.root) }
		var ee *sandbox.ExitError
		switch {
		case errors.As(err, &ee):
			return nil, fmt.Errorf("`%s` failed %s", fix(k.rawCommandLine(args)), fix(strings.TrimSpace(ee.Error())))
		case errors.Is(err, sandbox.ErrTimeout):
			timeout := k.limits.Timeout
			if timeout == 0 {
				timeout = sandbox.DefaultTimeout
			}
			return nil, fmt.Errorf("`%s` failed timeout after %s: %w", fix(k.rawCommandLine(args)), timeout, err)
		default:
			return nil, fmt.Errorf("`%s` failed: %w", fix(k.rawCommandLine(args)), err)
		}
	}
	return res.Stdout, nil
}

func (k *kustomizeRun) rawCommandLine(args []string) string {
	return k.display + " " + strings.Join(args, " ")
}

// renderKustomize renders a Kustomize source (SRC-048..SRC-068). Overrides are applied
// with `kustomize edit` to a scratch copy of the repository; the shared checkout is
// never modified.
func (r *Renderer) renderKustomize(ctx context.Context, c *call) ([]*unstructured.Unstructured, []string, error) {
	opts := c.src.Kustomize
	binary, display, err := r.kustomizeBinary(c)
	if err != nil {
		return nil, nil, err
	}
	kubeVersionRaw := kubeVersionOrDefault(c.src, c.in.KubeVersion)
	kubeVersion, err := helm.NormalizeKubeVersion(kubeVersionRaw)
	if err != nil {
		return nil, nil, err
	}
	apiVersions := apiVersionsOrDefault(c.src, c.in.APIVersions)
	buildOptions := c.in.KustomizeOptions.BuildOptions
	enableHelm := strings.Contains(buildOptions, "--enable-helm")

	scratch, err := os.MkdirTemp(r.cfg.WorkDir, "patrol-kustomize-")
	if err != nil {
		return nil, nil, fmt.Errorf("create kustomize scratch directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			c.log.Warn("failed to remove kustomize scratch directory", "error", err)
		}
	}()
	if err := os.Chmod(scratch, 0o700); err != nil {
		return nil, nil, err
	}

	root, appPath := c.root, c.appPath
	if kustomizeMutates(opts) || enableHelm {
		copyRoot := filepath.Join(scratch, "repo")
		lim := c.limits
		if err := copyTree(ctx, c.root, copyRoot, lim.MaxScratchCopyBytes, lim.MaxScratchCopyFiles); err != nil {
			return nil, nil, fmt.Errorf("failed to copy repository for kustomize: %w", err)
		}
		rel, err := relToRoot(c.root, c.appPath)
		if err != nil {
			return nil, nil, err
		}
		root, appPath = copyRoot, filepath.Join(copyRoot, rel)
	}

	toolPATH := c.toolPATH(r)
	home := filepath.Join(scratch, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return nil, nil, err
	}
	wrapperDir, err := r.installWrappers(c, scratch, toolPATH)
	if err != nil {
		return nil, nil, err
	}
	buildPATH := toolPATH
	if wrapperDir != "" {
		buildPATH = wrapperDir + string(os.PathListSeparator) + toolPATH
	}
	homeEnv := []string{
		"HOME=" + home,
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
	}
	editEnv := sandbox.BaseEnv(append([]string{"PATH=" + toolPATH}, homeEnv...)...)
	extra := append([]string{"PATH=" + buildPATH}, homeEnv...)
	extra = append(extra, c.env.Environ()...)
	extra = append(extra, proxyEnv(c.in.Repo.Proxy, c.in.Repo.NoProxy)...)
	if caEnv, err := r.gitCAEnv(c, scratch); err != nil {
		return nil, nil, err
	} else if caEnv != "" {
		extra = append(extra, caEnv)
	}
	if c.in.GitCreds != nil {
		credsDir := filepath.Join(scratch, "creds")
		if err := os.Mkdir(credsDir, 0o700); err != nil {
			return nil, nil, err
		}
		credEnv, cleanup, err := c.in.GitCreds.Environ(credsDir)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get git credentials environment: %w", err)
		}
		defer cleanup()
		extra = append(extra, credEnv...)
	}
	buildEnv := sandbox.BaseEnv(extra...)

	limits := r.cfg.Exec
	if limits.MaxStdout == 0 {
		limits.MaxStdout = c.limits.MaxOutputBytes
	}
	k := &kustomizeRun{r: r, c: c, binary: binary, display: display, root: root, limits: limits, editEnv: editEnv, buildEnv: buildEnv}

	if opts != nil {
		if err := k.applyEdits(ctx, opts, appPath, binary, toolPATH); err != nil {
			return nil, nil, err
		}
	}

	args := []string{"build", appPath}
	if buildOptions != "" {
		args = append(args, strings.Fields(buildOptions)...)
		if enableHelm && !r.kustomizeVersion(ctx, c, binary, toolPATH).LessThan(kustomizeV530) {
			if kubeVersion != "" {
				args = append(args, "--helm-kube-version", kubeVersion)
			}
			for _, v := range apiVersions {
				args = append(args, "--helm-api-versions", v)
			}
		}
	}
	out, err := k.run(ctx, root, buildEnv, args...)
	if err != nil {
		return nil, nil, err
	}
	objs, err := splitYAML(out, c.budget)
	if err != nil {
		return nil, nil, err
	}
	return objs, k.commands, nil
}

// applyEdits runs the `kustomize edit` overrides in the upstream order: nameprefix,
// namesuffix, images, replicas, labels, annotations, namespace, patches, components.
func (k *kustomizeRun) applyEdits(ctx context.Context, opts *v1alpha1.ApplicationSourceKustomize, appPath, binary, toolPATH string) error {
	c := k.c
	edit := func(args ...string) error {
		_, err := k.run(ctx, appPath, k.editEnv, args...)
		return err
	}
	if opts.NamePrefix != "" {
		if err := edit("edit", "set", "nameprefix", "--", opts.NamePrefix); err != nil {
			return err
		}
	}
	if opts.NameSuffix != "" {
		if err := edit("edit", "set", "namesuffix", "--", opts.NameSuffix); err != nil {
			return err
		}
	}
	if len(opts.Images) > 0 {
		args := []string{"edit", "set", "image"}
		for _, img := range opts.Images {
			args = append(args, c.env.Envsubst(string(img)))
		}
		if err := edit(args...); err != nil {
			return err
		}
	}
	if len(opts.Replicas) > 0 {
		args := []string{"edit", "set", "replicas"}
		for _, rp := range opts.Replicas {
			n, err := replicaCount(rp)
			if err != nil {
				return err
			}
			args = append(args, fmt.Sprintf("%s=%d", rp.Name, n))
		}
		if err := edit(args...); err != nil {
			return err
		}
	}
	version := func() *semver.Version { return k.r.kustomizeVersion(ctx, c, binary, toolPATH) }
	if len(opts.CommonLabels) > 0 {
		args := []string{"edit", "add", "label"}
		if opts.ForceCommonLabels {
			args = append(args, "--force")
		}
		if opts.LabelWithoutSelector {
			args = append(args, "--without-selector")
		}
		if opts.LabelIncludeTemplates {
			args = append(args, "--include-templates")
		}
		labels := make(map[string]string, len(opts.CommonLabels))
		for name, value := range opts.CommonLabels {
			labels[name] = c.env.Envsubst(value)
		}
		args = append(args, editAddArgs(labels, version())...)
		if err := edit(args...); err != nil {
			return err
		}
	}
	if len(opts.CommonAnnotations) > 0 {
		args := []string{"edit", "add", "annotation"}
		if opts.ForceCommonAnnotations {
			args = append(args, "--force")
		}
		annotations := opts.CommonAnnotations
		if opts.CommonAnnotationsEnvsubst {
			annotations = make(map[string]string, len(opts.CommonAnnotations))
			for name, value := range opts.CommonAnnotations {
				annotations[name] = c.env.Envsubst(value)
			}
		}
		args = append(args, editAddArgs(annotations, version())...)
		if err := edit(args...); err != nil {
			return err
		}
	}
	if opts.Namespace != "" {
		if err := edit("edit", "set", "namespace", "--", opts.Namespace); err != nil {
			return err
		}
	}
	if len(opts.Patches) > 0 {
		if err := addPatches(appPath, opts.Patches); err != nil {
			return err
		}
		k.commands = append(k.commands, PatchesCommandComment)
	}
	if len(opts.Components) > 0 {
		if version().LessThan(kustomizeV370) {
			return errors.New("kustomize components require kustomize v3.7.0 and above")
		}
		components := opts.Components
		if opts.IgnoreMissingComponents {
			found, err := existingComponents(k.root, appPath, opts.Components)
			if err != nil {
				return err
			}
			components = found
		}
		if len(components) > 0 {
			args := append([]string{"edit", "add", "component"}, components...)
			if _, err := k.run(ctx, appPath, k.buildEnv, args...); err != nil {
				return err
			}
		}
	}
	return nil
}

// editAddArgs formats label/annotation pairs for `kustomize edit add`: one "k:v"
// argument per pair (sorted by key), or a single comma-joined argument for kustomize
// older than v3.8.5.
func editAddArgs(m map[string]string, v *semver.Version) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	pairs := make([]string, len(keys))
	for i, key := range keys {
		pairs[i] = key + ":" + m[key]
	}
	if v.LessThan(kustomizeV385) {
		return []string{strings.Join(pairs, ",")}
	}
	return pairs
}

// existingComponents keeps the components that exist inside root (SRC-063); paths
// escaping root are dropped.
func existingComponents(root, appPath string, components []string) ([]string, error) {
	rt, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("failed to open the repo folder: %w", err)
	}
	defer func() { _ = rt.Close() }()
	var found []string
	for _, comp := range components {
		rel, err := filepath.Rel(root, filepath.Join(appPath, comp))
		if err != nil {
			return nil, fmt.Errorf("kustomize components path failed: %w", err)
		}
		if _, err := rt.Stat(rel); err != nil {
			continue
		}
		found = append(found, comp)
	}
	return found, nil
}

// addPatches appends patches to the "patches" list of the kustomization file in
// appPath (SRC-061). The file is re-marshalled (comments and ordering are lost) with
// its mode preserved.
func addPatches(appPath string, patches v1alpha1.KustomizePatches) error {
	var name string
	for _, n := range KustomizationNames {
		if _, err := os.Lstat(filepath.Join(appPath, n)); err == nil {
			name = n
			break
		}
	}
	if name == "" {
		return errors.New("kustomization file not found in the path")
	}
	p := filepath.Join(appPath, name)
	info, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("failed to stat kustomization.yaml: %w", err)
	}
	if !info.Mode().IsRegular() {
		// The copy's symlinks stay inside it, but rewriting through a link would
		// change another file; refuse instead.
		return fmt.Errorf("failed to load kustomization.yaml: %s is not a regular file", name)
	}
	b, err := readFileBounded(p, 16<<20)
	if err != nil {
		return fmt.Errorf("failed to load kustomization.yaml: %w", err)
	}
	var kustomization any
	if err := yaml.Unmarshal(b, &kustomization); err != nil {
		return fmt.Errorf("failed to unmarshal kustomization.yaml: %w", err)
	}
	kMap, ok := kustomization.(map[string]any)
	if !ok {
		return fmt.Errorf("expected kustomization.yaml to be type map[string]any, but got %T", kustomization)
	}
	if existing, ok := kMap["patches"]; ok {
		list, ok := existing.([]any)
		if !ok {
			return fmt.Errorf("expected 'patches' field in kustomization.yaml to be []any, but got %T", existing)
		}
		for _, patch := range patches {
			list = append(list, patch)
		}
		kMap["patches"] = list
	} else {
		kMap["patches"] = patches
	}
	updated, err := yaml.Marshal(kMap)
	if err != nil {
		return fmt.Errorf("failed to marshal kustomization.yaml after adding patches: %w", err)
	}
	if err := os.WriteFile(p, updated, info.Mode().Perm()); err != nil {
		return fmt.Errorf("failed to write kustomization.yaml with updated 'patches' field: %w", err)
	}
	return nil
}

// gitCAEnv returns GIT_SSL_CAINFO for an HTTPS application repository whose host has
// a configured CA bundle (written into the call's scratch directory), so remote bases
// on the same host verify like the checkout did (upstream behavior); "" otherwise.
func (r *Renderer) gitCAEnv(c *call, scratch string) (string, error) {
	if r.cfg.CAData == nil || !strings.HasPrefix(strings.ToLower(c.in.Repo.URL), "https://") {
		return "", nil
	}
	host := repoHost(c.in.Repo.URL)
	if host == "" {
		return "", nil
	}
	ca := r.cfg.CAData(host)
	if len(ca) == 0 {
		return "", nil
	}
	p := filepath.Join(scratch, "git-ca.pem")
	if err := os.WriteFile(p, ca, 0o600); err != nil {
		return "", fmt.Errorf("failed to write CA bundle for kustomize build: %w", err)
	}
	return "GIT_SSL_CAINFO=" + p, nil
}

// proxyEnvKeys are the proxy variables Go tools (kustomize, git through curl, helm)
// read, in both spellings.
var proxyEnvKeys = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"}

// proxyEnv returns the proxy variables of a kustomize build (upstream starts from the
// repo-server environment and applies proxy.UpsertEnv). With a repository proxy, the
// proxy and no-proxy variables are set in both spellings; no_proxy is always set,
// possibly empty, so a pod-level NO_PROXY never applies to a repository proxy.
// Without one, the repo-server's own proxy variables are passed through (the rest of
// the build environment is minimal, so they would otherwise be lost).
func proxyEnv(proxy, noProxy string) []string {
	if proxy == "" {
		var env []string
		for _, k := range proxyEnvKeys {
			if v, ok := os.LookupEnv(k); ok {
				env = append(env, k+"="+v)
			}
		}
		return env
	}
	return []string{
		"HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "http_proxy=" + proxy, "https_proxy=" + proxy,
		"NO_PROXY=" + noProxy, "no_proxy=" + noProxy,
	}
}

// Wrapper scripts installed ahead of the real binaries on the kustomize build PATH.
//
// The git wrapper inserts --end-of-options into the only git fetch shape kustomize
// uses for remote bases (`git fetch --depth=1 <repo> <ref>`) and refuses any other
// fetch, so a crafted ?ref= cannot become a git option (SRC-067, GHSA-9v9p-x54c-58gc).
// The helm wrapper pins HELM_DATA_HOME, HELM_CONFIG_HOME and HELM_PLUGINS to an empty
// per-call directory, so a kustomization (helmGlobals.configHome) cannot make Helm
// load plugins (SRC-068, GHSA-fw5c-w8rc-j7fx).
const (
	gitWrapperScript = `#!/bin/sh
REAL_GIT='%s'
if [ "$1" != "fetch" ]; then
	exec "$REAL_GIT" "$@"
fi
if [ "$#" -eq 4 ] && [ "$2" = "--depth=1" ]; then
	exec "$REAL_GIT" "$1" "$2" --end-of-options "$3" "$4"
fi
echo "git wrapper: unrecognized 'git fetch' invocation, refusing (see GHSA-9v9p-x54c-58gc)" >&2
exit 1
`
	helmWrapperScript = `#!/bin/sh
REAL_HELM='%[1]s'
export HELM_DATA_HOME='%[2]s'
export HELM_CONFIG_HOME='%[2]s'
export HELM_PLUGINS='%[2]s/plugins'
exec "$REAL_HELM" "$@"
`
)

// installWrappers writes the git and helm wrappers into scratch/bin and returns that
// directory ("" when neither real binary is available).
func (r *Renderer) installWrappers(c *call, scratch, toolPATH string) (string, error) {
	bin := filepath.Join(scratch, "bin")
	installed := false
	write := func(name, script string) error {
		if !installed {
			if err := os.Mkdir(bin, 0o700); err != nil {
				return err
			}
			installed = true
		}
		return os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700)
	}
	if realGit, err := lookPath(r.cfg.GitPath, toolPATH); err != nil {
		c.log.Warn("git not found; kustomize remote-base argument-injection guard not installed", "error", err)
	} else {
		if strings.ContainsAny(realGit, "'\n") {
			return "", fmt.Errorf("failed to install git wrapper for kustomize build: git path %q contains an unexpected character", realGit)
		}
		if err := write("git", fmt.Sprintf(gitWrapperScript, realGit)); err != nil {
			return "", fmt.Errorf("failed to install git wrapper for kustomize build: %w", err)
		}
	}
	if realHelm, err := lookPath(r.cfg.HelmPath, toolPATH); err != nil {
		c.log.Debug("helm not found; kustomize helm-plugin guard not installed", "error", err)
	} else {
		safe := filepath.Join(scratch, "helm-safe")
		if strings.ContainsAny(realHelm, "'\n") || strings.ContainsAny(safe, "'\n") {
			return "", fmt.Errorf("failed to install helm wrapper for kustomize build: helm path %q contains an unexpected character", realHelm)
		}
		if err := os.MkdirAll(filepath.Join(safe, "plugins"), 0o700); err != nil {
			return "", fmt.Errorf("failed to install helm wrapper for kustomize build: %w", err)
		}
		if err := write("helm", fmt.Sprintf(helmWrapperScript, realHelm, safe)); err != nil {
			return "", fmt.Errorf("failed to install helm wrapper for kustomize build: %w", err)
		}
	}
	if !installed {
		return "", nil
	}
	return bin, nil
}

// lookPath resolves name to an absolute executable: absolute names are checked, bare
// names are looked up in the absolute entries of pathList.
func lookPath(name, pathList string) (string, error) {
	if filepath.IsAbs(name) {
		if err := isExecutable(name); err != nil {
			return "", err
		}
		return name, nil
	}
	if strings.ContainsRune(name, filepath.Separator) {
		return "", fmt.Errorf("relative program path %q", name)
	}
	for _, dir := range filepath.SplitList(pathList) {
		if !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, name)
		if isExecutable(p) == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: %w", name, fs.ErrNotExist)
}

func isExecutable(p string) error {
	info, err := os.Stat(p)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s: %w", p, fs.ErrPermission)
	}
	return nil
}
