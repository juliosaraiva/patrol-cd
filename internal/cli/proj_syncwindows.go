package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

func newProjectWindowsCommand(f *cmdutil.Factory) *cobra.Command {
	c := &cobra.Command{
		Use:   "windows",
		Short: "Manage a project's sync windows",
		Example: `
#Add a sync window to a project
patrol proj windows add my-project \
--schedule "0 0 * * 1-5" \
--duration 3600 \
--prune

#Delete a sync window from a project
patrol proj windows delete <project-name> <window-id>

#List project sync windows
patrol proj windows list <project-name>`,
		RunE: func(c *cobra.Command, args []string) error { return cmdutil.HelpExit(c, args) },
	}
	c.AddCommand(
		newProjectWindowsToggleCommand(f, "disable-manual-sync PROJECT ID", "Disable manual sync for a sync window", "Disable manual sync for a sync window. Requires ID which can be found by running \"patrol proj windows list PROJECT\"",
			"\n#Disable manual sync for a sync window for the Project\npatrol proj windows disable-manual-sync PROJECT ID", func(w *v1alpha1.SyncWindow) { w.ManualSync = false }),
		newProjectWindowsToggleCommand(f, "enable-manual-sync PROJECT ID", "Enable manual sync for a sync window", "Enable manual sync for a sync window. Requires ID which can be found by running \"patrol proj windows list PROJECT\"",
			"\n#Enabling manual sync for a general case\npatrol proj windows enable-manual-sync PROJECT ID", func(w *v1alpha1.SyncWindow) { w.ManualSync = true }),
		newProjectWindowsToggleCommand(f, "disable-sync-overrun PROJECT ID", "Disable sync overrun for a sync window", "Disable sync overrun for a sync window. Requires ID which can be found by running \"patrol proj windows list PROJECT\"",
			"\n#Disable sync overrun for a sync window\npatrol proj windows disable-sync-overrun PROJECT ID", func(w *v1alpha1.SyncWindow) { w.SyncOverrun = false }),
		newProjectWindowsToggleCommand(f, "enable-sync-overrun PROJECT ID", "Enable sync overrun for a sync window", "Enable sync overrun for a sync window. Requires ID which can be found by running \"patrol proj windows list PROJECT\"",
			"\n#Enable sync overrun for a sync window\npatrol proj windows enable-sync-overrun PROJECT ID", func(w *v1alpha1.SyncWindow) { w.SyncOverrun = true }),
		newProjectWindowsAddCommand(f),
		newProjectWindowsDeleteCommand(f),
		newProjectWindowsListCommand(f),
		newProjectWindowsUpdateCommand(f),
	)
	return c
}

func windowID(s string) (int, error) {
	id, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid window id %q: %w", s, err)
	}
	return id, nil
}

func windowNotFound(id int) error { return fmt.Errorf("window with id '%d' not found", id) }

func newProjectWindowsToggleCommand(f *cmdutil.Factory, use, short, long, example string, fn func(*v1alpha1.SyncWindow)) *cobra.Command {
	return &cobra.Command{
		Use: use, Short: short, Long: long, Example: example,
		Args:              cmdutil.ExactArgs(2),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			id, err := windowID(args[1])
			if err != nil {
				return err
			}
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				if id < 0 || id >= len(p.Spec.SyncWindows) {
					return false, windowNotFound(id)
				}
				fn(&p.Spec.SyncWindows[id])
				return true, nil
			})
		},
	}
}

