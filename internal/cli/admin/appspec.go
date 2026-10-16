package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Defaults of the sync retry flags (upstream DefaultSyncRetry*).
const (
	defaultRevisionHistoryLimit = 10
	defaultSyncRetryDuration    = 5 * time.Second
	defaultSyncRetryMaxDuration = 3 * time.Minute
	defaultSyncRetryFactor      = 2
)

// appOptions are the Application flags shared with "patrol app create" (upstream
// cmdutil.AppOptions). generate-spec applies only the flags that were set.
type appOptions struct {
	repoURL, appPath, chart, env, revision, tagPrefix                                 string
	drySourceRepo, drySourceRevision, drySourcePath, syncSourceBranch, syncSourcePath string
	hydrateToBranch                                                                   string
	revisionHistoryLimit                                                              int
	destServer, destName, destNamespace                                               string
	parameters, valuesFiles                                                           []string
	ignoreMissingValueFiles                                                           bool
	valuesLiteralFile, releaseName, helmVersion                                       string
	helmPassCredentials                                                               bool
	helmSets, helmSetStrings, helmSetFiles                                            []string
	helmSkipCrds, helmSkipSchemaValidation, helmSkipTests                             bool
	helmNamespace, helmKubeVersion                                                    string
	helmAPIVersions                                                                   []string
	project, syncPolicy                                                               string
	syncOptions                                                                       []string
	autoPrune, selfHeal, allowEmpty                                                   bool
	namePrefix, nameSuffix, kustomizeVersion                                          string
	directoryRecurse                                                                  bool
	configManagementPlugin                                                            string
	jsonnetTLAStr, jsonnetTLACode, jsonnetExtVarStr, jsonnetExtVarCode, jsonnetLibs   []string
	kustomizeImages, kustomizeReplicas                                                []string
	ignoreMissingComponents                                                           bool
	pluginEnvs                                                                        []string
	validate                                                                          bool
	kustomizeCommonLabels, kustomizeCommonAnnotations                                 []string
	kustomizeLabelWithoutSelector, kustomizeLabelIncludeTemplates                     bool
	kustomizeForceCommonLabels, kustomizeForceCommonAnnotations                       bool
	kustomizeNamespace, kustomizeKubeVersion                                          string
	kustomizeAPIVersions                                                              []string
	directoryExclude, directoryInclude                                                string
	retryLimit                                                                        int64
	retryBackoffDuration, retryBackoffMaxDuration                                     time.Duration
	retryBackoffFactor                                                                int64
	retryRefresh                                                                      bool
	ref, sourceName                                                                   string
}

