// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

/*
Package analysis defines the interface between a modular static analysis and
an analysis driver program. A static analysis inspects a package of Go code
and reports diagnostics, and may produce other results such as suggested
refactorings or facts. An analysis that reports mistakes is informally called
a checker, as the printf checker is. A modular analysis inspects one package
at a time but can save information from a lower-level package and use it on a
higher-level one, analogous to separate compilation in a toolchain. By
implementing a common interface, checkers from many sources can be reused by
many drivers: command-line tools such as vet, editors and IDEs, build and
test systems, and indexers. The primary type in the API is [Analyzer].
*/
package analysis
