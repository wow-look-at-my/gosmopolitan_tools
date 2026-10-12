// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package workspace

import (
	"strings"
	"testing"

	"golang.org/x/tools/gopls/internal/protocol"

	. "golang.org/x/tools/gopls/internal/test/integration"
)

func TestStubMethods64087(t *testing.T) {
	// We can't use the @fix or @quickfixerr or @codeaction
	// because the error now reported by the corrected logic
	// is internal and silently causes no fix to be offered.
	//
	// See also the similar TestStubMethods64545 below.

	const files = `
This is a regression test for a panic (issue #64087) in stub methods.

The illegal expression int("") caused a "cannot convert" error that
spuriously triggered the "stub methods" in a function whose return
statement had too many operands, leading to an out-of-bounds index.

-- go.mod --
module mod.com
go 1.18

-- a.go --
package a

func f() error {
	return nil, myerror{int("")}
}

type myerror struct{any}
`
	Run(t, files, func(t *testing.T, env *Env) {
		env.OpenFile("a.go")

		// Expect a "wrong result count" diagnostic.
		var d protocol.PublishDiagnosticsParams
		env.AfterChange(ReadDiagnostics("a.go", &d))

		// In no particular order, we expect:
		//  "...too many return values..." (compiler)
		//  "...cannot convert..." (compiler)
		// and possibly:
		//  "...too many return values..." (fillreturns)
		// We check only for the first of these.
		found := false
		for i, diag := range d.Diagnostics {
			msg := diag.MessageString()
			t.Logf("Diagnostics[%d] = %q (%s)", i, msg, diag.Source)
			if strings.Contains(msg, "too many return") {
				found = true
			}
		}
		if !found {
			t.Fatalf("Expected WrongResultCount diagnostic not found.")
		}

		// GetQuickFixes should not panic (the original bug).
		fixes := env.GetQuickFixes("a.go", d.Diagnostics)

		// We should not be offered a "stub methods" fix.
		for _, fix := range fixes {
			if strings.Contains(fix.Title, "Implement error") {
				t.Errorf("unexpected 'stub methods' fix: %#v", fix)
			}
		}
	})
}

func TestStubMethods64545(t *testing.T) {
	// We can't use the @fix or @quickfixerr or @codeaction
	// because the error now reported by the corrected logic
	// is internal and silently causes no fix to be offered.
	//
	// TODO(adonovan): we may need to generalize this test and
	// TestStubMethods64087 if this happens a lot.

	const files = `
This is a regression test for a panic (issue #64545) in stub methods.

The illegal expression int("") caused a "cannot convert" error that
spuriously triggered the "stub methods" in a function whose var
spec had no RHS values, leading to an out-of-bounds index.

-- go.mod --
module mod.com
go 1.18

-- a.go --
package a

var _ [int("")]byte
`
	Run(t, files, func(t *testing.T, env *Env) {
		env.OpenFile("a.go")

		// Expect a "cannot convert" diagnostic, and perhaps others.
		var d protocol.PublishDiagnosticsParams
		env.AfterChange(ReadDiagnostics("a.go", &d))

		found := false
		for i, diag := range d.Diagnostics {
			msg := diag.MessageString()
			t.Logf("Diagnostics[%d] = %q (%s)", i, msg, diag.Source)
			if strings.Contains(msg, "cannot convert") {
				found = true
			}
		}
		if !found {
			t.Fatalf("Expected 'cannot convert' diagnostic not found.")
		}

		// GetQuickFixes should not panic (the original bug).
		fixes := env.GetQuickFixes("a.go", d.Diagnostics)

		// We should not be offered a "stub methods" fix.
		for _, fix := range fixes {
			if strings.Contains(fix.Title, "Implement error") {
				t.Errorf("unexpected 'stub methods' fix: %#v", fix)
			}
		}
	})
}

// quick fix didn't offer add imports
func TestIssue70755(t *testing.T) {
	files := `
-- go.mod --
module failure.com
go 1.23
-- bar/bar.go --
package notbar
type NotBar struct{}
-- baz/baz.go --
package baz
type Baz struct{}
-- foo/foo.go --
package foo
type foo struct {
bar notbar.NotBar
bzz baz.Baz
}
`

	Run(t, files, func(t *testing.T, env *Env) {
		env.OpenFile("foo/foo.go")
		var d protocol.PublishDiagnosticsParams
		env.AfterChange(ReadDiagnostics("foo/foo.go", &d))
		// should get two, one for undefined notbar
		// and one for undefined baz
		fixes := env.GetQuickFixes("foo/foo.go", d.Diagnostics)
		if len(fixes) != 2 {
			t.Fatalf("got %v, want 2 quick fixes", fixes)
		}
		good := 0
		var failures strings.Builder
		for _, f := range fixes {
			ti := f.Title
			// these may be overly white-space sensitive
			if ti == "Add import: notbar \"failure.com/bar\"" ||
				ti == "Add import:  \"failure.com/baz\"" {
				good++
			} else {
				failures.WriteString(ti)
			}
		}
		if good != 2 {
			t.Errorf("failed to find\n%q, got\n%q\n%q", failures.String(), fixes[0].Title,
				fixes[1].Title)
		}

	})
}