// addAppFlags registers the Application flags (upstream AddAppFlags).
func addAppFlags(cmd *cobra.Command, o *appOptions) {
	fl := cmd.Flags()
	fl.StringVar(&o.repoURL, "repo", "", "Repository URL, ignored if a file is set")
	fl.StringVar(&o.appPath, "path", "", "Path in repository to the app directory, ignored if a file is set")
	fl.StringVar(&o.chart, "helm-chart", "", "Helm Chart name")
	fl.StringVar(&o.env, "env", "", "Application environment to monitor")
	fl.StringVar(&o.revision, "revision", "", "The tracking source branch, tag, commit or Helm chart version the application will sync to")
	fl.StringVar(&o.tagPrefix, "tag-prefix", "", "Filter git tags by this prefix before evaluating targetRevision as a semver constraint")
	fl.StringVar(&o.drySourceRepo, "dry-source-repo", "", "Repository URL of the app dry source")
	fl.StringVar(&o.drySourceRevision, "dry-source-revision", "", "Revision of the app dry source")
	fl.StringVar(&o.drySourcePath, "dry-source-path", "", "Path in repository to the app directory for the dry source")
	fl.StringVar(&o.syncSourceBranch, "sync-source-branch", "", "The branch from which the app will sync")
	fl.StringVar(&o.syncSourcePath, "sync-source-path", "", "The path in the repository from which the app will sync")
	fl.StringVar(&o.hydrateToBranch, "hydrate-to-branch", "", "The branch to hydrate the app to")
	fl.IntVar(&o.revisionHistoryLimit, "revision-history-limit", defaultRevisionHistoryLimit, "How many items to keep in revision history")
	fl.StringVar(&o.destServer, "dest-server", "", "K8s cluster URL (e.g. https://kubernetes.default.svc)")
	fl.StringVar(&o.destName, "dest-name", "", "K8s cluster Name (e.g. minikube)")
	fl.StringVar(&o.destNamespace, "dest-namespace", "", "K8s target namespace")
	fl.StringArrayVarP(&o.parameters, "parameter", "p", []string{}, "set a parameter override (e.g. -p guestbook=image=example/guestbook:latest)")
	fl.StringArrayVar(&o.valuesFiles, "values", []string{}, "Helm values file(s) to use")
	fl.BoolVar(&o.ignoreMissingValueFiles, "ignore-missing-value-files", false, "Ignore locally missing valueFiles when setting helm template --values")
	fl.StringVar(&o.valuesLiteralFile, "values-literal-file", "", "Filename or URL to import as a literal Helm values block")
	fl.StringVar(&o.releaseName, "release-name", "", "Helm release-name")
	fl.StringVar(&o.helmVersion, "helm-version", "", "Helm version")
	fl.BoolVar(&o.helmPassCredentials, "helm-pass-credentials", false, "Pass credentials to all domain")
	fl.StringArrayVar(&o.helmSets, "helm-set", []string{}, "Helm set values on the command line (can be repeated to set several values: --helm-set key1=val1 --helm-set key2=val2)")
	fl.StringArrayVar(&o.helmSetStrings, "helm-set-string", []string{}, "Helm set STRING values on the command line (can be repeated to set several values: --helm-set-string key1=val1 --helm-set-string key2=val2)")
	fl.StringArrayVar(&o.helmSetFiles, "helm-set-file", []string{}, "Helm set values from respective files specified via the command line (can be repeated to set several values: --helm-set-file key1=path1 --helm-set-file key2=path2)")
	fl.BoolVar(&o.helmSkipCrds, "helm-skip-crds", false, "Skip helm crd installation step")
	fl.BoolVar(&o.helmSkipSchemaValidation, "helm-skip-schema-validation", false, "Skip helm schema validation step")
	fl.BoolVar(&o.helmSkipTests, "helm-skip-tests", false, "Skip helm test manifests installation step")
	fl.StringVar(&o.helmNamespace, "helm-namespace", "", "Helm namespace to use when running helm template. If not set, use app.spec.destination.namespace")
	fl.StringVar(&o.helmKubeVersion, "helm-kube-version", "", "Helm kube-version to use when running helm template. If not set, use the kube version from the destination cluster")
	fl.StringArrayVar(&o.helmAPIVersions, "helm-api-versions", []string{}, "Helm api-versions (in format [group/]version/kind) to use when running helm template (Can be repeated to set several values: --helm-api-versions traefik.io/v1alpha1/TLSOption --helm-api-versions v1/Service). If not set, use the api-versions from the destination cluster")
	fl.StringVar(&o.project, "project", "", "Application project name")
	fl.StringVar(&o.syncPolicy, "sync-policy", "", "Set the sync policy (one of: manual (aliases of manual: none), automated (aliases of automated: auto, automatic))")
	fl.StringArrayVar(&o.syncOptions, "sync-option", []string{}, "Add or remove a sync option, e.g add `Prune=false`. Remove using `!` prefix, e.g. `!Prune=false`")
	fl.BoolVar(&o.autoPrune, "auto-prune", false, "Set automatic pruning for automated sync policy")
	fl.BoolVar(&o.selfHeal, "self-heal", false, "Set self healing for automated sync policy")
	fl.BoolVar(&o.allowEmpty, "allow-empty", false, "Set allow zero live resources for automated sync policy")
	fl.StringVar(&o.namePrefix, "nameprefix", "", "Kustomize nameprefix")
	fl.StringVar(&o.nameSuffix, "namesuffix", "", "Kustomize namesuffix")
	fl.StringVar(&o.kustomizeVersion, "kustomize-version", "", "Kustomize version")
	fl.BoolVar(&o.directoryRecurse, "directory-recurse", false, "Recurse directory")
	fl.StringVar(&o.configManagementPlugin, "config-management-plugin", "", "Config management plugin name")
	fl.StringArrayVar(&o.jsonnetTLAStr, "jsonnet-tla-str", []string{}, "Jsonnet top level string arguments")
	fl.StringArrayVar(&o.jsonnetTLACode, "jsonnet-tla-code", []string{}, "Jsonnet top level code arguments")
	fl.StringArrayVar(&o.jsonnetExtVarStr, "jsonnet-ext-var-str", []string{}, "Jsonnet string ext var")
	fl.StringArrayVar(&o.jsonnetExtVarCode, "jsonnet-ext-var-code", []string{}, "Jsonnet ext var")
	fl.StringArrayVar(&o.jsonnetLibs, "jsonnet-libs", []string{}, "Additional jsonnet libs (prefixed by repoRoot)")
	fl.StringArrayVar(&o.kustomizeImages, "kustomize-image", []string{}, "Kustomize images (e.g. --kustomize-image node:8.15.0 --kustomize-image mysql=mariadb,alpine@sha256:24a0c4b4a4c0eb97a1aabb8e29f18e917d05abfe1b7a7c07857230879ce7d3d)")
	fl.StringArrayVar(&o.kustomizeReplicas, "kustomize-replica", []string{}, "Kustomize replicas (e.g. --kustomize-replica my-development=2 --kustomize-replica my-statefulset=4)")
	fl.BoolVar(&o.ignoreMissingComponents, "ignore-missing-components", false, "Ignore locally missing component directories when setting Kustomize components")
	fl.StringArrayVar(&o.pluginEnvs, "plugin-env", []string{}, "Additional plugin envs")
	fl.BoolVar(&o.validate, "validate", true, "Validation of repo and cluster")
	fl.StringArrayVar(&o.kustomizeCommonLabels, "kustomize-common-label", []string{}, "Set common labels in Kustomize")
	fl.StringArrayVar(&o.kustomizeCommonAnnotations, "kustomize-common-annotation", []string{}, "Set common labels in Kustomize")
	fl.BoolVar(&o.kustomizeLabelWithoutSelector, "kustomize-label-without-selector", false, "Do not apply common label to selectors. Also do not apply label to templates unless --kustomize-label-include-templates is set")
	fl.BoolVar(&o.kustomizeLabelIncludeTemplates, "kustomize-label-include-templates", false, "Apply common label to resource templates")
	fl.BoolVar(&o.kustomizeForceCommonLabels, "kustomize-force-common-label", false, "Force common labels in Kustomize")
	fl.BoolVar(&o.kustomizeForceCommonAnnotations, "kustomize-force-common-annotation", false, "Force common annotations in Kustomize")
	fl.StringVar(&o.kustomizeNamespace, "kustomize-namespace", "", "Kustomize namespace")
	fl.StringVar(&o.kustomizeKubeVersion, "kustomize-kube-version", "", "kube-version to use when running helm template. If not set, use the kube version from the destination cluster. Only applicable when Helm is enabled for Kustomize builds")
	fl.StringArrayVar(&o.kustomizeAPIVersions, "kustomize-api-versions", nil, "api-versions (in format [group/]version/kind) to use when running helm template (Can be repeated to set several values: --helm-api-versions traefik.io/v1alpha1/TLSOption --helm-api-versions v1/Service). If not set, use the api-versions from the destination cluster. Only applicable when Helm is enabled for Kustomize builds")
	fl.StringVar(&o.directoryExclude, "directory-exclude", "", "Set glob expression used to exclude files from application source path")
	fl.StringVar(&o.directoryInclude, "directory-include", "", "Set glob expression used to include files from application source path")
	fl.Int64Var(&o.retryLimit, "sync-retry-limit", 0, "Max number of allowed sync retries")
	fl.DurationVar(&o.retryBackoffDuration, "sync-retry-backoff-duration", defaultSyncRetryDuration, "Sync retry backoff base duration. Input needs to be a duration (e.g. 2m, 1h)")
	fl.DurationVar(&o.retryBackoffMaxDuration, "sync-retry-backoff-max-duration", defaultSyncRetryMaxDuration, "Max sync retry backoff duration. Input needs to be a duration (e.g. 2m, 1h)")
	fl.Int64Var(&o.retryBackoffFactor, "sync-retry-backoff-factor", defaultSyncRetryFactor, "Factor multiplies the base duration after each failed sync retry")
	fl.BoolVar(&o.retryRefresh, "sync-retry-refresh", false, "Indicates if the latest revision should be used on retry instead of the initial one")
	fl.StringVar(&o.ref, "ref", "", "Ref is reference to another source within sources field")
	fl.StringVar(&o.sourceName, "source-name", "", "Name of the source from the list of sources of the app.")
}

