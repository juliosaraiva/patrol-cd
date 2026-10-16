package clitest

import (
	"context"
	"io"
	"strings"
	"sync"

	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// RepoServerClient is the repo-server client the API server uses.
type RepoServerClient = apiclient.Client

// FakeRepoServer is an in-process repo-server: connection tests succeed unless the URL
// contains "unreachable" (or Fail says otherwise), refs, apps and charts come from its
// fields. Every TestRepository call is recorded. Methods the CLI core commands never
// reach return a not-implemented repo-server error.
type FakeRepoServer struct {
	mu sync.Mutex
	// Fail, when set, decides the TestRepository outcome.
	Fail func(apiclient.Repository) error
	// Refs, Apps, Charts and Plugins are the canned answers.
	Refs    apiclient.Refs
	Apps    map[string]string
	Charts  []apiclient.HelmChart
	Plugins []apiclient.PluginInfo

	tested []apiclient.Repository
}

var _ apiclient.Client = (*FakeRepoServer)(nil)

func notImplemented(op string) error {
	return &apiclient.Error{Code: apiclient.CodeInternal, Message: "fake repo-server: " + op + " not implemented"}
}

// Tested returns the repositories passed to TestRepository.
func (f *FakeRepoServer) Tested() []apiclient.Repository {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]apiclient.Repository(nil), f.tested...)
}

// TestRepository implements apiclient.Client.
func (f *FakeRepoServer) TestRepository(_ context.Context, req *apiclient.TestRepositoryRequest) (*apiclient.TestRepositoryResponse, error) {
	f.mu.Lock()
	f.tested = append(f.tested, req.Repo)
	fail := f.Fail
	f.mu.Unlock()
	if fail != nil {
		if err := fail(req.Repo); err != nil {
			return nil, err
		}
	} else if strings.Contains(req.Repo.URL, "unreachable") {
		return nil, &apiclient.Error{Code: apiclient.CodeUnavailable, Message: "repository not accessible: dial tcp: connection refused"}
	}
	return &apiclient.TestRepositoryResponse{VerifiedRepository: true}, nil
}

// ListRefs implements apiclient.Client.
func (f *FakeRepoServer) ListRefs(context.Context, *apiclient.ListRefsRequest) (*apiclient.Refs, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.Refs
	return &r, nil
}

// ListOCITags implements apiclient.Client.
func (f *FakeRepoServer) ListOCITags(context.Context, *apiclient.ListRefsRequest) (*apiclient.Refs, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &apiclient.Refs{Tags: append([]string(nil), f.Refs.Tags...)}, nil
}

// ListApps implements apiclient.Client.
func (f *FakeRepoServer) ListApps(context.Context, *apiclient.ListAppsRequest) (*apiclient.AppList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	apps := map[string]string{}
	for k, v := range f.Apps {
		apps[k] = v
	}
	return &apiclient.AppList{Apps: apps}, nil
}

// ListPlugins implements apiclient.Client.
func (f *FakeRepoServer) ListPlugins(context.Context) (*apiclient.PluginList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &apiclient.PluginList{Items: append([]apiclient.PluginInfo(nil), f.Plugins...)}, nil
}

// GetHelmCharts implements apiclient.Client.
func (f *FakeRepoServer) GetHelmCharts(context.Context, *apiclient.HelmChartsRequest) (*apiclient.HelmChartsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &apiclient.HelmChartsResponse{Items: append([]apiclient.HelmChart(nil), f.Charts...)}, nil
}

// GenerateManifest implements apiclient.Client.
func (f *FakeRepoServer) GenerateManifest(context.Context, *apiclient.ManifestRequest) (*apiclient.ManifestResponse, error) {
	return nil, notImplemented("GenerateManifest")
}

// GenerateManifestWithFiles implements apiclient.Client.
func (f *FakeRepoServer) GenerateManifestWithFiles(_ context.Context, _ *apiclient.ManifestRequest, files io.Reader) (*apiclient.ManifestResponse, error) {
	if c, ok := files.(io.Closer); ok {
		_ = c.Close()
	}
	return nil, notImplemented("GenerateManifestWithFiles")
}

// ResolveRevision implements apiclient.Client.
func (f *FakeRepoServer) ResolveRevision(context.Context, *apiclient.ResolveRevisionRequest) (*apiclient.ResolveRevisionResponse, error) {
	return nil, notImplemented("ResolveRevision")
}

// GetAppDetails implements apiclient.Client.
func (f *FakeRepoServer) GetAppDetails(context.Context, *apiclient.AppDetailsQuery) (*apiclient.AppDetails, error) {
	return nil, notImplemented("GetAppDetails")
}

// GetRevisionMetadata implements apiclient.Client.
func (f *FakeRepoServer) GetRevisionMetadata(context.Context, *apiclient.RevisionMetadataRequest) (*apiclient.RevisionMetadata, error) {
	return nil, notImplemented("GetRevisionMetadata")
}

// GetRevisionChartDetails implements apiclient.Client.
func (f *FakeRepoServer) GetRevisionChartDetails(context.Context, *apiclient.ChartDetailsRequest) (*apiclient.ChartDetails, error) {
	return nil, notImplemented("GetRevisionChartDetails")
}

// GetOCIMetadata implements apiclient.Client.
func (f *FakeRepoServer) GetOCIMetadata(context.Context, *apiclient.ChartDetailsRequest) (*apiclient.OCIMetadata, error) {
	return nil, notImplemented("GetOCIMetadata")
}

// GetGitFiles implements apiclient.Client.
func (f *FakeRepoServer) GetGitFiles(context.Context, *apiclient.GitFilesRequest) (*apiclient.GitFilesResponse, error) {
	return nil, notImplemented("GetGitFiles")
}

// GetGitDirectories implements apiclient.Client.
func (f *FakeRepoServer) GetGitDirectories(context.Context, *apiclient.GitDirectoriesRequest) (*apiclient.GitDirectoriesResponse, error) {
	return nil, notImplemented("GetGitDirectories")
}

// UpdateRevisionForPaths implements apiclient.Client.
func (f *FakeRepoServer) UpdateRevisionForPaths(context.Context, *apiclient.UpdateRevisionForPathsRequest) (*apiclient.UpdateRevisionForPathsResponse, error) {
	return nil, notImplemented("UpdateRevisionForPaths")
}
