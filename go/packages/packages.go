// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package packages

// See doc.go for package documentation and implementation notes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"golang.org/x/tools/go/gcexportdata"
	"golang.org/x/tools/internal/gocommand"
	"golang.org/x/tools/internal/packagesinternal"
	"golang.org/x/tools/internal/typesinternal"
)

// A LoadMode controls the amount of detail to return when loading.
type LoadMode int

const (
	// NeedName adds Name and PkgPath.
	NeedName LoadMode = 1 << iota

	// NeedFiles adds Dir, GoFiles, OtherFiles, and IgnoredFiles
	NeedFiles

	// NeedCompiledGoFiles adds CompiledGoFiles.
	NeedCompiledGoFiles

	// NeedImports adds Imports.
	NeedImports

	// NeedDeps adds the fields requested by the LoadMode in the packages in Imports.
	NeedDeps

	// NeedExportFile adds ExportFile.
	NeedExportFile

	// NeedTypes adds Types, Fset, and IllTyped.
	NeedTypes

	// NeedSyntax adds Syntax and Fset.
	NeedSyntax

	// NeedTypesInfo adds TypesInfo and Fset.
	NeedTypesInfo

	// NeedTypesSizes adds TypesSizes.
	NeedTypesSizes

	// needInternalDepsErrors adds the internal deps errors field for use by gopls.
	needInternalDepsErrors

	// NeedForTest adds ForTest.
	NeedForTest

	// typecheckCgo enables full support for type checking cgo.
	typecheckCgo

	// NeedModule adds Module.
	NeedModule

	// NeedEmbedFiles adds EmbedFiles.
	NeedEmbedFiles

	// NeedEmbedPatterns adds EmbedPatterns.
	NeedEmbedPatterns

	// NeedTarget adds Target.
	NeedTarget
)

const (
	// LoadFiles loads the name and file names for the initial packages.
	LoadFiles = NeedName | NeedFiles | NeedCompiledGoFiles

	// LoadImports loads the name, file names, and import mapping for the initial packages.
	LoadImports = LoadFiles | NeedImports

	// LoadTypes loads exported type information for the initial packages.
	LoadTypes = LoadImports | NeedTypes | NeedTypesSizes

	// LoadSyntax loads typed syntax for the initial packages.
	LoadSyntax = LoadTypes | NeedSyntax | NeedTypesInfo

	// LoadAllSyntax loads typed syntax for the initial packages and all dependencies.
	LoadAllSyntax = LoadSyntax | NeedDeps

	//go:fix inline
	NeedExportsFile = NeedExportFile
)

// A Config specifies details about how packages should be loaded.
// The zero value is a valid configuration.
//
// Calls to [Load] do not modify this struct.
type Config struct {
	// Mode controls the level of information returned for each package.
	Mode LoadMode

	// Context specifies the context for the load operation.
	Context context.Context

	// Logf is the logger for the config. If the user provides a logger, debug logging is enabled.
	Logf func(format string, args ...any)

	// Dir is the directory in which to run the build system's query tool that provides information about the packages.
	Dir string

	// Env is the environment to use when invoking the build system's query tool.
	Env []string

	// BuildFlags is a list of command-line flags to be passed through to the build system's query tool.
	BuildFlags []string

	// Fset provides source position information for syntax trees and types.
	Fset *token.FileSet

	// ParseFile is called to read and parse each file when preparing a package's type-checked syntax tree.
	ParseFile func(fset *token.FileSet, filename string, src []byte) (*ast.File, error)

	// If Tests is set, the loader includes not the packages matching a particular pattern but also any related test packages.
	Tests bool

	// Overlay is a mapping from absolute file paths to file contents.
	Overlay map[string][]byte
}

// Load loads and returns the Go packages named by the given patterns.
//
// The cfg parameter specifies loading options; nil behaves the same as an empty [Config].
//
// The [Config.Mode] field is a set of bits that determine what kinds
// of information should be computed and returned. Modes that require
// more information tend to be slower. See [LoadMode] for details
// and important caveats. Its zero value is equivalent to
// [NeedName] | [NeedFiles] | [NeedCompiledGoFiles].
//
// Each call to Load returns a new set of [Package] instances.
// The Packages and their Imports form a directed acyclic graph.
func Load(cfg *Config, patterns ...string) ([]*Package, error) {
	ld := newLoader(cfg)
	response, external, err := defaultDriver(&ld.Config, patterns...)
	if err != nil {
		return nil, err
	}

	ld.sizes = types.SizesFor(response.Compiler, response.Arch)
	if ld.sizes == nil && ld.Config.Mode&(NeedTypes|NeedTypesSizes|NeedTypesInfo) != 0 {
		// Type size information is needed but unavailable.
		if external {
			// An external driver may fail to populate the Compiler/GOARCH fields, especially since they are relatively new.
			ld.sizes = types.SizesFor("gc", runtime.GOARCH)
			if ld.sizes == nil { // gccgo-only arch
				ld.sizes = types.SizesFor("gc", "amd64")
			}
		} else {
			// Go list should never fail to deliver accurate size information.
			// Reject the whole Load since the error is the same for every package.
			return nil, fmt.Errorf("can't determine type sizes for compiler %q on GOARCH %q",
				response.Compiler, response.Arch)
		}
	}

	ld.externalDriver = external

	return ld.refine(response)
}

