// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package unsafeptr defines an Analyzer that checks for invalid
// conversions of uintptr to unsafe.Pointer.
package unsafeptr

import (
	_ "embed"
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/internal/analysis/analyzerutil"
	"golang.org/x/tools/internal/typesinternal"
)

//go:embed doc.go
var doc string

var Analyzer = &analysis.Analyzer{
	Name:     "unsafeptr",
	Doc:      analyzerutil.MustExtractDoc(doc, "unsafeptr"),
	URL:      "https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/unsafeptr",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	nodeFilter := []ast.Node{
		(*ast.CallExpr)(nil),
		(*ast.StarExpr)(nil),
		(*ast.UnaryExpr)(nil),
	}
	inspect.Preorder(nodeFilter, func(n ast.Node) {
		switch x := n.(type) {
		case *ast.CallExpr:
			if len(x.Args) == 1 &&
				hasBasicType(pass.TypesInfo, x.Fun, types.UnsafePointer) &&
				hasBasicType(pass.TypesInfo, x.Args[0], types.Uintptr) &&
				!isSafeUintptr(pass.TypesInfo, x.Args[0]) {
				pass.ReportRangef(x, "possible misuse of unsafe.Pointer")
			}
		case *ast.StarExpr:
			if t := pass.TypesInfo.Types[x].Type; isReflectHeader(t) {
				pass.ReportRangef(x, "possible misuse of %s", t)
			}
		case *ast.UnaryExpr:
			if x.Op != token.AND {
				return
			}
			if t := pass.TypesInfo.Types[x.X].Type; isReflectHeader(t) {
				pass.ReportRangef(x, "possible misuse of %s", t)
			}
		}
	})
	return nil, nil
}

// isSafeUintptr reports whether x - already known to be a uintptr -
// is safe to convert to unsafe.Pointer.
func isSafeUintptr(info *types.Info, x ast.Expr) bool {
	// Check unsafe.Pointer safety rules according to
	// https://golang.org/pkg/unsafe/#Pointer.

	switch x := ast.Unparen(x).(type) {
	case *ast.SelectorExpr:
		// "(6) Conversion of a reflect.SliceHeader or
		// reflect.StringHeader Data field to or from Pointer."
		if x.Sel.Name != "Data" {
			break
		}
		// StringHeader are okay, but only if they are pointing at a real slice or string. use x ...
		pt, ok := types.Unalias(info.Types[x.X].Type).(*types.Pointer)
		if ok && isReflectHeader(pt.Elem()) {
			return true
		}

	case *ast.CallExpr:
		// "(5) Conversion of the result of reflect.Value.Pointer or
		// reflect.Value.UnsafeAddr from uintptr to Pointer."
		if len(x.Args) != 0 {
			break
		}
		sel, ok := x.Fun.(*ast.SelectorExpr)
		if !ok {
			break
		}
		switch sel.Sel.Name {
		case "Pointer", "UnsafeAddr":
			if typesinternal.IsTypeNamed(info.Types[sel.X].Type, "reflect", "Value") {
				return true
			}
		}
	}

	// "(3) Conversion of a Pointer to a uintptr and back, with arithmetic."
	return isSafeArith(info, x)
}

// isSafeArith reports whether x is a pointer arithmetic expression that is safe
// to convert to unsafe.Pointer.
func isSafeArith(info *types.Info, x ast.Expr) bool {
	switch x := ast.Unparen(x).(type) {
	case *ast.CallExpr:
		// Base case: initial conversion from unsafe.Pointer to uintptr.
		return len(x.Args) == 1 &&
			hasBasicType(info, x.Fun, types.Uintptr) &&
			hasBasicType(info, x.Args[0], types.UnsafePointer)

	case *ast.BinaryExpr:
		// "It is valid both to add and to subtract offsets from a pointer in this
		// way.
		switch x.Op {
		case token.ADD, token.SUB, token.AND_NOT:
			// TODO(mdempsky): Match compiler semantics.
			return isSafeArith(info, x.X) && !isSafeArith(info, x.Y)
		}
	}

	return false
}

// hasBasicType reports whether x's type is a types.Basic with the given kind.
func hasBasicType(info *types.Info, x ast.Expr, kind types.BasicKind) bool {
	t := info.Types[x].Type
	if t != nil {
		t = t.Underlying()
	}
	b, ok := t.(*types.Basic)
	return ok && b.Kind() == kind
}

// isReflectHeader reports whether t is reflect.SliceHeader or reflect.StringHeader.
func isReflectHeader(t types.Type) bool {
	return typesinternal.IsTypeNamed(t, "reflect", "SliceHeader", "StringHeader")
}
