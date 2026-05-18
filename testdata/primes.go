package main

func main() {
	for n := 2; n <= 100; n++ {
		prime := 1
		for d := 2; d*d <= n; d++ {
			if n%d == 0 {
				prime = 0
				break
			}
		}
		if prime > 0 {
			if n > 2 {
				print(" ")
			}
			print(n)
		}
	}
	println()
}
