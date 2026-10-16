package azuredevops

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

type azPull struct {
	PullRequestID int64  `json:"pullRequestId"`
	Title         string `json:"title"`
	Status        string `json:"status"`
	IsDraft       bool   `json:"isDraft"`
	SourceRefName string `json:"sourceRefName"`
	TargetRefName string `json:"targetRefName"`
	URL           string `json:"url"`
	CreatedBy     struct {
		UniqueName  string `json:"uniqueName"`
		DisplayName string `json:"displayName"`
	} `json:"createdBy"`
	LastMergeSourceCommit *struct {
		CommitID string `json:"commitId"`
	} `json:"lastMergeSourceCommit"`
	Labels []struct {
		Name   string `json:"name"`
		Active *bool  `json:"active"`
	} `json:"labels"`
	Repository *struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		WebURL  string `json:"webUrl"`
		Project struct {
			Name string `json:"name"`
		} `json:"project"`
	} `json:"repository"`
}

func (pr azPull) labelNames() []string {
	out := []string{}
	for _, l := range pr.Labels {
		out = append(out, l.Name)
	}
	return out
}

// author returns the part of the creator's unique name before '@', as upstream.
func (pr azPull) author() string {
	name, _, _ := strings.Cut(pr.CreatedBy.UniqueName, "@")
	return name
}

// ListPullRequests implements scm.PullRequestLister like upstream: the pull requests
// of the team project (RepoRef.Owner) are listed (GET
// {project}/_apis/git/pullrequests, status active by default, every $top/$skip page;
// upstream reads only the first page) and those whose repository name equals
// RepoRef.Name are kept. A missing project fails with KindNotFound (upstream's
// "The following project does not exist"); a missing repository in an existing project
// yields no pull requests, as upstream. closed lists completed and abandoned pull
// requests, merged only completed ones. Labels are filtered client-side (all required,
// exact). Pull requests missing their repository, ID, refs or last merge source commit
// are skipped. Branch names have "refs/heads/" stripped; Author is the creator's unique
// name up to '@'; State is the raw Azure status.
func (p *Provider) ListPullRequests(ctx context.Context, ref scm.RepoRef, q scm.PRQuery) ([]scm.PullRequest, error) {
	const op = "ListPullRequests"
	if _, err := repoPath(op, ref); err != nil {
		return nil, err
	}
	status := ""
	switch q.State {
	case "", scm.PRStateOpen:
		status = "active"
	case scm.PRStateMerged:
		status = "completed"
	case scm.PRStateClosed, scm.PRStateAll:
		status = "all"
	default:
		return nil, invalidErr(op, fmt.Errorf("unknown pull request state %q", q.State))
	}
	params := url.Values{"searchCriteria.status": {status}}
	if q.TargetBranch != "" {
		params.Set("searchCriteria.targetRefName", "refs/heads/"+q.TargetBranch)
	}
	path := url.PathEscape(ref.Owner) + "/_apis/git/pullrequests"
	pulls, err := scm.CollectPages(ctx, p.c.maxPages, func(ctx context.Context, skip int) ([]azPull, int, error) {
		qq := versioned(cloneValues(params))
		qq.Set("$top", strconv.Itoa(prPageSize))
		if skip > 0 {
			qq.Set("$skip", strconv.Itoa(skip))
		}
		var page list[azPull]
		if _, err := p.c.get(ctx, op, path, qq, &page); err != nil {
			return nil, 0, err
		}
		if len(page.Value) < prPageSize {
			return page.Value, 0, nil
		}
		return page.Value, skip + len(page.Value), nil
	})
	if err != nil {
		return nil, fillOp(op, err)
	}
	out := []scm.PullRequest{}
	for _, pr := range pulls {
		if pr.Repository == nil || pr.Repository.Name == "" || pr.PullRequestID == 0 || pr.SourceRefName == "" ||
			pr.TargetRefName == "" || pr.LastMergeSourceCommit == nil || pr.LastMergeSourceCommit.CommitID == "" {
			continue
		}
		if pr.Repository.Name != ref.Name {
			continue
		}
		if q.State == scm.PRStateClosed && pr.Status != "completed" && pr.Status != "abandoned" {
			continue
		}
		target := strings.TrimPrefix(pr.TargetRefName, "refs/heads/")
		if q.TargetBranch != "" && target != q.TargetBranch {
			continue
		}
		labels := pr.labelNames()
		if !hasAll(labels, q.Labels) {
			continue
		}
		web := ""
		if pr.Repository.WebURL != "" {
			web = strings.TrimRight(pr.Repository.WebURL, "/") + "/pullrequest/" + strconv.FormatInt(pr.PullRequestID, 10)
		}
		out = append(out, scm.PullRequest{
			Number:       pr.PullRequestID,
			Title:        pr.Title,
			Branch:       strings.TrimPrefix(pr.SourceRefName, "refs/heads/"),
			TargetBranch: target,
			HeadSHA:      pr.LastMergeSourceCommit.CommitID,
			Author:       pr.author(),
			Labels:       labels,
			URL:          web,
			State:        pr.Status,
			Draft:        pr.IsDraft,
			Merged:       pr.Status == "completed",
		})
	}
	return out, nil
}

func hasAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}
