package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	pdiff "github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/sync"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Defaults of the diff flags.
const (
	defaultServerSideDiffConcurrency = -1
	defaultServerSideDiffMaxBatchKB  = 250
	defaultJQTimeout                 = time.Second
)

// resourceKey identifies a resource independently of its version.
type resourceKey struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

func (k resourceKey) String() string {
	return k.Group + "/" + k.Kind + "/" + k.Namespace + "/" + k.Name
}

func objectKey(obj *unstructured.Unstructured) resourceKey {
	gvk := obj.GroupVersionKind()
	return resourceKey{Group: gvk.Group, Kind: gvk.Kind, Namespace: obj.GetNamespace(), Name: obj.GetName()}
}

// comparison is one changed resource.
type comparison struct {
	key    resourceKey
	live   *unstructured.Unstructured
	target *unstructured.Unstructured
}

// diffRequest describes a comparison between live and desired state.
type diffRequest struct {
	app      *v1alpha1.Application
	managed  []apiclient.ResourceDiff
	settings *apiclient.Settings
	// targets are the desired objects; nil uses the target states of managed.
	targets []*unstructured.Unstructured
	// excludeSecrets drops core Secrets from both sides (local and revision targets cannot
	// be masked consistently with the live state).
	excludeSecrets bool
	serverSide     bool
	concurrency    int
	maxBatchKB     int
	jqTimeout      time.Duration
	warn           io.Writer
}

type diffOptions struct {
	refresh                   bool
	hardRefresh               bool
	exitCode                  bool
	diffExitCode              int
	local                     string
	revision                  string
	localRepoRoot             string
	serverSideGenerate        bool
	serverSideDiff            bool
	serverSideDiffConcurrency int
	serverSideDiffMaxBatchKB  int
	localIncludes             []string
	appNamespace              string
	revisions                 []string
	sourcePositions           []int64
	sourceNames               []string
	jqTimeout                 time.Duration
}

func newDiffCommand(e *env) *cobra.Command {
	o := &diffOptions{}
	short := "Perform a diff against the target and live state."
	c := &cobra.Command{
		Use:   "diff APPNAME",
		Short: short,
		Long: short + "\nPrints a unified diff of the normalized live state and the desired state of every changed resource. " +
			"PATROL_DIFF_PROGRAM (or KUBECTL_EXTERNAL_DIFF) selects an external diff program, called with the live and target files.\n" +
			"Returns the following exit codes: 2 on general errors, 1 when a diff is found, and 0 when no diff is found\n" +
			"Secret data is never shown: the server masks it, and Secrets are left out when comparing with local or revision manifests.",
		RunE: func(c *cobra.Command, args []string) error {
			err := runDiff(c, e, o, args)
			return withExitCode(err, exitDiffError)
		},
	}
	fs := c.Flags()
	fs.BoolVar(&o.refresh, "refresh", false, "Refresh application data when retrieving")
	fs.BoolVar(&o.hardRefresh, "hard-refresh", false, "Refresh application data as well as target manifests cache")
	fs.BoolVar(&o.exitCode, "exit-code", true, "Return non-zero exit code when there is a diff. May also return non-zero exit code if there is an error.")
	fs.IntVar(&o.diffExitCode, "diff-exit-code", exitDiffFound, "Return specified exit code when there is a diff. Typical error code is 20 but use another exit code if you want to differentiate from the generic exit code (20) returned by all CLI commands.")
	fs.StringVar(&o.local, "local", "", "Compare live app to a local manifests")
	fs.StringVar(&o.revision, "revision", "", "Compare live app to a particular revision")
	fs.StringVar(&o.localRepoRoot, "local-repo-root", "/", "Path to the repository root. Used together with --local allows setting the repository root")
	fs.BoolVar(&o.serverSideGenerate, "server-side-generate", false, "Used with --local, this will send your manifests to the server for diffing")
	fs.BoolVar(&o.serverSideDiff, "server-side-diff", false, "Use server-side diff to calculate the diff. This will default to true if the ServerSideDiff annotation is set on the application.")
	addServerSideDiffPerfFlags(c, &o.serverSideDiffConcurrency, &o.serverSideDiffMaxBatchKB)
	fs.StringArrayVar(&o.localIncludes, "local-include", defaultLocalIncludes, "Used with --server-side-generate, specify patterns of filenames to send. Matching is based on filename and not path.")
	fs.StringVarP(&o.appNamespace, "app-namespace", "N", "", "Only render the difference in namespace")
	fs.StringArrayVar(&o.revisions, "revisions", []string{}, "Show manifests at specific revisions for source position in source-positions")
	fs.Int64SliceVar(&o.sourcePositions, "source-positions", []int64{}, "List of source positions. Default is empty array. Counting start at 1.")
	fs.StringArrayVar(&o.sourceNames, "source-names", []string{}, "List of source names. Default is an empty array.")
	fs.DurationVar(&o.jqTimeout, "ignore-normalizer-jq-execution-timeout", defaultJQTimeout, "Set ignore normalizer JQ execution timeout")
	return c
}

