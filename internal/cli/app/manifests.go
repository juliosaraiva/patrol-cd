package app

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func newManifestsCommand(e *env) *cobra.Command {
	var (
		source          string
		revision        string
		revisions       []string
		sourcePositions []int64
		sourceNames     []string
		local           string
		localRepoRoot   string
		localIncludes   []string
		serverGenerate  bool
		appNamespace    string
	)
	c := &cobra.Command{
		Use:   "manifests APPNAME",
		Short: "Print manifests of an application",
		Example: `  # Get manifests for an application
  patrol app manifests my-app

  # Get manifests for an application at a specific revision
  patrol app manifests my-app --revision 0.0.1

  # Get manifests for a multi-source application at specific revisions for specific sources
  patrol app manifests my-app --revisions 0.0.1 --source-names src-base --revisions 0.0.2 --source-names src-values

  # Get manifests for a multi-source application at specific revisions for specific sources
  patrol app manifests my-app --revisions 0.0.1 --source-positions 1 --revisions 0.0.2 --source-positions 2

  # Get the live manifests of an application
  patrol app manifests my-app --source live`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) != 1 {
				return usage(c, args)
			}
			if err := validateRevisionFlags(revisions, sourcePositions, sourceNames); err != nil {
				return err
			}
			if source != "git" && source != "live" {
				return fatal("Unknown source type '%s'", source)
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
			positions := sourcePositions
			if len(sourceNames) > 0 {
				if positions, err = resolveSourceNames(app, sourceNames); err != nil {
					return err
				}
			}
			ref := AppRef{Name: app.Name, Namespace: app.Namespace}
			var objs []*unstructured.Unstructured
			switch {
			case source == "live":
				managed, err := managedResources(ctx, cl, ref)
				if err != nil {
					return err
				}
				for _, m := range managed {
					obj, err := unmarshalObject(m.LiveState)
					if err != nil {
						return err
					}
					if obj != nil {
						objs = append(objs, obj)
					}
				}
			case local != "":
				var manifests []string
				if serverGenerate {
					manifests, err = serverSideGenerate(ctx, cl, app, local, localIncludes)
				} else {
					settings, serr := cl.GetSettings(ctx)
					if serr != nil {
						return serr
					}
					manifests, err = e.renderLocal(ctx, cl, app, settings, local, localRepoRoot)
				}
				if err != nil {
					return err
				}
				if objs, err = manifestsToObjects(manifests); err != nil {
					return err
				}
			case len(revisions) > 0 && len(positions) > 0, revision != "":
				res, err := cl.GetApplicationManifests(ctx, &apiclient.ApplicationManifestQuery{
					Name: app.Name, AppNamespace: app.Namespace, Revision: revision, Revisions: revisions, SourcePositions: positions,
				})
				if err != nil {
					return err
				}
				if objs, err = manifestsToObjects(res.Manifests); err != nil {
					return err
				}
			default:
				managed, err := managedResources(ctx, cl, ref)
				if err != nil {
					return err
				}
				for _, m := range managed {
					obj, err := unmarshalObject(m.TargetState)
					if err != nil {
						return err
					}
					if obj != nil {
						objs = append(objs, obj)
					}
				}
			}
			out := c.OutOrStdout()
			for _, obj := range objs {
				data, err := yaml.Marshal(obj.Object)
				if err != nil {
					return fmt.Errorf("encode manifest: %w", err)
				}
				_, _ = fmt.Fprintln(out, "---")
				_, _ = fmt.Fprintf(out, "%s\n", data)
			}
			return nil
		},
	}
	fs := c.Flags()
	fs.StringVar(&source, "source", "git", "Source of manifests. One of: live|git")
	fs.StringVar(&revision, "revision", "", "Show manifests at a specific revision")
	fs.StringVarP(&appNamespace, "app-namespace", "N", "", "Namespace of the application")
	fs.StringArrayVar(&revisions, "revisions", []string{}, "Show manifests at specific revisions for the source at position in source-positions")
	fs.Int64SliceVar(&sourcePositions, "source-positions", []int64{}, "List of source positions. Default is empty array. Counting start at 1.")
	fs.StringArrayVar(&sourceNames, "source-names", []string{}, "List of source names. Default is an empty array.")
	fs.StringVar(&local, "local", "", "If set, show locally-generated manifests. Value is the absolute path to app manifests within the manifest repo. Example: '/home/username/apps/env/app-1'.")
	fs.StringVar(&localRepoRoot, "local-repo-root", ".", "Path to the local repository root. Used together with --local allows setting the repository root. Example: '/home/username/apps'.")
	fs.BoolVar(&serverGenerate, "server-side-generate", false, "Used with --local: render the local files on the server (supports config management plugins; Secret data is masked)")
	fs.StringArrayVar(&localIncludes, "local-include", defaultLocalIncludes, "Used with --server-side-generate, specify patterns of filenames to send. Matching is based on filename and not path.")
	return c
}
