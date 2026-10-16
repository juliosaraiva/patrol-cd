package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func TestParseOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts v1alpha1.SyncOptions
		want func(*Options)
	}{
		{name: "defaults", want: func(*Options) {}},
		{name: "Validate=false", opts: v1alpha1.SyncOptions{"Validate=false"}, want: func(o *Options) { o.Validate = false }},
		{name: "SkipDryRunOnMissingResource", opts: v1alpha1.SyncOptions{"SkipDryRunOnMissingResource=true"}, want: func(o *Options) { o.SkipDryRunOnMissingResource = true }},
		{name: "CreateNamespace", opts: v1alpha1.SyncOptions{"CreateNamespace=true"}, want: func(o *Options) { o.CreateNamespace = true }},
		{name: "PruneLast", opts: v1alpha1.SyncOptions{"PruneLast=true"}, want: func(o *Options) { o.PruneLast = true }},
		{name: "ApplyOutOfSyncOnly", opts: v1alpha1.SyncOptions{"ApplyOutOfSyncOnly=true"}, want: func(o *Options) { o.ApplyOutOfSyncOnly = true }},
		{name: "ApplyOutOfSyncOnly=false has no effect", opts: v1alpha1.SyncOptions{"ApplyOutOfSyncOnly=false"}, want: func(*Options) {}},
		{name: "Replace", opts: v1alpha1.SyncOptions{"Replace=true"}, want: func(o *Options) { o.Replace = true }},
		{name: "ServerSideApply", opts: v1alpha1.SyncOptions{"ServerSideApply=true"}, want: func(o *Options) { o.ServerSideApply = true }},
		{name: "ClientSideApplyMigration=false", opts: v1alpha1.SyncOptions{"ClientSideApplyMigration=false"}, want: func(o *Options) { o.ClientSideApplyMigration = false }},
		{name: "RespectIgnoreDifferences", opts: v1alpha1.SyncOptions{"RespectIgnoreDifferences=true"}, want: func(o *Options) { o.RespectIgnoreDifferences = true }},
		{name: "FailOnSharedResource", opts: v1alpha1.SyncOptions{"FailOnSharedResource=true"}, want: func(o *Options) { o.FailOnSharedResource = true }},
		{name: "PrunePropagationPolicy=background", opts: v1alpha1.SyncOptions{"PrunePropagationPolicy=background"}, want: func(o *Options) { o.PrunePropagationPolicy = metav1.DeletePropagationBackground }},
		{name: "PrunePropagationPolicy=orphan", opts: v1alpha1.SyncOptions{"PrunePropagationPolicy=orphan"}, want: func(o *Options) { o.PrunePropagationPolicy = metav1.DeletePropagationOrphan }},
		{name: "PrunePropagationPolicy unknown keeps foreground", opts: v1alpha1.SyncOptions{"PrunePropagationPolicy=whatever"}, want: func(*Options) {}},
		{name: "background wins over orphan", opts: v1alpha1.SyncOptions{"PrunePropagationPolicy=orphan", "PrunePropagationPolicy=background"}, want: func(o *Options) { o.PrunePropagationPolicy = metav1.DeletePropagationBackground }},
		{name: "case sensitive", opts: v1alpha1.SyncOptions{"createnamespace=true", "CreateNamespace=True"}, want: func(*Options) {}},
		{name: "Prune and Delete values, first wins", opts: v1alpha1.SyncOptions{"Prune=confirm", "Prune=false", "Delete=false"}, want: func(o *Options) {
			o.Prune = strPtr("confirm")
			o.Delete = strPtr("false")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want := Options{Validate: true, ClientSideApplyMigration: true, PrunePropagationPolicy: metav1.DeletePropagationForeground}
			tt.want(&want)
			assert.Equal(t, want, ParseOptions(tt.opts))
		})
	}
}

func strPtr(v string) *string { return &v }

func TestAnnotationOptions(t *testing.T) {
	t.Parallel()
	obj := annotate(pod("p"), v1alpha1.AnnotationKeySyncOptions, " Prune=false , Validate=false,,Prune=false ")
	assert.Equal(t, []string{"Prune=false", "Validate=false"}, AnnotationOptions(obj, v1alpha1.AnnotationKeySyncOptions))
	assert.Nil(t, AnnotationOptions(pod("x"), v1alpha1.AnnotationKeySyncOptions))
	assert.Nil(t, AnnotationOptions(nil, v1alpha1.AnnotationKeySyncOptions))

	opts := ParseOptions(nil)
	assert.True(t, opts.PruneDisabled(obj))
	assert.False(t, opts.ValidateFor(obj))
}

