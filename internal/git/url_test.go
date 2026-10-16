package git

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"pgregory.net/rapid"
)

func TestIsSSHURL(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		ssh  bool
		user string
	}{
		"git://github.com/argoproj/test.git":          {false, ""},
		"git@GITHUB.com:argoproj/test.git":            {true, "git"},
		"git@github.com:test":                         {true, "git"},
		"https://github.com/argoproj/test":            {false, ""},
		"https://user@github.com/argoproj/test.git":   {false, ""},
		"ssh://git@GITHUB.com:argoproj/test":          {true, ""},
		"ssh://git@github.com/test.git":               {true, "git"},
		"ssh://john@john-server.org:29418/project":    {true, "john"},
		"john@john-server.org:29418/project":          {true, "john"},
		"john@doe.org@john-server.org:29418/project":  {true, "john@doe.org"},
		"ssh://john@doe.org@john-server.org:29418/pr": {true, "john@doe.org"},
		"github.com:org/repo":                         {false, ""},
	}
	for in, want := range tests {
		ok, user := IsSSHURL(in)
		assert.Equal(t, want.ssh, ok, in)
		if want.user != "" {
			assert.Equal(t, want.user, user, in)
		}
	}
}

func TestIsHTTPSURL(t *testing.T) {
	t.Parallel()
	assert.True(t, IsHTTPSURL("https://github.com/x"))
	assert.True(t, IsHTTPSURL("HTTPS://github.com/x"))
	assert.False(t, IsHTTPSURL("http://github.com/x"))
	assert.True(t, IsHTTPURL("http://github.com/x"))
	assert.False(t, IsHTTPSURL("git@github.com:x"))
}

func TestSameURLUpstreamCases(t *testing.T) {
	t.Parallel()
	equal := map[string]string{
		"git@GITHUB.com:argoproj/test":                     "git@github.com:argoproj/test.git",
		"git@GITHUB.com:test.git":                          "git@github.com:test.git",
		"https://GITHUB.com/argoproj/test":                 "https://github.com/argoproj/test.git",
		"https://github.com/FOO":                           "https://github.com/foo",
		"https://github.com:4443/TEST.git":                 "https://github.com:4443/TEST",
		"ssh://git@GITHUB.com/argoproj/test":               "git@github.com:argoproj/test.git",
		"ssh://git@github.com/test":                        "git@github.com:test.git",
		" https://github.com/argoproj/test ":               "https://github.com/argoproj/test.git",
		"\thttps://github.com/argoproj/test\n":             "https://github.com/argoproj/test.git",
		"https://1234.visualstudio.com/myproj/_git/myrepo": "https://1234.visualstudio.com/myproj/_git/myrepo",
		"https://github.com:443/argoproj/test":             "https://github.com/argoproj/test",
		"https://user:pw@github.com/argoproj/test":         "https://github.com/argoproj/test",
		"ssh://git@github.com:22/argoproj/test":            "git@github.com:argoproj/test",
		"ssh://git@GITHUB.com:argoproj/test":               "ssh://git@github.com:argoproj/test",
	}
	for a, b := range equal {
		assert.True(t, SameURL(a, b), "%q vs %q", a, b)
		assert.True(t, SameURL(b, a), "%q vs %q", b, a)
	}
	different := [][2]string{
		{"https://github.com/argoproj/test", "https://github.com/argoproj/test2"},
		{"https://github.com/argoproj/test", "https://gitlab.com/argoproj/test"},
		{"https://github.com:4443/x", "https://github.com/x"},
		{"git@github.com:org/repo", "deploy@github.com:org/repo"},
		{"https://github.com/org/repo", ""},
		{"ssh://git@github.com:2222/a/b.git", "git@github.com:a/b.git"},
	}
	for _, d := range different {
		assert.False(t, SameURL(d[0], d[1]), "%q vs %q", d[0], d[1])
	}
}

