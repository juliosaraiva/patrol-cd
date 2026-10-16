package reposerver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/oci"
	"github.com/juliosaraiva/patrol-cd/internal/render"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

// cachedManifestGenerationPrefix prefixes cached generation errors (upstream text).
const cachedManifestGenerationPrefix = "Manifest generation error (cached)"

// streamedRevision is the revision reported for uploaded sources.
const streamedRevision = "streamed"

var refKeyPattern = regexp.MustCompile(`^\$?[a-zA-Z0-9_-]+$`)

// GenerateManifest renders one application source (SRC-094..SRC-121). The revision is
// resolved first (git commit SHA, Helm chart version or OCI digest), then referenced
// `$ref` sources; the manifest cache is consulted before and (for git) after the
// checkout; generation runs under the parallelism limit.
func (s *Service) GenerateManifest(ctx context.Context, req *apiclient.ManifestRequest) (_ *apiclient.ManifestResponse, err error) {
	defer s.finish(&err)
	if req == nil || req.Repo.URL == "" {
		return nil, errorf(apiclient.CodeInvalidArgument, "must pass a valid repo")
	}
	src := req.Source
	if src.RepoURL == "" {
		src.RepoURL = req.Repo.URL
	}
	kind := kindOf(req.Repo, src)
	noRevCache := req.NoRevisionCache || req.NoCache
	unresolved := firstNonEmpty(req.Revision, src.TargetRevision)

	// Ref-only sources of multi-source applications only resolve their revision.
	if req.HasMultipleSources && src.Path == "" && kind == kindGit && src.Ref != "" {
		sha, err := s.resolveGit(ctx, req.Repo, unresolved, src.TagPrefix, !noRevCache)
		if err != nil {
			return nil, err
		}
		return &apiclient.ManifestResponse{Revision: sha}, nil
	}

	revision, err := s.resolveSource(ctx, req.Repo, src, kind, unresolved, noRevCache)
	if err != nil {
		return nil, err
	}
	refs, err := s.resolveRefs(ctx, req.HasMultipleSources, src.Helm, req.RefSources, kind, src.RepoURL, unresolved, revision, noRevCache)
	if err != nil {
		return nil, err
	}
	g := &generation{
		s: s, req: req, src: src, kind: kind, unresolved: unresolved, revision: revision, refs: refs,
	}
	spec := s.renderSpecOf(req, src, kind)
	if spec.Keyring != "" {
		// Verification depends on the requested revision (an annotated tag is verified
		// instead of its commit).
		spec.IntegrityRevision = unresolved
	}
	g.specDigest = digestJSON(spec)
	g.identity = digestJSON(identityOf(src.RepoURL, kind, req.AppName, src, req.HasMultipleSources))
	g.refRevs = refRevisions(refs)
	g.key = manifestKey(g.specDigest, revision, g.refRevs)

	if !req.NoCache {
		if resp, err, ok := s.cachedManifest(g.key, revision, true); ok {
			return resp, err
		}
	}
	if s.sem != nil {
		start := time.Now()
		err := s.sem.Acquire(ctx, 1)
		s.metrics.parallelismWait.Observe(time.Since(start).Seconds())
		if err != nil {
			return nil, err
		}
		defer s.sem.Release(1)
	}
	resp, err := g.run(ctx)
	if errors.Is(err, errCachedResult) {
		return g.cached.resp, g.cached.err
	}
	s.recordGeneration(g, resp, err)
	if err != nil {
		return nil, err
	}
	return cloneManifestResponse(resp), nil
}

// errCachedResult reports that the post-checkout cache check found the result.
var errCachedResult = errors.New("cached result")

// generation is one GenerateManifest cache miss.
type generation struct {
	s          *Service
	req        *apiclient.ManifestRequest
	src        v1alpha1.ApplicationSource
	kind       sourceKind
	unresolved string
	revision   string
	refs       []resolvedRef
	specDigest string
	identity   string
	refRevs    map[string]string
	key        string
	cached     struct {
		resp *apiclient.ManifestResponse
		err  error
	}
}

