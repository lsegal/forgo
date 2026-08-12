package a

import "errors"

// simple: single-statement return guard, gets rewritten to postfix if.
// (Deliberately doesn't use "throw" here: analysistest's default driver
// uses x/tools' unpatched go/ast/inspector, which -- like every other
// forgo addition -- needs the same patching build-forgopls.sh applies
// before building the real forgopls; see that script's comment. Using
// "throw" in this package's own test fixtures would panic for that
// reason, independent of this analyzer's own correctness.)
func check(s string) (n int, err error) {
	if s == "" { // want `can use postfix if instead`
		return 0, errors.New("empty")
	}
	n = len(s)
	return
}

// return: single-statement return guard, gets rewritten.
func classify(x int) string {
	if x < 0 { // want `can use postfix if instead`
		return "negative"
	}
	return "non-negative"
}

// continue: single-statement branch guard inside a loop, gets rewritten.
func sumOdds(upto int) int {
	total := 0
	for i := 0; i < upto; i++ {
		if i%2 == 0 { // want `can use postfix if instead`
			continue
		}
		total += i
	}
	return total
}

// plain assignment guard, gets rewritten.
func clamp(x int) int {
	if x < 0 { // want `can use postfix if instead`
		x = 0
	}
	return x
}

// short variable declaration: must NOT fire, since wrapping ":=" would
// change the declared variable's scope.
func declGuard(cond bool) int {
	if cond {
		y := 5
		return y
	}
	return 0
}

// two statements in the body: must NOT fire.
func twoStmts(cond bool) int {
	total := 0
	if cond {
		total++
		total++
	}
	return total
}

// has an else branch: must NOT fire.
func withElse(cond bool) int {
	if cond {
		return 1
	} else {
		return 2
	}
}

// has an init clause: must NOT fire.
func withInit(f func() bool) int {
	if ok := f(); ok {
		return 1
	}
	return 0
}

var _ = errors.New
