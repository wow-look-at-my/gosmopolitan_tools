// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// The fix package defines the suite of analyzers used by cmd/fix, the default
// analysis tool run by "go fix". Its behavior is equivalent to.
//
//	func main() { unitchecker.Main(fix.Suite...) }
//
// If you need a different suite, define your own tool and run "go vet
// -vettool=mytool".
package fix

import (
	"slices"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildtag"
	"golang.org/x/tools/go/analysis/passes/hostport"
	"golang.org/x/tools/go/analysis/passes/inline"
	"golang.org/x/tools/go/analysis/passes/modernize"
)

// Suite is the suite of analyzers run by cmd/fix.
var Suite = slices.Concat(
	[]*analysis.Analyzer{
		buildtag.Analyzer,
		hostport.Analyzer,
		inline.Analyzer,
	},
	modernize.Suite,
)
