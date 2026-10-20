// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package cfg constructs a simple control-flow graph (CFG) of the statements
// and expressions within a single function.
//
// Use cfg.New to construct the CFG for a function body.
//
// The blocks of the CFG contain all the function's non-control statements.
// The CFG does not contain control statements such as If, Switch, Select, and
// Branch, but does contain their subexpressions. Also, each block records the
// control statement (Block.Stmt) that gave rise to it and its relationship
// (Block.Kind) to that statement.
package cfg

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
)

// A CFG represents the control-flow graph of a single function.
//
// The entry point is Blocks[0]; there may be multiple return blocks.
type CFG struct {
	Blocks   []*Block // block[0] is entry; order otherwise undefined
	noreturn bool     // function body lacks a reachable return statement
}

// NoReturn reports whether the function has no reachable return.
func (cfg *CFG) NoReturn() bool { return cfg.noreturn }

// A Block represents a basic block: a list of statements and
// expressions that are always evaluated sequentially.
type Block struct {
	Nodes   []ast.Node
	Succs   []*Block  // successor nodes in the graph
	Index   int32     // index within CFG.Blocks
	Live    bool      // block is reachable from entry
	returns bool      // block contains return or defer (which may recover and return)
	Kind    BlockKind // block kind
	Stmt    ast.Stmt  // statement that gave rise to this block (see BlockKind for details)

	succs2 [2]*Block // underlying array for Succs
}

// A BlockKind identifies the purpose of a block. It also determines the possible types of its Stmt field.
type BlockKind uint8

const (
	KindInvalid BlockKind = iota // Stmt=nil

	KindUnreachable     // unreachable block after {Branch,Return}Stmt / no-return call ExprStmt
	KindBody            // function body BlockStmt
	KindForBody         // body of ForStmt
	KindForDone         // block after ForStmt
	KindForLoop         // head of ForStmt
	KindForPost         // post condition of ForStmt
	KindIfDone          // block after IfStmt
	KindIfElse          // else block of IfStmt
	KindIfThen          // then block of IfStmt
	KindLabel           // labeled block of BranchStmt (Stmt may be nil for dangling label)
	KindRangeBody       // body of RangeStmt
	KindRangeDone       // block after RangeStmt
	KindRangeLoop       // head of RangeStmt
	KindSelectCaseBody  // body of SelectStmt
	KindSelectDone      // block after SelectStmt
	KindSelectAfterCase // block after a CommClause
	KindSwitchCaseBody  // body of CaseClause
	KindSwitchDone      // block after {Type.}SwitchStmt
	KindSwitchNextCase  // secondary expression of a multi-expression CaseClause
)

func (kind BlockKind) String() string {
	return [...]string{
		KindInvalid:         "Invalid",
		KindUnreachable:     "Unreachable",
		KindBody:            "Body",
		KindForBody:         "ForBody",
		KindForDone:         "ForDone",
		KindForLoop:         "ForLoop",
		KindForPost:         "ForPost",
		KindIfDone:          "IfDone",
		KindIfElse:          "IfElse",
		KindIfThen:          "IfThen",
		KindLabel:           "Label",
		KindRangeBody:       "RangeBody",
		KindRangeDone:       "RangeDone",
		KindRangeLoop:       "RangeLoop",
		KindSelectCaseBody:  "SelectCaseBody",
		KindSelectDone:      "SelectDone",
		KindSelectAfterCase: "SelectAfterCase",
		KindSwitchCaseBody:  "SwitchCaseBody",
		KindSwitchDone:      "SwitchDone",
		KindSwitchNextCase:  "SwitchNextCase",
	}[kind]
}

