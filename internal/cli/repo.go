package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// repoOptions are the flags of `repo add` and `repocreds add` (upstream RepoOptions).
type repoOptions struct {
	Type                              string
	Name                              string
	Project                           string
	Username                          string
	Password                          string
	BearerToken                       string
	SSHPrivateKeyPath                 string
	TLSClientCertPath                 string
	TLSClientCertKeyPath              string
	InsecureIgnoreHostKey             bool
	InsecureSkipServerVerification    bool
	EnableLFS                         bool
	EnableOCI                         bool
	GitHubAppID                       int64
	GitHubAppInstallationID           int64
	GitHubAppPrivateKeyPath           string
	GitHubAppEnterpriseBaseURL        string
	Proxy                             string
	NoProxy                           string
	GCPServiceAccountKeyPath          string
	ForceHTTPBasicAuth                bool
	UseAzureWorkloadIdentity          bool
	InsecureOCIForceHTTP              bool
	Depth                             int64
	WebhookManifestCacheWarmDisabled  bool
	AzureServicePrincipalTenantID     string
	AzureServicePrincipalClientID     string
	AzureServicePrincipalClientSecret string
	AzureActiveDirectoryEndpoint      string
	Upsert                            bool
}

// addCommonCredentialFlags registers the flags shared by repo add and repocreds add.
func (o *repoOptions) addCommonCredentialFlags(c *cobra.Command) {
	fs := c.Flags()
	fs.StringVar(&o.Username, "username", "", "username to the repository")
	fs.StringVar(&o.Password, "password", "", "password to the repository")
	fs.StringVar(&o.SSHPrivateKeyPath, "ssh-private-key-path", "", "path to the private ssh key (e.g. ~/.ssh/id_rsa)")
	fs.StringVar(&o.TLSClientCertPath, "tls-client-cert-path", "", "path to the TLS client cert (must be PEM format)")
	fs.StringVar(&o.TLSClientCertKeyPath, "tls-client-cert-key-path", "", "path to the TLS client cert's key (must be PEM format)")
	fs.Int64Var(&o.GitHubAppID, "github-app-id", 0, "id of the GitHub Application")
	fs.Int64Var(&o.GitHubAppInstallationID, "github-app-installation-id", 0, "installation id of the GitHub Application (optional, will be auto-discovered if not provided)")
	fs.StringVar(&o.GitHubAppPrivateKeyPath, "github-app-private-key-path", "", "private key of the GitHub Application")
	fs.StringVar(&o.GitHubAppEnterpriseBaseURL, "github-app-enterprise-base-url", "", "base url to use when using GitHub Enterprise (e.g. https://ghe.example.com/api/v3")
	fs.StringVar(&o.GCPServiceAccountKeyPath, "gcp-service-account-key-path", "", "service account key for the Google Cloud Platform")
	fs.BoolVar(&o.UseAzureWorkloadIdentity, "use-azure-workload-identity", false, "whether to use azure workload identity for authentication")
	fs.BoolVar(&o.InsecureOCIForceHTTP, "insecure-oci-force-http", false, "Use http when accessing an OCI repository")
	fs.StringVar(&o.AzureServicePrincipalTenantID, "azure-service-principal-tenant-id", "", "tenant id of the Azure Service Principal")
	fs.StringVar(&o.AzureServicePrincipalClientID, "azure-service-principal-client-id", "", "client id of the Azure Service Principal")
	fs.StringVar(&o.AzureServicePrincipalClientSecret, "azure-service-principal-client-secret", "", "client secret of the Azure Service Principal")
	fs.StringVar(&o.AzureActiveDirectoryEndpoint, "azure-active-directory-endpoint", "", "Active Directory endpoint when not using default Azure public cloud (e.g. https://login.microsoftonline.de)")
	fs.BoolVar(&o.Upsert, "upsert", false, "Override an existing repository with the same name even if the spec differs")
}

