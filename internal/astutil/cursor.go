// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package astutil

import (
	"go/ast"

	"golang.org/x/tools/go/ast/edge"
	"golang.org/x/tools/go/ast/inspector"
)

// UnparenCursor returns the cursor for an expression with any enclosing
// parentheses removed, similar to [ast.Unparen].
func UnparenCursor(cur inspector.Cursor) inspector.Cursor {
	for is[*ast.ParenExpr](cur) {
		cur, _ = cur.FirstChild()
	}
	return cur
}

// UnparenEnclosingCursor returns the first element of the [Cursor.Enclosing]
// sequence that is not itself enclosed in parens.
func UnparenEnclosingCursor(cur inspector.Cursor) inspector.Cursor {
	for cur.ParentEdgeKind() == edge.ParenExpr_X {
		cur = cur.Parent()
	}
	return cur
}
