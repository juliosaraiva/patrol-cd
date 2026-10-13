package notifications

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

func dest(service, recipient string) services.Destination {
	return services.Destination{Service: service, Recipient: recipient}
}

func TestAnnotationDestinations(t *testing.T) {
	t.Parallel()
	p := DefaultAnnotationPrefix
	tests := []struct {
		name        string
		annotations map[string]string
		defaults    []string
		svcDefaults map[string][]string
		want        Destinations
		wantWarn    bool
	}{
		{
			name:        "trigger and service with multiple recipients",
			annotations: map[string]string{p + "/subscribe.on-sync-succeeded.slack": "my-channel1; my-channel2;"},
			want:        Destinations{"on-sync-succeeded": {dest("slack", "my-channel1"), dest("slack", "my-channel2")}},
		},
		{
			name:        "empty recipient for webhook-like services",
			annotations: map[string]string{p + "/subscribe.on-deployed.github": ""},
			want:        Destinations{"on-deployed": {dest("github", "")}},
		},
		{
			name:        "service only uses default triggers",
			annotations: map[string]string{p + "/subscribe.slack": "chan"},
			defaults:    []string{"on-sync-status-unknown"},
			want:        Destinations{"on-sync-status-unknown": {dest("slack", "chan")}},
		},
		{
			name:        "service specific default triggers win",
			annotations: map[string]string{p + "/subscribe.slack": "chan", p + "/subscribe.mattermost": "mm"},
			defaults:    []string{"on-sync-status-unknown"},
			svcDefaults: map[string][]string{"mattermost": {"on-sync-running", "on-sync-succeeded"}},
			want: Destinations{
				"on-sync-status-unknown": {dest("slack", "chan")},
				"on-sync-running":        {dest("mattermost", "mm")},
				"on-sync-succeeded":      {dest("mattermost", "mm")},
			},
		},
		{
			name:        "dotted service name",
			annotations: map[string]string{p + "/subscribe.on-x.my.hook": ""},
			want:        Destinations{"on-x": {dest("my.hook", "")}},
		},
		{
			name: "yaml subscriptions annotation",
			annotations: map[string]string{p + "/subscriptions": `
- trigger: [on-sync-succeeded, on-sync-failed]
  destinations:
    - service: slack
      recipients: [c1, c2]
    - service: email
      recipients: [ops@example.com]
- destinations:
    - service: webhook-x
`},
			defaults: []string{"on-created"},
			want: Destinations{
				"on-sync-succeeded": {dest("slack", "c1"), dest("slack", "c2"), dest("email", "ops@example.com")},
				"on-sync-failed":    {dest("slack", "c1"), dest("slack", "c2"), dest("email", "ops@example.com")},
				"on-created":        {dest("webhook-x", "")},
			},
		},
		{
			name:        "invalid yaml annotation is reported",
			annotations: map[string]string{p + "/subscriptions": "- trigger: [unclosed"},
			want:        Destinations{},
			wantWarn:    true,
		},
		{
			name:        "other prefixes ignored",
			annotations: map[string]string{"notifications.argoproj.io/subscribe.on-x.slack": "c", "unrelated": "v"},
			want:        Destinations{},
		},
		{
			name: "duplicates collapse",
			annotations: map[string]string{
				p + "/subscribe.on-x.slack": "c;c",
				p + "/subscriptions":        "- trigger: [on-x]\n  destinations: [{service: slack, recipients: [c]}]",
			},
			want: Destinations{"on-x": {dest("slack", "c")}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, warnings := AnnotationDestinations(p, tc.annotations, tc.defaults, tc.svcDefaults)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			if (len(warnings) > 0) != tc.wantWarn {
				t.Fatalf("warnings %v", warnings)
			}
		})
	}
}