// defaultDriver is a driver that implements go/packages' fallback behavior.
// It will try to request to an external driver, if one exists. If there's
// no external driver, or the driver returns a response with NotHandled set,
// defaultDriver will fall back to the go list driver.
// The boolean result indicates that an external driver handled the request.
func defaultDriver(cfg *Config, patterns ...string) (*DriverResponse, bool, error) {
	const (
		// windowsArgMax specifies the maximum command line length for the Windows' CreateProcess function.
		windowsArgMax = 32767
		// maxEnvSize is a rough estimation of the maximum environment size of a user.
		maxEnvSize = 16384
		// safeArgMax specifies the maximum safe command line length to use by the underlying driver excl. the environment.
		safeArgMax = windowsArgMax - maxEnvSize
	)
	chunks, err := splitIntoChunks(patterns, safeArgMax)
	if err != nil {
		return nil, false, err
	}

	if driver := findExternalDriver(cfg); driver != nil {
		response, err := callDriverOnChunks(driver, cfg, chunks)
		if err != nil {
			return nil, false, err
		} else if !response.NotHandled {
			return response, true, nil
		}
	}

	// go list fallback

	// Write overlays once, as there are many calls to 'go list' (one per chunk plus others too).
	overlayFile, cleanupOverlay, err := gocommand.WriteOverlays(cfg.Overlay)
	if err != nil {
		return nil, false, err
	}
	defer cleanupOverlay()

	var runner gocommand.Runner // (shared across many 'go list' calls)
	driver := func(cfg *Config, patterns []string) (*DriverResponse, error) {
		return goListDriver(cfg, &runner, overlayFile, patterns)
	}
	response, err := callDriverOnChunks(driver, cfg, chunks)
	if err != nil {
		return nil, false, err
	}
	return response, false, err
}

// splitIntoChunks chunks the slice so that the total number of characters
// in a chunk is no longer than argMax.
func splitIntoChunks(patterns []string, argMax int) ([][]string, error) {
	if argMax <= 0 {
		return nil, errors.New("failed to split patterns into chunks, negative safe argMax value")
	}
	var chunks [][]string
	charsInChunk := 0
	nextChunkStart := 0
	for i, v := range patterns {
		vChars := len(v)
		if vChars > argMax {
			// a single pattern is longer than the maximum safe ARG_MAX, hardly should happen
			return nil, errors.New("failed to split patterns into chunks, a pattern is too long")
		}
		charsInChunk += vChars + 1 // +1 is for a whitespace between patterns that has to be counted too
		if charsInChunk > argMax {
			chunks = append(chunks, patterns[nextChunkStart:i])
			nextChunkStart = i
			charsInChunk = vChars
		}
	}
	// add the last chunk
	if nextChunkStart < len(patterns) {
		chunks = append(chunks, patterns[nextChunkStart:])
	}
	return chunks, nil
}

func callDriverOnChunks(driver driver, cfg *Config, chunks [][]string) (*DriverResponse, error) {
	if len(chunks) == 0 {
		return driver(cfg, nil)
	}
	responses := make([]*DriverResponse, len(chunks))
	errNotHandled := errors.New("driver returned NotHandled")
	var g errgroup.Group
	for i, chunk := range chunks {
		g.Go(func() (err error) {
			responses[i], err = driver(cfg, chunk)
			if responses[i] != nil && responses[i].NotHandled {
				err = errNotHandled
			}
			return err
		})
	}
	if err := g.Wait(); err != nil {
		if errors.Is(err, errNotHandled) {
			return &DriverResponse{NotHandled: true}, nil
		}
		return nil, err
	}
	return mergeResponses(responses...), nil
}

func mergeResponses(responses ...*DriverResponse) *DriverResponse {
	if len(responses) == 0 {
		return nil
	}
	// No dedup needed
	if len(responses) == 1 {
		return responses[0]
	}
	response := newDeduper()
	response.dr.NotHandled = false
	response.dr.Compiler = responses[0].Compiler
	response.dr.Arch = responses[0].Arch
	response.dr.GoVersion = responses[0].GoVersion
	for _, v := range responses {
		response.addAll(v)
	}
	return response.dr
}

// A Package describes a loaded Go package.
//
// It also defines part of the JSON schema of [DriverResponse].
// See the package documentation for an overview.
type Package struct {
	// ID is a unique identifier for a package, in a syntax provided by the underlying build system.
	ID string

	// Name is the package name as it appears in the package source code.
	Name string

	// PkgPath is the package path as used by the go/types package.
	PkgPath string

	// Dir is the directory associated with the package, if it exists.
	Dir string

	// Errors contains any errors encountered querying the metadata of the package.
	Errors []Error

	// TypeErrors contains the subset of errors produced during type checking.
	TypeErrors []types.Error

	// GoFiles lists the absolute file paths of the package's Go source files.
	GoFiles []string

	// CompiledGoFiles lists the absolute file paths of the package's source files that are suitable for type checking.
	CompiledGoFiles []string

	// OtherFiles lists the absolute file paths of the package's non-Go source files, including assembly, C, C++, Fortran, Objective-C, SWIG.
	OtherFiles []string

	// EmbedFiles lists the absolute file paths of the package's files embedded with go:embed.
	EmbedFiles []string

	// EmbedPatterns lists the absolute file patterns of the package's files embedded with go:embed.
	EmbedPatterns []string

	// IgnoredFiles lists source files that are not part of the package using the current build configuration.
	IgnoredFiles []string

	// ExportFile is the absolute path to a file containing type information for the package as provided by the build system.
	ExportFile string

	// Target is the absolute install path of the .a file, for libraries, and of the executable file, for binaries.
	Target string

	// Imports maps import paths appearing in the package's Go source files to corresponding loaded Packages.
	Imports map[string]*Package

	// Module is the module information for the package if it exists.
	Module *Module

	// -- The following fields are not part of the driver JSON schema. --

	// Types provides type information for the package.
	Types *types.Package `json:"-"`

	// Fset provides position information for Types, TypesInfo, and Syntax. It is set only when Types is set.
	Fset *token.FileSet `json:"-"`

	// IllTyped indicates whether the package or any dependency contains errors. It is set only when Types is set.
	IllTyped bool `json:"-"`

	// Syntax is the package's syntax trees, for the files listed in CompiledGoFiles.
	Syntax []*ast.File `json:"-"`

	// TypesInfo provides type information about the package's syntax trees. It is set only when Syntax is set.
	TypesInfo *types.Info `json:"-"`

	// TypesSizes provides the effective size function for types in TypesInfo.
	TypesSizes types.Sizes `json:"-"`

	// -- internal --

	// ForTest is the package under test, if any.
	ForTest string

	// depsErrors is the DepsErrors field from the go list response, if any.
	depsErrors []*packagesinternal.PackageError

	// exportDataError is the error encountered reading export data, if any.
	exportDataError error
}

