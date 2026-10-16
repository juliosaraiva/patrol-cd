package app

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func newHistoryCommand(e *env) *cobra.Command {
	var output, appNamespace string
	c := &cobra.Command{
		Use:   "history APPNAME",
		Short: "Show application deployment history",
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) != 1 {
				return usage(c, args)
			}
			switch output {
			case outWide, outID, "":
			default:
				return fmt.Errorf("unknown output format: %s", output)
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			name, ns := parseQualifiedName(args[0], appNamespace)
			app, err := getApp(ctx, cl, name, ns, "")
			if err != nil {
				return err
			}
			if output == outID {
				printHistoryIDs(c.OutOrStdout(), app.Status.History)
			} else {
				printHistoryTable(c.OutOrStdout(), app.Status.History)
			}
			return nil
		},
	}
	c.Flags().StringVarP(&appNamespace, "app-namespace", "N", "", "Only show application deployment history in namespace")
	c.Flags().StringVarP(&output, "output", "o", outWide, "Output format. One of: wide|id")
	return c
}

// findRevisionHistory returns the history entry with id, or the previous deployment for
// id -1 (upstream findRevisionHistory).
func findRevisionHistory(app *v1alpha1.Application, id int64) (*v1alpha1.RevisionHistory, error) {
	if id == -1 {
		n := len(app.Status.History)
		if n < 2 {
			return nil, fmt.Errorf("application '%s' should have at least two successful deployments", app.Name)
		}
		return &app.Status.History[n-2], nil
	}
	for i := range app.Status.History {
		if app.Status.History[i].ID == id {
			return &app.Status.History[i], nil
		}
	}
	return nil, fmt.Errorf("application '%s' does not have deployment id '%d' in history", app.Name, id)
}

func newRollbackCommand(e *env) *cobra.Command {
	var (
		prune        bool
		dryRun       bool
		timeout      uint
		output       string
		appNamespace string
	)
	c := &cobra.Command{
		Use:   "rollback APPNAME [ID]",
		Short: "Rollback application to a previous deployed version by History ID, omitted will Rollback to the previous version",
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) == 0 || len(args) > 2 {
				return usage(c, args)
			}
			if err := validateWaitOutput(output); err != nil {
				return err
			}
			id := int64(-1)
			if len(args) > 1 {
				v, err := strconv.ParseInt(args[1], 10, 64)
				if err != nil {
					return fmt.Errorf("invalid history id %q: %w", args[1], err)
				}
				id = v
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			name, ns := parseQualifiedName(args[0], appNamespace)
			app, err := getApp(ctx, cl, name, ns, "")
			if err != nil {
				return err
			}
			entry, err := findRevisionHistory(app, id)
			if err != nil {
				return err
			}
			resp, err := cl.RollbackApplication(ctx, &apiclient.ApplicationRollbackRequest{
				Name: name, AppNamespace: ns, ID: entry.ID, Prune: prune, DryRun: dryRun,
			})
			if err != nil {
				return err
			}
			opName := ""
			if resp.Operation != nil {
				opName = resp.Operation.Name
			}
			w := &waiter{e: e, cl: cl, out: c.OutOrStdout(), name: name, namespace: ns, timeout: time.Duration(timeout) * time.Second,
				watch: watchOpts{operation: true}, output: output, operation: opName}
			result, err := w.wait(ctx)
			if err != nil {
				if errors.Is(err, errWaitTimeout) {
					return fatalGeneric("timed out (%ds) waiting for app %q match desired state", timeout, qualify(name, ns))
				}
				return err
			}
			if dryRun || result.Operation == nil {
				return nil
			}
			if !result.Operation.Status.Phase.IsSuccessful() {
				return fatal("Operation has completed with phase: %s", result.Operation.Status.Phase)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&prune, "prune", false, "Allow deleting unexpected resources")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "Preview the rollback without affecting the cluster")
	c.Flags().UintVar(&timeout, "timeout", 0, "Time out after this many seconds")
	c.Flags().StringVarP(&output, "output", "o", outWide, "Output format. One of: json|yaml|wide|tree|tree=detailed")
	c.Flags().StringVarP(&appNamespace, "app-namespace", "N", "", "Rollback application in namespace")
	return c
}

func newTerminateOpCommand(e *env) *cobra.Command {
	var appNamespace string
	c := &cobra.Command{
		Use:   "terminate-op APPNAME",
		Short: "Terminate running operation of an application",
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) != 1 {
				return usage(c, args)
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			name, ns := parseQualifiedName(args[0], appNamespace)
			if _, err := cl.TerminateApplicationOperation(ctx, &apiclient.ApplicationRef{Name: name, AppNamespace: ns}); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(c.OutOrStdout(), "Application '%s' operation terminating\n", name)
			return nil
		},
	}
	c.Flags().StringVarP(&appNamespace, "app-namespace", "N", "", "Namespace of the application")
	return c
}

func newConfirmDeletionCommand(e *env) *cobra.Command {
	var appNamespace string
	c := &cobra.Command{
		Use:   "confirm-deletion APPNAME",
		Short: "Confirms deletion/pruning of an application resources",
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) != 1 {
				return usage(c, args)
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			name, ns := parseQualifiedName(args[0], appNamespace)
			app, err := getApp(ctx, cl, name, ns, "")
			if err != nil {
				return err
			}
			if app.Annotations == nil {
				app.Annotations = map[string]string{}
			}
			app.Annotations[v1alpha1.AnnotationKeyDeletionApproved] = e.now().UTC().Format(time.RFC3339)
			validate := false
			if _, err := cl.UpdateApplication(ctx, &apiclient.ApplicationUpdateRequest{
				Name: app.Name, AppNamespace: app.Namespace, Project: app.Spec.Project, Validate: &validate, Application: app,
			}); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(c.OutOrStdout(), "Application '%s' updated successfully\n", app.Name)
			return nil
		},
	}
	c.Flags().StringVarP(&appNamespace, "app-namespace", "N", "", "Namespace of the target application where the source will be appended")
	return c
}
