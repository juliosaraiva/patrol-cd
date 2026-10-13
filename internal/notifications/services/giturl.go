package services

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	scpLikeURL = regexp.MustCompile(`^(?:(?P<user>[^@/]+)@)?(?P<host>[^:/\s]+):(?P<path>[^\\].*)$`)
	gitSuffix  = regexp.MustCompile(`\.git/?$`)
)

// ParseGitURL parses the repository URL forms accepted by git: scheme URLs (https, http,
// ssh, git, git+ssh, file, ...), scp-like "[user@]host:path" and local paths.
func ParseGitURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty repository URL")
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid repository URL: %w", err)
		}
		return u, nil
	}
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, ".") {
		return &url.URL{Scheme: "file", Path: raw}, nil
	}
	if m := scpLikeURL.FindStringSubmatch(raw); m != nil {
		u := &url.URL{Scheme: "ssh", Host: m[2], Path: m[3]}
		if m[1] != "" {
			u.User = url.User(m[1])
		}
		return u, nil
	}
	return &url.URL{Scheme: "file", Path: raw}, nil
}

// RepoURLToHTTPS converts a git URL to its https form without credentials.
func RepoURLToHTTPS(raw string) (string, error) {
	u, err := ParseGitURL(raw)
	if err != nil {
		return "", err
	}
	u.Scheme = "https"
	u.User = nil
	return u.String(), nil
}

// FullNameByRepoURL returns "<owner>/<repo>" (the first two path segments without a .git
// suffix) for GitHub, GitLab and Bitbucket style URLs. Unparseable input yields "".
func FullNameByRepoURL(raw string) string {
	u, err := ParseGitURL(raw)
	if err != nil {
		return ""
	}
	p := gitSuffix.ReplaceAllString(u.Path, "")
	var parts []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return p
}
