package cli

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// policyTemplate is the policy line of `proj role add-policy` (upstream).
const policyTemplate = "p, proj:%s:%s, %s, %s, %s/%s, %s"

type policyOpts struct {
	action, permission, object, resource string
}

func (o *policyOpts) addFlags(c *cobra.Command) {
	c.Flags().StringVarP(&o.action, "action", "a", "", "Action to grant/deny permission on (e.g. get, create, list, update, delete)")
	c.Flags().StringVarP(&o.permission, "permission", "p", "allow", "Whether to allow or deny access to object with the action.  This can only be 'allow' or 'deny'")
	c.Flags().StringVarP(&o.object, "object", "o", "", "Object within the project to grant/deny access.  Use '*' for a wildcard. Will want access to '<project>/<object>'")
	c.Flags().StringVarP(&o.resource, "resource", "r", "applications", "Resource e.g. 'applications', 'applicationsets', 'logs', 'exec', etc.")
}

func roleIndex(p *v1alpha1.AppProject, name string) (int, error) {
	i := slices.IndexFunc(p.Spec.Roles, func(r v1alpha1.ProjectRole) bool { return r.Name == name })
	if i < 0 {
		return -1, fmt.Errorf("role '%s' does not exist in project '%s'", name, p.Name)
	}
	return i, nil
}

func newProjectRoleCommand(f *cmdutil.Factory) *cobra.Command {
	c := &cobra.Command{
		Use:   "role",
		Short: "Manage a project's roles",
		RunE:  func(c *cobra.Command, args []string) error { return cmdutil.HelpExit(c, args) },
	}
	c.AddCommand(newProjectRoleListCommand(f), newProjectRoleGetCommand(f), newProjectRoleCreateCommand(f),
		newProjectRoleDeleteCommand(f), newProjectRoleCreateTokenCommand(f), newProjectRoleListTokensCommand(f),
		newProjectRoleDeleteTokenCommand(f), newProjectRoleAddPolicyCommand(f), newProjectRoleRemovePolicyCommand(f),
		newProjectRoleAddGroupCommand(f), newProjectRoleRemoveGroupCommand(f))
	return c
}

func newProjectRoleAddPolicyCommand(f *cmdutil.Factory) *cobra.Command {
	var o policyOpts
	c := &cobra.Command{
		Use:   "add-policy PROJECT ROLE-NAME",
		Short: "Add a policy to a project role",
		Example: `# Add a policy to the role test-role of project test-project to update applications
patrol proj role add-policy test-project test-role -a update -p allow -o project

# Add a policy to read logs
patrol proj role add-policy test-project test-role -a get -p allow -o project -r logs`,
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) != 2 || !rbac.IsProjectScoped(o.resource) {
				return cmdutil.HelpExit(c, args)
			}
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				i, err := roleIndex(p, args[1])
				if err != nil {
					return false, err
				}
				policy := fmt.Sprintf(policyTemplate, p.Name, args[1], o.resource, o.action, p.Name, o.object, o.permission)
				if slices.Contains(p.Spec.Roles[i].Policies, policy) {
					return false, nil
				}
				p.Spec.Roles[i].Policies = append(p.Spec.Roles[i].Policies, policy)
				return true, nil
			})
		},
	}
	o.addFlags(c)
	return c
}

func newProjectRoleRemovePolicyCommand(f *cmdutil.Factory) *cobra.Command {
	var o policyOpts
	c := &cobra.Command{
		Use:   "remove-policy PROJECT ROLE-NAME",
		Short: "Remove a policy from a role within a project",
		Example: `# Remove the policy granting update on applications from the role test-role
patrol proj role remove-policy test-project test-role -a update -p allow -o project`,
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) != 2 || !rbac.IsProjectScoped(o.resource) {
				return cmdutil.HelpExit(c, args)
			}
			return updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				i, err := roleIndex(p, args[1])
				if err != nil {
					return false, err
				}
				policy := fmt.Sprintf(policyTemplate, p.Name, args[1], o.resource, o.action, p.Name, o.object, o.permission)
				j := slices.Index(p.Spec.Roles[i].Policies, policy)
				if j < 0 {
					return false, nil
				}
				ok, err := f.OptionalPrompt().Confirm(fmt.Sprintf("Are you sure you want to delete '%s' policy? [y/n]", policy))
				if err != nil {
					return false, err
				}
				if !ok {
					output.Printf(f.Out(), "The command to delete policy '%s' was cancelled.\n", policy)
					return false, nil
				}
				p.Spec.Roles[i].Policies = slices.Delete(p.Spec.Roles[i].Policies, j, j+1)
				return true, nil
			})
		},
	}
	o.addFlags(c)
	return c
}