func TestOptionPrecedence(t *testing.T) {
	t.Parallel()
	withOpt := func(opt string) *Options { o := ParseOptions(v1alpha1.SyncOptions{opt}); return &o }
	none := ParseOptions(nil)
	// Prune: the resource value overrides the application default.
	assert.False(t, withOpt("Prune=false").PruneDisabled(annotate(pod("p"), v1alpha1.AnnotationKeySyncOptions, "Prune=true")))
	assert.True(t, withOpt("Prune=false").PruneDisabled(pod("p")))
	assert.True(t, none.PruneDisabled(annotate(pod("p"), v1alpha1.AnnotationKeySyncOptions, "Prune=false")))
	assert.True(t, withOpt("Prune=false").RequiresPruneConfirmation(annotate(pod("p"), v1alpha1.AnnotationKeySyncOptions, "Prune=confirm")))
	assert.False(t, withOpt("Prune=confirm").RequiresPruneConfirmation(annotate(pod("p"), v1alpha1.AnnotationKeySyncOptions, "Prune=true")))
	assert.True(t, withOpt("Prune=confirm").RequiresPruneConfirmation(pod("p")))

	// Delete: same precedence; confirmation also via Prune=confirm.
	assert.True(t, withOpt("Delete=false").DeleteDisabled(pod("p")))
	assert.False(t, withOpt("Delete=false").DeleteDisabled(annotate(pod("p"), v1alpha1.AnnotationKeySyncOptions, "Delete=true")))
	assert.True(t, none.RequiresDeletionConfirmation(annotate(pod("p"), v1alpha1.AnnotationKeySyncOptions, "Delete=confirm")))
	assert.True(t, withOpt("Prune=confirm").RequiresDeletionConfirmation(pod("p")))
	assert.False(t, none.RequiresDeletionConfirmation(nil))

	// Replace: app OR target OR live; Replace=false on the resource has no effect.
	assert.True(t, withOpt("Replace=true").ReplaceFor(annotate(pod("p"), v1alpha1.AnnotationKeySyncOptions, "Replace=false"), nil))
	assert.True(t, none.ReplaceFor(pod("p"), annotate(pod("p"), v1alpha1.AnnotationKeySyncOptions, "Replace=true")))
	assert.False(t, none.ReplaceFor(pod("p"), nil))

	// Force: strategy OR target OR live; app-level Force=true is not an option.
	assert.True(t, ForceFor(false, annotate(pod("p"), v1alpha1.AnnotationKeySyncOptions, "Force=true"), nil))
	assert.True(t, ForceFor(true, pod("p"), nil))
	assert.False(t, ForceFor(false, pod("p"), nil))

	// ServerSideApply: resource false overrides app true.
	assert.False(t, withOpt("ServerSideApply=true").ServerSideApplyFor(annotate(pod("p"), v1alpha1.AnnotationKeySyncOptions, "ServerSideApply=false")))
	assert.True(t, none.ServerSideApplyFor(annotate(pod("p"), v1alpha1.AnnotationKeySyncOptions, "ServerSideApply=true")))
	assert.True(t, withOpt("ServerSideApply=true").ServerSideApplyFor(pod("p")))

	// Validate / SkipDryRunOnMissingResource / PruneLast: app OR resource.
	assert.False(t, withOpt("Validate=false").ValidateFor(pod("p")))
	assert.True(t, none.ValidateFor(pod("p")))
	assert.True(t, none.SkipDryRunFor(annotate(pod("p"), v1alpha1.AnnotationKeySyncOptions, "SkipDryRunOnMissingResource=true")))
	assert.True(t, withOpt("SkipDryRunOnMissingResource=true").SkipDryRunFor(pod("p")))
	assert.True(t, none.PruneLastFor(annotate(pod("p"), v1alpha1.AnnotationKeySyncOptions, "PruneLast=true")))
	assert.True(t, withOpt("PruneLast=true").PruneLastFor(pod("p")))
	assert.False(t, none.PruneLastFor(pod("p")))
}

func TestDeletionApproved(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	key := v1alpha1.AnnotationKeyDeletionApproved
	assert.False(t, DeletionApproved(nil, start))
	assert.False(t, DeletionApproved(map[string]string{key: "yes"}, start))
	assert.False(t, DeletionApproved(map[string]string{key: "2026-10-13T11:59:59Z"}, start), "approval older than the operation")
	assert.True(t, DeletionApproved(map[string]string{key: "2026-10-13T12:00:00Z"}, start))
	assert.True(t, DeletionApproved(map[string]string{key: "2026-10-13T13:00:00+01:00"}, start))
}

func TestWaveDelayFromEnv(t *testing.T) {
	t.Parallel()
	env := func(v string) func(string) string { return func(string) string { return v } }
	assert.Equal(t, DefaultWaveDelay, WaveDelayFromEnv(env("")))
	assert.Equal(t, DefaultWaveDelay, WaveDelayFromEnv(env("abc")))
	assert.Equal(t, 5*time.Second, WaveDelayFromEnv(env("5")))
	assert.Equal(t, time.Duration(0), WaveDelayFromEnv(env("0")))
	assert.Equal(t, time.Duration(0), WaveDelayFromEnv(env("-3")))
}
