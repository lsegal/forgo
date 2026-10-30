// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gc

import (
	"cmd/compile/internal/objw"
	"cmd/internal/obj"
	"internal/abi"
)

// fgoAddShadowInfo declares fn's FUNCDATA_FgoShadow symbol (see
// ssagen.fgoShadowInfo), as addGCLocals does for the other funcdata.
func fgoAddShadowInfo(fn *obj.FuncInfo) {
	fd := fn.Pcln.Funcdata
	if len(fd) <= abi.FUNCDATA_FgoShadow {
		return
	}
	if x := fd[abi.FUNCDATA_FgoShadow]; x != nil && !x.OnList() {
		objw.Global(x, int32(len(x.P)), obj.RODATA|obj.DUPOK)
		x.Set(obj.AttrStatic, true)
	}
}