func newProjectRoleCreateCommand(f *cmdutil.Factory) *cobra.Command {
	var description string
	c := &cobra.Command{
		Use:   "create PROJECT ROLE-NAME",
		Short: "Create a project role",
		Example: `  # Create a project role in the "my-project" project with the name "my-role".
  patrol proj role create my-project my-role --description "My project role description"`,
		Args:              cmdutil.ExactArgs(2),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			role := args[1]
			created := false
			err := updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				if _, err := roleIndex(p, role); err == nil {
					output.Printf(f.Out(), "Role '%s' already exists\n", role)
					return false, nil
				}
				p.Spec.Roles = append(p.Spec.Roles, v1alpha1.ProjectRole{Name: role, Description: description})
				created = true
				return true, nil
			})
			if err == nil && created {
				output.Printf(f.Out(), "Role '%s' created\n", role)
			}
			return err
		},
	}
	c.Flags().StringVar(&description, "description", "", "Project description")
	return c
}

func newProjectRoleDeleteCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:               "delete PROJECT ROLE-NAME",
		Short:             "Delete a project role",
		Example:           `$ patrol proj role delete test-project test-role`,
		Args:              cmdutil.ExactArgs(2),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			role := args[1]
			deleted := false
			err := updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				i, err := roleIndex(p, role)
				if err != nil {
					output.Printf(f.Out(), "Role '%s' does not exist in project\n", role)
					return false, nil
				}
				ok, err := f.OptionalPrompt().Confirm(fmt.Sprintf("Are you sure you want to delete '%s' role? [y/n]", role))
				if err != nil {
					return false, err
				}
				if !ok {
					output.Printf(f.Out(), "The command to delete role '%s' was cancelled.\n", role)
					return false, nil
				}
				p.Spec.Roles = slices.Delete(p.Spec.Roles, i, i+1)
				deleted = true
				return true, nil
			})
			if err == nil && deleted {
				output.Printf(f.Out(), "Role '%s' deleted\n", role)
			}
			return err
		},
	}
}

func tokenTime(t int64) string {
	if t <= 0 {
		return "Never"
	}
	return formatUnix(t)
}

func newProjectRoleCreateTokenCommand(f *cmdutil.Factory) *cobra.Command {
	var (
		expiresIn string
		tokenOnly bool
		tokenID   string
	)
	c := &cobra.Command{
		Use:   "create-token PROJECT ROLE-NAME",
		Short: "Create a project token",
		Example: `$ patrol proj role create-token test-project test-role
Create token succeeded for proj:test-project:test-role.
  ID: f316c466-40bd-4cfd-8a8c-1392e92255d4
  Issued At: 2023-10-08T14:21:40Z
  Expires At: Never
  Token: xxx`,
		Aliases:           []string{"token-create"},
		Args:              cmdutil.ExactArgs(2),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			if expiresIn == "" {
				expiresIn = "0s"
			}
			d, err := parseDuration(expiresIn)
			if err != nil {
				return err
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			res, err := client.CreateProjectToken(c.Context(), &apiclient.ProjectTokenCreateRequest{
				Project: args[0], Role: args[1], ExpiresIn: int64(d / time.Second), ID: tokenID,
			})
			if err != nil {
				return err
			}
			// Emitting the token is the purpose of this command.
			if tokenOnly {
				_, _ = fmt.Fprintln(f.Out(), res.Token)
				return nil
			}
			claims, err := cmdutil.TokenClaims(res.Token)
			if err != nil {
				return fmt.Errorf("received malformed token %w", err)
			}
			output.Printf(f.Out(), "Create token succeeded for %s.\n", cmdutil.ClaimString(claims, "sub"))
			output.Printf(f.Out(), "  ID: %s\n  Issued At: %s\n  Expires At: %s\n", res.ID, tokenTime(res.IssuedAt), tokenTime(res.ExpiresAt))
			_, _ = fmt.Fprintln(f.Out(), "  Token: "+res.Token)
			return nil
		},
	}
	c.Flags().StringVarP(&expiresIn, "expires-in", "e", "", "Duration before the token will expire, e.g. \"12h\", \"7d\". (Default: No expiration)")
	c.Flags().StringVarP(&tokenID, "id", "i", "", "Token unique identifier. (Default: Random UUID)")
	c.Flags().BoolVarP(&tokenOnly, "token-only", "t", false, "Output token only - for use in scripts.")
	return c
}