func TestRepoKey(t *testing.T) {
	t.Parallel()
	// Spellings of one location share a key.
	sameKey := map[string]string{
		"git@GITHUB.com:argoproj/test":            "ssh://git@github.com/argoproj/test",
		"ssh://git@github.com:22/argoproj/test":   "git@github.com:argoproj/test",
		"HTTPS://GitHub.com:443/Org/Repo.git/":    "https://github.com/Org/Repo.git",
		"https://user:pw@github.com/argoproj/x":   "https://github.com/argoproj/x",
		" https://github.com/argoproj/test/ ":     "https://github.com/argoproj/test",
		"https://example.com/x#frag":              "https://example.com/x",
		"ssh://git:secret@example.com:2222/x.git": "ssh://git@example.com:2222/x.git",
		"file:///srv/repos/x.git/":                "file:///srv/repos/x.git",
	}
	for a, b := range sameKey {
		assert.Equal(t, RepoKey(a), RepoKey(b), "%q vs %q", a, b)
		assert.True(t, SameURL(a, b), "%q vs %q", a, b)
	}
	// Locations a server may treat as different repositories get different keys,
	// although upstream's SameURL considers them equal.
	differentKey := [][2]string{
		{"https://github.com/argoproj/test", "https://github.com/argoproj/test.git"},
		{"https://github.com/FOO", "https://github.com/foo"},
		{"git@github.com:org/Repo", "git@github.com:org/repo"},
		{"file:///srv/repos/x", "file:///srv/repos/x.git"},
		{"https://example.com/x.git.git", "https://example.com/x.git"},
	}
	for _, d := range differentKey {
		assert.NotEqual(t, RepoKey(d[0]), RepoKey(d[1]), "%q vs %q", d[0], d[1])
		assert.True(t, SameURL(d[0], d[1]), "%q vs %q", d[0], d[1])
		assert.Equal(t, sameURLKey(d[0]), sameURLKey(d[1]), "invalidation still groups %q and %q", d[0], d[1])
	}
	assert.NotEqual(t, RepoKey("git@github.com:org/repo"), RepoKey("deploy@github.com:org/repo"))
}

func TestNormalizeURL(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"git@GitHub.com:Org/Repo.git":             "ssh://git@github.com/Org/Repo",
		"HTTPS://GitHub.com:443/Org/Repo.git/":    "https://github.com/Org/Repo",
		"https://u:p@example.com/x.git.git":       "https://example.com/x",
		"ssh://git:secret@example.com:2222/x.git": "ssh://git@example.com:2222/x",
		"file:///srv/repos/x.git":                 "file:///srv/repos/x",
		"https://example.com/x#frag":              "https://example.com/x",
		"":                                        "",
		"https://example.com/\x00":                "",
		"? #":                                     "",
		"https://example.com/x?a=1 #f":            "https://example.com/x?a=1",
	}
	for in, want := range tests {
		assert.Equal(t, want, NormalizeURL(in), "%q", in)
	}
}

func TestSanitizeURL(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "https://user:******@example.com/x", SanitizeURL("https://user:p%40ss@example.com/x"))
	assert.Equal(t, "https://example.com/x", SanitizeURL("https://example.com/x"))
	assert.Equal(t, "git@example.com:x", SanitizeURL("git@example.com:x"))
}

func TestSSHHostWithPort(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"git@github.com:argoproj/test.git":            "github.com:22",
		"ssh://git@github.com/argoproj/test.git":      "github.com:22",
		"ssh://git@github.com:2222/argoproj/test.git": "github.com:2222",
		"ssh://john@john-server.org:29418/project":    "john-server.org:29418",
		"https://github.com/argoproj/test":            "",
		"":                                            "",
	}
	for in, want := range tests {
		assert.Equal(t, want, SSHHostWithPort(in), "%q", in)
	}
}

func TestCommitSHAHelpers(t *testing.T) {
	t.Parallel()
	assert.True(t, IsCommitSHA(strings.Repeat("a", 40)))
	assert.True(t, IsCommitSHA(strings.Repeat("B", 64)))
	assert.False(t, IsCommitSHA(strings.Repeat("a", 39)))
	assert.False(t, IsCommitSHA(strings.Repeat("g", 40)))
	assert.True(t, IsTruncatedCommitSHA("abcdef1"))
	assert.False(t, IsTruncatedCommitSHA("abcde"))
	assert.False(t, IsTruncatedCommitSHA("main"))
}

// urlParts generates the components of a repository location.
type urlParts struct {
	host, user, password string
	segments             []string
}

func genURLParts(t *rapid.T) urlParts {
	return urlParts{
		host:     rapid.StringMatching(`[a-z0-9]{1,8}(\.[a-z]{2,4}){0,2}`).Draw(t, "host"),
		user:     rapid.StringMatching(`[a-z][a-z0-9_]{0,6}`).Draw(t, "user"),
		password: rapid.StringMatching(`[A-Za-z0-9!$%&*+=?^~@:/]{3,16}`).Draw(t, "password"),
		segments: rapid.SliceOfN(rapid.StringMatching(`[A-Za-z0-9_][A-Za-z0-9_.\-]{0,8}`), 1, 3).Draw(t, "segments"),
	}
}

func TestNormalizeURLProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		p := genURLParts(t)
		path := strings.Join(p.segments, "/")
		hostCase := rapid.SampledFrom([]func(string) string{strings.ToLower, strings.ToUpper}).Draw(t, "hostCase")
		suffix := rapid.SampledFrom([]string{"", ".git", "/", ".git/"}).Draw(t, "suffix")

		// Equivalent spellings of the same SSH location.
		gitSuffix := strings.Contains(suffix, ".git")
		scp := p.user + "@" + hostCase(p.host) + ":" + path + suffix
		sshForm := "ssh://" + p.user + "@" + p.host + "/" + path
		sshPort := "ssh://" + p.user + "@" + p.host + ":22/" + path + ".git"
		for _, form := range []string{scp, sshForm, sshPort} {
			n := NormalizeURL(form)
			if n == "" {
				t.Fatalf("NormalizeURL(%q) failed", form)
			}
			if NormalizeURL(n) != n {
				t.Fatalf("not idempotent: %q -> %q -> %q", form, n, NormalizeURL(n))
			}
			if !SameURL(form, scp) {
				t.Fatalf("%q and %q should be the same location", form, scp)
			}
			// RepoKey keeps the ".git" suffix: keys match exactly when both
			// spellings agree on it.
			formGit := form == sshPort || (form == scp && gitSuffix)
			if wantSame := formGit == gitSuffix; (RepoKey(form) == RepoKey(scp)) != wantSame {
				t.Fatalf("RepoKey(%q) == RepoKey(%q) is %v, want %v", form, scp, !wantSame, wantSame)
			}
		}
		if RepoKey(sshForm) != RepoKey(scp+"/") && !gitSuffix {
			t.Fatalf("trailing slash changed the key of %q", scp)
		}
		// Path case is preserved by RepoKey (but ignored by SameURL).
		if upper := strings.ToUpper(path); upper != path {
			flipped := "ssh://" + p.user + "@" + p.host + "/" + upper
			if !SameURL(flipped, sshForm) || RepoKey(flipped) == RepoKey(sshForm) {
				t.Fatalf("%q vs %q: want SameURL and distinct RepoKey", flipped, sshForm)
			}
		}

		// HTTPS with credentials: equivalent to the bare URL, never leaks the password.
		u := url.URL{Scheme: "https", Host: hostCase(p.host), Path: "/" + path + suffix, User: url.UserPassword(p.user, p.password)}
		withCreds := u.String()
		plain := "https://" + p.host + "/" + path
		n := NormalizeURL(withCreds)
		if strings.Contains(n, "@") {
			t.Fatalf("NormalizeURL(%q) = %q keeps credentials", withCreds, n)
		}
		if !SameURL(withCreds, plain) || (RepoKey(withCreds) == RepoKey(plain)) == gitSuffix {
			t.Fatalf("%q and %q should be the same location", withCreds, plain)
		}
		su, err := url.Parse(SanitizeURL(withCreds))
		if err != nil {
			t.Fatalf("SanitizeURL(%q) is not a URL: %v", withCreds, err)
		}
		if pw, _ := su.User.Password(); pw != "******" || su.User.Username() != p.user {
			t.Fatalf("SanitizeURL(%q) = %q", withCreds, su)
		}
		clean, user, pw := stripURLPassword(withCreds)
		cu, err := url.Parse(clean)
		if err != nil || pw != p.password || user != p.user {
			t.Fatalf("stripURLPassword(%q) = %q, %q, %q", withCreds, clean, user, pw)
		}
		if _, has := cu.User.Password(); has || cu.User.Username() != p.user {
			t.Fatalf("stripURLPassword(%q) kept the password: %q", withCreds, clean)
		}

		// A different path is a different location.
		other := "https://" + p.host + "/" + path + "x"
		if SameURL(plain, other) {
			t.Fatalf("%q and %q must differ", plain, other)
		}
	})
}

func TestSameURLArbitraryInputProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		a := rapid.String().Draw(t, "a")
		b := rapid.String().Draw(t, "b")
		if SameURL(a, b) != SameURL(b, a) {
			t.Fatalf("SameURL not symmetric for %q, %q", a, b)
		}
		if strings.TrimSpace(a) != "" && !SameURL(a, a) {
			t.Fatalf("SameURL not reflexive for %q", a)
		}
		if n := NormalizeURL(a); n != "" && NormalizeURL(n) != n {
			t.Fatalf("not idempotent: %q -> %q -> %q", a, n, NormalizeURL(n))
		}
		if k := RepoKey(a); len(k) != 40 {
			t.Fatalf("RepoKey(%q) = %q", a, k)
		}
		// Keys are finer than SameURL: equal keys imply the same URL.
		if strings.TrimSpace(a) != "" && RepoKey(a) == RepoKey(b) && !SameURL(a, b) {
			t.Fatalf("RepoKey(%q) == RepoKey(%q) but SameURL is false", a, b)
		}
	})
}
