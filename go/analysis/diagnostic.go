// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package analysis

import "go/token"

// A Diagnostic is a message associated with a source location or range.
type Diagnostic struct {
	Pos      token.Pos
	End      token.Pos // optional
	Category string    // optional
	Message  string

	// URL is the optional location of a web page that provides additional documentation for this diagnostic.
	URL string

	// SuggestedFixes is an optional list of fixes to address the problem described by the diagnostic.
	SuggestedFixes []SuggestedFix

	// Related contains optional secondary positions and messages related to the primary diagnostic.
	Related []RelatedInformation
}

// RelatedInformation contains information related to a diagnostic.
type RelatedInformation struct {
	Pos     token.Pos
	End     token.Pos // optional
	Message string
}

// A SuggestedFix is a code change associated with a Diagnostic that a user
// can choose to apply to their code.
type SuggestedFix struct {
	// A verb phrase describing the fix, to be shown to a user trying to decide whether to accept it.
	Message   string
	TextEdits []TextEdit
}

// A TextEdit represents the replacement of the code between Pos and End with
// the new text. Each TextEdit should apply to a single file.
type TextEdit struct {
	// For a pure insertion, End can either be set to Pos or token.NoPos.
	Pos     token.Pos
	End     token.Pos
	NewText []byte
}
