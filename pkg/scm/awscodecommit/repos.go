package awscodecommit

import (
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"slices"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/codecommit"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	rgtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"golang.org/x/sync/errgroup"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

const (
	resourceTypeCodeCommitRepository = "codecommit:repository"
	prefixGitURLHTTPS                = "https://git-codecommit."
	prefixGitURLHTTPSFIPS            = "https://git-codecommit-fips."
)

// FIPSCloneURL rewrites a CodeCommit HTTPS clone URL to its FIPS endpoint
// ("https://git-codecommit." → "https://git-codecommit-fips."), as upstream. A FIPS URL
// is returned unchanged; other URLs fail. The region must support FIPS for the result
// to be reachable.
func FIPSCloneURL(repoURL string) (string, error) {
	if strings.HasPrefix(repoURL, prefixGitURLHTTPSFIPS) {
		return repoURL, nil
	}
	if !strings.HasPrefix(repoURL, prefixGitURLHTTPS) {
		return "", fmt.Errorf("the provided https endpoint isn't recognized, cannot be transformed to FIPS endpoint: %s", repoURL)
	}
	return prefixGitURLHTTPSFIPS + strings.TrimPrefix(repoURL, prefixGitURLHTTPS), nil
}

// tagFilters groups the configured filters by key (values of one key are ORed; a key
// without values matches any value), sorted by key for a deterministic request.
func tagFilters(filters []TagFilter) []rgtypes.TagFilter {
	byKey := map[string][]string{}
	var keys []string
	for _, f := range filters {
		if _, ok := byKey[f.Key]; !ok {
			keys = append(keys, f.Key)
			byKey[f.Key] = nil
		}
		if f.Value != "" && !slices.Contains(byKey[f.Key], f.Value) {
			byKey[f.Key] = append(byKey[f.Key], f.Value)
		}
	}
	sort.Strings(keys)
	out := make([]rgtypes.TagFilter, 0, len(keys))
	for _, k := range keys {
		out = append(out, rgtypes.TagFilter{Key: aws.String(k), Values: byKey[k]})
	}
	return out
}

// repoNames lists repository names: with tag filters through the Resource Groups
// Tagging API (ARNs that cannot be parsed are skipped with a warning), otherwise with
// CodeCommit ListRepositories; both follow pagination tokens with the page cap.
func (p *Provider) repoNames(ctx context.Context, op string) ([]string, error) {
	if len(p.tagFilters) == 0 {
		names, err := scm.CollectPages(ctx, p.maxPages, func(ctx context.Context, token string) ([]string, string, error) {
			in := &codecommit.ListRepositoriesInput{}
			if token != "" {
				in.NextToken = aws.String(token)
			}
			out, err := p.cc.ListRepositories(ctx, in)
			if err != nil {
				return nil, "", mapError(ctx, op, err)
			}
			var names []string
			for _, r := range out.Repositories {
				names = append(names, aws.ToString(r.RepositoryName))
			}
			return names, aws.ToString(out.NextToken), nil
		})
		return names, mapError(ctx, op, err)
	}
	filters := tagFilters(p.tagFilters)
	names, err := scm.CollectPages(ctx, p.maxPages, func(ctx context.Context, token string) ([]string, string, error) {
		in := &resourcegroupstaggingapi.GetResourcesInput{
			ResourceTypeFilters: []string{resourceTypeCodeCommitRepository},
			TagFilters:          filters,
		}
		if token != "" {
			in.PaginationToken = aws.String(token)
		}
		out, err := p.tagging.GetResources(ctx, in)
		if err != nil {
			return nil, "", mapError(ctx, op, err)
		}
		var names []string
		for _, m := range out.ResourceTagMappingList {
			a, err := arn.Parse(aws.ToString(m.ResourceARN))
			if err != nil || a.Resource == "" {
				p.logger.Warn("skipping CodeCommit repository with an unparsable ARN", "provider", Type, "arn", aws.ToString(m.ResourceARN))
				continue
			}
			names = append(names, a.Resource)
		}
		return names, aws.ToString(out.PaginationToken), nil
	})
	return names, mapError(ctx, op, err)
}

// ListRepositories implements scm.RepositoryLister. Repositories are discovered with
// ListRepositories, or with the Resource Groups Tagging API when tag filters are set,
// then read with GetRepository (a failure fails the listing, as upstream: an AWS outage
// must not remove Applications). Repositories without a default branch (uninitialized)
// are skipped.
//
// Organization is the AWS account ID; RepositoryID the repository ID; Labels are
// empty. RepoQuery.Organization is ignored (the account is the credentials' account);
// Topics match nothing (CodeCommit tags are selected with Options.TagFilters). Clone
// protocols: ssh (default), https, and CloneProtocolHTTPSFIPS.
//
// Unlike upstream, which leaves `sha` empty, the head commit of every returned branch is
// read with GetBranch (one call per repository for the default branch, one per branch
// with AllBranches, bounded parallelism).
func (p *Provider) ListRepositories(ctx context.Context, q scm.RepoQuery) ([]scm.Repository, error) {
	const op = "ListRepositories"
	switch q.CloneProtocol {
	case scm.CloneProtocolDefault, scm.CloneProtocolSSH, scm.CloneProtocolHTTPS, CloneProtocolHTTPSFIPS:
	default:
		return nil, invalidErr(op, fmt.Errorf("unknown clone protocol for codecommit %v", q.CloneProtocol))
	}
	names, err := p.repoNames(ctx, op)
	if err != nil {
		return nil, fmt.Errorf("failed to list codecommit repository: %w", err)
	}
	cands := make([]*scm.Repository, len(names))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for i, name := range names {
		g.Go(func() error {
			if scm.ValidateSegment("repository", name) != nil {
				p.logger.Warn("skipping CodeCommit repository with an unexpected name", "provider", Type, "repository", name)
				return nil
			}
			out, err := p.cc.GetRepository(gctx, &codecommit.GetRepositoryInput{RepositoryName: aws.String(name)})
			if err != nil {
				return fmt.Errorf("failed to get codecommit repository: %w", mapError(gctx, op, err))
			}
			m := out.RepositoryMetadata
			if m == nil {
				p.logger.Warn("codecommit returned an invalid response for repository, skipped", "provider", Type, "repository", name)
				return nil
			}
			if aws.ToString(m.DefaultBranch) == "" {
				p.logger.Warn("repository does not have a default branch, skipped", "provider", Type, "repository", name)
				return nil
			}
			u := aws.ToString(m.CloneUrlSsh)
			switch q.CloneProtocol {
			case scm.CloneProtocolHTTPS:
				u = aws.ToString(m.CloneUrlHttp)
			case CloneProtocolHTTPSFIPS:
				if u, err = FIPSCloneURL(aws.ToString(m.CloneUrlHttp)); err != nil {
					return invalidErr(op, fmt.Errorf("https-fips is provided but repoUrl can't be transformed to FIPS endpoint: %w", err))
				}
			}
			account, repoName := aws.ToString(m.AccountId), aws.ToString(m.RepositoryName)
			cands[i] = &scm.Repository{
				Organization:  account,
				Name:          repoName,
				FullName:      account + "/" + repoName,
				URL:           u,
				HTTPSURL:      aws.ToString(m.CloneUrlHttp),
				SSHURL:        aws.ToString(m.CloneUrlSsh),
				Branch:        aws.ToString(m.DefaultBranch),
				DefaultBranch: aws.ToString(m.DefaultBranch),
				Labels:        []string{},
				RepositoryID:  aws.ToString(m.RepositoryId),
				Private:       true,
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	var kept []scm.Repository
	for _, c := range cands {
		if c == nil || len(q.Topics) > 0 {
			continue
		}
		if q.Match != nil && !q.Match(*c) {
			continue
		}
		kept = append(kept, *c)
	}
	results := make([][]scm.Repository, len(kept))
	g, gctx = errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for i, r := range kept {
		g.Go(func() error {
			var branches []scm.Branch
			var err error
			if q.AllBranches {
				branches, err = p.allBranches(gctx, op, r.Name)
			} else {
				branches, err = p.branch(gctx, op, r.Name, r.DefaultBranch)
			}
			if err != nil {
				return fmt.Errorf("error listing branches for %s: %w", r.FullName, err)
			}
			for _, b := range branches {
				e := r
				e.Branch, e.SHA = b.Name, b.SHA
				results[i] = append(results[i], e)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := []scm.Repository{}
	for _, r := range results {
		out = append(out, r...)
	}
	return out, nil
}

// branch returns the named branch with its head commit, or no branch when it does not
// exist.
func (p *Provider) branch(ctx context.Context, op, repo, name string) ([]scm.Branch, error) {
	out, err := p.cc.GetBranch(ctx, &codecommit.GetBranchInput{RepositoryName: aws.String(repo), BranchName: aws.String(name)})
	if err != nil {
		mapped := mapError(ctx, op, err)
		var api interface{ ErrorCode() string }
		if errors.As(err, &api) && api.ErrorCode() == "BranchDoesNotExistException" {
			return []scm.Branch{}, nil
		}
		return nil, mapped
	}
	if out.Branch == nil {
		return []scm.Branch{}, nil
	}
	return []scm.Branch{{Name: aws.ToString(out.Branch.BranchName), SHA: aws.ToString(out.Branch.CommitId)}}, nil
}

// allBranches lists every branch (ListBranches pagination) and reads each head commit
// with bounded parallelism, keeping the API order.
func (p *Provider) allBranches(ctx context.Context, op, repo string) ([]scm.Branch, error) {
	names, err := scm.CollectPages(ctx, p.maxPages, func(ctx context.Context, token string) ([]string, string, error) {
		in := &codecommit.ListBranchesInput{RepositoryName: aws.String(repo)}
		if token != "" {
			in.NextToken = aws.String(token)
		}
		out, err := p.cc.ListBranches(ctx, in)
		if err != nil {
			return nil, "", mapError(ctx, op, err)
		}
		return out.Branches, aws.ToString(out.NextToken), nil
	})
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	heads := make([][]scm.Branch, len(names))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for i, name := range names {
		g.Go(func() error {
			b, err := p.branch(gctx, op, repo, name)
			heads[i] = b
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := make([]scm.Branch, 0, len(names))
	for _, b := range heads {
		out = append(out, b...)
	}
	return out, nil
}

func validateRef(op string, ref scm.RepoRef) error {
	if err := scm.ValidateSegment("repository", ref.Name); err != nil {
		return invalidErr(op, err)
	}
	if ref.Owner != "" {
		if err := scm.ValidateSegment("owner", ref.Owner); err != nil {
			return invalidErr(op, err)
		}
	}
	return nil
}

// ListBranches implements scm.BranchLister. A missing repository fails with
// KindNotFound.
func (p *Provider) ListBranches(ctx context.Context, ref scm.RepoRef, all bool) ([]scm.Branch, error) {
	const op = "ListBranches"
	if err := validateRef(op, ref); err != nil {
		return nil, err
	}
	if all {
		return p.allBranches(ctx, op, ref.Name)
	}
	out, err := p.cc.GetRepository(ctx, &codecommit.GetRepositoryInput{RepositoryName: aws.String(ref.Name)})
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	if out.RepositoryMetadata == nil || aws.ToString(out.RepositoryMetadata.DefaultBranch) == "" {
		return []scm.Branch{}, nil
	}
	return p.branch(ctx, op, ref.Name, aws.ToString(out.RepositoryMetadata.DefaultBranch))
}

// PathExists implements scm.PathChecker like upstream: GetFolder is called on the
// parent folder (it does not download blobs, unlike GetFile) and the base name is
// matched against files, folders, submodules and symbolic links. The root always
// exists. A missing repository, revision or folder yields false. An empty revision
// selects the default branch.
func (p *Provider) PathExists(ctx context.Context, ref scm.RepoRef, revision, path string) (bool, error) {
	const op = "PathExists"
	if err := validateRef(op, ref); err != nil {
		return false, err
	}
	for seg := range strings.SplitSeq(path, "/") {
		if seg == ".." {
			return false, invalidErr(op, fmt.Errorf("path %q must not contain '..' segments", path))
		}
	}
	for seg := range strings.SplitSeq(revision, "/") {
		if seg == ".." {
			return false, invalidErr(op, fmt.Errorf("revision %q must not contain '..' segments", revision))
		}
	}
	abs := pathpkg.Clean("/" + path)
	if abs == "/" {
		return true, nil
	}
	in := &codecommit.GetFolderInput{RepositoryName: aws.String(ref.Name), FolderPath: aws.String(pathpkg.Dir(abs))}
	if revision != "" {
		in.CommitSpecifier = aws.String(revision)
	}
	out, err := p.cc.GetFolder(ctx, in)
	if err != nil {
		mapped := mapError(ctx, op, err)
		if scm.IsNotFound(mapped) {
			return false, nil
		}
		return false, mapped
	}
	base := pathpkg.Base(abs)
	for _, m := range out.SubModules {
		if aws.ToString(m.RelativePath) == base {
			return true, nil
		}
	}
	for _, f := range out.SubFolders {
		if aws.ToString(f.RelativePath) == base {
			return true, nil
		}
	}
	for _, f := range out.Files {
		if aws.ToString(f.RelativePath) == base {
			return true, nil
		}
	}
	for _, l := range out.SymbolicLinks {
		if aws.ToString(l.RelativePath) == base {
			return true, nil
		}
	}
	return false, nil
}