// Module provides module information for a package.
//
// It also defines part of the JSON schema of [DriverResponse]. See the package documentation for an overview.
type Module struct {
	Path      string       // module path
	Version   string       // module version
	Replace   *Module      // replaced by this module
	Time      *time.Time   // time version was created
	Main      bool         // is this the main module?
	Indirect  bool         // is this module only an indirect dependency of main module?
	Dir       string       // directory holding files for this module, if any
	GoMod     string       // path to go.mod file used when loading this module, if any
	GoVersion string       // go version used in module
	Error     *ModuleError // error loading module
}

// ModuleError holds errors loading a module.
type ModuleError struct {
	Err string // the error itself
}

func init() {
	packagesinternal.GetDepsErrors = func(p any) []*packagesinternal.PackageError {
		return p.(*Package).depsErrors
	}
	packagesinternal.TypecheckCgo = int(typecheckCgo)
	packagesinternal.DepsErrors = int(needInternalDepsErrors)
}

// An Error describes a problem with a package's metadata, syntax, or types.
type Error struct {
	Pos  string // "file:line:col" or "file:line" or "" or "-"
	Msg  string
	Kind ErrorKind
}

// ErrorKind describes the source of the error, allowing the user to differentiate between errors generated by the driver, the parser.
type ErrorKind int

const (
	UnknownError ErrorKind = iota
	ListError
	ParseError
	TypeError
)

func (err Error) Error() string {
	pos := err.Pos
	if pos == "" {
		pos = "-" // like token.Position{}.String()
	}
	return pos + ": " + err.Msg
}

// flatPackage is the JSON form of Package
// It drops all the type and syntax fields, and transforms the Imports
//
// TODO(adonovan): identify this struct with Package, effectively
// publishing the JSON protocol.
type flatPackage struct {
	ID              string
	Name            string            `json:",omitempty"`
	PkgPath         string            `json:",omitempty"`
	Errors          []Error           `json:",omitempty"`
	GoFiles         []string          `json:",omitempty"`
	CompiledGoFiles []string          `json:",omitempty"`
	OtherFiles      []string          `json:",omitempty"`
	EmbedFiles      []string          `json:",omitempty"`
	EmbedPatterns   []string          `json:",omitempty"`
	IgnoredFiles    []string          `json:",omitempty"`
	ExportFile      string            `json:",omitempty"`
	Imports         map[string]string `json:",omitempty"`
}

// MarshalJSON returns the Package in its JSON form. For the most part, the
// structure fields are written out unmodified, and the type and syntax fields
// are skipped. The imports are written out as a map of path to package id.
// The errors are written using a custom type that tries to preserve the
// structure of error types we know about.
//
// This method exists to enable support for additional build systems. It is
// not intended for use by clients of the API and we may change the format.
func (p *Package) MarshalJSON() ([]byte, error) {
	flat := &flatPackage{
		ID:              p.ID,
		Name:            p.Name,
		PkgPath:         p.PkgPath,
		Errors:          p.Errors,
		GoFiles:         p.GoFiles,
		CompiledGoFiles: p.CompiledGoFiles,
		OtherFiles:      p.OtherFiles,
		EmbedFiles:      p.EmbedFiles,
		EmbedPatterns:   p.EmbedPatterns,
		IgnoredFiles:    p.IgnoredFiles,
		ExportFile:      p.ExportFile,
	}
	if len(p.Imports) > 0 {
		flat.Imports = make(map[string]string, len(p.Imports))
		for path, ipkg := range p.Imports {
			flat.Imports[path] = ipkg.ID
		}
	}
	return json.Marshal(flat)
}

