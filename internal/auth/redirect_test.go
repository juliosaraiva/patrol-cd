package auth

import (
	"errors"
	"net/url"
	"path"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestValidateRedirectURL(t *testing.T) {
	t.Parallel()
	allowed := []string{
		"https://patrol.example.com",
		"https://patrol.example.com:8443/sub",
		"http://localhost:8080/",
		"::not a url::",
	}
	tests := []struct {
		url  string
		want bool
	}{
		// Upstream-compatible accepts.
		{"", true},
		{"https://patrol.example.com", true},
		{"https://patrol.example.com/", true},
		{"https://patrol.example.com/applications?view=tree#frag", true},
		{"https://patrol.example.com:8443/sub", true},
		{"https://patrol.example.com:8443/sub/", true},
		{"https://patrol.example.com:8443/sub/applications", true},
		{"http://localhost:8080/applications", true},
		{"https://PATROL.example.com/x", true},
		// Upstream-compatible rejects.
		{"http://patrol.example.com/", false},            // scheme mismatch
		{"https://patrol.example.com:443/", false},       // host:port must match textually
		{"https://evil.example.com/", false},             // other host
		{"https://patrol.example.com.evil.com/", false},  // suffix trick
		{"https://patrol.example.com:8443/other", false}, // outside base path
		{"https://patrol.example.com:8443/", false},      // above base path
		{"https://patrol.example.com:8443/sub/../other", false},
		{"https://patrol.example.com:8443/sub/%2e%2e/other", false},
		{"https://patrol.example.com/%0d%0aSet-Cookie:x=y", false}, // CRLF in path
		{"/applications", false},                                   // relative URLs are not accepted (upstream)
		{"//evil.example.com/", false},                             // scheme-relative
		// Hardening beyond upstream.
		{"https://patrol.example.com:8443/subx", false}, // segment boundary
		{"https://patrol.example.com@evil.example.com/", false},
		{"https://user:pw@patrol.example.com/", false},
		{"https://evil.example.com\\@patrol.example.com/", false},
		{"https:patrol.example.com", false}, // opaque
		{"javascript:alert(1)", false},
		{"ftp://patrol.example.com/", false},
		{" https://patrol.example.com/", false},
		{"https://patrol.example.com/\tx", false},
		{"https://patrol.example.com/a b", false},
		{"https://patrol.example.com/" + strings.Repeat("a", 2100), false}, // too long
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			t.Parallel()
			err := ValidateRedirectURL(tc.url, allowed)
			if got := err == nil; got != tc.want {
				t.Fatalf("ValidateRedirectURL(%q) = %v, want valid=%v", tc.url, err, tc.want)
			}
			if err != nil && !errors.Is(err, ErrInvalidRedirectURL) {
				t.Fatalf("error does not wrap ErrInvalidRedirectURL: %v", err)
			}
			if IsValidRedirectURL(tc.url, allowed) != tc.want {
				t.Fatal("IsValidRedirectURL disagrees")
			}
		})
	}
	if IsValidRedirectURL("https://patrol.example.com/", nil) {
		t.Fatal("no allowed URLs must reject every non-empty URL")
	}
}

// Property: a redirect to any host other than the allowed one is rejected, whatever the
// path, query or fragment.
func TestRedirectForeignHostProperty(t *testing.T) {
	t.Parallel()
	allowed := []string{"https://patrol.example.com/root"}
	rapid.Check(t, func(rt *rapid.T) {
		host := rapid.StringMatching(`[a-z0-9-]{1,20}(\.[a-z]{2,5}){0,3}(:[0-9]{1,5})?`).Draw(rt, "host")
		if host == "patrol.example.com" {
			rt.Skip("allowed host")
		}
		path := rapid.StringMatching(`(/[A-Za-z0-9._~%@!$&'()*+,;=:-]{0,10}){0,4}`).Draw(rt, "path")
		u := rapid.SampledFrom([]string{"https://", "http://", "//", ""}).Draw(rt, "scheme") + host + path
		if IsValidRedirectURL(u, allowed) {
			rt.Fatalf("foreign redirect accepted: %q", u)
		}
		// Same host: only paths under /root may pass.
		same := "https://patrol.example.com" + path
		if IsValidRedirectURL(same, allowed) {
			if p := cleanedPath(same); p != "/root" && !hasPrefixSegment(p, "/root") {
				rt.Fatalf("path outside base accepted: %q (%q)", same, p)
			}
		}
	})
}

func cleanedPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Path == "" {
		return "/"
	}
	return path.Clean(u.Path)
}

func hasPrefixSegment(p, base string) bool { return strings.HasPrefix(p, base+"/") }
