package webhook

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// maxURLLength bounds URLs taken from webhook payloads before they are turned into
// regular expressions.
const maxURLLength = 2048

// usernamePattern is the user part accepted in repository URLs (RFC 3986 user info
// restricted to the shadow-utils user name grammar), as upstream.
const usernamePattern = `[\w\.][\w\.-]{0,30}[\w\.\$-]?`

// WebURLRegex compiles a case-insensitive regular expression that matches every
// repository URL designating the repository whose web URL (from a webhook payload) is
// webURL: an optional http(s):// or ssh:// scheme, an optional user, an optional ssh. or
// altssh. host prefix, the host, an optional port, ':' or '/', the path and an optional
// .git suffix (REPO-124). Host and path are regex-escaped, so "." never acts as a
// wildcard.
func WebURLRegex(webURL string) (*regexp.Regexp, error) {
	return urlRegex(webURL, `(?i)^((https?|ssh)://)?(%[1]s@)?((alt)?ssh\.)?%[2]s(:\d+)?[:/]%[3]s(\.git)?$`)
}

// APIURLRegex compiles a case-insensitive regular expression matching API base URLs on
// the host of apiURL: an optional http(s):// scheme, an optional user, the host, an
// optional port and an optional trailing '/', without a path (upstream GetAPIURLRegex).
func APIURLRegex(apiURL string) (*regexp.Regexp, error) {
	return urlRegex(apiURL, `(?i)^(https?://)?(%[1]s@)?%[2]s(:\d+)?/?$`)
}

func urlRegex(raw, format string) (*regexp.Regexp, error) {
	if len(raw) > maxURLLength {
		return nil, fmt.Errorf("URL of %d bytes exceeds the %d-byte limit", len(raw), maxURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL '%s'", scm.RedactURLString(raw))
	}
	host := regexp.QuoteMeta(u.Hostname())
	p := regexp.QuoteMeta(strings.TrimPrefix(u.EscapedPath(), "/"))
	re, err := regexp.Compile(fmt.Sprintf(format, usernamePattern, host, p))
	if err != nil {
		return nil, fmt.Errorf("failed to compile regexp for URL '%s'", scm.RedactURLString(raw))
	}
	return re, nil
}

// apiHostMatches reports whether a generator API base URL (scheme optional) designates
// the host of the API URL carried by a webhook payload. Ports, user info and paths are
// ignored, so GitHub Enterprise bases such as https://ghe.example.com/api/v3/ match too
// (upstream's host-only regex rejects any path; see the component doc). Schemes other
// than http and https never match.
func apiHostMatches(generatorAPI, eventAPIURL string) bool {
	gh := hostOf(generatorAPI)
	eh := hostOf(eventAPIURL)
	return gh != "" && eh != "" && strings.EqualFold(gh, eh)
}

// hostOf returns the host name of an http(s) URL; a value without a scheme is read as
// https. Other schemes and unparsable values yield "".
func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLLength {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.Hostname()
}

// sourceRevisionHasChanged reports whether a push of revision (the short ref name) moved
// the revision an Application source tracks (REPO-125): an empty or HEAD targetRevision
// follows the default branch; refs/heads/ and refs/tags/ prefixes are reduced to their
// last part; otherwise the full targetRevision is compared. Branches and tags are not
// distinguished, as upstream.
func sourceRevisionHasChanged(targetRevision, revision string, touchedHead bool) bool {
	targetRev := scm.ParseRevision(targetRevision)
	if targetRev == "HEAD" || targetRev == "" {
		return touchedHead
	}
	if strings.HasPrefix(targetRevision, "refs/heads/") || strings.HasPrefix(targetRevision, "refs/tags/") {
		return compareRevisions(revision, targetRev)
	}
	return compareRevisions(revision, targetRevision)
}

// compareRevisions reports whether revision satisfies targetRevision: equal strings, or
// targetRevision is a semver constraint satisfied by revision as a semver version.
func compareRevisions(revision, targetRevision string) bool {
	if revision == targetRevision {
		return true
	}
	if revision == "" || targetRevision == "" {
		return false
	}
	constraint, err := semver.NewConstraint(targetRevision)
	if err != nil {
		return false
	}
	version, err := semver.NewVersion(revision)
	if err != nil {
		return false
	}
	return constraint.Check(version)
}

// generatorRevisionHasChanged is the Git generator variant (ASET-105): an empty or HEAD
// revision follows the default branch; otherwise the short or the full generator
// revision must equal the pushed revision (no semver).
func generatorRevisionHasChanged(genRevision, revision string, touchedHead bool) bool {
	targetRev := scm.ParseRevision(genRevision)
	if targetRev == "HEAD" || targetRev == "" {
		return touchedHead
	}
	return targetRev == revision || genRevision == revision
}

// refreshPaths returns the repository paths whose changes affect a source (REPO-126):
// the semicolon-separated entries of the manifest-generate-paths annotation, trimmed and
// without empty entries, absolute entries taken from the repository root and relative
// entries joined to the source path. It returns nil without the annotation (every change
// matters). isSyncSource selects the hydrator sync source, whose own path is used and the
// annotation ignored.
func refreshPaths(annotation string, sourcePath string, isSyncSource bool) []string {
	if isSyncSource {
		return []string{sourcePath}
	}
	if annotation == "" {
		return nil
	}
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
		out = append(out, path.Clean(path.Join(sourcePath, item)))
	}
	return out
}

// filesHaveChanged reports whether a push touching changedFiles affects refreshPaths
// (upstream AppFilesHaveChanged): always true without changed files or without refresh
// paths; otherwise a changed file must equal a refresh path, lie in or under its
// directory, or match it as a path.Match glob.
func filesHaveChanged(refreshPaths, changedFiles []string) bool {
	if len(changedFiles) == 0 || len(refreshPaths) == 0 {
		return true
	}
	for _, f := range changedFiles {
		f = ensureAbs(f)
		for _, item := range refreshPaths {
			item = ensureAbs(item)
			if f == item || isUnderRoot(item, f) {
				return true
			}
			if ok, err := path.Match(item, f); err == nil && ok {
				return true
			}
		}
	}
	return false
}

func ensureAbs(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}

// isUnderRoot reports whether the directory of file is root or below it (both absolute).
func isUnderRoot(root, file string) bool {
	root = path.Clean(root)
	dir := path.Dir(file)
	if root == "/" || root == dir {
		return true
	}
	return strings.HasPrefix(strings.TrimSuffix(dir, "/")+"/", strings.TrimSuffix(root, "/")+"/")
}

// normalizeOCI normalizes an OCI repository URL for registry webhook matching: without
// the oci:// scheme and a trailing '/', lower-cased.
func normalizeOCI(u string) string {
	u = strings.TrimPrefix(strings.TrimSpace(u), "oci://")
	u = strings.TrimSuffix(u, "/")
	return strings.ToLower(u)
}
