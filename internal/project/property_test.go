package project

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Small alphabets keep collisions between patterns and values frequent.
var (
	propHosts = []string{"github.com", "GitLab.com"}
	propOrgs  = []string{"a", "b"}
	propRepos = []string{"x", "y", "x/y"}
)

func genRepoURL() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		u := fmt.Sprintf("https://%s/%s/%s", rapid.SampledFrom(propHosts).Draw(t, "host"), rapid.SampledFrom(propOrgs).Draw(t, "org"), rapid.SampledFrom(propRepos).Draw(t, "repo"))
		if rapid.Bool().Draw(t, "dotgit") {
			u += ".git"
		}
		return u
	})
}

func genRepoPattern() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		host := rapid.SampledFrom(append(slices.Clone(propHosts), "*")).Draw(t, "host")
		org := rapid.SampledFrom(append(slices.Clone(propOrgs), "*")).Draw(t, "org")
		repo := rapid.SampledFrom(append(slices.Clone(propRepos), "*", "**")).Draw(t, "repo")
		if rapid.IntRange(0, 9).Draw(t, "star") == 0 {
			return "*"
		}
		return fmt.Sprintf("https://%s/%s/%s", host, org, repo)
	})
}

func genSourceList() *rapid.Generator[[]string] {
	entry := rapid.Custom(func(t *rapid.T) string {
		p := genRepoPattern().Draw(t, "p")
		if rapid.IntRange(0, 2).Draw(t, "neg") == 0 && p != "*" {
			return "!" + p
		}
		return p
	})
	return rapid.SliceOfN(entry, 0, 6)
}

func insertAt[T any](t *rapid.T, list []T, v T) []T {
	i := rapid.IntRange(0, len(list)).Draw(t, "pos")
	return slices.Insert(slices.Clone(list), i, v)
}

func sourcePermitted(list []string, url string) bool {
	return IsSourcePermitted(&v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{SourceRepos: list}}, url)
}

// patternMatchesURL reports whether a negated entry "!pattern" names url. Negated entries
// use the plain glob: the "*" shortcut of allow entries does not apply, so "!*" names no URL
// containing '/' (upstream quirk, see TestNegatedStarQuirk).
func patternMatchesURL(pattern, url string) bool {
	return globMatch(NormalizeGitURL(pattern), NormalizeGitURL(url), '/')
}

// Deny wins: a source named by a negated entry is never permitted, wherever the entry is.
func TestPropertySourceDenyWins(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		list := genSourceList().Draw(t, "list")
		url := genRepoURL().Draw(t, "url")
		deny := genRepoPattern().Draw(t, "deny")
		if !patternMatchesURL(deny, url) {
			t.Skip("deny entry does not name the url")
		}
		if sourcePermitted(insertAt(t, list, "!"+deny), url) {
			t.Fatalf("%v + !%s permits %s", list, deny, url)
		}
	})
}

// Deny-line monotonicity: adding a negated entry never permits a source it names, and never
// revokes a source it does not name.
func TestPropertySourceDenyLineMonotonic(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		list := genSourceList().Draw(t, "list")
		url := genRepoURL().Draw(t, "url")
		deny := genRepoPattern().Draw(t, "deny")
		before := sourcePermitted(list, url)
		after := sourcePermitted(insertAt(t, list, "!"+deny), url)
		names := patternMatchesURL(deny, url)
		if names && after {
			t.Fatalf("!%s grants %s", deny, url)
		}
		if !names && before && !after {
			t.Fatalf("!%s revokes unrelated %s", deny, url)
		}
	})
}

// Adding an allow entry never revokes; entry order never matters.
func TestPropertySourceAllowMonotonicAndOrderIndependent(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		list := genSourceList().Draw(t, "list")
		url := genRepoURL().Draw(t, "url")
		allow := genRepoPattern().Draw(t, "allow")
		before := sourcePermitted(list, url)
		if before && !sourcePermitted(insertAt(t, list, allow), url) {
			t.Fatalf("allow %s revoked %s from %v", allow, url, list)
		}
		perm := rapid.Permutation(list).Draw(t, "perm")
		if sourcePermitted(perm, url) != before {
			t.Fatalf("order changes the decision: %v vs %v for %s", list, perm, url)
		}
	})
}

var (
	propServers    = []string{"https://a.example.com", "https://b.example.com", "https://kubernetes.default.svc"}
	propNames      = []string{"a", "b", "in-cluster"}
	propNamespaces = []string{"", "default", "kube-system", "team-a"}
)

