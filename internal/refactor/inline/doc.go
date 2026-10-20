// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

/*
Package inline implements inlining of Go function calls. The client provides
information about the caller and callee, including the source text, syntax
tree, and type information, and the inliner returns the modified source file
for the caller, or an error if the inlining operation is invalid (for example
because the function body refers to names that are inaccessible to the
caller). Although this interface demands more information from the client
than might seem necessary, it enables smoother integration with existing
batch and interactive tools that have their own ways of managing the
processes of reading, parsing, and type-checking packages. In particular,
this package does not assume that the caller and callee belong to the same
token.FileSet or types.Importer realms. There are many aspects to a function
call.
*/
package inline
