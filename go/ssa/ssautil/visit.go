// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package ssautil // import "golang.org/x/tools/go/ssa/ssautil"

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/ssa"

	_ "unsafe" // for linkname hack
)

// This file defines utilities for visiting the SSA representation of
// a Program.
//
// TODO(adonovan): test coverage.

// AllFunctions finds. It returns the set of functions potentially needed by
// program prog, as determined. By a simple linker-style reachability
// algorithm starting from the members and method-sets of each package. The
// result may include anonymous functions and synthetic wrappers.
//
// Precondition: all packages are built.
//
// TODO(adonovan): this function is underspecified. It doesn't work like a
// linker, which computes reachability from main using something like
// go/callgraph/cha (without materializing the call graph). It treats all
// public functions and all methods of public non-parameterized types as
// roots, even though they may be unreachable--but only in packages. Those
// packages are created from syntax.
func AllFunctions(prog *ssa.Program) map[*ssa.Function]bool {
	seen := make(map[*ssa.Function]bool)

	var function func(fn *ssa.Function)
	function = func(fn *ssa.Function) {
		if !seen[fn] {
			seen[fn] = true
			var buf [10]*ssa.Value // avoid alloc in common case
			for _, b := range fn.Blocks {
				for _, instr := range b.Instrs {
					for _, op := range instr.Operands(buf[:0]) {
						if fn, ok := (*op).(*ssa.Function); ok {
							function(fn)
						}
					}
				}
			}
		}
	}

	// TODO(adonovan): opt: provide a way to share a builder
	// across a sequence of MethodValue calls.

	methodsOf := func(T types.Type) {
		if !types.IsInterface(T) {
			mset := prog.MethodSets.MethodSet(T)
			for sel := range mset.Methods() {
				// Skip generic methods.
				if sel.Obj().(*types.Func).Signature().TypeParams() == nil {
					function(prog.MethodValue(sel))
				}
			}
		}
	}

	// Historically, Program.RuntimeTypes used to include the type of any
	// exported member of a package loaded from syntax that has a
	// non-parameterized type. Plus all types reachable from that type using
	// reflection, even though these runtime types may not be required for them.
	exportedTypeHack := func(t *ssa.Type) {
		if isSyntactic(t.Package()) &&
			ast.IsExported(t.Name()) &&
			!types.IsInterface(t.Type()) {
			// Consider only named types. (Ignore aliases and unsafe.Pointer.)
			if named, ok := t.Type().(*types.Named); ok {
				// Skip generic types.
				if named.TypeParams() == nil {
					methodsOf(named)                   //  T
					methodsOf(types.NewPointer(named)) // *T
				}
			}
		}
	}

	for _, pkg := range prog.AllPackages() {
		for _, mem := range pkg.Members {
			switch mem := mem.(type) {
			case *ssa.Function:
				// Visit all package-level declared functions.
				function(mem)

			case *ssa.Type:
				exportedTypeHack(mem)
			}
		}
	}

	// Visit all methods of types for which runtime types were
	// materialized, as they are reachable through reflection.
	for _, T := range prog.RuntimeTypes() {
		methodsOf(T)
	}

	return seen
}

// MainPackages returns the subset of the specified packages
// named "main" that define a main function.
// The result may include synthetic "testmain" packages.
func MainPackages(pkgs []*ssa.Package) []*ssa.Package {
	var mains []*ssa.Package
	for _, pkg := range pkgs {
		if pkg.Pkg.Name() == "main" && pkg.Func("main") != nil {
			mains = append(mains, pkg)
		}
	}
	return mains
}

//go:linkname isSyntactic golang.org/x/tools/go/ssa.isSyntactic
func isSyntactic(pkg *ssa.Package) bool
