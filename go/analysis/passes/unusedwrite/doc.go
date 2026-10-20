// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package unusedwrite checks for unused writes to the elements of a struct or array object.
//
// # Analyzer unusedwrite
//
// unusedwrite: checks for unused writes
//
// The analyzer reports instances of writes to struct fields and
// arrays that are never read. Suppose a struct object or an
// array is copied. Then its elements are copied implicitly by
// the compiler, and any element write to this copy does nothing
// with the original object. This happens specifically.
package unusedwrite
