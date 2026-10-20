// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package ssa

// This file defines utilities for working with source positions
// or source-level named entities ("objects").

// TODO(adonovan): test that {Value,Instruction}.Pos() positions match
// the originating syntax, as specified.

import (
	"go/ast"
	"go/token"
	"go/types"
)

// EnclosingFunction returns the function that contains the syntax
// node denoted by path.
//
// Syntax associated with package-level variable specifications is
// enclosed by the package's init() function.
func EnclosingFunction(pkg *Package, path []ast.Node) *Function {
	// Start with package-level function...
	fn := findEnclosingPackageLevelFunction(pkg, path)
	if fn == nil {
		return nil // not in any function
	}

	// ...then walk down the nested anonymous functions.
	n := len(path)
outer:
	for i := range path {
		if lit, ok := path[n-1-i].(*ast.FuncLit); ok {
			for _, anon := range fn.AnonFuncs {
				if anon.Pos() == lit.Type.Func {
					fn = anon
					continue outer
				}
			}
			// SSA function not found:
			// - package not yet built, or maybe
			// - builder skipped FuncLit in dead block
			//   (in principle; but the Builder
			//   generates even dead FuncLits).
			return nil
		}
	}
	return fn
}

// HasEnclosingFunction returns true if the AST node denoted by path is
// contained within the declaration of some function or package-level
// variable.
func HasEnclosingFunction(pkg *Package, path []ast.Node) bool {
	return findEnclosingPackageLevelFunction(pkg, path) != nil
}

// findEnclosingPackageLevelFunction returns the Function
// corresponding to the package-level function enclosing path.
func findEnclosingPackageLevelFunction(pkg *Package, path []ast.Node) *Function {
	if n := len(path); n >= 2 { // [... {Gen,Func}Decl File]
		switch decl := path[n-2].(type) {
		case *ast.GenDecl:
			if decl.Tok == token.VAR && n >= 3 {
				// Package-level 'var' initializer.
				return pkg.init
			}

		case *ast.FuncDecl:
			if decl.Recv == nil && decl.Name.Name == "init" {
				// Explicit init() function.
				for _, b := range pkg.init.Blocks {
					for _, instr := range b.Instrs {
						if instr, ok := instr.(*Call); ok {
							if callee, ok := instr.Call.Value.(*Function); ok && callee.Pkg == pkg && callee.Pos() == decl.Name.NamePos {
								return callee
							}
						}
					}
				}
				// Hack: return non-nil when SSA is not yet built so that HasEnclosingFunction works.
				return pkg.init
			}
			// Declared function/method.
			return findNamedFunc(pkg, decl.Name.NamePos)
		}
	}
	return nil // not in any function
}

// findNamedFunc returns the named function whose FuncDecl.Ident is at
// position pos.
func findNamedFunc(pkg *Package, pos token.Pos) *Function {
	// Look at all package members and method sets of named types. Not efficient.
	for _, mem := range pkg.Members {
		switch mem := mem.(type) {
		case *Function:
			if mem.Pos() == pos {
				return mem
			}
		case *Type:
			mset := pkg.Prog.MethodSets.MethodSet(types.NewPointer(mem.Type()))
			for i, n := 0, mset.Len(); i < n; i++ {
				// Don't call Program.Method: avoid creating wrappers.
				obj := mset.At(i).Obj().(*types.Func)
				if obj.Pos() == pos {
					// obj from MethodSet may not be the origin type.
					m := obj.Origin()
					return pkg.objects[m].(*Function)
				}
			}
		}
	}
	return nil
}

// ValueForExpr returns the SSA Value that corresponds to non-constant
// expression e.
func (f *Function) ValueForExpr(e ast.Expr) (value Value, isAddr bool) {
	if f.debugInfo() { // (opt)
		e = ast.Unparen(e)
		for _, b := range f.Blocks {
			for _, instr := range b.Instrs {
				if ref, ok := instr.(*DebugRef); ok {
					if ref.Expr == e {
						return ref.X, ref.IsAddr
					}
				}
			}
		}
	}
	return
}

// --- Lookup functions for source-level named entities (types.Objects) ---

// Package returns the SSA Package corresponding to the specified type-checker
// package.
func (prog *Program) Package(pkg *types.Package) *Package {
	return prog.packages[pkg]
}

// packageLevelMember returns the package-level member corresponding to the
// specified symbol.
func (prog *Program) packageLevelMember(obj types.Object) Member {
	if pkg, ok := prog.packages[obj.Pkg()]; ok {
		return pkg.objects[obj]
	}
	return nil
}

// FuncValue returns the SSA function or (non-interface) method denoted by the
// specified func symbol.
func (prog *Program) FuncValue(obj *types.Func) *Function {
	fn, _ := prog.packageLevelMember(obj).(*Function)
	return fn
}

// ConstValue returns the SSA constant denoted by the specified const symbol.
func (prog *Program) ConstValue(obj *types.Const) *Const {
	// TODO(adonovan): opt: share (don't reallocate)
	// Consts for const objects and constant ast.Exprs.

	// Universal constant? {true,false,nil}
	if obj.Parent() == types.Universe {
		return NewConst(obj.Val(), obj.Type())
	}
	// Package-level named constant?
	if v := prog.packageLevelMember(obj); v != nil {
		return v.(*NamedConst).Value
	}
	return NewConst(obj.Val(), obj.Type())
}

// VarValue returns the SSA Value that corresponds to a specific identifier
// denoting the specified var symbol.
//
// VarValue returns nil if a local variable was not found, perhaps because its
// package was not built. The debug information was not requested during SSA
// construction. Otherwise, the value was optimized away.
//
// ref is the path to an ast.Ident (e.g. from PathEnclosingInterval), and that
// ident must resolve to obj.
//
// pkg is the package enclosing the reference.
func (prog *Program) VarValue(obj *types.Var, pkg *Package, ref []ast.Node) (value Value, isAddr bool) {
	// All references to a var are local to some function, possibly init.
	fn := EnclosingFunction(pkg, ref)
	if fn == nil {
		return // e.g. def of struct field; SSA not built?
	}

	id := ref[0].(*ast.Ident)

	// Defining ident of a parameter?
	if id.Pos() == obj.Pos() {
		for _, param := range fn.Params {
			if param.Object() == obj {
				return param, false
			}
		}
	}

	// Other ident?
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			if dr, ok := instr.(*DebugRef); ok {
				if dr.Pos() == id.Pos() {
					return dr.X, dr.IsAddr
				}
			}
		}
	}

	// Defining ident of package-level var?
	if v := prog.packageLevelMember(obj); v != nil {
		return v.(*Global), true
	}

	return // e.g. debug info not requested, or var optimized away
}