// run obtains the content and renders it.
func (g *generation) run(ctx context.Context) (*apiclient.ManifestResponse, error) {
	s := g.s
	if g.kind == kindGit {
		return g.runGit(ctx)
	}
	// Helm chart and OCI sources: referenced git sources are checked out first.
	set, err := s.holdRefs(ctx, g.refs, g.req.NoCache)
	if err != nil {
		return nil, err
	}
	defer set.release()
	if g.kind == kindOCI {
		dir, cleanup, err := s.pullOCI(ctx, g.req.Repo, g.src.RepoURL, g.revision, g.req.NoCache)
		if err != nil {
			return nil, asContentError(err)
		}
		defer cleanup()
		return g.render(ctx, dir, set)
	}
	if g.req.NoCache {
		pass := g.src.Helm != nil && g.src.Helm.PassCredentials
		if err := s.helm.CleanChartCache(g.req.Repo.URL, g.src.Chart, g.revision, s.helmCreds(g.req.Repo, pass), helmEnableOCI(g.req.Repo)); err != nil {
			s.log.Warn("reposerver: cleaning chart cache failed", slog.Any("error", err))
		}
	}
	return g.render(ctx, "", set)
}

// runGit checks out the main repository and the referenced repositories (locks taken
// in path order), re-checks the cache, verifies source integrity and renders.
func (g *generation) runGit(ctx context.Context) (*apiclient.ManifestResponse, error) {
	s := g.s
	main := holdSpec{repo: g.req.Repo, revision: g.revision, tagPrefix: g.src.TagPrefix, readRefCache: !g.req.NoCache && !g.req.NoRevisionCache, submodules: !s.cfg.DisableSubmodules}
	set, err := s.holdAll(ctx, append([]holdSpec{main}, refHoldSpecs(g.refs, !s.cfg.DisableSubmodules)...))
	if err != nil {
		if holdFailed(err, 0) {
			// The main checkout failed (a `$ref` sharing its working copy included).
			return nil, asContentError(err)
		}
		return nil, err
	}
	defer set.release()
	mainHold := set.holds[0]
	if err := s.checkSymlinks(ctx, mainHold.root, g.revision, g.req.NoCache, "repository", ".git"); err != nil {
		return nil, asContentError(err)
	}
	for i := range g.refs {
		if err := s.checkSymlinks(ctx, set.holds[i+1].root, g.refs[i].sha, g.req.NoCache, "repository", ".git"); err != nil {
			return nil, err
		}
	}
	if !g.req.NoCache {
		if resp, err, ok := s.cachedManifest(g.key, g.revision, false); ok {
			g.cached.resp, g.cached.err = resp, err
			return nil, errCachedResult
		}
	}
	return g.render(ctx, mainHold.root, set)
}

