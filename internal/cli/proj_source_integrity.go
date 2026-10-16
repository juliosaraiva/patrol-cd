package cli

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

const msgNoGitPolicies = "no source integrity git policies defined for project %q"

func newProjectSourceIntegrityCommand(f *cmdutil.Factory) *cobra.Command {
	c := &cobra.Command{
		Use:   "source-integrity",
		Short: "Manage criteria for source integrity",
		Example: `  # List the git source integrity policies of project PROJECT
  patrol proj source-integrity git policies list PROJECT`,
		RunE: func(c *cobra.Command, args []string) error { return cmdutil.HelpExit(c, args) },
	}
	git := &cobra.Command{
		Use:   "git",
		Short: "Manage policies for Git repositories",
		RunE:  func(c *cobra.Command, args []string) error { return cmdutil.HelpExit(c, args) },
	}
	policies := &cobra.Command{
		Use:   "policies",
		Short: "Manage git source integrity policies",
		RunE:  func(c *cobra.Command, args []string) error { return cmdutil.HelpExit(c, args) },
	}
	policies.AddCommand(newGitPoliciesListCommand(f), newGitPoliciesAddCommand(f), newGitPoliciesDeleteCommand(f), newGitPoliciesUpdateCommand(f))
	git.AddCommand(policies)
	c.AddCommand(git)
	return c
}

func gitPolicies(p *v1alpha1.AppProject) []v1alpha1.SourceIntegrityGitPolicy {
	if p.Spec.SourceIntegrity == nil || p.Spec.SourceIntegrity.Git == nil {
		return nil
	}
	return p.Spec.SourceIntegrity.Git.Policies
}

func listGitPolicies(w io.Writer, p *v1alpha1.AppProject) {
	t := output.NewTable(w)
	t.Row("ID", "GPG-MODE", "GPG-KEYS", "REPO-URLS")
	for i, pol := range gitPolicies(p) {
		mode, keys := "<none>", "<none>"
		if pol.GPG != nil {
			mode = string(pol.GPG.Mode)
			if len(pol.GPG.Keys) > 0 {
				keys = strings.Join(pol.GPG.Keys, ", ")
			}
		}
		urls := make([]string, 0, len(pol.Repos))
		for _, r := range pol.Repos {
			urls = append(urls, r.URL)
		}
		t.Row(i, mode, keys, output.JoinOrNone(urls, ", "))
	}
	_ = t.Flush()
}

func newGitPoliciesListCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:               "list PROJECT",
		Short:             "List git source integrity policies",
		Long:              "List git source integrity policies.\n\nThe listed policy ID is used to identify given policy in delete and update commands.",
		Args:              cmdutil.ExactArgs(1),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			p, err := client.GetProject(c.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed getting project %q: %w", args[0], err)
			}
			if len(gitPolicies(p)) == 0 {
				return fmt.Errorf(msgNoGitPolicies, args[0])
			}
			listGitPolicies(f.Out(), p)
			return nil
		},
	}
}

func gpgMode(s string) (v1alpha1.SourceIntegrityGitPolicyGPGMode, error) {
	switch s {
	case "strict", "head", "none":
		return v1alpha1.SourceIntegrityGitPolicyGPGMode(s), nil
	case "":
		return "", errors.New("gpg-mode must be set")
	}
	return "", errors.New("gpg-mode must be one of: strict, head, none")
}

func normalizeKeys(keys []string) ([]string, error) {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		id, err := keyID(k)
		if err != nil {
			return nil, fmt.Errorf("invalid GPG key ID '%s': %w", k, err)
		}
		out = append(out, id)
	}
	return out, nil
}

// warnPolicy prints the upstream warnings about a policy that can never apply or
// validate, and about keys missing from the keyring.
func warnPolicy(c *cobra.Command, f *cmdutil.Factory, client *apiclient.Client, pol *v1alpha1.SourceIntegrityGitPolicy) error {
	w := f.ErrOut()
	if len(pol.Repos) == 0 {
		_, _ = fmt.Fprintln(w, "Warning: Policy has no repository URLs and will never be used")
	}
	if pol.GPG == nil {
		return nil
	}
	if len(pol.GPG.Keys) == 0 {
		_, _ = fmt.Fprintln(w, "Warning: Policy has no GPG keys and will never validate any revision")
		return nil
	}
	ring, err := client.ListGPGKeys(c.Context())
	if err != nil {
		return fmt.Errorf("failed listing GPG keys: %w", err)
	}
	var absent []string
	for _, k := range pol.GPG.Keys {
		if !slices.ContainsFunc(ring.Items, func(g apiclient.GnuPGPublicKey) bool { return strings.EqualFold(g.KeyID, k) }) && !slices.Contains(absent, k) {
			absent = append(absent, k)
		}
	}
	if len(absent) > 0 {
		slices.Sort(absent)
		output.Printf(w, "Warning: Following GPG keys are not in repo-server keyring: %s\n", strings.Join(absent, ", "))
	}
	return nil
}

