package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

type isoKey struct {
	key     string
	valid   [2]string
	invalid string
	get     func(*Settings) any
}

var isoKeys = []isoKey{
	{KeyUserSessionDuration, [2]string{"1h", "2h"}, "x", func(s *Settings) any { return s.UserSessionDuration() }},
	{KeyPasswordPattern, [2]string{"^a$", "^b+$"}, "([", func(s *Settings) any { return s.PasswordPattern() }},
	{KeyResourceRespectRBAC, [2]string{"normal", "strict"}, "bogus", func(s *Settings) any { return s.RespectRBAC() }},
	{KeyTrackingMethod, [2]string{"label", "annotation+label"}, "magic", func(s *Settings) any { return s.TrackingMethod() }},
	{KeyIgnoreResourceUpdates, [2]string{"false", "true"}, "maybe", func(s *Settings) any { return s.IgnoreResourceUpdatesEnabled() }},
	{KeyReconciliationTimeout, [2]string{"10s", "1m"}, "soon", func(s *Settings) any { return s.Reconciliation().Timeout }},
	{KeyWebhookMaxPayloadSizeMB, [2]string{"1", "2"}, "x", func(s *Settings) any { return s.Webhook().MaxPayloadSize }},
	{KeyMaxPodLogsToRender, [2]string{"3", "4"}, "y", func(s *Settings) any { return s.MaxPodLogsToRender() }},
	{KeyInstanceLabelKey, [2]string{"example.com/app", "app"}, "bad key!", func(s *Settings) any { return s.InstanceLabelKey() }},
	{KeyWebhookJitterThreshold, [2]string{"1", "2"}, "-1", func(s *Settings) any { return s.Webhook().RefreshJitterThreshold }},
	{KeyDisableFineGrainedRBAC, [2]string{"false", "true"}, "sometimes", func(s *Settings) any { return s.ApplicationFineGrainedRBACInheritanceDisabled() }},
	{KeyResourceExclusions, [2]string{"- kinds: [A]", "- kinds: [B]"}, "- kinds: [", func(s *Settings) any {
		var kinds []string
		for _, r := range s.ResourceFilter().ResourceExclusions {
			kinds = append(kinds, r.Kinds...)
		}
		return kinds
	}},
	{KeyResourceCompareOptions, [2]string{"ignoreAggregatedRoles: true", "ignoreResourceStatusField: crd"}, "ignoreAggregatedRoles: [", func(s *Settings) any { return s.CompareOptions() }},
	{KeyApplicationLinks, [2]string{"- url: https://a\n  title: a", "- url: https://b\n  title: b"}, "- url: [", func(s *Settings) any {
		var titles []string
		for _, l := range s.DeepLinks(KeyApplicationLinks) {
			titles = append(titles, l.Title)
		}
		return titles
	}},
	{KeyAdditionalURLs, [2]string{"- https://a", "- https://b"}, "{", func(s *Settings) any { return s.AdditionalURLs() }},
	{"resource.customizations.health.apps_Deployment", [2]string{"return {}", "return nil"}, "", func(s *Settings) any { return s.ResourceOverrides()["apps/Deployment"].HealthLua }},
	{"resource.customizations.ignoreDifferences.apps_Deployment", [2]string{"jsonPointers: [/a]", "jsonPointers: [/b]"}, "jsonPointers: [", func(s *Settings) any { return s.ResourceOverrides()["apps/Deployment"].IgnoreDifferences }},
	{"extension.config.ext", [2]string{"services: [{url: http://a}]", "services: [{url: http://b}]"}, "services: []", func(s *Settings) any {
		var urls []string
		for _, e := range s.Extensions() {
			for _, svc := range e.Backend.Services {
				urls = append(urls, svc.URL)
			}
		}
		return urls
	}},
}