func newProjectWindowsAddCommand(f *cmdutil.Factory) *cobra.Command {
	var (
		kind, schedule, duration, timeZone, description string
		applications, namespaces, clusters              []string
		manualSync, syncOverrun, andOperator            bool
	)
	c := &cobra.Command{
		Use:   "add PROJECT",
		Short: "Add a sync window to a project",
		Example: `
#Add a 1 hour allow sync window
patrol proj windows add PROJECT \
    --kind allow \
    --schedule "0 22 * * *" \
    --duration 1h \
    --applications "*" \
    --description "Ticket 123"

#Add a deny sync window with the ability to manually sync.
patrol proj windows add PROJECT \
    --kind deny \
    --schedule "30 10 * * *" \
    --duration 30m \
    --applications "prod-\\*,website" \
    --namespaces "default,\\*-prod" \
    --clusters "prod,staging" \
    --manual-sync \
    --sync-overrun \
    --description "Ticket 123"`,
		Args:              cmdutil.ExactArgs(1),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			if kind == "" || schedule == "" || duration == "" || len(applications)+len(namespaces)+len(clusters) == 0 {
				return errors.New("cannot create window: require kind, schedule, duration and one or more of applications, namespaces and clusters")
			}
			w := v1alpha1.SyncWindow{
				Kind: kind, Schedule: schedule, Duration: duration, ManualSync: manualSync, TimeZone: timeZone,
				UseAndOperator: andOperator, Description: description, SyncOverrun: syncOverrun,
			}
			if len(applications) > 0 {
				w.Applications = applications
			}
			if len(namespaces) > 0 {
				w.Namespaces = namespaces
			}
			if len(clusters) > 0 {
				w.Clusters = clusters
			}
			if w.TimeZone == "" {
				w.TimeZone = "UTC"
			}
			if err := project.ValidateSyncWindow(w); err != nil {
				return err
			}
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				p.Spec.SyncWindows = append(p.Spec.SyncWindows, w)
				return true, nil
			})
		},
	}
	c.Flags().StringVarP(&kind, "kind", "k", "", "Sync window kind, either allow or deny")
	c.Flags().StringVar(&schedule, "schedule", "", "Sync window schedule in cron format. (e.g. --schedule \"0 22 * * *\")")
	c.Flags().StringVar(&duration, "duration", "", "Sync window duration. (e.g. --duration 1h)")
	c.Flags().StringSliceVar(&applications, "applications", []string{}, "Applications that the schedule will be applied to. Comma separated, wildcards supported (e.g. --applications prod-\\*,website)")
	c.Flags().StringSliceVar(&namespaces, "namespaces", []string{}, "Namespaces that the schedule will be applied to. Comma separated, wildcards supported (e.g. --namespaces default,\\*-prod)")
	c.Flags().StringSliceVar(&clusters, "clusters", []string{}, "Clusters that the schedule will be applied to. Comma separated, wildcards supported (e.g. --clusters prod,staging)")
	c.Flags().BoolVar(&manualSync, "manual-sync", false, "Allow manual syncs for both deny and allow windows")
	c.Flags().BoolVar(&syncOverrun, "sync-overrun", false, "Allow syncs to continue: for deny windows, syncs that started before the window; for allow windows, syncs that started during the window")
	c.Flags().StringVar(&timeZone, "time-zone", "UTC", "Time zone of the sync window")
	c.Flags().BoolVar(&andOperator, "use-and-operator", false, "Use AND operator for matching applications, namespaces and clusters instead of the default OR operator")
	c.Flags().StringVar(&description, "description", "", "Sync window description")
	return c
}

func newProjectWindowsDeleteCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "delete PROJECT ID",
		Short: "Delete a sync window from a project. Requires ID which can be found by running \"patrol proj windows list PROJECT\"",
		Example: `
#Delete a sync window from a project (default) with ID 0
patrol proj windows delete default 0

#Delete a sync window from a project (new-project) with ID 1
patrol proj windows delete new-project 1`,
		Args:              cmdutil.ExactArgs(2),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			id, err := windowID(args[1])
			if err != nil {
				return err
			}
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				if id < 0 || id >= len(p.Spec.SyncWindows) {
					return false, windowNotFound(id)
				}
				ok, err := f.OptionalPrompt().Confirm("Are you sure you want to delete sync window? [y/n]")
				if err != nil {
					return false, err
				}
				if !ok {
					output.Println(f.Out(), "The command to delete the sync window was cancelled")
					return false, nil
				}
				p.Spec.SyncWindows = append(p.Spec.SyncWindows[:id], p.Spec.SyncWindows[id+1:]...)
				return true, nil
			})
		},
	}
}

