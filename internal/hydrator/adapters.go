package hydrator

import (
	"context"
	"fmt"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// RepositoryAccessResolver is the subset of *repository.Resolver the hydrator uses.
type RepositoryAccessResolver interface {
	ResolveForURL(ctx context.Context, repoURL, project string, purpose v1alpha1.RepositoryPurpose) (*apiclient.Repository, error)
	ReposForProject(ctx context.Context, proj *v1alpha1.AppProject, includeOCI bool) ([]apiclient.Repository, error)
	HelmCredentialTemplates(ctx context.Context, proj *v1alpha1.AppProject, includeOCI bool) ([]apiclient.RepositoryCredentialTemplate, error)
}

// Repositories adapts a *repository.Resolver to RepositoryResolver (read purpose),
// WriteCredentialResolver (write purpose: Repository objects and credential templates with
// purpose write, labeled repository-write / repo-write-creds Secrets) and
// HelmRepositoryLister (Helm and OCI dependency repositories permitted by the project).
type Repositories struct {
	Resolver RepositoryAccessResolver
}

var (
	_ RepositoryResolver      = Repositories{}
	_ WriteCredentialResolver = Repositories{}
	_ HelmRepositoryLister    = Repositories{}
)

// GetRepository implements RepositoryResolver.
func (r Repositories) GetRepository(ctx context.Context, repoURL, project string) (*apiclient.Repository, error) {
	return r.Resolver.ResolveForURL(ctx, repoURL, project, v1alpha1.RepositoryPurposeRead)
}

// GetWriteRepository implements WriteCredentialResolver. Without a matching write
// Repository or credential template the result has no credentials (anonymous push).
func (r Repositories) GetWriteRepository(ctx context.Context, repoURL, project string) (*apiclient.Repository, error) {
	return r.Resolver.ResolveForURL(ctx, repoURL, project, v1alpha1.RepositoryPurposeWrite)
}

// ListHelmRepositories implements HelmRepositoryLister.
func (r Repositories) ListHelmRepositories(ctx context.Context, proj *v1alpha1.AppProject) ([]apiclient.Repository, []apiclient.RepositoryCredentialTemplate, error) {
	repos, err := r.Resolver.ReposForProject(ctx, proj, true)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list Helm repositories: %w", err)
	}
	creds, err := r.Resolver.HelmCredentialTemplates(ctx, proj, true)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list Helm credential templates: %w", err)
	}
	return repos, creds, nil
}
