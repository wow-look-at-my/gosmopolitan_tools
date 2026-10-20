// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

/*
Package inline defines an analyzer that inlines calls to functions and uses
of constants marked with a "//go:fix inline" directive. # Analyzer inline
inline: apply fixes based on 'go:fix inline' comment directives The inline
analyzer inlines functions, constants, and type aliases that are marked for
inlining. ## Functions Given a function that is marked for inlining, like
this: func Square(x int) int { return Pow(x,) } this analyzer will
recommend that calls to the function elsewhere, in the same or other
packages, should be inlined. Inlining can be used to move off of a deprecated
function: // Deprecated: prefer Pow(x,). F(nil) can have no effect on the
program, so this mechanism provides a low-risk way to update large numbers of
calls. We recommend, where possible, expressing the API in terms of the new
one to enable automatic migration.
*/
//go:fix inline
//go:fix inline
//go:fix inline
//go:fix inline
//go:fix inline
//go:fix inline
//go:fix inline
package inline
