package diff

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// These tests cover cancellation of the caller's context while normalization runs. An ignore
// rule that cannot be evaluated because the context is done must fail the operation: skipping
// it would report a field the user told Patrol to ignore as drift (and could trigger an
// auto-sync that overwrites it).

const (
	cancelConfigMapConfig = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","namespace":"shop"},"data":{"k":"desired","keep":"same"}}`
	cancelConfigMapLive   = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","namespace":"shop"},"data":{"k":"changed-by-operator","keep":"same"}}`
)

var cancelIgnoreRules = []IgnoreRule{{Group: "", Kind: "ConfigMap", JQPathExpressions: []string{".data.k"}}}

// cancellingNormalizer cancels the context on its first call (from any goroutine) and then
// behaves like a normalizer that ignores the context and reports success.
type cancellingNormalizer struct {
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancellingNormalizer) Normalize(context.Context, *unstructured.Unstructured) error {
	c.once.Do(c.cancel)
	return nil
}

func TestJQPatchParentContext(t *testing.T) {
	t.Parallel()
	loop, err := compileJQPatch(`until(true==false; [.] + [1])`, time.Minute)
	require.NoError(t, err)
	simple, err := compileJQPatch(`.a`, time.Minute)
	require.NoError(t, err)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := []struct {
		name  string
		patch jqPatch
		ctx   func(t *testing.T) context.Context
		want  error
	}{
		{"cancelled before evaluation", simple, func(*testing.T) context.Context { return cancelled }, context.Canceled},
		{"parent deadline already passed", simple, func(*testing.T) context.Context { return expired }, context.DeadlineExceeded},
		{"parent deadline hit while evaluating", loop, func(t *testing.T) context.Context {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			t.Cleanup(cancel)
			return ctx
		}, context.DeadlineExceeded},
		{"parent cancelled while evaluating", loop, func(t *testing.T) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			time.AfterFunc(50*time.Millisecond, cancel)
			return ctx
		}, context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			_, err := tt.patch.apply(tt.ctx(t), map[string]any{"a": int64(1)})
			require.ErrorIs(t, err, tt.want)
			assert.NotContains(t, err.Error(), "jq patch execution timed out",
				"a parent deadline is not the per-expression timeout")
			assert.Less(t, time.Since(start), 30*time.Second)
		})
	}

	t.Run("per-expression timeout is not a context error", func(t *testing.T) {
		t.Parallel()
		short, err := compileJQPatch(`until(true==false; [.] + [1])`, 50*time.Millisecond)
		require.NoError(t, err)
		_, err = short.apply(context.Background(), map[string]any{})
		require.ErrorContains(t, err, "jq patch execution timed out")
		assert.NotErrorIs(t, err, context.DeadlineExceeded)
		assert.NotErrorIs(t, err, context.Canceled)
	})
}

