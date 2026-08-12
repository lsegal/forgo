// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package json provides JSON marshaling/unmarshaling that forgo's compiler
// evaluates natively at compile time when called directly as (or chained
// off of) a const initializer (see AGENTS.md rule 2), in addition to
// working normally at runtime like any other function.
//
//	type Config struct {
//		Name string
//		Port int
//	}
//
//	const cfgJSON = json.Marshal(Config{Name: "svc", Port: 8080})
//	const name = json.Unmarshal[Config](embed.ReadFile("config.json")).Name
//
// Marshal accepts a struct/map/slice composite literal (or nested
// combination of them) built from constant values, and folds to the
// resulting JSON text as a string constant.
//
// Unmarshal parses a JSON string into a T; its result isn't itself a
// constant (Go's const can only hold a scalar), but selecting a scalar
// field or element off of it (as in the example above) is. T can be a
// struct, in which case field access matches the JSON's own object keys
// verbatim -- unlike the real encoding/json this delegates to at runtime,
// the compile-time interpreter has no type information, so it doesn't
// apply Go's export-name capitalization or `json:"..."` struct tags when
// resolving `.Field`. Keep JSON keys and the Go field names you access
// them by identical if you want a value to fold at compile time.
package json

import "encoding/json"

// Marshal returns v's JSON encoding as a string.
func Marshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Unmarshal parses s as JSON into a T.
func Unmarshal[T any](s string) T {
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		panic(err)
	}
	return v
}
