package main

import "fmt"

// Example of compile-time math evaluation, forgo-style.
//
//forgo:comptime
func calculateFactorial(n int) int {
	result := 1
	for i := 1; i <= n; i++ {
		result *= i
	}
	return result
}

// String interpolation evaluated at compile time.
//
//forgo:comptime
func factorialMessage(n int) string {
	return fmt.Sprintf("Factorial of %d is %d", n, calculateFactorial(n))
}

// Evaluated entirely by the compiler.
const factFive = calculateFactorial(5)

const msg = factorialMessage(5)

// A tiny AST macro: expands to its argument's expression, duplicated.
//
//forgo:macro
func double(x Node) Node {
	return Quote(func() {
		Splice(x) + Splice(x)
	})
}

func compute() int {
	return 21
}

// factFive is a genuine compile-time constant: it can size an array, which
// only accepts constant expressions.
var proof [factFive]byte

func main() {
	fmt.Println(msg)
	fmt.Println("factFive constant:", factFive)
	fmt.Println("array sized by factFive:", len(proof))
	fmt.Println("double(compute()):", double(compute()))
}