// UnmarshalJSON reads in a Package from its JSON format.
// See MarshalJSON for details about the format accepted.
func (p *Package) UnmarshalJSON(b []byte) error {
	flat := &flatPackage{}
	if err := json.Unmarshal(b, &flat); err != nil {
		return err
	}
	*p = Package{
		ID:              flat.ID,
		Name:            flat.Name,
		PkgPath:         flat.PkgPath,
		Errors:          flat.Errors,
		GoFiles:         flat.GoFiles,
		CompiledGoFiles: flat.CompiledGoFiles,
		OtherFiles:      flat.OtherFiles,
		EmbedFiles:      flat.EmbedFiles,
		EmbedPatterns:   flat.EmbedPatterns,
		IgnoredFiles:    flat.IgnoredFiles,
		ExportFile:      flat.ExportFile,
	}
	if len(flat.Imports) > 0 {
		p.Imports = make(map[string]*Package, len(flat.Imports))
		for path, id := range flat.Imports {
			p.Imports[path] = &Package{ID: id}
		}
	}
	return nil
}

func (p *Package) String() string { return p.ID }

// loaderPackage augments Package with state used during the loading phase
type loaderPackage struct {
	*Package
	importErrors    map[string]error // maps each bad import to its error
	preds           []*loaderPackage // packages that import this one
	unfinishedSuccs atomic.Int32     // number of direct imports not yet loaded
	color           uint8            // for cycle detection
	needsrc         bool             // load from source (Mode >= LoadTypes)
	needtypes       bool             // type information is either requested or depended.
	initial         bool             // package was matched by a pattern
	goVersion       int              // minor version number of go command on PATH
}

// loader holds the working state of a single call to load.
type loader struct {
	pkgs map[string]*loaderPackage // keyed by Package.ID
	Config
	sizes          types.Sizes // non-nil if needed by mode
	parseCache     map[string]*parseValue
	parseCacheMu   sync.Mutex
	exportMu       sync.Mutex // enforces mutual exclusion of exportdata operations
	externalDriver bool       // true if an external GOPACKAGESDRIVER handled the request

	// Config.Mode contains the implied mode (see impliedLoadMode).
	requestedMode LoadMode
}

type parseValue struct {
	f     *ast.File
	err   error
	ready chan struct{}
}

func newLoader(cfg *Config) *loader {
	ld := &loader{
		parseCache: map[string]*parseValue{},
	}
	if cfg != nil {
		ld.Config = *cfg
		// If the user has provided a logger, use it.
		ld.Config.Logf = cfg.Logf
	}
	if ld.Config.Logf == nil {
		// If the GOPACKAGESDEBUG environment variable is set to true,
		// but the user has not provided a logger, default to log.Printf.
		if debug {
			ld.Config.Logf = log.Printf
		} else {
			ld.Config.Logf = func(format string, args ...any) {}
		}
	}
	if ld.Config.Mode == 0 {
		ld.Config.Mode = NeedName | NeedFiles | NeedCompiledGoFiles // Preserve zero behavior of Mode for backwards compatibility.
	}
	if ld.Config.Env == nil {
		ld.Config.Env = os.Environ()
	}
	if ld.Context == nil {
		ld.Context = context.Background()
	}
	if ld.Dir == "" {
		if dir, err := os.Getwd(); err == nil {
			ld.Dir = dir
		}
	}

	// Save the requested fields. We'll zero them out before returning packages to the user.
	ld.requestedMode = ld.Mode
	ld.Mode = impliedLoadMode(ld.Mode)

	if ld.Mode&(NeedSyntax|NeedTypes|NeedTypesInfo) != 0 {
		if ld.Fset == nil {
			ld.Fset = token.NewFileSet()
		}

		// ParseFile is required even in LoadTypes mode
		// because we load source if export data is missing.
		if ld.ParseFile == nil {
			ld.ParseFile = func(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
				// We implicitly promise to keep doing ast.Object resolution. :(
				const mode = parser.AllErrors | parser.ParseComments
				return parser.ParseFile(fset, filename, src, mode)
			}
		}
	}

	return ld
}

