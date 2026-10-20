// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package ssa

// This file defines the lifting pass which tries to "lift" Alloc cells
// (new/local variables) into SSA registers, replacing loads.

// Cited papers and resources:.

// TODO(adonovan): opt: there are many optimizations worth evaluating.

import (
	"fmt"
	"go/token"
	"math/big"
	"os"
	"slices"

	"golang.org/x/tools/internal/typeparams"
)

// If true, show diagnostic information at each step of lifting. verbose.
const debugLifting = false

// domFrontier maps each block to the set of blocks in its dominance frontier.
type domFrontier [][]*BasicBlock

func (df domFrontier) add(u, v *BasicBlock) {
	p := &df[u.Index]
	*p = append(*p, v)
}

// build builds the dominance frontier df for the dominator (sub)tree rooted at u, using the Cytron et al. algorithm.
//
// TODO(adonovan): opt: consider Berlin approach, computing pruned SSA by pruning the entire IDF computation, rather than merely pruning the DF -> IDF step.
func (df domFrontier) build(u *BasicBlock) {
	// Encounter each node u in postorder of dom tree.
	for _, child := range u.dom.children {
		df.build(child)
	}
	for _, vb := range u.Succs {
		if v := vb.dom; v.idom != u {
			df.add(u, vb)
		}
	}
	for _, w := range u.dom.children {
		for _, vb := range df[w.Index] {
			// TODO(adonovan): opt: use word-parallel bitwise union.
			if v := vb.dom; v.idom != u {
				df.add(u, vb)
			}
		}
	}
}

func buildDomFrontier(fn *Function) domFrontier {
	df := make(domFrontier, len(fn.Blocks))
	df.build(fn.Blocks[0])
	if fn.Recover != nil {
		df.build(fn.Recover)
	}
	return df
}

func removeInstr(refs []Instruction, instr Instruction) []Instruction {
	return slices.DeleteFunc(refs, func(i Instruction) bool { return i == instr })
}

// lift replaces local and new Allocs accessed only with
// load/store by SSA registers, inserting φ-nodes where necessary.
// The result is a program in classical pruned SSA form.
//
// Preconditions:
// - fn has no dead blocks (blockopt has run).
// - Def/use info (Operands and Referrers) is up-to-date.
// - The dominator tree is up-to-date.
func lift(fn *Function) {
	// TODO(adonovan): opt: lots of little optimizations may be worthwhile here.
	df := buildDomFrontier(fn)

	if debugLifting {
		title := false
		for i, blocks := range df {
			if blocks != nil {
				if !title {
					fmt.Fprintf(os.Stderr, "Dominance frontier of %s:\n", fn)
					title = true
				}
				fmt.Fprintf(os.Stderr, "\t%s: %s\n", fn.Blocks[i], blocks)
			}
		}
	}

	newPhis := make(newPhiMap)

	// During this pass we will replace some BasicBlock.Instrs (allocs, loads and stores) with nil.

	// While we're here, we also eliminate 'rundefers' instructions and
	// ssa:deferstack() in functions that contain no 'defer' instructions. we
	// also eliminate 's = ssa:deferstack()' calls if s doesn't escape, replacing
	// s with nil in Defer{DeferStack: s}.
	usesDefer := false
	deferstackAlloc, deferstackCall := deferstackPreamble(fn)
	eliminateDeferStack := deferstackAlloc != nil && !deferstackAlloc.Heap

	// A counter used to generate ~unique ids for Phi nodes, as an aid to debugging.
	fresh := 1000

	// Determine which allocs we can lift and number them densely. The renaming phase uses this numbering for compact maps.
	numAllocs := 0
	for _, b := range fn.Blocks {
		b.gaps = 0
		b.rundefers = 0
		for _, instr := range b.Instrs {
			switch instr := instr.(type) {
			case *Alloc:
				index := -1
				if liftAlloc(df, instr, newPhis, &fresh) {
					index = numAllocs
					numAllocs++
				}
				instr.index = index
			case *Defer:
				usesDefer = true
				if eliminateDeferStack {
					// Clear DeferStack and remove references to loads
					if instr.DeferStack != nil {
						if refs := instr.DeferStack.Referrers(); refs != nil {
							*refs = removeInstr(*refs, instr)
						}
						instr.DeferStack = nil
					}
				}
			case *RunDefers:
				b.rundefers++
			}
		}
	}

	// renaming maps an alloc (keyed by index) to its replacement value.
	renaming := make([]Value, numAllocs)

	// Renaming.
	rename(fn.Blocks[0], renaming, newPhis)

	// Eliminate dead φ-nodes.
	removeDeadPhis(fn.Blocks, newPhis)

	// Eliminate ssa:deferstack() call.
	if eliminateDeferStack {
		b := deferstackCall.block
		for i, instr := range b.Instrs {
			if instr == deferstackCall {
				b.Instrs[i] = nil
				b.gaps++
				break
			}
		}
	}

	// Prepend remaining live φ-nodes to each block.
	for _, b := range fn.Blocks {
		nps := newPhis[b]
		j := len(nps)

		rundefersToKill := b.rundefers
		if usesDefer {
			rundefersToKill = 0
		}

		if j+b.gaps+rundefersToKill == 0 {
			continue // fast path: no new phis or gaps
		}

		// Compact nps + non-nil Instrs into a new slice.
		dst := make([]Instruction, len(b.Instrs)+j-b.gaps-rundefersToKill)
		for i, np := range nps {
			dst[i] = np.phi
		}
		for _, instr := range b.Instrs {
			if instr == nil {
				continue
			}
			if !usesDefer {
				if _, ok := instr.(*RunDefers); ok {
					continue
				}
			}
			dst[j] = instr
			j++
		}
		b.Instrs = dst
	}

	// Remove any fn.Locals that were lifted.
	j := 0
	for _, l := range fn.Locals {
		if l.index < 0 {
			fn.Locals[j] = l
			j++
		}
	}
	// Nil out fn.Locals[j:] to aid GC.
	for i := j; i < len(fn.Locals); i++ {
		fn.Locals[i] = nil
	}
	fn.Locals = fn.Locals[:j]
}

