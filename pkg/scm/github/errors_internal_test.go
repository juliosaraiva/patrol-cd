package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	gh "github.com/google/go-github/v88/github"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

func TestMapError(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)
	mkResp := func(status int, kv ...string) *http.Response {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return &http.Response{StatusCode: status, Header: h, Request: req}
	}
	retry := 7 * time.Second
	var syntaxErr *json.SyntaxError
	if err := json.Unmarshal([]byte("{x"), &struct{}{}); !errors.As(err, &syntaxErr) {
		t.Fatal("could not build a json.SyntaxError")
	}
	tests := []struct {
		name       string
		err        error
		kind       scm.ErrorKind
		retryAfter time.Duration
		status     int
	}{
		{"primary rate limit", &gh.RateLimitError{Rate: gh.Rate{Reset: gh.Timestamp{Time: now.Add(10 * time.Minute)}}, Response: mkResp(403)}, scm.KindRateLimited, 10 * time.Minute, 403},
		{"primary rate limit, reset passed", &gh.RateLimitError{Rate: gh.Rate{Reset: gh.Timestamp{Time: now.Add(-time.Minute)}}, Response: mkResp(429)}, scm.KindRateLimited, time.Second, 429},
		{"secondary rate limit with retry-after", &gh.AbuseRateLimitError{Response: mkResp(403), RetryAfter: &retry}, scm.KindRateLimited, retry, 403},
		{"secondary rate limit without retry-after", &gh.AbuseRateLimitError{Response: mkResp(403)}, scm.KindRateLimited, time.Minute, 403},
		{"403 secondary by message", &gh.ErrorResponse{Response: mkResp(403, "Retry-After", "30"), Message: "You have exceeded a secondary rate limit"}, scm.KindRateLimited, 30 * time.Second, 403},
		{"429 plain", &gh.ErrorResponse{Response: mkResp(429, "Retry-After", "4"), Message: "Too Many Requests"}, scm.KindRateLimited, 4 * time.Second, 429},
		{"429 with reset", &gh.ErrorResponse{Response: mkResp(429, "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", strconv.FormatInt(now.Add(2*time.Minute).Unix(), 10))}, scm.KindRateLimited, 2 * time.Minute, 429},
		{"403 permission", &gh.ErrorResponse{Response: mkResp(403), Message: "Resource not accessible by integration"}, scm.KindForbidden, 0, 403},
		{"503 retry-after", &gh.ErrorResponse{Response: mkResp(503, "Retry-After", "3")}, scm.KindUnavailable, 3 * time.Second, 503},
		{"202 accepted", &gh.AcceptedError{}, scm.KindUnavailable, 5 * time.Second, 202},
		{"redirect", &gh.RedirectionError{StatusCode: 301}, scm.KindInvalid, 0, 301},
		{"path traversal", fmt.Errorf("x: %w", gh.ErrPathForbidden), scm.KindInvalid, 0, 0},
		{"size cap", &url.Error{Op: "Get", URL: "u", Err: scm.ErrResponseTooLarge}, scm.KindInvalid, 0, 0},
		{"network", &url.Error{Op: "Get", URL: "u", Err: errors.New("connection refused")}, scm.KindUnavailable, 0, 0},
		{"attempt timeout", &url.Error{Op: "Get", URL: "u", Err: fmt.Errorf("attempt: %w", scm.ErrAttemptTimeout)}, scm.KindUnavailable, 0, 0},
		{"bad json", syntaxErr, scm.KindUnavailable, 0, 0},
		{"canceled", context.Canceled, scm.KindUnknown, 0, 0},
	}
	for _, tc := range tests {
		err := mapError("Op", nil, tc.err, now)
		var se *scm.Error
		if !errors.As(err, &se) {
			t.Fatalf("%s: not an *scm.Error: %v", tc.name, err)
		}
		if se.Kind != tc.kind || se.RetryAfter != tc.retryAfter || se.StatusCode != tc.status || se.Provider != Type || se.Op != "Op" {
			t.Errorf("%s: got kind=%q retry=%v status=%d, want %q %v %d", tc.name, se.Kind, se.RetryAfter, se.StatusCode, tc.kind, tc.retryAfter, tc.status)
		}
		if !errors.Is(err, tc.err) {
			t.Errorf("%s: cause not preserved", tc.name)
		}
	}
	if mapError("Op", nil, nil, now) != nil {
		t.Fatal("nil error must map to nil")
	}
	inner := &scm.Error{Kind: scm.KindNotFound}
	if got := mapError("Op", nil, inner, now).(*scm.Error); got.Provider != Type || got.Op != "Op" || got.Kind != scm.KindNotFound || inner.Provider != "" {
		t.Fatalf("existing scm.Error: %+v (input mutated: %v)", got, inner.Provider != "")
	}
}
