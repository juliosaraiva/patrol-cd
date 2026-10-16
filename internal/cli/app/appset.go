package app

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func newAppSetCommand(e *env) *cobra.Command {
	c := &cobra.Command{
		Use:   "appset",
		Short: "Manage ApplicationSets",
		Example: `  # Get an ApplicationSet.
  patrol appset get APPSETNAME

  # List all the ApplicationSets
  patrol appset list

  # Create an ApplicationSet from a YAML stored in a file or at given URL
  patrol appset create <filename or URL> (<filename or URL>...)

  # Delete an ApplicationSet
  patrol appset delete APPSETNAME (APPSETNAME...)

  # Namespace precedence for --appset-namespace (-N):
  # - get/delete: if the argument is namespace/name, that namespace wins; -N is ignored.
  # - create/generate: metadata.namespace in the YAML wins when set; -N applies only when the manifest omits namespace.`,
		RunE: func(c *cobra.Command, args []string) error { return usage(c, args) },
	}
	c.AddCommand(
		newAppSetGetCommand(e),
		newAppSetCreateCommand(e),
		newAppSetListCommand(e),
		newAppSetDeleteCommand(e),
		newAppSetGenerateCommand(e),
	)
	return c
}

func appSetQualifiedName(as *v1alpha1.ApplicationSet) string { return qualify(as.Name, as.Namespace) }

func newAppSetGetCommand(e *env) *cobra.Command {
	var (
		outputFormat    string
		showParams      bool
		appSetNamespace string
	)
	c := &cobra.Command{
		Use:   "get APPSETNAME",
		Short: "Get ApplicationSet details",
		Example: `  # Get ApplicationSets
  patrol appset get APPSETNAME

  # Get ApplicationSet in a specific namespace using qualified name (namespace/name)
  patrol appset get APPSET_NAMESPACE/APPSETNAME

  # Get ApplicationSet in a specific namespace using --appset-namespace flag
  patrol appset get --appset-namespace=APPSET_NAMESPACE APPSETNAME`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) == 0 {
				return usage(c, args)
			}
			switch outputFormat {
			case outJSON, outYAML, outWide, "":
			default:
				return fmt.Errorf("unknown output format: %s", outputFormat)
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			name, ns := parseQualifiedName(args[0], appSetNamespace)
			as, err := cl.GetApplicationSet(ctx, name, ns)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if outputFormat == outJSON || outputFormat == outYAML {
				return output.PrintResource(out, as, outputFormat)
			}
			printAppSetDetails(out, as)
			if showParams {
				src := firstSource(&as.Spec.Template.Spec)
				printHelmParams(out, src.Helm)
			}
			return nil
		},
	}
	c.Flags().StringVarP(&outputFormat, "output", "o", outWide, "Output format. One of: json|yaml|wide")
	c.Flags().BoolVar(&showParams, "show-params", false, "Show ApplicationSet parameters and overrides")
	c.Flags().StringVarP(&appSetNamespace, "appset-namespace", "N", "", "Only get ApplicationSet from a namespace (ignored when qualified name is provided)")
	return c
}

// printAppSetDetails prints the summary and the conditions of an ApplicationSet.
func printAppSetDetails(w io.Writer, as *v1alpha1.ApplicationSet) {
	printAppSetSummary(w, as)
	if len(as.Status.Conditions) > 0 {
		_, _ = fmt.Fprintln(w)
		printAppSetConditions(w, as.Status.Conditions)
		_, _ = fmt.Fprintln(w)
	}
}

func printAppSetSummary(w io.Writer, as *v1alpha1.ApplicationSet) {
	spec := &as.Spec.Template.Spec
	summary(w, "Name:", appSetQualifiedName(as))
	summary(w, "Project:", spec.GetProject())
	summary(w, "Server:", appServer(spec))
	summary(w, "Namespace:", spec.Destination.Namespace)
	summary(w, "Health Status:", string(as.Status.Health.Status))
	if spec.HasMultipleSources() {
		_, _ = fmt.Fprintln(w, "Sources:")
	} else {
		_, _ = fmt.Fprintln(w, "Source:")
	}
	for _, src := range spec.GetSources() {
		printSourceDetails(w, &src)
	}
	policy := "<none>"
	if spec.IsAutomatedSyncEnabled() {
		policy = "Automated"
		if spec.SyncPolicy.Automated.GetPrune() {
			policy += " (Prune)"
		}
	}
	summary(w, "SyncPolicy:", policy)
}

