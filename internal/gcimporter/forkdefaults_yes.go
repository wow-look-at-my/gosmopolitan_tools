// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build gosmopolitan

package gcimporter

import "go/types"

// This file is the half that KEEPS what the fork's export data carries. It
// needs a go/types that can hold a parameter default and a readonly bit, so
// it builds only under the gosmopolitan tag. A consumer that wants the values
// -- gopls -- is built by the fork and sets the tag. One built by a stock Go
// takes forkdefaults_no.go instead: the reader still consumes the same bits,
// so the bitstream stays aligned either way, and only the storing differs.

type paramDefault = types.ParamDefault

type fieldDefault = types.FieldDefault

func setParamDefault(v *types.Var, d *paramDefault) { v.SetDefault(d) }

func setReadonly(v *types.Var, ro bool) { v.SetReadonly(ro) }
