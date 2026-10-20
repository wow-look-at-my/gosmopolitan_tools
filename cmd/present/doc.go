// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

/*
Present displays slide presentations and articles. It runs a web server that
presents slide and article files from the current directory. Present assumes
presentation authors are trusted, and should not be used with untrusted
inputs. It may be run as a stand-alone command or an App Engine app. To use
with App Engine, copy the files in the tools/cmd/present directory to the
root of your application and create an app.yaml file similar to this:
runtime: go111 handlers: - url: /favicon.ico static_files: static/favicon.ico
upload: static/favicon.ico - url: /static static_dir: static - url: /.*
script: auto # nobuild_files is a regexp that identifies which files to not
build. It # is useful for embedding static assets like code snippets and
preventing # them from producing build errors for your project.
*/
package main
