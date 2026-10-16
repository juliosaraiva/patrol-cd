package core

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

func TestNamespaceMatcher(t *testing.T) {
	t.Parallel()
	m := NewNamespaceMatcher("patrol", []string{"team-*", "/^prod-(a|b)$/", ""})
	for ns, want := range map[string]bool{
		"patrol": true, "team-a": true, "team-": true, "prod-a": true, "prod-b": true,
		"prod-c": false, "other": false, "": false, "xteam-a": false,
	} {
		assert.Equal(t, want, m.Allowed(ns), ns)
	}
	assert.True(t, m.ClusterWide())
	assert.Equal(t, []string{"team-*", "/^prod-(a|b)$/"}, m.Patterns())
	only := NewNamespaceMatcher("patrol", nil)
	assert.False(t, only.ClusterWide())
	assert.True(t, only.Allowed("patrol"))
	assert.False(t, only.Allowed("team-a"))
	assert.Equal(t, "patrol", only.ControlPlane())
}

func TestRevisionHistoryLimit(t *testing.T) {
	t.Parallel()
	assert.Equal(t, DefaultRevisionHistoryLimit, RevisionHistoryLimit(nil))
	assert.Equal(t, DefaultRevisionHistoryLimit, RevisionHistoryLimit(&v1alpha1.Application{}))
	app := func(n int64) *v1alpha1.Application {
		return &v1alpha1.Application{Spec: v1alpha1.ApplicationSpec{RevisionHistoryLimit: ptr.To(n)}}
	}
	assert.Equal(t, 3, RevisionHistoryLimit(app(3)))
	assert.Equal(t, 0, RevisionHistoryLimit(app(0)))
	assert.Equal(t, 0, RevisionHistoryLimit(app(-1)))
}

func TestAppendHistory(t *testing.T) {
	t.Parallel()
	var h v1alpha1.RevisionHistories
	for i := range 5 {
		h = AppendHistory(h, v1alpha1.RevisionHistory{Revision: string(rune('a' + i)), OperationName: "op" + string(rune('a'+i))}, 3)
	}
	require.Len(t, h, 3)
	assert.Equal(t, []int64{2, 3, 4}, []int64{h[0].ID, h[1].ID, h[2].ID}, "ids keep increasing after truncation")
	assert.Equal(t, "e", h[2].Revision)
	assert.True(t, HistoryRecorded(h, "ope"))
	assert.False(t, HistoryRecorded(h, "opa"))
	e, ok := FindHistory(h, 3)
	require.True(t, ok)
	assert.Equal(t, "d", e.Revision)
	_, ok = FindHistory(h, 0)
	assert.False(t, ok)
	assert.Nil(t, AppendHistory(h, v1alpha1.RevisionHistory{}, 0), "limit 0 keeps no entry")

	rapid.Check(t, func(t *rapid.T) {
		limit := rapid.IntRange(-2, 12).Draw(t, "limit")
		n := rapid.IntRange(0, 30).Draw(t, "n")
		var h v1alpha1.RevisionHistories
		for range n {
			before := h
			h = AppendHistory(h, v1alpha1.RevisionHistory{}, limit)
			if len(h) > max(limit, 0) {
				t.Fatalf("history of %d entries exceeds limit %d", len(h), limit)
			}
			if len(before) > 0 && len(h) > 0 && h[len(h)-1].ID != before[len(before)-1].ID+1 {
				t.Fatalf("ids not consecutive")
			}
		}
	})
}

