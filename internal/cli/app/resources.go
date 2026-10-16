package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Patch types accepted by patch-resource.
const (
	patchTypeJSONPatch      = "application/json-patch+json"
	patchTypeMergePatch     = "application/merge-patch+json"
	patchTypeStrategicMerge = "application/strategic-merge-patch+json"
)

func newResourcesCommand(e *env) *cobra.Command {
	var (
		orphaned     bool
		outputFormat string
		project      string
		appNamespace string
	)
	c := &cobra.Command{
		Use:   "resources APPNAME",
		Short: "List resources of application",
		Example: `  # List first-level resources of application
  patrol app resources my-app

  # List only the orphaned resources of application
  patrol app resources my-app --orphaned

  # Shows resource hierarchy with parent-child relationships
  patrol app resources my-app --output tree

  # Shows resource hierarchy with parent-child relationships including information about age, health and reason
  patrol app resources my-app --output tree=detailed`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) != 1 {
				return usage(c, args)
			}
			switch outputFormat {
			case "", outTree, outTreeDetailed:
			default:
				return fmt.Errorf("unknown output format: %s", outputFormat)
			}
			listAll := !c.Flags().Changed("orphaned")
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			name, ns := parseQualifiedName(args[0], appNamespace)
			tree, err := resourceTree(ctx, cl, AppRef{Name: name, Namespace: ns, Project: project})
			if err != nil {
				return err
			}
			printResourceList(c.OutOrStdout(), tree, listAll, orphaned, outputFormat, e.now())
			return nil
		},
	}
	c.Flags().BoolVar(&orphaned, "orphaned", false, "Lists only orphaned resources")
	c.Flags().StringVarP(&appNamespace, "app-namespace", "N", "", "Namespace of the application")
	c.Flags().StringVar(&outputFormat, "output", "", `Output format. One of: tree|tree=detailed.
  tree: Shows resource hierarchy with parent-child relationships
  tree=detailed: Same as tree, but includes AGE, HEALTH, and REASON columns`)
	c.Flags().StringVar(&project, "project", "", `The name of the application's project - specifying this allows the command to report "not found" instead of "permission denied" if the app does not exist`)
	return c
}

func newGetResourceCommand(e *env) *cobra.Command {
	var (
		resourceName      string
		kind              string
		group             string
		project           string
		filterFields      []string
		showManagedFields bool
		outputFormat      string
		appNamespace      string
	)
	c := &cobra.Command{
		Use:   "get-resource APPNAME",
		Short: "Get details about the live Kubernetes manifests of a resource in an application. The filter-fields flag can be used to only display fields you want to see.",
		Example: `  # Get a specific resource, Pod my-app-pod, in 'my-app' by name in wide format
  patrol app get-resource my-app --kind Pod --resource-name my-app-pod

  # Get a specific resource, Pod my-app-pod, in 'my-app' by name in yaml format
  patrol app get-resource my-app --kind Pod --resource-name my-app-pod -o yaml

  # Get details about all Pods in the application
  patrol app get-resource my-app --kind Pod

  # Get the details of multiple specific fields in a specific resource in 'my-app' in the wide format
  patrol app get-resource my-app --kind Pod --resource-name my-app-pod --filter-fields status.podIP,status.hostIP`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) != 1 {
				return usage(c, args)
			}
			switch outputFormat {
			case outWide, outJSON, outYAML:
			default:
				return fmt.Errorf("unknown output format: %s", outputFormat)
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			name, ns := parseQualifiedName(args[0], appNamespace)
			app := AppRef{Name: name, Namespace: ns, Project: project}
			tree, err := resourceTree(ctx, cl, app)
			if err != nil {
				return err
			}
			var objs []*unstructured.Unstructured
			var fetched []string
			for _, n := range tree.Nodes {
				if (resourceName != "" && n.Name != resourceName) || (group != "" && n.Group != group) || n.Kind != kind {
					continue
				}
				res, err := cl.GetApplicationResource(ctx, resourceRequest(app, n.Group, n.Version, n.Kind, n.Namespace, n.Name))
				if err != nil {
					return err
				}
				obj, err := unmarshalObject(res.Manifest)
				if err != nil {
					return err
				}
				if obj == nil {
					continue
				}
				if !showManagedFields {
					unstructured.RemoveNestedField(obj.Object, "metadata", "managedFields")
				}
				if len(filterFields) > 0 {
					obj = filterFieldsFromObject(obj, filterFields)
				}
				fetched = append(fetched, obj.GetName())
				objs = append(objs, obj)
			}
			if err := printManifests(c.OutOrStdout(), objs, len(filterFields) > 0, resourceName == "", outputFormat); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(c.ErrOrStderr(), "Resources '%s' fetched\n", strings.Join(fetched, ", "))
			return nil
		},
	}
	fs := c.Flags()
	fs.StringVarP(&appNamespace, "app-namespace", "N", "", "Namespace of the application")
	fs.StringVar(&resourceName, "resource-name", "", "Name of resource, if none is included will output details of all resources with specified kind")
	fs.StringVar(&kind, "kind", "", "Kind of resource [REQUIRED]")
	_ = c.MarkFlagRequired("kind")
	fs.StringVar(&group, "group", "", "Group")
	fs.StringVar(&project, "project", "", "Project of resource")
	fs.StringSliceVar(&filterFields, "filter-fields", nil, "A comma separated list of fields to display, if not provided will output the entire manifest")
	fs.BoolVar(&showManagedFields, "show-managed-fields", false, "Show managed fields in the output manifest")
	fs.StringVarP(&outputFormat, "output", "o", outWide, "Format of the output, wide, yaml, or json")
	return c
}