// credentials reads the key files and builds the write-only credentials (nil when none).
// Validation messages follow upstream.
func (o *repoOptions) credentials(f *cmdutil.Factory, url string) (*apiclient.RepositoryCredentials, error) {
	creds := &apiclient.RepositoryCredentials{Username: o.Username, Password: o.Password, BearerToken: o.BearerToken, AzureServicePrincipalClientSecret: o.AzureServicePrincipalClientSecret}
	if o.SSHPrivateKeyPath != "" {
		if ok, _ := git.IsSSHURL(url); !ok {
			return nil, errors.New("--ssh-private-key-path is only supported for SSH repositories.") //nolint:staticcheck // upstream wording
		}
		b, err := readInputFile(f, o.SSHPrivateKeyPath)
		if err != nil {
			return nil, err
		}
		creds.SSHPrivateKey = string(b)
	}
	if (o.TLSClientCertPath == "") != (o.TLSClientCertKeyPath == "") {
		return nil, errors.New("--tls-client-cert-path and --tls-client-cert-key-path must be specified together")
	}
	if o.TLSClientCertPath != "" {
		if !git.IsHTTPSURL(url) {
			return nil, errors.New("--tls-client-cert-path is only supported for HTTPS repositories")
		}
		cert, err := readInputFile(f, o.TLSClientCertPath)
		if err != nil {
			return nil, err
		}
		key, err := readInputFile(f, o.TLSClientCertKeyPath)
		if err != nil {
			return nil, err
		}
		creds.TLSClientCertData, creds.TLSClientCertKey = string(cert), string(key)
	}
	if o.GitHubAppPrivateKeyPath != "" {
		if !git.IsHTTPSURL(url) {
			return nil, errors.New("--github-app-private-key-path is only supported for HTTPS repositories")
		}
		b, err := readInputFile(f, o.GitHubAppPrivateKeyPath)
		if err != nil {
			return nil, err
		}
		creds.GitHubAppPrivateKey = string(b)
	}
	if o.GCPServiceAccountKeyPath != "" {
		if !git.IsHTTPSURL(url) {
			return nil, errors.New("--gcp-service-account-key-path is only supported for HTTPS repositories")
		}
		b, err := readInputFile(f, o.GCPServiceAccountKeyPath)
		if err != nil {
			return nil, err
		}
		creds.GCPServiceAccountKey = string(b)
	}
	return creds, nil
}

// validate applies the upstream client-side checks after the password prompt.
func (o *repoOptions) validate(url string, creds *apiclient.RepositoryCredentials) error {
	if creds.BearerToken != "" && creds.Password != "" {
		return errors.New("only --bearer-token or --password is allowed, not both")
	}
	if creds.BearerToken != "" && o.Type != string(v1alpha1.RepositoryTypeGit) {
		return errors.New("--bearer-token is only supported for Git repositories")
	}
	if creds.BearerToken != "" && !git.IsHTTPSURL(url) {
		return errors.New("--bearer-token is only supported for HTTPS repositories")
	}
	if o.InsecureOCIForceHTTP && o.Type != string(v1alpha1.RepositoryTypeOCI) && (o.Type != string(v1alpha1.RepositoryTypeHelm) || !o.EnableOCI) {
		return errors.New("--insecure-oci-force-http requires --type oci or --enable-oci")
	}
	return nil
}

// promptPassword asks for the password when only a username was given.
func (o *repoOptions) promptPassword(f *cmdutil.Factory, creds *apiclient.RepositoryCredentials) error {
	if creds.Username != "" && creds.Password == "" {
		p, err := f.Prompter().Password("Password")
		if err != nil {
			return err
		}
		creds.Password = p
	}
	return nil
}

// access builds the connection options shared by repositories and templates.
func (o *repoOptions) access(proxy string) v1alpha1.RepositoryAccess {
	a := v1alpha1.RepositoryAccess{
		EnableOCI: o.EnableOCI, InsecureOCIForceHTTP: o.InsecureOCIForceHTTP, ForceHTTPBasicAuth: o.ForceHTTPBasicAuth,
		Proxy: proxy, NoProxy: o.NoProxy, UseAzureWorkloadIdentity: o.UseAzureWorkloadIdentity,
	}
	if o.GitHubAppID != 0 || o.GitHubAppInstallationID != 0 || o.GitHubAppEnterpriseBaseURL != "" {
		a.GitHubApp = &v1alpha1.RepositoryGitHubApp{AppID: o.GitHubAppID, EnterpriseBaseURL: o.GitHubAppEnterpriseBaseURL}
		if o.GitHubAppInstallationID != 0 {
			id := o.GitHubAppInstallationID
			a.GitHubApp.InstallationID = &id
		}
	}
	if o.AzureServicePrincipalClientID != "" || o.AzureServicePrincipalTenantID != "" || o.AzureActiveDirectoryEndpoint != "" {
		a.AzureServicePrincipal = &v1alpha1.AzureServicePrincipal{
			ClientID: o.AzureServicePrincipalClientID, TenantID: o.AzureServicePrincipalTenantID, ActiveDirectoryEndpoint: o.AzureActiveDirectoryEndpoint,
		}
	}
	return a
}

