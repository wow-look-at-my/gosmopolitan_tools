// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package inline

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/constant"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	pathpkg "path"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/types/typeutil"
	internalastutil "golang.org/x/tools/internal/astutil"
	"golang.org/x/tools/internal/astutil/free"
	"golang.org/x/tools/internal/packagepath"
	"golang.org/x/tools/internal/refactor"
	"golang.org/x/tools/internal/typeparams"
	"golang.org/x/tools/internal/typesinternal"
	"golang.org/x/tools/internal/versions"
)

// A Caller describes the function call and its enclosing context.
//
// The client is responsible for populating this struct and passing it to Inline.
type Caller struct {
	Fset  *token.FileSet
	Types *types.Package
	Info  *types.Info
	File  *ast.File
	Call  *ast.CallExpr

	// CountUses is an optional optimized computation of the number of times pkgname appears in Info.Uses.
	CountUses func(pkgname *types.PkgName) int

	path          []ast.Node    // path from call to root of file syntax tree
	enclosingFunc *ast.FuncDecl // top-level function/method enclosing the call, if any
}

type logger = func(string, ...any)

// Options specifies parameters affecting the inliner algorithm.
// All fields are optional.
type Options struct {
	Logf          logger // log output function, records decision-making process
	IgnoreEffects bool   // ignore potential side effects of arguments (unsound)
	Recover       bool   // catch panics from inliner and report as errors (for ill-typed ASTs)
}

// Result holds the result of code transformation.
type Result struct {
	Edits       []refactor.Edit // edits around CallExpr and imports
	Literalized bool            // chosen strategy replaced callee() with func(){...}()
	BindingDecl bool            // transformation added "var params = args" declaration
}

// Inline inlines the called function (callee) into the function call (caller)
// and returns the updated, formatted content of the caller source file.
//
// Inline does not mutate any public fields of Caller or Callee.
func Inline(caller *Caller, callee *Callee, opts *Options) (res *Result, err error) {
	if opts == nil {
		opts = new(Options)
	} else {
		opts = new(*opts)
	}
	// Set default options.
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.Recover {
		defer func() {
			if x := recover(); x != nil {
				err = fmt.Errorf("inlining failed (%q), likely because inputs were ill-typed", x)
			}
		}()
	}

	st := &state{
		caller: caller,
		callee: callee,
		opts:   opts,
	}
	return st.inline()
}

// state holds the working state of the inliner.
type state struct {
	caller *Caller
	callee *Callee
	opts   *Options
}

func (st *state) inline() (*Result, error) {
	logf, caller, callee := st.opts.Logf, st.caller, st.callee

	logf("inline %s @ %v",
		debugFormatNode(caller.Fset, caller.Call),
		caller.Fset.PositionFor(caller.Call.Lparen, false))

	if ast.IsGenerated(caller.File) {
		return nil, fmt.Errorf("cannot inline calls from generated files")
	}

	res, err := st.inlineCall()
	if err != nil {
		return nil, err
	}

	// Replace the call (or some node that encloses it) by new syntax.
	assert(res.old != nil, "old is nil")
	assert(res.new != nil, "new is nil")

	// A single return operand inlined to a unary expression context may need
	// parens.
	if new, ok := res.new.(ast.Expr); ok {
		parent := caller.path[slices.Index(caller.path, res.old)+1]
		res.new = internalastutil.MaybeParenthesize(parent, res.old.(ast.Expr), new)
	}

	// Some reduction strategies return a new block holding the callee's statements.
	elideBraces := res.elideBraces
	if !elideBraces {
		if newBlock, ok := res.new.(*ast.BlockStmt); ok {
			i := slices.Index(caller.path, res.old)
			parent := caller.path[i+1]
			var body []ast.Stmt
			switch parent := parent.(type) {
			case *ast.BlockStmt:
				body = parent.List
			case *ast.CommClause:
				body = parent.Body
			case *ast.CaseClause:
				body = parent.Body
			}
			if body != nil {
				callerNames := declares(body)

				// If BlockStmt is a function body,
				// include its receiver, params, and results.
				addFieldNames := func(fields *ast.FieldList) {
					if fields != nil {
						for _, field := range fields.List {
							for _, id := range field.Names {
								callerNames[id.Name] = true
							}
						}
					}
				}
				switch f := caller.path[i+2].(type) {
				case *ast.FuncDecl:
					addFieldNames(f.Recv)
					addFieldNames(f.Type.Params)
					addFieldNames(f.Type.Results)
				case *ast.FuncLit:
					addFieldNames(f.Type.Params)
					addFieldNames(f.Type.Results)
				}

				if len(callerLabels(caller.path)) > 0 {
					// TODO(adonovan): be more precise and reject only forward gotos across the inlined block.
					logf("keeping block braces: caller uses control labels")
				} else if intersects(declares(newBlock.List), callerNames) {
					logf("keeping block braces: avoids name conflict")
				} else {
					elideBraces = true
				}
			}
		}
	}

	var edits []refactor.Edit

	// Format the cloned callee.
	{
		// TODO(adonovan): might it make more sense to use callee.Fset when formatting res.new?

		var out bytes.Buffer
		if elideBraces {
			for i, stmt := range res.new.(*ast.BlockStmt).List {
				if i > 0 {
					out.WriteByte('\n')
				}
				if err := format.Node(&out, caller.Fset, stmt); err != nil {
					return nil, err
				}
			}
		} else {
			if err := format.Node(&out, caller.Fset, res.new); err != nil {
				return nil, err
			}
		}

		edits = append(edits, refactor.Edit{
			Pos:     res.old.Pos(),
			End:     res.old.End(),
			NewText: out.Bytes(),
		})
	}

	// Add new imports.
	//
	// It's possible that not all are needed (e.g. for type names that melted
	// away), but we'll let the client (such as an analysis driver) clean it up
	// since it must remove unused imports anyway.
	for _, imp := range res.newImports {
		// Check that the new imports are accessible.
		if !packagepath.CanImport(caller.Types.Path(), imp.path) {
			return nil, fmt.Errorf("can't inline function %v as its body refers to inaccessible package %q", callee, imp.path)
		}

		// We've already validated the import, so we call AddImportEdits directly to compute the edit.
		name := ""
		if imp.explicit {
			name = imp.name
		}
		edits = append(edits, refactor.AddImportEdits(caller.File, name, imp.path)...)
	}

	literalized := false
	if call, ok := res.new.(*ast.CallExpr); ok && is[*ast.FuncLit](call.Fun) {
		literalized = true
	}

	// Delete imports referenced only by caller.Call.Fun.
	//
	// It's ambiguous to let the client (e.g. analysis driver) remove unneeded
	// imports in this case. This is because it is common to inlining a call from
	// "dir1/a".F to "dir2/a".F. This leaves imports of packages named 'a', both
	// providing a.F.
	//
	// However, the only import deletion tools at our disposal are
	// astutil.DeleteNamedImport, which mutates the AST, and
	// refactor.Delete{Spec,Decl}, which need a Cursor. So we need to reinvent
	// the wheel here.
	for _, oldImport := range res.oldImports {
		spec := oldImport.spec

		// Include adjacent comments.
		pos := spec.Pos()
		if doc := spec.Doc; doc != nil {
			pos = doc.Pos()
		}
		end := spec.End()
		if doc := spec.Comment; doc != nil {
			end = doc.End()
		}

		// Find the enclosing import decl.
		// If it's paren-less, we must delete it too.
		for _, decl := range caller.File.Decls {
			decl, ok := decl.(*ast.GenDecl)
			if !(ok && decl.Tok == token.IMPORT) {
				break // stop at first non-import decl
			}
			if internalastutil.NodeContainsPos(decl, spec.Pos()) && !decl.Rparen.IsValid() {
				// Include adjacent comments.
				pos = decl.Pos()
				if doc := decl.Doc; doc != nil {
					pos = doc.Pos()
				}
				end = decl.End()
				break
			}
		}

		edits = append(edits, refactor.Edit{
			Pos: pos,
			End: end,
		})
	}

	return &Result{
		Edits:       edits,
		Literalized: literalized,
		BindingDecl: res.bindingDecl,
	}, nil
}

// An oldImport is an import that will be deleted from the caller file.
type oldImport struct {
	pkgName *types.PkgName
	spec    *ast.ImportSpec
}

// A newImport is an import that will be added to the caller file.
type newImport struct {
	name     string
	path     string
	explicit bool // use name as ImportSpec.Name
}

// importState tracks information about imports.
type importState struct {
	logf       func(string, ...any)
	caller     *Caller
	importMap  map[string][]string // from package paths in the caller's file to local names
	newImports []newImport         // for references to free names in callee; to be added to the file
	oldImports []oldImport         // referenced only by caller.Call.Fun; to be removed from the file
}

// newImportState returns an importState with initial information about the caller's imports.
func newImportState(logf func(string, ...any), caller *Caller, callee *gobCallee) *importState {
	// For simplicity we ignore existing dot imports, so that a qualified
	// identifier (QI) in the callee is always represented by a QI in the caller.
	ist := &importState{
		logf:      logf,
		caller:    caller,
		importMap: make(map[string][]string),
	}

	// Provide an inefficient default implementation of CountUses. (Ideally clients amortize this for the entire package.)
	countUses := caller.CountUses
	if countUses == nil {
		uses := make(map[*types.PkgName]int)
		for _, obj := range caller.Info.Uses {
			if pkgname, ok := obj.(*types.PkgName); ok {
				uses[pkgname]++
			}
		}
		countUses = func(pkgname *types.PkgName) int {
			return uses[pkgname]
		}
	}

	for _, imp := range caller.File.Imports {
		if pkgName, ok := importedPkgName(caller.Info, imp); ok &&
			pkgName.Name() != "." &&
			pkgName.Name() != "_" {

			// Where p.F is a qualified identifier, the p import may not be necessary.
			needed := true
			if sel, ok := ast.Unparen(caller.Call.Fun).(*ast.SelectorExpr); ok &&
				is[*ast.Ident](sel.X) &&
				caller.Info.Uses[sel.X.(*ast.Ident)] == pkgName &&
				countUses(pkgName) == 1 {
				needed = false // no longer needed by caller
				// Check to see if any of the inlined free objects need this package.
				for _, obj := range callee.FreeObjs {
					if obj.PkgPath == pkgName.Imported().Path() && obj.Shadow[pkgName.Name()] == 0 {
						needed = true // needed by callee
						break
					}
				}
			}

			// Exclude imports not needed by the caller or callee after inlining; the second
			// return value holds these.
			if needed {
				path := pkgName.Imported().Path()
				ist.importMap[path] = append(ist.importMap[path], pkgName.Name())
			} else {
				ist.oldImports = append(ist.oldImports, oldImport{pkgName: pkgName, spec: imp})
			}
		}
	}
	return ist
}

// importName finds an existing import name to use in a particular shadowing
// context. It is used to determine the set of new imports in localName, and
// is also used for writing out names in inlining strategies below.
func (i *importState) importName(pkgPath string, shadow shadowMap) string {
	for _, name := range i.importMap[pkgPath] {
		// Check that either the import preexisted, or that it was newly added
		// (no PkgName) but is not shadowed, either in the callee (shadows) or
		// caller (caller.lookup).
		if shadow[name] == 0 {
			found := i.caller.lookup(name)
			if is[*types.PkgName](found) || found == nil {
				return name
			}
		}
	}
	return ""
}