// filterFieldsFromObject keeps only the given dotted fields (lists are traversed: a field
// below a list keeps that field of every element; upstream filterFieldsFromObject).
func filterFieldsFromObject(obj *unstructured.Unstructured, fields []string) *unstructured.Unstructured {
	filtered := &unstructured.Unstructured{Object: map[string]any{}}
	for _, f := range fields {
		path := strings.Split(f, ".")
		if v, ok := extractField(obj.Object, path); ok {
			mergeInto(filtered.Object, path, v)
		}
	}
	filtered.SetName(obj.GetName())
	return filtered
}

// extractField returns the value at path; lists along the path are mapped element-wise.
func extractField(obj any, path []string) (any, bool) {
	if len(path) == 0 {
		return obj, true
	}
	switch v := obj.(type) {
	case map[string]any:
		child, ok := v[path[0]]
		if !ok {
			return nil, false
		}
		return extractField(child, path[1:])
	case []any:
		var out []any
		for _, el := range v {
			if x, ok := extractField(el, path); ok {
				out = append(out, x)
			}
		}
		return out, len(out) > 0
	}
	return nil, false
}

// mergeInto sets path in dst to v, creating maps.
func mergeInto(dst map[string]any, path []string, v any) {
	for i, p := range path {
		if i == len(path)-1 {
			dst[p] = v
			return
		}
		next, ok := dst[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			dst[p] = next
		}
		dst = next
	}
}

// printManifests prints objects as YAML/JSON documents or as FIELD/VALUE tables.
func printManifests(w io.Writer, objs []*unstructured.Unstructured, filtered, showName bool, format string) error {
	if format == outJSON || format == outYAML {
		for i, o := range objs {
			var data []byte
			var err error
			if format == outJSON {
				data, err = json.MarshalIndent(o.Object, "", " ")
			} else {
				data, err = yaml.Marshal(o.Object)
			}
			if err != nil {
				return fmt.Errorf("encode manifest: %w", err)
			}
			_, _ = fmt.Fprintln(w, string(data))
			if len(objs) > 1 && i != len(objs)-1 {
				_, _ = fmt.Fprintln(w, "---")
			}
		}
		return nil
	}
	tw := output.NewTable(w)
	if showName {
		tw.Row("FIELD", "RESOURCE NAME", "VALUE")
	} else {
		tw.Row("FIELD", "VALUE")
	}
	for _, o := range objs {
		name := o.GetName()
		obj := o.Object
		if filtered {
			obj = o.DeepCopy().Object
			unstructured.RemoveNestedField(obj, "metadata", "name")
		}
		printManifestTable(tw, name, showName, obj, "")
	}
	return tw.Flush()
}

func printManifestTable(tw *output.Tab, name string, showName bool, obj map[string]any, parent string) {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	row := func(field string, v any) {
		if showName {
			tw.Row(field, name, v)
		} else {
			tw.Row(field, v)
		}
	}
	for _, k := range keys {
		field := parent + k
		switch v := obj[k].(type) {
		case map[string]any:
			printManifestTable(tw, name, showName, v, field+".")
		case []any:
			for i, el := range v {
				idx := "[" + strconv.Itoa(i) + "]"
				if m, ok := el.(map[string]any); ok {
					printManifestTable(tw, name, showName, m, field+idx+".")
				} else {
					row(field+idx, el)
				}
			}
		default:
			row(field, v)
		}
	}
}

// filterManagedResources returns the live objects of managed resources matching the
// filters (upstream cmdutil.FilterResources). groupSet distinguishes an explicit empty group.
func filterManagedResources(managed []apiclient.ResourceDiff, groupSet bool, group, kind, namespace, name string, all bool) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for _, m := range managed {
		obj, err := unmarshalObject(m.LiveState)
		if err != nil {
			return nil, err
		}
		if obj == nil {
			continue
		}
		gvk := obj.GroupVersionKind()
		if groupSet && group != gvk.Group {
			continue
		}
		if namespace != "" && namespace != obj.GetNamespace() {
			continue
		}
		if name != "" && name != obj.GetName() {
			continue
		}
		if kind != "" && kind != gvk.Kind {
			continue
		}
		out = append(out, obj)
	}
	if len(out) == 0 {
		return nil, errors.New("no matching resource found")
	}
	if len(out) > 1 && !all {
		return nil, errors.New("multiple resources match inputs, use the --all flag to patch multiple resources")
	}
	return out, nil
}

