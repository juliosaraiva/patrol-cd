package template

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestReplaceFasttemplate(t *testing.T) {
	t.Parallel()
	params := map[string]any{
		"cluster":     "prod",
		"values.env":  "staging",
		"number":      float64(3),
		"path[0]":     "apps",
		"path.base":   "guestbook",
		"empty-value": "",
	}
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "tag without spaces", in: "{{cluster}}", want: "prod"},
		{name: "tag with spaces", in: "{{ cluster }}", want: "prod"},
		{name: "dotted key", in: "app-{{values.env}}", want: "app-staging"},
		{name: "indexed key", in: "{{path[0]}}/{{path.base}}", want: "apps/guestbook"},
		{name: "unknown tag kept with its spacing", in: "{{ unknown }}-{{other}}", want: "{{ unknown }}-{{other}}"},
		{name: "non-string parameter not substituted", in: "{{number}}", want: "{{number}}"},
		{name: "empty tag kept", in: "{{}}", want: "{{}}"},
		{name: "empty string value", in: "x{{empty-value}}y", want: "xy"},
		{name: "no tags", in: "plain", want: "plain"},
		{name: "tags on different lines are not templated", in: "{{\ncluster}}", want: "{{\ncluster}}"},
		{name: "unclosed tag", in: "{{cluster}} {{other}", wantErr: "invalid template"},
		{name: "go syntax is not evaluated", in: "{{ .cluster }}", want: "{{ .cluster }}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Replace(tc.in, params, Options{})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestReplaceGoTemplate(t *testing.T) {
	t.Parallel()
	params := map[string]any{
		"name": "My_App.Name-",
		"path": map[string]any{"basename": "guestbook", "segments": []any{"apps", "guestbook"}},
		"metadata": map[string]any{
			"labels": map[string]string{"env": "prod"},
		},
		"values": map[string]any{"a": map[string]any{"b": "deep"}},
		"count":  3,
	}
	tests := []struct {
		name    string
		in      string
		opts    []string
		want    string
		wantErr string
	}{
		{name: "field access", in: "{{.path.basename}}", want: "guestbook"},
		{name: "index", in: `{{index .metadata.labels "env"}}`, want: "prod"},
		{name: "segments", in: `{{index .path.segments 0}}`, want: "apps"},
		{name: "missing key default", in: "{{.nope}}", want: "<no value>"},
		{name: "missing key zero", in: "{{.nope}}", opts: []string{"missingkey=zero"}, want: "<no value>"},
		{name: "missing key error", in: "{{.nope}}", opts: []string{"missingkey=error"}, wantErr: "map has no entry for key"},
		{name: "invalid option", in: "{{.name}}", opts: []string{"missingkey=bogus"}, wantErr: "invalid goTemplateOptions entry"},
		{name: "unknown option", in: "{{.name}}", opts: []string{"nope"}, wantErr: "invalid goTemplateOptions entry"},
		{name: "parse error", in: "{{ .name ", wantErr: "failed to parse template"},
		{name: "env removed", in: `{{ env "HOME" }}`, wantErr: `function "env" not defined`},
		{name: "expandenv removed", in: `{{ expandenv "$HOME" }}`, wantErr: `function "expandenv" not defined`},
		{name: "getHostByName removed", in: `{{ getHostByName "localhost" }}`, wantErr: `function "getHostByName" not defined`},
		{name: "normalize", in: "{{ normalize .name }}", want: "my-app.name"},
		{name: "slugify default", in: `{{ slugify "feat/My Branch" }}`, want: "feat-my-branch"},
		{name: "sprig dig", in: `{{ dig "a" "b" "none" .values }}`, want: "deep"},
		{name: "sprig default", in: `{{ default "dflt" .missing }}`, want: "dflt"},
		{name: "sprig upper", in: `{{ upper .path.basename }}`, want: "GUESTBOOK"},
		{name: "sprig b64enc", in: `{{ b64enc "abc" }}`, want: "YWJj"},
		{name: "sprig toJson", in: `{{ toJson .path.segments }}`, want: `["apps","guestbook"]`},
		{name: "toYaml", in: `{{ toYaml .metadata }}`, want: "labels:\n  env: prod"},
		{name: "fromYaml", in: `{{ (fromYaml "a: b").a }}`, want: "b"},
		{name: "fromYamlArray", in: `{{ index (fromYamlArray "[x, z]") 1 }}`, want: "z"},
		{name: "fromYaml invalid", in: `{{ fromYaml "a: [" }}`, wantErr: "failed to execute go template"},
		{name: "no actions", in: "plain }} text", want: "plain }} text"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Replace(tc.in, params, Options{GoTemplate: true, GoTemplateOptions: tc.opts})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestReplaceOutputLimit(t *testing.T) {
	t.Parallel()
	_, err := Replace(`{{ repeat 2000000 "x" }}`, map[string]any{"a": "b"}, Options{GoTemplate: true})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrOutputTooLarge), "got %v", err)

	big := strings.Repeat("y", MaxRenderedBytes)
	_, err = Replace("{{v}}{{v}}", map[string]any{"v": big}, Options{})
	require.ErrorIs(t, err, ErrOutputTooLarge)
}

func TestSanitizeName(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"My_App.Name-":           "my-app.name",
		"My_Cluster":             "my-cluster",
		"-.leading.trailing":     "leading.trailing",
		"UPPER":                  "upper",
		"a/b c":                  "a-b-c",
		strings.Repeat("a", 300): strings.Repeat("a", 253),
	}
	for in, want := range tests {
		assert.Equal(t, want, SanitizeName(in), in)
	}
}

