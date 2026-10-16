package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Limits of local manifests accepted by the sync API (Operation CRD bounds).
const (
	maxLocalManifests      = 1024
	maxLocalManifestsBytes = 1 << 20
)

type syncOptions struct {
	revision                  string
	revisions                 []string
	sourcePositions           []int64
	sourceNames               []string
	resources                 []string
	labels                    []string
	selector                  string
	prune                     bool
	dryRun                    bool
	timeout                   uint
	strategy                  string
	force                     bool
	replace                   bool
	serverSideApply           bool
	applyOutOfSyncOnly        bool
	async                     bool
	retryLimit                int64
	retryRefresh              bool
	retryBackoffDuration      time.Duration
	retryBackoffMaxDuration   time.Duration
	retryBackoffFactor        int64
	local                     string
	localRepoRoot             string
	localIncludes             []string
	serverSideGenerate        bool
	infos                     []string
	previewChanges            bool
	assumeYes                 bool
	projects                  []string
	output                    string
	appNamespace              string
	jqTimeout                 time.Duration
	serverSideDiffConcurrency int
	serverSideDiffMaxBatchKB  int
}

func newSyncCommand(e *env) *cobra.Command {
	o := &syncOptions{}
	c := &cobra.Command{
		Use:   "sync [APPNAME... | -l selector | --project project-name]",
		Short: "Sync an application to its target state",
		Example: `  # Sync an app
  patrol app sync my-app

  # Sync multiples apps
  patrol app sync my-app other-app

  # Sync apps by label, in this example we sync apps that are children of another app (aka app-of-apps)
  patrol app sync -l app.kubernetes.io/instance=my-app
  patrol app sync -l app.kubernetes.io/instance!=my-app
  patrol app sync -l app.kubernetes.io/instance
  patrol app sync -l '!app.kubernetes.io/instance'
  patrol app sync -l 'app.kubernetes.io/instance notin (my-app,other-app)'

  # Sync a multi-source application for specific revision of specific sources
  patrol app sync my-app --revisions 0.0.1 --source-positions 1 --revisions 0.0.2 --source-positions 2
  patrol app sync my-app --revisions 0.0.1 --source-names my-chart --revisions 0.0.2 --source-names my-values

  # Sync a specific resource
  # Resource should be formatted as GROUP:KIND:NAME. If no GROUP is specified then :KIND:NAME
  patrol app sync my-app --resource :Service:my-service
  patrol app sync my-app --resource argoproj.io:Rollout:my-rollout
  patrol app sync my-app --resource '!apps:Deployment:my-service'
  patrol app sync my-app --resource apps:Deployment:my-service --resource :Service:my-service
  patrol app sync my-app --resource '!*:Service:*'
  # Specify namespace if the application has resources with the same name in different namespaces
  patrol app sync my-app --resource argoproj.io:Rollout:my-namespace/my-rollout

  # Sync local manifests (requires the override permission)
  patrol app sync my-app --local ./manifests --local-repo-root .`,
		RunE: func(c *cobra.Command, args []string) error { return runSync(c, e, o, args) },
	}
	fs := c.Flags()
	fs.BoolVar(&o.dryRun, "dry-run", false, "Preview apply without affecting cluster")
	fs.BoolVar(&o.prune, "prune", false, "Allow deleting unexpected resources")
	fs.StringVar(&o.revision, "revision", "", "Sync to a specific revision. Preserves parameter overrides")
	fs.StringArrayVar(&o.resources, "resource", []string{}, "Sync only specific resources as GROUP:KIND:NAME or !GROUP:KIND:NAME. Fields may be blank and '*' can be used. This option may be specified repeatedly")
	fs.StringVarP(&o.selector, "selector", "l", "", "Sync apps that match this label. Supports '=', '==', '!=', in, notin, exists & not exists. Matching apps must satisfy all of the specified label constraints.")
	fs.StringArrayVar(&o.labels, "label", []string{}, "Sync only specific resources with a label. This option may be specified repeatedly.")
	fs.UintVar(&o.timeout, "timeout", 0, "Time out after this many seconds")
	fs.Int64Var(&o.retryLimit, "retry-limit", 0, "Max number of allowed sync retries")
	fs.BoolVar(&o.retryRefresh, "retry-refresh", false, "Indicates if the latest revision should be used on retry instead of the initial one")
	fs.DurationVar(&o.retryBackoffDuration, "retry-backoff-duration", defaultRetryDuration, "Retry backoff base duration. Input needs to be a duration (e.g. 2m, 1h)")
	fs.DurationVar(&o.retryBackoffMaxDuration, "retry-backoff-max-duration", defaultRetryMaxDuration, "Max retry backoff duration. Input needs to be a duration (e.g. 2m, 1h)")
	fs.Int64Var(&o.retryBackoffFactor, "retry-backoff-factor", defaultRetryFactor, "Factor multiplies the base duration after each failed retry")
	fs.StringVar(&o.strategy, "strategy", "", "Sync strategy (one of: apply|hook)")
	fs.BoolVar(&o.force, "force", false, "Use a force apply")
	fs.BoolVar(&o.replace, "replace", false, "Use a kubectl create/replace instead apply")
	fs.BoolVar(&o.serverSideApply, "server-side", false, "Use server-side apply while syncing the application")
	fs.BoolVar(&o.applyOutOfSyncOnly, "apply-out-of-sync-only", false, "Sync only out-of-sync resources")
	fs.BoolVar(&o.async, "async", false, "Do not wait for application to sync before continuing")
	fs.StringVar(&o.local, "local", "", "Path to a local directory. When this flag is present no git queries will be made")
	fs.StringVar(&o.localRepoRoot, "local-repo-root", "/", "Path to the repository root. Used together with --local allows setting the repository root")
	fs.BoolVar(&o.serverSideGenerate, "server-side-generate", false, "Used with --local: send the local files to the server and sync the manifests it renders (Secrets cannot be synced this way)")
	fs.StringArrayVar(&o.localIncludes, "local-include", defaultLocalIncludes, "Used with --server-side-generate, specify patterns of filenames to send. Matching is based on filename and not path.")
	fs.StringArrayVar(&o.infos, "info", []string{}, "A list of key-value pairs during sync process. These infos will be persisted in app.")
	fs.BoolVar(&o.assumeYes, "assumeYes", false, "Assume yes as answer for all user queries or prompts")
	fs.BoolVar(&o.previewChanges, "preview-changes", false, "Preview difference against the target and live state before syncing app and wait for user confirmation")
	fs.StringArrayVar(&o.projects, "project", []string{}, "Sync apps that belong to the specified projects. This option may be specified repeatedly.")
	fs.StringVarP(&o.output, "output", "o", outWide, "Output format. One of: json|yaml|wide|tree|tree=detailed")
	fs.StringVarP(&o.appNamespace, "app-namespace", "N", "", "Only sync an application in namespace")
	fs.DurationVar(&o.jqTimeout, "ignore-normalizer-jq-execution-timeout", defaultJQTimeout, "Set ignore normalizer JQ execution timeout")
	fs.StringArrayVar(&o.revisions, "revisions", []string{}, "Show manifests at specific revisions for source position in source-positions")
	fs.Int64SliceVar(&o.sourcePositions, "source-positions", []int64{}, "List of source positions. Default is empty array. Counting start at 1.")
	fs.StringArrayVar(&o.sourceNames, "source-names", []string{}, "List of source names. Default is an empty array.")
	addServerSideDiffPerfFlags(c, &o.serverSideDiffConcurrency, &o.serverSideDiffMaxBatchKB)
	return c
}

