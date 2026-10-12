// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package workspace

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/tools/gopls/internal/cache"
	"golang.org/x/tools/gopls/internal/protocol"
	"golang.org/x/tools/gopls/internal/protocol/command"
	"golang.org/x/tools/gopls/internal/test/integration/fake"
	"golang.org/x/tools/gopls/internal/util/bug"
	"golang.org/x/tools/gopls/internal/util/goversion"
	"golang.org/x/tools/internal/gocommand"

	. "golang.org/x/tools/gopls/internal/test/integration"
)

func TestMain(m *testing.M) {
	bug.PanicOnBugs = true
	os.Exit(Main(m))
}

const workspaceProxy = `
-- example.com@v1.2.3/go.mod --
module example.com

go 1.12
-- example.com@v1.2.3/blah/blah.go --
package blah

import "fmt"

func SaySomething() {
	fmt.Println("something")
}
-- random.org@v1.2.3/go.mod --
module random.org

go 1.12
-- random.org@v1.2.3/bye/bye.go --
package bye

func Goodbye() {
	println("Bye")
}
`

// TODO: Add a replace directive.
const workspaceModule = `
-- pkg/go.mod --
module mod.com

go 1.14

require (
	example.com v1.2.3
	random.org v1.2.3
)
-- pkg/go.sum --
example.com v1.2.3 h1:veRD4tUnatQRgsULqULZPjeoBGFr2qBhevSCZllD2Ds=
example.com v1.2.3/go.mod h1:Y2Rc5rVWjWur0h3pd9aEvK5Pof8YKDANh9gHA2Maujo=
random.org v1.2.3 h1:+JE2Fkp7gS0zsHXGEQJ7hraom3pNTlkxC4b2qPfA+/Q=
random.org v1.2.3/go.mod h1:E9KM6+bBX2g5ykHZ9H27w16sWo3QwgonyjM44Dnej3I=
-- pkg/main.go --
package main

import (
	"example.com/blah"
	"mod.com/inner"
	"random.org/bye"
)

func main() {
	blah.SaySomething()
	inner.Hi()
	bye.Goodbye()
}
-- pkg/main2.go --
package main

import "fmt"

func _() {
	fmt.Print("%s")
}
-- pkg/inner/inner.go --
package inner

import "example.com/blah"

func Hi() {
	blah.SaySomething()
}
-- goodbye/bye/bye.go --
package bye

func Bye() {}
-- goodbye/go.mod --
module random.org

go 1.12
`

// Confirm that find references returns all of the references in the module,
// regardless of what the workspace root is.
func TestReferences(t *testing.T) {
	for _, tt := range []struct {
		name, rootPath string
	}{
		{
			name:     "module root",
			rootPath: "pkg",
		},
		{
			name:     "subdirectory",
			rootPath: "pkg/inner",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := []RunOption{ProxyFiles(workspaceProxy)}
			if tt.rootPath != "" {
				opts = append(opts, WorkspaceFolders(tt.rootPath))
			}
			WithOptions(opts...).Run(t, workspaceModule, func(t *testing.T, env *Env) {
				f := "pkg/inner/inner.go"
				env.OpenFile(f)
				locations := env.References(env.RegexpSearch(f, `SaySomething`))
				want := 3
				if got := len(locations); got != want {
					t.Fatalf("expected %v locations, got %v", want, got)
				}
			})
		})
	}
}

// Make sure that analysis diagnostics are cleared for the whole package when
// the only opened file is closed. This test was inspired by the experience in
// VS Code, where clicking on a reference result triggers a
// textDocument/didOpen without a corresponding textDocument/didClose.
func TestClearAnalysisDiagnostics(t *testing.T) {
	WithOptions(
		ProxyFiles(workspaceProxy),
		WorkspaceFolders("pkg/inner"),
	).Run(t, workspaceModule, func(t *testing.T, env *Env) {
		env.OpenFile("pkg/main.go")
		env.AfterChange(
			Diagnostics(env.AtRegexp("pkg/main2.go", "fmt.Print")),
		)
		env.CloseBuffer("pkg/main.go")
		env.AfterChange(
			NoDiagnostics(ForFile("pkg/main2.go")),
		)
	})
}

