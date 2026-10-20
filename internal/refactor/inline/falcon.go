// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package inline

// This file defines the callee side of the "fallible constant" analysis.

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/format"
	"go/token"
	"go/types"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/types/typeutil"
	"golang.org/x/tools/internal/typeparams"
)

// falconResult is the result of the analysis of the callee.
type falconResult struct {
	Types       []falconType // types for falcon constraint environment
	Constraints []string     // constraints (Go expressions) on values of fallible constants
}

// A falconType specifies the name and underlying type of a synthetic
// defined type for use in falcon constraints.
type falconType struct {
	Name string
	Kind types.BasicKind // string/number/bool
}

// falcon identifies "fallible constant" expressions, which are expressions
// that may fail to compile if one or more of their operands is.
//
// Consider:
//
//	func sub(s string, i, j int) string { return s[i:j] }
//
// If parameters are replaced by constants, the compiler is required to
// perform these additional checks:
//
//   - if i is constant, 0 <= i.
//   - if s and i are constant, i <= len(s).
//   - ditto for j.
//   - if i and j are constant, i <= j.
func falcon(logf func(string, ...any), fset *token.FileSet, params map[*types.Var]*paramInfo, info *types.Info, decl *ast.FuncDecl) falconResult {

	st := &falconState{
		logf:   logf,
		fset:   fset,
		params: params,
		info:   info,
		decl:   decl,
	}

	// type mapping
	st.int = st.typename(types.Typ[types.Int])
	st.any = "interface{}" // don't use "any" as it may be shadowed

	// Sort params by Index for determinism
	sortedParams := make([]*types.Var, 0, len(st.params))
	for obj := range st.params {
		if isBasic(obj.Type(), types.IsConstType) {
			sortedParams = append(sortedParams, obj)
		}
	}
	slices.SortFunc(sortedParams, func(a, b *types.Var) int {
		return st.params[a].Index - st.params[b].Index
	})
	for _, obj := range sortedParams {
		st.params[obj].FalconType = st.typename(obj.Type())
	}

	st.stmt(st.decl.Body)

	return st.result
}

type falconState struct {
	// inputs
	logf   func(string, ...any)
	fset   *token.FileSet
	params map[*types.Var]*paramInfo
	info   *types.Info
	decl   *ast.FuncDecl

	// working state
	int       string
	any       string
	typenames typeutil.Map

	result falconResult
}

// typename returns the name in the falcon constraint system of a given
// string/number/bool type t. Falcon types are specified directly in go/types
// data structures rather than by name, avoiding potential shadowing conflicts
// with confusing parameter names such as "int".
func (st *falconState) typename(t types.Type) string {
	name, ok := st.typenames.At(t).(string)
	if !ok {
		basic := t.Underlying().(*types.Basic)

		// It is unlikely to appear in a real program.
		name = fmt.Sprintf("%s۰%d", basic, st.typenames.Len())
		st.typenames.Set(t, name)
		st.logf("falcon: emit type %s %s // %q", name, basic, t)
		st.result.Types = append(st.result.Types, falconType{
			Name: name,
			Kind: basic.Kind(),
		})
	}
	return name
}

// -- constraint emission --

// emit emits a Go expression that must have a legal type. In effect, we let
// the go/types constant folding algorithm do most of the heavy lifting
// (though it may be hard to believe from the complexity of this algorithm!).
func (st *falconState) emit(constraint ast.Expr) {
	var out strings.Builder
	if err := format.Node(&out, st.fset, constraint); err != nil {
		panic(err) // can't happen
	}
	syntax := out.String()
	st.logf("falcon: emit constraint %s", syntax)
	st.result.Constraints = append(st.result.Constraints, syntax)
}

// emitNonNegative emits an []T{}[index] constraint,
// which ensures index is non-negative if constant.
func (st *falconState) emitNonNegative(index ast.Expr) {
	st.emit(&ast.IndexExpr{
		X: &ast.CompositeLit{
			Type: &ast.ArrayType{
				Elt: makeIdent(st.int),
			},
		},
		Index: index,
	})
}

