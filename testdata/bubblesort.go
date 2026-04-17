package main

func main() {
	a := [5]byte{'5', '3', '1', '4', '2'}

	// Bubble sort
	for range len(a) {
		for i := range len(a) - 1 {
			if a[i] > a[i+1] {
				a[i], a[i+1] = a[i+1], a[i]
			}
		}
	}

	for i := range len(a) {
		if i > 0 {
			print(" ")
		}
		print(string(a[i]))
	}
	println()
}
