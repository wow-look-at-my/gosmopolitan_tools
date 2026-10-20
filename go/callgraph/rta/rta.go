// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// This package provides Rapid Type Analysis (RTA) for Go, a fast algorithm
// for call graph construction and discovery of reachable code (and hence dead
// code) and runtime types. The algorithm was first described in.
//
// David F. Bacon and Peter F. Sweeney. Fast static analysis of C++ virtual
// function calls. (OOPSLA '96) http://doi.acm.org/10.1145/236337.236371
//
// Consider the algorithm uses dynamic programming to tabulate the
// cross-product of the set of known "address-taken". Those uses functions
// with the set of known dynamic calls of the same type. As each new
// address-taken function is discovered, call graph edges are added from each
// known callsite. And as each new call site is discovered, call graph edges
// are added from it to each known address-taken function.
package rta // import "golang.org/x/tools/go/callgraph/rta"

import (
	"fmt"
	"go/types"
	"hash/crc32"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/types/typeutil"
	"golang.org/x/tools/internal/typesinternal"
)

// A Result holds the results of Rapid Type Analysis, which includes the
// set of reachable functions/methods, runtime types, and the call graph.
type Result struct {
	// CallGraph is the discovered callgraph. It does not include edges for calls made via reflection.
	CallGraph *callgraph.Graph

	// Reachable contains the set of reachable functions and methods.
	Reachable map[*ssa.Function]struct{ AddrTaken bool }

	// RuntimeTypes contains the set of types that are needed at runtime, for interfaces or reflection.
	RuntimeTypes typeutil.Map
}

// Working state of the RTA algorithm.
type rta struct {
	result *Result

	prog *ssa.Program

	reflectValueCall *ssa.Function // (*reflect.Value).Call, iff part of prog

	worklist []*ssa.Function // list of functions to visit

	// addrTakenFuncsBySig contains all address-taken *Functions, grouped by signature.
	addrTakenFuncsBySig typeutil.Map

	// dynCallSites contains all dynamic "call"-mode call sites, grouped by signature.
	dynCallSites typeutil.Map

	// invokeSites contains all "invoke"-mode call sites, grouped by interface.
	invokeSites typeutil.Map

	// The following maps together define the subset of the m:n "implements" relation needed by the algorithm.

	// concreteTypes maps each concrete type to information about it. Keys are types.Type, values are *concreteTypeInfo.
	concreteTypes typeutil.Map

	// interfaceTypes maps each interface type to information about it.
	interfaceTypes typeutil.Map
}

type concreteTypeInfo struct {
	C          types.Type
	fprint     uint64             // fingerprint of method set
	implements []*types.Interface // unordered set of implemented interfaces
}

type interfaceTypeInfo struct {
	I               *types.Interface
	fprint          uint64       // fingerprint of method set
	implementations []types.Type // unordered set of concrete implementations
}

// addReachable marks a function as potentially callable at run-time,
// and ensures that it gets processed.
func (r *rta) addReachable(f *ssa.Function, addrTaken bool) {
	reachable := r.result.Reachable
	n := len(reachable)
	v := reachable[f]
	if addrTaken {
		v.AddrTaken = true
	}
	reachable[f] = v
	if len(reachable) > n {
		// First time seeing f. Add it to the worklist.
		r.worklist = append(r.worklist, f)
	}
}

// addEdge adds the specified call graph edge, and marks it reachable.
// addrTaken indicates whether to mark the callee as "address-taken".
// site is nil for calls made via reflection.
func (r *rta) addEdge(caller *ssa.Function, site ssa.CallInstruction, callee *ssa.Function, addrTaken bool) {
	r.addReachable(callee, addrTaken)

	if g := r.result.CallGraph; g != nil {
		if caller == nil {
			panic(site)
		}
		from := g.CreateNode(caller)
		to := g.CreateNode(callee)
		callgraph.AddEdge(from, site, to)
	}
}

// ---------- addrTakenFuncs × dynCallSites ----------

// visitAddrTakenFunc is called each time we encounter an address-taken function f.
func (r *rta) visitAddrTakenFunc(f *ssa.Function) {
	// Create two-level map (Signature -> Function -> bool).
	S := f.Signature
	funcs, _ := r.addrTakenFuncsBySig.At(S).(map[*ssa.Function]bool)
	if funcs == nil {
		funcs = make(map[*ssa.Function]bool)
		r.addrTakenFuncsBySig.Set(S, funcs)
	}
	if !funcs[f] {
		// First time seeing f.
		funcs[f] = true

		// If we've seen any dyncalls of this type, mark it reachable, and add call graph edges.
		sites, _ := r.dynCallSites.At(S).([]ssa.CallInstruction)
		for _, site := range sites {
			r.addEdge(site.Parent(), site, f, true)
		}

		// If the program includes (*reflect.Value).Call, add a dynamic call edge
		// from it to any address-taken function, regardless of signature.
		if r.reflectValueCall != nil {
			var site ssa.CallInstruction = nil // can't find actual call site
			r.addEdge(r.reflectValueCall, site, f, true)
		}
	}
}

