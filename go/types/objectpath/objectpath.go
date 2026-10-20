// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package objectpath defines a naming scheme for types.Objects (that is,
// named entities in Go programs) relative to their enclosing package.
// Type-checker objects are canonical and usually identified by their address
// in memory. A pointer has meaning only within one address space. Objectpath
// names let an object's identity travel between programs, and establish a
// correspondence between types.Object variables that are distinct but
// logically equivalent. A single object may have multiple paths. In this
// example,
//
//	type A struct{ X int }
//	type B A
//
// the field X has paths due to its membership of both A and B. The For(obj)
// function returns one of them, arbitrarily but consistently.
package objectpath

import (
	"encoding/binary"
	"fmt"
	"go/types"
	"slices"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/tools/internal/typesinternal"
)

// TODO(adonovan): think about generic aliases.

// A Path is an opaque name that identifies a types.Object relative to its package.
type Path string

// Encoding.
const (
	// object->type operators
	opType = '.' // .Type() (Object)

	// type->type operators
	opElem          = 'E' // .Elem() (Pointer, Slice, Array, Chan, Map)
	opKey           = 'K' // .Key() (Map)
	opParams        = 'P' // .Params() (Signature)
	opResults       = 'R' // .Results() (Signature)
	opUnderlying    = 'U' // .Underlying() (Named)
	opTypeParam     = 'T' // .TypeParams.At(i) (Named, Signature)
	opRecvTypeParam = 'r' // .RecvTypeParams.At(i) (Signature)
	opConstraint    = 'C' // .Constraint() (TypeParam)
	opRhs           = 'a' // .Rhs() (Alias)

	// type->object operators
	opAt     = 'A' // .At(i) (Tuple)
	opField  = 'F' // .Field(i) (Struct)
	opMethod = 'M' // .Method(i) (Named or Interface; not Struct: "promoted" names are ignored)
	opObj    = 'O' // .Obj() (Named, TypeParam)
)

// For is equivalent to new(Encoder).For(obj).
//
// It may be more efficient to reuse a single Encoder across several calls.
func For(obj types.Object) (Path, error) {
	return new(Encoder).For(obj)
}

// An Encoder amortizes the cost of encoding the paths of multiple objects.
// The zero value of an Encoder is ready to use.
type Encoder struct {
	pkgIndexMu sync.Mutex
	pkgIndex   map[*types.Package]*pkgIndex
}

// A traversal encapsulates the state of a single traversal of the object/type graph.
type traversal struct {
	pkg *types.Package
	ix  *pkgIndex // non-nil if we are building the index

	target types.Object // the sought symbol (if ix == nil)
	found  Path         // the found path (if ix == nil)

	// These maps are used to short circuit cycles through interface methods.
	seenTParamNames map[*types.TypeName]bool // global cycle breaking through type parameters
	seenMethods     map[*types.Func]bool     // global cycle breaking through recursive interfaces
}

// A pkgIndex holds a compressed index of objectpaths of all symbols (fields,
// methods, params) requiring search for an entire package.
type pkgIndex struct {
	pkg        *types.Package
	data       []byte                  // encoding of traversal; nil if not yet constructed
	scopeNames []string                // memo of pkg.Scope().Names() to avoid O(n) alloc/sort at lookup
	offsets    map[types.Object]uint32 // each object's node offset within encoded traversal data
}

