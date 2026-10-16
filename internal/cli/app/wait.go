package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// watchOpts are the conditions `app wait` waits for.
type watchOpts struct {
	sync      bool
	health    bool
	operation bool
	suspended bool
	degraded  bool
	delete    bool
	hydrated  bool
}

// withDefaults waits for sync, health and operation when nothing was selected.
func (w watchOpts) withDefaults() watchOpts {
	if w == (watchOpts{}) {
		return watchOpts{sync: true, health: true, operation: true}
	}
	return w
}

// operationsToInspect bounds the Operations listed to find one still in progress.
const operationsToInspect = 10

func newWaitCommand(e *env) *cobra.Command {
	var (
		watch        watchOpts
		timeout      uint
		selector     string
		resources    []string
		output       string
		appNamespace string
	)
	c := &cobra.Command{
		Use:   "wait [APPNAME.. | -l selector]",
		Short: "Wait for an application to reach a synced and healthy state",
		Example: `  # Wait for an app
  patrol app wait my-app

  # Wait for multiple apps
  patrol app wait my-app other-app

  # Wait for apps by resource
  # Resource should be formatted as GROUP:KIND:NAME. If no GROUP is specified then :KIND:NAME.
  patrol app wait my-app --resource :Service:my-service
  patrol app wait my-app --resource argoproj.io:Rollout:my-rollout
  patrol app wait my-app --resource '!apps:Deployment:my-service'
  patrol app wait my-app --resource apps:Deployment:my-service --resource :Service:my-service
  patrol app wait my-app --resource '!*:Service:*'
  # Specify namespace if the application has resources with the same name in different namespaces
  patrol app wait my-app --resource argoproj.io:Rollout:my-namespace/my-rollout

  # Wait for apps by label, in this example we waiting for apps that are children of another app (aka app-of-apps)
  patrol app wait -l app.kubernetes.io/instance=my-app
  patrol app wait -l app.kubernetes.io/instance!=my-app
  patrol app wait -l app.kubernetes.io/instance
  patrol app wait -l '!app.kubernetes.io/instance'
  patrol app wait -l 'app.kubernetes.io/instance notin (my-app,other-app)'`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) == 0 && selector == "" {
				return usage(c, args)
			}
			if err := validateWaitOutput(output); err != nil {
				return err
			}
			selectors, err := parseResourceSelectors(resources)
			if err != nil {
				return err
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			names := append([]string{}, args...)
			if selector != "" {
				apps, err := listAllApps(ctx, cl, apiclient.ApplicationQuery{Selector: selector})
				if err != nil {
					return err
				}
				for i := range apps {
					names = append(names, apps[i].QualifiedName())
				}
			}
			for _, full := range names {
				name, ns := parseQualifiedName(full, appNamespace)
				w := &waiter{e: e, cl: cl, out: c.OutOrStdout(), name: name, namespace: ns, timeout: time.Duration(timeout) * time.Second,
					watch: watch.withDefaults(), selectors: selectors, output: output}
				if _, err := w.wait(ctx); err != nil {
					if errors.Is(err, errWaitTimeout) {
						return fatal("timed out (%ds) waiting for app %q to match the expected conditions", timeout, qualify(name, ns))
					}
					return err
				}
			}
			return nil
		},
	}
	fs := c.Flags()
	fs.BoolVar(&watch.sync, "sync", false, "Wait for sync")
	fs.BoolVar(&watch.health, "health", false, "Wait for health")
	fs.BoolVar(&watch.suspended, "suspended", false, "Wait for suspended")
	fs.BoolVar(&watch.degraded, "degraded", false, "Wait for degraded")
	fs.BoolVar(&watch.delete, "delete", false, "Wait for delete")
	fs.BoolVar(&watch.hydrated, "hydrated", false, "Wait for hydration operations")
	fs.StringVarP(&selector, "selector", "l", "", "Wait for apps by label. Supports '=', '==', '!=', in, notin, exists & not exists. Matching apps must satisfy all of the specified label constraints.")
	fs.StringArrayVar(&resources, "resource", []string{}, "Sync only specific resources as GROUP:KIND:NAME or !GROUP:KIND:NAME. Fields may be blank and '*' can be used. This option may be specified repeatedly")
	fs.BoolVar(&watch.operation, "operation", false, "Wait for pending operations")
	fs.UintVar(&timeout, "timeout", 0, "Time out after this many seconds")
	fs.StringVarP(&appNamespace, "app-namespace", "N", "", "Only wait for an application  in namespace")
	fs.StringVarP(&output, "output", "o", outWide, "Output format. One of: json|yaml|wide|tree|tree=detailed")
	return c
}

