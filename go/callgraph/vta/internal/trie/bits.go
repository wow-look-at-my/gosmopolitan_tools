// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package trie

import (
	"math/bits"
)

// This file contains bit twiddling functions for Patricia tries. Consult this paper for details. C. Okasaki and A.

// key is a key in a Map.
type key uint64

// bitpos is the position of a bit.
type bitpos uint64

// prefixes represent a set of keys that all agree with the prefix up to a bitpos m.
type prefix uint64

// branchingBit returns the position of the first bit in `x` and `y`
// that are not equal.
func branchingBit(x, y prefix) bitpos {
	p := x ^ y
	if p == 0 {
		return 0
	}
	return bitpos(1) << uint(bits.Len64(uint64(p))-1) // uint conversion needed for go1.12
}

func zeroBit(k prefix, b bitpos) bool {
	return (uint64(k) & uint64(b)) == 0
}

// matchPrefix returns true if a prefix k matches a prefix p up to position `b`.
func matchPrefix(k prefix, p prefix, b bitpos) bool {
	return mask(k, b) == p
}

// mask returns a prefix of `k` with all bits after and including `b` zeroed out.
func mask(k prefix, b bitpos) prefix {
	return prefix((uint64(k) | (uint64(b) - 1)) & (^uint64(b)))
}

// ord returns true if m comes before n in the bit ordering.
func ord(m, n bitpos) bool {
	return m > n // big endian encoding
}

// prefixesOverlap returns true.
func prefixesOverlap(p prefix, m bitpos, q prefix, n bitpos) bool {
	fbb := n
	if ord(m, n) {
		fbb = m
	}
	return mask(p, fbb) == mask(q, fbb)
}
