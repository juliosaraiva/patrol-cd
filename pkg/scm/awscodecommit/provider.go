// Package awscodecommit implements the scm contract for AWS CodeCommit with
// aws-sdk-go-v2 (codecommit, and resourcegroupstaggingapi for tag filters), as upstream.
//
// It serves what the Argo CD v3.5.5 baseline uses CodeCommit for: the SCM Provider
// generator (repositories of the account and region, optionally discovered by tags;
// branches; path checks). CodeCommit has no Pull Request generator or webhook support
// upstream, so those capabilities are not declared.
//
// Repository references: RepoRef.Name is the repository name. RepoRef.Owner is
// informational (the AWS account ID reported in Repository.Organization); the account is
// the one of the credentials in use, so the owner is not sent to AWS.
package awscodecommit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/codecommit"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// Type is the registered provider type.
const Type = "awscodecommit"

// CloneProtocolHTTPSFIPS selects the FIPS HTTPS clone endpoint
// (https://git-codecommit-fips.<region>...), CodeCommit only.
const CloneProtocolHTTPSFIPS scm.CloneProtocol = "https-fips"

// concurrency bounds parallel per-repository and per-branch calls.
const concurrency = 4

// TagFilter selects repositories by tag (upstream awsCodeCommit.tagFilters). Filters
// with the same key are ORed over their values; an empty value matches any value of the
// key; different keys are ANDed (Resource Groups Tagging API semantics).
type TagFilter struct {
	Key   string
	Value string
}

// Options are CodeCommit settings that scm.Config cannot express.
type Options struct {
	// Region overrides the AWS region (upstream `region`; default: the region of the
	// AWS environment, e.g. AWS_REGION).
	Region string
	// Role is an IAM role ARN assumed through STS for cross-account discovery (upstream
	// `role`); empty uses the ambient credentials.
	Role string
	// TagFilters switch repository discovery to the Resource Groups Tagging API.
	TagFilters []TagFilter
	// AWSConfig, when set, replaces loading the default AWS configuration (environment,
	// shared files, IRSA, instance metadata). Region and Role still apply on top of it.
	// Its BaseEndpoint, when set, is used for CodeCommit, Resource Groups Tagging and STS.
	AWSConfig *aws.Config
}

// codeCommitAPI is the subset of the CodeCommit client the provider uses.
type codeCommitAPI interface {
	ListRepositories(context.Context, *codecommit.ListRepositoriesInput, ...func(*codecommit.Options)) (*codecommit.ListRepositoriesOutput, error)
	GetRepository(context.Context, *codecommit.GetRepositoryInput, ...func(*codecommit.Options)) (*codecommit.GetRepositoryOutput, error)
	ListBranches(context.Context, *codecommit.ListBranchesInput, ...func(*codecommit.Options)) (*codecommit.ListBranchesOutput, error)
	GetBranch(context.Context, *codecommit.GetBranchInput, ...func(*codecommit.Options)) (*codecommit.GetBranchOutput, error)
	GetFolder(context.Context, *codecommit.GetFolderInput, ...func(*codecommit.Options)) (*codecommit.GetFolderOutput, error)
}

// taggingAPI is the subset of the Resource Groups Tagging client the provider uses.
type taggingAPI interface {
	GetResources(context.Context, *resourcegroupstaggingapi.GetResourcesInput, ...func(*resourcegroupstaggingapi.Options)) (*resourcegroupstaggingapi.GetResourcesOutput, error)
}

// Provider is the AWS CodeCommit provider. It is safe for concurrent use.
type Provider struct {
	info       scm.Info
	cc         codeCommitAPI
	tagging    taggingAPI
	tagFilters []TagFilter
	maxPages   int
	logger     *slog.Logger
}

var (
	_ scm.Provider         = (*Provider)(nil)
	_ scm.RepositoryLister = (*Provider)(nil)
	_ scm.BranchLister     = (*Provider)(nil)
	_ scm.PathChecker      = (*Provider)(nil)
)

// New is the scm.Factory for CodeCommit (default AWS configuration, no tag filters).
func New(ctx context.Context, cfg scm.Config) (scm.Provider, error) {
	return NewProvider(ctx, cfg, Options{})
}

// Register adds the CodeCommit factory to r.
func Register(r *scm.Registry) error { return r.Register(Type, New) }