func validateWaitOutput(format string) error {
	switch format {
	case outJSON, outYAML, outWide, "", outTree, outTreeDetailed:
		return nil
	}
	return fmt.Errorf("unknown output format: %s", format)
}

// errWaitTimeout is returned when the timeout elapsed before the conditions held.
var errWaitTimeout = errors.New("timed out waiting for the application")

// waiter waits for one Application to meet watch conditions, printing resource state
// changes as they happen (upstream waitOnApplicationStatus).
type waiter struct {
	e         *env
	cl        Client
	out       io.Writer
	name      string
	namespace string
	timeout   time.Duration
	watch     watchOpts
	selectors []resourceSelector
	output    string
	// operation, when set, is the Operation this wait follows (created by sync/rollback).
	operation string
}

// waitResult is the outcome of a wait.
type waitResult struct {
	// App is the final state (nil after --delete).
	App *v1alpha1.Application
	// Operation is the followed (or latest) Operation when the conditions held.
	Operation *v1alpha1.Operation
}

// opStatus is what the waiter knows about the Application's operations.
type opStatus struct {
	inProgress bool
	latest     *v1alpha1.Operation
}

func (w *waiter) printSummary() bool { return w.output != outJSON && w.output != outYAML }

func (w *waiter) wait(ctx context.Context) (*waitResult, error) {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var fired atomic.Bool
	if w.timeout > 0 {
		t := time.AfterFunc(w.timeout, func() {
			fired.Store(true)
			cancel()
		})
		defer t.Stop()
	}
	timedOut := func() bool { return fired.Load() }

	tw := tabwriter.NewWriter(w.out, 5, 0, 2, ' ', 0)
	if w.printSummary() {
		_, _ = fmt.Fprintf(tw, waitFormat, "TIMESTAMP", "GROUP", "KIND", "NAMESPACE", "NAME", "STATUS", "HEALTH", "HOOK", "MESSAGE")
	}
	prev := map[string]*resourceState{}
	sawOperation := false
	var result *waitResult
	var latest *v1alpha1.Application

	evaluate := func(app *v1alpha1.Application) (bool, error) {
		latest = app
		ops, err := w.operationStatus(wctx, app)
		if err != nil {
			return false, err
		}
		if ops.latest != nil && !ops.latest.IsDryRun() && (ops.inProgress || w.operation != "") {
			sawOperation = true
		}
		ready := w.conditionsMet(app)
		if ready && (!ops.inProgress || !w.watch.operation) {
			result = &waitResult{App: app, Operation: ops.latest}
			return true, nil
		}
		var results []v1alpha1.ResourceResult
		if ops.latest != nil && ops.latest.Status.SyncResult != nil {
			results = ops.latest.Status.SyncResult.Resources
		}
		for _, n := range groupResourceStates(app, results, w.selectors) {
			key := n.key()
			p, found := prev[key]
			doPrint := true
			if found {
				if w.watch.health && p.Health != string(v1alpha1.HealthStatusUnknown) && p.Health != string(v1alpha1.HealthStatusDegraded) &&
					n.Health == string(v1alpha1.HealthStatusDegraded) {
					_ = tw.Flush()
					_, _ = w.printFinal(ctx, app, false, ops.latest)
					return true, fmt.Errorf("application '%s' health state has transitioned from %s to %s", qualify(w.name, w.namespace), p.Health, n.Health)
				}
				doPrint = p.merge(n)
			} else {
				prev[key] = n
				p = n
			}
			if doPrint && w.printSummary() {
				_, _ = fmt.Fprintf(tw, waitFormat, w.e.now().Format("2006-01-02T15:04:05-07:00"), output.Sanitize(p.Group), output.Sanitize(p.Kind),
					output.Sanitize(p.Namespace), output.Sanitize(p.Name), output.Sanitize(p.Status), output.Sanitize(p.Health), output.Sanitize(p.Hook), output.Sanitize(p.Message))
			}
		}
		_ = tw.Flush()
		return false, nil
	}

	app, err := getApp(wctx, w.cl, w.name, w.namespace, "")
	if err != nil {
		if w.watch.delete && isNotFoundOrDenied(err) {
			_, _ = fmt.Fprintf(w.out, "Application '%s' deleted\n", qualify(w.name, w.namespace))
			return &waitResult{}, nil
		}
		return nil, w.timeoutErr(timedOut(), err)
	}
	if !w.watch.delete {
		done, err := evaluate(app)
		if err != nil {
			return nil, w.timeoutErr(timedOut(), err)
		}
		if done {
			final, err := w.printFinal(ctx, result.App, sawOperation, result.Operation)
			if err != nil {
				return nil, err
			}
			result.App = final
			return result, nil
		}
	}
	watcher := &appWatcher{e: w.e, cl: w.cl, name: w.name, namespace: w.namespace, checkGone: w.watch.delete}
	deleted := false
	err = watcher.run(wctx, func(eventType string, app *v1alpha1.Application) (bool, error) {
		if eventType == apiclient.EventDeleted {
			if w.watch.delete {
				deleted = true
				return true, nil
			}
			latest = app
			return false, nil
		}
		if w.watch.delete {
			latest = app
			return false, nil
		}
		return evaluate(app)
	})
	_ = tw.Flush()
	if deleted || (w.watch.delete && isNotFoundOrDenied(err)) {
		_, _ = fmt.Fprintf(w.out, "Application '%s' deleted\n", qualify(w.name, w.namespace))
		return &waitResult{}, nil
	}
	if err != nil {
		if timedOut() {
			if latest != nil {
				w.printTimedOut(ctx, latest)
			}
			return nil, errWaitTimeout
		}
		return nil, err
	}
	if result == nil {
		if latest != nil && w.printSummary() {
			_, _ = w.printFinal(ctx, latest, false, nil)
		}
		return nil, fmt.Errorf("timed out (%ds) waiting for app %q match desired state", int(w.timeout.Seconds()), qualify(w.name, w.namespace))
	}
	final, err := w.printFinal(ctx, result.App, sawOperation, result.Operation)
	if err != nil {
		return nil, err
	}
	result.App = final
	return result, nil
}

