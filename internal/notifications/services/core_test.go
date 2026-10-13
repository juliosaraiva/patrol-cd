package services

import (
	"encoding/json"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

func TestBackoffBounds(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		minWait := time.Duration(rapid.Int64Range(0, int64(10*time.Second)).Draw(t, "min"))
		maxWait := minWait + time.Duration(rapid.Int64Range(0, int64(time.Minute)).Draw(t, "extra"))
		p := RetryPolicy{MaxRetries: 3, WaitMin: minWait, WaitMax: maxWait}.normalized()
		attempt := rapid.IntRange(0, 50).Draw(t, "attempt")
		d := backoff(p, attempt, "")
		if d < p.WaitMin || d > p.WaitMax {
			t.Fatalf("backoff %v outside [%v, %v]", d, p.WaitMin, p.WaitMax)
		}
		ra := rapid.IntRange(0, 100000).Draw(t, "retryAfter")
		d = backoff(p, attempt, strconv.Itoa(ra))
		if d > maxRetryAfter && d > p.WaitMin {
			t.Fatalf("Retry-After backoff %v exceeds cap", d)
		}
	})
}

func TestRetryPolicyNormalized(t *testing.T) {
	t.Parallel()
	p := RetryPolicy{MaxRetries: 1000, WaitMin: -1, WaitMax: -5}.normalized()
	if p.MaxRetries != maxRetries || p.WaitMin != 0 || p.WaitMax != 0 {
		t.Fatalf("normalized %+v", p)
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		ok   bool
		want time.Duration
	}{
		{"", false, 0}, {"5", true, 5 * time.Second}, {"-1", false, 0}, {"soon", false, 0},
		{time.Now().Add(-time.Hour).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"), true, 0},
	}
	for _, tc := range tests {
		got, ok := parseRetryAfter(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("parseRetryAfter(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRedactorNeverLeaks(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		secret := rapid.StringMatching(`[A-Za-z0-9_\-+/=]{4,40}`).Draw(t, "secret")
		prefix := rapid.String().Draw(t, "prefix")
		suffix := rapid.String().Draw(t, "suffix")
		r := NewRedactor(secret)
		out := r.Redact(prefix + secret + suffix)
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q survived redaction: %q", secret, out)
		}
		// Redaction is idempotent.
		if again := r.Redact(out); again != out {
			t.Fatalf("not idempotent: %q -> %q", out, again)
		}
	})
}

func TestSanitizeURL(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"https://user:pw@example.com/p?token=abc&x=1": "https://%2A%2A%2A%2A%2A%2A@example.com/p?token=%2A%2A%2A%2A%2A%2A&x=%2A%2A%2A%2A%2A%2A",
		"https://example.com/plain":                   "https://example.com/plain",
		"not a url":                                   "not a url",
	}
	for in, want := range tests {
		if got := SanitizeURL(in); got != want {
			t.Fatalf("SanitizeURL(%q) = %q, want %q", in, got, want)
		}
	}
	r := NewRedactor()
	msg := r.Redact(`Post "https://hooks.example.com/x?key=SECRETKEY": dial tcp: refused`)
	if strings.Contains(msg, "SECRETKEY") {
		t.Fatalf("query value leaked: %q", msg)
	}
}

func TestRedactedErrorClassification(t *testing.T) {
	t.Parallel()
	cause := &StatusError{Service: "x", Method: "POST", URL: "https://h/p?t=s3cret", StatusCode: 500, Body: "boom"}
	err := redactErr(NewRedactor("s3cret"), cause)
	if !errors.Is(err, ErrDeliveryFailed) {
		t.Fatal("classification lost")
	}
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != 500 || strings.Contains(se.URL, "s3cret") {
		t.Fatalf("As: %+v", se)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("leak: %v", err)
	}
	if errors.Unwrap(err) != nil {
		t.Fatal("redacted error must not expose the raw cause via Unwrap")
	}
}

