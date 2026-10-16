package project

import (
	"net/url"
	"regexp"
	"strings"
)

// sshURLRegex recognizes scp-like and ssh:// git URLs (user@host...), as upstream.
var sshURLRegex = regexp.MustCompile(`^(ssh://)?([^/:]*?)@[^@]+$`)

// NormalizeGitURL normalizes a repository URL for comparison exactly like upstream
// git.NormalizeGitURL: lower-cased and trimmed, scp-like SSH URLs ("git@host:org/repo")
// rewritten to "host/org/repo" form with the user kept, a trailing ".git" removed, and the
// result re-serialized by net/url with any "ssh://" prefix removed. URLs that net/url cannot
// parse normalize to "" (so two unparsable URLs compare equal, as upstream).
func NormalizeGitURL(repo string) string {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if sshURLRegex.MatchString(repo) && !strings.HasPrefix(repo, "ssh://") {
		// Replace the first ':' of git@server:org/repo with '/', otherwise net/url would
		// read "org" as the port.
		repo = "ssh://" + strings.Replace(repo, ":", "/", 1)
	}
	repo = strings.TrimSuffix(repo, ".git")
	u, err := url.Parse(repo)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.String(), "ssh://")
}

// SameRepoURL reports whether two repository URLs designate the same location (upstream
// git.SameURL): their normalized forms are equal and non-empty; an unparsable URL is
// compared verbatim.
func SameRepoURL(a, b string) bool {
	na, nb := NormalizeGitURL(a), NormalizeGitURL(b)
	if na == "" {
		na = a
	}
	if nb == "" {
		nb = b
	}
	return na != "" && nb != "" && na == nb
}
