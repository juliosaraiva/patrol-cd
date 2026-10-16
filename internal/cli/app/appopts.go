package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/spf13/pflag"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Defaults of the sync retry flags (upstream DefaultSyncRetryDuration, ...MaxDuration,
// ...Factor) and of revisionHistoryLimit.
const (
	defaultRetryDuration        = 5 * time.Second
	defaultRetryMaxDuration     = 3 * time.Minute
	defaultRetryFactor          = int64(2)
	defaultRevisionHistoryLimit = 10
)

// appOptions holds the shared Application flags of create, set and add-source (upstream
// cmd/util AppOptions).
type appOptions struct {
	repoURL                         string
	appPath                         string
	chart                           string
	env                             string
	revision                        string
	tagPrefix                       string
	revisionHistoryLimit            int
	destName                        string
	destServer                      string
	destNamespace                   string
	parameters                      []string
	valuesFiles                     []string
	ignoreMissingValueFiles         bool
	values                          string
	releaseName                     string
	helmSets                        []string
	helmSetStrings                  []string
	helmSetFiles                    []string
	helmVersion                     string
	helmPassCredentials             bool
	helmSkipCrds                    bool
	helmSkipSchemaValidation        bool
	helmSkipTests                   bool
	helmNamespace                   string
	helmKubeVersion                 string
	helmAPIVersions                 []string
	project                         string
	syncPolicy                      string
	syncOptions                     []string
	autoPrune                       bool
	selfHeal                        bool
	allowEmpty                      bool
	namePrefix                      string
	nameSuffix                      string
	directoryRecurse                bool
	configManagementPlugin          string
	jsonnetTLAStr                   []string
	jsonnetTLACode                  []string
	jsonnetExtVarStr                []string
	jsonnetExtVarCode               []string
	jsonnetLibs                     []string
	kustomizeImages                 []string
	kustomizeReplicas               []string
	kustomizeVersion                string
	kustomizeCommonLabels           []string
	kustomizeCommonAnnotations      []string
	kustomizeLabelWithoutSelector   bool
	kustomizeLabelIncludeTemplates  bool
	kustomizeForceCommonLabels      bool
	kustomizeForceCommonAnnotations bool
	kustomizeNamespace              string
	kustomizeKubeVersion            string
	kustomizeAPIVersions            []string
	ignoreMissingComponents         bool
	pluginEnvs                      []string
	validate                        bool
	directoryExclude                string
	directoryInclude                string
	retryLimit                      int64
	retryBackoffDuration            time.Duration
	retryBackoffMaxDuration         time.Duration
	retryBackoffFactor              int64
	retryRefresh                    bool
	ref                             string
	sourceName                      string
	drySourceRepo                   string
	drySourceRevision               string
	drySourcePath                   string
	syncSourceBranch                string
	syncSourcePath                  string
	hydrateToBranch                 string
}

// appFlagNames returns the names of the flags added by addAppFlags; only they count as "an
// option to update".
var appFlagNames = sync.OnceValue(func() map[string]bool {
	fs := pflag.NewFlagSet("app", pflag.ContinueOnError)
	addAppFlags(fs, &appOptions{})
	names := map[string]bool{}
	fs.VisitAll(func(f *pflag.Flag) { names[f.Name] = true })
	return names
})