func TestIgnoreNormalizerContextDone(t *testing.T) {
	t.Parallel()
	n, err := NewIgnoreNormalizer(cancelIgnoreRules, IgnoreNormalizerOptions{})
	require.NoError(t, err)

	t.Run("cancelled before Normalize", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := n.Normalize(ctx, obj(t, cancelConfigMapLive))
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("cancelled by a preceding normalizer in a chain", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := Chain(&cancellingNormalizer{cancel: cancel}, n).Normalize(ctx, obj(t, cancelConfigMapLive))
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("parent deadline exceeded", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		err := n.Normalize(ctx, obj(t, cancelConfigMapLive))
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
	t.Run("pre-cancelled context fails even when no rule matches", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		svc := obj(t, `{"apiVersion":"v1","kind":"Service","metadata":{"name":"s"}}`)
		require.ErrorIs(t, n.Normalize(ctx, svc), context.Canceled)
	})
	t.Run("live context still skips failing patches", func(t *testing.T) {
		t.Parallel()
		bad, err := NewIgnoreNormalizer([]IgnoreRule{{Kind: "ConfigMap", JQPathExpressions: []string{".data.k[]"}}}, IgnoreNormalizerOptions{})
		require.NoError(t, err)
		u := obj(t, cancelConfigMapLive)
		require.NoError(t, bad.Normalize(context.Background(), u))
		assert.Equal(t, obj(t, cancelConfigMapLive), u)
	})
}

func TestChainStopsWhenContextDone(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := false
	after := NormalizerFunc(func(context.Context, *unstructured.Unstructured) error {
		called = true
		return nil
	})
	err := Chain(&cancellingNormalizer{cancel: cancel}, after).Normalize(ctx, obj(t, cancelConfigMapLive))
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, called, "normalizers after cancellation are not run")
}

// TestDiffContextCancelledDuringNormalization reproduces the false-drift report: the context
// is cancelled while the user normalizer runs, for every diff strategy. Diff must return the
// context error and no Result.
func TestDiffContextCancelledDuringNormalization(t *testing.T) {
	t.Parallel()
	configMapLiveWithLastApplied := func(t *testing.T) *unstructured.Unstructured {
		return withLastApplied(t, obj(t, cancelConfigMapLive), obj(t, cancelConfigMapConfig))
	}
	tests := []struct {
		name string
		live func(t *testing.T) *unstructured.Unstructured
		opts func(t *testing.T) Options
	}{
		{"two-way", func(t *testing.T) *unstructured.Unstructured { return obj(t, cancelConfigMapLive) }, func(*testing.T) Options { return Options{} }},
		{"three-way", configMapLiveWithLastApplied, func(*testing.T) Options { return Options{} }},
		{"structured merge", func(t *testing.T) *unstructured.Unstructured { return obj(t, cancelConfigMapLive) }, func(t *testing.T) Options {
			return Options{StructuredMerge: true, GVKParser: testParser(t)}
		}},
		{"server-side", func(t *testing.T) *unstructured.Unstructured { return obj(t, cancelConfigMapLive) }, func(t *testing.T) Options {
			return Options{ServerSide: true, DryRunner: &stubDryRunner{t: t, result: cancelConfigMapConfig}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ignore, err := NewIgnoreNormalizer(cancelIgnoreRules, IgnoreNormalizerOptions{})
			require.NoError(t, err)
			config, live := obj(t, cancelConfigMapConfig), tt.live(t)

			// Sanity: with a live context the ignored field is not drift.
			o := tt.opts(t)
			o.Normalizer = ignore
			res, err := Diff(context.Background(), config, live, o)
			require.NoError(t, err)
			require.False(t, res.Modified, "ignored field must not be drift: predicted=%s live=%s", res.PredictedLive, res.NormalizedLive)

			for _, variant := range []struct {
				name  string
				build func(cancel context.CancelFunc) Normalizer
			}{
				// The ignore normalizer itself observes the cancellation.
				{"ignore rules interrupted", func(cancel context.CancelFunc) Normalizer {
					return Chain(&cancellingNormalizer{cancel: cancel}, ignore)
				}},
				// A normalizer that swallows the cancellation: Diff's own re-check catches it.
				{"normalizer swallows cancellation", func(cancel context.CancelFunc) Normalizer {
					return NormalizerFunc(func(ctx context.Context, un *unstructured.Unstructured) error {
						cancel()
						return ignore.Normalize(context.WithoutCancel(ctx), un)
					})
				}},
			} {
				ctx, cancel := context.WithCancel(context.Background())
				o := tt.opts(t)
				o.Normalizer = variant.build(cancel)
				res, err := Diff(ctx, obj(t, cancelConfigMapConfig), tt.live(t), o)
				cancel()
				require.ErrorIs(t, err, context.Canceled, variant.name)
				assert.Nil(t, res, variant.name)
			}
		})
	}
}

func TestDiffArrayContextCancelledDuringNormalization(t *testing.T) {
	t.Parallel()
	ignore, err := NewIgnoreNormalizer(cancelIgnoreRules, IgnoreNormalizerOptions{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := Options{Normalizer: Chain(&cancellingNormalizer{cancel: cancel}, ignore)}
	list, err := DiffArray(ctx,
		[]*unstructured.Unstructured{obj(t, cancelConfigMapConfig)},
		[]*unstructured.Unstructured{obj(t, cancelConfigMapLive)}, o)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, list)
}

func TestNormalizeContextDone(t *testing.T) {
	t.Parallel()
	ignore, err := NewIgnoreNormalizer(cancelIgnoreRules, IgnoreNormalizerOptions{})
	require.NoError(t, err)

	u := obj(t, cancelConfigMapLive)
	require.NoError(t, Normalize(context.Background(), u, Options{Normalizer: ignore}))
	_, found, _ := unstructured.NestedString(u.Object, "data", "k")
	assert.False(t, found)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = Normalize(ctx, obj(t, cancelConfigMapLive), Options{Normalizer: Chain(&cancellingNormalizer{cancel: cancel}, ignore)})
	require.ErrorIs(t, err, context.Canceled)

	// Other normalizer errors are logged, not returned (baseline).
	boom := NormalizerFunc(func(context.Context, *unstructured.Unstructured) error { return errors.New("boom") })
	require.NoError(t, Normalize(context.Background(), obj(t, cancelConfigMapLive), Options{Normalizer: boom}))
}

func TestResourceUpdateHashContextDone(t *testing.T) {
	t.Parallel()
	ignore, err := NewIgnoreNormalizer(cancelIgnoreRules, IgnoreNormalizerOptions{})
	require.NoError(t, err)

	h1, err := ResourceUpdateHash(context.Background(), obj(t, cancelConfigMapLive), ignore)
	require.NoError(t, err)
	h2, err := ResourceUpdateHash(context.Background(), obj(t, cancelConfigMapConfig), ignore)
	require.NoError(t, err)
	require.Equal(t, h1, h2, "objects differing only in ignored fields hash equally")

	for _, swallow := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		n := Chain(&cancellingNormalizer{cancel: cancel}, ignore)
		if swallow {
			n = NormalizerFunc(func(context.Context, *unstructured.Unstructured) error {
				cancel()
				return nil
			})
		}
		_, err := ResourceUpdateHash(ctx, obj(t, cancelConfigMapLive), n)
		cancel()
		require.ErrorIs(t, err, context.Canceled, "swallow=%v", swallow)
	}
}
