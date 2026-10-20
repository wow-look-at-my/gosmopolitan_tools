// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// This file implements access to gccgo-generated export data.

package main

import (
	"go/importer"
	"go/token"
	"go/types"
)

func init() {
	register("gccgo", importer.ForCompiler(token.NewFileSet(), "gccgo", nil))
}

// Print the extra gccgo compiler data for this package, if it exists.
func (p *printer) printGccgoExtra(pkg *types.Package) {

}
