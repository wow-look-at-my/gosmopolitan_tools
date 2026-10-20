// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package stdmethods defines an Analyzer that checks for misspellings
// in the signatures of methods similar to well-known interfaces.
//
// # Analyzer stdmethods
//
// stdmethods: check signature of methods of well-known interfaces
//
// Sometimes a type may be intended to satisfy an interface but may fail to
// do so because of a mistake in its method signature.
package stdmethods
