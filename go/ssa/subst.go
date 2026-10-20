// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"fmt"
	"go/types"

	"golang.org/x/tools/go/types/typeutil"
	"golang.org/x/tools/internal/aliases"
)

// subster defines a type substitution operation of a set of type parameters
// to type parameter free replacement types.
type subster struct {
	replacements map[*types.TypeParam]types.Type // values should contain no type params
	cache        map[types.Type]types.Type       // cache of subst results
	origin       *types.Func                     // types.Objects declared within this origin function are unique within this context
	ctxt         *types.Context                  // speeds up repeated instantiations
	uniqueness   typeutil.Map                    // determines the uniqueness of the instantiations within the function
}

// Returns a subster that replaces rtparams[i] with rtargs[i] and tparams[i] with targs[i].
// Uses ctxt as a cache. rtargs and targs should not contain any types in rtparams or tparams.
// fn is the generic function for which we are substituting.
func makeSubster(ctxt *types.Context, fn *types.Func, rtparams *types.TypeParamList, rtargs []types.Type, tparams *types.TypeParamList, targs []types.Type) *subster {
	got := len(rtargs) + len(targs)
	want := rtparams.Len() + tparams.Len()
	if got != want {
		panic(fmt.Sprintf("makeSubster argument count must match: got %d; want %d", got, want))
	}

	subst := &subster{
		replacements: make(map[*types.TypeParam]types.Type, want),
		cache:        make(map[types.Type]types.Type),
		origin:       fn.Origin(),
		ctxt:         ctxt,
	}
	for i := 0; i < rtparams.Len(); i++ {
		subst.replacements[rtparams.At(i)] = rtargs[i]
	}
	for i := 0; i < tparams.Len(); i++ {
		subst.replacements[tparams.At(i)] = targs[i]
	}
	return subst
}

// typ returns the type of t with the type parameter tparams[i] substituted
// for the type targs[i]. This happens where subst was created using
// tparams and targs.
func (subst *subster) typ(t types.Type) (res types.Type) {
	if subst == nil {
		return t // A nil subst is type preserving.
	}
	if r, ok := subst.cache[t]; ok {
		return r
	}
	defer func() {
		subst.cache[t] = res
	}()

	switch t := t.(type) {
	case *types.TypeParam:
		if r := subst.replacements[t]; r != nil {
			return r
		}
		return t

	case *types.Basic:
		return t

	case *types.Array:
		if r := subst.typ(t.Elem()); r != t.Elem() {
			return types.NewArray(r, t.Len())
		}
		return t

	case *types.Slice:
		if r := subst.typ(t.Elem()); r != t.Elem() {
			return types.NewSlice(r)
		}
		return t

	case *types.Pointer:
		if r := subst.typ(t.Elem()); r != t.Elem() {
			return types.NewPointer(r)
		}
		return t

	case *types.Tuple:
		return subst.tuple(t)

	case *types.Struct:
		return subst.struct_(t)

	case *types.Map:
		key := subst.typ(t.Key())
		elem := subst.typ(t.Elem())
		if key != t.Key() || elem != t.Elem() {
			return types.NewMap(key, elem)
		}
		return t

	case *types.Chan:
		if elem := subst.typ(t.Elem()); elem != t.Elem() {
			return types.NewChan(t.Dir(), elem)
		}
		return t

	case *types.Signature:
		return subst.signature(t)

	case *types.Union:
		return subst.union(t)

	case *types.Interface:
		return subst.interface_(t)

	case *types.Alias:
		return subst.alias(t)

	case *types.Named:
		return subst.named(t)

	default:
		panic("unreachable")
	}
}

// types returns the result of {subst.typ(ts[i])}.
func (subst *subster) types(ts []types.Type) []types.Type {
	res := make([]types.Type, len(ts))
	for i := range ts {
		res[i] = subst.typ(ts[i])
	}
	return res
}

func (subst *subster) tuple(t *types.Tuple) *types.Tuple {
	if t != nil {
		if vars := subst.varlist(t); vars != nil {
			return types.NewTuple(vars...)
		}
	}
	return t
}

type varlist interface {
	At(i int) *types.Var
	Len() int
}

// fieldlist is an adapter for structs for the varlist interface.
type fieldlist struct {
	str *types.Struct
}

func (fl fieldlist) At(i int) *types.Var { return fl.str.Field(i) }
func (fl fieldlist) Len() int            { return fl.str.NumFields() }

