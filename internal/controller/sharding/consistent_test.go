package sharding

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"testing"

	"pgregory.net/rapid"
)

var consistentRegression = map[string]int{"a": 1, "b": 2, "c": 2, "d": 0, "e": 0, "f": 0, "g": 2, "h": 1}

// naiveRing mirrors the structure of the baseline's ring (a hash → member map overwritten in
// insertion order, a successor search from the last visited hash, a load map), written
// independently of boundedRing as a differential oracle.
type naiveRing struct {
	owner map[uint64]string
	keys  []uint64
	load  map[string]int64
	total int64
}

func newNaiveRing(n int) *naiveRing {
	r := &naiveRing{owner: map[uint64]string{}, load: map[string]int64{}}
	for m := range n {
		name := fmt.Sprint(m)
		r.load[name] = 0
		for i := range 1000 {
			r.owner[ringHash(fmt.Sprintf("%s%d", name, i))] = name
		}
	}
	for k := range r.owner {
		r.keys = append(r.keys, k)
	}
	slices.Sort(r.keys)
	return r
}

func (r *naiveRing) getLeast(key string) string {
	h := ringHash(key)
	for {
		// successor strictly greater than h, wrapping to the smallest key
		idx := slices.IndexFunc(r.keys, func(k uint64) bool { return k > h })
		next := r.keys[0]
		if idx >= 0 {
			next = r.keys[idx]
		}
		host := r.owner[next]
		avg := float64((r.total + 1) / int64(len(r.load)))
		if avg == 0 {
			avg = 1
		}
		if float64(r.load[host]) < math.Ceil(avg*1.25) {
			return host
		}
		h = next
	}
}

func TestConsistentHashingMatchesNaiveRing(t *testing.T) {
	t.Parallel()
	rings := map[int]*naiveRing{}
	rapid.Check(t, func(t *rapid.T) {
		replicas := rapid.IntRange(1, 13).Draw(t, "replicas")
		n := rapid.IntRange(0, 12).Draw(t, "clusters")
		var list []ClusterInfo
		apps := map[string]int64{}
		for i := range n {
			c := cl(rapid.StringMatching(`[a-z0-9-]{1,12}`).Draw(t, "id") + "/" + strconv.Itoa(i))
			list = append(list, c)
			apps[c.Server] = rapid.Int64Range(0, 30).Draw(t, "apps")
		}
		got := ConsistentHashingShards(list, apps, replicas)

		base, ok := rings[replicas]
		if !ok {
			base = newNaiveRing(replicas)
			rings[replicas] = base
		}
		ref := &naiveRing{owner: base.owner, keys: base.keys, load: map[string]int64{}}
		for m := range base.load {
			ref.load[m] = 0
		}
		for _, c := range sortedByID(list) {
			member := ref.getLeast(c.ID)
			want, _ := strconv.Atoi(member)
			if got[c.ID] != want {
				t.Fatalf("cluster %s: got shard %d, reference %d", c.ID, got[c.ID], want)
			}
			ref.load[member] += apps[c.Server]
			ref.total += apps[c.Server]
		}
	})
}