func validateSyncArgs(o *syncOptions, args []string) error {
	if len(args) > 1 && o.selector != "" {
		return fatal("Cannot use selector option when application name(s) passed as argument(s)")
	}
	if len(args) != 1 && (len(o.revisions) > 0 || len(o.sourcePositions) > 0) {
		return fatal("Cannot use --revisions and --source-positions options when 0 or more than 1 application names are passed as argument(s)")
	}
	if len(args) != 1 && (len(o.revisions) > 0 || len(o.sourceNames) > 0) {
		return fatal("Cannot use --revisions and --source-names options when 0 or more than 1 application names are passed as argument(s)")
	}
	if len(o.sourceNames) > 0 && len(o.sourcePositions) > 0 {
		return fatal("Only one of source-positions and source-names can be specified.")
	}
	if len(o.sourcePositions) > 0 && len(o.revisions) != len(o.sourcePositions) {
		return fatal("While using --revisions and --source-positions, length of values for both flags should be same.")
	}
	if len(o.sourceNames) > 0 && len(o.revisions) != len(o.sourceNames) {
		return fatal("While using --revisions and --source-names, length of values for both flags should be same.")
	}
	for _, p := range o.sourcePositions {
		if p <= 0 {
			return fatal("source-position cannot be less than or equal to 0, Counting starts at 1")
		}
	}
	switch o.strategy {
	case "", "hook", "apply":
	default:
		return fatal("Unknown sync strategy: '%s'", o.strategy)
	}
	if o.serverSideDiffConcurrency == 0 {
		return fatalGeneric("invalid value for --server-side-diff-concurrency: 0 is not allowed (use -1 for unlimited, or a positive number to limit concurrency)")
	}
	return validateWaitOutput(o.output)
}

