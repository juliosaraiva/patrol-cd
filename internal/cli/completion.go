package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// supportedShells are the shells of `patrol completion`.
var supportedShells = []string{"bash", "zsh", "fish", "powershell"}

// NewCompletionCommand returns `patrol completion SHELL` (API-062).
func NewCompletionCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "completion SHELL",
		Short: "Output shell completion code for the specified shell (bash, zsh, fish or powershell)",
		Long: `Write shell completion code for the specified shell (bash, zsh, fish or powershell).
The shell code must be evaluated to provide interactive completion of patrol commands,
including the names of applications, projects, repositories, clusters and contexts.`,
		Example: `# For bash, ensure you have bash completions installed and enabled.
# To access completions in your current shell, run
$ source <(patrol completion bash)
# Alternatively, write it to a file and source in .bash_profile
$ patrol completion bash > ~/.patrol_completion
$ echo 'source ~/.patrol_completion' >> ~/.bash_profile

# For zsh, add the following to your ~/.zshrc file:
source <(patrol completion zsh)
compdef _patrol patrol

# For fish, write the completion to the fish completions directory:
patrol completion fish > ~/.config/fish/completions/patrol.fish

# For PowerShell, add the following to your profile:
patrol completion powershell | Out-String | Invoke-Expression`,
		ValidArgs: supportedShells,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) != 1 {
				return cmdutil.HelpExit(c, args)
			}
			root := c.Root()
			out := f.Out()
			var err error
			switch args[0] {
			case "bash":
				err = root.GenBashCompletionV2(out, true)
			case "zsh":
				err = root.GenZshCompletion(out)
			case "fish":
				err = root.GenFishCompletion(out, true)
			case "powershell":
				err = root.GenPowerShellCompletionWithDesc(out)
			default:
				return cmdutil.Fatalf("Invalid shell '%s'. The supported shells are bash, zsh, fish and powershell.", args[0])
			}
			if err != nil {
				return fmt.Errorf("generate %s completion: %w", args[0], err)
			}
			return nil
		},
	}
}

// completionTimeout bounds the API calls of dynamic completion.
const completionTimeout = 5 * time.Second

// completeFromAPI returns a ValidArgsFunction listing names from the API (nothing when
// the server is unreachable or the caller is not logged in). maxArgs limits completion
// to the first maxArgs positional arguments (0: unlimited).
func completeFromAPI(f *cmdutil.Factory, maxArgs int, list func(ctx context.Context, c *apiclient.Client) ([]string, error)) cobra.CompletionFunc {
	return func(c *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if maxArgs > 0 && len(args) >= maxArgs {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		ctx, cancel := context.WithTimeout(c.Context(), completionTimeout)
		defer cancel()
		client, err := f.NewClient(ctx)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		names, err := list(ctx, client)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		out := names[:0]
		for _, n := range names {
			if strings.HasPrefix(n, toComplete) && !slices.Contains(args, n) {
				out = append(out, n)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// CompleteProjects completes project names.
func CompleteProjects(f *cmdutil.Factory, maxArgs int) cobra.CompletionFunc {
	return completeFromAPI(f, maxArgs, func(ctx context.Context, c *apiclient.Client) ([]string, error) {
		l, err := c.ListProjects(ctx, nil)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(l.Items))
		for _, p := range l.Items {
			out = append(out, p.Name)
		}
		return out, nil
	})
}

// CompleteClusters completes cluster names and server URLs.
func CompleteClusters(f *cmdutil.Factory, maxArgs int) cobra.CompletionFunc {
	return completeFromAPI(f, maxArgs, func(ctx context.Context, c *apiclient.Client) ([]string, error) {
		l, err := c.ListClusters(ctx, nil)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, cl := range l.Items {
			if cl.Spec.Name != "" {
				out = append(out, cl.Spec.Name)
			}
			out = append(out, cl.Spec.Server)
		}
		return out, nil
	})
}

// CompleteRepositories completes repository URLs.
func CompleteRepositories(f *cmdutil.Factory, maxArgs int) cobra.CompletionFunc {
	return completeFromAPI(f, maxArgs, func(ctx context.Context, c *apiclient.Client) ([]string, error) {
		l, err := c.ListRepositories(ctx, nil)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(l.Items))
		for _, r := range l.Items {
			out = append(out, r.Spec.URL)
		}
		return out, nil
	})
}

// CompleteApplications completes Application names ("<namespace>/<name>" outside the
// control-plane namespace).
func CompleteApplications(f *cmdutil.Factory, maxArgs int) cobra.CompletionFunc {
	return completeFromAPI(f, maxArgs, func(ctx context.Context, c *apiclient.Client) ([]string, error) {
		l, err := c.ListApplications(ctx, &apiclient.ApplicationQuery{})
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(l.Items))
		for _, a := range l.Items {
			out = append(out, a.Name)
		}
		return out, nil
	})
}

// completeContexts completes the context names of the local configuration.
func completeContexts(f *cmdutil.Factory) cobra.CompletionFunc {
	return func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		cfg, err := f.ReadConfig()
		if err != nil || cfg == nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var out []string
		for _, c := range cfg.Contexts {
			if strings.HasPrefix(c.Name, toComplete) {
				out = append(out, c.Name)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}
