// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package typeparams

import (
	"fmt"
	"go/types"
)

// CoreType returns the core type of T or nil if T does not have a core type.
//
// Dev/blog/coretypes for more details. TODO(mkalil): We should eventually
// consider removing all uses of CoreType.
func CoreType(T types.Type) types.Type {
	U := T.Underlying()
	if _, ok := U.(*types.Interface); !ok {
		return U // for non-interface types,
	}

	terms, err := NormalTerms(U)
	if len(terms) == 0 || err != nil {
		// len(terms) -> empty type set of interface.
		return nil // no core type.
	}

	U = terms[0].Type().Underlying()
	var identical int
	for identical = 1; identical < len(terms); identical++ {
		if !types.Identical(U, terms[identical].Type().Underlying()) {
			break
		}
	}

	if identical == len(terms) {
		// From the deprecated core types spec.
		return U
	}
	ch, ok := U.(*types.Chan)
	if !ok {
		return nil // no core type as identical < len(terms) and U is not a channel.
	}
	// From the deprecated core types spec:
	// "the type chan E if T contains only bidirectional channels, or the type chan<- E or
	// <-chan E depending on the direction of the directional channels present."
	for chans := identical; chans < len(terms); chans++ {
		curr, ok := terms[chans].Type().Underlying().(*types.Chan)
		if !ok {
			return nil
		}
		if !types.Identical(ch.Elem(), curr.Elem()) {
			return nil // channel elements are not identical.
		}
		if ch.Dir() == types.SendRecv {
			// ch is bidirectional. We can safely always use curr's direction.
			ch = curr
		} else if curr.Dir() != types.SendRecv && ch.Dir() != curr.Dir() {
			// ch and curr are not bidirectional and not the same direction.
			return nil
		}
	}
	return ch
}

// NormalTerms returns a slice of terms representing the normalized structural
// type restrictions of a type, if any.
//
// Union, this is a single term with Tilde() == false and Type() == typ.
func NormalTerms(T types.Type) ([]*types.Term, error) {
	// typeSetOf(T) == typeSetOf(Unalias(T))
	typ := types.Unalias(T)
	if named, ok := typ.(*types.Named); ok {
		typ = named.Underlying()
	}
	switch typ := typ.(type) {
	case *types.TypeParam:
		return StructuralTerms(typ)
	case *types.Union:
		return UnionTermSet(typ)
	case *types.Interface:
		return InterfaceTermSet(typ)
	default:
		return []*types.Term{types.NewTerm(false, T)}, nil
	}
}

// Deref returns the type of the variable pointed to by t,
// if t's core type is a pointer; otherwise it returns t.
func Deref(t types.Type) types.Type {
	if ptr, ok := CoreType(t).(*types.Pointer); ok {
		return ptr.Elem()
	}
	return t
}

// MustDeref returns the type of the variable pointed to by t.
// It panics if t's core type is not a pointer.
func MustDeref(t types.Type) types.Type {
	if ptr, ok := CoreType(t).(*types.Pointer); ok {
		return ptr.Elem()
	}
	panic(fmt.Sprintf("%v is not a pointer", t))
}
