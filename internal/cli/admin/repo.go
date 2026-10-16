package admin

import (
	"bufio"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

// repoOptions are the flags of "repo generate-spec" (upstream RepoOptions/AddRepoFlags).
type repoOptions struct {
	repoType, name, project, username, password, bearerToken                      string
	sshPrivateKeyPath, tlsClientCertPath, tlsClientCertKeyPath                    string
	insecureIgnoreHostKey, insecureSkipServerVerification, enableLFS, enableOCI   bool
	githubAppID, githubAppInstallationID                                          int64
	githubAppPrivateKeyPath, githubAppEnterpriseBaseURL                           string
	proxy, noProxy, gcpServiceAccountKeyPath                                      string
	forceHTTPBasicAuth, useAzureWorkloadIdentity, insecureOCIForceHTTP            bool
	depth                                                                         int64
	webhookManifestCacheWarmDisabled                                              bool
	azureTenantID, azureClientID, azureClientSecret, azureActiveDirectoryEndpoint string
	output, outputFile                                                            string
}

func newRepoCommand(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Manage repositories configuration",
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(newRepoGenerateSpecCommand(f))
	return cmd
}

func newRepoGenerateSpecCommand(f *Factory) *cobra.Command {
	o := &repoOptions{}
	cmd := &cobra.Command{
		Use:   "generate-spec REPOURL",
		Short: "Generate declarative config for a repo",
		Long: `Generate the declarative configuration of a repository: a Repository object and the
credentials Secret it references. Nothing is sent to a server.

Printed credentials are redacted; write the complete manifests with --output-file (mode 0600).`,
		Example: `
  # Add a Git repository via SSH using a private key for authentication, ignoring the server's host key:
  patrol admin repo generate-spec git@git.example.com:repos/repo --insecure-ignore-host-key --ssh-private-key-path ~/id_rsa

  # Add a private Git repository via HTTPS using username/password and TLS client certificates:
  patrol admin repo generate-spec https://git.example.com/repos/repo --username git --password secret --tls-client-cert-path ~/mycert.crt --tls-client-cert-key-path ~/mycert.key --output-file repo.yaml

  # Add a public Helm repository named 'stable' via HTTPS
  patrol admin repo generate-spec https://charts.helm.sh/stable --type helm --name stable

  # Add a private Git repository on GitHub.com via GitHub App
  patrol admin repo generate-spec https://git.example.com/repos/repo --github-app-id 1 --github-app-installation-id 2 --github-app-private-key-path test.private-key.pem --output-file repo.yaml
`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runRepoGenerateSpec(f, o, args[0])
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&o.repoType, "type", string(v1alpha1.RepositoryTypeGit), "type of the repository, \"git\", \"oci\" or \"helm\"")
	fl.StringVar(&o.name, "name", "", "name of the repository, mandatory for repositories of type helm")
	fl.StringVar(&o.project, "project", "", "project of the repository")
	fl.StringVar(&o.username, "username", "", "username to the repository")
	fl.StringVar(&o.password, "password", "", "password to the repository")
	fl.StringVar(&o.bearerToken, "bearer-token", "", "bearer token to the Git BitBucket Data Center repository")
	fl.StringVar(&o.sshPrivateKeyPath, "ssh-private-key-path", "", "path to the private ssh key (e.g. ~/.ssh/id_rsa)")
	fl.StringVar(&o.tlsClientCertPath, "tls-client-cert-path", "", "path to the TLS client cert (must be PEM format)")
	fl.StringVar(&o.tlsClientCertKeyPath, "tls-client-cert-key-path", "", "path to the TLS client cert's key (must be PEM format)")
	fl.BoolVar(&o.insecureIgnoreHostKey, "insecure-ignore-host-key", false, "disables SSH strict host key checking (deprecated, use --insecure-skip-server-verification instead)")
	fl.BoolVar(&o.insecureSkipServerVerification, "insecure-skip-server-verification", false, "disables server certificate and host key checks")
	fl.BoolVar(&o.enableLFS, "enable-lfs", false, "enable git-lfs (Large File Support) on this repository")
	fl.BoolVar(&o.enableOCI, "enable-oci", false, "enable helm-oci (Helm OCI-Based Repository) (only valid for helm type repositories)")
	fl.Int64Var(&o.githubAppID, "github-app-id", 0, "id of the GitHub Application")
	fl.Int64Var(&o.githubAppInstallationID, "github-app-installation-id", 0, "installation id of the GitHub Application (optional, will be auto-discovered if not provided)")
	fl.StringVar(&o.githubAppPrivateKeyPath, "github-app-private-key-path", "", "private key of the GitHub Application")
	fl.StringVar(&o.githubAppEnterpriseBaseURL, "github-app-enterprise-base-url", "", "base url to use when using GitHub Enterprise (e.g. https://ghe.example.com/api/v3")
	fl.StringVar(&o.proxy, "proxy", "", "use proxy to access repository")
	fl.StringVar(&o.noProxy, "no-proxy", "", "don't access these targets via proxy")
	fl.StringVar(&o.gcpServiceAccountKeyPath, "gcp-service-account-key-path", "", "service account key for the Google Cloud Platform")
	fl.BoolVar(&o.forceHTTPBasicAuth, "force-http-basic-auth", false, "whether to force use of basic auth when connecting repository via HTTP")
	fl.BoolVar(&o.useAzureWorkloadIdentity, "use-azure-workload-identity", false, "whether to use azure workload identity for authentication")
	fl.BoolVar(&o.insecureOCIForceHTTP, "insecure-oci-force-http", false, "Use http when accessing an OCI repository")
	fl.Int64Var(&o.depth, "depth", 0, "Specify a custom depth for git clone operations. Unless specified, a full clone is performed using the depth of 0")
	fl.BoolVar(&o.webhookManifestCacheWarmDisabled, "webhook-manifest-cache-warm-disabled", false, "disable manifest cache warming during webhook processing for this repository (recommended for large monorepos with plain YAML manifests)")
	fl.StringVar(&o.azureTenantID, "azure-service-principal-tenant-id", "", "tenant id of the Azure Service Principal")
	fl.StringVar(&o.azureClientID, "azure-service-principal-client-id", "", "client id of the Azure Service Principal")
	fl.StringVar(&o.azureClientSecret, "azure-service-principal-client-secret", "", "client secret of the Azure Service Principal")
	fl.StringVar(&o.azureActiveDirectoryEndpoint, "azure-active-directory-endpoint", "", "Active Directory endpoint when not using default Azure public cloud (e.g. https://login.microsoftonline.de)")
	fl.StringVarP(&o.output, "output", "o", formatYAML, "Output format. One of: json|yaml")
	fl.StringVar(&o.outputFile, "output-file", "", "Write the manifests, including credentials, to this file (mode 0600)")
	return cmd
}

// repositoryObjectName is the name the API gives repositories it creates (upstream
// RepoURLToSecretName: "repo-<hash of the normalized URL and project>").
func repositoryObjectName(url, projectName string) string {
	key := project.NormalizeGitURL(url)
	if key == "" {
		key = strings.TrimSpace(url)
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(projectName))
	return fmt.Sprintf("repo-%016x", h.Sum64())
}

// promptPassword asks for a password on the terminal (without echo when in is a
// terminal), as upstream does when --username is given without --password.
func promptPassword(in io.Reader, out io.Writer) (string, error) {
	_, _ = fmt.Fprint(out, "Password: ")
	defer func() { _, _ = fmt.Fprintln(out) }()
	if fd, ok := in.(*os.File); ok && term.IsTerminal(int(fd.Fd())) { //nolint:gosec // file descriptors fit in int
		b, err := term.ReadPassword(int(fd.Fd())) //nolint:gosec // file descriptors fit in int
		return string(b), err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func runRepoGenerateSpec(f *Factory, o *repoOptions, repoURL string) error {
	data := map[string][]byte{}
	put := func(key, value string) {
		if value != "" {
			data[key] = []byte(value)
		}
	}
	readInto := func(key, path string) error {
		if path == "" {
			return nil
		}
		b, err := readFileLimited(path)
		if err != nil {
			return err
		}
		data[key] = b
		return nil
	}
	if o.sshPrivateKeyPath != "" {
		if ok, _ := git.IsSSHURL(repoURL); !ok {
			return errors.New("--ssh-private-key-path is only supported for SSH repositories")
		}
		if err := readInto(v1alpha1.RepositorySecretKeySSHPrivateKey, o.sshPrivateKeyPath); err != nil {
			return err
		}
	}
	if (o.tlsClientCertPath != "") != (o.tlsClientCertKeyPath != "") {
		return errors.New("--tls-client-cert-path and --tls-client-cert-key-path must be specified together")
	}
	if o.tlsClientCertPath != "" {
		if !git.IsHTTPSURL(repoURL) {
			return errors.New("--tls-client-cert-path is only supported for HTTPS repositories")
		}
		if err := readInto(v1alpha1.RepositorySecretKeyTLSClientCertData, o.tlsClientCertPath); err != nil {
			return err
		}
		if err := readInto(v1alpha1.RepositorySecretKeyTLSClientCertKey, o.tlsClientCertKeyPath); err != nil {
			return err
		}
	}
	repoType := v1alpha1.RepositoryType(o.repoType)
	switch repoType {
	case v1alpha1.RepositoryTypeGit, v1alpha1.RepositoryTypeHelm, v1alpha1.RepositoryTypeOCI:
	default:
		return fmt.Errorf("unsupported repository type %q: one of git, helm, oci", o.repoType)
	}
	if repoType == v1alpha1.RepositoryTypeHelm && o.name == "" {
		return errors.New("must specify --name for repos of type 'helm'")
	}
	password := o.password
	if o.username != "" && password == "" {
		var err error
		if password, err = promptPassword(f.in(), f.errOut()); err != nil {
			return err
		}
	}
	if o.bearerToken != "" && password != "" {
		return errors.New("only --bearer-token or --password is allowed, not both")
	}
	if o.bearerToken != "" && !git.IsHTTPSURL(repoURL) {
		return errors.New("--bearer-token is only supported for HTTPS repositories")
	}
	if o.bearerToken != "" && repoType != v1alpha1.RepositoryTypeGit {
		return errors.New("--bearer-token is only supported for Git repositories")
	}
	put(v1alpha1.RepositorySecretKeyUsername, o.username)
	put(v1alpha1.RepositorySecretKeyPassword, password)
	put(v1alpha1.RepositorySecretKeyBearerToken, o.bearerToken)
	put(v1alpha1.RepositorySecretKeyAzureServicePrincipalClientSecret, o.azureClientSecret)
	if err := readInto(v1alpha1.RepositorySecretKeyGitHubAppPrivateKey, o.githubAppPrivateKeyPath); err != nil {
		return err
	}
	if err := readInto(v1alpha1.RepositorySecretKeyGCPServiceAccountKey, o.gcpServiceAccountKeyPath); err != nil {
		return err
	}
	name := repositoryObjectName(repoURL, o.project)
	repo := &v1alpha1.Repository{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Repository"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.RepositorySpec{
			URL: repoURL, Type: repoType, Name: o.name, Project: o.project,
			Insecure: o.insecureSkipServerVerification, InsecureIgnoreHostKey: o.insecureIgnoreHostKey, //nolint:staticcheck // deprecated field kept for parity (repo add --insecure-ignore-host-key)
			EnableLFS: o.enableLFS, Depth: o.depth, WebhookManifestCacheWarmDisabled: o.webhookManifestCacheWarmDisabled,
			RepositoryAccess: v1alpha1.RepositoryAccess{
				EnableOCI: o.enableOCI, InsecureOCIForceHTTP: o.insecureOCIForceHTTP, ForceHTTPBasicAuth: o.forceHTTPBasicAuth,
				Proxy: o.proxy, NoProxy: o.noProxy, UseAzureWorkloadIdentity: o.useAzureWorkloadIdentity,
			},
		},
	}
	if o.githubAppID != 0 {
		repo.Spec.GitHubApp = &v1alpha1.RepositoryGitHubApp{AppID: o.githubAppID, EnterpriseBaseURL: o.githubAppEnterpriseBaseURL}
		if o.githubAppInstallationID != 0 {
			id := o.githubAppInstallationID
			repo.Spec.GitHubApp.InstallationID = &id
		}
	}
	if o.azureClientID != "" || o.azureTenantID != "" {
		repo.Spec.AzureServicePrincipal = &v1alpha1.AzureServicePrincipal{ClientID: o.azureClientID, TenantID: o.azureTenantID, ActiveDirectoryEndpoint: o.azureActiveDirectoryEndpoint}
	}
	var secret *corev1.Secret
	if len(data) > 0 {
		secretName := name + "-creds"
		repo.Spec.CredentialsRef = &v1alpha1.LocalSecretReference{Name: secretName}
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Labels: map[string]string{v1alpha1.LabelKeySecretType: v1alpha1.LabelValueSecretTypeRepository}},
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}
	}
	return printSpecWithSecret(f, o.output, o.outputFile, []any{repo}, secret, func(key, _ string) bool {
		return key != v1alpha1.RepositorySecretKeyUsername && key != v1alpha1.RepositorySecretKeyTLSClientCertData
	})
}
