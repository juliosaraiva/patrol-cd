package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

var (
	revBase = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	// Secret data keys must match this (k8s.io/apimachinery IsConfigMapKey).
	secretKeyRE = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
)

// dataSize recomputes Size from the serialized form.
func dataSize(data map[string][]byte) int {
	n := 0
	for k, v := range data {
		n += len(k) + len(v)
	}
	return n
}

func TestRevocationTableKeysAreFixedLength(t *testing.T) {
	t.Parallel()
	tbl := NewRevocationTable()
	ids := []string{
		"a",
		"8f14e45f-ceea-467f-a0e6-b6d7e0e8b3a1",
		strings.Repeat("x", 253),            // was stored verbatim before
		strings.Repeat("é", 512),            // 1024 bytes, the maximum
		"id/with+chars==\x00and controls\n", // not key-safe
	}
	subjects := []string{"", "alice", strings.Repeat("s", maxRevocationSubjectBytes)}
	for _, sub := range subjects {
		for _, id := range ids {
			ref := TokenRef{ID: id, Subject: sub, IssuedAt: revBase, ExpiresAt: revBase.Add(time.Hour)}
			if _, err := tbl.Revoke(ref, revBase, RevocationLimits{}); err != nil {
				t.Fatalf("revoke %q: %v", id, err)
			}
			if !tbl.IsRevoked(ref, revBase) {
				t.Fatalf("%q not revoked", id)
			}
		}
	}
	data := tbl.Data()
	if len(data) != len(ids)*len(subjects) {
		t.Fatalf("entries = %d", len(data))
	}
	for k, v := range data {
		if !secretKeyRE.MatchString(k) || len(k) != revTokenKeyLen {
			t.Fatalf("key %q (len %d) is not a fixed-length secret key", k, len(k))
		}
		if len(v) > 2*20+1 {
			t.Fatalf("value %q too long", v)
		}
	}
	if got := dataSize(data); got != tbl.Size() {
		t.Fatalf("Size() = %d, serialized %d", tbl.Size(), got)
	}
	for name, ref := range map[string]TokenRef{
		"empty id":     {Subject: "s", ExpiresAt: revBase.Add(time.Hour)},
		"long id":      {ID: strings.Repeat("x", maxRevocationIDBytes+1), ExpiresAt: revBase.Add(time.Hour)},
		"long subject": {ID: "x", Subject: strings.Repeat("s", maxRevocationSubjectBytes+1), ExpiresAt: revBase.Add(time.Hour)},
	} {
		if _, err := tbl.Revoke(ref, revBase, RevocationLimits{}); err == nil || errors.Is(err, ErrNotRevocable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := tbl.Revoke(TokenRef{ID: "x"}, revBase, RevocationLimits{}); !errors.Is(err, ErrNotRevocable) {
		t.Fatalf("zero expiry: %v", err)
	}
}

func TestRevocationTableCollapsesSubjectOverLimit(t *testing.T) {
	t.Parallel()
	now := revBase
	lim := RevocationLimits{MaxEntriesPerSubject: 3}
	tbl := NewRevocationTable()
	var alice []TokenRef
	for i := range 4 {
		ref := TokenRef{
			ID:        fmt.Sprint("a", i),
			Subject:   "alice",
			IssuedAt:  now.Add(time.Duration(i-10) * time.Minute),
			ExpiresAt: now.Add(time.Hour + time.Duration(i)*time.Minute),
		}
		if changed, err := tbl.Revoke(ref, now, lim); err != nil || !changed {
			t.Fatalf("revoke %d: %v %v", i, changed, err)
		}
		alice = append(alice, ref)
		if i < 3 && tbl.Len() != i+1 {
			t.Fatalf("len after %d = %d", i, tbl.Len())
		}
	}
	// The fourth entry exceeds the per-subject limit: one watermark remains.
	m, ok := tbl.marks[revSubjectTag("alice")]
	if tbl.Len() != 1 || !ok {
		t.Fatalf("not collapsed: len %d marks %v", tbl.Len(), tbl.marks)
	}
	if m.cutoff != alice[3].IssuedAt.Unix() || m.until != alice[3].ExpiresAt.Unix() {
		t.Fatalf("watermark = %+v", m)
	}
	for _, ref := range alice {
		if !tbl.IsRevoked(ref, now) {
			t.Fatalf("%s no longer revoked", ref.ID)
		}
	}
	// Collateral: alice's other tokens issued at or before the cutoff are revoked too...
	older := TokenRef{ID: "other", Subject: "alice", IssuedAt: alice[1].IssuedAt.Add(30 * time.Second)}
	if !tbl.IsRevoked(older, now) {
		t.Fatal("older token of the collapsed subject not covered")
	}
	// ...but not tokens issued later, nor other subjects' tokens.
	newer := TokenRef{ID: "newer", Subject: "alice", IssuedAt: alice[3].IssuedAt.Add(time.Second), ExpiresAt: now.Add(time.Hour)}
	if tbl.IsRevoked(newer, now) {
		t.Fatal("token issued after the cutoff revoked")
	}
	if tbl.IsRevoked(TokenRef{ID: "a0", Subject: "bob", IssuedAt: alice[0].IssuedAt}, now) {
		t.Fatal("same id under another subject revoked")
	}
	// Revoking a covered token only extends the watermark.
	longer := older
	longer.ExpiresAt = now.Add(3 * time.Hour)
	if changed, err := tbl.Revoke(longer, now, lim); err != nil || !changed || tbl.Len() != 1 {
		t.Fatalf("covered revoke: %v %v len %d", changed, err, tbl.Len())
	}
	if got := tbl.marks[revSubjectTag("alice")].until; got != longer.ExpiresAt.Unix() {
		t.Fatalf("until = %d", got)
	}
	if changed, err := tbl.Revoke(alice[0], now, lim); err != nil || changed {
		t.Fatalf("already covered: %v %v", changed, err)
	}
	// Later tokens get individual entries again.
	if _, err := tbl.Revoke(newer, now, lim); err != nil || tbl.Len() != 2 || !tbl.IsRevoked(newer, now) {
		t.Fatalf("newer: %v len %d", err, tbl.Len())
	}
	// The watermark lapses at its expiry and is pruned.
	end := longer.ExpiresAt
	if tbl.IsRevoked(older, end) {
		t.Fatal("watermark applied after its expiry")
	}
	if n := tbl.Prune(end); n != 2 || tbl.Len() != 0 || tbl.Size() != 0 {
		t.Fatalf("pruned %d, len %d, size %d", n, tbl.Len(), tbl.Size())
	}
}

// Found by TestRevocationTableProperties: revoking the same (subject, ID) again with a
// later iat (a reused account token ID) must stay covered when the subject collapses.
func TestRevocationTableReusedIDStaysCoveredAfterCollapse(t *testing.T) {
	t.Parallel()
	now := revBase
	lim := RevocationLimits{MaxEntriesPerSubject: 1}
	tbl := NewRevocationTable()
	first := TokenRef{ID: "ci", Subject: "alice", IssuedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
	second := TokenRef{ID: "ci", Subject: "alice", IssuedAt: now, ExpiresAt: now.Add(30 * time.Minute)}
	third := TokenRef{ID: "x", Subject: "alice", IssuedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(time.Hour)}
	for _, ref := range []TokenRef{first, second, third} {
		if _, err := tbl.Revoke(ref, now, lim); err != nil {
			t.Fatal(err)
		}
	}
	if len(tbl.tokens) != 0 || len(tbl.marks) != 1 {
		t.Fatalf("not collapsed: %v", tbl.Data())
	}
	for _, ref := range []TokenRef{first, second, third} {
		if !tbl.IsRevoked(ref, now) {
			t.Fatalf("%+v no longer revoked", ref)
		}
	}
}

// Regression: one principal used to be able to fill the store so that every other
// principal's logout failed with ErrRevocationStoreFull.
func TestRevocationTableOneSubjectCannotExhaustStore(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		lim     RevocationLimits
		revokes int
	}{
		"entry limit":       {RevocationLimits{MaxEntries: 50, MaxEntriesPerSubject: 1000}, 500},
		"byte limit":        {RevocationLimits{MaxBytes: 50 * 64, MaxEntriesPerSubject: 1000}, 500},
		"per-subject limit": {RevocationLimits{MaxEntries: 50, MaxEntriesPerSubject: 10}, 500},
		"defaults":          {RevocationLimits{}, 3 * DefaultRevocationMaxEntriesPerSubject},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := revBase
			lim := tc.lim.withDefaults()
			tbl := NewRevocationTable()
			attacker := make([]TokenRef, 0, tc.revokes)
			for i := range tc.revokes {
				ref := TokenRef{
					ID:        fmt.Sprintf("%0253d", i), // long caller-chosen IDs
					Subject:   "mallory",
					IssuedAt:  now.Add(-time.Duration(tc.revokes-i) * time.Second),
					ExpiresAt: now.Add(24 * time.Hour),
				}
				if _, err := tbl.Revoke(ref, now, tc.lim); err != nil {
					t.Fatalf("attacker revocation %d: %v", i, err)
				}
				attacker = append(attacker, ref)
				if tbl.Len() > lim.MaxEntries || tbl.Size() > lim.MaxBytes {
					t.Fatalf("limits exceeded: %d entries, %d bytes", tbl.Len(), tbl.Size())
				}
			}
			for j := range 20 {
				v := TokenRef{ID: fmt.Sprint("victim-", j), Subject: fmt.Sprint("user-", j), IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
				if _, err := tbl.Revoke(v, now, tc.lim); err != nil {
					t.Fatalf("victim %d logout failed: %v", j, err)
				}
				if !tbl.IsRevoked(v, now) {
					t.Fatalf("victim %d not revoked", j)
				}
			}
			for _, ref := range attacker {
				if !tbl.IsRevoked(ref, now) {
					t.Fatalf("attacker token %s un-revoked", ref.ID[245:])
				}
			}
			// Victims' other sessions are untouched.
			if tbl.IsRevoked(TokenRef{ID: "other", Subject: "user-0", IssuedAt: now.Add(-time.Hour)}, now) {
				t.Fatal("victim's other session revoked")
			}
			if tbl.Len() > lim.MaxEntries || tbl.Size() > lim.MaxBytes || dataSize(tbl.Data()) != tbl.Size() {
				t.Fatalf("final: %d entries, %d bytes", tbl.Len(), tbl.Size())
			}
		})
	}
}

func TestRevocationTableFullOnlyWithDistinctSubjects(t *testing.T) {
	t.Parallel()
	now := revBase
	lim := RevocationLimits{MaxEntries: 3}
	tbl := NewRevocationTable()
	ref := func(sub, id string, ttl time.Duration) TokenRef {
		return TokenRef{ID: id, Subject: sub, IssuedAt: now, ExpiresAt: now.Add(ttl)}
	}
	for i, ttl := range []time.Duration{time.Minute, time.Hour, time.Hour} {
		if _, err := tbl.Revoke(ref(fmt.Sprint("s", i), "t", ttl), now, lim); err != nil {
			t.Fatal(err)
		}
	}
	before := tbl.Data()
	_, err := tbl.Revoke(ref("s3", "t", time.Hour), now, lim)
	if !errors.Is(err, ErrRevocationStoreFull) {
		t.Fatalf("full: %v", err)
	}
	if !maps.EqualFunc(before, tbl.Data(), bytes.Equal) || tbl.IsRevoked(ref("s3", "t", 0), now) {
		t.Fatal("failed revoke changed the table")
	}
	// A second token of an existing subject collapses that subject instead of failing.
	second := ref("s1", "t2", 2*time.Hour)
	if _, err := tbl.Revoke(second, now, lim); err != nil {
		t.Fatalf("second token of s1: %v", err)
	}
	if tbl.Len() != 3 || !tbl.IsRevoked(second, now) || !tbl.IsRevoked(ref("s1", "t", 0), now) {
		t.Fatalf("after collapse: len %d", tbl.Len())
	}
	// Room frees up when entries expire.
	now = now.Add(2 * time.Minute)
	if _, err := tbl.Revoke(ref("s3", "t", time.Hour), now, lim); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
}

func TestDecodeRevocationTable(t *testing.T) {
	t.Parallel()
	tbl := NewRevocationTable()
	lim := RevocationLimits{MaxEntriesPerSubject: 2}
	for i := range 5 {
		ref := TokenRef{ID: fmt.Sprint("id", i), Subject: fmt.Sprint("s", i%2), IssuedAt: revBase, ExpiresAt: revBase.Add(time.Hour)}
		if _, err := tbl.Revoke(ref, revBase, lim); err != nil {
			t.Fatal(err)
		}
	}
	data := tbl.Data()
	got, bad := DecodeRevocationTable(data)
	if bad != 0 || !maps.EqualFunc(got.Data(), data, bytes.Equal) || got.Size() != tbl.Size() || len(got.marks) != 1 {
		t.Fatalf("round trip: bad %d, %v", bad, got.Data())
	}

	tag16, tag22 := strings.Repeat("A", revSubjectTagLen), strings.Repeat("b", revTokenTagLen)
	withJunk := maps.Clone(data)
	for k, v := range map[string]string{
		"8f14e45f-ceea-467f-a0e6-b6d7e0e8b3a1": "1760000000", // legacy verbatim key
		"sha256-0123abcd":                      "1760000000", // legacy hashed key
		"t.short.x":                            "1.2",
		"t." + tag16 + "." + tag22[1:] + "!":   "1.2",
		"t." + tag16:                           "1.2",
		"w." + tag16[1:] + "-":                 "notanumber",
		"w." + tag16:                           "-1.5",
		"w." + tag16[1:] + "_":                 "5",
		"x." + tag16:                           "1.1",
	} {
		withJunk[k] = []byte(v)
	}
	got, bad = DecodeRevocationTable(withJunk)
	if bad != 9 || !maps.EqualFunc(got.Data(), data, bytes.Equal) {
		t.Fatalf("junk: bad %d, %v", bad, got.Data())
	}
}

// Properties for random operation sequences and limits: revoked tokens stay revoked
// until they expire, limits always hold, unrevoked tokens are only reported revoked
// through their own subject's watermark, and the serialized form round-trips.
func TestRevocationTableProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		lim := RevocationLimits{
			MaxEntries:           rapid.IntRange(1, 12).Draw(rt, "maxEntries"),
			MaxBytes:             rapid.IntRange(60, 1200).Draw(rt, "maxBytes"),
			MaxEntriesPerSubject: rapid.IntRange(1, 6).Draw(rt, "perSubject"),
		}
		tbl := NewRevocationTable()
		now := revBase
		var revoked []TokenRef
		ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
		subjects := []string{"s1", "s2", "s3", "s4"}
		steps := rapid.IntRange(1, 60).Draw(rt, "steps")
		for range steps {
			now = now.Add(time.Duration(rapid.IntRange(0, 300).Draw(rt, "dt")) * time.Second)
			ref := TokenRef{
				ID:       rapid.SampledFrom(ids).Draw(rt, "id"),
				Subject:  rapid.SampledFrom(subjects).Draw(rt, "subject"),
				IssuedAt: now.Add(-time.Duration(rapid.IntRange(0, 3600).Draw(rt, "age")) * time.Second),
				ExpiresAt: now.Add(time.Duration(rapid.IntRange(-60, 3600).Draw(rt, "ttl"))*time.Second +
					time.Duration(rapid.IntRange(0, 999).Draw(rt, "ms"))*time.Millisecond),
			}
			before := tbl.Data()
			_, err := tbl.Revoke(ref, now, lim)
			switch {
			case err == nil:
				revoked = append(revoked, ref)
			case errors.Is(err, ErrRevocationStoreFull):
				// Only pruning may have happened.
				for k, v := range tbl.Data() {
					if !bytes.Equal(before[k], v) {
						rt.Fatalf("failed revoke modified %s", k)
					}
				}
			default:
				rt.Fatalf("unexpected error: %v", err)
			}
			if tbl.Len() > lim.MaxEntries || tbl.Size() > lim.MaxBytes {
				rt.Fatalf("limits exceeded: %d entries, %d bytes", tbl.Len(), tbl.Size())
			}
			perSubject := map[string]int{}
			for k := range tbl.tokens {
				perSubject[k.subject]++
			}
			for s, n := range perSubject {
				if n > lim.MaxEntriesPerSubject {
					rt.Fatalf("subject %s holds %d entries", s, n)
				}
			}
			for _, r := range revoked {
				if r.ExpiresAt.After(now) && !tbl.IsRevoked(r, now) {
					rt.Fatalf("revoked token %+v reported valid at %v", r, now)
				}
			}
			for _, sub := range subjects {
				probe := TokenRef{ID: "never-revoked", Subject: sub, IssuedAt: now.Add(-time.Hour)}
				if _, hasMark := tbl.marks[revSubjectTag(sub)]; tbl.IsRevoked(probe, now) && !hasMark {
					rt.Fatalf("unrevoked token of %s reported revoked without a watermark", sub)
				}
			}
			data := tbl.Data()
			if dataSize(data) != tbl.Size() {
				rt.Fatalf("Size() = %d, serialized %d", tbl.Size(), dataSize(data))
			}
			decoded, bad := DecodeRevocationTable(data)
			if bad != 0 || !maps.EqualFunc(decoded.Data(), data, bytes.Equal) || decoded.Size() != tbl.Size() {
				rt.Fatalf("round trip failed (bad %d)", bad)
			}
		}
	})
}

