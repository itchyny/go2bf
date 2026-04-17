package main

import "os"

func getchar() byte {
	var buf [1]byte
	n, _ := os.Stdin.Read(buf[:])
	if n == 0 {
		return 0
	}
	return buf[0]
}