func newGitPoliciesAddCommand(f *cmdutil.Factory) *cobra.Command {
	var repoURLs, gpgKeys []string
	var mode string
	c := &cobra.Command{
		Use:   "add PROJECT",
		Short: "Add a git source integrity policy",
		Example: `  # Add a policy verifying every commit of the repositories of an organization
  patrol proj source-integrity git policies add PROJECT \
    --repo-url 'https://github.com/foo/*' \
    --gpg-mode strict \
    --gpg-key D56C4FCA57A46444`,
		Args:              cmdutil.ExactArgs(1),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			m, err := gpgMode(mode)
			if err != nil {
				return err
			}
			keys, err := normalizeKeys(gpgKeys)
			if err != nil {
				return err
			}
			pol := v1alpha1.SourceIntegrityGitPolicy{GPG: &v1alpha1.SourceIntegrityGitPolicyGPG{Mode: m, Keys: keys}}
			for _, u := range repoURLs {
				pol.Repos = append(pol.Repos, v1alpha1.SourceIntegrityGitPolicyRepo{URL: u})
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			p, err := client.GetProject(c.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed getting project %q: %w", args[0], err)
			}
			if err := warnPolicy(c, f, client, &pol); err != nil {
				return err
			}
			if p.Spec.SourceIntegrity == nil {
				p.Spec.SourceIntegrity = &v1alpha1.SourceIntegrity{}
			}
			if p.Spec.SourceIntegrity.Git == nil {
				p.Spec.SourceIntegrity.Git = &v1alpha1.SourceIntegrityGit{}
			}
			p.Spec.SourceIntegrity.Git.Policies = append(p.Spec.SourceIntegrity.Git.Policies, pol)
			updated, err := client.UpdateProject(c.Context(), p)
			if err != nil {
				return fmt.Errorf("failed updating project %q: %w", args[0], err)
			}
			listGitPolicies(f.Out(), updated)
			return nil
		},
	}
	c.Flags().StringSliceVar(&repoURLs, "repo-url", []string{}, "Repository URL pattern (can be repeated)")
	c.Flags().StringVar(&mode, "gpg-mode", "", "GPG verification mode (strict, head, or none)")
	c.Flags().StringSliceVar(&gpgKeys, "gpg-key", []string{}, "GPG key ID (can be repeated)")
	return c
}

func policyIndex(s string, n int) (int, error) {
	i, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid POLICY_ID '%s'", s)
	}
	if i < 0 || i >= n {
		return 0, fmt.Errorf("the POLICY_ID %d is out of range (0-%d)", i, n-1)
	}
	return i, nil
}

func newGitPoliciesDeleteCommand(f *cmdutil.Factory) *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "delete PROJECT POLICY_ID...",
		Short: "Delete a git source integrity policy",
		Example: `  # Delete policies 0 and 2 of project PROJECT
  patrol proj source-integrity git policies delete PROJECT 0 2`,
		Args:              cmdutil.MinimumArgs(2),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			p, err := client.GetProject(c.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed getting project %q: %w", args[0], err)
			}
			pols := gitPolicies(p)
			if len(pols) == 0 {
				return fmt.Errorf(msgNoGitPolicies, args[0])
			}
			del := map[int]bool{}
			for _, s := range args[1:] {
				i, err := policyIndex(s, len(pols))
				if err != nil {
					return err
				}
				del[i] = true
			}
			if !yes {
				ids := make([]int, 0, len(del))
				for i := range del {
					ids = append(ids, i)
				}
				slices.Sort(ids)
				strs := make([]string, len(ids))
				for k, i := range ids {
					strs[k] = strconv.Itoa(i)
				}
				ok, err := f.Prompter().Confirm(fmt.Sprintf("Are you sure you want to delete policies %s from project %q? [y/N] ", strings.Join(strs, ", "), args[0]))
				if err != nil {
					// No answer (no input): fail like upstream AskToProceedS instead of
					// reporting success.
					return fmt.Errorf("confirm deletion: %w", err)
				}
				if !ok {
					output.Println(f.Out(), "Aborted by user.")
					return nil
				}
			}
			kept := make([]v1alpha1.SourceIntegrityGitPolicy, 0, len(pols)-len(del))
			for i, pol := range pols {
				if !del[i] {
					kept = append(kept, pol)
				}
			}
			p.Spec.SourceIntegrity.Git.Policies = kept
			if len(kept) == 0 {
				p.Spec.SourceIntegrity = nil
			}
			if _, err := client.UpdateProject(c.Context(), p); err != nil {
				return fmt.Errorf("failed updating project %q: %w", args[0], err)
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "Skip explicit confirmation")
	return c
}

