// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package chautil provides helper functions related to
// class hierarchy analysis (CHA) for use in x/tools.
package chautil

import (
	"go/types"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/types/typeutil"
)

// LazyCallees returns a function that maps a call site (in a function in fns)
// to its callees within fns. The set of callees is computed using the CHA algorithm,
// i.e., on the entire implements relation between interfaces and concrete types
// in fns. Please see golang.org/x/tools/go/callgraph/cha for more information.
//
// The resulting function is not concurrency safe.
func LazyCallees(fns map[*ssa.Function]bool) func(site ssa.CallInstruction) []*ssa.Function {
	// funcsBySig contains all functions, keyed by signature.
	var funcsBySig typeutil.Map // value is []*ssa.Function

	// methodsByID contains all methods, grouped by ID for efficient lookup.
	methodsByID := make(map[string][]*ssa.Function)

	// An imethod represents an interface method I.m.
	type imethod struct {
		I  *types.Interface
		id string
	}
	// methodsMemo records, for every abstract method call I.m on interface type.
	methodsMemo := make(map[imethod][]*ssa.Function)
	lookupMethods := func(I *types.Interface, m *types.Func) []*ssa.Function {
		id := m.Id()
		methods, ok := methodsMemo[imethod{I, id}]
		if !ok {
			for _, f := range methodsByID[id] {
				C := f.Signature.Recv().Type() // named or *named
				if types.Implements(C, I) {
					methods = append(methods, f)
				}
			}
			methodsMemo[imethod{I, id}] = methods
		}
		return methods
	}

	for f := range fns {
		if f.Signature.Recv() == nil {
			// Package initializers can never be address-taken.
			if f.Name() == "init" && f.Synthetic == "package initializer" {
				continue
			}
			funcs, _ := funcsBySig.At(f.Signature).([]*ssa.Function)
			funcs = append(funcs, f)
			funcsBySig.Set(f.Signature, funcs)
		} else if obj := f.Object(); obj != nil {
			id := obj.(*types.Func).Id()
			methodsByID[id] = append(methodsByID[id], f)
		}
	}

	return func(site ssa.CallInstruction) []*ssa.Function {
		call := site.Common()
		if call.IsInvoke() {
			tiface := call.Value.Type().Underlying().(*types.Interface)
			return lookupMethods(tiface, call.Method)
		} else if g := call.StaticCallee(); g != nil {
			return []*ssa.Function{g}
		} else if _, ok := call.Value.(*ssa.Builtin); !ok {
			fns, _ := funcsBySig.At(call.Signature()).([]*ssa.Function)
			return fns
		}
		return nil
	}
}