// NewProvider builds a CodeCommit provider. Unless opts.AWSConfig is set, it loads the
// default AWS configuration (environment and shared configuration files; no network
// I/O: credentials are resolved lazily on the first call). cfg.APIURL must be empty
// (upstream has no custom API URL, so the ApplicationSet API URL allow-list never applies
// to CodeCommit); custom endpoints go through opts.AWSConfig.BaseEndpoint.
//
// Every SDK request goes through Patrol's HTTP stack (TLS settings, per-attempt timeout,
// response size cap, pacing, observer, redacted logging); retries are the SDK's own
// (AWS JSON requests are POSTs, which scm.Chain never retries), bounded by
// cfg.Retry.MaxAttempts and cfg.Retry.MaxDelay.
func NewProvider(ctx context.Context, cfg scm.Config, opts Options) (*Provider, error) {
	cfg = cfg.WithDefaults()
	if strings.TrimSpace(cfg.APIURL) != "" {
		return nil, invalidErr("New", errors.New("AWS CodeCommit has no API URL; use Options.AWSConfig.BaseEndpoint for custom endpoints"))
	}
	for _, f := range opts.TagFilters {
		if strings.TrimSpace(f.Key) == "" {
			return nil, invalidErr("New", errors.New("tag filter key must not be empty"))
		}
	}
	var awsCfg aws.Config
	if opts.AWSConfig != nil {
		awsCfg = opts.AWSConfig.Copy()
	} else {
		loaded, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, invalidErr("New", fmt.Errorf("error loading default config: %w", err))
		}
		awsCfg = loaded
	}
	if opts.Region != "" {
		awsCfg.Region = opts.Region
	}
	if awsCfg.Region == "" {
		return nil, invalidErr("New", errors.New("an AWS region is required (Options.Region or the AWS environment)"))
	}
	hc, err := httpClient(cfg)
	if err != nil {
		return nil, err
	}
	awsCfg.HTTPClient = hc
	awsCfg.Retryer = func() aws.Retryer {
		return retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = cfg.Retry.MaxAttempts
			o.MaxBackoff = cfg.Retry.MaxDelay
			// Patrol paces requests itself; the SDK's retry quota would turn sustained
			// throttling into opaque "retry quota exceeded" errors.
			o.RateLimiter = ratelimit.None
		})
	}
	if opts.Role != "" {
		awsCfg.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(awsCfg), opts.Role))
	}
	return &Provider{
		info: scm.Info{
			Type:            Type,
			Name:            cfg.Name,
			ContractVersion: scm.ContractVersion,
			Capabilities:    []scm.Capability{scm.CapabilityRepositories, scm.CapabilityBranches, scm.CapabilityPaths},
		},
		cc:         codecommit.NewFromConfig(awsCfg),
		tagging:    resourcegroupstaggingapi.NewFromConfig(awsCfg),
		tagFilters: slices.Clone(opts.TagFilters),
		maxPages:   cfg.MaxPages,
		logger:     cfg.Logger,
	}, nil
}

// httpClient builds the HTTP client handed to the SDK: base transport (TLS, CA, proxy
// from scm.Config) → per-attempt timeout → response size cap → scm.Chain (logging,
// pacing, observer; POSTs are not retried there).
func httpClient(cfg scm.Config) (*http.Client, error) {
	base, err := scm.BaseTransport(cfg)
	if err != nil {
		return nil, err
	}
	attempt := cfg.Timeout
	if cfg.HTTPClient != nil && cfg.HTTPClient.Timeout > 0 {
		attempt = cfg.HTTPClient.Timeout
	}
	inner := scm.LimitResponseBody(scm.AttemptTimeout(base, attempt), cfg.MaxResponseBytes)
	return &http.Client{Transport: scm.Chain(inner, scm.ChainOptions{
		Provider: Type,
		Logger:   cfg.Logger,
		Limiter:  scm.NewLimiter(cfg.RateLimit),
		Observer: cfg.Observer,
		Retry:    cfg.Retry,
		Clock:    cfg.Clock,
	})}, nil
}

// Info implements scm.Provider.
func (p *Provider) Info() scm.Info {
	i := p.info
	i.Capabilities = append([]scm.Capability(nil), p.info.Capabilities...)
	return i
}

// throttleRetryAfter is reported for AWS throttling errors, which carry no reset hint.
const throttleRetryAfter = 10 * time.Second

func invalidErr(op string, err error) error {
	return &scm.Error{Kind: scm.KindInvalid, Provider: Type, Op: op, Err: err}
}
