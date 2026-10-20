// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

//go:build !(unix || aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package testenv

import "os"

// Sigquit is the signal to send to kill a hanging subprocess.
var Sigquit = os.Kill