func genDestinationValue(values []string) *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		v := rapid.SampledFrom(append(slices.Clone(values), "*", "", "team-*", "https://*.example.com")).Draw(t, "v")
		if v != "" && v != "*" && rapid.IntRange(0, 3).Draw(t, "neg") == 0 {
			return "!" + v
		}
		return v
	})
}

func genDestEntry() *rapid.Generator[v1alpha1.ApplicationDestination] {
	return rapid.Custom(func(t *rapid.T) v1alpha1.ApplicationDestination {
		return v1alpha1.ApplicationDestination{
			Server:    genDestinationValue(propServers).Draw(t, "server"),
			Name:      genDestinationValue(propNames).Draw(t, "name"),
			Namespace: genDestinationValue(propNamespaces).Draw(t, "namespace"),
		}
	})
}

func genDestination() *rapid.Generator[Destination] {
	return rapid.Custom(func(t *rapid.T) Destination {
		return Destination{
			Server:    rapid.SampledFrom(propServers).Draw(t, "server"),
			Name:      rapid.SampledFrom(propNames).Draw(t, "name"),
			Namespace: rapid.SampledFrom(propNamespaces).Draw(t, "namespace"),
		}
	})
}

func destPermitted(t *rapid.T, entries []v1alpha1.ApplicationDestination, d Destination) bool {
	ok, err := IsDestinationPermitted(context.Background(), &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{Destinations: entries}}, d, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return ok
}

// Deny lines on namespace, server or name reject the destination they name, wherever they
// are listed and whatever else the project allows.
func TestPropertyDestinationDenyWins(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		entries := rapid.SliceOfN(genDestEntry(), 0, 5).Draw(t, "entries")
		d := genDestination().Draw(t, "dest")
		var deny v1alpha1.ApplicationDestination
		switch rapid.IntRange(0, 2).Draw(t, "kind") {
		case 0:
			if d.Namespace == "" {
				t.Skip("an empty namespace cannot be negated")
			}
			deny = v1alpha1.ApplicationDestination{Server: "*", Namespace: "!" + d.Namespace}
		case 1:
			deny = v1alpha1.ApplicationDestination{Server: "!" + d.Server, Namespace: "*"}
		default:
			deny = v1alpha1.ApplicationDestination{Name: "!" + d.Name, Namespace: rapid.SampledFrom(propNamespaces).Draw(t, "anyns")}
		}
		if destPermitted(t, insertAt(t, entries, deny), d) {
			t.Fatalf("%+v + deny %+v permits %+v", entries, deny, d)
		}
	})
}

// Adding a non-negated entry never revokes; entry order never matters.
func TestPropertyDestinationAllowMonotonicAndOrderIndependent(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		entries := rapid.SliceOfN(genDestEntry(), 0, 5).Draw(t, "entries")
		d := genDestination().Draw(t, "dest")
		before := destPermitted(t, entries, d)
		allow := v1alpha1.ApplicationDestination{
			Server:    rapid.SampledFrom(append(slices.Clone(propServers), "*", "")).Draw(t, "server"),
			Name:      rapid.SampledFrom(append(slices.Clone(propNames), "*", "")).Draw(t, "name"),
			Namespace: rapid.SampledFrom(append(slices.Clone(propNamespaces), "*")).Draw(t, "namespace"),
		}
		if before && !destPermitted(t, insertAt(t, entries, allow), d) {
			t.Fatalf("allow %+v revoked %+v", allow, d)
		}
		perm := rapid.Permutation(entries).Draw(t, "perm")
		if destPermitted(t, perm, d) != before {
			t.Fatalf("order changes the decision for %+v", d)
		}
	})
}

// A source permitted by a project stays permitted in its virtual project (global projects
// only append), except through namespaceResourceWhitelist which is not a source rule.
func TestPropertyVirtualProjectOnlyWidensSources(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		own := genSourceList().Draw(t, "own")
		global := genSourceList().Draw(t, "global")
		url := genRepoURL().Draw(t, "url")
		proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: v1alpha1.AppProjectSpec{SourceRepos: own}}
		virtual := MergeVirtualProject(proj, &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{SourceRepos: global}})
		// Global deny lines may revoke; without them the virtual project only widens.
		if slices.ContainsFunc(global, func(s string) bool { return strings.HasPrefix(s, "!") }) {
			t.Skip("global project has deny lines")
		}
		if IsSourcePermitted(proj, url) && !IsSourcePermitted(virtual, url) {
			t.Fatalf("merge revoked %s", url)
		}
	})
}

