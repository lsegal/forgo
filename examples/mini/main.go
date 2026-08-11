package main

import "fmt"

//forgo:comptime
func double(n int) int {
	return n * 2
}

const ten = double(5)

func main() {
	fmt.Println(ten)
}
