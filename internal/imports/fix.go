// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package imports

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"io/ioutil"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/internal/event"
	"golang.org/x/tools/internal/gocommand"
	"golang.org/x/tools/internal/gopathwalk"
	"golang.org/x/tools/internal/modindex"
	"golang.org/x/tools/internal/stdlib"
)

// importToGroup is a list of functions which map from an import path to
// a group number.
var importToGroup = []func(localPrefix, importPath string) (num int, ok bool){
	func(localPrefix, importPath string) (num int, ok bool) {
		if localPrefix == "" {
			return
		}
		for p := range strings.SplitSeq(localPrefix, ",") {
			if strings.HasPrefix(importPath, p) || strings.TrimSuffix(p, "/") == importPath {
				return 3, true
			}
		}
		return
	},
	func(_, importPath string) (num int, ok bool) {
		if strings.HasPrefix(importPath, "appengine") {
			return 2, true
		}
		return
	},
	func(_, importPath string) (num int, ok bool) {
		firstComponent := strings.Split(importPath, "/")[0]
		if strings.Contains(firstComponent, ".") {
			return 1, true
		}
		return
	},
}

func importGroup(localPrefix, importPath string) int {
	for _, fn := range importToGroup {
		if n, ok := fn(localPrefix, importPath); ok {
			return n
		}
	}
	return 0
}

type ImportFixType int

const (
	AddImport ImportFixType = iota
	DeleteImport
	SetImportName
)

type ImportFix struct {
	// StmtInfo represents the import statement this fix will add, remove, or change.
	StmtInfo ImportInfo
	// IdentName is the identifier that this fix will add or remove.
	IdentName string
	// FixType is the type of fix this is (AddImport, DeleteImport, SetImportName).
	FixType   ImportFixType
	Relevance float64 // see pkg
}

// parseOtherFiles parses all the Go files in srcDir except filename, including
// test files if filename looks like a test.
//
// It returns an error only if ctx is cancelled. Files with parse errors are
// ignored.
func parseOtherFiles(ctx context.Context, fset *token.FileSet, srcDir, filename string) ([]*ast.File, error) {
	// This can use go/packages but it does not buy much.
	considerTests := strings.HasSuffix(filename, "_test.go")

	fileBase := filepath.Base(filename)
	packageFileInfos, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, ctx.Err()
	}

	var files []*ast.File
	for _, fi := range packageFileInfos {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if fi.Name() == fileBase || !strings.HasSuffix(fi.Name(), ".go") {
			continue
		}
		if !considerTests && strings.HasSuffix(fi.Name(), "_test.go") {
			continue
		}

		f, err := parser.ParseFile(fset, filepath.Join(srcDir, fi.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}

		files = append(files, f)
	}

	return files, ctx.Err()
}

// addGlobals puts the names of package vars into the provided map.
func addGlobals(f *ast.File, globals map[string]bool) {
	for _, decl := range f.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}

		for _, spec := range genDecl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			globals[valueSpec.Names[0].Name] = true
		}
	}
}

// collectReferences builds a map of selector expressions, from
// left hand side (X) to a set of right hand sides (Sel).
func collectReferences(f *ast.File) References {
	refs := References{}

	var visitor visitFn
	visitor = func(node ast.Node) ast.Visitor {
		if node == nil {
			return visitor
		}
		switch v := node.(type) {
		case *ast.SelectorExpr:
			xident, ok := v.X.(*ast.Ident)
			if !ok {
				break
			}
			if xident.Obj != nil {
				// If the parser can resolve it, it's not a package ref.
				break
			}
			if !ast.IsExported(v.Sel.Name) {
				// Whatever this is, it's not exported from a package.
				break
			}
			pkgName := xident.Name
			r := refs[pkgName]
			if r == nil {
				r = make(map[string]bool)
				refs[pkgName] = r
			}
			r[v.Sel.Name] = true
		}
		return visitor
	}
	ast.Walk(visitor, f)
	return refs
}

// collectImports returns all the imports in f.
// Unnamed imports (., _) and "C" are ignored.
func collectImports(f *ast.File) []*ImportInfo {
	var imports []*ImportInfo
	for _, imp := range f.Imports {
		var name string
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if imp.Path.Value == `"C"` || name == "_" || name == "." {
			continue
		}
		path := strings.Trim(imp.Path.Value, `"`)
		imports = append(imports, &ImportInfo{
			Name:       name,
			ImportPath: path,
		})
	}
	return imports
}

// findMissingImport searches pass's candidates for an import that provides
// pkg, containing all of syms.
func (p *pass) findMissingImport(pkg string, syms map[string]bool) *ImportInfo {
	for _, candidate := range p.candidates {
		pkgInfo, ok := p.knownPackages[candidate.ImportPath]
		if !ok {
			continue
		}
		if p.importIdentifier(candidate) != pkg {
			continue
		}

		allFound := true
		for right := range syms {
			if !pkgInfo.Exports[right] {
				allFound = false
				break
			}
		}

		if allFound {
			return candidate
		}
	}
	return nil
}

// A pass contains all the inputs and state necessary to fix a file's imports.
// It can be modified in some ways during use; see comments below.
type pass struct {
	// Inputs. These must be set before a call to load, and not modified after.
	fset                 *token.FileSet // fset used to parse f and its siblings.
	f                    *ast.File      // the file being fixed.
	srcDir               string         // the directory containing f.
	logf                 func(string, ...any)
	source               Source      // the environment to use for go commands, etc.
	loadRealPackageNames bool        // if true, load package names from disk rather than guessing them.
	otherFiles           []*ast.File // sibling files.
	goroot               string

	// Intermediate state, generated by load.
	existingImports map[string][]*ImportInfo
	allRefs         References
	missingRefs     References

	// Inputs to fix. These can be augmented between successive fix calls.
	lastTry       bool                    // indicates that this is the last call and fix should clean up as best it can.
	candidates    []*ImportInfo           // candidate imports in priority order.
	knownPackages map[string]*PackageInfo // information about all known packages.
}

// loadPackageNames saves the package names for everything referenced by imports.
func (p *pass) loadPackageNames(ctx context.Context, imports []*ImportInfo) error {
	if p.logf != nil {
		p.logf("loading package names for %v packages", len(imports))
		defer func() {
			p.logf("done loading package names for %v packages", len(imports))
		}()
	}
	var unknown []string
	for _, imp := range imports {
		if _, ok := p.knownPackages[imp.ImportPath]; ok {
			continue
		}
		unknown = append(unknown, imp.ImportPath)
	}
	names, err := p.source.LoadPackageNames(ctx, p.srcDir, unknown)
	if err != nil {
		return err
	}

	// TODO(rfindley): revisit this. Why do we need to store known packages with
	// no exports? The inconsistent data is confusing.
	for path, name := range names {
		p.knownPackages[path] = &PackageInfo{
			Name:    name,
			Exports: map[string]bool{},
		}
	}
	return nil
}

