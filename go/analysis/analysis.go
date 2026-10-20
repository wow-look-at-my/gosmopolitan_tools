// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package analysis

import (
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"time"
)

// An Analyzer describes an analysis function and its options.
type Analyzer struct {
	// The Name of the analyzer must be a valid Go identifier as it may appear in command-line flags, URLs, and so on.
	Name string

	// Doc is the documentation for the analyzer.
	Doc string

	// URL holds an optional link to a web page with additional documentation for this analyzer.
	URL string

	// Flags defines any flags accepted by the analyzer.
	Flags flag.FlagSet

	// Run applies the analyzer to a package. It returns an error if the analyzer failed.
	Run func(*Pass) (any, error)

	// RunDespiteErrors allows the driver to invoke the Run method of this analyzer even on a package that contains parse or type errors.
	RunDespiteErrors bool

	// Requires is a set of analyzers that must run successfully before this on a given package.
	Requires []*Analyzer

	// ResultType is the type of the optional result of the Run function.
	ResultType reflect.Type

	// FactTypes indicates that this analyzer imports and exports Facts of the specified concrete types.
	FactTypes []Fact
}

func (a *Analyzer) String() string { return a.Name }

// A Pass provides information to the Run function that
// applies a specific analyzer to a single Go package.
//
// It forms the interface between the analysis logic and the driver
// program, and has both input and an output components.
//
// As in a compiler, one pass may depend on the result computed by another.
//
// The Run function should not call any of the Pass functions concurrently.
type Pass struct {
	Analyzer *Analyzer // the identity of the current analyzer

	// syntax and type information
	Fset         *token.FileSet // file position information; Run may add new files
	Files        []*ast.File    // the abstract syntax tree of each file
	OtherFiles   []string       // names of non-Go files of this package
	IgnoredFiles []string       // names of ignored source files in this package
	Pkg          *types.Package // type information about the package
	TypesInfo    *types.Info    // type information about the syntax trees
	TypesSizes   types.Sizes    // function for computing sizes of types
	TypeErrors   []types.Error  // type errors (only if Analyzer.RunDespiteErrors)

	Module *Module // the package's enclosing module (possibly nil in some drivers)

	// Report reports a Diagnostic.
	Report func(Diagnostic)

	// ResultOf provides the inputs to this analysis pass.
	ResultOf map[*Analyzer]any

	// ReadFile returns the contents of the named file.
	ReadFile func(filename string) ([]byte, error)

	// -- facts --

	// ImportObjectFact retrieves a fact associated with obj.
	ImportObjectFact func(obj types.Object, fact Fact) bool

	// ImportPackageFact retrieves a fact associated with package pkg.
	ImportPackageFact func(pkg *types.Package, fact Fact) bool

	// ExportObjectFact associates a fact of type *T with the obj, replacing any previous fact of that type.
	ExportObjectFact func(obj types.Object, fact Fact)

	// ExportPackageFact associates a fact with the current package. See comments for ExportObjectFact.
	ExportPackageFact func(fact Fact)

	// AllPackageFacts returns a new slice containing all package facts of the analysis's FactTypes in unspecified order.
	AllPackageFacts func() []PackageFact

	// AllObjectFacts returns a new slice containing all object facts of the analysis's FactTypes in unspecified order.
	AllObjectFacts func() []ObjectFact
}

// PackageFact is a package together with an associated fact.
type PackageFact struct {
	Package *types.Package
	Fact    Fact
}

// ObjectFact is an object together with an associated fact.
type ObjectFact struct {
	Object types.Object
	Fact   Fact
}

// Reportf is a helper function that reports a Diagnostic using the
// specified position and formatted error message.
func (pass *Pass) Reportf(pos token.Pos, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	pass.Report(Diagnostic{Pos: pos, Message: msg})
}

// The Range interface provides a range. It's equivalent to and satisfied by
// ast.Node.
type Range interface {
	Pos() token.Pos // position of first character belonging to the node
	End() token.Pos // position of first character immediately after the node
}

// ReportRangef is a helper function that reports a Diagnostic using the
// range provided. ast.Node values can be passed in as the range because
// they satisfy the Range interface.
func (pass *Pass) ReportRangef(rng Range, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	pass.Report(Diagnostic{Pos: rng.Pos(), End: rng.End(), Message: msg})
}

func (pass *Pass) String() string {
	return fmt.Sprintf("%s@%s", pass.Analyzer.Name, pass.Pkg.Path())
}

// A Fact is an intermediate fact produced during analysis.
type Fact interface {
	AFact() // dummy method to avoid type errors
}

// A Module describes the module to which a package belongs.
type Module struct {
	Path      string       // module path
	Version   string       // module version ("" if unknown, such as for workspace modules)
	Replace   *Module      // replaced by this module
	Time      *time.Time   // time version was created
	Main      bool         // is this the main module?
	Indirect  bool         // is this module only an indirect dependency of main module?
	Dir       string       // directory holding files for this module, if any
	GoMod     string       // path to go.mod file used when loading this module, if any
	GoVersion string       // go version used in module (e.g. "go1.22.0")
	Error     *ModuleError // error loading module
}

// ModuleError holds errors loading a module.
type ModuleError struct {
	Err string // the error itself
}
