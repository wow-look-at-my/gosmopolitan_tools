// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package pkgbits implements low-level coding abstractions for Unified IR's
// export data format.
//
// At a low-level, a package is a collection of bitstream elements. Each
// element has a "kind" and a dense, non-negative index. Elements can be
// randomly accessed given their kind and index.
//
// Individual elements are sequences of variable-length values (e.g.,
// integers, booleans, strings, go/constant values, cross-references to other
// elements). Package pkgbits provides APIs for encoding and decoding these
// low-level values. However, the details of mapping higher-level Go
// constructs into elements is left to higher-level abstractions.
package pkgbits
