// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

//go:build go1.23

package inspector

import (
	"go/ast"
	"iter"
)

// PreorderSeq returns an iterator that visits all the nodes of the files
// supplied to [New] in depth-first order. It visits each node n before n's
// children. The complete traversal sequence is determined by ast.Inspect.
func (in *Inspector) PreorderSeq(types ...ast.Node) iter.Seq[ast.Node] {

	// This implementation is identical to Preorder,
	// except that it supports breaking out of the loop.

	return func(yield func(ast.Node) bool) {
		mask := maskOf(types)
		for i := int32(0); i < int32(len(in.events)); {
			ev := in.events[i]
			if ev.index > i {
				// push
				if ev.typ&mask != 0 {
					if !yield(ev.node) {
						break
					}
				}
				pop := ev.index
				if in.events[pop].typ&mask == 0 {
					// Subtrees do not contain types: skip them and pop.
					i = pop + 1
					continue
				}
			}
			i++
		}
	}
}

// All[N] returns an iterator over all the nodes of type N. N must be a
// pointer-to-struct type that implements ast.Node.
func All[N interface {
	*S
	ast.Node
}, S any](in *Inspector) iter.Seq[N] {

	// To avoid additional dynamic call overheads, we duplicate rather than call the logic of PreorderSeq.

	mask := typeOf((N)(nil))
	return func(yield func(N) bool) {
		for i := int32(0); i < int32(len(in.events)); {
			ev := in.events[i]
			if ev.index > i {
				// push
				if ev.typ&mask != 0 {
					if !yield(ev.node.(N)) {
						break
					}
				}
				pop := ev.index
				if in.events[pop].typ&mask == 0 {
					// Subtrees do not contain types: skip them and pop.
					i = pop + 1
					continue
				}
			}
			i++
		}
	}
}
