// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package modernize

// This file defines modernizers that use the "reflect" package.

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/edge"
	"golang.org/x/tools/go/types/typeutil"
	"golang.org/x/tools/internal/analysis/analyzerutil"
	typeindexanalyzer "golang.org/x/tools/internal/analysis/typeindex"
	"golang.org/x/tools/internal/astutil"
	"golang.org/x/tools/internal/typesinternal"
	"golang.org/x/tools/internal/typesinternal/typeindex"
	"golang.org/x/tools/internal/versions"
)

var ReflectTypeForAnalyzer = &analysis.Analyzer{
	Name: "reflecttypefor",
	Doc:  analyzerutil.MustExtractDoc(doc, "reflecttypefor"),
	Requires: []*analysis.Analyzer{
		inspect.Analyzer,
		typeindexanalyzer.Analyzer,
	},
	Run: reflecttypefor,
	URL: "https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/modernize#hdr-Analyzer_reflecttypefor",
}

func reflecttypefor(pass *analysis.Pass) (any, error) {
	var (
		index = pass.ResultOf[typeindexanalyzer.Analyzer].(*typeindex.Index)
		info  = pass.TypesInfo

		reflectTypeOf = index.Object("reflect", "TypeOf")
	)

	for curCall := range index.Calls(reflectTypeOf) {
		call := curCall.Node().(*ast.CallExpr)
		// Have: reflect.TypeOf(expr)

		expr := call.Args[0]

		// reflect.TypeFor cannot be instantiated with an untyped nil.
		if info.Types[expr].IsNil() {
			continue
		}

		if !typesinternal.NoEffects(info, expr) {
			continue // don't eliminate operand: may have effects
		}

		t := info.TypeOf(expr)
		var edits []analysis.TextEdit

		// Special cases for TypeOf((*T)(nil)).Elem(), and
		// TypeOf([]T(nil)).Elem(), needed when T is an interface type.
		if curCall.ParentEdgeKind() == edge.SelectorExpr_X {
			curSel := astutil.UnparenEnclosingCursor(curCall).Parent()
			if curSel.ParentEdgeKind() == edge.CallExpr_Fun {
				call2 := astutil.UnparenEnclosingCursor(curSel).Parent().Node().(*ast.CallExpr) // potentially .Elem()
				obj := typeutil.Callee(info, call2)
				if typesinternal.IsMethodNamed(obj, "reflect", "Type", "Elem") {
					// reflect.TypeOf(expr).Elem() ------- reflect.TypeOf(expr)
					if typ, hasElem := t.(interface{ Elem() types.Type }); hasElem {
						// Have: TypeOf(expr).Elem() where expr is *T, []T, [k]T, chan T, map[K]T, etc.
						t = typ.Elem()
						edits = []analysis.TextEdit{{
							Pos: call.End(),
							End: call2.End(),
						}}
					}
				}
			}
		}

		// TypeOf(x) where x has an interface type is a dynamic operation; don't
		// transform it to TypeFor.
		if types.IsInterface(t) && edits == nil {
			continue
		}

		// Don't offer the fix if it would erase a reference to a (non-type) symbol,
		// as this may break intended coupling.
		if usesNonTypeSymbol(info, expr) {
			continue
		}

		file := astutil.EnclosingFile(curCall)
		if !analyzerutil.FileUsesGoVersion(pass, file, versions.Go1_22) {
			continue // TypeFor requires go1.22
		}

		// Format the type as valid Go syntax.
		qual := typesinternal.FileQualifier(file, pass.Pkg)
		tstr := types.TypeString(t, qual)

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue // e.g. reflect was dot-imported
		}

		// Don't offer a fix if the type contains an unnamed struct or unnamed
		// interface because the replacement would be significantly more verbose.
		if isComplicatedType(t) {
			continue
		}

		// Don't offer the fix if the type string is too long.
		oldLen := int(expr.End() - expr.Pos())
		newLen := len(tstr)
		if newLen >= 16 && newLen > 3*oldLen {
			continue
		}

		pass.Report(analysis.Diagnostic{
			Pos:     call.Fun.Pos(),
			End:     call.Fun.End(),
			Message: "reflect.TypeOf call can be simplified using TypeFor",
			SuggestedFixes: []analysis.SuggestedFix{{
				// reflect.TypeOf (...T value...) ------------- reflect.TypeFor[T]( )
				Message: "Replace TypeOf by TypeFor",
				TextEdits: append([]analysis.TextEdit{
					{
						Pos:     sel.Sel.Pos(),
						End:     sel.Sel.End(),
						NewText: []byte("TypeFor[" + tstr + "]"),
					},
					// delete (pure) argument
					{
						Pos: call.Lparen + 1,
						End: call.Rparen,
					},
				}, edits...),
			}},
		})
	}

	return nil, nil
}

// usesNonTypeSymbol reports whether expr uses a non-type symbol. A
// value-level object (a var, const, or func) or any other named entity whose
// identifier would disappear in a TypeFor replacement.
func usesNonTypeSymbol(info *types.Info, expr ast.Expr) bool {
	for n := range ast.Preorder(expr) {
		id, ok := n.(*ast.Ident)
		if !ok {
			continue
		}
		switch info.Uses[id].(type) {
		case *types.TypeName, *types.PkgName, *types.Nil, *types.Builtin:
			// Type-level names reappear in (or are irrelevant to) the
			// type argument, so they may be erased safely.
		default:
			return true
		}
	}
	return false
}

// isComplicatedType reports whether type t is complicated, e.g. it is or contains an
// unnamed struct, interface, or function signature.
func isComplicatedType(t types.Type) bool {
	var check func(typ types.Type) bool
	check = func(typ types.Type) bool {
		switch t := typ.(type) {
		case typesinternal.NamedOrAlias:
			for ta := range t.TypeArgs().Types() {
				if check(ta) {
					return true
				}
			}
			return false
		case *types.Struct, *types.Interface, *types.Signature:
			// These are complex types with potentially many elements so we should avoid duplicating their definition.
			return true
		case *types.Pointer:
			return check(t.Elem())
		case *types.Slice:
			return check(t.Elem())
		case *types.Array:
			return check(t.Elem())
		case *types.Chan:
			return check(t.Elem())
		case *types.Map:
			return check(t.Key()) || check(t.Elem())
		case *types.Basic:
			return false
		case *types.TypeParam:
			return false
		default:
			// Includes types.Union
			return true
		}
	}

	return check(t)
}
