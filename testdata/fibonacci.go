package main

func fib(n byte) byte {
	if n <= 1 {
		return n
	}
	return fib(n-1) + fib(n-2)
}

func main() {
	for i := byte(1); i <= 10; i++ {
		print("fib(")
		print(i)
		print(") = ")
		println(fib(i))
	}
}
