package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Refs is the remote ref listing summarized as upstream's ListRefs: short branch
// and tag names, each sorted lexicographically.
type Refs struct {
	Branches []string
	Tags     []string
	// HEAD is the commit the remote HEAD points to ("" when the remote has no
	// HEAD, for example an empty repository or a HEAD naming a missing branch).
	HEAD string
	// HEADRef is the full name of the branch the remote HEAD symbolically points
	// to (for example "refs/heads/main"), when advertised.
	HEADRef string
}

// remoteRef is one advertised reference. Tags carry the peeled commit hash when
// the server advertised one (annotated tags), as upstream does.
type remoteRef struct {
	name   string // full name, e.g. refs/heads/main or HEAD
	hash   string // object name ("" for a symbolic ref without a hash line)
	target string // symbolic ref target ("" for hash refs)
}

// refList is a parsed ls-remote listing in advertisement order.
type refList struct {
	refs []remoteRef
}

// parseLsRemote parses `git ls-remote --symref` output.
func parseLsRemote(out []byte) (*refList, error) {
	list := &refList{}
	index := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		left, name, ok := strings.Cut(line, "\t")
		if !ok || name == "" {
			return nil, fmt.Errorf("git: malformed ls-remote line %q", truncateForMessage(line))
		}
		if target, isSym := strings.CutPrefix(left, "ref: "); isSym {
			if i, seen := index[name]; seen {
				list.refs[i].target = target
			} else {
				index[name] = len(list.refs)
				list.refs = append(list.refs, remoteRef{name: name, target: target})
			}
			continue
		}
		if !isHex(left) || (len(left) != 40 && len(left) != 64) {
			return nil, fmt.Errorf("git: malformed object name in ls-remote line %q", truncateForMessage(line))
		}
		if base, peeled := strings.CutSuffix(name, "^{}"); peeled {
			if i, seen := index[base]; seen && strings.HasPrefix(base, "refs/tags/") {
				list.refs[i].hash = left
			}
			continue
		}
		if i, seen := index[name]; seen {
			list.refs[i].hash = left
			continue
		}
		index[name] = len(list.refs)
		list.refs = append(list.refs, remoteRef{name: name, hash: left})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("git: read ls-remote output: %w", err)
	}
	return list, nil
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// shortRefName mirrors go-git's ReferenceName.Short (used by upstream): the
// last matching rule of refs/<x>, refs/tags/<x>, refs/heads/<x>, refs/remotes/<x>
// wins.
func shortRefName(name string) string {
	res := name
	if s, ok := strings.CutPrefix(name, "refs/"); ok && s != "" {
		res = s
	}
	for _, p := range []string{"refs/tags/", "refs/heads/", "refs/remotes/"} {
		if s, ok := strings.CutPrefix(name, p); ok && s != "" {
			res = s
		}
	}
	return res
}

// isShortRef mirrors upstream IsShortRef.
func isShortRef(rev string) bool { return shortRefName(rev) == rev }