// For returns the path to an object relative to its package, or an error if
// the object is not accessible from the package's Scope.
//
// For is safe for concurrent use.
//
// The For function guarantees to return a path only for the following objects:
// - package-level types
// - exported package-level non-types
// - methods
// - parameter and result variables
// - struct fields
// These objects are sufficient to define the API of their package.
// The objects described by a package's export data are drawn from this set.
func (enc *Encoder) For(obj types.Object) (Path, error) {
	pkg := obj.Pkg()

	// This table lists the cases of interest.

	// 1. reference to nil or builtin?
	if pkg == nil {
		return "", fmt.Errorf("predeclared %s has no path", obj)
	}

	// 2. package-level object?
	if pkg.Scope().Lookup(obj.Name()) == obj {
		// Only exported objects (and non-exported types) have a path.
		// Non-exported types may be referenced by other objects.
		if _, ok := obj.(*types.TypeName); !ok && !obj.Exported() {
			return "", fmt.Errorf("no path for non-exported %v", obj)
		}
		return Path(obj.Name()), nil
	}

	// Not a package-level object. Reject non-viable cases.
	switch obj := obj.(type) {
	case *types.TypeName:
		if _, ok := types.Unalias(obj.Type()).(*types.TypeParam); !ok {
			// With the exception of type parameters, only package-level type names have a path.
			return "", fmt.Errorf("no path for %v", obj)
		}

	case *types.Const, // Only package-level constants have a path.
		*types.Label,   // Labels are function-local.
		*types.PkgName: // PkgNames are file-local.
		return "", fmt.Errorf("no path for %v", obj)

	case *types.Var:
		// A var, if not package-level, must be a
		// parameter (incl. receiver) or result, or a struct field.
		if obj.Kind() == types.LocalVar {
			return "", fmt.Errorf("no path for local %v", obj)
		}

	case *types.Func:
		// A func, if not package-level, must be a method.
		if recv := obj.Signature().Recv(); recv == nil {
			return "", fmt.Errorf("func is not a method: %v", obj)
		}

		if path, ok := enc.concreteMethod(obj); ok {
			// Fast path for concrete methods that avoids looping over scope.
			return path, nil
		}

	default:
		panic(obj)
	}

	enc.pkgIndexMu.Lock()
	defer enc.pkgIndexMu.Unlock()

	// 4. Search the object/type graph for the path to the var (field/param/result) or method.
	ix, ok := enc.pkgIndex[pkg]
	if !ok {
		// First search: don't build an index, traverse.
		ix = &pkgIndex{pkg: pkg}

		if enc.pkgIndex == nil {
			enc.pkgIndex = make(map[*types.Package]*pkgIndex)
		}
		enc.pkgIndex[pkg] = ix // build the index next time

		f := traversal{pkg: pkg, target: obj}
		f.traverse()

		if f.found != "" {
			return f.found, nil
		}
	} else {
		// Second search: build an index while traversing.
		if ix.data == nil {
			ix.offsets = make(map[types.Object]uint32)
			ix.data = []byte{0}
			(&traversal{pkg: pkg, ix: ix}).traverse()
		}

		// Second and later searches: consult the index.
		if offset, ok := ix.offsets[obj]; ok {
			return ix.path(offset), nil
		}
	}

	return "", fmt.Errorf("can't find path for %v in %s", obj, pkg.Path())
}

