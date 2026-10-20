// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

/*
Goyacc is a version of yacc for Go. It is written in Go and generates parsers
written in Go. Usage: goyacc args... It is largely transliterated from the
Inferno version written in Limbo which in turn was largely transliterated
from the Plan version written in C and documented at
https://9p.io/magic/man2html/1/yacc Adepts of the original yacc will have no
trouble adapting to this form of the tool. The directory
$GOPATH/src/golang.org/x/tools/cmd/goyacc/testdata/expr is a yacc program for
a simple expression parser. See expr.y and main.go in that directory for
examples of how to write and build goyacc programs. The generated parser is
reentrant. The parsing function yyParse expects to be given an argument that
conforms to the following interface: type yyLexer interface { Lex(lval place
other token information in lval (which replaces the usual yylval).
*/
package main