// addAppFlags registers the shared Application flags (names, defaults and help as upstream).
func addAppFlags(fs *pflag.FlagSet, o *appOptions) {
	fs.StringVar(&o.repoURL, "repo", "", "Repository URL, ignored if a file is set")
	fs.StringVar(&o.appPath, "path", "", "Path in repository to the app directory, ignored if a file is set")
	fs.StringVar(&o.chart, "helm-chart", "", "Helm Chart name")
	fs.StringVar(&o.env, "env", "", "Application environment to monitor")
	fs.StringVar(&o.revision, "revision", "", "The tracking source branch, tag, commit or Helm chart version the application will sync to")
	fs.StringVar(&o.tagPrefix, "tag-prefix", "", "Filter git tags by this prefix before evaluating targetRevision as a semver constraint")
	fs.StringVar(&o.drySourceRepo, "dry-source-repo", "", "Repository URL of the app dry source")
	fs.StringVar(&o.drySourceRevision, "dry-source-revision", "", "Revision of the app dry source")
	fs.StringVar(&o.drySourcePath, "dry-source-path", "", "Path in repository to the app directory for the dry source")
	fs.StringVar(&o.syncSourceBranch, "sync-source-branch", "", "The branch from which the app will sync")
	fs.StringVar(&o.syncSourcePath, "sync-source-path", "", "The path in the repository from which the app will sync")
	fs.StringVar(&o.hydrateToBranch, "hydrate-to-branch", "", "The branch to hydrate the app to")
	fs.IntVar(&o.revisionHistoryLimit, "revision-history-limit", defaultRevisionHistoryLimit, "How many items to keep in revision history")
	fs.StringVar(&o.destServer, "dest-server", "", "K8s cluster URL (e.g. https://kubernetes.default.svc)")
	fs.StringVar(&o.destName, "dest-name", "", "K8s cluster Name (e.g. minikube)")
	fs.StringVar(&o.destNamespace, "dest-namespace", "", "K8s target namespace")
	fs.StringArrayVarP(&o.parameters, "parameter", "p", []string{}, "set a parameter override (e.g. -p guestbook=image=example/guestbook:latest)")
	fs.StringArrayVar(&o.valuesFiles, "values", []string{}, "Helm values file(s) to use")
	fs.BoolVar(&o.ignoreMissingValueFiles, "ignore-missing-value-files", false, "Ignore locally missing valueFiles when setting helm template --values")
	fs.StringVar(&o.values, "values-literal-file", "", "Filename or URL to import as a literal Helm values block")
	fs.StringVar(&o.releaseName, "release-name", "", "Helm release-name")
	fs.StringVar(&o.helmVersion, "helm-version", "", "Helm version")
	fs.BoolVar(&o.helmPassCredentials, "helm-pass-credentials", false, "Pass credentials to all domain")
	fs.StringArrayVar(&o.helmSets, "helm-set", []string{}, "Helm set values on the command line (can be repeated to set several values: --helm-set key1=val1 --helm-set key2=val2)")
	fs.StringArrayVar(&o.helmSetStrings, "helm-set-string", []string{}, "Helm set STRING values on the command line (can be repeated to set several values: --helm-set-string key1=val1 --helm-set-string key2=val2)")
	fs.StringArrayVar(&o.helmSetFiles, "helm-set-file", []string{}, "Helm set values from respective files specified via the command line (can be repeated to set several values: --helm-set-file key1=path1 --helm-set-file key2=path2)")
	fs.BoolVar(&o.helmSkipCrds, "helm-skip-crds", false, "Skip helm crd installation step")
	fs.BoolVar(&o.helmSkipSchemaValidation, "helm-skip-schema-validation", false, "Skip helm schema validation step")
	fs.BoolVar(&o.helmSkipTests, "helm-skip-tests", false, "Skip helm test manifests installation step")
	fs.StringVar(&o.helmNamespace, "helm-namespace", "", "Helm namespace to use when running helm template. If not set, use app.spec.destination.namespace")
	fs.StringVar(&o.helmKubeVersion, "helm-kube-version", "", "Helm kube-version to use when running helm template. If not set, use the kube version from the destination cluster")
	fs.StringArrayVar(&o.helmAPIVersions, "helm-api-versions", []string{}, "Helm api-versions (in format [group/]version/kind) to use when running helm template (Can be repeated to set several values: --helm-api-versions traefik.io/v1alpha1/TLSOption --helm-api-versions v1/Service). If not set, use the api-versions from the destination cluster")
	fs.StringVar(&o.project, "project", "", "Application project name")
	fs.StringVar(&o.syncPolicy, "sync-policy", "", "Set the sync policy (one of: manual (aliases of manual: none), automated (aliases of automated: auto, automatic))")
	fs.StringArrayVar(&o.syncOptions, "sync-option", []string{}, "Add or remove a sync option, e.g add `Prune=false`. Remove using `!` prefix, e.g. `!Prune=false`")
	fs.BoolVar(&o.autoPrune, "auto-prune", false, "Set automatic pruning for automated sync policy")
	fs.BoolVar(&o.selfHeal, "self-heal", false, "Set self healing for automated sync policy")
	fs.BoolVar(&o.allowEmpty, "allow-empty", false, "Set allow zero live resources for automated sync policy")
	fs.StringVar(&o.namePrefix, "nameprefix", "", "Kustomize nameprefix")
	fs.StringVar(&o.nameSuffix, "namesuffix", "", "Kustomize namesuffix")
	fs.StringVar(&o.kustomizeVersion, "kustomize-version", "", "Kustomize version")
	fs.BoolVar(&o.directoryRecurse, "directory-recurse", false, "Recurse directory")
	fs.StringVar(&o.configManagementPlugin, "config-management-plugin", "", "Config management plugin name")
	fs.StringArrayVar(&o.jsonnetTLAStr, "jsonnet-tla-str", []string{}, "Jsonnet top level string arguments")
	fs.StringArrayVar(&o.jsonnetTLACode, "jsonnet-tla-code", []string{}, "Jsonnet top level code arguments")
	fs.StringArrayVar(&o.jsonnetExtVarStr, "jsonnet-ext-var-str", []string{}, "Jsonnet string ext var")
	fs.StringArrayVar(&o.jsonnetExtVarCode, "jsonnet-ext-var-code", []string{}, "Jsonnet ext var")
	fs.StringArrayVar(&o.jsonnetLibs, "jsonnet-libs", []string{}, "Additional jsonnet libs (prefixed by repoRoot)")
	fs.StringArrayVar(&o.kustomizeImages, "kustomize-image", []string{}, "Kustomize images (e.g. --kustomize-image node:8.15.0 --kustomize-image mysql=mariadb,alpine@sha256:24a0c4b4a4c0eb97a1aabb8e29f18e917d05abfe1b7a7c07857230879ce7d3d)")
	fs.StringArrayVar(&o.kustomizeReplicas, "kustomize-replica", []string{}, "Kustomize replicas (e.g. --kustomize-replica my-development=2 --kustomize-replica my-statefulset=4)")
	fs.BoolVar(&o.ignoreMissingComponents, "ignore-missing-components", false, "Ignore locally missing component directories when setting Kustomize components")
	fs.StringArrayVar(&o.pluginEnvs, "plugin-env", []string{}, "Additional plugin envs")
	fs.BoolVar(&o.validate, "validate", true, "Validation of repo and cluster")
	fs.StringArrayVar(&o.kustomizeCommonLabels, "kustomize-common-label", []string{}, "Set common labels in Kustomize")
	fs.StringArrayVar(&o.kustomizeCommonAnnotations, "kustomize-common-annotation", []string{}, "Set common labels in Kustomize")
	fs.BoolVar(&o.kustomizeLabelWithoutSelector, "kustomize-label-without-selector", false, "Do not apply common label to selectors. Also do not apply label to templates unless --kustomize-label-include-templates is set")
	fs.BoolVar(&o.kustomizeLabelIncludeTemplates, "kustomize-label-include-templates", false, "Apply common label to resource templates")
	fs.BoolVar(&o.kustomizeForceCommonLabels, "kustomize-force-common-label", false, "Force common labels in Kustomize")
	fs.BoolVar(&o.kustomizeForceCommonAnnotations, "kustomize-force-common-annotation", false, "Force common annotations in Kustomize")
	fs.StringVar(&o.kustomizeNamespace, "kustomize-namespace", "", "Kustomize namespace")
	fs.StringVar(&o.kustomizeKubeVersion, "kustomize-kube-version", "", "kube-version to use when running helm template. If not set, use the kube version from the destination cluster. Only applicable when Helm is enabled for Kustomize builds")
	fs.StringArrayVar(&o.kustomizeAPIVersions, "kustomize-api-versions", nil, "api-versions (in format [group/]version/kind) to use when running helm template (Can be repeated to set several values: --helm-api-versions traefik.io/v1alpha1/TLSOption --helm-api-versions v1/Service). If not set, use the api-versions from the destination cluster. Only applicable when Helm is enabled for Kustomize builds")
	fs.StringVar(&o.directoryExclude, "directory-exclude", "", "Set glob expression used to exclude files from application source path")
	fs.StringVar(&o.directoryInclude, "directory-include", "", "Set glob expression used to include files from application source path")
	fs.Int64Var(&o.retryLimit, "sync-retry-limit", 0, "Max number of allowed sync retries")
	fs.DurationVar(&o.retryBackoffDuration, "sync-retry-backoff-duration", defaultRetryDuration, "Sync retry backoff base duration. Input needs to be a duration (e.g. 2m, 1h)")
	fs.DurationVar(&o.retryBackoffMaxDuration, "sync-retry-backoff-max-duration", defaultRetryMaxDuration, "Max sync retry backoff duration. Input needs to be a duration (e.g. 2m, 1h)")
	fs.Int64Var(&o.retryBackoffFactor, "sync-retry-backoff-factor", defaultRetryFactor, "Factor multiplies the base duration after each failed sync retry")
	fs.BoolVar(&o.retryRefresh, "sync-retry-refresh", false, "Indicates if the latest revision should be used on retry instead of the initial one")
	fs.StringVar(&o.ref, "ref", "", "Ref is reference to another source within sources field")
	fs.StringVar(&o.sourceName, "source-name", "", "Name of the source from the list of sources of the app.")
}

