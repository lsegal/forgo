// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package embed provides filesystem helpers that forgo's compiler
// evaluates natively at compile time when called directly as a const
// initializer (see AGENTS.md rule 2), in addition to working normally at
// runtime like any other function.
//
// At compile time, a relative path is resolved against the directory of
// the source file containing the call, so `embed.ReadFile("data.txt")`
// reads the file next to the .go/.fgo source it's written in. At runtime,
// relative paths are resolved the normal way, against the process's
// current working directory.
//
//	const banner = embed.ReadFile("banner.txt")
//	const hasConfig = embed.Exists("config.json")
//	const assetNames = embed.ReadDir("assets") // newline-joined names
//
// Every function here panics on error rather than returning one, since a
// (string, error) result can't be folded into a single compile-time
// constant. Callers that need to handle a missing/unreadable file
// gracefully at runtime should use the os package directly instead.
package embed

import (
	"os"
	"strings"
)

// ReadFile returns the contents of the file at path as a string.
func ReadFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// ReadFileRange returns the length bytes of the file at path starting at
// offset, as a string. It's the "seek" of this package: a way to pull a
// slice out of a file without reading (or folding into the binary) the
// whole thing.
func ReadFileRange(path string, offset, length int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	if offset < 0 || length < 0 || offset+length > len(data) {
		panic("embed: ReadFileRange: range out of bounds")
	}
	return string(data[offset : offset+length])
}

// Exists reports whether path exists.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// IsDir reports whether path exists and is a directory.
func IsDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		panic(err)
	}
	return info.IsDir()
}

// ReadDir returns the names of path's directory entries, one per line,
// sorted by filename.
func ReadDir(path string) string {
	entries, err := os.ReadDir(path)
	if err != nil {
		panic(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return strings.Join(names, "\n")
}

// Getwd returns the process's current working directory.
func Getwd() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return wd
}