// render renders the source from root (ignored for Helm chart sources) with the
// referenced repositories of set, and builds the response.
func (g *generation) render(ctx context.Context, root string, set *holdSet) (*apiclient.ManifestResponse, error) {
	s := g.s
	req := g.req
	resp := &apiclient.ManifestResponse{Revision: g.revision}

	// Source integrity (git sources and git ref sources), before rendering.
	if g.kind == kindGit {
		mainRepo := req.Repo
		mainRepo.URL = g.src.RepoURL
		res, legacy, err := s.verifyIntegrity(ctx, set.holds[0].gc, req.SourceIntegrity, mainRepo, g.unresolved, g.revision)
		if err != nil {
			return nil, err
		}
		resp.SourceIntegrityResult, resp.VerifyResult = res, legacy
	}
	if set != nil && req.SourceIntegrity != nil {
		offset := 0
		if g.kind == kindGit {
			offset = 1
		}
		for i, ref := range g.refs {
			h := set.holds[i+offset]
			res, _, err := s.verifyIntegrity(ctx, h.gc, req.SourceIntegrity, ref.target.Repo, ref.target.TargetRevision, ref.sha)
			if err != nil {
				return nil, err
			}
			resp.SourceIntegrityResult = mergeIntegrity(resp.SourceIntegrityResult, res, ref.key)
		}
	}

	// Sources of multi-source applications without path and chart render nothing.
	if req.HasMultipleSources && g.src.Path == "" && g.src.Chart == "" && g.kind == kindGit {
		return resp, nil
	}

	if g.kind != kindHelm {
		if _, err := appDir(root, firstNonEmpty(g.src.Path, ".")); err != nil {
			if g.kind == kindOCI {
				// Upstream resolves the OCI application path before generation.
				return nil, asContentError(err)
			}
			return nil, err
		}
	}
	in := render.Input{
		RootDir:               root,
		AppPath:               g.src.Path,
		Source:                g.src,
		Revision:              g.revision,
		Repo:                  req.Repo,
		AppName:               req.AppName,
		ProjectName:           req.ProjectName,
		Namespace:             req.Namespace,
		KubeVersion:           req.KubeVersion,
		APIVersions:           req.APIVersions,
		Repos:                 req.Repos,
		HelmRepoCreds:         req.HelmRepoCreds,
		ProjectSourceRepos:    req.ProjectSourceRepos,
		HasMultipleSources:    req.HasMultipleSources,
		KustomizeOptions:      req.KustomizeOptions,
		HelmOptions:           req.HelmOptions,
		EnabledSourceTypes:    req.EnabledSourceTypes,
		ManifestGeneratePaths: req.ManifestGeneratePaths,
		Tracking:              trackingOf(req),
	}
	in.GitCreds = s.remoteBaseCreds(g.kind, req.Repo)
	if set != nil {
		in.RefSources = set.refRoots(g.refs, g.kind == kindGit)
		in.OnSourceArchived = set.release
	}
	if g.kind == kindHelm {
		in.RootDir = ""
		in.AppPath = ""
	}
	res, err := s.renderTimed(ctx, in)
	if err != nil {
		if g.kind == kindHelm && isChartFetchError(err) {
			// The renderer pulls chart sources (upstream ExtractChart, before generation).
			return nil, asContentError(err)
		}
		return nil, err
	}
	if err := s.fillManifests(resp, res); err != nil {
		return nil, err
	}
	return resp, nil
}

// renderTimed runs the renderer and records its duration.
func (s *Service) renderTimed(ctx context.Context, in render.Input) (*render.Result, error) {
	start := time.Now()
	res, err := s.renderer.Generate(ctx, in)
	st := ""
	if res != nil {
		st = res.SourceType
	}
	s.metrics.render(st, start, err)
	return res, err
}

// fillManifests serializes rendered objects into resp, bounded by MaxRequestBytes.
func (s *Service) fillManifests(resp *apiclient.ManifestResponse, res *render.Result) error {
	var total int64
	resp.Manifests = make([]string, 0, len(res.Manifests))
	for _, o := range res.Manifests {
		data, err := json.Marshal(o.Object)
		if err != nil {
			return fmt.Errorf("failed to marshal manifest: %w", err)
		}
		total += int64(len(data))
		if total > s.cfg.MaxRequestBytes {
			return errorf(apiclient.CodeResourceExhausted, "generated manifests exceed the maximum response size of %d bytes", s.cfg.MaxRequestBytes)
		}
		resp.Manifests = append(resp.Manifests, string(data))
	}
	resp.SourceType = res.SourceType
	resp.Commands = res.Commands
	return nil
}

// trackingOf returns the tracking configuration of a request: upstream writes tracking
// metadata only when both the instance label key and the application name are set.
func trackingOf(req *apiclient.ManifestRequest) *render.Tracking {
	if req.InstanceLabelKey == "" || req.AppName == "" {
		return nil
	}
	return &render.Tracking{
		Tracker:        tracking.New(tracking.Config{InstanceLabelKey: req.InstanceLabelKey}),
		Method:         tracking.Method(req.TrackingMethod),
		InstallationID: req.InstallationID,
	}
}