// WithoutVersion removes a trailing major version, if there is one.
func WithoutVersion(nm string) string {
	if v := path.Base(nm); len(v) > 0 && v[0] == 'v' {
		if _, err := strconv.Atoi(v[1:]); err == nil {
			// this is, for instance, called with rand/v2 and returns rand
			if len(v) < len(nm) {
				xnm := nm[:len(nm)-len(v)-1]
				return path.Base(xnm)
			}
		}
	}
	return nm
}

// importIdentifier returns the identifier that imp will introduce. It will
// guess if the package name has not been loaded, e.g. because the source
// is not available.
func (p *pass) importIdentifier(imp *ImportInfo) string {
	if imp.Name != "" {
		return imp.Name
	}
	known := p.knownPackages[imp.ImportPath]
	if known != nil && known.Name != "" {
		return WithoutVersion(known.Name)
	}
	return ImportPathToAssumedName(imp.ImportPath)
}

// load reads in everything necessary to run a pass, and reports whether the
// file already has all the imports it needs. It fills in p.missingRefs with the
// file's missing symbols, if any, or removes unused imports if not. times:
// self, otherFiles, loadRealPackageNames
func (p *pass) load(ctx context.Context) ([]*ImportFix, bool) {
	p.knownPackages = map[string]*PackageInfo{}
	p.missingRefs = References{}
	p.existingImports = map[string][]*ImportInfo{}

	// Load basic information about the file in question.
	p.allRefs = collectReferences(p.f)

	// Load stuff from other files in the same package.
	globals := map[string]bool{}
	for _, otherFile := range p.otherFiles {
		// Don't load globals from files that are in the same directory
		// but a different package. Using them to suggest imports is OK.
		if p.f.Name.Name == otherFile.Name.Name {
			addGlobals(otherFile, globals)
		}
		p.candidates = append(p.candidates, collectImports(otherFile)...)
	}

	// Resolve all the import paths we've seen to package names, and store f's imports by the identifier they introduce.
	imports := collectImports(p.f)
	if p.loadRealPackageNames {
		err := p.loadPackageNames(ctx, append(imports, p.candidates...))
		if err != nil {
			if p.logf != nil {
				p.logf("loading package names: %v", err)
			}
			return nil, false
		}
	}
	for _, imp := range imports {
		p.existingImports[p.importIdentifier(imp)] = append(p.existingImports[p.importIdentifier(imp)], imp)
	}

	// Find missing references.
	for left, rights := range p.allRefs {
		if globals[left] {
			continue
		}
		_, ok := p.existingImports[left]
		if !ok {
			p.missingRefs[left] = rights
			continue
		}
	}
	if len(p.missingRefs) != 0 {
		return nil, false
	}

	return p.fix()
}

// fix attempts to satisfy missing imports using p.candidates. Or if p.lastTry is
// true, it updates fixes to add the imports it found, delete anything unused, and
// update import names, and returns true. This happens if it finds everything.
func (p *pass) fix() ([]*ImportFix, bool) {
	// Find missing imports.
	var selected []*ImportInfo
	for left, rights := range p.missingRefs {
		if imp := p.findMissingImport(left, rights); imp != nil {
			selected = append(selected, imp)
		}
	}

	if !p.lastTry && len(selected) != len(p.missingRefs) {
		return nil, false
	}

	// Found everything, or giving up. Add the new imports and remove any unused.
	var fixes []*ImportFix
	for _, identifierImports := range p.existingImports {
		for _, imp := range identifierImports {
			// We deliberately ignore globals here, because we can't be sure they're in
			// the same package.
			if _, ok := p.allRefs[p.importIdentifier(imp)]; !ok {
				fixes = append(fixes, &ImportFix{
					StmtInfo:  *imp,
					IdentName: p.importIdentifier(imp),
					FixType:   DeleteImport,
				})
				continue
			}

			// An existing import may need to update its import name to be correct.
			if name := p.importSpecName(imp); name != imp.Name {
				fixes = append(fixes, &ImportFix{
					StmtInfo: ImportInfo{
						Name:       name,
						ImportPath: imp.ImportPath,
					},
					IdentName: p.importIdentifier(imp),
					FixType:   SetImportName,
				})
			}
		}
	}
	// Collecting fixes involved map iteration, so sort for stability.
	sortFixes(fixes)

	// collect selected fixes in a separate slice, so that it can be sorted separately. these fixes must occur after fixes to existing imports.
	var selectedFixes []*ImportFix
	for _, imp := range selected {
		selectedFixes = append(selectedFixes, &ImportFix{
			StmtInfo: ImportInfo{
				Name:       p.importSpecName(imp),
				ImportPath: imp.ImportPath,
			},
			IdentName: p.importIdentifier(imp),
			FixType:   AddImport,
		})
	}
	sortFixes(selectedFixes)

	return append(fixes, selectedFixes...), true
}

func sortFixes(fixes []*ImportFix) {
	sort.Slice(fixes, func(i, j int) bool {
		fi, fj := fixes[i], fixes[j]
		if fi.StmtInfo.ImportPath != fj.StmtInfo.ImportPath {
			return fi.StmtInfo.ImportPath < fj.StmtInfo.ImportPath
		}
		if fi.StmtInfo.Name != fj.StmtInfo.Name {
			return fi.StmtInfo.Name < fj.StmtInfo.Name
		}
		if fi.IdentName != fj.IdentName {
			return fi.IdentName < fj.IdentName
		}
		return fi.FixType < fj.FixType
	})
}

func hasDeleteFix(fixes []*ImportFix) bool {
	for _, fix := range fixes {
		if fix.FixType == DeleteImport {
			return true
		}
	}
	return false
}

// importSpecName gets the import name of imp in the import spec.
//
// When the import identifier matches the assumed import name, the import name does
// not appear in the import spec.
func (p *pass) importSpecName(imp *ImportInfo) string {
	// If we did not load the real package names, or the name is already set, we
	// return the existing name.
	if !p.loadRealPackageNames || imp.Name != "" {
		return imp.Name
	}

	ident := p.importIdentifier(imp)
	if ident == ImportPathToAssumedName(imp.ImportPath) {
		return "" // ident not needed since the assumed and real names are the same.
	}
	return ident
}

// apply will perform the fixes on f in order.
func apply(fset *token.FileSet, f *ast.File, fixes []*ImportFix) {
	for _, fix := range fixes {
		switch fix.FixType {
		case DeleteImport:
			astutil.DeleteNamedImport(fset, f, fix.StmtInfo.Name, fix.StmtInfo.ImportPath)
		case AddImport:
			astutil.AddNamedImport(fset, f, fix.StmtInfo.Name, fix.StmtInfo.ImportPath)
		case SetImportName:
			// Find the matching import path and change the name.
			for _, spec := range f.Imports {
				path := strings.Trim(spec.Path.Value, `"`)
				if path == fix.StmtInfo.ImportPath {
					spec.Name = &ast.Ident{
						Name:    fix.StmtInfo.Name,
						NamePos: spec.Pos(),
					}
				}
			}
		}
	}
}