// Scoped repository URLs as a Repository author may write them, including values that read
// as globs, negations or unparsable URLs.
func genScopedRepoURL() *rapid.Generator[string] {
	return rapid.OneOf(
		genRepoURL(),
		genRepoPattern(),
		rapid.Map(genRepoPattern(), func(p string) string { return "!" + p }),
		rapid.Map(genRepoURL(), func(u string) string { return " !" + u }),
		rapid.SampledFrom([]string{"", "!x", "not a url %zz", "https://github.com/a/x?", "https://github.com/[ab]/x", "https://github.com/{a,b}/x", `https://github.com/a/\x`}),
	)
}

// Sources an Application author may use, including unparsable and empty URLs.
func genSourceURL() *rapid.Generator[string] {
	return rapid.OneOf(genRepoURL(), rapid.SampledFrom([]string{"", "x", "%zz", "https://github.com/a/x?", "https://github.com/a/z"}))
}

// Security: project-scoped repositories widen the effective project by exactly themselves
// (same normalized URL), never revoke, and are never read as patterns.
func TestPropertyScopedRepositoriesWidenByExactlyThemselves(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		own := genSourceList().Draw(t, "own")
		scoped := rapid.SliceOfN(genScopedRepoURL(), 0, 4).Draw(t, "scoped")
		src := genSourceURL().Draw(t, "src")
		proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: v1alpha1.AppProjectSpec{SourceRepos: own}}
		eff, _, err := EffectiveProject(context.Background(), proj, Lookups{Repositories: memRepos{"p": scoped}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		before, after := IsSourcePermitted(proj, src), IsSourcePermitted(eff, src)
		if before && !after {
			t.Fatalf("scoped %q revoked %q", scoped, src)
		}
		if after && !before && !slices.ContainsFunc(scoped, func(r string) bool {
			return NormalizeGitURL(r) != "" && NormalizeGitURL(r) == NormalizeGitURL(src)
		}) {
			t.Fatalf("scoped %q widened %v to %q", scoped, own, src)
		}
	})
}

type scopedClusterDraw struct {
	server     string
	namespaces []string
}

func genScopedCluster() *rapid.Generator[scopedClusterDraw] {
	return rapid.Custom(func(t *rapid.T) scopedClusterDraw {
		server := rapid.SampledFrom(append(slices.Clone(propServers),
			"https://*", "https://*.example.com", "!https://a.example.com", "https://a.example.com/", "https://[fd00::1]:6443", "https://{a,b}.example.com", "")).Draw(t, "server")
		ns := rapid.SliceOfN(rapid.SampledFrom(append(slices.Clone(propNamespaces), "*", "!default", "team-*", "kube-?")), 0, 3).Draw(t, "namespaces")
		return scopedClusterDraw{server: server, namespaces: ns}
	})
}

// Security: project-scoped clusters widen the effective project by exactly themselves (their
// normalized server, with any namespace or only the listed ones, compared verbatim), never
// revoke, and are never read as patterns.
func TestPropertyScopedClustersWidenByExactlyThemselves(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		entries := rapid.SliceOfN(genDestEntry(), 0, 4).Draw(t, "entries")
		draws := rapid.SliceOfN(genScopedCluster(), 0, 3).Draw(t, "scoped")
		clusters := make([]v1alpha1.Cluster, 0, len(draws))
		for i, c := range draws {
			clusters = append(clusters, v1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("c%d", i)},
				Spec:       v1alpha1.ClusterSpec{Name: fmt.Sprintf("c%d", i), Server: c.server, Project: "p", Namespaces: c.namespaces},
			})
		}
		d := genDestination().Draw(t, "dest")
		if len(draws) > 0 && rapid.Bool().Draw(t, "own") {
			// The destination of one of the scoped clusters.
			c := rapid.SampledFrom(draws).Draw(t, "cluster")
			d.Server = normalizeServer(c.server)
			d.Namespace = rapid.SampledFrom(append(slices.Clone(propNamespaces), "*", "!default", "team-*", "team-a")).Draw(t, "ns")
		}
		if d.Server == "" && d.Name == "" {
			t.Skip("no cluster")
		}
		proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: v1alpha1.AppProjectSpec{Destinations: entries}}
		eff, _, err := EffectiveProject(context.Background(), proj, Lookups{Clusters: NewClusterIndex(clusters, true)})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		before, after := destPermitted(t, proj.Spec.Destinations, d), destPermitted(t, eff.Spec.Destinations, d)
		if before && !after {
			t.Fatalf("scoped %+v revoked %+v", draws, d)
		}
		if after && !before && !slices.ContainsFunc(draws, func(c scopedClusterDraw) bool {
			server := normalizeServer(c.server)
			return server != "" && server == d.Server && (len(c.namespaces) == 0 || slices.Contains(c.namespaces, d.Namespace))
		}) {
			t.Fatalf("scoped %+v widened %+v to %+v", draws, entries, d)
		}
	})
}

