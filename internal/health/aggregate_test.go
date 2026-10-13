package health

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"pgregory.net/rapid"
)

func schemaGK(g, k string) schema.GroupKind { return schema.GroupKind{Group: g, Kind: k} }

func live(kind string, s Status) ResourceHealth {
	return ResourceHealth{Kind: kind, Live: true, Health: &HealthStatus{Status: s}}
}

func TestIsWorseOrder(t *testing.T) {
	t.Parallel()
	order := []Status{StatusHealthy, StatusSuspended, StatusProgressing, StatusMissing, StatusDegraded, StatusUnknown}
	for i, a := range order {
		for j, b := range order {
			assert.Equal(t, j > i, IsWorse(a, b), "IsWorse(%s, %s)", a, b)
		}
	}
	// Unrecognized codes rank as Healthy.
	assert.False(t, IsWorse(StatusHealthy, "Bogus"))
	assert.False(t, IsWorse("Bogus", StatusHealthy))
	assert.True(t, IsWorse("", StatusSuspended))
}

func TestAggregate(t *testing.T) {
	t.Parallel()
	appGroup := func(s Status) ResourceHealth {
		return ResourceHealth{Group: ApplicationGroup, Kind: ApplicationKind, Live: true, Health: &HealthStatus{Status: s}}
	}
	tests := []struct {
		name      string
		in        []ResourceHealth
		want      Status
		persisted []*HealthStatus
	}{
		{name: "no resources", in: nil, want: StatusHealthy, persisted: []*HealthStatus{}},
		{
			name:      "worst wins",
			in:        []ResourceHealth{live("Deployment", StatusHealthy), live("Pod", StatusDegraded), live("Svc", StatusProgressing)},
			want:      StatusDegraded,
			persisted: []*HealthStatus{{Status: StatusHealthy}, {Status: StatusDegraded}, {Status: StatusProgressing}},
		},
		{
			name:      "missing resource is recorded but does not affect health",
			in:        []ResourceHealth{live("Deployment", StatusHealthy), {Kind: "Service"}},
			want:      StatusHealthy,
			persisted: []*HealthStatus{{Status: StatusHealthy}, {Status: StatusMissing}},
		},
		{
			name:      "all missing makes app Missing",
			in:        []ResourceHealth{{Kind: "Service"}, {Kind: "Deployment"}},
			want:      StatusMissing,
			persisted: []*HealthStatus{{Status: StatusMissing}, {Status: StatusMissing}},
		},
		{
			name:      "hooks are excluded and do not count as resources",
			in:        []ResourceHealth{{Kind: "Job", Hook: true}, {Kind: "Pod", Hook: true, Live: true, Health: &HealthStatus{Status: StatusDegraded}}},
			want:      StatusHealthy,
			persisted: []*HealthStatus{nil, nil},
		},
		{
			name:      "hook live + missing non-hook is Missing",
			in:        []ResourceHealth{{Kind: "Job", Hook: true, Live: true, Health: &HealthStatus{Status: StatusHealthy}}, {Kind: "Service"}},
			want:      StatusMissing,
			persisted: []*HealthStatus{nil, {Status: StatusMissing}},
		},
		{
			name:      "ignore-healthcheck counts as live but not aggregated",
			in:        []ResourceHealth{{Kind: "Pod", Live: true, IgnoreHealthCheck: true, Health: &HealthStatus{Status: StatusDegraded}}, {Kind: "Service"}},
			want:      StatusHealthy,
			persisted: []*HealthStatus{nil, {Status: StatusMissing}},
		},
		{
			name:      "self reference skipped",
			in:        []ResourceHealth{{Group: ApplicationGroup, Kind: ApplicationKind, Live: true, SelfReference: true, Health: &HealthStatus{Status: StatusDegraded}}},
			want:      StatusHealthy,
			persisted: []*HealthStatus{nil},
		},
		{
			name:      "resources without health are ignored",
			in:        []ResourceHealth{{Kind: "ConfigMap", Live: true}},
			want:      StatusHealthy,
			persisted: []*HealthStatus{nil},
		},
		{
			name:      "child app missing/unknown does not affect parent",
			in:        []ResourceHealth{appGroup(StatusMissing), appGroup(StatusUnknown), live("Deployment", StatusProgressing)},
			want:      StatusProgressing,
			persisted: []*HealthStatus{{Status: StatusMissing}, {Status: StatusUnknown}, {Status: StatusProgressing}},
		},
		{
			name:      "child app degraded affects parent",
			in:        []ResourceHealth{appGroup(StatusDegraded)},
			want:      StatusDegraded,
			persisted: []*HealthStatus{{Status: StatusDegraded}},
		},
		{
			name:      "other group application unknown does affect parent",
			in:        []ResourceHealth{{Group: "argoproj.io", Kind: "Application", Live: true, Health: &HealthStatus{Status: StatusUnknown}}},
			want:      StatusUnknown,
			persisted: []*HealthStatus{{Status: StatusUnknown}},
		},
		{
			name:      "live resource reporting Missing counts",
			in:        []ResourceHealth{live("Custom", StatusMissing)},
			want:      StatusMissing,
			persisted: []*HealthStatus{{Status: StatusMissing}},
		},
		{
			name:      "not-live ignore flags are not honored",
			in:        []ResourceHealth{{Kind: "Pod", IgnoreHealthCheck: true, SelfReference: true}},
			want:      StatusMissing,
			persisted: []*HealthStatus{{Status: StatusMissing}},
		},
		{
			name:      "suspended",
			in:        []ResourceHealth{live("CronJob", StatusSuspended), live("Deployment", StatusHealthy)},
			want:      StatusSuspended,
			persisted: []*HealthStatus{{Status: StatusSuspended}, {Status: StatusHealthy}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := AggregateDetailed(tc.in)
			assert.Equal(t, tc.want, got.Status)
			assert.Equal(t, tc.persisted, got.Resources)
			assert.Equal(t, tc.want, Aggregate(tc.in))
		})
	}
}