// specMapper applies the shared Application flags to a spec (upstream SetAppSpecOptions,
// ConstructSource and constructSourceHydrator).
type specMapper struct {
	ctx  context.Context
	e    *env
	fs   *pflag.FlagSet
	opts *appOptions
}

// apply updates spec from the changed flags and returns the number of changed Application
// flags (0 means "nothing to update"). The hydrator flags edit spec.sourceHydrator. The
// source flags edit the source at sourcePosition (1-based; <= 0 means the single source, or a
// new source appended to a multi-source Application), or, when the Application uses a source
// hydrator, its dry source (see hydratorSource).
func (m *specMapper) apply(spec *v1alpha1.ApplicationSpec, sourcePosition int) (int, error) {
	visited := 0
	names := appFlagNames()
	m.fs.Visit(func(f *pflag.Flag) {
		if names[f.Name] {
			visited++
		}
	})
	if h, hasHydratorFlag := m.sourceHydrator(spec.SourceHydrator); hasHydratorFlag {
		spec.SourceHydrator = h
	}
	if spec.SourceHydrator != nil {
		if err := m.hydratorSource(spec.SourceHydrator, sourcePosition); err != nil {
			return visited, err
		}
	} else {
		source := sourceByPosition(spec, sourcePosition)
		if source == nil {
			source = &v1alpha1.ApplicationSource{}
		}
		if _, err := m.source(source); err != nil {
			return visited, err
		}
		if spec.HasMultipleSources() {
			switch {
			case sourcePosition == 0:
				spec.Sources[0] = *source
			case sourcePosition > 0:
				spec.Sources[sourcePosition-1] = *source
			default:
				spec.Sources = append(spec.Sources, *source)
			}
		} else {
			spec.Source = source
		}
	}
	if err := m.specFields(spec); err != nil {
		return visited, err
	}
	return visited, nil
}

