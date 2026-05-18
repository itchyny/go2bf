package main

// Fibonacci numbers using uint32.
// Prints fib(1) through fib(47); fib(47) = 2971215073.
func main() {
	a, b := uint32(0), uint32(1)
	for i := 1; i <= 47; i++ {
		a, b = b, a+b
		print("fib(")
		print(i)
		print(") = ")
		println(a)
	}
}