// emitMonotonic emits an []T{}[i:j] constraint,
// which ensures i <= j if both are constant.
func (st *falconState) emitMonotonic(i, j ast.Expr) {
	st.emit(&ast.SliceExpr{
		X: &ast.CompositeLit{
			Type: &ast.ArrayType{
				Elt: makeIdent(st.int),
			},
		},
		Low:  i,
		High: j,
	})
}

// T may be a map, slice, or array depending on the desired check semantics.
func (st *falconState) emitUnique(typ ast.Expr, elems []ast.Expr) {
	if len(elems) > 1 {
		var elts []ast.Expr
		for _, elem := range elems {
			elts = append(elts, &ast.KeyValueExpr{
				Key:   elem,
				Value: makeIntLit(0),
			})
		}
		st.emit(&ast.CompositeLit{
			Type: typ,
			Elts: elts,
		})
	}
}

// -- traversal --

// The traversal functions scan the callee body for expressions that are not
// constant but would become constant. This happens if the parameter vars were
// redeclared as constants. Emits for each a constraint (a Go expression) with
// the property that it will not type-check (using types.CheckExpr) if the
// argument values are unsuitable.
//
// These constraints are checked by Inline with the actual constant argument
// values. Violations cause it to reject parameters as candidates for
// substitution.

func (st *falconState) stmt(s ast.Stmt) {
	ast.Inspect(s, func(n ast.Node) bool {
		switch n := n.(type) {
		case ast.Expr:
			_ = st.expr(n)
			return false // skip usual traversal

		case *ast.AssignStmt:
			switch n.Tok {
			case token.QUO_ASSIGN, token.REM_ASSIGN:
				// x /= y Possible "integer division by zero" Emit constraint: 1/y.
				_ = st.expr(n.Lhs[0])
				kY := st.expr(n.Rhs[0])
				if kY, ok := kY.(ast.Expr); ok {
					op := token.QUO
					if n.Tok == token.REM_ASSIGN {
						op = token.REM
					}
					st.emit(&ast.BinaryExpr{
						Op: op,
						X:  makeIntLit(1),
						Y:  kY,
					})
				}
				return false // skip usual traversal
			}

		case *ast.SwitchStmt:
			if n.Init != nil {
				st.stmt(n.Init)
			}
			tBool := types.Type(types.Typ[types.Bool])
			tagType := tBool // default: true
			if n.Tag != nil {
				st.expr(n.Tag)
				tagType = st.info.TypeOf(n.Tag)
			}

			// Possible "duplicate case value".
			var unique []ast.Expr
			for _, clause := range n.Body.List {
				clause := clause.(*ast.CaseClause)
				for _, caseval := range clause.List {
					if k := st.expr(caseval); k != nil {
						unique = append(unique, st.toExpr(k))
					}
				}
				for _, stmt := range clause.Body {
					st.stmt(stmt)
				}
			}
			if unique != nil && !types.Identical(tagType.Underlying(), tBool) {
				tname := st.any
				if !types.IsInterface(tagType) {
					tname = st.typename(tagType)
				}
				t := &ast.MapType{
					Key:   makeIdent(tname),
					Value: makeIdent(st.int),
				}
				st.emitUnique(t, unique)
			}
		}
		return true
	})
}

// fieldTypes visits the .Type of each field in the list.
func (st *falconState) fieldTypes(fields *ast.FieldList) {
	if fields != nil {
		for _, field := range fields.List {
			_ = st.expr(field.Type)
		}
	}
}

