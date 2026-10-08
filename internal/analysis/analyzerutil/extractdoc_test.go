// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package analyzerutil_test

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"golang.org/x/tools/internal/analysis/analyzerutil"
)

// ExtractDoc reads a line-comment doc without the parser. Its answer is the
// parser's, whatever the comment holds: a directive, runs of blank lines,
// or trailing space. This also covers a tab after the package keyword, or a
// block comment.
func TestExtractDocMatchesTheParser(t *testing.T) {
	for _, content := range []string{
		"// Package p\n//\n// # Analyzer foo\n//\n// foo: a summary  \n//\n//\n//\n// more\n//go:generate nothing\n// last\npackage p\n",
		"// Copyright\n\n//go:build tag\n\n//Package p\n//\n//# Analyzer foo\n//\n//foo: a summary\n//  indented\n//\tline\npackage\tp\n",
		"/*\nPackage p\n\n# Analyzer foo\n\nfoo: a summary\n*/\npackage p\n",
		"// Package p\n//\n// # Analyzer foo\n//\n// foo: a summary\n//\n// # Analyzer bar\n//\n// bar: another\npackage p\n",
	} {
		for _, name := range []string{"foo", "bar", "nope"} {
			got, err := analyzerutil.ExtractDoc(content, name)
			if err != nil {
				got = "error: " + err.Error()
			}
			want := parserExtractDoc(content, name)
			if got != want {
				t.Errorf("ExtractDoc(%q) on <<%s>> returned <<%s>>, the parser gives <<%s>>", name, content, got, want)
			}
		}
	}
}

// parserExtractDoc is ExtractDoc as the parser answers it.
func parserExtractDoc(content, name string) string {
	f, err := parser.ParseFile(token.NewFileSet(), "", content, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil {
		return "error: not a Go source file"
	}
	for section := range strings.SplitSeq(f.Doc.Text(), "\n# ") {
		if body := strings.TrimPrefix(section, "Analyzer "+name); body != section &&
			body != "" &&
			body[0] == '\r' || body[0] == '\n' {
			body = strings.TrimSpace(body)
			rest := strings.TrimPrefix(body, name+":")
			if rest == body {
				return "error: 'Analyzer " + name + "' heading not followed by '" + name + ": summary...' line"
			}
			return strings.TrimSpace(rest)
		}
	}
	return "error: package doc comment contains no 'Analyzer " + name + "' heading"
}

func TestExtractDoc(t *testing.T) {
	const multi = `// Copyright

//+build tag

// Package foo
//
// # Irrelevant heading
//
// This is irrelevant doc.
//
// # Analyzer nocolon
//
// This one has the wrong form for this line.
//
// # Analyzer food
//
// food: reports dining opportunities
//
// This is the doc for analyzer 'food'.
//
// # Analyzer foo
//
// foo: reports diagnostics
//
// This is the doc for analyzer 'foo'.
//
// # Analyzer bar
//
// bar: reports drinking opportunities
//
// This is the doc for analyzer 'bar'.
package blah

var x = syntax error
`

	for _, test := range []struct {
		content, name string
		want          string // doc or "error: %w" string
	}{
		{"", "foo",
			"error: empty Go source file"},
		{"//foo", "foo",
			"error: not a Go source file"},
		{"//foo\npackage foo", "foo",
			"error: package doc comment contains no 'Analyzer foo' heading"},
		{multi, "foo",
			"reports diagnostics\n\nThis is the doc for analyzer 'foo'."},
		{multi, "bar",
			"reports drinking opportunities\n\nThis is the doc for analyzer 'bar'."},
		{multi, "food",
			"reports dining opportunities\n\nThis is the doc for analyzer 'food'."},
		{multi, "nope",
			"error: package doc comment contains no 'Analyzer nope' heading"},
		{multi, "nocolon",
			"error: 'Analyzer nocolon' heading not followed by 'nocolon: summary...' line"},
	} {
		got, err := analyzerutil.ExtractDoc(test.content, test.name)
		if err != nil {
			got = "error: " + err.Error()
		}
		if test.want != got {
			t.Errorf("ExtractDoc(%q) returned <<%s>>, want <<%s>>, given input <<%s>>",
				test.name, got, test.want, test.content)
		}
	}
}
