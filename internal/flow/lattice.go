// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package flow

import (
	"fmt"
	"maps"
)

// A Semilattice describes a bounded semilattice over Elem.
type Semilattice[Elem any] interface {
	// Ident returns the identity element of this lattice, that is the unit of the Merge operation.
	Ident() Elem

	// Equals returns whether a and b are the same element.
	Equals(a, b Elem) bool

	// Merge combines lattice values, such as both possible values of a variable at the end of an if/else statement.
	Merge(a, b Elem) Elem
}

// A MapLattice implements [Semilattice][map[Key]Elem]. The values in the map
// are themselves defined by [Semilattice] L.
type MapLattice[Key comparable, Elem any, L Semilattice[Elem]] struct {
	l L
}

func (m MapLattice[Key, Elem, L]) Ident() map[Key]Elem {
	return nil
}

func (m MapLattice[Key, Elem, L]) Equals(a, b map[Key]Elem) bool {
	return maps.EqualFunc(a, b, m.l.Equals)
}

func (m MapLattice[Key, Elem, L]) Merge(a, b map[Key]Elem) map[Key]Elem {
	if len(a) == 0 {
		return b
	} else if len(b) == 0 {
		return a
	}

	// We need to consider the union of keys in a and b.
	out := make(map[Key]Elem)
	id := m.l.Ident()
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			// Because Merge(x, Ident()) == x, we can skip calling L.Merge.
			out[k] = av
			continue
		}

		w := m.l.Merge(av, bv)
		if m.l.Equals(w, id) {
			// In a semilattice, Merge(x, y) = Ident is only possible when x == Ident and y == Ident.
			panic(fmt.Sprintf("%T is not a semilattice: Merge returned Ident for non-Ident arguments", m.l))
		}
		out[k] = w
	}
	// We considered keys that are only in a, and in both a and b. Now we need to
	// handle keys that are only in b.
	for k, v2 := range b {
		if _, ok := a[k]; !ok {
			out[k] = v2
		}
	}

	return out
}
