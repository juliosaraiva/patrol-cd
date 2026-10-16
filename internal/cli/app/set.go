package app

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

type setOptions struct {
	app            appOptions
	appNamespace   string
	sourcePosition int
}

func newSetCommand(e *env) *cobra.Command {
	o := &setOptions{}
	c := &cobra.Command{
		Use:   "set APPNAME",
		Short: "Set application parameters",
		Example: `  # Set application parameters for the application "my-app"
  patrol app set my-app --parameter key1=value1 --parameter key2=value2

  # Set and validate application parameters for "my-app"
  patrol app set my-app --parameter key1=value1 --parameter key2=value2 --validate

  # Set and override application parameters for a source at position 1 under spec.sources of app my-app. source-position starts at 1.
  patrol app set my-app --source-position 1 --repo https://github.com/argoproj/argocd-example-apps.git

  # Set and override application parameters for a source named "test" under spec.sources of app my-app.
  patrol app set my-app --source-name test --repo https://github.com/argoproj/argocd-example-apps.git

  # Set application parameters and specify the namespace
  patrol app set my-app --parameter key1=value1 --parameter key2=value2 --app-namespace my-namespace`,
		RunE: func(c *cobra.Command, args []string) error { return runSet(c, e, o, args) },
	}
	addAppFlags(c.Flags(), &o.app)
	c.Flags().StringVarP(&o.appNamespace, "app-namespace", "N", "", "Set application parameters in namespace")
	c.Flags().IntVar(&o.sourcePosition, "source-position", -1, "Position of the source from the list of sources of the app. Counting starts at 1.")
	return c
}

func runSet(c *cobra.Command, e *env, o *setOptions, args []string) error {
	ctx := cmdContext(c)
	if len(args) != 1 {
		return usage(c, args)
	}
	cl, err := e.client(ctx)
	if err != nil {
		return err
	}
	name, ns := parseQualifiedName(args[0], o.appNamespace)
	app, err := getApp(ctx, cl, name, ns, "")
	if err != nil {
		return err
	}
	position, err := resolveSourcePosition(app, o.sourcePosition, o.app.sourceName, true)
	if err != nil {
		return err
	}
	m := &specMapper{ctx: ctx, e: e, fs: c.Flags(), opts: &o.app}
	visited, err := m.apply(&app.Spec, position)
	if err != nil {
		return err
	}
	if visited == 0 {
		_, _ = fmt.Fprintln(c.ErrOrStderr(), "Please set at least one option to update")
		return usage(c, args)
	}
	if err := setParameterOverrides(app, o.app.parameters, position); err != nil {
		return err
	}
	return updateSpec(ctx, cl, app, &o.app.validate)
}

// updateSpec stores the spec of app (PUT .../spec).
func updateSpec(ctx context.Context, cl Client, app *v1alpha1.Application, validate *bool) error {
	_, err := cl.UpdateApplicationSpec(ctx, &apiclient.ApplicationUpdateSpecRequest{
		Name: app.Name, AppNamespace: app.Namespace, Spec: &app.Spec, Validate: validate,
	})
	return err
}
