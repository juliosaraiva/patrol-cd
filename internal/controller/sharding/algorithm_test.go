package sharding

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
	"pgregory.net/rapid"
)

func cl(id string) ClusterInfo {
	return ClusterInfo{ID: id, Name: "cluster" + id, Server: "https://kubernetes.default.svc?" + id}
}

func pinnedCl(id string, shard int64) ClusterInfo {
	c := cl(id)
	c.Shard = ptr.To(shard)
	return c
}

// Golden values from the baseline's tests (controller/sharding/sharding_test.go).
func TestLegacyShardGolden(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		c        *ClusterInfo
		replicas int
		want     int
	}{
		{"id 1, 5 replicas", ptr.To(cl("1")), 5, 4},
		{"id 2, 5 replicas", ptr.To(cl("2")), 5, 1},
		{"id 3, 5 replicas", ptr.To(cl("3")), 5, 2},
		{"id 4, 5 replicas", ptr.To(cl("4")), 5, 2},
		{"one replica", ptr.To(cl("3")), 1, 0},
		{"nil cluster (in-cluster)", nil, 5, 0},
		{"empty id", ptr.To(cl("")), 5, 0},
		{"no replicas", ptr.To(cl("1")), 0, -1},
		{"no replicas nil", nil, 0, -1},
		{"pinned below replicas", ptr.To(pinnedCl("5", 4)), 5, 4},
		{"pinned 1", ptr.To(pinnedCl("4", 1)), 5, 1},
		{"pinned beyond replicas is assigned automatically", ptr.To(pinnedCl("1", 7)), 5, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, LegacyShard(tc.c, tc.replicas))
		})
	}
}

func TestLegacyShardIsFNV32a(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		id := rapid.StringN(1, 64, -1).Draw(t, "id")
		replicas := rapid.IntRange(1, MaxReplicas).Draw(t, "replicas")
		h := fnv.New32a()
		_, _ = h.Write([]byte(id))
		want := int(h.Sum32() % uint32(replicas)) //nolint:gosec // bounded
		c := ClusterInfo{ID: id}
		if got := LegacyShard(&c, replicas); got != want {
			t.Fatalf("LegacyShard(%q, %d) = %d, want %d", id, replicas, got, want)
		}
	})
}

func TestRoundRobinShardGolden(t *testing.T) {
	t.Parallel()
	list := []ClusterInfo{cl("1"), cl("2"), cl("3"), cl("4"), cl("5")}
	t.Run("two replicas", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 0, RoundRobinShard(nil, list, 2))
		assert.Equal(t, []int{0, 1, 0, 1, 0}, rrAll(list, list, 2))
	})
	t.Run("four replicas with a pin", func(t *testing.T) {
		t.Parallel()
		withPin := slices.Clone(list)
		withPin[4] = pinnedCl("5", 1)
		assert.Equal(t, []int{0, 1, 2, 3, 1}, rrAll(withPin, withPin, 4))
		pinned4 := pinnedCl("4", 1)
		assert.Equal(t, 1, RoundRobinShard(&pinned4, withPin, 4))
	})
	t.Run("5 clusters over 2 replicas give 3/2", func(t *testing.T) {
		t.Parallel()
		got := rrAll(list, list, 2)
		count := map[int]int{}
		for _, s := range got {
			count[s]++
		}
		assert.Equal(t, map[int]int{0: 3, 1: 2}, count)
	})
	t.Run("no replicas", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, -1, RoundRobinShard(nil, list, 0))
		assert.Equal(t, []int{-1, -1, -1, -1, -1}, rrAll(list, list, 0))
	})
	t.Run("unknown cluster", func(t *testing.T) {
		t.Parallel()
		other := cl("9")
		assert.Equal(t, -1, RoundRobinShard(&other, list, 2))
	})
}

// The reshuffle scenario of the baseline's TestShuffle: removing the first-ranked cluster
// moves every other cluster, adding one in the middle moves the ones after it.
func TestRoundRobinReshuffle(t *testing.T) {
	t.Parallel()
	c10, c20, c30, c40, c50, c60, c25 := cl("10"), cl("20"), cl("30"), cl("40"), cl("50"), cl("60"), cl("25")
	all := []ClusterInfo{c10, c20, c30, c40, c50, c60}
	assert.Equal(t, []int{0, 1, 2, 0, 1, 2}, rrAll(all, all, 3))

	without := all[1:]
	assert.Equal(t, -1, RoundRobinShard(&c10, without, 3))
	assert.Equal(t, []int{0, 1, 2, 0, 1}, rrAll(without, without, 3))

	with25 := append(slices.Clone(without), c25)
	assert.Equal(t, []int{0, 2, 0, 1, 2, 1}, rrAll([]ClusterInfo{c20, c30, c40, c50, c60, c25}, with25, 3))
}