func (subst *subster) struct_(t *types.Struct) *types.Struct {
	if t != nil {
		if fields := subst.varlist(fieldlist{t}); fields != nil {
			tags := make([]string, t.NumFields())
			for i, n := 0, t.NumFields(); i < n; i++ {
				tags[i] = t.Tag(i)
			}
			return types.NewStruct(fields, tags)
		}
	}
	return t
}

// varlist returns subst(in[i]) or return nils if subst(v[i]) == v[i] for all i.
func (subst *subster) varlist(in varlist) []*types.Var {
	var out []*types.Var // nil => no updates
	for i, n := 0, in.Len(); i < n; i++ {
		v := in.At(i)
		w := subst.var_(v)
		if v != w && out == nil {
			out = make([]*types.Var, n)
			for j := 0; j < i; j++ {
				out[j] = in.At(j)
			}
		}
		if out != nil {
			out[i] = w
		}
	}
	return out
}

func (subst *subster) var_(v *types.Var) *types.Var {
	if v != nil {
		if typ := subst.typ(v.Type()); typ != v.Type() {
			if v.IsField() {
				return types.NewField(v.Pos(), v.Pkg(), v.Name(), typ, v.Embedded())
			}
			return types.NewParam(v.Pos(), v.Pkg(), v.Name(), typ)
		}
	}
	return v
}

func (subst *subster) union(u *types.Union) *types.Union {
	var out []*types.Term // nil => no updates

	for i, n := 0, u.Len(); i < n; i++ {
		t := u.Term(i)
		r := subst.typ(t.Type())
		if r != t.Type() && out == nil {
			out = make([]*types.Term, n)
			for j := 0; j < i; j++ {
				out[j] = u.Term(j)
			}
		}
		if out != nil {
			out[i] = types.NewTerm(t.Tilde(), r)
		}
	}

	if out != nil {
		return types.NewUnion(out)
	}
	return u
}

func (subst *subster) interface_(iface *types.Interface) *types.Interface {
	if iface == nil {
		return nil
	}

	// methods for the interface. Initially nil if there is no known change needed.
	var methods []*types.Func
	initMethods := func(n int) { // copy first n explicit methods
		methods = make([]*types.Func, iface.NumExplicitMethods())
		for i := range n {
			f := iface.ExplicitMethod(i)
			norecv := changeRecv(f.Type().(*types.Signature), nil)
			methods[i] = types.NewFunc(f.Pos(), f.Pkg(), f.Name(), norecv)
		}
	}
	for i := 0; i < iface.NumExplicitMethods(); i++ {
		f := iface.ExplicitMethod(i)
		// On interfaces, we need to cycle break on anonymous interface types being in a cycle with their signatures being in cycles with their receivers.
		norecv := changeRecv(f.Type().(*types.Signature), nil)
		sig := subst.typ(norecv)
		if sig != norecv && methods == nil {
			initMethods(i)
		}
		if methods != nil {
			methods[i] = types.NewFunc(f.Pos(), f.Pkg(), f.Name(), sig.(*types.Signature))
		}
	}

	var embeds []types.Type
	initEmbeds := func(n int) { // copy first n embedded types
		embeds = make([]types.Type, iface.NumEmbeddeds())
		for i := range n {
			embeds[i] = iface.EmbeddedType(i)
		}
	}
	for i := 0; i < iface.NumEmbeddeds(); i++ {
		e := iface.EmbeddedType(i)
		r := subst.typ(e)
		if e != r && embeds == nil {
			initEmbeds(i)
		}
		if embeds != nil {
			embeds[i] = r
		}
	}

	if methods == nil && embeds == nil {
		return iface
	}
	if methods == nil {
		initMethods(iface.NumExplicitMethods())
	}
	if embeds == nil {
		initEmbeds(iface.NumEmbeddeds())
	}
	return types.NewInterfaceType(methods, embeds).Complete()
}

