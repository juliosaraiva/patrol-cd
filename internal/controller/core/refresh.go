package core

import (
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// CompareLevel is how much a refresh re-evaluates (upstream CompareWith).
type CompareLevel int

const (
	// CompareNone requests no comparison.
	CompareNone CompareLevel = iota
	// CompareRecent compares at the revisions of the last comparison, without resolving
	// targetRevision (live-state changes of managed resources).
	CompareRecent
	// CompareLatest resolves targetRevision with the repo-server revision cache.
	CompareLatest
	// CompareForceResolve resolves targetRevision bypassing the revision cache.
	CompareForceResolve
)

// String returns the level name used in logs.
func (l CompareLevel) String() string {
	switch l {
	case CompareRecent:
		return "recent"
	case CompareLatest:
		return "latest"
	case CompareForceResolve:
		return "force-resolve"
	default:
		return "none"
	}
}

// RefreshRequester asks the Application reconciler to compare an Application at a level, now
// or after a delay. Requests coalesce to the highest level. The Operation reconciler uses it
// after an operation completed (latest for automated, force-resolve for manual syncs).
type RefreshRequester interface {
	RequestRefresh(app types.NamespacedName, level CompareLevel, after time.Duration)
}

// NopRefresher discards requests.
type NopRefresher struct{}

// RequestRefresh implements RefreshRequester.
func (NopRefresher) RequestRefresh(types.NamespacedName, CompareLevel, time.Duration) {}
