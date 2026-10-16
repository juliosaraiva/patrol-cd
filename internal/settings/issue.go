package settings

import (
	"cmp"
	"fmt"
	"slices"
)

// Severity classifies an Issue.
type Severity string

const (
	// SeverityError marks a value that could not be used. The key falls back to the value the
	// previous snapshot accepted, or to its default (see Parse for the exceptions).
	SeverityError Severity = "error"
	// SeverityWarning marks a value that is used but is probably not what the author meant
	// (non-canonical booleans, deprecated or unknown keys, unresolved references, ...).
	SeverityWarning Severity = "warning"
)

// Issue is one problem found in the settings.
type Issue struct {
	// Source is the logical settings object.
	Source Source `json:"source"`
	// Object is the object name (for example "patrol-cm"); empty when not tied to one object.
	Object string `json:"object,omitempty"`
	// Key is the data key the issue is about; empty when it concerns the whole object.
	Key string `json:"key,omitempty"`
	// Severity is SeverityError or SeverityWarning.
	Severity Severity `json:"severity"`
	// Message explains the problem. It never contains Secret values.
	Message string `json:"message"`
}

// String formats the issue as "<object>: <key>: <severity>: <message>".
func (i Issue) String() string {
	obj := i.Object
	if obj == "" {
		obj = string(i.Source)
	}
	if i.Key == "" {
		return fmt.Sprintf("%s: %s: %s", obj, i.Severity, i.Message)
	}
	return fmt.Sprintf("%s: %s: %s: %s", obj, i.Key, i.Severity, i.Message)
}

// IsError reports whether the issue has error severity.
func (i Issue) IsError() bool { return i.Severity == SeverityError }

// sortIssues orders issues by object, key, severity and message, and removes duplicates.
func sortIssues(in []Issue) []Issue {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b Issue) int {
		return cmp.Or(
			cmp.Compare(a.Object, b.Object),
			cmp.Compare(a.Source, b.Source),
			cmp.Compare(a.Key, b.Key),
			cmp.Compare(a.Severity, b.Severity),
			cmp.Compare(a.Message, b.Message),
		)
	})
	return slices.Compact(out)
}

// HasErrors reports whether issues contains an error-severity issue.
func HasErrors(issues []Issue) bool {
	return slices.ContainsFunc(issues, Issue.IsError)
}
