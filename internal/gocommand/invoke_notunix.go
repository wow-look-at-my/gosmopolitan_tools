// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

//go:build !unix

package gocommand

import "os"

// sigStuckProcess is the signal to send to kill a hanging subprocess.
var sigStuckProcess = os.Kill
