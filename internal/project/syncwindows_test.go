package project

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Fixed evaluation dates: 2026-03-10 is after the US DST change (EDT, UTC-4) and before the
// EU one; 2026-01-15 is winter time everywhere.
var (
	marchDay   = time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	januaryDay = time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	utcMinus4  = time.FixedZone("UTC-4", -4*60*60)
)

func at(day time.Time, hour int, loc *time.Location) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, loc)
}

func win(kind, schedule, duration, tz string) v1alpha1.SyncWindow {
	return v1alpha1.SyncWindow{Kind: kind, Schedule: schedule, Duration: duration, TimeZone: tz, Applications: []string{"*"}}
}

// Ports the semantics of upstream TestSyncWindows_Active.
func TestActiveSyncWindows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		windows v1alpha1.SyncWindows
		now     func(day time.Time) time.Time
		want    []int
		err     bool
	}{
		{"MatchFirst", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", ""), win("allow", "* 14 * * *", "2h", "")}, func(d time.Time) time.Time { return at(d, 11, time.UTC) }, []int{0}, false},
		{"MatchSecond", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", ""), win("allow", "* 14 * * *", "2h", "")}, func(d time.Time) time.Time { return at(d, 15, time.UTC) }, []int{1}, false},
		{"MatchBoth", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "3h", ""), win("allow", "* 11 * * *", "3h", "")}, func(d time.Time) time.Time { return at(d, 12, time.UTC) }, []int{0, 1}, false},
		{"MatchNone", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", ""), win("allow", "* 14 * * *", "2h", "")}, func(d time.Time) time.Time { return at(d, 17, time.UTC) }, nil, false},
		{"MatchFirst-NonUTC", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", ""), win("allow", "* 14 * * *", "2h", "")}, func(d time.Time) time.Time { return at(d, 11-4, utcMinus4) }, []int{0}, false},
		{"MatchSecond-NonUTC", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", ""), win("allow", "* 14 * * *", "2h", "")}, func(d time.Time) time.Time { return at(d, 15-4, utcMinus4) }, []int{1}, false},
		{"MatchNone-NonUTC", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", ""), win("allow", "* 14 * * *", "2h", "")}, func(d time.Time) time.Time { return at(d, 17-4, utcMinus4) }, nil, false},
		{"MatchFirst-TimeZoneSpecified", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", "America/New_York"), win("allow", "* 14 * * *", "2h", "America/New_York")}, func(d time.Time) time.Time { return at(d, 16, time.UTC) }, []int{0}, false},
		{"MatchSecond-TimeZoneSpecified", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", "America/New_York"), win("allow", "* 14 * * *", "2h", "America/New_York")}, func(d time.Time) time.Time { return at(d, 20, time.UTC) }, []int{1}, false},
		{"MatchNone-TimeZoneSpecified", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", "America/New_York"), win("allow", "* 14 * * *", "2h", "America/New_York")}, func(d time.Time) time.Time { return at(d, 22, time.UTC) }, nil, false},
		{"MatchFirst-PositiveTimeZoneSpecified", v1alpha1.SyncWindows{win("allow", "* 8 * * *", "2h", "Asia/Dhaka"), win("allow", "* 12 * * *", "2h", "Asia/Dhaka")}, func(d time.Time) time.Time { return at(d, 3, time.UTC) }, []int{0}, false},
		{"MatchNone-InvalidSchedule", v1alpha1.SyncWindows{win("allow", "* 10 * * 7", "3h", ""), win("allow", "* 11 * * 7", "3h", "")}, func(d time.Time) time.Time { return at(d, 12, time.UTC) }, nil, true},
		{"MatchNone-InvalidDuration", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "3a", ""), win("allow", "* 11 * * *", "3a", "")}, func(d time.Time) time.Time { return at(d, 12, time.UTC) }, nil, true},
	}
	for _, day := range []time.Time{marchDay, januaryDay} {
		for _, tt := range tests {
			got, err := ActiveSyncWindows(tt.windows, tt.now(day))
			if tt.err {
				require.Error(t, err, tt.name)
				continue
			}
			require.NoError(t, err, tt.name)
			var want v1alpha1.SyncWindows
			for _, i := range tt.want {
				want = append(want, tt.windows[i])
			}
			assert.Equal(t, want, got, "%s on %s", tt.name, day.Format(time.DateOnly))
		}
	}
}

// Ports the semantics of upstream TestSyncWindows_InactiveAllows.
func TestInactiveAllowSyncWindows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		windows v1alpha1.SyncWindows
		now     time.Time
		want    []int
		err     bool
	}{
		{"MatchFirst", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", ""), win("allow", "* 5 * * *", "2h", "")}, at(marchDay, 6, time.UTC), []int{0}, false},
		{"MatchSecond", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", ""), win("allow", "* 14 * * *", "2h", "")}, at(marchDay, 11, time.UTC), []int{1}, false},
		{"MatchBoth", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", ""), win("allow", "* 14 * * *", "2h", "")}, at(marchDay, 17, time.UTC), []int{0, 1}, false},
		{"MatchNone", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "4h", ""), win("allow", "* 11 * * *", "4h", "")}, at(marchDay, 12, time.UTC), nil, false},
		{"MatchFirst-NonUTC", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", ""), win("allow", "* 5 * * *", "2h", "")}, at(marchDay, 6-4, utcMinus4), []int{0}, false},
		{"MatchBoth-NonUTC", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", ""), win("allow", "* 14 * * *", "2h", "")}, at(marchDay, 17-4, utcMinus4), []int{0, 1}, false},
		{"MatchFirst-TimeZoneSpecified", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", "America/New_York"), win("allow", "* 5 * * *", "2h", "America/New_York")}, at(marchDay, 11, time.UTC), []int{0}, false},
		{"MatchSecond-TimeZoneSpecified", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", "America/New_York"), win("allow", "* 14 * * *", "2h", "America/New_York")}, at(marchDay, 16, time.UTC), []int{1}, false},
		{"MatchBoth-TimeZoneSpecified", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2h", "America/New_York"), win("allow", "* 14 * * *", "2h", "America/New_York")}, at(marchDay, 6, time.UTC), []int{0, 1}, false},
		{"MatchFirst-PositiveTimeZoneSpecified", v1alpha1.SyncWindows{win("allow", "* 8 * * *", "2h", "Asia/Dhaka"), win("allow", "* 12 * * *", "2h", "Asia/Dhaka")}, at(marchDay, 7, time.UTC), []int{0}, false},
		{"DenyIgnored", v1alpha1.SyncWindows{win("deny", "* 10 * * *", "2h", "")}, at(marchDay, 17, time.UTC), nil, false},
		{"InvalidSchedule", v1alpha1.SyncWindows{win("allow", "* 10 * * 7", "2h", "")}, at(marchDay, 17, time.UTC), nil, true},
		{"InvalidDuration", v1alpha1.SyncWindows{win("allow", "* 10 * * *", "2a", "")}, at(marchDay, 17, time.UTC), nil, true},
	}
	for _, tt := range tests {
		got, err := InactiveAllowSyncWindows(tt.windows, tt.now)
		if tt.err {
			require.Error(t, err, tt.name)
			continue
		}
		require.NoError(t, err, tt.name)
		var want v1alpha1.SyncWindows
		for _, i := range tt.want {
			want = append(want, tt.windows[i])
		}
		assert.Equal(t, want, got, tt.name)
	}
}

// Ports the semantics of upstream TestSyncWindow_Active, including the open interval:
// a window opened by a fire at f is active strictly between f and f+duration.
func TestSyncWindowActive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		w    v1alpha1.SyncWindow
		now  time.Time
		want bool
		err  bool
	}{
		{win("allow", "* 10 * * *", "2h", ""), at(marchDay, 11, time.UTC), true, false},
		{win("allow", "* 10 * * *", "2h", ""), at(marchDay, 13, time.UTC), false, false},
		{win("deny", "* 10 * * *", "2h", ""), at(marchDay, 11, time.UTC), true, false},
		{win("deny", "* 10 * * *", "2h", ""), at(marchDay, 13, time.UTC), false, false},
		{win("allow", "* 10 * * *", "2h", ""), at(marchDay, 11-4, utcMinus4), true, false},
		{win("allow", "* 10 * * *", "2h", ""), at(marchDay, 13-4, utcMinus4), false, false},
		{win("allow", "* 10 * * 7", "2h", ""), at(marchDay, 11, time.UTC), false, true},
		{win("allow", "* 10 * * *", "2a", ""), at(marchDay, 11, time.UTC), false, true},
		{win("allow", "* * * * *", "1h", ""), at(marchDay, 11, time.UTC), true, false},
		// Boundaries of "0 10 * * *" for 1h.
		{win("allow", "0 10 * * *", "1h", ""), at(marchDay, 10, time.UTC), false, false},
		{win("allow", "0 10 * * *", "1h", ""), at(marchDay, 10, time.UTC).Add(time.Second), true, false},
		{win("allow", "0 10 * * *", "1h", ""), at(marchDay, 11, time.UTC).Add(-time.Second), true, false},
		{win("allow", "0 10 * * *", "1h", ""), at(marchDay, 11, time.UTC), false, false},
		// Non-positive durations are never active.
		{win("allow", "* * * * *", "0s", ""), at(marchDay, 11, time.UTC), false, false},
		{win("allow", "* * * * *", "-1h", ""), at(marchDay, 11, time.UTC), false, false},
		// A schedule that never fires is "always active" upstream (zero time is before now).
		{win("deny", "0 0 30 2 *", "1h", ""), at(marchDay, 11, time.UTC), true, false},
	}
	for _, tt := range tests {
		got, err := SyncWindowActive(tt.w, tt.now)
		if tt.err {
			require.Error(t, err, "%+v", tt.w)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, tt.want, got, "%s %s at %s", tt.w.Schedule, tt.w.Duration, tt.now)
	}
}

func TestScheduleParsing(t *testing.T) {
	t.Parallel()
	now := at(marchDay, 2, time.UTC).Add(30 * time.Minute)
	// The upstream parser takes no descriptors although the CRD pattern admits them.
	_, err := SyncWindowActive(win("allow", "@daily", "1h", ""), now)
	require.Error(t, err)
	assert.Equal(t, "cannot parse schedule '@daily': parser does not accept descriptors: @daily", err.Error())
	_, err = SyncWindowActive(win("allow", "@every 1h", "1h", ""), now)
	require.Error(t, err)

	// Seconds field not accepted.
	_, err = SyncWindowActive(win("allow", "0 0 10 * * *", "1h", ""), now)
	require.Error(t, err)

	// TZ= prefix is honored by robfig/cron (on top of the timeZone field, as upstream).
	ok, err := SyncWindowActive(win("allow", "TZ=Asia/Dhaka 0 8 * * *", "1h", ""), now)
	require.NoError(t, err)
	assert.True(t, ok, "08:00 Dhaka is 02:00 UTC")
	ok, err = SyncWindowActive(win("allow", "CRON_TZ=Asia/Dhaka 0 8 * * *", "1h", ""), now)
	require.NoError(t, err)
	assert.True(t, ok)

	// Upstream robfig/cron panics here; Patrol returns an error.
	_, err = SyncWindowActive(win("allow", "TZ=UTC", "1h", ""), now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot parse schedule 'TZ=UTC'")
	_, err = SyncWindowActive(win("allow", "CRON_TZ=Bad/Zone 0 8 * * *", "1h", ""), now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provided bad location Bad/Zone")
	_, err = SyncWindowActive(win("allow", "", "1h", ""), now)
	require.Error(t, err)
	assert.Equal(t, "cannot parse schedule '': empty spec string", err.Error())
	_, err = SyncWindowActive(win("allow", "* * * * *", "1000days", ""), now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot parse duration '1000days'")
}

// TEN-025: "0 22 * * *" in Europe/Amsterdam is active at 20:xx UTC in summer and 21:xx UTC in
// winter; unknown zones fall back to UTC.
func TestSyncWindowTimeZoneDST(t *testing.T) {
	t.Parallel()
	w := win("deny", "0 22 * * *", "1h", "Europe/Amsterdam")
	summer := time.Date(2026, 7, 1, 20, 30, 0, 0, time.UTC)
	winter := time.Date(2026, 1, 15, 21, 30, 0, 0, time.UTC)
	for _, tt := range []struct {
		now  time.Time
		want bool
	}{
		{summer, true},
		{summer.Add(time.Hour), false},
		{winter, true},
		{winter.Add(-time.Hour), false},
	} {
		got, err := SyncWindowActive(w, tt.now)
		require.NoError(t, err)
		assert.Equal(t, tt.want, got, tt.now)
	}

	bad := win("allow", "0 22 * * *", "1h", "Mars/Olympus")
	got, err := SyncWindowActive(bad, time.Date(2026, 1, 15, 22, 30, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.True(t, got, "unknown zone is UTC")

	// The offset is the one in effect at the evaluation instant: just before and after the
	// EU spring change (2026-03-29 01:00 UTC) the same UTC-relative check shifts by one hour.
	w2 := win("allow", "0 2 * * *", "30m", "Europe/Amsterdam")
	before := time.Date(2026, 3, 28, 1, 15, 0, 0, time.UTC) // CET: 02:15 local
	after := time.Date(2026, 3, 30, 0, 15, 0, 0, time.UTC)  // CEST: 02:15 local
	for _, now := range []time.Time{before, after} {
		got, err := SyncWindowActive(w2, now)
		require.NoError(t, err)
		assert.True(t, got, now)
	}
}

func syncTestApp() *v1alpha1.Application {
	return &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "test-app"},
		Spec: v1alpha1.ApplicationSpec{Destination: v1alpha1.ApplicationDestination{
			Namespace: "default", Server: "cluster1", Name: "clusterName",
		}},
	}
}

// Ports the semantics of upstream TestSyncWindows_Matches and
// TestSyncWindows_Matches_AND_Operator as one OR/AND truth table (TEN-022).
func TestMatchSyncWindows(t *testing.T) {
	t.Parallel()
	app := syncTestApp()
	tests := []struct {
		name               string
		apps, clusters, ns []string
		or, and            bool
	}{
		{"namespace", nil, nil, []string{"default"}, true, true},
		{"cluster server", nil, []string{"cluster1"}, nil, true, true},
		{"cluster name", nil, []string{"clusterName"}, nil, true, true},
		{"app", []string{"test-app"}, nil, nil, true, true},
		{"app+ns", []string{"test-app"}, nil, []string{"default"}, true, true},
		{"app+cluster", []string{"test-app"}, []string{"clusterName"}, nil, true, true},
		{"ns+cluster", nil, []string{"clusterName"}, []string{"default"}, true, true},
		{"all", []string{"test-app"}, []string{"clusterName"}, []string{"default"}, true, true},
		{"app glob", []string{"test-*"}, nil, nil, true, true},
		{"app glob+ns", []string{"test-*"}, nil, []string{"default"}, true, true},
		{"app glob+cluster glob", []string{"test-*"}, []string{"clusterN*"}, nil, true, true},
		{"none", nil, nil, nil, false, false},
		{"app mismatch only", []string{"other"}, nil, nil, false, false},
		{"app ok, ns mismatch", []string{"test-app"}, nil, []string{"other"}, true, false},
		{"app mismatch, ns ok", []string{"other"}, nil, []string{"default"}, true, false},
		{"app ok, cluster mismatch", []string{"test-app"}, []string{"other"}, nil, true, false},
		{"cluster ok, ns mismatch", nil, []string{"cluster1"}, []string{"other"}, true, false},
		{"all mismatch", []string{"a"}, []string{"b"}, []string{"c"}, false, false},
		{"app, cluster ok, ns mismatch", []string{"test-app"}, []string{"cluster1"}, []string{"other"}, true, false},
		// No negation in window selectors: "!x" is a literal.
		{"literal bang", []string{"!other"}, nil, nil, false, false},
	}
	for _, tt := range tests {
		for _, and := range []bool{false, true} {
			w := v1alpha1.SyncWindow{Kind: "allow", Schedule: "* * * * *", Duration: "1h", Applications: tt.apps, Clusters: tt.clusters, Namespaces: tt.ns, UseAndOperator: and}
			got := MatchSyncWindows(v1alpha1.SyncWindows{w}, app)
			want := tt.or
			if and {
				want = tt.and
			}
			assert.Equal(t, want, len(got) == 1, "%s and=%v", tt.name, and)
		}
	}
	assert.Nil(t, MatchSyncWindows(v1alpha1.SyncWindows{win("allow", "* * * * *", "1h", "")}, nil))

	// TEN-022: an app addressed by server does not match a clusters entry naming the cluster.
	byServer := syncTestApp()
	byServer.Spec.Destination.Name = ""
	assert.Empty(t, MatchSyncWindows(v1alpha1.SyncWindows{{Kind: "deny", Schedule: "* * * * *", Duration: "1h", Clusters: []string{"clusterName"}}}, byServer))
}

// windowSet builds windows relative to a fixed now (12:00 UTC): active windows fire every
// minute, inactive ones at 22:00.
type windowSet struct{ ws v1alpha1.SyncWindows }

func (b *windowSet) add(kind string, active, manual bool) *windowSet {
	schedule := "0 22 * * *"
	if active {
		schedule = "* * * * *"
	}
	b.ws = append(b.ws, v1alpha1.SyncWindow{Kind: kind, Schedule: schedule, Duration: "1h", Applications: []string{"app1"}, Namespaces: []string{"public"}, ManualSync: manual})
	return b
}

// Ports the semantics of upstream TestSyncWindows_CanSync (TEN-023 matrix).
func TestCanSyncWithWindows(t *testing.T) {
	t.Parallel()
	now := at(marchDay, 12, time.UTC)
	type w struct {
		kind           string
		active, manual bool
	}
	tests := []struct {
		name    string
		windows []w
		manual  bool
		want    bool
	}{
		{"no windows manual", nil, true, true},
		{"no windows auto", nil, false, true},
		{"inactive deny manual=true", []w{{"deny", false, true}}, true, true},
		{"inactive deny manual=false", []w{{"deny", false, false}}, true, true},
		{"one inactive allow without manual", []w{{"allow", false, true}, {"allow", false, false}}, true, false},
		{"active allow manual=true", []w{{"allow", true, true}}, true, true},
		{"active allow manual=false", []w{{"allow", true, false}}, true, true},
		{"active allow auto", []w{{"allow", true, false}}, false, true},
		{"active allow + inactive deny manual", []w{{"allow", true, false}, {"deny", false, false}}, true, true},
		{"active allow + inactive deny auto", []w{{"allow", true, false}, {"deny", false, false}}, false, true},
		{"inactive allow manual", []w{{"allow", false, false}}, true, false},
		{"inactive allow auto", []w{{"allow", false, false}}, false, false},
		{"inactive allow ManualSync manual", []w{{"allow", false, true}}, true, true},
		{"inactive allow ManualSync auto", []w{{"allow", false, true}}, false, false},
		{"inactive allow + inactive deny manual", []w{{"allow", false, false}, {"deny", false, false}}, true, false},
		{"inactive allow + inactive deny auto", []w{{"allow", false, false}, {"deny", false, false}}, false, false},
		{"active allow + inactive allow auto", []w{{"allow", true, false}, {"allow", false, false}}, false, true},
		{"active deny manual", []w{{"deny", true, false}}, true, false},
		{"active deny auto", []w{{"deny", true, false}}, false, false},
		{"active deny ManualSync manual", []w{{"deny", true, true}}, true, true},
		{"active deny ManualSync auto", []w{{"deny", true, true}}, false, false},
		{"many active deny one without manual (manual)", []w{{"deny", true, true}, {"deny", true, true}, {"deny", true, true}, {"deny", true, false}}, true, false},
		{"many active deny one without manual (auto)", []w{{"deny", true, true}, {"deny", true, true}, {"deny", true, true}, {"deny", true, false}}, false, false},
		{"active deny + active allow manual", []w{{"allow", true, false}, {"deny", true, false}}, true, false},
		{"active deny ManualSync + active allow manual", []w{{"allow", true, false}, {"deny", true, true}}, true, true},
		{"active deny ManualSync + active allow auto", []w{{"allow", true, false}, {"deny", true, true}}, false, false},
		// A window of unknown kind affects nothing (besides parse errors).
		{"unknown kind", []w{{"maybe", true, false}}, false, true},
	}
	for _, tt := range tests {
		b := &windowSet{}
		for _, x := range tt.windows {
			b.add(x.kind, x.active, x.manual)
		}
		got, err := CanSyncWithWindows(b.ws, tt.manual, nil, now)
		require.NoError(t, err, tt.name)
		assert.Equal(t, tt.want, got, tt.name)
	}

	invalid := v1alpha1.SyncWindows{
		{Kind: "allow", Schedule: "* 10 * * 7", Duration: "1h"},
		{Kind: "deny", Schedule: "* 10 * * 7", Duration: "1h"},
	}
	got, err := CanSyncWithWindows(invalid, false, nil, now)
	require.Error(t, err)
	assert.False(t, got)
	assert.Contains(t, err.Error(), "invalid sync windows: cannot parse schedule")

	inactiveBadDuration := (&windowSet{}).add("allow", true, false).ws
	inactiveBadDuration = append(inactiveBadDuration, v1alpha1.SyncWindow{Kind: "allow", Schedule: "0 22 * * *", Duration: "invalid-duration"})
	_, err = CanSyncWithWindows(inactiveBadDuration, true, nil, now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid sync windows")
	assert.Contains(t, err.Error(), "cannot parse duration")
}

// TEN-024 / SYNC-086: the nine documented transition scenarios plus the "new sync" and
// "started inside the deny window" cases. The operation started at 11:30, now is 12:30;
// "allow" windows were active at the start (11:00-12:00), "deny" windows are active now
// (12:00-13:00).
func TestSyncOverrunScenarios(t *testing.T) {
	t.Parallel()
	start := at(marchDay, 11, time.UTC).Add(30 * time.Minute)
	now := at(marchDay, 12, time.UTC).Add(30 * time.Minute)
	allow := func(overrun bool) v1alpha1.SyncWindow {
		return v1alpha1.SyncWindow{Kind: "allow", Schedule: "0 11 * * *", Duration: "1h", Applications: []string{"*"}, SyncOverrun: overrun}
	}
	deny := func(overrun bool) v1alpha1.SyncWindow {
		return v1alpha1.SyncWindow{Kind: "deny", Schedule: "0 12 * * *", Duration: "1h", Applications: []string{"*"}, SyncOverrun: overrun}
	}
	tests := []struct {
		name    string
		windows v1alpha1.SyncWindows
		start   *time.Time
		want    bool
	}{
		{"1 no window -> deny with overrun", v1alpha1.SyncWindows{deny(true)}, &start, true},
		{"2 no window -> deny without overrun", v1alpha1.SyncWindows{deny(false)}, &start, false},
		{"3 allow with overrun -> deny with overrun", v1alpha1.SyncWindows{allow(true), deny(true)}, &start, true},
		{"4 allow with overrun -> deny without overrun", v1alpha1.SyncWindows{allow(true), deny(false)}, &start, false},
		{"5 allow without overrun -> deny with overrun", v1alpha1.SyncWindows{allow(false), deny(true)}, &start, true},
		{"6 allow without overrun -> deny without overrun", v1alpha1.SyncWindows{allow(false), deny(false)}, &start, false},
		{"7 allow with overrun -> allow ends", v1alpha1.SyncWindows{allow(true)}, &start, true},
		{"8 allows with overrun -> all end", v1alpha1.SyncWindows{allow(true), {Kind: "allow", Schedule: "30 10 * * *", Duration: "90m", Namespaces: []string{"*"}, SyncOverrun: true}}, &start, true},
		{"9 allows, one without overrun -> all end", v1alpha1.SyncWindows{allow(true), {Kind: "allow", Schedule: "30 10 * * *", Duration: "90m", Namespaces: []string{"*"}}}, &start, false},
		{"new sync ignores overrun", v1alpha1.SyncWindows{deny(true)}, nil, false},
		{"zero start ignores overrun", v1alpha1.SyncWindows{deny(true)}, &time.Time{}, false},
		{"started inside the deny window", v1alpha1.SyncWindows{{Kind: "deny", Schedule: "0 11 * * *", Duration: "2h", Applications: []string{"*"}, SyncOverrun: true}}, &start, false},
		{"concurrent denies all with overrun", v1alpha1.SyncWindows{allow(false), deny(true), {Kind: "deny", Schedule: "15 12 * * *", Duration: "1h", Clusters: []string{"*"}, SyncOverrun: true}}, &start, true},
		{"concurrent denies, one without overrun", v1alpha1.SyncWindows{allow(false), deny(true), {Kind: "deny", Schedule: "15 12 * * *", Duration: "1h", Clusters: []string{"*"}}}, &start, false},
		{"started while inactive allows blocked it", v1alpha1.SyncWindows{{Kind: "allow", Schedule: "0 9 * * *", Duration: "1h", Applications: []string{"*"}, SyncOverrun: true}}, &start, false},
	}
	for _, tt := range tests {
		got, err := CanSyncWithWindows(tt.windows, false, tt.start, now)
		require.NoError(t, err, tt.name)
		assert.Equal(t, tt.want, got, tt.name)
	}
}

func TestCanSyncAndActiveWindowsForApp(t *testing.T) {
	t.Parallel()
	now := at(marchDay, 12, time.UTC)
	app := syncTestApp()
	proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: v1alpha1.AppProjectSpec{SyncWindows: v1alpha1.SyncWindows{
		{Kind: "deny", Schedule: "* * * * *", Duration: "1h", Applications: []string{"other-*"}},
		{Kind: "allow", Schedule: "0 22 * * *", Duration: "1h", Namespaces: []string{"default"}, ManualSync: true},
	}}}
	active, err := ActiveWindows(proj, app, now)
	require.NoError(t, err)
	assert.Empty(t, active, "the active deny window does not apply to test-app")

	ok, err := CanSync(proj, app, false, nil, now)
	require.NoError(t, err)
	assert.False(t, ok, "inactive allow blocks automated syncs")
	ok, err = CanSync(proj, app, true, nil, now)
	require.NoError(t, err)
	assert.True(t, ok, "manualSync on the inactive allow permits manual syncs")

	ok, err = CanSync(nil, app, false, nil, now)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestGetApplicationSyncWindowsState(t *testing.T) {
	t.Parallel()
	now := at(marchDay, 12, time.UTC)
	app := syncTestApp()
	tests := []struct {
		name    string
		windows v1alpha1.SyncWindows
		status  SyncWindowStatus
		canSync bool
		active  int
	}{
		{"no windows", nil, SyncWindowStatusAllowed, true, 0},
		{"active allow", v1alpha1.SyncWindows{win("allow", "* * * * *", "1h", "")}, SyncWindowStatusAllowed, true, 1},
		{"active deny", v1alpha1.SyncWindows{win("deny", "* * * * *", "1h", "")}, SyncWindowStatusDenied, false, 1},
		{"active deny manual", v1alpha1.SyncWindows{{Kind: "deny", Schedule: "* * * * *", Duration: "1h", Applications: []string{"*"}, ManualSync: true}}, SyncWindowStatusManualAllowed, true, 1},
		{"inactive allow", v1alpha1.SyncWindows{win("allow", "0 22 * * *", "1h", "")}, SyncWindowStatusDenied, false, 0},
		{"inactive allow manual", v1alpha1.SyncWindows{{Kind: "allow", Schedule: "0 22 * * *", Duration: "1h", Applications: []string{"*"}, ManualSync: true}}, SyncWindowStatusManualAllowed, true, 0},
		{"inactive deny", v1alpha1.SyncWindows{win("deny", "0 22 * * *", "1h", "")}, SyncWindowStatusAllowed, true, 0},
	}
	for _, tt := range tests {
		proj := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{SyncWindows: tt.windows}}
		st, err := GetApplicationSyncWindowsState(proj, app, now)
		require.NoError(t, err, tt.name)
		assert.Equal(t, tt.status, st.Status, tt.name)
		assert.Equal(t, tt.canSync, st.CanSync, tt.name)
		assert.Len(t, st.ActiveWindows, tt.active, tt.name)
		assert.Len(t, st.AssignedWindows, len(tt.windows), tt.name)
	}

	proj := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{SyncWindows: v1alpha1.SyncWindows{win("allow", "* 10 * * 7", "1h", "")}}}
	_, err := GetApplicationSyncWindowsState(proj, app, now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid sync windows: invalid sync windows: cannot parse schedule '* 10 * * 7'")

	proj = &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{SyncWindows: v1alpha1.SyncWindows{win("deny", "0 13 * * *", "1h", "")}}}
	st, err := GetApplicationSyncWindowsState(proj, app, now)
	require.NoError(t, err)
	require.NotNil(t, st.NextTransition)
	assert.Equal(t, at(marchDay, 13, time.UTC), *st.NextTransition)
}

func TestGetSyncWindowTimes(t *testing.T) {
	t.Parallel()
	day := marchDay
	tests := []struct {
		name       string
		w          v1alpha1.SyncWindow
		now        time.Time
		active     bool
		start, end time.Time
	}{
		{"active", win("allow", "0 10 * * *", "2h", ""), at(day, 11, time.UTC), true, at(day, 10, time.UTC), at(day, 12, time.UTC)},
		{"upcoming", win("allow", "0 10 * * *", "2h", ""), at(day, 9, time.UTC), false, at(day, 10, time.UTC), at(day, 12, time.UTC)},
		{"tomorrow", win("allow", "0 10 * * *", "2h", ""), at(day, 13, time.UTC), false, at(day, 10, time.UTC).AddDate(0, 0, 1), at(day, 12, time.UTC).AddDate(0, 0, 1)},
		{"merged minutes", win("allow", "* 10 * * *", "2h", ""), at(day, 11, time.UTC), true, at(day, 10, time.UTC), at(day, 12, time.UTC).Add(59 * time.Minute)},
		{"time zone", win("allow", "0 8 * * *", "1h", "Asia/Dhaka"), at(day, 2, time.UTC).Add(30 * time.Minute), true, at(day, 2, time.UTC), at(day, 3, time.UTC)},
		{"fire exactly now", win("allow", "0 10 * * *", "1h", ""), at(day, 10, time.UTC), false, at(day, 10, time.UTC), at(day, 11, time.UTC)},
		{"never closes", win("allow", "* * * * *", "1h", ""), at(day, 10, time.UTC), true, at(day, 9, time.UTC).Add(time.Minute), time.Time{}},
		{"zero duration", win("allow", "* * * * *", "0s", ""), at(day, 10, time.UTC), false, time.Time{}, time.Time{}},
		{"never fires", win("deny", "0 0 30 2 *", "1h", ""), at(day, 10, time.UTC), true, time.Time{}, time.Time{}},
	}
	for _, tt := range tests {
		got, err := GetSyncWindowTimes(tt.w, tt.now)
		require.NoError(t, err, tt.name)
		assert.Equal(t, tt.active, got.Active, tt.name)
		assert.True(t, tt.start.Equal(got.Start), "%s start: want %s got %s", tt.name, tt.start, got.Start)
		assert.True(t, tt.end.Equal(got.End), "%s end: want %s got %s", tt.name, tt.end, got.End)
		active, err := SyncWindowActive(tt.w, tt.now)
		require.NoError(t, err)
		assert.Equal(t, active, got.Active, "%s: consistent with SyncWindowActive", tt.name)
	}
	_, err := GetSyncWindowTimes(win("allow", "bad", "1h", ""), day)
	require.Error(t, err)

	next, ok, err := NextSyncWindowTransition(v1alpha1.SyncWindows{win("allow", "0 10 * * *", "2h", ""), win("deny", "0 13 * * *", "1h", "")}, at(day, 11, time.UTC))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, at(day, 12, time.UTC), next)
	next, ok, err = NextSyncWindowTransition(v1alpha1.SyncWindows{win("allow", "0 10 * * *", "2h", ""), win("deny", "0 13 * * *", "1h", "")}, at(day, 12, time.UTC).Add(30*time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, at(day, 13, time.UTC), next)
	_, ok, err = NextSyncWindowTransition(nil, day)
	require.NoError(t, err)
	assert.False(t, ok)
	_, _, err = NextSyncWindowTransition(v1alpha1.SyncWindows{win("allow", "bad", "1h", "")}, day)
	require.Error(t, err)
}

func ExampleCanSync() {
	now := time.Date(2026, 3, 10, 12, 30, 0, 0, time.UTC)
	proj := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{SyncWindows: v1alpha1.SyncWindows{
		{Kind: "deny", Schedule: "0 12 * * *", Duration: "1h", Namespaces: []string{"prod-*"}, ManualSync: true},
	}}}
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: v1alpha1.ApplicationSpec{
		Destination: v1alpha1.ApplicationDestination{Server: "https://prod", Namespace: "prod-eu"},
	}}
	auto, _ := CanSync(proj, app, false, nil, now)
	manual, _ := CanSync(proj, app, true, nil, now)
	fmt.Println(auto, manual)
	// Output: false true
}