func TestGlobalDestinations(t *testing.T) {
	t.Parallel()
	cfg, err := ParseConfig(map[string]string{
		"defaultTriggers": "- on-default",
		"subscriptions": `
- recipients: [slack:test2, email:test@gmail.com, my-webhook]
  triggers: [on-sync-status-unknown]
- recipients: [slack:test3]
  selector: test=true
  triggers: [on-sync-status-unknown]
- recipients: [slack:all]
`,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.GlobalDestinations(map[string]string{"test": "false"})
	want := Destinations{
		"on-sync-status-unknown": {dest("slack", "test2"), dest("email", "test@gmail.com"), dest("my-webhook", "")},
		"on-default":             {dest("slack", "all")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	got = cfg.GlobalDestinations(map[string]string{"test": "true"})
	if len(got["on-sync-status-unknown"]) != 4 {
		t.Fatalf("selector match should add slack:test3: %v", got)
	}
}

// TestDestinationsProperties: Add is idempotent and Merge is order-insensitive as a set.
func TestDestinationsProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		gen := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) [3]string {
			return [3]string{
				rapid.SampledFrom([]string{"on-a", "on-b", "on-c"}).Draw(t, "trigger"),
				rapid.SampledFrom([]string{"slack", "email", "webhook"}).Draw(t, "service"),
				rapid.SampledFrom([]string{"", "x", "y"}).Draw(t, "recipient"),
			}
		}), 0, 30)
		items := gen.Draw(t, "items")
		a, b := Destinations{}, Destinations{}
		for _, it := range items {
			a.Add(it[0], dest(it[1], it[2]))
			a.Add(it[0], dest(it[1], it[2]))
		}
		for i := len(items) - 1; i >= 0; i-- {
			b.Add(items[i][0], dest(items[i][1], items[i][2]))
		}
		if setOf(a) != setOf(b) {
			t.Fatalf("set mismatch %v vs %v", a, b)
		}
		for trig, ds := range a {
			seen := map[services.Destination]bool{}
			for _, d := range ds {
				if seen[d] {
					t.Fatalf("duplicate %v for %s", d, trig)
				}
				seen[d] = true
			}
		}
		merged := Destinations{}
		merged.Merge(a)
		merged.Merge(b)
		if setOf(merged) != setOf(a) {
			t.Fatalf("merge not idempotent")
		}
	})
}

func setOf(d Destinations) string {
	var out []string
	for t, ds := range d {
		for _, x := range ds {
			out = append(out, fmt.Sprintf("%s|%s|%s", t, x.Service, x.Recipient))
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestSplitRecipients(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		v := rapid.String().Draw(t, "v")
		out := splitRecipients(v)
		if len(out) == 0 {
			t.Fatal("never empty")
		}
		if len(out) == 1 && out[0] == "" {
			return
		}
		for _, r := range out {
			if r == "" || r != strings.TrimSpace(r) || strings.Contains(r, ";") {
				t.Fatalf("bad recipient %q from %q", r, v)
			}
		}
	})
}

func TestLegacyRecipientsDestinations(t *testing.T) {
	t.Parallel()
	suffix := LegacyRecipientsAnnotationSuffix(DefaultAnnotationPrefix)
	if suffix != "recipients.notifications.cd.patrol.example" {
		t.Fatalf("suffix %q", suffix)
	}
	tests := []struct {
		name        string
		annotations map[string]string
		defaults    []string
		svcDefaults map[string][]string
		want        Destinations
	}{
		{"trigger prefix", map[string]string{"my-trigger." + suffix: "slack:my-channel"}, nil, nil,
			Destinations{"my-trigger": {dest("slack", "my-channel")}}},
		{"bare key uses default triggers", map[string]string{suffix: "slack:c, email:a@b.com:extra, webhookname,"}, []string{"my-trigger"}, nil,
			Destinations{"my-trigger": {dest("slack", "c"), dest("email", "a@b.com"), dest("webhookname", "")}}},
		{"service defaults replace explicit trigger", map[string]string{"my-trigger." + suffix: "slack:c"}, nil, map[string][]string{"slack": {"trigger-a", "trigger-b"}},
			Destinations{"trigger-a": {dest("slack", "c")}, "trigger-b": {dest("slack", "c")}}},
		{"no defaults and no trigger", map[string]string{suffix: "slack:c"}, nil, nil, Destinations{}},
	}
	for _, tc := range tests {
		if got := LegacyRecipientsDestinations(suffix, tc.annotations, tc.defaults, tc.svcDefaults); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
