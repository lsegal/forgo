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
	forgoExpectErrors(t, src, "nogc: strict: ")
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

// TestForgoNogcCross builds each program in testdata/fgonogc for other
// architectures, whose lowering the nogc pass must also leave schedulable.
func TestForgoNogcCross(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if testing.Short() {
		t.Skip("slow")
	}
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("testdata", "fgonogc", "*.fgo"))
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64", "386", "arm", "riscv64"} {
		t.Run(arch, func(t *testing.T) {
			t.Parallel()
			for _, file := range files {
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
				cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", os.DevNull, ".")
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("%s: %v\n%s", filepath.Base(file), err, out)
				}
			}
		})
	}
}

// forgoExpectErrors builds src, the main package, and checks that it fails
// with an error starting with prefix and containing msg on each line
// marked // ERROR "msg", and with no errors on other lines.
func forgoExpectErrors(t *testing.T, src, prefix string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module errs\n\ngo 1.27\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.fgo"), []byte(src), 0o666); err != nil {
		t.Fatal(err)
	}
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", os.DevNull, ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("build succeeded, want errors:\n%s", out)
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
			if strings.Contains(g, prefix) && strings.Contains(g, msg) {
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

// TestForgoWithFree checks the errors for with statements freeing their
// value that would let a reference outlive the statement: those found
// before type checking it again (copies and closures), and those the
// nogc pass finds.
func TestForgoWithFree(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	t.Parallel()

	const prelude = `package main

import "fmt"

type Node struct {
	Val  int
	Next *Node
}

type Holder struct{ N *Node }

var global *Node

func build(n int) *Node {
	var l *Node
	for i := range n {
		l = &Node{Val: i, Next: l}
	}
	return l
}
`
	t.Run("copies", func(t *testing.T) {
		t.Parallel()
		forgoExpectErrors(t, prelude+`
//fgo:nogc
func copies(n *Node) *Node {
	var outer *Node
	with a := build(3) {
		b := a
		outer = b // ERROR "a outlives its with statement: copied to outer, declared outside it"
		global = a // ERROR "copied to global, declared outside it"
		func() { _ = a.Val }() // ERROR "used by a function literal"
		_ = &a // ERROR "its address is taken"
	}
	with c := n { // ERROR "freeing c needs a new object"
		_ = c
	}
	with d := new(Node) {
		d = nil // ERROR "cannot assign to d"
	}
	return outer
}

func gc() {
	with e := build(1) { // ERROR "freeing it at the end of the block needs nogc code"
		_ = e
	}
}

func main() { copies(nil); gc(); fmt.Println() }
`, "forgo: with: ")
	})
	t.Run("pass", func(t *testing.T) {
		t.Parallel()
		forgoExpectErrors(t, prelude+`
//fgo:nogc
func stores(h *Holder) {
	with a := build(3) {
		h.N = a // ERROR "a outlives its with statement: stored in another object"
		fmt.Println(a) // ERROR "converted to an interface"
		_ = &Holder{N: a} // ERROR "stored in another object"
	}
}

//fgo:nogc
func returns() *Node {
	with d := build(2) {
		return d // ERROR "d outlives its with statement: returned"
	}
}

// fine lets nothing outlive its with statements: no errors.
//
//fgo:nogc
func fine() int {
	t := 0
	with l := build(4) {
		for cur := l; cur != nil; cur = cur.Next {
			t += cur.Val
		}
		h := &Holder{N: l.Next} // a child, counted on its own
		t += h.N.Val
	}
	return t
}

func main() { stores(nil); fmt.Println(returns(), fine()) }
`, "nogc: with: ")
	})
}
