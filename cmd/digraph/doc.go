// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

/*
The digraph command performs queries over unlabelled directed graphs
represented in text form, so it integrates with UNIX pipelines. Input is one
edge per line: zero or more words, separated by unquoted whitespace. A word
may contain a Go-style double-quoted portion. The commands are nodes, degree,
transpose, preds, succs, forward, reverse, somepath, allpaths, sccs, scc,
focus and to dot, which prints Graphviz output. Most take the node names they
work on, as in "digraph succs <node>".
*/
package main