// findNewLocalName returns a new local package name to use in a particular shadowing context.
// It considers the existing local name used by the callee, or construct a new local name
// based on the package name.
func (i *importState) findNewLocalName(pkgName, calleePkgName string, shadow shadowMap) string {
	newlyAdded := func(name string) bool {
		return slices.ContainsFunc(i.newImports, func(n newImport) bool { return n.name == name })
	}

	// shadowedInCaller reports whether a candidate package name
	// already refers to a declaration in the caller.
	shadowedInCaller := func(name string) bool {
		obj := i.caller.lookup(name)
		if obj == nil {
			return false
		}
		// If obj will be removed, the name is available.
		return !slices.ContainsFunc(i.oldImports, func(o oldImport) bool { return o.pkgName == obj })
	}

	// import added by callee
	//
	// Try to preserve the local package name used by the callee first.
	if shadow[calleePkgName] == 0 && !shadowedInCaller(calleePkgName) && !newlyAdded(calleePkgName) && calleePkgName != "init" {
		return calleePkgName
	}

	base := pkgName
	name := base
	for n := 0; shadow[name] != 0 || shadowedInCaller(name) || newlyAdded(name) || name == "init"; n++ {
		name = fmt.Sprintf("%s%d", base, n)
	}

	return name
}

// localName returns the local name for a given imported package path,
// adding one if it doesn't exists.
func (i *importState) localName(pkgPath, pkgName, calleePkgName string, shadow shadowMap) string {
	// Does an import already exist that works in this shadowing context?
	if name := i.importName(pkgPath, shadow); name != "" {
		return name
	}

	name := i.findNewLocalName(pkgName, calleePkgName, shadow)
	i.logf("adding import %s %q", name, pkgPath)
	// Use explicit pkgname (out of necessity) when it differs from the declared name,
	// or (for good style) when it differs from base(pkgpath).
	i.newImports = append(i.newImports, newImport{
		name:     name,
		path:     pkgPath,
		explicit: name != pkgName || name != pathpkg.Base(pkgPath),
	})
	i.importMap[pkgPath] = append(i.importMap[pkgPath], name)
	return name
}

type inlineCallResult struct {
	newImports []newImport // to add
	oldImports []oldImport // to remove

	// If elideBraces is set, old is an ast.Stmt and new is an ast.BlockStmt to be spliced in.
	elideBraces bool
	bindingDecl bool     // transformation inserted "var params = args" declaration
	old, new    ast.Node // e.g. replace call expr by callee function body expression
}