// refine connects the supplied packages into a graph and then adds type and
// syntax information by the LoadMode.
func (ld *loader) refine(response *DriverResponse) ([]*Package, error) {
	roots := response.Roots
	rootMap := make(map[string]int, len(roots))
	for i, root := range roots {
		rootMap[root] = i
	}
	ld.pkgs = make(map[string]*loaderPackage)
	// first pass, fixup and build the map and roots
	var initial = make([]*loaderPackage, len(roots))
	for _, pkg := range response.Packages {
		rootIndex := -1
		if i, found := rootMap[pkg.ID]; found {
			rootIndex = i
		}

		// Overlays can invalidate export data.
		exportDataInvalid := len(ld.Overlay) > 0 || pkg.ExportFile == "" && pkg.PkgPath != "unsafe"
		// This package needs type information if the caller requested types and the package is either a root.
		needtypes := (ld.Mode&(NeedTypes|NeedTypesInfo) != 0 && (rootIndex >= 0 || ld.Mode&NeedDeps != 0))
		// This package needs source if the call requested source (or types info, which implies source) and the package is either a root, or itas a non- root....
		needsrc := ((ld.Mode&(NeedSyntax|NeedTypesInfo) != 0 && (rootIndex >= 0 || ld.Mode&NeedDeps != 0)) ||
			// ... or if we need types and the exportData is invalid.
			(ld.Mode&(NeedTypes|NeedTypesInfo) != 0 && exportDataInvalid)) && pkg.PkgPath != "unsafe"
		lpkg := &loaderPackage{
			Package:   pkg,
			needtypes: needtypes,
			needsrc:   needsrc,
			goVersion: response.GoVersion,
		}
		// Don't trust the driver to respond with duplicate-free
		// package names (go.dev/issue/63822).
		if _, ok := ld.pkgs[lpkg.ID]; ok {
			return nil, fmt.Errorf("%s response contained duplicate packages for ID %q",
				cond(ld.externalDriver, "go/packages driver", "go list"), lpkg.ID)
		}
		ld.pkgs[lpkg.ID] = lpkg
		if rootIndex >= 0 {
			initial[rootIndex] = lpkg
			lpkg.initial = true
		}
	}
	for i, root := range roots {
		if initial[i] == nil {
			return nil, fmt.Errorf("root package %v is missing", root)
		}
	}

	// Materialize the import graph if it is needed (NeedImports), or if we'll be using loadPackages.
	var leaves []*loaderPackage // packages with no unfinished successors
	if ld.Mode&(NeedImports|NeedSyntax|NeedTypes|NeedTypesInfo) != 0 {
		const (
			white = 0 // new
			grey  = 1 // in progress
			black = 2 // complete
		)

		// visit traverses the import graph, depth-first, and materializes the graph as Packages.Imports.
		var stack []*loaderPackage
		var visit func(from, lpkg *loaderPackage) bool
		visit = func(from, lpkg *loaderPackage) bool {
			if lpkg.color == grey {
				panic("internal error: grey node")
			}
			if lpkg.color == white {
				lpkg.color = grey
				stack = append(stack, lpkg) // push
				stubs := lpkg.Imports       // the structure form has only stubs with the ID in the Imports
				lpkg.Imports = make(map[string]*Package, len(stubs))
				for importPath, ipkg := range stubs {
					var importErr error
					imp := ld.pkgs[ipkg.ID]
					if imp == nil {
						// (includes package "C" when DisableCgo)
						importErr = fmt.Errorf("missing package: %q", ipkg.ID)
					} else if imp.color == grey {
						importErr = fmt.Errorf("import cycle: %s", stack)
					}
					if importErr != nil {
						if lpkg.importErrors == nil {
							lpkg.importErrors = make(map[string]error)
						}
						lpkg.importErrors[importPath] = importErr
						continue
					}

					if visit(lpkg, imp) {
						lpkg.needsrc = true
					}
					lpkg.Imports[importPath] = imp.Package
				}

				// -- postorder --

				// Complete type information is required for the
				// immediate dependencies of each source package.
				if lpkg.needsrc && ld.Mode&NeedTypes != 0 {
					for _, ipkg := range lpkg.Imports {
						ld.pkgs[ipkg.ID].needtypes = true
					}
				}

				// NeedTypeSizes causes TypeSizes to be set even
				// on packages for which types aren't needed.
				if ld.Mode&NeedTypesSizes != 0 {
					lpkg.TypesSizes = ld.sizes
				}

				// Add packages with no imports directly to the queue of leaves.
				if len(lpkg.Imports) == 0 {
					leaves = append(leaves, lpkg)
				}

				stack = stack[:len(stack)-1] // pop
				lpkg.color = black
			}

			// Add edge from predecessor.
			if from != nil {
				from.unfinishedSuccs.Add(+1) // incref
				lpkg.preds = append(lpkg.preds, from)
			}

			return lpkg.needsrc
		}

		// For each initial package, create its import DAG.
		for _, lpkg := range initial {
			visit(nil, lpkg)
		}

	} else {
		// !NeedImports: drop the stub (ID-only) import packages
		// that we are not even going to try to resolve.
		for _, lpkg := range initial {
			lpkg.Imports = nil
		}
	}

	// Load type data and syntax if needed, starting at
	// the initial packages (roots of the import DAG).
	if ld.Mode&(NeedSyntax|NeedTypes|NeedTypesInfo) != 0 {

		// We avoid using g.SetLimit to limit concurrency as it makes g.Go stop accepting work, which prevents workers from enqeuing, and thus finishing.
		g, _ := errgroup.WithContext(ld.Context)

		// enqueues adds a package to the type-checking queue. It must have no unfinished successors.
		var enqueue func(*loaderPackage)
		enqueue = func(lpkg *loaderPackage) {
			g.Go(func() error {
				// Parse and type-check.
				ld.loadPackage(lpkg)

				// Notify each waiting predecessor, and enqueue it when it becomes a leaf.
				for _, pred := range lpkg.preds {
					if pred.unfinishedSuccs.Add(-1) == 0 { // decref
						enqueue(pred)
					}
				}

				return nil
			})
		}

		// Load leaves first, adding new packages
		// to the queue as they become leaves.
		for _, leaf := range leaves {
			enqueue(leaf)
		}

		if err := g.Wait(); err != nil {
			return nil, err // cancelled
		}
	}

	// If the context is done, return its error and
	// throw out [likely] incomplete packages.
	if err := ld.Context.Err(); err != nil {
		return nil, err
	}

	result := make([]*Package, len(initial))
	for i, lpkg := range initial {
		result[i] = lpkg.Package
	}
	for i := range ld.pkgs {
		// Clear all unrequested fields,
		// to catch programs that use more than they request.
		if ld.requestedMode&NeedName == 0 {
			ld.pkgs[i].Name = ""
			ld.pkgs[i].PkgPath = ""
		}
		if ld.requestedMode&NeedFiles == 0 {
			ld.pkgs[i].GoFiles = nil
			ld.pkgs[i].OtherFiles = nil
			ld.pkgs[i].IgnoredFiles = nil
		}
		if ld.requestedMode&NeedEmbedFiles == 0 {
			ld.pkgs[i].EmbedFiles = nil
		}
		if ld.requestedMode&NeedEmbedPatterns == 0 {
			ld.pkgs[i].EmbedPatterns = nil
		}
		if ld.requestedMode&NeedCompiledGoFiles == 0 {
			ld.pkgs[i].CompiledGoFiles = nil
		}
		if ld.requestedMode&NeedImports == 0 {
			ld.pkgs[i].Imports = nil
		}
		if ld.requestedMode&NeedExportFile == 0 {
			ld.pkgs[i].ExportFile = ""
		}
		if ld.requestedMode&NeedTypes == 0 {
			ld.pkgs[i].Types = nil
			ld.pkgs[i].IllTyped = false
		}
		if ld.requestedMode&NeedSyntax == 0 {
			ld.pkgs[i].Syntax = nil
		}
		if ld.requestedMode&(NeedSyntax|NeedTypes|NeedTypesInfo) == 0 {
			ld.pkgs[i].Fset = nil
		}
		if ld.requestedMode&NeedTypesInfo == 0 {
			ld.pkgs[i].TypesInfo = nil
		}
		if ld.requestedMode&NeedTypesSizes == 0 {
			ld.pkgs[i].TypesSizes = nil
		}
		if ld.requestedMode&NeedModule == 0 {
			ld.pkgs[i].Module = nil
		}
	}

	return result, nil
}