// traverse performs a complete traversal of all symbols reachable from the package.
func (tr *traversal) traverse() {
	scope := tr.pkg.Scope()
	names := scope.Names()
	if tr.ix != nil {
		tr.ix.scopeNames = names
	}

	empty := make([]byte, 0, 48) // initial space for stack (ix == nil)

	// First inspect package-level type names. In the presence of path aliases,
	// these give the best paths because non-types may refer to types, but not
	// the reverse.
	for i, name := range names {
		if tr.found != "" {
			return // found (ix == nil)
		}

		obj := scope.Lookup(name)
		if _, ok := obj.(*types.TypeName); !ok {
			continue // handle non-types in second pass
		}

		// emit (name, opType)
		var path []byte
		var offset uint32
		if tr.ix == nil {
			path = append(empty, name...)
			path = append(path, opType)
		} else {
			offset = tr.ix.emitPackageLevel(i)
			tr.ix.offsets[obj] = offset
			offset = tr.ix.emitPathSegment(offset, opType, -1)
		}

		// A TypeName (for Named or Alias) may have type parameters.
		switch t := obj.Type().(type) {
		case *types.Alias:
			tr.tparams(t.TypeParams(), path, offset, opTypeParam)
			tr.typ(path, offset, opRhs, -1, t.Rhs())
		case *types.Named:
			tr.tparams(t.TypeParams(), path, offset, opTypeParam)
			tr.typ(path, offset, opUnderlying, -1, t.Underlying())
		}
	}

	// Then inspect everything else:
	// exported non-types, and declared methods of defined types.
	for i, name := range names {
		if tr.found != "" {
			return // found (ix == nil)
		}

		obj := scope.Lookup(name)

		if tname, ok := obj.(*types.TypeName); !ok {
			if obj.Exported() {
				// exported non-type (const, var, func)
				var path []byte
				var offset uint32
				if tr.ix == nil {
					path = append(empty, name...)
				} else {
					offset = tr.ix.emitPackageLevel(i)
					tr.ix.offsets[obj] = offset
				}
				tr.typ(path, offset, opType, -1, obj.Type())
			}

		} else if T, ok := types.Unalias(tname.Type()).(*types.Named); ok {
			// defined type
			var path []byte
			var offset uint32
			if tr.ix == nil {
				path = append(empty, name...)
				path = append(path, opType)
			} else {
				// Inv: map entry for obj was populated in first pass.
				offset = tr.ix.emitPathSegment(tr.ix.offsets[obj], opType, -1)
			}

			// Inspect declared methods of defined types.
			for i := 0; i < T.NumMethods(); i++ {
				m := T.Method(i)
				tr.object(path, offset, opMethod, i, m)
			}
		}
	}
}

func (tr *traversal) visitType(path []byte, offset uint32, T types.Type) {
	switch T := T.(type) {
	case *types.Alias:
		tr.typ(path, offset, opRhs, -1, T.Rhs())

	case *types.Basic, *types.Named:
		// Named types belonging to pkg were handled already, so T must belong to another package. No path.
		return

	case *types.Pointer, *types.Slice, *types.Array, *types.Chan:
		type hasElem interface{ Elem() types.Type } // note: includes Map
		tr.typ(path, offset, opElem, -1, T.(hasElem).Elem())

	case *types.Map:
		tr.typ(path, offset, opKey, -1, T.Key())
		tr.typ(path, offset, opElem, -1, T.Elem())

	case *types.Signature:
		tr.tparams(T.RecvTypeParams(), path, offset, opRecvTypeParam)
		tr.tparams(T.TypeParams(), path, offset, opTypeParam)
		tr.typ(path, offset, opParams, -1, T.Params())
		tr.typ(path, offset, opResults, -1, T.Results())

	case *types.Struct:
		for i := 0; i < T.NumFields(); i++ {
			tr.object(path, offset, opField, i, T.Field(i))
		}

	case *types.Tuple:
		for i := 0; i < T.Len(); i++ {
			tr.object(path, offset, opAt, i, T.At(i))
		}

	case *types.Interface:
		for i := 0; i < T.NumMethods(); i++ {
			m := T.Method(i)
			if m.Pkg() != nil && m.Pkg() != tr.pkg {
				continue // embedded method from another package
			}
			if !tr.seenMethods[m] {
				if tr.seenMethods == nil {
					tr.seenMethods = make(map[*types.Func]bool)
				}
				tr.seenMethods[m] = true
				tr.object(path, offset, opMethod, i, m)
			}
		}

	case *types.TypeParam:
		tname := T.Obj()
		if tname.Pkg() != nil && tname.Pkg() != tr.pkg {
			return // type parameter from another package
		}
		if !tr.seenTParamNames[tname] {
			if tr.seenTParamNames == nil {
				tr.seenTParamNames = make(map[*types.TypeName]bool)
			}
			tr.seenTParamNames[tname] = true
			tr.object(path, offset, opObj, -1, tname)
			tr.typ(path, offset, opConstraint, -1, T.Constraint())
		}
	}
}