func newGitPoliciesUpdateCommand(f *cmdutil.Factory) *cobra.Command {
	var (
		repoURLs, deleteRepoURLs, addRepoURLs []string
		gpgKeys, deleteGPGKeys, addGPGKeys    []string
		mode                                  string
		yes                                   bool
	)
	c := &cobra.Command{
		Use:   "update PROJECT POLICY_ID",
		Short: "Update a git source integrity policy",
		Example: `  # Replace the repository URL patterns of policy 0
  patrol proj source-integrity git policies update PROJECT 0 \
    --repo-url 'https://github.com/foo/*'

  # Add and delete repository URL patterns
  patrol proj source-integrity git policies update PROJECT 0 \
    --add-repo-url 'https://github.com/bar/*' \
    --delete-repo-url 'https://github.com/foo/*'

  # Change the GPG mode and add a key
  patrol proj source-integrity git policies update PROJECT 0 \
    --gpg-mode strict \
    --add-gpg-key D56C4FCA57A46444`,
		Args:              cmdutil.ExactArgs(2),
		ValidArgsFunction: CompleteProjects(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			p, err := client.GetProject(c.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed getting project %q: %w", args[0], err)
			}
			pols := gitPolicies(p)
			if len(pols) == 0 {
				return fmt.Errorf(msgNoGitPolicies, args[0])
			}
			idx, err := policyIndex(args[1], len(pols))
			if err != nil {
				return err
			}
			if len(gpgKeys) > 0 && (len(deleteGPGKeys) > 0 || len(addGPGKeys) > 0) {
				return errors.New("option --gpg-key, cannot be combined with --add-gpg-key or --delete-gpg-key")
			}
			if len(repoURLs) > 0 && (len(deleteRepoURLs) > 0 || len(addRepoURLs) > 0) {
				return errors.New("option --repo-url, cannot be combined with --add-repo-url or --delete-repo-url")
			}
			pol := &p.Spec.SourceIntegrity.Git.Policies[idx]
			if len(repoURLs) > 0 {
				pol.Repos = nil
				for _, u := range repoURLs {
					pol.Repos = append(pol.Repos, v1alpha1.SourceIntegrityGitPolicyRepo{URL: u})
				}
			}
			for _, u := range deleteRepoURLs {
				pol.Repos = slices.DeleteFunc(pol.Repos, func(r v1alpha1.SourceIntegrityGitPolicyRepo) bool { return r.URL == u })
			}
			for _, u := range addRepoURLs {
				if !slices.ContainsFunc(pol.Repos, func(r v1alpha1.SourceIntegrityGitPolicyRepo) bool { return r.URL == u }) {
					pol.Repos = append(pol.Repos, v1alpha1.SourceIntegrityGitPolicyRepo{URL: u})
				}
			}
			if pol.GPG == nil {
				pol.GPG = &v1alpha1.SourceIntegrityGitPolicyGPG{}
			}
			if mode != "" {
				m, err := gpgMode(mode)
				if err != nil {
					return err
				}
				pol.GPG.Mode = m
			} else if pol.GPG.Mode == "" {
				return errors.New("gpg-mode must be set")
			}
			if len(gpgKeys) > 0 {
				if pol.GPG.Keys, err = normalizeKeys(gpgKeys); err != nil {
					return err
				}
			}
			dels, err := normalizeKeys(deleteGPGKeys)
			if err != nil {
				return err
			}
			adds, err := normalizeKeys(addGPGKeys)
			if err != nil {
				return err
			}
			same := func(a, b string) bool {
				ka, err := keyID(a)
				return err == nil && strings.EqualFold(ka, b)
			}
			for _, k := range dels {
				pol.GPG.Keys = slices.DeleteFunc(pol.GPG.Keys, func(x string) bool { return same(x, k) })
			}
			for _, k := range adds {
				if !slices.ContainsFunc(pol.GPG.Keys, func(x string) bool { return same(x, k) }) {
					pol.GPG.Keys = append(pol.GPG.Keys, k)
				}
			}
			if err := warnPolicy(c, f, client, pol); err != nil {
				return err
			}
			listGitPolicies(f.Out(), p)
			if !yes {
				ok, err := f.Prompter().Confirm(fmt.Sprintf("Are you sure you want to update policy %d from project %q? [y/N] ", idx, args[0]))
				if err != nil {
					return fmt.Errorf("confirm update: %w", err)
				}
				if !ok {
					output.Println(f.Out(), "Aborted by user.")
					return nil
				}
			}
			if _, err := client.UpdateProject(c.Context(), p); err != nil {
				return fmt.Errorf("failed updating project %q: %w", args[0], err)
			}
			return nil
		},
	}
	c.Flags().StringVar(&mode, "gpg-mode", "", "Set GPG verification mode (strict, head, or none)")
	c.Flags().StringSliceVar(&repoURLs, "repo-url", []string{}, "Set repository URL pattern (replaces existing)")
	c.Flags().StringSliceVar(&addRepoURLs, "add-repo-url", []string{}, "Add repository URL pattern")
	c.Flags().StringSliceVar(&deleteRepoURLs, "delete-repo-url", []string{}, "Delete repository URL pattern")
	c.Flags().StringSliceVar(&gpgKeys, "gpg-key", []string{}, "Set GPG key ID (replaces existing)")
	c.Flags().StringSliceVar(&addGPGKeys, "add-gpg-key", []string{}, "Add GPG key ID")
	c.Flags().StringSliceVar(&deleteGPGKeys, "delete-gpg-key", []string{}, "Delete GPG key ID")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "Skip explicit confirmation")
	return c
}
