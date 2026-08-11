package main

import "errors"

func makeThing2(s string) (*Thing, error) {
	if s == "" {
		return nil, errors.New("empty")
	}
	return &Thing{name: s}, nil
}

func (t *Thing) validated() (*Thing, error) {
	if t.name == "bad" {
		return nil, errors.New("bad thing 2")
	}
	return t, nil
}

// Exercises the literal inline chained form: foo()?.bar()?
func inlineChain(s string) (name string, err error) {
	name = makeThing2(s)?.validated()?.name
	return
}