// cachedManifest consults the manifest cache. ok reports a final answer: a cached
// response or a cached generation error (SRC-111).
func (s *Service) cachedManifest(key, revision string, first bool) (*apiclient.ManifestResponse, error, bool) { //nolint:revive // three results read naturally here
	e, found := s.manifests.get(key)
	if !found {
		s.metrics.cache("manifest", false)
		return nil, nil, false
	}
	if e.resp == nil {
		threshold := s.cfg.PauseGenerationAfterFailedAttempts
		if threshold <= 0 || e.failures < threshold {
			s.metrics.cache("manifest", false)
			return nil, nil, false
		}
		now := s.cfg.Now()
		if s.cfg.PauseGenerationMinutes > 0 && now.Sub(e.firstFailure) >= time.Duration(s.cfg.PauseGenerationMinutes)*time.Minute {
			s.manifests.remove(key)
			s.metrics.cache("manifest", false)
			return nil, nil, false
		}
		if s.cfg.PauseGenerationRequests > 0 && e.cachedResponses > 0 && e.cachedResponses >= s.cfg.PauseGenerationRequests {
			s.manifests.remove(key)
			s.metrics.cache("manifest", false)
			return nil, nil, false
		}
		if first {
			e.cachedResponses++
			s.manifests.set(e)
		}
		s.metrics.cache("manifest", true)
		return nil, fmt.Errorf("%s: %s", cachedManifestGenerationPrefix, e.lastError), true
	}
	s.metrics.cache("manifest", true)
	resp := e.resp
	resp.Revision = revision
	return resp, nil, true
}

// recordGeneration stores a generation result, or a failure record when error caching
// is enabled.
func (s *Service) recordGeneration(g *generation, resp *apiclient.ManifestResponse, err error) {
	e := &manifestEntry{key: g.key, specDigest: g.specDigest, identity: g.identity, revision: g.revision, refRevs: g.refRevs}
	if err == nil {
		e.resp = cloneManifestResponse(resp)
		s.manifests.set(e)
		return
	}
	if s.cfg.PauseGenerationAfterFailedAttempts <= 0 || isContentError(err) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if prev, ok := s.manifests.get(g.key); ok && prev.resp == nil {
		e = prev
	}
	if e.firstFailure.IsZero() {
		e.firstFailure = s.cfg.Now()
	}
	e.failures++
	e.lastError = s.sanitizer.apply(err.Error())
	s.manifests.set(e)
}

// resolveSource resolves the revision of the main source.
func (s *Service) resolveSource(ctx context.Context, repo apiclient.Repository, src v1alpha1.ApplicationSource, kind sourceKind, revision string, noRevCache bool) (string, error) {
	switch kind {
	case kindOCI:
		c, err := s.ociClient(repo)
		if err != nil {
			return "", fmt.Errorf("failed to initialize oci client: %w", err)
		}
		d, err := c.ResolveReference(ctx, src.RepoURL, revision)
		if err != nil {
			return "", fmt.Errorf("failed to resolve revision %q: %w", revision, err)
		}
		return d, nil
	case kindHelm:
		if noRevCache {
			s.helm.InvalidateIndex(repo.URL)
		}
		pass := src.Helm != nil && src.Helm.PassCredentials
		v, err := s.helm.ResolveChartVersion(ctx, repo.URL, src.Chart, revision, s.helmCreds(repo, pass), helmEnableOCI(repo))
		if err != nil {
			return "", err
		}
		return v, nil
	default:
		return s.resolveGit(ctx, repo, revision, src.TagPrefix, !noRevCache)
	}
}

// pullOCI pulls an OCI artifact into a fresh directory and checks its symlinks.
func (s *Service) pullOCI(ctx context.Context, repo apiclient.Repository, repoURL, digest string, noCache bool) (string, func(), error) {
	c, err := s.ociClient(repo)
	if err != nil {
		return "", nil, fmt.Errorf("failed to initialize oci client: %w", err)
	}
	if noCache {
		if err := c.CleanCache(repoURL); err != nil {
			s.log.Warn("reposerver: cleaning OCI cache failed", slog.Any("error", err))
		}
	}
	tmp, err := tempDir(s.root, prefixOCI)
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = removeTree(tmp) }
	dest := filepath.Join(tmp, "artifact")
	if _, err := c.PullArtifact(ctx, repoURL, digest, dest, oci.Limits{AllowedMediaTypes: s.cfg.OCILayerMediaTypes, Archive: s.cfg.ociExtractLimits()}); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := s.checkSymlinks(ctx, dest, digest, true, "oci image"); err != nil {
		cleanup()
		return "", nil, err
	}
	return dest, cleanup, nil
}

// resolvedRef is a validated, resolved `$ref` source.
type resolvedRef struct {
	key    string // "$values"
	target apiclient.RefTarget
	urlKey string // repoCacheURL of the repository
	sha    string
}

