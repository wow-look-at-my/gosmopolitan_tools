# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repository is

A fork of `golang.org/x/tools`, including `gopls`, for the **gosmopolitan** Go toolchain (wow-look-at-my/gosmopolitan). Everything here is upstream x/tools except the changes listed below. Take upstream's side on any conflict that does not touch them.

## Build and test with the fork's toolchain, never a stock Go

The fork's language has parameter defaults. A stock Go cannot compile the tests here that assert them. Install the fork first. Then pin `GOOS` and `GOARCH` to the host on every command. The fork defaults to `GOOS=cosmo`. An APE cannot be an editor's language server.

```bash
curl -fL --compressed "https://dl.pazer.build/gosmopolitan?branch=master&os=linux&arch=amd64" | tar -xz
export PATH="$PWD/go/bin:$PATH"
GOOS=linux GOARCH=amd64 go build -o ~/bin/gopls ./gopls
GOOS=linux GOARCH=amd64 go test ./internal/gcimporter/... ./internal/pkgbits/... ./internal/refactor/inline/... ./go/ssa/...
```

## The fork changes, and what breaks without each

Depth, including the toolchain-side half: `docs/GOPLS.md` and `docs/OPTIONAL-PARAMS.md` in the gosmopolitan repository.

- **`internal/gcimporter` (indexed format).** `iexportVersionParamDefaults` (4) writes a bool per parameter and the constant behind it. gopls caches each package as shallow export data. A default dropped here makes a call in ANOTHER package report a missing argument.
- **`internal/pkgbits` and `internal/gcimporter` (unified format).** Bitstream V5 means parameter defaults in this fork. Upstream spends V5 on a method index instead. This tree must not read one. A reader that takes the wrong field desynchronizes rather than failing. So `ureader.go` keeps V4's method ordering.
- **`go/ssa`.** The builder reads an omitted argument's value off `Var.Default`. It used to index one argument per parameter. That ran past the end of a short call and broke every SSA-based analyzer on that file.
- **`internal/refactor/inline`.** The inliner supplies an argument for each omitted parameter. Its arity assert fired otherwise.
- **`gopls/internal/golang/types_format.go`.** A signature prints `= <default>` on an optional parameter. A cloned `ast.Field` keeps its `Default`.

## Known gap

staticcheck (`honnef.co/go/tools`) builds its own IR, not `go/ssa`. That builder still indexes one argument per parameter. A call that omits a defaulted argument panics it. gopls recovers the panic and drops staticcheck's diagnostics for that package. The module is third-party and is not forked here. Depth: `docs/GOPLS.md` in the gosmopolitan repository.

## CI

`.github/workflows/cosmo-ci.yml` installs the published toolchain on every push, builds gopls with it, and runs the tests for each package above. Building gopls is itself an assertion: its go.mod requires `go 1.27.0`, which a toolchain whose version string no parser accepts cannot satisfy.
