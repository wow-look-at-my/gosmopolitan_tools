// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package inspect defines an Analyzer that provides an AST inspector
// (golang.org/x/tools/go/ast/inspector.Inspector) for the syntax trees
// of a package. It is only a building block for other analyzers.
package inspect

import (
	"reflect"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ast/inspector"
)

var Analyzer = &analysis.Analyzer{
	Name:             "inspect",
	Doc:              "optimize AST traversal for later passes",
	URL:              "https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/inspect",
	Run:              run,
	RunDespiteErrors: true,
	ResultType:       reflect.TypeFor[*inspector.Inspector](),
}

func run(pass *analysis.Pass) (any, error) {
	return inspector.New(pass.Files), nil
}
