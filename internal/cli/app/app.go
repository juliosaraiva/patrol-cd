package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// listPageSize is the page size used when the CLI lists every Application.
const listPageSize = 500

// maxListPages bounds the pages read by one listing (500 × 200 = 100 000 Applications).
const maxListPages = 200

func newAppCommand(e *env) *cobra.Command {
	c := &cobra.Command{
		Use:   "app",
		Short: "Manage applications",
		Example: `  # List all the applications.
  patrol app list

  # Get the details of a application
  patrol app get my-app

  # Set an override parameter
  patrol app set my-app -p image.tag=v1.0.1`,
		Args: cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error { return usage(c, args) },
	}
	c.AddCommand(
		newCreateCommand(e),
		newGetCommand(e),
		newDiffCommand(e),
		newSetCommand(e),
		newUnsetCommand(e),
		newSyncCommand(e),
		newHistoryCommand(e),
		newRollbackCommand(e),
		newListCommand(e),
		newDeleteCommand(e),
		newWaitCommand(e),
		newManifestsCommand(e),
		newTerminateOpCommand(e),
		newEditCommand(e),
		newPatchCommand(e),
		newGetResourceCommand(e),
		newPatchResourceCommand(e),
		newDeleteResourceCommand(e),
		newActionsCommand(e),
		newResourcesCommand(e),
		newLogsCommand(e),
		newAddSourceCommand(e),
		newRemoveSourceCommand(e),
		newConfirmDeletionCommand(e),
	)
	return c
}

// cmdContext returns the command context (Background when cobra ran without one).
func cmdContext(c *cobra.Command) context.Context {
	if ctx := c.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// getApp reads an Application, optionally refreshing it first ("normal" or "hard").
func getApp(ctx context.Context, cl Client, name, namespace, refresh string) (*v1alpha1.Application, error) {
	return cl.GetApplication(ctx, &apiclient.ApplicationGetRequest{Name: name, AppNamespace: namespace, Refresh: refresh})
}

// refreshType maps --refresh / --hard-refresh to the refresh query value.
func refreshType(refresh, hard bool) string {
	switch {
	case hard:
		return string(v1alpha1.RefreshTypeHard)
	case refresh:
		return string(v1alpha1.RefreshTypeNormal)
	}
	return ""
}

// listAllApps lists every Application matching q, following continue tokens.
func listAllApps(ctx context.Context, cl Client, q apiclient.ApplicationQuery) ([]v1alpha1.Application, error) {
	q.Limit = listPageSize
	var out []v1alpha1.Application
	for range maxListPages {
		list, err := cl.ListApplications(ctx, &q)
		if err != nil {
			return nil, err
		}
		out = append(out, list.Items...)
		if list.Continue == "" {
			return out, nil
		}
		q.Continue = list.Continue
	}
	return nil, fmt.Errorf("listing applications: more than %d pages", maxListPages)
}

// sourcePositionByName resolves --source-name to a 1-based position.
func sourcePositionByName(app *v1alpha1.Application, name string) (int, error) {
	for i, s := range app.Spec.Sources {
		if s.Name == name {
			return i + 1, nil
		}
	}
	return 0, fatal("Unknown source name '%s'", name)
}

// resolveSourcePosition applies the shared --source-name / --source-position rules of get,
// set and unset (multi-source Applications need a position in range).
func resolveSourcePosition(app *v1alpha1.Application, position int, name string, requireForMulti bool) (int, error) {
	if name != "" && position != -1 {
		return 0, fatalGeneric("Only one of source-position and source-name can be specified.")
	}
	if name != "" {
		p, err := sourcePositionByName(app, name)
		if err != nil {
			return 0, err
		}
		position = p
	}
	if requireForMulti && app.Spec.HasMultipleSources() {
		if position <= 0 {
			return 0, fatalGeneric("Source position should be specified and must be greater than 0 for applications with multiple sources")
		}
		if len(app.Spec.GetSources()) < position {
			return 0, fatalGeneric("Source position should be less than the number of sources in the application")
		}
	}
	return position, nil
}

// resolveSourceNames converts --source-names into positions.
func resolveSourceNames(app *v1alpha1.Application, names []string) ([]int64, error) {
	var out []int64
	for _, n := range names {
		p, err := sourcePositionByName(app, n)
		if err != nil {
			return nil, err
		}
		out = append(out, int64(p))
	}
	return out, nil
}

// validateRevisionFlags checks --revisions with --source-positions / --source-names.
func validateRevisionFlags(revisions []string, positions []int64, names []string) error {
	if len(names) > 0 && len(positions) > 0 {
		return fatalGeneric("Only one of source-positions and source-names can be specified.")
	}
	if len(positions) > 0 && len(revisions) != len(positions) {
		return fatalGeneric("While using --revisions and --source-positions, length of values for both flags should be same.")
	}
	if len(names) > 0 && len(revisions) != len(names) {
		return fatalGeneric("While using --revisions and --source-names, length of values for both flags should be same.")
	}
	for _, p := range positions {
		if p <= 0 {
			return fatal("source-position cannot be less than or equal to 0, Counting starts at 1")
		}
	}
	return nil
}

// appURL is the UI URL of an Application: settings url, else the server address.
func appURL(settings *apiclient.Settings, baseURL string, app *v1alpha1.Application) string {
	path := "/applications/" + app.Name
	if settings != nil && settings.ControllerNamespace != "" && app.Namespace != "" && app.Namespace != settings.ControllerNamespace {
		path = "/applications/" + app.Namespace + "/" + app.Name
	}
	if settings != nil && settings.URL != "" {
		return strings.TrimSuffix(settings.URL, "/") + path
	}
	return strings.TrimSuffix(strings.TrimSuffix(baseURL, "/"), ":443") + path
}

// isNotFoundOrDenied reports errors for a missing Application (the API answers 403 for
// missing and forbidden Applications alike).
func isNotFoundOrDenied(err error) bool {
	return apiclient.IsNotFound(err) || apiclient.IsPermissionDenied(err)
}
