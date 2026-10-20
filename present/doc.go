// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

/*
Package present implements parsing and rendering of present files: slide
presentations as in golang.org/x/tools/cmd/present, and articles as in
golang.org/x/blog. Present authors are trusted, so do not use it with
untrusted input. A present file begins with a header giving the title and
other metadata: # Title of document Subtitle of document: Jan Tags: foo, bar,
baz Summary: This is a great document you want to read. OldURL:
former-path-for-this-doc The "# " prefix marks a Markdown-enabled present
file; without it the file uses legacy present markup. Only the title is
required. After the header come zero or more author blocks, each preceded by
its own blank line, whose first line is conventionally the author name.
*/
package present // import "golang.org/x/tools/present"