func TestOperationOrdering(t *testing.T) {
	t.Parallel()
	ts := func(s int) metav1.Time { return metav1.NewTime(time.Unix(int64(s), 0)) }
	ops := []v1alpha1.Operation{
		{ObjectMeta: metav1.ObjectMeta{Name: "c", CreationTimestamp: ts(1)}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b", CreationTimestamp: ts(2)}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a", CreationTimestamp: ts(2)}, Status: v1alpha1.OperationStatus{Phase: v1alpha1.OperationRunning}},
	}
	SortOperations(ops)
	assert.Equal(t, []string{"c", "a", "b"}, []string{ops[0].Name, ops[1].Name, ops[2].Name})
	assert.True(t, Older(&ops[0], &ops[1]))
	assert.True(t, Older(&ops[1], &ops[2]), "equal timestamps order by name")
	assert.True(t, IsActive(&ops[1]))
	assert.False(t, IsActive(&ops[0]))
	assert.True(t, HasNonTerminal(ops))
	done := []v1alpha1.Operation{{Status: v1alpha1.OperationStatus{Phase: v1alpha1.OperationSucceeded}}, {Status: v1alpha1.OperationStatus{Phase: v1alpha1.OperationRejected}}}
	assert.False(t, HasNonTerminal(done))

	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "app", UID: "u1"}}
	ref := ApplicationOwnerReference(app)
	assert.Equal(t, "Application", ref.Kind)
	assert.True(t, *ref.Controller)
	op := &v1alpha1.Operation{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{ref}}}
	assert.True(t, HasOwnerReference(op, app))
	assert.False(t, HasOwnerReference(&v1alpha1.Operation{}, app))
}

func TestSkipReconcile(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{"true": true, "1": true, "T": true, "TRUE": true, "yes": false, "false": false, "": false} {
		assert.Equal(t, want, SkipReconcile(map[string]string{v1alpha1.AnnotationKeySkipReconcile: v}), v)
	}
	assert.False(t, SkipReconcile(nil))
}

func TestParamsFrom(t *testing.T) {
	t.Parallel()
	def := ParamsFrom(nil)
	assert.Equal(t, DefaultParams(), def)
	require.NotNil(t, def.SelfHealBackoff)
	assert.Equal(t, 2*time.Second, def.SelfHealBackoff.Duration)

	p, issues := settings.ResolveParams(settings.ComponentController, map[string]string{
		ParamApplicationNamespaces:  "team-*, prod",
		ParamOperationProcessors:    "3",
		ParamSelfHealBackoffTimeout: "0",
		ParamSelfHealTimeout:        "7",
		ParamSyncTimeout:            "60",
	}, func(string) (string, bool) { return "", false })
	for _, i := range issues {
		require.False(t, i.IsError(), i.String())
	}
	got := ParamsFrom(p)
	assert.Equal(t, []string{"team-*", "prod"}, got.ApplicationNamespaces)
	assert.Equal(t, 3, got.OperationProcessors)
	assert.Equal(t, 20, got.StatusProcessors)
	assert.Nil(t, got.SelfHealBackoff, "backoff timeout 0 disables the backoff")
	assert.Equal(t, 7*time.Second, got.SelfHealTimeout)
	assert.Equal(t, time.Minute, got.SyncTimeout)
}

func TestMetricsWrapConfig(t *testing.T) {
	t.Parallel()
	var nilMetrics *Metrics
	assert.NotNil(t, nilMetrics.WrapConfig(&rest.Config{Host: "https://x"}, "https://x"), "nil metrics copy the config")
	nilMetrics.ObserveSync("ns", "a", "p", "s", "Succeeded", false, time.Second)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"major":"1","minor":"36","gitVersion":"v1.36.0"}`))
	}))
	t.Cleanup(srv.Close)
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	require.NoError(t, err)
	_, err = NewMetrics(reg)
	require.NoError(t, err, "registering twice is tolerated")
	cfg := m.WrapConfig(&rest.Config{Host: srv.URL}, "https://dest")
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	require.NoError(t, err)
	_, err = dc.ServerVersion()
	require.NoError(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(m.K8sRequests.WithLabelValues("https://dest", "get", "200")))
	m.ObserveSync("ns", "a", "p", "s", "Succeeded", true, time.Second)
	assert.Equal(t, 1.0, testutil.ToFloat64(m.SyncTotal.WithLabelValues("ns", "a", "p", "s", "Succeeded", "true")))
}
