package core

import (
	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// DefaultRevisionHistoryLimit is the number of history entries and terminal Operations kept
// when spec.revisionHistoryLimit is unset.
const DefaultRevisionHistoryLimit = 10

// RevisionHistoryLimit returns the Application's history limit (SYNC-090): the default when
// unset, 0 for negative values.
func RevisionHistoryLimit(app *v1alpha1.Application) int {
	if app == nil || app.Spec.RevisionHistoryLimit == nil {
		return DefaultRevisionHistoryLimit
	}
	if n := *app.Spec.RevisionHistoryLimit; n > 0 {
		return int(n)
	}
	return 0
}

// AppendHistory appends entry with the next id (last id + 1, 0 for an empty history) and keeps
// the newest limit entries (SYNC-089/090). Ids keep increasing after truncation. history is
// not modified.
func AppendHistory(history v1alpha1.RevisionHistories, entry v1alpha1.RevisionHistory, limit int) v1alpha1.RevisionHistories {
	var next int64
	if n := len(history); n > 0 {
		next = history[n-1].ID + 1
	}
	entry.ID = next
	out := make(v1alpha1.RevisionHistories, 0, len(history)+1)
	out = append(out, history...)
	out = append(out, entry)
	if limit < 0 {
		limit = 0
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// HistoryRecorded reports whether history has an entry produced by the named Operation.
func HistoryRecorded(history v1alpha1.RevisionHistories, operationName string) bool {
	for _, h := range history {
		if h.OperationName == operationName {
			return true
		}
	}
	return false
}

// FindHistory returns the history entry with id.
func FindHistory(history v1alpha1.RevisionHistories, id int64) (*v1alpha1.RevisionHistory, bool) {
	for i := range history {
		if history[i].ID == id {
			return &history[i], true
		}
	}
	return nil, false
}