// readFileOrURL reads a local file, or an http(s) URL (bounded, verified TLS).
func readFileOrURL(ctx context.Context, location string) ([]byte, error) {
	u, err := url.ParseRequestURI(location)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return readFileLimited(location)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("error reading %s: %s", location, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxInputFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxInputFileBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", location, maxInputFileBytes)
	}
	return data, nil
}

// kustomizeImageKey is the identity of an image override: the name before "=", else the
// image name without tag or digest.
func kustomizeImageKey(img string) string {
	if k, _, ok := strings.Cut(img, "="); ok {
		return k
	}
	if i := strings.Index(img, "@"); i >= 0 {
		img = img[:i]
	}
	if i := strings.LastIndex(img, ":"); i > strings.LastIndex(img, "/") {
		img = img[:i]
	}
	return img
}

func mergeKustomizeImage(k *v1alpha1.ApplicationSourceKustomize, img string) {
	key := kustomizeImageKey(img)
	for i, existing := range k.Images {
		if kustomizeImageKey(string(existing)) == key {
			k.Images[i] = v1alpha1.KustomizeImage(img)
			return
		}
	}
	k.Images = append(k.Images, v1alpha1.KustomizeImage(img))
}

func parseKustomizeReplica(s string) (v1alpha1.KustomizeReplica, error) {
	name, count, ok := strings.Cut(s, "=")
	if !ok || name == "" {
		return v1alpha1.KustomizeReplica{}, fmt.Errorf("expected parameter of the form: name=count. Received: %s", s)
	}
	n, err := strconv.Atoi(count)
	if err != nil {
		return v1alpha1.KustomizeReplica{}, fmt.Errorf("expected integer value for count. Received: %s", count)
	}
	return v1alpha1.KustomizeReplica{Name: name, Count: intstr.FromInt32(int32(n))}, nil //nolint:gosec // replica counts are small
}