// refCandidates returns the `$...` value files and file parameter paths in order.
func refCandidates(h *v1alpha1.ApplicationSourceHelm) []string {
	if h == nil {
		return nil
	}
	out := slices.Clone(h.ValueFiles)
	for _, fp := range h.FileParameters {
		out = append(out, fp.Path)
	}
	return slices.DeleteFunc(out, func(p string) bool { return !strings.HasPrefix(p, "$") })
}

// lookupRef finds a ref source by "$name" (keys with or without the "$").
func lookupRef(refSources map[string]apiclient.RefTarget, refVar string) (apiclient.RefTarget, bool) {
	if t, ok := refSources[refVar]; ok {
		return t, true
	}
	t, ok := refSources[strings.TrimPrefix(refVar, "$")]
	return t, ok
}

// resolveRefs validates and resolves the `$ref` sources used by value files and file
// parameters of a multi-source Helm source (SRC-095/096). Each repository is resolved
// once; refs are returned in first-use order.
func (s *Service) resolveRefs(ctx context.Context, hasMultiple bool, h *v1alpha1.ApplicationSourceHelm, refSources map[string]apiclient.RefTarget, mainKind sourceKind, mainURL, mainUnresolved, mainRevision string, noRevCache bool) ([]resolvedRef, error) {
	if !hasMultiple {
		return nil, nil
	}
	for k := range refSources {
		if !refKeyPattern.MatchString(k) {
			return nil, errorf(apiclient.CodeInvalidArgument, "sources.ref %s cannot contain any special characters except '_' and '-'", strings.TrimPrefix(k, "$"))
		}
	}
	var out []resolvedRef
	byURL := map[string]int{}
	for _, cand := range refCandidates(h) {
		refVar := strings.SplitN(cand, "/", 2)[0]
		target, ok := lookupRef(refSources, refVar)
		if !ok {
			if len(refSources) == 0 {
				return nil, errorf(apiclient.CodeInvalidArgument, "source referenced %q, but no source has a 'ref' field defined", refVar)
			}
			keys := make([]string, 0, len(refSources))
			for k := range refSources {
				if !strings.HasPrefix(k, "$") {
					k = "$" + k
				}
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return nil, errorf(apiclient.CodeInvalidArgument, "source referenced %q, which is not one of the available sources (%s)", refVar, strings.Join(keys, ", "))
		}
		if target.Chart != "" {
			return nil, errorf(apiclient.CodeInvalidArgument, "source has a 'chart' field defined, but Helm charts are not yet not supported for 'ref' sources")
		}
		urlKey := repoCacheURL(target.Repo.URL, kindGit)
		if i, seen := byURL[urlKey]; seen {
			prev := out[i]
			if prev.key == refVar {
				continue
			}
			if prev.target.TargetRevision != target.TargetRevision {
				return nil, errorf(apiclient.CodeInvalidArgument, "cannot reference multiple revisions for the same repository (%s references %q while %s references %q)", refVar, target.TargetRevision, prev.key, prev.target.TargetRevision)
			}
			continue
		}
		sha, err := s.resolveGit(ctx, target.Repo, target.TargetRevision, "", !noRevCache)
		if err != nil {
			s.log.Error("reposerver: resolving referenced source failed", slog.String("repo", git.SanitizeURL(target.Repo.URL)), slog.Any("error", s.sanitizer.sanitize(err)))
			return nil, withCode(ErrorCode(err), fmt.Errorf("failed to get git client for repo %s", git.SanitizeURL(target.Repo.URL)))
		}
		if mainKind == kindGit && repoCacheURL(mainURL, kindGit) == urlKey && sha != mainRevision {
			return nil, errorf(apiclient.CodeInvalidArgument, "cannot reference a different revision of the same repository (%s references %q which resolves to %q while the application references %q which resolves to %q)", refVar, target.TargetRevision, sha, mainUnresolved, mainRevision)
		}
		byURL[urlKey] = len(out)
		out = append(out, resolvedRef{key: refVar, target: target, urlKey: urlKey, sha: sha})
	}
	return out, nil
}

// refRevisions maps referenced repositories to their resolved commits (cache key).
func refRevisions(refs []resolvedRef) map[string]string {
	if len(refs) == 0 {
		return nil
	}
	out := make(map[string]string, len(refs))
	for _, r := range refs {
		out[r.urlKey] = r.sha
	}
	return out
}

// mergeIntegrity appends ref-source checks (problems prefixed by the ref name).
func mergeIntegrity(into, add *apiclient.SourceIntegrityResult, refKey string) *apiclient.SourceIntegrityResult {
	if add == nil {
		return into
	}
	if into == nil {
		into = &apiclient.SourceIntegrityResult{}
	}
	for _, c := range add.Checks {
		nc := apiclient.SourceIntegrityCheck{Name: c.Name}
		for _, p := range c.Problems {
			nc.Problems = append(nc.Problems, refKey+": "+p)
		}
		into.Checks = append(into.Checks, nc)
	}
	return into
}

// holdSpec describes one working copy to hold.
type holdSpec struct {
	repo         apiclient.Repository
	revision     string
	tagPrefix    string
	readRefCache bool
	submodules   bool
}

func refHoldSpecs(refs []resolvedRef, submodules bool) []holdSpec {
	out := make([]holdSpec, len(refs))
	for i, r := range refs {
		out[i] = holdSpec{repo: r.target.Repo, revision: r.sha, readRefCache: true, submodules: submodules}
	}
	return out
}

// holdError is a holdAll failure of the working copy serving specs (indices into the
// holdAll argument).
type holdError struct {
	specs []int
	err   error
}

func (e *holdError) Error() string { return e.err.Error() }
func (e *holdError) Unwrap() error { return e.err }

// holdFailed reports whether err is a holdAll failure of spec i.
func holdFailed(err error, i int) bool {
	var he *holdError
	return errors.As(err, &he) && slices.Contains(he.specs, i)
}

// holdSet is a set of working copies held together.
type holdSet struct {
	holds   []*wcHold // in spec order (shared holds repeated)
	release func()
}

// refRoots maps "$ref" names to their checkout roots.
func (h *holdSet) refRoots(refs []resolvedRef, mainIsGit bool) map[string]string {
	if len(refs) == 0 {
		return nil
	}
	offset := 0
	if mainIsGit {
		offset = 1
	}
	out := make(map[string]string, len(refs))
	for i, r := range refs {
		out[r.key] = h.holds[i+offset].root
	}
	return out
}

// holdRefs holds the referenced repositories of a non-git main source.
func (s *Service) holdRefs(ctx context.Context, refs []resolvedRef, noCache bool) (*holdSet, error) {
	set, err := s.holdAll(ctx, refHoldSpecs(refs, !s.cfg.DisableSubmodules))
	if err != nil {
		return nil, err
	}
	for i := range refs {
		if err := s.checkSymlinks(ctx, set.holds[i].root, refs[i].sha, noCache, "repository", ".git"); err != nil {
			set.release()
			return nil, err
		}
	}
	return set, nil
}

// holdAll checks out and locks every spec. Working copies are locked in path order so
// requests holding several repositories never deadlock; a working copy needed twice at
// the same revision is locked once.
func (s *Service) holdAll(ctx context.Context, specs []holdSpec) (*holdSet, error) {
	type pending struct {
		spec holdSpec
		dir  string
		idx  []int
	}
	set := &holdSet{holds: make([]*wcHold, len(specs))}
	var releases []func()
	set.release = func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
		releases = nil
	}
	// Group by working copy identity (location + credentials).
	groups := map[string]*pending{}
	var order []*pending
	for i, sp := range specs {
		id := git.RepoKey(sp.repo.URL) + "|" + s.fp.of(sp.repo)
		if p, ok := groups[id]; ok {
			if p.spec.revision != sp.revision {
				return nil, errorf(apiclient.CodeInvalidArgument, "cannot reference a different revision of the same repository (%s and %s)", p.spec.revision, sp.revision)
			}
			p.idx = append(p.idx, i)
			continue
		}
		p := &pending{spec: sp, idx: []int{i}}
		groups[id] = p
		order = append(order, p)
	}
	// Acquire directories first to learn the lock order.
	for _, p := range order {
		dir, _, rel, err := s.workdirs.acquire(p.spec.repo, s.fp.of(p.spec.repo))
		if err != nil {
			set.release()
			return nil, &holdError{specs: p.idx, err: err}
		}
		p.dir = dir
		releases = append(releases, rel)
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].dir < order[j].dir })
	for _, p := range order {
		h, err := s.holdWorkingCopy(ctx, p.spec.repo, gitClientOpts{tagPrefix: p.spec.tagPrefix, readRefCache: p.spec.readRefCache}, p.spec.revision, true,
			s.checkoutPrepare(p.spec.repo.Depth, p.spec.revision, p.spec.submodules))
		if err != nil {
			set.release()
			return nil, &holdError{specs: p.idx, err: err}
		}
		releases = append(releases, h.release)
		for _, i := range p.idx {
			set.holds[i] = h
		}
	}
	var once sync.Once
	inner := set.release
	set.release = func() { once.Do(inner) }
	return set, nil
}