func addServerSideDiffPerfFlags(c *cobra.Command, concurrency, maxBatchKB *int) {
	c.Flags().IntVar(concurrency, "server-side-diff-concurrency", defaultServerSideDiffConcurrency, "Max concurrent batches for server-side diff. -1 = unlimited, 1 = sequential, 2+ = concurrent (0 = invalid)")
	c.Flags().IntVar(maxBatchKB, "server-side-diff-max-batch-kb", defaultServerSideDiffMaxBatchKB, "Max batch size in KB for server-side diff. Smaller values are safer for proxies")
}

func runDiff(c *cobra.Command, e *env, o *diffOptions, args []string) error {
	ctx := cmdContext(c)
	if len(args) != 1 {
		c.HelpFunc()(c, args)
		return silentExit(exitDiffError)
	}
	if err := validateRevisionFlags(o.revisions, o.sourcePositions, o.sourceNames); err != nil {
		return err
	}
	if o.serverSideDiffConcurrency == 0 {
		return fatalGeneric("invalid value for --server-side-diff-concurrency: 0 is not allowed (use -1 for unlimited, or a positive number to limit concurrency)")
	}
	if o.serverSideDiffMaxBatchKB <= 0 {
		return fatalGeneric("invalid value for --server-side-diff-max-batch-kb: must be positive")
	}
	cl, err := e.client(ctx)
	if err != nil {
		return err
	}
	name, ns := parseQualifiedName(args[0], o.appNamespace)
	app, err := getApp(ctx, cl, name, ns, refreshType(o.refresh, o.hardRefresh))
	if err != nil {
		return err
	}
	positions := o.sourcePositions
	if len(o.sourceNames) > 0 {
		if positions, err = resolveSourceNames(app, o.sourceNames); err != nil {
			return err
		}
	}
	ref := AppRef{Name: app.Name, Namespace: app.Namespace}
	managed, err := managedResources(ctx, cl, ref)
	if err != nil {
		return err
	}
	settings, err := cl.GetSettings(ctx)
	if err != nil {
		return err
	}
	errW := c.ErrOrStderr()
	hasAnnotation := serverSideDiffAnnotation(app)
	serverSide := o.serverSideDiff
	if !c.Flags().Changed("server-side-diff") {
		serverSide = hasAnnotation
	} else if serverSide && !hasAnnotation {
		_, _ = fmt.Fprint(errW, "Warning: Application does not have ServerSideDiff=true annotation.\n")
	}
	if serverSide && o.local != "" && !o.serverSideGenerate {
		return fatal("--server-side-diff with --local requires --server-side-generate.")
	}
	req := &diffRequest{
		app: app, managed: managed, settings: settings, serverSide: serverSide,
		concurrency: o.serverSideDiffConcurrency, maxBatchKB: o.serverSideDiffMaxBatchKB, jqTimeout: o.jqTimeout, warn: errW,
	}
	switch {
	case app.Spec.HasMultipleSources() && len(o.revisions) > 0 && len(positions) > 0:
		n := int64(len(app.Spec.GetSources()))
		for _, p := range positions {
			if p <= 0 || p > n {
				return fatal("source-position cannot be less than 1 or more than number of sources in the app. Counting starts at 1.")
			}
		}
		res, err := cl.GetApplicationManifests(ctx, &apiclient.ApplicationManifestQuery{
			Name: app.Name, AppNamespace: app.Namespace, Revisions: o.revisions, SourcePositions: positions, NoCache: o.hardRefresh,
		})
		if err != nil {
			return err
		}
		if req.targets, err = manifestsToObjects(res.Manifests); err != nil {
			return err
		}
		req.excludeSecrets = true
	case o.revision != "":
		res, err := cl.GetApplicationManifests(ctx, &apiclient.ApplicationManifestQuery{
			Name: app.Name, AppNamespace: app.Namespace, Revision: o.revision, NoCache: o.hardRefresh,
		})
		if err != nil {
			return err
		}
		if req.targets, err = manifestsToObjects(res.Manifests); err != nil {
			return err
		}
		req.excludeSecrets = true
	case o.local != "":
		var manifests []string
		if o.serverSideGenerate {
			manifests, err = serverSideGenerate(ctx, cl, app, o.local, o.localIncludes)
		} else {
			_, _ = fmt.Fprint(errW, "Warning: local diff without --server-side-generate is deprecated and does not work with plugins.\n")
			manifests, err = e.renderLocal(ctx, cl, app, settings, o.local, o.localRepoRoot)
		}
		if err != nil {
			return err
		}
		if req.targets, err = manifestsToObjects(manifests); err != nil {
			return err
		}
		req.excludeSecrets = true
	}
	changed, err := e.computeDiff(ctx, cl, req)
	if err != nil {
		return err
	}
	out := c.OutOrStdout()
	for _, ch := range changed {
		if err := e.printResourceDiff(ctx, out, errW, ch.key, ch.live, ch.target); err != nil {
			return err
		}
	}
	if len(changed) > 0 && o.exitCode {
		return silentExit(o.diffExitCode)
	}
	return nil
}

