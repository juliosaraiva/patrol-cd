package clusters

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

const (
	// AWSTokenPrefix starts every EKS bearer token.
	AWSTokenPrefix = "k8s-aws-v1."
	// AWSClusterIDHeader is the signed header naming the EKS cluster.
	AWSClusterIDHeader = "x-k8s-aws-id"
	// awsPresignExpiresParam is the legacy X-Amz-Expires value; STS ignores it and presigned
	// GetCallerIdentity URLs are valid for 15 minutes.
	awsPresignExpiresParam = 60
	// AWSTokenLifetime is the expiry reported for EKS tokens: one minute less than the
	// 15-minute validity of the presigned URL.
	AWSTokenLifetime = 14 * time.Minute
	// AWSPresignTimeout and AWSPresignInterval bound the retries of the k8s-auth command.
	AWSPresignTimeout  = time.Minute
	AWSPresignInterval = 5 * time.Second
)

// AWSTokenRequest describes an EKS token. ClusterName, RoleARN and Profile are the
// awsAuthConfig fields. The remaining fields are not part of the Cluster API; they exist for
// private STS endpoints and tests.
type AWSTokenRequest struct {
	ClusterName string
	RoleARN     string
	Profile     string

	// Region overrides the region from the AWS configuration.
	Region string
	// STSEndpoint overrides the STS endpoint (aws BaseEndpoint).
	STSEndpoint string
	// HTTPClient is used for AWS calls (credential providers, AssumeRole).
	HTTPClient *http.Client
	// LoadOptions are appended to the default configuration loading options.
	LoadOptions []func(*awsconfig.LoadOptions) error
	// Now returns the current time (expiry computation); defaults to time.Now.
	Now func() time.Time
}

// AWSToken returns an EKS bearer token: "k8s-aws-v1." followed by the unpadded base64url
// encoding of a presigned sts:GetCallerIdentity URL that signs the x-k8s-aws-id header. When
// RoleARN is set the role is assumed first. Presigning needs credentials but no network call
// unless a role is assumed or the credential provider is remote.
func AWSToken(ctx context.Context, req AWSTokenRequest) (Token, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if req.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(req.Profile))
	}
	if req.Region != "" {
		opts = append(opts, awsconfig.WithRegion(req.Region))
	}
	if req.HTTPClient != nil {
		opts = append(opts, awsconfig.WithHTTPClient(req.HTTPClient))
	}
	opts = append(opts, req.LoadOptions...)
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return Token{}, fmt.Errorf("error loading AWS configuration: %w", err)
	}
	stsOpts := func(o *sts.Options) {
		if req.STSEndpoint != "" {
			o.BaseEndpoint = aws.String(req.STSEndpoint)
		}
	}
	client := sts.NewFromConfig(cfg, stsOpts)
	if req.RoleARN != "" {
		cfg.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(client, req.RoleARN))
		client = sts.NewFromConfig(cfg, stsOpts)
	}
	// The headers are added through the presign client options (as aws-iam-authenticator does)
	// so the canonical request matches the one EKS rebuilds when it validates the token.
	presigned, err := sts.NewPresignClient(client).PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{},
		func(po *sts.PresignOptions) {
			po.ClientOptions = append(po.ClientOptions, func(o *sts.Options) {
				o.APIOptions = append(o.APIOptions,
					smithyhttp.SetHeaderValue(AWSClusterIDHeader, req.ClusterName),
					smithyhttp.SetHeaderValue("X-Amz-Expires", strconv.Itoa(awsPresignExpiresParam)))
			})
		})
	if err != nil {
		return Token{}, fmt.Errorf("error presigning AWS request: %w", err)
	}
	now := time.Now
	if req.Now != nil {
		now = req.Now
	}
	return Token{
		Value:  AWSTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(presigned.URL)),
		Expiry: now().Add(AWSTokenLifetime),
	}, nil
}

// AWSTokenWithRetry calls AWSToken until it succeeds, retrying every interval for at most
// timeout (upstream: 5s and 1 minute). The final error names the last failure.
func AWSTokenWithRetry(ctx context.Context, req AWSTokenRequest, timeout, interval time.Duration) (Token, error) {
	return retryToken(ctx, timeout, interval, func(ctx context.Context) (Token, error) { return AWSToken(ctx, req) })
}

func retryToken(ctx context.Context, timeout, interval time.Duration, fn func(context.Context) (Token, error)) (Token, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		tok, err := fn(ctx)
		if err == nil {
			return tok, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Token{}, fmt.Errorf("timeout while trying to get signed aws request: last error: %w", err)
		case <-timer.C:
		}
	}
}

// DecodeAWSToken returns the presigned URL inside an EKS token.
func DecodeAWSToken(token string) (string, error) {
	rest, ok := strings.CutPrefix(token, AWSTokenPrefix)
	if !ok {
		return "", errors.New("token lacks the k8s-aws-v1 prefix")
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	return string(b), nil
}

// awsTokenSource returns tokens for one awsAuthConfig. Each refresh loads the AWS
// configuration again, so rotated credentials (IRSA, Pod Identity) are picked up.
func awsTokenSource(req AWSTokenRequest) TokenSource {
	return TokenSourceFunc(func(ctx context.Context) (Token, error) {
		return AWSTokenWithRetry(ctx, req, AWSPresignTimeout, AWSPresignInterval)
	})
}