func (w *waiter) timeoutErr(timedOut bool, err error) error {
	if timedOut {
		return errWaitTimeout
	}
	return err
}

// printTimedOut prints the state after a timeout (upstream timeout handler).
func (w *waiter) printTimedOut(ctx context.Context, app *v1alpha1.Application) {
	if !w.printSummary() {
		return
	}
	_, _ = fmt.Fprintln(w.out)
	_, _ = fmt.Fprintln(w.out, "This is the state of the app after wait timed out:")
	if fresh, err := getApp(ctx, w.cl, w.name, w.namespace, ""); err == nil {
		app = fresh
	}
	_, _ = w.printFinal(ctx, app, false, nil)
	_, _ = fmt.Fprintln(w.out)
	_, _ = fmt.Fprintln(w.out, "The command timed out waiting for the conditions to be met.")
}

// waitFormat is the progress row layout (upstream waitFormatString).
const waitFormat = "%s\t%5s\t%10s\t%10s\t%20s\t%8s\t%7s\t%10s\t%s\n"

// conditionsMet evaluates the sync, health and hydration conditions (upstream
// checkAppWaitConditions without the operation part).
func (w *waiter) conditionsMet(app *v1alpha1.Application) bool {
	hydrated := hydrationFinished(app)
	if len(w.selectors) > 0 {
		for _, s := range resourceStates(app, nil, w.selectors) {
			if !checkResourceStatus(w.watch, s.Health, s.Status, hydrated) {
				return false
			}
		}
		return true
	}
	return checkResourceStatus(w.watch, string(app.Status.Health.Status), string(app.Status.Sync.Status), hydrated)
}