// TestReloadOnlyOnce checks that changes to the go.mod file do not result in
// redundant package loads (golang/go#54473).
//
// Note that this test may be fragile, as it depends on specific structure to
// log messages around reinitialization. Nevertheless, it is important for
// guarding against accidentally duplicate reloading.
func TestReloadOnlyOnce(t *testing.T) {
	WithOptions(
		ProxyFiles(workspaceProxy),
		WorkspaceFolders("pkg"),
	).Run(t, workspaceModule, func(t *testing.T, env *Env) {
		dir := env.Sandbox.Workdir.URI("goodbye").Path()
		goModWithReplace := fmt.Sprintf(`%s
replace random.org => %s
`, env.ReadWorkspaceFile("pkg/go.mod"), dir)
		env.WriteWorkspaceFile("pkg/go.mod", goModWithReplace)
		env.Await(
			LogMatching(protocol.Info, `packages\.Load #\d+\n`, 2, false),
		)
	})
}

const workspaceModuleProxy = `
-- example.com@v1.2.3/go.mod --
module example.com

go 1.12
-- example.com@v1.2.3/blah/blah.go --
package blah

import "fmt"

func SaySomething() {
	fmt.Println("something")
}
-- b.com@v1.2.3/go.mod --
module b.com

go 1.12
-- b.com@v1.2.3/b/b.go --
package b

func Hello() {}
`

const multiModule = `
-- moda/a/go.mod --
module a.com

require b.com v1.2.3
-- moda/a/go.sum --
b.com v1.2.3 h1:tXrlXP0rnjRpKNmkbLYoWBdq0ikb3C3bKK9//moAWBI=
b.com v1.2.3/go.mod h1:D+J7pfFBZK5vdIdZEFquR586vKKIkqG7Qjw9AxG5BQ8=
-- moda/a/a.go --
package a

import (
	"b.com/b"
)

func main() {
	var x int
	_ = b.Hello()
}
-- modb/go.mod --
module b.com

-- modb/b/b.go --
package b

func Hello() int {
	var x int
}
`

func TestModuleWithExclude(t *testing.T) {
	const proxy = `
-- c.com@v1.2.3/go.mod --
module c.com

go 1.12

require b.com v1.2.3
-- c.com@v1.2.3/blah/blah.go --
package blah

import "fmt"

func SaySomething() {
	fmt.Println("something")
}
-- b.com@v1.2.3/go.mod --
module b.com

go 1.12
-- b.com@v1.2.4/b/b.go --
package b

func Hello() {}
-- b.com@v1.2.4/go.mod --
module b.com

go 1.12
`
	const files = `
-- go.mod --
module a.com

require c.com v1.2.3

exclude b.com v1.2.3
-- main.go --
package a

func main() {
	var x int
}
`
	WithOptions(
		WriteGoSum("."),
		ProxyFiles(proxy),
	).Run(t, files, func(t *testing.T, env *Env) {
		env.OnceMet(
			InitialWorkspaceLoad,
			Diagnostics(env.AtRegexp("main.go", "x")),
		)
	})
}

// This test confirms that a gopls workspace can recover from initialization
// with one invalid module.
func TestOneBrokenModule(t *testing.T) {
	const multiModule = `
-- moda/a/go.mod --
module a.com

require b.com v1.2.3

-- moda/a/a.go --
package a

import (
	"b.com/b"
)

func main() {
	var x int
	_ = b.Hello()
}
-- modb/go.mod --
modul b.com // typo here

-- modb/b/b.go --
package b

func Hello() int {
	var x int
}
`
	WithOptions(
		ProxyFiles(workspaceModuleProxy),
	).Run(t, multiModule, func(t *testing.T, env *Env) {
		env.OpenFile("modb/go.mod")
		env.AfterChange(
			Diagnostics(AtPosition("modb/go.mod", 0, 0)),
		)
		env.RegexpReplace("modb/go.mod", "modul", "module")
		env.SaveBufferWithoutActions("modb/go.mod")
		env.AfterChange(
			Diagnostics(env.AtRegexp("modb/b/b.go", "x")),
		)
	})
}