func emptyCredentials(c *apiclient.RepositoryCredentials) bool {
	return c == nil || *c == apiclient.RepositoryCredentials{}
}

// NewRepoCommand returns `patrol repo` (API-070, REPO-047).
func NewRepoCommand(f *cmdutil.Factory) *cobra.Command {
	c := &cobra.Command{
		Use:   "repo",
		Short: "Manage repository connection parameters",
		RunE:  func(c *cobra.Command, args []string) error { return cmdutil.HelpExit(c, args) },
		Example: `
# Add git repository connection parameters
patrol repo add git@git.example.com:repos/repo

# Get a Configured Repository by URL
patrol repo get https://github.com/yourusername/your-repo.git

# List Configured Repositories
patrol repo list

# Remove Configured Repositories
patrol repo rm https://github.com/yourusername/your-repo.git
`,
	}
	c.AddCommand(newRepoAddCommand(f), newRepoGetCommand(f), newRepoListCommand(f), newRepoRemoveCommand(f))
	return c
}

const repoAddExamples = `  # Add a Git repository via SSH using a private key for authentication, ignoring the server's host key:
  patrol repo add git@git.example.com:repos/repo --insecure-ignore-host-key --ssh-private-key-path ~/id_rsa

  # Add a Git repository via SSH on a non-default port - need to use ssh:// style URLs here
  patrol repo add ssh://git@git.example.com:2222/repos/repo --ssh-private-key-path ~/id_rsa

  # Add a private Git repository via HTTPS using username/password and TLS client certificates:
  patrol repo add https://git.example.com/repos/repo --username git --password secret --tls-client-cert-path ~/mycert.crt --tls-client-cert-key-path ~/mycert.key

  # Add a private Git repository via HTTPS using username/password without verifying the server's TLS certificate
  patrol repo add https://git.example.com/repos/repo --username git --password secret --insecure-skip-server-verification

  # Add a public Helm repository named 'stable' via HTTPS
  patrol repo add https://charts.helm.sh/stable --type helm --name stable

  # Add a private Helm OCI-based repository named 'stable' via HTTPS
  patrol repo add helm-oci-registry.example.com --type helm --name stable --enable-oci --username test --password test

  # Add a private HTTPS OCI repository named 'stable'
  patrol repo add oci://helm-oci-registry.example.com --type oci --name stable --username test --password test

  # Add a private HTTP OCI repository named 'stable'
  patrol repo add oci://helm-oci-registry.example.com --type oci --name stable --username test --password test --insecure-oci-force-http

  # Add a private Git repository on GitHub.com via GitHub App. github-app-installation-id is optional, if not provided, the installation id will be fetched from the GitHub API.
  patrol repo add https://git.example.com/repos/repo --github-app-id 1 --github-app-installation-id 2 --github-app-private-key-path test.private-key.pem

  # Add a private Git repository on GitHub Enterprise via GitHub App.
  patrol repo add https://ghe.example.com/repos/repo --github-app-id 1 --github-app-private-key-path test.private-key.pem --github-app-enterprise-base-url https://ghe.example.com/api/v3

  # Add a private Git repository on Google Cloud Sources via GCP service account credentials
  patrol repo add https://source.developers.google.com/p/my-google-cloud-project/r/my-repo --gcp-service-account-key-path service-account-key.json

  # Add a private Git repository on Azure Devops via Azure Service Principal credentials
  patrol repo add https://dev.azure.com/my-devops-organization/my-devops-project/_git/my-devops-repo --azure-service-principal-client-id 12345678-1234-1234-1234-123456789012 --azure-service-principal-client-secret test --azure-service-principal-tenant-id 12345678-1234-1234-1234-123456789012
`