// TestPerKeyIsolationProperty: whatever mix of valid, invalid and absent keys a ConfigMap
// holds, every valid key is interpreted exactly as if it were alone, every invalid key keeps
// the value the previous snapshot accepted (or its default without one), and only invalid keys
// are reported as errors.
func TestPerKeyIsolationProperty(t *testing.T) {
	t.Parallel()
	alone := func(key, v string) *Settings { return Parse(in(map[string]string{key: v})) }
	defaults := Parse(in(map[string]string{}))
	rapid.Check(t, func(t *rapid.T) {
		withPrev := rapid.Bool().Draw(t, "withPrev")
		prevData := map[string]string{}
		next := map[string]string{}
		choice := map[string]int{}
		for _, k := range isoKeys {
			prevData[k.key] = k.valid[0]
			c := rapid.IntRange(0, 2).Draw(t, k.key) // 0 valid, 1 invalid, 2 absent
			choice[k.key] = c
			switch c {
			case 0:
				next[k.key] = k.valid[1]
			case 1:
				if k.invalid == "" {
					next[k.key] = k.valid[1]
					choice[k.key] = 0
				} else {
					next[k.key] = k.invalid
				}
			}
		}
		var opts []ParseOption
		var prev *Settings
		if withPrev {
			prev = Parse(in(prevData))
			opts = append(opts, WithPrevious(prev))
		}
		s := Parse(in(next), opts...)
		for _, k := range isoKeys {
			var want any
			switch choice[k.key] {
			case 0:
				want = k.get(alone(k.key, k.valid[1]))
			case 1:
				if withPrev {
					want = k.get(prev)
				} else {
					want = k.get(defaults)
				}
			case 2:
				want = k.get(defaults)
			}
			if got := k.get(s); !assert.ObjectsAreEqual(want, got) {
				t.Fatalf("key %s (choice %d, prev %v): got %#v, want %#v\n%s", k.key, choice[k.key], withPrev, got, want, dumpIssues(s.Validate()))
			}
			hasErr := false
			for _, is := range issuesFor(s, DefaultConfigMapName, k.key) {
				hasErr = hasErr || is.IsError()
			}
			if hasErr != (choice[k.key] == 1) {
				t.Fatalf("key %s: error reported=%v for choice %d\n%s", k.key, hasErr, choice[k.key], dumpIssues(s.Validate()))
			}
		}
	})
}

func TestIsolationRecoversAfterFix(t *testing.T) {
	t.Parallel()
	good := Parse(in(map[string]string{KeyResourceRespectRBAC: "normal", KeyURL: "https://a"}))
	bad := Parse(in(map[string]string{KeyResourceRespectRBAC: "always", KeyURL: "https://b"}), WithPrevious(good))
	assert.Equal(t, "normal", bad.RespectRBAC(), "last valid value kept")
	assert.Equal(t, "https://b", bad.URL(), "other keys still update")
	requireIssue(t, bad, DefaultConfigMapName, KeyResourceRespectRBAC, SeverityError, "keeping the last valid value")
	// The bad value persists across reloads without losing the last valid one.
	again := Parse(in(map[string]string{KeyResourceRespectRBAC: "always", KeyURL: "https://c"}), WithPrevious(bad))
	assert.Equal(t, "normal", again.RespectRBAC())
	fixed := Parse(in(map[string]string{KeyResourceRespectRBAC: "strict"}), WithPrevious(again))
	assert.Equal(t, "strict", fixed.RespectRBAC())
	assert.Empty(t, issuesFor(fixed, DefaultConfigMapName, KeyResourceRespectRBAC))
	// Removing the key returns to the default, not to the last value.
	removed := Parse(in(map[string]string{}), WithPrevious(fixed))
	assert.Equal(t, "", removed.RespectRBAC())
}

func TestFingerprint(t *testing.T) {
	t.Parallel()
	a := Parse(in(map[string]string{KeyURL: "https://a"}))
	b := Parse(in(map[string]string{KeyURL: "https://a"}))
	c := Parse(in(map[string]string{KeyURL: "https://b"}))
	assert.Equal(t, a.Fingerprint(), b.Fingerprint())
	assert.NotEqual(t, a.Fingerprint(), c.Fingerprint())
	d := Parse(in(map[string]string{KeyURL: "https://a"}, newSecret("other", map[string]string{"k": "v"}, true)))
	assert.NotEqual(t, a.Fingerprint(), d.Fingerprint(), "referenced Secrets are part of the input")
	require.Len(t, a.Fingerprint(), 64)
}
