package applications

import (
	"context"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	pubapi "github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// sourceFor selects the source of a revision request (upstream
// getAppSourceBySourceIndexAndVersionId): the spec sources, or those of the status.history
// entry versionID; then the 0-based sourceIndex (default 0).
func sourceFor(app *v1alpha1.Application, sourceIndex, versionID *int32) (v1alpha1.ApplicationSource, error) {
	sources := app.Spec.GetSources()
	if versionID != nil {
		if len(app.Status.History) == 0 {
			return v1alpha1.ApplicationSource{}, rest.InvalidArgument("version ID %d not found because the app has no history", *versionID)
		}
		i := slices.IndexFunc(app.Status.History, func(h v1alpha1.RevisionHistory) bool { return h.ID == int64(*versionID) })
		if i < 0 {
			return v1alpha1.ApplicationSource{}, rest.InvalidArgument("revision history not found for version ID %d", *versionID)
		}
		h := app.Status.History[i]
		switch {
		case len(h.Sources) > 0:
			sources = h.Sources
		case h.Source != nil:
			sources = v1alpha1.ApplicationSources{*h.Source}
		default:
			return v1alpha1.ApplicationSource{}, rest.InvalidArgument("revision history %d has no source", *versionID)
		}
	}
	idx := 0
	if sourceIndex != nil {
		idx = int(*sourceIndex)
		if idx < 0 {
			return v1alpha1.ApplicationSource{}, rest.InvalidArgument("source index %d is out of range", idx)
		}
		if idx >= len(sources) {
			if len(sources) == 1 {
				return v1alpha1.ApplicationSource{}, rest.InvalidArgument("source index %d not found because there is only 1 source", idx)
			}
			return v1alpha1.ApplicationSource{}, rest.InvalidArgument("source index %d not found because there are only %d sources", idx, len(sources))
		}
	}
	if len(sources) == 0 {
		return v1alpha1.ApplicationSource{}, rest.InvalidArgument("application %s has no source", app.Name)
	}
	return sources[idx], nil
}

// revisionSource authorizes the request and resolves the selected source and its
// repository access (the project's credentials; refused for repositories the project does
// not permit).
func (s *Service) revisionSource(ctx context.Context, req *pubapi.ApplicationRevisionRequest) (*v1alpha1.Application, *v1alpha1.AppProject, v1alpha1.ApplicationSource, *apiclient.Repository, RepoServer, error) {
	fail := func(err error) (*v1alpha1.Application, *v1alpha1.AppProject, v1alpha1.ApplicationSource, *apiclient.Repository, RepoServer, error) {
		return nil, nil, v1alpha1.ApplicationSource{}, nil, nil, err
	}
	app, proj, err := s.getApp(ctx, rbac.ActionGet, appRequest{name: req.Name, namespace: req.AppNamespace, project: req.Project})
	if err != nil {
		return fail(err)
	}
	if req.Revision == "" {
		return fail(rest.InvalidArgument("revision is required"))
	}
	source, err := sourceFor(app, req.SourceIndex, req.VersionID)
	if err != nil {
		return fail(err)
	}
	if err := checkSourcesPermitted(proj, []v1alpha1.ApplicationSource{source}); err != nil {
		return fail(err)
	}
	rs, err := s.repoServer()
	if err != nil {
		return fail(err)
	}
	repos, err := s.repositories()
	if err != nil {
		return fail(err)
	}
	repo, err := repos.GetRepository(ctx, source.RepoURL, proj.Name)
	if err != nil {
		return fail(repositoryError(err))
	}
	return app, proj, source, repo, rs, nil
}

// revisionMetadata serves GET /applications/{name}/revisions/{revision}/metadata.
func (s *Service) revisionMetadata(ctx context.Context, req *pubapi.ApplicationRevisionRequest) (*pubapi.ApplicationRevisionMetadata, error) {
	_, proj, _, repo, rs, err := s.revisionSource(ctx, req)
	if err != nil {
		return nil, err
	}
	si, _ := project.EffectiveSourceIntegrity(proj)
	md, err := rs.GetRevisionMetadata(ctx, &apiclient.RevisionMetadataRequest{Repo: *repo, Revision: req.Revision, SourceIntegrity: si, CheckSignature: si != nil})
	if err != nil {
		return nil, repoServerError("error getting revision metadata", err)
	}
	out := &pubapi.ApplicationRevisionMetadata{
		Author: md.Author, Tags: md.Tags, Message: md.Message, SignatureInfo: md.SignatureInfo,
	}
	if !md.Date.IsZero() {
		d := metav1.NewTime(md.Date)
		out.Date = &d
	}
	for _, r := range md.References {
		ref := pubapi.ApplicationRevisionReference{RepoURL: redactURL(r.RepoURL), SHA: r.SHA, Author: r.Author, Subject: r.Subject, Body: r.Body, Path: r.Path}
		if !r.Date.IsZero() {
			d := metav1.NewTime(r.Date)
			ref.Date = &d
		}
		out.References = append(out.References, ref)
	}
	if res := md.SourceIntegrityResult; res != nil {
		out.SourceIntegrityResult = &pubapi.ApplicationSourceIntegrityResult{}
		for _, c := range res.Checks {
			out.SourceIntegrityResult.Checks = append(out.SourceIntegrityResult.Checks, pubapi.ApplicationSourceIntegrityCheck{Name: c.Name, Problems: c.Problems})
		}
	}
	return out, nil
}

// revisionChartDetails serves GET /applications/{name}/revisions/{revision}/chartdetails.
func (s *Service) revisionChartDetails(ctx context.Context, req *pubapi.ApplicationRevisionRequest) (*pubapi.ApplicationChartDetails, error) {
	app, _, source, repo, rs, err := s.revisionSource(ctx, req)
	if err != nil {
		return nil, err
	}
	if source.Chart == "" {
		return nil, rest.InvalidArgument("no chart found for application: %v", app.Name)
	}
	cd, err := rs.GetRevisionChartDetails(ctx, &apiclient.ChartDetailsRequest{Repo: *repo, Name: source.Chart, Revision: req.Revision})
	if err != nil {
		return nil, repoServerError("error getting chart details", err)
	}
	return &pubapi.ApplicationChartDetails{Description: cd.Description, Home: cd.Home, Maintainers: cd.Maintainers}, nil
}

// revisionOCIMetadata serves GET /applications/{name}/revisions/{revision}/ocimetadata.
func (s *Service) revisionOCIMetadata(ctx context.Context, req *pubapi.ApplicationRevisionRequest) (*pubapi.ApplicationOCIMetadata, error) {
	_, _, source, repo, rs, err := s.revisionSource(ctx, req)
	if err != nil {
		return nil, err
	}
	md, err := rs.GetOCIMetadata(ctx, &apiclient.ChartDetailsRequest{Repo: *repo, Name: source.Chart, Revision: req.Revision})
	if err != nil {
		return nil, repoServerError("error getting OCI metadata", err)
	}
	return &pubapi.ApplicationOCIMetadata{
		CreatedAt: md.CreatedAt, Authors: md.Authors, ImageURL: md.ImageURL, DocsURL: md.DocsURL,
		SourceURL: md.SourceURL, Version: md.Version, Description: md.Description,
	}, nil
}

// syncWindows serves GET /applications/{name}/syncwindows (manual-sync evaluation now).
func (s *Service) syncWindows(ctx context.Context, req *pubapi.ApplicationRef) (*pubapi.ApplicationSyncWindowsResponse, error) {
	app, proj, err := s.getApp(ctx, rbac.ActionGet, appRequest{name: req.Name, namespace: req.AppNamespace, project: req.Project})
	if err != nil {
		return nil, err
	}
	state, err := project.GetApplicationSyncWindowsState(proj, app, s.now())
	if err != nil {
		return nil, rest.InvalidArgument("%s", err.Error())
	}
	out := &pubapi.ApplicationSyncWindowsResponse{
		ActiveWindows:   convertWindows(state.ActiveWindows),
		AssignedWindows: convertWindows(state.AssignedWindows),
		CanSync:         state.CanSync,
		Status:          string(state.Status),
	}
	if state.NextTransition != nil {
		nt := metav1.NewTime(*state.NextTransition)
		out.NextTransition = &nt
	}
	return out, nil
}

func convertWindows(ws v1alpha1.SyncWindows) []pubapi.ApplicationSyncWindow {
	if len(ws) == 0 {
		return nil
	}
	out := make([]pubapi.ApplicationSyncWindow, 0, len(ws))
	for _, w := range ws {
		out = append(out, pubapi.ApplicationSyncWindow{Kind: w.Kind, Schedule: w.Schedule, Duration: w.Duration, ManualSync: w.ManualSync, TimeZone: w.TimeZone})
	}
	return out
}
