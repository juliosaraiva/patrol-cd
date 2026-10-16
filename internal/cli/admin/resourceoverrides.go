package admin

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/health"
	"github.com/juliosaraiva/patrol-cd/internal/lua"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

func newResourceOverridesCommand(f *Factory, o *settingsOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resource-overrides",
		Short: "Troubleshoot resource overrides",
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(
		newIgnoreDifferencesCommand(f, o),
		newIgnoreResourceUpdatesCommand(f, o),
		newListActionsCommand(f, o),
		newRunActionCommand(f, o),
		newResourceHealthCommand(f, o),
	)
	return cmd
}

// overrideContext is a resource manifest with the settings' resource overrides.
type overrideContext struct {
	res       *unstructured.Unstructured
	settings  *settings.Settings
	overrides map[string]lua.ResourceOverride
	key       string
	override  lua.ResourceOverride
	found     bool
}

func (c *overrideContext) groupKind() string {
	gvk := c.res.GroupVersionKind()
	return gvk.Group + "/" + gvk.Kind
}

func loadOverrideContext(ctx context.Context, o *settingsOptions, path string) (*overrideContext, error) {
	res, err := readResourceManifest(path)
	if err != nil {
		return nil, err
	}
	s, err := o.load(ctx)
	if err != nil {
		return nil, err
	}
	oc := &overrideContext{res: res, settings: s, overrides: s.ResourceOverrides(), key: lua.OverrideKey(res.GroupVersionKind().GroupKind())}
	oc.override, oc.found = oc.overrides[oc.key]
	return oc, nil
}

// runtimeFor returns a Lua runtime over the overrides that logs nothing.
func runtimeFor(overrides map[string]lua.ResourceOverride) *lua.Runtime {
	return lua.New(overrides, lua.WithLogger(slog.New(slog.DiscardHandler)))
}

// printNormalized prints what a normalizer removed from res (upstream: a diff of the
// original and the normalized object).
func printNormalized(ctx context.Context, w io.Writer, res *unstructured.Unstructured, rules []diff.IgnoreRule, timeout time.Duration, notIgnored string) error {
	normalizer, err := diff.NewIgnoreNormalizer(rules, diff.IgnoreNormalizerOptions{JQTimeout: timeout})
	if err != nil {
		return err
	}
	normalized := res.DeepCopy()
	if err := normalizer.Normalize(ctx, normalized); err != nil {
		return err
	}
	if reflect.DeepEqual(res.Object, normalized.Object) {
		_, err := io.WriteString(w, notIgnored)
		return err
	}
	_, _ = fmt.Fprint(w, "Following fields are ignored:\n\n")
	return printObjectDiff(w, res.GetName(), res, normalized)
}

func newIgnoreDifferencesCommand(f *Factory, o *settingsOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "ignore-differences RESOURCE_YAML_PATH",
		Short: "Renders fields excluded from diffing",
		Long:  "Renders ignored fields using the 'ignoreDifferences' setting specified in the 'resource.customizations' field of 'patrol-cm' ConfigMap",
		Example: `
patrol admin settings resource-overrides ignore-differences ./deploy.yaml --patrol-cm-path ./patrol-cm.yaml`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			oc, err := loadOverrideContext(ctx, o, args[0])
			if err != nil {
				return err
			}
			w := f.out()
			d, err := oc.override.GetIgnoreDifferences()
			if err != nil {
				return err
			}
			if len(d.JSONPointers) == 0 && len(d.JQPathExpressions) == 0 {
				_, _ = fmt.Fprintf(w, "Ignore differences are not configured for '%s'\n", oc.groupKind())
				return nil
			}
			// managedFieldsManagers rules need live objects and are not evaluated here.
			return printNormalized(ctx, w, oc.res, oc.settings.SystemIgnoreRules(), 0,
				fmt.Sprintf("No fields are ignored by ignoreDifferences settings: \n%s\n", strings.TrimSpace(oc.override.IgnoreDifferences)))
		},
	}
}

func newIgnoreResourceUpdatesCommand(f *Factory, o *settingsOptions) *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "ignore-resource-updates RESOURCE_YAML_PATH",
		Short: "Renders fields excluded from resource updates",
		Long:  "Renders ignored fields using the 'ignoreResourceUpdates' setting specified in the 'resource.customizations' field of 'patrol-cm' ConfigMap",
		Example: `
patrol admin settings resource-overrides ignore-resource-updates ./deploy.yaml --patrol-cm-path ./patrol-cm.yaml`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			oc, err := loadOverrideContext(ctx, o, args[0])
			if err != nil {
				return err
			}
			w := f.out()
			gk := oc.res.GroupVersionKind().GroupKind()
			rules := oc.settings.ResourceUpdateIgnoreRules()
			var own []diff.IgnoreRule
			for _, r := range rules {
				if r.Group == gk.Group && r.Kind == gk.Kind {
					own = append(own, r)
				}
			}
			if len(own) == 0 {
				_, _ = fmt.Fprintf(w, "No overrides configured for '%s'\n", oc.groupKind())
				return nil
			}
			configured := false
			var doc strings.Builder
			for _, r := range own {
				if len(r.JSONPointers) > 0 || len(r.JQPathExpressions) > 0 {
					configured = true
				}
				b, _ := yaml.Marshal(map[string]any{"jsonPointers": r.JSONPointers, "jqPathExpressions": r.JQPathExpressions})
				doc.Write(b)
			}
			if !configured {
				_, _ = fmt.Fprintf(w, "Ignore resource updates are not configured for '%s'\n", oc.groupKind())
				return nil
			}
			return printNormalized(ctx, w, oc.res, rules, timeout,
				fmt.Sprintf("No fields are ignored by ignoreResourceUpdates settings: \n%s\n", strings.TrimSpace(doc.String())))
		},
	}
	cmd.Flags().DurationVar(&timeout, "ignore-normalizer-jq-execution-timeout", diff.DefaultJQTimeout, "Set ignore normalizer JQ execution timeout")
	return cmd
}