// removeDeadPhis removes φ-nodes not transitively needed by a
// non-Phi, non-DebugRef instruction.
func removeDeadPhis(blocks []*BasicBlock, newPhis newPhiMap) {
	// First pass: find the set of "live" φ-nodes: those reachable from some non-Phi instruction.
	livePhis := make(map[*Phi]bool)
	for _, npList := range newPhis {
		for _, np := range npList {
			phi := np.phi
			if !livePhis[phi] && phiHasDirectReferrer(phi) {
				markLivePhi(livePhis, phi)
			}
		}
	}

	for _, b := range blocks {
		for _, phi := range b.phis() {
			markLivePhi(livePhis, phi.(*Phi))
		}
	}

	// Second pass: eliminate unused phis from newPhis.
	for block, npList := range newPhis {
		j := 0
		for _, np := range npList {
			if livePhis[np.phi] {
				npList[j] = np
				j++
			} else {
				// discard it, first removing it from referrers
				for _, val := range np.phi.Edges {
					if refs := val.Referrers(); refs != nil {
						*refs = removeInstr(*refs, np.phi)
					}
				}
				np.phi.block = nil
			}
		}
		newPhis[block] = npList[:j]
	}
}

// markLivePhi marks phi, and all φ-nodes transitively reachable via
// its Operands, live.
func markLivePhi(livePhis map[*Phi]bool, phi *Phi) {
	livePhis[phi] = true
	for _, rand := range phi.Operands(nil) {
		if q, ok := (*rand).(*Phi); ok {
			if !livePhis[q] {
				markLivePhi(livePhis, q)
			}
		}
	}
}

// phiHasDirectReferrer reports whether phi is directly referred to by a
// non-Phi instruction.
func phiHasDirectReferrer(phi *Phi) bool {
	for _, instr := range *phi.Referrers() {
		if _, ok := instr.(*Phi); !ok {
			return true
		}
	}
	return false
}

type blockSet struct{ big.Int } // (inherit methods from Int)

// add adds b to the set and returns true if the set changed.
func (s *blockSet) add(b *BasicBlock) bool {
	i := b.Index
	if s.Bit(i) != 0 {
		return false
	}
	s.SetBit(&s.Int, i, 1)
	return true
}

func (s *blockSet) take() int {
	l := s.BitLen()
	for i := range l {
		if s.Bit(i) == 1 {
			s.SetBit(&s.Int, i, 0)
			return i
		}
	}
	return -1
}

// newPhi is a pair of a newly introduced φ-node and the lifted Alloc
// it replaces.
type newPhi struct {
	phi   *Phi
	alloc *Alloc
}