func (tr *traversal) tparams(list *types.TypeParamList, path []byte, offset uint32, op byte) {
	for i := 0; i < list.Len(); i++ {
		tr.typ(path, offset, op, i, list.At(i))
	}
}

// typ descends the type graph edge (op, index), then proceeds to traverse type t.
func (tr *traversal) typ(path []byte, offset uint32, op byte, index int, t types.Type) {
	if tr.ix == nil {
		path = appendOpArg(path, op, index)
	} else {
		offset = tr.ix.emitPathSegment(offset, op, index)
	}
	tr.visitType(path, offset, t)
}

// object descends the type graph edge (op, index), records object
// obj, then proceeds to traverse its type.
func (tr *traversal) object(path []byte, offset uint32, op byte, index int, obj types.Object) {
	if tr.ix == nil {
		path = appendOpArg(path, op, index)
		if obj == tr.target && tr.found == "" {
			tr.found = Path(path)
		}
		path = append(path, opType)
	} else {
		offset = tr.ix.emitPathSegment(offset, op, index)
		if _, ok := tr.ix.offsets[obj]; !ok {
			tr.ix.offsets[obj] = offset
		}
		offset = tr.ix.emitPathSegment(offset, opType, -1)
	}
	tr.visitType(path, offset, obj.Type())
}

// emitPackageLevel encodes a record for a package-level symbol,
// identified by its index in ix.scopeNames.
func (p *pkgIndex) emitPackageLevel(index int) uint32 {
	off := uint32(len(p.data))
	p.data = append(p.data, 0) // zero varint => no parent
	p.data = binary.AppendUvarint(p.data, uint64(index))
	return off
}

// emitPathSegment emits a record for a non-initial object path segment.
func (p *pkgIndex) emitPathSegment(parent uint32, op byte, index int) uint32 {
	off := uint32(len(p.data))
	p.data = binary.AppendUvarint(p.data, uint64(parent))
	p.data = append(p.data, op)
	switch op {
	case opAt, opField, opMethod, opTypeParam, opRecvTypeParam:
		p.data = binary.AppendUvarint(p.data, uint64(index))
	}
	return off
}

// path returns the Path for the encoded node at the specified offset.
func (p *pkgIndex) path(offset uint32) Path {
	var elems []string // path elements in reverse
	for {
		// Read parent index.
		parent, n := binary.Uvarint(p.data[offset:])
		offset += uint32(n)

		if parent == 0 {
			break // root (end of path)
		}

		op := p.data[offset]
		offset++

		// The [AFMTr] operators have a numeric operand.
		switch op {
		case opAt, opField, opMethod, opTypeParam, opRecvTypeParam:
			val, n := binary.Uvarint(p.data[offset:])
			offset += uint32(n)
			elems = append(elems, strconv.Itoa(int(val)))
		}

		elems = append(elems, string([]byte{op}))

		offset = uint32(parent)
	}
	idx, _ := binary.Uvarint(p.data[offset:])

	// Convert index to Path string.
	name := p.scopeNames[idx]
	sz := len(name)
	for _, elem := range elems {
		sz += len(elem)
	}
	var buf strings.Builder
	buf.Grow(sz)
	buf.WriteString(name)
	for _, elem := range slices.Backward(elems) {
		buf.WriteString(elem)
	}
	return Path(buf.String())
}

// appendOpArg appends (op, index) to the object path.
// A negative index is ignored.
func appendOpArg(path []byte, op byte, index int) []byte {
	path = append(path, op)
	if index >= 0 {
		path = strconv.AppendInt(path, int64(index), 10)
	}
	return path
}

