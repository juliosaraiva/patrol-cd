package reposerver

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// RefreshPaths returns the refresh paths of a source from the manifest-generate-paths
// annotation value (SRC-135): ';'-separated entries, whitespace trimmed, empty entries
// skipped; "/x" is repository-root relative, other entries are joined to sourcePath.
// An empty annotation yields no paths (always refresh).
func RefreshPaths(annotation, sourcePath string) []string {
	var out []string
	for item := range strings.SplitSeq(annotation, ";") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.HasPrefix(item, "/") {
			out = append(out, item[1:])
			continue
		}
		out = append(out, filepath.Clean(filepath.Join(sourcePath, item)))
	}
	return out
}

// AppFilesHaveChanged reports whether any changed file is relevant to the refresh paths
// (upstream AppFilesHaveChanged): equal to a refresh path, below it, or matching it as a
// filepath.Match glob. No refresh paths or no changed files mean "changed".
func AppFilesHaveChanged(refreshPaths, changedFiles []string) bool {
	if len(changedFiles) == 0 || len(refreshPaths) == 0 {
		return true
	}
	for _, f := range changedFiles {
		f = ensureAbsPath(f)
		for _, item := range refreshPaths {
			item = ensureAbsPath(item)
			if f == item {
				return true
			}
			if item == "/" || strings.HasPrefix(f, strings.TrimSuffix(item, "/")+"/") {
				return true
			}
			if ok, err := filepath.Match(item, f); err == nil && ok {
				return true
			}
		}
	}
	return false
}

func ensureAbsPath(p string) string {
	p = filepath.Clean("/" + p)
	return p
}

// UpdateRevisionForPaths reports whether files under the refresh paths changed between
// the synced and the new revision (SRC-134). When nothing relevant changed but the
// revisions advanced, every cached rendering of the source at the synced revision is
// copied to the new revision so the next GenerateManifest is a cache hit.
func (s *Service) UpdateRevisionForPaths(ctx context.Context, req *apiclient.UpdateRevisionForPathsRequest) (_ *apiclient.UpdateRevisionForPathsResponse, err error) {
	defer s.finish(&err)
	if req == nil || req.Repo.URL == "" {
		return nil, errorf(apiclient.CodeInvalidArgument, "must pass a valid repo")
	}
	if len(req.Paths) == 0 {
		return &apiclient.UpdateRevisionForPathsResponse{Changes: true}, nil
	}
	src := req.Source
	if src.RepoURL == "" {
		src.RepoURL = req.Repo.URL
	}
	kind := kindOf(req.Repo, src)
	isGit := req.Repo.Type == v1alpha1.RepositoryTypeGit
	if req.Repo.Type == "" {
		isGit = kind == kindGit
	}
	if !isGit && len(req.RefSources) == 0 {
		return &apiclient.UpdateRevisionForPathsResponse{}, nil
	}
	readCache := !req.NoRevisionCache
	newRev, syncedRev := req.Revision, req.SyncedRevision
	advanced := false
	if isGit && req.SyncedRevision != req.Revision {
		resolved, synced, changed, err := s.gitSourceHasChanges(ctx, req.Repo, req.Revision, req.SyncedRevision, req.Paths, readCache)
		if err != nil {
			return nil, err
		}
		newRev, syncedRev = resolved, synced
		advanced = resolved != synced
		if changed {
			return &apiclient.UpdateRevisionForPathsResponse{Revision: newRev, Changes: true}, nil
		}
	}

	oldRefs, newRefs := map[string]string{}, map[string]string{}
	if req.HasMultipleSources {
		seen := map[string]bool{}
		for _, cand := range refCandidates(src.Helm) {
			refName := strings.SplitN(cand, "/", 2)[0]
			if seen[refName] {
				continue
			}
			seen[refName] = true
			synced, ok := lookupRef(req.SyncedRefSources, refName)
			if !ok {
				return &apiclient.UpdateRevisionForPathsResponse{Changes: true, Revision: newRev},
					errorf(apiclient.CodeInvalidArgument, "source referenced %q, but no source has a 'ref' field defined", refName)
			}
			current, _ := lookupRef(req.RefSources, refName)
			urlKey := repoCacheURL(synced.Repo.URL, kindGit)
			if synced.TargetRevision == current.TargetRevision {
				continue
			}
			resolved, old, changed, err := s.gitSourceHasChanges(ctx, synced.Repo, current.TargetRevision, synced.TargetRevision, req.Paths, readCache)
			if err != nil {
				return nil, err
			}
			if changed {
				return &apiclient.UpdateRevisionForPathsResponse{Revision: newRev, Changes: true}, nil
			}
			if resolved != old {
				advanced = true
			}
			oldRefs[urlKey], newRefs[urlKey] = old, resolved
		}
	}
	if !advanced {
		return &apiclient.UpdateRevisionForPathsResponse{Revision: newRev}, nil
	}
	identity := digestJSON(identityOf(src.RepoURL, kind, req.AppName, src, req.HasMultipleSources))
	moved := s.manifests.moveRevision(identity, isGit, syncedRev, newRev, oldRefs, newRefs)
	if moved == 0 {
		s.log.Info("reposerver: manifest cache miss while moving manifests to the new revision",
			slog.String("application", req.AppName), slog.String("revision", newRev))
	}
	return &apiclient.UpdateRevisionForPathsResponse{Revision: newRev}, nil
}

