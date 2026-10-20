// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package versions

import (
	"strings"
)

// Lang returns the Go language version for version x. If x is not a valid
// version, Lang returns the empty string.
func Lang(x string) string {
	v := lang(stripGo(x))
	if v == "" {
		return ""
	}
	return x[:2+len(v)] // "go"+v without allocation
}

func Compare(x, y string) int { return compare(stripGo(x), stripGo(y)) }

// IsValid reports whether the version x is valid.
func IsValid(x string) bool { return isValid(stripGo(x)) }

// stripGo converts from a "go1.21" version to a "1.21" version.
func stripGo(v string) string {
	v, _, _ = strings.Cut(v, "-") // strip -bigcorp suffix.
	if len(v) < 2 || v[:2] != "go" {
		return ""
	}
	return v[2:]
}
