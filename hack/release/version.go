package main

import (
	"fmt"
	"regexp"
	"strconv"
)

// version is a release version MAJOR.MINOR.PATCH. Pre-releases are not produced.
type version struct{ Major, Minor, Patch int }

var versionRe = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func parseVersion(s string) (version, error) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return version{}, fmt.Errorf("invalid version %q (want MAJOR.MINOR.PATCH)", s)
	}
	var v version
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	return v, nil
}

func (v version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

func (v version) Tag() string { return "v" + v.String() }

func (v version) less(o version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// bump is the size of a version change.
type bump int

const (
	bumpNone bump = iota
	bumpPatch
	bumpMinor
	bumpMajor
)

// bumpFor is the change the commits call for: a breaking change is major, feat is minor, fix,
// perf and revert are patch; every other type (docs, test, chore, ci, build, refactor, style)
// does not make a release on its own.
func bumpFor(commits []commit) bump {
	b := bumpNone
	for _, c := range commits {
		var cb bump
		switch {
		case c.Type == "":
			continue
		case c.Breaking:
			cb = bumpMajor
		case c.Type == "feat":
			cb = bumpMinor
		case c.Type == "fix", c.Type == "perf", c.Type == "revert":
			cb = bumpPatch
		}
		b = max(b, cb)
	}
	return b
}

// apply returns the next version. Before 1.0.0 a breaking change bumps the minor version, as
// semantic versioning leaves 0.y.z free to change: 1.0.0 is chosen explicitly (-version).
func (v version) apply(b bump) version {
	if b == bumpMajor && v.Major == 0 {
		b = bumpMinor
	}
	switch b {
	case bumpMajor:
		return version{v.Major + 1, 0, 0}
	case bumpMinor:
		return version{v.Major, v.Minor + 1, 0}
	case bumpPatch:
		return version{v.Major, v.Minor, v.Patch + 1}
	}
	return v
}
