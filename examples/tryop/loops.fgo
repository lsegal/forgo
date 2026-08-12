package main

import (
	"errors"
	"fmt"
)

func step(n int) (int, error) {
	if n == 4 {
		return 0, errors.New("boom at 4")
	}
	return n + 1, nil
}

func hasNext(n int) (bool, error) {
	if n == 4 {
		return false, errors.New("boom at 4")
	}
	return n < 10, nil
}

func checkPositive(n int) error {
	if n < 0 {
		return errors.New("negative")
	}
	return nil
}

// Bare `?` in an if-init clause (no value bound).
func ifInitBare(n int) (ok bool, err error) {
	if checkPositive(n)?; n > 2 {
		ok = true
	}
	return
}

// `?` in a for-loop's Cond clause: exits the enclosing function (not just
// the loop) as soon as hasNext() fails, via a real error return.
func sumUntilFail(start int) (sum int, err error) {
	for v := start; hasNext(v)?; v++ {
		sum += v
	}
	return
}

// `?` in Post, plus `continue` and `break` inside the body, proving the
// continue->goto rewrite doesn't break control flow.
func sumWithSkip(start, count int) (sum int, err error) {
	i := start
	for n := 0; n < count; n, i = n+1, step(i)? {
		if i%2 == 0 {
			continue
		}
		if i > 1000 {
			break
		}
		sum += i
	}
	return
}

// Labeled loop with `continue Outer` targeting the outer loop specifically,
// while `?` is used inside the inner loop's body.
func labeledContinue(limit int) (total int, err error) {
Outer:
	for i := 0; i < limit; i++ {
		for j := 0; j < 3; j++ {
			v := step(i + j)?
			if v%2 == 0 {
				continue Outer
			}
			total += v
		}
	}
	return
}

func runLoopsDemo() {
	ok, err := ifInitBare(5)
	fmt.Println("ifInitBare(5):", ok, err)
	ok, err = ifInitBare(-1)
	fmt.Println("ifInitBare(-1):", ok, err)

	sum, err := sumUntilFail(1)
	fmt.Println("sumUntilFail(1):", sum, err)

	sum, err = sumWithSkip(0, 3)
	fmt.Println("sumWithSkip(0,3):", sum, err)
	sum, err = sumWithSkip(3, 5)
	fmt.Println("sumWithSkip(3,5):", sum, err)

	total, err := labeledContinue(2)
	fmt.Println("labeledContinue(2):", total, err)
	total, err = labeledContinue(5)
	fmt.Println("labeledContinue(5):", total, err)
}
