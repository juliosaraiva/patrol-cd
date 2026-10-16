package reposerver

import (
	"context"
	"fmt"
	"os"

	"github.com/juliosaraiva/patrol-cd/internal/oci"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// GetRevisionChartDetails returns Helm chart metadata (SRC-129): description, home and
// maintainers ("Name <email>").
func (s *Service) GetRevisionChartDetails(ctx context.Context, req *apiclient.ChartDetailsRequest) (_ *apiclient.ChartDetails, err error) {
	defer s.finish(&err)
	if req == nil || req.Repo.URL == "" || req.Name == "" {
		return nil, errorf(apiclient.CodeInvalidArgument, "must pass a valid repo and chart name")
	}
	key := auxKey("chartdetails", repoCacheURL(req.Repo.URL, kindHelm), s.fp.of(req.Repo), req.Name, req.Revision)
	if d, ok := getAux[apiclient.ChartDetails](s.aux, key); ok {
		s.metrics.cache("chartdetails", true)
		return d, nil
	}
	s.metrics.cache("chartdetails", false)
	creds := s.helmCreds(req.Repo, false)
	enableOCI := helmEnableOCI(req.Repo)
	s.helm.InvalidateIndex(req.Repo.URL)
	version, err := s.helm.ResolveChartVersion(ctx, req.Repo.URL, req.Name, req.Revision, creds, enableOCI)
	if err != nil {
		return nil, fmt.Errorf("helm client error: %w", err)
	}
	dir, err := s.helm.PullChart(ctx, req.Repo.URL, req.Name, version, creds, enableOCI)
	if err != nil {
		return nil, fmt.Errorf("error extracting chart: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	md, err := s.helm.ShowChart(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("error inspecting chart: %w", err)
	}
	d := md.Details()
	out := &apiclient.ChartDetails{Description: d.Description, Home: d.Home, Maintainers: d.Maintainers}
	putAux(s.aux, key, out, s.cfg.RepoCacheExpiration)
	return out, nil
}

// GetOCIMetadata returns the annotation metadata of an OCI manifest (SRC-102).
func (s *Service) GetOCIMetadata(ctx context.Context, req *apiclient.ChartDetailsRequest) (_ *apiclient.OCIMetadata, err error) {
	defer s.finish(&err)
	if req == nil || req.Repo.URL == "" {
		return nil, errorf(apiclient.CodeInvalidArgument, "must pass a valid repo")
	}
	key := auxKey("ocimetadata", repoCacheURL(req.Repo.URL, kindOCI), s.fp.of(req.Repo), req.Revision)
	if m, ok := getAux[apiclient.OCIMetadata](s.aux, key); ok {
		s.metrics.cache("ocimetadata", true)
		return m, nil
	}
	s.metrics.cache("ocimetadata", false)
	c, err := s.ociClient(req.Repo)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize oci client: %w", err)
	}
	manifest, _, err := c.FetchManifest(ctx, req.Repo.URL, req.Revision)
	if err != nil {
		return nil, fmt.Errorf("failed to extract digest metadata for revision %q: %w", req.Revision, err)
	}
	md := oci.MetadataFromManifest(manifest)
	out := &apiclient.OCIMetadata{
		CreatedAt: md.CreatedAt, Authors: md.Authors, DocsURL: md.DocsURL, SourceURL: md.SourceURL,
		Version: md.Version, Description: md.Description,
	}
	putAux(s.aux, key, out, s.cfg.RepoCacheExpiration)
	return out, nil
}

// GetHelmCharts lists the charts of an HTTP(S) Helm repository (SRC-130), bypassing
// and refreshing the index cache. OCI registries have no index.
func (s *Service) GetHelmCharts(ctx context.Context, req *apiclient.HelmChartsRequest) (_ *apiclient.HelmChartsResponse, err error) {
	defer s.finish(&err)
	if req == nil || req.Repo.URL == "" {
		return nil, errorf(apiclient.CodeInvalidArgument, "must pass a valid repo")
	}
	if helmEnableOCI(req.Repo) {
		return nil, errorf(apiclient.CodeInvalidArgument, "listing charts is not supported for OCI Helm repositories")
	}
	charts, err := s.helm.ListCharts(ctx, req.Repo.URL, s.helmCreds(req.Repo, false))
	if err != nil {
		return nil, err
	}
	out := &apiclient.HelmChartsResponse{Items: make([]apiclient.HelmChart, 0, len(charts))}
	for _, c := range charts {
		out.Items = append(out.Items, apiclient.HelmChart{Name: c.Name, Versions: c.Versions})
	}
	return out, nil
}