func newProjectWindowsUpdateCommand(f *cmdutil.Factory) *cobra.Command {
	var (
		schedule, duration, timeZone, description string
		applications, namespaces, clusters        []string
	)
	c := &cobra.Command{
		Use:   "update PROJECT ID",
		Short: "Update a project sync window",
		Long:  "Update a project sync window. Requires ID which can be found by running \"patrol proj windows list PROJECT\"",
		Example: `# Change a sync window's schedule
patrol proj windows update PROJECT ID \
    --schedule "0 20 * * *"`,
		Args:              cmdutil.ExactArgs(2),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			id, err := windowID(args[1])
			if err != nil {
				return err
			}
			if schedule == "" && duration == "" && len(applications) == 0 && len(namespaces) == 0 && len(clusters) == 0 && description == "" {
				return errors.New("cannot update: require one or more of schedule, duration, application, namespace, cluster or description")
			}
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				if id < 0 || id >= len(p.Spec.SyncWindows) {
					return false, windowNotFound(id)
				}
				w := &p.Spec.SyncWindows[id]
				if schedule != "" {
					w.Schedule = schedule
				}
				if duration != "" {
					w.Duration = duration
				}
				if len(applications) > 0 {
					w.Applications = applications
				}
				if len(namespaces) > 0 {
					w.Namespaces = namespaces
				}
				if len(clusters) > 0 {
					w.Clusters = clusters
				}
				if description != "" {
					w.Description = description
				}
				w.TimeZone = timeZone
				if w.TimeZone == "" {
					w.TimeZone = "UTC"
				}
				return true, project.ValidateSyncWindow(*w)
			})
		},
	}
	c.Flags().StringVar(&schedule, "schedule", "", "Sync window schedule in cron format. (e.g. --schedule \"0 22 * * *\")")
	c.Flags().StringVar(&duration, "duration", "", "Sync window duration. (e.g. --duration 1h)")
	c.Flags().StringSliceVar(&applications, "applications", []string{}, "Applications that the schedule will be applied to. Comma separated, wildcards supported (e.g. --applications prod-\\*,website)")
	c.Flags().StringSliceVar(&namespaces, "namespaces", []string{}, "Namespaces that the schedule will be applied to. Comma separated, wildcards supported (e.g. --namespaces default,\\*-prod)")
	c.Flags().StringSliceVar(&clusters, "clusters", []string{}, "Clusters that the schedule will be applied to. Comma separated, wildcards supported (e.g. --clusters prod,staging)")
	c.Flags().StringVar(&timeZone, "time-zone", "UTC", "Time zone of the sync window. (e.g. --time-zone \"America/New_York\")")
	c.Flags().StringVar(&description, "description", "", "Sync window description")
	return c
}

func newProjectWindowsListCommand(f *cmdutil.Factory) *cobra.Command {
	var format string
	c := &cobra.Command{
		Use:   "list PROJECT",
		Short: "List project sync windows",
		Example: `
#List project windows
patrol proj windows list PROJECT

#List project windows in yaml format
patrol proj windows list PROJECT -o yaml

#List project windows info in json format
patrol proj windows list PROJECT -o json`,
		Args:              cmdutil.ExactArgs(1),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := output.CheckFormat(format, output.JSON, output.YAML, output.Wide, ""); err != nil {
				return output.UnknownFormat(format)
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			p, err := client.GetProject(c.Context(), args[0])
			if err != nil {
				return err
			}
			if format == output.JSON || format == output.YAML {
				return output.PrintResourceList(f.Out(), []v1alpha1.SyncWindow(p.Spec.SyncWindows), format, false)
			}
			t := output.NewTable(f.Out())
			t.Row("ID", "STATUS", "KIND", "SCHEDULE", "DURATION", "APPLICATIONS", "NAMESPACES", "CLUSTERS", "MANUALSYNC", "SYNCOVERRUN", "TIMEZONE", "USEANDOPERATOR")
			now := time.Now()
			for i, w := range p.Spec.SyncWindows {
				active, _ := project.SyncWindowActive(w, now)
				t.Row(strconv.Itoa(i), map[bool]string{true: "Active", false: "Inactive"}[active], w.Kind, w.Schedule, w.Duration,
					listOrDash(w.Applications), listOrDash(w.Namespaces), listOrDash(w.Clusters),
					enabled(w.ManualSync), enabled(w.SyncOverrun), w.TimeZone, enabled(w.UseAndOperator))
			}
			return t.Flush()
		},
	}
	c.Flags().StringVarP(&format, "output", "o", output.Wide, "Output format. One of: json|yaml|wide")
	return c
}

func listOrDash(l []string) string {
	if len(l) == 0 {
		return "-"
	}
	return strings.Join(l, ",")
}

func enabled(b bool) string {
	if b {
		return "Enabled"
	}
	return "Disabled"
}
