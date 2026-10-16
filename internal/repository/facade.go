package repository

import (
	"context"
	"errors"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// GetRepository resolves read access to repoURL for an Application of project (upstream
// db.GetRepository): the project-scoped Repository, else the unscoped one, else a stub,
// with credential templates applied when it has no credentials of its own. It implements
// the hydrator's RepositoryResolver.
func (r *Resolver) GetRepository(ctx context.Context, repoURL, project string) (*apiclient.Repository, error) {
	return r.ResolveForURL(ctx, repoURL, project, v1alpha1.RepositoryPurposeRead)
}

// GetWriteRepository resolves write access to repoURL for the source hydrator (upstream
// db.GetWriteRepository): only Repositories and RepositoryCredentials of purpose write are
// considered, so read credentials never grant push. When nothing matches, the result is a
// stub without credentials (the push is attempted anonymously), never nil. It implements the
// hydrator's WriteCredentialResolver.
func (r *Resolver) GetWriteRepository(ctx context.Context, repoURL, project string) (*apiclient.Repository, error) {
	res, err := r.Resolve(ctx, Query{URL: repoURL, Project: project, Purpose: v1alpha1.RepositoryPurposeWrite, Type: v1alpha1.RepositoryTypeGit})
	if err != nil {
		return nil, err
	}
	return res.Repository, nil
}

// ListHelmRepositories returns the Helm dependency repositories and Helm credential
// templates visible to proj (ManifestRequest.Repos and HelmRepoCreds for a non-OCI source).
// It implements the hydrator's HelmRepositoryLister. Use ReposForProject and
// HelmCredentialTemplates with includeOCI for OCI sources.
func (r *Resolver) ListHelmRepositories(ctx context.Context, proj *v1alpha1.AppProject) ([]apiclient.Repository, []apiclient.RepositoryCredentialTemplate, error) {
	return r.ListDependencyRepositories(ctx, proj, false)
}

// ListDependencyRepositories returns ReposForProject and HelmCredentialTemplates for proj.
// With includeOCI (an OCI source, which may be a Helm chart with OCI dependencies) OCI
// repositories and OCI credential templates are included, as upstream GetRepoObjs does.
func (r *Resolver) ListDependencyRepositories(ctx context.Context, proj *v1alpha1.AppProject, includeOCI bool) ([]apiclient.Repository, []apiclient.RepositoryCredentialTemplate, error) {
	if proj == nil {
		return nil, nil, errors.New("list dependency repositories: nil project")
	}
	repos, err := r.ReposForProject(ctx, proj, includeOCI)
	if err != nil {
		return nil, nil, err
	}
	creds, err := r.HelmCredentialTemplates(ctx, proj, includeOCI)
	if err != nil {
		return nil, nil, err
	}
	return repos, creds, nil
}
