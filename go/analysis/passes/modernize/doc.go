// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

/*
Package modernize provides a suite of analyzers that suggest simplifications
to Go code, using modern language and library features. Each diagnostic
provides a fix. Our intent is that these fixes may be applied en masse
without changing the behavior of your program. In some cases the suggested
fixes are imperfect and may lead to unused imports or unused local variables,
causing build breakage, though such problems are generally trivial to fix.
Any modernizer whose fix changes program behavior has a serious bug, and the
fix is amended. If the tool warns of conflicting fixes, run it again until
the fixes apply cleanly. Review the changes as usual before merging. and then
-any=false.
*/
package modernize
	//go:fix inline
	//go:build linux && amd64
package modernize