// assumeSiblingImportsValid assumes that siblings' use of packages is valid,
// adding the exports they use.
func (p *pass) assumeSiblingImportsValid() {
	for _, f := range p.otherFiles {
		refs := collectReferences(f)
		imports := collectImports(f)
		importsByName := map[string]*ImportInfo{}
		for _, imp := range imports {
			importsByName[p.importIdentifier(imp)] = imp
		}
		for left, rights := range refs {
			if imp, ok := importsByName[left]; ok {
				if m, ok := stdlib.PackageSymbols()[imp.ImportPath]; ok {
					// We have the stdlib in memory; no need to guess.
					rights = symbolNameSet(m)
				}
				// TODO(rfindley): we should set package name here, for consistency.
				p.addCandidate(imp, &PackageInfo{
					// no name; we already know it.
					Exports: rights,
				})
			}
		}
	}
}

// addCandidate adds a candidate import to p, and merges in the information
// in pkg.
func (p *pass) addCandidate(imp *ImportInfo, pkg *PackageInfo) {
	p.candidates = append(p.candidates, imp)
	if existing, ok := p.knownPackages[imp.ImportPath]; ok {
		if existing.Name == "" {
			existing.Name = pkg.Name
		}
		for export := range pkg.Exports {
			existing.Exports[export] = true
		}
	} else {
		p.knownPackages[imp.ImportPath] = pkg
	}
}

// fixImports adds and removes imports from f so that all its references are satisfied and there are no unused imports.
var fixImports = fixImportsDefault

func fixImportsDefault(fset *token.FileSet, f *ast.File, filename string, env *ProcessEnv) error {
	fixes, err := getFixes(context.Background(), fset, f, filename, env)
	if err != nil {
		return err
	}
	apply(fset, f, fixes)
	return nil
}

// getFixes gets the import fixes that need to be made to f to fix the
// imports. It does not modify the ast.
func getFixes(ctx context.Context, fset *token.FileSet, f *ast.File, filename string, env *ProcessEnv) ([]*ImportFix, error) {
	source, err := NewProcessEnvSource(env, filename, f.Name.Name)
	if err != nil {
		return nil, err
	}
	goEnv, err := env.goEnv()
	if err != nil {
		return nil, err
	}
	return getFixesWithSource(ctx, fset, f, filename, goEnv["GOROOT"], env.logf, source)
}

func getFixesWithSource(ctx context.Context, fset *token.FileSet, f *ast.File, filename string, goroot string, logf func(string, ...any), source Source) ([]*ImportFix, error) {
	// This logic is defensively duplicated from getFixes.
	abs, err := filepath.Abs(filename)
	if err != nil {
		return nil, err
	}
	srcDir := filepath.Dir(abs)

	if logf != nil {
		logf("fixImports(filename=%q), srcDir=%q ...", filename, srcDir)
	}

	// First pass: looking only at f, and using the naive algorithm to derive
	// package names from import paths, see if the file is already complete.
	p := &pass{
		fset:   fset,
		f:      f,
		srcDir: srcDir,
		logf:   logf,
		goroot: goroot,
		source: source,
	}
	if fixes, done := p.load(ctx); done {
		return fixes, nil
	}

	otherFiles, err := parseOtherFiles(ctx, fset, srcDir, filename)
	if err != nil {
		return nil, err
	}

	// Second pass: add information from other files in the same package, like their package vars and imports.
	p.otherFiles = otherFiles
	if fixes, done := p.load(ctx); done {
		return fixes, nil
	}

	// Now we can try adding imports from the stdlib.
	p.assumeSiblingImportsValid()
	addStdlibCandidates(p, p.missingRefs)
	if fixes, done := p.fix(); done && !hasDeleteFix(fixes) {
		return fixes, nil
	}

	// Third pass: get real package names where we had used the naive algorithm.
	p = &pass{
		fset:   fset,
		f:      f,
		srcDir: srcDir,
		logf:   logf,
		goroot: goroot,
		source: p.source, // safe to reuse, as it's just a wrapper around env
	}
	p.loadRealPackageNames = true
	p.otherFiles = otherFiles

	// If there is an Index for the GOMODCACHE, remember that, and later make it.
	var ix *modindex.Index
	if src, ok := source.(*ProcessEnvSource); ok {
		var err error
		if ix, err = modindex.Read(src.env.Env["GOMODCACHE"]); err != nil {
			ix = nil // don't use it if there was an error
		}
	}

	if ix != nil {
		src, ok := p.source.(*ProcessEnvSource)
		if ok {
			// For safety, clone the env so that we don't modify the caller's env.
			env := *src.env
			env.Env = maps.Clone(src.env.Env)
			src.env = &env
			// avoid looking in the module cache, as we have the index instead.
			src.env.Env["GOMODCACHE"] = ""
		}
	}
	if fixes, done := p.load(ctx); done {
		return fixes, nil
	}

	if err := addStdlibCandidates(p, p.missingRefs); err != nil {
		return nil, err
	}
	p.assumeSiblingImportsValid()
	if fixes, done := p.fix(); done {
		return fixes, nil
	}

	// Go look for candidates in $GOPATH, etc. We don't necessarily load
	// the real exports of sibling imports, so keep assuming their contents.
	if err := addExternalCandidates(ctx, p, p.missingRefs, filename, ix); err != nil {
		return nil, err
	}

	p.lastTry = true
	fixes, _ := p.fix()
	return fixes, nil
}

// MaxRelevance is the highest relevance, used for the standard library.
const MaxRelevance = 7.0

