package gitlab

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// PRStateLocked selects locked merge requests (GitLab-only state accepted by
// ListPullRequests in addition to the contract states).
const PRStateLocked scm.PRState = "locked"

type glMergeRequest struct {
	IID            int64    `json:"iid"`
	Title          string   `json:"title"`
	SourceBranch   string   `json:"source_branch"`
	TargetBranch   string   `json:"target_branch"`
	SHA            string   `json:"sha"`
	Labels         []string `json:"labels"`
	WebURL         string   `json:"web_url"`
	State          string   `json:"state"`
	Draft          bool     `json:"draft"`
	WorkInProgress bool     `json:"work_in_progress"`
	Author         struct {
		Username string `json:"username"`
	} `json:"author"`
}

// ListPullRequests implements scm.PullRequestLister with GET
// /projects/:id/merge_requests (100 per page, every page).
//
// State mapping (GitLab vocabulary, as upstream's pullRequestState): "" and open →
// state=opened, closed → closed (not merged), merged → merged, PRStateLocked → locked,
// all → every state. Note that upstream's default (empty pullRequestState) is every
// state: callers adapting the generator must pass scm.PRStateAll for it.
//
// Labels are sent as GitLab's server-side "labels" filter and are not re-checked
// client-side, exactly as upstream. GitLab therefore applies its own semantics: every
// listed label is required, and the predefined keywords "None" (merge requests without
// labels) and "Any" (merge requests with at least one label) are honored,
// case-insensitively. This intentionally widens scm.PRQuery.Labels' "all of these
// labels, exact match" wording for parity.
// TargetBranch maps to target_branch. Number is the MR IID, HeadSHA the MR "sha",
// Author the author's username and State GitLab's raw state.
func (p *Provider) ListPullRequests(ctx context.Context, ref scm.RepoRef, q scm.PRQuery) ([]scm.PullRequest, error) {
	const op = "ListPullRequests"
	pid, err := projectID(op, ref)
	if err != nil {
		return nil, err
	}
	params := url.Values{}
	switch q.State {
	case "", scm.PRStateOpen:
		params.Set("state", "opened")
	case scm.PRStateClosed:
		params.Set("state", "closed")
	case scm.PRStateMerged:
		params.Set("state", "merged")
	case PRStateLocked:
		params.Set("state", "locked")
	case scm.PRStateAll:
		params.Set("state", "all")
	default:
		return nil, invalidErr(op, fmt.Errorf("unknown pull request state %q", q.State))
	}
	if len(q.Labels) > 0 {
		params.Set("labels", strings.Join(q.Labels, ","))
	}
	if q.TargetBranch != "" {
		params.Set("target_branch", q.TargetBranch)
	}
	mrs, err := collect[glMergeRequest](ctx, p.c, op, "projects/"+pid+"/merge_requests", params)
	if err != nil {
		return nil, err
	}
	out := []scm.PullRequest{}
	for _, mr := range mrs {
		if q.TargetBranch != "" && mr.TargetBranch != q.TargetBranch {
			continue
		}
		out = append(out, scm.PullRequest{
			Number:       mr.IID,
			Title:        mr.Title,
			Branch:       mr.SourceBranch,
			TargetBranch: mr.TargetBranch,
			HeadSHA:      mr.SHA,
			Author:       mr.Author.Username,
			Labels:       slices.Clone(mr.Labels),
			URL:          mr.WebURL,
			State:        mr.State,
			Draft:        mr.Draft || mr.WorkInProgress,
			Merged:       mr.State == "merged",
		})
	}
	return out, nil
}
