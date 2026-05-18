package main

func main() {
	var cnt byte
	for n, w := uint16(2), 0; n < 1000; n++ {
		prime := 1
		for d := uint16(2); d*d <= n; d++ {
			if n%d == 0 {
				prime = 0
				break
			}
		}
		if prime > 0 {
			cnt++
			if w >= 70 {
				print("\n")
				w = 0
			} else if n > 2 {
				print(" ")
				w++
			}
			print(n)
			for m := n; m > 0; m /= 10 {
				w++
			}
		}
	}
	println()
	println("Found", cnt, "primes!")
}
