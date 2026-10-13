package oci

import (
	"fmt"
	"slices"

	"github.com/Masterminds/semver/v3"
)

// IsVersion reports whether s parses as a semantic version. Masterminds coercion
// applies, so "1.2", "v1.2.0" and "1" are versions (pins), not constraints.
func IsVersion(s string) bool {
	_, err := semver.NewVersion(s)
	return err == nil
}

// IsConstraint reports whether s is a semver constraint that is not also an exact
// version (for example "1.2.*", ">=1.0.0 <2.0.0", "^1.2", "*").
func IsConstraint(s string) bool {
	if IsVersion(s) {
		return false
	}
	_, err := semver.NewConstraint(s)
	return err == nil
}

// MaxVersion resolves revision against tags with the baseline semantics:
//
//   - an exact version is returned as written, without consulting tags;
//   - a semver constraint returns the highest tag satisfying it (ties keep the first
//     tag seen); tags that are not semantic versions are ignored; pre-releases only
//     match constraints that allow them (for example "*-0");
//   - any other string must equal one of the tags literally.
//
// Errors carry ErrNoMatchingVersion or ErrInvalidConstraint and the upstream
// messages ("version matching constraint not found in N tags",
// "failed to determine semver constraint: ...").
func MaxVersion(revision string, tags []string) (string, error) {
	if v, err := semver.NewVersion(revision); err == nil {
		return v.Original(), nil
	}
	constraint, err := semver.NewConstraint(revision)
	if err != nil {
		if slices.Contains(tags, revision) {
			return revision, nil
		}
		return "", tag(ErrInvalidConstraint, fmt.Errorf("failed to determine semver constraint: %w", err))
	}
	var best *semver.Version
	for _, t := range tags {
		v, err := semver.NewVersion(t)
		if err != nil {
			continue
		}
		if !constraint.Check(v) {
			continue
		}
		if best == nil || v.GreaterThan(best) {
			best = v
		}
	}
	if best == nil {
		return "", tag(ErrNoMatchingVersion, fmt.Errorf("version matching constraint not found in %d tags", len(tags)))
	}
	return best.Original(), nil
}