func TestNonWorkspaceFileCreation(t *testing.T) {
	const files = `
-- work/go.mod --
module mod.com

go 1.12
-- work/x.go --
package x
`

	const code = `
package foo
import "fmt"
var _ = fmt.Printf
`
	WithOptions(
		WorkspaceFolders("work"), // so that outside/... is outside the workspace
	).Run(t, files, func(t *testing.T, env *Env) {
		env.CreateBuffer("outside/foo.go", "")
		env.EditBuffer("outside/foo.go", fake.NewEdit(0, 0, 0, 0, code))
		env.FirstDefinition(env.RegexpSearch("outside/foo.go", `Printf`))
	})
}

// Tests the fix for golang/go#52500.
func TestChangeTestVariant_Issue52500(t *testing.T) {
	const src = `
-- go.mod --
module mod.test

go 1.12
-- main_test.go --
package main_test

type Server struct{}

const MainConst = otherConst
-- other_test.go --
package main_test

const otherConst = 0

func (Server) Foo() {}
`

	Run(t, src, func(t *testing.T, env *Env) {
		env.OpenFile("other_test.go")
		env.RegexpReplace("other_test.go", "main_test", "main")

		// For this test to function, it is necessary to wait on both of the
		// expectations below: the bug is that when switching the package name in
		// other_test.go from main->main_test, metadata for main_test is not marked
		// as invalid. So we need to wait for the metadata of main_test.go to be
		// updated before moving other_test.go back to the main_test package.
		env.Await(
			Diagnostics(env.AtRegexp("other_test.go", "Server")),
			Diagnostics(env.AtRegexp("main_test.go", "otherConst")),
		)
		env.RegexpReplace("other_test.go", "main", "main_test")
		env.AfterChange(
			NoDiagnostics(ForFile("other_test.go")),
			NoDiagnostics(ForFile("main_test.go")),
		)

		// This will cause a test failure if other_test.go is not in any package.
		_ = env.FirstDefinition(env.RegexpSearch("other_test.go", "Server"))
	})
}

// Test for golang/go#48929.
func TestClearNonWorkspaceDiagnostics(t *testing.T) {
	const ws = `
-- a/go.mod --
module a

go 1.17
-- a/main.go --
package main

func main() {
   var V string
}
-- b/go.mod --
module b

go 1.17
-- b/main.go --
package b

import (
        _ "fmt"
)
`
	Run(t, ws, func(t *testing.T, env *Env) {
		env.OpenFile("b/main.go")
		env.AfterChange(
			NoDiagnostics(ForFile("a/main.go")),
		)
		env.OpenFile("a/main.go")
		env.AfterChange(
			Diagnostics(env.AtRegexp("a/main.go", "V"), WithMessage("not used")),
		)
		// Here, diagnostics are added because of zero-config gopls.
		// In the past, they were added simply due to diagnosing changed files.
		// (see TestClearNonWorkspaceDiagnostics_NoView below for a
		// reimplementation of that test).
		if got, want := len(env.Views()), 3; got != want {
			t.Errorf("after opening a/main.go, got %d views, want %d", got, want)
		}
		env.CloseBuffer("a/main.go")
		env.AfterChange(
			NoDiagnostics(ForFile("a/main.go")),
		)
		if got, want := len(env.Views()), 2; got != want {
			t.Errorf("after closing a/main.go, got %d views, want %d", got, want)
		}
	})
}

