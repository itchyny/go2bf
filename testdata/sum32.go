package main

// Sum of 1..1000 using uint32, printing the result.
func main() {
	var sum uint32 = uint32(0)
	for i := uint16(1); i <= 1000; i++ {
		sum += uint32(i)
	}
	println(sum)
}
