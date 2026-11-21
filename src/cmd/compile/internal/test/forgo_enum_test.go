// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forgoEnumModule writes a module named fgoenum to a new temporary
// directory, with the given files, and returns the directory.
func forgoEnumModule(t *testing.T, files map[string]string) string {
	dir := t.TempDir()
	files["go.mod"] = "module fgoenum\n\ngo 1.27\n"
	for name, src := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const forgoEnumShapes = `package shapes

import "math"

type Shape enum {
	Circle(Radius float64)
	Rect(W, H float64)
	Empty
}

func (s Shape) Area() float64 {
	switch v := s.(type) {
	case Shape.Circle:
		return math.Pi * v.Radius * v.Radius
	case Shape.Rect:
		return v.W * v.H
	case Shape.Empty:
		return 0
	}
}

// Kind is small enough to be inlined into its callers, enum switch and all.
func Kind(s Shape) string {
	switch s.(type) {
	case Shape.Circle, Shape.Rect:
		return "solid"
	default:
		return "empty"
	}
}

type Option[T any] enum {
	Some(T)
	None
}

func Some[T any](v T) Option[T] { return Option.Some(v) }
`

// TestForgoEnumRun checks enum construction, matching and binding,
// generic enums, equality, the zero value, printing, and the use of an
// enum declared in another package.
func TestForgoEnumRun(t *testing.T) {
	testenv.MustHaveGoRun(t)
	t.Parallel()

	const main = `package main

import (
	"fmt"
	"reflect"

	"fgoenum/shapes"
)

type Result[T, E any] enum {
	Ok(T)
	Err(E)
}

func div(a, b int) Result[int, string] {
	if b == 0 {
		return Result[int, string].Err("division by zero")
	}
	return Result.Ok[int, string](a / b)
}

func Map[T, U any](o shapes.Option[T], f func(T) U) shapes.Option[U] {
	switch v := o.(type) {
	case shapes.Option.Some:
		return shapes.Option.Some(f(v._0))
	case shapes.Option.None:
		return shapes.Option[U].None
	}
}

type Color enum { Red; Green; Blue }

func (c Color) String() string {
	switch c {
	case Color.Red:
		return "red"
	case Color.Green:
		return "green"
	case Color.Blue:
		return "blue"
	}
}

func main() {
	all := []shapes.Shape{shapes.Shape.Circle(1), shapes.Shape.Rect(2, 3), shapes.Shape.Empty}
	for _, s := range all {
		switch v := s.(type) {
		case shapes.Shape.Circle:
			fmt.Printf("circle r=%v area=%.2f %s\n", v.Radius, s.Area(), shapes.Kind(s))
		case shapes.Shape.Rect, shapes.Shape.Empty:
			fmt.Printf("%v area=%.2f %s\n", v, v.Area(), shapes.Kind(v))
		}
	}

	var zero shapes.Shape
	fmt.Println(zero == shapes.Shape.Circle(0), zero)
	fmt.Println(shapes.Shape.Rect(1, 2) == shapes.Shape.Rect(1, 2), shapes.Shape.Rect(1, 2) == shapes.Shape.Rect(2, 1), shapes.Shape.Empty == shapes.Shape.Circle(0))

	seen := map[shapes.Shape]int{}
	for _, s := range append(all, shapes.Shape.Rect(2, 3)) {
		seen[s]++
	}
	fmt.Println(len(seen), seen[shapes.Shape.Rect(2, 3)])

	fmt.Printf("%v %+v %#v\n", shapes.Shape.Rect(1, 2), shapes.Shape.Rect(1, 2), shapes.Shape.Rect(1, 2))
	fmt.Printf("%v %+v %#v\n", shapes.Shape.Empty, shapes.Shape.Empty, shapes.Shape.Empty)

	for _, r := range []Result[int, string]{div(7, 2), div(1, 0)} {
		switch v := r.(type) {
		case Result.Ok:
			fmt.Println("ok", v._0)
		case Result.Err:
			fmt.Println("err", v._0)
		}
		fmt.Printf("%v %+v\n", r, r)
	}

	o := Map(shapes.Some(20), func(i int) string { return fmt.Sprint(i * 2) })
	fmt.Printf("%v %#v\n", o, Map(shapes.Option[int].None, func(i int) string { return "" }))

	fmt.Println(Color.Green, []Color{Color.Blue, Color.Red})

	t := reflect.TypeOf(zero)
	fmt.Println(t.Kind(), t.NumField(), t.Field(0).Name, t.Field(0).Type, t.Field(0).Tag, t.Field(2).Name)
}
`
	dir := forgoEnumModule(t, map[string]string{
		"shapes/shapes.go": forgoEnumShapes,
		"main.go":          main,
	})
	cmd := testenv.Command(t, testenv.GoToolPath(t), "run", ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run: %v\n%s", err, out)
	}
	const want = `circle r=1 area=3.14 solid
Rect{2 3} area=6.00 solid
Empty area=0.00 empty
true Circle{0}
true false false
3 2
Rect{1 2} Rect{W:1 H:2} shapes.Shape.Rect(1, 2)
Empty Empty shapes.Shape.Empty
ok 3
Ok{3} Ok{3}
err division by zero
Err{division by zero} Err{division by zero}
Some{40} shapes.Option[string].None
green [blue red]
struct 4 _tag uint8 fgo:"enum" Rect
`
	if got := string(out); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
}

// TestForgoEnumAddedVariant checks that adding a variant to an enum makes
// a type switch over it that was exhaustive fail to compile, naming the
// new variant, in a package that imports the enum.
func TestForgoEnumAddedVariant(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	t.Parallel()

	const main = `package main

import (
	"fmt"

	"fgoenum/shapes"
)

func describe(s shapes.Shape) string {
	switch v := s.(type) {
	case shapes.Shape.Circle:
		return fmt.Sprint("circle ", v.Radius)
	case shapes.Shape.Rect:
		return fmt.Sprint("rect ", v.W, v.H)
	case shapes.Shape.Empty:
		return "empty"
	}
}

func main() { fmt.Println(describe(shapes.Shape.Empty)) }
`
	build := func(shapesSrc string) (string, error) {
		dir := forgoEnumModule(t, map[string]string{
			"shapes/shapes.go": shapesSrc,
			"main.go":          main,
		})
		cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", os.DevNull, ".")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := build(forgoEnumShapes); err != nil {
		t.Fatalf("exhaustive switch: go build: %v\n%s", err, out)
	}

	added := strings.Replace(forgoEnumShapes, "\tEmpty\n", "\tEmpty\n\tTriangle(A, B, C float64)\n\tLine(Length float64)\n", 1)
	added = strings.Replace(added, "\tcase Shape.Empty:\n\t\treturn 0\n", "\tcase Shape.Empty, Shape.Triangle, Shape.Line:\n\t\treturn 0\n", 1)
	out, err := build(added)
	if err == nil {
		t.Fatalf("switch missing the added variants compiled:\n%s", out)
	}
	const want = "main.go:10:2: switch on shapes.Shape is not exhaustive: missing cases shapes.Shape.Triangle, shapes.Shape.Line"
	if !strings.Contains(out, want) {
		t.Errorf("go build output:\n%s\nwant it to contain:\n%s", out, want)
	}
}

// TestForgoEnumInvalidTag checks that a switch over an enum without a
// default clause, which may be a terminating statement, panics rather
// than falling off its end for a value whose tag names no variant.
func TestForgoEnumInvalidTag(t *testing.T) {
	testenv.MustHaveGoRun(t)
	t.Parallel()

	const main = `package main

import (
	"fmt"
	"unsafe"

	"fgoenum/shapes"
)

type Color enum { Red; Green }

func name(c Color) string {
	switch c {
	case Color.Red:
		return "red"
	case Color.Green:
		return "green"
	}
}

func try(what string, f func() any) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println(what, "panic:", r)
		}
	}()
	fmt.Println(what, f())
}

func main() {
	s := shapes.Shape.Empty
	try("area", func() any { return s.Area() })
	*(*uint8)(unsafe.Pointer(&s)) = 3
	try("area", func() any { return s.Area() })
	try("stmt", func() any {
		switch s.(type) {
		case shapes.Shape.Circle, shapes.Shape.Rect, shapes.Shape.Empty:
			return "matched"
		}
		return "fell through"
	})

	c := Color.Green
	try("name", func() any { return name(c) })
	*(*uint8)(unsafe.Pointer(&c)) = 200
	try("name", func() any { return name(c) })
}
`
	dir := forgoEnumModule(t, map[string]string{
		"shapes/shapes.go": forgoEnumShapes,
		"main.go":          main,
	})
	cmd := testenv.Command(t, testenv.GoToolPath(t), "run", ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run: %v\n%s", err, out)
	}
	const want = `area 0
area panic: forgo: enum value has an invalid tag
stmt panic: forgo: enum value has an invalid tag
name green
name panic: forgo: enum value has an invalid tag
`
	if got := string(out); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
}
