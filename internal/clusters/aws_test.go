package clusters

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters/internal/fakecloud"
)

func staticAWS(akid string) []func(*awsconfig.LoadOptions) error {
	return []func(*awsconfig.LoadOptions) error{
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(akid, "fakeSecretAccessKey", "")),
	}
}

func presignedQuery(t *testing.T, token string) (*url.URL, url.Values) {
	t.Helper()
	raw, err := DecodeAWSToken(token)
	require.NoError(t, err)
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u, u.Query()
}

// TestAWSTokenFormat covers REPO-111: the token decodes to a presigned GetCallerIdentity URL
// against the configured STS endpoint that signs x-k8s-aws-id.
func TestAWSTokenFormat(t *testing.T) {
	fakecloud.IsolateAWSEnv(t)
	sts := fakecloud.NewSTS(t)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	tok, err := AWSToken(context.Background(), AWSTokenRequest{
		ClusterName: "eks-prod",
		Region:      "eu-west-1",
		STSEndpoint: sts.URL,
		LoadOptions: staticAWS("AKIDSTATIC0000000001"),
		Now:         func() time.Time { return now },
	})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(tok.Value, AWSTokenPrefix))
	assert.NotContains(t, strings.TrimPrefix(tok.Value, AWSTokenPrefix), "=", "base64url without padding")
	assert.Equal(t, now.Add(14*time.Minute), tok.Expiry)

	u, q := presignedQuery(t, tok.Value)
	stsURL, err := url.Parse(sts.URL)
	require.NoError(t, err)
	assert.Equal(t, stsURL.Host, u.Host)
	assert.Equal(t, "GetCallerIdentity", q.Get("Action"))
	assert.Equal(t, "2011-06-15", q.Get("Version"))
	assert.Equal(t, "AWS4-HMAC-SHA256", q.Get("X-Amz-Algorithm"))
	assert.Equal(t, "60", q.Get("X-Amz-Expires"))
	assert.True(t, strings.HasPrefix(q.Get("X-Amz-Credential"), "AKIDSTATIC0000000001/"), q.Get("X-Amz-Credential"))
	assert.Contains(t, q.Get("X-Amz-Credential"), "/eu-west-1/sts/aws4_request")
	assert.Contains(t, strings.Split(q.Get("X-Amz-SignedHeaders"), ";"), "x-k8s-aws-id")
	assert.Contains(t, strings.Split(q.Get("X-Amz-SignedHeaders"), ";"), "host")
	assert.Regexp(t, `^[0-9a-f]{64}$`, q.Get("X-Amz-Signature"))
	assert.Empty(t, sts.Requests(), "presigning makes no STS call")

	// A different cluster name changes the signature.
	tok2, err := AWSToken(context.Background(), AWSTokenRequest{
		ClusterName: "eks-other", Region: "eu-west-1", STSEndpoint: sts.URL, LoadOptions: staticAWS("AKIDSTATIC0000000001"),
		Now: func() time.Time { return now },
	})
	require.NoError(t, err)
	_, q2 := presignedQuery(t, tok2.Value)
	if q.Get("X-Amz-Date") == q2.Get("X-Amz-Date") {
		assert.NotEqual(t, q.Get("X-Amz-Signature"), q2.Get("X-Amz-Signature"))
	}
}

// TestAWSTokenAssumeRole covers the role ARN path with static credentials.
func TestAWSTokenAssumeRole(t *testing.T) {
	fakecloud.IsolateAWSEnv(t)
	sts := fakecloud.NewSTS(t)
	tok, err := AWSToken(context.Background(), AWSTokenRequest{
		ClusterName: "eks-prod",
		RoleARN:     "arn:aws:iam::123456789012:role/patrol",
		Region:      "us-east-1",
		STSEndpoint: sts.URL,
		LoadOptions: staticAWS("AKIDSTATIC0000000002"),
	})
	require.NoError(t, err)
	reqs := sts.Requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "AssumeRole", reqs[0].Get("Action"))
	assert.Equal(t, "arn:aws:iam::123456789012:role/patrol", reqs[0].Get("RoleArn"))
	_, q := presignedQuery(t, tok.Value)
	assert.True(t, strings.HasPrefix(q.Get("X-Amz-Credential"), fakecloud.AssumedAccessKeyID+"/"), "signed with the assumed role")
	assert.Equal(t, "fake-session-token", q.Get("X-Amz-Security-Token"))
}

func TestAWSTokenConfigErrors(t *testing.T) {
	fakecloud.IsolateAWSEnv(t)
	_, err := AWSToken(context.Background(), AWSTokenRequest{ClusterName: "x", Profile: "does-not-exist"})
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "error loading AWS configuration: "), err.Error())
}

// TestAWSTokenRetry covers the retry and timeout behavior of the k8s-auth aws command.
func TestAWSTokenRetry(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	tok, err := retryToken(context.Background(), time.Second, time.Millisecond, func(context.Context) (Token, error) {
		if calls.Add(1) < 3 {
			return Token{}, errors.New("transient")
		}
		return Token{Value: "ok"}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", tok.Value)
	assert.Equal(t, int32(3), calls.Load())

	_, err = retryToken(context.Background(), 30*time.Millisecond, 5*time.Millisecond, func(context.Context) (Token, error) {
		return Token{}, errors.New("no credentials")
	})
	require.EqualError(t, err, "timeout while trying to get signed aws request: last error: no credentials")
}

// TestAWSNativeTransport checks that awsAuthConfig authenticates requests in-process.
func TestAWSNativeTransport(t *testing.T) {
	fakecloud.IsolateAWSEnv(t)
	sts := fakecloud.NewSTS(t)
	srv := newTLSAPIServer(t)
	c := cluster("eks", srv.URL, func(c *v1alpha1.Cluster) {
		c.Spec.Config.AWSAuthConfig = &v1alpha1.AWSAuthConfig{ClusterName: "eks-native"}
	})
	opts := Options{
		Providers: mustProviderCache(t),
		AWS:       AWSTokenRequest{Region: "us-west-2", STSEndpoint: sts.URL, LoadOptions: staticAWS("AKIDSTATIC0000000003")},
	}
	_, err := probeWith(t, c, map[string][]byte{"caData": srv.caPEM()}, opts)
	require.NoError(t, err)
	auth := srv.lastHeader(t).Get("Authorization")
	tok, ok := strings.CutPrefix(auth, "Bearer ")
	require.True(t, ok)
	_, q := presignedQuery(t, tok)
	assert.Equal(t, "GetCallerIdentity", q.Get("Action"))
	assert.Equal(t, 1, opts.Providers.Len())

	// The cached token is reused by a second configuration for the same cluster.
	_, err = probeWith(t, c, map[string][]byte{"caData": srv.caPEM()}, opts)
	require.NoError(t, err)
	assert.Equal(t, auth, srv.lastHeader(t).Get("Authorization"))
}
