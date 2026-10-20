// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package flow_test

import (
	"iter"
	"math/rand"
	"slices"
	"testing"
	"time"

	"golang.org/x/tools/internal/flow"
)

type simpleGraph struct {
	numNodes int
	edges    [][]int
}

func (g *simpleGraph) NumNodes() int {
	return g.numNodes
}

func (g *simpleGraph) Nodes() iter.Seq[int] {
	return func(yield func(int) bool) {
		for i := range g.numNodes {
			if !yield(i) {
				break
			}
		}
	}
}

func (g *simpleGraph) Out(nid int) iter.Seq[int] {
	return slices.Values(g.edges[nid])
}

func checkAnalysis[Fact comparable](t *testing.T, analysis *flow.Analysis[Fact, int], wantIns map[int]Fact, wantEdges map[[2]int]Fact) {
	t.Helper()
	for nid, want := range wantIns {
		if got := analysis.In(nid); got != want {
			t.Errorf("In(%d): got %v, want %v", nid, got, want)
		}
	}

	for edge, want := range wantEdges {
		from, to := edge[0], edge[1]
		if got := analysis.Edge(from, to); got != want {
			t.Errorf("Edge(%d, %d): got %v, want %v", from, to, got, want)
		}
	}
}

func TestBasic(t *testing.T) {
	g := &simpleGraph{
		numNodes: 4,
		edges: [][]int{
			0: {1},
			1: {2},
			2: {3},
			3: {},
		},
	}

	// Transfer function: OR source node's ID into the bitset.
	transfer := func(from, to int, fact nodeSet) nodeSet {
		return fact | (1 << uint(from))
	}

	analysis := flow.Forward[nodeSetUnion](g, nil, transfer)

	checkAnalysis(t, analysis,
		map[int]nodeSet{
			0: 0,
			1: set(0),
			2: set(0, 1),
			3: set(0, 1, 2),
		},
		map[[2]int]nodeSet{
			{0, 1}: set(0),
			{1, 2}: set(0, 1),
			{2, 3}: set(0, 1, 2),
		},
	)
}

func TestDiamond(t *testing.T) {
	g := &simpleGraph{
		numNodes: 4,
		edges: [][]int{
			0: {1, 2},
			1: {3},
			2: {3},
			3: {},
		},
	}

	transfer := func(from, to int, fact nodeSet) nodeSet {
		return fact | (1 << uint(from))
	}

	analysis := flow.Forward[nodeSetUnion](g, nil, transfer)

	checkAnalysis(t, analysis,
		map[int]nodeSet{
			0: 0,
			1: set(0),
			2: set(0),
			3: set(0, 1, 2),
		},
		map[[2]int]nodeSet{
			{0, 1}: set(0),
			{0, 2}: set(0),
			{1, 3}: set(0, 1),
			{2, 3}: set(0, 2),
		},
	)
}

func TestCycle(t *testing.T) {
	g := &simpleGraph{
		numNodes: 3,
		edges: [][]int{
			0: {1},
			1: {2},
			2: {1},
		},
	}

	transfer := func(from, to int, fact nodeSet) nodeSet {
		return fact | (1 << uint(from))
	}

	analysis := flow.Forward[nodeSetUnion](g, nil, transfer)

	checkAnalysis(t, analysis,
		map[int]nodeSet{
			0: 0,
			1: set(0, 1, 2),
			2: set(0, 1, 2),
		},
		map[[2]int]nodeSet{
			{0, 1}: set(0),
			{1, 2}: set(0, 1, 2),
			{2, 1}: set(0, 1, 2),
		},
	)
}

func TestFlowOrder(t *testing.T) {
	// This test constructs a "Spine with Bottleneck" graph in which a naive visit order would result in quadratic time.

	T := 64 // Max ID allowed by nodeSet
	N := 2 * T

	// Permute IDs to prevent "lucky" ordering.
	perm := rand.New(rand.NewSource(42)).Perm(N)
	invPerm := make([]int, N)
	for i, p := range perm {
		invPerm[p] = i
	}
	// node maps a logical node ID to a permuted NodeID.
	node := func(logical int) int {
		return int(perm[logical])
	}

	// Build the edges.
	edges := make([][]int, N)
	nEdges := 0
	addEdge := func(u, v int) {
		uID := node(u)
		edges[uID] = append(edges[uID], node(v))
		nEdges++
	}
	for i := range T {
		if i+1 != T {
			addEdge(i, i+1)
		}
		addEdge(i, T)
	}
	// Tail: T -> T+1 -> ... -> N-1
	for i := T; i < N-2; i++ {
		addEdge(i, i+1)
	}

	g := &simpleGraph{
		numNodes: N,
		edges:    edges,
	}

	transfers := 0
	// Transfer: if pred is on the spine (< T), set its bit.
	transfer := func(from, to int, in nodeSet) nodeSet {
		transfers++
		logicalPred := invPerm[from]
		if logicalPred < T {
			return in | (1 << uint64(logicalPred))
		}
		return in
	}

	// Measure flow.Forward (RPO)
	start := time.Now()
	_ = flow.Forward[nodeSetUnion](g, nil, transfer)
	durRPO := time.Since(start)
	t.Logf("RPO time: %v", durRPO)

	// RPO should result in optimal evaluation, visiting each edge exactly once.
	if transfers != nEdges {
		t.Errorf("got %d transfer calls, expected %d for optimal evaluation strategy", transfers, nEdges)
	}
}
