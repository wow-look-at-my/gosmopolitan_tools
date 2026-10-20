// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package robustio

import "syscall"

// The robustio package is copied from cmd/go/internal/robustio, a package.

//go:generate go run copyfiles.go

// Since the gopls module cannot access internal/syscall/windows, copy a necessary constant.
const ERROR_SHARING_VIOLATION syscall.Errno = 32
