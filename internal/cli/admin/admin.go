package admin

import (
	"github.com/spf13/cobra"
)

// Register adds the "admin" command tree to root.
func Register(root *cobra.Command, f *Factory) {
	root.AddCommand(NewCommand(f))
}

// NewCommand returns the "admin" command: administrative commands that use the Kubernetes
// API directly (kubeconfig credentials) instead of the Patrol CD API server.
func NewCommand(f *Factory) *cobra.Command {
	if f == nil {
		f = &Factory{}
	}
	kube := newKubeOptions(f)
	var logFormat, logLevel string
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Contains a set of commands useful for Patrol CD administrators and requires direct Kubernetes access",
		Example: `# Access the Patrol CD web UI through a local port-forward
$ patrol admin dashboard

# Print the initial admin password
$ patrol admin initial-password
`,
		RunE: func(c *cobra.Command, args []string) error {
			return c.Help()
		},
	}
	kube.bind(cmd.PersistentFlags())
	cmd.Flags().StringVar(&logFormat, "logformat", "json", "Set the logging format. One of: json|text")
	cmd.Flags().StringVar(&logLevel, "loglevel", "info", "Set the logging level. One of: debug|info|warn|error")

	cmd.AddCommand(
		newExportCommand(f, kube),
		newImportCommand(f, kube),
		newSettingsCommand(f, kube),
		newNotificationsCommand(f, kube),
		newClusterCommand(f, kube),
		newAppCommand(f, kube),
		newRepoCommand(f),
		newProjCommand(f, kube),
		newInitialPasswordCommand(f, kube),
		newRedisInitialPasswordCommand(f),
		newDashboardCommand(f, kube),
		newMigrateGroupCommand(f, kube),
		newArgoCDImportCommand(f, kube),
	)
	return cmd
}