// loadPackage loads/parses/typechecks the specified package. It must be
// called only once per Package, after immediate dependencies are loaded.
func (ld *loader) loadPackage(lpkg *loaderPackage) {
	if lpkg.PkgPath == "unsafe" {
		// To avoid surprises, fill in the blanks consistent with other packages.
		lpkg.Types = types.Unsafe
		lpkg.Fset = ld.Fset
		lpkg.Syntax = []*ast.File{}
		lpkg.TypesInfo = ld.newTypesInfo()
		lpkg.TypesSizes = ld.sizes
		return
	}

	// Call NewPackage directly with explicit name.
	lpkg.Types = types.NewPackage(lpkg.PkgPath, lpkg.Name)
	lpkg.Fset = ld.Fset

	// Start shutting down if the context is done and do not load source or
	// export data files. = nil.
	if ld.Context.Err() != nil {
		return
	}

	// Subtle: we populate all Types fields with an empty Package before loading
	// export data so that export data.
	if !lpkg.needtypes && !lpkg.needsrc {
		return
	}

	// TODO(adonovan): this condition looks wrong: I think it should be
	// lpkg.needtypes && !lpkg.needsrc. That NeedSyntax without NeedTypes
	// can be satisfied by export data.
	if !lpkg.needsrc {
		if err := ld.loadFromExportData(lpkg); err != nil {
			lpkg.exportDataError = err
			lpkg.Errors = append(lpkg.Errors, Error{
				Pos:  "-",
				Msg:  err.Error(),
				Kind: UnknownError, // e.g. can't find/open/parse export data
			})
		}
		return // not a source package, don't get syntax trees
	}

	appendError := func(err error) {
		// Convert various error types into the true Error.
		var errs []Error
		switch err := err.(type) {
		case Error:
			// from driver
			errs = append(errs, err)

		case *os.PathError:
			// from parser
			errs = append(errs, Error{
				Pos:  err.Path + ":1",
				Msg:  err.Err.Error(),
				Kind: ParseError,
			})

		case scanner.ErrorList:
			// from parser
			for _, err := range err {
				errs = append(errs, Error{
					Pos:  err.Pos.String(),
					Msg:  err.Msg,
					Kind: ParseError,
				})
			}

		case types.Error:
			// from type checker
			lpkg.TypeErrors = append(lpkg.TypeErrors, err)
			errs = append(errs, Error{
				Pos:  err.Fset.Position(err.Pos).String(),
				Msg:  err.Msg,
				Kind: TypeError,
			})

		default:
			// unexpected impoverished error from parser?
			errs = append(errs, Error{
				Pos:  "-",
				Msg:  err.Error(),
				Kind: UnknownError,
			})

			// If you see this error message, please file a bug.
			log.Printf("internal error: error %q (%T) without position", err, err)
		}

		lpkg.Errors = append(lpkg.Errors, errs...)
	}

	// Then the go/{scanner,ast,parser,types} packages from the standard library may be unable to process the files selected by go list.
	var runtimeVersion int
	if _, err := fmt.Sscanf(runtime.Version(), "go1.%d", &runtimeVersion); err == nil && runtimeVersion < lpkg.goVersion {
		defer func() {
			if len(lpkg.Errors) > 0 {
				appendError(Error{
					Pos:  "-",
					Msg:  fmt.Sprintf("This application uses version go1.%d of the source-processing packages but runs version go1.%d of 'go list'. It may fail to process source files that rely on newer language features. If so, rebuild the application using a newer version of Go.", runtimeVersion, lpkg.goVersion),
					Kind: UnknownError,
				})
			}
		}()
	}

	if ld.Config.Mode&NeedTypes != 0 && len(lpkg.CompiledGoFiles) == 0 && lpkg.ExportFile != "" {
		// The config requested loading sources and types, but sources are missing.
		appendError(Error{"-", fmt.Sprintf("sources missing for package %s", lpkg.ID), ParseError})
		_ = ld.loadFromExportData(lpkg) // ignore any secondary errors

		return // can't get syntax trees for this package
	}

	files, errs := ld.parseFiles(lpkg.CompiledGoFiles)
	for _, err := range errs {
		appendError(err)
	}

	lpkg.Syntax = files
	if ld.Config.Mode&(NeedTypes|NeedTypesInfo) == 0 {
		return
	}

	// Start shutting down if the context is done and do not type check. = nil.
	if ld.Context.Err() != nil {
		return
	}

	lpkg.TypesInfo = ld.newTypesInfo()
	lpkg.TypesSizes = ld.sizes

	importer := importerFunc(func(path string) (*types.Package, error) {
		if path == "unsafe" {
			return types.Unsafe, nil
		}

		// The imports map is keyed by import path.
		ipkg := lpkg.Imports[path]
		if ipkg == nil {
			if err := lpkg.importErrors[path]; err != nil {
				return nil, err
			}
			// There was skew between the metadata and the import declarations, likely due to an edit race.
			return nil, fmt.Errorf("no metadata for %s", path)
		}

		if ipkg.Types != nil && ipkg.Types.Complete() {
			return ipkg.Types, nil
		}

		// If types are unavailable, there must be an export data error.
		if ipkg.exportDataError != nil {
			return nil, ipkg.exportDataError
		}

		log.Fatalf("internal error: expected complete types for package %q", path)
		panic("unreachable")
	})

	// type-check
	tc := &types.Config{
		Importer: importer,

		// Type-check bodies of functions only in initial packages.
		IgnoreFuncBodies: ld.Mode&NeedDeps == 0 && !lpkg.initial,

		Error: appendError,
		Sizes: ld.sizes, // may be nil
	}
	if lpkg.Module != nil && lpkg.Module.GoVersion != "" {
		tc.GoVersion = "go" + lpkg.Module.GoVersion
	} else if ld.externalDriver && lpkg.goVersion != 0 {
		// Module information is missing when GOPACKAGESDRIVER is used, so use the go version from the driver response.
		tc.GoVersion = fmt.Sprintf("go1.%d", lpkg.goVersion)
	}
	if (ld.Mode & typecheckCgo) != 0 {
		if !typesinternal.SetUsesCgo(tc) {
			appendError(Error{
				Msg:  "typecheckCgo requires Go 1.15+",
				Kind: ListError,
			})
			return
		}
	}

	// Type-checking is CPU intensive.
	cpuLimit <- unit{}            // acquire a token
	defer func() { <-cpuLimit }() // release a token

	typErr := types.NewChecker(tc, ld.Fset, lpkg.Types, lpkg.TypesInfo).Files(lpkg.Syntax)
	lpkg.importErrors = nil // no longer needed

	// We rely on the runtimeVersion error to give the suggested remedy.
	if typErr != nil && len(lpkg.Errors) == 0 && len(lpkg.Syntax) > 0 {
		if msg := typErr.Error(); strings.HasPrefix(msg, "package requires newer Go version") {
			appendError(types.Error{
				Fset: ld.Fset,
				Pos:  lpkg.Syntax[0].Package,
				Msg:  msg,
			})
		}
	}

	// If !Cgo, the type-checker uses FakeImportC mode. It doesn't invoke the
	// importer for import "C", nor report an error for the import, or for any
	// undefined C.f reference.
	if tc.FakeImportC {
	outer:
		for _, f := range lpkg.Syntax {
			for _, imp := range f.Imports {
				if imp.Path.Value == `"C"` {
					err := types.Error{Fset: ld.Fset, Pos: imp.Pos(), Msg: `import "C" ignored`}
					appendError(err)
					break outer
				}
			}
		}
	}

	// If types.Checker.Files had an error that was unreported,
	// make sure to report the unknown error so the package is illTyped.
	if typErr != nil && len(lpkg.Errors) == 0 {
		appendError(typErr)
	}

	// Record accumulated errors.
	illTyped := len(lpkg.Errors) > 0
	if !illTyped {
		for _, imp := range lpkg.Imports {
			if imp.IllTyped {
				illTyped = true
				break
			}
		}
	}
	lpkg.IllTyped = illTyped
}