func splitPair(s, what string) (string, string, error) {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return "", "", fmt.Errorf("expected %s of the form: param=value. Received: %s", what, s)
	}
	return k, v, nil
}

func addHelmParameter(h *v1alpha1.ApplicationSourceHelm, p v1alpha1.HelmParameter) {
	for i := range h.Parameters {
		if h.Parameters[i].Name == p.Name {
			h.Parameters[i] = p
			return
		}
	}
	h.Parameters = append(h.Parameters, p)
}

func isZeroHelm(h *v1alpha1.ApplicationSourceHelm) bool {
	return h == nil || (len(h.ValueFiles) == 0 && len(h.Parameters) == 0 && h.ReleaseName == "" && h.Values == "" && len(h.FileParameters) == 0 &&
		h.Version == "" && !h.PassCredentials && !h.IgnoreMissingValueFiles && !h.SkipCrds && h.ValuesObject == nil && h.Namespace == "" &&
		h.KubeVersion == "" && len(h.APIVersions) == 0 && !h.SkipTests && !h.SkipSchemaValidation)
}

// constructSource applies the source flags that were set (upstream ConstructSource).
func (o *appOptions) constructSource(ctx context.Context, src *v1alpha1.ApplicationSource, flags *pflag.FlagSet) error {
	helm := func() *v1alpha1.ApplicationSourceHelm {
		if src.Helm == nil {
			src.Helm = &v1alpha1.ApplicationSourceHelm{}
		}
		return src.Helm
	}
	kust := func() *v1alpha1.ApplicationSourceKustomize {
		if src.Kustomize == nil {
			src.Kustomize = &v1alpha1.ApplicationSourceKustomize{}
		}
		return src.Kustomize
	}
	dir := func() *v1alpha1.ApplicationSourceDirectory {
		if src.Directory == nil {
			src.Directory = &v1alpha1.ApplicationSourceDirectory{}
		}
		return src.Directory
	}
	jsonnetVars := func(entries []string, code bool) ([]v1alpha1.JsonnetVar, error) {
		var out []v1alpha1.JsonnetVar
		for _, e := range entries {
			k, v, err := splitPair(e, "jsonnet variable")
			if err != nil {
				return nil, err
			}
			out = append(out, v1alpha1.JsonnetVar{Name: k, Value: v, Code: code})
		}
		return out, nil
	}
	var err error
	flags.Visit(func(f *pflag.Flag) {
		if err != nil {
			return
		}
		switch f.Name {
		case "repo":
			src.RepoURL = o.repoURL
		case "path":
			src.Path = o.appPath
		case "helm-chart":
			src.Chart = o.chart
		case "revision":
			src.TargetRevision = o.revision
		case "tag-prefix":
			src.TagPrefix = o.tagPrefix
		case "values":
			helm().ValueFiles = o.valuesFiles
		case "ignore-missing-value-files":
			helm().IgnoreMissingValueFiles = o.ignoreMissingValueFiles
		case "values-literal-file":
			var data []byte
			if data, err = readFileOrURL(ctx, o.valuesLiteralFile); err != nil {
				return
			}
			var probe map[string]any
			if yerr := yaml.Unmarshal(data, &probe); yerr != nil {
				err = fmt.Errorf("invalid helm values in %s: %w", o.valuesLiteralFile, yerr)
				return
			}
			helm().Values = string(data)
		case "release-name":
			helm().ReleaseName = o.releaseName
		case "helm-version":
			helm().Version = o.helmVersion
		case "helm-pass-credentials":
			helm().PassCredentials = o.helmPassCredentials
		case "helm-set", "helm-set-string":
			entries, force := o.helmSets, false
			if f.Name == "helm-set-string" {
				entries, force = o.helmSetStrings, true
			}
			for _, e := range entries {
				k, v, perr := splitPair(e, "parameter")
				if perr != nil {
					err = perr
					return
				}
				addHelmParameter(helm(), v1alpha1.HelmParameter{Name: k, Value: v, ForceString: force})
			}
		case "helm-set-file":
			for _, e := range o.helmSetFiles {
				k, v, perr := splitPair(e, "file parameter")
				if perr != nil {
					err = perr
					return
				}
				h := helm()
				replaced := false
				for i := range h.FileParameters {
					if h.FileParameters[i].Name == k {
						h.FileParameters[i].Path, replaced = v, true
					}
				}
				if !replaced {
					h.FileParameters = append(h.FileParameters, v1alpha1.HelmFileParameter{Name: k, Path: v})
				}
			}
		case "helm-skip-crds":
			helm().SkipCrds = o.helmSkipCrds
		case "helm-skip-schema-validation":
			helm().SkipSchemaValidation = o.helmSkipSchemaValidation
		case "helm-skip-tests":
			helm().SkipTests = o.helmSkipTests
		case "helm-namespace":
			helm().Namespace = o.helmNamespace
		case "helm-kube-version":
			helm().KubeVersion = o.helmKubeVersion
		case "helm-api-versions":
			helm().APIVersions = o.helmAPIVersions
		case "directory-recurse":
			dir().Recurse = o.directoryRecurse
		case "directory-exclude":
			dir().Exclude = o.directoryExclude
		case "directory-include":
			dir().Include = o.directoryInclude
		case "config-management-plugin":
			src.Plugin = &v1alpha1.ApplicationSourcePlugin{Name: o.configManagementPlugin}
		case "nameprefix":
			kust().NamePrefix = o.namePrefix
		case "namesuffix":
			kust().NameSuffix = o.nameSuffix
		case "kustomize-image":
			for _, img := range o.kustomizeImages {
				mergeKustomizeImage(kust(), img)
			}
		case "kustomize-replica":
			for _, r := range o.kustomizeReplicas {
				rep, perr := parseKustomizeReplica(r)
				if perr != nil {
					err = perr
					return
				}
				k := kust()
				replaced := false
				for i := range k.Replicas {
					if k.Replicas[i].Name == rep.Name {
						k.Replicas[i], replaced = rep, true
					}
				}
				if !replaced {
					k.Replicas = append(k.Replicas, rep)
				}
			}
		case "kustomize-version":
			kust().Version = o.kustomizeVersion
		case "kustomize-namespace":
			kust().Namespace = o.kustomizeNamespace
		case "kustomize-kube-version":
			kust().KubeVersion = o.kustomizeKubeVersion
		case "kustomize-api-versions":
			kust().APIVersions = o.kustomizeAPIVersions
		case "kustomize-common-label":
			var m map[string]string
			if m, err = parseKeyValues(o.kustomizeCommonLabels, "label"); err == nil {
				kust().CommonLabels = m
			}
		case "kustomize-common-annotation":
			var m map[string]string
			if m, err = parseKeyValues(o.kustomizeCommonAnnotations, "annotation"); err == nil {
				kust().CommonAnnotations = m
			}
		case "kustomize-label-without-selector":
			kust().LabelWithoutSelector = o.kustomizeLabelWithoutSelector
		case "kustomize-label-include-templates":
			kust().LabelIncludeTemplates = o.kustomizeLabelIncludeTemplates
		case "kustomize-force-common-label":
			kust().ForceCommonLabels = o.kustomizeForceCommonLabels
		case "kustomize-force-common-annotation":
			kust().ForceCommonAnnotations = o.kustomizeForceCommonAnnotations
		case "ignore-missing-components":
			kust().IgnoreMissingComponents = o.ignoreMissingComponents
		case "jsonnet-tla-str", "jsonnet-tla-code":
			entries, code := o.jsonnetTLAStr, false
			if f.Name == "jsonnet-tla-code" {
				entries, code = o.jsonnetTLACode, true
			}
			var vars []v1alpha1.JsonnetVar
			if vars, err = jsonnetVars(entries, code); err == nil {
				dir().Jsonnet.TLAs = append(dir().Jsonnet.TLAs, vars...)
			}
		case "jsonnet-ext-var-str", "jsonnet-ext-var-code":
			entries, code := o.jsonnetExtVarStr, false
			if f.Name == "jsonnet-ext-var-code" {
				entries, code = o.jsonnetExtVarCode, true
			}
			var vars []v1alpha1.JsonnetVar
			if vars, err = jsonnetVars(entries, code); err == nil {
				dir().Jsonnet.ExtVars = append(dir().Jsonnet.ExtVars, vars...)
			}
		case "jsonnet-libs":
			dir().Jsonnet.Libs = append(dir().Jsonnet.Libs, o.jsonnetLibs...)
		case "plugin-env":
			if src.Plugin == nil {
				src.Plugin = &v1alpha1.ApplicationSourcePlugin{}
			}
			for _, e := range o.pluginEnvs {
				k, v, perr := splitPair(e, "environment variable")
				if perr != nil {
					err = perr
					return
				}
				replaced := false
				for i := range src.Plugin.Env {
					if src.Plugin.Env[i].Name == k {
						src.Plugin.Env[i].Value, replaced = v, true
					}
				}
				if !replaced {
					src.Plugin.Env = append(src.Plugin.Env, v1alpha1.EnvEntry{Name: k, Value: v})
				}
			}
		case "ref":
			src.Ref = o.ref
		case "source-name":
			src.Name = o.sourceName
		}
	})
	if err != nil {
		return err
	}
	if isZeroHelm(src.Helm) {
		src.Helm = nil
	}
	return nil
}