// serverSideDiffAnnotation reports the ServerSideDiff=true compare option on the Application.
func serverSideDiffAnnotation(app *v1alpha1.Application) bool {
	c := pdiff.ParseCompareAnnotation(app.Annotations, v1alpha1.AnnotationKeyCompareOptions)
	return c.ServerSideDiff != nil && *c.ServerSideDiff
}

// serverSideGenerate archives dir and renders it on the server with the Application's
// source settings (POST .../manifests-with-files).
func serverSideGenerate(ctx context.Context, cl Client, app *v1alpha1.Application, dir string, includes []string) ([]string, error) {
	archive, err := buildLocalArchive(dir, includes)
	if err != nil {
		return nil, fmt.Errorf("failed to compress files: %w", err)
	}
	defer archive.Remove()
	f, err := archive.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	res, err := cl.GetApplicationManifestsWithFiles(ctx, &apiclient.ApplicationManifestsWithFilesQuery{
		Name: app.Name, AppNamespace: app.Namespace, Checksum: archive.checksum,
	}, f)
	if err != nil {
		return nil, err
	}
	return res.Manifests, nil
}

// computeDiff returns the changed resources, sorted by key (upstream findAndPrintDiff
// without printing).
func (e *env) computeDiff(ctx context.Context, cl Client, req *diffRequest) ([]comparison, error) {
	namespaced := map[schema.GroupKind]bool{}
	for _, m := range req.managed {
		namespaced[schema.GroupKind{Group: m.Group, Kind: m.Kind}] = m.Namespace != ""
	}
	targets := req.targets
	if targets == nil {
		for _, m := range req.managed {
			obj, err := unmarshalObject(m.TargetState)
			if err != nil {
				return nil, err
			}
			if obj != nil {
				targets = append(targets, obj)
			}
		}
	}
	targets, err := normalizeTargets(req, targets, namespaced)
	if err != nil {
		return nil, err
	}
	var lives []*unstructured.Unstructured
	for _, m := range req.managed {
		if req.excludeSecrets && m.Group == "" && m.Kind == "Secret" {
			continue
		}
		state := m.NormalizedLiveState
		if state == "" {
			state = m.LiveState
		}
		obj, err := unmarshalObject(state)
		if err != nil {
			return nil, err
		}
		if obj != nil {
			lives = append(lives, obj)
		}
	}
	items := pairObjects(targets, lives, req.excludeSecrets, req.warn)
	var results []pdiff.Result
	if req.serverSide {
		results, err = serverSideDiffItems(ctx, cl, req, items)
	} else {
		results, err = clientSideDiffItems(ctx, req, items)
	}
	if err != nil {
		return nil, err
	}
	var out []comparison
	for _, r := range results {
		liveState, targetState := string(r.NormalizedLive), string(r.PredictedLive)
		hasLive := liveState != "" && liveState != "null"
		hasTarget := targetState != "" && targetState != "null"
		if (!r.Modified && hasLive && hasTarget) || (!hasLive && !hasTarget) {
			continue
		}
		live, err := unmarshalObject(liveState)
		if err != nil {
			return nil, err
		}
		target, err := unmarshalObject(targetState)
		if err != nil {
			return nil, err
		}
		var key resourceKey
		if live != nil {
			key = objectKey(live)
		} else {
			key = objectKey(target)
		}
		out = append(out, comparison{key: key, live: live, target: target})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key.String() < out[j].key.String() })
	return out, nil
}

