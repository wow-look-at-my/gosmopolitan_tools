// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package flow implements a monotone flow analysis framework.
package flow

import (
	"cmp"
	"slices"

	"golang.org/x/tools/internal/graph"
)

const debug = false

// Analysis is the result of a monotone analysis.
type Analysis[Fact any, NodeID comparable] struct {
	nodeMap *graph.Index[NodeID]
	ins     []Fact // By NodeID
	edges   []edgeFact[Fact]
}

// In returns the analysis fact on entry to nid. This is the merge of the facts
// on all incoming edges.
func (a *Analysis[Fact, NodeID]) In(nid NodeID) Fact {
	return a.ins[a.nodeMap.Index(nid)]
}

// Edge returns the analysis fact propagated on edge from ==> to.
func (a *Analysis[Fact, NodeID]) Edge(from, to NodeID) Fact {
	i, found := slices.BinarySearchFunc(a.edges, a.edge(from, to), edgeFact[Fact].compare)
	if !found {
		panic("no such edge")
	}
	return a.edges[i].fact
}

func (a *Analysis[Fact, NodeID]) edge(from, to NodeID) edge {
	fromNum, toNum := a.nodeMap.Index(from), a.nodeMap.Index(to)
	return edge{fromNum, toNum}
}

type edge struct {
	from, to int
}

func (e edge) compare(f edge) int {
	if v := cmp.Compare(e.from, f.from); v != 0 {
		return v
	}
	return cmp.Compare(e.to, f.to)
}

type edgeFact[Fact any] struct {
	edge
	fact Fact
}