// expr visits the expression (or type). It returns a non-nil result if the
// expression is constant or would become constant if all suitable function
// parameters were redeclared as constants.
//
// If the expression is constant, st.expr returns its type and value
// (types.TypeAndValue). St.expr returns an ast.Expr tree whose leaves are
// literals and parameter references. Whose interior nodes are operations
// that may become constant, such as -x, x+y, f(x), and T(x). This happens if
// the expression would become constant. We call these would-be constant
// expressions "fallible constants", since they may fail to type-check for
// some values of x, i, and j. (We refer to the non-nil cases collectively as
// "maybe constant", and the nil case as "definitely non-constant".)
func (st *falconState) expr(e ast.Expr) (res any) { // = types.TypeAndValue | ast.Expr
	tv := st.info.Types[e]
	if tv.Value != nil {
		// A constant value overrides any other result.
		defer func() { res = tv }()
	}

	switch e := e.(type) {
	case *ast.Ident:
		if v, ok := st.info.Uses[e].(*types.Var); ok {
			if _, ok := st.params[v]; ok && isBasic(v.Type(), types.IsConstType) {
				return e // reference to constable parameter
			}
		}
		// (References to *types.Const are handled by the defer.)

	case *ast.BasicLit:
		// constant

	case *ast.ParenExpr:
		return st.expr(e.X)

	case *ast.FuncLit:
		_ = st.expr(e.Type)
		st.stmt(e.Body)
		// definitely non-constant

	case *ast.CompositeLit:
		// T{k: v, ...}, where T ∈ {array,*array,slice,map}, imposes a constraint
		// that all constant k are distinct and, for arrays [n]T.
		if e.Type != nil {
			_ = st.expr(e.Type)
		}
		t := types.Unalias(typeparams.Deref(tv.Type))
		ct := typeparams.CoreType(t)
		var mapKeys []ast.Expr // map key expressions; must be distinct if constant
		for _, elt := range e.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if is[*types.Map](ct) {
					if k := st.expr(kv.Key); k != nil {
						mapKeys = append(mapKeys, st.toExpr(k))
					}
				}
				_ = st.expr(kv.Value)
			} else {
				_ = st.expr(elt)
			}
		}
		if len(mapKeys) > 0 {
			// Inlining a map literal may replace variable key expressions by constants.
			t := ct.(*types.Map)
			var typ ast.Expr
			if types.IsInterface(t.Key()) {
				typ = &ast.MapType{
					Key:   makeIdent(st.any),
					Value: makeIdent(st.int),
				}
			} else {
				typ = &ast.MapType{
					Key:   makeIdent(st.typename(t.Key())),
					Value: makeIdent(st.int),
				}
			}
			st.emitUnique(typ, mapKeys)
		}
		// definitely non-constant

	case *ast.SelectorExpr:
		_ = st.expr(e.X)
		_ = st.expr(e.Sel)
		// The defer is sufficient to handle qualified identifiers (pkg.Const). All
		// other cases are definitely non-constant.

	case *ast.IndexExpr:
		if tv.IsType() {
			// type C[T]
			_ = st.expr(e.X)
			_ = st.expr(e.Index)
		} else {
			// term x[i].
			kX := st.expr(e.X)
			kI := st.expr(e.Index)
			if kI != nil && !is[*types.Map](st.info.TypeOf(e.X).Underlying()) {
				if kI, ok := kI.(ast.Expr); ok {
					st.emitNonNegative(kI)
				}
				// Emit constraint to check indices against known length. TODO(adonovan): factor with SliceExpr logic.
				var x ast.Expr
				if kX != nil {
					// string
					x = st.toExpr(kX)
				} else if arr, ok := typeparams.CoreType(typeparams.Deref(st.info.TypeOf(e.X))).(*types.Array); ok {
					// array, *array
					x = &ast.CompositeLit{
						Type: &ast.ArrayType{
							Len: makeIntLit(arr.Len()),
							Elt: makeIdent(st.int),
						},
					}
				}
				if x != nil {
					st.emit(&ast.IndexExpr{
						X:     x,
						Index: st.toExpr(kI),
					})
				}
			}
		}
		// definitely non-constant

	case *ast.SliceExpr:
		// x[low:high:max].

		kX := st.expr(e.X)
		var kLow, kHigh, kMax any
		if e.Low != nil {
			kLow = st.expr(e.Low)
			if kLow != nil {
				if kLow, ok := kLow.(ast.Expr); ok {
					st.emitNonNegative(kLow)
				}
			}
		}
		if e.High != nil {
			kHigh = st.expr(e.High)
			if kHigh != nil {
				if kHigh, ok := kHigh.(ast.Expr); ok {
					st.emitNonNegative(kHigh)
				}
				if kLow != nil {
					st.emitMonotonic(st.toExpr(kLow), st.toExpr(kHigh))
				}
			}
		}
		if e.Max != nil {
			kMax = st.expr(e.Max)
			if kMax != nil {
				if kMax, ok := kMax.(ast.Expr); ok {
					st.emitNonNegative(kMax)
				}
				if kHigh != nil {
					st.emitMonotonic(st.toExpr(kHigh), st.toExpr(kMax))
				}
			}
		}

		// Emit constraint to check indices against known length.
		var x ast.Expr
		if kX != nil {
			// string
			x = st.toExpr(kX)
		} else if arr, ok := typeparams.CoreType(typeparams.Deref(st.info.TypeOf(e.X))).(*types.Array); ok {
			// array, *array
			x = &ast.CompositeLit{
				Type: &ast.ArrayType{
					Len: makeIntLit(arr.Len()),
					Elt: makeIdent(st.int),
				},
			}
		}
		if x != nil {
			// Avoid slice[::max] if kHigh is nonconstant (nil).
			high, max := st.toExpr(kHigh), st.toExpr(kMax)
			if high == nil {
				high = max // => slice[:max:max]
			}
			st.emit(&ast.SliceExpr{
				X:    x,
				Low:  st.toExpr(kLow),
				High: high,
				Max:  max,
			})
		}
		// definitely non-constant

	case *ast.TypeAssertExpr:
		_ = st.expr(e.X)
		if e.Type != nil {
			_ = st.expr(e.Type)
		}

	case *ast.CallExpr:
		_ = st.expr(e.Fun)
		if tv, ok := st.info.Types[e.Fun]; ok && tv.IsType() {
			// conversion T(x).
			kX := st.expr(e.Args[0])
			if kX != nil && isBasic(tv.Type, types.IsConstType) {
				conv := convert(makeIdent(st.typename(tv.Type)), st.toExpr(kX))
				if is[ast.Expr](kX) {
					st.emit(conv)
				}
				return conv
			}
			return nil // definitely non-constant
		}

		// call f(x)

		all := true // all args are possibly-constant
		kArgs := make([]ast.Expr, len(e.Args))
		for i, arg := range e.Args {
			if kArg := st.expr(arg); kArg != nil {
				kArgs[i] = st.toExpr(kArg)
			} else {
				all = false
			}
		}

		// Calls to built-ins with fallibly constant arguments may become constant.
		// All other calls are either constant or non-constant
		if id, ok := e.Fun.(*ast.Ident); ok && all && tv.Value == nil {
			if builtin, ok := st.info.Uses[id].(*types.Builtin); ok {
				switch builtin.Name() {
				case "len", "imag", "real", "complex", "min", "max":
					return &ast.CallExpr{
						Fun:      id,
						Args:     kArgs,
						Ellipsis: e.Ellipsis,
					}
				}
			}
		}

	case *ast.StarExpr: // *T, *ptr
		_ = st.expr(e.X)

	case *ast.UnaryExpr:
		// Possible "negation of minint". Emit constraint: -x
		kX := st.expr(e.X)
		if kX != nil && !is[types.TypeAndValue](kX) {
			if e.Op == token.SUB {
				st.emit(&ast.UnaryExpr{
					Op: e.Op,
					X:  st.toExpr(kX),
				})
			}

			return &ast.UnaryExpr{
				Op: e.Op,
				X:  st.toExpr(kX),
			}
		}

	case *ast.BinaryExpr:
		kX := st.expr(e.X)
		kY := st.expr(e.Y)
		switch e.Op {
		case token.QUO, token.REM:
			// x/y, x%y
			//
			// Possible "integer division by zero" or "minint / -1" overflow. Emit
			// constraint: x/y or 1/y
			if kY != nil {
				if kX == nil {
					kX = makeIntLit(1)
				}
				st.emit(&ast.BinaryExpr{
					Op: e.Op,
					X:  st.toExpr(kX),
					Y:  st.toExpr(kY),
				})
			}

		case token.ADD, token.SUB, token.MUL:
			// x+y, x-y, x*y
			//
			// Possible "arithmetic overflow". Emit constraint: x+y
			if kX != nil && kY != nil {
				st.emit(&ast.BinaryExpr{
					Op: e.Op,
					X:  st.toExpr(kX),
					Y:  st.toExpr(kY),
				})
			}

		case token.SHL, token.SHR:
			// x << y, x >> y
			//
			// Possible "constant shift too large". Either operand may be too large
			// individually, and they may be too large together.
			if kX != nil || kY != nil {
				x := st.toExpr(kX)
				if x == nil {
					x = makeIntLit(1)
				}
				y := st.toExpr(kY)
				if y == nil {
					y = makeIntLit(0)
				}
				st.emit(&ast.BinaryExpr{
					Op: e.Op,
					X:  x,
					Y:  y,
				})
			}

		case token.LSS, token.GTR, token.EQL, token.NEQ, token.LEQ, token.GEQ:
		}
		if kX != nil && kY != nil {
			return &ast.BinaryExpr{
				Op: e.Op,
				X:  st.toExpr(kX),
				Y:  st.toExpr(kY),
			}
		}

	// types.

	case *ast.IndexListExpr:
		_ = st.expr(e.X)
		for _, expr := range e.Indices {
			_ = st.expr(expr)
		}

	case *ast.Ellipsis:
		if e.Elt != nil {
			_ = st.expr(e.Elt)
		}

	case *ast.ArrayType:
		if e.Len != nil {
			_ = st.expr(e.Len)
		}
		_ = st.expr(e.Elt)

	case *ast.StructType:
		st.fieldTypes(e.Fields)

	case *ast.FuncType:
		st.fieldTypes(e.TypeParams)
		st.fieldTypes(e.Params)
		st.fieldTypes(e.Results)

	case *ast.InterfaceType:
		st.fieldTypes(e.Methods)

	case *ast.MapType:
		_ = st.expr(e.Key)
		_ = st.expr(e.Value)

	case *ast.ChanType:
		_ = st.expr(e.Value)
	}
	return
}

