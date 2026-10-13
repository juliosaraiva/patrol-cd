package scm

import (
	"net/http"
	"net/url"
	"strings"
)

const redacted = "REDACTED"

// sensitiveQueryKeys are query parameters whose values are always redacted.
var sensitiveQueryKeys = []string{
	"access_token", "token", "private_token", "client_secret", "secret", "password",
	"sig", "signature", "x-amz-signature", "x-amz-credential", "x-amz-security-token", "code", "jwt", "key",
}

// SensitiveHeaders are never logged by RedactHeaders.
var SensitiveHeaders = []string{
	"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie",
	"Private-Token", "Job-Token", "X-Hub-Signature", "X-Hub-Signature-256", "X-Gitlab-Token",
	"X-Gitea-Signature", "X-Gogs-Signature", "X-Fake-Signature",
}

// RedactURL renders u for logs: user info is dropped and sensitive query values are
// replaced. A nil URL renders as "".
func RedactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	if c.User != nil {
		c.User = url.User(redacted)
	}
	if c.RawQuery != "" {
		q := c.Query()
		for k := range q {
			if isSensitiveKey(k) {
				q[k] = []string{redacted}
			}
		}
		c.RawQuery = q.Encode()
	}
	c.Fragment = ""
	c.RawFragment = ""
	return c.String()
}

func isSensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	for _, s := range sensitiveQueryKeys {
		if lk == s {
			return true
		}
	}
	return strings.Contains(lk, "token") || strings.Contains(lk, "secret") || strings.Contains(lk, "password")
}

// RedactURLString parses and redacts s; unparsable input is fully redacted.
func RedactURLString(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return redacted
	}
	return RedactURL(u)
}

// RedactHeaders returns a copy of h with sensitive header values replaced.
func RedactHeaders(h http.Header) http.Header {
	out := h.Clone()
	if out == nil {
		return http.Header{}
	}
	for _, k := range SensitiveHeaders {
		if _, ok := out[http.CanonicalHeaderKey(k)]; ok {
			out[http.CanonicalHeaderKey(k)] = []string{redacted}
		}
	}
	return out
}