// inlineCall returns a pair of an old node (the call, or something enclosing
// it) and a new node (its replacement, which may be a combination of caller,
// callee, and new nodes), along with the set of new imports needed.
//
// TODO(adonovan): rethink the 'result' interface. The assumption of a
// one-to-one replacement seems fragile. One can easily imagine the
// transformation replacing the call and adding new variable declarations.
// That transformation is for example, or replacing a call statement by zero
// or many statements.) NOTE(rfindley). We've sort-of done this, with the
// 'elideBraces' flag that allows inlining a statement list. However, due to
// loss of comments, more sophisticated rewrites are challenging.
//
// TODO(rfindley): see if we can reduce the amount of comment lossiness by
// using printer.CommentedNode, which has been useful elsewhere.
func (st *state) inlineCall() (*inlineCallResult, error) {
	logf, caller, callee := st.opts.Logf, st.caller, &st.callee.impl

	checkInfoFields(caller.Info)

	// Inlining of dynamic calls is not supported, even for local closure calls.
	calleeSymbol := typeutil.StaticCallee(caller.Info, caller.Call)
	if calleeSymbol == nil {
		// e.g. interface method
		return nil, fmt.Errorf("cannot inline: not a static function call")
	}

	// Reject cross-package inlining if callee has free references to unexported symbols.
	samePkg := caller.Types.Path() == callee.PkgPath
	if !samePkg && len(callee.Unexported) > 0 {
		return nil, fmt.Errorf("cannot inline call to %s because body refers to non-exported %s",
			callee.Name, callee.Unexported[0])
	}

	callerGoVersion := caller.Info.FileVersions[caller.File]
	if callerGoVersion != "" && callee.GoVersion != "" && versions.Before(callerGoVersion, callee.GoVersion) {
		return nil, fmt.Errorf("cannot inline call to %s (declared using %s) into a file using %s",
			callee.Name, callee.GoVersion, callerGoVersion)
	}

	// -- analyze callee's free references in caller context --

	// Compute syntax path enclosing Call, innermost first (Path[0]=Call), and outermost enclosing function, if any.
	caller.path, _ = astutil.PathEnclosingInterval(caller.File, caller.Call.Pos(), caller.Call.End())
	for _, n := range caller.path {
		if decl, ok := n.(*ast.FuncDecl); ok {
			caller.enclosingFunc = decl
			break
		}
	}

	// If call is within a function, analyze all its local vars for the "single assignment" property.
	var assign1 func(v *types.Var) bool // reports whether v a single-assignment local var
	{
		updatedLocals := make(map[*types.Var]bool)
		if caller.enclosingFunc != nil {
			escape(caller.Info, caller.enclosingFunc, func(v *types.Var, _ bool) {
				updatedLocals[v] = true
			})
			logf("multiple-assignment vars: %v", updatedLocals)
		}
		assign1 = func(v *types.Var) bool { return !updatedLocals[v] }
	}

	// Extract information about the caller's imports.
	istate := newImportState(logf, caller, callee)

	// Compute the renaming of the callee's free identifiers.
	objRenames, err := st.renameFreeObjs(istate)
	if err != nil {
		return nil, err
	}

	res := &inlineCallResult{
		newImports: istate.newImports,
		oldImports: istate.oldImports,
	}

	// Parse callee function declaration.
	calleeFset, calleeDecl, err := parseCompact(callee.Content)
	if err != nil {
		return nil, err // "can't happen"
	}

	// replaceCalleeID replaces an identifier in the callee. See [replacer] for
	// more detailed semantics.
	replaceCalleeID := func(offset int, repl ast.Expr, unpackVariadic bool) {
		path, id := findIdent(calleeDecl, calleeDecl.Pos()+token.Pos(offset))
		logf("- replace id %q @ #%d to %q", id.Name, offset, debugFormatNode(calleeFset, repl))
		// Replace f([]T{a, b, c}...) with f(a, b, c).
		if lit, ok := repl.(*ast.CompositeLit); ok && unpackVariadic && len(path) > 0 {
			if call, ok := last(path).(*ast.CallExpr); ok &&
				call.Ellipsis.IsValid() &&
				id == last(call.Args) {

				call.Args = append(call.Args[:len(call.Args)-1], lit.Elts...)
				call.Ellipsis = token.NoPos
				return
			}
		}
		if len(path) > 0 {
			repl = internalastutil.MaybeParenthesize(last(path), id, repl)
		}
		replaceNode(calleeDecl, id, repl)
	}

	// Generate replacements for each free identifier.
	// (The same tree may be spliced in multiple times, resulting in a DAG.)
	for _, ref := range callee.FreeRefs {
		if repl := objRenames[ref.Object]; repl != nil {
			replaceCalleeID(ref.Offset, repl, false)
		}
	}

	// Gather the effective call arguments, including the receiver.
	args, err := st.arguments(caller, calleeDecl, assign1)
	if err != nil {
		return nil, err // e.g. implicit field selection cannot be made explicit
	}

	// Gather effective parameter tuple, including the receiver if any.
	var params []*parameter // including receiver; nil => parameter substituted
	{
		sig := calleeSymbol.Type().(*types.Signature)
		if sig.Recv() != nil {
			params = append(params, &parameter{
				obj:       sig.Recv(),
				fieldType: calleeDecl.Recv.List[0].Type,
				info:      callee.Params[0],
			})
		}

		// Flatten the list of syntactic types.
		var types []ast.Expr
		for _, field := range calleeDecl.Type.Params.List {
			if field.Names == nil {
				types = append(types, field.Type)
			} else {
				for range field.Names {
					types = append(types, field.Type)
				}
			}
		}

		for i := 0; i < sig.Params().Len(); i++ {
			params = append(params, &parameter{
				obj:       sig.Params().At(i),
				fieldType: types[i],
				info:      callee.Params[len(params)],
			})
		}

		// Variadic function?
		//
		// There are possible types of call: - ordinary f(a1, ..., aN) - ellipsis
		// f(a1, ..., slice...) - spread f(recv?, g()) where g() is a tuple.
		// Fortunately it is vanishingly rare.
		//
		// TODO(adonovan): extract this to a function.
		if sig.Variadic() {
			lastParam := last(params)
			if len(args) > 0 && last(args).spread {
				// spread call to variadic: tricky
				lastParam.variadic = true
			} else {
				// ordinary/ellipsis call to variadic

				// simplify decl: func(T...) -> func([]T)
				var lastParamFieldType ast.Expr
				if len(calleeDecl.Type.Params.List) > 0 {
					lastParamField := last(calleeDecl.Type.Params.List)
					if ellipsis, ok := lastParamField.Type.(*ast.Ellipsis); ok {
						lastParamField.Type = &ast.ArrayType{
							Elt: ellipsis.Elt,
						}
					}
					lastParamFieldType = lastParamField.Type
				}

				if caller.Call.Ellipsis.IsValid() {
				} else {
					// ordinary call.
					n := len(params) - 1
					ordinary, extra := args[:n], args[n:]
					var elts []ast.Expr
					freevars := make(map[string]bool)
					pure, effects := true, false
					for _, arg := range extra {
						elts = append(elts, arg.expr)
						pure = pure && arg.pure
						effects = effects || arg.effects
						maps.Copy(freevars, arg.freevars)
					}
					args = append(ordinary, &argument{
						expr: &ast.CompositeLit{
							Type: lastParamFieldType,
							Elts: elts,
						},
						typ:        lastParam.obj.Type(),
						constant:   nil,
						pure:       pure,
						effects:    effects,
						duplicable: false,
						freevars:   freevars,
						variadic:   true,
					})
				}
			}
		}
	}

	// Substitute type parameters in calleeDecl AST with type arguments from the
	// call, and synchronize the parameter metadata.
	{
		typeArgs := st.typeArguments(caller.Call)
		if len(typeArgs) != len(callee.TypeParams) {
			return nil, fmt.Errorf("cannot inline: type parameter inference is not yet supported")
		}
		if err := substituteTypeParams(logf, callee.TypeParams, typeArgs, replaceCalleeID); err != nil {
			return nil, err
		}
		// Synchronize the parameters' type pointers with the mutated calleeDecl.
		syncParamFieldTypes(calleeDecl, params)
	}

	// Log effective arguments.
	for i, arg := range args {
		logf("arg #%d: %s pure=%t effects=%t duplicable=%t free=%v type=%v",
			i, debugFormatNode(caller.Fset, arg.expr),
			arg.pure, arg.effects, arg.duplicable, arg.freevars, arg.typ)
	}

	// Note: computation below should be expressed in terms of the args and params slices, not the raw material.

	// Perform parameter substitution. May eliminate some elements of params/args.
	substitute(logf, caller, params, args, callee.Effects, callee.Falcon, replaceCalleeID)

	// Update the callee's signature syntax.
	updateCalleeParams(calleeDecl, params)

	// Create a var (param = arg; ...) decl for use by some strategies.
	bindingDecl := createBindingDecl(logf, caller, args, calleeDecl, callee.Results)

	var remainingArgs []ast.Expr
	for _, arg := range args {
		if arg != nil {
			remainingArgs = append(remainingArgs, arg.expr)
		}
	}

	// -- let the inlining strategies begin --
	//
	// When we commit to a strategy, we log a message of the form:
	//
	//   "strategy: reduce expr-context call to { return expr }"
	//
	// This is a terse way of saying:
	//
	//    we plan to reduce a call
	//    that appears in expression context
	//    to a function whose body is of the form { return expr }

	// TODO(adonovan): split this huge function into a sequence of function calls
	// with an error sentinel that means "try the next strategy". It make sure
	// each strategy writes to the log the reason it didn't match.

	// Special case: eliminate a call to a function whose body is empty.
	// (=> callee has no results and caller is a statement.)
	//
	//    func f(params) {}
	//    f(args)
	//    => _, _ = args
	if len(calleeDecl.Body.List) == 0 {
		logf("strategy: reduce call to empty body")

		// Evaluate the arguments for effects and delete the call entirely. TODO:
		// discard go or defer statements as well.
		if stmt := callStmt(caller.path, false); stmt != nil {
			res.old = stmt
			if nargs := len(remainingArgs); nargs > 0 {
				// Emit "_, _ = args" to discard results.

				// TODO(adonovan): if args is the []T{a1, ..., an} literal synthesized
				// during variadic simplification, consider unwrapping it to its (pure)
				// elements. Perhaps there's no harm doing this for any slice literal.

				// Make correction for spread calls
				// f(g()) or recv.f(g()) where g() is a tuple.
				if last := last(args); last != nil && last.spread {
					nspread := last.typ.(*types.Tuple).Len()
					if len(args) > 1 { // [recv, g()]
						// A single AssignStmt cannot discard both, so use a 2-spec var decl.
						res.new = &ast.GenDecl{
							Tok: token.VAR,
							Specs: []ast.Spec{
								&ast.ValueSpec{
									Names:  []*ast.Ident{makeIdent("_")},
									Values: []ast.Expr{args[0].expr},
								},
								&ast.ValueSpec{
									Names:  blanks[*ast.Ident](nspread),
									Values: []ast.Expr{args[1].expr},
								},
							},
						}
						return res, nil
					}

					// Sole argument is spread call.
					nargs = nspread
				}

				res.new = &ast.AssignStmt{
					Lhs: blanks[ast.Expr](nargs),
					Tok: token.ASSIGN,
					Rhs: remainingArgs,
				}

			} else {
				// No remaining arguments: delete call statement entirely
				res.new = &ast.EmptyStmt{}
			}
			return res, nil
		}
	}

	// If all parameters have been substituted and no result variable is referenced, we don't need a binding decl.
	allResultsUnreferenced := forall(callee.Results, func(i int, r *paramInfo) bool { return len(r.Refs) == 0 })
	needBindingDecl := !allResultsUnreferenced ||
		exists(params, func(i int, p *parameter) bool { return p != nil })

	// Both strategies below overlap for a tail call of {return exprs}: The
	// expr-context reduction is nice. This is because it keeps the caller's
	// return stmt and merely switches its operand, without introducing a new
	// block. But it doesn't work with implicit return conversions.
	//
	// TODO(adonovan): unify these cases more cleanly, allowing return- operand
	// replacement and implicit conversions, by adding conversions around each
	// return operand (if not a spread return).

	// Special case: call to { return exprs }.
	//
	// Reduces to: { var (bindings); _, _ = exprs } or _, _ = exprs or expr
	//
	// If:
	// - the body is "return expr" with trivial implicit conversions,
	//   or the caller's return type matches the callee's,
	// - all parameters and result vars can be eliminated
	//   or replaced by a binding decl,
	// then the call expression can be replaced by the
	// callee's body expression, suitably substituted.
	if len(calleeDecl.Body.List) == 1 &&
		is[*ast.ReturnStmt](calleeDecl.Body.List[0]) &&
		len(calleeDecl.Body.List[0].(*ast.ReturnStmt).Results) > 0 { // not a bare return
		results := calleeDecl.Body.List[0].(*ast.ReturnStmt).Results

		parent, grandparent := callContext(caller.path)

		// statement context
		if stmt, ok := parent.(*ast.ExprStmt); ok &&
			(!needBindingDecl || bindingDecl != nil) {
			logf("strategy: reduce stmt-context call to { return exprs }")
			clearPositions(calleeDecl.Body)

			if callee.ValidForCallStmt {
				logf("callee body is valid as statement")
				// Inv: len(results) == 1
				if !needBindingDecl {
					// Reduces to: expr
					res.old = caller.Call
					res.new = results[0]
				} else {
					// Reduces to: { var (bindings); expr }
					res.bindingDecl = true
					res.old = stmt
					res.new = &ast.BlockStmt{
						List: []ast.Stmt{
							bindingDecl.stmt,
							&ast.ExprStmt{X: results[0]},
						},
					}
				}
			} else {
				logf("callee body is not valid as statement")
				// The call is a standalone statement, but the callee body is not suitable
				// as a standalone statement (f() or <-ch), explicitly discard the
				// results.
				discard := &ast.AssignStmt{
					Lhs: blanks[ast.Expr](callee.NumResults),
					Tok: token.ASSIGN,
					Rhs: results,
				}
				res.old = stmt
				if !needBindingDecl {
					// Reduces to: _, _ = exprs
					res.new = discard
				} else {
					// Reduces to: { var (bindings); _, _ = exprs }
					res.bindingDecl = true
					res.new = &ast.BlockStmt{
						List: []ast.Stmt{
							bindingDecl.stmt,
							discard,
						},
					}
				}
			}
			return res, nil
		}

		// Assignment context.
		//
		// An assignment a, b := f() can be reduced to a, b := x, y. This happens
		// if there is no binding decl, or if the binding decl declares no names.
		if stmt, ok := parent.(*ast.AssignStmt); ok &&
			is[*ast.BlockStmt](grandparent) &&
			(!needBindingDecl || (bindingDecl != nil && len(bindingDecl.names) == 0)) {

			// Reduces to: { var (bindings); lhs... := rhs... }
			if newStmts, ok := st.assignStmts(stmt, results, istate.importName); ok {
				logf("strategy: reduce assign-context call to { return exprs }")

				clearPositions(calleeDecl.Body)

				block := &ast.BlockStmt{
					List: newStmts,
				}
				if needBindingDecl {
					res.bindingDecl = true
					block.List = prepend(bindingDecl.stmt, block.List...)
				}

				// assignStmts does not introduce new bindings.
				res.elideBraces = true
				res.old = stmt
				res.new = block
				return res, nil
			}
		}

		// expression context
		if !needBindingDecl {
			clearPositions(calleeDecl.Body)

			anyNonTrivialReturns := hasNonTrivialReturn(callee.Returns)

			if callee.NumResults == 1 {
				logf("strategy: reduce expr-context call to { return expr }")
				// (includes some simple tail-calls)

				// Make implicit return conversion explicit.
				if anyNonTrivialReturns {
					results[0] = convert(calleeDecl.Type.Results.List[0].Type, results[0])
				}

				res.old = caller.Call
				res.new = results[0]
				return res, nil

			} else if !anyNonTrivialReturns {
				logf("strategy: reduce spread-context call to { return expr }")
				// There is no general way to reify conversions in a spread return, hence the requirement above.

				// The call returns multiple results but is not a standalone call statement.
				res.old = parent
				switch context := parent.(type) {
				case *ast.AssignStmt:
					// Inv: the call must be in Rhs[0], not Lhs.
					assign := shallowCopy(context)
					assign.Rhs = results
					res.new = assign
				case *ast.ValueSpec:
					// Inv: the call must be in Values[0], not Names.
					spec := shallowCopy(context)
					spec.Values = results
					res.new = spec
				case *ast.CallExpr:
					// Inv: the call must be in Args[0], not Fun.
					call := shallowCopy(context)
					call.Args = results
					res.new = call
				case *ast.ReturnStmt:
					// Inv: the call must be Results[0].
					ret := shallowCopy(context)
					ret.Results = results
					res.new = ret
				default:
					return nil, fmt.Errorf("internal error: unexpected context %T for spread call", context)
				}
				return res, nil
			}
		}
	}

	// Special case: tail-call.
	parent, _ := callContext(caller.path)
	if ret, ok := parent.(*ast.ReturnStmt); ok &&
		len(ret.Results) == 1 &&
		tailCallSafeReturn(caller, calleeSymbol, callee) &&
		!callee.HasBareReturn &&
		(!needBindingDecl || bindingDecl != nil) &&
		!hasLabelConflict(caller.path, callee.Labels) &&
		allResultsUnreferenced {
		logf("strategy: reduce tail-call")
		body := calleeDecl.Body
		clearPositions(body)
		if needBindingDecl {
			res.bindingDecl = true
			body.List = prepend(bindingDecl.stmt, body.List...)
		}
		res.old = ret
		res.new = body
		return res, nil
	}

	// Special case: call to void function.
	if stmt := callStmt(caller.path, true); stmt != nil &&
		(!needBindingDecl || bindingDecl != nil) &&
		!callee.HasDefer &&
		!hasLabelConflict(caller.path, callee.Labels) &&
		len(callee.Returns) == 0 {
		logf("strategy: reduce stmt-context call to { stmts }")
		body := calleeDecl.Body
		var repl ast.Stmt = body
		clearPositions(repl)
		if needBindingDecl {
			body.List = prepend(bindingDecl.stmt, body.List...)
		}
		res.old = stmt
		res.new = repl
		return res, nil
	}

	// TODO(adonovan): parameterless call to { stmts. Return expr } from one of
	// these contexts: x, y = f() x, y := f() var x, y = f() => var (x T1, y T2).

	// Literalization isn't infallible.
	if len(args) == 2 && args[0] != nil && args[1] != nil && is[*types.Tuple](args[1].typ) {
		return nil, fmt.Errorf("can't yet inline spread call to method")
	}

	// Infallible general case: literalization.
	logf("strategy: literalization")
	funcLit := &ast.FuncLit{
		Type: calleeDecl.Type,
		Body: calleeDecl.Body,
	}
	// clear positions before prepending the binding decl below, since the binding decl contains syntax from the caller and we must not mutate the caller.
	clearPositions(funcLit)

	// Literalization can still use a binding decl as it gives a more natural
	// reading order:.
	if bindingDecl != nil && allResultsUnreferenced {
		funcLit.Type.Params.List = nil
		remainingArgs = nil
		res.bindingDecl = true
		funcLit.Body.List = prepend(bindingDecl.stmt, funcLit.Body.List...)
	}

	// Emit a new call to a function literal in place of
	// the callee name, with appropriate replacements.
	newCall := &ast.CallExpr{
		Fun:      funcLit,
		Ellipsis: token.NoPos, // f(slice...) is always simplified
		Args:     remainingArgs,
	}
	res.old = caller.Call
	res.new = newCall
	return res, nil
}