func rrAll(targets, list []ClusterInfo, replicas int) []int {
	out := make([]int, len(targets))
	for i := range targets {
		out[i] = RoundRobinShard(&targets[i], list, replicas)
	}
	return out
}

// BLAKE2b-512("abc") (RFC 7693 appendix A) is ba80a53f981c4d0d…; the ring uses the first
// eight bytes as a little-endian integer.
func TestRingHashIsBLAKE2b512LittleEndian(t *testing.T) {
	t.Parallel()
	assert.Equal(t, uint64(0x0d4d1c983fa580ba), ringHash("abc"))
}

func TestRingVirtualNodeCollisionsFollowInsertionOrder(t *testing.T) {
	t.Parallel()
	// With 12 members "1"+"11" and "11"+"1" are the same virtual node key; the later member
	// ("11") owns it.
	r := ringFor(12)
	h := ringHash("111")
	i, found := slices.BinarySearch(r.hashes, h)
	require.True(t, found)
	assert.Equal(t, "11", r.members[r.owners[i]])
	assert.Len(t, r.hashes, len(slices.Compact(slices.Clone(r.hashes))))
	assert.Less(t, len(r.hashes), 12*ringReplicationFactor, "colliding virtual nodes are deduplicated")
}

// The baseline's TestConsistentHashingWhenClusterIsAddedAndRemoved: 133 clusters over 3
// replicas stay within ±10% of the average; going to 2 replicas only moves clusters of the
// removed shard; a removed cluster is unassigned.
func TestConsistentHashingDistributionAndScaleDown(t *testing.T) {
	t.Parallel()
	var list []ClusterInfo
	for i := range 133 {
		list = append(list, cl(fmt.Sprintf("%06d", i)))
	}
	apps := map[string]int64{"cluster1": 2, "cluster2": 2, "cluster3": 1} // no server matches: no load
	three := ConsistentHashingShards(list, apps, 3)
	count := map[int]int{}
	for _, s := range three {
		count[s]++
	}
	avg := 133.0 / 3
	for s, n := range count {
		assert.GreaterOrEqual(t, float64(n), avg*0.9, "shard %d", s)
		assert.LessOrEqual(t, float64(n), avg*1.1, "shard %d", s)
	}
	two := ConsistentHashingShards(list, apps, 2)
	for _, c := range list {
		if three[c.ID] != 2 {
			assert.Equal(t, three[c.ID], two[c.ID], "cluster %s moved although its shard still exists", c.ID)
		}
	}
	removed := list[len(list)-1]
	assert.Equal(t, -1, ConsistentHashingShard(&removed, list[:len(list)-1], apps, 2))
	assert.Equal(t, two[removed.ID], ConsistentHashingShard(&removed, list, apps, 2))
}

func TestConsistentHashingEdgeCases(t *testing.T) {
	t.Parallel()
	list := []ClusterInfo{cl("01")}
	assert.Equal(t, -1, ConsistentHashingShard(nil, list, nil, 0))
	assert.Equal(t, 0, ConsistentHashingShard(nil, list, nil, 3))
	pinned := pinnedCl("1", 1)
	assert.Equal(t, 1, ConsistentHashingShard(&pinned, []ClusterInfo{pinned}, nil, 5))
	assert.Empty(t, ConsistentHashingShards(list, nil, 0))
}

// Regression values produced by this implementation (not upstream output): they pin the ring
// so that an accidental change of hashing or iteration is caught.
func TestConsistentHashingRegression(t *testing.T) {
	t.Parallel()
	list := []ClusterInfo{cl("a"), cl("b"), cl("c"), cl("d"), cl("e"), cl("f"), cl("g"), cl("h")}
	apps := map[string]int64{list[0].Server: 5, list[1].Server: 1, list[2].Server: 3, list[5].Server: 8}
	got := ConsistentHashingShards(list, apps, 3)
	assert.Equal(t, consistentRegression, got)
}

// Bounded loads: when a cluster is assigned, its member is under the bound
// ceil(max(floor((T+1)/n), 1) × 1.25), so no member ends above that bound − 1 plus the largest
// cluster weight; clusters without Applications never move when one is added.
func TestConsistentHashingBoundedLoadsProperty(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		replicas := rapid.IntRange(1, 12).Draw(t, "replicas")
		n := rapid.IntRange(0, 60).Draw(t, "clusters")
		var list []ClusterInfo
		apps := map[string]int64{}
		var total, maxW int64
		for i := range n {
			c := cl(rapid.StringMatching(`[a-f0-9]{8}`).Draw(t, "id") + strconv.Itoa(i))
			list = append(list, c)
			w := rapid.Int64Range(0, 20).Draw(t, "apps")
			apps[c.Server] = w
			total += w
			maxW = max(maxW, w)
		}
		got := ConsistentHashingShards(list, apps, replicas)
		loads := make([]int64, replicas)
		for _, c := range list {
			s := got[c.ID]
			if s < 0 || s >= replicas {
				t.Fatalf("cluster %s got shard %d with %d replicas", c.ID, s, replicas)
			}
			loads[s] += apps[c.Server]
		}
		avg := float64((total + 1) / int64(replicas))
		if avg == 0 {
			avg = 1
		}
		bound := int64(math.Ceil(avg*ringLoadFactor)) - 1 + maxW
		for s, l := range loads {
			if l > bound {
				t.Fatalf("shard %d load %d exceeds bound %d (total %d)", s, l, bound, total)
			}
		}
		// Determinism regardless of input order.
		shuffled := slices.Clone(list)
		slices.Reverse(shuffled)
		if again := ConsistentHashingShards(shuffled, apps, replicas); !mapsEqual(again, got) {
			t.Fatalf("assignment depends on input order")
		}
	})
}

