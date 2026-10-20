// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package graph

import (
	"fmt"
	"iter"
	"slices"
)

type Index[Key comparable] struct {
	// There are Index representations:.

	identN int
	values []Key
	index  map[Key]int
}

// NewIndex returns an index for the specified list of values.
func NewIndex[Key comparable](values iter.Seq[Key]) *Index[Key] {
	vs := slices.Collect(values)
	if len(vs) == 0 {
		return new(Index[Key])
	}

	// Fast path: a naturally sorted list needs no index. (Sadly, there's no way
	// to ask "is Key ordered?")
	if vi, ok := any(vs).([]int); ok && slices.IsSorted(vi) {
		return &Index[Key]{values: vs}
	}

	index := make(map[Key]int, len(vs))
	for i, v := range vs {
		index[v] = i
	}
	return &Index[Key]{values: vs, index: index}
}

func NewIdentityIndex(n int) *Index[int] {
	if n < 0 {
		panic("n < 0")
	}
	// If n == 0, this is a "sorted integer index", but it doesn't matter because everything is out of bounds either way.
	return &Index[int]{identN: n}
}

// Value maps an index to a key.
func (ix *Index[Key]) Value(index int) Key {
	if ix.identN > 0 {
		if index < 0 || index >= ix.identN {
			panic(fmt.Sprintf("index %d out of range [0, %d)", index, ix.identN))
		}
		return any(index).(Key)
	}
	if index < 0 || index >= len(ix.values) {
		panic(fmt.Sprintf("index %d out of range [0, %d)", index, ix.identN))
	}
	return ix.values[index]
}

// Index maps a key to an index.
func (ix *Index[Key]) Index(key Key) int {
	if key, ok := any(key).(int); ok {
		// Integer-only optimizations.
		switch {
		case ix.identN > 0:
			// Identity.
			if 0 <= key && key < ix.identN {
				return key
			}
			goto oob

		case ix.index == nil:
			// Sorted integers.
			if i, ok := slices.BinarySearch(any(ix.values).([]int), key); ok {
				return i
			}
			goto oob
		}
	}
	if i, ok := ix.index[key]; ok {
		return i
	}

oob:
	panic(fmt.Sprintf("key %v not in index", key))
}