// concreteMethod returns the path for meth, which must have a non-nil
// receiver. The second return value indicates success and may be false if the
// method is an interface method or if it is an instantiated method.
//
// This function is an optimization that avoids the general scope walking
// approach. You are expected to fall back to the general approach if this
// function fails.
func (enc *Encoder) concreteMethod(meth *types.Func) (Path, bool) {
	// Concrete methods can only be declared on package-scoped named types.

	if meth.Origin() != meth {
		return "", false
	}

	_, named := typesinternal.ReceiverNamed(meth.Signature().Recv())
	if named == nil {
		return "", false
	}

	if types.IsInterface(named) {
		// Named interfaces do not have to be package-scoped.
		return "", false
	}

	// Preallocate space for the name, opType, opMethod, and some digits.
	name := named.Obj().Name()
	path := make([]byte, 0, len(name)+8)
	path = append(path, name...)
	path = append(path, opType)

	// Method indices are w.r.t. the go/types data structures, ultimately
	// deriving from source order, which is preserved by export data.
	for i := 0; i < named.NumMethods(); i++ {
		if named.Method(i) == meth {
			path = appendOpArg(path, opMethod, i)
			return Path(path), true
		}
	}

	// Due to golang/go#go/types fails to associate the receiver with certain methods on cgo types.
	return "", false
}

