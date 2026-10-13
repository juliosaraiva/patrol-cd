package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bradleyfalzon/ghinstallation/v2"
)

const (
	githubDefaultAPI           = "https://api.github.com"
	githubMaxStatusDescription = 140
	githubMaxCommentLength     = 65536
	githubMaxPages             = 10
	githubCommentMarkerPrefix  = "<!-- patrol-notifications "
	// Comments created by an Argo CD installation are recognized for upserts so that a
	// migrated configuration keeps updating the same comment.
	githubLegacyMarkerPrefix = "<!-- argocd-notifications "
)

// GitHubOptions configure a github service (GitHub App authentication).
type GitHubOptions struct {
	AppID             flexString `json:"appID"`
	InstallationID    flexString `json:"installationID"`
	PrivateKey        string     `json:"privateKey"`
	EnterpriseBaseURL string     `json:"enterpriseBaseURL"`
	TransportOptions
}

type githubService struct {
	apiURL string
	http   *httpClient
	deps   Deps
}

func newGitHubService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts GitHubOptions
	if err := decodeOptions(TypeGitHub, raw, &opts); err != nil {
		return nil, err
	}
	r.Add(opts.PrivateKey)
	appID, err := opts.AppID.int64()
	if err != nil || appID <= 0 {
		return nil, fmt.Errorf("%w: github: appID must be a positive integer", ErrInvalidOptions)
	}
	installationID, err := opts.InstallationID.int64()
	if err != nil || installationID <= 0 {
		return nil, fmt.Errorf("%w: github: installationID must be a positive integer", ErrInvalidOptions)
	}
	if strings.TrimSpace(opts.PrivateKey) == "" {
		return nil, fmt.Errorf("%w: github: privateKey is required", ErrInvalidOptions)
	}
	api := strings.TrimRight(strings.TrimSpace(opts.EnterpriseBaseURL), "/")
	if api == "" {
		api = githubDefaultAPI
	}
	if u, perr := parseEndpoint(api); perr != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("%w: github: invalid enterpriseBaseURL", ErrInvalidOptions)
	}
	var keyErr error
	wrap := func(base http.RoundTripper) http.RoundTripper {
		itr, err := ghinstallation.New(base, appID, installationID, []byte(opts.PrivateKey))
		if err != nil {
			keyErr = err
			return base
		}
		itr.BaseURL = api
		return itr
	}
	hc, err := newHTTPClient(TypeGitHub, deps, opts.TransportOptions, r, wrap)
	if err != nil {
		return nil, err
	}
	if keyErr != nil {
		return nil, fmt.Errorf("%w: github: invalid privateKey: %v", ErrInvalidOptions, keyErr)
	}
	return &githubService{apiURL: api, http: hc, deps: deps}, nil
}

func githubHeaders() http.Header {
	h := http.Header{}
	h.Set("Accept", "application/vnd.github+json")
	h.Set("X-GitHub-Api-Version", "2022-11-28")
	return h
}

func (s *githubService) Send(ctx context.Context, n Notification, _ Destination) error {
	gh := n.GitHub
	if gh == nil {
		return fmt.Errorf("%w: github: template has no github section", ErrInvalidNotification)
	}
	fullName := FullNameByRepoURL(gh.RepoURLPath)
	owner, repo, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" || repo == "" {
		return fmt.Errorf("%w: github: GitHub.repoURL (%s) does not have a /", ErrInvalidNotification, gh.RepoURLPath)
	}
	sha := strings.TrimSpace(gh.RevisionPath)
	if sha == "" || sha == "<no value>" {
		return fmt.Errorf("%w: github: revision is empty", ErrInvalidNotification)
	}
	repoAPI := s.apiURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)

	if gh.Status != nil {
		if err := s.createStatus(ctx, repoAPI, sha, n.Message, gh.Status); err != nil {
			return err
		}
	}
	if gh.Deployment != nil {
		if err := s.createDeployment(ctx, repoAPI, sha, n.Message, gh.Deployment); err != nil {
			return err
		}
	}
	if gh.PullRequestComment != nil {
		if err := s.upsertPullRequestComments(ctx, repoAPI, sha, gh.PullRequestComment); err != nil {
			return err
		}
	}
	if gh.CheckRun != nil {
		if err := s.createCheckRun(ctx, repoAPI, sha, gh.CheckRun); err != nil {
			return err
		}
	}
	return nil
}

func (s *githubService) call(ctx context.Context, method, endpoint string, payload any, out any) error {
	resp, err := s.http.doJSON(ctx, method, endpoint, githubHeaders(), payload)
	if err != nil {
		return err
	}
	if err := s.http.expect2xx(method, endpoint, resp); err != nil {
		return err
	}
	if out != nil && len(resp.Body) > 0 {
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return fmt.Errorf("%w: github: %s %s: invalid response: %v", ErrDeliveryFailed, method, SanitizeURL(endpoint), err)
		}
	}
	return nil
}

func (s *githubService) createStatus(ctx context.Context, repoAPI, sha, message string, st *GitHubStatus) error {
	payload := map[string]any{"state": st.State}
	setIf(payload, "description", truncateRunes(message, githubMaxStatusDescription))
	setIf(payload, "target_url", st.TargetURL)
	setIf(payload, "context", st.Label)
	err := s.call(ctx, http.MethodPost, repoAPI+"/statuses/"+url.PathEscape(sha), payload, nil)
	var se *StatusError
	if errors.As(err, &se) && se.StatusCode == http.StatusUnprocessableEntity &&
		strings.Contains(strings.ToLower(se.Body), "maximum number of statuses") {
		// GitHub caps statuses per SHA+context at 1000; further attempts are pointless.
		s.deps.logger().Warn("github: commit status limit reached; ignoring", "context", st.Label)
		return nil
	}
	return err
}