// constructSourceHydrator applies the hydrator flags (upstream constructSourceHydrator). It
// reports whether any hydrator flag was set.
func (o *appOptions) constructSourceHydrator(h *v1alpha1.SourceHydrator, flags *pflag.FlagSet) (*v1alpha1.SourceHydrator, bool) {
	set := false
	ensure := func(notEmpty bool) {
		set = true
		if notEmpty && h == nil {
			h = &v1alpha1.SourceHydrator{}
		}
	}
	flags.Visit(func(f *pflag.Flag) {
		switch f.Name {
		case "dry-source-repo":
			ensure(o.drySourceRepo != "")
			if h != nil {
				h.DrySource.RepoURL = o.drySourceRepo
			}
		case "dry-source-path":
			ensure(o.drySourcePath != "")
			if h != nil {
				h.DrySource.Path = o.drySourcePath
			}
		case "dry-source-revision":
			ensure(o.drySourceRevision != "")
			if h != nil {
				h.DrySource.TargetRevision = o.drySourceRevision
			}
		case "sync-source-branch":
			ensure(o.syncSourceBranch != "")
			if h != nil {
				h.SyncSource.TargetBranch = o.syncSourceBranch
			}
		case "sync-source-path":
			ensure(o.syncSourcePath != "")
			if h != nil {
				h.SyncSource.Path = o.syncSourcePath
			}
		case "hydrate-to-branch":
			ensure(o.hydrateToBranch != "")
			if h != nil {
				if o.hydrateToBranch == "" {
					h.HydrateTo = nil
				} else {
					h.HydrateTo = &v1alpha1.HydrateTo{TargetBranch: o.hydrateToBranch}
				}
			}
		}
	})
	return h, set
}