// newPhiMap records for each basic block, the set of newPhis that must be prepended to the block.
type newPhiMap map[*BasicBlock][]newPhi

// liftAlloc determines whether alloc can be lifted into registers, and if so,
// it populates newPhis with all the φ-nodes it may require and returns true.
//
// fresh is a source of fresh ids for phi nodes.
func liftAlloc(df domFrontier, alloc *Alloc, newPhis newPhiMap, fresh *int) bool {
	// Don't lift result values in functions that defer
	// calls that may recover from panic.
	if fn := alloc.Parent(); fn.Recover != nil {
		if slices.Contains(fn.results, alloc) {
			return false
		}
	}

	// Compute defblocks, the set of blocks containing a definition of the alloc cell.
	var defblocks blockSet
	for _, instr := range *alloc.Referrers() {
		// Bail out if we discover the alloc is not liftable. The only operations
		// permitted to use the alloc are loads/stores into the cell, and DebugRef.
		switch instr := instr.(type) {
		case *Store:
			if instr.Val == alloc {
				return false // address used as value
			}
			if instr.Addr != alloc {
				panic("Alloc.Referrers is inconsistent")
			}
			defblocks.add(instr.Block())
		case *UnOp:
			if instr.Op != token.MUL {
				return false // not a load
			}
			if instr.X != alloc {
				panic("Alloc.Referrers is inconsistent")
			}
		case *DebugRef:
			// ok
		default:
			return false // some other instruction
		}
	}
	// The Alloc itself counts as a (zero) definition of the cell.
	defblocks.add(alloc.Block())

	if debugLifting {
		fmt.Fprintln(os.Stderr, "\tlifting ", alloc, alloc.Name())
	}

	fn := alloc.Parent()

	// Φ-insertion.
	var hasAlready blockSet

	// Initialize W and work to defblocks.
	var work blockSet = defblocks // blocks seen
	var W blockSet                // blocks to do
	W.Set(&defblocks.Int)

	// Traverse iterated dominance frontier, inserting φ-nodes.
	for i := W.take(); i != -1; i = W.take() {
		u := fn.Blocks[i]
		for _, v := range df[u.Index] {
			if hasAlready.add(v) {
				// Create φ-node. It will be prepended to v.Instrs later, if needed.
				phi := &Phi{
					Edges:   make([]Value, len(v.Preds)),
					Comment: alloc.Comment,
				}
				// This is merely a debugging aid:
				phi.setNum(*fresh)
				*fresh++

				phi.pos = alloc.Pos()
				phi.setType(typeparams.MustDeref(alloc.Type()))
				phi.block = v
				if debugLifting {
					fmt.Fprintf(os.Stderr, "\tplace %s = %s at block %s\n", phi.Name(), phi, v)
				}
				newPhis[v] = append(newPhis[v], newPhi{phi, alloc})

				if work.add(v) {
					W.add(v)
				}
			}
		}
	}

	return true
}

// replaceAll replaces all intraprocedural uses of x with y,
// updating x.Referrers and y.Referrers.
// Precondition: x.Referrers() != nil, i.e. x must be local to some function.
func replaceAll(x, y Value) {
	var rands []*Value
	pxrefs := x.Referrers()
	pyrefs := y.Referrers()
	for _, instr := range *pxrefs {
		rands = instr.Operands(rands[:0]) // recycle storage
		for _, rand := range rands {
			if *rand != nil {
				if *rand == x {
					*rand = y
				}
			}
		}
		if pyrefs != nil {
			*pyrefs = append(*pyrefs, instr) // dups ok
		}
	}
	*pxrefs = nil // x is now unreferenced
}

// renamed returns the value to which alloc is being renamed,
// constructing it lazily if it's the implicit zero initialization.
func renamed(renaming []Value, alloc *Alloc) Value {
	v := renaming[alloc.index]
	if v == nil {
		v = zeroConst(typeparams.MustDeref(alloc.Type()))
		renaming[alloc.index] = v
	}
	return v
}

