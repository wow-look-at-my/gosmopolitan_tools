// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package trie

// Collision functions combine a left and right hand side.
type Collision func(lhs any, rhs any) any

// TakeLhs always returns the left value in a collision.
func TakeLhs(lhs, rhs any) any { return lhs }

// TakeRhs always returns the right hand side in a collision.
func TakeRhs(lhs, rhs any) any { return rhs }

// Builder creates new Map. Each Builder has a unique Scope.
type Builder struct {
	scope Scope

	// hash-consing maps for each node type.
	empty    *empty
	leaves   map[leaf]*leaf
	branches map[branch]*branch
}

// NewBuilder creates a new Builder with a unique Scope.
func NewBuilder() *Builder {
	s := newScope()
	return &Builder{
		scope:    s,
		empty:    &empty{s},
		leaves:   make(map[leaf]*leaf),
		branches: make(map[branch]*branch),
	}
}

func (b *Builder) Scope() Scope { return b.scope }

// Rescope changes the builder's scope to a new unique Scope.
//
// Any Maps created using the scope need to be Cloned before any operation.
func (b *Builder) Rescope() {
	s := newScope()
	b.scope = s
	b.empty = &empty{s}
	b.leaves = make(map[leaf]*leaf)
	b.branches = make(map[branch]*branch)
}

// Empty is the empty map.
func (b *Builder) Empty() Map { return Map{b.Scope(), b.empty} }

// InsertWith inserts a new association from k to v into the Map m to create a
// new map.
func (b *Builder) InsertWith(c Collision, m Map, k uint64, v any) Map {
	m = b.Clone(m)
	return Map{b.Scope(), b.insert(c, m.n, b.mkLeaf(key(k), v), false)}
}

// Inserts a new association from key to value into the Map m to create a new
// map in the current scope.
func (b *Builder) Insert(m Map, k uint64, v any) Map {
	return b.InsertWith(TakeLhs, m, k, v)
}

// Updates a (key, value) in the map. This is roughly corresponds to updating a map[uint64]interface{} by.
//
//	m[key] = val
func (b *Builder) Update(m Map, key uint64, val any) Map {
	return b.InsertWith(TakeRhs, m, key, val)
}

// Merge maps lhs and rhs to create a new map in the current scope.
func (b *Builder) MergeWith(c Collision, lhs, rhs Map) Map {
	lhs, rhs = b.Clone(lhs), b.Clone(rhs)
	return Map{b.Scope(), b.merge(c, lhs.n, rhs.n)}
}

// Merge maps lhs and rhs to create a new map in the current scope.
func (b *Builder) Merge(lhs, rhs Map) Map {
	return b.MergeWith(TakeLhs, lhs, rhs)
}

// Clone returns a Map that contains the same (key, value) elements
// within b.Scope(), i.e. return m if m.Scope() == b.Scope().
// Otherwise, return a deep copy of m within b.Scope() otherwise.
func (b *Builder) Clone(m Map) Map {
	if m.Scope() == b.Scope() {
		return m
	} else if m.n == nil {
		return Map{b.Scope(), b.empty}
	}
	return Map{b.Scope(), b.clone(m.n)}
}
func (b *Builder) clone(n node) node {
	switch n := n.(type) {
	case *empty:
		return b.empty
	case *leaf:
		return b.mkLeaf(n.k, n.v)
	case *branch:
		return b.mkBranch(n.prefix, n.branching, b.clone(n.left), b.clone(n.right))
	default:
		panic("unreachable")
	}
}

// Remove a key from a Map m and return the resulting Map.
func (b *Builder) Remove(m Map, k uint64) Map {
	m = b.Clone(m)
	return Map{b.Scope(), b.remove(m.n, key(k))}
}

// Intersect Maps lhs and rhs and returns a map with all of the keys in both
// lhs and rhs.
func (b *Builder) Intersect(lhs, rhs Map) Map {
	return b.IntersectWith(TakeLhs, lhs, rhs)
}

// IntersectWith take lhs and rhs and returns the intersection with the value
// coming from the collision function.
func (b *Builder) IntersectWith(c Collision, lhs, rhs Map) Map {
	l, r := b.Clone(lhs), b.Clone(rhs)
	return Map{b.Scope(), b.intersect(c, l.n, r.n)}
}

// MutMap is a convenient wrapper for a Map and a *Builder that will be used to create
// new Maps from it.
type MutMap struct {
	B *Builder
	M Map
}

// MutEmpty is an empty MutMap for a builder.
func (b *Builder) MutEmpty() MutMap {
	return MutMap{b, b.Empty()}
}

// Insert an element into the map using the collision function for the builder.
// Returns true if the element was inserted.
func (mm *MutMap) Insert(k uint64, v any) bool {
	old := mm.M
	mm.M = mm.B.Insert(old, k, v)
	return old != mm.M
}

