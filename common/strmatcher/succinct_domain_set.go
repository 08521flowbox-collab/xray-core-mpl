package strmatcher

import (
	"math/bits"
	"sort"
	"strings"
)

// DomainSet is a static set of domain rules stored as a succinct trie: the
// reversed names packed into a label byte array plus two bitmaps, in the
// manner of openacid/succinct (MIT). Building it allocates roughly the size
// of the input; the result is a fraction of the input.
//
// A suffix rule ("domain:" type) matches the name itself and any subdomain;
// a full rule matches the name only. Suffix rules are stored with a trailing
// marker byte so both kinds share one trie.
type DomainSet struct {
	leaves, labelBitmap []uint64
	labels              []byte
	ranks               []int32
}

const domainSuffixMark = 0

func NewDomainSet(full, suffix []string) *DomainSet {
	keys := make([]string, 0, len(full)+len(suffix))
	for _, d := range full {
		if d != "" {
			keys = append(keys, reverseDomain(d))
		}
	}
	for _, d := range suffix {
		if d != "" {
			keys = append(keys, reverseDomain(d)+string(rune(domainSuffixMark)))
		}
	}
	sort.Strings(keys)
	n := 0
	for i, k := range keys {
		if i == 0 || k != keys[i-1] {
			keys[n] = k
			n++
		}
	}
	keys = keys[:n]

	ss := &DomainSet{}
	lIdx, nodeId, col := 0, 0, 0
	type span struct{ s, e int32 }
	level := []span{{0, int32(len(keys))}}
	var next []span
	for len(level) > 0 {
		next = next[:0]
		for _, elt := range level {
			if elt.s < elt.e && col == len(keys[elt.s]) {
				elt.s++
				setBit(&ss.leaves, nodeId, 1)
			}
			for j := elt.s; j < elt.e; {
				frm := j
				for ; j < elt.e && keys[j][col] == keys[frm][col]; j++ {
				}
				next = append(next, span{frm, j})
				ss.labels = append(ss.labels, keys[frm][col])
				setBit(&ss.labelBitmap, lIdx, 0)
				lIdx++
			}
			setBit(&ss.labelBitmap, lIdx, 1)
			lIdx++
			nodeId++
		}
		level, next = next, level
		col++
	}
	ss.ranks = make([]int32, len(ss.labelBitmap)+1)
	for i, w := range ss.labelBitmap {
		ss.ranks[i+1] = ss.ranks[i] + int32(bits.OnesCount64(w))
	}
	return ss
}

// Size reports how many rules were stored.
func (ss *DomainSet) Size() int {
	n := 0
	for _, w := range ss.leaves {
		n += bits.OnesCount64(w)
	}
	return n
}

// Match reports whether the domain equals a full rule or is covered by a
// suffix rule. The input is expected in lower case.
func (ss *DomainSet) Match(domain string) bool {
	if ss == nil || len(ss.labelBitmap) == 0 {
		return false
	}
	nodeId, bmIdx := 0, 0
	for i := len(domain) - 1; i >= 0; i-- {
		c := domain[i]
		if c == '.' && ss.hasChild(nodeId, bmIdx, domainSuffixMark) {
			return true
		}
		next, ok := ss.child(nodeId, bmIdx, c)
		if !ok {
			return false
		}
		nodeId = next
		bmIdx = ss.firstLabel(nodeId)
	}
	return getBit(ss.leaves, nodeId) != 0 || ss.hasChild(nodeId, bmIdx, domainSuffixMark)
}

func (ss *DomainSet) hasChild(nodeId, bmIdx int, c byte) bool {
	_, ok := ss.child(nodeId, bmIdx, c)
	return ok
}

func (ss *DomainSet) child(nodeId, bmIdx int, c byte) (int, bool) {
	for ; ; bmIdx++ {
		if getBit(ss.labelBitmap, bmIdx) != 0 {
			return 0, false
		}
		if ss.labels[bmIdx-nodeId] == c {
			return ss.zerosBefore(bmIdx + 1), true
		}
	}
}

func (ss *DomainSet) firstLabel(nodeId int) int {
	return ss.selectOne(nodeId-1) + 1
}

func (ss *DomainSet) zerosBefore(i int) int {
	ones := int(ss.ranks[i>>6]) + bits.OnesCount64(ss.labelBitmap[i>>6]&(1<<uint(i&63)-1))
	return i - ones
}

func (ss *DomainSet) selectOne(k int) int {
	lo, hi := 0, len(ss.labelBitmap)-1
	for lo < hi {
		mid := (lo + hi + 1) >> 1
		if int(ss.ranks[mid]) <= k {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	w := ss.labelBitmap[lo]
	rem := k - int(ss.ranks[lo])
	for j := 0; j < rem; j++ {
		w &= w - 1
	}
	return lo<<6 + bits.TrailingZeros64(w)
}

func setBit(bm *[]uint64, i int, v int) {
	for i>>6 >= len(*bm) {
		*bm = append(*bm, 0)
	}
	(*bm)[i>>6] |= uint64(v) << uint(i&63)
}

func getBit(bm []uint64, i int) uint64 {
	if i>>6 >= len(bm) {
		return 0
	}
	return bm[i>>6] & (1 << uint(i&63))
}

func reverseDomain(d string) string {
	d = strings.ToLower(d)
	b := make([]byte, len(d))
	for i := 0; i < len(d); i++ {
		b[len(d)-1-i] = d[i]
	}
	return string(b)
}