// hydratorLocationFlags are the source flags that locate or name a plain source. A source
// hydrator has no plain source: its locations are set with --dry-source-* / --sync-source-*.
var hydratorLocationFlags = []string{"repo", "path", "revision", "helm-chart", "tag-prefix", "ref", "source-name"}

// hydratorSource applies the source flags to an Application that uses a source hydrator.
// Upstream edits a detached copy of the dry or sync source and drops the result, reporting
// success for a change that never happens; here every source flag either takes effect or
// fails. Tool options (Helm, Kustomize, directory/Jsonnet, plugin) say how the dry source is
// rendered and are stored in spec.sourceHydrator.drySource. Location flags are rejected
// (they would compete with the --dry-source-*/--sync-source-* flags), and so are tool options
// for --source-position > 0 (upstream's sync source, which holds rendered manifests).
func (m *specMapper) hydratorSource(h *v1alpha1.SourceHydrator, position int) error {
	var location []string
	for _, name := range hydratorLocationFlags {
		if m.fs.Changed(name) {
			location = append(location, "--"+name)
		}
	}
	if len(location) > 0 {
		return fatalGeneric("%s cannot be used with an application that uses a source hydrator; "+
			"use --dry-source-repo, --dry-source-path, --dry-source-revision, --sync-source-branch or --sync-source-path",
			strings.Join(location, ", "))
	}
	view := drySourceView(h)
	applied, err := m.source(&view)
	if err != nil || len(applied) == 0 {
		return err
	}
	if position > 0 {
		return fatalGeneric("an application that uses a source hydrator has no source at position %d: "+
			"--%s applies to the dry source, omit --source-position", position, applied[0])
	}
	storeDrySource(h, &view)
	return nil
}

