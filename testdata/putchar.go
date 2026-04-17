package main

import "os"

func putchar(c byte) {
	os.Stdout.Write([]byte{c})
}