func (l *refList) tags() []string {
	var tags []string
	for _, r := range l.refs {
		if t, ok := strings.CutPrefix(r.name, "refs/tags/"); ok && t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// resolve mirrors upstream lsRemote's matching loop over the listing.
func (l *refList) resolve(revision string) (string, bool) {
	short := isShortRef(revision)
	hashes := make(map[string]string, len(l.refs))
	for _, r := range l.refs {
		if r.hash != "" && r.target == "" {
			hashes[r.name] = r.hash
		}
	}
	symTarget := ""
	for _, r := range l.refs {
		if (short && shortRefName(r.name) == revision) || r.name == revision {
			if r.target == "" && r.hash != "" {
				return r.hash, true
			}
			if r.target != "" {
				symTarget = r.target
				if h, ok := hashes[r.target]; ok {
					return h, true
				}
				if r.hash != "" {
					return r.hash, true
				}
			}
		}
	}
	if symTarget != "" {
		if h, ok := hashes[symTarget]; ok {
			return h, true
		}
	}
	return "", false
}

func (l *refList) summary() Refs {
	out := Refs{Branches: []string{}, Tags: []string{}}
	for _, r := range l.refs {
		switch {
		case r.name == "HEAD":
			out.HEADRef = r.target
			out.HEAD = r.hash
		case strings.HasPrefix(r.name, "refs/heads/"):
			out.Branches = append(out.Branches, strings.TrimPrefix(r.name, "refs/heads/"))
		case strings.HasPrefix(r.name, "refs/tags/"):
			out.Tags = append(out.Tags, strings.TrimPrefix(r.name, "refs/tags/"))
		}
	}
	if out.HEAD == "" && out.HEADRef != "" {
		if h, ok := l.resolve(out.HEADRef); ok {
			out.HEAD = h
		}
	}
	slices.Sort(out.Branches)
	slices.Sort(out.Tags)
	return out
}

// RefCache shares remote ref listings between clients for a TTL (upstream's
// git-refs cache, SRC-107). Entries are keyed by the repository location (RepoKey,
// so URLs that may name different repositories never share a listing) and a
// process-local digest of the credentials, so a listing obtained with one set of
// credentials is never served to a client holding different ones. Concurrent
// listings of the same key are coalesced into one ls-remote. The zero value is
// not usable; use NewRefCache.
type RefCache struct {
	ttl   time.Duration
	max   int
	now   func() time.Time
	mu    sync.Mutex
	items map[string]refCacheEntry
	group singleflight.Group
}

type refCacheEntry struct {
	refs    *refList
	expires time.Time
	group   string // sameURLKey of the repository URL, for Invalidate
}

// DefaultRefCacheTTL matches upstream's default revision cache expiration (3m).
const DefaultRefCacheTTL = 3 * time.Minute

// NewRefCache returns a cache holding at most maxEntries listings (0 = 1024) for
// ttl (0 = DefaultRefCacheTTL).
func NewRefCache(ttl time.Duration, maxEntries int) *RefCache {
	if ttl <= 0 {
		ttl = DefaultRefCacheTTL
	}
	if maxEntries <= 0 {
		maxEntries = 1024
	}
	return &RefCache{ttl: ttl, max: maxEntries, now: time.Now, items: map[string]refCacheEntry{}}
}

func (rc *RefCache) get(key string) (*refList, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	e, ok := rc.items[key]
	if !ok {
		return nil, false
	}
	if !rc.now().Before(e.expires) {
		delete(rc.items, key)
		return nil, false
	}
	return e.refs, true
}

func (rc *RefCache) set(key, group string, refs *refList) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	now := rc.now()
	if _, exists := rc.items[key]; !exists && len(rc.items) >= rc.max {
		// Drop expired entries first, then the entry closest to expiry.
		oldestKey, oldest := "", time.Time{}
		for k, e := range rc.items {
			if !now.Before(e.expires) {
				delete(rc.items, k)
				continue
			}
			if oldestKey == "" || e.expires.Before(oldest) {
				oldestKey, oldest = k, e.expires
			}
		}
		if len(rc.items) >= rc.max && oldestKey != "" {
			delete(rc.items, oldestKey)
		}
	}
	rc.items[key] = refCacheEntry{refs: refs, expires: now.Add(rc.ttl), group: group}
}

// Invalidate drops every cached listing of repoURL (for example after a webhook),
// including listings cached for any spelling SameURL considers equal.
func (rc *RefCache) Invalidate(repoURL string) {
	group := sameURLKey(repoURL)
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for k, e := range rc.items {
		if e.group == group {
			delete(rc.items, k)
		}
	}
}

// Len returns the number of cached listings (including expired ones not yet
// evicted).
func (rc *RefCache) Len() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.items)
}

