package main

func ackermann(m, n byte) byte {
	if m == 0 {
		return n + 1
	}
	if n == 0 {
		return ackermann(m-1, 1)
	}
	return ackermann(m-1, ackermann(m, n-1))
}

func main() {
	for m := byte(0); m <= 3; m++ {
		for n := byte(0); n <= 3; n++ {
			if n > 0 {
				print(" ")
			}
			print(ackermann(m, n))
		}
		println()
	}
}