func TestAggregateDoesNotAliasInput(t *testing.T) {
	t.Parallel()
	in := []ResourceHealth{live("Pod", StatusDegraded)}
	out := AggregateDetailed(in)
	out.Resources[0].Message = "mutated"
	assert.Empty(t, in[0].Health.Message)
}

var statuses = []Status{StatusHealthy, StatusSuspended, StatusProgressing, StatusMissing, StatusDegraded, StatusUnknown, "", "Bogus"}

func genResource() *rapid.Generator[ResourceHealth] {
	return rapid.Custom(func(t *rapid.T) ResourceHealth {
		r := ResourceHealth{
			Kind:              rapid.SampledFrom([]string{"Pod", "Deployment", ApplicationKind}).Draw(t, "kind"),
			Live:              rapid.Bool().Draw(t, "live"),
			Hook:              rapid.Bool().Draw(t, "hook"),
			IgnoreHealthCheck: rapid.Bool().Draw(t, "ignore"),
			SelfReference:     rapid.Bool().Draw(t, "self"),
		}
		if rapid.Bool().Draw(t, "patrolGroup") {
			r.Group = ApplicationGroup
		}
		if rapid.Bool().Draw(t, "hasHealth") {
			r.Health = &HealthStatus{Status: rapid.SampledFrom(statuses).Draw(t, "status")}
		}
		return r
	})
}

// The application health is independent of resource order, never better than any
// contributing resource, and adding a hook never changes it.
func TestAggregateProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		in := rapid.SliceOfN(genResource(), 0, 12).Draw(t, "resources")
		got := Aggregate(in)

		perm := rapid.Permutation(in).Draw(t, "perm")
		if p := Aggregate(perm); p != got {
			t.Fatalf("order dependent: %s vs %s", got, p)
		}

		withHook := append(slices.Clone(in), ResourceHealth{Kind: "Job", Hook: true, Live: true, Health: &HealthStatus{Status: StatusDegraded}})
		if h := Aggregate(withHook); h != got {
			t.Fatalf("hook changed health: %s vs %s", got, h)
		}

		detailed := AggregateDetailed(in)
		for i, r := range in {
			hs := detailed.Resources[i]
			if r.Hook && hs != nil {
				t.Fatalf("hook %d recorded health", i)
			}
			if !r.Hook && !r.Live && (hs == nil || hs.Status != StatusMissing) {
				t.Fatalf("missing resource %d not recorded as Missing: %v", i, hs)
			}
			contributes := r.Live && !r.Hook && !r.IgnoreHealthCheck && !r.SelfReference && r.Health != nil &&
				!ignoredChildApp(r)
			if contributes && IsWorse(got, r.Health.Status) {
				t.Fatalf("aggregate %s better than contributing %s", got, r.Health.Status)
			}
		}
		if got != StatusHealthy && !got.Valid() {
			t.Fatalf("aggregate produced invalid status %q", got)
		}
	})
}

// IsWorse is a strict weak order: irreflexive, asymmetric, transitive.
func TestIsWorseProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		a := rapid.SampledFrom(statuses).Draw(t, "a")
		b := rapid.SampledFrom(statuses).Draw(t, "b")
		c := rapid.SampledFrom(statuses).Draw(t, "c")
		if IsWorse(a, a) {
			t.Fatalf("IsWorse(%s,%s) reflexive", a, a)
		}
		if IsWorse(a, b) && IsWorse(b, a) {
			t.Fatalf("not asymmetric for %s %s", a, b)
		}
		if IsWorse(a, b) && IsWorse(b, c) && !IsWorse(a, c) {
			t.Fatalf("not transitive for %s %s %s", a, b, c)
		}
	})
}

func TestIgnoresHealthCheck(t *testing.T) {
	t.Parallel()
	mk := func(v string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "x"}}}
		if v != "" {
			u.SetAnnotations(map[string]string{AnnotationIgnoreHealthCheck: v})
		}
		return u
	}
	assert.True(t, IgnoresHealthCheck(mk("true")))
	assert.False(t, IgnoresHealthCheck(mk("True")))
	assert.False(t, IgnoresHealthCheck(mk("yes")))
	assert.False(t, IgnoresHealthCheck(mk("")))
	assert.False(t, IgnoresHealthCheck(nil))
}

func ignoredChildApp(r ResourceHealth) bool {
	return r.Group == ApplicationGroup && r.Kind == ApplicationKind &&
		(r.Health.Status == StatusMissing || r.Health.Status == StatusUnknown)
}