func newRepoAddCommand(f *cmdutil.Factory) *cobra.Command {
	o := &repoOptions{}
	c := &cobra.Command{
		Use:     "add REPOURL",
		Short:   "Add git, oci or helm repository connection parameters",
		Example: repoAddExamples,
		Args:    cmdutil.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			url := args[0]
			creds, err := o.credentials(f, url)
			if err != nil {
				return err
			}
			if o.Type == string(v1alpha1.RepositoryTypeHelm) && o.Name == "" {
				return errors.New("Must specify --name for repos of type 'helm'") //nolint:staticcheck // upstream wording
			}
			if err := o.validate(url, creds); err != nil {
				return err
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			if err := o.promptPassword(f, creds); err != nil {
				return err
			}
			if err := o.validate(url, creds); err != nil {
				return err
			}
			body := apiclient.RepositoryWriteRequest{
				Spec: v1alpha1.RepositorySpec{
					URL: url, Type: v1alpha1.RepositoryType(o.Type), Name: o.Name, Project: o.Project,
					Insecure: o.InsecureSkipServerVerification, InsecureIgnoreHostKey: o.InsecureIgnoreHostKey, //nolint:staticcheck // SA1019: upstream --insecure-ignore-host-key
					EnableLFS: o.EnableLFS, Depth: o.Depth, WebhookManifestCacheWarmDisabled: o.WebhookManifestCacheWarmDisabled,
					RepositoryAccess: o.access(o.Proxy),
				},
			}
			if !emptyCredentials(creds) {
				body.Credentials = creds
			}
			// The server tests access before saving (upstream ValidateAccess then Create).
			if _, err := client.ValidateRepositoryAccess(c.Context(), &apiclient.RepositoryValidateRequest{Repo: url, Body: body}); err != nil {
				return err
			}
			created, err := client.CreateRepository(c.Context(), &apiclient.RepositoryCreateRequest{Upsert: o.Upsert, Body: body})
			if err != nil {
				return err
			}
			output.Printf(f.Out(), "Repository '%s' added\n", created.Spec.URL)
			return nil
		},
	}
	fs := c.Flags()
	fs.StringVar(&o.Type, "type", string(v1alpha1.RepositoryTypeGit), "type of the repository, \"git\", \"oci\" or \"helm\"")
	fs.StringVar(&o.Name, "name", "", "name of the repository, mandatory for repositories of type helm")
	fs.StringVar(&o.Project, "project", "", "project of the repository")
	fs.StringVar(&o.BearerToken, "bearer-token", "", "bearer token to the Git BitBucket Data Center repository")
	fs.BoolVar(&o.InsecureIgnoreHostKey, "insecure-ignore-host-key", false, "disables SSH strict host key checking (deprecated, use --insecure-skip-server-verification instead)")
	fs.BoolVar(&o.InsecureSkipServerVerification, "insecure-skip-server-verification", false, "disables server certificate and host key checks")
	fs.BoolVar(&o.EnableLFS, "enable-lfs", false, "enable git-lfs (Large File Support) on this repository")
	fs.BoolVar(&o.EnableOCI, "enable-oci", false, "enable helm-oci (Helm OCI-Based Repository) (only valid for helm type repositories)")
	fs.StringVar(&o.Proxy, "proxy", "", "use proxy to access repository")
	fs.StringVar(&o.NoProxy, "no-proxy", "", "don't access these targets via proxy")
	fs.BoolVar(&o.ForceHTTPBasicAuth, "force-http-basic-auth", false, "whether to force use of basic auth when connecting repository via HTTP")
	fs.Int64Var(&o.Depth, "depth", 0, "Specify a custom depth for git clone operations. Unless specified, a full clone is performed using the depth of 0")
	fs.BoolVar(&o.WebhookManifestCacheWarmDisabled, "webhook-manifest-cache-warm-disabled", false, "disable manifest cache warming during webhook processing for this repository (recommended for large monorepos with plain YAML manifests)")
	o.addCommonCredentialFlags(c)
	return c
}

func refreshValue(refresh string) (bool, error) {
	switch refresh {
	case "":
		return false, nil
	case "hard":
		return true, nil
	}
	return false, fmt.Errorf("unknown refresh value: %s. Supported values: hard", refresh)
}