// getCandidatePkgs works with the passed callback to find all acceptable packages.
// It deduplicates by import path, and uses a cached stdlib rather than reading
// from disk.
func getCandidatePkgs(ctx context.Context, wrappedCallback *scanCallback, filename, filePkg string, env *ProcessEnv) error {
	notSelf := func(p *pkg) bool {
		return p.packageName != filePkg || p.dir != filepath.Dir(filename)
	}
	goenv, err := env.goEnv()
	if err != nil {
		return err
	}

	var mu sync.Mutex // to guard asynchronous access to dupCheck
	dupCheck := map[string]struct{}{}

	// Start off with the standard library.
	for importPath, symbols := range stdlib.PackageSymbols() {
		p := &pkg{
			dir:             filepath.Join(goenv["GOROOT"], "src", importPath),
			importPathShort: importPath,
			packageName:     path.Base(importPath),
			relevance:       MaxRelevance,
		}
		dupCheck[importPath] = struct{}{}
		if notSelf(p) && wrappedCallback.dirFound(p) && wrappedCallback.packageNameLoaded(p) {
			var exports []stdlib.Symbol
			for _, sym := range symbols {
				switch sym.Kind {
				case stdlib.Func, stdlib.Type, stdlib.Var, stdlib.Const:
					exports = append(exports, sym)
				}
			}
			wrappedCallback.exportsLoaded(p, exports)
		}
	}

	scanFilter := &scanCallback{
		rootFound: func(root gopathwalk.Root) bool {
			// Exclude goroot results -- getting them is relatively expensive, not cached.
			return root.Type != gopathwalk.RootGOROOT && wrappedCallback.rootFound(root)
		},
		dirFound: wrappedCallback.dirFound,
		packageNameLoaded: func(pkg *pkg) bool {
			mu.Lock()
			defer mu.Unlock()
			if _, ok := dupCheck[pkg.importPathShort]; ok {
				return false
			}
			dupCheck[pkg.importPathShort] = struct{}{}
			return notSelf(pkg) && wrappedCallback.packageNameLoaded(pkg)
		},
		exportsLoaded: func(pkg *pkg, exports []stdlib.Symbol) {
			// If we're an x_test, load the package under test's test variant.
			if strings.HasSuffix(filePkg, "_test") && pkg.dir == filepath.Dir(filename) {
				var err error
				_, exports, err = loadExportsFromFiles(ctx, env, pkg.dir, true)
				if err != nil {
					return
				}
			}
			wrappedCallback.exportsLoaded(pkg, exports)
		},
	}
	resolver, err := env.GetResolver()
	if err != nil {
		return err
	}
	return resolver.scan(ctx, scanFilter)
}

func ScoreImportPaths(ctx context.Context, env *ProcessEnv, paths []string) (map[string]float64, error) {
	result := make(map[string]float64)
	resolver, err := env.GetResolver()
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		result[path] = resolver.scoreImportPath(ctx, path)
	}
	return result, nil
}

func PrimeCache(ctx context.Context, resolver Resolver) error {
	// Fully scan the disk for directories, but don't read any Go files.
	callback := &scanCallback{
		rootFound: func(root gopathwalk.Root) bool {
			// See getCandidatePkgs: walking GOROOT is apparently expensive and unnecessary.
			return root.Type != gopathwalk.RootGOROOT
		},
		dirFound: func(pkg *pkg) bool {
			return false
		},
	}

	return resolver.scan(ctx, callback)
}

func candidateImportName(pkg *pkg) string {
	if ImportPathToAssumedName(pkg.importPathShort) != pkg.packageName {
		return pkg.packageName
	}
	return ""
}

// GetAllCandidates calls wrapped for each package whose name starts with
// searchPrefix, and can be imported from filename with the package name filePkg.
//
// Beware that the wrapped function may be called multiple times concurrently.
// TODO(adonovan): encapsulate the concurrency.
func GetAllCandidates(ctx context.Context, wrapped func(ImportFix), searchPrefix, filename, filePkg string, env *ProcessEnv) error {
	callback := &scanCallback{
		rootFound: func(gopathwalk.Root) bool {
			return true
		},
		dirFound: func(pkg *pkg) bool {
			if !CanUse(filename, pkg.dir) {
				return false
			}
			// Try the assumed package name first, then a simpler path match
			// in case of packages named vN, which are not uncommon.
			return strings.HasPrefix(ImportPathToAssumedName(pkg.importPathShort), searchPrefix) ||
				strings.HasPrefix(path.Base(pkg.importPathShort), searchPrefix)
		},
		packageNameLoaded: func(pkg *pkg) bool {
			if !strings.HasPrefix(pkg.packageName, searchPrefix) {
				return false
			}
			wrapped(ImportFix{
				StmtInfo: ImportInfo{
					ImportPath: pkg.importPathShort,
					Name:       candidateImportName(pkg),
				},
				IdentName: pkg.packageName,
				FixType:   AddImport,
				Relevance: pkg.relevance,
			})
			return false
		},
	}
	return getCandidatePkgs(ctx, callback, filename, filePkg, env)
}

// GetImportPaths calls wrapped for each package whose import path starts with
// searchPrefix, and can be imported from filename with the package name filePkg.
func GetImportPaths(ctx context.Context, wrapped func(ImportFix), searchPrefix, filename, filePkg string, env *ProcessEnv) error {
	callback := &scanCallback{
		rootFound: func(gopathwalk.Root) bool {
			return true
		},
		dirFound: func(pkg *pkg) bool {
			if !CanUse(filename, pkg.dir) {
				return false
			}
			return strings.HasPrefix(pkg.importPathShort, searchPrefix)
		},
		packageNameLoaded: func(pkg *pkg) bool {
			wrapped(ImportFix{
				StmtInfo: ImportInfo{
					ImportPath: pkg.importPathShort,
					Name:       candidateImportName(pkg),
				},
				IdentName: pkg.packageName,
				FixType:   AddImport,
				Relevance: pkg.relevance,
			})
			return false
		},
	}
	return getCandidatePkgs(ctx, callback, filename, filePkg, env)
}

// A PackageExport is a package and its exports.
type PackageExport struct {
	Fix     *ImportFix
	Exports []stdlib.Symbol
}

// GetPackageExports returns all known packages with name pkg and their exports.
func GetPackageExports(ctx context.Context, wrapped func(PackageExport), searchPkg, filename, filePkg string, env *ProcessEnv) error {
	callback := &scanCallback{
		rootFound: func(gopathwalk.Root) bool {
			return true
		},
		dirFound: func(pkg *pkg) bool {
			return pkgIsCandidate(filename, References{searchPkg: nil}, pkg)
		},
		packageNameLoaded: func(pkg *pkg) bool {
			return pkg.packageName == searchPkg
		},
		exportsLoaded: func(pkg *pkg, exports []stdlib.Symbol) {
			sortSymbols(exports)
			wrapped(PackageExport{
				Fix: &ImportFix{
					StmtInfo: ImportInfo{
						ImportPath: pkg.importPathShort,
						Name:       candidateImportName(pkg),
					},
					IdentName: pkg.packageName,
					FixType:   AddImport,
					Relevance: pkg.relevance,
				},
				Exports: exports,
			})
		},
	}
	return getCandidatePkgs(ctx, callback, filename, filePkg, env)
}

// TODO(rfindley): we should depend on GOOS and GOARCH, to provide accurate
// imports when doing cross-platform development.
var requiredGoEnvVars = []string{
	"GO111MODULE",
	"GOFLAGS",
	"GOINSECURE",
	"GOMOD",
	"GOMODCACHE",
	"GONOPROXY",
	"GONOSUMDB",
	"GOPATH",
	"GOPROXY",
	"GOROOT",
	"GOSUMDB",
}

