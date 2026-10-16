package k8sauth

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/clusters/internal/fakecloud"
)

type execCredential struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		Interactive bool `json:"interactive"`
	} `json:"spec"`
	Status struct {
		ExpirationTimestamp *time.Time `json:"expirationTimestamp"`
		Token               string     `json:"token"`
	} `json:"status"`
}

func run(t *testing.T, cfg Config, args ...string) (execCredential, string) {
	t.Helper()
	cmd := NewCommand(cfg)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	require.NoError(t, cmd.ExecuteContext(context.Background()), errOut.String())
	var ec execCredential
	require.NoError(t, json.Unmarshal(out.Bytes(), &ec), out.String())
	assert.Equal(t, clusters.ExecCredentialAPIVersion, ec.APIVersion)
	assert.Equal(t, "ExecCredential", ec.Kind)
	return ec, out.String()
}

// TestAWSCommand covers REPO-111: flags, token format and the 14-minute expiry.
func TestAWSCommand(t *testing.T) {
	fakecloud.IsolateAWSEnv(t)
	sts := fakecloud.NewSTS(t)
	cfg := Config{AWS: clusters.AWSTokenRequest{
		Region: "us-east-2", STSEndpoint: sts.URL,
		LoadOptions: []func(*awsconfig.LoadOptions) error{
			awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("AKIDCOMMAND000000001", "secret", "")),
		},
	}}
	start := time.Now()
	ec, raw := run(t, cfg, "aws", "--cluster-name", "eks-prod", "--role-arn", "arn:aws:iam::1:role/r")
	require.True(t, strings.HasPrefix(ec.Status.Token, clusters.AWSTokenPrefix))
	u, err := clusters.DecodeAWSToken(ec.Status.Token)
	require.NoError(t, err)
	assert.Contains(t, u, "Action=GetCallerIdentity")
	assert.Contains(t, u, "x-k8s-aws-id")
	assert.Contains(t, u, fakecloud.AssumedAccessKeyID, "the role was assumed")
	require.NotNil(t, ec.Status.ExpirationTimestamp)
	assert.WithinDuration(t, start.Add(14*time.Minute), *ec.Status.ExpirationTimestamp, 5*time.Second)
	assert.Contains(t, raw, `"spec":{"interactive":false}`)
	assert.NotContains(t, raw, "\n", "only the ExecCredential is printed")
}

func TestAWSCommandTimeout(t *testing.T) {
	fakecloud.IsolateAWSEnv(t)
	cmd := NewCommand(Config{AWSTimeout: 100 * time.Millisecond, AWSInterval: 20 * time.Millisecond})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"aws", "--cluster-name", "x", "--profile", "missing-profile"})
	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "timeout while trying to get signed aws request: last error: error loading AWS configuration: "), err.Error())
	assert.Empty(t, out.String())
}

// TestGCPCommand covers REPO-112 with a fake application-default-credentials file.
func TestGCPCommand(t *testing.T) {
	t.Parallel()
	g := fakecloud.NewGoogle(t)
	cfg := Config{GCP: clusters.GCPOptions{Lookup: func(k string) (string, bool) {
		if k == "GOOGLE_APPLICATION_CREDENTIALS" {
			return g.CredentialsFile, true
		}
		return "", false
	}}}
	ec, _ := run(t, cfg, "gcp")
	assert.Equal(t, g.AccessToken, ec.Status.Token)
	require.NotNil(t, ec.Status.ExpirationTimestamp)
	assert.WithinDuration(t, time.Now().Add(time.Hour), *ec.Status.ExpirationTimestamp, 5*time.Minute)
}

// TestAzureCommand covers REPO-113 with a service principal against a mock tenant.
func TestAzureCommand(t *testing.T) {
	t.Parallel()
	e := fakecloud.NewEntra(t)
	env := map[string]string{
		clusters.EnvAzureLoginMethod: "spn", clusters.EnvAzureClientID: "cid", clusters.EnvAzureClientSecret: "cs",
		clusters.EnvAzureTenantID: e.Tenant, clusters.EnvAzureAuthorityHost: e.URL,
	}
	cfg := Config{Azure: clusters.AzureOptions{
		Lookup:                   func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		ClientOptions:            azcore.ClientOptions{Transport: e.Client()},
		DisableInstanceDiscovery: true,
	}}
	ec, _ := run(t, cfg, "azure")
	assert.Equal(t, e.AccessToken, ec.Status.Token)
	reqs := e.TokenRequests()
	require.NotEmpty(t, reqs)
	assert.Contains(t, reqs[len(reqs)-1].Get("scope"), clusters.AzureDefaultServerApplicationID+"/.default")
}

func TestCommandTree(t *testing.T) {
	t.Parallel()
	cmd := NewCommand(Config{})
	assert.Equal(t, "k8s-auth", cmd.Name())
	var names []string
	for _, c := range cmd.Commands() {
		names = append(names, c.Name())
	}
	assert.ElementsMatch(t, []string{"aws", "gcp", "azure"}, names)
	aws, _, err := cmd.Find([]string{"aws"})
	require.NoError(t, err)
	for _, f := range []string{"cluster-name", "role-arn", "profile"} {
		assert.NotNil(t, aws.Flags().Lookup(f), f)
	}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "aws")
}