// renderSpecOf builds the cache spec of a request.
func (s *Service) renderSpecOf(req *apiclient.ManifestRequest, src v1alpha1.ApplicationSource, kind sourceKind) renderSpec {
	apis := slices.Clone(req.APIVersions)
	sort.Strings(apis)
	spec := renderSpec{
		Repo:               repoCacheURL(src.RepoURL, kind),
		RepoType:           string(req.Repo.Type),
		Source:             keySource(src, kind),
		AppName:            req.AppName,
		AppNamespace:       req.AppNamespace,
		Namespace:          req.Namespace,
		KubeVersion:        req.KubeVersion,
		APIVersions:        apis,
		KustomizeOptions:   req.KustomizeOptions,
		HelmOptions:        req.HelmOptions,
		EnabledSourceTypes: req.EnabledSourceTypes,
		InstanceLabelKey:   req.InstanceLabelKey,
		TrackingMethod:     req.TrackingMethod,
		InstallationID:     req.InstallationID,
		HasMultipleSources: req.HasMultipleSources,
		ProjectName:        req.ProjectName,
		ManifestGenPaths:   req.ManifestGeneratePaths,
		SourceIntegrity:    req.SourceIntegrity,
		Server:             s.serverStamp,
	}
	// Plugin sockets carry the plugin version in their names: an upgraded sidecar
	// renders anew.
	if s.renderer != nil {
		if plugins, err := s.renderer.ListPlugins(); err == nil {
			spec.Plugins = plugins
		}
	}
	if req.SourceIntegrity != nil && !s.cfg.DisableGPG {
		// Verification results depend on the mounted keys: a key change re-verifies.
		if _, stamp, err := s.keyring.readKeys(); err == nil {
			spec.Keyring = stamp
		}
	}
	if len(req.RefSources) > 0 {
		spec.RefSources = map[string]refSpec{}
		for k, v := range req.RefSources {
			spec.RefSources[k] = refSpec{Repo: repoCacheURL(v.Repo.URL, kindGit), TargetRevision: v.TargetRevision, Chart: v.Chart}
		}
	}
	return spec
}

