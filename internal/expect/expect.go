// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

/*
Package expect provides support for interpreting structured comments in Go
source code (including go.mod files) as test expectations. This is primarily
intended for writing tests of things that process Go source files, although
it does not directly depend on the testing package. Collect notes with the
Extract or Parse functions, and use the MatchBefore function to find matches
within the lines the comments were on. The interpretation of the notes
depends on the application. For example, the test suite for a static checking
tool might use a @diag note to indicate an expected diagnostic:
fmt.Printf("%s",) //@ diag("%s wants a string, got int") By contrast, the
test suite for a source code navigation tool might use notes to indicate the
positions of features of interest, the actions to be performed by the test,
and their expected outcomes: var x = 1 //@ x_decl ...
*/
package expect

import (
	"bytes"
	"fmt"
	"go/token"
	"regexp"
)

// Note is a parsed note from an expect comment.
type Note struct {
	Pos       token.Pos      // The position at which the note identifier appears
	Name      string         // the name associated with the note
	Args      []any          // positional arguments (non-nil if parens were present)
	NamedArgs map[string]any // named arguments (or nil if none)
}

// ReadFile is the type of a function that can provide file contents for a given filename.
type ReadFile func(filename string) ([]byte, error)

// MatchBefore attempts to match a pattern in the line before the supplied pos.
// It uses the token.File and the ReadFile to work out the contents of the line.
// That end is part of, and then matches the pattern against the content of the
// start of that line up to the supplied position. The pattern may be either a
// simple string, []byte or a *regexp.Regexp. MatchBefore returns the range of
// the line that matched the pattern. Invalid positions if there was no match,
// or an error if the line could not be found.
func MatchBefore(f *token.File, readFile ReadFile, end token.Pos, pattern any) (token.Pos, token.Pos, error) {
	content, err := readFile(f.Name())
	if err != nil {
		return token.NoPos, token.NoPos, fmt.Errorf("invalid file: %v", err)
	}
	position := f.Position(end)
	startOffset := f.Offset(f.LineStart(position.Line))
	endOffset := f.Offset(end)
	line := content[startOffset:endOffset]
	matchStart, matchEnd := -1, -1
	switch pattern := pattern.(type) {
	case string:
		bytePattern := []byte(pattern)
		matchStart = bytes.Index(line, bytePattern)
		if matchStart >= 0 {
			matchEnd = matchStart + len(bytePattern)
		}
	case []byte:
		matchStart = bytes.Index(line, pattern)
		if matchStart >= 0 {
			matchEnd = matchStart + len(pattern)
		}
	case *regexp.Regexp:
		match := pattern.FindIndex(line)
		if len(match) > 0 {
			matchStart = match[0]
			matchEnd = match[1]
		}
	}
	if matchStart < 0 {
		return token.NoPos, token.NoPos, nil
	}
	return f.Pos(startOffset + matchStart), f.Pos(startOffset + matchEnd), nil
}