func printAppSetConditions(w io.Writer, conds []metav1.Condition) {
	tw := output.NewTable(w)
	tw.Row("CONDITION", "STATUS", "MESSAGE", "LAST TRANSITION")
	for _, c := range conds {
		tw.Row(c.Type, c.Status, c.Message, c.LastTransitionTime)
	}
	_ = tw.Flush()
}

// printAppSetResources prints the Applications listed in status.resources (dry run).
func printAppSetResources(w io.Writer, resources []v1alpha1.ResourceStatus) {
	tw := output.NewTable(w)
	tw.Row("APPLICATION", "NAMESPACE", "STATUS", "HEALTH")
	for _, r := range resources {
		health := ""
		if r.Health != nil {
			health = string(r.Health.Status)
		}
		tw.Row(r.Name, r.Namespace, r.Status, health)
	}
	_ = tw.Flush()
}

// readApplicationSets reads ApplicationSets from a file or http(s) URL.
func readApplicationSets(ctx context.Context, e *env, pathOrURL string) ([]*v1alpha1.ApplicationSet, error) {
	data, err := e.readFileOrURL(ctx, pathOrURL)
	if err != nil {
		return nil, fmt.Errorf("error reading applicationset from file %s: %w", pathOrURL, err)
	}
	docs, err := splitYAMLDocuments(data)
	if err != nil {
		return nil, fmt.Errorf("error reading applicationset from file %s: %w", pathOrURL, err)
	}
	out := make([]*v1alpha1.ApplicationSet, 0, len(docs))
	for _, doc := range docs {
		var as v1alpha1.ApplicationSet
		if err := decodeStrict(doc, &as); err != nil {
			return nil, fmt.Errorf("error unmarshalling appset: %w", err)
		}
		out = append(out, &as)
	}
	return out, nil
}

// appSetChanged reports whether an upsert changed the ApplicationSet (upstream
// hasAppSetChanged).
func appSetChanged(req, res *v1alpha1.ApplicationSet, upsert bool) bool {
	if !upsert {
		return false
	}
	norm := func(m map[string]string) map[string]string {
		if len(m) == 0 {
			return nil
		}
		return m
	}
	normS := func(s []string) []string {
		if len(s) == 0 {
			return nil
		}
		return s
	}
	finalizers := func(s []string) []string {
		var out []string
		for _, f := range s {
			if f != v1alpha1.FinalizerApplicationSet {
				out = append(out, f)
			}
		}
		return normS(out)
	}
	same := reflect.DeepEqual(res.Spec, req.Spec) &&
		reflect.DeepEqual(norm(res.Labels), norm(req.Labels)) &&
		reflect.DeepEqual(norm(res.Annotations), norm(req.Annotations)) &&
		reflect.DeepEqual(finalizers(res.Finalizers), finalizers(req.Finalizers))
	return !same
}

