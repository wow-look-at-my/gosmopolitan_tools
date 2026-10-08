// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package analyzerutil

import (
	"fmt"
	"go/parser"
	"go/token"
	"strings"
	"sync"
)

// MustExtractDoc is like [ExtractDoc] but it panics on error.
//
// To use, define a doc.go file such as:
//
//	// Package halting defines an analyzer of program termination.
//	//
//	// # Analyzer halting
//	//
//	// halting: reports whether execution will halt.
//	//
//	// The halting analyzer reports a diagnostic for functions
//	// that run forever. To suppress the diagnostics, try inserting
//	// a 'break' statement into each loop.
//	package halting
//
//	import _ "embed"
//
//	//go:embed doc.go
//	var doc string
//
// And declare your analyzer as:
//
//	var Analyzer = &analysis.Analyzer{
//		Name:             "halting",
//		Doc:              analyzerutil.MustExtractDoc(doc, "halting"),
//		...
//	}
func MustExtractDoc(content, name string) string {
	doc, err := ExtractDoc(content, name)
	if err != nil {
		panic(err)
	}
	return doc
}

// ExtractDoc extracts a section of a package doc comment from the
// provided contents of an analyzer package's doc.go file.
//
// A section is a portion of the comment between one heading and
// the next, using this form:
//
//	# Analyzer NAME
//
//	NAME: SUMMARY
//
//	Full description...
//
// where NAME matches the name argument, and SUMMARY is a brief
// verb-phrase that describes the analyzer. The following lines, up
// until the next heading or the end of the comment, contain the full
// description. ExtractDoc returns the portion following the colon,
// which is the form expected by Analyzer.Doc.
//
// Example:
//
//	# Analyzer printf
//
//	printf: checks consistency of calls to printf
//
//	The printf analyzer checks consistency of calls to printf.
//	Here is the complete description...
//
// This notation allows a single doc comment to provide documentation
// for multiple analyzers, each in its own section.
// The HTML anchors generated for each heading are predictable.
//
// It returns an error if the content was not a valid Go source file
// containing a package doc comment with a heading of the required
// form.
//
// This machinery enables the package documentation (typically
// accessible via the web at https://pkg.go.dev/) and the command
// documentation (typically printed to a terminal) to be derived from
// the same source and formatted appropriately.
func ExtractDoc(content, name string) (string, error) {
	if content == "" {
		return "", fmt.Errorf("empty Go source file")
	}
	text, found := docText(content)
	if !found {
		// A doc comment in another shape, such as a block comment, which the parser reads.
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "", content, parser.ParseComments|parser.PackageClauseOnly)
		if err != nil {
			return "", fmt.Errorf("not a Go source file")
		}
		if f.Doc == nil {
			return "", fmt.Errorf("Go source file has no package doc comment")
		}
		text = f.Doc.Text()
	}
	for section := range strings.SplitSeq(text, "\n# ") {
		if body := strings.TrimPrefix(section, "Analyzer "+name); body != section &&
			body != "" &&
			body[0] == '\r' || body[0] == '\n' {
			body = strings.TrimSpace(body)
			rest := strings.TrimPrefix(body, name+":")
			if rest == body {
				return "", fmt.Errorf("'Analyzer %s' heading not followed by '%s: summary...' line", name, name)
			}
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("package doc comment contains no 'Analyzer %s' heading", name)
}

// docTexts holds the doc text of each file content read so far.
var docTexts sync.Map

// docText is lineCommentDoc, answered once per file content.
func docText(content string) (text string, found bool) {
	if cached, ok := docTexts.Load(content); ok {
		return cached.(string), true
	}
	text, found = lineCommentDoc(content)
	if found {
		docTexts.Store(content, text)
	}
	return text, found
}

// lineCommentDoc reads the package doc comment of a file whose doc is a run
// of comments, each starting a line, right before the package clause. It
// answers its text the way [ast.CommentGroup.Text] does. It reads no
// further than the package clause, and parses nothing: every analyzer calls
// ExtractDoc at init. A parse of each doc.go is most of what a tool pays to
// start. found is false for a file this reader does not recognize, which
// the parser then reads.
func lineCommentDoc(content string) (text string, found bool) {
	var lines []string
	for rest := content; ; {
		line, after, more := strings.Cut(rest, "\n")
		rest = after
		switch {
		case strings.HasPrefix(line, "//"):
			body := line[2:]
			if body == "" || body[0] == ' ' {
				lines = append(lines, stripTrailingWhitespace(strings.TrimPrefix(body, " ")))
			} else if !isDirective(body) {
				lines = append(lines, stripTrailingWhitespace(body))
			}
		case strings.HasPrefix(line, "/*"):
			body, tail, closed := strings.Cut(line[2:], "*/")
			if !closed {
				if !more {
					return "", false
				}
				inner, afterClose, closed := strings.Cut(rest, "*/")
				if !closed {
					return "", false
				}
				body += "\n" + inner
				tail, rest, more = strings.Cut(afterClose, "\n")
			}
			if strings.TrimSpace(tail) != "" {
				return "", false
			}
			for bodyLine := range strings.SplitSeq(body, "\n") {
				lines = append(lines, stripTrailingWhitespace(bodyLine))
			}
		case strings.TrimSpace(line) == "":
			lines = nil
		case strings.HasPrefix(line, "package ") || strings.HasPrefix(line, "package\t"):
			if len(lines) == 0 {
				return "", false
			}
			return commentText(lines), true
		default:
			return "", false
		}
		if !more {
			return "", false
		}
	}
}

// stripTrailingWhitespace is what [ast.CommentGroup.Text] trims from a line.
func stripTrailingWhitespace(s string) string {
	return strings.TrimRight(s, " \t")
}

// commentText joins the lines of a comment the way [ast.CommentGroup.Text]
// does. Leading and trailing empty lines removed, a run of empty lines
// reduced to one, and a newline after every line.
func commentText(lines []string) string {
	var sb strings.Builder
	empty := 0
	for _, line := range lines {
		if line == "" {
			empty++
			continue
		}
		if sb.Len() > 0 && empty > 0 {
			sb.WriteByte('\n')
		}
		empty = 0
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// isDirective reports whether c, the text after "//", is a comment
// directive, which [ast.CommentGroup.Text] leaves out of a doc comment.
func isDirective(c string) bool {
	if strings.HasPrefix(c, "line ") || strings.HasPrefix(c, "extern ") || strings.HasPrefix(c, "export ") {
		return true
	}
	colon := strings.Index(c, ":")
	if colon <= 0 || colon+1 >= len(c) {
		return false
	}
	for idx := 0; idx <= colon+1; idx++ {
		if idx == colon {
			continue
		}
		ch := c[idx]
		if ch < 'a' || ch > 'z' {
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}
	return true
}