func (ld *loader) newTypesInfo() *types.Info {
	// Populate TypesInfo only if needed, as it
	// causes the type checker to work much harder.
	if ld.Config.Mode&NeedTypesInfo == 0 {
		return nil
	}
	return &types.Info{
		Types:        make(map[ast.Expr]types.TypeAndValue),
		Defs:         make(map[*ast.Ident]types.Object),
		Uses:         make(map[*ast.Ident]types.Object),
		Implicits:    make(map[ast.Node]types.Object),
		Instances:    make(map[*ast.Ident]types.Instance),
		Scopes:       make(map[ast.Node]*types.Scope),
		Selections:   make(map[*ast.SelectorExpr]*types.Selection),
		FileVersions: make(map[*ast.File]string),
	}
}

// An importFunc is an implementation of the single-method types.Importer interface based on a function value.
type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// We use a counting semaphore to limit
// the number of parallel I/O calls or CPU threads per process.
var (
	ioLimit  = make(chan unit, 20)
	cpuLimit = make(chan unit, runtime.GOMAXPROCS(0))
)

func (ld *loader) parseFile(filename string) (*ast.File, error) {
	ld.parseCacheMu.Lock()
	v, ok := ld.parseCache[filename]
	if ok {
		// cache hit
		ld.parseCacheMu.Unlock()
		<-v.ready
	} else {
		// cache miss
		v = &parseValue{ready: make(chan struct{})}
		ld.parseCache[filename] = v
		ld.parseCacheMu.Unlock()

		var src []byte
		for f, contents := range ld.Config.Overlay {
			// TODO(adonovan): Inefficient for large overlays.
			if sameFile(f, filename) {
				src = contents
				break
			}
		}
		var err error
		if src == nil {
			ioLimit <- unit{} // acquire a token
			src, err = os.ReadFile(filename)
			<-ioLimit // release a token
		}
		if err != nil {
			v.err = err
		} else {
			// Parsing is CPU intensive.
			cpuLimit <- unit{} // acquire a token
			v.f, v.err = ld.ParseFile(ld.Fset, filename, src)
			<-cpuLimit // release a token
		}

		close(v.ready)
	}
	return v.f, v.err
}