func TestConsistentHashingAddingUnloadedClusterMovesNothing(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		replicas := rapid.IntRange(2, 8).Draw(t, "replicas")
		n := rapid.IntRange(1, 40).Draw(t, "clusters")
		var list []ClusterInfo
		for i := range n {
			list = append(list, cl(strconv.Itoa(rapid.IntRange(0, 1<<30).Draw(t, "id"))+"-"+strconv.Itoa(i)))
		}
		before := ConsistentHashingShards(list, nil, replicas)
		added := append(slices.Clone(list), cl("new-cluster"))
		after := ConsistentHashingShards(added, nil, replicas)
		for _, c := range list {
			if before[c.ID] != after[c.ID] {
				t.Fatalf("cluster %s moved from %d to %d", c.ID, before[c.ID], after[c.ID])
			}
		}
	})
}

func TestDistribute(t *testing.T) {
	t.Parallel()
	list := []ClusterInfo{cl("1"), cl("2"), cl("3"), cl("4"), pinnedCl("5", 1), pinnedCl("6", 9)}
	t.Run("legacy", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		got := Distribute(AlgorithmLegacy, list, nil, 5, slog.New(slog.NewTextHandler(&buf, nil)))
		assert.Equal(t, map[string]int{list[0].Server: 4, list[1].Server: 1, list[2].Server: 2, list[3].Server: 2, list[4].Server: 1, list[5].Server: 0}, got)
		assert.Contains(t, buf.String(), "Specified cluster shard (9) for cluster: https://kubernetes.default.svc?6 is greater than the number of available shard (5). Using shard 0.")
	})
	t.Run("one replica processes everything", func(t *testing.T) {
		t.Parallel()
		for _, alg := range []Algorithm{AlgorithmLegacy, AlgorithmRoundRobin, AlgorithmConsistentHashing} {
			for _, s := range Distribute(alg, list, nil, 1, nil) {
				assert.Equal(t, 0, s)
			}
		}
	})
	t.Run("round-robin ranks include pinned clusters", func(t *testing.T) {
		t.Parallel()
		got := Distribute(AlgorithmRoundRobin, list, nil, 2, nil)
		assert.Equal(t, []int{0, 1, 0, 1, 1, 0}, []int{got[list[0].Server], got[list[1].Server], got[list[2].Server], got[list[3].Server], got[list[4].Server], got[list[5].Server]})
	})
	t.Run("consistent-hashing matches ConsistentHashingShards", func(t *testing.T) {
		t.Parallel()
		apps := map[string]int64{list[0].Server: 3}
		got := Distribute(AlgorithmConsistentHashing, list, apps, 3, nil)
		ring := ConsistentHashingShards(list, apps, 3)
		for _, c := range list[:4] {
			assert.Equal(t, ring[c.ID], got[c.Server])
		}
		assert.Equal(t, 1, got[list[4].Server])
	})
	t.Run("synthetic in-cluster (empty id) is shard 0 for legacy and round-robin", func(t *testing.T) {
		t.Parallel()
		withLocal := append([]ClusterInfo{{Server: "https://kubernetes.default.svc", Name: "in-cluster"}}, list[:4]...)
		assert.Equal(t, 0, Distribute(AlgorithmLegacy, withLocal, nil, 3, nil)["https://kubernetes.default.svc"])
		assert.Equal(t, 0, Distribute(AlgorithmRoundRobin, withLocal, nil, 3, nil)["https://kubernetes.default.svc"])
	})
}

func TestParseAlgorithm(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	assert.Equal(t, AlgorithmLegacy, ParseAlgorithm("", logger))
	assert.Equal(t, AlgorithmRoundRobin, ParseAlgorithm("round-robin", logger))
	assert.Equal(t, AlgorithmConsistentHashing, ParseAlgorithm("consistent-hashing", logger))
	assert.Empty(t, buf.String())
	assert.Equal(t, AlgorithmLegacy, ParseAlgorithm("foo", logger))
	assert.Contains(t, buf.String(), "distribution type foo is not supported, defaulting to legacy")
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