// ---- Sync windows ----

var propZones = []string{"UTC", "America/New_York", "Europe/Amsterdam", "Asia/Kolkata", "Australia/Lord_Howe", "Etc/GMT-3", "Etc/GMT+5"}

// Instants of 2026 biased toward the US and EU DST changes.
func genInstant() *rapid.Generator[time.Time] {
	transitions := []time.Time{
		time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC),   // US spring
		time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC),  // EU spring
		time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC), // EU autumn
		time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC),  // US autumn
	}
	return rapid.Custom(func(t *rapid.T) time.Time {
		if rapid.Bool().Draw(t, "nearDST") {
			base := rapid.SampledFrom(transitions).Draw(t, "transition")
			return base.Add(time.Duration(rapid.Int64Range(-48*3600, 48*3600).Draw(t, "offsetSec")) * time.Second)
		}
		start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		return start.Add(time.Duration(rapid.Int64Range(0, 365*24*3600).Draw(t, "sec")) * time.Second)
	})
}

func genDailyWindow(zones []string) *rapid.Generator[v1alpha1.SyncWindow] {
	return rapid.Custom(func(t *rapid.T) v1alpha1.SyncWindow {
		return v1alpha1.SyncWindow{
			Kind:         rapid.SampledFrom([]string{"allow", "deny"}).Draw(t, "kind"),
			Schedule:     fmt.Sprintf("%d %d * * *", rapid.IntRange(0, 59).Draw(t, "minute"), rapid.IntRange(0, 23).Draw(t, "hour")),
			Duration:     fmt.Sprintf("%dm", rapid.IntRange(1, 1439).Draw(t, "minutes")),
			TimeZone:     rapid.SampledFrom(zones).Draw(t, "tz"),
			ManualSync:   rapid.Bool().Draw(t, "manual"),
			SyncOverrun:  rapid.Bool().Draw(t, "overrun"),
			Applications: []string{"*"},
		}
	})
}

// The decision depends on the instant, not on the Location the caller expresses it in.
func TestPropertyWindowsLocationIndependent(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		ws := rapid.SliceOfN(genDailyWindow(propZones), 0, 4).Draw(t, "windows")
		now := genInstant().Draw(t, "now")
		loc, err := time.LoadLocation(rapid.SampledFrom(propZones).Draw(t, "callerZone"))
		if err != nil {
			t.Fatal(err)
		}
		manual := rapid.Bool().Draw(t, "manual")
		a, errA := CanSyncWithWindows(ws, manual, nil, now)
		b, errB := CanSyncWithWindows(ws, manual, nil, now.In(loc))
		if a != b || (errA == nil) != (errB == nil) {
			t.Fatalf("decision differs by caller location: %v/%v vs %v/%v", a, errA, b, errB)
		}
		actA, _ := ActiveSyncWindows(ws, now)
		actB, _ := ActiveSyncWindows(ws, now.In(loc))
		if len(actA) != len(actB) {
			t.Fatalf("active windows differ by caller location")
		}
	})
}