// renameFreeObjs computes the renaming of the callee's free identifiers.
// It returns a slice of names (identifiers or selector expressions) corresponding
// to the callee's free objects (gobCallee.FreeObjs).
func (st *state) renameFreeObjs(istate *importState) ([]ast.Expr, error) {
	caller, callee := st.caller, &st.callee.impl
	objRenames := make([]ast.Expr, len(callee.FreeObjs)) // nil => no change
	for i, obj := range callee.FreeObjs {
		// obj is a free object of the callee.

		// We must consider potential shadowing both at the caller side (caller.lookup) and, when choosing new PkgNames, within the callee.

		var newName ast.Expr
		if obj.Kind == "pkgname" {
			// Use locally appropriate import, creating as needed.
			n := istate.localName(obj.PkgPath, obj.PkgName, obj.Name, obj.Shadow)
			newName = makeIdent(n) // imported package
		} else if !obj.ValidPos {
			// Built-in function, type, or value (e.g. nil, zero): check not shadowed at caller.
			found := caller.lookup(obj.Name) // always finds something
			if found.Pos().IsValid() {
				return nil, fmt.Errorf("cannot inline, because the callee refers to built-in %q, which in the caller is shadowed by a %s (declared at line %d)",
					obj.Name, objectKind(found),
					caller.Fset.PositionFor(found.Pos(), false).Line)
			}

		} else {
			// Must be reference to package-level var/func/const/type, since type parameters are not yet supported.
			qualify := false
			if obj.PkgPath == callee.PkgPath {
				// reference within callee package
				if caller.Types.Path() == callee.PkgPath {
					// Caller and callee are in same package. Check caller has not shadowed the decl.
					found := caller.lookup(obj.Name)
					if found != nil && !isPkgLevel(found) {
						return nil, fmt.Errorf("cannot inline, because the callee refers to %s %q, which in the caller is shadowed by a %s (declared at line %d)",
							obj.Kind, obj.Name,
							objectKind(found),
							caller.Fset.PositionFor(found.Pos(), false).Line)
					}
				} else {
					// Cross-package reference.
					qualify = true
				}
			} else {
				// Reference to a package-level declaration in another package, without a qualified identifier.
				qualify = true
			}

			// Form a qualified identifier, pkg.Name.
			if qualify {
				pkgName := istate.localName(obj.PkgPath, obj.PkgName, obj.PkgName, obj.Shadow)
				newName = &ast.SelectorExpr{
					X:   makeIdent(pkgName),
					Sel: makeIdent(obj.Name),
				}
			}
		}
		objRenames[i] = newName
	}
	return objRenames, nil
}

type argument struct {
	expr          ast.Expr
	typ           types.Type      // may be tuple for sole non-receiver arg in spread call
	constant      constant.Value  // value of argument if constant
	spread        bool            // final arg is call() assigned to multiple params
	pure          bool            // expr is pure (doesn't read variables)
	effects       bool            // expr has effects (updates variables)
	duplicable    bool            // expr may be duplicated
	freevars      map[string]bool // free names of expr
	variadic      bool            // is explicit []T{...} for eliminated variadic
	desugaredRecv bool            // is *recv or &recv, where operator was elided
}

// typeArguments returns the type arguments of the call. It only collects the
// arguments that are explicitly provided; it does not attempt type inference.
func (st *state) typeArguments(call *ast.CallExpr) []*argument {
	var exprs []ast.Expr
	switch d := ast.Unparen(call.Fun).(type) {
	case *ast.IndexExpr:
		exprs = []ast.Expr{d.Index}
	case *ast.IndexListExpr:
		exprs = d.Indices
	default:
		// No type arguments
		return nil
	}
	var args []*argument
	for _, e := range exprs {
		arg := &argument{expr: e, freevars: freeVars(st.caller.Info, e)}
		args = append(args, arg)
	}
	return args
}

// arguments returns the effective arguments of the call.
//
// If the receiver argument and parameter have
// different pointerness, make the "&" or "*" explicit.
//
// Also, if x.f() is shorthand for promoted method x.y.f(),
// make the .y explicit in T.f(x.y, ...).
func (st *state) arguments(caller *Caller, calleeDecl *ast.FuncDecl, assign1 func(*types.Var) bool) ([]*argument, error) {
	var args []*argument

	callArgs := caller.Call.Args
	if calleeDecl.Recv != nil {
		if len(st.callee.impl.TypeParams) > 0 {
			return nil, fmt.Errorf("cannot inline: generic methods not yet supported")
		}
		sel := ast.Unparen(caller.Call.Fun).(*ast.SelectorExpr)
		seln := caller.Info.Selections[sel]
		var recvArg ast.Expr
		switch seln.Kind() {
		case types.MethodVal: // recv.f(callArgs)
			recvArg = sel.X
		case types.MethodExpr: // T.f(recv, callArgs)
			recvArg = callArgs[0]
			callArgs = callArgs[1:]
		}
		if recvArg != nil {
			// Compute all the type-based predicates now, before we start meddling with
			// the syntax; the meddling will update them.
			arg := &argument{
				expr:       recvArg,
				typ:        caller.Info.TypeOf(recvArg),
				constant:   caller.Info.Types[recvArg].Value,
				pure:       pure(caller.Info, assign1, recvArg),
				effects:    st.effects(caller.Info, recvArg),
				duplicable: duplicable(caller.Info, recvArg),
				freevars:   freeVars(caller.Info, recvArg),
			}
			recvArg = nil // prevent accidental use

			// Move receiver argument recv.f(args) to argument list f(&recv, args).
			args = append(args, arg)

			// Make field selections explicit (recv.f -> recv.y.f), updating arg.{expr,typ}.
			indices := seln.Index()
			for _, index := range indices[:len(indices)-1] {
				fld := typeparams.CoreType(typeparams.Deref(arg.typ)).(*types.Struct).Field(index)
				if fld.Pkg() != caller.Types && !fld.Exported() {
					return nil, fmt.Errorf("in %s, implicit reference to unexported field .%s cannot be made explicit",
						debugFormatNode(caller.Fset, caller.Call.Fun),
						fld.Name())
				}
				if isPointer(arg.typ) {
					arg.pure = false // implicit *ptr operation => impure
				}
				arg.expr = &ast.SelectorExpr{
					X:   arg.expr,
					Sel: makeIdent(fld.Name()),
				}
				arg.typ = fld.Type()
				arg.duplicable = false
			}

			// Make * or & explicit.
			argIsPtr := isPointer(arg.typ)
			paramIsPtr := isPointer(seln.Obj().Type().Underlying().(*types.Signature).Recv().Type())
			if !argIsPtr && paramIsPtr {
				// &recv
				arg.expr = &ast.UnaryExpr{Op: token.AND, X: arg.expr}
				arg.typ = types.NewPointer(arg.typ)
				arg.desugaredRecv = true
			} else if argIsPtr && !paramIsPtr {
				// *recv
				arg.expr = &ast.StarExpr{X: arg.expr}
				arg.typ = typeparams.Deref(arg.typ)
				arg.duplicable = false
				arg.pure = false
				arg.desugaredRecv = true
			}
		}
	}
	for _, expr := range callArgs {
		tv := caller.Info.Types[expr]
		args = append(args, &argument{
			expr:       expr,
			typ:        tv.Type,
			constant:   tv.Value,
			spread:     is[*types.Tuple](tv.Type), // => last
			pure:       pure(caller.Info, assign1, expr),
			effects:    st.effects(caller.Info, expr),
			duplicable: duplicable(caller.Info, expr),
			freevars:   freeVars(caller.Info, expr),
		})
	}

	// A call omits a suffix of the parameter list when every parameter in that
	// suffix carries a default. The inlined body needs an argument for each.
	// The value is a constant, which is the simplest kind of argument there is:
	// pure, duplicable and free of effects. Depth: docs/OPTIONAL-PARAMS.md in
	// the gosmopolitan toolchain.
	if sig, ok := caller.Info.TypeOf(caller.Call.Fun).Underlying().(*types.Signature); ok && len(args) < sig.Params().Len() {
		if len(args) > 0 && last(args).spread {
			return nil, fmt.Errorf("cannot inline: call spreads a tuple over parameters that carry defaults")
		}
		for i := len(args); i < sig.Params().Len(); i++ {
			param := sig.Params().At(i)
			var deflt constant.Value
			if d := param.Default(); d != nil {
				deflt = d.Const
			}
			expr := defaultLiteral(deflt, caller.Call.Pos())
			if expr == nil {
				return nil, fmt.Errorf("cannot inline: parameter %s has no default to supply", param.Name())
			}
			args = append(args, &argument{
				expr:       expr,
				typ:        param.Type(),
				constant:   deflt,
				pure:       true,
				duplicable: true,
			})
		}
	}

	// Re-typecheck each constant argument expression in a neutral context.
	for _, arg := range args {
		if arg.constant == nil {
			continue
		}
		info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue)}
		if err := types.CheckExpr(caller.Fset, caller.Types, caller.Call.Pos(), arg.expr, info); err != nil {
			return nil, err
		}
		arg.typ = info.TypeOf(arg.expr)
	}

	return args, nil
}

type parameter struct {
	obj       *types.Var // parameter var from caller's signature
	fieldType ast.Expr   // syntax of type, from calleeDecl.Type.{Recv,Params}
	info      *paramInfo // information from AnalyzeCallee
	variadic  bool       // (final) parameter is unsimplified ...T
}

// A replacer replaces an identifier at the given offset in the callee.
type replacer = func(offset int, repl ast.Expr, unpackVariadic bool)

// substituteTypeParams replaces type parameters in the callee with the
// corresponding type arguments from the call.
func substituteTypeParams(logf logger, typeParams []*paramInfo, typeArgs []*argument, replace replacer) error {
	assert(len(typeParams) == len(typeArgs), "mismatched number of type params/args")
	for i, paramInfo := range typeParams {
		arg := typeArgs[i]
		// Perform a simplified, conservative shadow analysis: fail if there is any shadowing.
		for free := range arg.freevars {
			if paramInfo.Shadow[free] != 0 {
				return fmt.Errorf("cannot inline: type argument #%d (type parameter %s) is shadowed", i, paramInfo.Name)
			}
		}
		logf("replacing type param %s with %s", paramInfo.Name, debugFormatNode(token.NewFileSet(), arg.expr))
		for _, ref := range paramInfo.Refs {
			replace(ref.Offset, internalastutil.CloneNode(arg.expr), false)
		}
	}
	return nil
}

// syncParamFieldTypes synchronizes the fieldType of each parameter in params
// with the mutated calleeDecl AST. This is necessary because substituteTypeParams
// mutates the calleeDecl AST, replacing type nodes, but params still references
// the original (now outdated) type nodes.
func syncParamFieldTypes(calleeDecl *ast.FuncDecl, params []*parameter) {
	var i int
	setFieldType := func(t ast.Expr) {
		assert(i < len(params), "mismatched parameter count")
		params[i].fieldType = t
		i++
	}

	if calleeDecl.Recv != nil && len(calleeDecl.Recv.List) > 0 {
		setFieldType(calleeDecl.Recv.List[0].Type)
	}
	if calleeDecl.Type.Params != nil {
		for _, field := range calleeDecl.Type.Params.List {
			if field.Names == nil {
				setFieldType(field.Type)
			} else {
				for range field.Names {
					setFieldType(field.Type)
				}
			}
		}
	}
	assert(i == len(params), "mismatched parameter count")
}