// normalizeTargets sets the destination namespace on namespaced objects without one, clears
// it on cluster-scoped objects, re-applies tracking metadata to modified objects and drops
// duplicates (last wins; upstream NormalizeTargetObjects). Kinds without a live object are
// left as they are (their scope is unknown to the client).
func normalizeTargets(req *diffRequest, targets []*unstructured.Unstructured, namespaced map[schema.GroupKind]bool) ([]*unstructured.Unstructured, error) {
	st := req.settings
	if st == nil {
		st = &apiclient.Settings{}
	}
	tracker := tracking.New(tracking.Config{InstanceLabelKey: st.AppLabelKey, ControlPlaneNamespace: st.ControllerNamespace})
	method := tracking.EffectiveMethod(v1alpha1.TrackingMethod(st.TrackingMethod))
	instance := tracking.InstanceName(req.app.Name, req.app.Namespace, st.ControllerNamespace)
	destNS := req.app.Spec.Destination.Namespace
	byKey := map[resourceKey][]*unstructured.Unstructured{}
	var order []resourceKey
	for i, obj := range targets {
		if obj == nil {
			continue
		}
		modified := false
		isNamespaced, known := namespaced[obj.GroupVersionKind().GroupKind()]
		switch {
		case known && !isNamespaced && obj.GetNamespace() != "":
			obj.SetNamespace("")
			modified = true
		case known && isNamespaced && obj.GetNamespace() == "":
			obj.SetNamespace(destNS)
			modified = true
		}
		if modified {
			if err := tracker.SetAppInstance(obj, instance, destNS, method, st.InstallationID); err != nil {
				return nil, fmt.Errorf("failed to set app instance label on resource %s/%s: %w", obj.GetKind(), obj.GetName(), err)
			}
		}
		key := objectKey(obj)
		if key.Name == "" && obj.GetGenerateName() != "" {
			key.Name = fmt.Sprintf("%s%d", obj.GetGenerateName(), i)
		}
		if _, ok := byKey[key]; !ok {
			order = append(order, key)
		}
		byKey[key] = append(byKey[key], obj)
	}
	out := make([]*unstructured.Unstructured, 0, len(order))
	for _, k := range order {
		objs := byKey[k]
		if len(objs) > 1 && req.warn != nil {
			output.Printf(req.warn, "Warning: %s: Resource %s appeared %d times among application resources.\n",
				v1alpha1.ApplicationConditionRepeatedResourceWarning, k.String(), len(objs))
		}
		out = append(out, objs[len(objs)-1])
	}
	return out, nil
}

// pairObjects pairs targets with live objects by key, skipping hooks (and, with
// excludeSecrets, core Secrets).
func pairObjects(targets, lives []*unstructured.Unstructured, excludeSecrets bool, warn io.Writer) []comparison {
	keep := func(obj *unstructured.Unstructured, isTarget bool) bool {
		if obj == nil || sync.IsHook(obj) {
			return false
		}
		if excludeSecrets && obj.GroupVersionKind().Group == "" && obj.GetKind() == "Secret" {
			if isTarget && warn != nil {
				output.Printf(warn, "Warning: Secret %s/%s is not supported in local diff and will be ignored\n", obj.GetNamespace(), obj.GetName())
			}
			return false
		}
		return true
	}
	targetByKey := map[resourceKey]*unstructured.Unstructured{}
	var targetOrder []resourceKey
	for _, t := range targets {
		if !keep(t, true) {
			continue
		}
		k := objectKey(t)
		if _, ok := targetByKey[k]; !ok {
			targetOrder = append(targetOrder, k)
		}
		targetByKey[k] = t
	}
	var items []comparison
	seen := map[resourceKey]bool{}
	for _, l := range lives {
		if !keep(l, false) {
			continue
		}
		k := objectKey(l)
		if seen[k] {
			continue
		}
		seen[k] = true
		items = append(items, comparison{key: k, live: l, target: targetByKey[k]})
	}
	for _, k := range targetOrder {
		if !seen[k] {
			items = append(items, comparison{key: k, target: targetByKey[k]})
		}
	}
	return items
}

