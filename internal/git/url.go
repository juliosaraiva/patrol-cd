package git

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/url"
	"regexp"
	"strings"
)

var (
	// scpUserRegexp mirrors upstream's IsSSHURL rule: an optional ssh:// prefix, a
	// user without '/' or ':', '@', and a remainder without another '@'.
	scpUserRegexp = regexp.MustCompile(`^(?i:ssh://)?([^/:]*?)@[^@]+$`)
	commitSHARe   = regexp.MustCompile(`^(?:[0-9A-Fa-f]{40}|[0-9A-Fa-f]{64})$`)
	truncatedSHA  = regexp.MustCompile(`^[0-9A-Fa-f]{7,64}$`)
)

// defaultPorts are stripped by NormalizeURL so that explicit default ports compare
// equal to omitted ones.
var defaultPorts = map[string]string{
	"https": "443",
	"http":  "80",
	"ssh":   "22",
	"git":   "9418",
}

// IsCommitSHA reports whether s is a full commit object name: 40 hexadecimal
// characters (SHA-1, as upstream) or 64 (SHA-256 repositories).
func IsCommitSHA(s string) bool { return commitSHARe.MatchString(s) }

// IsTruncatedCommitSHA reports whether s looks like an abbreviated commit SHA
// (7 to 64 hexadecimal characters), as upstream's IsTruncatedCommitSHA.
func IsTruncatedCommitSHA(s string) bool { return truncatedSHA.MatchString(s) }

// IsSSHURL reports whether raw is an SSH repository URL and returns its user.
// It accepts every ssh:// URL and scp-like "user@host:path" forms (upstream requires
// the user part for scp-like URLs, and so does this function).
func IsSSHURL(raw string) (bool, string) {
	s := strings.TrimSpace(raw)
	if hasPrefixFold(s, "ssh://") {
		u, err := url.Parse(s)
		if err != nil {
			return true, ""
		}
		if u.User != nil {
			return true, u.User.Username()
		}
		return true, ""
	}
	if strings.Contains(s, "://") {
		return false, ""
	}
	m := scpUserRegexp.FindStringSubmatch(s)
	if m == nil {
		return false, ""
	}
	return true, m[1]
}

// IsHTTPSURL reports whether raw uses the https scheme (case-insensitive).
func IsHTTPSURL(raw string) bool { return hasPrefixFold(strings.TrimSpace(raw), "https://") }

// IsHTTPURL reports whether raw uses the plain http scheme (case-insensitive).
func IsHTTPURL(raw string) bool { return hasPrefixFold(strings.TrimSpace(raw), "http://") }

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// NormalizeURL returns a canonical form of a repository URL for comparison and cache
// keys. It lower-cases the scheme and host, strips default ports, removes
// credentials (all user info for http(s), git and file URLs; the password for ssh,
// whose user is kept), converts scp-like "user@host:path" URLs to
// "ssh://user@host/path", drops fragments and trailing slashes, and strips any number
// of trailing ".git" suffixes. Path case is preserved; SameURL compares
// case-insensitively like upstream. It returns "" when raw cannot be parsed.
//
// The output is idempotent (NormalizeURL(NormalizeURL(u)) == NormalizeURL(u)) but,
// as upstream warns, not a stable format across releases.
func NormalizeURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || strings.ContainsFunc(s, isControl) {
		return ""
	}
	if ok, _ := IsSSHURL(s); ok && !hasPrefixFold(s, "ssh://") {
		s = scpToSSH(s)
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	if u.Opaque != "" {
		// "host:path" without a user: treat as scp-like when it cannot carry
		// credentials, otherwise refuse.
		if strings.Contains(u.Opaque, "@") || u.User != nil {
			return ""
		}
		u, err = url.Parse(scpToSSH(s))
		if err != nil || u.Opaque != "" {
			return ""
		}
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if port := u.Port(); port != "" && defaultPorts[u.Scheme] == port {
		u.Host = hostWithoutPort(u.Host)
	}
	switch u.Scheme {
	case "ssh":
		if u.User != nil {
			u.User = url.User(u.User.Username())
		}
	default:
		u.User = nil
	}
	p := u.Path
	for {
		prev := p
		p = strings.TrimRight(p, "/")
		if len(p) >= 4 && strings.EqualFold(p[len(p)-4:], ".git") {
			p = p[:len(p)-4]
		}
		if p == prev {
			break
		}
	}
	u.Path = p
	u.RawPath = ""
	u.Fragment = ""
	u.RawFragment = ""
	u.ForceQuery = false
	return u.String()
}

// scpToSSH turns "user@host:path" into "ssh://user@host/path" (a leading '/' in path
// is not doubled).
func scpToSSH(s string) string {
	// The host part ends at the first ':' after the (optional) user.
	start := 0
	if at := strings.Index(s, "@"); at >= 0 {
		start = at + 1
	}
	colon := strings.Index(s[start:], ":")
	if colon < 0 {
		return "ssh://" + s
	}
	colon += start
	rest := s[colon+1:]
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return "ssh://" + s[:colon] + rest
}

func hostWithoutPort(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// SameURL reports whether two repository URLs point to the same location. Both are
// normalized with NormalizeURL and compared case-insensitively (upstream lower-cases
// the whole URL). URLs that cannot be normalized compare equal only when their
// trimmed text is identical.
func SameURL(a, b string) bool {
	na, nb := NormalizeURL(a), NormalizeURL(b)
	if na == "" || nb == "" {
		ta, tb := strings.TrimSpace(a), strings.TrimSpace(b)
		return ta != "" && ta == tb
	}
	return strings.EqualFold(na, nb)
}

// RepoKey returns a file-system safe key for a repository URL (40 lowercase hex
// characters), suitable for naming the persistent checkout directory. URLs for which
// SameURL is true map to the same key.
func RepoKey(raw string) string {
	n := NormalizeURL(raw)
	if n == "" {
		n = strings.TrimSpace(raw)
	}
	sum := sha256.Sum256([]byte(strings.ToLower(n)))
	return hex.EncodeToString(sum[:])[:40]
}

// SanitizeURL returns raw with any password replaced by "******", for logs and error
// messages. Unparseable input yields "<invalid URL>".
func SanitizeURL(raw string) string {
	s := strings.TrimSpace(raw)
	if ok, _ := IsSSHURL(s); ok && !hasPrefixFold(s, "ssh://") {
		return s // scp-like URLs cannot carry a password
	}
	u, err := url.Parse(s)
	if err != nil {
		return "<invalid URL>"
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			u.User = url.UserPassword(u.User.Username(), "******")
		}
	}
	return u.String()
}

// httpOrigin returns "scheme://host[:port]" for http(s) URLs and "" otherwise.
func httpOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + strings.ToLower(u.Host)
}

// sameOrigin compares two "scheme://host[:port]" origins, treating default ports as
// omitted.
func sameOrigin(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return false
	}
	norm := func(u *url.URL) string {
		host := strings.ToLower(u.Host)
		if p := u.Port(); p != "" && defaultPorts[strings.ToLower(u.Scheme)] == p {
			host = hostWithoutPort(host)
		}
		return strings.ToLower(u.Scheme) + "://" + host
	}
	return ua.Host != "" && norm(ua) == norm(ub)
}