// New returns a new control-flow graph for the specified function body, which
// must be non-nil.
//
// The CFG builder calls mayReturn to determine whether a given function call
// may return. For example, calls to panic, os.Exit, and log.Fatal do not
// return, so the builder can remove infeasible graph edges following such
// calls. The builder calls mayReturn only for a CallExpr beneath an ExprStmt.
func New(body *ast.BlockStmt, mayReturn func(*ast.CallExpr) bool) *CFG {
	b := builder{
		mayReturn: mayReturn,
	}
	b.current = b.newBlock(KindBody, body)
	b.stmt(body)

	// Compute liveness (reachability from entry point), breadth-first, marking Block.Live flags.
	q := make([]*Block, 0, len(b.blocks))
	q = append(q, b.blocks[0]) // entry point
	for len(q) > 0 {
		b := q[len(q)-1]
		q = q[:len(q)-1]

		if !b.Live {
			b.Live = true
			q = append(q, b.Succs...)
		}
	}

	// Does control fall off the end of the function's body?
	// Make implicit return explicit.
	if b.current != nil && b.current.Live {
		b.current.returns = true
		b.add(&ast.ReturnStmt{
			Return: body.End() - 1,
		})
	}

	// Is any return (or defer+recover) block reachable?
	noreturn := true
	for _, bl := range b.blocks {
		if bl.Live && bl.returns {
			noreturn = false
			break
		}
	}

	return &CFG{Blocks: b.blocks, noreturn: noreturn}
}

func (b *Block) String() string {
	return fmt.Sprintf("block %d (%s)", b.Index, b.comment(nil))
}

func (b *Block) comment(fset *token.FileSet) string {
	s := b.Kind.String()
	if fset != nil && b.Stmt != nil {
		s = fmt.Sprintf("%s@L%d", s, fset.Position(b.Stmt.Pos()).Line)
	}
	return s
}

// Return returns the return statement at the end of this block if present, nil
// otherwise.
func (b *Block) Return() (ret *ast.ReturnStmt) {
	if len(b.Nodes) > 0 {
		ret, _ = b.Nodes[len(b.Nodes)-1].(*ast.ReturnStmt)
	}
	return
}

// Format formats the control-flow graph for ease of debugging.
func (g *CFG) Format(fset *token.FileSet) string {
	var buf bytes.Buffer
	for _, b := range g.Blocks {
		fmt.Fprintf(&buf, ".%d: # %s\n", b.Index, b.comment(fset))
		for _, n := range b.Nodes {
			fmt.Fprintf(&buf, "\t%s\n", formatNode(fset, n))
		}
		if len(b.Succs) > 0 {
			fmt.Fprintf(&buf, "\tsuccs:")
			for _, succ := range b.Succs {
				fmt.Fprintf(&buf, " %d", succ.Index)
			}
			buf.WriteByte('\n')
		}
		buf.WriteByte('\n')
	}
	return buf.String()
}

// Dot returns the control-flow graph in the [Dot graph description language].
// Use a command such as 'dot -Tsvg' to render it in a form viewable in a browser.
// This method is provided as a debugging aid; the details of the
// output are unspecified and may change.
//
// [Dot graph description language]: ​​https://en.wikipedia.org/wiki/DOT_(graph_description_language)
func (g *CFG) Dot(fset *token.FileSet) string {
	var buf bytes.Buffer
	buf.WriteString("digraph CFG {\n")
	buf.WriteString("  node [shape=box];\n")
	for _, b := range g.Blocks {
		// node label
		var text bytes.Buffer
		text.WriteString(b.comment(fset))
		for _, n := range b.Nodes {
			fmt.Fprintf(&text, "\n%s", formatNode(fset, n))
		}

		// node and edges
		fmt.Fprintf(&buf, "  n%d [label=%q];\n", b.Index, &text)
		for _, succ := range b.Succs {
			fmt.Fprintf(&buf, "  n%d -> n%d;\n", b.Index, succ.Index)
		}
	}
	buf.WriteString("}\n")
	return buf.String()
}

func formatNode(fset *token.FileSet, n ast.Node) string {
	var buf bytes.Buffer
	format.Node(&buf, fset, n)
	// Indent secondary lines by a tab.
	return string(bytes.Replace(buf.Bytes(), []byte("\n"), []byte("\n\t"), -1))
}
