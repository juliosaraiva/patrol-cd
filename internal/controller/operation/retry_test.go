package operation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func TestNextRetryAt(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		retry   *v1alpha1.RetryStrategy
		count   int64
		want    time.Duration
		wantErr string
	}{
		{"defaults first", nil, 0, 5 * time.Second, ""},
		{"defaults second", nil, 1, 10 * time.Second, ""},
		{"defaults capped", nil, 10, 3 * time.Minute, ""},
		{"numeric seconds", &v1alpha1.RetryStrategy{Backoff: &v1alpha1.Backoff{Duration: "10"}}, 0, 10 * time.Second, ""},
		{"go duration and factor", &v1alpha1.RetryStrategy{Backoff: &v1alpha1.Backoff{Duration: "1s", Factor: ptr.To[int64](3)}}, 2, 9 * time.Second, ""},
		{"max duration", &v1alpha1.RetryStrategy{Backoff: &v1alpha1.Backoff{Duration: "1m", MaxDuration: "90"}}, 1, 90 * time.Second, ""},
		{"zero max duration disables the cap", &v1alpha1.RetryStrategy{Backoff: &v1alpha1.Backoff{Duration: "1h", MaxDuration: "0"}}, 1, 2 * time.Hour, ""},
		{"invalid duration", &v1alpha1.RetryStrategy{Backoff: &v1alpha1.Backoff{Duration: "abc"}}, 0, 0, "unable to parse abc as a duration"},
		{"invalid max duration", &v1alpha1.RetryStrategy{Backoff: &v1alpha1.Backoff{MaxDuration: "1x"}}, 0, 0, "unable to parse 1x as a duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := NextRetryAt(tc.retry, base, tc.count)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Sub(base))
		})
	}
}

func TestRetryAllowed(t *testing.T) {
	t.Parallel()
	assert.False(t, RetryAllowed(nil, 0))
	assert.False(t, RetryAllowed(&v1alpha1.RetryStrategy{Limit: 0}, 0))
	assert.True(t, RetryAllowed(&v1alpha1.RetryStrategy{Limit: 2}, 1))
	assert.False(t, RetryAllowed(&v1alpha1.RetryStrategy{Limit: 2}, 2))
	assert.True(t, RetryAllowed(&v1alpha1.RetryStrategy{Limit: -1}, 1000))
}

func TestEffectiveRetry(t *testing.T) {
	t.Parallel()
	appRetry := &v1alpha1.RetryStrategy{Limit: 3}
	app := &v1alpha1.Application{Spec: v1alpha1.ApplicationSpec{SyncPolicy: &v1alpha1.SyncPolicy{Retry: appRetry}}}
	opRetry := &v1alpha1.RetryStrategy{Limit: 1}
	assert.Same(t, opRetry, EffectiveRetry(&v1alpha1.Operation{Spec: v1alpha1.OperationSpec{Retry: opRetry}}, app))
	assert.Same(t, appRetry, EffectiveRetry(&v1alpha1.Operation{}, app))
	assert.Nil(t, EffectiveRetry(&v1alpha1.Operation{}, &v1alpha1.Application{}))
}

// TestNextRetryAtMonotonic: waits never shrink with the attempt count and never exceed a
// positive maxDuration.
func TestNextRetryAtMonotonic(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		d := rapid.IntRange(1, 600).Draw(t, "duration")
		f := rapid.Int64Range(1, 5).Draw(t, "factor")
		m := rapid.IntRange(0, 3600).Draw(t, "max")
		r := &v1alpha1.RetryStrategy{Backoff: &v1alpha1.Backoff{Duration: time.Duration(d * int(time.Second)).String(), Factor: &f, MaxDuration: time.Duration(m * int(time.Second)).String()}}
		base := time.Unix(0, 0)
		prev := time.Duration(0)
		for n := int64(0); n < 40; n++ {
			at, err := NextRetryAt(r, base, n)
			if err != nil {
				t.Fatal(err)
			}
			w := at.Sub(base)
			if w < prev {
				t.Fatalf("wait shrank at attempt %d: %v < %v", n, w, prev)
			}
			if m > 0 && w > time.Duration(m)*time.Second {
				t.Fatalf("wait %v exceeds max %ds", w, m)
			}
			prev = w
		}
	})
}