// specFields applies destination, project, history and sync policy flags.
func (m *specMapper) specFields(spec *v1alpha1.ApplicationSpec) error {
	o := m.opts
	var err error
	m.fs.Visit(func(f *pflag.Flag) {
		if err != nil {
			return
		}
		switch f.Name {
		case "revision-history-limit":
			i := int64(o.revisionHistoryLimit)
			spec.RevisionHistoryLimit = &i
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
				if syncPolicyIsZero(spec.SyncPolicy) {
					spec.SyncPolicy = nil
				}
			case "automated", "automatic", "auto":
				if spec.SyncPolicy == nil {
					spec.SyncPolicy = &v1alpha1.SyncPolicy{}
				}
				spec.SyncPolicy.Automated = &v1alpha1.SyncPolicyAutomated{}
			default:
				err = fatal("Invalid sync-policy: %s", o.syncPolicy)
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
			if syncPolicyIsZero(spec.SyncPolicy) {
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
					Limit: o.retryLimit,
					Backoff: &v1alpha1.Backoff{
						Duration:    o.retryBackoffDuration.String(),
						MaxDuration: o.retryBackoffMaxDuration.String(),
						Factor:      &factor,
					},
					Refresh: o.retryRefresh,
				}
			case o.retryLimit == 0:
				if spec.SyncPolicy != nil {
					spec.SyncPolicy.Retry = nil
				}
				if syncPolicyIsZero(spec.SyncPolicy) {
					spec.SyncPolicy = nil
				}
			default:
				err = fatal("Invalid sync-retry-limit [%d]", o.retryLimit)
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
	if m.fs.Changed("auto-prune") || m.fs.Changed("self-heal") || m.fs.Changed("allow-empty") {
		if spec.SyncPolicy == nil {
			spec.SyncPolicy = &v1alpha1.SyncPolicy{}
		}
		if spec.SyncPolicy.Automated == nil {
			disabled := false
			spec.SyncPolicy.Automated = &v1alpha1.SyncPolicyAutomated{Enabled: &disabled}
		}
		if m.fs.Changed("auto-prune") {
			v := o.autoPrune
			spec.SyncPolicy.Automated.Prune = &v
		}
		if m.fs.Changed("self-heal") {
			v := o.selfHeal
			spec.SyncPolicy.Automated.SelfHeal = &v
		}
		if m.fs.Changed("allow-empty") {
			v := o.allowEmpty
			spec.SyncPolicy.Automated.AllowEmpty = &v
		}
	}
	return nil
}

// source applies the source flags to s in place (upstream ConstructSource) and returns the
// names of the source flags it applied.
func (m *specMapper) source(s *v1alpha1.ApplicationSource) ([]string, error) {
	o := m.opts
	var (
		err     error
		applied []string
	)
	fail := func(e error) {
		if err == nil && e != nil {
			err = e
		}
	}
	helm := func(fn func(h *v1alpha1.ApplicationSourceHelm) error) {
		if s.Helm == nil {
			s.Helm = &v1alpha1.ApplicationSourceHelm{}
		}
		fail(fn(s.Helm))
		if helmIsZero(s.Helm) {
			s.Helm = nil
		}
	}
	kustomize := func(fn func(k *v1alpha1.ApplicationSourceKustomize) error) {
		if s.Kustomize == nil {
			s.Kustomize = &v1alpha1.ApplicationSourceKustomize{}
		}
		fail(fn(s.Kustomize))
		if kustomizeIsZero(s.Kustomize) {
			s.Kustomize = nil
		}
	}
	directory := func(fn func(d *v1alpha1.ApplicationSourceDirectory)) {
		if s.Directory == nil {
			s.Directory = &v1alpha1.ApplicationSourceDirectory{}
		}
		fn(s.Directory)
	}
	m.fs.Visit(func(f *pflag.Flag) {
		if err != nil {
			return
		}
		switch f.Name {
		case "repo":
			s.RepoURL = o.repoURL
		case "path":
			s.Path = o.appPath
		case "helm-chart":
			s.Chart = o.chart
		case "revision":
			s.TargetRevision = o.revision
		case "tag-prefix":
			s.TagPrefix = o.tagPrefix
		case "values":
			helm(func(h *v1alpha1.ApplicationSourceHelm) error {
				if len(o.valuesFiles) > 0 {
					h.ValueFiles = o.valuesFiles
				}
				return nil
			})
		case "ignore-missing-value-files":
			helm(func(h *v1alpha1.ApplicationSourceHelm) error {
				if o.ignoreMissingValueFiles {
					h.IgnoreMissingValueFiles = true
				}
				return nil
			})
		case "values-literal-file":
			data, rerr := m.e.readFileOrURL(m.ctx, o.values)
			if rerr != nil {
				fail(rerr)
				return
			}
			helm(func(h *v1alpha1.ApplicationSourceHelm) error {
				if len(data) == 0 {
					return nil
				}
				return setHelmValues(h, string(data))
			})
		case "release-name":
			helm(func(h *v1alpha1.ApplicationSourceHelm) error { setIf(&h.ReleaseName, o.releaseName); return nil })
		case "helm-version":
			helm(func(h *v1alpha1.ApplicationSourceHelm) error { setIf(&h.Version, o.helmVersion); return nil })
		case "helm-pass-credentials":
			helm(func(h *v1alpha1.ApplicationSourceHelm) error {
				setTrue(&h.PassCredentials, o.helmPassCredentials)
				return nil
			})
		case "helm-set":
			helm(func(h *v1alpha1.ApplicationSourceHelm) error { return addHelmParams(h, o.helmSets, false) })
		case "helm-set-string":
			helm(func(h *v1alpha1.ApplicationSourceHelm) error { return addHelmParams(h, o.helmSetStrings, true) })
		case "helm-set-file":
			helm(func(h *v1alpha1.ApplicationSourceHelm) error {
				for _, text := range o.helmSetFiles {
					p, perr := newHelmFileParameter(text)
					if perr != nil {
						return perr
					}
					addHelmFileParameter(h, p)
				}
				return nil
			})
		case "helm-skip-crds":
			helm(func(h *v1alpha1.ApplicationSourceHelm) error { setTrue(&h.SkipCrds, o.helmSkipCrds); return nil })
		case "helm-skip-schema-validation":
			helm(func(h *v1alpha1.ApplicationSourceHelm) error {
				setTrue(&h.SkipSchemaValidation, o.helmSkipSchemaValidation)
				return nil
			})
		case "helm-skip-tests":
			helm(func(h *v1alpha1.ApplicationSourceHelm) error { setTrue(&h.SkipTests, o.helmSkipTests); return nil })
		case "helm-namespace":
			helm(func(h *v1alpha1.ApplicationSourceHelm) error { setIf(&h.Namespace, o.helmNamespace); return nil })
		case "helm-kube-version":
			helm(func(h *v1alpha1.ApplicationSourceHelm) error { setIf(&h.KubeVersion, o.helmKubeVersion); return nil })
		case "helm-api-versions":
			helm(func(h *v1alpha1.ApplicationSourceHelm) error {
				if len(o.helmAPIVersions) > 0 {
					h.APIVersions = o.helmAPIVersions
				}
				return nil
			})
		case "directory-recurse":
			directory(func(d *v1alpha1.ApplicationSourceDirectory) { d.Recurse = o.directoryRecurse })
		case "directory-exclude":
			directory(func(d *v1alpha1.ApplicationSourceDirectory) { d.Exclude = o.directoryExclude })
		case "directory-include":
			directory(func(d *v1alpha1.ApplicationSourceDirectory) { d.Include = o.directoryInclude })
		case "config-management-plugin":
			s.Plugin = &v1alpha1.ApplicationSourcePlugin{Name: o.configManagementPlugin}
		case "nameprefix":
			kustomize(func(k *v1alpha1.ApplicationSourceKustomize) error { setIf(&k.NamePrefix, o.namePrefix); return nil })
		case "namesuffix":
			kustomize(func(k *v1alpha1.ApplicationSourceKustomize) error { setIf(&k.NameSuffix, o.nameSuffix); return nil })
		case "kustomize-image":
			kustomize(func(k *v1alpha1.ApplicationSourceKustomize) error {
				for _, image := range o.kustomizeImages {
					mergeImage(k, image)
				}
				return nil
			})
		case "kustomize-replica":
			kustomize(func(k *v1alpha1.ApplicationSourceKustomize) error {
				for _, text := range o.kustomizeReplicas {
					r, rerr := newKustomizeReplica(text)
					if rerr != nil {
						return rerr
					}
					mergeReplica(k, r)
				}
				return nil
			})
		case "kustomize-version":
			kustomize(func(k *v1alpha1.ApplicationSourceKustomize) error { setIf(&k.Version, o.kustomizeVersion); return nil })
		case "kustomize-namespace":
			kustomize(func(k *v1alpha1.ApplicationSourceKustomize) error {
				setIf(&k.Namespace, o.kustomizeNamespace)
				return nil
			})
		case "kustomize-kube-version":
			kustomize(func(k *v1alpha1.ApplicationSourceKustomize) error {
				setIf(&k.KubeVersion, o.kustomizeKubeVersion)
				return nil
			})
		case "kustomize-api-versions":
			kustomize(func(k *v1alpha1.ApplicationSourceKustomize) error {
				if len(o.kustomizeAPIVersions) > 0 {
					k.APIVersions = o.kustomizeAPIVersions
				}
				return nil
			})
		case "kustomize-common-label":
			kustomize(func(k *v1alpha1.ApplicationSourceKustomize) error {
				labels, perr := parseKeyValues(o.kustomizeCommonLabels)
				if perr != nil {
					return perr
				}
				if labels != nil {
					k.CommonLabels = labels
				}
				return nil
			})
		case "kustomize-common-annotation":
			kustomize(func(k *v1alpha1.ApplicationSourceKustomize) error {
				annotations, perr := parseKeyValues(o.kustomizeCommonAnnotations)
				if perr != nil {
					return perr
				}
				if annotations != nil {
					k.CommonAnnotations = annotations
				}
				return nil
			})
		case "kustomize-label-without-selector":
			kustomize(func(k *v1alpha1.ApplicationSourceKustomize) error {
				setTrue(&k.LabelWithoutSelector, o.kustomizeLabelWithoutSelector)
				return nil
			})
		case "kustomize-label-include-templates":
			kustomize(func(k *v1alpha1.ApplicationSourceKustomize) error {
				setTrue(&k.LabelIncludeTemplates, o.kustomizeLabelIncludeTemplates)
				return nil
			})
		case "kustomize-force-common-label":
			kustomize(func(k *v1alpha1.ApplicationSourceKustomize) error {
				setTrue(&k.ForceCommonLabels, o.kustomizeForceCommonLabels)
				return nil
			})
		case "kustomize-force-common-annotation":
			kustomize(func(k *v1alpha1.ApplicationSourceKustomize) error {
				setTrue(&k.ForceCommonAnnotations, o.kustomizeForceCommonAnnotations)
				return nil
			})
		case "ignore-missing-components":
			kustomize(func(k *v1alpha1.ApplicationSourceKustomize) error {
				setTrue(&k.IgnoreMissingComponents, o.ignoreMissingComponents)
				return nil
			})
		case "jsonnet-tla-str":
			directory(func(d *v1alpha1.ApplicationSourceDirectory) { addJsonnetVars(&d.Jsonnet.TLAs, o.jsonnetTLAStr, false) })
		case "jsonnet-tla-code":
			directory(func(d *v1alpha1.ApplicationSourceDirectory) { addJsonnetVars(&d.Jsonnet.TLAs, o.jsonnetTLACode, true) })
		case "jsonnet-ext-var-str":
			directory(func(d *v1alpha1.ApplicationSourceDirectory) {
				addJsonnetVars(&d.Jsonnet.ExtVars, o.jsonnetExtVarStr, false)
			})
		case "jsonnet-ext-var-code":
			directory(func(d *v1alpha1.ApplicationSourceDirectory) {
				addJsonnetVars(&d.Jsonnet.ExtVars, o.jsonnetExtVarCode, true)
			})
		case "jsonnet-libs":
			directory(func(d *v1alpha1.ApplicationSourceDirectory) {
				d.Jsonnet.Libs = append(d.Jsonnet.Libs, o.jsonnetLibs...)
			})
		case "plugin-env":
			if s.Plugin == nil {
				s.Plugin = &v1alpha1.ApplicationSourcePlugin{}
			}
			for _, text := range o.pluginEnvs {
				e, perr := newEnvEntry(text)
				if perr != nil {
					fail(perr)
					return
				}
				addEnvEntry(s.Plugin, e)
			}
		case "ref":
			s.Ref = o.ref
		case "source-name":
			s.Name = o.sourceName
		default:
			return
		}
		applied = append(applied, f.Name)
	})
	return applied, err
}

// sourceHydrator applies the hydrator flags. It returns the edited hydrator (possibly a new
// one) and whether any hydrator flag was set.
func (m *specMapper) sourceHydrator(h *v1alpha1.SourceHydrator) (*v1alpha1.SourceHydrator, bool) {
	o := m.opts
	has := false
	ensure := func(notEmpty bool) bool {
		has = true
		if notEmpty && h == nil {
			h = &v1alpha1.SourceHydrator{}
		}
		return h != nil
	}
	m.fs.Visit(func(f *pflag.Flag) {
		switch f.Name {
		case "dry-source-repo":
			if ensure(o.drySourceRepo != "") {
				h.DrySource.RepoURL = o.drySourceRepo
			}
		case "dry-source-path":
			if ensure(o.drySourcePath != "") {
				h.DrySource.Path = o.drySourcePath
			}
		case "dry-source-revision":
			if ensure(o.drySourceRevision != "") {
				h.DrySource.TargetRevision = o.drySourceRevision
			}
		case "sync-source-branch":
			if ensure(o.syncSourceBranch != "") {
				h.SyncSource.TargetBranch = o.syncSourceBranch
			}
		case "sync-source-path":
			if ensure(o.syncSourcePath != "") {
				h.SyncSource.Path = o.syncSourcePath
			}
		case "hydrate-to-branch":
			if ensure(o.hydrateToBranch != "") {
				if o.hydrateToBranch == "" {
					h.HydrateTo = nil
				} else {
					h.HydrateTo = &v1alpha1.HydrateTo{TargetBranch: o.hydrateToBranch}
				}
			}
		}
	})
	return h, has
}

func setIf(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func setTrue(dst *bool, v bool) {
	if v {
		*dst = true
	}
}

func addHelmParams(h *v1alpha1.ApplicationSourceHelm, texts []string, forceString bool) error {
	for _, text := range texts {
		p, err := newHelmParameter(text, forceString)
		if err != nil {
			return err
		}
		addHelmParameter(h, p)
	}
	return nil
}

func addJsonnetVars(dst *[]v1alpha1.JsonnetVar, texts []string, code bool) {
	for _, t := range texts {
		*dst = append(*dst, newJsonnetVar(t, code))
	}
}

// setParameterOverrides applies -p name=value overrides to the source at position (Helm
// only, upstream setParameterOverrides). For an Application that uses a source hydrator they
// go to the dry source (upstream edits a detached copy and loses them).
func setParameterOverrides(app *v1alpha1.Application, parameters []string, position int) error {
	if len(parameters) == 0 {
		return nil
	}
	if h := app.Spec.SourceHydrator; h != nil {
		if position > 0 {
			return fatalGeneric("an application that uses a source hydrator has no source at position %d: "+
				"--parameter applies to the dry source, omit --source-position", position)
		}
		view := drySourceView(h)
		// status.sourceType describes the rendered sync source (hydrated manifests), not the
		// dry source, so only the dry source's own tool block or the parameter syntax count.
		if err := overrideHelmParameters(&view, "", parameters); err != nil {
			return err
		}
		storeDrySource(h, &view)
		return nil
	}
	source := sourceByPosition(&app.Spec, position)
	if source == nil {
		return fatal("Parameters can only be set against Helm applications")
	}
	return overrideHelmParameters(source, app.Status.SourceType, parameters)
}

// overrideHelmParameters adds the parameters to source when it is (or, by statusType or the
// name=value syntax, is taken to be) a Helm source.
func overrideHelmParameters(source *v1alpha1.ApplicationSource, statusType v1alpha1.ApplicationSourceType, parameters []string) error {
	sourceType, err := explicitType(source)
	if err != nil {
		return err
	}
	switch {
	case sourceType != "":
	case statusType != "":
		sourceType = statusType
	case strings.Contains(parameters[0], "="):
		sourceType = v1alpha1.ApplicationSourceTypeHelm
	}
	if sourceType != v1alpha1.ApplicationSourceTypeHelm {
		return fatal("Parameters can only be set against Helm applications")
	}
	if source.Helm == nil {
		source.Helm = &v1alpha1.ApplicationSourceHelm{}
	}
	for _, text := range parameters {
		p, err := newHelmParameter(text, false)
		if err != nil {
			return fmt.Errorf("invalid parameter override: %w", err)
		}
		addHelmParameter(source.Helm, p)
	}
	return nil
}
