// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Package timeformat defines an Analyzer that checks for the use of
// time.Format or time.Parse calls with a bad format.
//
// # Analyzer timeformat
//
// The timeformat checker looks for time formats with the (yyyy-dd-mm) format.
// Internationally, "yyyy-dd-mm" does not occur in common calendar date
// standards, and so it is more likely that (yyyy-mm-dd) was intended.
package timeformat