// Updates an element in the map. Returns true if the map was updated.
func (mm *MutMap) Update(k uint64, v any) bool {
	old := mm.M
	mm.M = mm.B.Update(old, k, v)
	return old != mm.M
}

// Removes a key from the map. Returns true if the element was removed.
func (mm *MutMap) Remove(k uint64) bool {
	old := mm.M
	mm.M = mm.B.Remove(old, k)
	return old != mm.M
}

// Merge another map into the current one using the collision function
// for the builder. Returns true if the map changed.
func (mm *MutMap) Merge(other Map) bool {
	old := mm.M
	mm.M = mm.B.Merge(old, other)
	return old != mm.M
}

// Intersect another map into the current one using the collision function
// for the builder. Returns true if the map changed.
func (mm *MutMap) Intersect(other Map) bool {
	old := mm.M
	mm.M = mm.B.Intersect(old, other)
	return old != mm.M
}

func (b *Builder) Create(m map[uint64]any) Map {
	var leaves []*leaf
	for k, v := range m {
		leaves = append(leaves, b.mkLeaf(key(k), v))
	}
	return Map{b.Scope(), b.create(leaves)}
}

// Merge another map into the current one using the collision function
// for the builder. Returns true if the map changed.
func (mm *MutMap) MergeWith(c Collision, other Map) bool {
	old := mm.M
	mm.M = mm.B.MergeWith(c, old, other)
	return old != mm.M
}

// creates a map for a collection of leaf nodes.
func (b *Builder) create(leaves []*leaf) node {
	n := len(leaves)
	if n == 0 {
		return b.empty
	} else if n == 1 {
		return leaves[0]
	}
	// Note: we can do a more sophisticated algorithm by: - sorting the leaves ahead of time, - taking the prefix.

	m := n / 2 // (n >= 2) ==> 1 <= m < n
	l, r := leaves[:m], leaves[m:]
	return b.merge(nil, b.create(l), b.create(r))
}

// mkLeaf returns the hash-consed representative of (k, v) in the current scope.
func (b *Builder) mkLeaf(k key, v any) *leaf {
	rep, ok := b.leaves[leaf{k, v}]
	if !ok {
		rep = &leaf{k, v} // heap-allocated copy
		b.leaves[leaf{k, v}] = rep
	}
	return rep
}

// mkBranch returns the hash-consed representative of the tuple
//
//	(prefix, branch, left, right)
//
// in the current scope.
func (b *Builder) mkBranch(p prefix, bp bitpos, left node, right node) *branch {
	br := branch{
		sz:        left.size() + right.size(),
		prefix:    p,
		branching: bp,
		left:      left,
		right:     right,
	}
	rep, ok := b.branches[br]
	if !ok {
		rep = new(branch) // heap-allocated copy
		*rep = br
		b.branches[br] = rep
	}
	return rep
}

// join maps with prefixes p0 and p1 that are *known* to disagree.
func (b *Builder) join(p0 prefix, t0 node, p1 prefix, t1 node) *branch {
	m := branchingBit(p0, p1)
	var left, right node
	if zeroBit(p0, m) {
		left, right = t0, t1
	} else {
		left, right = t1, t0
	}
	prefix := mask(p0, m)
	return b.mkBranch(prefix, m, left, right)
}

// collide leaves with the same key to create a leaf with the collided value.
func (b *Builder) collide(c Collision, left, right *leaf) *leaf {
	if left == right {
		return left // c is idempotent: c(x, x) == x
	}
	val := left.v // keep the left value by default if c is nil
	if c != nil {
		val = c(left.v, right.v)
	}
	switch val {
	case left.v:
		return left
	case right.v:
		return right
	default:
		return b.mkLeaf(left.k, val)
	}
}

// inserts a leaf l into a map m and returns the resulting map.
// When lhs is true, l is the left hand side in a collision.
// Both l and m are in the current scope.
func (b *Builder) insert(c Collision, m node, l *leaf, lhs bool) node {
	switch m := m.(type) {
	case *empty:
		return l
	case *leaf:
		if m.k == l.k {
			left, right := l, m
			if !lhs {
				left, right = right, left
			}
			return b.collide(c, left, right)
		}
		return b.join(prefix(l.k), l, prefix(m.k), m)
	case *branch:
	}
	// m is a branch
	br := m.(*branch)
	if !matchPrefix(prefix(l.k), br.prefix, br.branching) {
		return b.join(prefix(l.k), l, br.prefix, br)
	}
	var left, right node
	if zeroBit(prefix(l.k), br.branching) {
		left, right = b.insert(c, br.left, l, lhs), br.right
	} else {
		left, right = br.left, b.insert(c, br.right, l, lhs)
	}
	if left == br.left && right == br.right {
		return m
	}
	return b.mkBranch(br.prefix, br.branching, left, right)
}

