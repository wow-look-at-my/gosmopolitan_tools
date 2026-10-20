// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package astutil

import (
	"go/ast"
	"go/token"
	"iter"
	"sort"
	"strings"
)

// Deprecation returns the paragraph of the doc comment that starts with the
// conventional "Deprecation: " marker. Or the end of a single-line comment
// with the deprecation marker, as defined by https://go.dev/wiki/Deprecated.
// Returns "" if the documented symbol is not deprecated.
//
// Deprecation(nil) returns the empty string.
func Deprecation(doc *ast.CommentGroup) string {
	// doc.Text() is newline-terminated.
	docText := doc.Text()
	for p := range strings.SplitSeq(docText, "\n\n") {
		// There is still some ambiguity for deprecation message. This function only
		// returns the paragraph introduced by "Deprecated: ".
		if strings.HasPrefix(p, "Deprecated: ") {
			return p
		}
	}

	// We also want to support deprecation markers in line comments.
	if doc == nil || len(doc.List) != 1 || !strings.HasPrefix(doc.List[0].Text, "//") {
		return ""
	}
	if i := strings.Index(docText, "Deprecated: "); i != -1 {
		return docText[i:]
	}
	return ""
}

// A directive is a comment line with special meaning to the Go toolchain or
// another tool.
type Directive struct {
	Pos  token.Pos // of preceding "//"
	Tool string
	Name string
	Args string // may contain internal spaces
}

// isDirective reports whether c is a comment directive.
// This code is also in go/printer.
func isDirective(c string) bool {
	// "//line " is a line directive. "//extern " is for gccgo. "//export " is
	// for cgo.
	if strings.HasPrefix(c, "line ") || strings.HasPrefix(c, "extern ") || strings.HasPrefix(c, "export ") {
		return true
	}

	// "//[a-z0-9]+:[a-z0-9]" (The // has been removed.)
	colon := strings.Index(c, ":")
	if colon <= 0 || colon+1 >= len(c) {
		return false
	}
	for i := 0; i <= colon+1; i++ {
		if i == colon {
			continue
		}
		b := c[i]
		if !('a' <= b && b <= 'z' || '0' <= b && b <= '9') {
			return false
		}
	}
	return true
}

// Directives returns the directives within the comment.
func Directives(g *ast.CommentGroup) (res []*Directive) {
	if g != nil {
		// Avoid (*ast.CommentGroup).Text() as it swallows directives.
		for _, c := range g.List {
			if len(c.Text) > 2 &&
				c.Text[1] == '/' &&
				c.Text[2] != ' ' &&
				isDirective(c.Text[2:]) {

				tool, nameargs, ok := strings.Cut(c.Text[2:], ":")
				if !ok {
					// Must be one of {line,extern,export}.
					tool, nameargs = "", tool
				}
				name, args, _ := strings.Cut(nameargs, " ") // tab??
				// Permit an additional line comment after the args, chiefly to support [golang.org/x/tools/go/analysis/analysistest].
				args, _, _ = strings.Cut(args, "//")
				res = append(res, &Directive{
					Pos:  c.Slash,
					Tool: tool,
					Name: name,
					Args: strings.TrimSpace(args),
				})
			}
		}
	}
	return
}

// Comments returns an iterator over the comments overlapping the specified interval.
// Comments are sorted by position in the file, so we can use binary search.
func Comments(file *ast.File, start, end token.Pos) iter.Seq[*ast.Comment] {
	return func(yield func(*ast.Comment) bool) {
		// Find the first comment group that overlaps the range.
		i := sort.Search(len(file.Comments), func(i int) bool {
			return file.Comments[i].End() >= start
		})
		for _, cg := range file.Comments[i:] {
			if cg.Pos() > end {
				return
			}
			// Find the first comment in the group that overlaps the range.
			j := sort.Search(len(cg.List), func(j int) bool {
				return cg.List[j].End() >= start
			})
			for _, co := range cg.List[j:] {
				if co.Pos() > end {
					return
				}
				if !yield(co) {
					return
				}
			}
		}
	}
}
