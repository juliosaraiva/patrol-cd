package bitbucketserver

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

type bsRef struct {
	ID           string `json:"id"`
	DisplayID    string `json:"displayId"`
	LatestCommit string `json:"latestCommit"`
}

type bsPull struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	From   bsRef  `json:"fromRef"`
	To     bsRef  `json:"toRef"`
	Author struct {
		User struct {
			Name string `json:"name"`
			Slug string `json:"slug"`
		} `json:"user"`
	} `json:"author"`
	Links struct {
		Self []bsLink `json:"self"`
	} `json:"links"`
}

// ErrLabelsUnsupported is the cause of the KindInvalid error returned when a pull
// request query filters on labels, which Bitbucket Server does not have.
var ErrLabelsUnsupported = errors.New("bitbucket server pull requests have no labels")

// ListPullRequests implements scm.PullRequestLister with GET
// .../pull-requests?state=OPEN (limit 100, every page), as upstream. closed lists
// DECLINED and MERGED pull requests, merged only MERGED, all every state. TargetBranch
// is sent as at=refs/heads/<branch> (incoming direction). Labels are unsupported (no
// labels upstream either): a non-empty PRQuery.Labels fails with KindInvalid wrapping
// ErrLabelsUnsupported rather than silently matching nothing. Branch and TargetBranch
// are the ref display IDs, HeadSHA the source ref's latest commit, Author the author's
// user name and State Bitbucket's raw state.
func (p *Provider) ListPullRequests(ctx context.Context, ref scm.RepoRef, q scm.PRQuery) ([]scm.PullRequest, error) {
	const op = "ListPullRequests"
	rp, err := repoPath(op, ref)
	if err != nil {
		return nil, err
	}
	if len(q.Labels) > 0 {
		return nil, invalidErr(op, ErrLabelsUnsupported)
	}
	params := url.Values{}
	switch q.State {
	case "", scm.PRStateOpen:
		params.Set("state", "OPEN")
	case scm.PRStateMerged:
		params.Set("state", "MERGED")
	case scm.PRStateClosed, scm.PRStateAll:
		params.Set("state", "ALL")
	default:
		return nil, invalidErr(op, fmt.Errorf("unknown pull request state %q", q.State))
	}
	if q.TargetBranch != "" {
		params.Set("at", "refs/heads/"+q.TargetBranch)
		params.Set("direction", "INCOMING")
	}
	pulls, err := collect[bsPull](ctx, p.c, op, rp+"/pull-requests", params)
	if err != nil {
		return nil, err
	}
	out := []scm.PullRequest{}
	for _, pr := range pulls {
		if q.State == scm.PRStateClosed && pr.State != "DECLINED" && pr.State != "MERGED" {
			continue
		}
		if q.TargetBranch != "" && pr.To.DisplayID != q.TargetBranch {
			continue
		}
		out = append(out, scm.PullRequest{
			Number:       pr.ID,
			Title:        pr.Title,
			Branch:       pr.From.DisplayID,
			TargetBranch: pr.To.DisplayID,
			HeadSHA:      pr.From.LatestCommit,
			Author:       pr.Author.User.Name,
			URL:          cloneLink(pr.Links.Self, ""),
			State:        pr.State,
			Draft:        pr.Draft,
			Merged:       pr.State == "MERGED",
		})
	}
	return out, nil
}
