package main

// Sieve of Eratosthenes using a slice.
func main() {
	var r []byte
	for i := byte(2); i <= 50; i++ {
		prime := byte(1)
		for j := byte(2); j*j <= i; j++ {
			if i%j == 0 {
				prime = 0
				break
			}
		}
		if prime != 0 {
			r = append(r, i)
		}
	}
	for i, v := range r {
		if i > 0 {
			print(" ")
		}
		print(v)
	}
	println()
}
