// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

/*
Package packages loads Go packages for inspection and analysis. The [Load]
function takes as input a list of patterns and returns a list of [Package]
values describing individual packages matched by those patterns. A [Config]
specifies configuration options, the most important of which is the
[LoadMode], which controls the amount of detail in the loaded packages. Load
passes most patterns directly to the underlying build tool. The default build
tool is the go command. Its supported patterns are described at
https://pkg.go.dev/cmd/go#hdr-Package_lists_and_patterns. Other build systems
may be supported by providing a "driver"; see [The driver protocol]. All
patterns with the prefix "query=", where query is a non-empty string of
letters from [a-z], are reserved and may be interpreted as query operators.
*/
package packages // import "golang.org/x/tools/go/packages"
