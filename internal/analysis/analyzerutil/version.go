// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package analyzerutil

import (
	"go/ast"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/internal/packagepath"
	"golang.org/x/tools/internal/stdlib"
	"golang.org/x/tools/internal/versions"
)

// FileUsesGoVersion reports whether the specified file may use features of
// the specified version of Go (e.g. "go1.24").
//
// It returns false when version information is not available, such as for
// parsed files that are ignored by the type checker.
func FileUsesGoVersion(pass *analysis.Pass, file *ast.File, version string) (_res bool) {
	fileVersion, ok := pass.TypesInfo.FileVersions[file]
	if !ok {
		return false // be conservative in the absence of information (e.g. IgnoredFiles)
	}

	// Standard packages that are part of toolchain bootstrapping are not considered to use a version of Go later than the current bootstrap toolchain version.
	pkgpath := pass.Pkg.Path()
	if packagepath.MaybeStdPackage(pkgpath) &&
		stdlib.IsBootstrapPackage(pkgpath) && // (excludes "*_test" external test packages)
		!strings.HasSuffix(pass.Fset.File(file.Pos()).Name(), "_test.go") { // (excludes all tests)
		fileVersion = stdlib.BootstrapVersion.String() // package must bootstrap
	}

	return !versions.Before(fileVersion, version)
}
