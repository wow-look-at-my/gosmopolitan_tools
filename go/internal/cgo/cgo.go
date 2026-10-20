// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package cgo handles cgo preprocessing of files containing `import "C"`.
//
// DESIGN
//
// The approach taken is to run the cgo processor on the package's CgoFiles
// and parse the output. That approach is faking the filenames of the
// resulting ASTs so that the synthetic file containing the C types is
// called "C" (e.g. "~/go/src/net/C"). The preprocessed files have their
// original names (e.g. "~/go/src/net/cgo_unix.go"), not the names of the
// actual temporary files.

package cgo

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// ProcessFiles invokes the cgo preprocessor on bp.CgoFiles, parses
// the output and returns the resulting ASTs.
func ProcessFiles(bp *build.Package, fset *token.FileSet, DisplayPath func(path string) string, mode parser.Mode) ([]*ast.File, error) {
	tmpdir, err := os.MkdirTemp("", strings.Replace(bp.ImportPath, "/", "_", -1)+"_C")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpdir)

	pkgdir := bp.Dir
	if DisplayPath != nil {
		pkgdir = DisplayPath(pkgdir)
	}

	cgoFiles, cgoDisplayFiles, err := Run(bp, pkgdir, tmpdir, false)
	if err != nil {
		return nil, err
	}
	var files []*ast.File
	for i := range cgoFiles {
		rd, err := os.Open(cgoFiles[i])
		if err != nil {
			return nil, err
		}
		display := filepath.Join(bp.Dir, cgoDisplayFiles[i])
		f, err := parser.ParseFile(fset, display, rd, mode)
		rd.Close()
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

var cgoRe = regexp.MustCompile(`[/\\:]`)

// Run invokes the cgo preprocessor on bp.CgoFiles and returns lists of
// files. The resulting processed files (in temporary directory tmpdir) and
// the corresponding names of the unprocessed files.
//
// Run is adapted from (*builder).cgo in $GOROOT/src/cmd/go/build.go, but
// these features are unsupported: Objective C, CGOPKGPATH, CGO_FLAGS.
//
// If useabs is set to true, absolute paths of the bp.CgoFiles will be passed
// in to the cgo preprocessor. This in turn will set the // line comments
// referring to those files to use absolute paths. This is needed for
// go/packages using the go list support so it can find the original files.
func Run(bp *build.Package, pkgdir, tmpdir string, useabs bool) (files, displayFiles []string, err error) {
	cgoCPPFLAGS, _, _, _ := cflags(bp, true)
	_, cgoexeCFLAGS, _, _ := cflags(bp, false)

	if len(bp.CgoPkgConfig) > 0 {
		pcCFLAGS, err := pkgConfigFlags(bp)
		if err != nil {
			return nil, nil, err
		}
		cgoCPPFLAGS = append(cgoCPPFLAGS, pcCFLAGS...)
	}

	// Allows including _cgo_export.h from .[ch] files in the package.
	cgoCPPFLAGS = append(cgoCPPFLAGS, "-I", tmpdir)

	// _cgo_gotypes.go (displayed "C") contains the type definitions.
	files = append(files, filepath.Join(tmpdir, "_cgo_gotypes.go"))
	displayFiles = append(displayFiles, "C")
	for _, fn := range bp.CgoFiles {
		// "foo.cgo1.go" (displayed "foo.go") is the processed Go source.
		f := cgoRe.ReplaceAllString(fn[:len(fn)-len("go")], "_")
		files = append(files, filepath.Join(tmpdir, f+"cgo1.go"))
		displayFiles = append(displayFiles, fn)
	}

	var cgoflags []string
	if bp.Goroot && bp.ImportPath == "runtime/cgo" {
		cgoflags = append(cgoflags, "-import_runtime_cgo=false")
	}
	if bp.Goroot && bp.ImportPath == "runtime/race" || bp.ImportPath == "runtime/cgo" {
		cgoflags = append(cgoflags, "-import_syscall=false")
	}

	var cgoFiles []string = bp.CgoFiles
	if useabs {
		cgoFiles = make([]string, len(bp.CgoFiles))
		for i := range cgoFiles {
			cgoFiles[i] = filepath.Join(pkgdir, bp.CgoFiles[i])
		}
	}

	args := stringList(
		"go", "tool", "cgo", "-objdir", tmpdir, cgoflags, "--",
		cgoCPPFLAGS, cgoexeCFLAGS, cgoFiles,
	)
	if false {
		log.Printf("Running cgo for package %q: %s (dir=%s)", bp.ImportPath, args, pkgdir)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = pkgdir
	cmd.Env = append(os.Environ(), "PWD="+pkgdir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, nil, fmt.Errorf("cgo failed: %s: %s", args, err)
	}

	return files, displayFiles, nil
}

// -- unmodified from 'go build' ---------------------------------------

// Return the flags to use when invoking the C or C++ compilers, or cgo.
func cflags(p *build.Package, def bool) (cppflags, cflags, cxxflags, ldflags []string) {
	var defaults string
	if def {
		defaults = "-g -O2"
	}

	cppflags = stringList(envList("CGO_CPPFLAGS", ""), p.CgoCPPFLAGS)
	cflags = stringList(envList("CGO_CFLAGS", defaults), p.CgoCFLAGS)
	cxxflags = stringList(envList("CGO_CXXFLAGS", defaults), p.CgoCXXFLAGS)
	ldflags = stringList(envList("CGO_LDFLAGS", defaults), p.CgoLDFLAGS)
	return
}

// envList returns the value of the given environment variable broken
// into fields, using the default value when the variable is empty.
func envList(key, def string) []string {
	v := os.Getenv(key)
	if v == "" {
		v = def
	}
	return strings.Fields(v)
}

// stringList's arguments should be a sequence of string or []string values.
// stringList flattens them into a single []string.
func stringList(args ...any) []string {
	var x []string
	for _, arg := range args {
		switch arg := arg.(type) {
		case []string:
			x = append(x, arg...)
		case string:
			x = append(x, arg)
		default:
			panic("stringList: invalid argument")
		}
	}
	return x
}
