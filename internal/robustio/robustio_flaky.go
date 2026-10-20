// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

//go:build windows || darwin || plan9

package robustio

import (
	"errors"
	"math/rand"
	"os"
	"time"
)

const arbitraryTimeout = 2000 * time.Millisecond

// retry retries ephemeral errors from f up to an arbitrary timeout to
// work around filesystem flakiness on Windows, Darwin, and a later plan.
func retry(f func() (err error, mayRetry bool)) error {
	var (
		bestErr     error
		lowestErrno uintptr
		start       time.Time
		nextSleep   time.Duration = 1 * time.Millisecond
	)
	for {
		err, mayRetry := f()
		if err == nil || !mayRetry {
			return err
		}

		if errno, ok := getErrno(err); ok && (lowestErrno == 0 || errno < lowestErrno) {
			bestErr = err
			lowestErrno = errno
		} else if bestErr == nil {
			bestErr = err
		}

		if start.IsZero() {
			start = time.Now()
		} else if d := time.Since(start) + nextSleep; d >= arbitraryTimeout {
			break
		}
		time.Sleep(nextSleep)
		nextSleep += time.Duration(rand.Int63n(int64(nextSleep)))
	}

	return bestErr
}

// rename is like os.Rename, but retries ephemeral errors.
func rename(oldpath, newpath string) (err error) {
	return retry(func() (err error, mayRetry bool) {
		err = os.Rename(oldpath, newpath)
		return err, isEphemeralError(err)
	})
}

// readFile is like os.ReadFile, but retries ephemeral errors.
func readFile(filename string) ([]byte, error) {
	var b []byte
	err := retry(func() (err error, mayRetry bool) {
		b, err = os.ReadFile(filename)

		// Unlike in rename, we do not retry errFileNotFound here: it can occur as a spurious error, but the file may also genuinely not exist.
		return err, isEphemeralError(err) && !errors.Is(err, errFileNotFound)
	})
	return b, err
}

func removeAll(path string) error {
	return retry(func() (err error, mayRetry bool) {
		err = os.RemoveAll(path)
		return err, isEphemeralError(err)
	})
}
