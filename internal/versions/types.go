// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package versions

import (
	"go/ast"
	"go/types"
)

// FileVersion returns a file's Go version. The reported version is an unknown
// Future version if a version cannot be determined.
func FileVersion(info *types.Info, file *ast.File) string {
	if v := info.FileVersions[file]; IsValid(v) {
		return v
	}
	// Note: we could instead return runtime.Version() [if valid].
	return Future
}