// This test is like TestClearNonWorkspaceDiagnostics, but bypasses the
// zero-config algorithm by opening a nested workspace folder.
//
// We should still compute diagnostics correctly for open packages.
func TestClearNonWorkspaceDiagnostics_NoView(t *testing.T) {
	const ws = `
-- a/go.mod --
module example.com/a

go 1.18

require example.com/b v1.2.3

replace example.com/b => ../b

-- a/a.go --
package a

import "example.com/b"

func _() {
	V := b.B // unused
}

-- b/go.mod --
module b

go 1.18

-- b/b.go --
package b

const B = 2

func _() {
	var V int // unused
}

-- b/b2.go --
package b

const B2 = B

-- c/c.go --
package main

func main() {
	var V int // unused
}
`
	WithOptions(
		WorkspaceFolders("a"),
	).Run(t, ws, func(t *testing.T, env *Env) {
		env.OpenFile("a/a.go")
		env.AfterChange(
			Diagnostics(env.AtRegexp("a/a.go", "V"), WithMessage("not used")),
			NoDiagnostics(ForFile("b/b.go")),
			NoDiagnostics(ForFile("c/c.go")),
		)
		env.OpenFile("b/b.go")
		env.AfterChange(
			Diagnostics(env.AtRegexp("a/a.go", "V"), WithMessage("not used")),
			Diagnostics(env.AtRegexp("b/b.go", "V"), WithMessage("not used")),
			NoDiagnostics(ForFile("c/c.go")),
		)

		// Opening b/b.go should not result in a new view, because b is not
		// contained in a workspace folder.
		//
		// Yet we should get diagnostics for b, because it is open.
		if got, want := len(env.Views()), 1; got != want {
			t.Errorf("after opening b/b.go, got %d views, want %d", got, want)
		}
		env.CloseBuffer("b/b.go")
		env.AfterChange(
			Diagnostics(env.AtRegexp("a/a.go", "V"), WithMessage("not used")),
			NoDiagnostics(ForFile("b/b.go")),
			NoDiagnostics(ForFile("c/c.go")),
		)

		// We should get references in the b package.
		bUse := env.RegexpSearch("a/a.go", `b\.(B)`)
		refs := env.References(bUse)
		wantRefs := []string{"a/a.go", "b/b.go", "b/b2.go"}
		var gotRefs []string
		for _, ref := range refs {
			gotRefs = append(gotRefs, env.Sandbox.Workdir.URIToPath(ref.URI))
		}
		sort.Strings(gotRefs)
		if diff := cmp.Diff(wantRefs, gotRefs); diff != "" {
			t.Errorf("references(b.B) mismatch (-want +got)\n%s", diff)
		}

		// Opening c/c.go should also not result in a new view, yet we should get
		// orphaned file diagnostics.
		env.OpenFile("c/c.go")
		env.AfterChange(
			Diagnostics(env.AtRegexp("a/a.go", "V"), WithMessage("not used")),
			NoDiagnostics(ForFile("b/b.go")),
			Diagnostics(env.AtRegexp("c/c.go", "V"), WithMessage("not used")),
		)
		if got, want := len(env.Views()), 1; got != want {
			t.Errorf("after opening b/b.go, got %d views, want %d", got, want)
		}

		env.CloseBuffer("c/c.go")
		env.AfterChange(
			Diagnostics(env.AtRegexp("a/a.go", "V"), WithMessage("not used")),
			NoDiagnostics(ForFile("b/b.go")),
			NoDiagnostics(ForFile("c/c.go")),
		)
		env.CloseBuffer("a/a.go")
		env.AfterChange(
			Diagnostics(env.AtRegexp("a/a.go", "V"), WithMessage("not used")),
			NoDiagnostics(ForFile("b/b.go")),
			NoDiagnostics(ForFile("c/c.go")),
		)
	})
}

// Test that we don't get a version warning when the Go version in PATH is
// supported.
func TestOldGoNotification_SupportedVersion(t *testing.T) {
	v := goVersion(t)
	if v < goversion.OldestSupported() {
		t.Skipf("go version 1.%d is unsupported", v)
	}

	Run(t, "", func(t *testing.T, env *Env) {
		env.OnceMet(
			InitialWorkspaceLoad,
			NoShownMessage("upgrade"),
		)
	})
}

