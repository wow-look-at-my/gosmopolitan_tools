// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package imports

import "context"

// These types document the APIs below.
//
// TODO(rfindley): consider making these defined types rather than aliases.
type (
	ImportPath  = string
	PackageName = string
	Symbol      = string

	// References is set of References found in a Go file.
	References = map[PackageName]map[Symbol]bool
)

// A Result satisfies a missing import.
type Result struct {
	Import  *ImportInfo
	Package *PackageInfo
}

// An ImportInfo represents a single import statement.
type ImportInfo struct {
	ImportPath string // import path, e.g. "crypto/rand".
	Name       string // import name, e.g. "crand", or "" if none.
}

// A PackageInfo represents what's known about a package.
type PackageInfo struct {
	Name    string          // package name in the package declaration, if known
	Exports map[string]bool // set of names of known package level sortSymbols
}

// A Source provides imports to satisfy unresolved references in the file being
// fixed.
type Source interface {
	// LoadPackageNames queries PackageName information for the requested import paths.
	LoadPackageNames(ctx context.Context, srcDir string, paths []ImportPath) (map[ImportPath]PackageName, error)

	// ResolveReferences asks the Source for the best package name to satisfy each of the missing references.
	ResolveReferences(ctx context.Context, filename string, missing References) ([]*Result, error)
}
