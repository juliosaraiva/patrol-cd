package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

type getOptions struct {
	refresh        bool
	hardRefresh    bool
	output         string
	timeout        uint
	showParams     bool
	showOperation  bool
	appNamespace   string
	sourcePosition int
	sourceName     string
}

func newGetCommand(e *env) *cobra.Command {
	o := &getOptions{}
	c := &cobra.Command{
		Use:   "get APPNAME",
		Short: "Get application details",
		Example: `  # Get basic details about the application "my-app" in wide format
  patrol app get my-app -o wide

  # Get detailed information about the application "my-app" in YAML format
  patrol app get my-app -o yaml

  # Get details of the application "my-app" in JSON format
  patrol app get my-app -o json

  # Get application details and include information about the current operation
  patrol app get my-app --show-operation

  # Show application parameters and overrides
  patrol app get my-app --show-params

  # Show application parameters and overrides for a source at position 1 under spec.sources of app my-app
  patrol app get my-app --show-params --source-position 1

  # Show application parameters and overrides for a source named "test"
  patrol app get my-app --show-params --source-name test

  # Refresh application data when retrieving
  patrol app get my-app --refresh

  # Perform a hard refresh, including refreshing application data and target manifests cache
  patrol app get my-app --hard-refresh

  # Get application details and display them in a tree format
  patrol app get my-app --output tree

  # Get application details and display them in a detailed tree format
  patrol app get my-app --output tree=detailed`,
		RunE: func(c *cobra.Command, args []string) error { return runGet(c, e, o, args) },
	}
	fs := c.Flags()
	fs.StringVarP(&o.output, "output", "o", outWide, "Output format. One of: json|yaml|wide|tree")
	fs.UintVar(&o.timeout, "timeout", 0, "Time out after this many seconds")
	fs.BoolVar(&o.showOperation, "show-operation", false, "Show application operation")
	fs.BoolVar(&o.showParams, "show-params", false, "Show application parameters and overrides")
	fs.BoolVar(&o.refresh, "refresh", false, "Refresh application data when retrieving")
	fs.BoolVar(&o.hardRefresh, "hard-refresh", false, "Refresh application data as well as target manifests cache")
	fs.StringVarP(&o.appNamespace, "app-namespace", "N", "", "Only get application from namespace")
	fs.IntVar(&o.sourcePosition, "source-position", -1, "Position of the source from the list of sources of the app. Counting starts at 1.")
	fs.StringVar(&o.sourceName, "source-name", "", "Name of the source from the list of sources of the app.")
	return c
}

func runGet(c *cobra.Command, e *env, o *getOptions, args []string) error {
	ctx := cmdContext(c)
	if len(args) == 0 {
		return usage(c, args)
	}
	switch o.output {
	case outJSON, outYAML, outWide, "", outTree, outTreeDetailed:
	default:
		return fmt.Errorf("unknown output format: %s", o.output)
	}
	cl, err := e.client(ctx)
	if err != nil {
		return err
	}
	name, ns := parseQualifiedName(args[0], o.appNamespace)
	app, err := getWithTimeout(ctx, c.OutOrStdout(), cl, name, ns, refreshType(o.refresh, o.hardRefresh), time.Duration(o.timeout)*time.Second)
	if err != nil {
		return err
	}
	position, err := resolveSourcePosition(app, o.sourcePosition, o.sourceName, o.showParams)
	if err != nil {
		return err
	}
	out := c.OutOrStdout()
	switch o.output {
	case outJSON, outYAML:
		return output.PrintResource(out, app, o.output)
	}
	if err := printAppHeader(ctx, e, cl, out, app, o.showOperation, o.showParams, position); err != nil {
		return err
	}
	results, err := lastOperationResults(ctx, cl, app)
	if err != nil {
		return err
	}
	states := resourceStates(app, results, nil)
	switch o.output {
	case outTree, outTreeDetailed:
		tree, err := resourceTree(ctx, cl, AppRef{Name: app.Name, Namespace: app.Namespace})
		if err != nil {
			return err
		}
		if len(tree.Nodes) > 0 {
			_, _ = fmt.Fprintln(out)
			printAppTree(out, tree, states, o.output == outTreeDetailed, e.now())
		}
	default:
		if len(app.Status.Resources) > 0 {
			_, _ = fmt.Fprintln(out)
			printAppResources(out, states)
		}
	}
	return nil
}

// getWithTimeout reads the Application, refreshing it first when refresh is set. With a
// timeout, a refresh that does not finish in time is abandoned and the Application is read
// without refresh (upstream getAppStateWithRetry).
func getWithTimeout(ctx context.Context, w io.Writer, cl Client, name, ns, refresh string, timeout time.Duration) (*v1alpha1.Application, error) {
	if timeout <= 0 || refresh == "" {
		return getApp(ctx, cl, name, ns, refresh)
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	app, err := getApp(tctx, cl, name, ns, refresh)
	if err == nil {
		return app, nil
	}
	if ctx.Err() != nil || !errors.Is(tctx.Err(), context.DeadlineExceeded) {
		return nil, err
	}
	_, _ = fmt.Fprintln(w, "Timeout function: cancelling context manually")
	return getApp(ctx, cl, name, ns, "")
}

// printAppHeader prints the summary, conditions, operation and parameters (upstream
// printHeader).
func printAppHeader(ctx context.Context, e *env, cl Client, w io.Writer, app *v1alpha1.Application, showOperation, showParams bool, position int) error {
	settings, err := cl.GetSettings(ctx)
	if err != nil {
		return err
	}
	windows, err := cl.GetApplicationSyncWindows(ctx, &apiclient.ApplicationRef{Name: app.Name, AppNamespace: app.Namespace})
	if err != nil {
		return err
	}
	ws := windowSummary(windows)
	printAppSummary(w, app, appURL(settings, cl.BaseURL(), app), &ws)
	if conds := activeConditions(app); len(conds) > 0 {
		_, _ = fmt.Fprintln(w)
		printAppConditions(w, conds)
		_, _ = fmt.Fprintln(w)
	}
	if showOperation && app.Status.OperationState != nil {
		_, _ = fmt.Fprintln(w)
		printOperationState(w, app.Status.OperationState, e.now())
	}
	if showParams {
		var src *v1alpha1.ApplicationSource
		if app.Spec.HasMultipleSources() {
			src = sourceByPosition(&app.Spec, position)
		} else {
			s := firstSource(&app.Spec)
			src = &s
		}
		if src != nil && src.Helm != nil {
			printHelmParams(w, src.Helm)
		}
	}
	return nil
}

// lastOperationResults returns the per-resource results of the Application's latest
// Operation (Patrol keeps them on the Operation object, not in the Application status). An
// Operation that no longer exists yields no results.
func lastOperationResults(ctx context.Context, cl Client, app *v1alpha1.Application) ([]v1alpha1.ResourceResult, error) {
	st := app.Status.OperationState
	if st == nil || st.OperationName == "" {
		return nil, nil
	}
	op, err := cl.GetApplicationOperation(ctx, &apiclient.ApplicationOperationRequest{Name: app.Name, AppNamespace: app.Namespace, Operation: st.OperationName})
	if err != nil {
		if apiclient.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if op.Status.SyncResult == nil {
		return nil, nil
	}
	return op.Status.SyncResult.Resources, nil
}
