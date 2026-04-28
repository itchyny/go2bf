package main

// Fibonacci numbers using uint32.
// Prints fib(1) through fib(47); fib(47) = 2971215073.
func main() {
	var a uint32 = uint32(0)
	var b uint32 = uint32(1)
	for i := byte(1); i <= 47; i++ {
		c := a + b
		a = b
		b = c
		print("fib(")
		print(i)
		print(") = ")
		println(a)
	}
}