// checkResourceStatus reports whether health and sync satisfy the watch options (health
// options are OR-ed).
func checkResourceStatus(w watchOpts, health, sync string, hydrated bool) bool {
	if w.delete {
		return false
	}
	healthOK := true
	if w.health || w.suspended || w.degraded {
		healthOK = w.health && health == string(v1alpha1.HealthStatusHealthy) ||
			w.suspended && health == string(v1alpha1.HealthStatusSuspended) ||
			w.degraded && health == string(v1alpha1.HealthStatusDegraded)
	}
	synced := !w.sync || sync == string(v1alpha1.SyncStatusCodeSynced)
	hydratedOK := !w.hydrated || hydrated
	return synced && healthOK && hydratedOK
}

// hydrationFinished reports a completed hydration of the current dry revision.
func hydrationFinished(app *v1alpha1.Application) bool {
	cur, last := app.Status.SourceHydrator.CurrentOperation, app.Status.SourceHydrator.LastSuccessfulOperation
	return cur != nil && last != nil && cur.Phase == v1alpha1.HydrateOperationPhaseHydrated &&
		reflect.DeepEqual(cur.SourceHydrator, last.SourceHydrator) && cur.DrySHA == last.DrySHA
}

// operationStatus finds out whether an operation is in progress. Patrol keeps operations in
// Operation objects: one is in progress while it is not terminal, and a finished non-dry-run
// operation stays "in progress" until the controller reconciled the Application after it
// (upstream waits for reconciledAt >= finishedAt). When the wait follows a specific
// Operation, only that one counts.
func (w *waiter) operationStatus(ctx context.Context, app *v1alpha1.Application) (opStatus, error) {
	var st opStatus
	if w.operation != "" {
		op, err := w.cl.GetApplicationOperation(ctx, &apiclient.ApplicationOperationRequest{Name: app.Name, AppNamespace: app.Namespace, Operation: w.operation})
		if err != nil {
			return st, err
		}
		st.latest = op
		st.inProgress = !op.IsTerminal() || needsReconcile(app, op)
		return st, nil
	}
	if !w.watch.operation && app.Status.OperationState == nil {
		return st, nil
	}
	list, err := w.cl.ListApplicationOperations(ctx, &apiclient.ApplicationOperationsRequest{Name: app.Name, AppNamespace: app.Namespace, Limit: operationsToInspect})
	if err != nil {
		return st, err
	}
	for i := range list.Items {
		op := &list.Items[i]
		if st.latest == nil {
			st.latest = op
		}
		if !op.IsTerminal() {
			st.inProgress = true
		}
	}
	if st.latest != nil && st.latest.IsTerminal() && needsReconcile(app, st.latest) {
		st.inProgress = true
	}
	return st, nil
}

// needsReconcile reports a finished, non-dry-run operation the controller has not yet
// reconciled after.
func needsReconcile(app *v1alpha1.Application, op *v1alpha1.Operation) bool {
	if !op.IsTerminal() || op.IsDryRun() || op.Status.FinishedAt == nil {
		return false
	}
	return app.Status.ReconciledAt == nil || app.Status.ReconciledAt.Before(op.Status.FinishedAt)
}