func (subst *subster) alias(t *types.Alias) types.Type {
	// See subster.named. This follows the same strategy.
	tparams := t.TypeParams()
	targs := t.TypeArgs()
	tname := t.Obj()
	torigin := t.Origin()

	if !declaredWithin(tname, subst.origin) {
		// t is declared outside of the function origin. So t is a package level type alias.
		if targs.Len() == 0 {
			// No type arguments so no instantiation needed.
			return t
		}

		// Instantiate with the substituted type arguments.
		newTArgs := subst.typelist(targs)
		return subst.instantiate(torigin, newTArgs)
	}

	if targs.Len() == 0 {
		// t is declared within the function origin and has no type arguments.

		// Copy and substitute type params.
		var newTParams []*types.TypeParam
		for cur := range tparams.TypeParams() {
			cobj := cur.Obj()
			cname := types.NewTypeName(cobj.Pos(), cobj.Pkg(), cobj.Name(), nil)
			ntp := types.NewTypeParam(cname, nil)
			subst.cache[cur] = ntp // See the comment "Note: Subtle" in subster.named.
			newTParams = append(newTParams, ntp)
		}

		// Substitute rhs.
		rhs := subst.typ(t.Rhs())

		// Create the fresh alias.
		obj := aliases.New(tname.Pos(), tname.Pkg(), tname.Name(), rhs, newTParams)

		// Substitute into all of the constraints after they are created.
		for i, ntp := range newTParams {
			bound := tparams.At(i).Constraint()
			ntp.SetConstraint(subst.typ(bound))
		}
		return obj.Type()
	}

	// t is declared within the function origin and has type arguments.
	subOrigin := subst.typ(torigin)
	subTArgs := subst.typelist(targs)
	return subst.instantiate(subOrigin, subTArgs)
}

func (subst *subster) named(t *types.Named) types.Type {
	// A Named type is a user defined type.
	tparams := t.TypeParams() // x
	targs := t.TypeArgs()

	if !declaredWithin(t.Obj(), subst.origin) {
		// t is declared outside of Fn[m].
		if targs.Len() == 0 {
			// t has no type arguments. So it does not need to be instantiated.
			return t
		}

		// t is declared outside of Fn[m] and has type arguments.
		newTArgs := subst.typelist(targs)
		return subst.instantiate(t.Origin(), newTArgs)
	}

	// t is declared within Fn[m].

	if targs.Len() == 0 { // no type arguments?
		assert(t == t.Origin(), "local parameterized type abstraction must be an origin type")

		// t has no type arguments.
		tname := t.Obj()
		obj := types.NewTypeName(tname.Pos(), tname.Pkg(), tname.Name(), nil)
		fresh := types.NewNamed(obj, nil, nil)
		var newTParams []*types.TypeParam
		for cur := range tparams.TypeParams() {
			cobj := cur.Obj()
			cname := types.NewTypeName(cobj.Pos(), cobj.Pkg(), cobj.Name(), nil)
			ntp := types.NewTypeParam(cname, nil)
			subst.cache[cur] = ntp
			newTParams = append(newTParams, ntp)
		}
		fresh.SetTypeParams(newTParams)
		subst.cache[t] = fresh
		subst.cache[fresh] = fresh
		fresh.SetUnderlying(subst.typ(t.Underlying()))
		// Substitute into all of the constraints after they are created.
		for i, ntp := range newTParams {
			bound := tparams.At(i).Constraint()
			ntp.SetConstraint(subst.typ(bound))
		}
		return fresh
	}

	// t is defined within Fn[m] and t has type arguments (an instantiation).
	subOrigin := subst.typ(t.Origin())
	subTArgs := subst.typelist(targs)
	return subst.instantiate(subOrigin, subTArgs)
}

func (subst *subster) instantiate(orig types.Type, targs []types.Type) types.Type {
	i, err := types.Instantiate(subst.ctxt, orig, targs, false)
	assert(err == nil, "failed to Instantiate named (Named or Alias) type")
	if c, _ := subst.uniqueness.At(i).(types.Type); c != nil {
		return c.(types.Type)
	}
	subst.uniqueness.Set(i, i)
	return i
}

func (subst *subster) typelist(l *types.TypeList) []types.Type {
	res := make([]types.Type, l.Len())
	for i := 0; i < l.Len(); i++ {
		res[i] = subst.typ(l.At(i))
	}
	return res
}

func (subst *subster) signature(t *types.Signature) types.Type {
	tparams := t.TypeParams()

	// We are choosing not to support tparams.Len() > 0 until a need has been observed in practice.

	assert(tparams.Len() == 0, "Substituting types.Signatures with generic functions are currently unsupported.")

	// Receivers can be either: named pointer to named interface nil interface is the problematic case.
	recv := subst.var_(t.Recv())
	params := subst.tuple(t.Params())
	results := subst.tuple(t.Results())
	if recv != t.Recv() || params != t.Params() || results != t.Results() {
		return types.NewSignatureType(recv, nil, nil, params, results, t.Variadic())
	}
	return t
}
