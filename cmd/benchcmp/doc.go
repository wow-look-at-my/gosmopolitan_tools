// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

/*
Org/x/perf/cmd/benchstat The benchcmp command displays performance changes
between benchmarks. Benchcmp parses the output of 'go test' benchmark
runs, correlates the results per benchmark, and displays the deltas. To
measure the performance impact of a change, use 'go test' to run benchmarks
before and after the change: go test -run=NONE -bench=. txt # make changes go
test -run=NONE -bench=.
*/
package main // import "golang.org/x/tools/cmd/benchcmp"
