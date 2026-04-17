package main

func main() {
	for i := byte(1); i <= 5; i++ {
		print(i)
		print("! = ")
		println(factorial(i))
	}
}

func factorial(n byte) byte {
	if n <= 1 {
		return 1
	}
	return n * factorial(n-1)
}