// identityOf is the source identity used by UpdateRevisionForPaths.
func identityOf(repoURL string, kind sourceKind, appName string, src v1alpha1.ApplicationSource, hasMultiple bool) sourceIdentity {
	return sourceIdentity{Repo: repoCacheURL(repoURL, kind), AppName: appName, Source: keySource(src, kind), HasMultipleSources: hasMultiple}
}

// GenerateManifestWithFiles renders an uploaded source directory (SRC-119): the gzipped
// tar is extracted with size and entry limits into a fresh directory, checked for
// out-of-bounds symlinks and rendered with revision "streamed" (no cache).
func (s *Service) GenerateManifestWithFiles(ctx context.Context, req *apiclient.ManifestRequest, files io.Reader) (_ *apiclient.ManifestResponse, err error) {
	defer s.finish(&err)
	if closer, ok := files.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}
	if req == nil || files == nil {
		return nil, errorf(apiclient.CodeInvalidArgument, "missing request or files")
	}
	tmp, err := tempDir(s.root, prefixStreamed)
	if err != nil {
		return nil, err
	}
	defer func() {
		if rmErr := removeTree(tmp); rmErr != nil {
			s.log.Error("reposerver: removing streamed sources failed", slog.String("security", "critical"), slog.Any("error", rmErr))
		}
	}()
	dest := filepath.Join(tmp, "src")
	if err := os.Mkdir(dest, 0o700); err != nil {
		return nil, err
	}
	limited := &countingReader{r: files, limit: s.cfg.StreamedManifestMaxTarSize}
	lim := sandbox.ArchiveLimits{MaxTotalBytes: s.cfg.StreamedManifestMaxExtractedSize, MaxFileBytes: s.cfg.StreamedManifestMaxExtractedSize}
	if err := sandbox.ExtractTarGzContext(ctx, limited, dest, lim); err != nil {
		if limited.exceeded {
			return nil, errorf(apiclient.CodeResourceExhausted, "file exceeded max size of %d bytes", s.cfg.StreamedManifestMaxTarSize)
		}
		var oob *sandbox.OutOfBoundsSymlinkError
		if errors.As(err, &oob) {
			s.log.Warn("reposerver: out-of-bounds symlink rejected", slog.String("security", "high"), slog.String("tree", "streamed files"), slog.String("file", oob.Path))
			return nil, errorf(apiclient.CodeInvalidArgument, "streamed files contains out-of-bounds symlinks. file: %s", oob.Path)
		}
		code := apiclient.CodeInvalidArgument
		if errors.Is(err, sandbox.ErrArchiveLimit) {
			code = apiclient.CodeResourceExhausted
		}
		return nil, withCode(code, fmt.Errorf("error receiving manifest file stream: %w", err))
	}
	// Drain trailing bytes so the size limit covers the whole upload.
	if _, err := io.Copy(io.Discard, limited); err != nil || limited.exceeded {
		return nil, errorf(apiclient.CodeResourceExhausted, "file exceeded max size of %d bytes", s.cfg.StreamedManifestMaxTarSize)
	}
	if err := s.checkSymlinks(ctx, dest, "", true, "streamed files"); err != nil {
		return nil, err
	}
	src := req.Source
	// The upload is the source: never pull a chart or resolve a remote.
	src.Chart = ""
	if _, err := appDir(dest, src.Path); err != nil {
		return nil, fmt.Errorf("failed to get app path: %w", err)
	}
	in := render.Input{
		RootDir:               dest,
		AppPath:               src.Path,
		Source:                src,
		Revision:              streamedRevision,
		Repo:                  req.Repo,
		AppName:               req.AppName,
		ProjectName:           req.ProjectName,
		Namespace:             req.Namespace,
		KubeVersion:           req.KubeVersion,
		APIVersions:           req.APIVersions,
		Repos:                 req.Repos,
		HelmRepoCreds:         req.HelmRepoCreds,
		ProjectSourceRepos:    req.ProjectSourceRepos,
		KustomizeOptions:      req.KustomizeOptions,
		HelmOptions:           req.HelmOptions,
		EnabledSourceTypes:    req.EnabledSourceTypes,
		ManifestGeneratePaths: req.ManifestGeneratePaths,
		Tracking:              trackingOf(req),
	}
	res, err := s.renderTimed(ctx, in)
	if err != nil {
		return nil, err
	}
	resp := &apiclient.ManifestResponse{Revision: streamedRevision}
	if err := s.fillManifests(resp, res); err != nil {
		return nil, err
	}
	return resp, nil
}

// countingReader fails after limit bytes.
type countingReader struct {
	r        io.Reader
	limit    int64
	n        int64
	exceeded bool
}

var errUploadTooLarge = errors.New("upload exceeds the size limit")

func (c *countingReader) Read(p []byte) (int, error) {
	if c.exceeded {
		return 0, errUploadTooLarge
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.limit {
		c.exceeded = true
		return n, errUploadTooLarge
	}
	return n, err
}

// manifestsOf decodes response manifests (tests and helpers).
func manifestsOf(resp *apiclient.ManifestResponse) ([]*unstructured.Unstructured, error) {
	out := make([]*unstructured.Unstructured, 0, len(resp.Manifests))
	for _, m := range resp.Manifests {
		var obj map[string]any
		if err := json.Unmarshal([]byte(m), &obj); err != nil {
			return nil, err
		}
		out = append(out, &unstructured.Unstructured{Object: obj})
	}
	return out, nil
}