// groupResourceStates merges duplicate rows (hooks with results and live state).
func groupResourceStates(app *v1alpha1.Application, results []v1alpha1.ResourceResult, selectors []resourceSelector) []*resourceState {
	var order []string
	byKey := map[string]*resourceState{}
	for _, s := range resourceStates(app, results, selectors) {
		k := s.key()
		if p, ok := byKey[k]; ok {
			p.merge(s)
			continue
		}
		byKey[k] = s
		order = append(order, k)
	}
	out := make([]*resourceState, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}

// printFinal prints the final state; when an operation ran it refreshes the Application
// first so the sync status reflects the completed operation.
// printFinal prints the final state of app (re-read with a normal refresh when refresh is
// set). op, when known, is the Operation the wait observed: its state and resource results
// are printed, because the Application status may not reflect it yet.
func (w *waiter) printFinal(ctx context.Context, app *v1alpha1.Application, refresh bool, op *v1alpha1.Operation) (*v1alpha1.Application, error) {
	if refresh && ctx.Err() == nil {
		fresh, err := getApp(ctx, w.cl, w.name, w.namespace, string(v1alpha1.RefreshTypeNormal))
		switch {
		case err == nil:
			app = fresh
		case apiclient.IsUnavailable(err):
			// The refresh wait is bounded server side; fall back to the current state.
			if fresh, err := getApp(ctx, w.cl, w.name, w.namespace, ""); err == nil {
				app = fresh
			}
		default:
			return nil, err
		}
	}
	if w.printSummary() {
		settings, err := w.cl.GetSettings(ctx)
		if err != nil {
			return nil, err
		}
		_, _ = fmt.Fprintln(w.out)
		printAppSummary(w.out, app, appURL(settings, w.cl.BaseURL(), app), nil)
		_, _ = fmt.Fprintln(w.out)
		if w.watch.operation {
			st := app.Status.OperationState
			if op != nil {
				st = operationState(op)
			}
			printOperationState(w.out, st, w.e.now())
		}
	}
	switch w.output {
	case outJSON, outYAML:
		if err := output.PrintResource(w.out, app, w.output); err != nil {
			return nil, err
		}
	case outTree, outTreeDetailed:
		tree, err := resourceTree(ctx, w.cl, AppRef{Name: app.Name, Namespace: app.Namespace})
		if err != nil {
			return nil, err
		}
		results, err := w.finalResults(ctx, app, op)
		if err != nil {
			return nil, err
		}
		if len(tree.Nodes) > 0 {
			_, _ = fmt.Fprintln(w.out)
			printAppTree(w.out, tree, resourceStates(app, results, nil), w.output == outTreeDetailed, w.e.now())
		}
	default:
		if len(app.Status.Resources) > 0 {
			results, err := w.finalResults(ctx, app, op)
			if err != nil {
				return nil, err
			}
			_, _ = fmt.Fprintln(w.out)
			printAppResources(w.out, resourceStates(app, results, nil))
		}
	}
	return app, nil
}

// finalResults returns the resource results of op, or of the Application's last operation.
func (w *waiter) finalResults(ctx context.Context, app *v1alpha1.Application, op *v1alpha1.Operation) ([]v1alpha1.ResourceResult, error) {
	if op != nil {
		if op.Status.SyncResult == nil {
			return nil, nil
		}
		return op.Status.SyncResult.Resources, nil
	}
	return lastOperationResults(ctx, w.cl, app)
}

// operationState is the status.operationState view of op.
func operationState(op *v1alpha1.Operation) *v1alpha1.ApplicationOperationState {
	st := &v1alpha1.ApplicationOperationState{
		OperationName: op.Name, Phase: op.Status.Phase, Message: op.Status.Message, StartedAt: op.Status.StartedAt,
		FinishedAt: op.Status.FinishedAt, RetryCount: op.Status.RetryCount, InitiatedBy: op.Spec.InitiatedBy,
	}
	if r := op.Status.SyncResult; r != nil {
		st.SyncResult = &v1alpha1.SyncResultSummary{Revision: r.Revision, Revisions: r.Revisions, Source: r.Source, Sources: r.Sources}
	}
	return st
}