// substitute implements parameter elimination by substitution.
//
// It considers each parameter and its corresponding argument in turn and
// evaluate these conditions:
//
//   - the parameter is neither address-taken nor assigned;
//   - the argument is pure;
//   - if the parameter refcount is zero, the argument must
//     not contain the last use of a local var;
//   - if the parameter refcount is > 1, the argument must be duplicable;
//   - the argument (or types.Default(argument) if it's untyped) has
//     the same type as the parameter.
func substitute(logf logger, caller *Caller, params []*parameter, args []*argument, effects []int, falcon falconResult, replace replacer) {
	assert(len(args) <= len(params), "too many arguments")

	// Collect candidates for substitution.
	sg := make(substGraph)
next:
	for i, param := range params {
		arg := args[i]

		// Check argument against parameter.

		if arg.spread {
			// spread => last argument, but not always last parameter
			logf("keeping param %q and following ones: argument %s is spread",
				param.info.Name, debugFormatNode(caller.Fset, arg.expr))
			return // give up
		}
		assert(!param.variadic, "unsimplified variadic parameter")
		if param.info.Escapes {
			logf("keeping param %q: escapes from callee", param.info.Name)
			continue
		}
		if param.info.Assigned {
			logf("keeping param %q: assigned by callee", param.info.Name)
			continue // callee needs the parameter variable
		}
		if len(param.info.Refs) > 1 && !arg.duplicable {
			logf("keeping param %q: argument is not duplicable", param.info.Name)
			continue // incorrect or poor style to duplicate an expression
		}
		if len(param.info.Refs) == 0 {
			if arg.effects {
				logf("keeping param %q: though unreferenced, it has effects", param.info.Name)
				continue
			}

			// If the caller is within a function body, eliminating an unreferenced
			// parameter might remove the last reference to a caller local var.
			if caller.enclosingFunc != nil {
				for free := range arg.freevars {
					if v, ok := caller.lookup(free).(*types.Var); ok && within(v.Pos(), caller.enclosingFunc.Body) && !isUsedOutsideCall(caller, v) {

						// Check to see if the substituted var is used within other args
						// whose corresponding params ARE used in the callee
						usedElsewhere := func() bool {
							for i, param := range params {
								if i < len(args) && len(param.info.Refs) > 0 { // excludes original param
									for name := range args[i].freevars {
										if caller.lookup(name) == v {
											return true
										}
									}
								}
							}
							return false
						}
						if !usedElsewhere() {
							logf("keeping param %q: arg contains perhaps the last reference to caller local %v @ %v",
								param.info.Name, v, caller.Fset.PositionFor(v.Pos(), false))
							continue next
						}
					}
				}
			}
		}

		// Arg is a potential substitution candidate: analyze its shadowing.

		sg[arg] = nil // Absent shadowing, the arg is substitutable.
		for free := range arg.freevars {
			switch s := param.info.Shadow[free]; {
			case s < 0:
				// Shadowed by a non-parameter symbol, so arg is not substitutable.
				delete(sg, arg)
			case s > 0:
				// Shadowed by a parameter; arg may be substitutable, if only shadowed
				// by other substitutable parameters.
				if s > len(args) {
					// Defensive: this should not happen in the current factoring, since spread arguments are already handled.
					delete(sg, arg)
				}
				if edges, ok := sg[arg]; ok {
					sg[arg] = append(edges, args[s-1])
				}
			}
		}
	}

	// Process the initial state of the substitution graph.
	sg.prune()

	// Now we check various conditions on the substituted argument set as a
	// whole.

	// Reject constant arguments as substitution candidates if they cause
	// violation of falcon constraints.
	for checkFalconConstraints(logf, params, args, falcon, sg) {
		sg.prune()
	}

	// As a final step, introduce bindings to resolve any evaluation order
	// hazards.
	for resolveEffects(logf, args, effects, sg) {
		sg.prune()
	}

	// The remaining candidates are safe to substitute.
	for i, param := range params {
		if arg := args[i]; sg.has(arg) {

			// It is safe to substitute param and replace it with arg.
			// The formatter introduces parens as needed for precedence.
			logf("replacing parameter %q by argument %q",
				param.info.Name, debugFormatNode(caller.Fset, arg.expr))
			for _, ref := range param.info.Refs {
				// Apply any transformations necessary for this reference.
				argExpr := arg.expr

				// If the reference itself is being selected, and we applied desugaring
				// (an explicit &x or *x).
				if ref.IsSelectionOperand && arg.desugaredRecv {
					switch e := argExpr.(type) {
					case *ast.UnaryExpr:
						argExpr = e.X
					case *ast.StarExpr:
						argExpr = e.X
					}
				}

				// If the reference requires exact type agreement between parameter and
				// argument, wrap the argument in an explicit conversion if substitution
				// might materially change its type.
				needType := ref.AffectsInference ||
					(ref.Assignable && ref.IfaceAssignment && !param.info.IsInterface) ||
					(!ref.Assignable && !trivialConversion(arg.constant, arg.typ, param.obj.Type()))

				if needType &&
					!types.Identical(types.Default(arg.typ), param.obj.Type()) {

					// If arg.expr is already an interface call, strip it.
					if call, ok := argExpr.(*ast.CallExpr); ok && len(call.Args) == 1 {
						if typ, ok := isConversion(caller.Info, call); ok && isNonTypeParamInterface(typ) {
							argExpr = call.Args[0]
						}
					}

					argExpr = convert(param.fieldType, argExpr)
					logf("param %q (offset %d): adding explicit %s -> %s conversion around argument",
						param.info.Name, ref.Offset, arg.typ, param.obj.Type())
				}
				replace(ref.Offset, internalastutil.CloneNode(argExpr).(ast.Expr), arg.variadic)
			}
			params[i] = nil // substituted
			args[i] = nil   // substituted
		}
	}
}

// isConversion reports whether the given call is a type conversion, returning
// (operand, true) if so.
//
// If the call is not a conversion, it returns (nil, false).
func isConversion(info *types.Info, call *ast.CallExpr) (types.Type, bool) {
	if tv, ok := info.Types[call.Fun]; ok && tv.IsType() {
		return tv.Type, true
	}
	return nil, false
}

// isNonTypeParamInterface reports whether t is a non-type parameter interface
// type.
func isNonTypeParamInterface(t types.Type) bool {
	return !typeparams.IsTypeParam(t) && types.IsInterface(t)
}

// isUsedOutsideCall reports whether v is used outside of caller.Call, within
// the body of caller.enclosingFunc.
func isUsedOutsideCall(caller *Caller, v *types.Var) bool {
	used := false
	ast.Inspect(caller.enclosingFunc.Body, func(n ast.Node) bool {
		if n == caller.Call {
			return false
		}
		switch n := n.(type) {
		case *ast.Ident:
			if use := caller.Info.Uses[n]; use == v {
				used = true
			}
		case *ast.FuncType:
			// All params are used.
			for _, fld := range n.Params.List {
				for _, n := range fld.Names {
					if def := caller.Info.Defs[n]; def == v {
						used = true
					}
				}
			}
		}
		return !used // keep going until we find a use
	})
	return used
}

// checkFalconConstraints checks whether constant arguments are safe to
// substitute (e.g. s[i] -> ""[0] is not safe.)
//
// Any failed constraint causes us to reject all constant arguments as
// substitution candidates (by clearing args[i].substitution=false).
//
// TODO(adonovan): we could obtain a finer result rejecting only the freevars
// of each failed constraint. We processing constraints in order of
// increasing arity, but failures are rare.
func checkFalconConstraints(logf logger, params []*parameter, args []*argument, falcon falconResult, sg substGraph) bool {
	// Create a dummy package, as this is the only way to create an environment for CheckExpr.
	pkg := types.NewPackage("falcon", "falcon")

	// Declare types used by constraints.
	for _, typ := range falcon.Types {
		logf("falcon env: type %s %s", typ.Name, types.Typ[typ.Kind])
		pkg.Scope().Insert(types.NewTypeName(token.NoPos, pkg, typ.Name, types.Typ[typ.Kind]))
	}

	// Declared constants and variables for parameters.
	nconst := 0
	for i, param := range params {
		name := param.info.Name
		if name == "" {
			continue // unreferenced
		}
		arg := args[i]
		if arg.constant != nil && sg.has(arg) && param.info.FalconType != "" {
			t := pkg.Scope().Lookup(param.info.FalconType).Type()
			pkg.Scope().Insert(types.NewConst(token.NoPos, pkg, name, t, arg.constant))
			logf("falcon env: const %s %s = %v", name, param.info.FalconType, arg.constant)
			nconst++
		} else {
			v := types.NewVar(token.NoPos, pkg, name, arg.typ)
			typesinternal.SetVarKind(v, typesinternal.PackageVar)
			pkg.Scope().Insert(v)
			logf("falcon env: var %s %s", name, arg.typ)
		}
	}
	if nconst == 0 {
		return false // nothing to do
	}

	// Parse and evaluate the constraints in the environment.
	fset := token.NewFileSet()
	removed := false
	for _, falcon := range falcon.Constraints {
		expr, err := parser.ParseExprFrom(fset, "falcon", falcon, 0)
		if err != nil {
			panic(fmt.Sprintf("failed to parse falcon constraint %s: %v", falcon, err))
		}
		if err := types.CheckExpr(fset, pkg, token.NoPos, expr, nil); err != nil {
			logf("falcon: constraint %s violated: %v", falcon, err)
			for j, arg := range args {
				if arg.constant != nil && sg.has(arg) {
					logf("keeping param %q due falcon violation", params[j].info.Name)
					removed = sg.remove(arg) || removed
				}
			}
			break
		}
		logf("falcon: constraint %s satisfied", falcon)
	}
	return removed
}

// resolveEffects marks arguments as non-substitutable to resolve hazards
// resulting from the callee evaluation order described by the effects list.
//
// To do this, each argument is categorized as a read (R), write (W), or pure.
// A hazard occurs when the order of evaluation of a W changes any R or W.
// Pure arguments can be effectively ignored, as they can be safely evaluated
// in any order.
func resolveEffects(logf logger, args []*argument, effects []int, sg substGraph) bool {
	effectStr := func(effects bool, idx int) string {
		i := fmt.Sprint(idx)
		if idx == len(args) {
			i = "∞"
		}
		return string("RW"[btoi(effects)]) + i
	}
	removed := false
	for i, argi := range slices.Backward(args) {
		if sg.has(argi) && !argi.pure {
			// i is not bound: check whether it must be bound due to hazards.
			idx := slices.Index(effects, i)
			if idx >= 0 {
				for _, j := range effects[:idx] {
					var (
						ji int  // effective param index
						jw bool // j is a write
					)
					if j == winf || j == rinf {
						jw = j == winf
						ji = len(args)
					} else {
						jw = args[j].effects
						ji = j
					}
					if ji > i && (jw || argi.effects) { // out of order evaluation
						logf("binding argument %s: preceded by %s",
							effectStr(argi.effects, i), effectStr(jw, ji))

						removed = sg.remove(argi) || removed
						break
					}
				}
			}
		}
		if !sg.has(argi) {
			for j := range i {
				argj := args[j]
				if argj.pure {
					continue
				}
				if (argi.effects || argj.effects) && sg.has(argj) {
					logf("binding argument %s: %s is bound",
						effectStr(argj.effects, j), effectStr(argi.effects, i))

					removed = sg.remove(argj) || removed
				}
			}
		}
	}
	return removed
}

// A substGraph is a directed graph representing arguments that may be substituted, provided all of their related arguments.
type substGraph map[*argument][]*argument

// has reports whether arg is a candidate for substitution.
func (g substGraph) has(arg *argument) bool {
	_, ok := g[arg]
	return ok
}

// remove marks arg as not substitutable, reporting whether the arg was
// substitutable.
func (g substGraph) remove(arg *argument) bool {
	pre := len(g)
	delete(g, arg)
	return len(g) < pre
}

// prune updates the graph to remove any keys that reach other arguments not
// present in the graph.
func (g substGraph) prune() {
	// visit visits the forward transitive closure of arg and reports whether any missing argument was encountered.
	var visit func(*argument, map[*argument]unit) bool
	visit = func(arg *argument, seen map[*argument]unit) bool {
		deps, ok := g[arg]
		if !ok {
			return false
		}
		if _, ok := seen[arg]; !ok {
			seen[arg] = unit{}
			for _, dep := range deps {
				if !visit(dep, seen) {
					delete(g, arg)
					return false
				}
			}
		}
		return true
	}
	for arg := range g {
		// Remove any argument that is, or transitively depends upon, an unsubstitutable argument.
		visit(arg, make(map[*argument]unit))
	}
}

