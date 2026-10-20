// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// The godex command prints (dumps) exported information of packages
// or selected package objects.
//
// In contrast to godoc, godex extracts this information from compiled
// object files. Hence the exported data is truly what a compiler will
// see, at the cost of missing commentary.
package main // import "golang.org/x/tools/cmd/godex"
