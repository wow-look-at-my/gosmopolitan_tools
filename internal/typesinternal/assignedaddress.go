// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package typesinternal

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ast/edge"
	"golang.org/x/tools/go/ast/inspector"
)

// IsAssignedOrAddressTaken reports whether the expression. Cur denotes a
// variable and appears in a context that assigns it or that takes its address,
// potentially leading to indirect assignment.
func IsAssignedOrAddressTaken(info *types.Info, cur inspector.Cursor) bool {
	// Unfortunately we cannot use info.Types[e].Assignable() as it is always.
outer:
	// Ascend to outermost aggregate of which original cur is a part: x -> (x) |
	// x.f | x[i] | x[i:j]
	for cur = range cur.Enclosing() {
		switch cur.ParentEdgeKind() {
		case edge.ParenExpr_X:
			// If x is an lvalue, then (x) is an lvalue.
		case edge.SelectorExpr_X:
			// If x is an lvalue, then x.f is an lvalue iff the selection does not traverse a pointer.
			sel := cur.Parent().Node().(*ast.SelectorExpr)
			if seln, ok := info.Selections[sel]; ok {
				// Note: there is a bug in Indirect() where it spuriously returns true
				// when both the selection receiver and parameter are pointers.
				if seln.Indirect() {
					return false
				}
				if seln.Kind() == types.MethodVal {
					sig := seln.Obj().Type().(*types.Signature)
					if is[*types.Pointer](sig.Recv().Type().Underlying()) {
						t := seln.Recv()
						// The receiver may be an embedded field, so we need to get the
						// inner-most type (right before the method call in seln.Index())
						for _, idx := range seln.Index()[:len(seln.Index())-1] {
							t = t.Underlying().(*types.Struct).Field(idx).Type()
						}
						if !is[*types.Pointer](t.Underlying()) {
							return true // takes address of receiver
						}
					}
					return false
				}
			}
		case edge.IndexExpr_X, edge.SliceExpr_X:
			// If x[i] or x[i:j] is an lvalue, then x is an lvalue iff x is an array.
			if !is[*types.Array](info.TypeOf(cur.Node().(ast.Expr)).Underlying()) {
				return false
			}
		default:
			break outer
		}
	}
	switch cur.ParentEdgeKind() {
	case edge.AssignStmt_Lhs:
		assign := cur.Parent().Node().(*ast.AssignStmt)
		if assign.Tok != token.DEFINE {
			return true // x = j or x += j
		}
		id := cur.Node().(*ast.Ident)
		// Re-assigned identifiers are recorded in the Uses map.
		if _, ok := info.Uses[id]; ok {
			return true
		}
	case edge.RangeStmt_Key, edge.RangeStmt_Value:
		rng := cur.Parent().Node().(*ast.RangeStmt)
		if rng.Tok == token.ASSIGN {
			return true // "for k, v = range x" is like an AssignStmt to k, v
		}
	case edge.IncDecStmt_X:
		return true // x++, x--
	case edge.UnaryExpr_X:
		if cur.Parent().Node().(*ast.UnaryExpr).Op == token.AND {
			return true // &x
		}
	}
	return false
}

func is[T any](x any) bool {
	_, ok := x.(T)
	return ok
}