// updateCalleeParams updates the calleeDecl syntax to remove
// substituted parameters and move the receiver (if any) to the head
// of the ordinary parameters.
func updateCalleeParams(calleeDecl *ast.FuncDecl, params []*parameter) {
	// The logic is fiddly because of the forms of ast.Field:.

	paramIdx := 0 // index in original parameter list (incl. receiver)
	var newParams []*ast.Field
	filterParams := func(field *ast.Field) {
		var names []*ast.Ident
		if field.Names == nil {
			// Unnamed parameter field (e.g. func f(int)
			if params[paramIdx] != nil {
				// Give it an explicit name "_" since we will make the receiver.
				names = append(names, makeIdent("_"))
			}
			paramIdx++
		} else {
			// Named parameter field e.g. func f(x, y int) Remove substituted
			// parameters in place. If all were substituted, delete field.
			for _, id := range field.Names {
				if pinfo := params[paramIdx]; pinfo != nil {
					// Rename unreferenced parameters with "_".
					if len(pinfo.info.Refs) == 0 {
						id = makeIdent("_")
					}
					names = append(names, id)
				}
				paramIdx++
			}
		}
		if names != nil {
			newParams = append(newParams, &ast.Field{
				Names: names,
				Type:  field.Type,
			})
		}
	}
	if calleeDecl.Recv != nil {
		filterParams(calleeDecl.Recv.List[0])
		calleeDecl.Recv = nil
	}
	for _, field := range calleeDecl.Type.Params.List {
		filterParams(field)
	}
	calleeDecl.Type.Params.List = newParams
}

// bindingDeclInfo records information about the binding decl produced by
// createBindingDecl.
type bindingDeclInfo struct {
	names map[string]bool // names bound by the binding decl; possibly empty
	stmt  ast.Stmt        // the binding decl itself
}

// createBindingDecl constructs a "binding decl" that implements parameter
// assignment and declares any named result variables referenced by the
// callee. It returns nil if there were no unsubstituted parameters.
//
// It may not always be possible to create the decl (e.g. due to shadowing),
// in which case it also returns nil. But if it succeeds, the declaration may
// be used by reduction strategies to relax the requirement that all
// parameters have been substituted.
func createBindingDecl(logf logger, caller *Caller, args []*argument, calleeDecl *ast.FuncDecl, results []*paramInfo) *bindingDeclInfo {
	// Spread calls are tricky as they may not align with the parameters' field
	// groupings nor types.
	if lastArg := last(args); lastArg != nil && lastArg.spread {
		logf("binding decls not yet supported for spread calls")
		return nil
	}

	var (
		specs []ast.Spec
		names = make(map[string]bool) // names defined by previous specs
	)
	// shadow reports whether any name referenced by spec is shadowed by a name
	// declared by a previous spec (since, unlike parameters, each spec of a var
	// decl is within the scope of the specs).
	shadow := func(spec *ast.ValueSpec) bool {
		// Compute union of free names of type and values and detect shadowing.
		const includeComplitIdents = true
		free := free.Names(spec.Type, includeComplitIdents)
		for _, value := range spec.Values {
			for name := range freeVars(caller.Info, value) {
				free[name] = true
			}
		}
		for name := range free {
			if names[name] {
				logf("binding decl would shadow free name %q", name)
				return true
			}
		}
		for _, id := range spec.Names {
			if id.Name != "_" {
				names[id.Name] = true
			}
		}
		return false
	}

	// parameters.
	var values []ast.Expr
	for _, arg := range args {
		if arg != nil {
			values = append(values, arg.expr)
		}
	}
	for _, field := range calleeDecl.Type.Params.List {
		// Each field (param group) becomes a ValueSpec.
		spec := &ast.ValueSpec{
			Names:  cleanNodes(field.Names),
			Type:   cleanNode(field.Type),
			Values: values[:len(field.Names)],
		}
		values = values[len(field.Names):]
		if shadow(spec) {
			return nil
		}
		specs = append(specs, spec)
	}
	assert(len(values) == 0, "args/params mismatch")

	// results
	//
	// Add specs to declare any named result
	// variables that are referenced by the body.
	if calleeDecl.Type.Results != nil {
		resultIdx := 0
		for _, field := range calleeDecl.Type.Results.List {
			if field.Names == nil {
				resultIdx++
				continue // unnamed field
			}
			var names []*ast.Ident
			for _, id := range field.Names {
				if len(results[resultIdx].Refs) > 0 {
					names = append(names, id)
				}
				resultIdx++
			}
			if len(names) > 0 {
				spec := &ast.ValueSpec{
					Names: cleanNodes(names),
					Type:  cleanNode(field.Type),
				}
				if shadow(spec) {
					return nil
				}
				specs = append(specs, spec)
			}
		}
	}

	if len(specs) == 0 {
		logf("binding decl not needed: all parameters substituted")
		return nil
	}

	stmt := &ast.DeclStmt{
		Decl: &ast.GenDecl{
			Tok:   token.VAR,
			Specs: specs,
		},
	}
	logf("binding decl: %s", debugFormatNode(caller.Fset, stmt))
	return &bindingDeclInfo{names: names, stmt: stmt}
}

// lookup does a symbol lookup in the lexical environment of the caller.
func (caller *Caller) lookup(name string) types.Object {
	pos := caller.Call.Pos()
	for _, n := range caller.path {
		if scope := scopeFor(caller.Info, n); scope != nil {
			if _, obj := scope.LookupParent(name, pos); obj != nil {
				return obj
			}
		}
	}
	return nil
}

func scopeFor(info *types.Info, n ast.Node) *types.Scope {
	// The function body scope (containing not params) is associated with the
	// function's type, not body.
	switch fn := n.(type) {
	case *ast.FuncDecl:
		n = fn.Type
	case *ast.FuncLit:
		n = fn.Type
	}
	return info.Scopes[n]
}

// -- predicates over expressions --

// freeVars returns the names of all free identifiers of e:
// those lexically referenced by it but not defined within it.
// (Fields and methods are not included.)
func freeVars(info *types.Info, e ast.Expr) map[string]bool {
	free := make(map[string]bool)
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			if obj, ok := info.Uses[id]; ok && !within(obj.Pos(), e) && !isField(obj) {
				free[obj.Name()] = true
			}
		}
		return true
	})
	return free
}

// effects reports whether an expression might change the state of the
// program (through function calls and channel receives) and affect
// the evaluation of subsequent expressions.
func (st *state) effects(info *types.Info, expr ast.Expr) bool {
	effects := false
	ast.Inspect(expr, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false // prune descent

		case *ast.CallExpr:
			if info.Types[n.Fun].IsType() {
			} else if !typesinternal.CallsPureBuiltin(info, n) {
				// A handful of built-ins have no effect beyond those of their arguments.
				effects = true
			}

		case *ast.UnaryExpr:
			if n.Op == token.ARROW { // <-ch
				effects = true
			}
		}
		return true
	})

	// Even if consideration of effects is not desired,
	// we continue to compute, log, and discard them.
	if st.opts.IgnoreEffects && effects {
		effects = false
		st.opts.Logf("ignoring potential effects of argument %s",
			debugFormatNode(st.caller.Fset, expr))
	}

	return effects
}

// pure reports whether an expression has the same result no matter when it is
// executed relative to other expressions. It can be commuted with any other
// expression or statement without changing its meaning. An expression is
// considered impure if it reads the contents of any variable, with the
// exception of "single assignment" local variables (as classified by the
// provided callback). This are never updated after their initialization. Pure
// does not imply duplicable: for example, new(T) and T{} are pure expressions
// but both return a different value each time they are evaluated. They are
// not safe to duplicate. Purity does not imply freedom from run-time panics.
// We assume that target programs do not encounter run-time panics nor depend
// on them for correct operation. TODO(adonovan): add unit tests of this
// function.
func pure(info *types.Info, assign1 func(*types.Var) bool, e ast.Expr) bool {
	var pure func(e ast.Expr) bool
	pure = func(e ast.Expr) bool {
		switch e := e.(type) {
		case *ast.ParenExpr:
			return pure(e.X)

		case *ast.Ident:
			if v, ok := info.Uses[e].(*types.Var); ok {
				// In general variables are impure as they may be updated, but single-assignment local variables never change value.
				return !isPkgLevel(v) && assign1(v)
			}

			// All other kinds of reference are pure.
			return true

		case *ast.FuncLit:
			// A function literal may allocate a closure that references mutable variables, but mutation cannot be observed without calling the function.
			return true

		case *ast.BasicLit:
			return true

		case *ast.UnaryExpr: // + - ! ^ & but not <-
			return e.Op != token.ARROW && pure(e.X)

		case *ast.BinaryExpr: // arithmetic, shifts, comparisons, &&/||
			return pure(e.X) && pure(e.Y)

		case *ast.CallExpr:
			// A conversion is as pure as its operand.
			if info.Types[e.Fun].IsType() {
				return pure(e.Args[0])
			}

			// Calls to some built-ins are as pure as their arguments.
			if typesinternal.CallsPureBuiltin(info, e) {
				for _, arg := range e.Args {
					if !pure(arg) {
						return false
					}
				}
				return true
			}

			// All other calls are impure, so we can reject them without even looking at e.Fun.
			return false

		case *ast.CompositeLit:
			// T{...} is as pure as its elements.
			for _, elt := range e.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if !pure(kv.Value) {
						return false
					}
					if id, ok := kv.Key.(*ast.Ident); ok {
						if v, ok := info.Uses[id].(*types.Var); ok && v.IsField() {
							continue // struct {field: value}
						}
					}
					// map/slice/array {key: value}
					if !pure(kv.Key) {
						return false
					}

				} else if !pure(elt) {
					return false
				}
			}
			return true

		case *ast.SelectorExpr:
			if seln, ok := info.Selections[e]; ok {
				// See types.SelectionKind for background.
				switch seln.Kind() {
				case types.MethodExpr:
					// A method expression T.f acts like a reference to a func decl, so it is pure.
					return true

				case types.MethodVal, types.FieldVal:
					// A field or method selection x.f is pure if x is pure and the selection does not indirect a pointer.
					return !indirectSelection(seln) && pure(e.X)

				default:
					panic(seln)
				}
			} else {
				// A qualified identifier is treated like an unqualified one.
				return pure(e.Sel)
			}

		case *ast.StarExpr:
			return false // *ptr depends on the state of the heap

		default:
			return false
		}
	}
	return pure(e)
}