// Object returns the object denoted by path p within the package pkg.
func Object(pkg *types.Package, p Path) (types.Object, error) {
	pathstr := string(p)
	if pathstr == "" {
		return nil, fmt.Errorf("empty path")
	}

	var pkgobj, suffix string
	if dot := strings.IndexByte(pathstr, opType); dot < 0 {
		pkgobj = pathstr
	} else {
		pkgobj = pathstr[:dot]
		suffix = pathstr[dot:] // suffix starts with "."
	}

	obj := pkg.Scope().Lookup(pkgobj)
	if obj == nil {
		return nil, fmt.Errorf("package %s does not contain %q", pkg.Path(), pkgobj)
	}

	// abstraction of *types.{Pointer,Slice,Array,Chan,Map}
	type hasElem interface {
		Elem() types.Type
	}
	// abstraction of *types.{Named,Signature}
	type hasTypeParams interface {
		TypeParams() *types.TypeParamList
	}
	// abstraction of *types.{Alias,Named,TypeParam}
	type hasObj interface {
		Obj() *types.TypeName
	}

	// The loop state is the pair (t, obj), exactly one of which is non-nil, initially obj. All suffixes start with '.'
	var t types.Type
	for suffix != "" {
		code := suffix[0]
		suffix = suffix[1:]

		// Codes [AFMTr] have an integer operand.
		var index int
		switch code {
		case opAt, opField, opMethod, opTypeParam, opRecvTypeParam:
			rest := strings.TrimLeft(suffix, "0123456789")
			numerals := suffix[:len(suffix)-len(rest)]
			suffix = rest
			i, err := strconv.Atoi(numerals)
			if err != nil {
				return nil, fmt.Errorf("invalid path: bad numeric operand %q for code %q", numerals, code)
			}
			index = int(i)
		case opObj:
			// no operand
		default:
			// The suffix must end with a type->object operation.
			if suffix == "" {
				return nil, fmt.Errorf("invalid path: ends with %q, want [AFMO]", code)
			}
		}

		if code == opType {
			if t != nil {
				return nil, fmt.Errorf("invalid path: unexpected %q in type context", opType)
			}
			t = obj.Type()
			obj = nil
			continue
		}

		if t == nil {
			return nil, fmt.Errorf("invalid path: code %q in object context", code)
		}

		// Inv: t != nil, obj == nil

		t = types.Unalias(t)
		switch code {
		case opElem:
			hasElem, ok := t.(hasElem) // Pointer, Slice, Array, Chan, Map
			if !ok {
				return nil, fmt.Errorf("cannot apply %q to %s (got %T, want pointer, slice, array, chan or map)", code, t, t)
			}
			t = hasElem.Elem()

		case opKey:
			mapType, ok := t.(*types.Map)
			if !ok {
				return nil, fmt.Errorf("cannot apply %q to %s (got %T, want map)", code, t, t)
			}
			t = mapType.Key()

		case opParams:
			sig, ok := t.(*types.Signature)
			if !ok {
				return nil, fmt.Errorf("cannot apply %q to %s (got %T, want signature)", code, t, t)
			}
			t = sig.Params()

		case opResults:
			sig, ok := t.(*types.Signature)
			if !ok {
				return nil, fmt.Errorf("cannot apply %q to %s (got %T, want signature)", code, t, t)
			}
			t = sig.Results()

		case opUnderlying:
			named, ok := t.(*types.Named)
			if !ok {
				return nil, fmt.Errorf("cannot apply %q to %s (got %T, want named)", code, t, t)
			}
			t = named.Underlying()

		case opRhs:
			if alias, ok := t.(*types.Alias); ok {
				t = alias.Rhs()
			} else if false {
				// Now that go1.24 is assured, we should be able to replace this with "if true {", but it causes objectpath tests to fail. TODO(adonovan).
				return nil, fmt.Errorf("cannot apply %q to %s (got %T, want alias)", code, t, t)
			}

		case opTypeParam:
			hasTypeParams, ok := t.(hasTypeParams) // Named, Signature
			if !ok {
				return nil, fmt.Errorf("cannot apply %q to %s (got %T, want named or signature)", code, t, t)
			}
			tparams := hasTypeParams.TypeParams()
			if n := tparams.Len(); index >= n {
				return nil, fmt.Errorf("type parameter index %d out of range [0-%d)", index, n)
			}
			t = tparams.At(index)

		case opRecvTypeParam:
			sig, ok := t.(*types.Signature) // Signature
			if !ok {
				return nil, fmt.Errorf("cannot apply %q to %s (got %T, want signature)", code, t, t)
			}
			rtparams := sig.RecvTypeParams()
			if n := rtparams.Len(); index >= n {
				return nil, fmt.Errorf("receiver type parameter index %d out of range [0-%d)", index, n)
			}
			t = rtparams.At(index)

		case opConstraint:
			tparam, ok := t.(*types.TypeParam)
			if !ok {
				return nil, fmt.Errorf("cannot apply %q to %s (got %T, want type parameter)", code, t, t)
			}
			t = tparam.Constraint()

		case opAt:
			tuple, ok := t.(*types.Tuple)
			if !ok {
				return nil, fmt.Errorf("cannot apply %q to %s (got %T, want tuple)", code, t, t)
			}
			if n := tuple.Len(); index >= n {
				return nil, fmt.Errorf("tuple index %d out of range [0-%d)", index, n)
			}
			obj = tuple.At(index)
			t = nil

		case opField:
			structType, ok := t.(*types.Struct)
			if !ok {
				return nil, fmt.Errorf("cannot apply %q to %s (got %T, want struct)", code, t, t)
			}
			if n := structType.NumFields(); index >= n {
				return nil, fmt.Errorf("field index %d out of range [0-%d)", index, n)
			}
			obj = structType.Field(index)
			t = nil

		case opMethod:
			switch t := t.(type) {
			case *types.Interface:
				if index >= t.NumMethods() {
					return nil, fmt.Errorf("method index %d out of range [0-%d)", index, t.NumMethods())
				}
				obj = t.Method(index) // Id-ordered

			case *types.Named:
				if index >= t.NumMethods() {
					return nil, fmt.Errorf("method index %d out of range [0-%d)", index, t.NumMethods())
				}
				obj = t.Method(index)

			default:
				return nil, fmt.Errorf("cannot apply %q to %s (got %T, want interface or named)", code, t, t)
			}
			t = nil

		case opObj:
			hasObj, ok := t.(hasObj)
			if !ok {
				return nil, fmt.Errorf("cannot apply %q to %s (got %T, want named or type param)", code, t, t)
			}
			obj = hasObj.Obj()
			t = nil

		default:
			return nil, fmt.Errorf("invalid path: unknown code %q", code)
		}
	}

	if obj == nil {
		panic(p) // path does not end in an object-valued operator
	}

	if obj.Pkg() != pkg {
		return nil, fmt.Errorf("path denotes %s, which belongs to a different package", obj)
	}

	return obj, nil // success
}