// Oracle for a daily "M H * * *" window: the offset in effect at now turns now into a local
// wall clock; the window is active strictly between a fire (H:M:00 today or a previous day)
// and fire + duration on that wall clock. Holds across DST changes, since the offset is the
// one at the evaluation instant.
func TestPropertyWindowMatchesWallClockOracle(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		w := genDailyWindow(propZones).Draw(t, "window")
		now := genInstant().Draw(t, "now")
		var minute, hour int
		if _, err := fmt.Sscanf(w.Schedule, "%d %d", &minute, &hour); err != nil {
			t.Fatal(err)
		}
		d, err := time.ParseDuration(w.Duration)
		if err != nil {
			t.Fatal(err)
		}
		loc, err := time.LoadLocation(w.TimeZone)
		if err != nil {
			t.Fatal(err)
		}
		wc := now.In(loc)
		naive := time.Date(wc.Year(), wc.Month(), wc.Day(), wc.Hour(), wc.Minute(), wc.Second(), wc.Nanosecond(), time.UTC)
		want := false
		for back := 0; back <= 1; back++ {
			fire := time.Date(wc.Year(), wc.Month(), wc.Day()-back, hour, minute, 0, 0, time.UTC)
			if fire.After(naive.Add(-d)) && fire.Before(naive) {
				want = true
			}
		}
		got, err := SyncWindowActive(w, now)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("window %+v at %s (wall clock %s): got %v, oracle %v", w, now, naive, got, want)
		}
	})
}

// A window in a fixed-offset zone behaves like the same window shifted to UTC.
func TestPropertyFixedOffsetZoneEquivalentToShiftedUTC(t *testing.T) {
	t.Parallel()
	zones := map[string]int{"Etc/GMT-3": 180, "Etc/GMT+5": -300, "Asia/Kolkata": 330}
	names := []string{"Etc/GMT-3", "Etc/GMT+5", "Asia/Kolkata"}
	rapid.Check(t, func(t *rapid.T) {
		w := genDailyWindow(names).Draw(t, "window")
		now := genInstant().Draw(t, "now")
		var minute, hour int
		if _, err := fmt.Sscanf(w.Schedule, "%d %d", &minute, &hour); err != nil {
			t.Fatal(err)
		}
		shifted := ((hour*60+minute-zones[w.TimeZone])%1440 + 1440) % 1440
		utc := w
		utc.TimeZone = "UTC"
		utc.Schedule = fmt.Sprintf("%d %d * * *", shifted%60, shifted/60)
		a, errA := SyncWindowActive(w, now)
		b, errB := SyncWindowActive(utc, now)
		if errA != nil || errB != nil || a != b {
			t.Fatalf("%+v vs %+v at %s: %v/%v %v/%v", w, utc, now, a, errA, b, errB)
		}
	})
}

// GetSyncWindowTimes agrees with SyncWindowActive around the reported boundaries.
func TestPropertyWindowTimesConsistent(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		w := genDailyWindow([]string{"UTC", "Etc/GMT-3", "Asia/Kolkata"}).Draw(t, "window")
		now := genInstant().Draw(t, "now").Truncate(time.Second)
		times, err := GetSyncWindowTimes(w, now)
		if err != nil {
			t.Fatal(err)
		}
		active := func(at time.Time) bool {
			ok, err := SyncWindowActive(w, at)
			if err != nil {
				t.Fatal(err)
			}
			return ok
		}
		if times.Active != active(now) {
			t.Fatalf("Active mismatch at %s", now)
		}
		if times.Active {
			if times.End.IsZero() {
				t.Fatalf("a daily window always closes")
			}
			if !times.End.After(now) || active(times.End) || !active(times.End.Add(-time.Second)) {
				t.Fatalf("bad end %s for %+v at %s", times.End, w, now)
			}
			return
		}
		if times.Start.Before(now) || active(times.Start) || !active(times.Start.Add(time.Second)) {
			t.Fatalf("bad start %s for %+v at %s", times.Start, w, now)
		}
		mid := now.Add(times.Start.Sub(now) / 2)
		if active(mid) {
			t.Fatalf("active before the reported start: %s", mid)
		}
	})
}

// Manual syncs are never more restricted than automated ones, and an operation start time
// (overrun) never restricts.
func TestPropertyCanSyncMonotonic(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		ws := rapid.SliceOfN(genDailyWindow(propZones), 0, 4).Draw(t, "windows")
		now := genInstant().Draw(t, "now")
		start := now.Add(-time.Duration(rapid.IntRange(1, 600).Draw(t, "ago")) * time.Minute)
		auto, err := CanSyncWithWindows(ws, false, nil, now)
		if err != nil {
			t.Fatal(err)
		}
		manual, _ := CanSyncWithWindows(ws, true, nil, now)
		if auto && !manual {
			t.Fatalf("manual more restricted than automated")
		}
		for _, m := range []bool{false, true} {
			plain, _ := CanSyncWithWindows(ws, m, nil, now)
			withStart, _ := CanSyncWithWindows(ws, m, &start, now)
			if plain && !withStart {
				t.Fatalf("overrun restricted a sync")
			}
		}
	})
}