// ProcessEnv contains environment variables and settings that affect the use
// of the go command, the go/build package, etc.
type ProcessEnv struct {
	GocmdRunner *gocommand.Runner

	BuildFlags []string
	ModFlag    string

	// SkipPathInScan returns true if the path should be skipped from scans of the RootCurrentModule root type.
	SkipPathInScan func(string) bool

	// Env overrides the OS environment, and can be used to specify GOPROXY, GO111MODULE, etc. PATH cannot be set here.
	Env map[string]string

	WorkingDir string

	// If Logf is non-nil, debug logging is enabled through this function.
	Logf func(format string, args ...any)

	// If set, ModCache holds a shared cache of directory info to use across multiple ProcessEnvs.
	ModCache *DirInfoCache

	initialized bool // see TODO above

	// resolver and resolverErr are lazily evaluated (see GetResolver).
	resolver    Resolver
	resolverErr error
}

func (e *ProcessEnv) goEnv() (map[string]string, error) {
	if err := e.init(); err != nil {
		return nil, err
	}
	return e.Env, nil
}

func (e *ProcessEnv) matchFile(dir, name string) (bool, error) {
	bctx, err := e.buildContext()
	if err != nil {
		return false, err
	}
	return bctx.MatchFile(dir, name)
}

// CopyConfig copies the env's configuration into a new env.
func (e *ProcessEnv) CopyConfig() *ProcessEnv {
	copy := &ProcessEnv{
		GocmdRunner: e.GocmdRunner,
		initialized: e.initialized,
		BuildFlags:  e.BuildFlags,
		Logf:        e.Logf,
		WorkingDir:  e.WorkingDir,
		resolver:    nil,
		Env:         map[string]string{},
	}
	maps.Copy(copy.Env, e.Env)
	return copy
}

