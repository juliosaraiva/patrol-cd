package github

import (
	"context"
	"fmt"
	"regexp"

	gh "github.com/google/go-github/v88/github"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// GitHub API limits.
const (
	maxStatusDescription = 140
	maxCheckRunText      = 65535
)

var commitSHARE = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

func (p *Provider) validateSHA(op, sha string) error {
	if !commitSHARE.MatchString(sha) {
		return p.invalid(op, fmt.Errorf("commit %q must be a full hexadecimal SHA", sha))
	}
	return nil
}

// SetCommitStatus implements scm.CommitStatusReporter via POST
// /repos/{o}/{r}/statuses/{sha}. Descriptions are truncated to 140 characters. GitHub
// rejects more than 1000 statuses per (sha, context) with 422, which maps to KindInvalid.
func (p *Provider) SetCommitStatus(ctx context.Context, ref scm.RepoRef, sha string, st scm.CommitStatus) error {
	const op = "SetCommitStatus"
	if err := st.Validate(); err != nil {
		var se *scm.Error
		if asSCM(err, &se) {
			se.Provider = Type
		}
		return err
	}
	if err := p.validateRef(op, ref); err != nil {
		return err
	}
	if err := p.validateSHA(op, sha); err != nil {
		return err
	}
	status := gh.RepoStatus{
		State:   gh.Ptr(string(st.State)),
		Context: gh.Ptr(st.Context),
	}
	if st.Description != "" {
		status.Description = gh.Ptr(scm.TruncateRunes(st.Description, maxStatusDescription))
	}
	if st.TargetURL != "" {
		status.TargetURL = gh.Ptr(st.TargetURL)
	}
	_, resp, err := p.client.Repositories.CreateStatus(ctx, ref.Owner, ref.Name, sha, status)
	if err != nil {
		return p.mapErr(op, resp, err)
	}
	return nil
}

// UpsertCheckRun implements scm.CheckRunReporter. The existing run is looked up with
// GET /repos/{o}/{r}/commits/{sha}/check-runs?check_name=<name>&filter=all&app_id=<app>
// and matched by external_id; it is then PATCHed, otherwise a new run is POSTed.
// Concurrent upserts for the same (sha, name, externalID) must be serialized by the
// caller (GitHub has no conditional create).
func (p *Provider) UpsertCheckRun(ctx context.Context, ref scm.RepoRef, sha string, run scm.CheckRun) (int64, error) {
	const op = "UpsertCheckRun"
	if err := p.requireApp(op); err != nil {
		return 0, err
	}
	if err := run.Validate(); err != nil {
		var se *scm.Error
		if asSCM(err, &se) {
			se.Provider = Type
		}
		return 0, err
	}
	if err := p.validateRef(op, ref); err != nil {
		return 0, err
	}
	if err := p.validateSHA(op, sha); err != nil {
		return 0, err
	}
	existing, err := p.findCheckRun(ctx, op, ref, sha, run)
	if err != nil {
		return 0, err
	}
	var output *gh.CheckRunOutput
	if run.Title != "" {
		summary := run.Summary
		if summary == "" {
			summary = run.Title
		}
		output = &gh.CheckRunOutput{Title: gh.Ptr(run.Title), Summary: gh.Ptr(scm.TruncateRunes(summary, maxCheckRunText))}
		if run.Text != "" {
			output.Text = gh.Ptr(scm.TruncateRunes(run.Text, maxCheckRunText))
		}
	}
	var conclusion, detailsURL *string
	var completedAt, startedAt *gh.Timestamp
	if run.Conclusion != scm.CheckConclusionNone {
		conclusion = gh.Ptr(string(run.Conclusion))
		t := run.CompletedAt
		if t.IsZero() {
			t = p.now()
		}
		completedAt = &gh.Timestamp{Time: t.UTC()}
	}
	if !run.StartedAt.IsZero() {
		startedAt = &gh.Timestamp{Time: run.StartedAt.UTC()}
	}
	if run.DetailsURL != "" {
		detailsURL = gh.Ptr(run.DetailsURL)
	}
	if existing != 0 {
		cr, resp, err := p.client.Checks.UpdateCheckRun(ctx, ref.Owner, ref.Name, existing, gh.UpdateCheckRunOptions{
			Name:        run.Name,
			ExternalID:  gh.Ptr(run.ExternalID),
			DetailsURL:  detailsURL,
			Status:      gh.Ptr(string(run.Status)),
			Conclusion:  conclusion,
			CompletedAt: completedAt,
			Output:      output,
		})
		if err != nil {
			return 0, p.mapErr(op, resp, err)
		}
		return cr.GetID(), nil
	}
	cr, resp, err := p.client.Checks.CreateCheckRun(ctx, ref.Owner, ref.Name, gh.CreateCheckRunOptions{
		Name:        run.Name,
		HeadSHA:     sha,
		ExternalID:  gh.Ptr(run.ExternalID),
		DetailsURL:  detailsURL,
		Status:      gh.Ptr(string(run.Status)),
		Conclusion:  conclusion,
		StartedAt:   startedAt,
		CompletedAt: completedAt,
		Output:      output,
	})
	if err != nil {
		return 0, p.mapErr(op, resp, err)
	}
	return cr.GetID(), nil
}

func (p *Provider) findCheckRun(ctx context.Context, op string, ref scm.RepoRef, sha string, run scm.CheckRun) (int64, error) {
	opts := &gh.ListCheckRunsOptions{CheckName: gh.Ptr(run.Name), Filter: gh.Ptr("all")}
	if p.app != nil {
		opts.AppID = gh.Ptr(p.app.appID)
	}
	runs, err := collect(ctx, p, op, func(ctx context.Context, lo gh.ListOptions) ([]*gh.CheckRun, *gh.Response, error) {
		o := *opts
		o.ListOptions = lo
		res, resp, err := p.client.Checks.ListCheckRunsForRef(ctx, ref.Owner, ref.Name, sha, &o)
		if err != nil {
			return nil, resp, err
		}
		return res.CheckRuns, resp, nil
	})
	if err != nil {
		return 0, err
	}
	for _, r := range runs {
		if r.GetExternalID() == run.ExternalID && r.GetName() == run.Name {
			return r.GetID(), nil
		}
	}
	return 0, nil
}