// merge maps in the current scope.
func (b *Builder) merge(c Collision, lhs, rhs node) node {
	if lhs == rhs {
		return lhs
	}
	switch lhs := lhs.(type) {
	case *empty:
		return rhs
	case *leaf:
		return b.insert(c, rhs, lhs, true)
	case *branch:
		switch rhs := rhs.(type) {
		case *empty:
			return lhs
		case *leaf:
			return b.insert(c, lhs, rhs, false)
		case *branch:
		}
	}

	// Last remaining case is branch merging.
	s, t := lhs.(*branch), rhs.(*branch)
	p, m := s.prefix, s.branching
	q, n := t.prefix, t.branching

	if m == n && p == q { // prefixes are identical.
		left, right := b.merge(c, s.left, t.left), b.merge(c, s.right, t.right)
		return b.mkBranch(p, m, left, right)
	}
	if !prefixesOverlap(p, m, q, n) {
		return b.join(p, s, q, t) // prefixes are disjoint.
	}
	// prefixesOverlap(p, m, q, n) && !(m ==n && p == q) By prefixesOverlap(...),
	// either: higher(m, n) && matchPrefix(q, p, m), or higher(n, m) &&
	// matchPrefix(p, q, n). So either s or t may can be merged with one branch
	// or the other.
	switch {
	case ord(m, n) && zeroBit(q, m):
		return b.mkBranch(p, m, b.merge(c, s.left, t), s.right)
	case ord(m, n) && !zeroBit(q, m):
		return b.mkBranch(p, m, s.left, b.merge(c, s.right, t))
	case ord(n, m) && zeroBit(p, n):
		return b.mkBranch(q, n, b.merge(c, s, t.left), t.right)
	default:
		return b.mkBranch(q, n, t.left, b.merge(c, s, t.right))
	}
}

func (b *Builder) remove(m node, k key) node {
	switch m := m.(type) {
	case *empty:
		return m
	case *leaf:
		if m.k == k {
			return b.empty
		}
		return m
	case *branch:
	}
	br := m.(*branch)
	kp := prefix(k)
	if !matchPrefix(kp, br.prefix, br.branching) {
		// The prefix does not match. kp is not in br.
		return br
	}
	// the prefix matches. try to remove from the left or right branch.
	left, right := br.left, br.right
	if zeroBit(kp, br.branching) {
		left = b.remove(left, k) // k may be in the left branch.
	} else {
		right = b.remove(right, k) // k may be in the right branch.
	}
	if left == br.left && right == br.right {
		return br // no update
	} else if _, ok := left.(*empty); ok {
		return right // left updated and is empty.
	} else if _, ok := right.(*empty); ok {
		return left // right updated and is empty.
	}
	// Either left or right updated. Both left and right are not empty.
	return b.mkBranch(br.prefix, br.branching, left, right)
}

func (b *Builder) intersect(c Collision, l, r node) node {
	if l == r {
		return l
	}
	switch l := l.(type) {
	case *empty:
		return b.empty
	case *leaf:
		if rleaf := r.find(l.k); rleaf != nil {
			return b.collide(c, l, rleaf)
		}
		return b.empty
	case *branch:
		switch r := r.(type) {
		case *empty:
			return b.empty
		case *leaf:
			if lleaf := l.find(r.k); lleaf != nil {
				return b.collide(c, lleaf, r)
			}
			return b.empty
		case *branch:
		}
	}
	// Last remaining case is branch intersection.
	s, t := l.(*branch), r.(*branch)
	p, m := s.prefix, s.branching
	q, n := t.prefix, t.branching

	if m == n && p == q {
		// prefixes are identical.
		left, right := b.intersect(c, s.left, t.left), b.intersect(c, s.right, t.right)
		if _, ok := left.(*empty); ok {
			return right
		} else if _, ok := right.(*empty); ok {
			return left
		}
		// The left and right branches are both non-empty.
		return b.mkBranch(p, m, left, right)
	}

	if !prefixesOverlap(p, m, q, n) {
		return b.empty // The prefixes share no keys.
	}
	// prefixesOverlap(p, m, q, n) && !(m ==n && p == q) By prefixesOverlap(...), either.
	var lhs, rhs node
	switch {
	case ord(m, n) && zeroBit(q, m):
		lhs, rhs = s.left, t
	case ord(m, n) && !zeroBit(q, m):
		lhs, rhs = s.right, t
	case ord(n, m) && zeroBit(p, n):
		lhs, rhs = s, t.left
	default:
		lhs, rhs = s, t.right
	}
	return b.intersect(c, lhs, rhs)
}