func newAppSetCreateCommand(e *env) *cobra.Command {
	var (
		outputFormat         string
		upsert, dryRun, wait bool
		appSetNamespace      string
	)
	c := &cobra.Command{
		Use:   "create",
		Short: "Create one or more ApplicationSets",
		Example: `  # Create ApplicationSets
  patrol appset create <filename or URL> (<filename or URL>...)

  # Create ApplicationSet in a specific namespace using
  patrol appset create --appset-namespace=APPSET_NAMESPACE <filename or URL> (<filename or URL>...)

  # Dry-run AppSet creation to see what applications would be managed
  patrol appset create --dry-run <filename or URL> -o json | jq -r '.status.resources[].name'`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) == 0 {
				return usage(c, args)
			}
			switch outputFormat {
			case outJSON, outYAML, outWide, "":
			default:
				return fmt.Errorf("unknown output format: %s", outputFormat)
			}
			appSets, err := readApplicationSets(ctx, e, args[0])
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if len(appSets) == 0 {
				_, _ = fmt.Fprint(out, "No ApplicationSets found while parsing the input file")
				return silentExit(exitUsage)
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			for _, as := range appSets {
				if as.Name == "" {
					return fatalGeneric("Error creating ApplicationSet. ApplicationSet does not have Name field set")
				}
				if as.Namespace == "" && appSetNamespace != "" {
					_, _ = fmt.Fprintf(out, "ApplicationSet YAML file does not have namespace; using --appset-namespace=%q.\n", appSetNamespace)
					as.Namespace = appSetNamespace
				}
				existing, err := cl.GetApplicationSet(ctx, as.Name, as.Namespace)
				if err != nil {
					// Missing and forbidden ApplicationSets both answer 403 (no existence oracle).
					if !isNotFoundOrDenied(err) {
						return err
					}
					existing = nil
				}
				created, err := cl.CreateApplicationSet(ctx, &apiclient.ApplicationSetCreateRequest{Upsert: upsert, DryRun: dryRun, ApplicationSet: as})
				if err != nil {
					return err
				}
				action := "updated"
				switch {
				case existing == nil:
					action = "created"
				case !appSetChanged(existing, created, upsert):
					action = "unchanged"
				}
				suffix := ""
				if dryRun {
					suffix = " (dry-run)"
				}
				_, _ = fmt.Fprintf(c.ErrOrStderr(), "ApplicationSet '%s' %s%s\n", created.Name, action, suffix)
				if outputFormat == outJSON || outputFormat == outYAML {
					if err := output.PrintResource(out, created, outputFormat); err != nil {
						return err
					}
				} else {
					printAppSetDetails(out, created)
					if dryRun && len(created.Status.Resources) > 0 {
						printAppSetResources(out, created.Status.Resources)
					}
				}
				if wait && !dryRun {
					if err := waitForAppSetUpToDate(ctx, e, cl, created.Name, created.Namespace); err != nil {
						return err
					}
					_, _ = fmt.Fprintf(c.ErrOrStderr(), "ApplicationSet '%s' resources are up to date\n", created.Name)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&upsert, "upsert", false, "Allows to override ApplicationSet with the same name even if supplied ApplicationSet spec is different from existing spec")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "Allows to evaluate the ApplicationSet template on the server to get a preview of the applications that would be created")
	c.Flags().BoolVar(&wait, "wait", false, "Wait until the ApplicationSet's resources are up to date. Will block indefinitely if the ApplicationSet has errors")
	c.Flags().StringVarP(&outputFormat, "output", "o", outWide, "Output format. One of: json|yaml|wide")
	c.Flags().StringVarP(&appSetNamespace, "appset-namespace", "N", "", "Namespace where the ApplicationSet will be created in (ignored when provided YAML file has namespace set in metadata)")
	return c
}

func newAppSetGenerateCommand(e *env) *cobra.Command {
	var outputFormat, appSetNamespace string
	c := &cobra.Command{
		Use:   "generate",
		Short: "Generate apps of ApplicationSet rendered templates",
		Example: `  # Generate apps of ApplicationSet rendered templates
  patrol appset generate <filename or URL> (<filename or URL>...)

  # Generate apps of ApplicationSet rendered templates in a specific namespace
  patrol appset generate --appset-namespace=APPSET_NAMESPACE <filename or URL> (<filename or URL>...)`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) == 0 {
				return usage(c, args)
			}
			switch outputFormat {
			case outJSON, outYAML, outWide, "":
			default:
				return fmt.Errorf("unknown output format: %s", outputFormat)
			}
			appSets, err := readApplicationSets(ctx, e, args[0])
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if len(appSets) != 1 {
				_, _ = fmt.Fprint(out, "Input file must contain one ApplicationSet")
				return silentExit(exitUsage)
			}
			as := appSets[0]
			if as.Name == "" {
				return fatalGeneric("Error generating apps for ApplicationSet. ApplicationSet does not have Name field set")
			}
			if as.Namespace == "" && appSetNamespace != "" {
				_, _ = fmt.Fprintf(out, "ApplicationSet YAML file does not have namespace; using --appset-namespace=%q.\n", appSetNamespace)
				as.Namespace = appSetNamespace
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			resp, err := cl.GenerateApplicationSet(ctx, as)
			if err != nil {
				return err
			}
			apps := resp.Applications
			if outputFormat == outJSON || outputFormat == outYAML {
				items := make([]v1alpha1.Application, len(apps))
				for i := range apps {
					items[i] = apps[i]
					items[i].APIVersion = v1alpha1.GroupVersion.String()
					items[i].Kind = "Application"
				}
				return output.PrintResourceList(out, items, outputFormat, false)
			}
			printApplicationTable(out, apps, outWide)
			return nil
		},
	}
	c.Flags().StringVarP(&outputFormat, "output", "o", outWide, "Output format. One of: json|yaml|wide")
	c.Flags().StringVarP(&appSetNamespace, "appset-namespace", "N", "", "Namespace used for generating Applications (ignored when provided YAML file has namespace set in metadata)")
	return c
}

