package scm_test

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

func TestVerifyHMACSHA256(t *testing.T) {
	t.Parallel()
	body := []byte(`{"zen":"Keep it logically awesome."}`)
	secret := []byte("s3cr3t")
	good := "sha256=" + scm.SignHMACSHA256(secret, body)
	tests := []struct {
		name   string
		header string
		body   []byte
		secret []byte
		want   error
	}{
		{"valid", good, body, secret, nil},
		{"valid uppercase hex", "sha256=" + strings.ToUpper(scm.SignHMACSHA256(secret, body)), body, secret, nil},
		{"missing", "", body, secret, scm.ErrSignatureMissing},
		{"wrong prefix", "sha1=" + scm.SignHMACSHA256(secret, body), body, secret, scm.ErrSignatureMismatch},
		{"not hex", "sha256=zz", body, secret, scm.ErrSignatureMismatch},
		{"truncated", good[:len(good)-2], body, secret, scm.ErrSignatureMismatch},
		{"tampered body", good, append(slices.Clone(body), ' '), secret, scm.ErrSignatureMismatch},
		{"wrong secret", good, body, []byte("other"), scm.ErrSignatureMismatch},
	}
	for _, tc := range tests {
		err := scm.VerifyHMACSHA256(tc.header, "sha256=", tc.body, tc.secret)
		if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	// Known-answer vector (RFC 4231 test case 2).
	if got := scm.SignHMACSHA256([]byte("Jefe"), []byte("what do ya want for nothing?")); got != "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843" {
		t.Fatalf("HMAC-SHA256 KAT mismatch: %s", got)
	}
}

func TestSignatureTamperProperty(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		body := rapid.SliceOfN(rapid.Byte(), 1, 512).Draw(t, "body")
		secret := rapid.SliceOfN(rapid.Byte(), 1, 64).Draw(t, "secret")
		sig := "sha256=" + scm.SignHMACSHA256(secret, body)
		if err := scm.VerifyHMACSHA256(sig, "sha256=", body, secret); err != nil {
			t.Fatalf("valid signature rejected: %v", err)
		}
		i := rapid.IntRange(0, len(body)-1).Draw(t, "i")
		bit := byte(1) << rapid.IntRange(0, 7).Draw(t, "bit")
		tampered := slices.Clone(body)
		tampered[i] ^= bit
		if err := scm.VerifyHMACSHA256(sig, "sha256=", tampered, secret); !errors.Is(err, scm.ErrSignatureMismatch) {
			t.Fatalf("tampered body accepted: %v", err)
		}
	})
}

func TestReadWebhookBody(t *testing.T) {
	t.Parallel()
	b, err := scm.ReadWebhookBody(strings.NewReader("12345"), 5)
	if err != nil || string(b) != "12345" {
		t.Fatalf("at limit: %q %v", b, err)
	}
	_, err = scm.ReadWebhookBody(strings.NewReader("123456"), 5)
	if !errors.Is(err, scm.ErrPayloadTooLarge) || scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("over limit: %v", err)
	}
	b, err = scm.ReadWebhookBody(bytes.NewReader(make([]byte, 10)), 0)
	if err != nil || len(b) != 10 {
		t.Fatalf("default limit: %v", err)
	}
}

func TestParseRevision(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"refs/heads/main":       "main",
		"refs/heads/feature/x":  "feature/x",
		"refs/tags/v1.0.0":      "v1.0.0",
		"main":                  "main",
		"":                      "",
		"refs/pull/12/head":     "12/head",
		"refs/heads/a/b/c/d/e/": "a/b/c/d/e/",
	}
	for in, want := range tests {
		if got := scm.ParseRevision(in); got != want {
			t.Errorf("ParseRevision(%q) = %q, want %q", in, got, want)
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		branch := rapid.StringMatching(`[a-z0-9][a-z0-9/._-]{0,30}`).Draw(t, "branch")
		if got := scm.ParseRevision("refs/heads/" + branch); got != branch {
			t.Fatalf("ParseRevision(refs/heads/%s) = %q", branch, got)
		}
	})
}

func TestNormalizeChangedFilesProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		groups := rapid.SliceOfN(rapid.SliceOf(rapid.SampledFrom([]string{"", "a", "b", "c/d", "e/f/g", "README.md"})), 0, 6).Draw(t, "groups")
		got, truncated := scm.NormalizeChangedFiles(groups...)
		if truncated {
			t.Fatal("small input must not be truncated")
		}
		seen := map[string]bool{}
		var want []string
		for _, g := range groups {
			for _, f := range g {
				if f != "" && !seen[f] {
					seen[f] = true
					want = append(want, f)
				}
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v (first-seen order, no empties, no duplicates)", got, want)
		}
		again, _ := scm.NormalizeChangedFiles(got)
		if !slices.Equal(again, got) {
			t.Fatal("NormalizeChangedFiles is not idempotent")
		}
	})
	many := make([]string, scm.MaxChangedFiles+5)
	for i := range many {
		many[i] = fmt.Sprintf("f%d", i)
	}
	got, truncated := scm.NormalizeChangedFiles(many)
	if !truncated || len(got) != scm.MaxChangedFiles {
		t.Fatalf("cap: len=%d truncated=%v", len(got), truncated)
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func TestDeliveryCacheBasics(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: fixedNow}
	c := scm.NewDeliveryCache(2, time.Minute, scm.WithDeliveryClock(clk.Now))
	k := scm.DeliveryKey("github", "d1")
	if scm.DeliveryKey("github", "") != "" {
		t.Fatal("empty delivery ID must give an empty key")
	}
	first, second := c.Seen(""), c.Seen("")
	if first || second || c.Len() != 0 {
		t.Fatal("empty keys must never be recorded")
	}
	if c.Seen(k) {
		t.Fatal("first delivery reported as duplicate")
	}
	if !c.Seen(k) {
		t.Fatal("replay not detected")
	}
	clk.Advance(time.Minute)
	if c.Seen(k) {
		t.Fatal("expired entry reported as duplicate")
	}
	if !c.Seen(k) {
		t.Fatal("re-recorded entry not detected")
	}
	c.Forget(k)
	if c.Seen(k) {
		t.Fatal("forgotten entry reported as duplicate")
	}
	c.Seen("a")
	c.Seen("b") // evicts k (capacity 2)
	if c.Len() != 2 || c.Seen(k) {
		t.Fatal("LRU eviction")
	}
	if scm.DeliveryKey("github", "x") == scm.DeliveryKey("gitlab", "x") {
		t.Fatal("keys must be provider scoped")
	}
}

// TestDeliveryCacheModel checks the cache against a reference model: a key is a
// duplicate iff it was recorded within the TTL and is among the `capacity` most
// recently touched keys.
func TestDeliveryCacheModel(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		capacity := rapid.IntRange(1, 5).Draw(t, "capacity")
		ttl := time.Duration(rapid.IntRange(1, 10).Draw(t, "ttl")) * time.Second
		clk := &fakeClock{now: fixedNow}
		c := scm.NewDeliveryCache(capacity, ttl, scm.WithDeliveryClock(clk.Now))
		type entry struct {
			key  string
			seen time.Time
		}
		var model []entry // most recent first
		find := func(k string) int {
			for i, e := range model {
				if e.key == k {
					return i
				}
			}
			return -1
		}
		steps := rapid.IntRange(1, 60).Draw(t, "steps")
		for range steps {
			switch rapid.IntRange(0, 2).Draw(t, "op") {
			case 0:
				clk.Advance(time.Duration(rapid.IntRange(0, 4).Draw(t, "dt")) * time.Second)
			case 1:
				k := rapid.SampledFrom([]string{"a", "b", "c", "d", "e", "f", "g"}).Draw(t, "key")
				now := clk.Now()
				want := false
				if i := find(k); i >= 0 {
					e := model[i]
					model = slices.Delete(model, i, i+1)
					if now.Sub(e.seen) < ttl {
						want = true
					} else {
						e.seen = now
					}
					model = append([]entry{e}, model...)
				} else {
					model = append([]entry{{k, now}}, model...)
					if len(model) > capacity {
						model = model[:capacity]
					}
				}
				if got := c.Seen(k); got != want {
					t.Fatalf("Seen(%q) = %v, model says %v", k, got, want)
				}
			case 2:
				k := rapid.SampledFrom([]string{"a", "b", "c"}).Draw(t, "forget")
				c.Forget(k)
				if i := find(k); i >= 0 {
					model = slices.Delete(model, i, i+1)
				}
			}
			if c.Len() != len(model) || c.Len() > capacity {
				t.Fatalf("Len = %d, model %d, capacity %d", c.Len(), len(model), capacity)
			}
		}
	})
}

func TestDeliveryCacheConcurrent(t *testing.T) {
	t.Parallel()
	c := scm.NewDeliveryCache(1000, time.Hour)
	var wg sync.WaitGroup
	var mu sync.Mutex
	firsts := 0
	for range 16 {
		wg.Go(func() {
			if !c.Seen("same-delivery") {
				mu.Lock()
				firsts++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if firsts != 1 {
		t.Fatalf("exactly one concurrent delivery must be processed, got %d", firsts)
	}
}

func TestWebhookSignatureError(t *testing.T) {
	t.Parallel()
	err := scm.WebhookSignatureError("github", scm.ErrSignatureMismatch)
	if !errors.Is(err, scm.ErrSignatureMismatch) || !errors.Is(err, scm.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
}