func (e *ProcessEnv) init() error {
	if e.initialized {
		return nil
	}

	foundAllRequired := true
	for _, k := range requiredGoEnvVars {
		if _, ok := e.Env[k]; !ok {
			foundAllRequired = false
			break
		}
	}
	if foundAllRequired {
		e.initialized = true
		return nil
	}

	if e.Env == nil {
		e.Env = map[string]string{}
	}

	goEnv := map[string]string{}
	stdout, err := e.invokeGo(context.TODO(), "env", append([]string{"-json"}, requiredGoEnvVars...)...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(stdout.Bytes(), &goEnv); err != nil {
		return err
	}
	maps.Copy(e.Env, goEnv)
	e.initialized = true
	return nil
}

func (e *ProcessEnv) env() []string {
	var env []string // the gocommand package will prepend os.Environ.
	for k, v := range e.Env {
		env = append(env, k+"="+v)
	}
	return env
}

func (e *ProcessEnv) GetResolver() (Resolver, error) {
	if err := e.init(); err != nil {
		return nil, err
	}

	if e.resolver == nil && e.resolverErr == nil {
		// TODO(rfindley): we should only use a gopathResolver here if the working
		// directory is *in* GOPATH. (I seem to recall an open gopls issue for this
		// behavior, but I can't find it).
		//
		// For gopls, we can optionally explicitly choose a resolver type, since we
		// already know the view type.
		if e.Env["GOMOD"] == "" {
			e.resolver = newGopathResolver(e)
			e.logf("created gopath resolver")
		} else if r, err := newModuleResolver(e, e.ModCache); err != nil {
			e.resolverErr = err
			e.logf("failed to create module resolver: %v", err)
		} else {
			e.resolver = Resolver(r)
			e.logf("created module resolver")
		}
	}

	return e.resolver, e.resolverErr
}

// logf logs if e.Logf is non-nil.
func (e *ProcessEnv) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

// buildContext returns the build.Context to use for matching files.
//
// TODO(rfindley): support dynamic GOOS, GOARCH here, when doing cross-platform
// development.
func (e *ProcessEnv) buildContext() (*build.Context, error) {
	ctx := build.Default
	goenv, err := e.goEnv()
	if err != nil {
		return nil, err
	}
	ctx.GOROOT = goenv["GOROOT"]
	ctx.GOPATH = goenv["GOPATH"]

	// Populate it only if present.
	rc := reflect.ValueOf(&ctx).Elem()
	dir := rc.FieldByName("Dir")
	if dir.IsValid() && dir.Kind() == reflect.String {
		dir.SetString(e.WorkingDir)
	}

	ctx.ReadDir = ioutil.ReadDir

	return &ctx, nil
}

func (e *ProcessEnv) invokeGo(ctx context.Context, verb string, args ...string) (*bytes.Buffer, error) {
	inv := gocommand.Invocation{
		Verb:       verb,
		Args:       args,
		BuildFlags: e.BuildFlags,
		Env:        e.env(),
		Logf:       e.Logf,
		WorkingDir: e.WorkingDir,
	}
	return e.GocmdRunner.Run(ctx, inv)
}

func addStdlibCandidates(pass *pass, refs References) error {
	localbase := func(nm string) string {
		ans := path.Base(nm)
		if ans[0] == 'v' {
			// this is called, for instance, with math/rand/v2 and returns rand/v2
			if _, err := strconv.Atoi(ans[1:]); err == nil {
				ix := strings.LastIndex(nm, ans)
				more := path.Base(nm[:ix])
				ans = path.Join(more, ans)
			}
		}
		return ans
	}
	add := func(pkg string) {
		// Prevent self-imports.
		if path.Base(pkg) == pass.f.Name.Name && filepath.Join(pass.goroot, "src", pkg) == pass.srcDir {
			return
		}
		exports := symbolNameSet(stdlib.PackageSymbols()[pkg])
		pass.addCandidate(
			&ImportInfo{ImportPath: pkg},
			&PackageInfo{Name: localbase(pkg), Exports: exports})
	}
	for left := range refs {
		if left == "rand" {
			// Make sure we try crypto/rand before any version of math/rand as both have Int() and our policy is to recommend crypto
			add("crypto/rand")
			// if the user's no later than go1.21, this should be "math/rand" but we have no way of figuring out what the user is using TODO.
			add("math/rand/v2")
			// math/rand has an overlapping API TestIssue66407 fails without this
			add("math/rand")
			continue
		}
		for importPath := range stdlib.PackageSymbols() {
			if path.Base(importPath) == left {
				add(importPath)
			}
		}
	}
	return nil
}

// A Resolver does the build-system-specific parts of goimports.
type Resolver interface {
	// loadPackageNames loads the package names in importPaths.
	loadPackageNames(importPaths []string, srcDir string) (map[string]string, error)

	// scan works with callback to search for packages. See scanCallback for details.
	scan(ctx context.Context, callback *scanCallback) error

	// loadExports returns the package name and set of exported symbols in the package at dir.
	loadExports(ctx context.Context, pkg *pkg, includeTest bool) (string, []stdlib.Symbol, error)

	// scoreImportPath returns the relevance for an import path.
	scoreImportPath(ctx context.Context, path string) float64

	// ClearForNewScan returns a new Resolver based on the receiver that has cleared its internal caches of directory contents.
	ClearForNewScan() Resolver
}

// A scanCallback controls a call to scan and receives its results.
// In general, minor errors will be silently discarded; a user should not
// expect to receive a full series of calls for everything.
type scanCallback struct {
	// rootFound is called before scanning a new root dir. If it returns true, the root will be scanned.
	rootFound func(gopathwalk.Root) bool
	// dirFound is called when a directory is found that is possibly.
	dirFound func(pkg *pkg) bool
	// packageNameLoaded is called when a package is found and its name is loaded.
	packageNameLoaded func(pkg *pkg) bool
	// exportsLoaded is called when a package's exports have been loaded.
	exportsLoaded func(pkg *pkg, exports []stdlib.Symbol)
}

func addExternalCandidates(ctx context.Context, pass *pass, refs References, filename string, ix *modindex.Index) error {
	ctx, done := event.Start(ctx, "imports.addExternalCandidates")
	defer done()

	results, err := pass.source.ResolveReferences(ctx, filename, refs)
	if err != nil {
		return err
	}

	// Add candidates from the module cache.
	if ix != nil {
		for k, v := range refs {
			for n := range v {
				cands := ix.Lookup(k, n, false)
				for _, cand := range cands {
					x := &Result{
						&ImportInfo{ImportPath: cand.ImportPath},
						&PackageInfo{Name: cand.PkgName,
							Exports: map[string]bool{cand.Name: true},
						},
					}
					results = append(results, x)
				}
			}
		}
	}

	for _, result := range results {
		if result == nil {
			continue
		}
		// Don't offer completions that would shadow predeclared
		// names, such as github.com/coreos/etcd/error.
		if types.Universe.Lookup(result.Package.Name) != nil { // predeclared
			// Ideally we would skip this candidate only if the predeclared name is referenced by the file.
			continue
		}
		pass.addCandidate(result.Import, result.Package)
	}
	return nil
}

// notIdentifier reports whether ch is an invalid identifier character.
func notIdentifier(ch rune) bool {
	return !('a' <= ch && ch <= 'z' || 'A' <= ch && ch <= 'Z' ||
		'0' <= ch && ch <= '9' ||
		ch == '_' ||
		ch >= utf8.RuneSelf && (unicode.IsLetter(ch) || unicode.IsDigit(ch)))
}

// ImportPathToAssumedName returns the assumed package name of an import path.
// It does this using only string parsing of the import path. It picks the
// last element of the path that does not look like a major version. And then
// picks the valid identifier off the start of that element. It is used to
// determine if a local rename should be added to an import for clarity.
func ImportPathToAssumedName(importPath string) string {
	base := path.Base(importPath)
	if strings.HasPrefix(base, "v") {
		if _, err := strconv.Atoi(base[1:]); err == nil {
			dir := path.Dir(importPath)
			if dir != "." {
				base = path.Base(dir)
			}
		}
	}
	base = strings.TrimPrefix(base, "go-")
	if i := strings.IndexFunc(base, notIdentifier); i >= 0 {
		base = base[:i]
	}
	return base
}

// gopathResolver implements resolver for GOPATH workspaces.
type gopathResolver struct {
	env      *ProcessEnv
	cache    *DirInfoCache
	scanSema chan struct{} // scanSema prevents concurrent scans.
}

func newGopathResolver(env *ProcessEnv) *gopathResolver {
	r := &gopathResolver{
		env:      env,
		cache:    NewDirInfoCache(),
		scanSema: make(chan struct{}, 1),
	}
	r.scanSema <- struct{}{}
	return r
}

func (r *gopathResolver) ClearForNewScan() Resolver {
	return newGopathResolver(r.env)
}

func (r *gopathResolver) loadPackageNames(importPaths []string, srcDir string) (map[string]string, error) {
	names := map[string]string{}
	bctx, err := r.env.buildContext()
	if err != nil {
		return nil, err
	}
	for _, path := range importPaths {
		names[path] = importPathToName(bctx, path, srcDir)
	}
	return names, nil
}

// importPathToName finds out the actual package name, as declared in its .go files.
func importPathToName(bctx *build.Context, importPath, srcDir string) string {
	// Fast path for standard library without going to disk.
	if stdlib.HasPackage(importPath) {
		return path.Base(importPath) // stdlib packages always match their paths.
	}

	buildPkg, err := bctx.Import(importPath, srcDir, build.FindOnly)
	if err != nil {
		return ""
	}
	pkgName, err := packageDirToName(buildPkg.Dir)
	if err != nil {
		return ""
	}
	return pkgName
}

// packageDirToName is a faster version of build.Import if
// the only thing desired is the package name. Given a directory,
// packageDirToName then only parses one file in the package,
// trusting that the files in the directory are consistent.
func packageDirToName(dir string) (packageName string, err error) {
	d, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		return "", err
	}
	sort.Strings(names) // to have predictable behavior
	var lastErr error
	var nfile int
	for _, name := range names {
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		nfile++
		fullFile := filepath.Join(dir, name)

		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, fullFile, nil, parser.PackageClauseOnly)
		if err != nil {
			lastErr = err
			continue
		}
		pkgName := f.Name.Name
		if pkgName == "documentation" {
			// Special case from go/build.ImportDir, not handled by ctx.MatchFile.
			continue
		}
		if pkgName == "main" {
			// Also skip package main, assuming it's a +build ignore generator or example.
			continue
		}
		return pkgName, nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("no importable package found in %d Go files", nfile)
}

type pkg struct {
	dir             string  // absolute file path to pkg directory ("/usr/lib/go/src/net/http")
	importPathShort string  // vendorless import path ("net/http", "a/b")
	packageName     string  // package name loaded from source if requested
	relevance       float64 // a weakly-defined score of how relevant a package is.
}

type pkgDistance struct {
	pkg      *pkg
	distance int // relative distance to target
}

// byDistanceOrImportPathShortLength sorts by relative distance breaking ties on the short import path length and then the import string itself.
type byDistanceOrImportPathShortLength []pkgDistance

func (s byDistanceOrImportPathShortLength) Len() int { return len(s) }
func (s byDistanceOrImportPathShortLength) Less(i, j int) bool {
	di, dj := s[i].distance, s[j].distance
	if di == -1 {
		return false
	}
	if dj == -1 {
		return true
	}
	if di != dj {
		return di < dj
	}

	vi, vj := s[i].pkg.importPathShort, s[j].pkg.importPathShort
	if len(vi) != len(vj) {
		return len(vi) < len(vj)
	}
	return vi < vj
}
func (s byDistanceOrImportPathShortLength) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