func newAppSetListCommand(e *env) *cobra.Command {
	var (
		outputFormat    string
		selector        string
		projects        []string
		appSetNamespace string
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "List ApplicationSets",
		Example: `  # List all ApplicationSets
  patrol appset list`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			ctx := cmdContext(c)
			switch outputFormat {
			case outJSON, outYAML, outName, outWide, "":
			default:
				return fmt.Errorf("unknown output format: %s", outputFormat)
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			items, err := listAllAppSets(ctx, cl, apiclient.ApplicationSetListRequest{Selector: selector, Projects: projects, AppsetNamespace: appSetNamespace})
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			switch outputFormat {
			case outJSON, outYAML:
				return output.PrintResourceList(out, items, outputFormat, false)
			case outName:
				for i := range items {
					_, _ = fmt.Fprintln(out, appSetQualifiedName(&items[i]))
				}
			default:
				printAppSetTable(out, items, outputFormat)
			}
			return nil
		},
	}
	c.Flags().StringVarP(&outputFormat, "output", "o", outWide, "Output format. One of: wide|name|json|yaml")
	c.Flags().StringVarP(&selector, "selector", "l", "", "List applicationsets by label")
	c.Flags().StringArrayVarP(&projects, "project", "p", []string{}, "Filter by project name")
	c.Flags().StringVarP(&appSetNamespace, "appset-namespace", "N", "", "Only list applicationsets in namespace")
	return c
}

// listAllAppSets lists every ApplicationSet matching req, following continue tokens.
func listAllAppSets(ctx context.Context, cl Client, req apiclient.ApplicationSetListRequest) ([]v1alpha1.ApplicationSet, error) {
	req.Limit = listPageSize
	var out []v1alpha1.ApplicationSet
	for range maxListPages {
		list, err := cl.ListApplicationSets(ctx, &req)
		if err != nil {
			return nil, err
		}
		out = append(out, list.Items...)
		if list.Continue == "" {
			return out, nil
		}
		req.Continue = list.Continue
	}
	return nil, fmt.Errorf("listing applicationsets: more than %d pages", maxListPages)
}

// formatAppSetSyncPolicy summarizes spec.syncPolicy.
func formatAppSetSyncPolicy(p *v1alpha1.ApplicationSetSyncPolicy) string {
	if p == nil {
		return "<none>"
	}
	var parts []string
	if p.ApplicationsSync != nil {
		parts = append(parts, string(*p.ApplicationsSync))
	}
	if p.PreserveResourcesOnDeletion {
		parts = append(parts, "preserve-resources-on-deletion")
	}
	if len(parts) == 0 {
		return "<none>"
	}
	return strings.Join(parts, ",")
}

// trueConditions lists the types of conditions with status True.
func trueConditions(conds []metav1.Condition) string {
	var types []string
	for _, c := range conds {
		if c.Status == metav1.ConditionTrue {
			types = append(types, c.Type)
		}
	}
	if len(types) == 0 {
		return "<none>"
	}
	sort.Strings(types)
	return strings.Join(types, ",")
}