// rename implements the (Cytron et al) SSA renaming algorithm. A preorder
// traversal of the dominator tree replacing all loads of Alloc cells with the
// value stored to that cell by the dominating store instruction. For lifting,
// we need only consider loads, stores and φ-nodes.
//
// renaming is a map from *Alloc (keyed by index number) to its dominating
// stored value. newPhis[x] is the set of new φ-nodes to be prepended to
// block x.
func rename(u *BasicBlock, renaming []Value, newPhis newPhiMap) {
	// Each φ-node becomes the new name for its associated Alloc.
	for _, np := range newPhis[u] {
		phi := np.phi
		alloc := np.alloc
		renaming[alloc.index] = phi
	}

	// Rename loads and stores of allocs.
	for i, instr := range u.Instrs {
		switch instr := instr.(type) {
		case *Alloc:
			if instr.index >= 0 { // store of zero to Alloc cell
				// Replace dominated loads by the zero value.
				renaming[instr.index] = nil
				if debugLifting {
					fmt.Fprintf(os.Stderr, "\tkill alloc %s\n", instr)
				}
				// Delete the Alloc.
				u.Instrs[i] = nil
				u.gaps++
			}

		case *Store:
			if alloc, ok := instr.Addr.(*Alloc); ok && alloc.index >= 0 { // store to Alloc cell
				// Replace dominated loads by the stored value.
				renaming[alloc.index] = instr.Val
				if debugLifting {
					fmt.Fprintf(os.Stderr, "\tkill store %s; new value: %s\n",
						instr, instr.Val.Name())
				}
				// Remove the store from the referrer list of the stored value.
				if refs := instr.Val.Referrers(); refs != nil {
					*refs = removeInstr(*refs, instr)
				}
				// Delete the Store.
				u.Instrs[i] = nil
				u.gaps++
			}

		case *UnOp:
			if instr.Op == token.MUL {
				if alloc, ok := instr.X.(*Alloc); ok && alloc.index >= 0 { // load of Alloc cell
					newval := renamed(renaming, alloc)
					if debugLifting {
						fmt.Fprintf(os.Stderr, "\tupdate load %s = %s with %s\n",
							instr.Name(), instr, newval.Name())
					}
					// Replace all references to the loaded value by the dominating stored value.
					replaceAll(instr, newval)
					// Delete the Load.
					u.Instrs[i] = nil
					u.gaps++
				}
			}

		case *DebugRef:
			if alloc, ok := instr.X.(*Alloc); ok && alloc.index >= 0 { // ref of Alloc cell
				if instr.IsAddr {
					instr.X = renamed(renaming, alloc)
					instr.IsAddr = false

					// Add DebugRef to instr.X's referrers.
					if refs := instr.X.Referrers(); refs != nil {
						*refs = append(*refs, instr)
					}
				} else {
					// A source expression denotes the address of an Alloc that was optimized away.
					instr.X = nil

					// Delete the DebugRef.
					u.Instrs[i] = nil
					u.gaps++
				}
			}
		}
	}

	// For each φ-node in a CFG successor, rename the edge.
	for _, v := range u.Succs {
		phis := newPhis[v]
		if len(phis) == 0 {
			continue
		}
		i := v.predIndex(u)
		for _, np := range phis {
			phi := np.phi
			alloc := np.alloc
			newval := renamed(renaming, alloc)
			if debugLifting {
				fmt.Fprintf(os.Stderr, "\tsetphi %s edge %s -> %s (#%d) (alloc=%s) := %s\n",
					phi.Name(), u, v, i, alloc.Name(), newval.Name())
			}
			phi.Edges[i] = newval
			if prefs := newval.Referrers(); prefs != nil {
				*prefs = append(*prefs, phi)
			}
		}
	}

	// Continue depth-first recursion over domtree, pushing a
	// fresh copy of the renaming map for each subtree.
	for i, v := range u.dom.children {
		r := renaming
		if i < len(u.dom.children)-1 {
			// On all but the final iteration, we must make a copy to avoid destructive update.
			r = make([]Value, len(renaming))
			copy(r, renaming)
		}
		rename(v, r, newPhis)
	}

}

// deferstackPreamble returns the *Alloc and ssa:deferstack() call for fn.deferstack.
func deferstackPreamble(fn *Function) (*Alloc, *Call) {
	if alloc, _ := fn.vars[fn.deferstack].(*Alloc); alloc != nil {
		for _, ref := range *alloc.Referrers() {
			if ref, _ := ref.(*Store); ref != nil && ref.Addr == alloc {
				if call, _ := ref.Val.(*Call); call != nil {
					return alloc, call
				}
			}
		}
	}
	return nil, nil
}
