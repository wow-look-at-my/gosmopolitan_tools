// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package typeparams

import (
	"go/types"
)

// Free is a memoization of the set of free type parameters within a type.
type Free struct {
	seen map[types.Type]bool
}

// Has reports whether the specified type has a free type parameter.
func (w *Free) Has(typ types.Type) (res bool) {
	// detect cycles
	if x, ok := w.seen[typ]; ok {
		return x
	}
	if w.seen == nil {
		w.seen = make(map[types.Type]bool)
	}
	w.seen[typ] = false
	defer func() {
		w.seen[typ] = res
	}()

	switch t := typ.(type) {
	case nil, *types.Basic: // TODO(gri) should nil be handled here?
		break

	case *types.Alias:
		if t.TypeParams().Len() > t.TypeArgs().Len() {
			return true // This is an uninstantiated Alias.
		}
		// The expansion of an alias can have free type parameters.
		return w.Has(types.Unalias(t))

	case *types.Array:
		return w.Has(t.Elem())

	case *types.Slice:
		return w.Has(t.Elem())

	case *types.Struct:
		for i, n := 0, t.NumFields(); i < n; i++ {
			if w.Has(t.Field(i).Type()) {
				return true
			}
		}

	case *types.Pointer:
		return w.Has(t.Elem())

	case *types.Tuple:
		n := t.Len()
		for i := range n {
			if w.Has(t.At(i).Type()) {
				return true
			}
		}

	case *types.Signature:
		// t.tparams may not be nil if we are looking at a signature of a generic function type.
		return w.Has(t.Params()) || w.Has(t.Results())

	case *types.Interface:
		for i, n := 0, t.NumMethods(); i < n; i++ {
			if w.Has(t.Method(i).Type()) {
				return true
			}
		}
		terms, err := InterfaceTermSet(t)
		if err != nil {
			return false // ill typed
		}
		for _, term := range terms {
			if w.Has(term.Type()) {
				return true
			}
		}

	case *types.Map:
		return w.Has(t.Key()) || w.Has(t.Elem())

	case *types.Chan:
		return w.Has(t.Elem())

	case *types.Named:
		args := t.TypeArgs()
		if params := t.TypeParams(); params.Len() > args.Len() {
			return true // this is an uninstantiated named type.
		}
		for i, n := 0, args.Len(); i < n; i++ {
			if w.Has(args.At(i)) {
				return true
			}
		}
		return w.Has(t.Underlying()) // recurse for types local to parameterized functions

	case *types.TypeParam:
		return true

	default:
		panic(t) // unreachable
	}

	return false
}
