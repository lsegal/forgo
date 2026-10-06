// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file contains tests for vet on forgo's //fgo:comptime constants
// and //fgo:macro calls, which vet must fold and expand the way the
// compiler does before it can type-check the package.

package comptime

import (
	"comptime/embed"
	"comptime/json"
	"errors"
	"fmt"
	"net/http"
)

//fgo:comptime
func double(n int) int {
	return n * 2
}

//fgo:comptime
func label(n int) string {
	return fmt.Sprintf("n=%d", double(n))
}

const ten = double(5)

const tenLabel = label(5)

// A comptime constant is a real constant: it can size an array.
var sized [ten]byte

type Config struct {
	Name  string
	Sizes []int
}

// config.json is read relative to this file, not the working directory.
const config = json.Unmarshal[Config](embed.ReadFile("config.json"))

var sizedByField [config.Sizes[1]]byte

//fgo:macro
func twice(x Node) Node {
	return Quote(func() {
		Splice(x) + Splice(x)
	})
}

//fgo:macro
func logf(format, arg Node) Node {
	return Quote(func() {
		fmt.Printf(Splice(format), Splice(arg))
	})
}

func Use() int {
	logf("%s\n", tenLabel)
	logf("%d\n", config.Name) // ERROR "Printf format %d has arg config.Name of wrong type string"
	return twice(len(sized)) + len(sizedByField)
}

// Throw only uses "errors" through the errors.New that throw "..."
// lowers to, so the import must not be reported as unused.
func Throw(s string) (err error) {
	throw "empty" if s == ""
	return
}

// The ? checks the error before resp is used.
func Get(url string) (err error) {
	resp := http.Get(url)?
	defer resp.Body.Close()
	return
}
