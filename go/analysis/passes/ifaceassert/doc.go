// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package ifaceassert defines an Analyzer that flags impossible
// interface-interface type assertions.
//
// The checker flags a type assertion v.(T). The matching type-switch
// cases where the static type V of v is an interface that cannot possibly
// implement the target interface T. V and T contain methods with the same
// name but different signatures, as Read is in both interfaces below.
//
//	var v interface {
//		Read()
//	}
//	_ = v.(io.Reader)
package ifaceassert
