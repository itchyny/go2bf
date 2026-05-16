package main

// Roman numeral conversion via greedy subtraction.
// Demonstrates uint16 values, a struct with mixed
// uint16 + string fields, an ellipsis-sized top-level
// composite var, range over an inline array literal,
// and string concatenation that reuses the existing
// heap buffer in place when it sits at the top of the heap.

type RomanRule struct {
	value  uint16
	symbol string
}

var rules = [...]RomanRule{
	{value: 1000, symbol: "M"},
	{value: 900, symbol: "CM"},
	{value: 500, symbol: "D"},
	{value: 400, symbol: "CD"},
	{value: 100, symbol: "C"},
	{value: 90, symbol: "XC"},
	{value: 50, symbol: "L"},
	{value: 40, symbol: "XL"},
	{value: 10, symbol: "X"},
	{value: 9, symbol: "IX"},
	{value: 5, symbol: "V"},
	{value: 4, symbol: "IV"},
	{value: 1, symbol: "I"},
}

func main() {
	for _, n := range [...]uint16{
		1, 4, 13, 49, 99, 133, 444, 1999, 3888,
	} {
		k, s := n, ""
		for _, r := range rules {
			for k >= r.value {
				s += r.symbol
				k -= r.value
			}
		}
		println(n, "=", s)
	}
}