func isZeroSyncPolicy(p *v1alpha1.SyncPolicy) bool {
	return p == nil || (p.Automated == nil && len(p.SyncOptions) == 0 && p.Retry == nil && p.ManagedNamespaceMetadata == nil)
}

// setAppSpecOptions applies the flags that were set to spec (upstream SetAppSpecOptions).
func (o *appOptions) setAppSpecOptions(ctx context.Context, flags *pflag.FlagSet, spec *v1alpha1.ApplicationSpec) error {
	if h, set := o.constructSourceHydrator(spec.SourceHydrator, flags); set {
		spec.SourceHydrator = h
	} else {
		src := spec.Source
		if spec.HasMultipleSources() {
			src = &spec.Sources[0]
		}
		if src == nil {
			src = &v1alpha1.ApplicationSource{}
		}
		if err := o.constructSource(ctx, src, flags); err != nil {
			return err
		}
		if !spec.HasMultipleSources() {
			spec.Source = src
		}
	}
	var err error
	flags.Visit(func(f *pflag.Flag) {
		if err != nil {
			return
		}
		switch f.Name {
		case "revision-history-limit":
			n := int64(o.revisionHistoryLimit)
			spec.RevisionHistoryLimit = &n
		case "dest-name":
			spec.Destination.Name = o.destName
		case "dest-server":
			spec.Destination.Server = o.destServer
		case "dest-namespace":
			spec.Destination.Namespace = o.destNamespace
		case "project":
			spec.Project = o.project
		case "sync-policy":
			switch o.syncPolicy {
			case "none", "manual":
				if spec.SyncPolicy != nil {
					spec.SyncPolicy.Automated = nil
				}
				if isZeroSyncPolicy(spec.SyncPolicy) {
					spec.SyncPolicy = nil
				}
			case "automated", "automatic", "auto":
				if spec.SyncPolicy == nil {
					spec.SyncPolicy = &v1alpha1.SyncPolicy{}
				}
				spec.SyncPolicy.Automated = &v1alpha1.SyncPolicyAutomated{}
			default:
				err = fmt.Errorf("Invalid sync-policy: %s", o.syncPolicy) //nolint:staticcheck // upstream wording
			}
		case "sync-option":
			if spec.SyncPolicy == nil {
				spec.SyncPolicy = &v1alpha1.SyncPolicy{}
			}
			for _, option := range o.syncOptions {
				if after, ok := strings.CutPrefix(option, "!"); ok {
					spec.SyncPolicy.SyncOptions = spec.SyncPolicy.SyncOptions.RemoveOption(after)
				} else {
					spec.SyncPolicy.SyncOptions = spec.SyncPolicy.SyncOptions.AddOption(option)
				}
			}
			if isZeroSyncPolicy(spec.SyncPolicy) {
				spec.SyncPolicy = nil
			}
		case "sync-retry-limit":
			switch {
			case o.retryLimit > 0:
				if spec.SyncPolicy == nil {
					spec.SyncPolicy = &v1alpha1.SyncPolicy{}
				}
				factor := o.retryBackoffFactor
				spec.SyncPolicy.Retry = &v1alpha1.RetryStrategy{
					Limit:   o.retryLimit,
					Backoff: &v1alpha1.Backoff{Duration: o.retryBackoffDuration.String(), MaxDuration: o.retryBackoffMaxDuration.String(), Factor: &factor},
					Refresh: o.retryRefresh,
				}
			case o.retryLimit == 0:
				if spec.SyncPolicy != nil {
					spec.SyncPolicy.Retry = nil
				}
				if isZeroSyncPolicy(spec.SyncPolicy) {
					spec.SyncPolicy = nil
				}
			default:
				err = fmt.Errorf("Invalid sync-retry-limit [%d]", o.retryLimit) //nolint:staticcheck // upstream wording
			}
		case "sync-retry-refresh":
			if spec.SyncPolicy == nil {
				spec.SyncPolicy = &v1alpha1.SyncPolicy{}
			}
			if spec.SyncPolicy.Retry == nil {
				spec.SyncPolicy.Retry = &v1alpha1.RetryStrategy{}
			}
			spec.SyncPolicy.Retry.Refresh = o.retryRefresh
		}
	})
	if err != nil {
		return err
	}
	if flags.Changed("auto-prune") || flags.Changed("self-heal") || flags.Changed("allow-empty") {
		if spec.SyncPolicy == nil {
			spec.SyncPolicy = &v1alpha1.SyncPolicy{}
		}
		if spec.SyncPolicy.Automated == nil {
			disabled := false
			spec.SyncPolicy.Automated = &v1alpha1.SyncPolicyAutomated{Enabled: &disabled}
		}
		if flags.Changed("auto-prune") {
			v := o.autoPrune
			spec.SyncPolicy.Automated.Prune = &v
		}
		if flags.Changed("self-heal") {
			v := o.selfHeal
			spec.SyncPolicy.Automated.SelfHeal = &v
		}
		if flags.Changed("allow-empty") {
			v := o.allowEmpty
			spec.SyncPolicy.Automated.AllowEmpty = &v
		}
	}
	return nil
}

