package a

// crc is a pure function of a constant string: eligible for comptime.
func crc(s string) uint32 {
	var c uint32 = 0xFFFFFFFF
	for i := 0; i < len(s); i++ {
		c ^= uint32(s[i])
		for b := 0; b < 8; b++ {
			if c&1 != 0 {
				c = (c >> 1) ^ 0xEDB88320
			} else {
				c >>= 1
			}
		}
	}
	return ^c
}

var magicHash = crc("forgo") // want `magicHash is computed from constant arguments to crc`

// notPureEnough uses a slice, which isn't in the comptime subset.
func notPureEnough(xs []int) int {
	sum := 0
	for _, x := range xs {
		sum += x
	}
	return sum
}

var total = notPureEnough(nil) // must NOT fire: notPureEnough isn't comptime-eligible

func dynamicArg() string { return "runtime" }

var runtimeInput = crc(dynamicArg()) // must NOT fire: argument isn't constant

//forgo:comptime
func alreadyMarked(n int) int { return n * 2 }

var already = alreadyMarked(3) // must NOT fire: already has the pragma

func hash2(s string) uint32 {
	return crc(s) ^ 0xFF
}

// the "var x T" + "func init() { x = f(...) }" idiom.
var precomputed uint32 // want `precomputed is computed once in init from constant arguments to hash2`

func init() {
	precomputed = hash2("forgo")
}

// multi-statement init: must NOT fire (too risky to partially rewrite).
var notFromInit uint32

func init() {
	notFromInit = hash2("x")
	_ = notFromInit
}