// clientSideDiffItems diffs every pair locally with the Application's ignoreDifferences and
// the server's resource overrides.
func clientSideDiffItems(ctx context.Context, req *diffRequest, items []comparison) ([]pdiff.Result, error) {
	rules := make([]pdiff.IgnoreRule, 0, len(req.app.Spec.IgnoreDifferences))
	for _, d := range req.app.Spec.IgnoreDifferences {
		rules = append(rules, pdiff.IgnoreRule{
			Group: d.Group, Kind: d.Kind, Name: d.Name, Namespace: d.Namespace,
			JSONPointers: d.JSONPointers, JQPathExpressions: d.JQPathExpressions, ManagedFieldsManagers: d.ManagedFieldsManagers,
		})
	}
	var systemRules []pdiff.IgnoreRule
	if req.settings != nil {
		keys := make([]string, 0, len(req.settings.ResourceOverrides))
		for k := range req.settings.ResourceOverrides {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			o := req.settings.ResourceOverrides[k]
			if o.IgnoreDifferences == nil {
				continue
			}
			r, err := pdiff.SystemRule(k, o.IgnoreDifferences.JSONPointers, o.IgnoreDifferences.JQPathExpressions, o.IgnoreDifferences.ManagedFieldsManagers)
			if err != nil {
				continue
			}
			systemRules = append(systemRules, r)
		}
	}
	rules = append(rules, pdiff.SystemIgnoreRules(systemRules, pdiff.DefaultCompareOptions())...)
	normalizer, err := pdiff.NewNormalizer(pdiff.NormalizerConfig{Rules: rules, Ignore: pdiff.IgnoreNormalizerOptions{JQTimeout: req.jqTimeout}})
	if normalizer == nil {
		return nil, fmt.Errorf("build diff normalizer: %w", err)
	}
	opts := pdiff.DefaultOptions()
	opts.Normalizer = normalizer
	opts.StructuredMerge = req.app.Spec.SyncPolicy != nil && req.app.Spec.SyncPolicy.SyncOptions.IsOptionTrue(v1alpha1.SyncOptionKeyServerSideApply)
	if req.settings != nil && tracking.EffectiveMethod(v1alpha1.TrackingMethod(req.settings.TrackingMethod)) != v1alpha1.TrackingMethodLabel {
		labelKey := req.settings.AppLabelKey
		if labelKey == "" {
			labelKey = v1alpha1.LabelKeyAppInstanceDefault
		}
		opts.Tracking = pdiff.TrackingOptions{LabelKey: labelKey, AnnotationKey: v1alpha1.AnnotationKeyTrackingID}
	}
	out := make([]pdiff.Result, 0, len(items))
	for _, it := range items {
		r, err := pdiff.Diff(ctx, it.target, it.live, opts)
		if err != nil {
			return nil, fmt.Errorf("diff %s: %w", it.key.String(), err)
		}
		out = append(out, *r)
	}
	return out, nil
}

// serverSideDiffItems sends the pairs to the server-side diff route in batches of at most
// maxBatchKB, with bounded concurrency.
func serverSideDiffItems(ctx context.Context, cl Client, req *diffRequest, items []comparison) ([]pdiff.Result, error) {
	if len(items) == 0 {
		return nil, nil
	}
	live := make([]apiclient.ResourceDiff, len(items))
	targets := make([]string, len(items))
	for i, it := range items {
		live[i] = apiclient.ResourceDiff{Group: it.key.Group, Kind: it.key.Kind, Namespace: it.key.Namespace, Name: it.key.Name}
		if it.live != nil {
			data, err := json.Marshal(it.live.Object)
			if err != nil {
				return nil, fmt.Errorf("error marshaling live object: %w", err)
			}
			live[i].LiveState = string(data)
		}
		if it.target != nil {
			data, err := json.Marshal(it.target.Object)
			if err != nil {
				return nil, fmt.Errorf("error marshaling target object: %w", err)
			}
			targets[i] = string(data)
		}
	}
	type batch struct{ start, end int }
	var batches []batch
	maxBytes := req.maxBatchKB * 1024
	for i := 0; i < len(items); {
		start, size := i, 0
		for i < len(items) {
			n := len(live[i].LiveState) + len(targets[i])
			if size+n > maxBytes && i > start {
				break
			}
			size += n
			i++
		}
		batches = append(batches, batch{start, i})
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(req.concurrency)
	results := make([][]apiclient.ResourceDiff, len(batches))
	for i, b := range batches {
		g.Go(func() error {
			res, err := cl.ApplicationServerSideDiff(gctx, &apiclient.ApplicationServerSideDiffRequest{
				Name: req.app.Name, AppNamespace: req.app.Namespace, Project: req.app.Spec.Project,
				LiveResources: live[b.start:b.end], TargetManifests: targets[b.start:b.end],
			})
			if err != nil {
				return err
			}
			results[i] = res.Items
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	var out []pdiff.Result
	for _, rs := range results {
		for _, r := range rs {
			out = append(out, pdiff.Result{Modified: r.Modified, NormalizedLive: []byte(r.LiveState), PredictedLive: []byte(r.TargetState)})
		}
	}
	return out, nil
}

// printDiffs prints the changed resources.
func (e *env) printDiffs(ctx context.Context, w, errW io.Writer, changed []comparison) error {
	for _, ch := range changed {
		if err := e.printResourceDiff(ctx, w, errW, ch.key, ch.live, ch.target); err != nil {
			return err
		}
	}
	return nil
}