// gitFilesChanges is the cached changed-file list between two commits.
type gitFilesChanges struct {
	Files []string `json:"files"`
}

// gitSourceHasChanges resolves both revisions and compares the changed files with the
// refresh paths (no submodules; the synced commit is fetched when missing).
func (s *Service) gitSourceHasChanges(ctx context.Context, repo apiclient.Repository, revision, syncedRevision string, refreshPaths []string, readCache bool) (string, string, bool, error) {
	newSHA, err := s.resolveGit(ctx, repo, revision, "", readCache)
	if err != nil {
		return revision, syncedRevision, true, withCode(ErrorCode(err), fmt.Errorf("unable to resolve git revision %s: %w", revision, err))
	}
	oldSHA, err := s.resolveGit(ctx, repo, syncedRevision, "", readCache)
	if err != nil {
		return newSHA, syncedRevision, true, withCode(ErrorCode(err), fmt.Errorf("unable to resolve git revision %s: %w", syncedRevision, err))
	}
	if newSHA == oldSHA {
		return newSHA, oldSHA, false, nil
	}
	key := auxKey("gitFilesChanges", repoCacheURL(repo.URL, kindGit), s.fp.of(repo), newSHA, oldSHA)
	var files []string
	if c, ok := getAux[gitFilesChanges](s.aux, key); ok {
		s.metrics.cache("gitfileschanges", true)
		files = c.Files
	} else {
		s.metrics.cache("gitfileschanges", false)
		set, err := s.holdAll(ctx, []holdSpec{{repo: repo, revision: newSHA, readRefCache: readCache, submodules: false}})
		if err != nil {
			return newSHA, oldSHA, true, fmt.Errorf("unable to checkout git repo %s with revision %s: %w", git.SanitizeURL(repo.URL), newSHA, err)
		}
		defer set.release()
		gc := set.holds[0].gc
		if err := s.ensureFetched(ctx, gc, repo.Depth, oldSHA); err != nil {
			return newSHA, oldSHA, true, fmt.Errorf("unable to fetch git repo %s with syncedRevisions %s: %w", git.SanitizeURL(repo.URL), oldSHA, err)
		}
		files, err = gc.ChangedFiles(ctx, oldSHA, newSHA)
		if err != nil {
			return newSHA, oldSHA, true, fmt.Errorf("unable to get changed files for repo %s with revision %s: %w", git.SanitizeURL(repo.URL), newSHA, err)
		}
		putAux(s.aux, key, gitFilesChanges{Files: files}, s.cfg.RepoCacheExpiration)
	}
	changed := false
	if len(files) != 0 {
		changed = AppFilesHaveChanged(refreshPaths, files)
	}
	return newSHA, oldSHA, changed, nil
}
