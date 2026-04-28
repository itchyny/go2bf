package main

// Fibonacci numbers that exceed byte range, using uint16.
// Prints fib(1) through fib(24); fib(24) = 46368.
func main() {
	var a, b uint16 = uint16(0), uint16(1)
	for i := byte(1); i <= 24; i++ {
		a, b = b, a+b
		print("fib(")
		print(i)
		print(") = ")
		println(a)
	}
}
