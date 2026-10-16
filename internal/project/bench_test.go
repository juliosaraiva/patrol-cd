package project

import (
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Worst case of GetSyncWindowTimes: an every-minute schedule whose occurrences never stop
// overlapping, so the merge walk hits maxMergedOccurrences.
func BenchmarkGetSyncWindowTimesNeverCloses(b *testing.B) {
	w := v1alpha1.SyncWindow{Kind: "allow", Schedule: "* * * * *", Duration: "1h", Applications: []string{"*"}}
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	for b.Loop() {
		if _, err := GetSyncWindowTimes(w, now); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCanSyncWithWindows(b *testing.B) {
	ws := v1alpha1.SyncWindows{
		{Kind: "allow", Schedule: "0 8 * * 1-5", Duration: "10h", TimeZone: "Europe/Amsterdam", Applications: []string{"*"}},
		{Kind: "deny", Schedule: "0 22 * * *", Duration: "1h", Namespaces: []string{"prod-*"}},
	}
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Hour)
	for b.Loop() {
		if _, err := CanSyncWithWindows(ws, false, &start, now); err != nil {
			b.Fatal(err)
		}
	}
}
