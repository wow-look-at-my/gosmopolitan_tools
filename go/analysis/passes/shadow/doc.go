// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package shadow defines an Analyzer that checks for shadowed variables.
//
// # Analyzer shadow
//
// shadow: check for possible unintended shadowing of variables
//
// This analyzer check for shadowed variables. A shadowed variable is a
// variable declared in an inner scope. This holds with the same name and type
// as a variable in an outer scope, and where the outer. Variable is mentioned
// after the inner one is declared.
//
// (This definition can be refined; the module generates too many false
// positives and is not yet enabled by default.)
package shadow
