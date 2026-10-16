package repository

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

// maxURLLength bounds repository URLs accepted by lookups (the CRDs allow 2048 bytes).
const maxURLLength = 4096

// urlKey is the key under which two repository URLs are the same location, exactly as
// upstream git.SameURL decides: the normalized URL (NormalizeGitURL), or the URL itself when
// it cannot be normalized. Equal non-empty keys mean the same repository.
func urlKey(raw string) string {
	if n := project.NormalizeGitURL(raw); n != "" {
		return n
	}
	return raw
}

// prefixKey is the normalized form used for credential template matching (upstream
// getRepositoryCredentialIndex): lower-cased, scp-style SSH rewritten, one trailing ".git"
// removed; "" when the URL cannot be parsed (it then matches nothing). Trailing slashes are
// kept, so a template "https://host/org/" never matches "https://host/organization/x".
func prefixKey(raw string) string { return project.NormalizeGitURL(raw) }

// prefixMatches reports whether the template prefix applies to the repository URL and
// returns the length used to rank matches (longest wins).
func prefixMatches(repoKey, templateURL string) (int, bool) {
	tk := prefixKey(templateURL)
	if repoKey == "" || tk == "" || !strings.HasPrefix(repoKey, tk) {
		return 0, false
	}
	return len(tk), true
}

// scpLike matches "user@host:path" and "host:path", with an optional numeric port segment
// ("git@host:22/org/repo"), like the git-urls parser upstream uses.
var scpLike = regexp.MustCompile(`^(?:[^@/\s]+@)?([^:/\s]+):(?:([0-9]{1,5})/)?(.*)$`)

// ownerFromURL extracts the lower-cased organization (first path segment) of a repository
// URL for GitHub App installation discovery, accepting the forms upstream's
// ExtractOrgFromRepoURL accepts: https URLs (with or without port and ".git"), scp-style SSH,
// ssh:// with a port, and the malformed "ssh://git@host:org/repo".
func ownerFromURL(repoURL string) (string, error) {
	s := strings.TrimSpace(repoURL)
	if s == "" {
		return "", errors.New("repo URL is empty")
	}
	if strings.HasPrefix(s, "ssh://git@") {
		rem := strings.TrimPrefix(s, "ssh://")
		if _, after, ok := strings.Cut(rem, ":"); ok {
			slash := strings.Index(after, "/")
			isPort := false
			if slash > 0 {
				if _, err := strconv.Atoi(after[:slash]); err == nil {
					isPort = true
				}
			}
			if !isPort && slash != 0 {
				s = rem
			}
		}
	}
	var path string
	switch {
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("failed to parse repository URL %q", git.SanitizeURL(repoURL))
		}
		path = u.Path
	default:
		if m := scpLike.FindStringSubmatch(s); m != nil {
			path = m[3]
		} else {
			path = s
		}
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if path == "" {
		return "", fmt.Errorf("repository URL %q does not contain a path", git.SanitizeURL(repoURL))
	}
	if idx := strings.Index(path, "/"); idx > 0 {
		return strings.ToLower(path[:idx]), nil
	}
	return "", fmt.Errorf("could not extract organization from repository URL %q: path %q does not contain org/repo format", git.SanitizeURL(repoURL), path)
}

// repoHost returns the lower-cased host name (no port) of an https, http, ssh or scp-style
// repository URL, or "" when there is none.
func repoHost(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		return strings.ToLower(u.Hostname())
	}
	if m := scpLike.FindStringSubmatch(s); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

// urlScheme returns the lower-cased scheme of a repository URL ("" for scp-style SSH and
// scheme-less references).
func urlScheme(raw string) string {
	s := strings.TrimSpace(raw)
	i := strings.Index(s, "://")
	if i <= 0 {
		return ""
	}
	return strings.ToLower(s[:i])
}

// isHelmOCIRepo reports whether a Helm repository URL is an OCI registry reference written
// without a scheme ("registry.example.com/charts"), as upstream helm.IsHelmOciRepo.
func isHelmOCIRepo(repoURL string) bool {
	if repoURL == "" {
		return false
	}
	u, err := url.Parse(repoURL)
	return err == nil && u.Host == ""
}

// registryHost returns "host[:port]" of an OCI registry reference ("host/path",
// "oci://host/path" or "https://host/path"), as upstream's ACR flow takes the first path
// element of the Helm OCI repository URL.
func registryHost(repoURL string) string {
	s := strings.TrimSpace(repoURL)
	for _, p := range []string{"oci://", "https://", "http://"} {
		if len(s) >= len(p) && strings.EqualFold(s[:len(p)], p) {
			s = s[len(p):]
			break
		}
	}
	host, _, _ := strings.Cut(s, "/")
	if host == "" || strings.ContainsAny(host, "@ \t") {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil && h == "" {
		return ""
	}
	return strings.ToLower(host)
}

// hostOnly strips the port of "host[:port]".
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}