func distance(basepath, targetpath string) int {
	p, err := filepath.Rel(basepath, targetpath)
	if err != nil {
		return -1
	}
	if p == "." {
		return 0
	}
	return strings.Count(p, string(filepath.Separator)) + 1
}

func (r *gopathResolver) scan(ctx context.Context, callback *scanCallback) error {
	add := func(root gopathwalk.Root, dir string) {
		// We assume cached directories have not changed. We can skip them and their
		// children.
		if _, ok := r.cache.Load(dir); ok {
			return
		}

		importpath := filepath.ToSlash(dir[len(root.Path)+len("/"):])
		info := directoryPackageInfo{
			status:                 directoryScanned,
			dir:                    dir,
			rootType:               root.Type,
			nonCanonicalImportPath: VendorlessPath(importpath),
		}
		r.cache.Store(dir, info)
	}
	processDir := func(info directoryPackageInfo) {
		// Skip this directory if we were not able to get the package information successfully.
		if scanned, err := info.reachedStatus(directoryScanned); !scanned || err != nil {
			return
		}

		p := &pkg{
			importPathShort: info.nonCanonicalImportPath,
			dir:             info.dir,
			relevance:       MaxRelevance - 1,
		}
		if info.rootType == gopathwalk.RootGOROOT {
			p.relevance = MaxRelevance
		}

		if !callback.dirFound(p) {
			return
		}
		var err error
		p.packageName, err = r.cache.CachePackageName(info)
		if err != nil {
			return
		}

		if !callback.packageNameLoaded(p) {
			return
		}
		if _, exports, err := r.loadExports(ctx, p, false); err == nil {
			callback.exportsLoaded(p, exports)
		}
	}
	stop := r.cache.ScanAndListen(ctx, processDir)
	defer stop()

	goenv, err := r.env.goEnv()
	if err != nil {
		return err
	}
	var roots []gopathwalk.Root
	roots = append(roots, gopathwalk.Root{Path: filepath.Join(goenv["GOROOT"], "src"), Type: gopathwalk.RootGOROOT})
	for _, p := range filepath.SplitList(goenv["GOPATH"]) {
		roots = append(roots, gopathwalk.Root{Path: filepath.Join(p, "src"), Type: gopathwalk.RootGOPATH})
	}
	// The callback is not necessarily safe to use in the goroutine below. Process roots eagerly.
	roots = filterRoots(roots, callback.rootFound)
	// We can't cancel walks, because we need them to finish to have a usable cache.
	scanDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-r.scanSema:
		}
		defer func() { r.scanSema <- struct{}{} }()
		gopathwalk.Walk(roots, add, gopathwalk.Options{Logf: r.env.Logf, ModulesEnabled: false})
		close(scanDone)
	}()
	select {
	case <-ctx.Done():
	case <-scanDone:
	}
	return nil
}

func (r *gopathResolver) scoreImportPath(ctx context.Context, path string) float64 {
	if stdlib.HasPackage(path) {
		return MaxRelevance
	}
	return MaxRelevance - 1
}

func filterRoots(roots []gopathwalk.Root, include func(gopathwalk.Root) bool) []gopathwalk.Root {
	var result []gopathwalk.Root
	for _, root := range roots {
		if !include(root) {
			continue
		}
		result = append(result, root)
	}
	return result
}

func (r *gopathResolver) loadExports(ctx context.Context, pkg *pkg, includeTest bool) (string, []stdlib.Symbol, error) {
	if info, ok := r.cache.Load(pkg.dir); ok && !includeTest {
		return r.cache.CacheExports(ctx, r.env, info)
	}
	return loadExportsFromFiles(ctx, r.env, pkg.dir, includeTest)
}

// VendorlessPath returns the devendorized version of the import path ipath.
// For example, VendorlessPath("foo/bar/vendor/a/b") returns "a/b".
func VendorlessPath(ipath string) string {
	// Devendorize for use in import statement.
	if i := strings.LastIndex(ipath, "/vendor/"); i >= 0 {
		return ipath[i+len("/vendor/"):]
	}
	if strings.HasPrefix(ipath, "vendor/") {
		return ipath[len("vendor/"):]
	}
	return ipath
}

func loadExportsFromFiles(ctx context.Context, env *ProcessEnv, dir string, includeTest bool) (string, []stdlib.Symbol, error) {
	// Look for non-test, buildable .go files which could provide exports.
	all, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, err
	}
	var files []fs.DirEntry
	for _, fi := range all {
		name := fi.Name()
		if !strings.HasSuffix(name, ".go") || (!includeTest && strings.HasSuffix(name, "_test.go")) {
			continue
		}
		match, err := env.matchFile(dir, fi.Name())
		if err != nil || !match {
			continue
		}
		files = append(files, fi)
	}

	if len(files) == 0 {
		return "", nil, fmt.Errorf("dir %v contains no buildable, non-test .go files", dir)
	}

	var pkgName string
	var exports []stdlib.Symbol
	fset := token.NewFileSet()
	for _, fi := range files {
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		default:
		}

		fullFile := filepath.Join(dir, fi.Name())
		// Legacy ast.Object resolution is needed here.
		f, err := parser.ParseFile(fset, fullFile, nil, 0)
		if err != nil {
			env.logf("error parsing %v: %v", fullFile, err)
			continue
		}
		if f.Name.Name == "documentation" {
			// Special case from go/build.ImportDir, not handled by MatchFile above.
			continue
		}
		if includeTest && strings.HasSuffix(f.Name.Name, "_test") {
			// x_test package. We want internal test files only.
			continue
		}
		pkgName = f.Name.Name
		for name, obj := range f.Scope.Objects {
			if ast.IsExported(name) {
				var kind stdlib.Kind
				switch obj.Kind {
				case ast.Con:
					kind = stdlib.Const
				case ast.Typ:
					kind = stdlib.Type
				case ast.Var:
					kind = stdlib.Var
				case ast.Fun:
					kind = stdlib.Func
				}
				exports = append(exports, stdlib.Symbol{
					Name:    name,
					Kind:    kind,
					Version: 0, // unknown; be permissive
				})
			}
		}
	}
	sortSymbols(exports)

	env.logf("loaded exports in dir %v (package %v): %v", dir, pkgName, exports)
	return pkgName, exports, nil
}

func sortSymbols(syms []stdlib.Symbol) {
	sort.Slice(syms, func(i, j int) bool {
		return syms[i].Name < syms[j].Name
	})
}

// A symbolSearcher searches for a package with a set of symbols, among a set
// of candidates. See [symbolSearcher.search].
type symbolSearcher struct {
	logf        func(string, ...any)
	srcDir      string // directory containing the file
	xtest       bool   // if set, the file containing is an x_test file
	loadExports func(ctx context.Context, pkg *pkg, includeTest bool) (string, []stdlib.Symbol, error)
}