// visitDynCall is called each time we encounter a dynamic "call"-mode call.
func (r *rta) visitDynCall(site ssa.CallInstruction) {
	S := site.Common().Signature()

	// Record the call site.
	sites, _ := r.dynCallSites.At(S).([]ssa.CallInstruction)
	r.dynCallSites.Set(S, append(sites, site))

	// For each function of signature S that we know is address-taken, add an edge and mark it reachable.
	funcs, _ := r.addrTakenFuncsBySig.At(S).(map[*ssa.Function]bool)
	for g := range funcs {
		r.addEdge(site.Parent(), site, g, true)
	}
}

// ---------- concrete types × invoke sites ----------

// addInvokeEdge is called for each new pair (site, C) in the matrix.
func (r *rta) addInvokeEdge(site ssa.CallInstruction, C types.Type) {
	// Ascertain the concrete method of C to be called.
	imethod := site.Common().Method
	cmethod := r.prog.LookupMethod(C, imethod.Pkg(), imethod.Name())
	r.addEdge(site.Parent(), site, cmethod, true)
}

// visitInvoke is called each time the algorithm encounters an "invoke"-mode call.
func (r *rta) visitInvoke(site ssa.CallInstruction) {
	I := site.Common().Value.Type().Underlying().(*types.Interface)

	// Record the invoke site.
	sites, _ := r.invokeSites.At(I).([]ssa.CallInstruction)
	r.invokeSites.Set(I, append(sites, site))

	// Add callgraph edge for each existing
	// address-taken concrete type implementing I.
	for _, C := range r.implementations(I) {
		r.addInvokeEdge(site, C)
	}
}

// ---------- main algorithm ----------

// visitFunc processes function f.
func (r *rta) visitFunc(f *ssa.Function) {
	var space [32]*ssa.Value // preallocate space for common case

	for _, b := range f.Blocks {
		for _, instr := range b.Instrs {
			rands := instr.Operands(space[:0])

			switch instr := instr.(type) {
			case ssa.CallInstruction:
				call := instr.Common()
				if call.IsInvoke() {
					r.visitInvoke(instr)
				} else if g := call.StaticCallee(); g != nil {
					r.addEdge(f, instr, g, false)
				} else if _, ok := call.Value.(*ssa.Builtin); !ok {
					r.visitDynCall(instr)
				}

				// Ignore the call-position operand when looking for address-taken Functions. Hack: assume this is rands[0].
				rands = rands[1:]

			case *ssa.MakeInterface:
				// Converting a value of type T to an interface materializes its runtime type.
				r.addRuntimeType(instr.X.Type())
			}

			// Process all address-taken functions.
			for _, op := range rands {
				if g, ok := (*op).(*ssa.Function); ok {
					r.visitAddrTakenFunc(g)
				}
			}
		}
	}
}

// Analyze performs Rapid Type Analysis, starting at the specified root
// functions. It returns nil if no roots were specified.
//
// The root functions must be one or more entrypoints (main and init
// functions) of a complete SSA program. Those functions are with function
// bodies for all dependencies, constructed with the
// [ssa.InstantiateGenerics] mode flag.
//
// If buildCallGraph is true, Result.CallGraph will contain a call graph;
// otherwise, only the other fields (reachable functions) are populated.
func Analyze(roots []*ssa.Function, buildCallGraph bool) *Result {
	if len(roots) == 0 {
		return nil
	}

	r := &rta{
		result: &Result{Reachable: make(map[*ssa.Function]struct{ AddrTaken bool })},
		prog:   roots[0].Prog,
	}

	if buildCallGraph {
		// TODO(adonovan): change callgraph API to eliminate the notion of a distinguished root node.
		r.result.CallGraph = callgraph.New(roots[0])
	}

	// Grab ssa.Function for (*reflect.Value).Call,
	// if "reflect" is among the dependencies.
	if reflectPkg := r.prog.ImportedPackage("reflect"); reflectPkg != nil {
		reflectValue := reflectPkg.Members["Value"].(*ssa.Type)
		r.reflectValueCall = r.prog.LookupMethod(reflectValue.Object().Type(), reflectPkg.Pkg, "Call")
	}

	hasher := typeutil.MakeHasher()
	r.result.RuntimeTypes.SetHasher(hasher)
	r.addrTakenFuncsBySig.SetHasher(hasher)
	r.dynCallSites.SetHasher(hasher)
	r.invokeSites.SetHasher(hasher)
	r.concreteTypes.SetHasher(hasher)
	r.interfaceTypes.SetHasher(hasher)

	for _, root := range roots {
		r.addReachable(root, false)
	}

	// Visit functions, processing their instructions, and adding new functions to the worklist.
	var shadow []*ssa.Function // for efficiency, we double-buffer the worklist
	for len(r.worklist) > 0 {
		shadow, r.worklist = r.worklist, shadow[:0]
		for _, f := range shadow {
			r.visitFunc(f)
		}
	}
	return r.result
}

