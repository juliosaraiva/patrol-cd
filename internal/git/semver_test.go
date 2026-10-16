package git

import (
	"slices"
	"strings"
	"testing"

	"github.com/Masterminds/semver/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestMaxVersion(t *testing.T) {
	t.Parallel()
	tags := []string{"0.7.1", "0.5.4", "0.5.3", "0.7.2", "0.5.2", "0.5.1", "0.5.0", "2024.03-LTS-RC19"}
	prefixed := []string{"prod/v1.0.0", "prod/v1.0.1", "prod/v1.1.0", "staging/v1.0.0", "staging/v2.0.0", "v3.0.0", "app1/v1.0.0-rc.1"}
	tests := []struct {
		name, revision string
		tags           []string
		prefix         string
		want           string
		wantErr        string
	}{
		{"exact", "0.5.3", tags, "", "0.5.3", ""},
		{"exact nonsemver tag", "2024.03-LTS-RC19", tags, "", "2024.03-LTS-RC19", ""},
		{"exact missing is still a pin", "99.99", nil, "", "99.99", ""},
		{"greater than", "> 0.5.3", tags, "", "0.7.2", ""},
		{"range", ">0.5.0,<0.7.0", tags, "", "0.5.4", ""},
		{"wildcard", "0.7.*", tags, "", "0.7.2", ""},
		{"star", "*", tags, "", "0.7.2", ""},
		{"constraint missing", "0.7.*", nil, "", "", "version matching constraint not found in 0 tags"},
		{"branch name not a tag", "main", tags, "", "", "failed to determine semver constraint"},
		{"literal non-constraint tag", "release-candidate", []string{"release-candidate"}, "", "release-candidate", ""},
		{"prefix patch wildcard", "v1.0.*", prefixed, "prod/", "prod/v1.0.1", ""},
		{"prefix minor wildcard", "v1.*", prefixed, "prod/", "prod/v1.1.0", ""},
		{"prefix star", "*", prefixed, "prod/", "prod/v1.1.0", ""},
		{"prefix x syntax", "v1.x.x", prefixed, "prod/", "prod/v1.1.0", ""},
		{"prefix other namespace", "*", prefixed, "staging/", "staging/v2.0.0", ""},
		{"prefix exact pin", "v1.0", prefixed, "prod/", "prod/v1.0", ""},
		{"prefix prerelease", ">=v1.0.0-0", prefixed, "app1/", "app1/v1.0.0-rc.1", ""},
		{"prefix requires constraint", "main", prefixed, "prod/", "",
			`tagPrefix "prod/" requires targetRevision to be a semver constraint, but "main" is not a valid constraint`},
		{"prefix no match", "v9.*", prefixed, "prod/", "", "version matching constraint not found in 3 tags"},
		{"v prefix accepted by parser", "1.2.*", []string{"v1.2.3", "v1.3.0"}, "", "v1.2.3", ""},
		{"prerelease excluded by default", ">=1.0.0", []string{"v1.0.0", "v1.1.0-rc.1"}, "", "v1.0.0", ""},
		{"prerelease with -0", ">=1.0.0-0", []string{"v1.0.0", "v1.1.0-rc.1"}, "", "v1.1.0-rc.1", ""},
		{"strictly invalid tag fails evaluation", "*", []string{"1.0.0", "1.2.3-01"}, "", "", "invalid semver version in tags"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := MaxVersion(tt.revision, tt.tags, tt.prefix)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsConstraint(t *testing.T) {
	t.Parallel()
	assert.False(t, IsConstraint("0.5.3"))
	assert.False(t, IsConstraint("v1.0"))
	assert.True(t, IsConstraint("= 0.5.3"))
	assert.True(t, IsConstraint(">0.5.0,<0.7.0"))
	assert.True(t, IsConstraint("*"))
	assert.False(t, IsConstraint("main"))
}

// TestMaxVersionProperties: the result of a constraint is always one of the
// candidate tags, satisfies the constraint, and no satisfying tag is greater.
func TestMaxVersionProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		genVersion := rapid.Custom(func(t *rapid.T) string {
			v := rapid.SampledFrom([]string{"", "v"}).Draw(t, "v") +
				strings.Join([]string{
					rapid.StringMatching(`0|[1-9][0-9]?`).Draw(t, "major"),
					rapid.StringMatching(`0|[1-9][0-9]?`).Draw(t, "minor"),
					rapid.StringMatching(`0|[1-9][0-9]?`).Draw(t, "patch"),
				}, ".")
			if rapid.Bool().Draw(t, "pre") {
				v += "-rc." + rapid.StringMatching(`[1-9]`).Draw(t, "rc")
			}
			return v
		})
		prefix := rapid.SampledFrom([]string{"", "app/", "team-a/"}).Draw(t, "prefix")
		tags := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) string {
			p := rapid.SampledFrom([]string{"", "app/", "team-a/", "other/"}).Draw(t, "tagPrefix")
			if rapid.Bool().Draw(t, "garbage") {
				return p + rapid.StringMatching(`[a-z]{1,6}`).Draw(t, "name")
			}
			return p + genVersion.Draw(t, "version")
		}), 0, 12).Draw(t, "tags")
		constraintStr := rapid.SampledFrom([]string{"*", ">=1.0.0", "<5.0.0", "1.x", ">=0.0.0-0", "~1.2", "^2.0.0"}).Draw(t, "constraint")
		constraint, err := semver.NewConstraint(constraintStr)
		if err != nil {
			t.Fatalf("bad generated constraint %q", constraintStr)
		}

		got, err := MaxVersion(constraintStr, tags, prefix)
		var candidates []*semver.Version
		for _, tag := range tags {
			s, ok := strings.CutPrefix(tag, prefix)
			if !ok {
				continue
			}
			if v, err := semver.NewVersion(s); err == nil && constraint.Check(v) {
				candidates = append(candidates, v)
			}
		}
		if len(candidates) == 0 {
			if err == nil {
				t.Fatalf("MaxVersion(%q, %v, %q) = %q, want error", constraintStr, tags, prefix, got)
			}
			return
		}
		if err != nil {
			t.Fatalf("MaxVersion(%q, %v, %q): %v", constraintStr, tags, prefix, err)
		}
		if !slices.Contains(tags, got) {
			t.Fatalf("result %q is not one of the tags %v", got, tags)
		}
		gv, err := semver.NewVersion(strings.TrimPrefix(got, prefix))
		if err != nil || !constraint.Check(gv) {
			t.Fatalf("result %q does not satisfy %q", got, constraintStr)
		}
		for _, c := range candidates {
			if c.GreaterThan(gv) {
				t.Fatalf("result %q is not the maximum (found %s)", got, c.Original())
			}
		}
	})
}
