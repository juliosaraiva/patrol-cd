package git

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// MaxVersion returns the highest tag satisfying the semver constraint revision,
// mirroring upstream util/versions.MaxVersion:
//
//   - only tags starting with tagPrefix are considered; the prefix is stripped
//     before semver evaluation and re-added to the result;
//   - an exact version ("v1.0.0", "1.0") is a pin: tagPrefix+revision is returned
//     without consulting the tags;
//   - a revision that is neither a version nor a constraint is returned unchanged
//     when it equals a (stripped) tag, otherwise an error is returned; with a
//     non-empty tagPrefix such a revision is always an error;
//   - tags that are not semantic versions are ignored; a tag that matches the
//     semver pattern but fails stricter checks (for example a leading zero in a
//     prerelease) fails the whole evaluation, as upstream;
//   - no matching tag is an error.
//
// Callers resolving git revisions ignore the error and resolve the original
// string literally, as upstream does.
func MaxVersion(revision string, tags []string, tagPrefix string) (string, error) {
	var stripped []string
	for _, t := range tags {
		if tagPrefix == "" {
			stripped = append(stripped, t)
		} else if s, ok := strings.CutPrefix(t, tagPrefix); ok {
			stripped = append(stripped, s)
		}
	}
	if v, err := semver.NewVersion(revision); err == nil {
		return tagPrefix + v.Original(), nil
	}
	constraints, err := semver.NewConstraint(revision)
	if err != nil {
		if tagPrefix != "" {
			return "", fmt.Errorf("tagPrefix %q requires targetRevision to be a semver constraint, but %q is not a valid constraint",
				tagPrefix, truncateForMessage(revision))
		}
		if slices.Contains(stripped, revision) {
			return revision, nil
		}
		return "", fmt.Errorf("failed to determine semver constraint: %w", err)
	}
	var best *semver.Version
	for _, t := range stripped {
		v, err := semver.NewVersion(t)
		if errors.Is(err, semver.ErrInvalidSemVer) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("invalid semver version in tags: %w", err)
		}
		if constraints.Check(v) && (best == nil || v.GreaterThan(best)) {
			best = v
		}
	}
	if best == nil {
		return "", fmt.Errorf("version matching constraint not found in %d tags", len(stripped))
	}
	return tagPrefix + best.Original(), nil
}

// IsConstraint reports whether revision is a semver constraint that is not an
// exact version (upstream versions.IsConstraint).
func IsConstraint(revision string) bool {
	if _, err := semver.NewVersion(revision); err == nil {
		return false
	}
	_, err := semver.NewConstraint(revision)
	return err == nil
}
