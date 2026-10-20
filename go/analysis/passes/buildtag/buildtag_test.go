// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package buildtag_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
	"golang.org/x/tools/go/analysis/passes/buildtag"
)

func Test(t *testing.T) {
	// This test has a dedicated hack in the analysistest package.
	analysistest.Run(t, analysistest.TestData(), buildtag.Analyzer, "a", "b")
}
