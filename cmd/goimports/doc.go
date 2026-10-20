// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

/*
Command goimports updates your Go import lines, adding missing ones and
removing unreferenced ones. $ go install
golang.org/x/tools/cmd/goimports@latest In addition to fixing imports,
goimports also formats your code in the same style as gofmt so it can be used
as a replacement for your editor's gofmt-on-save hook. For emacs, make sure
you have the latest go-mode.el: https://github.com/dominikh/go-mode.el Then
in your .emacs file: (setq gofmt-command "goimports") (add-hook
'before-save-hook 'gofmt-before-save) For vim, set "gofmt_command" to
"goimports": https://golang.org/change/39c724dd7f252 the steps described
here: http://michaelwhatcott.com/gosublime-goimports/ For other editors, you
probably know what to do.
*/
package main // import "golang.org/x/tools/cmd/goimports"
