package notifications

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

func TestParseDeliveryState(t *testing.T) {
	t.Parallel()
	tests := map[string]DeliveryState{
		"":                           {},
		"not json":                   {},
		"null":                       {},
		`{"a:b:c:d": 100}`:           {"a:b:c:d": 100},
		`["array"]`:                  {},
		strings.Repeat("x", 300<<10): {},
	}
	for in, want := range tests {
		if got := ParseDeliveryState(in); !reflect.DeepEqual(got, want) {
			t.Fatalf("ParseDeliveryState(%.20q) = %v, want %v", in, got, want)
		}
	}
}

func TestStateKey(t *testing.T) {
	t.Parallel()
	cr := ConditionResult{Key: "[0].abc"}
	d := services.Destination{Service: "slack", Recipient: "chan"}
	if got := StateKey("", "on-x", cr, d); got != "on-x:[0].abc:slack:chan" {
		t.Fatalf("key %q", got)
	}
	cr.OncePer = "rev1"
	if got := StateKey("team-ns", "on-x", cr, d); got != "rev1:team-ns:on-x:[0].abc:slack:chan" {
		t.Fatalf("key %q", got)
	}
}

func TestApplyDeliveryState(t *testing.T) {
	t.Parallel()
	ann := map[string]string{"keep": "me", DeliveryStateAnnotation(DefaultAnnotationPrefix): `{"x":1}`}
	out, err := ApplyDeliveryState(ann, DefaultAnnotationPrefix, DeliveryState{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["notified.notifications.cd.patrol.example"]; ok || out["keep"] != "me" {
		t.Fatalf("empty state must remove the annotation: %v", out)
	}
	if _, ok := ann["notified.notifications.cd.patrol.example"]; !ok {
		t.Fatal("input annotations must not be modified")
	}
	out, err = ApplyDeliveryState(nil, DefaultAnnotationPrefix, DeliveryState{"k": 5})
	if err != nil || out["notified.notifications.cd.patrol.example"] != `{"k":5}` {
		t.Fatalf("got %v, %v", out, err)
	}
	if got := DeliveryStateFromAnnotations(out, DefaultAnnotationPrefix); !reflect.DeepEqual(got, DeliveryState{"k": 5}) {
		t.Fatalf("round trip %v", got)
	}
}

func TestTruncateKeepsNewest(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 250).Draw(t, "n")
		limit := rapid.IntRange(0, 150).Draw(t, "limit")
		s := DeliveryState{}
		for i := 0; i < n; i++ {
			s[fmt.Sprintf("k%03d", i)] = rapid.Int64Range(0, 50).Draw(t, "ts")
		}
		orig := s.Clone()
		s.Truncate(limit)
		if want := min(n, limit); len(s) != want {
			t.Fatalf("len = %d, want %d", len(s), want)
		}
		// Every removed entry is not newer than any kept entry.
		for k, ts := range orig {
			if _, kept := s[k]; kept {
				continue
			}
			for _, kts := range s {
				if ts > kts {
					t.Fatalf("removed %s (%d) newer than a kept entry (%d)", k, ts, kts)
				}
			}
		}
		// Encode/Parse round trip of a truncated state is lossless.
		enc, err := s.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) <= MaxDeliveryStateEntries {
			if got := ParseDeliveryState(enc); len(s) > 0 && !reflect.DeepEqual(got, s) {
				t.Fatalf("round trip mismatch")
			}
		}
	})
}

func TestSetNotifiedSemantics(t *testing.T) {
	t.Parallel()
	s := DeliveryState{}
	if !s.setNotified("k", false, true, 10) || s["k"] != 10 {
		t.Fatal("first notification must change state")
	}
	if s.setNotified("k", false, true, 20) || s["k"] != 10 {
		t.Fatal("second notification is a duplicate")
	}
	if !s.setNotified("k", false, false, 0) {
		t.Fatal("condition turning false clears the key")
	}
	if _, ok := s["k"]; ok {
		t.Fatal("key must be removed")
	}
	s.setNotified("o", true, true, 10)
	if s.setNotified("o", true, false, 0) {
		t.Fatal("oncePer keys are never cleared")
	}
	if _, ok := s["o"]; !ok {
		t.Fatal("oncePer key removed")
	}
}
