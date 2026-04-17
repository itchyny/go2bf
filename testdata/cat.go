package main

func main() {
	for {
		c := getchar()
		if c == 0 {
			return
		}
		putchar(c)
	}
}
