// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package main

import (
	"os"
	"os/exec"
	"time"
)

func cmdInterrupt(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		// On timeout, send interrupt, in hopes of shutting down process tree.
		cmd.Process.Signal(os.Interrupt)
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
}
