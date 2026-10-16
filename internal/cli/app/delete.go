package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"

	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

type deleteOptions struct {
	cascade           bool
	noPrompt          bool
	propagationPolicy string
	selector          string
	wait              bool
	appNamespace      string
}

func newDeleteCommand(e *env) *cobra.Command {
	o := &deleteOptions{}
	c := &cobra.Command{
		Use:   "delete APPNAME",
		Short: "Delete an application",
		Example: `  # Delete an app
  patrol app delete my-app

  # Delete multiple apps
  patrol app delete my-app other-app

  # Delete apps by label
  patrol app delete -l app.kubernetes.io/instance=my-app
  patrol app delete -l app.kubernetes.io/instance!=my-app
  patrol app delete -l app.kubernetes.io/instance
  patrol app delete -l '!app.kubernetes.io/instance'
  patrol app delete -l 'app.kubernetes.io/instance notin (my-app,other-app)'`,
		RunE: func(c *cobra.Command, args []string) error { return runDelete(c, e, o, args) },
	}
	fs := c.Flags()
	fs.BoolVar(&o.cascade, "cascade", true, "Perform a cascaded deletion of all application resources")
	fs.StringVarP(&o.propagationPolicy, "propagation-policy", "p", "foreground", "Specify propagation policy for deletion of application's resources. One of: foreground|background")
	fs.BoolVarP(&o.noPrompt, "yes", "y", false, "Turn off prompting to confirm cascaded deletion of application resources")
	fs.StringVarP(&o.selector, "selector", "l", "", "Delete all apps with matching label. Supports '=', '==', '!=', in, notin, exists & not exists. Matching apps must satisfy all of the specified label constraints.")
	fs.BoolVar(&o.wait, "wait", false, "Wait until deletion of the application(s) completes")
	fs.StringVarP(&o.appNamespace, "app-namespace", "N", "", "Namespace where the application will be deleted from")
	return c
}

func runDelete(c *cobra.Command, e *env, o *deleteOptions, args []string) error {
	ctx := cmdContext(c)
	if len(args) == 0 && o.selector == "" {
		return usage(c, args)
	}
	fs := c.Flags()
	if fs.Changed("propagation-policy") {
		switch strings.ToLower(o.propagationPolicy) {
		case "foreground", "background":
		default:
			return fmt.Errorf("invalid propagation policy %q: one of foreground|background", o.propagationPolicy)
		}
	}
	cl, err := e.client(ctx)
	if err != nil {
		return err
	}
	names, err := appNamesBySelector(ctx, cl, o.selector)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		names = args
	}
	out := c.OutOrStdout()
	interactive := o.cascade && e.stdoutIsTerminal(out) && !o.noPrompt
	p := cmdutil.OptionalPrompt{Enabled: interactive, P: e.prompter()}
	confirmAll := false
	for _, full := range names {
		name, ns := parseQualifiedName(full, o.appNamespace)
		req := &apiclient.ApplicationDeleteRequest{Name: name, AppNamespace: ns}
		if fs.Changed("cascade") {
			cascade := o.cascade
			req.Cascade = &cascade
		}
		if fs.Changed("propagation-policy") {
			req.PropagationPolicy = o.propagationPolicy
		}
		confirm := confirmAll
		if !confirmAll {
			single := "Are you sure you want to delete '" + full + "' and all its resources? [y/n] "
			all := "Are you sure you want to delete '" + full + "' and all its resources? [y/n/a] where 'a' is to delete all specified apps and their resources without prompting "
			var perr error
			confirm, confirmAll, perr = p.ConfirmBaseOnCount(single, all, len(names))
			if perr != nil {
				return perr
			}
		}
		if !confirm {
			_, _ = fmt.Fprintln(out, "The command to delete '"+full+"' was cancelled.")
			continue
		}
		if err := cl.DeleteApplication(ctx, req); err != nil {
			return err
		}
		if o.wait {
			if err := waitForAppDeletion(ctx, e, cl, name, ns); err != nil {
				return err
			}
		}
		_, _ = fmt.Fprintf(out, "application '%s' deleted\n", full)
	}
	return nil
}

// appNamesBySelector returns the qualified names of the Applications matching selector;
// an empty selector returns nil, a selector without matches is an error.
func appNamesBySelector(ctx context.Context, cl Client, selector string) ([]string, error) {
	if selector == "" {
		return nil, nil
	}
	apps, err := listAllApps(ctx, cl, apiclient.ApplicationQuery{Selector: selector})
	if err != nil {
		return nil, err
	}
	if len(apps) == 0 {
		return nil, fmt.Errorf("no apps match selector %v", selector)
	}
	names := make([]string, 0, len(apps))
	for i := range apps {
		names = append(names, apps[i].QualifiedName())
	}
	return names, nil
}
