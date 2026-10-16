package operation

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Retry defaults (upstream DefaultSyncRetryDuration/Factor/MaxDuration).
const (
	DefaultRetryDuration    = 5 * time.Second
	DefaultRetryFactor      = int64(2)
	DefaultRetryMaxDuration = 3 * time.Minute
	// DefaultAutomatedRetryLimit is the retry limit of automated syncs when the Application
	// has no syncPolicy.retry (SYNC-024).
	DefaultAutomatedRetryLimit = 5

	// maxRetryWait bounds a computed wait (unlimited retries with factor > 1 overflow).
	maxRetryWait = 100 * 365 * 24 * time.Hour
)

// EffectiveRetry returns the retry strategy of an Operation: spec.retry, else the
// Application's syncPolicy.retry, else nil (no retries).
func EffectiveRetry(op *v1alpha1.Operation, app *v1alpha1.Application) *v1alpha1.RetryStrategy {
	if op.Spec.Retry != nil {
		return op.Spec.Retry
	}
	if app != nil && app.Spec.SyncPolicy != nil && app.Spec.SyncPolicy.Retry != nil {
		return app.Spec.SyncPolicy.Retry
	}
	return nil
}

// RetryAllowed reports whether another attempt may follow retryCount completed retries
// (SYNC-026): limit < 0 retries forever, 0 never.
func RetryAllowed(r *v1alpha1.RetryStrategy, retryCount int64) bool {
	if r == nil {
		return false
	}
	return r.Limit < 0 || retryCount < r.Limit
}

// NextRetryAt returns when the attempt following retryCount retries may start, measured from
// lastAttempt (SYNC-027): duration × factor^retryCount, capped at maxDuration when it is
// positive. Durations are seconds ("10") or Go durations ("10s", "2m").
func NextRetryAt(r *v1alpha1.RetryStrategy, lastAttempt time.Time, retryCount int64) (time.Time, error) {
	duration, factor, maxDuration := DefaultRetryDuration, DefaultRetryFactor, DefaultRetryMaxDuration
	if r != nil && r.Backoff != nil {
		var err error
		if r.Backoff.Duration != "" {
			if duration, err = ParseRetryDuration(r.Backoff.Duration); err != nil {
				return time.Time{}, err
			}
		}
		if r.Backoff.MaxDuration != "" {
			if maxDuration, err = ParseRetryDuration(r.Backoff.MaxDuration); err != nil {
				return time.Time{}, err
			}
		}
		if r.Backoff.Factor != nil {
			factor = *r.Backoff.Factor
		}
	}
	wait := float64(duration) * math.Pow(float64(factor), float64(retryCount))
	if maxDuration > 0 {
		wait = math.Min(wait, float64(maxDuration))
	}
	if math.IsNaN(wait) || wait > float64(maxRetryWait) {
		wait = float64(maxRetryWait)
	}
	return lastAttempt.Add(time.Duration(wait)), nil
}

// ParseRetryDuration parses a backoff duration: an integer number of seconds or a Go
// duration string.
func ParseRetryDuration(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("unable to parse %s as a duration", s)
	}
	return d, nil
}
