package webhook

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketcloud"
)

// DefaultBitbucketCallbackTimeout bounds the Bitbucket Cloud callback (upstream: 10s).
const DefaultBitbucketCallbackTimeout = 10 * time.Second

// BitbucketCredentials authenticate the Bitbucket Cloud callback. A Username of
// "x-token-auth" sends Password as a bearer token; another Username with a Password uses
// basic authentication; otherwise BearerToken (possibly empty: anonymous) is sent, as
// upstream does with repository credentials.
type BitbucketCredentials struct {
	Username    string
	Password    string
	BearerToken string
}

// String redacts the secret values.
func (c BitbucketCredentials) String() string {
	return fmt.Sprintf("BitbucketCredentials{Username:%q, Password:<redacted>, BearerToken:<redacted>}", c.Username)
}

// LogValue implements slog.LogValuer without the secret values.
func (c BitbucketCredentials) LogValue() slog.Value {
	return slog.GroupValue(slog.String("username", c.Username))
}

// BitbucketCloudOptions configures the changed-files callback of verified Bitbucket Cloud
// pushes (REPO-119). Bitbucket Cloud push payloads list no files, so when the webhook UUID
// was verified the handler asks the Bitbucket API for the diffstat between the before
// and after commits and for the repository's main branch, as upstream does.
type BitbucketCloudOptions struct {
	// Disabled turns the callback off: every matching Application is refreshed.
	Disabled bool
	// AllowedAPIURLs lists the API base URLs the callback may call (default
	// https://api.bitbucket.org/2.0). The base is derived from the payload's
	// links.self.href, which is not covered by any signature, so other hosts are refused
	// to keep credentials from being sent elsewhere (upstream calls any host).
	AllowedAPIURLs []string
	// Credentials returns the credentials of the repository whose web URL is given (the
	// API server resolves them from Repository objects). Nil, or a nil error with empty
	// credentials, calls anonymously, which only works for public repositories.
	Credentials func(ctx context.Context, repoURL string) (BitbucketCredentials, error)
	// Timeout bounds the callback (default 10s).
	Timeout time.Duration
	// HTTPClient is used for the callback (tests); nil uses the scm transport defaults.
	HTTPClient *http.Client
}

type bitbucketRefiner struct {
	opts    BitbucketCloudOptions
	allowed map[string]bool
	log     *slog.Logger
}

func newBitbucketRefiner(opts BitbucketCloudOptions, log *slog.Logger) (*bitbucketRefiner, error) {
	if opts.Disabled {
		return nil, nil
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultBitbucketCallbackTimeout
	}
	if len(opts.AllowedAPIURLs) == 0 {
		opts.AllowedAPIURLs = []string{bitbucketcloud.DefaultAPIURL}
	}
	r := &bitbucketRefiner{opts: opts, allowed: map[string]bool{}, log: log}
	for _, a := range opts.AllowedAPIURLs {
		u, err := bitbucketcloud.APIBaseURL(a)
		if err != nil {
			return nil, fmt.Errorf("webhook: Bitbucket Cloud allowed API URL: %w", err)
		}
		r.allowed[canonicalAPIBase(u.String())] = true
	}
	return r, nil
}

func canonicalAPIBase(u string) string { return strings.ToLower(strings.TrimRight(u, "/")) }

// refine replaces the conservative defaults of a verified Bitbucket Cloud push (every
// path changed, default branch touched) with the diffstat result and the main branch
// check. Any failure keeps the defaults, so matching Applications are still refreshed.
func (r *bitbucketRefiner) refine(ctx context.Context, ev *scm.Event) {
	if r == nil || ev.Provider != bitbucketcloud.Type || ev.Type != scm.EventPush || !ev.Verified || ev.Repository == nil {
		return
	}
	repo := ev.Repository
	log := r.log.With("provider", ProviderBitbucketCloud, "deliveryID", ev.DeliveryID, "repo", logValue(scm.RedactURLString(repo.HTMLURL)))
	base, err := bitbucketcloud.APIBaseURLFromRepositoryURL(repo.APIURL, repo.FullName())
	if err != nil {
		log.Info("webhook: Bitbucket Cloud callback skipped", "error", logErr(err))
		return
	}
	if !r.allowed[canonicalAPIBase(base)] {
		log.Warn("webhook: Bitbucket Cloud callback refused: API URL is not allowed", "apiURL", logValue(scm.RedactURLString(base)))
		return
	}
	var creds BitbucketCredentials
	if r.opts.Credentials != nil {
		creds, err = r.opts.Credentials(ctx, repo.HTMLURL)
		if err != nil {
			log.Info("webhook: Bitbucket Cloud repository credentials unavailable; calling anonymously", "error", logErr(err))
			creds = BitbucketCredentials{}
		}
	}
	cfg := scm.Config{Type: bitbucketcloud.Type, Name: "webhook-callback", APIURL: base, HTTPClient: r.opts.HTTPClient, Timeout: r.opts.Timeout, Logger: r.log}
	var opts bitbucketcloud.Options
	switch {
	case creds.Username != "" && creds.Password != "":
		opts.Username = creds.Username
		pw := creds.Password
		cfg.Token = func(context.Context) (string, error) { return pw, nil }
	case creds.BearerToken != "":
		tok := creds.BearerToken
		cfg.Token = func(context.Context) (string, error) { return tok, nil }
	}
	p, err := bitbucketcloud.NewProvider(ctx, cfg, opts)
	if err != nil {
		log.Info("webhook: Bitbucket Cloud callback client", "error", logErr(err))
		return
	}
	cctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	files, truncated, err := p.ChangedFiles(cctx, repo.RepoRef, ev.Before, ev.After)
	if err != nil {
		log.Info("webhook: Bitbucket Cloud diffstat failed; every path is considered changed", "error", logErr(err))
	} else {
		ev.ChangedFiles, ev.ChangedFilesTruncated = files, truncated
	}
	touched, err := p.TouchesMainBranch(cctx, repo.RepoRef, ev.Revision)
	if err != nil {
		log.Info("webhook: Bitbucket Cloud main branch lookup failed; assuming the main branch moved", "error", logErr(err))
		return
	}
	ev.TouchedDefaultBranch = touched
}
