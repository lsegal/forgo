// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

// A c-shared main package imports "C". cmd/cgo cannot parse forgo's
// syntax, so the import is kept out of loader.go.

import "C"

func main() {}
