// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

/*
Package callgraph defines the call graph and the algorithms and utilities
that operate on it. A call graph is a labelled directed graph whose nodes are
functions and whose edge labels are syntactic call sites. An edge (caller,
site, callee) means caller may call callee there. It is a multigraph: the
same pair of nodes may hold several edges that differ by label. A sound call
graph overapproximates the dynamic behavior of the program in every possible
execution. One call graph is more precise than another when it is a smaller
overapproximation. All call graphs have a synthetic root node that calls main
and init. Calls to built-in functions are treated like built-in operators and
are not represented.
*/
package callgraph // import "golang.org/x/tools/go/callgraph"

// TODO(zpavlinovic): decide how callgraphs handle calls to and from generic function bodies.

import (
	"fmt"
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// A Graph represents a call graph.
type Graph struct {
	Root  *Node                   // the distinguished root node (Root.Func may be nil)
	Nodes map[*ssa.Function]*Node // all nodes by function
}

// New returns a new Graph with the specified (optional) root node.
func New(root *ssa.Function) *Graph {
	g := &Graph{Nodes: make(map[*ssa.Function]*Node)}
	g.Root = g.CreateNode(root)
	return g
}

// CreateNode returns the Node for fn, creating it if not present.
// The root node may have fn=nil.
func (g *Graph) CreateNode(fn *ssa.Function) *Node {
	n, ok := g.Nodes[fn]
	if !ok {
		n = &Node{Func: fn, ID: len(g.Nodes)}
		g.Nodes[fn] = n
	}
	return n
}

// A Node represents a node in a call graph.
type Node struct {
	Func *ssa.Function // the function this node represents
	ID   int           // 0-based sequence number
	In   []*Edge       // unordered set of incoming call edges (n.In[*].Callee == n)
	Out  []*Edge       // unordered set of outgoing call edges (n.Out[*].Caller == n)
}

func (n *Node) String() string {
	return fmt.Sprintf("n%d:%s", n.ID, n.Func)
}

// A Edge represents an edge in the call graph.
type Edge struct {
	Caller *Node
	Site   ssa.CallInstruction
	Callee *Node
}

func (e Edge) String() string {
	return fmt.Sprintf("%s --> %s", e.Caller, e.Callee)
}

func (e Edge) Description() string {
	var prefix string
	switch e.Site.(type) {
	case nil:
		return "synthetic call"
	case *ssa.Go:
		prefix = "concurrent "
	case *ssa.Defer:
		prefix = "deferred "
	}
	return prefix + e.Site.Common().Description()
}

func (e Edge) Pos() token.Pos {
	if e.Site == nil {
		return token.NoPos
	}
	return e.Site.Pos()
}

// AddEdge adds the edge (caller, site, callee) to the call graph.
// Elimination of duplicate edges is the caller's responsibility.
func AddEdge(caller *Node, site ssa.CallInstruction, callee *Node) {
	e := &Edge{caller, site, callee}
	callee.In = append(callee.In, e)
	caller.Out = append(caller.Out, e)
}
