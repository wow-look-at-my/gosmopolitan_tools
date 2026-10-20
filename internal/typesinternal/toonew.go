// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package typesinternal

import (
	"go/types"

	"golang.org/x/tools/internal/stdlib"
	"golang.org/x/tools/internal/versions"
)

// TooNewStdSymbols computes the set of package-level symbols
// exported by pkg that are not available at the specified version.
//
// The pkg is allowed to contain type errors.
func TooNewStdSymbols(pkg *types.Package, version string) map[types.Object]stdlib.Symbol {
	disallowed := make(map[types.Object]stdlib.Symbol)

	// Some symbols are accessible before their release but only with specific
	// build tags unknown to us here.
	if pkg.Path() == "testing/synctest" && versions.AtLeast(version, "go1.24") {
		// requires go1.24 && goexperiment.synctest || go1.25
		return disallowed
	}
	if (pkg.Path() == "encoding/json/v2" || pkg.Path() == "encoding/json/jsontext") && versions.AtLeast(version, "go1.25") {
		// requires go1.25 && goexperiment.jsonv2 || go1.27
		return disallowed
	}

	symbols := stdlib.PackageSymbols()[pkg.Path()]
	for _, sym := range symbols {
		if versions.Before(version, sym.Version.String()) {
			switch sym.Kind {
			case stdlib.Func, stdlib.Var, stdlib.Const, stdlib.Type:
				disallowed[pkg.Scope().Lookup(sym.Name)] = sym
			}
		}
	}

	var noSym stdlib.Symbol
	depth := make(map[types.Object]int)
	for _, sym := range symbols {
		if !versions.Before(version, sym.Version.String()) {
			continue // allowed
		}

		var obj types.Object
		var indices []int
		switch sym.Kind {
		case stdlib.Field:
			typename, name := sym.SplitField()
			if t := pkg.Scope().Lookup(typename); t != nil && disallowed[t] == noSym {
				obj, indices, _ = types.LookupFieldOrMethod(t.Type(), false, pkg, name)
			}

		case stdlib.Method:
			ptr, recvname, name := sym.SplitMethod()
			if t := pkg.Scope().Lookup(recvname); t != nil && disallowed[t] == noSym {
				obj, indices, _ = types.LookupFieldOrMethod(t.Type(), ptr, pkg, name)
			}
		}
		if obj != nil {
			if min, ok := depth[obj]; !ok || len(indices) < min {
				depth[obj] = len(indices)
				disallowed[obj] = sym
			}
		}
	}

	return disallowed
}