func runSync(c *cobra.Command, e *env, o *syncOptions, args []string) error {
	ctx := cmdContext(c)
	if len(args) == 0 && o.selector == "" && len(o.projects) == 0 {
		return usage(c, args)
	}
	if err := validateSyncArgs(o, args); err != nil {
		return err
	}
	selectedLabels, err := parseKeyValues(o.labels)
	if err != nil {
		return err
	}
	infos, err := parseInfos(o.infos)
	if err != nil {
		return err
	}
	cl, err := e.client(ctx)
	if err != nil {
		return err
	}
	positions := o.sourcePositions
	if len(args) == 1 && len(o.sourceNames) > 0 {
		name, ns := parseQualifiedName(args[0], o.appNamespace)
		app, err := getApp(ctx, cl, name, ns, "")
		if err != nil {
			return err
		}
		if positions, err = resolveSourceNames(app, o.sourceNames); err != nil {
			return err
		}
	}
	names := append([]string{}, args...)
	if o.selector != "" || len(o.projects) > 0 {
		apps, err := listAllApps(ctx, cl, apiclient.ApplicationQuery{Selector: o.selector, AppNamespace: o.appNamespace, Projects: o.projects})
		if err != nil {
			return err
		}
		if len(apps) == 0 {
			msg := "No matching apps found for filter:"
			if o.selector != "" {
				msg += " selector " + o.selector
			}
			if len(o.projects) > 0 {
				msg += fmt.Sprintf(" projects %v", o.projects)
			}
			return fatal("%s", msg)
		}
		for i := range apps {
			names = append(names, apps[i].QualifiedName())
		}
	}
	var settings *apiclient.Settings
	if o.local != "" || o.previewChanges {
		if settings, err = cl.GetSettings(ctx); err != nil {
			return err
		}
	}
	for _, full := range names {
		name, ns := parseQualifiedName(full, o.appNamespace)
		done, err := syncOne(ctx, c, e, cl, o, settings, selectedLabels, infos, positions, name, ns)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	return nil
}

// parseInfos parses --info key=value pairs (sorted by name for a stable request).
func parseInfos(items []string) ([]v1alpha1.Info, error) {
	m, err := parseKeyValues(items)
	if err != nil {
		return nil, err
	}
	out := make([]v1alpha1.Info, 0, len(m))
	for k, v := range m {
		out = append(out, v1alpha1.Info{Name: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// syncOne syncs one Application. stop reports that the command must end without syncing the
// remaining Applications (no differences found with --preview-changes, or the preview was
// declined), as upstream.
func syncOne(ctx context.Context, c *cobra.Command, e *env, cl Client, o *syncOptions, settings *apiclient.Settings,
	selectedLabels map[string]string, infos []v1alpha1.Info, positions []int64, name, ns string,
) (stop bool, err error) {
	out := c.OutOrStdout()
	resources := append([]string{}, o.resources...)
	if len(selectedLabels) > 0 {
		res, err := cl.GetApplicationManifests(ctx, &apiclient.ApplicationManifestQuery{
			Name: name, AppNamespace: ns, Revision: o.revision, Revisions: o.revisions, SourcePositions: positions,
		})
		if err != nil {
			return false, err
		}
		_, _ = fmt.Fprintln(out, "The name of the app is ", name)
		objs, err := manifestsToObjects(res.Manifests)
		if err != nil {
			return false, err
		}
		keys := make([]string, 0, len(selectedLabels))
		for k := range selectedLabels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, obj := range objs {
			for _, k := range keys {
				if v, ok := obj.GetLabels()[k]; ok && v == selectedLabels[k] {
					gvk := obj.GroupVersionKind()
					resources = append(resources, fmt.Sprintf("%s:%s:%s", gvk.Group, gvk.Kind, obj.GetName()))
				}
			}
		}
		if len(resources) == 0 {
			return false, fatal("No matching resources found for labels: %v", o.labels)
		}
	}
	selectors, err := parseResourceSelectors(resources)
	if err != nil {
		return false, err
	}
	app, err := getApp(ctx, cl, name, ns, "")
	if err != nil {
		return false, err
	}
	if app.Spec.HasMultipleSources() {
		if o.revision != "" {
			return false, fatal("patrol cli does not work on multi-source app with --revision flag. Use --revisions and --source-positions instead.")
		}
		if o.local != "" {
			return false, fatal("patrol cli does not work on multi-source app with --local flag")
		}
	}
	filtered := selectAppResources(app, selectors)
	if len(resources) > 0 && len(filtered) == 0 {
		return false, fatal("No matching app resources found for resource filter: %v", strings.Join(resources, ", "))
	}
	var localManifests []string
	if o.local != "" {
		if app.Spec.IsAutomatedSyncEnabled() && !o.dryRun {
			return false, fatal("Cannot use local sync when Automatic Sync Policy is enabled except with --dry-run")
		}
		if localManifests, err = localSyncManifests(ctx, e, cl, app, settings, o); err != nil {
			return false, err
		}
	}
	req := &apiclient.ApplicationSyncRequest{
		Name: name, AppNamespace: ns, DryRun: o.dryRun, Revision: o.revision, Resources: filtered, Prune: o.prune,
		Manifests: localManifests, Infos: infos, Revisions: o.revisions, SourcePositions: positions,
	}
	var items []string
	if o.replace {
		items = append(items, v1alpha1.SyncOptionKeyReplace+"=true")
	}
	if o.serverSideApply {
		items = append(items, v1alpha1.SyncOptionKeyServerSideApply+"=true")
	}
	if o.applyOutOfSyncOnly {
		items = append(items, v1alpha1.SyncOptionKeyApplyOutOfSyncOnly+"=true")
	}
	if len(items) > 0 {
		req.SyncOptions = &apiclient.ApplicationSyncOptions{Items: items}
	}
	if o.strategy == "apply" {
		req.Strategy = &v1alpha1.SyncStrategy{Apply: &v1alpha1.SyncStrategyApply{Force: o.force}}
	} else {
		req.Strategy = &v1alpha1.SyncStrategy{Hook: &v1alpha1.SyncStrategyHook{SyncStrategyApply: v1alpha1.SyncStrategyApply{Force: o.force}}}
	}
	if o.retryLimit != 0 {
		factor := o.retryBackoffFactor
		req.RetryStrategy = &v1alpha1.RetryStrategy{
			Limit: o.retryLimit, Refresh: o.retryRefresh,
			Backoff: &v1alpha1.Backoff{Duration: o.retryBackoffDuration.String(), MaxDuration: o.retryBackoffMaxDuration.String(), Factor: &factor},
		}
	}
	if o.previewChanges {
		proceed, err := previewSync(ctx, c, e, cl, o, settings, app, localManifests, positions)
		if err != nil || !proceed {
			return true, err
		}
	}
	resp, err := cl.SyncApplication(ctx, req)
	if err != nil {
		return false, err
	}
	if o.async {
		return false, nil
	}
	opName := ""
	if resp.Operation != nil {
		opName = resp.Operation.Name
	}
	w := &waiter{e: e, cl: cl, out: out, name: name, namespace: ns, timeout: time.Duration(o.timeout) * time.Second,
		watch: watchOpts{operation: true}, selectors: selectors, output: o.output, operation: opName}
	result, err := w.wait(ctx)
	if err != nil {
		if errors.Is(err, errWaitTimeout) {
			return false, fatalGeneric("timed out (%ds) waiting for app %q match desired state", o.timeout, qualify(name, ns))
		}
		return false, err
	}
	if o.dryRun {
		return false, nil
	}
	return false, checkSyncResult(result, len(selectors) > 0)
}

// checkSyncResult turns an unsuccessful operation into an exit-1 error (upstream
// "Operation has completed with phase" and "resources require pruning").
func checkSyncResult(result *waitResult, partial bool) error {
	if result == nil || result.Operation == nil {
		return nil
	}
	op := result.Operation
	if !op.Status.Phase.IsSuccessful() {
		return fatal("Operation has completed with phase: %s", op.Status.Phase)
	}
	if !partial && result.App != nil && result.App.Status.Sync.Status != v1alpha1.SyncStatusCodeSynced && op.Status.SyncResult != nil {
		pruning := 0
		for _, r := range op.Status.SyncResult.Resources {
			if r.Status == v1alpha1.ResultCodePruneSkipped {
				pruning++
			}
		}
		if pruning > 0 {
			return fatal("%d resources require pruning", pruning)
		}
	}
	return nil
}

// localSyncManifests renders --local for a sync: client side by default (Secrets included,
// as upstream), or on the server with --server-side-generate (refused when the output has a
// Secret, because the server masks Secret data in rendered manifests).
func localSyncManifests(ctx context.Context, e *env, cl Client, app *v1alpha1.Application, settings *apiclient.Settings, o *syncOptions) ([]string, error) {
	var manifests []string
	var err error
	if o.serverSideGenerate {
		manifests, err = serverSideGenerate(ctx, cl, app, o.local, o.localIncludes)
		if err != nil {
			return nil, err
		}
		objs, err := manifestsToObjects(manifests)
		if err != nil {
			return nil, err
		}
		for _, obj := range objs {
			if obj.GroupVersionKind().Group == "" && obj.GetKind() == "Secret" {
				return nil, fmt.Errorf("cannot sync Secret %s/%s rendered on the server: its data is masked; render locally (without --server-side-generate) instead",
					obj.GetNamespace(), obj.GetName())
			}
		}
	} else {
		manifests, err = e.renderLocal(ctx, cl, app, settings, o.local, o.localRepoRoot)
		if err != nil {
			return nil, err
		}
	}
	if len(manifests) > maxLocalManifests {
		return nil, fmt.Errorf("local sync supports at most %d manifests, got %d", maxLocalManifests, len(manifests))
	}
	total := 0
	for _, m := range manifests {
		total += len(m)
	}
	if total > maxLocalManifestsBytes {
		return nil, fmt.Errorf("local manifests exceed %d bytes (%d)", maxLocalManifestsBytes, total)
	}
	return manifests, nil
}

// previewSync prints the differences the sync would apply and asks for confirmation.
func previewSync(ctx context.Context, c *cobra.Command, e *env, cl Client, o *syncOptions, settings *apiclient.Settings,
	app *v1alpha1.Application, localManifests []string, positions []int64,
) (bool, error) {
	out := c.OutOrStdout()
	ref := AppRef{Name: app.Name, Namespace: app.Namespace}
	managed, err := managedResources(ctx, cl, ref)
	if err != nil {
		return false, err
	}
	_, _ = fmt.Fprintf(out, "====== Previewing differences between live and desired state of application %s ======\n", app.QualifiedName())
	req := &diffRequest{app: app, managed: managed, settings: settings, serverSide: serverSideDiffAnnotation(app),
		concurrency: o.serverSideDiffConcurrency, maxBatchKB: o.serverSideDiffMaxBatchKB, jqTimeout: o.jqTimeout, warn: c.ErrOrStderr()}
	var manifests []string
	switch {
	case len(localManifests) > 0:
		manifests = localManifests
	case len(o.revisions) > 0:
		res, err := cl.GetApplicationManifests(ctx, &apiclient.ApplicationManifestQuery{Name: app.Name, AppNamespace: app.Namespace, Revisions: o.revisions, SourcePositions: positions})
		if err != nil {
			return false, err
		}
		manifests = res.Manifests
	case o.revision != "":
		res, err := cl.GetApplicationManifests(ctx, &apiclient.ApplicationManifestQuery{Name: app.Name, AppNamespace: app.Namespace, Revision: o.revision})
		if err != nil {
			return false, err
		}
		manifests = res.Manifests
	}
	if manifests != nil {
		if req.targets, err = manifestsToObjects(manifests); err != nil {
			return false, err
		}
		req.excludeSecrets = true
	}
	changed, err := e.computeDiff(ctx, cl, req)
	if err != nil {
		return false, err
	}
	if err := e.printDiffs(ctx, out, c.ErrOrStderr(), changed); err != nil {
		return false, err
	}
	if len(changed) == 0 {
		_, _ = fmt.Fprint(out, "====== No Differences found ======\n")
		return false, nil
	}
	if o.assumeYes {
		return true, nil
	}
	return e.prompter().Confirm(fmt.Sprintf("Please review changes to application %s shown above. Do you want to continue the sync process? (y/n): ", app.QualifiedName()))
}
