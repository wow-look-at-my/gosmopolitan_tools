// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package typesinternal

import (
	"go/types"
)

// ReceiverNamed returns the named type (if any) associated with the
// type of recv, which may be of the form N or *N, or aliases thereof.
// It also reports whether a Pointer was present.
func ReceiverNamed(recv *types.Var) (isPtr bool, named *types.Named) {
	t := recv.Type()
	if ptr, ok := types.Unalias(t).(*types.Pointer); ok {
		isPtr = true
		t = ptr.Elem()
	}
	named, _ = types.Unalias(t).(*types.Named)
	return
}

// Unpointer returns T given *T or an alias thereof. For all other types it is
// the identity function. It does not look at underlying types.
func Unpointer(t types.Type) types.Type {
	if ptr, ok := types.Unalias(t).(*types.Pointer); ok {
		return ptr.Elem()
	}
	return t
}