// Test that we do get a version warning when the Go version in PATH is
// unsupported, though this test may never execute if we stop running CI at
// legacy Go versions (see also TestOldGoNotification_Fake)
func TestOldGoNotification_UnsupportedVersion(t *testing.T) {
	v := goVersion(t)
	if v >= goversion.OldestSupported() {
		t.Skipf("go version 1.%d is supported", v)
	}

	Run(t, "", func(t *testing.T, env *Env) {
		env.Await(
			// Note: cannot use OnceMet(InitialWorkspaceLoad, ...) here, as the
			// upgrade message may race with the IWL.
			ShownMessage("Please upgrade"),
		)
	})
}

func TestOldGoNotification_Fake(t *testing.T) {
	// Get the Go version from path, and make sure it's unsupported.
	//
	// In the future we'll stop running CI on legacy Go versions. By mutating the
	// oldest supported Go version here, we can at least ensure that the
	// ShowMessage pop-up works.
	ctx := context.Background()
	version, err := gocommand.GoVersion(ctx, gocommand.Invocation{}, &gocommand.Runner{})
	if err != nil {
		t.Fatal(err)
	}
	defer func(t []goversion.Support) {
		goversion.Supported = t
	}(goversion.Supported)
	goversion.Supported = []goversion.Support{
		{GoVersion: version, InstallGoplsVersion: "v1.0.0"},
	}

	Run(t, "", func(t *testing.T, env *Env) {
		env.Await(
			// Note: cannot use OnceMet(InitialWorkspaceLoad, ...) here, as the
			// upgrade message may race with the IWL.
			ShownMessage("Please upgrade"),
		)
	})
}

// goVersion returns the version of the Go command in PATH.
func goVersion(t *testing.T) int {
	t.Helper()
	ctx := context.Background()
	goversion, err := gocommand.GoVersion(ctx, gocommand.Invocation{}, &gocommand.Runner{})
	if err != nil {
		t.Fatal(err)
	}
	return goversion
}

func TestInitializeWithNonFileWorkspaceFolders(t *testing.T) {
	for _, tt := range []struct {
		name          string
		folders       []string
		wantViewRoots []string
	}{
		{
			name:          "real,virtual",
			folders:       []string{"modb", "virtual:///virtualpath"},
			wantViewRoots: []string{"./modb"},
		},
		{
			name:          "virtual,real",
			folders:       []string{"virtual:///virtualpath", "modb"},
			wantViewRoots: []string{"./modb"},
		},
		{
			name:          "real,virtual,real",
			folders:       []string{"moda/a", "virtual:///virtualpath", "modb"},
			wantViewRoots: []string{"./moda/a", "./modb"},
		},
		{
			name:          "virtual",
			folders:       []string{"virtual:///virtualpath"},
			wantViewRoots: nil,
		},
	} {

		t.Run(tt.name, func(t *testing.T) {
			opts := []RunOption{ProxyFiles(workspaceProxy), WorkspaceFolders(tt.folders...)}
			WithOptions(opts...).Run(t, multiModule, func(t *testing.T, env *Env) {
				summary := func(typ cache.ViewType, root, folder string) command.View {
					return command.View{
						Type:   typ.String(),
						Root:   env.Sandbox.Workdir.URI(root),
						Folder: env.Sandbox.Workdir.URI(folder),
					}
				}
				checkViews := func(want ...command.View) {
					got := env.Views()
					if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(command.View{}, "ID")); diff != "" {
						t.Errorf("SummarizeViews() mismatch (-want +got):\n%s", diff)
					}
				}
				var wantViews []command.View
				for _, root := range tt.wantViewRoots {
					wantViews = append(wantViews, summary(cache.GoModView, root, root))
				}
				env.Await(
					LogMatching(protocol.Warning, "skip adding virtual folder", 1, false),
				)
				checkViews(wantViews...)
			})
		})
	}
}