// duplicable reports whether it is appropriate for the expression to
// be freely duplicated.
//
// Given the declaration
//
//	func f(x T) T { return x + g() + x }
//
// an argument y is considered duplicable if we would wish to see a
// call f(y) simplified to y+g()+y. This is true for identifiers,
// integer literals, unary negation, and selectors x.f where x is not
// a pointer. But we would not wish to duplicate expressions that:
// - have side effects (e.g. nearly all calls),
// - are not referentially transparent (e.g. &T{}, ptr.field, *ptr), or
// - are long (e.g. "huge string literal").
func duplicable(info *types.Info, e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.ParenExpr:
		return duplicable(info, e.X)

	case *ast.Ident:
		return true

	case *ast.BasicLit:
		v := info.Types[e].Value
		switch e.Kind {
		case token.INT:
			return true // any int
		case token.STRING:
			return consteq(v, kZeroString) // only ""
		case token.FLOAT:
			return consteq(v, kZeroFloat) || consteq(v, kOneFloat)
		}

	case *ast.UnaryExpr:
		return (e.Op == token.ADD || e.Op == token.SUB) && duplicable(info, e.X)

	case *ast.CompositeLit:
		// Empty struct or array literals T{} are duplicable.
		if len(e.Elts) == 0 {
			switch info.TypeOf(e).Underlying().(type) {
			case *types.Struct, *types.Array:
				return true
			}
		}
		return false

	case *ast.CallExpr:
		// Treat type conversions as duplicable if they do not observably allocate.

		if !info.Types[e.Fun].IsType() { // check whether e.Fun is a type conversion
			return false
		}

		fun := info.TypeOf(e.Fun)
		arg := info.TypeOf(e.Args[0])

		switch fun := fun.Underlying().(type) {
		case *types.Slice:
			// Do not mark []byte(string) and []rune(string) as duplicable.
			elem, ok := fun.Elem().Underlying().(*types.Basic)
			if ok && (elem.Kind() == types.Rune || elem.Kind() == types.Byte) {
				from, ok := arg.Underlying().(*types.Basic)
				isString := ok && from.Info()&types.IsString != 0
				return !isString
			}
		case *types.TypeParam:
			return false // be conservative
		}
		return true

	case *ast.SelectorExpr:
		if seln, ok := info.Selections[e]; ok {
			// A field or method selection x.f is referentially transparent if it does not indirect a pointer.
			return !indirectSelection(seln)
		}
		// A qualified identifier pkg.Name is referentially transparent.
		return true
	}
	return false
}

func consteq(x, y constant.Value) bool {
	return constant.Compare(x, token.EQL, y)
}

var (
	kZeroInt    = constant.MakeInt64(0)
	kZeroString = constant.MakeString("")
	kZeroFloat  = constant.MakeFloat64(0.0)
	kOneFloat   = constant.MakeFloat64(1.0)
)

// -- inline helpers --

func assert(cond bool, msg string) {
	if !cond {
		panic(msg)
	}
}

// blanks returns a slice of n > 0 blank identifiers.
func blanks[E ast.Expr](n int) []E {
	if n == 0 {
		panic("blanks(0)")
	}
	res := make([]E, n)
	for i := range res {
		res[i] = ast.Expr(makeIdent("_")).(E) // ugh
	}
	return res
}

func makeIdent(name string) *ast.Ident {
	return &ast.Ident{Name: name}
}

// importedPkgName returns the PkgName object declared by an ImportSpec.
func importedPkgName(info *types.Info, imp *ast.ImportSpec) (*types.PkgName, bool) {
	var obj types.Object
	if imp.Name != nil {
		obj = info.Defs[imp.Name]
	} else {
		obj = info.Implicits[imp]
	}
	pkgname, ok := obj.(*types.PkgName)
	return pkgname, ok
}

func isPkgLevel(obj types.Object) bool {
	// TODO(adonovan): consider using the simpler obj.Parent() == obj.Pkg().Scope() instead.
	return obj.Pkg().Scope().Lookup(obj.Name()) == obj
}

// callContext returns both nodes immediately enclosing the call
// (specified as a PathEnclosingInterval), ignoring parens.
func callContext(callPath []ast.Node) (parent, grandparent ast.Node) {
	_ = callPath[0].(*ast.CallExpr) // sanity check
	for _, n := range callPath[1:] {
		if !is[*ast.ParenExpr](n) {
			if parent == nil {
				parent = n
			} else {
				return parent, n
			}
		}
	}
	return parent, nil
}

// hasLabelConflict reports whether the set of labels of the function
// enclosing the call (specified as a PathEnclosingInterval)
// intersects with the set of callee labels.
func hasLabelConflict(callPath []ast.Node, calleeLabels []string) bool {
	labels := callerLabels(callPath)
	for _, label := range calleeLabels {
		if labels[label] {
			return true // conflict
		}
	}
	return false
}

// callerLabels returns the set of control labels in the function (if
// any) enclosing the call (specified as a PathEnclosingInterval).
func callerLabels(callPath []ast.Node) map[string]bool {
	var callerBody *ast.BlockStmt
	switch f := callerFunc(callPath).(type) {
	case *ast.FuncDecl:
		callerBody = f.Body
	case *ast.FuncLit:
		callerBody = f.Body
	}
	var labels map[string]bool
	if callerBody != nil {
		ast.Inspect(callerBody, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				return false // prune traversal
			case *ast.LabeledStmt:
				if labels == nil {
					labels = make(map[string]bool)
				}
				labels[n.Label.Name] = true
			}
			return true
		})
	}
	return labels
}

// callerFunc returns the innermost Func{Decl,Lit} node enclosing the
// call (specified as a PathEnclosingInterval).
func callerFunc(callPath []ast.Node) ast.Node {
	_ = callPath[0].(*ast.CallExpr) // sanity check
	for _, n := range callPath[1:] {
		if is[*ast.FuncDecl](n) || is[*ast.FuncLit](n) {
			return n
		}
	}
	return nil
}

// callStmt reports whether the function call (specified as a
// PathEnclosingInterval) appears within an ExprStmt, and returns it if so.
//
// If unrestricted, callStmt returns nil if the ExprStmt f() appears in a
// restricted context (such as "if f(); cond {") where it cannot be replaced
// by an arbitrary statement. (See "statement theory".)
func callStmt(callPath []ast.Node, unrestricted bool) *ast.ExprStmt {
	parent, _ := callContext(callPath)
	stmt, ok := parent.(*ast.ExprStmt)
	if ok && unrestricted {
		switch callPath[slices.Index(callPath, ast.Node(stmt))+1].(type) {
		case *ast.LabeledStmt,
			*ast.BlockStmt,
			*ast.CaseClause,
			*ast.CommClause:
			// unrestricted
		default:
			// TODO(adonovan): handle restricted XYZStmt.Init contexts (but not.

			return nil // restricted
		}
	}
	return stmt
}

// Statement theory.

// replaceNode performs a destructive update of the tree rooted at
// root, replacing each occurrence of "from" with "to". If to is nil and
// the element is within a slice, the slice element is removed.
//
// The root itself cannot be replaced; an attempt will panic.
//
// This function must not be called on the caller's syntax tree.
//
// TODO(adonovan): polish this up and move it to astutil package.
// TODO(adonovan): needs a unit test.
func replaceNode(root ast.Node, from, to ast.Node) {
	if from == nil {
		panic("from == nil")
	}
	if reflect.ValueOf(from).IsNil() {
		panic(fmt.Sprintf("from == (%T)(nil)", from))
	}
	if from == root {
		panic("from == root")
	}
	found := false
	var parent reflect.Value // parent variable of interface type, containing a pointer
	var visit func(reflect.Value)
	visit = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Pointer:
			if v.Interface() == from {
				found = true

				// If v is a struct field or array element (e.g. Field.Comment or
				// Field.Names[i]) then it is addressable (a pointer variable).
				if !v.CanAddr() {
					v = parent
				}

				// to=nil => use zero value
				var toV reflect.Value
				if to != nil {
					toV = reflect.ValueOf(to)
				} else {
					toV = reflect.Zero(v.Type()) // e.g. ast.Expr(nil)
				}
				v.Set(toV)

			} else if !v.IsNil() {
				switch v.Interface().(type) {
				case *ast.Object, *ast.Scope:
					// Skip fields of types potentially involved in cycles.
				default:
					visit(v.Elem())
				}
			}

		case reflect.Struct:
			for i := range v.Type().NumField() {
				visit(v.Field(i))
			}

		case reflect.Slice:
			compact := false
			for i := range v.Len() {
				visit(v.Index(i))
				if v.Index(i).IsNil() {
					compact = true
				}
			}
			if compact {
				// Eliminate nils.
				j := 0
				for i := range v.Len() {
					if !v.Index(i).IsNil() {
						v.Index(j).Set(v.Index(i))
						j++
					}
				}
				v.SetLen(j)
			}
		case reflect.Interface:
			parent = v
			visit(v.Elem())

		case reflect.Array, reflect.Chan, reflect.Func, reflect.Map, reflect.UnsafePointer:
			panic(v) // unreachable in AST
		default:
		}
		parent = reflect.Value{}
	}
	visit(reflect.ValueOf(root))
	if !found {
		panic(fmt.Sprintf("%T not found", from))
	}
}

// cleanNode returns a clone of node with positions cleared.
func cleanNode[T ast.Node](node T) T {
	clone := internalastutil.CloneNode(node)
	clearPositions(clone)
	return clone
}

func cleanNodes[T ast.Node](nodes []T) []T {
	var clean []T
	for _, node := range nodes {
		clean = append(clean, cleanNode(node))
	}
	return clean
}

// clearPositions destroys token.Pos information within the tree rooted at root,
// as positions in callee trees may cause caller comments to be emitted prematurely.
func clearPositions(root ast.Node) {
	posType := reflect.TypeFor[token.Pos]()
	ast.Inspect(root, func(n ast.Node) bool {
		if n != nil {
			v := reflect.ValueOf(n).Elem() // deref the pointer to struct
			fields := v.Type().NumField()
			for i := range fields {
				f := v.Field(i)
				// Clearing Pos arbitrarily is destructive, as its presence may be
				// semantically significant (e.g. CallExpr.Ellipsis, TypeSpec.Assign) or
				// affect formatting preferences (e.g. GenDecl.Lparen).
				if f.Type() == posType {
					if f.Interface() != token.NoPos {
						f.Set(reflect.ValueOf(token.Pos(1)))
					}
				}
			}
		}
		return true
	})
}

// findIdent finds the Ident beneath root that has the given pos. It
// returns the path to the ident (excluding the ident), and the ident
// itself, where the path is the sequence of ast.Nodes. Those ast.Nodes
// are encountered in a depth-first search to find ident.
func findIdent(root ast.Node, pos token.Pos) ([]ast.Node, *ast.Ident) {
	// TODO(adonovan): opt: skip subtrees that don't contain pos.
	var (
		path  []ast.Node
		found *ast.Ident
	)
	ast.Inspect(root, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		if n == nil {
			path = path[:len(path)-1]
			return false
		}
		if id, ok := n.(*ast.Ident); ok {
			if id.Pos() == pos {
				found = id
				return true
			}
		}
		path = append(path, n)
		return true
	})
	if found == nil {
		panic(fmt.Sprintf("findIdent %d not found in %s",
			pos, debugFormatNode(token.NewFileSet(), root)))
	}
	return path, found
}

func prepend[T any](elem T, slice ...T) []T {
	return append([]T{elem}, slice...)
}

// debugFormatNode formats a node or returns a formatting error.
// Its sloppy treatment of errors is appropriate only for logging.
func debugFormatNode(fset *token.FileSet, n ast.Node) string {
	var out strings.Builder
	if err := format.Node(&out, fset, n); err != nil {
		out.WriteString(err.Error())
	}
	return out.String()
}

func shallowCopy[T any](ptr *T) *T {
	copy := *ptr
	return &copy
}

func forall[T any](list []T, f func(i int, x T) bool) bool {
	for i, x := range list {
		if !f(i, x) {
			return false
		}
	}
	return true
}

func exists[T any](list []T, f func(i int, x T) bool) bool {
	for i, x := range list {
		if f(i, x) {
			return true
		}
	}
	return false
}

// last returns the last element of a slice, or zero if empty.
func last[T any](slice []T) T {
	n := len(slice)
	if n > 0 {
		return slice[n-1]
	}
	return *new(T)
}

