package app

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func newAddSourceCommand(e *env) *cobra.Command {
	var (
		opts         appOptions
		appNamespace string
	)
	c := &cobra.Command{
		Use:   "add-source APPNAME",
		Short: "Adds a source to the list of sources in the application",
		Example: `  # Append a source to the list of sources in the application
  patrol app add-source guestbook --repo https://github.com/argoproj/argocd-example-apps.git --path guestbook --source-name guestbook`,
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
			if len(app.Spec.Sources) == 0 {
				return fatalGeneric("Cannot add source: application %s does not have spec.sources defined", name)
			}
			src := &v1alpha1.ApplicationSource{}
			m := &specMapper{ctx: ctx, e: e, fs: c.Flags(), opts: &opts}
			if _, err := m.source(src); err != nil {
				return err
			}
			if src.RepoURL == "" && src.Path == "" && src.Chart == "" && src.Ref == "" {
				return fatalGeneric("ApplicationSource needs atleast repoUrl, path or chart or ref field. No source to add.")
			}
			app.Spec.Sources = append(app.Spec.Sources, *src)
			if err := setParameterOverrides(app, opts.parameters, len(app.Spec.Sources)); err != nil {
				return err
			}
			if err := updateSpec(ctx, cl, app, &opts.validate); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(c.OutOrStdout(), "Application '%s' updated successfully\n", app.Name)
			return nil
		},
	}
	addAppFlags(c.Flags(), &opts)
	c.Flags().StringVarP(&appNamespace, "app-namespace", "N", "", "Namespace of the target application where the source will be appended")
	return c
}

func newRemoveSourceCommand(e *env) *cobra.Command {
	var (
		sourcePosition int
		sourceName     string
		appNamespace   string
	)
	c := &cobra.Command{
		Use:   "remove-source APPNAME",
		Short: "Remove a source from multiple sources application.",
		Example: `  # Remove the source at position 1 from application's sources. Counting starts at 1.
  patrol app remove-source myapplication --source-position 1

  # Remove the source named "test" from application's sources.
  patrol app remove-source myapplication --source-name test`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) != 1 {
				return usage(c, args)
			}
			if sourceName == "" && sourcePosition <= 0 {
				return fatalGeneric("Value of source-position must be greater than 0")
			}
			if sourceName != "" && sourcePosition != -1 {
				return fatalGeneric("Only one of source-position and source-name can be specified.")
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
			if sourceName != "" {
				if sourcePosition, err = sourcePositionByName(app, sourceName); err != nil {
					return err
				}
			}
			if !app.Spec.HasMultipleSources() {
				return fatalGeneric("Application does not have multiple sources configured")
			}
			if len(app.Spec.Sources) == 1 {
				return fatalGeneric("Cannot remove the only source remaining in the app")
			}
			if len(app.Spec.Sources) < sourcePosition {
				return fatalGeneric("Application does not have source at %d", sourcePosition)
			}
			app.Spec.Sources = append(app.Spec.Sources[:sourcePosition-1:sourcePosition-1], app.Spec.Sources[sourcePosition:]...)
			ok, err := e.optionalPrompt().Confirm("Are you sure you want to delete the source? [y/n]")
			if err != nil {
				return err
			}
			if !ok {
				_, _ = fmt.Fprintln(c.OutOrStdout(), "The command to delete the source was cancelled")
				return nil
			}
			if err := updateSpec(ctx, cl, app, nil); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(c.OutOrStdout(), "Application '%s' updated successfully\n", app.Name)
			return nil
		},
	}
	c.Flags().StringVarP(&appNamespace, "app-namespace", "N", "", "Namespace of the target application where the source will be appended")
	c.Flags().IntVar(&sourcePosition, "source-position", -1, "Position of the source from the list of sources of the app. Counting starts at 1.")
	c.Flags().StringVar(&sourceName, "source-name", "", "Name of the source from the list of sources of the app.")
	return c
}
