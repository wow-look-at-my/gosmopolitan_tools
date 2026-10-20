// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package nilness inspects the control-flow graph of an SSA function and
// reports errors such as nil pointer dereferences and degenerate nil pointer
// comparisons.
package nilness
