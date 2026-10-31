// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestForgoNogcStrict checks that //fgo:nogc strict turns every hand-off
// of a nogc object to the garbage collector into a compile error that
// says where the object goes, and leaves code that keeps its objects
// counted alone.
func TestForgoNogcStrict(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	t.Parallel()

	const src = `package main

import "fmt"

type Node struct {
	Val  int
	Next *Node
}

var keep *Node

var saved []*Node

func remember(n *Node) { saved = append(saved, n) } // ERROR "n escapes to the garbage collector: stored in memory reached from global saved"

//fgo:nogc strict
func leaks(n int) int {
	var head *Node
	for i := range n {
		head = &Node{Val: i, Next: head}
	}
	keep = head // ERROR "head escapes to the garbage collector: stored in global keep"
	var x any = head // ERROR "head escapes to the garbage collector: converted to an interface"
	fmt.Println(x)
	m := map[int]*Node{1: head} // ERROR "stored in a map"
	_ = m
	ch := make(chan *Node, 1)
	ch <- head // ERROR "sent on a channel"
	go func() { _ = head.Val }() // ERROR "captured by a goroutine"
	return sum(head)
}

//fgo:nogc strict
func viaCallee() int {
	n := &Node{Val: 1}
	remember(n) // ERROR "stored in memory reached from global saved"
	return n.Val
}

//fgo:nogc strict
func returns() *Node { return &Node{Val: 2} } // ERROR "returned to code that is not nogc"

//fgo:nogc strict
func cycle(a, b *Node) {
	a.Next = &Node{} // ERROR "assigned to field Next, through which the object can reach itself"
}

//fgo:nogc strict
type Stack struct{ top *Node }

func (s *Stack) Push(v int) { s.top = &Node{Val: v, Next: s.top} } // ERROR "stored in an object nogc code did not allocate"

func sum(n *Node) int {
	s := 0
	for ; n != nil; n = n.Next {
		s += n.Val
	}
	return s
}

// counted keeps everything it allocates counted: no errors.
//
//fgo:nogc strict
func counted(n int) int {
	var head *Node
	for i := range n {
		head = &Node{Val: i, Next: head}
	}
	return sum(head)
}

func main() {
	var s Stack
	s.Push(1)
	cycle(&Node{}, nil)
	fmt.Println(leaks(3), viaCallee(), returns().Val, counted(4))
}
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module strict\n\ngo 1.27\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.fgo"), []byte(src), 0o666); err != nil {
		t.Fatal(err)
	}
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", os.DevNull, ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("build succeeded, want strict errors:\n%s", out)
	}

	want := map[int]string{} // line -> expected message
	for i, line := range strings.Split(src, "\n") {
		if _, msg, ok := strings.Cut(line, `// ERROR "`); ok {
			want[i+1] = strings.TrimSuffix(msg, `"`)
		}
	}
	got := map[int][]string{}
	re := regexp.MustCompile(`(?m)^\./main\.fgo:(\d+):\d+: (.*)$`)
	for _, m := range re.FindAllStringSubmatch(string(out), -1) {
		var line int
		for _, c := range m[1] {
			line = line*10 + int(c-'0')
		}
		got[line] = append(got[line], m[2])
	}
	for line, msg := range want {
		found := false
		for _, g := range got[line] {
			if strings.Contains(g, "nogc: strict: ") && strings.Contains(g, msg) {
				found = true
			}
		}
		if !found {
			t.Errorf("line %d: missing error %q; got %q", line, msg, got[line])
		}
	}
	for line, msgs := range got {
		if _, ok := want[line]; !ok {
			t.Errorf("line %d: unexpected errors %q", line, msgs)
		}
	}
}

// TestForgoNogcRun builds and runs each program in testdata/fgonogc,
// which panics if the reference counts or live object counts it checks
// are wrong.
func TestForgoNogcRun(t *testing.T) {
	testenv.MustHaveGoRun(t)
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("testdata", "fgonogc", "*.fgo"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			t.Parallel()
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fgonogc\n\ngo 1.27\n"), 0o666); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "main.fgo"), src, 0o666); err != nil {
				t.Fatal(err)
			}
			cmd := testenv.Command(t, testenv.GoToolPath(t), "run", ".")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOGC=10")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%v\n%s", err, out)
			}
		})
	}
}