// declares returns the set of lexical names declared by a
// sequence of statements from the same block, excluding sub-blocks.
// (Lexical names do not include control labels.)
func declares(stmts []ast.Stmt) map[string]bool {
	names := make(map[string]bool)
	for _, stmt := range stmts {
		switch stmt := stmt.(type) {
		case *ast.DeclStmt:
			for _, spec := range stmt.Decl.(*ast.GenDecl).Specs {
				switch spec := spec.(type) {
				case *ast.ValueSpec:
					for _, id := range spec.Names {
						names[id.Name] = true
					}
				case *ast.TypeSpec:
					names[spec.Name.Name] = true
				}
			}

		case *ast.AssignStmt:
			if stmt.Tok == token.DEFINE {
				for _, lhs := range stmt.Lhs {
					names[lhs.(*ast.Ident).Name] = true
				}
			}
		}
	}
	delete(names, "_")
	return names
}

// A importNameFunc is used to query local import names in the caller, in a particular shadowing context.
type importNameFunc = func(pkgPath string, shadow shadowMap) string

// assignStmts rewrites a statement assigning the results of a call. Into zero
// or more statements that assign its return operands, or (nil, false) if no
// such rewrite is possible. The set of bindings created by the result of
// assignStmts is the same as the set of bindings created by the callerStmt.
//
// The callee must contain exactly one return statement.
func (st *state) assignStmts(callerStmt *ast.AssignStmt, returnOperands []ast.Expr, importName importNameFunc) ([]ast.Stmt, bool) {
	logf, caller, callee := st.opts.Logf, st.caller, &st.callee.impl

	assert(len(callee.Returns) == 1, "unexpected multiple returns")
	resultInfo := callee.Returns[0]

	// When constructing assign statements, we need.

	// Collect LHS information.
	var (
		lhs    []ast.Expr                                // shallow copy of the LHS slice, for mutation
		defs   = make([]*ast.Ident, len(callerStmt.Lhs)) // indexes in lhs of defining identifiers
		blanks = make([]bool, len(callerStmt.Lhs))       // indexes in lhs of blank identifiers
		byType typeutil.Map                              // map of distinct types -> indexes, for writing specs later
	)
	for i, expr := range callerStmt.Lhs {
		lhs = append(lhs, expr)
		if name, ok := expr.(*ast.Ident); ok {
			if name.Name == "_" {
				blanks[i] = true
				continue // no type
			}

			if obj, isDef := caller.Info.Defs[name]; isDef {
				defs[i] = name
				typ := obj.Type()
				idxs, _ := byType.At(typ).([]int)
				idxs = append(idxs, i)
				byType.Set(typ, idxs)
			}
		}
	}

	// Collect RHS information
	//
	// The RHS is either a parallel assignment or spread assignment, but by
	// looping over both callerStmt.Rhs and returnOperands we handle both.
	var (
		rhs             []ast.Expr              // new RHS of assignment, owned by the inliner
		callIdx         = -1                    // index of the call among the original RHS
		nilBlankAssigns = make(map[int]unit)    // indexes in rhs of _ = nil assignments, which can be deleted
		freeNames       = make(map[string]bool) // free(ish) names among rhs expressions
		nonTrivial      = make(map[int]bool)    // indexes in rhs of nontrivial result conversions
	)
	const includeComplitIdents = true

	for i, expr := range callerStmt.Rhs {
		if expr == caller.Call {
			assert(callIdx == -1, "malformed (duplicative) AST")
			callIdx = i
			for j, returnOperand := range returnOperands {
				maps.Copy(freeNames, free.Names(returnOperand, includeComplitIdents))
				rhs = append(rhs, returnOperand)
				if resultInfo[j]&nonTrivialResult != 0 {
					nonTrivial[i+j] = true
				}
				if blanks[i+j] && resultInfo[j]&untypedNilResult != 0 {
					nilBlankAssigns[i+j] = unit{}
				}
			}
		} else {
			// We must clone before clearing positions, since e came from the caller.
			expr = internalastutil.CloneNode(expr)
			clearPositions(expr)
			maps.Copy(freeNames, free.Names(expr, includeComplitIdents))
			rhs = append(rhs, expr)
		}
	}
	assert(callIdx >= 0, "failed to find call in RHS")

	// Substrategy "splice": Check to see if we can splice in the result
	// expressions from the callee, such as simplifying.
	if len(nonTrivial) == 0 { // no non-trivial conversions to worry about

		logf("substrategy: splice assignment")
		return []ast.Stmt{&ast.AssignStmt{
			Lhs:    lhs,
			Tok:    callerStmt.Tok,
			TokPos: callerStmt.TokPos,
			Rhs:    rhs,
		}}, true
	}

	// Inlining techniques below will need to write type information to preserve the correct types of LHS identifiers.
	universeAny := types.Universe.Lookup("any")
	typeExpr := func(typ types.Type, shadow shadowMap) ast.Expr {
		var (
			typeName string
			obj      *types.TypeName // nil for basic types
		)
		if tname := typesinternal.TypeNameFor(typ); tname != nil {
			obj = tname
			typeName = tname.Name()
		}

		// Special case: check for universe "any".
		if typ == universeAny.Type() {
			typeName = "any"
		}

		if typeName == "" {
			return nil
		}

		if obj == nil || obj.Pkg() == nil || obj.Pkg() == caller.Types { // local type or builtin
			if shadow[typeName] != 0 {
				logf("cannot write shadowed type name %q", typeName)
				return nil
			}
			obj, _ := caller.lookup(typeName).(*types.TypeName)
			if obj != nil && types.Identical(obj.Type(), typ) {
				return ast.NewIdent(typeName)
			}
		} else if pkgName := importName(obj.Pkg().Path(), shadow); pkgName != "" {
			return &ast.SelectorExpr{
				X:   ast.NewIdent(pkgName),
				Sel: ast.NewIdent(typeName),
			}
		}
		return nil
	}

	// Substrategy "spread". In the case of a spread call (func f() (T1, T2) return
	// g()), since we didn't hit the 'splice' substrategy. There must be some
	// non-declaring expression on the LHS. Simplify this by pre-declaring
	// variables, rewriting
	//
	//   x, y := f()
	//
	//
	//  var x int
	//  x, y = g()
	//
	// Which works as long as the predeclared variables do not overlap with free
	// names on the RHS.
	if len(rhs) != len(lhs) {
		assert(len(rhs) == 1 && len(returnOperands) == 1, "expected spread call")

		for _, id := range defs {
			if id != nil && freeNames[id.Name] {
				// By predeclaring variables, we're changing them to be in scope of the RHS.
				return nil, false
			}
		}

		// Write out the specs, being careful to avoid shadowing free names in
		// their type expressions.
		var (
			specs    []ast.Spec
			specIdxs []int
			shadow   = make(shadowMap)
		)
		failed := false
		byType.Iterate(func(typ types.Type, v any) {
			if failed {
				return
			}
			idxs := v.([]int)
			specIdxs = append(specIdxs, idxs[0])
			texpr := typeExpr(typ, shadow)
			if texpr == nil {
				failed = true
				return
			}
			spec := &ast.ValueSpec{
				Type: texpr,
			}
			for _, idx := range idxs {
				spec.Names = append(spec.Names, ast.NewIdent(defs[idx].Name))
			}
			specs = append(specs, spec)
		})
		if failed {
			return nil, false
		}
		logf("substrategy: spread assignment")
		return []ast.Stmt{
			&ast.DeclStmt{
				Decl: &ast.GenDecl{
					Tok:   token.VAR,
					Specs: specs,
				},
			},
			&ast.AssignStmt{
				Lhs: callerStmt.Lhs,
				Tok: token.ASSIGN,
				Rhs: returnOperands,
			},
		}, true
	}

	assert(len(lhs) == len(rhs), "mismatching LHS and RHS")

	// Substrategy "convert": write out RHS expressions with explicit type conversions as necessary.
	var origIdxs []int // maps back to original indexes after lhs and rhs are pruned
	i := 0
	for j := range lhs {
		if _, ok := nilBlankAssigns[j]; !ok {
			lhs[i] = lhs[j]
			rhs[i] = rhs[j]
			origIdxs = append(origIdxs, j)
			i++
		}
	}
	lhs = lhs[:i]
	rhs = rhs[:i]

	if len(lhs) == 0 {
		logf("trivial assignment after pruning nil blanks assigns")
		// After pruning, we have no remaining assignments. Signal this by returning a non-nil slice of statements.
		return nil, true
	}

	// Write out explicit conversions as necessary.
	//
	// A conversion is necessary if the LHS is being defined, and the RHS return
	// involved a nontrivial implicit conversion.
	for i, expr := range rhs {
		idx := origIdxs[i]
		if nonTrivial[idx] && defs[idx] != nil {
			typ := caller.Info.TypeOf(lhs[i])
			texpr := typeExpr(typ, nil)
			if texpr == nil {
				return nil, false
			}
			if _, ok := texpr.(*ast.StarExpr); ok {
				// TODO(rfindley): is this necessary? Doesn't the formatter add these parens?
				texpr = &ast.ParenExpr{X: texpr} // *T -> (*T) so that (*T)(x) is valid
			}
			rhs[i] = &ast.CallExpr{
				Fun:  texpr,
				Args: []ast.Expr{expr},
			}
		}
	}
	logf("substrategy: convert assignment")
	return []ast.Stmt{&ast.AssignStmt{
		Lhs: lhs,
		Tok: callerStmt.Tok,
		Rhs: rhs,
	}}, true
}

// tailCallSafeReturn reports whether the callee's return statements may be safely
// used to return from the function enclosing the caller (which must exist).
func tailCallSafeReturn(caller *Caller, calleeSymbol *types.Func, callee *gobCallee) bool {
	// It is safe if all callee returns involve only trivial conversions.
	if !hasNonTrivialReturn(callee.Returns) {
		return true
	}

	var callerType types.Type
	// Find type of innermost function enclosing call. (Beware: Caller.enclosingFunc is the outermost.)
loop:
	for _, n := range caller.path {
		switch f := n.(type) {
		case *ast.FuncDecl:
			callerType = caller.Info.ObjectOf(f.Name).Type()
			break loop
		case *ast.FuncLit:
			callerType = caller.Info.TypeOf(f)
			break loop
		}
	}

	// Non-trivial return conversions in the callee are permitted if the same non-trivial conversion would occur after inlining.
	callerResults := callerType.(*types.Signature).Results()
	calleeResults := calleeSymbol.Type().(*types.Signature).Results()
	return types.Identical(callerResults, calleeResults)
}

// hasNonTrivialReturn reports whether any of the returns involve a nontrivial
// implicit conversion of a result expression.
func hasNonTrivialReturn(returnInfo [][]returnOperandFlags) bool {
	for _, resultInfo := range returnInfo {
		for _, r := range resultInfo {
			if r&nonTrivialResult != 0 {
				return true
			}
		}
	}
	return false
}

type unit struct{} // for representing sets as maps

// defaultLiteral spells a parameter's default as the source a caller would
// have written. It answers nil for a value with no such spelling, which the
// type checker has already refused at the declaration.
func defaultLiteral(v constant.Value, pos token.Pos) ast.Expr {
	if v == nil {
		return nil
	}
	switch v.Kind() {
	case constant.Bool:
		if constant.BoolVal(v) {
			return &ast.Ident{NamePos: pos, Name: "true"}
		}
		return &ast.Ident{NamePos: pos, Name: "false"}
	case constant.String:
		return &ast.BasicLit{ValuePos: pos, Kind: token.STRING, Value: strconv.Quote(constant.StringVal(v))}
	case constant.Int:
		return &ast.BasicLit{ValuePos: pos, Kind: token.INT, Value: v.ExactString()}
	}
	return nil
}
