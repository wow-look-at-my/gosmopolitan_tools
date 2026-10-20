// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package internal

import "golang.org/x/tools/go/analysis"

// This function is set by the checker package to provide backdoor access to the private Pass field of the *checker.Action type.
var ActionPass func(action any) *analysis.Pass