// setParameterOverrides applies -p name=value Helm parameter overrides (upstream
// SetParameterOverrides).
func setParameterOverrides(app *v1alpha1.Application, parameters []string) error {
	if len(parameters) == 0 {
		return nil
	}
	src := app.Spec.Source
	if app.Spec.HasMultipleSources() {
		src = &app.Spec.Sources[0]
	}
	if src == nil {
		return errors.New("Parameters can only be set against Helm applications") //nolint:staticcheck // upstream wording
	}
	isHelm := src.Helm != nil || src.Chart != "" || app.Status.SourceType == v1alpha1.ApplicationSourceTypeHelm
	if !isHelm && src.Kustomize == nil && src.Directory == nil && src.Plugin == nil {
		_, _, isHelm = strings.Cut(parameters[0], "=")
	}
	if !isHelm {
		return errors.New("Parameters can only be set against Helm applications") //nolint:staticcheck // upstream wording
	}
	if src.Helm == nil {
		src.Helm = &v1alpha1.ApplicationSourceHelm{}
	}
	for _, p := range parameters {
		k, v, err := splitPair(p, "parameter")
		if err != nil {
			return err
		}
		addHelmParameter(src.Helm, v1alpha1.HelmParameter{Name: k, Value: v})
	}
	return nil
}

