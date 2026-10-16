package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/notifications"
	nctrl "github.com/juliosaraiva/patrol-cd/internal/notifications/controller"
	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

// emptySecretPath selects an empty notifications Secret (upstream ":empty").
const emptySecretPath = ":empty"

// maxOperationCacheBytes bounds the Operation cache of the notifications tooling.
const maxOperationCacheBytes = 8 << 20

type notificationsOptions struct {
	kube          *kubeOptions
	configMapPath string
	secretPath    string
	repo          repoServerOptions
}

func newNotificationsCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	o := &notificationsOptions{kube: kube}
	cmd := &cobra.Command{
		Use:   "notifications",
		Short: "Set of CLI commands that helps manage notifications settings",
		Long: `Troubleshoot notifications with the configuration of the cluster (patrol-notifications-cm and
patrol-notifications-secret) or of local files (--config-map, --secret). Applications are
read from the cluster, or from a manifest file when RESOURCE_NAME is a path.

Nothing is delivered except by "template notify", and only to the given recipients: the
default recipient console:stdout prints the rendered notification.`,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	pf := cmd.PersistentFlags()
	pf.StringVar(&o.configMapPath, "config-map", "", "patrol-notifications-cm.yaml file path ('-' reads stdin)")
	pf.StringVar(&o.secretPath, "secret", "", "patrol-notifications-secret.yaml file path. Use empty secret if provided value is ':empty'")
	o.repo.bind(pf, "repo-server", "")
	pf.StringVar(&o.repo.address, "argocd-repo-server", "", "Alias of --repo-server")
	_ = pf.MarkHidden("argocd-repo-server")
	pf.BoolVar(&o.repo.plaintext, "argocd-repo-server-plaintext", false, "Alias of --repo-server-plaintext")
	_ = pf.MarkHidden("argocd-repo-server-plaintext")

	template := &cobra.Command{Use: "template", Short: "Notification templates related commands", RunE: func(c *cobra.Command, _ []string) error { return c.Help() }}
	template.AddCommand(newTemplateGetCommand(f, o), newTemplateNotifyCommand(f, o))
	trigger := &cobra.Command{Use: "trigger", Short: "Notification triggers related commands", RunE: func(c *cobra.Command, _ []string) error { return c.Help() }}
	trigger.AddCommand(newTriggerGetCommand(f, o), newTriggerRunCommand(f, o))
	cmd.AddCommand(template, trigger)
	return cmd
}

// notificationsEnv is a parsed configuration plus what the engine needs.
type notificationsEnv struct {
	dry       *nctrl.DryRun
	opts      nctrl.DryRunOptions
	kc        *kubeClients
	namespace string
}

func readConfigMapFile(path string, stdin io.Reader) (map[string]string, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = readSource("-", stdin)
	} else {
		data, err = readFileLimited(path)
	}
	if err != nil {
		return nil, err
	}
	objs, err := splitObjects(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, obj := range objs {
		if obj.GetKind() == "ConfigMap" {
			cm := &corev1.ConfigMap{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, cm); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			return cm.Data, nil
		}
	}
	return nil, fmt.Errorf("%s contains no ConfigMap", path)
}

func readSecretFile(path string) (map[string][]byte, error) {
	_, secrets, err := manifestObjects(path)
	if err != nil {
		return nil, err
	}
	if len(secrets) == 0 {
		return nil, fmt.Errorf("%s contains no Secret", path)
	}
	return secrets[0].Data, nil
}

