// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !gosmopolitan

package gcimporter

import (
	"go/constant"
	"go/types"
)

// This file is the half a stock Go compiles. Its go/types cannot hold a
// parameter default or a readonly bit, so the values are read and dropped.
// The reader consumes the same bits either way, which is what keeps the
// bitstream aligned -- dropping a value costs a caller the value, whereas
// skipping the read would corrupt every field after it.

type paramDefault struct {
	Const  constant.Value
	Fields []fieldDefault
}

type fieldDefault struct {
	Name  string
	Value *paramDefault
}

func setParamDefault(*types.Var, *paramDefault) {}

func setReadonly(*types.Var, bool) {}