func (s *githubService) createDeployment(ctx context.Context, repoAPI, sha, message string, d *GitHubDeployment) error {
	ref := firstNonEmpty(d.Reference, sha)
	q := url.Values{}
	q.Set("ref", ref)
	if d.Environment != "" {
		q.Set("environment", d.Environment)
	}
	var existing []struct {
		ID int64 `json:"id"`
	}
	if err := s.call(ctx, http.MethodGet, repoAPI+"/deployments?"+q.Encode(), nil, &existing); err != nil {
		return err
	}
	var id int64
	if len(existing) > 0 {
		id = existing[0].ID
	} else {
		autoMerge := true
		if d.AutoMerge != nil {
			autoMerge = *d.AutoMerge
		}
		payload := map[string]any{"ref": ref, "auto_merge": autoMerge}
		setIf(payload, "environment", d.Environment)
		if d.RequiredContexts != nil {
			payload["required_contexts"] = d.RequiredContexts
		}
		if d.TransientEnvironment != nil {
			payload["transient_environment"] = *d.TransientEnvironment
		}
		var created struct {
			ID int64 `json:"id"`
		}
		if err := s.call(ctx, http.MethodPost, repoAPI+"/deployments", payload, &created); err != nil {
			return err
		}
		id = created.ID
	}
	if id == 0 {
		return fmt.Errorf("%w: github: deployment id missing in response", ErrDeliveryFailed)
	}
	status := map[string]any{"state": d.State}
	setIf(status, "description", truncateRunes(message, githubMaxStatusDescription))
	setIf(status, "log_url", d.LogURL)
	setIf(status, "environment_url", d.EnvironmentURL)
	setIf(status, "environment", d.Environment)
	return s.call(ctx, http.MethodPost, repoAPI+"/deployments/"+strconv.FormatInt(id, 10)+"/statuses", status, nil)
}

func commentMarker(prefix, tag string) string { return prefix + tag + " -->" }

func (s *githubService) upsertPullRequestComments(ctx context.Context, repoAPI, sha string, c *GitHubPullRequestComment) error {
	body := c.Content
	if c.CommentTag != "" {
		marker := "\n" + commentMarker(githubCommentMarkerPrefix, c.CommentTag)
		body = truncateRunes(body, githubMaxCommentLength-len([]rune(marker))) + marker
	} else {
		body = truncateRunes(body, githubMaxCommentLength)
	}
	var prs []struct {
		Number int64 `json:"number"`
	}
	if err := s.call(ctx, http.MethodGet, repoAPI+"/commits/"+url.PathEscape(sha)+"/pulls?per_page=100", nil, &prs); err != nil {
		return err
	}
	for _, pr := range prs {
		num := strconv.FormatInt(pr.Number, 10)
		if c.CommentTag != "" {
			id, err := s.findTaggedComment(ctx, repoAPI, num, c.CommentTag)
			if err != nil {
				return err
			}
			if id != 0 {
				if err := s.call(ctx, http.MethodPatch, repoAPI+"/issues/comments/"+strconv.FormatInt(id, 10), map[string]string{"body": body}, nil); err != nil {
					return err
				}
				continue
			}
		}
		if err := s.call(ctx, http.MethodPost, repoAPI+"/issues/"+num+"/comments", map[string]string{"body": body}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *githubService) findTaggedComment(ctx context.Context, repoAPI, prNumber, tag string) (int64, error) {
	markers := []string{commentMarker(githubCommentMarkerPrefix, tag), commentMarker(githubLegacyMarkerPrefix, tag)}
	for page := 1; page <= githubMaxPages; page++ {
		var comments []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
		}
		endpoint := repoAPI + "/issues/" + prNumber + "/comments?per_page=100&page=" + strconv.Itoa(page)
		if err := s.call(ctx, http.MethodGet, endpoint, nil, &comments); err != nil {
			return 0, err
		}
		for _, cm := range comments {
			for _, m := range markers {
				if strings.Contains(cm.Body, m) {
					return cm.ID, nil
				}
			}
		}
		if len(comments) < 100 {
			break
		}
	}
	return 0, nil
}

func (s *githubService) createCheckRun(ctx context.Context, repoAPI, sha string, cr *GitHubCheckRun) error {
	if cr.Name == "" {
		return fmt.Errorf("%w: github: checkRun.name is required", ErrInvalidNotification)
	}
	payload := map[string]any{"name": cr.Name, "head_sha": sha}
	setIf(payload, "details_url", cr.DetailsURL)
	setIf(payload, "external_id", cr.ExternalID)
	setIf(payload, "status", cr.Status)
	setIf(payload, "conclusion", cr.Conclusion)
	setIf(payload, "started_at", cr.StartedAt)
	setIf(payload, "completed_at", cr.CompletedAt)
	if o := cr.Output; o != nil && (o.Title != "" || o.Summary != "" || o.Text != "") {
		out := map[string]any{"title": o.Title, "summary": o.Summary}
		setIf(out, "text", o.Text)
		payload["output"] = out
	}
	return s.call(ctx, http.MethodPost, repoAPI+"/check-runs", payload, nil)
}
