package main

func main() {
	a := [16]byte{6, 2, 8, 5, 10, 11, 12, 0, 15, 4, 13, 14, 7, 1, 9, 3}

	print("input: ")
	for i := range len(a) {
		if i > 0 {
			print(" ")
		}
		print(a[i])
	}
	println()

	// Iterative quicksort using a manual stack.
	var slo [16]byte
	var shi [16]byte
	sp := 0
	slo[sp] = 0
	shi[sp] = byte(len(a)) - 1
	sp++

	for sp > 0 {
		sp--
		lo := slo[sp]
		hi := shi[sp]
		if lo >= hi {
			continue
		}

		pivot := a[hi]
		print("  sort [")
		printnum(lo)
		print("..")
		printnum(hi)
		print("] pivot=")
		printnum(pivot)

		i := lo
		for j := lo; j < hi; j++ {
			if a[j] < pivot {
				a[i], a[j] = a[j], a[i]
				i++
			}
		}
		a[i], a[hi] = a[hi], a[i]

		print(" -> ")
		for k := range len(a) {
			if k > 0 {
				print(" ")
			}
			printnum(a[k])
		}
		println()

		if i > 0 && lo < i-1 {
			slo[sp] = lo
			shi[sp] = i - 1
			sp++
		}
		if i+1 < hi {
			slo[sp] = i + 1
			shi[sp] = hi
			sp++
		}
	}

	print("result: ")
	for i := range len(a) {
		if i > 0 {
			print(" ")
		}
		print(a[i])
	}
	println()
}

func printnum(n byte) {
	if n < 10 {
		print(" ")
	}
	print(n)
}