// roleTokens returns the token records of a role: status.jwtTokensByRole plus legacy
// spec.roles[].jwtTokens entries not recorded in status.
func roleTokens(p *v1alpha1.AppProject, role string) []v1alpha1.JWTToken {
	out := slices.Clone(p.Status.JWTTokensByRole[role].Items)
	if i, err := roleIndex(p, role); err == nil {
		for _, t := range p.Spec.Roles[i].JWTTokens { //nolint:staticcheck // SA1019: legacy tokens are still listed (migration)
			if !slices.ContainsFunc(out, func(s v1alpha1.JWTToken) bool {
				return (t.ID != "" && s.ID == t.ID) || (t.ID == "" && s.IssuedAt == t.IssuedAt)
			}) {
				out = append(out, t)
			}
		}
	}
	return out
}

func newProjectRoleListTokensCommand(f *cmdutil.Factory) *cobra.Command {
	var unixTime bool
	c := &cobra.Command{
		Use:   "list-tokens PROJECT ROLE-NAME",
		Short: "List tokens for a given role.",
		Example: `$ patrol proj role list-tokens test-project test-role
ID                                      ISSUED AT                EXPIRES AT
f316c466-40bd-4cfd-8a8c-1392e92255d4    2023-10-08T14:21:40Z     Never`,
		Aliases:           []string{"list-token", "token-list"},
		Args:              cmdutil.ExactArgs(2),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			p, err := client.GetProject(c.Context(), args[0])
			if err != nil {
				return err
			}
			if _, err := roleIndex(p, args[1]); err != nil {
				return err
			}
			toks := roleTokens(p, args[1])
			if len(toks) == 0 {
				output.Printf(f.Out(), "No tokens for %s.%s\n", args[0], args[1])
				return nil
			}
			t := output.NewTable(f.Out())
			t.Row("ID", "ISSUED AT", "EXPIRES AT")
			for _, tok := range toks {
				if unixTime {
					t.Row(tok.ID, tok.IssuedAt, tok.ExpiresAt)
				} else {
					t.Row(tok.ID, tokenTime(tok.IssuedAt), tokenTime(tok.ExpiresAt))
				}
			}
			return t.Flush()
		},
	}
	c.Flags().BoolVarP(&unixTime, "unixtime", "u", false, "Print timestamps as Unix time instead of converting. Useful for piping into delete-token.")
	return c
}

func newProjectRoleDeleteTokenCommand(f *cmdutil.Factory) *cobra.Command {
	var id string
	c := &cobra.Command{
		Use:   "delete-token PROJECT ROLE-NAME ISSUED-AT",
		Short: "Delete a project token",
		Example: `# Delete the token issued at 1696769937 of the role test-role
patrol proj role delete-token test-project test-role 1696769937

# Delete a token by its ID
patrol proj role delete-token test-project test-role 0 --id c312450e-12e1-4e0d-9f65-fac9cb027b32`,
		Aliases:           []string{"token-delete", "remove-token"},
		Args:              cmdutil.ExactArgs(3),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			iat, err := strconv.ParseInt(args[2], 10, 64)
			if err != nil {
				return err
			}
			ok, err := f.OptionalPrompt().Confirm(fmt.Sprintf("Are you sure you want to delete '%s' project token? [y/n]", args[2]))
			if err != nil {
				return err
			}
			if !ok {
				output.Printf(f.Out(), "The command to delete project token '%s' was cancelled.\n", args[2])
				return nil
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			return client.DeleteProjectToken(c.Context(), args[0], args[1], iat, id)
		},
	}
	c.Flags().StringVar(&id, "id", "", "Token ID (jti); when set the token is located by ID instead of ISSUED-AT")
	return c
}

func newProjectRoleListCommand(f *cmdutil.Factory) *cobra.Command {
	var format string
	c := &cobra.Command{
		Use:   "list PROJECT",
		Short: "List all the roles in a project",
		Example: `# This command will list all the roles in argocd-project in a default table format.
patrol proj role list PROJECT

# List the roles in the project in formats like json, yaml, wide, or name.
patrol proj role list PROJECT --output json`,
		Args:              cmdutil.ExactArgs(1),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := output.CheckFormat(format, output.JSON, output.YAML, output.Wide, output.Name, ""); err != nil {
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
			switch format {
			case output.JSON, output.YAML:
				return output.PrintResourceList(f.Out(), p.Spec.Roles, format, false)
			case output.Name:
				for _, r := range p.Spec.Roles {
					output.Println(f.Out(), r.Name)
				}
				return nil
			}
			t := output.NewTable(f.Out())
			t.Row("ROLE-NAME", "DESCRIPTION")
			for _, r := range p.Spec.Roles {
				t.Row(r.Name, r.Description)
			}
			return t.Flush()
		},
	}
	c.Flags().StringVarP(&format, "output", "o", output.Wide, "Output format. One of: json|yaml|wide|name")
	return c
}

func newProjectRoleGetCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "get PROJECT ROLE-NAME",
		Short: "Get the details of a specific role",
		Example: `$ patrol proj role get test-project test-role
Role Name:     test-role
Description:
Policies:
p, proj:test-project:test-role, projects, get, test-project, allow
Groups:
<none>
JWT Tokens:
ID          ISSUED-AT                  EXPIRES-AT
1696774900  2023-10-08T14:21:40Z       <none>`,
		Args:              cmdutil.ExactArgs(2),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			p, err := client.GetProject(c.Context(), args[0])
			if err != nil {
				return err
			}
			i, err := roleIndex(p, args[1])
			if err != nil {
				return err
			}
			role := p.Spec.Roles[i]
			w := f.Out()
			const layout = "%-15s%s\n"
			output.Printf(w, layout, "Role Name:", role.Name)
			output.Printf(w, layout, "Description:", role.Description)
			_, _ = fmt.Fprintln(w, "Policies:")
			output.Printf(w, "p, proj:%s:%s, projects, get, %s, allow\n", p.Name, role.Name, p.Name)
			for _, pol := range role.Policies {
				output.Println(w, pol)
			}
			for _, g := range role.Groups {
				output.Printf(w, "g, %s, proj:%s:%s\n", g, p.Name, role.Name)
			}
			_, _ = fmt.Fprintln(w, "Groups:")
			if len(role.Groups) == 0 {
				_, _ = fmt.Fprintln(w, "<none>")
			}
			for _, g := range role.Groups {
				output.Printf(w, "  - %s\n", g)
			}
			_, _ = fmt.Fprintln(w, "JWT Tokens:")
			t := output.NewTable(w)
			t.Row("ID", "ISSUED-AT", "EXPIRES-AT")
			for _, tok := range roleTokens(p, role.Name) {
				exp := "<none>"
				if tok.ExpiresAt > 0 {
					exp = formatUnix(tok.ExpiresAt)
				}
				t.Row(tok.IssuedAt, formatUnix(tok.IssuedAt), exp)
			}
			return t.Flush()
		},
	}
}

func newProjectRoleAddGroupCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "add-group PROJECT ROLE-NAME GROUP-CLAIM",
		Short: "Add a group claim to a project role",
		Example: `  # Add a group claim to the role
  patrol proj role add-group PROJECT ROLE-NAME GROUP-CLAIM`,
		Args:              cmdutil.ExactArgs(3),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			role, group := args[1], args[2]
			added := false
			err := updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				i, err := roleIndex(p, role)
				if err != nil {
					return false, err
				}
				if slices.Contains(p.Spec.Roles[i].Groups, group) {
					output.Printf(f.Out(), "Group '%s' already present in role '%s'\n", group, role)
					return false, nil
				}
				p.Spec.Roles[i].Groups = append(p.Spec.Roles[i].Groups, group)
				added = true
				return true, nil
			})
			if err == nil && added {
				output.Printf(f.Out(), "Group '%s' added to role '%s'\n", group, role)
			}
			return err
		},
	}
}

func newProjectRoleRemoveGroupCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "remove-group PROJECT ROLE-NAME GROUP-CLAIM",
		Short: "Remove a group claim from a role within a project",
		Example: `  # Remove a group claim from the role
  patrol proj role remove-group PROJECT ROLE-NAME GROUP-CLAIM`,
		Args:              cmdutil.ExactArgs(3),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			role, group := args[1], args[2]
			removed := false
			err := updateProject(c, f, args[0], func(p *v1alpha1.AppProject) (bool, error) {
				i, err := roleIndex(p, role)
				if err != nil {
					return false, err
				}
				j := slices.Index(p.Spec.Roles[i].Groups, group)
				if j < 0 {
					output.Printf(f.Out(), "Group '%s' not present in role '%s'\n", group, role)
					return false, nil
				}
				ok, err := f.OptionalPrompt().Confirm(fmt.Sprintf("Are you sure you want to remove '%s' group from role '%s'? [y/n]", group, role))
				if err != nil {
					return false, err
				}
				if !ok {
					output.Printf(f.Out(), "The command to remove group '%s' from role '%s' was cancelled.\n", group, role)
					return false, nil
				}
				p.Spec.Roles[i].Groups = slices.Delete(p.Spec.Roles[i].Groups, j, j+1)
				removed = true
				return true, nil
			})
			if err == nil && removed {
				output.Printf(f.Out(), "Group '%s' removed from role '%s'\n", group, role)
			}
			return err
		},
	}
}
