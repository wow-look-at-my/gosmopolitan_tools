// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package printf defines an Analyzer that checks consistency of Printf format
// strings and arguments.
//
// # Analyzer printf
//
// printf: check consistency of Printf format strings and arguments
//
// The check applies to calls of the formatting functions such as [fmt.Printf]
// and [fmt.Sprintf]. As well as any detected wrappers of those functions such
// as [log.Printf]. It reports a variety of mistakes such as syntax errors in
// the format string and mismatches (of number and type) between the verbs and
// their arguments.
package printf
