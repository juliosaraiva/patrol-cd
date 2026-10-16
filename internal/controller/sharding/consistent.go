package sharding

import (
	"cmp"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"strconv"
	"sync"

	"golang.org/x/crypto/blake2b"
)

// Consistent hashing with bounded loads (Mirrokni, Thorup, Zadimoghaddam), re-implemented to
// produce the same assignments as the ring upstream's sharding uses: members get
// ringReplicationFactor virtual nodes hashed from "<member><i>"; keys and virtual nodes are
// hashed with BLAKE2b-512 (first 8 bytes, little endian); a key is served by the first
// virtual node strictly after its hash (wrapping around) whose member is under the load
// bound ceil(floor((total+1)/members) × 1.25) (at least ceil(1.25)).

const (
	ringReplicationFactor = 1000
	ringLoadFactor        = 1.25
)

// errNoMembers is returned by getLeast on an empty ring (upstream ErrNoHosts).
var errNoMembers = errors.New("no hosts added")

// ringNodes is the immutable part of a ring: sorted unique virtual-node hashes and the member
// owning each. It depends only on the member count, so it is cached.
type ringNodes struct {
	hashes  []uint64
	owners  []int32 // owners[i] serves hashes[i]
	members []string
}

var (
	ringCacheMu sync.Mutex
	ringCache   = map[int]*ringNodes{}
)

// ringFor returns the virtual nodes of a ring with members "0".."n-1". Members are added in
// order and a later member takes over a colliding virtual node (for example "1"+"11" and
// "11"+"1"), as with upstream's map-and-btree ring.
func ringFor(n int) *ringNodes {
	ringCacheMu.Lock()
	defer ringCacheMu.Unlock()
	if r, ok := ringCache[n]; ok {
		return r
	}
	type vnode struct {
		h uint64
		m int32
	}
	all := make([]vnode, 0, n*ringReplicationFactor)
	r := &ringNodes{members: make([]string, n)}
	for m := range n {
		name := strconv.Itoa(m)
		r.members[m] = name
		for i := range ringReplicationFactor {
			all = append(all, vnode{h: ringHash(name + strconv.Itoa(i)), m: int32(m)}) //nolint:gosec // m < MaxReplicas
		}
	}
	// Stable: among equal hashes the insertion order is kept, and the last insertion wins.
	slices.SortStableFunc(all, func(a, b vnode) int { return cmp.Compare(a.h, b.h) })
	r.hashes = make([]uint64, 0, len(all))
	r.owners = make([]int32, 0, len(all))
	for i, v := range all {
		if i+1 < len(all) && all[i+1].h == v.h {
			continue
		}
		r.hashes = append(r.hashes, v.h)
		r.owners = append(r.owners, v.m)
	}
	if len(ringCache) >= 8 {
		clear(ringCache)
	}
	ringCache[n] = r
	return r
}

// ringHash is the hash of a ring key or virtual node.
func ringHash(key string) uint64 {
	sum := blake2b.Sum512([]byte(key))
	return binary.LittleEndian.Uint64(sum[:8])
}

// boundedRing is a ring with per-member loads. It is not safe for concurrent use.
type boundedRing struct {
	nodes *ringNodes
	load  map[string]int64
	total int64
}

func newBoundedRing(members int) *boundedRing {
	nodes := ringFor(members)
	load := make(map[string]int64, members)
	for _, m := range nodes.members {
		load[m] = 0
	}
	return &boundedRing{nodes: nodes, load: load}
}

// updateLoad sets the load of member (unknown members are ignored).
func (r *boundedRing) updateLoad(member string, load int64) {
	old, ok := r.load[member]
	if !ok {
		return
	}
	r.total += load - old
	r.load[member] = load
}

// loadOK reports whether member may take one more key.
func (r *boundedRing) loadOK(member string) bool {
	total := max(r.total, 0)
	avg := float64((total + 1) / int64(len(r.load)))
	if avg == 0 {
		avg = 1
	}
	avg = math.Ceil(avg * ringLoadFactor)
	return float64(r.load[member]) < avg
}

// getLeast returns the member serving key under the load bound.
func (r *boundedRing) getLeast(key string) (string, error) {
	hashes := r.nodes.hashes
	if len(hashes) == 0 {
		return "", errNoMembers
	}
	h := ringHash(key)
	i, _ := slices.BinarySearch(hashes, h)
	if i < len(hashes) && hashes[i] == h {
		i++ // the first node strictly after the key
	}
	for range hashes {
		if i >= len(hashes) {
			i = 0
		}
		member := r.nodes.members[r.nodes.owners[i]]
		if r.loadOK(member) {
			return member, nil
		}
		i++
	}
	// Unreachable for non-negative loads: the least loaded member is always under the bound.
	return "", errors.New("no member under the load bound")
}