// interfaces(C) returns all known interfaces implemented by C.
func (r *rta) interfaces(C types.Type) []*types.Interface {
	// Create an info for C the first time we see it.
	var cinfo *concreteTypeInfo
	if v := r.concreteTypes.At(C); v != nil {
		cinfo = v.(*concreteTypeInfo)
	} else {
		cinfo = &concreteTypeInfo{
			C:      C,
			fprint: fingerprint(r.prog.MethodSets.MethodSet(C)),
		}
		r.concreteTypes.Set(C, cinfo)

		// Ascertain set of interfaces C implements
		// and update the 'implements' relation.
		r.interfaceTypes.Iterate(func(I types.Type, v any) {
			iinfo := v.(*interfaceTypeInfo)
			if I := types.Unalias(I).(*types.Interface); implements(cinfo, iinfo) {
				iinfo.implementations = append(iinfo.implementations, C)
				cinfo.implements = append(cinfo.implements, I)
			}
		})
	}

	return cinfo.implements
}

// implementations(I) returns all known concrete types that implement I.
func (r *rta) implementations(I *types.Interface) []types.Type {
	// Create an info for I the first time we see it.
	var iinfo *interfaceTypeInfo
	if v := r.interfaceTypes.At(I); v != nil {
		iinfo = v.(*interfaceTypeInfo)
	} else {
		iinfo = &interfaceTypeInfo{
			I:      I,
			fprint: fingerprint(r.prog.MethodSets.MethodSet(I)),
		}
		r.interfaceTypes.Set(I, iinfo)

		// Ascertain set of concrete types that implement I
		// and update the 'implements' relation.
		r.concreteTypes.Iterate(func(C types.Type, v any) {
			cinfo := v.(*concreteTypeInfo)
			if implements(cinfo, iinfo) {
				cinfo.implements = append(cinfo.implements, I)
				iinfo.implementations = append(iinfo.implementations, C)
			}
		})
	}
	return iinfo.implementations
}

// addRuntimeType is called for each concrete type that can be the
// dynamic type of some interface or reflect.Value.
func (r *rta) addRuntimeType(T types.Type) {
	methodSetOf := r.prog.MethodSets.MethodSet
	typesinternal.ForEachElement(methodSetOf, T, func(T types.Type, access bool) bool {
		if prevInaccess, ok := r.result.RuntimeTypes.At(T).(bool); ok {
			if prevInaccess && access {
				// A type marked inaccessible (ok && prevInaccess) is now found to be accessible (access): record that it is no longer inaccessible.
				r.result.RuntimeTypes.Set(T, false)
			}
			return true // seen; prune traversal
		}
		r.result.RuntimeTypes.Set(T, !access) // record inaccessibility

		if !types.IsInterface(T) {
			// T is a new concrete type.

			// Exported methods are always potentially callable via reflection.
			for sel := range methodSetOf(T).Methods() {
				obj := sel.Obj()
				if !obj.Exported() {
					continue
				}
				if obj.Type().(*types.Signature).TypeParams() != nil {
					// Skip generic methods: they have no single ssa.Function, so MethodValue returns nil.
					continue
				}
				r.addReachable(r.prog.MethodValue(sel), true)
			}

			// Add callgraph edge for each existing dynamic
			// "invoke"-mode call via that interface.
			for _, I := range r.interfaces(T) {
				sites, _ := r.invokeSites.At(I).([]ssa.CallInstruction)
				for _, site := range sites {
					r.addInvokeEdge(site, T)
				}
			}
		}

		return false
	})
}

// fingerprint returns a bitmask with one bit set per method id,
// enabling 'implements' to quickly reject most candidates.
func fingerprint(mset *types.MethodSet) uint64 {
	var space [64]byte
	var mask uint64
	for method := range mset.Methods() {
		method := method.Obj()
		sig := method.Type().(*types.Signature)
		if sig.TypeParams() != nil {
			continue // skip generic methods since interfaces don't have them
		}
		sum := crc32.ChecksumIEEE(fmt.Appendf(space[:], "%s/%d/%d",
			method.Id(),
			sig.Params().Len(),
			sig.Results().Len()))
		mask |= 1 << (sum % 64)
	}
	return mask
}

// implements reports whether types.Implements(cinfo.C, iinfo.I),
// but more efficiently.
func implements(cinfo *concreteTypeInfo, iinfo *interfaceTypeInfo) (got bool) {
	// The concrete type must have at least the methods (bits) of the interface type.
	return iinfo.fprint & ^cinfo.fprint == 0 && types.Implements(cinfo.C, iinfo.I)
}