// load parses the configuration. needCluster connects to Kubernetes even when both
// configuration objects come from files (to read Applications).
func (o *notificationsOptions) load(ctx context.Context, f *Factory, needCluster bool) (*notificationsEnv, error) {
	env := &notificationsEnv{opts: nctrl.DryRunOptions{Output: f.out(), Logger: f.logger()}}
	if needCluster || o.configMapPath == "" || o.secretPath == "" {
		kc, err := o.kube.get(nil)
		if err != nil {
			return nil, fmt.Errorf("Failed to parse k8s config: %w", err) //nolint:staticcheck // upstream message
		}
		env.kc, env.namespace = kc, kc.namespace
		env.opts.Projects = nctrl.NewProjectGetter(kc.ctrl, kc.namespace)
		env.opts.Operations = nctrl.NewOperationGetter(kc.ctrl, maxOperationCacheBytes)
	}
	var cmData map[string]string
	var secretData map[string][]byte
	var err error
	if o.configMapPath != "" {
		if cmData, err = readConfigMapFile(o.configMapPath, f.in()); err != nil {
			return nil, err
		}
	}
	switch o.secretPath {
	case emptySecretPath:
		secretData = map[string][]byte{}
	case "":
	default:
		if secretData, err = readSecretFile(o.secretPath); err != nil {
			return nil, err
		}
	}
	if o.configMapPath == "" || o.secretPath == "" {
		cm, sec, err := nctrl.LoadConfig(ctx, env.kc.ctrl, env.namespace, "", "")
		if err != nil {
			return nil, err
		}
		if o.configMapPath == "" {
			cmData = cm
		}
		if o.secretPath == "" {
			secretData = sec
		}
	}
	repo, err := o.repo.client()
	if err != nil {
		return nil, err
	}
	if repo != nil {
		env.opts.Repo = nctrl.NewRepoService(repo, nil)
	}
	if env.dry, err = nctrl.NewDryRun(cmData, secretData, env.opts); err != nil {
		return nil, err
	}
	return env, nil
}

// isLocalFile reports whether name is an existing regular file.
func isLocalFile(name string) bool {
	st, err := os.Stat(name)
	return err == nil && st.Mode().IsRegular()
}

// application reads RESOURCE_NAME: a manifest file when it exists, else the Application of
// that name in the namespace.
func (env *notificationsEnv) application(ctx context.Context, name string) (map[string]any, error) {
	if isLocalFile(name) {
		data, err := readFileLimited(name)
		if err != nil {
			return nil, err
		}
		return nctrl.ParseApplication(data)
	}
	if env.kc == nil {
		return nil, fmt.Errorf("%s is not a file and no cluster is configured", name)
	}
	return nctrl.GetApplication(ctx, env.kc.ctrl, env.namespace, name)
}

// needsClusterFor reports whether RESOURCE_NAME must be read from the cluster.
func needsClusterFor(name string) bool { return !isLocalFile(name) }

func validateListOutput(output string) error {
	switch output {
	case "wide", "name", formatJSON, formatYAML:
		return nil
	}
	return fmt.Errorf("unknown output format: %s", output)
}

// printDefinitions prints named definitions as json/yaml (one object for a single name,
// else a map by name).
func printDefinitions(w io.Writer, output string, defs map[string]any, single string) error {
	var v any = defs
	if single != "" {
		v = defs[single]
	}
	switch output {
	case formatJSON:
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(data))
		return err
	default:
		data, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
}

// preview returns the first line of s, shortened for a table.
func preview(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + "..."
	}
	if r := []rune(s); len(r) > 80 {
		s = string(r[:77]) + "..."
	}
	return s
}

func newTemplateGetCommand(f *Factory, o *notificationsOptions) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "get [NAME]",
		Short: "Prints information about configured templates",
		Example: `
# prints all templates
patrol admin notifications template get
# print YAML formatted app-sync-succeeded template definition
patrol admin notifications template get app-sync-succeeded -o=yaml
`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := validateListOutput(output); err != nil {
				return err
			}
			env, err := o.load(c.Context(), f, false)
			if err != nil {
				return err
			}
			templates := env.dry.Config().Templates
			names := sortedKeys(templates)
			single := ""
			if len(args) == 1 {
				if _, ok := templates[args[0]]; !ok {
					return fmt.Errorf("%w: template '%s' is not supported", notifications.ErrTemplateNotFound, args[0])
				}
				single, names = args[0], []string{args[0]}
			}
			w := f.out()
			switch output {
			case "name":
				for _, n := range names {
					_, _ = fmt.Fprintln(w, n)
				}
				return nil
			case "wide":
				tw := newTable(w)
				_, _ = fmt.Fprint(tw, "NAME\tPREVIEW\n")
				for _, n := range names {
					_, _ = fmt.Fprintf(tw, "%s\t%s\n", n, preview(templates[n].Message))
				}
				return tw.Flush()
			}
			defs := map[string]any{}
			for _, n := range names {
				defs[n] = templates[n]
			}
			return printDefinitions(w, output, defs, single)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "wide", "Output format. One of:json|yaml|wide|name")
	return cmd
}

