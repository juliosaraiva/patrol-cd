package github

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	gh "github.com/google/go-github/v88/github"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// secondaryRateLimitDefaultWait is used when GitHub reports a secondary rate limit without
// Retry-After (GitHub documents waiting at least one minute).
const secondaryRateLimitDefaultWait = time.Minute

// mapError normalizes a go-github error into *scm.Error. resp may be nil.
func mapError(op string, resp *gh.Response, err error, now time.Time) error {
	if err == nil {
		return nil
	}
	var se *scm.Error
	if errors.As(err, &se) {
		if se.Provider == "" || se.Op == "" {
			c := *se
			if c.Provider == "" {
				c.Provider = Type
			}
			if c.Op == "" {
				c.Op = op
			}
			return &c
		}
		return err
	}
	e := &scm.Error{Provider: Type, Op: op, Err: err}
	if resp != nil && resp.Response != nil {
		e.StatusCode = resp.StatusCode
	}
	var (
		rle   *gh.RateLimitError
		abuse *gh.AbuseRateLimitError
		er    *gh.ErrorResponse
		acc   *gh.AcceptedError
		redir *gh.RedirectionError
		ue    *url.Error
		ne    net.Error
		se2   *json.SyntaxError
		te    *json.UnmarshalTypeError
	)
	switch {
	case errors.Is(err, scm.ErrAttemptTimeout):
		// The server did not answer within the per-attempt timeout (after retries).
		e.Kind = scm.KindUnavailable
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		// Keep the context error recognizable; no kind so callers do not retry blindly.
		return e
	case errors.Is(err, scm.ErrResponseTooLarge):
		e.Kind = scm.KindInvalid
	case errors.Is(err, gh.ErrPathForbidden):
		e.Kind = scm.KindInvalid
	case errors.As(err, &rle):
		e.Kind = scm.KindRateLimited
		e.RetryAfter = untilReset(rle.Rate.Reset.Time, now)
		if rle.Response != nil {
			e.StatusCode = rle.Response.StatusCode
			if d, ok := scm.ParseRetryAfter(rle.Response.Header.Get("Retry-After"), now); ok && d > 0 {
				e.RetryAfter = d
			}
		}
	case errors.As(err, &abuse):
		e.Kind = scm.KindRateLimited
		e.RetryAfter = secondaryRateLimitDefaultWait
		if abuse.RetryAfter != nil && *abuse.RetryAfter > 0 {
			e.RetryAfter = *abuse.RetryAfter
		}
		if abuse.Response != nil {
			e.StatusCode = abuse.Response.StatusCode
		}
	case errors.As(err, &acc):
		e.Kind = scm.KindUnavailable
		e.StatusCode = http.StatusAccepted
		e.RetryAfter = 5 * time.Second
	case errors.As(err, &redir):
		e.Kind = scm.KindInvalid
		e.StatusCode = redir.StatusCode
	case errors.As(err, &er):
		status := 0
		if er.Response != nil {
			status = er.Response.StatusCode
		}
		e.StatusCode = status
		e.Kind = kindForStatus(status)
		msg := strings.ToLower(er.Message)
		if status == http.StatusForbidden && strings.Contains(msg, "rate limit") {
			e.Kind = scm.KindRateLimited
		}
		if e.Kind == scm.KindRateLimited {
			e.RetryAfter = secondaryRateLimitDefaultWait
			if er.Response != nil {
				if d, ok := scm.RateLimitWait(er.Response.Header, now); ok && d > 0 {
					e.RetryAfter = d
				} else if d, ok := scm.ParseRetryAfter(er.Response.Header.Get("Retry-After"), now); ok && d > 0 {
					e.RetryAfter = d
				}
			}
		}
		if e.Kind == scm.KindUnavailable && er.Response != nil {
			if d, ok := scm.ParseRetryAfter(er.Response.Header.Get("Retry-After"), now); ok {
				e.RetryAfter = d
			}
		}
	case errors.As(err, &se2) || errors.As(err, &te):
		e.Kind = scm.KindUnavailable // the server returned something that is not the documented shape
	case errors.As(err, &ue) || errors.As(err, &ne):
		e.Kind = scm.KindUnavailable
	default:
		e.Kind = scm.KindUnavailable
	}
	return e
}

func kindForStatus(status int) scm.ErrorKind {
	switch {
	case status == http.StatusNotFound || status == http.StatusGone:
		return scm.KindNotFound
	case status == http.StatusUnauthorized:
		return scm.KindUnauthorized
	case status == http.StatusForbidden:
		return scm.KindForbidden
	case status == http.StatusConflict:
		return scm.KindConflict
	case status == http.StatusTooManyRequests:
		return scm.KindRateLimited
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return scm.KindInvalid
	case status >= 500:
		return scm.KindUnavailable
	}
	return scm.KindUnknown
}

func untilReset(reset, now time.Time) time.Duration {
	if reset.IsZero() {
		return time.Minute
	}
	d := reset.Sub(now)
	if d < time.Second {
		d = time.Second
	}
	return d
}

func isStatus(resp *gh.Response, err error, codes ...int) bool {
	status := 0
	if resp != nil && resp.Response != nil {
		status = resp.StatusCode
	}
	var er *gh.ErrorResponse
	if status == 0 && errors.As(err, &er) && er.Response != nil {
		status = er.Response.StatusCode
	}
	for _, c := range codes {
		if status == c {
			return true
		}
	}
	return false
}
