// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

//go:build !android && !ios && (unix || aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || plan9 || windows)

package ssa_test

import (
	"bytes"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// TestBuildPackageDump checks the printed form of the package the example
// builds, and of the functions the builder synthesized for it.
func TestBuildPackageDump(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "hello.go", hello, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	pkg, _, err := ssautil.BuildPackage(
		&types.Config{Importer: importer.Default()}, fset, types.NewPackage("hello", ""), []*ast.File{f}, ssa.SanityCheckFunctions)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	pkg.WriteTo(&out)
	pkg.Func("init").WriteTo(&out)
	pkg.Func("main").WriteTo(&out)

	if got, want := out.String(), buildPackageDump; got != want {
		t.Errorf("package dump:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// buildPackageDump is the dump that TestBuildPackageDump compares against.
const buildPackageDump = `package hello:
  func  init       func()
  var   init$guard bool
  func  main       func()
  const message    message = "Hello, World!":untyped string

# Name: hello.init
# Package: hello
# Synthetic: package initializer
func init():
0:                                                                entry P:0 S:2
	t0 = *init$guard                                                   bool
	if t0 goto 2 else 1
1:                                                    init.start P:1 S:1 idom:0
	*init$guard = true:bool
	t1 = fmt.init()                                                      ()
	jump 2
2:                                                     init.done P:2 S:0 idom:0
	return

# Name: hello.main
# Package: hello
# Location: hello.go:8:6
func main():
0:                                                                entry P:0 S:0
	t0 = new [1]any (varargs)                                       *[1]any
	t1 = &t0[0:int]                                                    *any
	t2 = make any <- string ("Hello, World!":string)                    any
	*t1 = t2
	t3 = slice t0[:]                                                  []any
	t4 = fmt.Println(t3...)                              (n int, err error)
	return
`