func TestMemoryRevocationStore(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	m := NewMemoryRevocationStore(RevocationLimits{MaxEntries: 2}, clock.Now)
	ctx := context.Background()
	ref := func(sub, id string, ttl time.Duration) TokenRef {
		return TokenRef{ID: id, Subject: sub, IssuedAt: clock.Now(), ExpiresAt: clock.Now().Add(ttl)}
	}
	if err := m.Revoke(ctx, ref("u1", "a", time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke(ctx, ref("u1", "expired", -time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := m.IsRevoked(ctx, ref("u1", "expired", 0)); ok {
		t.Fatal("expired token stored")
	}
	if ok, _ := m.IsRevoked(ctx, ref("u1", "a", 0)); !ok {
		t.Fatal("revoked token not reported")
	}
	if err := m.Revoke(ctx, ref("u2", "b", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke(ctx, ref("u3", "c", time.Hour)); !errors.Is(err, ErrRevocationStoreFull) {
		t.Fatalf("full: %v", err)
	}
	clock.Advance(2 * time.Minute) // "a" expires and can be pruned
	if err := m.Revoke(ctx, ref("u3", "c", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke(ctx, ref("u1", "", time.Hour)); err == nil {
		t.Fatal("empty id accepted")
	}
	if err := m.Revoke(ctx, TokenRef{ID: "d"}); !errors.Is(err, ErrNotRevocable) {
		t.Fatalf("zero exp: %v", err)
	}
}

// Regression (TokenService level): one account logging in and out repeatedly can no
// longer make other principals' logouts fail. Its revocations collapse into a watermark
// that ends only its own older sessions; its API tokens are a separate scope.
func TestLogoutStormIsContainedToOnePrincipal(t *testing.T) {
	t.Parallel()
	e := newTokenEnv(t)
	ctx := context.Background()
	revs := NewMemoryRevocationStore(RevocationLimits{MaxEntries: 20, MaxEntriesPerSubject: 5}, e.clock.Now)
	svc, err := NewTokenService(TokenServiceConfig{Keys: e.keys, Accounts: AccountLookupFunc(e.lookup), Revocations: revs, Now: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	apiIss, meta, err := svc.IssueAccountToken(ctx, e.account("alice"), "", 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	alice := e.account("alice")
	e.update(func(cm, s map[string]string) (map[string]string, map[string]string) {
		s, err := AddToken(s, alice, meta)
		if err != nil {
			t.Fatal(err)
		}
		return cm, s
	})
	bobKeep, _ := svc.IssueLoginSession(ctx, e.account("bob"), time.Hour)
	aliceOpen, _ := svc.IssueLoginSession(ctx, e.account("alice"), time.Hour)
	e.clock.Advance(time.Second)

	var stormed []string
	for i := range 100 {
		iss, err := svc.IssueLoginSession(ctx, e.account("alice"), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		p, err := svc.Verify(ctx, iss.Token)
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		if err := svc.Revoke(ctx, p); err != nil {
			t.Fatalf("logout %d: %v", i, err)
		}
		stormed = append(stormed, iss.Token)
		e.clock.Advance(time.Second)
	}
	bobOut, _ := svc.IssueLoginSession(ctx, e.account("bob"), time.Hour)
	p, err := svc.Verify(ctx, bobOut.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Revoke(ctx, p); err != nil {
		t.Fatalf("bob's logout failed: %v", err)
	}
	_, err = svc.Verify(ctx, bobOut.Token)
	wantInvalid(t, err, ErrTokenRevoked)
	if _, err := svc.Verify(ctx, bobKeep.Token); err != nil {
		t.Fatalf("bob's other session: %v", err)
	}
	for i, tok := range stormed {
		_, err := svc.Verify(ctx, tok)
		if !errors.Is(err, ErrTokenRevoked) {
			t.Fatalf("logged-out session %d accepted again: %v", i, err)
		}
	}
	if _, err := svc.Verify(ctx, apiIss.Token); err != nil {
		t.Fatalf("alice's API token affected: %v", err)
	}
	// Documented collateral: alice's session that predates the collapse is ended too.
	_, err = svc.Verify(ctx, aliceOpen.Token)
	wantInvalid(t, err, ErrTokenRevoked)
	fresh, _ := svc.IssueLoginSession(ctx, e.account("alice"), time.Hour)
	if _, err := svc.Verify(ctx, fresh.Token); err != nil {
		t.Fatalf("new session after the storm: %v", err)
	}
	if n := revs.table.Len(); n > 20 {
		t.Fatalf("store holds %d entries", n)
	}
}

// Regression: a revoked token must stay revoked while the verifier still accepts it
// through the clock-skew leeway, even if an unrelated write prunes the store.
func TestRevokedTokenStaysRevokedThroughLeeway(t *testing.T) {
	t.Parallel()
	e := newTokenEnv(t)
	ctx := context.Background()
	iss, _ := e.svc.IssueLoginSession(ctx, e.account("alice"), time.Minute)
	p, err := e.svc.Verify(ctx, iss.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Revoke(ctx, p); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute + DefaultLeeway/2) // expired, but inside the leeway
	other, _ := e.svc.IssueLoginSession(ctx, e.account("bob"), time.Hour)
	op, err := e.svc.Verify(ctx, other.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Revoke(ctx, op); err != nil { // prunes expired entries
		t.Fatal(err)
	}
	_, err = e.svc.Verify(ctx, iss.Token)
	wantInvalid(t, err, ErrTokenRevoked)
}
