// Copyright 2026 The Forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fmt

import "reflect"

// printEnum prints f and reports true if f is a forgo enum value: a struct
// whose first field is the tag field _tag `fgo:"enum"`, followed by one
// payload struct per variant (see cmd/compile/internal/types2/forgo_enum.go).
// It prints the variant held by the tag and that variant's fields:
//
//	%v   Circle{2}            Empty
//	%+v  Circle{radius:2}     Empty
//	%#v  main.Shape.Circle(2) main.Shape.Empty
//
// Positional fields (named _0, _1, ...) are printed without names.
func (p *pp) printEnum(f reflect.Value, verb rune, depth int) bool {
	t := f.Type()
	if t.NumField() < 2 {
		return false
	}
	if tf := t.Field(0); tf.Name != "_tag" || tf.Tag != `fgo:"enum"` {
		return false
	}
	i := int(f.Field(0).Uint()) + 1
	if i >= t.NumField() {
		return false
	}
	payload := f.Field(i)
	if payload.Kind() != reflect.Struct {
		return false
	}

	if p.fmt.sharpV {
		p.buf.writeString(t.String())
		p.buf.writeByte('.')
	}
	p.buf.writeString(t.Field(i).Name)
	if payload.NumField() == 0 {
		return true
	}
	open, close := byte('{'), byte('}')
	if p.fmt.sharpV {
		open, close = '(', ')'
	}
	p.buf.writeByte(open)
	for j := 0; j < payload.NumField(); j++ {
		if j > 0 {
			if p.fmt.sharpV {
				p.buf.writeString(commaSpaceString)
			} else {
				p.buf.writeByte(' ')
			}
		}
		if name := payload.Type().Field(j).Name; p.fmt.plusV && !positionalField(name) {
			p.buf.writeString(name)
			p.buf.writeByte(':')
		}
		p.printValue(getField(payload, j), verb, depth+1)
	}
	p.buf.writeByte(close)
	return true
}

// positionalField reports whether name is the name of an enum variant's
// positional field: _ followed by decimal digits.
func positionalField(name string) bool {
	if len(name) < 2 || name[0] != '_' {
		return false
	}
	for _, c := range name[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