func newPatchResourceCommand(e *env) *cobra.Command {
	var (
		patch, patchType, resourceName, namespace, kind, group, project, appNamespace string
		all                                                                           bool
	)
	c := &cobra.Command{
		Use:   "patch-resource APPNAME",
		Short: "Patch resource in an application",
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) != 1 {
				return usage(c, args)
			}
			switch patchType {
			case patchTypeJSONPatch, patchTypeMergePatch, patchTypeStrategicMerge:
			default:
				return fmt.Errorf("unknown patch type %q", patchType)
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			name, ns := parseQualifiedName(args[0], appNamespace)
			app := AppRef{Name: name, Namespace: ns, Project: project}
			managed, err := managedResources(ctx, cl, app)
			if err != nil {
				return err
			}
			objs, err := filterManagedResources(managed, c.Flags().Changed("group"), group, kind, namespace, resourceName, all)
			if err != nil {
				return err
			}
			for _, obj := range objs {
				gvk := obj.GroupVersionKind()
				if _, err := cl.PatchApplicationResource(ctx, &apiclient.ApplicationResourcePatchRequest{
					Name: app.Name, AppNamespace: app.Namespace, Project: app.Project, Group: gvk.Group, Version: gvk.Version,
					Kind: gvk.Kind, Namespace: obj.GetNamespace(), ResourceName: obj.GetName(), Patch: patch, PatchType: patchType,
				}); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(c.ErrOrStderr(), "Resource '%s' patched\n", obj.GetName())
			}
			return nil
		},
	}
	fs := c.Flags()
	fs.StringVarP(&appNamespace, "app-namespace", "N", "", "Namespace of the application")
	fs.StringVar(&patch, "patch", "", "Patch")
	_ = c.MarkFlagRequired("patch")
	fs.StringVar(&patchType, "patch-type", patchTypeMergePatch, "Which Patching strategy to use: 'application/json-patch+json', 'application/merge-patch+json', or 'application/strategic-merge-patch+json'. Defaults to 'application/merge-patch+json'")
	fs.StringVar(&resourceName, "resource-name", "", "Name of resource")
	fs.StringVar(&kind, "kind", "", "Kind")
	_ = c.MarkFlagRequired("kind")
	fs.StringVar(&group, "group", "", "Group")
	fs.StringVar(&namespace, "namespace", "", "Namespace")
	fs.BoolVar(&all, "all", false, "Indicates whether to patch multiple matching of resources")
	fs.StringVar(&project, "project", "", `The name of the application's project - specifying this allows the command to report "not found" instead of "permission denied" if the app does not exist`)
	return c
}

func newDeleteResourceCommand(e *env) *cobra.Command {
	var (
		resourceName, namespace, kind, group, project, appNamespace string
		force, orphan, all                                          bool
	)
	c := &cobra.Command{
		Use:   "delete-resource APPNAME",
		Short: "Delete resource in an application",
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
			app := AppRef{Name: name, Namespace: ns, Project: project}
			managed, err := managedResources(ctx, cl, app)
			if err != nil {
				return err
			}
			objs, err := filterManagedResources(managed, c.Flags().Changed("group"), group, kind, namespace, resourceName, all)
			if err != nil {
				return err
			}
			p := e.optionalPrompt()
			for _, obj := range objs {
				gvk := obj.GroupVersionKind()
				desc := fmt.Sprintf("%s/%s %s/%s", gvk.Group, gvk.Kind, obj.GetNamespace(), obj.GetName())
				ok, perr := p.Confirm(fmt.Sprintf("Are you sure you want to delete %s ? [y/n]", desc))
				if perr != nil {
					return perr
				}
				if !ok {
					_, _ = fmt.Fprintf(c.OutOrStdout(), "The command to delete %s was cancelled.\n", desc)
					continue
				}
				f, o := force, orphan
				if err := cl.DeleteApplicationResource(ctx, &apiclient.ApplicationResourceDeleteRequest{
					Name: app.Name, AppNamespace: app.Namespace, Project: app.Project, Group: gvk.Group, Version: gvk.Version,
					Kind: gvk.Kind, Namespace: obj.GetNamespace(), ResourceName: obj.GetName(), Force: &f, Orphan: &o,
				}); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(c.ErrOrStderr(), "Resource '%s' deleted\n", obj.GetName())
			}
			return nil
		},
	}
	fs := c.Flags()
	fs.StringVar(&resourceName, "resource-name", "", "Name of resource")
	fs.StringVarP(&appNamespace, "app-namespace", "N", "", "Namespace of the application")
	fs.StringVar(&kind, "kind", "", "Kind")
	_ = c.MarkFlagRequired("kind")
	fs.StringVar(&group, "group", "", "Group")
	fs.StringVar(&namespace, "namespace", "", "Namespace")
	fs.BoolVar(&force, "force", false, "Indicates whether to force delete the resource")
	fs.BoolVar(&orphan, "orphan", false, "Indicates whether to orphan the dependents of the deleted resource")
	fs.BoolVar(&all, "all", false, "Indicates whether to patch multiple matching of resources")
	fs.StringVar(&project, "project", "", `The name of the application's project - specifying this allows the command to report "not found" instead of "permission denied" if the app does not exist`)
	return c
}
