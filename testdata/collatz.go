package main

func collatz(n byte) byte {
	if n <= 1 {
		return 0
	}
	if n%2 == 0 {
		return collatz(n/2) + 1
	}
	return collatz(n*3+1) + 1
}

func main() {
	for i := byte(1); i <= 20; i++ {
		print(i)
		print(": ")
		println(collatz(i))
	}
}