// search searches the provided candidates for a package containing all
// exported symbols.
//
// If successful, returns the resulting package.
func (s *symbolSearcher) search(ctx context.Context, candidates []pkgDistance, pkgName string, symbols map[string]bool) (*pkg, error) {
	// Sort the candidates by their import package length, assuming that shorter
	// package names are better than long ones. this sorts by the de-vendored
	// name, so there's no "penalty" for vendoring.
	sort.Sort(byDistanceOrImportPathShortLength(candidates))
	if s.logf != nil {
		for i, c := range candidates {
			s.logf("%s candidate %d/%d: %v in %v", pkgName, i+1, len(candidates), c.pkg.importPathShort, c.pkg.dir)
		}
	}

	// Arrange rescv so that we can we can await results in order of relevance and exit as soon as we find the first match.
	rescv := make([]chan *pkg, len(candidates))
	for i := range candidates {
		rescv[i] = make(chan *pkg, 1)
	}
	const maxConcurrentPackageImport = 4
	loadExportsSem := make(chan struct{}, maxConcurrentPackageImport)

	// Ensure that all work is completed at exit.
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()

	// Start the search.
	wg.Go(func() {
		for i, c := range candidates {
			select {
			case loadExportsSem <- struct{}{}:
			case <-ctx.Done():
				return
			}

			i := i
			c := c
			wg.Add(1)
			go func() {
				defer func() {
					<-loadExportsSem
					wg.Done()
				}()
				if s.logf != nil {
					s.logf("loading exports in dir %s (seeking package %s)", c.pkg.dir, pkgName)
				}
				pkg, err := s.searchOne(ctx, c, symbols)
				if err != nil {
					if s.logf != nil && ctx.Err() == nil {
						s.logf("loading exports in dir %s (seeking package %s): %v", c.pkg.dir, pkgName, err)
					}
					pkg = nil
				}
				rescv[i] <- pkg // may be nil
			}()
		}
	})

	// Await the first (best) result.
	for _, resc := range rescv {
		select {
		case r := <-resc:
			if r != nil {
				return r, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, nil
}

func (s *symbolSearcher) searchOne(ctx context.Context, c pkgDistance, symbols map[string]bool) (*pkg, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// If we're considering the package under test from an x_test, load the test variant.
	includeTest := s.xtest && c.pkg.dir == s.srcDir
	_, exports, err := s.loadExports(ctx, c.pkg, includeTest)
	if err != nil {
		return nil, err
	}

	exportsMap := make(map[string]bool, len(exports))
	for _, sym := range exports {
		exportsMap[sym.Name] = true
	}
	for symbol := range symbols {
		if !exportsMap[symbol] {
			return nil, nil // no match
		}
	}
	return c.pkg, nil
}

// pkgIsCandidate reports whether pkg is a candidate for satisfying the
// finding which package pkgIdent in the file named by filename is trying to
// refer to.
func pkgIsCandidate(filename string, refs References, pkg *pkg) bool {
	// Check "internal" and "vendor" visibility:
	if !CanUse(filename, pkg.dir) {
		return false
	}

	// Speed optimization to minimize disk I/O:
	//
	// Use the matchesPath heuristic to filter to package paths that could
	// reasonably match a dangling reference.
	for pkgIdent := range refs {
		if matchesPath(pkgIdent, pkg.importPathShort) {
			return true
		}
	}
	return false
}

// CanUse reports whether the package in dir is usable from filename,
// respecting the Go "internal" and "vendor" visibility rules.
func CanUse(filename, dir string) bool {
	// Fast path check, before any allocations.
	if !strings.Contains(dir, "vendor") && !strings.Contains(dir, "internal") {
		return true
	}

	dirSlash := filepath.ToSlash(dir)
	if !strings.Contains(dirSlash, "/vendor/") && !strings.Contains(dirSlash, "/internal/") && !strings.HasSuffix(dirSlash, "/internal") {
		return true
	}
	// Vendor or internal directory only visible from children of parent.
	absfile, err := filepath.Abs(filename)
	if err != nil {
		return false
	}
	absdir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absfile, absdir)
	if err != nil {
		return false
	}
	relSlash := filepath.ToSlash(rel)
	if i := strings.LastIndex(relSlash, "../"); i >= 0 {
		relSlash = relSlash[i+len("../"):]
	}
	return !strings.Contains(relSlash, "/vendor/") && !strings.Contains(relSlash, "/internal/") && !strings.HasSuffix(relSlash, "/internal")
}

// matchesPath reports whether ident may match a potential package name
// referred to by path, using heuristics to filter out unidiomatic package
// names. The segment-matching heuristic must allow for various conventions
// around segment naming, including go-foo, foo-go, and foo.v3. So the segment
// foo-go matches all of the following identifiers: foo, go, and foogo. All
// matches are case insensitive (for ASCII identifiers).
func matchesPath(ident, path string) bool {
	// Ignore case, for ASCII.
	lowerIfASCII := func(b byte) byte {
		if 'A' <= b && b <= 'Z' {
			return b + ('a' - 'A')
		}
		return b
	}

	// match reports whether path[start:end] matches ident, ignoring [.-].
	match := func(start, end int) bool {
		ii := len(ident) - 1 // current byte in ident
		pi := end - 1        // current byte in path
		for ; pi >= start && ii >= 0; pi-- {
			pb := path[pi]
			if pb == '-' || pb == '.' {
				continue
			}
			pb = lowerIfASCII(pb)
			ib := lowerIfASCII(ident[ii])
			if pb != ib {
				return false
			}
			ii--
		}
		return ii < 0 && pi < start // all bytes matched
	}

	// segmentEnd and subsegmentEnd hold the end points of the current segment and subsegment intervals.
	segmentEnd := len(path)
	subsegmentEnd := len(path)

	// Count slashes; we only care about the last segments.
	nslash := 0

	for i := len(path) - 1; i >= 0; i-- {
		switch b := path[i]; b {
		// TODO(rfindley): we handle backlashes here only because the heuristic
		// handled backslashes.
		case '/', '\\':
			if match(i+1, segmentEnd) || match(i+1, subsegmentEnd) {
				return true
			}
			nslash++
			if nslash == 2 {
				return false // did not match above
			}
			segmentEnd, subsegmentEnd = i, i // reset
		case '-', '.':
			if match(i+1, subsegmentEnd) {
				return true
			}
			subsegmentEnd = i
		}
	}
	return match(0, segmentEnd) || match(0, subsegmentEnd)
}

type visitFn func(node ast.Node) ast.Visitor

func (fn visitFn) Visit(node ast.Node) ast.Visitor {
	return fn(node)
}

func symbolNameSet(symbols []stdlib.Symbol) map[string]bool {
	names := make(map[string]bool)
	for _, sym := range symbols {
		switch sym.Kind {
		case stdlib.Const, stdlib.Var, stdlib.Type, stdlib.Func:
			names[sym.Name] = true
		}
	}
	return names
}