func newTemplateNotifyCommand(f *Factory, o *notificationsOptions) *cobra.Command {
	var (
		recipients []string
		dryRun     bool
	)
	cmd := &cobra.Command{
		Use:   "notify NAME RESOURCE_NAME",
		Short: "Generates notification using the specified template and send it to specified recipients",
		Long: `Generates the notification of template NAME for the Application RESOURCE_NAME (a file, or an
Application of the namespace) and delivers it to each --recipient ("<service>:<recipient>").

The console service (the default recipient console:stdout) prints the notification. Other
services are only rendered and printed unless --dry-run=false is given: then they are
really contacted (upstream always contacts them).`,
		Example: `
# Render notification render generated notification in console
patrol admin notifications template notify app-sync-succeeded guestbook

# Render what would be sent to a Slack channel without sending it
patrol admin notifications template notify app-sync-succeeded ./app.yaml --recipient slack:my-slack-channel

# Trigger notification using in-cluster config map and secret
patrol admin notifications template notify app-sync-succeeded guestbook --recipient slack:my-slack-channel --dry-run=false
`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			env, err := o.load(ctx, f, needsClusterFor(args[1]))
			if err != nil {
				return err
			}
			app, err := env.application(ctx, args[1])
			if err != nil {
				return err
			}
			send := recipients
			if dryRun {
				// Console deliveries are local output; other services are only rendered.
				var console, others []string
				for _, r := range recipients {
					d, err := nctrl.ParseRecipient(r)
					if err != nil {
						return err
					}
					if d.Service == services.TypeConsole {
						console = append(console, r)
					} else {
						others = append(others, r)
					}
				}
				if len(others) > 0 {
					if err := env.renderOnly(ctx, f.out(), args[0], app, others); err != nil {
						return err
					}
					_, _ = fmt.Fprintln(f.errOut(), "note: notifications were rendered, not sent; pass --dry-run=false to send them")
				}
				if len(console) == 0 {
					return nil
				}
				send = console
			}
			results, err := env.dry.TemplateNotify(ctx, args[0], app, send)
			if err != nil {
				return err
			}
			var errs []error
			for _, r := range results {
				if r.Status == notifications.StatusFailed {
					errs = append(errs, fmt.Errorf("failed to notify '%s': %w", r.Destination, r.Err))
				}
			}
			return errors.Join(errs...)
		},
	}
	cmd.Flags().StringArrayVar(&recipients, "recipient", []string{nctrl.DefaultRecipient}, "List of recipients")
	cmd.Flags().BoolVar(&dryRun, "dry-run", true, "Render notifications for services other than console and print them instead of sending them")
	return cmd
}

