// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package graph provides a common abstraction for directed graphs and standard
// graph algorithms.
//
// In general, this package does not provide or assume any concrete graph
// representation. It's up to the caller of this package to implement the
// [Graph] interface, either directly or as an adapter around another type.
package graph

import "iter"

// A Graph implements a directed graph where nodes in the graph are identified
// by the NodeID type.

type Graph[NodeID comparable] interface {
	// Nodes yields all nodes in this graph.
	Nodes() iter.Seq[NodeID]

	// NumNodes returns the total number of nodes in this graph.
	NumNodes() int

	// Out yields the out-edges of node.
	Out(node NodeID) iter.Seq[NodeID]
}