// toExpr converts the result of visitExpr to a falcon expression.
// (We don't do this in visitExpr as we first need to discriminate
// constants from maybe-constants.)
func (st *falconState) toExpr(x any) ast.Expr {
	switch x := x.(type) {
	case nil:
		return nil

	case types.TypeAndValue:
		lit := makeLiteral(x.Value)
		if !isBasic(x.Type, types.IsUntyped) {
			// convert to "typed" type
			lit = &ast.CallExpr{
				Fun:  makeIdent(st.typename(x.Type)),
				Args: []ast.Expr{lit},
			}
		}
		return lit

	case ast.Expr:
		return x

	default:
		panic(x)
	}
}

func makeLiteral(v constant.Value) ast.Expr {
	switch v.Kind() {
	case constant.Bool:
		op := token.EQL
		if !constant.BoolVal(v) {
			op = token.NEQ
		}
		return &ast.BinaryExpr{
			Op: op,
			X:  makeIntLit(0),
			Y:  makeIntLit(0),
		}

	case constant.String:
		return &ast.BasicLit{
			Kind:  token.STRING,
			Value: v.ExactString(),
		}

	case constant.Int:
		return &ast.BasicLit{
			Kind:  token.INT,
			Value: v.ExactString(),
		}

	case constant.Float:
		return &ast.BasicLit{
			Kind:  token.FLOAT,
			Value: v.ExactString(),
		}

	case constant.Complex:
		// The components could be float or int.
		y := makeLiteral(constant.Imag(v))
		y.(*ast.BasicLit).Value += "i" // ugh
		if re := constant.Real(v); !consteq(re, kZeroInt) {
			// complex: x + yi
			y = &ast.BinaryExpr{
				Op: token.ADD,
				X:  makeLiteral(re),
				Y:  y,
			}
		}
		return y

	default:
		panic(v.Kind())
	}
}

func makeIntLit(x int64) *ast.BasicLit {
	return &ast.BasicLit{
		Kind:  token.INT,
		Value: strconv.FormatInt(x, 10),
	}
}

func isBasic(t types.Type, info types.BasicInfo) bool {
	basic, ok := t.Underlying().(*types.Basic)
	return ok && basic.Info()&info != 0
}