// renderOnly renders template for each recipient without delivering it.
func (env *notificationsEnv) renderOnly(ctx context.Context, w io.Writer, template string, app map[string]any, recipients []string) error {
	cfg := env.dry.Config()
	if _, ok := cfg.Templates[template]; !ok {
		return fmt.Errorf("%w: template '%s' is not supported", notifications.ErrTemplateNotFound, template)
	}
	tmpl, err := nctrl.TemplateApplication(ctx, app, env.opts.Operations)
	if err != nil {
		return err
	}
	opts := []notifications.Option{notifications.WithDryRun(true), notifications.WithLogger(env.opts.Logger)}
	if env.opts.Projects != nil {
		opts = append(opts, notifications.WithProjectGetter(env.opts.Projects))
	}
	if env.opts.Repo != nil {
		opts = append(opts, notifications.WithRepoService(env.opts.Repo))
	}
	eng, err := notifications.NewEngine(cfg, opts...)
	if err != nil {
		return err
	}
	dests := make([]services.Destination, 0, len(recipients))
	var errs []error
	for _, r := range recipients {
		d, err := nctrl.ParseRecipient(r)
		if err != nil {
			return err
		}
		// A dry run does not reach the services: report unconfigured ones as a send would.
		if _, ok := cfg.Services[d.Service]; !ok {
			errs = append(errs, fmt.Errorf("failed to notify '%s': %w: notification service '%s' is not supported", d, notifications.ErrServiceNotConfigured, d.Service))
			continue
		}
		dests = append(dests, d)
	}
	results, err := eng.NotifyTemplate(ctx, tmpl, template, dests)
	if err != nil {
		return err
	}
	for _, r := range results {
		if r.Status == notifications.StatusFailed {
			errs = append(errs, fmt.Errorf("failed to render for '%s': %w", r.Destination, r.Err))
			continue
		}
		_, _ = fmt.Fprintf(w, "# %s (dry run, not sent)\n", r.Destination)
		data, err := yaml.Marshal(r.Notification)
		if err != nil {
			return err
		}
		_, _ = w.Write(data)
	}
	return errors.Join(errs...)
}

func newTriggerGetCommand(f *Factory, o *notificationsOptions) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "get [NAME]",
		Short: "Prints information about configured triggers",
		Example: `
# prints all triggers
patrol admin notifications trigger get
# print YAML formatted on-sync-failed trigger definition
patrol admin notifications trigger get on-sync-failed -o=yaml
`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := validateListOutput(output); err != nil {
				return err
			}
			env, err := o.load(c.Context(), f, false)
			if err != nil {
				return err
			}
			triggers := env.dry.Config().Triggers
			names := sortedKeys(triggers)
			single := ""
			if len(args) == 1 {
				if _, ok := triggers[args[0]]; !ok {
					return fmt.Errorf("%w: trigger with name '%s' is not defined", notifications.ErrTriggerNotFound, args[0])
				}
				single, names = args[0], []string{args[0]}
			}
			w := f.out()
			switch output {
			case "name":
				for _, n := range names {
					_, _ = fmt.Fprintln(w, n)
				}
				return nil
			case "wide":
				tw := newTable(w)
				_, _ = fmt.Fprint(tw, "NAME\tTEMPLATE\tCONDITION\n")
				for _, n := range names {
					for _, cond := range triggers[n] {
						_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", n, strings.Join(cond.Send, ", "), preview(cond.When))
					}
				}
				return tw.Flush()
			}
			defs := map[string]any{}
			for _, n := range names {
				defs[n] = triggers[n]
			}
			return printDefinitions(w, output, defs, single)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "wide", "Output format. One of:json|yaml|wide|name")
	return cmd
}

func newTriggerRunCommand(f *Factory, o *notificationsOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "run NAME RESOURCE_NAME",
		Short: "Evaluates specified trigger condition and prints the result",
		Example: `
# Execute trigger configured in 'patrol-notifications-cm' ConfigMap
patrol admin notifications trigger run on-sync-status-unknown ./sample-app.yaml

# Execute trigger using my-config-map.yaml instead of 'patrol-notifications-cm' ConfigMap
patrol admin notifications trigger run on-sync-status-unknown ./sample-app.yaml \
    --config-map ./my-config-map.yaml
`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			env, err := o.load(ctx, f, needsClusterFor(args[1]))
			if err != nil {
				return err
			}
			app, err := env.application(ctx, args[1])
			if err != nil {
				return err
			}
			results, err := env.dry.TriggerRun(ctx, args[0], app)
			if err != nil {
				return err
			}
			conds := env.dry.Config().Triggers[args[0]]
			tw := newTable(f.out())
			_, _ = fmt.Fprint(tw, "CONDITION\tRESULT\n")
			for i, r := range results {
				when := ""
				if i < len(conds) {
					when = conds[i].When
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\n", preview(when), strconv.FormatBool(r.Triggered))
				if r.Err != nil {
					_, _ = fmt.Fprintf(f.errOut(), "condition %d (%s): %v\n", i, preview(when), r.Err)
				}
			}
			return tw.Flush()
		},
	}
}