// lsRemote runs ls-remote against the repository.
func (c *Client) lsRemote(ctx context.Context) (*refList, error) {
	var list *refList
	err := c.withRetry(ctx, func() error {
		res, err := c.run(ctx, cmdSpec{
			args:         []string{"ls-remote", "--symref", "--", c.remoteURL},
			noRepo:       true,
			credentialed: true,
			maxStdout:    maxRefsOutput,
		})
		if err != nil {
			return err
		}
		list, err = parseLsRemote(res.Stdout)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list refs: %w", err)
	}
	return list, nil
}

// refs returns the remote listing, through the ref cache when configured.
func (c *Client) refs(ctx context.Context) (*refList, error) {
	rc := c.opts.refCache
	if rc == nil {
		return c.lsRemote(ctx)
	}
	fp := credsFingerprint(c.creds)
	if fp == "" {
		return c.lsRemote(ctx)
	}
	key := RepoKey(c.remoteURL) + "|" + fp
	group := sameURLKey(c.remoteURL)
	if !c.opts.readRefCache {
		list, err := c.lsRemote(ctx)
		if err == nil {
			rc.set(key, group, list)
		}
		return list, err
	}
	if list, ok := rc.get(key); ok {
		return list, nil
	}
	ch := rc.group.DoChan(key, func() (any, error) {
		if list, ok := rc.get(key); ok {
			return list, nil
		}
		// The shared listing must not be aborted by the first caller's
		// cancellation; it is still bounded by the command timeout.
		list, err := c.lsRemote(context.WithoutCancel(ctx))
		if err != nil {
			return nil, err
		}
		rc.set(key, group, list)
		return list, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.(*refList), nil
	}
}

// LsRemote lists the remote branches and tags (upstream LsRefs / ListRefs).
func (c *Client) LsRemote(ctx context.Context) (Refs, error) {
	list, err := c.refs(ctx)
	if err != nil {
		return Refs{}, err
	}
	return list.summary(), nil
}

// ResolveRevision resolves a revision to a full commit SHA, as upstream LsRemote:
//
//   - a full 40 (or 64) hex SHA is returned as is without contacting the remote;
//   - "" means HEAD;
//   - a semver constraint (not an exact version) is replaced by the highest
//     matching tag (scoped by TagPrefix); when nothing matches the revision is
//     resolved literally;
//   - refs are matched by short name ("main", "v1.0") or full name
//     ("refs/heads/main", "refs/pull/1/head") in remote listing order; symbolic
//     refs are followed and annotated tags resolve to the peeled commit;
//   - otherwise a 7+ hex string is a truncated SHA, expanded to the full SHA from
//     the local working copy (initialized and fetched when needed; callers hold
//     the repository lock).
//
// Unresolvable revisions return *RevisionNotFoundError (ErrRevisionNotFound).
func (c *Client) ResolveRevision(ctx context.Context, revision string) (string, error) {
	if IsCommitSHA(revision) {
		return revision, nil
	}
	list, err := c.refs(ctx)
	if err != nil {
		return "", err
	}
	if revision == "" {
		revision = "HEAD"
	}
	if v, err := MaxVersion(revision, list.tags(), c.opts.tagPrefix); err == nil {
		revision = v
	} else {
		c.log.Debug("git: revision is not a matching semver constraint", slog.Any("reason", err))
	}
	if sha, ok := list.resolve(revision); ok {
		return sha, nil
	}
	if IsTruncatedCommitSHA(revision) {
		return c.expandShortSHA(ctx, revision)
	}
	return "", &RevisionNotFoundError{Revision: revision}
}

// expandShortSHA resolves an abbreviated SHA in the working copy, fetching when
// the commit is not present locally.
func (c *Client) expandShortSHA(ctx context.Context, short string) (string, error) {
	if c.root == "" {
		return "", fmt.Errorf("resolve truncated SHA %s: %w", short, ErrNotInitialized)
	}
	if err := c.Init(ctx); err != nil {
		return "", err
	}
	if sha, err := c.revParseCommit(ctx, short); err == nil {
		return sha, nil
	}
	if err := c.Fetch(ctx, ""); err != nil {
		return "", err
	}
	sha, err := c.revParseCommit(ctx, short)
	if err != nil {
		if errors.Is(err, ErrRevisionNotFound) {
			return "", &RevisionNotFoundError{Revision: short}
		}
		return "", err
	}
	return sha, nil
}

// revParseCommit resolves rev to a local commit SHA.
func (c *Client) revParseCommit(ctx context.Context, rev string) (string, error) {
	if err := validateRevisionArg(rev); err != nil {
		return "", err
	}
	out, err := c.git(ctx, cmdSpec{
		args:      []string{"rev-parse", "--verify", "--quiet", "--end-of-options", rev + "^{commit}"},
		maxStdout: maxSmallOutput,
	})
	if err != nil {
		var ce *CommandError
		if errors.As(err, &ce) && ce.Kind == nil {
			ce.Kind = ErrRevisionNotFound // --quiet: no message, exit 1
		}
		return "", err
	}
	sha := strings.TrimSpace(out)
	if !IsCommitSHA(sha) {
		return "", &RevisionNotFoundError{Revision: rev}
	}
	return sha, nil
}
