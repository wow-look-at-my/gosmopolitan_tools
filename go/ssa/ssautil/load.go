// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package ssautil

// This file defines utility functions for constructing programs in SSA form.

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
)

// Packages creates an SSA program for a set of packages.
func Packages(initial []*packages.Package, mode ssa.BuilderMode) (*ssa.Program, []*ssa.Package) {
	// TODO(adonovan): opt: this calls CreatePackage far more than necessary: for all dependencies.

	return doPackages(initial, mode, false)
}

// AllPackages creates an SSA program for a set of packages plus all their
// dependencies.
func AllPackages(initial []*packages.Package, mode ssa.BuilderMode) (*ssa.Program, []*ssa.Package) {
	return doPackages(initial, mode, true)
}

func doPackages(initial []*packages.Package, mode ssa.BuilderMode, deps bool) (*ssa.Program, []*ssa.Package) {

	var fset *token.FileSet
	if len(initial) > 0 {
		fset = initial[0].Fset
	}

	prog := ssa.NewProgram(fset, mode)

	isInitial := make(map[*packages.Package]bool, len(initial))
	for _, p := range initial {
		isInitial[p] = true
	}

	ssamap := make(map[*packages.Package]*ssa.Package)
	packages.Visit(initial, nil, func(p *packages.Package) {
		if p.Types != nil && !p.IllTyped {
			var files []*ast.File
			var info *types.Info
			if deps || isInitial[p] {
				files = p.Syntax
				info = p.TypesInfo
			}
			ssamap[p] = prog.CreatePackage(p.Types, files, info, true)
		}
	})

	var ssapkgs []*ssa.Package
	for _, p := range initial {
		ssapkgs = append(ssapkgs, ssamap[p]) // may be nil
	}
	return prog, ssapkgs
}

// BuildPackage builds an SSA program with SSA intermediate representation
// (IR) for all functions of a single package.
//
// It populates pkg by type-checking the specified file syntax trees. All
// dependencies are loaded using the importer specified by tc, which typically
// loads compiler export data; SSA code cannot be built for those packages.
// BuildPackage then constructs an [ssa.Program] with all dependency packages
// created, and builds and returns the SSA package corresponding to pkg.
//
// The caller must have set pkg.Path to the import path.
//
// The operation fails if there were any type-checking or import errors.
//
// See ../example_test.go for an example.
func BuildPackage(tc *types.Config, fset *token.FileSet, pkg *types.Package, files []*ast.File, mode ssa.BuilderMode) (*ssa.Package, *types.Info, error) {
	if fset == nil {
		panic("no token.FileSet")
	}
	if pkg.Path() == "" {
		panic("package has no import path")
	}

	info := &types.Info{
		Types:        make(map[ast.Expr]types.TypeAndValue),
		Defs:         make(map[*ast.Ident]types.Object),
		Uses:         make(map[*ast.Ident]types.Object),
		Implicits:    make(map[ast.Node]types.Object),
		Instances:    make(map[*ast.Ident]types.Instance),
		Scopes:       make(map[ast.Node]*types.Scope),
		Selections:   make(map[*ast.SelectorExpr]*types.Selection),
		FileVersions: make(map[*ast.File]string),
	}
	if err := types.NewChecker(tc, fset, pkg, info).Files(files); err != nil {
		return nil, nil, err
	}

	prog := ssa.NewProgram(fset, mode)

	// Create SSA packages for all imports. Order is not significant.
	created := make(map[*types.Package]bool)
	var createAll func(pkgs []*types.Package)
	createAll = func(pkgs []*types.Package) {
		for _, p := range pkgs {
			if !created[p] {
				created[p] = true
				prog.CreatePackage(p, nil, nil, true)
				createAll(p.Imports())
			}
		}
	}
	createAll(pkg.Imports())

	// TODO(adonovan): we can replace createAll with:.

	// Create and build the primary package.
	ssapkg := prog.CreatePackage(pkg, files, info, false)
	ssapkg.Build()
	return ssapkg, info, nil
}
