package auth

import (
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Revocation table limits. The defaults keep the serialized table (Secret data) at or
// below 512 KiB of keys and values, about half of the 1 MiB Secret limit and a third of
// etcd's default 1.5 MiB request limit even after JSON/base64 encoding overhead.
const (
	DefaultRevocationMaxEntries           = 8192
	DefaultRevocationMaxBytes             = 512 << 10
	DefaultRevocationMaxEntriesPerSubject = 1000

	maxRevocationIDBytes      = 1024
	maxRevocationSubjectBytes = 4096

	revTokenPrefix     = "t."
	revWatermarkPrefix = "w."
	revSubjectTagBytes = 12 // 96-bit subject tag, 16 base64url characters
	revTokenTagBytes   = 16 // 128-bit token tag, 22 base64url characters
)

var (
	revSubjectTagLen = base64.RawURLEncoding.EncodedLen(revSubjectTagBytes)
	revTokenTagLen   = base64.RawURLEncoding.EncodedLen(revTokenTagBytes)
	revTokenKeyLen   = len(revTokenPrefix) + revSubjectTagLen + 1 + revTokenTagLen
	revMarkKeyLen    = len(revWatermarkPrefix) + revSubjectTagLen
)

// RevocationLimits bounds a RevocationTable. Zero or negative fields select the defaults.
type RevocationLimits struct {
	// MaxEntries bounds token entries plus watermarks.
	MaxEntries int
	// MaxBytes bounds the summed length of encoded keys and values (see Data).
	MaxBytes int
	// MaxEntriesPerSubject is how many individual revocations one subject may hold before
	// they are collapsed into the subject's watermark.
	MaxEntriesPerSubject int
}

func (l RevocationLimits) withDefaults() RevocationLimits {
	if l.MaxEntries <= 0 {
		l.MaxEntries = DefaultRevocationMaxEntries
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultRevocationMaxBytes
	}
	if l.MaxEntriesPerSubject <= 0 {
		l.MaxEntriesPerSubject = DefaultRevocationMaxEntriesPerSubject
	}
	return l
}

type revTokenKey struct{ subject, token string } // tags

type revToken struct{ exp, iat int64 }

// revMark revokes every token of a subject with iat <= cutoff while now < until.
type revMark struct{ cutoff, until int64 }

// RevocationTable is the bounded state behind the revocation stores, with a stable
// serialized form (Data, DecodeRevocationTable) usable as Secret data.
//
// Entries are keyed by fixed-length hashes, never by the raw token ID or subject:
//
//	t.<subject tag>.<token tag> = "<exp>.<iat>"   one revoked token
//	w.<subject tag>             = "<cutoff>.<until>" subject watermark
//
// Bounding works in two steps. When a subject holds more than MaxEntriesPerSubject
// token entries, they are collapsed into the subject's watermark: cutoff becomes the
// newest collapsed iat and until the latest collapsed expiry, so every collapsed token
// stays revoked, and the subject's other tokens issued at or before cutoff are revoked
// too until then. When the table still exceeds MaxEntries or MaxBytes, the subjects
// holding the most entries are collapsed first. Revoke fails with ErrRevocationStoreFull
// only when every subject is already down to a single entry and the limits are still
// exceeded, which takes about MaxEntries distinct subjects with live revocations. A
// single principal therefore holds at most MaxEntriesPerSubject + 1 entries, shrinks to
// one entry under pressure, and cannot make other principals' revocations fail.
//
// A table is not safe for concurrent mutation; IsRevoked, Len, Size and Data only read.
type RevocationTable struct {
	tokens map[revTokenKey]revToken
	marks  map[string]revMark
	size   int
}

// NewRevocationTable returns an empty table.
func NewRevocationTable() *RevocationTable {
	return &RevocationTable{tokens: map[revTokenKey]revToken{}, marks: map[string]revMark{}}
}

// DecodeRevocationTable parses the form produced by Data. It returns the table and the
// number of entries it ignored because they were malformed (including entries written
// by older formats).
func DecodeRevocationTable(data map[string][]byte) (*RevocationTable, int) {
	t := NewRevocationTable()
	bad := 0
	for k, v := range data {
		a, b, ok := parseRevPair(v)
		if !ok {
			bad++
			continue
		}
		if rest, ok := strings.CutPrefix(k, revTokenPrefix); ok {
			subj, tok, ok := strings.Cut(rest, ".")
			if !ok || !isRevTag(subj, revSubjectTagLen) || !isRevTag(tok, revTokenTagLen) {
				bad++
				continue
			}
			t.putToken(revTokenKey{subj, tok}, revToken{exp: a, iat: b})
			continue
		}
		if subj, ok := strings.CutPrefix(k, revWatermarkPrefix); ok && isRevTag(subj, revSubjectTagLen) {
			t.putMark(subj, revMark{cutoff: a, until: b})
			continue
		}
		bad++
	}
	return t, bad
}

// Data returns the serialized table in a new map.
func (t *RevocationTable) Data() map[string][]byte {
	out := make(map[string][]byte, t.Len())
	for k, r := range t.tokens {
		out[revTokenPrefix+k.subject+"."+k.token] = revPair(r.exp, r.iat)
	}
	for s, m := range t.marks {
		out[revWatermarkPrefix+s] = revPair(m.cutoff, m.until)
	}
	return out
}

// Len returns the number of entries (token entries plus watermarks).
func (t *RevocationTable) Len() int { return len(t.tokens) + len(t.marks) }

// Size returns the summed length of the keys and values returned by Data.
func (t *RevocationTable) Size() int { return t.size }

// IsRevoked reports whether ref is revoked at now, individually or by its subject's
// watermark. ref.ExpiresAt is not consulted.
func (t *RevocationTable) IsRevoked(ref TokenRef, now time.Time) bool {
	subj := revSubjectTag(ref.Subject)
	if ref.ID != "" {
		if _, ok := t.tokens[revTokenKey{subj, revTokenTag(ref.ID)}]; ok {
			return true
		}
	}
	m, ok := t.marks[subj]
	return ok && now.Unix() < m.until && unixOrZero(ref.IssuedAt) <= m.cutoff
}

// Revoke records ref (see the type documentation for the bounding rules) after pruning
// expired entries. It reports whether the table changed. On error nothing but the
// pruning of expired entries has been applied.
func (t *RevocationTable) Revoke(ref TokenRef, now time.Time, lim RevocationLimits) (bool, error) {
	if err := ref.Validate(); err != nil {
		return false, err
	}
	if !ref.ExpiresAt.After(now) {
		return false, nil // already expired: nothing to revoke
	}
	lim = lim.withDefaults()
	pruned := t.Prune(now) > 0
	subj, exp, iat := revSubjectTag(ref.Subject), ceilUnix(ref.ExpiresAt), unixOrZero(ref.IssuedAt)
	key := revTokenKey{subj, revTokenTag(ref.ID)}
	var undo func()
	if m, ok := t.marks[subj]; ok && iat <= m.cutoff {
		// Already covered by the watermark; make sure it lasts as long as this token.
		if exp > m.until {
			t.putMark(subj, revMark{cutoff: m.cutoff, until: exp})
			undo = func() { t.putMark(subj, m) }
		}
	} else if r, ok := t.tokens[key]; ok {
		// Same ID again (e.g. a reused account token ID): keep the latest expiry and iat
		// so that a later collapse still covers every token reported revoked.
		if exp > r.exp || iat > r.iat {
			t.putToken(key, revToken{exp: max(exp, r.exp), iat: max(iat, r.iat)})
			undo = func() { t.putToken(key, r) }
		}
	} else {
		t.putToken(key, revToken{exp: exp, iat: iat})
		undo = func() { t.delToken(key) }
	}
	if undo == nil {
		return pruned, nil
	}
	if err := t.enforce(lim); err != nil {
		undo()
		return false, err
	}
	return true, nil
}

// Prune removes entries that expired at or before now and returns how many it removed.
func (t *RevocationTable) Prune(now time.Time) int {
	nowUnix, n := now.Unix(), 0
	for k, r := range t.tokens {
		if r.exp <= nowUnix {
			t.delToken(k)
			n++
		}
	}
	for s, m := range t.marks {
		if m.until <= nowUnix {
			t.delMark(s)
			n++
		}
	}
	return n
}

// enforce collapses subjects until the table satisfies lim. It first plans the
// collapses and only applies them when the plan fits, so on ErrRevocationStoreFull the
// table is unchanged.
func (t *RevocationTable) enforce(lim RevocationLimits) error {
	bySubject := map[string][]revTokenKey{}
	for k := range t.tokens {
		bySubject[k.subject] = append(bySubject[k.subject], k)
	}
	count, size := t.Len(), t.size
	var plan []string
	take := func(s string) {
		dCount, dSize := t.collapseEffect(s, bySubject[s])
		count, size = count+dCount, size+dSize
		plan = append(plan, s)
		delete(bySubject, s)
	}
	for s, keys := range bySubject {
		if len(keys) > lim.MaxEntriesPerSubject {
			take(s)
		}
	}
	if count > lim.MaxEntries || size > lim.MaxBytes {
		// Largest subjects first; ties broken by tag for determinism.
		subjects := slices.SortedFunc(maps.Keys(bySubject), func(a, b string) int {
			return cmp.Or(cmp.Compare(len(bySubject[b]), len(bySubject[a])), strings.Compare(a, b))
		})
		for _, s := range subjects {
			if count <= lim.MaxEntries && size <= lim.MaxBytes {
				break
			}
			dCount, dSize := t.collapseEffect(s, bySubject[s])
			if (count > lim.MaxEntries && dCount < 0) || (size > lim.MaxBytes && dSize < 0) {
				take(s)
			}
		}
		if count > lim.MaxEntries || size > lim.MaxBytes {
			return fmt.Errorf("%w: %d entries / %d bytes exceed limits of %d entries / %d bytes",
				ErrRevocationStoreFull, count, size, lim.MaxEntries, lim.MaxBytes)
		}
	}
	for _, s := range plan {
		t.collapse(s)
	}
	return nil
}

// collapsed returns the watermark subject s would have after folding keys into it.
func (t *RevocationTable) collapsed(s string, keys []revTokenKey) revMark {
	m := t.marks[s]
	for _, k := range keys {
		r := t.tokens[k]
		m.cutoff = max(m.cutoff, r.iat)
		m.until = max(m.until, r.exp)
	}
	return m
}

// collapseEffect returns how collapsing keys of subject s would change Len and Size.
func (t *RevocationTable) collapseEffect(s string, keys []revTokenKey) (dCount, dSize int) {
	dCount = -len(keys)
	for _, k := range keys {
		r := t.tokens[k]
		dSize -= revTokenKeyLen + revPairLen(r.exp, r.iat)
	}
	if old, ok := t.marks[s]; ok {
		dSize -= revMarkKeyLen + revPairLen(old.cutoff, old.until)
	} else {
		dCount++
	}
	m := t.collapsed(s, keys)
	dSize += revMarkKeyLen + revPairLen(m.cutoff, m.until)
	return dCount, dSize
}

// collapse folds every token entry of subject s into its watermark.
func (t *RevocationTable) collapse(s string) {
	var keys []revTokenKey
	for k := range t.tokens {
		if k.subject == s {
			keys = append(keys, k)
		}
	}
	m := t.collapsed(s, keys)
	for _, k := range keys {
		t.delToken(k)
	}
	t.putMark(s, m)
}

func (t *RevocationTable) putToken(k revTokenKey, r revToken) {
	if old, ok := t.tokens[k]; ok {
		t.size -= revTokenKeyLen + revPairLen(old.exp, old.iat)
	}
	t.tokens[k] = r
	t.size += revTokenKeyLen + revPairLen(r.exp, r.iat)
}

func (t *RevocationTable) delToken(k revTokenKey) {
	if old, ok := t.tokens[k]; ok {
		t.size -= revTokenKeyLen + revPairLen(old.exp, old.iat)
		delete(t.tokens, k)
	}
}

func (t *RevocationTable) putMark(s string, m revMark) {
	if old, ok := t.marks[s]; ok {
		t.size -= revMarkKeyLen + revPairLen(old.cutoff, old.until)
	}
	t.marks[s] = m
	t.size += revMarkKeyLen + revPairLen(m.cutoff, m.until)
}

func (t *RevocationTable) delMark(s string) {
	if old, ok := t.marks[s]; ok {
		t.size -= revMarkKeyLen + revPairLen(old.cutoff, old.until)
		delete(t.marks, s)
	}
}

func revTag(domain, s string, n int) string {
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write([]byte{0})
	h.Write([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:n])
}

func revSubjectTag(subject string) string {
	return revTag("patrol-revocation-subject", subject, revSubjectTagBytes)
}

func revTokenTag(id string) string { return revTag("patrol-revocation-token", id, revTokenTagBytes) }

func isRevTag(s string, n int) bool {
	if len(s) != n {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return !isBase64URLChar(r) }) < 0
}

func isBase64URLChar(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || r == '-' || r == '_'
}

func revPair(a, b int64) []byte {
	out := strconv.AppendInt(make([]byte, 0, revPairLen(a, b)), a, 10)
	out = append(out, '.')
	return strconv.AppendInt(out, b, 10)
}

func revPairLen(a, b int64) int { return decimalLen(a) + 1 + decimalLen(b) }

func decimalLen(n int64) int {
	var buf [20]byte
	return len(strconv.AppendInt(buf[:0], n, 10))
}

func parseRevPair(v []byte) (int64, int64, bool) {
	as, bs, ok := strings.Cut(string(v), ".")
	if !ok {
		return 0, 0, false
	}
	a, errA := strconv.ParseInt(as, 10, 64)
	b, errB := strconv.ParseInt(bs, 10, 64)
	if errA != nil || errB != nil || a < 0 || b < 0 {
		return 0, 0, false
	}
	return a, b, true
}

// ceilUnix rounds up so an entry never lapses before the token it covers.
func ceilUnix(t time.Time) int64 {
	u := t.Unix()
	if t.Truncate(time.Second).Before(t) {
		u++
	}
	return u
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return max(t.Unix(), 0)
}
