package app

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func newEditCommand(e *env) *cobra.Command {
	var appNamespace string
	c := &cobra.Command{
		Use:   "edit APPNAME",
		Short: "Edit application",
		Long: `Edit the spec of an application in your editor (EDITOR, else vi).

The spec is validated locally (it must parse as an application spec without unknown
fields) and by the server (repository and destination checks). Validation errors are
shown at the top of the file and the editor opens again; saving an empty or unchanged
file cancels the edit.`,
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
			js, err := json.Marshal(app.Spec)
			if err != nil {
				return fmt.Errorf("encode spec: %w", err)
			}
			data, err := yaml.JSONToYAML(js)
			if err != nil {
				return fmt.Errorf("encode spec: %w", err)
			}
			return e.edit(ctx, name+"-*-edit.yaml", data, func(input []byte) error {
				spec, err := parseSpec(input)
				if err != nil {
					return err
				}
				validate := true
				_, err = cl.UpdateApplicationSpec(ctx, &apiclient.ApplicationUpdateSpecRequest{
					Name: app.Name, AppNamespace: app.Namespace, Spec: spec, Validate: &validate,
				})
				if err != nil {
					return fmt.Errorf("failed to update application spec: %w", err)
				}
				return nil
			})
		},
	}
	c.Flags().StringVarP(&appNamespace, "app-namespace", "N", "", "Only edit application in namespace")
	return c
}

// parseSpec decodes an edited spec strictly.
func parseSpec(input []byte) (*v1alpha1.ApplicationSpec, error) {
	js, err := yaml.YAMLToJSON(input)
	if err != nil {
		return nil, fmt.Errorf("error converting YAML to JSON: %w", err)
	}
	var spec v1alpha1.ApplicationSpec
	if err := decodeStrict(js, &spec); err != nil {
		return nil, fmt.Errorf("error unmarshaling input into application spec: %w", err)
	}
	return &spec, nil
}

func newPatchCommand(e *env) *cobra.Command {
	var patch, patchType, appNamespace string
	c := &cobra.Command{
		Use:   "patch APPNAME",
		Short: "Patch application",
		Example: `  # Update an application's source path using json patch
  patrol app patch myapplication --patch='[{"op": "replace", "path": "/spec/source/path", "value": "newPath"}]' --type json

  # Update an application's repository target revision using merge patch
  patrol app patch myapplication --patch '{"spec": { "source": { "targetRevision": "master" } }}' --type merge`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) != 1 {
				return usage(c, args)
			}
			switch patchType {
			case apiclient.PatchTypeJSON, apiclient.PatchTypeMerge:
			default:
				return fmt.Errorf("unknown patch type %q: one of [json merge]", patchType)
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			name, ns := parseQualifiedName(args[0], appNamespace)
			patched, err := cl.PatchApplication(ctx, &apiclient.ApplicationPatchRequest{
				Name: name, AppNamespace: ns, Patch: patch, PatchType: patchType,
			})
			if err != nil {
				return err
			}
			out, err := yaml.Marshal(patched)
			if err != nil {
				return fmt.Errorf("encode application: %w", err)
			}
			_, _ = fmt.Fprintln(c.OutOrStdout(), string(out))
			return nil
		},
	}
	c.Flags().StringVarP(&appNamespace, "app-namespace", "N", "", "Only patch application in namespace")
	c.Flags().StringVar(&patch, "patch", "", "Patch body")
	c.Flags().StringVar(&patchType, "type", apiclient.PatchTypeJSON, "The type of patch being provided; one of [json merge]")
	return c
}
