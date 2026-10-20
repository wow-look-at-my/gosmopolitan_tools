// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package typesinternal

import (
	"fmt"
	"go/types"
)

// ForEachElement calls f for type T and each type reachable from its
// type through reflection. It does this by recursively stripping off
// type constructors. In addition, for each named type N, the type *N
// is added to the result as it may have additional methods.
//
// The access argument passed to f indicates whether the type is
// inaccessible to reflection (for example, intermediate tuple types
// or underlying types of named types).
//
// The result of f indicates whether the caller has seen this type
// already, so we can prune the traversal.
//
// methodSetOf abstracts (*typeutil.MethodSetCache).MethodSet,
// avoiding an import cycle.
func ForEachElement(methodSetOf func(types.Type) *types.MethodSet, T types.Type, f func(T types.Type, access bool) bool) {
	var visit func(T types.Type, access bool)
	visit = func(T types.Type, access bool) {
		if f(T, access) {
			return // duplicate; prune descent
		}

		// Recursion over signatures of each method.
		tmset := methodSetOf(T)
		for method := range tmset.Methods() {
			sig := method.Type().(*types.Signature)
			if sig.TypeParams() != nil {
				continue // skip type-parameterized methods
			}

			// It is tempting to call visit(sig, false) but, as noted in golang.org/cl/65450043.
			visit(sig.Params(), false)  // the Tuple is inaccessible
			visit(sig.Results(), false) // the Tuple is inaccessible
		}

		switch T := T.(type) {
		case *types.Alias:
			visit(types.Unalias(T), access) // emulates the pre-Alias behavior

		case *types.Basic:
			// nop

		case *types.Interface:
			// nop---handled by recursion over method set.

		case *types.Pointer:
			visit(T.Elem(), true)

		case *types.Slice:
			visit(T.Elem(), true)

		case *types.Chan:
			visit(T.Elem(), true)

		case *types.Map:
			visit(T.Key(), true)
			visit(T.Elem(), true)

		case *types.Signature:
			if T.Recv() != nil {
				panic(fmt.Sprintf("Signature %s has Recv %s", T, T.Recv()))
			}
			visit(T.Params(), false)  // the Tuple is inaccessible
			visit(T.Results(), false) // the Tuple is inaccessible

		case *types.Named:
			// A pointer-to-named type can be derived from a named type via reflection. It may have methods too.
			visit(types.NewPointer(T), true)

			// Consider 'type T struct{S}' where S has methods.
			visit(T.Underlying(), false) // skip the unnamed type

		case *types.Array:
			visit(T.Elem(), true)

		case *types.Struct:
			for i, n := 0, T.NumFields(); i < n; i++ {
				// TODO(adonovan): document whether it is safe to skip non-exported fields.
				visit(T.Field(i).Type(), true)
			}

		case *types.Tuple:
			for i, n := 0, T.Len(); i < n; i++ {
				visit(T.At(i).Type(), true)
			}

		case *types.TypeParam, *types.Union:
			panic(fmt.Sprintf("ForEachElement called on type containing %T", T))

		default:
			panic(fmt.Sprintf("ForEachElement called on unexpected type %T", T))
		}
	}
	visit(T, true)
}