func repoCredsColumn(r apiclient.RepositoryInfo) string {
	if r.InheritedCredentials {
		return "inherited"
	}
	return strconv.FormatBool(r.HasCredentials)
}

func printRepoTable(f *cmdutil.Factory, repos []apiclient.RepositoryInfo) error {
	t := output.NewTable(f.Out())
	t.Row("TYPE", "NAME", "REPO", "INSECURE", "OCI", "LFS", "CREDS", "STATUS", "MESSAGE", "PROJECT")
	for _, r := range repos {
		typ := string(r.Spec.Type)
		if typ == "" {
			typ = string(v1alpha1.RepositoryTypeGit)
		}
		t.Row(typ, r.Spec.Name, r.Spec.URL, r.Spec.Insecure || r.Spec.InsecureIgnoreHostKey, r.Spec.EnableOCI, r.Spec.EnableLFS, //nolint:staticcheck // SA1019: upstream --insecure-ignore-host-key
			repoCredsColumn(r), r.ConnectionState.Status, r.ConnectionState.Message, r.Spec.Project)
	}
	return t.Flush()
}

func newRepoListCommand(f *cmdutil.Factory) *cobra.Command {
	var format, refresh string
	c := &cobra.Command{
		Use:   "list",
		Short: "List configured repositories",
		Example: `
  # List all repositories
  patrol repo list

  # List repositories in wide format
  patrol repo list -o wide

  # List repositories in YAML format
  patrol repo list -o yaml

  # List repositories in JSON format
  patrol repo list -o json

  # List urls of repositories
  patrol repo list -o url

  # Force refresh of cached repository connection status
  patrol repo list --refresh hard
`,
		RunE: func(c *cobra.Command, _ []string) error {
			force, err := refreshValue(refresh)
			if err != nil {
				return err
			}
			if err := output.CheckFormat(format, output.YAML, output.JSON, output.URL, output.Wide, ""); err != nil {
				return fmt.Errorf("unknown output format: %s. Supported formats: yaml|json|url|wide", format)
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			l, err := client.ListRepositories(c.Context(), &apiclient.RepositoryListRequest{ForceRefresh: force})
			if err != nil {
				return err
			}
			switch format {
			case output.JSON, output.YAML:
				return output.PrintResourceList(f.Out(), l.Items, format, false)
			case output.URL:
				for _, r := range l.Items {
					output.Println(f.Out(), r.Spec.URL)
				}
				return nil
			}
			return printRepoTable(f, l.Items)
		},
	}
	c.Flags().StringVarP(&format, "output", "o", output.Wide, "Output format. Supported formats: yaml|json|url|wide")
	c.Flags().StringVar(&refresh, "refresh", "", "Force a cache refresh on connection status. Supported values: hard")
	return c
}

func newRepoGetCommand(f *cmdutil.Factory) *cobra.Command {
	var format, refresh, project string
	c := &cobra.Command{
		Use:   "get REPO",
		Short: "Get a configured repository by URL",
		Example: `
  # Get Git or Helm repository details in wide format (default, '-o wide')
  patrol repo get https://git.example.com/repos/repo

  # Get repository details in YAML format
  patrol repo get https://git.example.com/repos/repo -o yaml

  # Get repository details in JSON format
  patrol repo get https://git.example.com/repos/repo -o json

  # Get repository URL
  patrol repo get https://git.example.com/repos/repo -o url
`,
		Args:              cmdutil.ExactArgs(1),
		ValidArgsFunction: CompleteRepositories(f, 1),
		RunE: func(c *cobra.Command, args []string) error {
			force, err := refreshValue(refresh)
			if err != nil {
				return err
			}
			if err := output.CheckFormat(format, output.YAML, output.JSON, output.URL, output.Wide, ""); err != nil {
				return fmt.Errorf("unknown output format: %s. Supported formats: yaml|json|url|wide", format)
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			r, err := client.GetRepository(c.Context(), &apiclient.RepositoryQuery{Repo: args[0], AppProject: project, ForceRefresh: force})
			if err != nil {
				return err
			}
			switch format {
			case output.JSON, output.YAML:
				return output.PrintResource(f.Out(), r, format)
			case output.URL:
				output.Println(f.Out(), r.Spec.URL)
				return nil
			}
			return printRepoTable(f, []apiclient.RepositoryInfo{*r})
		},
	}
	c.Flags().StringVar(&project, "project", "", "project of the repository")
	c.Flags().StringVarP(&format, "output", "o", output.Wide, "Output format. One of: json|yaml|wide|url")
	c.Flags().StringVar(&refresh, "refresh", "", "Force a cache refresh on connection status. Supported values: hard")
	return c
}

func newRepoRemoveCommand(f *cmdutil.Factory) *cobra.Command {
	var project string
	c := &cobra.Command{
		Use:   "rm REPO ...",
		Short: "Remove configured repositories",
		Example: `
  # Remove a single repository
  patrol repo rm https://github.com/yourusername/your-repo.git

  # Remove multiple repositories
  patrol repo rm https://github.com/yourusername/your-repo.git https://git.example.com/repo2.git

  # Remove repositories for a specific project
  patrol repo rm https://github.com/yourusername/your-repo.git --project myproject

  # Remove repository using SSH URL
  patrol repo rm git@github.com:yourusername/your-repo.git
`,
		Args:              cmdutil.MinimumArgs(1),
		ValidArgsFunction: CompleteRepositories(f, 0),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			p := f.OptionalPrompt()
			for _, url := range args {
				ok, err := p.Confirm(fmt.Sprintf("Are you sure you want to delete repository '%s'? [y/n]", url))
				if err != nil {
					return err
				}
				if !ok {
					output.Printf(f.Out(), "The command to delete '%s' was cancelled.\n", url)
					continue
				}
				if err := client.DeleteRepository(c.Context(), &apiclient.RepositoryQuery{Repo: url, AppProject: project}); err != nil {
					return err
				}
				output.Printf(f.Out(), "Repository '%s' removed\n", url)
			}
			return nil
		},
	}
	c.Flags().StringVar(&project, "project", "", "project of the repository")
	return c
}

// NewRepoCredsCommand returns `patrol repocreds` (API-071, REPO-048).
func NewRepoCredsCommand(f *cmdutil.Factory) *cobra.Command {
	c := &cobra.Command{
		Use:   "repocreds",
		Short: "Manage credential templates for repositories",
		RunE:  func(c *cobra.Command, args []string) error { return cmdutil.HelpExit(c, args) },
		Example: `
# Add credentials with user/pass authentication to use for all repositories under the specified URL
patrol repocreds add URL --username USERNAME --password PASSWORD

# List all the configured repository credentials
patrol repocreds list

# Remove credentials for the repositories with specified URL
patrol repocreds rm URL
`,
	}
	c.AddCommand(newRepoCredsAddCommand(f), newRepoCredsListCommand(f), newRepoCredsRemoveCommand(f))
	return c
}

const repoCredsAddExamples = `  # Add credentials with user/pass authentication to use for all repositories under https://git.example.com/repos
  patrol repocreds add https://git.example.com/repos/ --username git --password secret

  # Add credentials with bearer token authentication to use for all BitBucket Data Center repositories under https://bitbucket.example.com/scm
  patrol repocreds add https://bitbucket.example.com/scm --bearer-token secret-token

  # Add credentials with SSH private key authentication to use for all repositories under ssh://git@git.example.com/repos
  patrol repocreds add ssh://git@git.example.com/repos/ --ssh-private-key-path ~/.ssh/id_rsa

  # Add credentials with GitHub App authentication to use for all repositories under https://github.com/repos
  patrol repocreds add https://github.com/repos/ --github-app-id 1 --github-app-installation-id 2 --github-app-private-key-path test.private-key.pem

  # Add credentials with helm oci registry so that these oci registry urls do not need to be added as repos individually.
  patrol repocreds add localhost:5000/myrepo --enable-oci --type helm

  # Add credentials with GCP credentials for all repositories under https://source.developers.google.com/p/my-google-cloud-project/r/
  patrol repocreds add https://source.developers.google.com/p/my-google-cloud-project/r/ --gcp-service-account-key-path service-account-key.json

  # Add credentials with Azure Service Principal to use for all repositories under https://dev.azure.com/my-devops-organization
  patrol repocreds add https://dev.azure.com/my-devops-organization --azure-service-principal-client-id 12345678-1234-1234-1234-123456789012 --azure-service-principal-client-secret test --azure-service-principal-tenant-id 12345678-1234-1234-1234-123456789012
`

func newRepoCredsAddCommand(f *cmdutil.Factory) *cobra.Command {
	o := &repoOptions{}
	c := &cobra.Command{
		Use:     "add REPOURL",
		Short:   "Add git repository connection parameters",
		Example: repoCredsAddExamples,
		Args:    cmdutil.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			url := args[0]
			creds, err := o.credentials(f, url)
			if err != nil {
				return err
			}
			if err := o.validate(url, creds); err != nil {
				return err
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			if err := o.promptPassword(f, creds); err != nil {
				return err
			}
			if err := o.validate(url, creds); err != nil {
				return err
			}
			body := apiclient.RepositoryCredentialWriteRequest{
				Spec: v1alpha1.RepositoryCredentialSpec{URLPrefix: url, Type: v1alpha1.RepositoryType(o.Type), RepositoryAccess: o.access(o.Proxy)},
			}
			if !emptyCredentials(creds) {
				body.Credentials = creds
			}
			created, err := client.CreateRepositoryCredential(c.Context(), &apiclient.RepositoryCredentialCreateRequest{Upsert: o.Upsert, Body: body})
			if err != nil {
				return err
			}
			output.Printf(f.Out(), "Repository credentials for '%s' added\n", created.Spec.URLPrefix)
			return nil
		},
	}
	fs := c.Flags()
	fs.StringVar(&o.BearerToken, "bearer-token", "", "bearer token to the Git repository")
	fs.BoolVar(&o.EnableOCI, "enable-oci", false, "Specifies whether helm-oci support should be enabled for this repo")
	fs.StringVar(&o.Type, "type", string(v1alpha1.RepositoryTypeGit), "type of the repository, \"git\" or \"helm\"")
	fs.BoolVar(&o.ForceHTTPBasicAuth, "force-http-basic-auth", false, "whether to force basic auth when connecting via HTTP")
	fs.StringVar(&o.Proxy, "proxy-url", "", "If provided, this URL will be used to connect via proxy")
	o.addCommonCredentialFlags(c)
	return c
}

func newRepoCredsListCommand(f *cmdutil.Factory) *cobra.Command {
	var format string
	c := &cobra.Command{
		Use:   "list",
		Short: "List configured repository credentials",
		Example: `  patrol repocreds list

  patrol repocreds list -o json

  patrol repocreds list -o yaml

  patrol repocreds list -o url`,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := output.CheckFormat(format, output.YAML, output.JSON, output.URL, output.Wide, ""); err != nil {
				return output.UnknownFormat(format)
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			l, err := client.ListRepositoryCredentials(c.Context())
			if err != nil {
				return err
			}
			switch format {
			case output.JSON, output.YAML:
				return output.PrintResourceList(f.Out(), l.Items, format, false)
			case output.URL:
				for _, r := range l.Items {
					output.Println(f.Out(), r.Spec.URLPrefix)
				}
				return nil
			}
			t := output.NewTable(f.Out())
			t.Row("URL PATTERN", "USERNAME", "SSH_CREDS", "TLS_CREDS")
			for _, r := range l.Items {
				// The API never returns credential material, so (as upstream) the SSH and TLS
				// columns cannot tell whether such credentials are stored.
				t.Row(r.Spec.URLPrefix, output.OrDash(r.Username), false, false)
			}
			return t.Flush()
		},
	}
	c.Flags().StringVarP(&format, "output", "o", output.Wide, "Output format. One of: json|yaml|wide|url")
	return c
}

func newRepoCredsRemoveCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "rm CREDSURL",
		Short:   "Remove repository credentials",
		Example: `  patrol repocreds rm https://git.example.com/repos/`,
		Args:    cmdutil.MinimumArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			p := f.OptionalPrompt()
			for _, url := range args {
				ok, err := p.Confirm(fmt.Sprintf("Are you sure you want to remove '%s'? [y/n] ", url))
				if err != nil {
					return err
				}
				if !ok {
					output.Printf(f.Out(), "The command to remove '%s' was cancelled.\n", url)
					continue
				}
				if err := client.DeleteRepositoryCredential(c.Context(), url); err != nil {
					return err
				}
				output.Printf(f.Out(), "Repository credentials for '%s' removed\n", strings.TrimSpace(url))
			}
			return nil
		},
	}
}