// TestChangeAddedWorkspaceFolders tests issue71967 which an editor sends the following requests.
//
//  1. send an initialization request with rootURI but no workspaceFolders,
//     which gopls helps to find a workspaceFolders for it.
//  2. send a DidChangeWorkspaceFolders request with the exact the same folder gopls helps to find.
//
// It uses the same approach to simulate the scenario, and ensure we can skip the already added file.
func TestChangeAddedWorkspaceFolders(t *testing.T) {
	for _, tt := range []struct {
		name          string
		after         []string
		wantViewRoots []string
	}{
		{
			name:          "add an already added file",
			after:         []string{"modb"},
			wantViewRoots: []string{"./modb"},
		},
		{
			name:          "add an already added file but with an ending slash",
			after:         []string{"modb/"},
			wantViewRoots: []string{"./modb"},
		},
		{
			name:          "add an already added file and a new file",
			after:         []string{"modb", "moda/a"},
			wantViewRoots: []string{"./modb", "moda/a"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := []RunOption{ProxyFiles(workspaceProxy), RootPath("modb"), NoDefaultWorkspaceFiles()}
			WithOptions(opts...).Run(t, multiModule, func(t *testing.T, env *Env) {
				summary := func(typ cache.ViewType, root, folder string) command.View {
					return command.View{
						Type:   typ.String(),
						Root:   env.Sandbox.Workdir.URI(root),
						Folder: env.Sandbox.Workdir.URI(folder),
					}
				}
				checkViews := func(want ...command.View) {
					got := env.Views()
					if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(command.View{}, "ID")); diff != "" {
						t.Errorf("SummarizeViews() mismatch (-want +got):\n%s", diff)
					}
				}
				var wantViews []command.View
				for _, root := range tt.wantViewRoots {
					wantViews = append(wantViews, summary(cache.GoModView, root, root))
				}
				env.ChangeWorkspaceFolders(tt.after...)
				env.Await(
					LogMatching(protocol.Warning, "skip adding the already added folder", 1, false),
					NoOutstandingWork(IgnoreTelemetryPromptWork),
				)
				checkViews(wantViews...)
			})
		})
	}
}

// Test that non-file scheme Document URIs in ChangeWorkspaceFolders
// notification does not produce errors.
func TestChangeNonFileWorkspaceFolders(t *testing.T) {
	for _, tt := range []struct {
		name          string
		before        []string
		after         []string
		wantViewRoots []string
	}{
		{
			name:          "add",
			before:        []string{"modb"},
			after:         []string{"modb", "moda/a", "virtual:///virtualpath"},
			wantViewRoots: []string{"./modb", "moda/a"},
		},
		{
			name:          "remove",
			before:        []string{"modb", "virtual:///virtualpath", "moda/a"},
			after:         []string{"modb"},
			wantViewRoots: []string{"./modb"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := []RunOption{ProxyFiles(workspaceProxy), WorkspaceFolders(tt.before...)}
			WithOptions(opts...).Run(t, multiModule, func(t *testing.T, env *Env) {
				summary := func(typ cache.ViewType, root, folder string) command.View {
					return command.View{
						Type:   typ.String(),
						Root:   env.Sandbox.Workdir.URI(root),
						Folder: env.Sandbox.Workdir.URI(folder),
					}
				}
				checkViews := func(want ...command.View) {
					got := env.Views()
					if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(command.View{}, "ID")); diff != "" {
						t.Errorf("SummarizeViews() mismatch (-want +got):\n%s", diff)
					}
				}
				var wantViews []command.View
				for _, root := range tt.wantViewRoots {
					wantViews = append(wantViews, summary(cache.GoModView, root, root))
				}
				env.ChangeWorkspaceFolders(tt.after...)
				env.Await(
					LogMatching(protocol.Warning, "skip adding virtual folder", 1, false),
					NoOutstandingWork(IgnoreTelemetryPromptWork),
				)
				checkViews(wantViews...)
			})
		})
	}
}