func TestSlugify(t *testing.T) {
	t.Parallel()
	// Upstream applicationset/utils TestSlugify vectors.
	long := "feat/a_really+long_pull_request_name_to_test_argo_slugification_and_branch_name_shortening_feature"
	oneWord := "feat/areallylongpullrequestnametotestargoslugificationandbranchnameshorteningfeature"
	tests := []struct {
		in    string
		smart bool
		max   int
		want  string
	}{
		{in: long, smart: false, max: 50, want: "feat-a-really-long-pull-request-name-to-test-argo"},
		{in: long, smart: true, max: 53, want: "feat-a-really-long-pull-request-name-to-test-argo"},
		{in: oneWord, smart: true, max: 50, want: "feat"},
		{in: oneWord, smart: false, max: 50, want: "feat-areallylongpullrequestnametotestargoslugifica"},
	}
	for _, tc := range tests {
		got, err := Slugify(tc.max, tc.smart, tc.in)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
	got, err := Slugify(23, "a-much-longer-branch-name-than-twenty-three")
	require.NoError(t, err)
	assert.Equal(t, "a-much-longer-branch", got)

	_, err = Slugify("x", "y")
	require.ErrorContains(t, err, "first argument must be an int")
	_, err = Slugify(1, 2, "y")
	require.ErrorContains(t, err, "second argument must be a bool")
	_, err = Slugify(1)
	require.ErrorContains(t, err, "last argument must be a string")
}

func TestSlugPullRequestBranch(t *testing.T) {
	t.Parallel()
	opts := SlugOptions{MaxLength: 50, SmartTruncate: true, Substitutions: map[string]string{"_": "-"}}
	// Upstream applicationset/generators TestPullRequestGithubGenerateParams vectors.
	assert.Equal(t, "feat-areally-long-pull-request-name-to-test-argo",
		Slug("feat/areally+long_pull_request_name_to_test_argo_slugification_and_branch_name_shortening_feature", opts))
	assert.Equal(t, "feat-anotherreally-long-pull-request-name-to-test",
		Slug("feat/anotherreally_long_pull_request_name_to_test_argo_slugification_and_branch_name_shortening_feature", opts))
	assert.Equal(t, "a-very-short-sha", Slug("a-very-short-sha", opts))
	assert.Equal(t, "feature-branch-abc", Slug("Feature_Branch/ABC", opts))
	assert.Equal(t, "cafe-and-creme", Slug("Café & Crème", opts))
}

var slugChars = regexp.MustCompile(`^[a-z0-9_-]*$`)

func TestPropertySanitizeAndSlug(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "s")
		n := SanitizeName(s)
		if len(n) > maxDNSNameLength {
			t.Fatalf("sanitized name too long: %d", len(n))
		}
		if !regexp.MustCompile(`^[-a-z0-9.]*$`).MatchString(n) {
			t.Fatalf("sanitized name %q has invalid characters", n)
		}
		if SanitizeName(n) != n {
			t.Fatalf("SanitizeName is not idempotent for %q", s)
		}
		maxLen := rapid.IntRange(1, 80).Draw(t, "max")
		smart := rapid.Bool().Draw(t, "smart")
		slug := Slug(s, SlugOptions{MaxLength: maxLen, SmartTruncate: smart})
		if len(slug) > maxLen {
			t.Fatalf("slug %q longer than %d", slug, maxLen)
		}
		if !slugChars.MatchString(slug) {
			t.Fatalf("slug %q has invalid characters", slug)
		}
	})
}
