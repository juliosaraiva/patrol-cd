// Package k8sauth implements the `patrolcd k8s-auth` exec credential plugin (upstream
// argocd-k8s-auth): `aws`, `gcp` and `azure` subcommands that print a
// client.authentication.k8s.io/v1beta1 ExecCredential with a bearer token for EKS, GKE or AKS.
//
// The controller normally produces these tokens in-process (internal/clusters); the commands
// exist for exec configurations, kubeconfigs exported from Patrol and migrated Argo CD
// clusters. They read their configuration from flags and the environment only and never print
// anything but the ExecCredential on stdout.
package k8sauth

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/internal/clusters"
)

// Config holds overrides used by tests and embedders. The zero value uses the real cloud
// endpoints and the process environment.
type Config struct {
	// AWS supplies the fields of the AWS token request other than the flags.
	AWS clusters.AWSTokenRequest
	// AWSTimeout and AWSInterval bound presign retries (defaults: 1 minute, 5 seconds).
	AWSTimeout, AWSInterval time.Duration
	// GCP configures the GKE token source.
	GCP clusters.GCPOptions
	// Azure configures the AKS token source.
	Azure clusters.AzureOptions
}

// NewCommand returns the `k8s-auth` command with its aws, gcp and azure subcommands.
func NewCommand(cfg Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:           clusters.K8sAuthSubcommand,
		Short:         "Print Kubernetes exec credentials for managed cloud clusters",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help()
		},
	}
	cmd.AddCommand(newAWSCommand(cfg), newGCPCommand(cfg), newAzureCommand(cfg))
	return cmd
}

func writeCredential(w io.Writer, tok clusters.Token) error {
	b, err := clusters.ExecCredentialJSON(tok)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("write ExecCredential: %w", err)
	}
	return nil
}

func ctxOf(c *cobra.Command) context.Context {
	if ctx := c.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

func newAWSCommand(cfg Config) *cobra.Command {
	var clusterName, roleARN, profile string
	cmd := &cobra.Command{
		Use:   "aws",
		Short: "Print an EKS token (presigned sts:GetCallerIdentity)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			req := cfg.AWS
			req.ClusterName, req.RoleARN, req.Profile = clusterName, roleARN, profile
			timeout, interval := cfg.AWSTimeout, cfg.AWSInterval
			if timeout <= 0 {
				timeout = clusters.AWSPresignTimeout
			}
			if interval <= 0 {
				interval = clusters.AWSPresignInterval
			}
			tok, err := clusters.AWSTokenWithRetry(ctxOf(c), req, timeout, interval)
			if err != nil {
				return err
			}
			return writeCredential(c.OutOrStdout(), tok)
		},
	}
	cmd.Flags().StringVar(&clusterName, "cluster-name", "", "AWS Cluster name")
	cmd.Flags().StringVar(&roleARN, "role-arn", "", "AWS Role ARN")
	cmd.Flags().StringVar(&profile, "profile", "", "AWS Profile")
	return cmd
}

func newGCPCommand(cfg Config) *cobra.Command {
	return &cobra.Command{
		Use:   "gcp",
		Short: "Print a GKE token from Google application default credentials",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			tok, err := clusters.GCPTokenSource(cfg.GCP).Token(ctxOf(c))
			if err != nil {
				return err
			}
			return writeCredential(c.OutOrStdout(), tok)
		},
	}
}

func newAzureCommand(cfg Config) *cobra.Command {
	return &cobra.Command{
		Use:   "azure",
		Short: "Print an AKS token (Microsoft Entra ID; workload identity by default)",
		Long: "Print an AKS token. The login method comes from AAD_LOGIN_METHOD " +
			"(workloadidentity by default, spn, msi, azurecli, ropc) and the audience from " +
			"AAD_SERVER_APPLICATION_ID (default " + clusters.AzureDefaultServerApplicationID + ").",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			opts := cfg.Azure
			if opts.Lookup == nil {
				opts.Lookup = os.LookupEnv
			}
			src, err := clusters.AzureTokenSource(opts)
			if err != nil {
				return err
			}
			tok, err := src.Token(ctxOf(c))
			if err != nil {
				return err
			}
			return writeCredential(c.OutOrStdout(), tok)
		},
	}
}
