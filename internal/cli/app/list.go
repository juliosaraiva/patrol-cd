package app

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

type listOptions struct {
	output       string
	selector     string
	projects     []string
	repo         string
	appNamespace string
	cluster      string
	path         string
}

func newListCommand(e *env) *cobra.Command {
	o := &listOptions{}
	c := &cobra.Command{
		Use:   "list",
		Short: "List applications",
		Example: `  # List all apps
  patrol app list

  # List apps by label, in this example we listing apps that are children of another app (aka app-of-apps)
  patrol app list -l app.kubernetes.io/instance=my-app
  patrol app list -l app.kubernetes.io/instance!=my-app
  patrol app list -l app.kubernetes.io/instance
  patrol app list -l '!app.kubernetes.io/instance'
  patrol app list -l 'app.kubernetes.io/instance notin (my-app,other-app)'`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return runList(c, e, o) },
	}
	fs := c.Flags()
	fs.StringVarP(&o.output, "output", "o", outWide, "Output format. One of: wide|name|json|yaml")
	fs.StringVarP(&o.selector, "selector", "l", "", "List apps by label. Supports '=', '==', '!=', in, notin, exists & not exists. Matching apps must satisfy all of the specified label constraints.")
	fs.StringArrayVarP(&o.projects, "project", "p", []string{}, "Filter by project name")
	fs.StringVarP(&o.repo, "repo", "r", "", "List apps by source repo URL")
	fs.StringVarP(&o.appNamespace, "app-namespace", "N", "", "Only list applications in namespace")
	fs.StringVarP(&o.cluster, "cluster", "c", "", "List apps by cluster name or url")
	fs.StringVarP(&o.path, "path", "P", "", "List apps by path")
	return c
}

func runList(c *cobra.Command, e *env, o *listOptions) error {
	ctx := cmdContext(c)
	switch o.output {
	case outJSON, outYAML, outName, outWide, "":
	default:
		return fmt.Errorf("unknown output format: %s", o.output)
	}
	cl, err := e.client(ctx)
	if err != nil {
		return err
	}
	apps, err := listAllApps(ctx, cl, apiclient.ApplicationQuery{
		Selector: o.selector, AppNamespace: o.appNamespace, Projects: o.projects, Repo: o.repo,
	})
	if err != nil {
		return err
	}
	apps = filterApps(apps, o.cluster, o.path)
	out := c.OutOrStdout()
	switch o.output {
	case outJSON, outYAML:
		return output.PrintResourceList(out, apps, o.output, false)
	case outName:
		printApplicationNames(out, apps)
	default:
		printApplicationTable(out, apps, o.output)
	}
	return nil
}

// filterApps keeps Applications whose destination server or name is cluster and with a
// source of the given path (empty filters keep everything).
func filterApps(apps []v1alpha1.Application, cluster, path string) []v1alpha1.Application {
	if cluster == "" && path == "" {
		return apps
	}
	out := apps[:0:0]
	for _, app := range apps {
		if cluster != "" && app.Spec.Destination.Server != cluster && app.Spec.Destination.Name != cluster {
			continue
		}
		if path != "" && !hasSourcePath(&app.Spec, path) {
			continue
		}
		out = append(out, app)
	}
	return out
}

func hasSourcePath(spec *v1alpha1.ApplicationSpec, path string) bool {
	for _, s := range spec.GetSources() {
		if s.Path == path {
			return true
		}
	}
	return false
}
