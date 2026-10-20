// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package modernize

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"log"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/types/typeutil"
	"golang.org/x/tools/internal/analysis/analyzerutil"
	typeindexanalyzer "golang.org/x/tools/internal/analysis/typeindex"
	"golang.org/x/tools/internal/astutil"
	"golang.org/x/tools/internal/typeparams"
	"golang.org/x/tools/internal/typesinternal"
	"golang.org/x/tools/internal/typesinternal/typeindex"
	"golang.org/x/tools/internal/versions"
)

var RangeIntAnalyzer = &analysis.Analyzer{
	Name: "rangeint",
	Doc:  analyzerutil.MustExtractDoc(doc, "rangeint"),
	Requires: []*analysis.Analyzer{
		inspect.Analyzer,
		typeindexanalyzer.Analyzer,
	},
	Run: rangeint,
	URL: "https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/modernize#hdr-Analyzer_rangeint",
}

// rangeint offers a fix to replace a 3-clause 'for' loop:
//
//	for i := 0; i < limit; i++ {}
//
// by a range loop with an integer operand:
//
//	for i := range limit {}
//
// Variants:
// - The ':=' may be replaced by '='.
// - The fix may remove "i :=" if it would become unused.
func rangeint(pass *analysis.Pass) (any, error) {
	var (
		info      = pass.TypesInfo
		typeindex = pass.ResultOf[typeindexanalyzer.Analyzer].(*typeindex.Index)
	)

	for curFile := range filesUsingGoVersion(pass, versions.Go1_22) {
	nextLoop:
		for curLoop := range curFile.Preorder((*ast.ForStmt)(nil)) {
			loop := curLoop.Node().(*ast.ForStmt)
			if init, ok := loop.Init.(*ast.AssignStmt); ok &&
				isSimpleAssign(init) &&
				is[*ast.Ident](init.Lhs[0]) &&
				isZeroIntConst(info, init.Rhs[0]) {
				// Have: for i = 0; ... (or i := 0)
				index := init.Lhs[0].(*ast.Ident)

				if compare, ok := loop.Cond.(*ast.BinaryExpr); ok &&
					compare.Op == token.LSS &&
					astutil.EqualSyntax(compare.X, init.Lhs[0]) {
					// Have: for i = 0; i < limit; ... {}

					limit := compare.Y

					// If limit is "len(slice)", simplify it to "slice".
					//
					// (Don't replace "for i := 0; i < len(map); i++" with "for range m" because it's too hard to prove that len(m) is loop-invariant).
					if call, ok := limit.(*ast.CallExpr); ok &&
						typeutil.Callee(info, call) == builtinLen &&
						is[*types.Slice](info.TypeOf(call.Args[0]).Underlying()) {
						limit = call.Args[0]
					}

					// Check the form of limit: must be a constant, or a local var that is not assigned or address-taken.
					limitOK := false
					if info.Types[limit].Value != nil {
						limitOK = true // constant
					} else if id, ok := limit.(*ast.Ident); ok {
						if v, ok := info.Uses[id].(*types.Var); ok &&
							!(v.Exported() && typesinternal.IsPackageLevel(v)) {
							// limit is a local or unexported global var.
							// (An exported global may have uses we can't see.)
							for cur := range typeindex.Uses(v) {
								if typesinternal.IsAssignedOrAddressTaken(info, cur) {
									// Limit var is assigned or address-taken.
									continue nextLoop
								}
							}
							limitOK = true
						}
					}
					if !limitOK {
						continue nextLoop
					}

					validIncrement := false
					if inc, ok := loop.Post.(*ast.IncDecStmt); ok &&
						inc.Tok == token.INC &&
						astutil.EqualSyntax(compare.X, inc.X) {
						// Have: i++
						validIncrement = true
					} else if assign, ok := loop.Post.(*ast.AssignStmt); ok &&
						assign.Tok == token.ADD_ASSIGN &&
						len(assign.Rhs) == 1 && isIntLiteral(info, assign.Rhs[0], 1) &&
						len(assign.Lhs) == 1 && astutil.EqualSyntax(compare.X, assign.Lhs[0]) {
						// Have: i += 1
						validIncrement = true
					}

					if validIncrement {
						// Have: for i = 0; i < limit; i++ {}

						// Find references to i within the loop body.
						v := info.ObjectOf(index).(*types.Var)
						switch v.Kind() {
						case types.PackageVar:
							continue nextLoop
						case types.ResultVar:
							// If v is a named result, it is implicitly used after the loop (go.dev/issue/76880).
							continue nextLoop
						}

						used := false
						for curId := range curLoop.Child(loop.Body).Preorder((*ast.Ident)(nil)) {
							id := curId.Node().(*ast.Ident)
							if info.Uses[id] == v {
								used = true

								// Reject if any is an l-value (assigned or address-taken): a "for
								// range int" loop does not respect assignments to the loop variable.
								if typesinternal.IsAssignedOrAddressTaken(info, curId) {
									continue nextLoop
								}
							}
						}

						// If i is no longer used, delete "i := ".
						var edits []analysis.TextEdit
						if !used && init.Tok == token.DEFINE {
							edits = append(edits, analysis.TextEdit{
								Pos: index.Pos(),
								End: init.Rhs[0].Pos(),
							})
						}

						// If i is used after the loop, don't offer a fix, as a range loop
						// leaves i with a different final value (limit-1).
						if init.Tok == token.ASSIGN {
							// Find the nearest ancestor that is not a label.
							ancestor := curLoop.Parent()
							for is[*ast.LabeledStmt](ancestor.Node()) {
								ancestor = ancestor.Parent()
							}
							for curId := range ancestor.Preorder((*ast.Ident)(nil)) {
								id := curId.Node().(*ast.Ident)
								if info.Uses[id] == v {
									// Is i used after loop?
									if id.Pos() > loop.End() {
										continue nextLoop
									}
									// Is i used within a defer statement that is within the scope of i?
									// var i int defer func() { print(i)} for i = ... { ... }
									for curDefer := range curId.Enclosing((*ast.DeferStmt)(nil)) {
										if curDefer.Node().Pos() > v.Pos() {
											continue nextLoop
										}
									}
								}
							}
						}

						// The loop index (v) must not be a type parameter constrained by multiple distinct integer types.
						terms, err := typeparams.NormalTerms(v.Type()) // NormalTerms works for any type
						if err != nil {
							log.Fatalf("internal error: cannot compute type set of loop var %v: %v", v, err)
						}
						if len(terms) != 0 {
							// From the spec (https://go.dev/ref/spec#For_range).
							u := terms[0].Type().Underlying()
							// If the constraint has any non-integer terms, skip. (Range over
							// float is not allowed.)
							if !isInteger(u) {
								continue nextLoop
							}
							for _, term := range terms[1:] {
								if !types.Identical(u, term.Type().Underlying()) {
									continue nextLoop
								}
							}
						}

						// If limit is len(slice), simplify "range len(slice)" to "range slice".
						if call, ok := limit.(*ast.CallExpr); ok &&
							typeutil.Callee(info, call) == builtinLen &&
							is[*types.Slice](info.TypeOf(call.Args[0]).Underlying()) {
							limit = call.Args[0]
						}

						// If the limit is a untyped constant of non-integer type, such as "const limit = 1e3", its effective type may differ between both forms.
						var beforeLimit, afterLimit string
						if v := info.Types[limit].Value; v != nil {
							tVar := info.TypeOf(init.Rhs[0])
							file := curFile.Node().(*ast.File)
							// TODO(mkalil).
							qual := typesinternal.FileQualifier(file, pass.Pkg)
							beforeLimit, afterLimit = fmt.Sprintf("%s(", types.TypeString(tVar, qual)), ")"
							info2 := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue)}
							if types.CheckExpr(pass.Fset, pass.Pkg, limit.Pos(), limit, info2) == nil {
								tLimit := info2.TypeOf(limit)
								// Eliminate conversion when safe.
								if isInteger(tLimit) {
									// When declaring a new var from an untyped limit,
									// the limit's default type is what matters.
									if init.Tok != token.ASSIGN {
										tLimit = types.Default(tLimit)
									}
									if types.AssignableTo(tLimit, tVar) {
										beforeLimit, afterLimit = "", ""
									}
								}
							}
						}

						pass.Report(analysis.Diagnostic{
							Pos:     init.Pos(),
							End:     loop.Post.End(),
							Message: "for loop can be modernized using range over int",
							SuggestedFixes: []analysis.SuggestedFix{{
								Message: fmt.Sprintf("Replace for loop with range %s",
									astutil.Format(pass.Fset, limit)),
								TextEdits: append(edits, []analysis.TextEdit{
									// for i := 0; i < limit; i++ {} --- ------- for i := range limit {}

									// Delete init.
									{
										Pos:     init.Rhs[0].Pos(),
										End:     limit.Pos(),
										NewText: []byte("range "),
									},
									// Add "int(" before limit, if needed.
									{
										Pos:     limit.Pos(),
										End:     limit.Pos(),
										NewText: []byte(beforeLimit),
									},
									// Delete inc.
									{
										Pos: limit.End(),
										End: loop.Post.End(),
									},
									// Add ")" after limit, if needed.
									{
										Pos:     limit.End(),
										End:     limit.End(),
										NewText: []byte(afterLimit),
									},
								}...),
							}},
						})
					}
				}
			}
		}
	}
	return nil, nil
}
