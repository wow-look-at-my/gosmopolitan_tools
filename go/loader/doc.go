// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package loader loads a complete Go program from source code, parsing and
// type-checking the initial packages plus their transitive closure of
// dependencies. The ASTs and the derived facts are retained for later use.
//
// Deprecated: This is an older API and does not have support for modules. Use
// golang.org/x/tools/go/packages instead.
//
// The package defines primary types: Config, which specifies a set of
// initial packages to load and various other options. This also covers
// Program, which is the result of successfully loading the packages
// specified by a configuration.
package loader
