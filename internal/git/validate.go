package git

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// maxArgLen bounds revision and ref arguments passed to git.
const maxArgLen = 4096

// validateRevisionArg rejects revision arguments git could misinterpret: empty
// values, values starting with '-' (options), and control characters.
func validateRevisionArg(rev string) error {
	switch {
	case rev == "":
		return fmt.Errorf("%w: empty revision", ErrInvalidRevision)
	case len(rev) > maxArgLen:
		return fmt.Errorf("%w: revision too long", ErrInvalidRevision)
	case strings.HasPrefix(rev, "-"):
		return fmt.Errorf("%w: revision %q must not start with '-'", ErrInvalidRevision, truncateForMessage(rev))
	case strings.ContainsFunc(rev, isControlOrSpace):
		return fmt.Errorf("%w: revision %q contains whitespace or control characters", ErrInvalidRevision, truncateForMessage(rev))
	}
	return nil
}

func isControlOrSpace(r rune) bool { return r <= ' ' || r == 0x7f }

// validateFetchRevision accepts the revisions that may be passed to `git fetch`
// by name: HEAD, a (possibly abbreviated) object name, or a ref name valid under
// git check-ref-format ("main", "v1.0.0", "refs/pull/1/head"). Refspec syntax is
// rejected (":" separators, a leading "+" force flag or "^" negation, "*" globs)
// as are revision expressions ("~", "^", "@{", ".."), so a revision can never
// make fetch update local refs of the shared working copy.
func validateFetchRevision(rev string) error {
	if strings.HasPrefix(rev, "+") {
		return fmt.Errorf("%w: revision %q must not start with '+'", ErrInvalidRevision, truncateForMessage(rev))
	}
	return validateRefName("revision", rev)
}

// validateRefName checks a branch or notes namespace against git's
// check-ref-format rules (as applied to a name below refs/heads/ or refs/notes/).
func validateRefName(kind, name string) error {
	bad := func(reason string) error {
		return fmt.Errorf("%w: %s %q %s", ErrInvalidRevision, kind, truncateForMessage(name), reason)
	}
	switch {
	case name == "":
		return fmt.Errorf("%w: empty %s", ErrInvalidRevision, kind)
	case len(name) > maxArgLen:
		return bad("is too long")
	case strings.HasPrefix(name, "-"):
		return bad("must not start with '-'")
	case name == "@":
		return bad("must not be '@'")
	case strings.Contains(name, ".."), strings.Contains(name, "@{"), strings.Contains(name, "//"):
		return bad("contains '..', '@{' or '//'")
	case strings.HasPrefix(name, "/"), strings.HasSuffix(name, "/"), strings.HasSuffix(name, "."):
		return bad("must not start or end with '/' or end with '.'")
	case strings.ContainsFunc(name, func(r rune) bool {
		return isControlOrSpace(r) || strings.ContainsRune("~^:?*[\\", r)
	}):
		return bad("contains a forbidden character")
	}
	for _, comp := range strings.Split(name, "/") {
		if strings.HasPrefix(comp, ".") || strings.HasSuffix(comp, ".lock") {
			return bad("has a component starting with '.' or ending with '.lock'")
		}
	}
	return nil
}

var transportHelperRe = regexp.MustCompile(`^[A-Za-z0-9+.\-]+::`)

// allowedSchemes are the URL schemes git is allowed to use. Anything else (remote
// helpers such as ext:: or fd::) is rejected before reaching git.
var allowedSchemes = map[string]bool{
	"https": true, "http": true, "ssh": true, "git": true, "file": true,
	"git+ssh": true, "ssh+git": true,
}

// validateRepoURL rejects repository URLs that cannot be passed to git safely.
func validateRepoURL(raw string) error {
	s := strings.TrimSpace(raw)
	switch {
	case s == "":
		return fmt.Errorf("%w: empty URL", ErrInvalidURL)
	case len(s) > maxArgLen:
		return fmt.Errorf("%w: URL too long", ErrInvalidURL)
	case strings.HasPrefix(s, "-"):
		return fmt.Errorf("%w: URL must not start with '-'", ErrInvalidURL)
	case strings.ContainsFunc(s, isControlOrSpace):
		return fmt.Errorf("%w: URL contains whitespace or control characters", ErrInvalidURL)
	case transportHelperRe.MatchString(s):
		return fmt.Errorf("%w: remote helper transports are not supported", ErrInvalidURL)
	}
	if i := strings.Index(s, "://"); i >= 0 {
		scheme := strings.ToLower(s[:i])
		if !allowedSchemes[scheme] {
			return fmt.Errorf("%w: unsupported scheme %q", ErrInvalidURL, truncateForMessage(scheme))
		}
		u, err := url.Parse(s)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidURL, SanitizeURL(s))
		}
		if scheme != "file" && u.Host == "" {
			return fmt.Errorf("%w: missing host", ErrInvalidURL)
		}
		return nil
	}
	if filepath.IsAbs(s) {
		return nil // local path
	}
	if ok, _ := IsSSHURL(s); ok {
		return nil
	}
	// "host:path" (scp-like without a user) is accepted by git as SSH.
	if host, _, ok := strings.Cut(s, ":"); ok && host != "" && !strings.Contains(host, "/") {
		return nil
	}
	return fmt.Errorf("%w: unrecognized URL form", ErrInvalidURL)
}

// isLocalURL reports whether raw names a repository on the local file system.
func isLocalURL(raw string) bool {
	s := strings.TrimSpace(raw)
	return hasPrefixFold(s, "file://") || filepath.IsAbs(s)
}

// stripURLPassword removes a password from an http(s)/ssh URL and returns it so
// it can be delivered through the askpass helper instead of the command line and
// the on-disk git configuration.
func stripURLPassword(raw string) (clean, user, password string) {
	s := strings.TrimSpace(raw)
	if !strings.Contains(s, "://") {
		return s, "", ""
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s, "", ""
	}
	pw, ok := u.User.Password()
	if !ok {
		return s, u.User.Username(), ""
	}
	user = u.User.Username()
	if user == "" {
		u.User = nil
	} else {
		u.User = url.User(user)
	}
	return u.String(), user, pw
}
