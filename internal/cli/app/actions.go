package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// displayedAction is one row of `app actions list`.
type displayedAction struct {
	Group    string `json:"group"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Action   string `json:"action"`
	Disabled bool   `json:"disabled"`
}

func newActionsCommand(e *env) *cobra.Command {
	c := &cobra.Command{
		Use:   "actions",
		Short: "Manage Resource actions",
		Example: `  # List all the available actions for an application
  patrol app actions list APPNAME

  # Run an available action for an application
  patrol app actions run APPNAME ACTION --kind KIND [--resource-name RESOURCE] [--namespace NAMESPACE] [--group GROUP]`,
		RunE: func(c *cobra.Command, args []string) error { return usage(c, args) },
	}
	c.AddCommand(newActionsListCommand(e), newActionsRunCommand(e))
	return c
}

// actionableResources returns the managed resources plus the Application itself (actions
// can target the Application, e.g. toggle-auto-sync).
func actionableResources(ctx context.Context, cl Client, app AppRef) ([]apiclient.ResourceDiff, error) {
	managed, err := managedResources(ctx, cl, app)
	if err != nil {
		return nil, err
	}
	a, err := getApp(ctx, cl, app.Name, app.Namespace, "")
	if err != nil {
		return nil, err
	}
	a.Kind = "Application"
	a.APIVersion = v1alpha1.GroupVersion.String()
	data, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("encode application: %w", err)
	}
	return append(managed, apiclient.ResourceDiff{Group: v1alpha1.GroupName, Kind: "Application", Namespace: a.Namespace, Name: a.Name, LiveState: string(data)}), nil
}

func newActionsListCommand(e *env) *cobra.Command {
	var namespace, kind, group, resourceName, out, appNamespace string
	c := &cobra.Command{
		Use:   "list APPNAME",
		Short: "Lists available actions on a resource",
		Example: `  # List all the available actions for an application
  patrol app actions list APPNAME`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) != 1 {
				return usage(c, args)
			}
			switch out {
			case "", outJSON, outYAML:
			default:
				return fmt.Errorf("unknown output format: %s", out)
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			name, ns := parseQualifiedName(args[0], appNamespace)
			app := AppRef{Name: name, Namespace: ns}
			resources, err := actionableResources(ctx, cl, app)
			if err != nil {
				return err
			}
			objs, err := filterManagedResources(resources, c.Flags().Changed("group"), group, kind, namespace, resourceName, true)
			if err != nil {
				return err
			}
			var actions []displayedAction
			for _, obj := range objs {
				gvk := obj.GroupVersionKind()
				available, err := cl.ListApplicationResourceActions(ctx, refOf(app, obj))
				if err != nil {
					return err
				}
				for _, a := range available.Actions {
					actions = append(actions, displayedAction{Group: gvk.Group, Kind: gvk.Kind, Name: obj.GetName(), Action: a.Name, Disabled: a.Disabled})
				}
			}
			w := c.OutOrStdout()
			switch out {
			case outYAML:
				data, err := yaml.Marshal(actions)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintln(w, string(data))
			case outJSON:
				data, err := json.MarshalIndent(actions, "", "  ")
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintln(w, string(data))
			default:
				tw := output.NewTable(w)
				tw.Row("GROUP", "KIND", "NAME", "ACTION", "DISABLED")
				for _, a := range actions {
					tw.Row(a.Group, a.Kind, a.Name, a.Action, strconv.FormatBool(a.Disabled))
				}
				_ = tw.Flush()
			}
			return nil
		},
	}
	fs := c.Flags()
	fs.StringVarP(&appNamespace, "app-namespace", "N", "", "Namespace of the application")
	fs.StringVar(&resourceName, "resource-name", "", "Name of resource")
	fs.StringVar(&kind, "kind", "", "Kind")
	fs.StringVar(&group, "group", "", "Group")
	fs.StringVar(&namespace, "namespace", "", "Namespace")
	fs.StringVarP(&out, "out", "o", "", "Output format. One of: yaml, json")
	return c
}

func newActionsRunCommand(e *env) *cobra.Command {
	var (
		namespace, appNamespace, resourceName, kind, group string
		all                                                bool
		params                                             []string
	)
	c := &cobra.Command{
		Use:   "run APPNAME ACTION",
		Short: "Runs an available action on resource(s) matching the specified filters.",
		Long:  "All filters except --kind are optional. Use --all to run the action on all matching resources if more than one resource matches the filters. Actions may only be run on resources that are represented in git and cannot be run on child resources.",
		Example: `  # Run an available action for an application
  patrol app actions run APPNAME ACTION --kind KIND [--resource-name RESOURCE] [--namespace NAMESPACE] [--group GROUP]

  # Run an action that takes parameters
  patrol app actions run my-app scale --kind Deployment --resource-name web --param replicas=3`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) != 2 {
				return usage(c, args)
			}
			actionParams, err := parseActionParams(params)
			if err != nil {
				return err
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			name, ns := parseQualifiedName(args[0], appNamespace)
			action := args[1]
			app := AppRef{Name: name, Namespace: ns}
			resources, err := actionableResources(ctx, cl, app)
			if err != nil {
				return err
			}
			objs, err := filterManagedResources(resources, c.Flags().Changed("group"), group, kind, namespace, resourceName, all)
			if err != nil {
				return err
			}
			firstGroup := objs[0].GroupVersionKind().Group
			for _, obj := range objs[1:] {
				if obj.GroupVersionKind().Group != firstGroup {
					return fatal("Ambiguous resource group. Use flag --group to specify resource group explicitly.")
				}
			}
			for _, obj := range objs {
				ref := refOf(app, obj)
				if err := cl.RunApplicationResourceAction(ctx, &apiclient.ResourceActionRunRequest{
					Name: ref.Name, AppNamespace: ref.AppNamespace, Project: ref.Project, Group: ref.Group, Version: ref.Version,
					Kind: ref.Kind, Namespace: ref.Namespace, ResourceName: ref.ResourceName, Action: action, ResourceActionParameters: actionParams,
				}); err != nil {
					return err
				}
			}
			return nil
		},
	}
	fs := c.Flags()
	fs.StringVar(&resourceName, "resource-name", "", "Name of resource on which the action should be run")
	fs.StringVarP(&appNamespace, "app-namespace", "N", "", "Namespace of the application")
	fs.StringVar(&namespace, "namespace", "", "Namespace of the resource on which the action should be run")
	fs.StringVar(&kind, "kind", "", "Kind of the resource on which the action should be run")
	fs.StringVar(&group, "group", "", "Group of the resource on which the action should be run")
	_ = c.MarkFlagRequired("kind")
	fs.BoolVar(&all, "all", false, "Indicates whether to run the action on multiple matching resources")
	fs.StringArrayVar(&params, "param", []string{}, "Action parameter as name=value (repeatable), for actions that declare parameters")
	return c
}

// parseActionParams parses --param name=value (values may contain "=").
func parseActionParams(items []string) ([]apiclient.ResourceActionParameter, error) {
	out := make([]apiclient.ResourceActionParameter, 0, len(items))
	seen := map[string]bool{}
	for _, it := range items {
		k, v, ok := strings.Cut(it, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("action parameter %q must have the form name=value", it)
		}
		if seen[k] {
			return nil, fmt.Errorf("action parameter %q given more than once", k)
		}
		seen[k] = true
		out = append(out, apiclient.ResourceActionParameter{Name: k, Value: v})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func refOf(app AppRef, obj *unstructured.Unstructured) *apiclient.ApplicationResourceRequest {
	gvk := obj.GroupVersionKind()
	return resourceRequest(app, gvk.Group, gvk.Version, gvk.Kind, obj.GetNamespace(), obj.GetName())
}