func printAppSetTable(w io.Writer, items []v1alpha1.ApplicationSet, format string) {
	tw := output.NewTable(w)
	headers := []string{"NAME", "PROJECT", "SYNCPOLICY", "HEALTH", "CONDITIONS"}
	wide := format == outWide
	if wide {
		headers = append(headers, "REPO", "PATH", "TARGET")
	}
	tw.Row(toCells(headers)...)
	for i := range items {
		as := &items[i]
		vals := []string{
			appSetQualifiedName(as), as.Spec.Template.Spec.Project, formatAppSetSyncPolicy(as.Spec.SyncPolicy),
			string(as.Status.Health.Status), trueConditions(as.Status.Conditions),
		}
		if wide {
			src := firstSource(&as.Spec.Template.Spec)
			vals = append(vals, src.RepoURL, src.Path, src.TargetRevision)
		}
		tw.Row(toCells(vals)...)
	}
	_ = tw.Flush()
}

func newAppSetDeleteCommand(e *env) *cobra.Command {
	var (
		noPrompt          bool
		wait              bool
		cascade           bool
		propagationPolicy string
		appSetNamespace   string
	)
	c := &cobra.Command{
		Use:   "delete",
		Short: "Delete one or more ApplicationSets",
		Example: `  # Delete an applicationset
  patrol appset delete APPSETNAME (APPSETNAME...)

  # Delete ApplicationSet in a specific namespace using qualified name (namespace/name)
  patrol appset delete APPSET_NAMESPACE/APPSETNAME

  # Delete ApplicationSet in a specific namespace using --appset-namespace flag
  patrol appset delete --appset-namespace=APPSET_NAMESPACE APPSETNAME`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx := cmdContext(c)
			if len(args) == 0 {
				return usage(c, args)
			}
			if c.Flags().Changed("propagation-policy") {
				switch strings.ToLower(propagationPolicy) {
				case "foreground", "background":
				default:
					return fmt.Errorf("invalid propagation policy %q: one of foreground|background", propagationPolicy)
				}
			}
			cl, err := e.client(ctx)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			p := cmdutil.OptionalPrompt{Enabled: e.stdoutIsTerminal(out) && !noPrompt, P: e.prompter()}
			confirmAll := false
			for _, full := range args {
				name, ns := parseQualifiedName(full, appSetNamespace)
				confirm := confirmAll
				if !confirmAll {
					single := "Are you sure you want to delete '" + full + "' and all its Applications? [y/n] "
					all := "Are you sure you want to delete '" + full + "' and all its Applications? [y/n/a] where 'a' is to delete all specified ApplicationSets and their Applications without prompting"
					var perr error
					confirm, confirmAll, perr = p.ConfirmBaseOnCount(single, all, len(args))
					if perr != nil {
						return perr
					}
				}
				if !confirm {
					_, _ = fmt.Fprintln(out, "The command to delete '"+full+"' was cancelled.")
					continue
				}
				req := &apiclient.ApplicationSetDeleteRequest{Name: name, AppsetNamespace: ns}
				if c.Flags().Changed("cascade") {
					v := cascade
					req.Cascade = &v
				}
				if c.Flags().Changed("propagation-policy") {
					req.PropagationPolicy = propagationPolicy
				}
				if err := cl.DeleteApplicationSet(ctx, req); err != nil {
					return err
				}
				if wait {
					if err := waitForAppSetDeletion(ctx, e, cl, name, ns); err != nil {
						return err
					}
				}
				_, _ = fmt.Fprintf(out, "applicationset '%s' deleted\n", full)
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&noPrompt, "yes", "y", false, "Turn off prompting to confirm cascaded deletion of Application resources")
	c.Flags().BoolVar(&wait, "wait", false, "Wait until deletion of the applicationset(s) completes")
	c.Flags().BoolVar(&cascade, "cascade", true, "Delete the generated Applications (false orphans them)")
	c.Flags().StringVar(&propagationPolicy, "propagation-policy", "background", "Propagation policy for the generated Applications. One of: foreground|background")
	c.Flags().StringVarP(&appSetNamespace, "appset-namespace", "N", "", "Namespace where the ApplicationSet will be deleted from (ignored when qualified name is provided)")
	return c
}

// watchAppSet calls fn with each observed state of the ApplicationSet until it returns
// done, from the watch stream (re-opened on failures) or, without one, by polling. With
// checkGone, the ApplicationSet is read once after each stream opened: a stream reports
// nothing for an ApplicationSet deleted before it opened.
func watchAppSet(ctx context.Context, e *env, cl Client, name, ns string, checkGone bool, fn func(eventType string, as *v1alpha1.ApplicationSet) (bool, error)) error {
	gone := func() *v1alpha1.ApplicationSet {
		g := &v1alpha1.ApplicationSet{}
		g.Name, g.Namespace = name, ns
		return g
	}
	for failures := 0; failures < maxStreamFailures; failures++ {
		s, err := cl.WatchApplicationSets(ctx, &apiclient.ApplicationSetWatchRequest{Name: name, AppsetNamespace: ns}, nil)
		if err != nil {
			if fatalStreamError(err) {
				return err
			}
			if streamUnsupported(err) {
				break
			}
		} else {
			done, serr := func() (bool, error) {
				defer func() { _ = s.Close() }()
				if checkGone {
					if _, err := cl.GetApplicationSet(ctx, name, ns); err != nil && isNotFoundOrDenied(err) {
						if d, ferr := fn(apiclient.EventDeleted, gone()); d || ferr != nil {
							return true, ferr
						}
					}
				}
				for {
					ev, err := s.Next()
					if err != nil {
						if isEOF(err) {
							return false, nil
						}
						return false, err
					}
					if ev.Object.Name != name {
						continue
					}
					obj := ev.Object
					if d, ferr := fn(ev.Type, &obj); d || ferr != nil {
						return true, ferr
					}
				}
			}()
			if done {
				return serr
			}
			if serr != nil && fatalStreamError(serr) {
				return serr
			}
		}
		if err := sleepCtx(ctx, e.watchRetryDelay); err != nil {
			return err
		}
	}
	for {
		as, err := cl.GetApplicationSet(ctx, name, ns)
		switch {
		case err == nil:
			if done, ferr := fn(apiclient.EventModified, as); done || ferr != nil {
				return ferr
			}
		case isNotFoundOrDenied(err):
			if done, ferr := fn(apiclient.EventDeleted, gone()); done || ferr != nil {
				return ferr
			}
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		}
		if err := sleepCtx(ctx, e.pollInterval); err != nil {
			return err
		}
	}
}

// waitForAppSetUpToDate waits for the ResourcesUpToDate condition.
func waitForAppSetUpToDate(ctx context.Context, e *env, cl Client, name, ns string) error {
	return watchAppSet(ctx, e, cl, name, ns, false, func(eventType string, as *v1alpha1.ApplicationSet) (bool, error) {
		if eventType == apiclient.EventDeleted {
			return true, fmt.Errorf("ApplicationSet %q was deleted before reaching ResourcesUpToDate", qualify(name, ns))
		}
		for _, c := range as.Status.Conditions {
			if c.Type == v1alpha1.ApplicationSetConditionResourcesUpToDate {
				return c.Status == metav1.ConditionTrue, nil
			}
		}
		return false, nil
	})
}

// waitForAppSetDeletion waits until the ApplicationSet is gone.
func waitForAppSetDeletion(ctx context.Context, e *env, cl Client, name, ns string) error {
	if _, err := cl.GetApplicationSet(ctx, name, ns); err != nil {
		if isNotFoundOrDenied(err) {
			return nil
		}
		return err
	}
	err := watchAppSet(ctx, e, cl, name, ns, true, func(eventType string, _ *v1alpha1.ApplicationSet) (bool, error) {
		return eventType == apiclient.EventDeleted, nil
	})
	if err != nil && isNotFoundOrDenied(err) {
		return nil
	}
	return err
}