// constructApps builds the Application(s) of generate-spec from a manifest file (or "-"
// for stdin) and/or flags (upstream ConstructApps).
func constructApps(ctx context.Context, stdin io.Reader, fileURL, appName string, labels, annotations, args []string, o *appOptions, flags *pflag.FlagSet) ([]*v1alpha1.Application, error) {
	var apps []*v1alpha1.Application
	if fileURL != "" {
		var data []byte
		var err error
		if fileURL == "-" {
			data, err = readSource("-", stdin)
		} else {
			data, err = readFileOrURL(ctx, fileURL)
		}
		if err != nil {
			return nil, err
		}
		objs, err := splitObjects(data)
		if err != nil {
			return nil, err
		}
		for _, obj := range objs {
			app := &v1alpha1.Application{}
			if err := runtimeConvert(obj.Object, app); err != nil {
				return nil, err
			}
			apps = append(apps, app)
		}
		if fileURL == "-" {
			return apps, nil
		}
		for _, app := range apps {
			if len(args) == 1 && args[0] != app.Name {
				return nil, fmt.Errorf("app name '%s' does not match app spec metadata.name '%s'", args[0], app.Name)
			}
			if appName != "" && appName != app.Name {
				app.Name = appName
			}
			if app.Name == "" {
				return nil, errors.New("app.Name is empty. --name argument can be used to provide app.Name")
			}
			if err := mergeAppMetadata(app, labels, annotations); err != nil {
				return nil, err
			}
			if !app.Spec.HasMultipleSources() {
				if err := o.setAppSpecOptions(ctx, flags, &app.Spec); err != nil {
					return nil, err
				}
				if err := setParameterOverrides(app, o.parameters); err != nil {
					return nil, err
				}
			}
		}
		return apps, nil
	}
	if len(args) == 1 {
		if appName != "" && appName != args[0] {
			return nil, fmt.Errorf("--name argument '%s' does not match app name %s", appName, args[0])
		}
		appName = args[0]
	}
	ns, name := "", appName
	if before, after, ok := strings.Cut(appName, "/"); ok {
		ns, name = before, after
	}
	app := &v1alpha1.Application{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Application"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
	if err := o.setAppSpecOptions(ctx, flags, &app.Spec); err != nil {
		return nil, err
	}
	if err := setParameterOverrides(app, o.parameters); err != nil {
		return nil, err
	}
	if err := mergeAppMetadata(app, labels, annotations); err != nil {
		return nil, err
	}
	return []*v1alpha1.Application{app}, nil
}

func mergeAppMetadata(app *v1alpha1.Application, labels, annotations []string) error {
	l, err := parseKeyValues(labels, "label")
	if err != nil {
		return err
	}
	if len(l) > 0 {
		merged := maps.Clone(app.Labels)
		if merged == nil {
			merged = map[string]string{}
		}
		maps.Copy(merged, l)
		app.Labels = merged
	}
	if len(annotations) > 0 && app.Annotations == nil {
		app.Annotations = map[string]string{}
	}
	for _, a := range annotations {
		k, v, _ := strings.Cut(a, "=")
		app.Annotations[k] = v
	}
	return nil
}