func newResourceHealthCommand(f *Factory, o *settingsOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "health RESOURCE_YAML_PATH",
		Short: "Assess resource health",
		Long:  "Assess resource health using the lua script configured in the 'resource.customizations' field of 'patrol-cm' ConfigMap",
		Example: `
patrol admin settings resource-overrides health ./deploy.yaml --patrol-cm-path ./patrol-cm.yaml`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			oc, err := loadOverrideContext(c.Context(), o, args[0])
			if err != nil {
				return err
			}
			hs, err := health.Assess(oc.res, runtimeFor(oc.overrides))
			if err != nil {
				return err
			}
			w := f.out()
			if hs == nil {
				_, _ = fmt.Fprintf(w, "Health script is not configured for '%s'\n", oc.groupKind())
				return nil
			}
			_, _ = fmt.Fprintf(w, "STATUS: %s\n", hs.Status)
			_, _ = fmt.Fprintf(w, "MESSAGE: %s\n", hs.Message)
			return nil
		},
	}
}

func newListActionsCommand(f *Factory, o *settingsOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list-actions RESOURCE_YAML_PATH",
		Short: "List available resource actions",
		Long:  "List actions available for given resource action using the lua scripts configured in the 'resource.customizations' field of 'patrol-cm' ConfigMap and outputs updated fields",
		Example: `
patrol admin settings resource-overrides list-actions /tmp/deploy.yaml --patrol-cm-path ./patrol-cm.yaml`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			oc, err := loadOverrideContext(ctx, o, args[0])
			if err != nil {
				return err
			}
			w := f.out()
			if oc.override.Actions == "" {
				_, _ = fmt.Fprintf(w, "Actions are not configured for '%s'\n", oc.groupKind())
				return nil
			}
			actions, err := runtimeFor(oc.overrides).ListActions(ctx, oc.res)
			if err != nil {
				return err
			}
			tw := newTable(w)
			_, _ = fmt.Fprint(tw, "NAME\tDISABLED\n")
			for _, a := range actions {
				_, _ = fmt.Fprintf(tw, "%s\t%s\n", a.Name, strconv.FormatBool(a.Disabled))
			}
			return tw.Flush()
		},
	}
}

// parseActionParams parses --param name=value entries.
func parseActionParams(params []string) ([]lua.ActionParameter, error) {
	out := make([]lua.ActionParameter, 0, len(params))
	for _, p := range params {
		name, value, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("Invalid parameter format: %s", p) //nolint:staticcheck // upstream message
		}
		out = append(out, lua.ActionParameter{Name: name, Value: value})
	}
	return out, nil
}

func newRunActionCommand(f *Factory, o *settingsOptions) *cobra.Command {
	var params []string
	cmd := &cobra.Command{
		Use:     "run-action RESOURCE_YAML_PATH ACTION",
		Aliases: []string{"action"},
		Short:   "Executes resource action",
		Long:    "Executes resource action using the lua script configured in the 'resource.customizations' field of 'patrol-cm' ConfigMap and outputs updated fields",
		Example: `
patrol admin settings resource-overrides run-action /tmp/deploy.yaml restart --patrol-cm-path ./patrol-cm.yaml`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			parsed, err := parseActionParams(params)
			if err != nil {
				return err
			}
			oc, err := loadOverrideContext(ctx, o, args[0])
			if err != nil {
				return err
			}
			w := f.out()
			if oc.override.Actions == "" {
				_, _ = fmt.Fprintf(w, "Actions are not configured for '%s'\n", oc.groupKind())
				return nil
			}
			impacted, err := runtimeFor(oc.overrides).ExecuteAction(ctx, oc.res, args[1], parsed)
			if err != nil {
				return err
			}
			for _, r := range impacted {
				switch r.Operation {
				case lua.OperationPatch:
					if reflect.DeepEqual(oc.res.Object, r.Object.Object) {
						_, _ = fmt.Fprintf(w, "No fields had been changed by action: \n%s\n", args[1])
						continue
					}
					_, _ = fmt.Fprint(w, "Following fields have been changed:\n\n")
					if err := printObjectDiff(w, oc.res.GetName(), oc.res, r.Object); err != nil {
						return err
					}
				case lua.OperationCreate:
					data, err := yaml.Marshal(r.Object.Object)
					if err != nil {
						return err
					}
					_, _ = fmt.Fprintln(w, "Following resource was created:")
					_, _ = fmt.Fprintln(w, string(data))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&params, "param", []string{}, "Action parameters (e.g. --param key1=value1)")
	return cmd
}
