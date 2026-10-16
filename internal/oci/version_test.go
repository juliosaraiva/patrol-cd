package oci_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Masterminds/semver/v3"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/internal/oci"
)

var baselineTags = []string{"0.7.1", "0.5.4", "0.5.3", "0.7.2", "0.5.2", "0.5.1", "0.5.0", "2024.03-LTS-RC19"}

func TestMaxVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		revision string
		tags     []string
		want     string
		wantErr  error
	}{
		{name: "exact", revision: "0.5.3", tags: baselineTags, want: "0.5.3"},
		{name: "exact non-semver tag", revision: "2024.03-LTS-RC19", tags: baselineTags, want: "2024.03-LTS-RC19"},
		{name: "exact missing is a pin", revision: "99.99", tags: nil, want: "99.99"},
		{name: "exact keeps v prefix", revision: "v1.2", tags: nil, want: "v1.2"},
		{name: "greater than", revision: "> 0.5.3", tags: baselineTags, want: "0.7.2"},
		{name: "greater than zero", revision: "> 0.0.0", tags: baselineTags, want: "0.7.2"},
		{name: "range", revision: ">0.5.0,<0.7.0", tags: baselineTags, want: "0.5.4"},
		{name: "wildcard patch", revision: "0.7.*", tags: baselineTags, want: "0.7.2"},
		{name: "star", revision: "*", tags: baselineTags, want: "0.7.2"},
		{name: "caret", revision: "^0.5", tags: baselineTags, want: "0.5.4"},
		{name: "tilde", revision: "~0.5.1", tags: baselineTags, want: "0.5.4"},
		{name: "prerelease excluded by default", revision: "*", tags: []string{"1.0.0", "2.0.0-rc.1"}, want: "1.0.0"},
		{name: "prerelease included with -0", revision: "*-0", tags: []string{"1.0.0", "2.0.0-rc.1"}, want: "2.0.0-rc.1"},
		{name: "build metadata tag", revision: "1.*", tags: []string{"1.0.0+build.1", "0.9.0"}, want: "1.0.0+build.1"},
		{name: "non-semver tags ignored", revision: "1.*", tags: []string{"latest", "main", "1.1.0", "1.0.0"}, want: "1.1.0"},
		{name: "ties keep first", revision: "1.*", tags: []string{"v1.2.0", "1.2.0"}, want: "v1.2.0"},
		{name: "literal non-constraint tag", revision: "latest", tags: []string{"latest", "1.0.0"}, want: "latest"},
		{name: "constraint without match", revision: "0.7.*", tags: nil, wantErr: oci.ErrNoMatchingVersion},
		{name: "constraint without match in tags", revision: ">5", tags: baselineTags, wantErr: oci.ErrNoMatchingVersion},
		{name: "invalid constraint not in tags", revision: "latest", tags: []string{"1.0.0"}, wantErr: oci.ErrInvalidConstraint},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := oci.MaxVersion(tt.revision, tt.tags)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("MaxVersion(%q) error = %v, want %v", tt.revision, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("MaxVersion(%q) error = %v", tt.revision, err)
			}
			if got != tt.want {
				t.Fatalf("MaxVersion(%q) = %q, want %q", tt.revision, got, tt.want)
			}
		})
	}
}

func TestMaxVersionErrorMessages(t *testing.T) {
	t.Parallel()
	_, err := oci.MaxVersion("0.7.*", []string{"1.0.0", "2.0.0"})
	if err == nil || err.Error() != "version matching constraint not found in 2 tags" {
		t.Fatalf("unexpected error %v", err)
	}
	_, err = oci.MaxVersion("not a constraint", []string{"1.0.0"})
	if err == nil || len(err.Error()) < len("failed to determine semver constraint:") ||
		err.Error()[:len("failed to determine semver constraint:")] != "failed to determine semver constraint:" {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestIsConstraint(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"0.5.3":            false,
		"v1.2":             false,
		"2024.03-LTS-RC19": false,
		"= 0.5.3":          true,
		"> 0.5.3":          true,
		">0.5.0,<0.7.0":    true,
		"0.7.*":            true,
		"*":                true,
		"latest":           false,
	}
	for in, want := range tests {
		if got := oci.IsConstraint(in); got != want {
			t.Errorf("IsConstraint(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestMaxVersionProperties checks, for random tag sets and constraints, that the
// result satisfies the constraint, is one of the tags, and that no satisfying tag
// is greater; and that tag order does not change the selected version.
func TestMaxVersionProperties(t *testing.T) {
	t.Parallel()
	genVersion := rapid.Custom(func(t *rapid.T) string {
		v := fmt.Sprintf("%d.%d.%d", rapid.IntRange(0, 4).Draw(t, "maj"), rapid.IntRange(0, 4).Draw(t, "min"), rapid.IntRange(0, 4).Draw(t, "pat"))
		if rapid.IntRange(0, 4).Draw(t, "pre") == 0 {
			v += "-rc." + fmt.Sprint(rapid.IntRange(0, 3).Draw(t, "rc"))
		}
		return v
	})
	genConstraint := rapid.SampledFrom([]string{"*", "*-0", "1.*", ">=1.0.0 <3.0.0", "~2.1", "^0.3", ">4", "2.x", "< 1.2.3"})
	rapid.Check(t, func(t *rapid.T) {
		tags := rapid.SliceOfN(rapid.OneOf(genVersion, rapid.SampledFrom([]string{"latest", "main"})), 0, 12).Draw(t, "tags")
		cons := genConstraint.Draw(t, "constraint")
		got, err := oci.MaxVersion(cons, tags)
		c, cerr := semver.NewConstraint(cons)
		if cerr != nil {
			t.Fatalf("bad generator constraint %q: %v", cons, cerr)
		}
		var best *semver.Version
		for _, tg := range tags {
			v, err := semver.NewVersion(tg)
			if err != nil || !c.Check(v) {
				continue
			}
			if best == nil || v.GreaterThan(best) {
				best = v
			}
		}
		if best == nil {
			if !errors.Is(err, oci.ErrNoMatchingVersion) {
				t.Fatalf("MaxVersion(%q, %v) = %q, %v; want ErrNoMatchingVersion", cons, tags, got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("MaxVersion(%q, %v) error %v", cons, tags, err)
		}
		gv, perr := semver.NewVersion(got)
		if perr != nil || !c.Check(gv) || !gv.Equal(best) {
			t.Fatalf("MaxVersion(%q, %v) = %q, want a tag equal to %s", cons, tags, got, best)
		}
		// Permutation invariance of the selected version (not of the spelling).
		perm := rapid.Permutation(tags).Draw(t, "perm")
		got2, err := oci.MaxVersion(cons, perm)
		if err != nil {
			t.Fatalf("permuted MaxVersion error %v", err)
		}
		if gv2, _ := semver.NewVersion(got2); gv2 == nil || !gv2.Equal(gv) {
			t.Fatalf("permutation changed result: %q vs %q", got, got2)
		}
	})
}

func TestTagVersionRoundTrip(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		v := rapid.StringMatching(`[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}(-[a-z0-9.]{1,8})?(\+[a-z0-9.]{1,8})?`).Draw(t, "v")
		tg := oci.TagFromVersion(v)
		for _, r := range tg {
			if r == '+' {
				t.Fatalf("tag %q contains '+'", tg)
			}
		}
		if back := oci.VersionFromTag(tg); back != v {
			t.Fatalf("round trip %q -> %q -> %q", v, tg, back)
		}
	})
}
