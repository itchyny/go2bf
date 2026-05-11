package main

// Pascal's triangle via the recursive binomial coefficient.
func choose(n, k byte) uint16 {
	if k == 0 || k == n {
		return uint16(1)
	}
	return choose(n-1, k-1) + choose(n-1, k)
}

func main() {
	for n := byte(0); n <= 12; n++ {
		for k := byte(0); k <= n; k++ {
			if k > 0 {
				print(" ")
			}
			print(choose(n, k))
		}
		println()
	}
}