// parseFiles reads and parses the Go source files. It returns the ASTs
// of the ones that could be at least partially parsed, along with a
// list of I/O and parse errors encountered.
//
// Because files are scanned in parallel, the token.Pos
// positions of the resulting ast.Files are not ordered.
func (ld *loader) parseFiles(filenames []string) ([]*ast.File, []error) {
	var (
		n      = len(filenames)
		parsed = make([]*ast.File, n)
		errors = make([]error, n)
	)
	var g errgroup.Group
	for i, filename := range filenames {
		// This creates goroutines unnecessarily in the
		// cache-hit case, but that case is uncommon.
		g.Go(func() error {
			parsed[i], errors[i] = ld.parseFile(filename)
			return nil
		})
	}
	g.Wait()

	// Eliminate nils, preserving order.
	var o int
	for _, f := range parsed {
		if f != nil {
			parsed[o] = f
			o++
		}
	}
	parsed = parsed[:o]

	o = 0
	for _, err := range errors {
		if err != nil {
			errors[o] = err
			o++
		}
	}
	errors = errors[:o]

	return parsed, errors
}

// sameFile returns true if x and y have the same basename and denote
// the same file.
func sameFile(x, y string) bool {
	if x == y {
		// It could be the case that y doesn't exist.
		return true
	}
	if strings.EqualFold(filepath.Base(x), filepath.Base(y)) { // (optimisation)
		if xi, err := os.Stat(x); err == nil {
			if yi, err := os.Stat(y); err == nil {
				return os.SameFile(xi, yi)
			}
		}
	}
	return false
}

// loadFromExportData ensures that type information is present for the specified
// package, loading it from an export data file on the first request.
// On success it sets lpkg.Types to a new Package.
func (ld *loader) loadFromExportData(lpkg *loaderPackage) error {
	if lpkg.PkgPath == "" {
		log.Fatalf("internal error: Package %s has no PkgPath", lpkg)
	}

	// All exportdata operations must be sequential.
	ld.exportMu.Lock()
	defer ld.exportMu.Unlock()

	if tpkg := lpkg.Types; tpkg != nil && tpkg.Complete() {
		return nil // cache hit
	}

	lpkg.IllTyped = true // fail safe

	if lpkg.ExportFile == "" {
		// Errors while building export data will have been printed to stderr.
		return fmt.Errorf("no export data file")
	}
	f, err := os.Open(lpkg.ExportFile)
	if err != nil {
		return err
	}
	defer f.Close()

	// Read gc export data.
	r, err := gcexportdata.NewReader(f)
	if err != nil {
		return fmt.Errorf("reading %s: %v", lpkg.ExportFile, err)
	}

	// Build the view. The gcexportdata machinery has no concept of package ID.
	view := make(map[string]*types.Package) // view seen by gcexportdata
	seen := make(map[*loaderPackage]bool)   // all visited packages
	var visit func(pkgs map[string]*Package)
	visit = func(pkgs map[string]*Package) {
		for _, p := range pkgs {
			lpkg := ld.pkgs[p.ID]
			if !seen[lpkg] {
				seen[lpkg] = true
				view[lpkg.PkgPath] = lpkg.Types
				visit(lpkg.Imports)
			}
		}
	}
	visit(lpkg.Imports)

	viewLen := len(view) + 1 // adding the self package
	// Parse the export data. (May modify incomplete packages in view but not create new ones.)
	tpkg, err := gcexportdata.Read(r, ld.Fset, view, lpkg.PkgPath)
	if err != nil {
		return fmt.Errorf("reading %s: %v", lpkg.ExportFile, err)
	}
	if _, ok := view["go.shape"]; ok {
		// Account for the pseudopackage "go.shape" that gets created by generic code.
		viewLen++
	}
	if viewLen != len(view) {
		log.Panicf("golang.org/x/tools/go/packages: unexpected new packages during load of %s", lpkg.PkgPath)
	}

	lpkg.Types = tpkg
	lpkg.IllTyped = false
	return nil
}

// impliedLoadMode returns loadMode with its dependencies.
func impliedLoadMode(loadMode LoadMode) LoadMode {
	if loadMode&(NeedDeps|NeedTypes|NeedTypesInfo) != 0 {
		// All these things require knowing the import graph.
		loadMode |= NeedImports
	}
	if loadMode&NeedTypes != 0 {
		// Types require the GoVersion from Module.
		loadMode |= NeedModule
	}

	return loadMode
}

func usesExportData(cfg *Config) bool {
	return cfg.Mode&NeedExportFile != 0 || cfg.Mode&NeedTypes != 0 && cfg.Mode&NeedDeps == 0
}

type unit struct{}

func cond[T any](cond bool, t, f T) T {
	if cond {
		return t
	} else {
		return f
	}
}
