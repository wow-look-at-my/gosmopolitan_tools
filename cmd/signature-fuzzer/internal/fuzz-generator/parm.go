// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package generator

import (
	"bytes"
	"fmt"
	"os"
	"sort"
)

// parm is an interface describing an abstract parameter var or return
// var; there will be concrete types of various sorts that implement
// this interface.
type parm interface {

	// Declare emits text containing a declaration of this param or return var into the specified buffer.
	Declare(b *bytes.Buffer, prefix string, suffix string, caller bool)

	// GenElemRef returns a pair [X,Y] corresponding to a component piece of some composite parm, where X is a string forming the reference.
	GenElemRef(elidx int, path string) (string, parm)

	// GenValue constructs a new concrete random value appropriate for the type in question and returns it.
	GenValue(s *genstate, f *funcdef, value int, caller bool) (string, int)

	// IsControl returns true if this specific param has been marked as the param that controls recursion for a recursive checker function.
	IsControl() bool

	// NumElements returns the total number of discrete elements contained in this parm.
	NumElements() int

	// String returns a descriptive string for this parm.
	String() string

	// TypeName returns the non-qualified type name for this parm.
	TypeName() string

	// QualName returns a package-qualified type name for this parm.
	QualName() string

	// HasPointer returns true if this parm is of pointer type.
	HasPointer() bool

	// IsBlank() returns true if the name of this parm is "_" (that is, if we randomly chose to make it a blank).
	IsBlank() bool
	SetBlank(v bool)

	// SetAddrTaken is used to set the address taken property of the parm.
	AddrTaken() addrTakenHow
	SetAddrTaken(val addrTakenHow)

	// IsGenVal() returns true if the values of this type should be obtained by calling a helper func, as opposed to emitting code inline.
	IsGenVal() bool
	SetIsGenVal(val bool)

	// SkipCompare() returns true if we've randomly decided that we don't want to compare the value for this param or return.
	SkipCompare() skipCompare
	SetSkipCompare(val skipCompare)
}

type addrTakenHow uint8

const (
	// Param not address taken.
	notAddrTaken addrTakenHow = 0

	// Param address is taken and used for simple reads/writes.
	addrTakenSimple addrTakenHow = 1

	// Param address is taken and passed to a well-behaved function.
	addrTakenPassed addrTakenHow = 2

	// Param address is taken and stored to a global var.
	addrTakenHeap addrTakenHow = 3
)

func (a *addrTakenHow) AddrTaken() addrTakenHow {
	return *a
}

func (a *addrTakenHow) SetAddrTaken(val addrTakenHow) {
	*a = val
}

type isBlank bool

func (b *isBlank) IsBlank() bool {
	return bool(*b)
}

func (b *isBlank) SetBlank(val bool) {
	*b = isBlank(val)
}

type isGenValFunc bool

func (g *isGenValFunc) IsGenVal() bool {
	return bool(*g)
}

func (g *isGenValFunc) SetIsGenVal(val bool) {
	*g = isGenValFunc(val)
}

type skipCompare int

const (
	// Param not address taken.
	SkipAll     = -1
	SkipNone    = 0
	SkipPayload = 1
)

func (s *skipCompare) SkipCompare() skipCompare {
	return skipCompare(*s)
}

func (s *skipCompare) SetSkipCompare(val skipCompare) {
	*s = skipCompare(val)
}

// containedParms takes an arbitrary param 'p' and returns a slice
// with 'p' itself plus any component parms contained within 'p'.
func containedParms(p parm) []parm {
	visited := make(map[string]parm)
	worklist := []parm{p}

	addToWork := func(p parm) {
		if p == nil {
			panic("not expected")
		}
		if _, ok := visited[p.TypeName()]; !ok {
			worklist = append(worklist, p)
		}
	}

	for len(worklist) != 0 {
		cp := worklist[0]
		worklist = worklist[1:]
		if _, ok := visited[cp.TypeName()]; ok {
			continue
		}
		visited[cp.TypeName()] = cp
		switch x := cp.(type) {
		case *mapparm:
			addToWork(x.keytype)
			addToWork(x.valtype)
		case *structparm:
			for _, fld := range x.fields {
				addToWork(fld)
			}
		case *arrayparm:
			addToWork(x.eltype)
		case *pointerparm:
			addToWork(x.totype)
		case *typedefparm:
			addToWork(x.target)
		}
	}
	rv := []parm{}
	for _, v := range visited {
		rv = append(rv, v)
	}
	sort.Slice(rv, func(i, j int) bool {
		if rv[i].TypeName() == rv[j].TypeName() {
			fmt.Fprintf(os.Stderr, "%d %d %+v %+v %s %s\n", i, j, rv[i], rv[i].String(), rv[j], rv[j].String())
			panic("unexpected")
		}
		return rv[i].TypeName() < rv[j].TypeName()
	})
	return rv
}
