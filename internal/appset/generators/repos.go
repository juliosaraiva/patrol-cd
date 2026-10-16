package generators

import (
	"context"
	"errors"
	"fmt"

	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// RepoService is the repository access of the Git generator.
type RepoService interface {
	// GetFiles returns the content of the files matching pattern at revision.
	GetFiles(ctx context.Context, req RepoRequest, pattern string) (map[string][]byte, error)
	// GetDirectories returns every directory at revision (relative paths, repository root
	// excluded).
	GetDirectories(ctx context.Context, req RepoRequest) ([]string, error)
}

// RepoRequest identifies the repository content the Git generator reads.
type RepoRequest struct {
	RepoURL  string
	Revision string
	// Project scopes repository credential lookup; empty for templated projects (only global
	// credentials apply).
	Project string
	// NoRevisionCache bypasses the repo-server revision cache (refresh annotation set).
	NoRevisionCache bool
	// VerifyCommit requests commit signature verification (the project has source integrity
	// criteria for the repository).
	VerifyCommit bool
}

// RepositoryResolver resolves the access material (URL, credentials, transport options) of a
// repository for a project, from Repository / RepositoryCredential / SCMProvider objects.
type RepositoryResolver interface {
	ResolveRepository(ctx context.Context, repoURL, project string) (apiclient.Repository, error)
}

// RepositoryResolverFunc adapts a function to RepositoryResolver.
type RepositoryResolverFunc func(ctx context.Context, repoURL, project string) (apiclient.Repository, error)

// ResolveRepository implements RepositoryResolver.
func (f RepositoryResolverFunc) ResolveRepository(ctx context.Context, repoURL, project string) (apiclient.Repository, error) {
	return f(ctx, repoURL, project)
}

// AnonymousRepositories resolves every repository without credentials. It is the fallback until
// a credential resolver is wired; private repositories then fail at the repo-server.
var AnonymousRepositories RepositoryResolver = RepositoryResolverFunc(func(_ context.Context, repoURL, project string) (apiclient.Repository, error) {
	return apiclient.Repository{URL: repoURL, Project: project}, nil
})

// RepoServerService implements RepoService with the repo-server API.
type RepoServerService struct {
	// Client is the repo-server client.
	Client apiclient.Client
	// Repositories resolves repository credentials (nil: AnonymousRepositories).
	Repositories RepositoryResolver
	// SubmodulesEnabled checks out git submodules (applicationsetcontroller.enable.git.submodule).
	SubmodulesEnabled bool
	// NewGitFileGlobbing selects doublestar globbing for the files generator.
	NewGitFileGlobbing bool
}

func (s *RepoServerService) repository(ctx context.Context, req RepoRequest) (apiclient.Repository, error) {
	if s.Client == nil {
		return apiclient.Repository{}, errors.New("error initializing new repo server client: no repo-server client configured")
	}
	resolver := s.Repositories
	if resolver == nil {
		resolver = AnonymousRepositories
	}
	repo, err := resolver.ResolveRepository(ctx, req.RepoURL, req.Project)
	if err != nil {
		return apiclient.Repository{}, fmt.Errorf("error in GetRepository: %w", err)
	}
	return repo, nil
}

// GetFiles implements RepoService.
func (s *RepoServerService) GetFiles(ctx context.Context, req RepoRequest, pattern string) (map[string][]byte, error) {
	repo, err := s.repository(ctx, req)
	if err != nil {
		return nil, err
	}
	resp, err := s.Client.GetGitFiles(ctx, &apiclient.GitFilesRequest{
		Repo:                      repo,
		SubmoduleEnabled:          s.SubmodulesEnabled,
		Revision:                  req.Revision,
		Path:                      pattern,
		NewGitFileGlobbingEnabled: s.NewGitFileGlobbing,
		NoRevisionCache:           req.NoRevisionCache,
		VerifyCommit:              req.VerifyCommit,
	})
	if err != nil {
		return nil, fmt.Errorf("error retrieving Git files: %w", err)
	}
	if resp == nil {
		return map[string][]byte{}, nil
	}
	return resp.Map, nil
}

// GetDirectories implements RepoService.
func (s *RepoServerService) GetDirectories(ctx context.Context, req RepoRequest) ([]string, error) {
	repo, err := s.repository(ctx, req)
	if err != nil {
		return nil, err
	}
	resp, err := s.Client.GetGitDirectories(ctx, &apiclient.GitDirectoriesRequest{
		Repo:             repo,
		SubmoduleEnabled: s.SubmodulesEnabled,
		Revision:         req.Revision,
		NoRevisionCache:  req.NoRevisionCache,
		VerifyCommit:     req.VerifyCommit,
	})
	if err != nil {
		return nil, fmt.Errorf("error retrieving Git Directories: %w", err)
	}
	if resp == nil {
		return nil, nil
	}
	return resp.Paths, nil
}
