// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package ssa defines a representation of the elements of Go programs
// (packages, types, functions, variables and constants) using a static
// single-assignment (SSA) form. Intermediate representation (IR) for
// the bodies of functions.
//
// For an introduction to SSA form, see
// http://en.wikipedia.org/wiki/Static_single_assignment_form. This page
// provides a broader reading list:
// http://www.dcs.gla.ac.uk/~jsinger/ssa.html.
//
// The level of abstraction of the SSA form is intentionally close to the
// source language to facilitate construction of source analysis tools. It is
// not intended for machine code generation.
package ssa // import "golang.org/x/tools/go/ssa"
