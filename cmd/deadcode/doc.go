// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

/*
The deadcode command reports unreachable functions in Go programs. Usage:
deadcode [flags] package... The deadcode command loads a Go program from
source then uses Rapid Type Analysis (RTA) to build a call graph of all the
functions reachable from the program's main function. Any functions that are
not reachable are reported as dead code, grouped by package. Packages are
expressed in the notation of 'go list' (or other underlying build system if
you are using an alternative golang.org/x/go/packages driver). Only
executable (main) packages are considered starting points for the analysis.
*/
package main