func TestFlexTypes(t *testing.T) {
	t.Parallel()
	var v struct {
		A flexString `json:"a"`
		B flexString `json:"b"`
		C flexBool   `json:"c"`
		D flexBool   `json:"d"`
	}
	if err := json.Unmarshal([]byte(`{"a": 12345678901, "b": "42", "c": "true", "d": true}`), &v); err != nil {
		t.Fatal(err)
	}
	a, _ := v.A.int64()
	b, _ := v.B.int64()
	if a != 12345678901 || b != 42 || !bool(v.C) || !bool(v.D) {
		t.Fatalf("decoded %+v", v)
	}
	if err := json.Unmarshal([]byte(`{"c": "maybe"}`), &v); err == nil {
		t.Fatal("invalid bool accepted")
	}
	if _, err := parseFlexDuration("-1s"); err == nil {
		t.Fatal("negative duration accepted")
	}
	if d, _ := parseFlexDuration("3"); d != 3*time.Second {
		t.Fatalf("bare integer seconds: %v", d)
	}
}

func TestGuardAddrAllowed(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "::1": false, "fe80::1": false, "fc00::1": false, "224.0.0.1": false,
		"8.8.8.8": true, "2606:4700:4700::1111": true, "93.184.216.34": true,
		// IPv6 forms embedding an IPv4 destination are judged by the embedded address.
		"64:ff9b::7f00:1": false, "64:ff9b::a9fe:a9fe": false, "64:ff9b::808:808": true,
		"2002:7f00:1::1": false, "2002:a00:1::1": false, "2002:808:808::1": true,
		"::ffff:127.0.0.1": false,
	}
	for ip, want := range tests {
		if got := addrAllowed(netip.MustParseAddr(ip), GuardOptions{}); got != want {
			t.Fatalf("addrAllowed(%s) = %v, want %v", ip, got, want)
		}
	}
	allow := GuardOptions{AllowedPrefixes: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, BlockedPrefixes: []netip.Prefix{netip.MustParsePrefix("8.8.8.0/24")}}
	if !addrAllowed(netip.MustParseAddr("10.9.9.9"), allow) || addrAllowed(netip.MustParseAddr("8.8.8.8"), allow) {
		t.Fatal("allow/block prefixes not honored")
	}
}

func TestNewUnsupportedType(t *testing.T) {
	t.Parallel()
	if _, err := New("carrier-pigeon", nil, testDeps()); !errors.Is(err, ErrUnsupportedServiceType) {
		t.Fatalf("got %v", err)
	}
	want := []string{"alertmanager", "awssqs", "email", "gcppubsub", "github", "googlechat", "grafana", "mattermost", "nats", "newrelic",
		"opsgenie", "pagerduty", "pagerdutyv2", "pushover", "rocketchat", "slack", "teams", "teams-workflows", "telegram", "webex", "webhook"}
	if got := SupportedTypes(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("SupportedTypes = %v", got)
	}
}

func TestOptionDecodingErrorsAreRedacted(t *testing.T) {
	t.Parallel()
	deps := testDeps()
	deps.SensitiveValues = []string{"topsecretvalue"}
	_, err := New(TypeSlack, []byte(`{"token": {"nested": "topsecretvalue"}}`), deps)
	if err == nil || strings.Contains(err.Error(), "topsecretvalue") || !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("got %v", err)
	}
}

func TestConsole(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	svc := NewConsole(&buf)
	mustSend(t, svc, Notification{Message: "hello"}, Destination{Service: TypeConsole, Recipient: "stdout"})
	mustSend(t, svc, Notification{Message: "with email", Email: &EmailNotification{Subject: "s"}}, Destination{})
	want := "hello\nwith email\n---\nemail:\n  subject: s\n"
	if buf.String() != want {
		t.Fatalf("console output %q, want %q", buf.String(), want)
	}
}
