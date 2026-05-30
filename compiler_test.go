package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompile(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		input  string
		output string
	}{
		// --- putchar getchar ---
		{
			"putchar",
			`package main
func main() { putchar(72) }`,
			"", "H",
		},
		{
			"multiple putchar",
			`package main
func main() { putchar(72); putchar(105); putchar(10) }`,
			"", "Hi\n",
		},
		{
			"putchar rune literal",
			`package main
func main() { putchar('H'); putchar('i'); putchar('!') }`,
			"", "Hi!",
		},
		{
			"putchar variable",
			`package main
func main() {
	x := byte(72)
	putchar(x)
}`,
			"", "H",
		},
		{
			"putchar byte struct field",
			`package main
type P struct{ n uint16; tag byte }
func main() {
	p := P{n: 100, tag: 65}
	putchar(p.tag)
}`,
			"", "A",
		},
		{
			"getchar",
			`package main
func main() {
	c := getchar()
	putchar(c + 1)
}`,
			"A", "B",
		},
		{
			"getchar eof",
			`package main
func main() {
	c := getchar()
	putchar(c + 1)
}`,
			"", "\x01",
		},
		{
			"getchar in expression",
			`package main
func main() { putchar(getchar() + 1) }`,
			"A", "B",
		},
		// --- Builtins: print println ---
		{
			"print string",
			`package main
func main() { print("Hello") }`,
			"", "Hello",
		},
		{
			"print byte",
			`package main
func main() {
	x := byte(42)
	print(x)
}`,
			"", "42",
		},
		{
			"print escape sequences",
			`package main
func main() { print("a\tb\nc") }`,
			"", "a\tb\nc",
		},
		{
			"println string",
			`package main
func main() { println("Hi") }`,
			"", "Hi\n",
		},
		{
			"println byte",
			`package main
func main() {
	println(0)
	println(7)
	println(42)
	println(100)
	println(255)
}`,
			"", "0\n7\n42\n100\n255\n",
		},
		{
			"println mixed",
			`package main
func main() { x := 65; println("x =", x) }`,
			"", "x = 65\n",
		},
		{
			"println no args",
			`package main
func main() { println() }`,
			"", "\n",
		},
		{
			"println multiple bytes",
			`package main
func main() { println(1, 2, 3) }`,
			"", "1 2 3\n",
		},
		{
			"println multi-return function",
			`package main
func divmod(a, b byte) (byte, byte) { return a / b, a % b }
func main() { println(divmod(72, 10)) }`,
			"", "7 2\n",
		},
		{
			"println byte value",
			`package main
func main() {
	println(byte(42))
}`,
			"", "42\n",
		},
		{
			"print string and byte",
			`package main
func main() {
	print("x=")
	println(byte(42))
}`,
			"", "x=42\n",
		},
		// --- Builtins: min max panic ---
		{
			"min two args",
			`package main
func main() { print(min(byte(5), byte(3))) }`,
			"", "3",
		},
		{
			"max two args",
			`package main
func main() { print(max(byte(5), byte(3))) }`,
			"", "5",
		},
		{
			"min three args",
			`package main
func main() { print(min(byte(10), byte(20), byte(5))) }`,
			"", "5",
		},
		{
			"max three args",
			`package main
func main() { print(max(byte(10), byte(20), byte(5))) }`,
			"", "20",
		},
		{
			"min equal values",
			`package main
func main() { print(min(byte(7), byte(7))) }`,
			"", "7",
		},
		{
			"min uint16",
			`package main
func main() {
	a, b := uint16(300), uint16(500)
	m := min(a, b)
	print(m)
}`,
			"", "300",
		},
		{
			"max uint32",
			`package main
func main() {
	print(max(uint32(50000), uint32(80000)))
}`,
			"", "80000",
		},
		{
			"max uint16 via function",
			`package main
func g(a, b uint16) uint16 { return max(a, b) }
func main() { print(g(300, 500)) }`,
			"", "500",
		},
		{
			"min uint32",
			`package main
func main() { print(min(70000, 80000)) }`,
			"", "70000",
		},
		{
			"min max with variables",
			`package main
func main() {
	a, b := byte(10), byte(3)
	println(min(a, b), max(a, b))
}`,
			"", "3 10\n",
		},
		// --- If statement ---
		{
			"if true",
			`package main
func main() {
	x := 1
	if x != 0 {
		putchar(72)
	}
}`,
			"", "H",
		},
		{
			"if false",
			`package main
func main() {
	x := 0
	if x != 0 {
		putchar(72)
	}
	putchar(33)
}`,
			"", "!",
		},
		{
			"if else true",
			`package main
func main() {
	x := 1
	if x != 0 {
		putchar(89)
	} else {
		putchar(78)
	}
}`,
			"", "Y",
		},
		{
			"if else false",
			`package main
func main() {
	x := 0
	if x != 0 {
		putchar(89)
	} else {
		putchar(78)
	}
}`,
			"", "N",
		},
		{
			"nested if",
			`package main
func main() {
	x := 3
	if x > 1 {
		if x < 5 {
			putchar(89)
		} else {
			putchar(78)
		}
	}
}`,
			"", "Y",
		},
		{
			"else if chain",
			`package main
func main() {
	x := 2
	if x == 1 {
		putchar('A')
	} else if x == 2 {
		putchar('B')
	} else if x == 3 {
		putchar('C')
	} else {
		putchar('D')
	}
}`,
			"", "B",
		},
		{
			"if with init",
			`package main
func main() {
	for i := byte(0); i < 3; i++ {
		if x := i * 2; x > 2 {
			putchar('Y')
		} else {
			putchar('N')
		}
	}
}`,
			"", "NNY",
		},
		{
			"nested if else if",
			`package main
func main() {
	for i := byte(0); i < 4; i++ {
		if i == 0 {
			putchar('A')
		} else if i == 1 {
			putchar('B')
		} else if i == 2 {
			putchar('C')
		} else {
			putchar('D')
		}
	}
}`,
			"", "ABCD",
		},
		{
			"if with init statement",
			`package main
func main() {
	if x := byte(5); x > 3 {
		println(x)
	}
}`,
			"", "5\n",
		},
		{
			"if branch declares same name with wider type",
			`package main
func main() {
	x := byte(1)
	if x == 1 {
		x := uint16(50000)
		println(x)
	} else {
		x := []byte{9, 9}
		println(len(x))
	}
	println(x)
}`,
			"", "50000\n1\n",
		},
		{
			"else-if branches each declare same name with different kinds",
			`package main
func main() {
	x := byte(2)
	if x == 1 {
		x := byte(11)
		print(x)
	} else if x == 2 {
		x := uint16(42000)
		print(x)
	} else if x == 3 {
		x := []byte{9, 9, 9}
		print(len(x))
	} else {
		x := byte(99)
		print(x)
	}
	print("/")
	println(x)
}`,
			"", "42000/2\n",
		},
		// --- For loops ---
		{
			"for loop countdown",
			`package main
func main() {
	i := 5
	for i > 0 {
		putchar(48 + i)
		i--
	}
}`,
			"", "54321",
		},
		{
			"for with condition",
			`package main
func main() {
	i := 0
	for i < 5 {
		putchar(48 + i)
		i++
	}
}`,
			"", "01234",
		},
		{
			"for c-style",
			`package main
func main() {
	for i := 1; i <= 5; i++ {
		putchar(48 + i)
	}
}`,
			"", "12345",
		},
		{
			"nested for",
			`package main
func main() {
	for i := 1; i <= 3; i++ {
		for j := 1; j <= 3; j++ {
			putchar(48 + i*j)
		}
	}
}`,
			"", "123246369",
		},
		{
			"for range byte",
			`package main
func main() {
	for i := range 5 {
		putchar(48 + i)
	}
}`,
			"", "01234",
		},
		{
			"for range no var",
			`package main
func main() {
	for range 3 {
		putchar('*')
	}
}`,
			"", "***",
		},
		{
			"for condition only",
			`package main
func main() {
	i := byte(0)
	for i < 5 {
		print(i)
		i++
	}
}`,
			"", "01234",
		},
		{
			"for post decrement",
			`package main
func main() {
	for i := byte(5); i > 0; i-- {
		putchar(48 + i)
	}
}`,
			"", "54321",
		},
		{
			"for-loop body declares same name as init with wider type",
			`package main
func main() {
	for i := byte(0); i < 3; i++ {
		if i > 0 { print(" ") }
		i := uint16(i) * 200
		print(i)
	}
}`,
			"", "0 200 400",
		},
		{
			"for-range body declares same name as range variable",
			`package main
func main() {
	v := byte(99)
	for _, v := range []byte{1, 2, 3} {
		v := uint16(v) * 100
		print(v)
		print(" ")
	}
	println(v)
}`,
			"", "100 200 300 99\n",
		},
		{
			"shadowing := with self-reference reads outer",
			`package main
func main() {
	v := byte(99)
	for _, v := range []byte{1, 2, 3} {
		v := uint16(v) * 100
		print(v)
		print(" ")
	}
	println(v)
}`,
			"", "100 200 300 99\n",
		},
		{
			"range over slice literal of structs",
			`package main
type Pt struct{ x, y byte }
func main() {
	for _, p := range []Pt{Pt{1, 2}, Pt{3, 4}, Pt{5, 6}} {
		println(p.x, p.y)
	}
}`,
			"", "1 2\n3 4\n5 6\n",
		},
		{
			"nested for-loops shadow same iter variable",
			`package main
func main() {
	for i := byte(0); i < 2; i++ {
		for i := byte(10); i < 12; i++ {
			print(i)
			print(" ")
		}
		print("/")
	}
	println()
}`,
			"", "10 11 /10 11 /\n",
		},
		// --- Break/Continue ---
		{
			"for range continue",
			`package main
func main() {
	for i := range 6 {
		if i%2 == 0 { continue }
		putchar(48 + i)
	}
}`,
			"", "135",
		},
		{
			"for range break",
			`package main
func main() {
	for i := range 10 {
		if i == 5 { break }
		putchar(48 + i)
	}
}`,
			"", "01234",
		},
		{
			"break",
			`package main
func main() {
	for i := 0; i < 10; i++ {
		if i == 5 { break }
		putchar(48 + i)
	}
}`,
			"", "01234",
		},
		{
			"continue",
			`package main
func main() {
	for i := 0; i < 10; i++ {
		if i%2 == 0 { continue }
		putchar(48 + i)
	}
}`,
			"", "13579",
		},
		{
			"break in infinite loop",
			`package main
func main() {
	i := 0
	for {
		if i >= 5 { break }
		putchar(48 + i)
		i++
	}
}`,
			"", "01234",
		},
		{
			"nested break",
			`package main
func main() {
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			if j == 2 { break }
			putchar(48 + i*3 + j)
		}
	}
}`,
			"", "013467",
		},
		{
			"labeled break",
			`package main
func main() {
outer:
	for i := byte(0); i < 4; i++ {
		for j := byte(0); j < 4; j++ {
			if i*j > 3 { break outer }
			print(i, j)
		}
	}
}`,
			"", "00010203101112132021",
		},
		{
			"labeled continue",
			`package main
func main() {
outer:
	for i := byte(0); i < 4; i++ {
		for j := byte(0); j < 4; j++ {
			if j == 2 { continue outer }
			print(i, j)
		}
	}
}`,
			"", "0001101120213031",
		},
		// --- Return ---
		{
			"return in main",
			`package main
func main() {
	putchar('A')
	return
	putchar('B')
}`,
			"", "A",
		},
		{
			"return in main loop",
			`package main
func main() {
	for {
		c := getchar()
		if c == 0 { return }
		putchar(c)
	}
}`,
			"Hi", "Hi",
		},
		{
			"return with divmod after return",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	q := n / 10
	r := n % 10
	return q + r
}
func main() { print(f(42)) }`,
			"", "6",
		},
		{
			"selector on function result",
			`package main
type P struct { x byte; y byte }
func makePoint(a, b byte) P { return P{x: a, y: b} }
func main() {
	p := makePoint(3, 7)
	print(p.x + p.y)
}`,
			"", "10",
		},
		{
			"method call as expression",
			`package main
type P struct { x byte; y byte }
func (p P) sum() byte { return p.x + p.y }
func main() {
	p := P{x: 5, y: 3}
	print(p.sum() + 1)
}`,
			"", "9",
		},
		// --- Switch statement ---
		{
			"switch in loop",
			`package main
func main() {
	for i := 1; i <= 4; i++ {
		switch i {
		case 1:
			putchar('A')
		case 2:
			putchar('B')
		case 3:
			putchar('C')
		default:
			putchar('Z')
		}
	}
}`,
			"", "ABCZ",
		},
		{
			"switch multiple values",
			`package main
func main() {
	for i := 1; i <= 6; i++ {
		switch i {
		case 1, 3, 5:
			putchar('O')
		case 2, 4, 6:
			putchar('E')
		}
	}
}`,
			"", "OEOEOE",
		},
		{
			"switch bool",
			`package main
func main() {
	for i := 1; i <= 5; i++ {
		switch {
		case i <= 2:
			putchar('A')
		case i <= 4:
			putchar('B')
		default:
			putchar('C')
		}
	}
}`,
			"", "AABBC",
		},
		{
			"switch fallthrough",
			`package main
func main() {
	for i := range 4 {
		switch i {
		case 0:
			putchar('A')
			fallthrough
		case 1:
			putchar('B')
		case 2:
			putchar('C')
			fallthrough
		default:
			putchar('D')
		}
		putchar(' ')
	}
}`,
			"", "AB B CD D ",
		},
		{
			"switch default fallthrough",
			`package main
func main() {
	for i := byte(0); i < 4; i++ {
		switch i {
		case 0:
			putchar('A')
		case 1:
			putchar('B')
			fallthrough
		case 2:
			putchar('C')
		default:
			putchar('Z')
		}
	}
}`,
			"", "ABCCZ",
		},
		{
			"switch chained fallthrough",
			`package main
func main() {
	for i := byte(1); i <= 3; i++ {
		switch i {
		case 1:
			putchar('A')
			fallthrough
		case 2:
			putchar('B')
			fallthrough
		case 3:
			putchar('C')
		}
		putchar(' ')
	}
}`,
			"", "ABC BC C ",
		},
		{
			"switch no default",
			`package main
func main() {
	for i := byte(0); i < 4; i++ {
		switch i {
		case 1:
			putchar('A')
		case 2:
			putchar('B')
		}
	}
}`,
			"", "AB",
		},
		{
			"empty switch",
			`package main
func main() {
	switch byte(1) {
	}
	putchar('A')
}`,
			"", "A",
		},
		{
			"switch only default",
			`package main
func main() {
	switch byte(1) {
	default:
		putchar('Z')
	}
}`,
			"", "Z",
		},
		{
			"fallthrough empty case",
			`package main
func main() {
	x := byte(1)
	switch x {
	case 1:
		fallthrough
	case 2:
		putchar('B')
	}
}`,
			"", "B",
		},
		{
			"fallthrough in default",
			`package main
func main() {
	x := byte(5)
	switch x {
	case 1:
		putchar('A')
	default:
		putchar('Z')
		fallthrough
	case 2:
		putchar('B')
	}
}`,
			"", "ZB",
		},
		{
			"switch on function call",
			`package main
func f() byte { return 3 }
func main() {
	switch f() {
	case 1:
		print(1)
	case 3:
		print(3)
	default:
		print(0)
	}
}`,
			"", "3",
		},
		{
			"switch fallthrough to default",
			`package main
func main() {
	x := byte(1)
	switch x {
	case 1:
		putchar('A')
		fallthrough
	default:
		putchar('B')
	}
}`,
			"", "AB",
		},
		{
			"switch case declares same name with wider type",
			`package main
func main() {
	x := byte(7)
	switch x {
	case 7:
		x := uint16(40000)
		println(x)
	default:
		x := []byte{1}
		println(len(x))
	}
	println(x)
}`,
			"", "40000\n7\n",
		},
		{
			"switch init declares same name as outer slice",
			`package main
func main() {
	x := []byte{77, 88, 99}
	switch x := byte(2); x {
	case 1:
		println("one")
	case 2:
		println("two")
	default:
		println("other")
	}
	println(len(x))
}`,
			"", "two\n3\n",
		},
		// --- Goto ---
		{
			"backward goto",
			`package main
func main() {
	i := byte(0)
loop:
	if i < 5 {
		print(i)
		i++
		goto loop
	}
}`,
			"", "01234",
		},
		{
			"forward goto",
			`package main
func main() {
	x := byte(5)
	if x > 3 {
		goto big
	}
	print("S")
big:
	print("B")
}`,
			"", "B",
		},
		{
			"goto cleanup pattern",
			`package main
func main() {
	x := byte(0)
	if x == 0 { goto cleanup }
	print("A")
cleanup:
	print("C")
}`,
			"", "C",
		},
		{
			"goto out of nested loops",
			`package main
func main() {
	for i := byte(0); i < 5; i++ {
		for j := byte(0); j < 5; j++ {
			print(i, j)
			if i == 1 && j == 2 { goto done }
		}
	}
done:
	print("!")
}`,
			"", "0001020304101112!",
		},
		{
			"goto in non-main function",
			`package main
func count(n byte) byte {
	x := byte(0)
loop:
	if x < n {
		x++
		goto loop
	}
	return x
}
func main() { print(count(7)) }`,
			"", "7",
		},
		// --- Arithmetic operators ---
		{
			"addition",
			`package main
func main() {
	x := 60
	y := 12
	putchar(x + y)
}`,
			"", "H",
		},
		{
			"subtraction",
			`package main
func main() {
	x := 80
	y := 8
	putchar(x - y)
}`,
			"", "H",
		},
		{
			"multiplication",
			`package main
func main() {
	x := 9
	y := 8
	putchar(x * y)
}`,
			"", "H",
		},
		{
			"division",
			`package main
func main() {
	x := 216
	y := 3
	putchar(x / y)
}`,
			"", "H",
		},
		{
			"modulo",
			`package main
func main() {
	putchar(100 % 10 + 72)
}`,
			"", "H",
		},
		{
			"increment decrement",
			`package main
func main() {
	x := 71
	x++
	putchar(x)
	x--
	x--
	putchar(x)
}`,
			"", "HF",
		},
		{
			"complex expression",
			`package main
func main() {
	x := 2
	y := 3
	z := 10
	putchar(48 + x*y + z/5)
}`,
			"", "8",
		},
		{
			"byte wrapping add",
			`package main
func main() {
	x := 200
	y := 72
	putchar(x + y)
}`,
			"", "\x10", // 272 mod 256 = 16
		},
		{
			"byte wrapping sub",
			`package main
func main() {
	x := 0
	y := 1
	putchar(x - y)
}`,
			"", "\xff", // 0 - 1 = 255
		},
		{
			"add dst equals src2",
			`package main
func f(a byte, b byte) byte { return a + b }
func main() { putchar(f(20, 52)) }`,
			"", "H", // 20 + 52 = 72 = 'H'
		},
		{
			"sub large",
			`package main
func main() {
	x := byte(100)
	y := byte(28)
	putchar(x - y)
}`,
			"", "H",
		},
		{
			"assignment operators",
			`package main
func main() {
	x := byte(10)
	println(x)
	x += 5
	println(x)
	x -= 3
	println(x)
	x *= 2
	println(x)
	x /= 6
	println(x)
	x %= 3
	println(x)
}`,
			"", "10\n15\n12\n24\n4\n1\n",
		},
		{
			"add assign to same var",
			`package main
func main() {
	a := byte(3)
	b := byte(4)
	b = a + b
	println(b)
}`,
			"", "7\n",
		},
		{
			"sub assign to same var",
			`package main
func main() {
	a := byte(10)
	b := byte(3)
	a = a - b
	println(a)
}`,
			"", "7\n",
		},
		{
			"add to self",
			`package main
func main() {
	x := byte(7)
	x = x + x
	println(x)
}`,
			"", "14\n",
		},
		// --- Div/mod fusion ---
		{
			"div then mod fusion",
			`package main
func main() {
	x := byte(17)
	q := x / 5
	r := x % 5
	print(q)
	print(r)
}`,
			"", "32", // 17/5=3, 17%5=2
		},
		{
			"mod then div fusion",
			`package main
func main() {
	x := byte(17)
	r := x % 5
	q := x / 5
	print(r)
	print(q)
}`,
			"", "23", // 17%5=2, 17/5=3
		},
		{
			"div mod in loop",
			`package main
func main() {
	for i := byte(10); i <= 12; i++ {
		if i > 10 { print(" ") }
		q := i / 5
		r := i % 5
		print(q)
		print(r)
	}
}`,
			"", "20 21 22",
		},
		{
			"div mod with break",
			`package main
func f() byte {
	for i := byte(10); i < 20; i++ {
		q := i / 7
		r := i % 7
		if r == 0 {
			return q
		}
	}
	return 0
}
func main() { print(f()) }`,
			"", "2", // 14/7=2, 14%7=0
		},
		{
			"div mod different divisors",
			`package main
func main() {
	x := byte(17)
	q := x / 5
	r := x % 3
	print(q)
	print(r)
}`,
			"", "32", // 17/5=3, 17%3=2
		},
		{
			"div mod call operands",
			`package main
func d() byte { putchar('.'); return 5 }
func main() {
	x := byte(17)
	q := x / d()
	r := x % d()
	print(q)
	print(r)
}`,
			"", "..32", // d() called twice (not fused)
		},
		{
			"div mod assign existing",
			`package main
func main() {
	x := byte(17)
	var q byte
	var r byte
	q = x / 5
	r = x % 5
	print(q)
	print(r)
}`,
			"", "32",
		},
		{
			"return mod div fused",
			`package main
func moddiv(a, b byte) (byte, byte) {
	return a % b, a / b
}
func main() {
	r, q := moddiv(17, 5)
	print(r)
	print(q)
}`,
			"", "23",
		},
		{
			"return div mod different divisors",
			`package main
func f(a, b, c byte) (byte, byte) {
	return a / b, a % c
}
func main() {
	q, r := f(17, 5, 3)
	print(q)
	print(r)
}`,
			"", "32", // 17/5=3, 17%3=2
		},
		{
			"div mod guarded define",
			`package main
func main() {
	b := byte(5)
	if b != 0 {
		q := byte(17) / b
		r := byte(17) % b
		print(q)
		print(r)
	}
}`,
			"", "32",
		},
		// --- Unary operators ---
		{
			"unary negation",
			`package main
func main() {
	v := byte(200)
	print(-v)
}`,
			"", "56",
		},
		// --- Comparison operators ---
		{
			"comparison eq",
			`package main
func main() {
	x := 3
	y := 3
	if x == y { putchar(89) } else { putchar(78) }
}`,
			"", "Y",
		},
		{
			"comparison neq",
			`package main
func main() {
	x := 3
	y := 4
	if x != y { putchar(89) } else { putchar(78) }
}`,
			"", "Y",
		},
		{
			"comparison lt",
			`package main
func main() {
	x := 3
	y := 5
	if x < y { putchar(89) } else { putchar(78) }
}`,
			"", "Y",
		},
		{
			"comparison gt",
			`package main
func main() {
	x := 5
	y := 3
	if x > y { putchar(89) } else { putchar(78) }
}`,
			"", "Y",
		},
		{
			"comparison not lt",
			`package main
func main() {
	x := 5
	y := 3
	if x < y { putchar(89) } else { putchar(78) }
}`,
			"", "N",
		},
		{
			"comparison le true",
			`package main
func main() {
	if 3 <= 3 { putchar(89) } else { putchar(78) }
}`,
			"", "Y",
		},
		{
			"comparison le false",
			`package main
func main() {
	if 5 <= 3 { putchar(89) } else { putchar(78) }
}`,
			"", "N",
		},
		{
			"comparison ge true",
			`package main
func main() {
	if 3 >= 3 { putchar(89) } else { putchar(78) }
}`,
			"", "Y",
		},
		{
			"comparison ge false",
			`package main
func main() {
	if 1 >= 3 { putchar(89) } else { putchar(78) }
}`,
			"", "N",
		},
		{
			"comparison eq false",
			`package main
func main() {
	if 3 == 4 { putchar(89) } else { putchar(78) }
}`,
			"", "N",
		},
		{
			"comparison neq false",
			`package main
func main() {
	if 3 != 3 { putchar(89) } else { putchar(78) }
}`,
			"", "N",
		},
		// --- Logical operators ---
		{
			"logical not",
			`package main
func main() {
	x := 0 != 0
	if !x { putchar(89) } else { putchar(78) }
}`,
			"", "Y",
		},
		{
			"logical and",
			`package main
func main() {
	a := 1 != 0
	b := 1 != 0
	if a && b { putchar(89) } else { putchar(78) }
}`,
			"", "Y",
		},
		{
			"logical and false",
			`package main
func main() {
	a := 1 != 0
	b := 0 != 0
	if a && b { putchar(89) } else { putchar(78) }
}`,
			"", "N",
		},
		{
			"logical or",
			`package main
func main() {
	a := 0 != 0
	b := 1 != 0
	if a || b { putchar(89) } else { putchar(78) }
}`,
			"", "Y",
		},
		{
			"logical not in expression",
			`package main
func f(x byte) byte {
	if !(x != 0) { return 1 }
	return 0
}
func main() {
	putchar(48 + f(0))
	putchar(48 + f(1))
	putchar(48 + f(5))
}`,
			"", "100",
		},
		{
			"logical and short circuit",
			`package main
func f() byte { putchar('.'); return 5 }
func main() {
	x := byte(0)
	if x > 3 && x < f() {
		putchar('Y')
	} else {
		putchar('N')
	}
}`,
			"", "N",
		},
		{
			"logical or short circuit",
			`package main
func f() byte { putchar('.'); return 1 }
func main() {
	x := byte(5)
	if x > 3 || x == f() {
		putchar('Y')
	} else {
		putchar('N')
	}
}`,
			"", "Y",
		},
		// --- Bitwise operators ---
		{
			"bitwise and",
			`package main
func main() {
	for i := byte(0); i < 4; i++ {
		for j := byte(0); j < 4; j++ {
			print(i & j)
		}
	}
	x := byte(0x0F)
	x &= 0x03
	print(" ")
	print(x)
}`,
			"", "0000010100220123 3",
		},
		{
			"bitwise or",
			`package main
func main() {
	for i := byte(0); i < 4; i++ {
		for j := byte(0); j < 4; j++ {
			print(i | j)
		}
	}
	x := byte(0x03)
	x |= 0xF0
	print(" ")
	print(x)
}`,
			"", "0123113323233333 243",
		},
		{
			"bitwise xor",
			`package main
func main() {
	for i := byte(0); i < 4; i++ {
		for j := byte(0); j < 4; j++ {
			print(i ^ j)
		}
	}
}`,
			"", "0123103223013210",
		},
		{
			"bitwise complement",
			`package main
func main() { println(^byte(0x0F)) }`,
			"", "240\n",
		},
		{
			"bit clear",
			`package main
func main() {
	x := byte(0xFF)
	x &^= 0x0F
	println(x)
}`,
			"", "240\n",
		},
		{
			"shift operators",
			`package main
func main() {
	for i := byte(0); i < 8; i++ {
		if i > 0 { print(" ") }
		print(byte(5) << i)
	}
	println()
	for i := byte(0); i < 8; i++ {
		if i > 0 { print(" ") }
		print(byte(172) >> i)
	}
	println()
}`,
			"", "5 10 20 40 80 160 64 128\n172 86 43 21 10 5 2 1\n",
		},
		{
			"shift assign then print",
			`package main
func main() {
	a := byte(1) << 4
	b := byte(16) >> 4
	println(a, b)
}`,
			"", "16 1\n",
		},
		{
			"xor swap",
			`package main
func main() {
	a := byte(42)
	b := byte(99)
	println(a, b)
	a ^= b
	b ^= a
	a ^= b
	println(a, b)
}`,
			"", "42 99\n99 42\n",
		},
		{
			"compound bitwise nibble extract",
			`package main
func main() {
	x := byte(0xAB)
	println(x & 0x0F, (x >> 4) & 0x0F, ^x & 0x0F)
}`,
			"", "11 10 4\n",
		},
		// --- Functions ---
		{
			"simple function",
			`package main
func double(x byte) byte { return x + x }
func main() { print(double(5)) }`,
			"", "10",
		},
		{
			"function no return",
			`package main
func greet() { putchar(72); putchar(105) }
func main() { greet() }`,
			"", "Hi",
		},
		{
			"function with locals",
			`package main
func add3(a, b, c byte) byte {
	sum := a + b
	return sum + c
}
func main() { print(add3(1, 2, 3)) }`,
			"", "6",
		},
		{
			"function early return",
			`package main
func clamp(x byte) byte {
	if x > 9 { return 9 }
	return x
}
func main() {
	print(clamp(5))
	print(clamp(15))
}`,
			"", "59",
		},
		{
			"multiple return values",
			`package main
func swap(a, b byte) (byte, byte) {
	return b, a
}
func main() {
	x, y := swap(75, 79)
	putchar(x)
	putchar(y)
}`,
			"", "OK",
		},
		{
			"multiple return divmod",
			`package main
func divmod(a byte, b byte) (byte, byte) {
	return a / b, a % b
}
func main() {
	q, r := divmod(72, 10)
	println(q, r)
}`,
			"", "7 2\n",
		},
		{
			"multi-return into multi-byte int array element",
			`package main
func wide() (uint16, byte) { return 50000, 7 }
func main() {
	var a [3]uint16
	var b [3]byte
	a[0], b[0] = wide()
	println(a[0], b[0])
}`,
			"", "50000 7\n",
		},
		{
			"nested function calls",
			`package main
func inc(x byte) byte { return x + 1 }
func double(x byte) byte { return x + x }
func main() { print(double(inc(2))) }`,
			"", "6",
		},
		{
			"function called multiple times",
			`package main
func digit(n byte) { print(n) }
func main() { digit(1); digit(2); digit(3) }`,
			"", "123",
		},
		{
			"named return value",
			`package main
func f() (x byte) {
	x = 42
	return
}
func main() { println(f()) }`,
			"", "42\n",
		},
		{
			"named return explicit",
			`package main
func f() (r byte) {
	r = 42
	return r
}
func main() { println(f()) }`,
			"", "42\n",
		},
		{
			"multiple named returns",
			`package main
func divmod(a, b byte) (q byte, r byte) {
	q = a / b
	r = a % b
	return
}
func main() {
	q, r := divmod(17, 5)
	println(q, r)
}`,
			"", "3 2\n",
		},
		{
			"widen narrower integer literal to uintN function param",
			`package main
func addN(d uint32) uint32 { return d + 1 }
func twoN(a, b uint32) uint32 { return a + b }
type Counter struct{ n uint32 }
func (c *Counter) AddN(d uint32) { c.n += d }
func main() {
	println(addN(500))
	println(twoN(1000, 2000))
	c := Counter{n: 1000}
	(&c).AddN(500)
	println(c.n)
}`,
			"", "501\n3000\n1500\n",
		},
		{
			"widen narrower integer literal in mixed-width multi-return",
			`package main
func split() (byte, uint32, uint64) {
	return 5, 1000, 100000
}
func main() {
	a, b, c := split()
	println(a, b, c)
}`,
			"", "5 1000 100000\n",
		},
		{
			"uint16 of a byte-returning function call widens correctly",
			`package main
func count() byte { return 200 }
func main() { println(uint16(count())) }`,
			"", "200\n",
		},
		{
			"uint16 of various byte-source forms widens correctly",
			`package main
type R struct{ v byte }
type P struct{ x byte }
func main() {
	a := [3]byte{10, 20, 250}
	p := P{x: 100}
	s := [2]R{{v: 50}, {v: 99}}
	println(uint16(a[2]) * 2, uint16(p.x) * 3, uint16(s[1].v) * 4)
}`,
			"", "500 300 396\n",
		},
		{
			"named returns with mixed widths",
			`package main
func split(n uint16) (lo byte, hi uint16) {
	lo = byte(n)
	hi = n >> 8
	return
}
func combine(lo byte, hi uint16) (a byte, b uint16, c uint32) {
	a = lo
	b = hi
	c = uint32(hi)<<8 | uint32(lo)
	return
}
func main() {
	println(split(0xABCD))
	println(combine(0x12, uint16(0xFE)))
}`,
			"", "205 171\n18 254 65042\n",
		},
		{
			"blank identifier",
			`package main
func divmod(a, b byte) (byte, byte) { return a / b, a % b }
func main() {
	_, r := divmod(17, 5)
	println(r)
}`,
			"", "2\n",
		},
		{
			"function call as statement",
			`package main
func f(x byte) byte { return x + 1 }
func main() {
	x := f(71)
	putchar(x)
}`,
			"", "H",
		},
		{
			"struct literal as function argument",
			`package main
type Point struct { x byte; y byte }
func sum(p Point) byte { return p.x + p.y }
func main() { println(sum(Point{3, 7})) }`,
			"", "10\n",
		},
		{
			"nested struct literal as function argument",
			`package main
type Point struct { x byte; y byte }
type Rect struct { min Point; max Point }
func area(r Rect) byte { return (r.max.x - r.min.x) * (r.max.y - r.min.y) }
func main() { println(area(Rect{min: Point{1, 2}, max: Point{4, 6}})) }`,
			"", "12\n",
		},
		{
			"struct array as function parameter",
			`package main
type Point struct { x byte; y byte }
func f(a [2]Point) byte { return a[0].x + a[1].y }
func main() {
	a := [2]Point{Point{1, 2}, Point{3, 4}}
	println(f(a))
}`,
			"", "5\n",
		},
		{
			"array of arrays as function parameter",
			`package main
func f(a [2][3]byte) byte {
	s := byte(0)
	for i := range 2 { for j := range 3 { s += a[i][j] } }
	return s
}
func main() { println(f([2][3]byte{{1, 2, 3}, {4, 5, 6}})) }`,
			"", "21\n",
		},
		{
			"2d array element as function argument",
			`package main
func sum(a [3]byte) byte { return a[0] + a[1] + a[2] }
func main() {
	m := [2][3]byte{{1, 2, 3}, {4, 5, 6}}
	println(sum(m[0]), sum(m[1]))
	i := byte(1)
	println(sum(m[i]))
}`,
			"", "6 15\n15\n",
		},
		{
			"function returning array as argument",
			`package main
func makeArr(x byte) [3]byte { return [3]byte{x, x + 1, x + 2} }
func sum(a [3]byte) byte { return a[0] + a[1] + a[2] }
func main() { println(sum(makeArr(10))) }`,
			"", "33\n",
		},
		{
			"struct with array field as argument",
			`package main
type Vec struct { d [3]byte; n byte }
func sum(v Vec) byte {
	s := byte(0)
	for i := byte(0); i < v.n; i++ { s += v.d[i] }
	return s
}
func main() { println(sum(Vec{d: [3]byte{10, 20, 30}, n: 3})) }`,
			"", "60\n",
		},
		{
			"multi return divmod reversed",
			`package main
func f(a, b byte) (byte, byte) {
	return a % b, a / b
}
func main() {
	r, q := f(17, 5)
	println(q, r)
}`,
			"", "3 2\n",
		},
		{
			"multi return non-divmod",
			`package main
func f(a, b byte) (byte, byte) {
	return a + b, a - b
}
func main() {
	s, d := f(10, 3)
	println(s, d)
}`,
			"", "13 7\n",
		},
		{
			"multi return to array index",
			`package main
func minmax(a, b byte) (byte, byte) {
	if a < b { return a, b }
	return b, a
}
func main() {
	lo, hi := minmax(5, 3)
	println(lo, hi)
}`,
			"", "3 5\n",
		},
		{
			"multi return three values",
			`package main
func f(a byte) (byte, byte, byte) {
	return a, a + 1, a + 2
}
func main() {
	x, y, z := f(5)
	println(x, y, z)
}`,
			"", "5 6 7\n",
		},
		{
			"multi return spread into another call",
			`package main
func splitNum() (byte, byte) { return 17, 5 }
func divmod(x, y byte) (byte, byte) { return x / y, x % y }
func main() {
	q, r := divmod(splitNum())
	println(q, r)
}`,
			"", "3 2\n",
		},
		{
			"expression statement call",
			`package main
func emit(c byte) byte { putchar(c); return 0 }
func main() {
	emit(65)
	emit(66)
}`,
			"", "AB",
		},
		{
			"multi return to array const index",
			`package main
func divmod(a, b byte) (byte, byte) { return a / b, a % b }
func main() {
	var a [2]byte
	a[0], a[1] = divmod(17, 5)
	println(a[0], a[1])
}`,
			"", "3 2\n",
		},
		{
			"multi return to struct field",
			`package main
type P struct { x byte; y byte }
func swap(a, b byte) (byte, byte) { return b, a }
func main() {
	var p P
	p.x, p.y = swap(3, 7)
	println(p.x, p.y)
}`,
			"", "7 3\n",
		},
		{
			"multi return to chained array const index",
			`package main
func f() (byte, byte) { return 10, 20 }
func main() {
	var a [2][2]byte
	a[0][0], a[1][1] = f()
	println(a[0][0], a[1][1])
}`,
			"", "10 20\n",
		},
		{
			"multi return to array variable index",
			`package main
func f() (byte, byte) { return 5, 15 }
func main() {
	var a [3]byte
	i := byte(1)
	a[0], a[i] = f()
	println(a[0], a[1])
}`,
			"", "5 15\n",
		},
		{
			"multi return to chained array variable index",
			`package main
func f() (byte, byte) { return 10, 20 }
func main() {
	var a [2][3]byte
	j := byte(2)
	a[0][0], a[1][j] = f()
	println(a[0][0], a[1][2])
}`,
			"", "10 20\n",
		},
		{
			"multi return to struct field direct",
			`package main
type P struct { x byte; y byte }
func f() (byte, byte) { return 3, 7 }
func main() {
	var p P
	p.x, p.y = f()
	println(p.x, p.y)
}`,
			"", "3 7\n",
		},
		// --- Var declarations ---
		{
			"var declaration with init",
			`package main
func main() {
	var x byte = 72
	putchar(x)
}`,
			"", "H",
		},
		{
			"var without init",
			`package main
func main() {
	var x byte
	putchar(48 + x)
	x = 5
	putchar(48 + x)
}`,
			"", "05",
		},
		{
			"var with init value",
			`package main
func main() {
	var x byte = 72
	putchar(x)
}`,
			"", "H",
		},
		{
			"var array declaration",
			`package main
func main() {
	var a [3]byte
	a[0] = 72
	a[1] = 105
	a[2] = 33
	for i := range 3 {
		putchar(a[i])
	}
}`,
			"", "Hi!",
		},
		{
			"block scope variable",
			`package main
func main() {
	x := byte(1)
	for i := byte(0); i < 3; i++ {
		y := x + i
		putchar(48 + y)
	}
}`,
			"", "123",
		},
		{
			"parallel assignment swap",
			`package main
func main() {
	a := 75
	b := 79
	a, b = b, a
	putchar(a)
	putchar(b)
}`,
			"", "OK",
		},
		{
			"no return in main with swap and println",
			`package main
func main() {
	a := byte(3)
	b := byte(4)
	c := byte(1)
	d := byte(2)
	a, b, c, d = c, d, a, b
	println(a, b, c, d)
}`,
			"", "1 2 3 4\n",
		},
		{
			"typeless var with composite literal RHS",
			`package main
func main() {
	var x = []byte{1, 2, 3}
	var p = []uint16{100, 200, 300}
	println(len(x), x[0], x[1], x[2])
	println(len(p), p[0], p[1], p[2])
}`,
			"", "3 1 2 3\n3 100 200 300\n",
		},
		{
			"local var shadows predeclared nil",
			`package main
func main() {
	nil := 30
	if nil == 30 {
		print(nil)
	}
}`,
			"", "30",
		},
		{
			"four levels of nested shadow with different kinds",
			`package main
func main() {
	x := byte(1)
	{
		x := uint16(2)
		{
			x := uint32(3)
			{
				x := []byte{4, 5, 6}
				print(len(x))
				print(" ")
			}
			print(x)
			print(" ")
		}
		print(x)
		print(" ")
	}
	println(x)
}`,
			"", "3 3 2 1\n",
		},
		{
			"global var byte/uintN, with and without initializer",
			`package main
var a byte = 10
var b uint16 = 1000
var c uint32 = 100000
var d uint64 = 5000000000
var z byte
func main() {
	a++
	b += 7
	c *= 2
	d++
	z = 9
	println(a, b, c, d, z)
}`,
			"", "11 1007 200000 5000000001 9\n",
		},
		{
			"global var block declaration with const-expression init",
			`package main
const K = 7
var ( x uint16 = K * K + 1; y byte = K )
func main() { println(x, y) }`,
			"", "50 7\n",
		},
		{
			"global accessed from non-recursive helpers",
			`package main
var n uint16 = 100
func add(d uint16) { n += d }
func get() uint16 { return n }
func main() { add(250); add(325); print(get()) }`,
			"", "675",
		},
		{
			"init function runs before main",
			`package main
var b byte
var msg string
func init() {
	b = 32
	msg = "hi"
}
func main() {
	println(b, msg)
}`,
			"", "32 hi\n",
		},
		// --- Type conversion ---
		{
			"string conversion in print",
			`package main
func main() {
	x := byte(72)
	print(string(x))
}`,
			"", "H",
		},
		{
			"string conversion in println",
			`package main
func main() {
	println(string(65), string(66))
}`,
			"", "A B\n",
		},
		{
			"mixed string and byte in println",
			`package main
func main() {
	println(string(65), byte(66))
}`,
			"", "A 66\n",
		},
		{
			"byte conversion",
			`package main
func main() { putchar(byte(72)) }`,
			"", "H",
		},
		{
			"byte conversion variable",
			`package main
func main() {
	x := byte(65)
	putchar(x)
}`,
			"", "A",
		},
		// --- Tail recursion ---
		{
			"tail recursive factorial",
			`package main
func factorial(n, acc byte) byte {
	if n <= 1 { return acc }
	return factorial(n-1, n*acc)
}
func main() {
	putchar(factorial(5, 1))
}`,
			"", "x", // 5! = 120 = 'x'
		},
		{
			"tail recursive gcd",
			`package main
func gcd(a, b byte) byte {
	if b == 0 { return a }
	return gcd(b, a%b)
}
func main() {
	print(gcd(12, 8))
}`,
			"", "4",
		},
		{
			"tail recursive sum",
			`package main
func sum(n, acc byte) byte {
	if n == 0 { return acc }
	return sum(n-1, acc+n)
}
func main() {
	print(sum(5, 0))
}`,
			"", "15",
		},
		{
			"tail recursive calls general recursive",
			`package main
func grec(n byte) uint16 {
	if n == 0 { return 0 }
	r := grec(n - 1)
	return r + uint16(n)
}
func trec(n byte, acc uint16) uint16 {
	if n == 0 { return acc }
	return trec(n-1, acc+grec(n))
}
func main() { print(trec(3, 0)) }`,
			"", "10",
		},
		{
			"tail recursive return",
			`package main
func f(n byte, acc byte) byte {
	if n == 0 { return acc }
	return f(n-1, acc+n)
}
func main() { print(f(4, 0)) }`,
			"", "10", // 0+4+3+2+1=10
		},
		{
			"tail rec else if",
			`package main
func f(n byte, acc byte) byte {
	if n == 0 {
		return acc
	} else if n == 1 {
		return acc + 1
	} else {
		return f(n-1, acc+n)
	}
}
func main() { print(f(4, 0)) }`,
			"", "10", // f(4,0)->f(3,4)->f(2,7)->f(1,9)->9+1=10
		},
		{
			"tail rec with block",
			`package main
func f(n byte, acc byte) byte {
	if n == 0 { return acc }
	{
		return f(n-1, acc+n)
	}
}
func main() { print(f(5, 0)) }`,
			"", "15",
		},
		{
			"tail recursive call widens narrower literal arg",
			`package main
func walk(n byte, acc uint32) uint32 {
	if n == 0 { return acc }
	return walk(n - 1, 1000)
}
func main() { println(walk(3, 0)) }`,
			"", "1000\n",
		},
		// --- General recursion ---
		{
			"general rec base case",
			`package main
func f(n byte) byte {
	if n == 0 { return 42 }
	a := f(n-1)
	return a
}
func main() { putchar(f(0)) }`,
			"", "*", // 42 = '*'
		},
		{
			"general rec one level",
			`package main
func f(n byte) byte {
	if n == 0 { return 42 }
	a := f(n-1)
	return a
}
func main() { putchar(f(1)) }`,
			"", "*",
		},
		{
			"fibonacci",
			`package main
func fib(n byte) byte {
	if n <= 1 { return n }
	a := fib(n-1)
	b := fib(n-2)
	return a + b
}
func main() { print(fib(7)) }`,
			"", "13",
		},
		{
			"fibonacci inline return",
			`package main
func fib(n byte) byte {
	if n <= 1 { return n }
	return fib(n-1) + fib(n-2)
}
func main() { println(fib(7)) }`,
			"", "13\n",
		},
		{
			"general rec factorial",
			`package main
func factorial(n byte) byte {
	if n <= 1 { return 1 }
	return n * factorial(n - 1)
}
func main() { putchar(factorial(5)) }`,
			"", "x", // 5! = 120 = 'x'
		},
		{
			"general rec named return with bare return",
			`package main
func sum(n byte) (r byte) {
	if n == 0 { return }
	r = n + sum(n - 1)
	return
}
func main() { println(sum(10)) }`,
			"", "55\n",
		},
		{
			"many locals in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a0, a1, a2, a3, a4, a5, a6, a7, a8, a9 := n, n, n, n, n, n, n, n, n, n
	b0, b1, b2, b3, b4, b5, b6, b7, b8, b9 := n, n, n, n, n, n, n, n, n, n
	c0, c1, c2, c3, c4, c5, c6, c7, c8, c9 := n, n, n, n, n, n, n, n, n, n
	d0, d1, d2, d3, d4, d5, d6, d7, d8, d9 := n, n, n, n, n, n, n, n, n, n
	s := f(n - 1)
	return s + a0 + a1 + a2 + a3 + a4 + a5 + a6 + a7 + a8 + a9 +
		b0 + b1 + b2 + b3 + b4 + b5 + b6 + b7 + b8 + b9 +
		c0 + c1 + c2 + c3 + c4 + c5 + c6 + c7 + c8 + c9 +
		d0 + d1 + d2 + d3 + d4 + d5 + d6 + d7 + d8 + d9
}
func main() { print(f(2)) }`,
			"", "120", // f(0)=0; f(1)=0+1*40=40; f(2)=40+2*40=120
		},
		{
			"print in recursive function",
			`package main
func f(n byte) byte {
	print(n)
	print(" ")
	if n == 0 { return 0 }
	c := f(n - 1)
	return c + 1
}
func main() { println(f(2)) }`,
			"", "2 1 0 2\n",
		},
		{
			"large frame recursive sum",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := n
	b := n + 1
	c := n + 2
	d := n + 3
	s := f(n - 1)
	return s + a + b + c + d
}
func main() { putchar(f(3)) }`,
			"", "*", // f(3) = 42 = '*'
		},
		{
			"large frame recursive multi-call",
			`package main
func f(n byte) byte {
	if n <= 1 { return n }
	a := n
	b := n + 1
	c := n + 2
	d := n + 3
	x := f(n - 1)
	y := f(n - 2)
	return x + y + a + b + c + d
}
func main() { putchar(f(4)) }`,
			"", "G", // f(4) = 71 = 'G'
		},
		{
			"recursive inc/dec",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	n--
	s := f(n)
	s++
	return s
}
func main() { print(f(5)) }`,
			"", "5", // f(5)=5
		},
		{
			"recursive greater than",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	if a > 2 {
		return a
	} else {
		return a + 1
	}
}
func main() { print(f(5)) }`,
			"", "3", // f(0)=0, f(1)=1, f(2)=2, f(3)=3, f(4)=3, f(5)=3
		},
		{
			"recursive less equal",
			`package main
func f(n byte) byte {
	if n == 0 { return 5 }
	a := f(n - 1)
	if a <= 1 {
		return 0
	} else {
		return a - 1
	}
}
func main() { print(f(4)) }`,
			"", "1", // f(0)=5, f(1)=4, f(2)=3, f(3)=2, f(4)=1
		},
		{
			"recursive return without else",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	if a > 2 {
		return a
	}
	return a + 1
}
func main() { print(f(5)) }`,
			"", "3", // f(0)=0, f(1)=1, f(2)=2, f(3)=3, f(4)=3, f(5)=3
		},
		{
			"if-modify after recursive call",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	r := f(n - 1)
	if n != 0 {
		r = r + byte(1)
	}
	return r
}
func main() { print(f(5)) }`,
			"", "5",
		},
		{
			"if-modify uint16 result after recursive call",
			`package main
func f(n byte) uint16 {
	if n == 0 { return 0 }
	r := f(n - 1)
	x := uint16(n) * 100
	if x < 300 {
		r = r + 1
	}
	return r
}
func main() { print(f(5)) }`,
			"", "2",
		},
		{
			"recursive logical and in if",
			`package main
func f(n byte) byte {
	if n == 0 { return 1 }
	a := f(n - 1)
	if a > 0 && n > 0 {
		return a + 1
	}
	return 0
}
func main() { print(f(3)) }`,
			"", "4", // f(0)=1, f(1)=2, f(2)=3, f(3)=4
		},
		{
			"recursive logical or in if",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	if a > 3 || n > 3 {
		return a
	}
	return a + 1
}
func main() { print(f(5)) }`,
			"", "3", // f(0)=0, f(1)=1, f(2)=2, f(3)=3, f(4)=3 (3>3||4>3->T), f(5)=3
		},
		{
			"recursive logical and expr",
			`package main
func f(n byte) byte {
	if n == 0 { return 3 }
	a := f(n - 1)
	b := a > 0 && n > 0
	if b {
		return a + 1
	} else {
		return 0
	}
}
func main() { print(f(3)) }`,
			"", "6", // f(0)=3, f(1)=4, f(2)=5, f(3)=6
		},
		{
			"recursive logical or expr",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	b := a > 2 || n > 2
	if b {
		return a
	} else {
		return a + 1
	}
}
func main() { print(f(4)) }`,
			"", "2", // f(0)=0, f(1)=1, f(2)=2, f(3)=2 (2>2=F||3>2=T->a), f(4)=2
		},
		{
			"recursive unary negation",
			`package main
func f(n byte) byte {
	if n == 0 { return 1 }
	a := f(n - 1)
	return -a
}
func main() { putchar(f(2)) }`,
			"", "\x01", // f(0)=1, f(1)=255 (-1 mod 256), f(2)=1 (-255 mod 256)
		},
		{
			"recursive unary not",
			`package main
func f(n byte) byte {
	if n == 0 { return 3 }
	a := f(n - 1)
	if !( a == 0 ) {
		return a - 1
	}
	return 0
}
func main() { print(f(3)) }`,
			"", "0", // f(0)=3, f(1)=2, f(2)=1, f(3)=0
		},
		{
			"recursive if else",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	if a > 2 {
		return a
	} else {
		return a + 1
	}
}
func main() { print(f(5)) }`,
			"", "3", // f(0)=0, f(1)=1, f(2)=2, f(3)=3, f(4)=3 (>2), f(5)=3
		},
		{
			"recursive if else with modulo",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	if n%2 == 0 {
		return a + 1
	} else {
		return a + 2
	}
}
func main() { print(f(4)) }`,
			"", "6", // f(0)=0, f(1)=2, f(2)=3, f(3)=5, f(4)=6
		},
		{
			"recursive call in if then branch",
			`package main
func f(n byte) byte {
	if n > 1 {
		a := f(n - 1)
		return a + 1
	}
	return n
}
func main() { print(f(4)) }`,
			"", "4", // f(0)=0, f(1)=1, f(2)=f(1)+1=2, f(3)=f(2)+1=3, f(4)=f(3)+1=4
		},
		{
			"recursive call in if else branch",
			`package main
func f(n byte) byte {
	if n <= 1 {
		return n
	} else {
		a := f(n - 1)
		return a + 1
	}
}
func main() { print(f(4)) }`,
			"", "4",
		},
		{
			"recursive return in if then branch",
			`package main
func f(n byte) byte {
	if n > 0 {
		return f(n - 1) + 2
	}
	return 1
}
func main() { print(f(3)) }`,
			"", "7", // f(0)=1, f(1)=3, f(2)=5, f(3)=7
		},
		{
			"recursive call in if with else if",
			`package main
func f(n byte) byte {
	if n == 0 {
		return 1
	} else if n == 1 {
		return 2
	} else {
		a := f(n - 1)
		return a + 1
	}
}
func main() { print(f(4)) }`,
			"", "5", // f(0)=1, f(1)=2, f(2)=f(1)+1=3, f(3)=f(2)+1=4, f(4)=f(3)+1=5
		},
		{
			"recursive call in both if branches",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	if n%2 == 0 {
		return f(n-1) + 1
	} else {
		return f(n-1) + 2
	}
}
func main() { print(f(4)) }`,
			"", "6", // f(0)=0, f(1)=2, f(2)=3, f(3)=5, f(4)=6
		},
		{
			"recursive call in if then and fallthrough",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	if n%2 == 0 {
		return f(n-1) + 1
	}
	return f(n-1) + 2
}
func main() { print(f(4)) }`,
			"", "6",
		},
		{
			"recursive call in switch",
			`package main
func f(n byte) byte {
	switch {
	case n == 0:
		return 1
	case n == 1:
		return 2
	default:
		a := f(n - 1)
		return a + 1
	}
}
func main() { print(f(4)) }`,
			"", "5", // f(0)=1, f(1)=2, f(2)=3, f(3)=4, f(4)=5
		},
		{
			"recursive call in switch with tag",
			`package main
func f(n byte) byte {
	switch n {
	case 0:
		return 1
	case 1:
		return 2
	default:
		return f(n-1) + f(n-2)
	}
}
func main() { print(f(7)) }`,
			"", "34", // fib-like: 1 2 3 5 8 13 21 34
		},
		{
			"switch in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	switch a {
	case 3:
		return 10
	case 10:
		return 20
	default:
		return a + 1
	}
}
func main() { print(f(6)) }`,
			"", "21", // f(0)=0,f(1)=1,f(2)=2,f(3)=3,f(4)=10,f(5)=20,f(6)=21
		},
		{
			"recursive calls in switch cases",
			`package main
func f(n byte) byte {
	if n <= 1 { return n }
	switch n {
	case 2: return f(1) + f(0)
	case 3: return f(2) + f(1)
	default: return f(n-1) + f(n-2)
	}
}
func main() { print(f(6)) }`,
			"", "8",
		},
		{
			"for loop in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	s := byte(0)
	for i := byte(0); i < n; i++ {
		s += a
	}
	return s + 1
}
func main() { print(f(3)) }`,
			"", "10", // f(0)=0, f(1)=0*1+1=1, f(2)=1*2+1=3, f(3)=3*3+1=10
		},
		{
			"range in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 1 }
	a := f(n - 1)
	s := byte(0)
	for range n {
		s += a
	}
	return s
}
func main() { print(f(3)) }`,
			"", "6", // f(0)=1, f(1)=1*1=1, f(2)=1*2=2, f(3)=2*3=6
		},
		{
			"range no key in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	s := byte(0)
	for range 3 {
		s += a
	}
	return s + 1
}
func main() { print(f(2)) }`,
			"", "4", // f(0)=0, f(1)=0*3+1=1, f(2)=1*3+1=4
		},
		{
			"for break in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 3 }
	a := f(n - 1)
	s := byte(0)
	for i := byte(0); i < 10; i++ {
		s += a
		if s > 5 { break }
	}
	return s
}
func main() { print(f(1)) }`,
			"", "6", // a=3, loop: s=3,6(break). Result: 6
		},
		{
			"for continue in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	for i := byte(0); i < n; i++ {
		if i%2 == 0 { continue }
		a += i
	}
	return a
}
func main() { print(f(4)) }`,
			"", "6", // f(0)=0, f(1)=0, f(2)=0+1=1, f(3)=1+1=2, f(4)=2+1+3=6
		},
		{
			"infinite for with break in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 1 }
	a := f(n - 1)
	s := byte(0)
	for {
		s += a
		if s >= 10 { break }
	}
	return s
}
func main() { print(f(2)) }`,
			"", "10", // f(0)=1, f(1)=10 (1+1+...=10), f(2)=10
		},
		{
			"labeled break in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
outer:
	for i := byte(0); i < 4; i++ {
		for j := byte(0); j < 4; j++ {
			if i*j > 3 { break outer }
			print(i, j)
		}
	}
	return a
}
func main() { f(1) }`,
			"", "00010203101112132021",
		},
		{
			"range with key in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	s := byte(0)
	for i := range n {
		s += a + i
	}
	return s
}
func main() { print(f(3)) }`,
			"", "6",
		},
		{
			"compound assign in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 1 }
	a := f(n - 1)
	a *= 2
	return a + 1
}
func main() { print(f(3)) }`,
			"", "15", // f(0)=1, f(1)=1*2+1=3, f(2)=3*2+1=7, f(3)=7*2+1=15
		},
		{
			"swap in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := byte(1)
	b := byte(2)
	a, b = b, a
	return a*10 + b + f(n-1)
}
func main() { print(f(1)) }`,
			"", "21",
		},
		{
			"var declaration in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	var s byte = a + n
	return s
}
func main() { print(f(3)) }`,
			"", "6", // f(0)=0, f(1)=1, f(2)=3, f(3)=6
		},
		{
			"const declaration in recursive function",
			`package main
func f(n byte) byte {
	const inc = byte(10)
	if n == 0 { return 0 }
	return f(n - 1) + inc
}
func main() { print(f(3)) }`,
			"", "30",
		},
		{
			"const block with iota in recursive function",
			`package main
func f(n byte) byte {
	const (
		a = iota
		b
		c
	)
	if n == 0 { return byte(a + b + c) }
	return f(n - 1) + byte(c)
}
func main() { print(f(3)) }`,
			"", "9", // base = 0+1+2=3, then +2 three times: 3+2+2+2=9
		},
		{
			"recursive paren expr",
			`package main
func f(n byte) byte {
	if n == 0 { return 1 }
	a := f(n - 1)
	return (a) * 2
}
func main() { print(f(3)) }`,
			"", "8", // f(0)=1, f(1)=2, f(2)=4, f(3)=8
		},
		{
			"recursive byte conversion",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	return byte(a + 10)
}
func main() { print(f(3)) }`,
			"", "30", // f(0)=0, f(1)=10, f(2)=20, f(3)=30
		},
		{
			"min in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	return min(f(n - 1) + 3, byte(5))
}
func main() { print(f(4)) }`,
			"", "5", // saturates at 5: f(0)=0, f(1)=3, f(2)=5, f(3)=5, f(4)=5
		},
		{
			"max in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 1 }
	return max(f(n - 1), byte(n))
}
func main() { print(f(4)) }`,
			"", "4", // f(0)=1, f(1..4)=n
		},
		{
			"min with three args in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 10 }
	return min(f(n - 1), byte(n) * 2, byte(15))
}
func main() { print(f(5)) }`,
			"", "2", // f(0)=10, f(1)=min(10,2,15)=2, then f(k>=2)=2
		},
		{
			"min uint16 in tail-recursive function",
			`package main
func f(n byte, acc uint16) uint16 {
	if n == 0 { return acc }
	return f(n-1, min(acc, uint16(n)*100))
}
func main() { print(f(5, 250)) }`,
			"", "100",
		},
		{
			"min uint16 in general-recursive function",
			`package main
func f(n byte) uint16 {
	if n == 0 { return 500 }
	return min(f(n - 1), uint16(n)*100)
}
func main() { print(f(5)) }`,
			"", "100",
		},
		{
			"bitwise ops in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := byte(12) & byte(10)
	b := byte(12) | byte(10)
	c := byte(12) ^ byte(10)
	d := byte(12) &^ byte(10)
	return f(n - 1) + a + b + c + d
}
func main() { print(f(2)) }`,
			"", "64", // a=8, b=14, c=6, d=4; sum=32 per level; f(2)=2*32
		},
		{
			"bitwise compound assigns in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	x := byte(15)
	x &= byte(12)
	x |= byte(3)
	x ^= byte(7)
	return f(n - 1) + x
}
func main() { print(f(3)) }`,
			"", "24", // x ends at 8; f(3) = 3*8
		},
		{
			"uint16 xor in recursive function",
			`package main
func f(n byte) uint16 {
	if n == 0 { return 0 }
	return f(n - 1) + (uint16(0x1234) ^ uint16(0x00FF))
}
func main() { print(f(3)) }`,
			"", "14433", // 0x12CB = 4811; f(3) = 3*4811
		},
		{
			"uint32 xor in recursive function",
			`package main
func f(n byte) uint32 {
	if n == 0 { return 0 }
	return f(n - 1) + (uint32(0x12345678) ^ uint32(0x00FF00FF))
}
func main() { print(f(3)) }`,
			"", "945947541", // 0x12CB5687 = 315315847; f(3) = 3*315315847
		},
		{
			"uint32 and in recursive function",
			`package main
func f(n byte) uint32 {
	if n == 0 { return uint32(0xABCDEF00) }
	return f(n - 1) & uint32(0xFF00FF00)
}
func main() { print(f(3)) }`,
			"", "2868965120", // 0xABCDEF00 & 0xFF00FF00 = 0xAB00EF00
		},
		{
			"recursive mul div mod",
			`package main
func f(n byte) byte {
	if n == 0 { return 12 }
	a := f(n - 1)
	return a * 2 / 3
}
func main() { print(f(2)) }`,
			"", "5", // f(0)=12, f(1)=24/3=8, f(2)=16/3=5
		},
		{
			"recursive comparisons",
			`package main
func f(n byte) byte {
	if n == 0 { return 5 }
	a := f(n - 1)
	b := byte(0)
	if a < 3 { b = b + 1 }
	if a != 3 { b = b + 10 }
	if a >= 3 { b = b + 100 }
	return b
}
func main() { print(f(1)) }`,
			"", "110",
		},
		{
			"recursive getchar",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	c := getchar()
	a := f(n - 1)
	return a + c
}
func main() { putchar(f(2)) }`,
			"!!", "B", // '!'=33, f(0)=0, f(1)=0+33=33, f(2)=33+33=66='B'
		},
		{
			"recursive print call",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	println(a)
	return a + 1
}
func main() { f(3) }`,
			"", "0\n1\n2\n",
		},
		{
			"recursive if with init",
			`package main
func f(n byte) byte {
	if n == 0 { return 10 }
	a := f(n - 1)
	if x := a + 1; x > 5 {
		return x
	} else {
		return x + 1
	}
}
func main() { print(f(3)) }`,
			"", "13", // f(0)=10, f(1)=11>5->11, f(2)=12>5->12, f(3)=13
		},
		{
			"recursive assign existing",
			`package main
func f(n byte) byte {
	if n == 0 { return 1 }
	a := f(n - 1)
	a = a + n
	return a
}
func main() { print(f(4)) }`,
			"", "11", // f(0)=1, f(1)=2, f(2)=4, f(3)=7, f(4)=11
		},
		{
			"recursive index expr",
			`package main
func f(n byte) byte {
	if n == 0 { return 3 }
	a := f(n - 1)
	return byte(a - 1)
}
func main() { print(f(2)) }`,
			"", "1", // f(0)=3, f(1)=2, f(2)=1
		},
		{
			"non-tail rec add",
			`package main
func f(n byte) byte {
	if n <= 1 { return n }
	a := f(n-1)
	b := f(n-2)
	return a + b
}
func main() { print(f(6)) }`,
			"", "8", // fibonacci: f(6)=8
		},
		{
			"rec extract paren unary",
			`package main
func f(n byte) byte {
	if n <= 1 { return n }
	return -(f(n-1)) + f(n-2)
}
func main() { putchar(f(6)) }`,
			"", "\xf8", // f(6)=248 (wrapping arithmetic)
		},
		{
			"tail call in general rec",
			`package main
func f(n byte) byte {
	if n <= 1 { return n }
	a := f(n - 1)
	return f(a)
}
func main() { print(f(5)) }`,
			"", "1", // f(5)->f(f(4))->...->f(1)=1
		},
		{
			"recursive putchar call",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	putchar('0' + a)
	return a + 1
}
func main() { f(4) }`,
			"", "0123",
		},
		{
			"recursive if with else",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	if n > 3 {
		return a + 10
	} else {
		return a + 1
	}
}
func main() { print(f(5)) }`,
			"", "23",
		},
		{
			"recursive call in if with init and else",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	if x := a + n; x > 5 {
		return x
	} else {
		return x + 1
	}
}
func main() { print(f(3)) }`,
			"", "8",
		},
		{
			"recursive with both branches and else",
			`package main
func f(n byte) byte {
	if n <= 1 { return 1 }
	if n%2 == 0 {
		return f(n-1) + 1
	} else {
		return f(n-2) + 2
	}
}
func main() { print(f(6)) }`,
			"", "6",
		},
		{
			"recursive both branches with statements",
			`package main
func f(n byte) byte {
	if n <= 1 { return 1 }
	if n%2 == 0 {
		a := f(n - 1)
		return a + 1
	} else {
		b := n * 2
		c := f(n - 2)
		return b + c
	}
}
func main() { print(f(5)) }`,
			"", "17",
		},
		{
			"const in recursive function",
			`package main
const limit = 5
func f(n byte) byte {
	if n >= limit { return n }
	a := f(n + 1)
	return a
}
func main() { println(f(0)) }`,
			"", "5\n",
		},
		{
			"switch on param in recursive function",
			`package main
func f(n byte) byte {
	switch n {
	case 0:
		return 1
	case 1:
		return 2
	default:
		a := f(n - 1)
		return a + n
	}
}
func main() { print(f(4)) }`,
			"", "11",
		},
		{
			"print call in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	print(n)
	return a
}
func main() { f(3) }`,
			"", "123",
		},
		{
			"switch with default in recursive function",
			`package main
func f(n byte) byte {
	switch n {
	case 1:
		a := f(n - 1)
		return a + 10
	default:
		return n
	}
}
func main() { print(f(1)) }`,
			"", "10",
		},
		{
			"putchar in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	putchar(n + 48)
	return a + n
}
func main() { print(f(3)) }`,
			"", "1236",
		},
		{
			"var decl with init in recursive function",
			`package main
func f(n byte) byte {
	var x byte = n * 2
	if n == 0 { return 0 }
	a := f(n - 1)
	return a + x
}
func main() { print(f(3)) }`,
			"", "12",
		},
		{
			"compound assign with array in recursive function",
			`package main
func f(n byte) byte {
	x := n
	x += 10
	if n == 0 { return x }
	a := f(n - 1)
	return a + x
}
func main() { print(f(2)) }`,
			"", "33",
		},
		{
			"for loop with sum in recursive function",
			`package main
func f(n byte) byte {
	s := byte(0)
	for i := byte(1); i <= n; i++ {
		s += i
	}
	if n <= 1 { return s }
	b := f(n - 1)
	return s + b
}
func main() { print(f(3)) }`,
			"", "10",
		},
		{
			"if with init in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 1 }
	if x := n * 2; x > 4 {
		a := f(n - 1)
		return a + x
	}
	a := f(n - 1)
	return a + n
}
func main() { print(f(4)) }`,
			"", "18",
		},
		{
			"else branch in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	if n % 2 == 0 {
		return a + n
	} else {
		return a + n * 2
	}
}
func main() { print(f(4)) }`,
			"", "14",
		},
		{
			"switch with tag in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	switch n % 2 {
	case 0:
		return f(n - 1)
	case 1:
		return 1 + f(n - 1)
	}
	return 0
}
func main() { print(f(5)) }`,
			"", "3",
		},
		{
			"switch with tag wider than byte in recursive function",
			`package main
func f(n uint16) byte {
	if n == 0 { return 0 }
	switch n {
	case 100:
		return 1
	case 300:
		return 2
	default:
		return f(n - 1)
	}
}
func main() { println(f(300)) }`,
			"", "2\n",
		},
		{
			"named return in recursive function",
			`package main
func f(n byte) (r byte) {
	if n == 0 { return 1 }
	r = f(n - 1)
	return
}
func main() { print(f(1)) }`,
			"", "1",
		},
		{
			"named return accumulate in recursive function",
			`package main
func f(n byte) (sum byte) {
	if n == 0 { return 0 }
	sum = f(n - 1)
	sum += n
	return
}
func main() { print(f(5)) }`,
			"", "15",
		},
		{
			"named return explicit value in recursive function",
			`package main
func f(n byte) (r byte) {
	if n == 0 {
		r = 42
		return
	}
	return f(n - 1) + 1
}
func main() { print(f(3)) }`,
			"", "45",
		},
		{
			"named return with recursive function assign",
			`package main
func fib(n byte) (r byte) {
	if n <= 1 { return n }
	r = fib(n-1) + fib(n-2)
	return
}
func main() { print(fib(8)) }`,
			"", "21",
		},
		{
			"recursive for with break and continue",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	s := byte(0)
	for i := byte(0); i < n; i++ {
		if i == 2 { continue }
		if i == 4 { break }
		s += i
	}
	return s + f(n-1)
}
func main() { print(f(5)) }`,
			"", "10",
		},
		{
			"nested recursive call ackermann",
			`package main
func ack(m, n byte) byte {
	if m == 0 { return n + 1 }
	if n == 0 { return ack(m-1, 1) }
	return ack(m-1, ack(m, n-1))
}
func main() { print(ack(3, 2)) }`,
			"", "29",
		},
		{
			"switch without tag in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	switch {
	case n%2 == 0:
		return n + f(n-1)
	default:
		return n*2 + f(n-1)
	}
}
func main() { print(f(4)) }`,
			"", "14",
		},
		{
			"for with continue in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	s := byte(0)
	for i := byte(0); i < 3; i++ {
		if i == 1 { continue }
		s += i
	}
	return s + f(n-1)
}
func main() { print(f(2)) }`,
			"", "4",
		},
		{
			"if else in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	if n%2 == 0 {
		return n + f(n-1)
	} else {
		return n*2 + f(n-1)
	}
}
func main() { print(f(4)) }`,
			"", "14",
		},
		{
			"blank identifier in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	_ = n
	return 1 + f(n-1)
}
func main() { print(f(3)) }`,
			"", "3",
		},
		{
			"break in for loop in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	s := byte(0)
	for i := byte(0); i < 10; i++ {
		if i == 3 { break }
		s += i
	}
	return s + f(n-1)
}
func main() { print(f(2)) }`,
			"", "6",
		},
		{
			"three conditional calls in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	x := n
	if x == 1 { return f(n-1) }
	if x == 2 { return 10 + f(n-1) }
	return 20 + f(n-1)
}
func main() { print(f(1)); print(" "); print(f(2)); print(" "); print(f(3)) }`,
			"", "0 10 30",
		},
		{
			"switch with modulo in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	switch n % 3 {
	case 0:
		return f(n-1)
	case 1:
		return 1 + f(n-1)
	default:
		return 2 + f(n-1)
	}
	return 0
}
func main() { print(f(6)) }`,
			"", "6",
		},
		{
			"if with init modulo in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	if x := n % 2; x == 0 {
		return n + f(n-1)
	}
	return n*2 + f(n-1)
}
func main() { print(f(4)) }`,
			"", "14",
		},
		{
			"return with inline call in recursive function",
			`package main
func g(a, b byte) byte { return a + b }
func f(n byte) byte {
	if n == 0 { return 0 }
	return g(n, f(n-1))
}
func main() { print(f(3)) }`,
			"", "6",
		},
		{
			"inline call in recursive function",
			`package main
func g(a, b byte) byte { return a + b }
func f(n byte) byte {
	if n == 0 { return 0 }
	r := f(n-1)
	return g(n, r)
}
func main() { print(f(3)) }`,
			"", "6",
		},
		{
			"inline call with locals in recursive function",
			`package main
func double(x byte) byte {
	r := x * 2
	return r
}
func f(n byte) byte {
	if n == 0 { return 0 }
	v := f(n - 1)
	return double(n) + v
}
func main() { println(f(4)) }`,
			"", "20\n",
		},
		{
			"void recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 42 }
	f(n - 1)
	return n
}
func main() { print(f(2)) }`,
			"", "2",
		},
		{
			"user function call as statement in recursive function",
			`package main
func g(x byte) { print(x); print(" ") }
func f(n byte) byte {
	if n == 0 { return 0 }
	g(n)
	r := f(n - 1)
	return r
}
func main() { print(f(3)) }`,
			"", "3 2 1 0",
		},
		{
			"multi-return function in recursive function",
			`package main
func divmod(a, b byte) (byte, byte) { return a / b, a % b }
func f(n byte) byte {
	if n < 10 { return n }
	q, r := divmod(n, 10)
	return f(q) + r
}
func main() { print(f(123)) }`,
			"", "6",
		},
		{
			"general rec initial call widens narrower literal arg",
			`package main
func walk(n byte, acc uint32, mark byte) uint32 {
	if n == 0 { return acc + uint32(mark) }
	r := walk(n - 1, acc, mark)
	return r
}
func main() { println(walk(2, 1000, 99)) }`,
			"", "1099\n",
		},
		{
			"general rec inner call widens narrower literal arg",
			`package main
func reset(n byte, acc uint32) uint32 {
	if n == 0 { return acc }
	return reset(n - 1, 1000) + 1
}
func main() { println(reset(3, 0)) }`,
			"", "1003\n",
		},
		{
			"general rec multi-return byte tuple via explicit values",
			`package main
func sumMax(n byte) (byte, byte) {
	if n == 0 { return 0, 0 }
	s, m := sumMax(n - 1)
	s += n
	if n > m { m = n }
	return s, m
}
func main() { println(sumMax(10)) }`,
			"", "55 10\n",
		},
		{
			"general rec multi-return uintN tuple via explicit values",
			`package main
func evensOdds(n byte) (uint16, uint16) {
	if n == 0 { return 0, 0 }
	e, o := evensOdds(n - 1)
	if n%2 == 0 { e++ } else { o++ }
	return e, o
}
func main() { println(evensOdds(100)) }`,
			"", "50 50\n",
		},
		{
			"general rec multi-return mixed-width named with tuple assign",
			`package main
func walk(n byte) (b byte, u uint16) {
	if n == 0 {
		b = 1
		u = 100
		return
	}
	b, u = walk(n - 1)
	b++
	u += 10
	return
}
func main() { println(walk(5)) }`,
			"", "6 150\n",
		},
		{
			"general rec multi-return mixed-width define then accumulate",
			`package main
func split(n byte) (byte, uint16) {
	if n == 0 { return 0, 1000 }
	b, u := split(n - 1)
	return b + 1, u + 7
}
func main() {
	println(split(8))
}`,
			"", "8 1056\n",
		},
		{
			"general rec multi-return three uint16 spans algo-temp",
			`package main
func three(n byte) (uint16, uint16, uint16) {
	if n == 0 { return 100, 200, 300 }
	a, b, c := three(n - 1)
	return a + 1, b + 10, c + 100
}
func main() { println(three(2)) }`,
			"", "102 220 500\n",
		},
		{
			"general rec multi-return shares non-temp source",
			`package main
func f(n byte, x uint16) (uint16, uint16) {
	if n == 0 { return x, x }
	a, b := f(n-1, x)
	return a, b
}
func main() { println(f(2, 13)) }`,
			"", "13 13\n",
		},
		{
			"multi-return spread feeding a recursive function",
			`package main
func three() (uint16, uint16, uint16) { return 100, 200, 300 }
func consume(a, b, c uint16) (uint16, uint16, uint16) {
	if a == 0 { return 0, 0, 0 }
	x, y, z := consume(a-1, b, c)
	return x + 1, y + 1, z + 1
}
func main() { println(consume(three())) }`,
			"", "100 100 100\n",
		},
		{
			"general rec uint16 local initialized from unary expression",
			`package main
func f(n byte) uint16 {
	if n == 0 { return 100 }
	a := ^uint16(n)
	r := f(n - 1)
	mask := uint16(0xFF)
	return r + (a & mask)
}
func main() {
	for i := byte(0); i < 5; i++ { println(f(i)) }
}`,
			"", "100\n354\n607\n859\n1110\n",
		},
		{
			"general rec inline-call with literal widened to uintN param",
			`package main
func add(a, b uint16) uint16 { return a + b }
func mul(a, b uint16) uint16 { return a * b }
func f(n byte) uint16 {
	if n == 0 { return 1 }
	r := f(n - 1)
	return add(r, mul(uint16(n), 3))
}
func main() {
	for i := byte(0); i < 5; i++ { println(f(i)) }
}`,
			"", "1\n4\n10\n19\n31\n",
		},
		{
			"general rec with uint16 tail-style return inside branch",
			`package main
func f(n byte) uint16 {
	if n == 0 { return 1000 }
	if n == 1 { return f(n - 1) }
	return f(n-1) + uint16(n)
}
func main() {
	for i := byte(0); i < 5; i++ { println(f(i)) }
}`,
			"", "1000\n1000\n1002\n1005\n1009\n",
		},
		{
			"general rec mixed-width multi-return assigned inside body",
			`package main
func split(n byte) (uint16, byte) { return uint16(n) * 1000, n + 1 }
func f(n byte) byte {
	if n == 0 { return 99 }
	a, b := split(n)
	switch a {
	case 1000:
		return b
	case 2000:
		return b * 2
	default:
		return f(n - 1)
	}
}
func main() {
	for i := byte(0); i < 5; i++ { println(f(i)) }
}`,
			"", "99\n2\n6\n6\n6\n",
		},
		{
			"switch with print cases in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	switch n % 3 {
	case 0:
		print("fizz ")
	case 1:
		print("one ")
	default:
		print("other ")
	}
	return f(n - 1)
}
func main() { f(6) }`,
			"", "fizz other one fizz other one ",
		},
		{
			"switch with init in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	switch x := n % 2; x {
	case 0:
		print("even ")
	default:
		print("odd ")
	}
	r := f(n - 1)
	return r
}
func main() { f(4) }`,
			"", "even odd even odd ",
		},
		{
			"bool switch in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	switch {
	case n > 5:
		print("big ")
	case n > 2:
		print("mid ")
	default:
		print("low ")
	}
	r := f(n - 1)
	return r + 1
}
func main() { print(f(7)) }`,
			"", "big big mid mid mid low low 7",
		},
		{
			"for loop sum in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	s := byte(0)
	for i := byte(1); i <= n; i++ {
		s += i
	}
	r := f(n - 1)
	return s + r
}
func main() { println(f(3)) }`,
			"", "10\n",
		},
		{
			"recursive switch with cases",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	switch n % 3 {
	case 0:
		return f(n - 1)
	case 1:
		return 1 + f(n - 1)
	default:
		return 2 + f(n - 1)
	}
}
func main() { print(f(6)) }`,
			"", "6",
		},
		{
			"recursive if with init statement",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	if x := n * 2; x > 5 {
		return x + f(n-1)
	}
	return f(n-1)
}
func main() { print(f(4)) }`,
			"", "14",
		},
		{
			"recursive switch with uint16 init-defined tag",
			`package main
func f(n byte) byte {
	if n == 0 { return 99 }
	switch x := uint16(n) * 100; x {
	case 100:
		return 1
	case 200:
		return 2
	case 65535:
		return 3
	default:
		return f(n-1)
	}
}
func main() {
	for i := byte(0); i < 5; i++ { println(f(i)) }
}`,
			"", "99\n1\n2\n2\n2\n",
		},
		{
			"uint16 local in recursive function",
			`package main
func f(n byte) byte {
	x := uint16(100)
	if n == 0 { return byte(x / uint16(10)) }
	r := f(n - 1)
	return r
}
func main() { print(f(2)) }`,
			"", "10",
		},
		{
			"uint16 return in recursive function",
			`package main
func f(n byte) uint16 {
	if n == 0 { return uint16(1000) }
	r := f(n - 1)
	return r
}
func main() { print(f(3)) }`,
			"", "1000",
		},
		{
			"uint16 add across recursive frames",
			`package main
func sum(n byte) uint16 {
	if n == 0 { return 0 }
	r := sum(n - 1)
	return r + uint16(n)
}
func main() { print(sum(10)) }`,
			"", "55",
		},
		{
			"uint16 fibonacci (binary recursion)",
			`package main
func fib(n byte) uint16 {
	if n <= 1 { return uint16(n) }
	return fib(n-1) + fib(n-2)
}
func main() { print(fib(15)) }`,
			"", "610",
		},
		{
			"uint32 sum across recursive frames",
			`package main
func sum(n byte) uint32 {
	if n == 0 { return 0 }
	return sum(n - 1) + uint32(n)
}
func main() { print(sum(20)) }`,
			"", "210",
		},
		{
			"uint32 local in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return byte(0) }
	x := uint32(100) + uint32(n)
	r := f(n - 1)
	return byte(x) + r
}
func main() { print(f(3)) }`,
			"", "50",
		},
		{
			"uint16 param in tail-recursive function",
			`package main
func f(n uint16) uint16 {
	if n == 0 { return 42 }
	return f(n - 1)
}
func main() { print(f(5)) }`,
			"", "42",
		},
		{
			"uint16 param accumulator (tail-recursive)",
			`package main
func sum(n, acc uint16) uint16 {
	if n == 0 { return acc }
	return sum(n-1, acc+n)
}
func main() { print(sum(100, 0)) }`,
			"", "5050",
		},
		{
			"uint16 param in general recursion",
			`package main
func f(n uint16) uint16 {
	if n == 0 { return 0 }
	r := f(n - 1)
	return r + n
}
func main() {
	print(f(10))
}`,
			"", "55",
		},
		{
			"uint16 param fibonacci (binary recursion)",
			`package main
func fib(n uint16) uint16 {
	if n <= 1 { return n }
	return fib(n-1) + fib(n-2)
}
func main() { print(fib(20)) }`,
			"", "6765",
		},
		{
			"mixed byte and uint16 params in recursion",
			`package main
func f(n byte, acc uint16) uint16 {
	if n == 0 { return acc }
	return f(n-1, acc+uint16(n))
}
func main() { print(f(10, 1000)) }`,
			"", "1055",
		},
		{
			"uint32 param in recursion",
			`package main
func sum(n uint32) uint32 {
	if n == 0 { return 0 }
	return n + sum(n-1)
}
func main() { print(sum(50)) }`,
			"", "1275",
		},
		{
			"multiple uint16 locals in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return byte(0) }
	a := 100 + uint16(n)
	b := 200 - uint16(n)
	r := f(n - 1)
	return byte(a) + byte(b) + r
}
func main() { print(f(3)) }`,
			"", "132",
		},
		{
			"uint16 div and mod in recursive function",
			`package main
func f(n byte) uint16 {
	if n == 0 { return 0 }
	x := 1000 + uint16(n)
	q := x / 10
	r := x % 10
	rec := f(n - 1)
	return rec + q + r
}
func main() { print(f(5)) }`,
			"", "515",
		},
		{
			"uint16 return assigned to uint16 local in caller",
			`package main
func f(n byte) uint16 {
	if n == 0 { return 7 }
	x := uint16(3)
	rec := f(n - 1)
	return rec + x
}
func main() { print(f(3)) }`,
			"", "16",
		},
		{
			"uint16 incdec in recursive function",
			`package main
func f(n byte) uint16 {
	if n == 0 { return 0 }
	x := uint16(100)
	x++
	x++
	r := f(n - 1)
	return r + x
}
func main() { print(f(3)) }`,
			"", "306",
		},
		{
			"uint16 inc with carry in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return byte(0) }
	x := uint16(255)
	x++
	r := f(n - 1)
	return byte(x>>byte(8)) + r
}
func main() { print(f(3)) }`,
			"", "3",
		},
		{
			"uint16-returning function inlined in recursive function",
			`package main
func g(n byte) uint16 { return uint16(n) * 100 }
func f(n byte) byte {
	if n == 0 { return byte(0) }
	x := g(n)
	r := f(n - 1)
	return byte(x>>byte(8)) + r
}
func main() { print(f(5)) }`,
			"", "3",
		},
		{
			"uint16-param function inlined in recursive function",
			`package main
func g(n uint16) uint16 { return n * 10 }
func f(n byte) uint16 {
	if n == 0 { return 0 }
	r := f(n - 1)
	return g(uint16(n)) + r
}
func main() { print(f(5)) }`,
			"", "150",
		},
		{
			"named uint16 return in recursive function",
			`package main
func f(n byte) (s uint16) {
	if n == 0 { s = 0; return }
	r := f(n - 1)
	s = r + uint16(n)
	return
}
func main() { print(f(3)) }`,
			"", "6",
		},
		{
			"named uint16 return with non-zero high byte (bare return)",
			`package main
func f(n byte) (s uint16) {
	if n == 0 { s = 500; return }
	r := f(n - 1)
	s = r + 100
	return
}
func main() { print(f(3)) }`,
			"", "800",
		},
		{
			"for loop with uint16 local in recursive function",
			`package main
func f(n byte) uint16 {
	if n == 0 { return 0 }
	x := uint16(0)
	for i := byte(0); i < n; i++ {
		x = x + uint16(i)
	}
	r := f(n - 1)
	return r + x
}
func main() { print(f(4)) }`,
			"", "10",
		},
		// --- Defer ---
		{
			"defer basic",
			`package main
func main() {
	defer putchar('!')
	putchar('H')
	putchar('i')
}`,
			"", "Hi!",
		},
		{
			"defer LIFO order",
			`package main
func main() {
	defer putchar('3')
	defer putchar('2')
	defer putchar('1')
	putchar('G')
	putchar('o')
}`,
			"", "Go123",
		},
		{
			"defer println",
			`package main
func main() {
	defer println("world")
	print("hello ")
}`,
			"", "hello world\n",
		},
		{
			"defer in function with return",
			`package main
func f() byte {
	defer putchar('!')
	putchar('H')
	putchar('i')
	return 10
}
func main() { println(f()) }`,
			"", "Hi!10\n",
		},
		{
			"defer with early return",
			`package main
func f(x byte) byte {
	defer putchar('.')
	if x == 0 {
		return 0
	}
	return x + 1
}
func main() {
	print(f(0))
	print(f(5))
}`,
			"", ".0.6",
		},
		{
			"defer captures value",
			`package main
func main() {
	x := byte(1)
	defer print(x)
	x = 2
	print(x)
}`,
			"", "21",
		},
		{
			"defer in recursive function",
			`package main
func f(n byte) byte {
	defer putchar('.')
	if n == 0 { return 0 }
	a := f(n - 1)
	return a + 1
}
func main() { print(f(3)) }`,
			"", "....3",
		},
		{
			"defer captures value in recursive function",
			`package main
func f(n byte) byte {
	defer print(n)
	if n == 0 { return 0 }
	a := f(n - 1)
	return a + 1
}
func main() { print(f(3)) }`,
			"", "01233",
		},
		{
			"defer println in recursive function",
			`package main
func f(n byte) byte {
	defer println("done")
	if n == 0 { return 0 }
	a := f(n - 1)
	return a + 1
}
func main() { f(2) }`,
			"", "done\ndone\ndone\n",
		},
		{
			"defer in if true",
			`package main
func main() {
	if byte(1) > 0 {
		defer putchar('!')
	}
	putchar('A')
}`,
			"", "A!",
		},
		{
			"defer in if false",
			`package main
func main() {
	if byte(0) > 0 {
		defer putchar('!')
	}
	putchar('A')
}`,
			"", "A",
		},
		{
			"defer in switch",
			`package main
func main() {
	x := byte(2)
	switch x {
	case 1:
		defer putchar('A')
	case 2:
		defer putchar('B')
	case 3:
		defer putchar('C')
	default:
		defer putchar('D')
	}
	putchar('!')
}`,
			"", "!B",
		},
		{
			"defer conditional",
			`package main
func f(x byte) {
	if x > 0 {
		defer putchar('D')
	}
	putchar('!')
}
func main() { f(1); f(0) }`,
			"", "!D!",
		},
		{
			"defer in recursive with println",
			`package main
func f(n byte) byte {
	defer putchar(n + 48)
	if n == 0 { return 0 }
	a := f(n - 1)
	return a + n
}
func main() { println(f(3)) }`,
			"", "01236\n",
		},
		{
			"defer after base case in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	defer putchar(n + 48)
	r := f(n - 1)
	return r + 1
}
func main() { print(f(3)) }`,
			"", "1233",
		},
		{
			"defer println multi arg in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	defer println(n, n*2)
	return f(n - 1) + n
}
func main() { print(f(3)) }`,
			"", "1 2\n2 4\n3 6\n6",
		},
		{
			"defer with uint16 arg in recursive function",
			`package main
func f(n byte) uint16 {
	if n == 0 { return 0 }
	x := uint16(n) * 100
	defer println(n, x)
	return f(n - 1) + x
}
func main() { println(f(3)) }`,
			"", "1 100\n2 200\n3 300\n600\n",
		},
		{
			"defer with mixed-width args in recursive function",
			`package main
func f(n byte) uint16 {
	if n == 0 { return 0 }
	a := uint16(n) * 1000
	b := byte(n) + 50
	defer println(a, b, n, a+uint16(n))
	return f(n - 1) + a
}
func main() { println(f(3)) }`,
			"", "1000 51 1 1001\n2000 52 2 2002\n3000 53 3 3003\n6000\n",
		},
		{
			"defer in tail-recursive function",
			`package main
func f(n byte) byte {
	defer print(n)
	if n == 0 { return 0 }
	return f(n - 1)
}
func main() { f(4); println() }`,
			"", "01234\n",
		},
		{
			"nested defer across functions",
			`package main
func inner() {
	defer print("C")
	print("B")
}
func main() {
	defer print("D\n")
	print("A")
	inner()
}`,
			"", "ABCD\n",
		},
		{
			"defer with slice argument",
			`package main
func show(s []byte) {
	for _, v := range s { print(v); print(" ") }
	println()
}
func main() {
	s := make([]byte, 3)
	s[0] = 1; s[1] = 2; s[2] = 3
	defer show(s)
	s[1] = 99
}`,
			"", "1 99 3 \n",
		},
		// --- Constants ---
		{
			"const declaration",
			`package main
const n = 42
func main() { println(n) }`,
			"", "42\n",
		},
		{
			"untyped const promoted to uint16",
			`package main
const n = 300
func main() { println(n) }`,
			"", "300\n",
		},
		{
			"char literal widened into uint16 struct field",
			`package main
type Box struct { v uint16 }
func main() {
	other := uint16(0xABCD)
	var b Box
	b.v = 'A'
	println(b.v)
	println(other)
}`,
			"", "65\n43981\n",
		},
		{
			"char literal widened into uint16 array element",
			`package main
func main() {
	other := uint16(0xABCD)
	var arr [3]uint16
	arr[0] = 'A'
	arr[1] = 'B'
	arr[2] = 'C'
	println(arr[0], arr[1], arr[2], other)
}`,
			"", "65 66 67 43981\n",
		},
		{
			"char literal widened into uint16 return position",
			`package main
func get(k byte) uint16 {
	if k == 0 { return 'A' }
	if k == 1 { return 'B' }
	return 'Z'
}
func main() {
	other := uint16(0xABCD)
	for i := byte(0); i < 3; i++ { println(get(i)) }
	println(other)
}`,
			"", "65\n66\n90\n43981\n",
		},
		{
			"byte-fitting untyped const widened into uint16 field",
			`package main
const X = 5
type Box struct { v uint16 }
func main() {
	other := uint16(0xABCD)
	var b Box
	b.v = X
	println(b.v)
	println(other)
}`,
			"", "5\n43981\n",
		},
		{
			"const in array size",
			`package main
const size = 3
func main() {
	var a [size]byte
	a[0] = 1
	a[1] = 2
	a[2] = 3
	println(a[0], a[1], a[2])
}`,
			"", "1 2 3\n",
		},
		{
			"const in expression",
			`package main
const x = 10
func main() {
	println(x + 5)
	println(x * 2)
}`,
			"", "15\n20\n",
		},
		{
			"string constant",
			`package main
const hello = "Hello"
func main() { print(hello) }`,
			"", "Hello",
		},
		{
			"string constant in println",
			`package main
const hello = "Hello"
const world = "World"
func main() { println(hello, world) }`,
			"", "Hello World\n",
		},
		{
			"string constant in const block",
			`package main
const (
	fizz = "Fizz"
	buzz = "Buzz"
)
func main() {
	print(fizz)
	println(buzz)
}`,
			"", "FizzBuzz\n",
		},
		{
			"string constant in defer",
			`package main
const msg = "Done"
func main() {
	defer println(msg)
	print("Hi ")
}`,
			"", "Hi Done\n",
		},
		{
			"char constant",
			`package main
const nl = '\n'
func main() {
	print("hello")
	putchar(nl)
}`,
			"", "hello\n",
		},
		{
			"const block",
			`package main
const (
	a = 10
	b = 20
)
func main() { println(a, b) }`,
			"", "10 20\n",
		},
		{
			"iota",
			`package main
const (
	x = iota
	y
	z
)
func main() { println(x, y, z) }`,
			"", "0 1 2\n",
		},
		{
			"iota with expression",
			`package main
const (
	p = iota * 10
	q
	r
)
func main() { println(p, q, r) }`,
			"", "0 10 20\n",
		},
		{
			"iota with concrete value",
			`package main
const (
	a = iota
	b
	c = 100
	d
	e = iota
	f
)
func main() { println(a, b, c, d, e, f) }`,
			"", "0 1 100 100 4 5\n",
		},
		{
			"const expressions",
			`package main
const (
	a = 3 + 5
	b = 10 - 1
	c = a * 2
	d = 100 / 4
	e = b % 5
	f = (1 + 2) * 3
	g = 1 << 4
	h = 128 >> 2
	i = 0x35 & 0x1C
	j = 0x12 | 0x05
	k = 0x37 ^ 0x1C
	l = 0x37 &^ 0x14
	m = ^byte(0x0F)
)
func main() { println(a, b, c, d, e, f, g, h, i, j, k, l, m) }`,
			"", "8 9 16 25 4 9 16 32 20 23 43 35 240\n",
		},
		{
			"typed constant",
			`package main
const x byte = 42
func main() { println(x) }`,
			"", "42\n",
		},
		{
			"const as array index",
			`package main
const idx = 1
func main() {
	a := [3]byte{10, 20, 30}
	print(a[idx])
}`,
			"", "20",
		},
		{
			"const as 2d array index",
			`package main
const row = 1
const col = 2
func main() {
	var a [2][3]byte
	a[row][col] = 42
	print(a[row][col])
}`,
			"", "42",
		},
		{
			"local const",
			`package main
func main() {
	const x = 42
	println(x)
}`,
			"", "42\n",
		},
		{
			"local const block with iota",
			`package main
func main() {
	const (
		a = iota
		b
		c
	)
	println(a, b, c)
	const (
		x = iota * 10
		y
		z
	)
	println(x, y, z)
}`,
			"", "0 1 2\n0 10 20\n",
		},
		{
			"local const expression",
			`package main
func main() {
	const x = 10
	const y = x + 5
	const z = x * y
	println(x, y, z)
}`,
			"", "10 15 150\n",
		},
		{
			"local const string",
			`package main
func main() {
	const msg = "hi"
	println(msg)
}`,
			"", "hi\n",
		},
		{
			"local string const does not leak across functions",
			`package main
func b() string {
	const msg = "beta"
	return msg
}
func a() string {
	const msg = "alpha"
	inner := b()
	return msg + " " + inner
}
func main() {
	println(a())
}`,
			"", "alpha beta\n",
		},
		{
			"inner byte var shadows outer string const",
			`package main
const x = "alpha"
func main() {
	{
		x := byte(5)
		println(x)
	}
}`,
			"", "5\n",
		},
		// --- Arrays ---
		{
			"array composite literal",
			`package main
func main() {
	a := [3]byte{72, 105, 10}
	for i := 0; i < 3; i++ {
		putchar(a[i])
	}
}`,
			"", "Hi\n",
		},
		{
			"array composite keyed",
			`package main
func main() {
	a := [5]byte{0: 'H', 4: 'o'}
	a[1] = 'e'
	a[2] = 'l'
	a[3] = 'l'
	for i := range 5 {
		putchar(a[i])
	}
}`,
			"", "Hello",
		},
		{
			"array constant index",
			`package main
func main() {
	var a [5]byte
	a[0] = 72
	a[1] = 101
	a[2] = 108
	a[3] = 108
	a[4] = 111
	putchar(a[0])
	putchar(a[1])
	putchar(a[2])
	putchar(a[3])
	putchar(a[4])
}`,
			"", "Hello",
		},
		{
			"array variable index read",
			`package main
func main() {
	var a [3]byte
	a[0] = 65
	a[1] = 66
	a[2] = 67
	for i := 0; i < 3; i++ {
		putchar(a[i])
	}
}`,
			"", "ABC",
		},
		{
			"array variable index write",
			`package main
func main() {
	var a [3]byte
	for i := 0; i < 3; i++ {
		a[i] = byte(65 + i)
	}
	putchar(a[0])
	putchar(a[1])
	putchar(a[2])
}`,
			"", "ABC",
		},
		{
			"array variable index write high slot",
			`package main
func main() {
	a := byte(1)
	b := byte(2)
	c := byte(3)
	d := byte(4)
	e := byte(5)
	f := byte(6)
	g := byte(7)
	h := byte(8)
	i := byte(9)
	j := byte(10)
	k := byte(11)
	l := byte(12)
	m := byte(13)
	var arr [5]byte
	idx := byte(3)
	arr[idx] = a + b + c + d + e + f + g + h + i + j + k + l + m
	println(arr[3])
}`,
			"", "91\n",
		},
		{
			"array as function parameter",
			`package main
func sum(a [3]byte) byte {
	return a[0] + a[1] + a[2]
}
func main() {
	a := [3]byte{10, 20, 30}
	print(sum(a))
}`,
			"", "60",
		},
		{
			"array return from function",
			`package main
func makeArray() [3]byte {
	a := [3]byte{10, 20, 30}
	return a
}
func main() {
	a := makeArray()
	println(a[0], a[1], a[2])
}`,
			"", "10 20 30\n",
		},
		{
			"return array literal directly",
			`package main
func f() [3]byte {
	return [3]byte{1, 2, 3}
}
func main() {
	a := f()
	println(a[0], a[1], a[2])
}`,
			"", "1 2 3\n",
		},
		{
			"index function returning array",
			`package main
func f() [3]byte { return [3]byte{10, 20, 30} }
func main() {
	println(f()[0], f()[1], f()[2])
}`,
			"", "10 20 30\n",
		},
		{
			"index function returning array variable index",
			`package main
func f() [3]byte { return [3]byte{10, 20, 30} }
func main() {
	i := byte(2)
	println(f()[i])
}`,
			"", "30\n",
		},
		{
			"field of function returning struct",
			`package main
type Point struct { x byte; y byte }
func makePoint() Point { return Point{x: 5, y: 7} }
func main() { println(makePoint().x, makePoint().y) }`,
			"", "5 7\n",
		},
		{
			"field array of function returning struct",
			`package main
type Row struct { data [3]byte; n byte }
func makeRow() Row { return Row{data: [3]byte{1, 2, 3}, n: 3} }
func main() {
	println(makeRow().data[0], makeRow().data[1], makeRow().data[2])
	println(makeRow().n)
}`,
			"", "1 2 3\n3\n",
		},
		{
			"array copy assignment",
			`package main
func main() {
	a := [3]byte{1, 2, 3}
	b := a
	b[0] = 10
	println(a[0], a[1], a[2])
	println(b[0], b[1], b[2])
}`,
			"", "1 2 3\n10 2 3\n",
		},
		{
			"array element inc dec",
			`package main
func main() {
	a := [3]byte{1, 2, 3}
	a[0]++
	a[2]--
	println(a[0], a[1], a[2])
}`,
			"", "2 2 2\n",
		},
		{
			"array element inc dec variable index",
			`package main
func main() {
	a := [3]byte{10, 20, 30}
	i := byte(1)
	a[i]++
	a[i]++
	println(a[0], a[1], a[2])
	j := byte(0)
	a[j]--
	println(a[0], a[1], a[2])
}`,
			"", "10 22 30\n9 22 30\n",
		},
		{
			"pointer array element inc dec variable index",
			`package main
func main() {
	a := [3]byte{10, 20, 30}
	p := &a
	p[1]++
	i := byte(2)
	p[i]--
	println(a[0], a[1], a[2])
}`,
			"", "10 21 29\n",
		},
		{
			"array element assign op",
			`package main
func main() {
	a := [3]byte{10, 20, 30}
	a[1] += 5
	println(a[1])
}`,
			"", "25\n",
		},
		{
			"array literal with zeros after large array",
			`package main
func main() {
	var a [64]byte
	a[27] = 2
	b := [8]byte{255, 0, 1, 0, 255, 0, 1, 0}
	for i := range 8 {
		if i > 0 { print(" ") }
		print(b[i])
	}
}`,
			"", "255 0 1 0 255 0 1 0",
		},
		{
			"zero value array literal",
			`package main
func main() {
	a := [3]byte{}
	println(a[0], a[1], a[2])
}`,
			"", "0 0 0\n",
		},
		{
			"var array with init",
			`package main
func main() {
	var a [3]byte = [3]byte{1, 2, 3}
	println(a[0], a[1], a[2])
}`,
			"", "1 2 3\n",
		},
		{
			"array len",
			`package main
func main() {
	a := [3]byte{'A', 'B', 'C'}
	for i := range len(a) {
		putchar(a[i])
	}
}`,
			"", "ABC",
		},
		{
			"array cap",
			`package main
func main() {
	a := [5]byte{1, 2, 3, 4, 5}
	print(cap(a))
}`,
			"", "5",
		},
		{
			"array variable index range",
			`package main
func main() {
	a := [5]byte{72, 101, 108, 108, 111}
	for i := range 5 {
		putchar(a[i])
	}
}`,
			"", "Hello",
		},
		{
			"array variable index range write",
			`package main
func main() {
	var a [5]byte
	for i := range 5 {
		a[i] = byte(65 + i)
	}
	for i := range 5 {
		putchar(a[i])
	}
}`,
			"", "ABCDE",
		},
		{
			"array element swap",
			`package main
func main() {
	a := [3]byte{65, 66, 67}
	a[0], a[2] = a[2], a[0]
	putchar(a[0])
	putchar(a[1])
	putchar(a[2])
}`,
			"", "CBA",
		},
		{
			"parallel assignment array swap",
			`package main
func main() {
	a := [3]byte{'C', 'A', 'B'}
	a[0], a[1], a[2] = a[1], a[2], a[0]
	for i := range 3 { putchar(a[i]) }
}`,
			"", "ABC",
		},
		{
			"array variable index swap",
			`package main
func main() {
	a := [4]byte{'D', 'C', 'B', 'A'}
	for i := 0; i < 2; i++ {
		j := 3 - i
		a[i], a[j] = a[j], a[i]
	}
	for i := range 4 {
		putchar(a[i])
	}
}`,
			"", "ABCD",
		},
		{
			"array of arrays parallel swap",
			`package main
func main() {
	a := [2][2]byte{{1, 2}, {3, 4}}
	a[0][0], a[1][1] = a[1][1], a[0][0]
	println(a[0][0], a[0][1], a[1][0], a[1][1])
}`,
			"", "4 2 3 1\n",
		},
		{
			"array of structs reassign",
			`package main
type Point struct { x byte; y byte }
func main() {
	a := [2]Point{Point{1, 2}, Point{3, 4}}
	a = [2]Point{Point{x: 5, y: 6}, Point{x: 7, y: 8}}
	println(a[0].x, a[0].y, a[1].x, a[1].y)
}`,
			"", "5 6 7 8\n",
		},
		{
			"array of structs composite lit from ident",
			`package main
type Point struct { x byte; y byte }
func f() Point { return Point{x: 3, y: 7} }
func main() {
	p := f()
	a := [2]Point{p, Point{x: 1, y: 2}}
	println(a[0].x, a[0].y, a[1].x, a[1].y)
}`,
			"", "3 7 1 2\n",
		},
		{
			"array of structs composite lit from func call",
			`package main
type Point struct { x byte; y byte }
func makePoint(a, b byte) Point { return Point{x: a, y: b} }
func main() {
	a := [2]Point{makePoint(3, 7), makePoint(10, 20)}
	println(a[0].x, a[0].y, a[1].x, a[1].y)
}`,
			"", "3 7 10 20\n",
		},
		{
			"struct with nested struct init",
			`package main
type Inner struct { x byte; y byte }
type Outer struct { a Inner; b byte }
func f() Outer { return Outer{a: Inner{x: 3, y: 4}, b: 5} }
func main() {
	o := f()
	println(o.a.x, o.a.y, o.b)
}`,
			"", "3 4 5\n",
		},
		{
			"struct with array field init",
			`package main
type S struct { data [3]byte; n byte }
func f() S { return S{data: [3]byte{10, 20, 30}, n: 3} }
func main() {
	s := f()
	println(s.data[0], s.data[1], s.data[2], s.n)
}`,
			"", "10 20 30 3\n",
		},
		{
			"whole array swap",
			`package main
func main() {
	a := [3]byte{1, 2, 3}
	b := [3]byte{4, 5, 6}
	a, b = b, a
	println(a[0], a[1], a[2])
	println(b[0], b[1], b[2])
}`,
			"", "4 5 6\n1 2 3\n",
		},
		{
			"whole struct swap",
			`package main
type P struct { x byte; y byte }
func main() {
	a := P{x: 1, y: 2}
	b := P{x: 3, y: 4}
	a, b = b, a
	println(a.x, a.y)
	println(b.x, b.y)
}`,
			"", "3 4\n1 2\n",
		},
		{
			"array reassign",
			`package main
func main() {
	a := [3]byte{1, 2, 3}
	a = [3]byte{'A', 'B', 'C'}
	putchar(a[0])
	putchar(a[1])
	putchar(a[2])
}`,
			"", "ABC",
		},
		{
			"array equality",
			`package main
func main() {
	a := [3]byte{1, 2, 3}
	b := [3]byte{1, 2, 3}
	c := [3]byte{1, 2, 4}
	if a == b { print("Y") } else { print("N") }
	if a != c { print("Y") } else { print("N") }
}`,
			"", "YY",
		},
		{
			"range over array with value",
			`package main
func main() {
	a := [3]byte{10, 20, 30}
	s := byte(0)
	for _, v := range a {
		s += v
	}
	println(s)
}`,
			"", "60\n",
		},
		{
			"range over inline array literal",
			`package main
func main() {
	s := byte(0)
	for _, v := range [4]byte{1, 2, 4, 8} {
		s += v
	}
	println(s)
}`,
			"", "15\n",
		},
		{
			"range over inline slice literal",
			`package main
func main() {
	s := uint16(0)
	for _, v := range []uint16{100, 200, 300, 400} {
		s += v
	}
	println(s)
}`,
			"", "1000\n",
		},
		{
			"range over ellipsis-sized array literal",
			`package main
func main() {
	s := byte(0)
	for i, v := range [...]byte{2, 4, 8, 16, 32} {
		s += v
		println(i, v)
	}
	println(s)
}`,
			"", "0 2\n1 4\n2 8\n3 16\n4 32\n62\n",
		},
		{
			"ellipsis-sized array as top-level var",
			`package main
var nums = [...]uint16{10, 20, 30, 40, 50}
func main() {
	var sum uint16
	for _, n := range nums {
		sum += n
	}
	println(sum)
}`,
			"", "150\n",
		},
		{
			"ellipsis-sized struct array literal",
			`package main
type Pair struct { a, b byte }
func main() {
	for _, p := range [...]Pair{{1, 2}, {3, 4}, {5, 6}} {
		println(p.a, p.b)
	}
}`,
			"", "1 2\n3 4\n5 6\n",
		},
		{
			"range over nested array literal",
			`package main
func main() {
	for _, row := range [...][3]byte{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}} {
		for _, v := range row {
			print(v)
		}
		println()
	}
}`,
			"", "123\n456\n789\n",
		},
		{
			"range over named nested array",
			`package main
var grid = [2][3]byte{{10, 20, 30}, {40, 50, 60}}
func main() {
	for _, row := range grid {
		for _, v := range row {
			print(v)
			print(" ")
		}
		println()
	}
}`,
			"", "10 20 30 \n40 50 60 \n",
		},
		{
			"return uint16 array literal",
			`package main
func make4() [4]uint16 {
	return [4]uint16{100, 200, 300, 400}
}
func main() {
	a := make4()
	println(a[0], a[1], a[2], a[3])
}`,
			"", "100 200 300 400\n",
		},
		{
			"return struct array literal",
			`package main
type P struct { x, y byte }
func makeBox() [2]P {
	return [2]P{{1, 2}, {3, 4}}
}
func main() {
	b := makeBox()
	println(b[0].x, b[0].y, b[1].x, b[1].y)
}`,
			"", "1 2 3 4\n",
		},
		{
			"address of uint16 array literal",
			`package main
func main() {
	p := &[3]uint16{100, 200, 300}
	_ = p
	println("ok")
}`,
			"", "ok\n",
		},
		{
			"range over array with break",
			`package main
func main() {
	a := [5]byte{1, 2, 3, 4, 5}
	for _, v := range a {
		if v == 3 { break }
		print(v)
	}
}`,
			"", "12",
		},
		{
			"range over array with continue",
			`package main
func main() {
	a := [5]byte{1, 2, 3, 4, 5}
	for _, v := range a {
		if v == 3 { continue }
		print(v)
	}
}`,
			"", "1245",
		},
		{
			"var array zeroed in loop",
			`package main
func main() {
	for i := byte(0); i < 3; i++ {
		var a [3]byte
		a[i] = i + 1
		println(a[0], a[1], a[2])
	}
}`,
			"", "1 0 0\n0 2 0\n0 0 3\n",
		},
		{
			"len in for",
			`package main
func main() {
	a := [3]byte{'A', 'B', 'C'}
	for i := 0; i < len(a); i++ {
		putchar(a[i])
	}
}`,
			"", "ABC",
		},
		{
			"array keyed literal sparse",
			`package main
func main() {
	a := [4]byte{0: 'H', 3: '!'}
	putchar(a[0])
	putchar(a[3])
}`,
			"", "H!",
		},
		{
			"array assign from variable index",
			`package main
func main() {
	a := [3]byte{10, 20, 30}
	b := [3]byte{}
	for i := range 3 {
		b[i] = a[i]
	}
	for i := range 3 {
		if i > 0 { print(" ") }
		print(b[i])
	}
}`,
			"", "10 20 30",
		},
		{
			"struct array field variable index read",
			`package main
type Data struct { vals [3]byte; count byte }
func main() {
	d := Data{vals: [3]byte{10, 20, 30}, count: 3}
	i := byte(1)
	println(d.vals[i])
}`,
			"", "20\n",
		},
		{
			"struct array field variable index write",
			`package main
type Data struct { vals [3]byte; count byte }
func main() {
	d := Data{vals: [3]byte{10, 20, 30}, count: 3}
	i := byte(2)
	d.vals[i] = 99
	println(d.vals[2])
}`,
			"", "99\n",
		},
		{
			"struct array with uint16 array field double var index",
			`package main
type S struct { counts [4]uint16; tag byte }
func main() {
	var arr [3]S
	arr[1].counts[2] = 100
	arr[1].counts[3] = 200
	i := byte(1)
	arr[i].counts[2] = 888
	println(arr[1].counts[2], arr[1].counts[3])
}`,
			"", "888 200\n",
		},
		{
			"struct array with uint16 array field nested loop read write",
			`package main
type S struct { counts [4]uint16; tag byte }
func main() {
	var arr [3]S
	for i := byte(0); i < 3; i++ {
		arr[i].tag = i
		for j := byte(0); j < 4; j++ {
			arr[i].counts[j] = uint16(i)*100 + uint16(j)
		}
	}
	for i := byte(0); i < 3; i++ {
		for j := byte(0); j < 4; j++ {
			print(arr[i].counts[j])
			print(" ")
		}
		println(arr[i].tag)
	}
}`,
			"", "0 1 2 3 0\n100 101 102 103 1\n200 201 202 203 2\n",
		},
		{
			"range over uint16 array field of var-indexed struct array",
			`package main
type S struct { counts [4]uint16 }
func main() {
	var arr [3]S
	for i := byte(0); i < 3; i++ {
		for j := byte(0); j < 4; j++ {
			arr[i].counts[j] = uint16(i)*100 + uint16(j)
		}
	}
	i := byte(1)
	for j, v := range arr[i].counts {
		if j > 0 { print(" ") }
		print(j); print(":"); print(v)
	}
	println()
}`,
			"", "0:100 1:101 2:102 3:103\n",
		},
		{
			"range over byte array field of var-indexed struct array",
			`package main
type S struct { counts [4]byte }
func main() {
	var arr [3]S
	for i := byte(0); i < 3; i++ {
		for j := byte(0); j < 4; j++ {
			arr[i].counts[j] = i*10 + j
		}
	}
	i := byte(1)
	for _, v := range arr[i].counts {
		print(v); print(" ")
	}
	println()
}`,
			"", "10 11 12 13 \n",
		},
		{
			"2d array variable outer constant inner read",
			`package main
func main() {
	a := [2][3]byte{{1, 2, 3}, {4, 5, 6}}
	i := byte(1)
	println(a[i][0], a[i][1], a[i][2])
}`,
			"", "4 5 6\n",
		},
		{
			"struct array field constant index inc",
			`package main
type Data struct { vals [3]byte; count byte }
func main() {
	d := Data{vals: [3]byte{10, 20, 30}, count: 3}
	d.vals[0]++
	d.vals[2] += 5
	println(d.vals[0], d.vals[2])
}`,
			"", "11 35\n",
		},
		{
			"array parallel swap from indices",
			`package main
func main() {
	a := [3]byte{10, 20, 30}
	a[0], a[2] = a[2], a[0]
	println(a[0], a[1], a[2])
}`,
			"", "30 20 10\n",
		},
		{
			"2d array variable outer constant inner write",
			`package main
func main() {
	var a [2][3]byte
	i := byte(0)
	a[i][0] = 7
	a[i][1] = 8
	a[i][2] = 9
	println(a[0][0], a[0][1], a[0][2])
}`,
			"", "7 8 9\n",
		},
		{
			"2d array variable index row copy",
			`package main
func main() {
	var a [2][3]byte
	a[0] = [3]byte{1, 2, 3}
	a[1] = [3]byte{4, 5, 6}
	for i := range byte(2) {
		if i > 0 { print(" ") }
		row := a[i]
		print(row[0] + row[1] + row[2])
	}
}`,
			"", "6 15",
		},
		{
			"array in struct function",
			`package main
func sum(a [3]byte) byte {
	s := byte(0)
	for i := range 3 {
		s += a[i]
	}
	return s
}
func main() {
	a := [3]byte{1, 2, 3}
	print(sum(a))
}`,
			"", "6",
		},
		{
			"array of arrays",
			`package main
func main() {
	a := [2][3]byte{{1, 2, 3}, {4, 5, 6}}
	println(a[0][0], a[0][1], a[0][2])
	println(a[1][0], a[1][1], a[1][2])
}`,
			"", "1 2 3\n4 5 6\n",
		},
		{
			"array of arrays write",
			`package main
func main() {
	a := [2][3]byte{{1, 2, 3}, {4, 5, 6}}
	a[0][1] = 20
	a[1][2] = 60
	println(a[0][0], a[0][1], a[0][2])
	println(a[1][0], a[1][1], a[1][2])
}`,
			"", "1 20 3\n4 5 60\n",
		},
		{
			"array of arrays variable index",
			`package main
func main() {
	a := [3][2]byte{{1, 2}, {3, 4}, {5, 6}}
	for i := range 3 {
		println(a[i][0], a[i][1])
	}
}`,
			"", "1 2\n3 4\n5 6\n",
		},
		{
			"array of arrays variable index write",
			`package main
func main() {
	var a [3][2]byte
	for i := range 3 {
		a[i][0] = byte(i*2 + 1)
		a[i][1] = byte(i*2 + 2)
	}
	for i := range 3 {
		println(a[i][0], a[i][1])
	}
}`,
			"", "1 2\n3 4\n5 6\n",
		},
		{
			"array of arrays variable index subarray assign",
			`package main
func main() {
	var a [3][2]byte
	for i := range byte(3) {
		a[i] = [2]byte{i + 1, (i + 1) * 10}
	}
	for i := range byte(3) {
		println(a[i][0], a[i][1])
	}
}`,
			"", "1 10\n2 20\n3 30\n",
		},
		{
			"array of arrays const outer var inner",
			`package main
func main() {
	a := [2][3]byte{{10, 20, 30}, {40, 50, 60}}
	for j := range 3 {
		if j > 0 { print(" ") }
		print(a[0][j])
	}
	println()
	for j := range 3 {
		if j > 0 { print(" ") }
		print(a[1][j])
	}
}`,
			"", "10 20 30\n40 50 60",
		},
		{
			"array of arrays const outer var inner write",
			`package main
func main() {
	var a [2][3]byte
	for j := range 3 {
		a[0][j] = byte((j + 1) * 10)
	}
	for j := range 3 {
		if j > 0 { print(" ") }
		print(a[0][j])
	}
}`,
			"", "10 20 30",
		},
		{
			"array of arrays both variable index",
			`package main
func main() {
	a := [2][3]byte{{1, 2, 3}, {4, 5, 6}}
	s := byte(0)
	for i := range 2 {
		for j := range 3 {
			s += a[i][j]
		}
	}
	println(s)
}`,
			"", "21\n",
		},
		{
			"array of arrays both variable index write",
			`package main
func main() {
	var a [2][3]byte
	for i := range 2 {
		for j := range 3 {
			a[i][j] = byte(i*3 + j + 1)
		}
	}
	s := byte(0)
	for i := range 2 {
		for j := range 3 {
			s += a[i][j]
		}
	}
	println(s)
}`,
			"", "21\n",
		},
		{
			"array of arrays assign from variable",
			`package main
func main() {
	var a [2][3]byte
	b := [3]byte{1, 2, 3}
	a[0] = b
	a[1] = [3]byte{4, 5, 6}
	println(a[0][0], a[0][1], a[0][2])
	println(a[1][0], a[1][1], a[1][2])
}`,
			"", "1 2 3\n4 5 6\n",
		},
		{
			"array of structs variable index",
			`package main
type Point struct { x byte; y byte }
func main() {
	a := [3]Point{Point{1, 2}, Point{3, 4}, Point{5, 6}}
	for i := range len(a) {
		println(a[i].x, a[i].y)
	}
}`,
			"", "1 2\n3 4\n5 6\n",
		},
		{
			"array of structs variable index write",
			`package main
type Point struct { x byte; y byte }
func main() {
	var a [3]Point
	for i := range len(a) {
		a[i].x = byte(i + 1)
		a[i].y = byte(i + 10)
	}
	for i := range len(a) {
		println(a[i].x, a[i].y)
	}
}`,
			"", "1 10\n2 11\n3 12\n",
		},
		{
			"2d array of size-1 struct field write and read",
			`package main
type Cell struct { v byte }
func main() {
	var grid [2][2]Cell
	grid[0][0].v = 1
	grid[0][1].v = 2
	grid[1][0].v = 3
	grid[1][1].v = 4
	for i := byte(0); i < 2; i++ {
		for j := byte(0); j < 2; j++ {
			println(grid[i][j].v)
		}
	}
}`,
			"", "1\n2\n3\n4\n",
		},
		{
			"1d array of size-1 struct field assignment",
			`package main
type Cell struct { v byte }
func main() {
	var row [3]Cell
	row[0].v = 10
	row[1].v = 20
	row[2].v = 30
	println(row[0].v, row[1].v, row[2].v)
}`,
			"", "10 20 30\n",
		},
		{
			"array of structs variable index inc/dec on uint16 field",
			`package main
type R struct{ v uint16 }
func main() {
	a := [2]R{R{v: 255}, R{v: 256}}
	i := byte(0)
	a[i].v++
	a[1].v--
	println(a[0].v, a[1].v)
}`,
			"", "256 255\n",
		},
		{
			"nested array of multi-byte int variable read/write",
			`package main
func main() {
	var a [2][2]uint16
	for i := byte(0); i < 2; i++ {
		for j := byte(0); j < 2; j++ {
			a[i][j] = uint16(i)*100 + uint16(j)*10
		}
	}
	for i := byte(0); i < 2; i++ {
		for j := byte(0); j < 2; j++ {
			println(a[i][j])
		}
	}
}`,
			"", "0\n10\n100\n110\n",
		},
		{
			"array of structs variable index struct assign",
			`package main
type Point struct { x, y byte }
func main() {
	var pts [3]Point
	for i := range byte(3) {
		pts[i] = Point{i + 1, (i + 1) * 2}
	}
	for i := range byte(3) {
		println(pts[i].x, pts[i].y)
	}
}`,
			"", "1 2\n2 4\n3 6\n",
		},
		{
			"array of structs variable index copy",
			`package main
type Point struct { x byte; y byte }
func main() {
	a := [3]Point{Point{1, 2}, Point{3, 4}, Point{5, 6}}
	for i := range len(a) {
		if i > 0 { print(" ") }
		p := a[i]
		print(p.x)
	}
}`,
			"", "1 3 5",
		},
		{
			"array of structs variable index inc dec",
			`package main
type Point struct { x byte; y byte }
func main() {
	a := [3]Point{Point{1, 10}, Point{2, 20}, Point{3, 30}}
	for i := range len(a) {
		a[i].x++
		a[i].y += 5
	}
	for i := range len(a) {
		if i > 0 { print(" ") }
		print(a[i].x)
		print(" ")
		print(a[i].y)
	}
}`,
			"", "2 15 3 25 4 35",
		},
		{
			"array of structs",
			`package main
type Point struct { x byte; y byte }
func main() {
	a := [3]Point{Point{1, 2}, Point{3, 4}, Point{5, 6}}
	println(a[0].x, a[0].y, a[1].x, a[1].y, a[2].x, a[2].y)
}`,
			"", "1 2 3 4 5 6\n",
		},
		{
			"array of structs field write",
			`package main
type Point struct { x byte; y byte }
func main() {
	a := [2]Point{Point{1, 2}, Point{3, 4}}
	a[0].x = 10
	a[1] = Point{7, 8}
	println(a[0].x, a[0].y, a[1].x, a[1].y)
}`,
			"", "10 2 7 8\n",
		},
		{
			"parallel assign to array variable index",
			`package main
func main() {
	var a [3]byte
	i := byte(2)
	a[i], a[0] = 30, 10
	println(a[0], a[2])
}`,
			"", "10 30\n",
		},
		{
			"chained array variable index assign",
			`package main
func main() {
	var a [2][3]byte
	for i := byte(0); i < 2; i++ {
		for j := byte(0); j < 3; j++ {
			a[i][j] = i*3 + j + 1
		}
	}
	for i := byte(0); i < 2; i++ {
		for j := byte(0); j < 3; j++ {
			if i > 0 || j > 0 { print(" ") }
			print(a[i][j])
		}
	}
}`,
			"", "1 2 3 4 5 6",
		},
		{
			"array of arrays nested variable index write",
			`package main
func main() {
	var a [3][2]byte
	for i := 0; i < 3; i++ {
		for j := 0; j < 2; j++ {
			a[i][j] = byte(i*2 + j + 1)
		}
	}
	for i := byte(0); i < 3; i++ {
		if i > 0 { print(" ") }
		print(a[i][0])
		print(",")
		print(a[i][1])
	}
}`,
			"", "1,2 3,4 5,6",
		},
		{
			"struct from array element",
			`package main
type P struct { x byte; y byte }
func main() {
	a := [2]P{{x: 1, y: 2}, {x: 3, y: 4}}
	p := a[1]
	println(p.x, p.y)
}`,
			"", "3 4\n",
		},
		{
			"3d array constant index",
			`package main
func main() {
	var a [2][3][4]byte
	a[0][0][0] = 1
	a[1][2][3] = 99
	println(a[0][0][0], a[1][2][3])
}`,
			"", "1 99\n",
		},
		{
			"3d array variable index",
			`package main
func main() {
	var a [2][3][4]byte
	for i := byte(0); i < 2; i++ {
		for j := byte(0); j < 3; j++ {
			for k := byte(0); k < 4; k++ {
				a[i][j][k] = i*12 + j*4 + k + 1
			}
		}
	}
	println(a[0][0][0], a[0][0][3], a[0][2][3], a[1][0][0], a[1][2][3])
}`,
			"", "1 4 12 13 24\n",
		},
		{
			"2d array of structs",
			`package main
type Point struct { x byte; y byte }
func main() {
	var a [2][3]Point
	a[0][0] = Point{x: 1, y: 2}
	a[1][2] = Point{x: 5, y: 6}
	println(a[0][0].x, a[0][0].y, a[1][2].x, a[1][2].y)
}`,
			"", "1 2 5 6\n",
		},
		{
			"2d array of structs with variable index",
			`package main
type P struct { x, y byte }
func main() {
	var grid [3][3]P
	for i := byte(0); i < 3; i++ {
		for j := byte(0); j < 3; j++ {
			grid[i][j] = P{x: i, y: j}
		}
	}
	for i := byte(0); i < 3; i++ {
		for j := byte(0); j < 3; j++ {
			println(grid[i][j].x, grid[i][j].y)
		}
	}
}`,
			"", "0 0\n0 1\n0 2\n1 0\n1 1\n1 2\n2 0\n2 1\n2 2\n",
		},
		{
			"size-1 struct array field write with var index",
			`package main
type Cell struct { v byte }
func main() {
	var matrix [3][3]Cell
	for i := byte(0); i < 3; i++ {
		for j := byte(0); j < 3; j++ {
			matrix[i][j].v = i*3 + j
		}
	}
	for i := byte(0); i < 3; i++ {
		for j := byte(0); j < 3; j++ {
			print(matrix[i][j].v)
			print(" ")
		}
		println()
	}
}`,
			"", "0 1 2 \n3 4 5 \n6 7 8 \n",
		},
		{
			"size-1 struct array element copy and range",
			`package main
type Cell struct { v byte }
func main() {
	var m [3][3]Cell
	for i := byte(0); i < 3; i++ {
		for j := byte(0); j < 3; j++ {
			m[i][j].v = i*3 + j
		}
	}
	c := m[2][1]
	print(c.v)
	for _, c := range m[1] { print(c.v) }
}`,
			"", "7345",
		},
		{
			"struct with 2d array field",
			`package main
type Matrix struct { data [2][3]byte; rows byte }
func main() {
	var m Matrix
	m.data[0][0] = 1
	m.data[0][2] = 3
	m.data[1][1] = 5
	m.rows = 2
	println(m.data[0][0], m.data[0][2], m.data[1][1], m.rows)
}`,
			"", "1 3 5 2\n",
		},
		{
			"2d array of structs variable index field write",
			`package main
type Point struct { x byte; y byte }
func main() {
	var a [2][3]Point
	for i := byte(0); i < 2; i++ {
		for j := byte(0); j < 3; j++ {
			a[i][j].x = i*3 + j + 1
			a[i][j].y = (i*3 + j + 1) * 10
		}
	}
	println(a[0][0].x, a[0][0].y, a[1][2].x, a[1][2].y)
}`,
			"", "1 10 6 60\n",
		},
		{
			"2d array of structs method call",
			`package main
type Point struct { x byte; y byte }
func (p Point) sum() byte { return p.x + p.y }
func main() {
	var a [2][2]Point
	a[0][0] = Point{x: 1, y: 2}
	a[1][1] = Point{x: 7, y: 8}
	i := byte(1)
	println(a[0][0].sum(), a[i][i].sum())
}`,
			"", "3 15\n",
		},
		{
			"2d array copy preserves structure",
			`package main
func main() {
	a := [2][3]byte{{1,2,3},{4,5,6}}
	b := a
	b[0][0] = 99
	println(a[0][0], b[0][0], b[1][2])
}`,
			"", "1 99 6\n",
		},
		{
			"struct with 2d array field variable index",
			`package main
type Matrix struct { data [2][3]byte; rows byte }
func main() {
	var m Matrix
	for i := byte(0); i < 2; i++ {
		for j := byte(0); j < 3; j++ {
			m.data[i][j] = i*3 + j + 1
		}
	}
	m.rows = 2
	i := byte(1)
	j := byte(2)
	println(m.data[i][j], m.rows)
}`,
			"", "6 2\n",
		},
		// --- Slices ---
		{
			"slice make len cap",
			`package main
func main() {
	s := make([]byte, 3, 5)
	println(len(s), cap(s))
}`,
			"", "3 5\n",
		},
		{
			"slice index write read",
			`package main
func main() {
	s := make([]byte, 3)
	s[0] = 10; s[1] = 20; s[2] = 30
	println(s[0], s[1], s[2])
}`,
			"", "10 20 30\n",
		},
		{
			"slice append no reallocation",
			`package main
func main() {
	s := make([]byte, 0, 3)
	s = append(s, 10)
	s = append(s, 20)
	s = append(s, 30)
	println(s[0], s[1], s[2])
}`,
			"", "10 20 30\n",
		},
		{
			"slice append with reallocation",
			`package main
func main() {
	s := make([]byte, 0, 1)
	s = append(s, 10)
	s = append(s, 20)
	println(s[0], s[1], len(s))
}`,
			"", "10 20 2\n",
		},
		{
			"slice append from nil",
			`package main
func main() {
	var s []byte
	s = append(s, 10)
	s = append(s, 20)
	println(s[0], s[1], len(s))
}`,
			"", "10 20 2\n",
		},
		{
			"slice append to new variable",
			`package main
func main() {
	s := []byte{1, 2, 3}
	t := append(s, 4)
	println(t[0], t[1], t[2], t[3], len(t))
}`,
			"", "1 2 3 4 4\n",
		},
		{
			"slice from array",
			`package main
func main() {
	a := [5]byte{10, 20, 30, 40, 50}
	s := a[1:4]
	println(len(s), cap(s), s[0], s[1], s[2])
}`,
			"", "3 4 20 30 40\n",
		},
		{
			"slice range with value",
			`package main
func main() {
	s := []byte{10, 20, 30}
	sum := byte(0)
	for _, v := range s { sum += v }
	println(sum)
}`,
			"", "60\n",
		},
		{
			"slice copy shared backing",
			`package main
func main() {
	s := make([]byte, 2)
	s[0] = 10; s[1] = 20
	t := s
	t[0] = 99
	println(s[0], t[0], t[1])
}`,
			"", "99 99 20\n",
		},
		{
			"slice element inc dec",
			`package main
func main() {
	s := make([]byte, 3)
	s[0] = 10; s[1] = 20; s[2] = 30
	s[1]++
	s[2] += 5
	println(s[0], s[1], s[2])
}`,
			"", "10 21 35\n",
		},
		{
			"slice reverse in place",
			`package main
func main() {
	s := make([]byte, 5)
	for i := range byte(5) { s[i] = i + 1 }
	for i := byte(0); i < 2; i++ {
		j := 4 - i
		t := s[i]; s[i] = s[j]; s[j] = t
	}
	for i, v := range s {
		if i > 0 { print(" ") }
		print(v)
	}
	println()
}`,
			"", "5 4 3 2 1\n",
		},
		{
			"slice index with len expression",
			`package main
func main() {
	s := make([]byte, 0, 10)
	s = append(s, 10)
	s = append(s, 20)
	s = append(s, 30)
	i := len(s) - 1
	top := s[i]
	s = s[:i]
	println(top, len(s))
}`,
			"", "30 2\n",
		},
		{
			"slice reslice with low",
			`package main
func main() {
	s := make([]byte, 5)
	for i := range byte(5) { s[i] = i + 10 }
	t := s[1:4]
	println(len(t), cap(t), t[0], t[1], t[2])
}`,
			"", "3 4 11 12 13\n",
		},
		{
			"slice reslice same slice with low",
			`package main
func main() {
	s := make([]byte, 5)
	for i := range byte(5) { s[i] = i + 10 }
	s = s[2:]
	for i, v := range s {
		if i > 0 { print(" ") }
		print(v)
	}
	println()
}`,
			"", "12 13 14\n",
		},
		{
			"slice reslice with both bounds",
			`package main
func main() {
	s := []byte{10, 20, 30, 40, 50}
	t := s[1:3]
	println(t[0], t[1], len(t), cap(t))
}`,
			"", "20 30 2 4\n",
		},
		{
			"slice reslice after append",
			`package main
func main() {
	a := make([]byte, 0, 4)
	a = append(a, 10)
	a = append(a, 20)
	a = append(a, 30)
	b := a[:2]
	println(b[0], b[1], len(b), cap(b))
}`,
			"", "10 20 2 4\n",
		},
		{
			"slice function param",
			`package main
func sum(s []byte) byte {
	t := byte(0)
	for _, v := range s { t += v }
	return t
}
func main() {
	s := make([]byte, 3)
	s[0] = 10; s[1] = 20; s[2] = 30
	println(sum(s))
}`,
			"", "60\n",
		},
		{
			"slice of structs function param",
			`package main
type Point struct { x, y byte }
func total(pts []Point) byte {
	sum := byte(0)
	for i := range byte(len(pts)) {
		sum += pts[i].x + pts[i].y
	}
	return sum
}
func main() {
	s := make([]Point, 2)
	s[0].x = 3; s[0].y = 4
	s[1].x = 10; s[1].y = 20
	println(total(s))
}`,
			"", "37\n",
		},
		{
			"slice of slices element as param",
			`package main
func sum(s []byte) byte {
	t := byte(0)
	for _, v := range s { t += v }
	return t
}
func main() {
	m := make([][]byte, 2)
	m[0] = []byte{1, 2, 3}
	m[1] = []byte{4, 5, 6}
	a := m[0]
	b := m[1]
	println(sum(a), sum(b))
}`,
			"", "6 15\n",
		},
		{
			"slice return from function",
			`package main
func collect(n byte) []byte {
	s := make([]byte, 0, 10)
	for i := byte(1); i <= n; i++ {
		s = append(s, i)
	}
	return s
}
func main() {
	s := collect(4)
	println(s[0], s[1], s[2], s[3], len(s))
}`,
			"", "1 2 3 4 4\n",
		},
		{
			"slice assigned from inner scope to outer var",
			`package main
func main() {
	var outer []byte
	for i := byte(0); i < 5; i++ {
		inner := []byte{i, i + 1, i + 2}
		outer = inner
	}
	println(outer[0], outer[1], outer[2])
}`,
			"", "4 5 6\n",
		},
		{
			"slice expression aliases into outer scope",
			`package main
func main() {
	var outer []byte
	{
		s := []byte{10, 20, 30, 40, 50}
		outer = s[1:4]
	}
	println(outer[0], outer[1], outer[2])
}`,
			"", "20 30 40\n",
		},
		{
			"slice stored in struct field across scopes",
			`package main
type Holder struct { data []byte }
func main() {
	var h Holder
	for i := byte(0); i < 5; i++ {
		s := []byte{i, i + 10, i + 20}
		h.data = s
	}
	println(h.data[0], h.data[1], h.data[2])
}`,
			"", "4 14 24\n",
		},
		{
			"slice stored in array element across scopes",
			`package main
func main() {
	var arr [3][]byte
	{
		x := []byte{1, 2, 3}
		arr[0] = x
	}
	println(arr[0][0], arr[0][1], arr[0][2])
}`,
			"", "1 2 3\n",
		},
		{
			"composite literal captures inner-scope slices",
			`package main
type Pair struct { a, b []byte }
func main() {
	var p Pair
	{
		x := []byte{1, 2, 3}
		y := []byte{4, 5, 6}
		p = Pair{a: x, b: y}
	}
	println(p.a[0], p.a[1], p.a[2])
	println(p.b[0], p.b[1], p.b[2])
}`,
			"", "1 2 3\n4 5 6\n",
		},
		{
			"string concat across many loop iterations reclaims buffer",
			`package main
func main() {
	total := byte(0)
	for i := byte(0); i < 40; i++ {
		s := ""
		for j := byte(0); j < 5; j++ { s += "X" }
		total += byte(len(s))
	}
	println(total)
}`,
			"", "200\n",
		},
		{
			"string concat with byte-cast operand",
			`package main
func line(c byte, n byte) string {
	s := ""
	for i := byte(0); i < n; i++ {
		s += string(c)
	}
	return s
}
func main() {
	for i := byte(0); i < 12; i++ {
		s := line('A'+i, 8)
		println(i, s)
	}
}`,
			"", `0 AAAAAAAA
1 BBBBBBBB
2 CCCCCCCC
3 DDDDDDDD
4 EEEEEEEE
5 FFFFFFFF
6 GGGGGGGG
7 HHHHHHHH
8 IIIIIIII
9 JJJJJJJJ
10 KKKKKKKK
11 LLLLLLLL
`,
		},
		{
			"slice literal as function argument",
			`package main
func sum(s []byte) byte {
	t := byte(0)
	for _, v := range s { t += v }
	return t
}
func main() { println(sum([]byte{10, 20, 30})) }`,
			"", "60\n",
		},
		{
			"struct slice composite literal",
			`package main
type P struct { x, y byte }
func main() {
	s := []P{P{x: 1, y: 2}, P{x: 3, y: 4}, P{x: 5, y: 6}}
	for i := range byte(len(s)) { println(s[i].x, s[i].y) }
}`,
			"", "1 2\n3 4\n5 6\n",
		},
		{
			"slice literal of size-1 struct elements",
			`package main
type P struct { age byte }
func main() {
	ps := []P{{age: 1}, {age: 2}, {age: 3}}
	for i := 0; i < len(ps); i++ {
		println(ps[i].age)
	}
}`,
			"", "1\n2\n3\n",
		},
		{
			"array slice composite literal",
			`package main
func main() {
	s := [][2]byte{[2]byte{1, 2}, [2]byte{3, 4}}
	println(s[0][0], s[0][1], s[1][0], s[1][1])
}`,
			"", "1 2 3 4\n",
		},
		{
			"slice filter function",
			`package main
func filter(s []byte) []byte {
	r := make([]byte, 0, 10)
	for _, v := range s {
		if v > 2 { r = append(r, v) }
	}
	return r
}
func main() {
	s := filter([]byte{1, 2, 3, 4, 5})
	println(s[0], s[1], s[2], len(s))
}`,
			"", "3 4 5 3\n",
		},
		{
			"slice multi-assign swap",
			`package main
func main() {
	s := make([]byte, 4)
	s[0] = 1; s[1] = 2; s[2] = 3; s[3] = 4
	s[0], s[3] = s[3], s[0]
	s[1], s[2] = s[2], s[1]
	for i, v := range s {
		if i > 0 { print(" ") }
		print(v)
	}
}`,
			"", "4 3 2 1",
		},
		{
			"slice of structs parallel swap",
			`package main
type Item struct{ key, val byte }
func main() {
	xs := []Item{Item{key: 5, val: 50}, Item{key: 2, val: 20}, Item{key: 8, val: 80}}
	xs[0], xs[2] = xs[2], xs[0]
	for i := 0; i < len(xs); i++ {
		if i > 0 { print(" ") }
		print(xs[i].key); print(":"); print(xs[i].val)
	}
}`,
			"", "8:80 2:20 5:50",
		},
		{
			"slice of structs selection sort",
			`package main
type Item struct{ key, val byte }
func sortItems(xs []Item) {
	n := byte(len(xs))
	for i := byte(0); i < n; i++ {
		min := i
		for j := i + 1; j < n; j++ {
			if xs[j].key < xs[min].key { min = j }
		}
		xs[i], xs[min] = xs[min], xs[i]
	}
}
func main() {
	xs := []Item{Item{key: 5, val: 50}, Item{key: 2, val: 20}, Item{key: 8, val: 80}, Item{key: 1, val: 10}}
	sortItems(xs)
	for i := 0; i < len(xs); i++ {
		if i > 0 { print(" ") }
		print(xs[i].key); print(":"); print(xs[i].val)
	}
}`,
			"", "1:10 2:20 5:50 8:80",
		},
		{
			"slice of structs read write",
			`package main
type Point struct { x, y byte }
func main() {
	s := make([]Point, 3)
	s[0].x = 1; s[0].y = 2
	s[1].x = 3; s[1].y = 4
	s[2].x = 5; s[2].y = 6
	for i := range byte(3) {
		println(s[i].x, s[i].y)
	}
}`,
			"", "1 2\n3 4\n5 6\n",
		},
		{
			"slice of struct uintN field write widens narrower literal",
			`package main
type R struct {
	v uint32
	tag byte
}
func main() {
	s := []R{{v: 1000, tag: 1}, {v: 100000, tag: 2}}
	s[0].v = 500
	s[1].v = 999
	for i := byte(0); i < 2; i++ {
		println(s[i].v, s[i].tag)
	}
}`,
			"", "500 1\n999 2\n",
		},
		{
			"slice of struct field write preserves non-temp source",
			`package main
type R struct {
	tag byte
	v   uint32
}
func main() {
	s := []R{{tag: 0, v: 0}, {tag: 0, v: 0}}
	x := byte(42)
	y := uint32(100000)
	s[0].tag = x
	s[1].tag = x
	s[0].v = y
	s[1].v = y
	println(s[0].tag, s[1].tag, x)
	println(s[0].v, s[1].v, y)
}`,
			"", "42 42 42\n100000 100000 100000\n",
		},
		{
			"slice of structs field inc",
			`package main
type Point struct { x, y byte }
func main() {
	s := make([]Point, 2)
	s[0].x = 10; s[0].y = 20
	s[1].x = 30; s[1].y = 40
	s[0].x++; s[1].y += 5
	println(s[0].x, s[0].y, s[1].x, s[1].y)
}`,
			"", "11 20 30 45\n",
		},
		{
			"slice of structs uint16 field write",
			`package main
type Item struct { count uint16; tag byte }
func main() {
	s := make([]Item, 2)
	s[0].count = 1000; s[0].tag = 7
	s[1].count = 50000; s[1].tag = 8
	println(s[0].count, s[0].tag)
	println(s[1].count, s[1].tag)
}`,
			"", "1000 7\n50000 8\n",
		},
		{
			"slice of structs string field write",
			`package main
type Entry struct { name string; v byte }
func main() {
	s := make([]Entry, 2)
	s[0].name = "foo"; s[0].v = 10
	s[1].name = "bar"; s[1].v = 20
	print(s[0].name); print(" "); println(s[0].v)
	print(s[1].name); print(" "); println(s[1].v)
}`,
			"", "foo 10\nbar 20\n",
		},
		{
			"slice of structs array field",
			`package main
type S struct { data [3]byte }
func main() {
	s := make([]S, 2)
	s[0].data[0] = 10; s[0].data[1] = 20; s[0].data[2] = 30
	s[1].data[0] = 40; s[1].data[1] = 50; s[1].data[2] = 60
	for i := range byte(2) {
		println(s[i].data[0], s[i].data[1], s[i].data[2])
	}
}`,
			"", "10 20 30\n40 50 60\n",
		},
		{
			"slice of arrays read write",
			`package main
func main() {
	s := make([][3]byte, 2)
	s[0][0] = 1; s[0][1] = 2; s[0][2] = 3
	s[1][0] = 4; s[1][1] = 5; s[1][2] = 6
	println(s[0][0], s[0][1], s[0][2], s[1][0], s[1][1], s[1][2])
}`,
			"", "1 2 3 4 5 6\n",
		},
		{
			"slice of arrays variable index",
			`package main
func main() {
	s := make([][2]byte, 3)
	for i := range byte(3) {
		s[i][0] = i + 1
		s[i][1] = (i + 1) * 10
	}
	for i := range byte(3) {
		println(s[i][0], s[i][1])
	}
}`,
			"", "1 10\n2 20\n3 30\n",
		},
		{
			"slice of slices read write",
			`package main
func main() {
	s := make([][]byte, 2)
	s[0] = []byte{1, 2, 3}
	s[1] = []byte{4, 5}
	println(s[0][0], s[0][2], s[1][0], s[1][1])
}`,
			"", "1 3 4 5\n",
		},
		{
			"slice of slices element write",
			`package main
func main() {
	s := make([][]byte, 2)
	s[0] = []byte{1, 2, 3}
	s[1] = []byte{4, 5}
	s[0][1] = 99
	s[1][0] = 88
	println(s[0][1], s[1][0])
}`,
			"", "99 88\n",
		},
		{
			"slice of slices inner len cap",
			`package main
func main() {
	m := make([][]byte, 2)
	m[0] = []byte{1, 2, 3}
	m[1] = []byte{10, 20}
	sum := byte(0)
	for i := range byte(len(m)) {
		inner := m[i]
		for j := range byte(len(inner)) {
			sum += inner[j]
		}
	}
	println(sum, len(m[0]), len(m[1]))
}`,
			"", "36 3 2\n",
		},
		{
			"slice reslice both bounds variable",
			`package main
func main() {
	s := []byte{10, 20, 30, 40, 50}
	low := byte(1)
	high := byte(4)
	t := s[low:high]
	println(len(t), t[0], t[2])
}`,
			"", "3 20 40\n",
		},
		{
			"slice make with cap",
			`package main
func main() {
	s := make([]byte, 2, 5)
	s[0] = 10
	s[1] = 20
	println(len(s), cap(s), s[0], s[1])
}`,
			"", "2 5 10 20\n",
		},
		{
			"slice as accumulator across functions",
			`package main
func addRange(s []byte, lo, hi byte) []byte {
	for i := lo; i <= hi; i++ { s = append(s, i) }
	return s
}
func main() {
	s := make([]byte, 0, 20)
	s = addRange(s, 1, 3)
	s = addRange(s, 10, 11)
	println(s[0], s[1], s[2], s[3], s[4], len(s))
}`,
			"", "1 2 3 10 11 5\n",
		},
		{
			"three slices growing interleaved",
			`package main
func main() {
	var a, b, c []byte
	for i := byte(0); i < 4; i++ {
		a = append(a, i+1)
		b = append(b, (i+1)*10)
		c = append(c, (i+1)+50)
	}
	println(a[0], a[3], b[0], b[3], c[0], c[3])
}`,
			"", "1 4 10 40 51 54\n",
		},
		{
			"slice with recursive function call",
			`package main
func fib(n byte) byte {
	if n <= 1 { return n }
	return fib(n-1) + fib(n-2)
}
func main() {
	var s []byte
	for i := byte(0); i < 6; i++ {
		s = append(s, fib(i))
	}
	for _, v := range s {
		print(v)
		print(" ")
	}
	println(len(s))
}`,
			"", "0 1 1 2 3 5 6\n",
		},
		{
			"slice append result to new variable",
			`package main
func main() {
	s := make([]byte, 0, 4)
	s = append(s, 1)
	t := append(s, 2)
	println(t[0], t[1], len(t))
}`,
			"", "1 2 2\n",
		},
		{
			"reslice variable both bounds",
			`package main
func main() {
	s := make([]byte, 5, 5)
	for i := range byte(5) { s[i] = (i + 1) * 10 }
	i := byte(1)
	t := s[i:i+3]
	println(t[0], t[1], t[2], len(t))
}`,
			"", "20 30 40 3\n",
		},
		{
			"reslice variable high bound",
			`package main
func main() {
	s := make([]byte, 5, 5)
	for i := range byte(5) { s[i] = (i + 1) * 10 }
	i := byte(3)
	t := s[:i]
	println(t[0], t[1], t[2], len(t))
}`,
			"", "10 20 30 3\n",
		},
		{
			"reslice variable low bound",
			`package main
func main() {
	s := make([]byte, 5, 5)
	for i := range byte(5) { s[i] = (i + 1) * 10 }
	i := byte(2)
	t := s[i:]
	println(t[0], t[1], t[2], len(t))
}`,
			"", "30 40 50 3\n",
		},
		{
			"nil slice append does not corrupt earlier vars",
			`package main
func main() {
	x := byte(10)
	y := byte(20)
	var s []byte
	s = append(s, 42)
	println(x, y, s[0])
}`,
			"", "10 20 42\n",
		},
		{
			"slice struct composite literal assign",
			`package main
type P struct { x, y byte }
func (p P) sum() byte { return p.x + p.y }
func main() {
	s := make([]P, 2)
	s[0] = P{x: 3, y: 4}
	s[1] = P{x: 5, y: 6}
	println(s[0].sum(), s[1].sum())
}`,
			"", "7 11\n",
		},
		{
			"slice struct composite literal variable index",
			`package main
type P struct { x, y byte }
func main() {
	s := make([]P, 3)
	for i := range byte(3) {
		s[i] = P{x: i + 1, y: (i + 1) * 10}
	}
	println(s[0].x, s[0].y, s[1].x, s[1].y, s[2].x, s[2].y)
}`,
			"", "1 10 2 20 3 30\n",
		},
		{
			"slice struct append composite literal",
			`package main
type P struct { x, y byte }
func main() {
	var s []P
	s = append(s, P{x: 10, y: 20})
	s = append(s, P{x: 30, y: 40})
	println(s[0].x, s[0].y, s[1].x, s[1].y)
}`,
			"", "10 20 30 40\n",
		},
		{
			"slice struct append variable",
			`package main
type P struct { x, y byte }
func main() {
	var s []P
	p := P{x: 1, y: 2}
	s = append(s, p)
	println(s[0].x, s[0].y)
}`,
			"", "1 2\n",
		},
		{
			"range over struct slice",
			`package main
type P struct { x, y byte }
func (p P) sum() byte { return p.x + p.y }
func main() {
	s := make([]P, 3)
	s[0] = P{x: 1, y: 2}
	s[1] = P{x: 3, y: 4}
	s[2] = P{x: 5, y: 6}
	for i, p := range s {
		if i > 0 { print(" ") }
		print(p.sum())
	}
}`,
			"", "3 7 11",
		},
		{
			"struct slice append reuse variable",
			`package main
type P struct { x, y byte }
func main() {
	var s []P
	var p P
	p.x = 1; p.y = 2
	s = append(s, p)
	p.x = 3; p.y = 4
	s = append(s, p)
	println(s[0].x, s[0].y, s[1].x, s[1].y)
}`,
			"", "1 2 3 4\n",
		},
		{
			"slice of slices append named variable",
			`package main
func main() {
	var s [][]byte
	a := []byte{1, 2, 3}
	b := []byte{4, 5}
	s = append(s, a)
	s = append(s, b)
	println(s[0][0], s[0][2], s[1][0], s[1][1])
}`,
			"", "1 3 4 5\n",
		},
		{
			"slice return from reslice",
			`package main
func first2(s []byte) []byte {
	return s[:2]
}
func main() {
	s := make([]byte, 4)
	s[0] = 1; s[1] = 2; s[2] = 3; s[3] = 4
	t := first2(s)
	println(t[0], t[1], len(t))
}`,
			"", "1 2 2\n",
		},
		{
			"struct slice return from function",
			`package main
type P struct { x, y byte }
func makePts() []P {
	var s []P
	s = append(s, P{x: 1, y: 2})
	s = append(s, P{x: 3, y: 4})
	return s
}
func main() {
	s := makePts()
	println(s[0].x, s[0].y, s[1].x, s[1].y)
}`,
			"", "1 2 3 4\n",
		},
		{
			"struct slice reslice",
			`package main
type P struct { x, y byte }
func main() {
	s := make([]P, 3)
	s[0] = P{x: 1, y: 2}; s[1] = P{x: 3, y: 4}; s[2] = P{x: 5, y: 6}
	t := s[1:]
	println(t[0].x, t[0].y, t[1].x, t[1].y)
}`,
			"", "3 4 5 6\n",
		},
		{
			"array slice append composite literal",
			`package main
func main() {
	var s [][3]byte
	s = append(s, [3]byte{1, 2, 3})
	s = append(s, [3]byte{4, 5, 6})
	println(s[0][0], s[0][1], s[0][2], s[1][0], s[1][1], s[1][2])
}`,
			"", "1 2 3 4 5 6\n",
		},
		{
			"struct slice element copy",
			`package main
type P struct { x, y byte }
func main() {
	s := make([]P, 2)
	s[0] = P{x: 1, y: 2}; s[1] = P{x: 3, y: 4}
	s[0] = s[1]
	println(s[0].x, s[0].y)
}`,
			"", "3 4\n",
		},
		{
			"local struct from slice element",
			`package main
type P struct { x, y byte }
func main() {
	s := make([]P, 2)
	s[0] = P{x: 1, y: 2}; s[1] = P{x: 3, y: 4}
	tmp := s[0]
	s[1] = tmp
	println(tmp.x, tmp.y, s[1].x, s[1].y)
}`,
			"", "1 2 1 2\n",
		},
		{
			"make slice with variable length",
			`package main
func main() {
	n := byte(5)
	s := make([]byte, n)
	for i := range n { s[i] = i * 10 }
	println(s[0], s[1], s[2], s[3], s[4])
}`,
			"", "0 10 20 30 40\n",
		},
		{
			"pointer slice deref and modify",
			`package main
func main() {
	x := byte(10)
	y := byte(20)
	var s []*byte
	s = append(s, &x)
	s = append(s, &y)
	*s[0] += 5
	*s[1]++
	println(x, y)
}`,
			"", "15 21\n",
		},
		{
			"struct pointer slice field access and method",
			`package main
type P struct { x, y byte }
func (p P) sum() byte { return p.x + p.y }
func main() {
	a := P{x: 3, y: 4}
	b := P{x: 5, y: 6}
	var s []*P
	s = append(s, &a)
	s = append(s, &b)
	println(s[0].x, s[0].y, s[1].sum())
	s[1].x++
	println(b.x)
}`,
			"", "3 4 11\n6\n",
		},
		{
			"struct pointer slice element assign",
			`package main
type P struct { x, y byte }
func main() {
	a := P{x: 1, y: 2}
	b := P{x: 3, y: 4}
	s := make([]*P, 2)
	s[0] = &a; s[1] = &b
	s[0] = s[1]
	println(s[0].x, s[0].y, s[1].x, s[1].y)
}`,
			"", "3 4 3 4\n",
		},
		{
			"slice struct append indexed element",
			`package main
type P struct { x, y byte }
func filter(pts []P, minX byte) []P {
	var r []P
	for i := range byte(len(pts)) {
		if pts[i].x >= minX {
			r = append(r, pts[i])
		}
	}
	return r
}
func main() {
	s := make([]P, 3)
	s[0] = P{x: 1, y: 2}; s[1] = P{x: 5, y: 6}; s[2] = P{x: 3, y: 4}
	t := filter(s, 3)
	for _, p := range t { println(p.x, p.y) }
}`,
			"", "5 6\n3 4\n",
		},
		{
			"nested struct field through slice",
			`package main
type Inner struct { x, y byte }
type Outer struct { a Inner; b byte }
func main() {
	s := make([]Outer, 2)
	s[0].a.x = 3; s[0].a.y = 4; s[0].b = 5
	s[1].a.x = 6; s[1].a.y = 7; s[1].b = 8
	println(s[0].a.x, s[0].a.y, s[0].b)
	println(s[1].a.x, s[1].a.y, s[1].b)
}`,
			"", "3 4 5\n6 7 8\n",
		},
		{
			"range over pointer slice",
			`package main
type P struct { x, y byte }
func main() {
	a := P{x: 1, y: 2}
	b := P{x: 3, y: 4}
	var s []*P
	s = append(s, &a)
	s = append(s, &b)
	for _, p := range s {
		println(p.x, p.y)
	}
}`,
			"", "1 2\n3 4\n",
		},
		{
			"range over function returning struct slice",
			`package main
type P struct { x, y byte }
func makePts() []P {
	var s []P
	s = append(s, P{x: 1, y: 2})
	s = append(s, P{x: 3, y: 4})
	return s
}
func main() {
	for _, p := range makePts() {
		println(p.x, p.y)
	}
}`,
			"", "1 2\n3 4\n",
		},
		{
			"function return to slice element",
			`package main
type P struct { x, y byte }
func makeP(a, b byte) P { return P{x: a, y: b} }
func main() {
	s := make([]P, 2)
	s[0] = makeP(1, 2)
	s[1] = makeP(3, 4)
	println(s[0].x, s[0].y, s[1].x, s[1].y)
}`,
			"", "1 2 3 4\n",
		},
		{
			"range over slice expression",
			`package main
func main() {
	s := []byte{10, 20, 30, 40, 50}
	sum := byte(0)
	for _, v := range s[1:4] {
		sum += v
	}
	print(sum)
}`,
			"", "90",
		},
		{
			"range over struct slice expression",
			`package main
type P struct { x, y byte }
func main() {
	s := make([]P, 3)
	s[0] = P{x: 1, y: 2}; s[1] = P{x: 3, y: 4}; s[2] = P{x: 5, y: 6}
	for _, p := range s[1:] {
		println(p.x, p.y)
	}
}`,
			"", "3 4\n5 6\n",
		},
		{
			"binary search on sorted slice",
			`package main
func bsearch(s []byte, v byte) byte {
	lo, hi := byte(0), byte(len(s))
	for lo < hi {
		mid := (lo + hi) / 2
		if s[mid] < v {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
func main() {
	s := []byte{2, 5, 8, 12, 16, 23, 38, 56, 72, 91}
	println(bsearch(s, 23), bsearch(s, 50))
}`,
			"", "5 7\n",
		},
		{
			"computed index expression",
			`package main
func main() {
	s := make([]byte, 5)
	for i := range byte(5) { s[i] = (i + 1) * 10 }
	j := byte(1)
	println(s[j+1], s[j+1])
}`,
			"", "30 30\n",
		},
		{
			"matrix multiply with computed indices",
			`package main
func main() {
	a := make([]byte, 4)
	b := make([]byte, 4)
	a[0] = 1; a[1] = 2; a[2] = 3; a[3] = 4
	b[0] = 5; b[1] = 6; b[2] = 7; b[3] = 8
	for i := range byte(2) {
		for j := range byte(2) {
			sum := byte(0)
			for k := range byte(2) {
				sum += a[i*2+k] * b[k*2+j]
			}
			print(sum)
			print(" ")
		}
	}
}`,
			"", "19 22 43 50 ",
		},
		{
			"address of slice element",
			`package main
func swap(a, b *byte) { t := *a; *a = *b; *b = t }
func main() {
	s := make([]byte, 3)
	s[0] = 10; s[1] = 20; s[2] = 30
	swap(&s[0], &s[2])
	println(s[0], s[1], s[2])
}`,
			"", "30 20 10\n",
		},
		{
			"address of struct slice element",
			`package main
type P struct { x, y byte }
func inc(p *P) { p.x++; p.y++ }
func main() {
	s := make([]P, 2)
	s[0] = P{x: 10, y: 20}; s[1] = P{x: 30, y: 40}
	inc(&s[0])
	i := byte(1)
	inc(&s[i])
	println(s[0].x, s[0].y, s[1].x, s[1].y)
}`,
			"", "11 21 31 41\n",
		},
		{
			"copy builtin dst shorter",
			`package main
func main() {
	src := []byte{10, 20, 30, 40}
	dst := make([]byte, 3)
	copy(dst, src)
	println(dst[0], dst[1], dst[2])
}`,
			"", "10 20 30\n",
		},
		{
			"copy builtin dst longer",
			`package main
func main() {
	src := []byte{10, 20, 30}
	dst := make([]byte, 5)
	copy(dst, src)
	println(dst[0], dst[1], dst[2], dst[3], dst[4])
}`,
			"", "10 20 30 0 0\n",
		},
		{
			"copy builtin from array slice",
			`package main
func main() {
	a := [5]byte{1, 2, 3, 4, 5}
	dst := make([]byte, 3)
	copy(dst, a[:])
	println(dst[0], dst[1], dst[2])
}`,
			"", "1 2 3\n",
		},
		{
			"copy overlapping dst after src",
			`package main
func main() {
	a := []byte{1, 2, 3, 4, 5}
	copy(a[2:], a)
	for i, v := range a {
		if i > 0 { print(" ") }
		print(v)
	}
}`,
			"", "1 2 1 2 3",
		},
		{
			"copy overlapping dst before src",
			`package main
func main() {
	a := []byte{1, 2, 3, 4, 5}
	copy(a, a[2:])
	for i, v := range a {
		if i > 0 { print(" ") }
		print(v)
	}
}`,
			"", "3 4 5 4 5",
		},
		{
			"copy return value",
			`package main
func main() {
	src := []byte{1, 2, 3, 4, 5}
	dst := make([]byte, 3)
	n := copy(dst, src)
	println(n)
	dst2 := make([]byte, 10)
	println(copy(dst2, src))
}`,
			"", "3\n5\n",
		},
		{
			"clear builtin",
			`package main
func main() {
	s := []byte{1, 2, 3}
	clear(s)
	println(s[0], s[1], s[2])
}`,
			"", "0 0 0\n",
		},
		{
			"three-index slice",
			`package main
func main() {
	s := make([]byte, 5, 5)
	for i := range byte(5) { s[i] = (i + 1) * 10 }
	t := s[1:3:4]
	println(t[0], t[1], len(t), cap(t))
}`,
			"", "20 30 2 3\n",
		},
		{
			"variadic append",
			`package main
func main() {
	var s []byte
	s = append(s, 1, 2, 3, 4, 5)
	for _, v := range s { print(v); print(" ") }
	println()
}`,
			"", "1 2 3 4 5 \n",
		},
		{
			"append spread",
			`package main
func main() {
	a := []byte{1, 2, 3}
	b := []byte{4, 5, 6}
	a = append(a, b...)
	for _, v := range a { print(v); print(" ") }
	println(len(a))
}`,
			"", "1 2 3 4 5 6 6\n",
		},
		{
			"append to make result",
			`package main
func main() {
	s := append(make([]byte, 2), 30)
	println(s[0], s[1], s[2], len(s))
}`,
			"", "0 0 30 3\n",
		},
		{
			"append variadic to literal",
			`package main
func main() {
	s := append([]byte{1, 2}, 3, 4, 5)
	for _, v := range s { print(v); print(" ") }
	println(len(s))
}`,
			"", "1 2 3 4 5 5\n",
		},
		{
			"append slice from outer scope inside nested block",
			`package main
func main() {
	var x []byte
	{
		v := append(x, 3)
		println(v[0])
	}
	_ = x
}`,
			"", "3\n",
		},
		{
			"shadowing := with append self-reference reads outer",
			`package main
func main() {
	s := []byte{1, 2, 3}
	{
		s := append(s, 4)
		println(len(s), s[0], s[3])
	}
	println(len(s))
}`,
			"", "4 1 4\n3\n",
		},
		{
			"return slice literal from function",
			`package main
func f() []byte { return []byte{10, 20, 30} }
func main() {
	s := f()
	println(s[0], s[1], s[2])
}`,
			"", "10 20 30\n",
		},
		{
			"len and range of pointer array return",
			`package main
func f() *[3]byte {
	a := [3]byte{10, 20, 30}
	return &a
}
func main() {
	println(len(f()))
	for _, v := range f() { print(v); print(" ") }
}`,
			"", "3\n10 20 30 ",
		},
		{
			"pointer to struct array return",
			`package main
type P struct { v byte }
func f() *[2]P {
	a := [2]P{{v: 10}, {v: 20}}
	return &a
}
func main() {
	p := f()
	print(p[0].v); print(byte(' '))
	print(p[1].v)
	println()
}`,
			"", "103220\n",
		},
		{
			"pointer to struct array param",
			`package main
type P struct { v byte }
func sum(arr *[3]P) byte {
	var s byte
	for i := 0; i < 3; i++ { s += arr[i].v }
	return s
}
func main() {
	a := [3]P{{v: 10}, {v: 20}, {v: 30}}
	print(sum(&a))
}`,
			"", "60",
		},
		{
			"slice of slices return",
			`package main
func f() [][]byte { return [][]byte{{'a','b'}, {'c'}} }
func main() {
	xs := f()
	for i := 0; i < len(xs); i++ {
		for j := 0; j < len(xs[i]); j++ { putchar(xs[i][j]) }
		putchar('\n')
	}
}`,
			"", "ab\nc\n",
		},
		{
			"slice of fixed arrays return",
			`package main
func f() [][3]byte { return [][3]byte{{1,2,3},{4,5,6}} }
func main() {
	xs := f()
	for i := 0; i < len(xs); i++ {
		for j := 0; j < 3; j++ { print(xs[i][j]); print(byte(' ')) }
	}
	println()
}`,
			"", "132232332432532632\n",
		},
		{
			"slice nil comparison",
			`package main
func main() {
	var s []byte
	if s == nil { print("Y") } else { print("N") }
	s = make([]byte, 0)
	if s == nil { print("Y") } else { print("N") }
	s = append(s, 1)
	if s != nil { print("Y") } else { print("N") }
}`,
			"", "YNY",
		},
		{
			"return nil from slice function",
			`package main
func f() []byte { return nil }
func main() {
	s := f()
	if s == nil { print("Y") } else { print("N") }
}`,
			"", "Y",
		},
		{
			"nil as slice function argument",
			`package main
func f(s []byte) {
	if s == nil { print("Y") } else { print("N") }
}
func main() { f(nil) }`,
			"", "Y",
		},
		{
			"return pointer to struct from function",
			`package main
type P struct { x, y byte }
func makeP(a, b byte) *P {
	p := &P{x: a, y: b}
	return p
}
func main() {
	pt := makeP(3, 4)
	println(pt.x, pt.y)
}`,
			"", "3 4\n",
		},
		{
			"return nil pointer from function",
			`package main
func f() *byte { return nil }
func main() {
	p := f()
	if p == nil { print("Y") } else { print("N") }
}`,
			"", "Y",
		},
		// --- Structs ---
		{
			"struct literal return as function argument",
			`package main
type Point struct { x byte; y byte }
func sum(p Point) byte { return p.x + p.y }
func makePoint(a, b byte) Point { return Point{x: a, y: b} }
func main() { println(sum(makePoint(3, 4))) }`,
			"", "7\n",
		},
		{
			"struct literal and field access",
			`package main
type Point struct { x byte; y byte }
func main() {
	p := Point{x: 3, y: 5}
	println(p.x, p.y)
}`,
			"", "3 5\n",
		},
		{
			"struct literal reversed fields",
			`package main
type Point struct { x byte; y byte }
func main() {
	p := Point{y: 10, x: 20}
	println(p.x, p.y)
}`,
			"", "20 10\n",
		},
		{
			"struct field assignment",
			`package main
type Point struct { x byte; y byte }
func main() {
	var p Point
	p.x = 3
	p.y = 5
	print(p.x + p.y)
}`,
			"", "8",
		},
		{
			"struct field modify",
			`package main
type Pair struct { a byte; b byte }
func main() {
	p := Pair{a: 10, b: 20}
	p.a += p.b
	print(p.a)
}`,
			"", "30",
		},
		{
			"struct as function parameter",
			`package main
type Point struct { x byte; y byte }
func sum(p Point) byte { return p.x + p.y }
func main() {
	p := Point{x: 30, y: 42}
	println(sum(p))
}`,
			"", "72\n",
		},
		{
			"struct return from function",
			`package main
type Point struct { x byte; y byte }
func makePoint(x byte, y byte) Point {
	p := Point{x: x, y: y}
	return p
}
func main() {
	p := makePoint(3, 7)
	println(p.x, p.y)
}`,
			"", "3 7\n",
		},
		{
			"return struct literal directly",
			`package main
type Point struct { x byte; y byte }
func makePoint() Point {
	return Point{x: 10, y: 20}
}
func main() {
	p := makePoint()
	println(p.x, p.y)
}`,
			"", "10 20\n",
		},
		{
			"struct pass and return",
			`package main
type Pair struct { a byte; b byte }
func swap(p Pair) Pair {
	return Pair{a: p.b, b: p.a}
}
func main() {
	p := Pair{a: 1, b: 2}
	q := swap(p)
	println(q.a, q.b)
}`,
			"", "2 1\n",
		},
		{
			"var struct with init",
			`package main
type Point struct { x byte; y byte }
func main() {
	var p Point = Point{x: 3, y: 5}
	println(p.x, p.y)
}`,
			"", "3 5\n",
		},
		{
			"zero value struct literal",
			`package main
type Point struct { x byte; y byte }
func main() {
	p := Point{}
	println(p.x, p.y)
}`,
			"", "0 0\n",
		},
		{
			"positional struct literal",
			`package main
type Point struct { x byte; y byte }
func main() {
	p := Point{7, 9}
	println(p.x, p.y)
}`,
			"", "7 9\n",
		},
		{
			"struct parallel swap",
			`package main
type Point struct { x byte; y byte }
func main() {
	a := Point{1, 2}
	b := Point{3, 4}
	a, b = b, a
	println(a.x, a.y, b.x, b.y)
}`,
			"", "3 4 1 2\n",
		},
		{
			"struct field parallel swap",
			`package main
type Point struct { x byte; y byte }
func main() {
	p := Point{x: 5, y: 10}
	p.x, p.y = p.y, p.x
	println(p.x, p.y)
}`,
			"", "10 5\n",
		},
		{
			"parallel swap of byte fields via pointer and value",
			`package main
type T struct { x byte }
func swapPtr(p *T, q *T) { p.x, q.x = q.x, p.x }
func main() {
	a := T{x: 1}
	b := T{x: 2}
	swapPtr(&a, &b)
	println(a.x, b.x)
	c := T{x: 3}
	d := T{x: 4}
	c.x, d.x = d.x, c.x
	println(c.x, d.x)
}`,
			"", "2 1\n4 3\n",
		},
		{
			"struct array element swap",
			`package main
type Point struct { x byte; y byte }
func main() {
	a := [2]Point{Point{1, 2}, Point{3, 4}}
	a[0], a[1] = a[1], a[0]
	println(a[0].x, a[0].y, a[1].x, a[1].y)
}`,
			"", "3 4 1 2\n",
		},
		{
			"struct equality with literal",
			`package main
type Point struct { x byte; y byte }
func main() {
	p := Point{1, 2}
	if (p == Point{1, 2}) { print("Y") } else { print("N") }
	if (p == Point{1, 3}) { print("Y") } else { print("N") }
}`,
			"", "YN",
		},
		{
			"struct copy assignment",
			`package main
type Point struct { x byte; y byte }
func main() {
	p := Point{x: 3, y: 5}
	q := p
	q.x = 10
	println(p.x, q.x)
}`,
			"", "3 10\n",
		},
		{
			"struct field inc dec",
			`package main
type Point struct { x byte; y byte }
func main() {
	p := Point{x: 10, y: 20}
	p.x++
	p.y--
	println(p.x, p.y)
}`,
			"", "11 19\n",
		},
		{
			"zero field struct equality",
			`package main
type Empty struct{}
func main() {
	a := Empty{}
	b := Empty{}
	if a == b { print("Y") } else { print("N") }
}`,
			"", "Y",
		},
		{
			"struct equality",
			`package main
type Point struct { x byte; y byte }
func main() {
	a := Point{x: 1, y: 2}
	b := Point{x: 1, y: 2}
	c := Point{x: 1, y: 3}
	if a == b { print("Y") } else { print("N") }
	if a != c { print("Y") } else { print("N") }
}`,
			"", "YY",
		},
		{
			"struct method",
			`package main
type Point struct { x byte; y byte }
func (p Point) sum() byte {
	return p.x + p.y
}
func main() {
	p := Point{x: 3, y: 5}
	println(p.sum())
}`,
			"", "8\n",
		},
		{
			"method call on array element",
			`package main
type P struct { x byte; y byte }
func (p P) sum() byte { return p.x + p.y }
func main() {
	a := [2]P{{x: 1, y: 2}, {x: 3, y: 4}}
	println(a[0].sum(), a[1].sum())
}`,
			"", "3 7\n",
		},
		{
			"method call on array element variable index",
			`package main
type P struct { x byte; y byte }
func (p P) sum() byte { return p.x + p.y }
func main() {
	a := [3]P{{x: 1, y: 2}, {x: 3, y: 4}, {x: 5, y: 6}}
	for i := byte(0); i < 3; i++ {
		if i > 0 { print(" ") }
		print(a[i].sum())
	}
}`,
			"", "3 7 11",
		},
		{
			"method multi return",
			`package main
type P struct { x byte; y byte }
func (p P) swap() (byte, byte) { return p.y, p.x }
func main() {
	p := P{x: 3, y: 7}
	a, b := p.swap()
	println(a, b)
}`,
			"", "7 3\n",
		},
		{
			"method on struct literal",
			`package main
type P struct{ x byte }
func (p P) double() byte { return p.x * 2 }
func main() {
	x := P{x: 10}.double()
	println(x)
}`,
			"", "20\n",
		},
		{
			"struct method returning struct",
			`package main
type Point struct { x byte; y byte }
func (p Point) scale(n byte) Point {
	return Point{x: p.x * n, y: p.y * n}
}
func main() {
	p := Point{x: 3, y: 5}
	q := p.scale(10)
	println(q.x, q.y)
}`,
			"", "30 50\n",
		},
		{
			"struct expression as function argument",
			`package main
type Pair struct { a byte; b byte }
func add(x, y Pair) Pair {
	return Pair{x.a + y.a, x.b + y.b}
}
func sub(x, y Pair) Pair {
	return Pair{x.a - y.a, x.b - y.b}
}
func main() {
	a := Pair{10, 20}
	b := Pair{3, 5}
	c := Pair{1, 2}
	r := add(sub(a, b), c)
	println(r.a, r.b)
}`,
			"", "8 17\n",
		},
		{
			"struct field assign expression",
			`package main
type Point struct { x byte; y byte }
func main() {
	p := Point{1, 2}
	p.x = p.x + p.y
	println(p.x, p.y)
}`,
			"", "3 2\n",
		},
		{
			"struct return assign existing",
			`package main
type Point struct { x byte; y byte }
func make(a, b byte) Point {
	return Point{a, b}
}
func main() {
	p := make(1, 2)
	p = make(p.x + 1, p.y + 1)
	println(p.x, p.y)
}`,
			"", "2 3\n",
		},
		{
			"nested struct",
			`package main
type Point struct { x byte; y byte }
type Rect struct { min Point; max Point }
func main() {
	r := Rect{min: Point{1, 2}, max: Point{3, 4}}
	println(r.min.x, r.min.y, r.max.x, r.max.y)
}`,
			"", "1 2 3 4\n",
		},
		{
			"nested struct field assignment",
			`package main
type Point struct { x byte; y byte }
type Rect struct { min Point; max Point }
func main() {
	var r Rect
	r.min = Point{1, 2}
	r.max.x = 3
	r.max.y = 4
	println(r.min.x, r.min.y, r.max.x, r.max.y)
}`,
			"", "1 2 3 4\n",
		},
		{
			"nested struct as function parameter",
			`package main
type Point struct { x byte; y byte }
type Rect struct { min Point; max Point }
func area(r Rect) byte {
	return (r.max.x - r.min.x) * (r.max.y - r.min.y)
}
func main() {
	r := Rect{min: Point{1, 2}, max: Point{4, 6}}
	println(area(r))
}`,
			"", "12\n",
		},
		{
			"nested struct return from function",
			`package main
type Point struct { x byte; y byte }
type Rect struct { min Point; max Point }
func makeRect(x1, y1, x2, y2 byte) Rect {
	return Rect{min: Point{x1, y1}, max: Point{x2, y2}}
}
func main() {
	r := makeRect(1, 2, 3, 4)
	println(r.min.x, r.min.y, r.max.x, r.max.y)
}`,
			"", "1 2 3 4\n",
		},
		{
			"nested struct equality",
			`package main
type Point struct { x byte; y byte }
type Rect struct { min Point; max Point }
func main() {
	a := Rect{min: Point{1, 2}, max: Point{3, 4}}
	b := Rect{min: Point{1, 2}, max: Point{3, 4}}
	c := Rect{min: Point{1, 2}, max: Point{3, 5}}
	if a == b { print("Y") } else { print("N") }
	if a == c { print("Y") } else { print("N") }
}`,
			"", "YN",
		},
		{
			"struct equality with array uint16 and nested array fields",
			`package main
type S struct { a [3]byte; b byte }
type T struct { v uint16; tag byte }
type N struct { g [2][3]byte; k byte }
func main() {
	s1 := S{a: [3]byte{1, 2, 3}, b: 4}
	s2 := S{a: [3]byte{1, 2, 3}, b: 4}
	s3 := S{a: [3]byte{1, 2, 9}, b: 4}
	t1 := T{v: 1000, tag: 7}
	t2 := T{v: 1000, tag: 7}
	t3 := T{v: 2000, tag: 7}
	n1 := N{g: [2][3]byte{{1,2,3},{4,5,6}}, k: 7}
	n2 := N{g: [2][3]byte{{1,2,3},{4,5,6}}, k: 7}
	n3 := N{g: [2][3]byte{{1,2,3},{9,5,6}}, k: 7}
	if s1 == s2 { print("Y") } else { print("N") }
	if s1 == s3 { print("Y") } else { print("N") }
	if t1 == t2 { print("Y") } else { print("N") }
	if t1 == t3 { print("Y") } else { print("N") }
	if n1 == n2 { print("Y") } else { print("N") }
	if n1 == n3 { print("Y") } else { print("N") }
}`,
			"", "YNYNYN",
		},
		{
			"nested struct copy from variable",
			`package main
type Point struct { x byte; y byte }
type Rect struct { min Point; max Point }
func main() {
	p := Point{5, 6}
	r := Rect{min: p, max: Point{7, 8}}
	println(r.min.x, r.min.y, r.max.x, r.max.y)
}`,
			"", "5 6 7 8\n",
		},
		{
			"struct with array field",
			`package main
type Vec struct { data [3]byte; len byte }
func main() {
	v := Vec{data: [3]byte{10, 20, 30}, len: 3}
	for i := byte(0); i < v.len; i++ {
		if i > 0 { print(" ") }
		print(v.data[i])
	}
}`,
			"", "10 20 30",
		},
		{
			"struct with array field write",
			`package main
type Vec struct { data [3]byte; len byte }
func main() {
	var v Vec
	v.len = 3
	for i := byte(0); i < v.len; i++ {
		v.data[i] = (i + 1) * 10
	}
	for i := byte(0); i < v.len; i++ {
		if i > 0 { print(" ") }
		print(v.data[i])
	}
}`,
			"", "10 20 30",
		},
		{
			"nested struct field assign and read",
			`package main
type Inner struct { x byte; y byte }
type Outer struct { a Inner; b byte }
func main() {
	var o Outer
	o.a.x = 10
	o.a.y = 20
	o.b = 30
	print(o.a.x + o.a.y + o.b)
}`,
			"", "60",
		},
		{
			"3 level nested struct",
			`package main
type A struct { v byte }
type B struct { a A; w byte }
type C struct { b B; x byte }
func main() {
	c := C{b: B{a: A{v: 1}, w: 2}, x: 3}
	c.b.a.v = 99
	println(c.b.a.v, c.b.w, c.x)
}`,
			"", "99 2 3\n",
		},
		{
			"struct array field variable index loop read",
			`package main
type S struct { data [3]byte; n byte }
func main() {
	var s S
	s.data[0] = 10
	s.data[1] = 20
	s.data[2] = 30
	s.n = 3
	for i := byte(0); i < s.n; i++ {
		if i > 0 { print(" ") }
		print(s.data[i])
	}
}`,
			"", "10 20 30",
		},
		{
			"struct with nested struct and array field init",
			`package main
type Inner struct { x byte; y byte }
type Outer struct { a Inner; data [2]byte }
func main() {
	o := Outer{a: Inner{x: 1, y: 2}, data: [2]byte{10, 20}}
	println(o.a.x, o.a.y)
	println(o.data[0], o.data[1])
}`,
			"", "1 2\n10 20\n",
		},
		{
			"struct variable copy in struct init",
			`package main
type P struct { x byte; y byte }
func make() P { return P{x: 3, y: 4} }
func main() {
	a := make()
	b := a
	println(b.x, b.y)
}`,
			"", "3 4\n",
		},
		{
			"array of structs variable index field inc dec",
			`package main
type P struct { x byte; y byte }
func main() {
	var a [3]P
	a[0] = P{x: 10, y: 20}
	i := byte(0)
	a[i].x++
	a[i].y--
	println(a[i].x, a[i].y)
}`,
			"", "11 19\n",
		},
		{
			"method returning struct",
			`package main
type Point struct { x, y byte }
func (p Point) add(q Point) Point {
	return Point{p.x + q.x, p.y + q.y}
}
func main() {
	a := Point{1, 2}
	b := Point{3, 4}
	c := a.add(b)
	print(c.x); print(" "); println(c.y)
}`,
			"", "4 6\n",
		},
		{
			"method returning uintN infers shape for :=",
			`package main
type Counter struct{ n uint16 }
func (c Counter) Get() uint16 { return c.n }
type Big struct{ a uint32; b uint64 }
func (b Big) GetA() uint32 { return b.a }
func (b Big) GetB() uint64 { return b.b }
func main() {
	c := Counter{n: 1000}
	r := c.Get()
	r += 50
	println(r)

	big := Big{a: 100000, b: 10000000000}
	x := big.GetA()
	x *= 2
	println(x)
	y := big.GetB()
	y++
	println(y)
}`,
			"", "1050\n200000\n10000000001\n",
		},
		{
			"method call on function return",
			`package main
type Point struct { x byte; y byte }
func (p Point) sum() byte { return p.x + p.y }
func makePoint(a, b byte) Point { return Point{x: a, y: b} }
func main() { println(makePoint(3, 7).sum()) }`,
			"", "10\n",
		},
		{
			"chained method call on function return",
			`package main
type Point struct { x byte; y byte }
func (p Point) sum() byte { return p.x + p.y }
func (p Point) scale(n byte) Point { return Point{x: p.x * n, y: p.y * n} }
func makePoint(a, b byte) Point { return Point{x: a, y: b} }
func main() { println(makePoint(1, 2).scale(3).sum()) }`,
			"", "9\n",
		},
		{
			"chained pointer-returning method calls",
			`package main
type Stack struct { v [5]byte; n byte }
func (s *Stack) push(x byte) *Stack { s.v[s.n] = x; s.n++; return s }
func (s *Stack) pop() byte { s.n--; return s.v[s.n] }
func main() {
	s := &Stack{}
	s.push(1).push(2).push(3)
	println(s.pop(), s.pop(), s.pop())
}`,
			"", "3 2 1\n",
		},
		{
			"explicit address-of in method call receiver",
			`package main
type Box struct { v byte }
func (b *Box) square() byte { return b.v * b.v }
func main() {
	b := Box{v: 6}
	println((&b).square())
}`,
			"", "36\n",
		},
		{
			"method call on function return as statement",
			`package main
type W struct { n byte }
func (w W) emit() { print(w.n) }
func makeW(n byte) W { return W{n: n} }
func main() {
	makeW(42).emit()
	println()
}`,
			"", "42\n",
		},
		{
			"function return field in arithmetic",
			`package main
type Point struct { x byte; y byte }
func makePoint(a, b byte) Point { return Point{x: a, y: b} }
func main() { println(makePoint(3, 7).x + makePoint(1, 2).y) }`,
			"", "5\n",
		},
		{
			"nested struct field method call",
			`package main
type Inner struct { x byte }
type Outer struct { a Inner }
func (i Inner) val() byte { return i.x }
func main() {
	o := Outer{a: Inner{x: 42}}
	print(o.a.val())
}`,
			"", "42",
		},
		{
			"pointer receiver mutates struct",
			`package main
type C struct { n byte }
func (c *C) inc() { c.n++ }
func (c *C) add(d byte) { c.n += d }
func (c *C) get() byte { return c.n }
func main() {
	c := C{n: 0}
	c.inc()
	c.add(5)
	pc := &c
	pc.inc()
	pc.add(10)
	println(c.get())
	println(pc.get())
}`,
			"", "17\n17\n",
		},
		{
			"pointer receiver string field rename",
			`package main
type T struct { name string }
func (t *T) rename(s string) { t.name = s }
func main() {
	a := T{name: "alice"}
	a.rename("ALICE")
	pa := &a
	pa.rename("everyone")
	println(a.name)
}`,
			"", "everyone\n",
		},
		{
			"value and pointer methods on same type",
			`package main
type Point struct { x, y byte }
func (p Point) translated(dx, dy byte) Point { return Point{x: p.x + dx, y: p.y + dy} }
func (p *Point) shift(dx, dy byte) { p.x += dx; p.y += dy }
func main() {
	p := Point{x: 1, y: 2}
	q := p.translated(10, 20)
	println(q.x, q.y)
	p.shift(5, 5)
	pp := &p
	pp.shift(100, 100)
	println(p.x, p.y)
}`,
			"", "11 22\n106 107\n",
		},
		{
			"value method called via pointer auto-derefs",
			`package main
type C struct { n byte }
func (c C) snapshot() byte { return c.n }
func main() {
	c := C{n: 42}
	pc := &c
	println(pc.snapshot())
	c.n = 99
	println(pc.snapshot())
}`,
			"", "42\n99\n",
		},
		{
			"pointer receiver on array element via variable index",
			`package main
type Item struct { tag, val byte }
func (i *Item) set(t, v byte) { i.tag = t; i.val = v }
func main() {
	xs := [3]Item{}
	for i := byte(0); i < 3; i++ {
		xs[i].set(i, i*10)
	}
	for i := 0; i < 3; i++ {
		println(xs[i].tag, xs[i].val)
	}
}`,
			"", "0 0\n1 10\n2 20\n",
		},
		{
			"pointer receiver method chaining other methods on self",
			`package main
type N struct { v byte }
func (n *N) inc()    { n.v++ }
func (n *N) double() { n.v *= 2 }
func (n *N) chain()  { n.inc(); n.double(); n.inc() }
func main() {
	x := N{v: 1}
	px := &x
	px.chain()
	println(x.v)
	px.chain()
	println(x.v)
}`,
			"", "5\n13\n",
		},
		{
			"pointer receiver multi-return divmod",
			`package main
type C struct { n byte }
func (c *C) divmod(d byte) (byte, byte) { return c.n / d, c.n % d }
func main() {
	c := C{n: 23}
	q, r := c.divmod(5)
	println(q, r)
	pc := &c
	q2, r2 := pc.divmod(7)
	println(q2, r2)
}`,
			"", "4 3\n3 2\n",
		},
		{
			"local struct type",
			`package main
func main() {
	type Point struct { x byte; y byte }
	p := Point{x: 3, y: 7}
	println(p.x, p.y)
}`,
			"", "3 7\n",
		},
		{
			"local struct type with nested struct",
			`package main
type Inner struct { v byte }
func main() {
	type Wrapper struct { a Inner; b byte }
	w := Wrapper{a: Inner{v: 10}, b: 20}
	println(w.a.v, w.b)
}`,
			"", "10 20\n",
		},
		{
			"range over array of size-1 struct",
			`package main
type P struct { x byte }
func main() {
	var a [4]P
	a[0] = P{x: 1}
	a[1] = P{x: 2}
	a[2] = P{x: 3}
	a[3] = P{x: 4}
	for _, p := range a {
		println(p.x)
	}
}`,
			"", "1\n2\n3\n4\n",
		},
		{
			"struct field with struct array",
			`package main
type Inner struct { a, b byte }
type Outer struct { items [3]Inner }
func main() {
	var o Outer
	o.items[0].a = 1
	o.items[0].b = 2
	o.items[1].a = 3
	o.items[2].b = 6
	println(o.items[0].a, o.items[1].a, o.items[2].b)
	a := o.items
	println(a[0].a, a[2].b)
}`,
			"", "1 3 6\n1 6\n",
		},
		{
			"struct field with struct array literal init",
			`package main
type Inner struct { a, b byte }
type Outer struct { items [3]Inner }
func main() {
	o := Outer{items: [3]Inner{{1, 2}, {3, 4}, {5, 6}}}
	println(o.items[0].a, o.items[1].b, o.items[2].a)
}`,
			"", "1 4 5\n",
		},
		{
			"struct field with nested struct array",
			`package main
type Inner struct { a, b byte }
type Outer struct { grid [2][3]Inner }
func main() {
	var o Outer
	o.grid[0][1].a = 5
	o.grid[1][2].b = 9
	println(o.grid[0][1].a, o.grid[1][2].b)
}`,
			"", "5 9\n",
		},
		{
			"multi-byte array field via pointer",
			`package main
type P struct { x [3]uint16 }
func mutate(p *P) {
	p.x[0] = 100
	p.x[1] = 40000
	p.x[2] = 60000
}
func main() {
	var p P
	mutate(&p)
	println(p.x[0], p.x[1], p.x[2])
}`,
			"", "100 40000 60000\n",
		},
		{
			"multi-return struct values",
			`package main
type P struct{ x, y byte }
func twoP() (P, P) { return P{x: 1, y: 2}, P{x: 3, y: 4} }
func main() {
	a, b := twoP()
	println(a.x, a.y, b.x, b.y)
}`,
			"", "1 2 3 4\n",
		},
		{
			"multi-return array values",
			`package main
func twoArr() ([3]byte, [3]byte) {
	a := [3]byte{1, 2, 3}
	b := [3]byte{4, 5, 6}
	return a, b
}
func main() {
	a, b := twoArr()
	println(a[0], a[2], b[0], b[2])
}`,
			"", "1 3 4 6\n",
		},
		{
			"parallel swap of byte fields on var-indexed size-1 struct array",
			`package main
type S struct { v byte }
func main() {
	var arr [3]S
	arr[0].v = 10
	arr[1].v = 20
	arr[2].v = 30
	i, j := byte(0), byte(2)
	arr[i].v, arr[j].v = arr[j].v, arr[i].v
	println(arr[0].v, arr[1].v, arr[2].v)
}`,
			"", "30 20 10\n",
		},
		{
			"chained selector read through struct field of var-indexed size-1 struct",
			`package main
type Inner struct { x byte }
type Outer struct { inner Inner }
func main() {
	var arr [3]Outer
	arr[0].inner.x = 10
	arr[1].inner.x = 20
	arr[2].inner.x = 30
	i := byte(1)
	println(arr[i].inner.x)
}`,
			"", "20\n",
		},
		{
			"chained selector write through struct field of var-indexed size-1 struct",
			`package main
type Inner struct { x byte }
type Outer struct { inner Inner }
func main() {
	var arr [3]Outer
	i := byte(1)
	arr[i].inner.x = 42
	println(arr[0].inner.x, arr[1].inner.x, arr[2].inner.x)
}`,
			"", "0 42 0\n",
		},
		// --- uint16 ---
		{
			"uint16 basics",
			`package main
func main() {
	var x uint16 = 300
	println(x)
	println(uint16(65535))
	println(byte(x))
	z := uint16(byte(200))
	println(z)
	var y uint16 = 65535
	y++
	println(y)
	y--
	println(y)
}`,
			"", "300\n65535\n44\n200\n0\n65535\n",
		},
		{
			"uint16 arithmetic",
			`package main
func main() {
	var a uint16 = 300
	var b uint16 = 400
	println(a + b)
	var zero uint16 = 0
	var one uint16 = 1
	println(zero - one)
	var x uint16 = 255
	x++
	println(x)
	x--
	println(x)
	println(a * 200)
	println(uint16(1000) / 7)
	println(uint16(10000) % 7)
	var max16 uint16 = 65535
	println(^max16)
	var five uint16 = 5
	println(-five)
	x = 1000
	x += 500
	println(x)
	x -= 200
	println(x)
}`,
			"", "700\n65535\n256\n255\n60000\n142\n4\n0\n65531\n1500\n1300\n",
		},
		{
			"uint16 comparison",
			`package main
func main() {
	var a uint16 = 300
	var b uint16 = 400
	if a < b { print("1") }
	if a <= b { print("2") }
	if b > a { print("3") }
	if b >= a { print("4") }
	if a == a { print("5") }
	if a != b { print("6") }
}`,
			"", "123456",
		},
		{
			"uint16 bitwise",
			`package main
func main() {
	println(uint16(255) | uint16(256))
	var a uint16 = 0xFF00
	var b uint16 = 0x0FF0
	println(a & b)
	println(a ^ b)
	println(a &^ b)
	println(uint16(1) << 8)
	println(uint16(512) >> 1)
}`,
			"", "511\n3840\n61680\n61440\n256\n256\n",
		},
		{
			"uint16 shift assigned to variable",
			`package main
func main() {
	x := uint16(1) << 8
	y := uint16(256) >> 4
	z := uint16(3) << 4
	println(x, y, z)
}`,
			"", "256 16 48\n",
		},
		{
			"uint16 function",
			`package main
func add16(a, b uint16) uint16 { return a + b }
func double(x uint16) uint16 { return x + x }
func main() {
	println(add16(100, 200))
	println(add16(1000, 2000))
	var a uint16 = 150
	println(double(a))
	println(double(500))
}`,
			"", "300\n3000\n300\n1000\n",
		},
		{
			"uint16 sum loop",
			`package main
func main() {
	var sum uint16 = 0
	var i uint16 = 1
	var limit uint16 = 100
	for i <= limit {
		sum = sum + i
		i++
	}
	println(sum)
}`,
			"", "5050\n",
		},
		{
			"uint16 divmod fused",
			`package main
func main() {
	a := uint16(1000)
	b := uint16(7)
	q := a / b
	r := a % b
	println(q, r)
	q2 := uint16(50000) / 1000
	r2 := uint16(50000) % 1000
	println(q2, r2)
}`,
			"", "142 6\n50 0\n",
		},
		{
			"uint16 fibonacci",
			`package main
func main() {
	var a uint16 = 0
	var b uint16 = 1
	for range byte(24) {
		a, b = b, a + b
	}
	println(a)
}`,
			"", "46368\n",
		},
		{
			"uint16 operation after comparison with literal",
			`package main
func main() {
	var x uint16 = 258
	if x > 256 {
		print("Y")
	}
	var y uint16 = 1000
	println(x + y)
}`,
			"", "Y1258\n",
		},
		{
			"uint16 struct field",
			`package main
type Sensor struct { id byte; value uint16; max uint16 }
func main() {
	s := Sensor{id: 1, value: 500, max: 1000}
	println(s.id, s.value)
	s.value = 255
	s.value++
	println(s.value)
	s.value += 100
	println(s.value)
	if s.value < s.max {
		println(s.max - s.value)
	}
}`,
			"", "1 500\n256\n356\n644\n",
		},
		{
			"uint16 pointer",
			`package main
func main() {
	x := uint16(1000)
	p := &x
	println(*p)
	*p = 2000
	println(x)
	*p++
	println(x)
	*p--
	println(x)
}`,
			"", "1000\n2000\n2001\n2000\n",
		},
		{
			"uint16 struct pointer swap",
			`package main
type Pair struct { a uint16; b uint16 }
func swap(p *Pair) {
	tmp := p.a
	p.a = p.b
	p.b = tmp
}
func inc(p *Pair) { p.a++ }
func main() {
	q := Pair{a: 255, b: 2000}
	inc(&q)
	println(q.a)
	swap(&q)
	println(q.a, q.b)
}`,
			"", "256\n2000 256\n",
		},
		{
			"uint16 multi return",
			`package main
func swap16(a, b uint16) (uint16, uint16) { return b, a }
func divmod16(a, b uint16) (uint16, uint16) { return a / b, a % b }
func main() {
	x, y := swap16(1000, 2000)
	println(x, y)
	println(divmod16(50000, 7))
}`,
			"", "2000 1000\n7142 6\n",
		},
		{
			"uint16 defer",
			`package main
func main() {
	defer println(uint16(999))
	println(uint16(111))
}`,
			"", "111\n999\n",
		},
		{
			"uint16 switch",
			`package main
func main() {
	x := uint16(300)
	switch x {
	case 100:
		print("A")
	case 300:
		print("B")
	default:
		print("C")
	}
}`,
			"", "B",
		},
		{
			"uint16 short decl from expression",
			`package main
func double(x uint16) uint16 { return x + x }
func main() {
	a := uint16(100)
	b := a + 50
	println(b)
	c := -a
	println(c)
	d := double(a)
	println(d)
	type S struct { v uint16 }
	s := S{v: 500}
	e := s.v
	println(e)
	p := uint16(10)
	ptr := &p
	f := *ptr
	println(f)
}`,
			"", "150\n65436\n200\n500\n10\n",
		},
		{
			"uint32 short decl from const",
			`package main
const big = 100000
func main() {
	x := big + big
	println(x)
}`,
			"", "200000\n",
		},
		{
			"uint16 range",
			`package main
func main() {
	sum := uint16(0)
	for i := range uint16(300) {
		sum += i
	}
	println(sum)
}`,
			"", "44850\n",
		},
		{
			"uint16 local const",
			`package main
func main() {
	const big uint16 = 10000
	var x uint16 = big
	println(x + 10)
	if x > 100 {
		println(x - 256)
	}
}`,
			"", "10010\n9744\n",
		},
		{
			"inner byte shadows outer uint16",
			`package main
func main() {
	x := uint16(50000)
	_ = x
	{
		x := byte(5)
		print(x * x)
	}
}`,
			"", "25",
		},
		{
			"discard multi-byte expression with blank identifier",
			`package main
func main() {
	x := uint16(50000)
	_ = x
	_ = x + 7
	print("ok")
}`,
			"", "ok",
		},
		{
			"parallel swap on uint16 array",
			`package main
func main() {
	a := [4]uint16{100, 200, 300, 400}
	i, j := byte(0), byte(3)
	a[i], a[j] = a[j], a[i]
	println(a[0], a[1], a[2], a[3])
}`,
			"", "400 200 300 100\n",
		},
		{
			"parallel swap on struct array via var index",
			`package main
type S struct { vals [3]uint16 }
func main() {
	var arr [2]S
	arr[0].vals[0] = 10
	arr[0].vals[1] = 20
	arr[1].vals[0] = 100
	arr[1].vals[1] = 200
	i, j := byte(0), byte(1)
	arr[i], arr[j] = arr[j], arr[i]
	println(arr[0].vals[0], arr[0].vals[1])
	println(arr[1].vals[0], arr[1].vals[1])
}`,
			"", "100 200\n10 20\n",
		},
		{
			"parallel swap through uint16 pointer params",
			`package main
func swap16(a, b *uint16) { *a, *b = *b, *a }
func main() {
	x, y := uint16(1000), uint16(2000)
	swap16(&x, &y)
	println(x, y)
}`,
			"", "2000 1000\n",
		},
		{
			"parallel swap of uint16 fields on var-indexed struct array element",
			`package main
type S struct { a, b uint16 }
func main() {
	var arr [2]S
	arr[1].a = 1000
	arr[1].b = 2000
	i := byte(1)
	arr[i].a, arr[i].b = arr[i].b, arr[i].a
	println(arr[1].a, arr[1].b)
}`,
			"", "2000 1000\n",
		},
		{
			"parallel swap of uint16 fields with const index",
			`package main
type S struct { a, b uint16 }
func main() {
	arr := [2]S{{0, 0}, {1000, 2000}}
	arr[1].a, arr[1].b = arr[1].b, arr[1].a
	println(arr[1].a, arr[1].b)
}`,
			"", "2000 1000\n",
		},
		{
			"parallel swap of uint16 fields on slice-of-struct element",
			`package main
type S struct { a, b uint16 }
func main() {
	s := []S{{0, 0}, {1000, 2000}}
	i := 1
	s[i].a, s[i].b = s[i].b, s[i].a
	println(s[i].a, s[i].b)
}`,
			"", "2000 1000\n",
		},
		{
			"parallel swap of uint16 fields through struct pointer",
			`package main
type S struct { a, b uint16 }
func swap(p *S) { p.a, p.b = p.b, p.a }
func main() {
	s := S{1000, 2000}
	swap(&s)
	println(s.a, s.b)
}`,
			"", "2000 1000\n",
		},
		// --- uint32 ---
		{
			"uint32 arithmetic",
			`package main
func main() {
	a := uint32(70000)
	b := uint32(80000)
	println(a + b)
	println(b - a)
	x := uint32(255)
	x++
	println(x)
	x--
	println(x)
	var zero uint32 = 0
	var one uint32 = 1
	println(zero - one)
}`,
			"", "150000\n10000\n256\n255\n4294967295\n",
		},
		{
			"uint32 comparison and bitwise",
			`package main
func main() {
	a := uint32(70000)
	b := uint32(80000)
	if a < b { print("1") }
	if a == a { print("2") }
	if b > a { print("3") }
	if a != b { print("4") }
	println()
	println(uint32(0xFF00) | uint32(0x00FF))
	println(uint32(0xFF00) & uint32(0x0FF0))
	println(uint32(42))
}`,
			"", "1234\n65535\n3840\n42\n",
		},
		{
			"uint32 multiply and divide",
			`package main
func main() {
	println(uint32(300) * uint32(200))
	q := uint32(1000) / 7
	r := uint32(1000) % (7)
	println(q, r)
	q2 := uint32(1000000) / 10000
	r2 := uint32(1000000) % 10000
	println(q2, r2)
}`,
			"", "60000\n142 6\n100 0\n",
		},
		{
			"uint32 shift",
			`package main
func main() {
	println(uint32(1) << 8)
}`,
			"", "256\n",
		},
		{
			"uint32 fibonacci",
			`package main
func main() {
	a, b := uint32(0), uint32(1)
	for range byte(45) {
		a, b = b, a + b
	}
	println(a)
}`,
			"", "1134903170\n",
		},
		{
			"uint32 constant",
			`package main
const big uint32 = 1000000
func main() {
	println(big)
	const x = 100000
	println(x + big)
}`,
			"", "1000000\n1100000\n",
		},
		{
			"uint32 element write requires explicit cast",
			`package main
func main() {
	a := make([]uint32, 2)
	a[0] = 50000
	a[1] = 100
	println(a[0], a[1])
	var b [2]uint32
	b[0] = 70000
	b[1] = uint32(byte(50))
	println(b[0], b[1])
}`,
			"", "50000 100\n70000 50\n",
		},
		{
			"uint32 struct field inc dec",
			`package main
type Counter struct { val uint32 }
func main() {
	c := Counter{val: 65535}
	c.val++
	println(c.val)
	c.val--
	println(c.val)
}`,
			"", "65536\n65535\n",
		},
		{
			"uint32 struct array variable index field read",
			`package main
type Item struct { id byte; val uint32 }
func main() {
	a := [2]Item{Item{id: 1, val: 100000}, Item{id: 2, val: 200000}}
	for i := range byte(2) {
		println(a[i].val)
	}
}`,
			"", "100000\n200000\n",
		},
		{
			"uint32 struct pointer field read",
			`package main
type Pair struct { a uint32; b uint32 }
func main() {
	p := Pair{a: 100000, b: 200000}
	ptr := &p
	println(ptr.a, ptr.b)
}`,
			"", "100000 200000\n",
		},
		{
			"uint32 function param and return",
			`package main
func double(x uint32) uint32 { return x + x }
func main() { println(double(500000)) }`,
			"", "1000000\n",
		},
		{
			"uint32 pointer deref and assign",
			`package main
func main() {
	x := uint32(70000)
	p := &x
	*p += uint32(80000)
	print(x)
}`,
			"", "150000",
		},
		{
			"uintN pointer deref widens narrower literal",
			`package main
func main() {
	var x uint32 = 0
	p := &x
	*p = 1000
	println(*p)
}`,
			"", "1000\n",
		},
		{
			"parallel swap through uint32 pointer params",
			`package main
func swap32(a, b *uint32) { *a, *b = *b, *a }
func main() {
	x, y := uint32(1000000), uint32(2000000)
	swap32(&x, &y)
	println(x, y)
}`,
			"", "2000000 1000000\n",
		},
		// --- uint64 ---
		{
			"uint64 basics",
			`package main
func main() {
	x := uint64(4294967295)
	x++
	println(x)
	x += 1000
	println(x)
	println(uint64(18446744073709551615))
}`,
			"", "4294967296\n4294968296\n18446744073709551615\n",
		},
		{
			"uint64 arithmetic",
			`package main
func main() {
	a := uint64(4294967296)
	b := uint64(1000000000)
	println(a + b)
	println(a - b)
	println(^uint64(0))
}`,
			"", "5294967296\n3294967296\n18446744073709551615\n",
		},
		{
			"uint64 fibonacci",
			`package main
func main() {
	a := uint64(0)
	b := uint64(1)
	for range byte(80) {
		a, b = b, a + b
	}
	println(a)
}`,
			"", "23416728348467685\n",
		},
		{
			"uint64 struct and pointer",
			`package main
type Big struct { val uint64 }
func main() {
	s := Big{val: 4294967296}
	s.val++
	println(s.val)
	s.val += 100
	println(s.val)
	p := &s.val
	*p += 1000
	println(s.val)
}`,
			"", "4294967297\n4294967397\n4294968397\n",
		},
		{
			"uint64 conversion",
			`package main
func main() {
	println(uint64(uint32(4294967295)))
	println(uint64(uint16(65535)))
	v := uint64(4294967296)
	println(byte(v))
}`,
			"", "4294967295\n65535\n0\n",
		},
		{
			"consecutive for-loops with same name at different widths",
			`package main
func main() {
	for i := uint8(0); i < uint8(3); i++ {
		println(i)
	}
	for i := uint16(0); i < uint16(3); i++ {
		println(i)
	}
	for i := uint32(0); i < uint32(3); i++ {
		println(i)
	}
	for i := uint64(0); i < uint64(3); i++ {
		println(i)
	}
}`,
			"", "0\n1\n2\n0\n1\n2\n0\n1\n2\n0\n1\n2\n",
		},
		{
			"nested for-loops shadowing same name at different widths",
			`package main
func main() {
	for i := uint32(10); i < uint32(13); i++ {
		for i := byte(0); i < 3; i++ {
			print(i)
			print(" ")
		}
		println(i)
	}
}`,
			"", "0 1 2 10\n0 1 2 11\n0 1 2 12\n",
		},
		{
			"inner var shadows outer const",
			`package main
func main() {
	const x = 5
	{
		x := byte(1)
		print(x)
	}
	print(" ")
	print(x)
}`,
			"", "1 5",
		},
		{
			"inner var shadows outer uint16 const",
			`package main
func main() {
	const x uint16 = 50000
	{
		x := byte(7)
		print(x)
	}
	print(" ")
	print(x)
}`,
			"", "7 50000",
		},
		{
			"standalone block shadows outer with different width",
			`package main
func main() {
	v := uint64(0x0123456789ABCDEF)
	{
		x := v >> 1
		println(x)
	}
	x := uint32(0xDEADBEEF)
	y := uint32(0x12345678)
	println(x ^ y)
}`,
			"", "40992764608243447\n3432638615\n",
		},
		{
			"uint64 constant",
			`package main
const small uint64 = 100
const big uint64 = 5000000000
const huge = 18000000000
func main() {
	println(small + 50000)
	println(big + 1)
	const local uint64 = 200
	println(local * 3)
	println(huge + 1)
}`,
			"", "50100\n5000000001\n600\n18000000001\n",
		},
		{
			"uint64 const above int64 range",
			`package main
const (
	MaxU64    uint64 = 18446744073709551615
	JustAbove uint64 = 9223372036854775808
)
func main() {
	println(MaxU64)
	println(JustAbove)
	println(MaxU64 - JustAbove)
}`,
			"", "18446744073709551615\n9223372036854775808\n9223372036854775807\n",
		},
		{
			"uintN type cast in const expression",
			`package main
const (
	A uint16 = uint16(1000)
	B uint32 = uint32(100000)
	C uint64 = uint64(10000000000)
	D uint32 = uint32(1000) + uint32(500)
)
func main() {
	println(A, B, C, D)
}`,
			"", "1000 100000 10000000000 1500\n",
		},
		{
			"short decl from multi-byte array element",
			`package main
func main() {
	a := [3]uint32{50000, 100, 2000000}
	x := a[0]
	y := a[1]
	if x > y { print("yes ") } else { print("no ") }
	println(x, y)
}`,
			"", "yes 50000 100\n",
		},
		{
			"min and max for multi-byte ints",
			`package main
func main() {
	a := uint64(8000000000)
	b := uint64(15000000000000000000)
	c := uint64(100)
	println(min(a, b, c))
	println(max(a, b, c))
}`,
			"", "100\n15000000000000000000\n",
		},
		{
			"uint64 divmod",
			`package main
func main() {
	a := uint64(18446744073709551615)
	b := uint64(9999999999)
	q := a / b
	r := a % b
	println(q, r)
}`,
			"", "1844674407 5554226022\n",
		},
		{
			"uint16 array",
			`package main
func main() {
	var a [3]uint16
	a[0] = 60000
	a[1] = 40000
	a[2] = 1000
	for i, v := range a {
		if i > 0 { print(" ") }
		print(v)
	}
}`,
			"", "60000 40000 1000",
		},
		{
			"uint16 array literal",
			`package main
func main() {
	a := [3]uint16{100, 2000, 60000}
	println(a[0], a[1], a[2])
}`,
			"", "100 2000 60000\n",
		},
		{
			"uint16 array return",
			`package main
func f() [3]uint16 {
	var a [3]uint16
	a[0] = 100
	a[1] = 1000
	a[2] = 30000
	return a
}
func main() {
	a := f()
	println(a[0], a[1], a[2])
}`,
			"", "100 1000 30000\n",
		},
		{
			"uint32 slice with append and range",
			`package main
func main() {
	s := []uint32{100000, 200000}
	s = append(s, 3000000000)
	s = append(s, 0)
	for i, v := range s {
		if i > 0 { print(" ") }
		print(v)
	}
}`,
			"", "100000 200000 3000000000 0",
		},
		{
			"uint16 slice param and return",
			`package main
func makeSlice() []uint16 { return []uint16{100, 40000, 60000} }
func sum(s []uint16) uint16 {
	r := uint16(0)
	for _, v := range s { r += v }
	return r
}
func main() { println(sum(makeSlice())) }`,
			"", "34564\n",
		},
		{
			"index into call result",
			`package main
func makeSlice() []uint16 { return []uint16{100, 40000, 60000} }
func main() {
	x := makeSlice()[1]
	println(x)
}`,
			"", "40000\n",
		},
		{
			"index into nested multi-byte array",
			`package main
func main() {
	var a [2][3]uint16
	a[0][1] = 200
	a[1][2] = 60000
	x := a[0][1]
	y := a[1][2]
	println(x, y)
}`,
			"", "200 60000\n",
		},
		{
			"nested multi-byte array variable index both levels",
			`package main
func main() {
	var a [2][3]uint16
	for i := byte(0); i < 2; i++ {
		for j := byte(0); j < 3; j++ {
			a[i][j] = uint16(i)*10 + uint16(j)
		}
	}
	for i := byte(0); i < 2; i++ {
		for j := byte(0); j < 3; j++ {
			if i*3+j > 0 { print(" ") }
			print(a[i][j])
		}
	}
	println()
}`,
			"", "0 1 2 10 11 12\n",
		},
		{
			"nested multi-byte array outer var inner const",
			`package main
func main() {
	var a [3][2]uint16
	a[0][0] = 11; a[0][1] = 22
	a[1][0] = 33; a[1][1] = 44
	a[2][0] = 55; a[2][1] = 66
	for i := byte(0); i < 3; i++ {
		println(a[i][0], a[i][1])
	}
}`,
			"", "11 22\n33 44\n55 66\n",
		},
		{
			"parenthesized index of multi-byte slice",
			`package main
func main() {
	a := []uint16{100, 40000, 60000}
	x := (a[1])
	y := ((a)[2])
	println(x, y)
}`,
			"", "40000 60000\n",
		},
		{
			"slice through parens",
			`package main
func main() {
	a := []uint16{100, 200, 300}
	t := (a)[1:]
	println(t[0], t[1])
}`,
			"", "200 300\n",
		},
		{
			"slice cast identity",
			`package main
func main() {
	a := []uint16{100, 200, 300}
	t := []uint16(a)
	println(t[0], t[1], t[2])
}`,
			"", "100 200 300\n",
		},
		{
			"slice on call result multi-byte",
			`package main
func mk16() []uint16 { return []uint16{100, 200, 300, 400, 500} }
func main() {
	t := mk16()[1:4]
	println(t[0], t[1], t[2])
	sum := uint16(0)
	for _, v := range t { sum += v }
	println(sum)
}`,
			"", "200 300 400\n900\n",
		},
		{
			"struct field multi-byte array literal init",
			`package main
type P struct { x [3]uint16 }
func main() {
	p := P{x: [3]uint16{100, 40000, 60000}}
	println(p.x[0], p.x[1], p.x[2])
}`,
			"", "100 40000 60000\n",
		},
		{
			"nested multi-byte array literal init",
			`package main
func main() {
	a := [2][3]uint16{{100, 200, 300}, {400, 500, 600}}
	println(a[0][1], a[1][2])
}`,
			"", "200 600\n",
		},
		{
			"struct field nested array copy and chained index",
			`package main
type P struct { arr [3][2]byte }
func main() {
	var p P
	p.arr[0][0] = 1
	p.arr[1][1] = 4
	p.arr[2][1] = 6
	println(p.arr[1][1])
	a := p.arr
	println(a[0][0], a[2][1])
	b := p.arr[1]
	println(b[0], b[1])
}`,
			"", "4\n1 6\n0 4\n",
		},
		{
			"struct field nested array literal init",
			`package main
type P struct { arr [3][2]byte }
func main() {
	p := P{arr: [3][2]byte{{1, 2}, {3, 4}, {5, 6}}}
	println(p.arr[0][0], p.arr[1][1], p.arr[2][0])
}`,
			"", "1 4 5\n",
		},
		{
			"struct field multi-byte nested array",
			`package main
type Q struct { grid [2][3]uint16 }
func main() {
	q := Q{}
	q.grid[0][0] = 100
	q.grid[1][2] = 60000
	println(q.grid[0][0], q.grid[1][2])
	a := q.grid
	println(a[0][0], a[1][2])
}`,
			"", "100 60000\n100 60000\n",
		},
		{
			"chained selector struct field copy",
			`package main
type Inner struct { a, b byte }
type Outer struct { inner Inner }
func main() {
	o := Outer{inner: Inner{a: 3, b: 5}}
	i := o.inner
	println(i.a, i.b)
}`,
			"", "3 5\n",
		},
		{
			"address of nested struct field",
			`package main
type P struct { x, y byte }
type Q struct { p P; n byte }
func main() {
	q := Q{p: P{x: 3, y: 5}, n: 7}
	pp := &q.p
	println(pp.x, pp.y)
	pn := &q.n
	*pn = 99
	println(q.n)
}`,
			"", "3 5\n99\n",
		},
		{
			"address of multi-byte array",
			`package main
func main() {
	a := [3]uint16{100, 200, 300}
	pa := &a
	pa[1] = 500
	println(a[0], a[1], a[2])
	println(pa[0], pa[1], pa[2])
}`,
			"", "100 500 300\n100 500 300\n",
		},
		{
			"address of multi-byte array element",
			`package main
func main() {
	a := [3]uint64{100, 8000000000, 99}
	p := &a[1]
	*p = *p + uint64(1000)
	for i, v := range a { if i > 0 { print(" ") }; print(v) }
}`,
			"", "100 8000001000 99",
		},
		{
			"nested multi-byte array",
			`package main
func main() {
	var a [2][2]uint16
	a[0][0] = 100
	a[0][1] = 40000
	a[1][0] = 60000
	a[1][1] = 1
	println(a[0][0], a[0][1], a[1][0], a[1][1])
}`,
			"", "100 40000 60000 1\n",
		},
		{
			"struct field array of multi-byte",
			`package main
type S struct { vals [3]uint16 }
func main() {
	s := S{}
	s.vals[0] = 1000
	s.vals[1] = 40000
	s.vals[2] = 60000
	for i, v := range s.vals { if i > 0 { print(" ") }; print(v) }
}`,
			"", "1000 40000 60000",
		},
		{
			"range over struct array with multi-byte fields",
			`package main
type Pt struct { x, y uint32 }
func main() {
	a := [2]Pt{Pt{x: 100000, y: 200000}, Pt{x: 3000000000, y: 50}}
	for i, p := range a {
		if i > 0 { print(" ") }
		print(p.x); print(":"); print(p.y)
	}
}`,
			"", "100000:200000 3000000000:50",
		},
		{
			"variable-index write to struct-array multi-byte field",
			`package main
type R struct { val uint32 }
func main() {
	rs := [2]R{{val: 100}, {val: 200}}
	for i := 0; i < 2; i++ {
		rs[i].val = rs[i].val + 1
	}
	print(rs[0].val)
	print(" ")
	print(rs[1].val)
}`,
			"", "101 201",
		},
		{
			"keyed multi-byte array literal",
			`package main
func main() {
	a := [5]uint32{0: 100, 2: 2000000, 4: 99999}
	for i, v := range a {
		if i > 0 { print(" ") }
		print(v)
	}
}`,
			"", "100 0 2000000 0 99999",
		},
		{
			"slice of multi-byte array",
			`package main
func main() {
	a := [4]uint64{10, 8000000000, 15000000000000000000, 99}
	s := a[1:3]
	for i, v := range s { if i > 0 { print(" ") }; print(v) }
}`,
			"", "8000000000 15000000000000000000",
		},
		{
			"range without value over multi-byte slice",
			`package main
func main() {
	s := []uint16{100, 40000, 60000}
	cnt := byte(0)
	for range s { cnt++ }
	println(cnt)
}`,
			"", "3\n",
		},
		{
			"address of multi-byte slice element",
			`package main
func main() {
	s := []uint16{100, 40000, 60000}
	p := &s[1]
	*p = *p + uint16(1)
	for i, v := range s { if i > 0 { print(" ") }; print(v) }
}`,
			"", "100 40001 60000",
		},
		{
			"address of var-indexed struct array field",
			`package main
type S struct { a uint16 }
func bump(p *uint16) { *p += 5 }
func main() {
	var arr [3]S
	arr[1].a = 1000
	i := byte(1)
	bump(&arr[i].a)
	println(arr[1].a)
}`,
			"", "1005\n",
		},
		{
			"address of slice-of-struct field",
			`package main
type S struct { a uint16 }
func bump(p *uint16) { *p += 5 }
func main() {
	s := []S{{1000}}
	bump(&s[0].a)
	println(s[0].a)
}`,
			"", "1005\n",
		},
		{
			"chained selector through struct field of var-indexed struct array",
			`package main
type Inner struct { x uint16 }
type Outer struct { inner Inner }
func main() {
	var arr [2]Outer
	arr[0].inner.x = 1000
	arr[1].inner.x = 2000
	i := byte(1)
	println(arr[i].inner.x)
}`,
			"", "2000\n",
		},
		{
			"multi-return into var-indexed struct fields",
			`package main
type S struct { a, b uint16 }
func make16() (uint16, uint16) { return 1234, 5678 }
func main() {
	var arr [2]S
	i := byte(1)
	arr[i].a, arr[i].b = make16()
	println(arr[1].a, arr[1].b)
}`,
			"", "1234 5678\n",
		},
		{
			"composite-literal assign to chained var-indexed struct field",
			`package main
type Inner struct { a, b uint16 }
type Outer struct { inner Inner }
func main() {
	var arr [3]Outer
	i := byte(1)
	arr[i].inner = Inner{a: 100, b: 200}
	println(arr[1].inner.a, arr[1].inner.b)
}`,
			"", "100 200\n",
		},
		{
			"return struct value at var-indexed array element",
			`package main
type S struct { a, b uint16 }
func main() {
	var arr [3]S
	arr[1].a = 100
	arr[1].b = 200
	i := byte(1)
	s := arr[i]
	println(s.a, s.b)
}`,
			"", "100 200\n",
		},
		{
			"return slice-of-struct element by value",
			`package main
type S struct { a, b uint16 }
func get(s []S, i byte) S { return s[i] }
func main() {
	s := []S{{0, 0}, {100, 200}}
	r := get(s, 1)
	println(r.a, r.b)
}`,
			"", "100 200\n",
		},
		{
			"return var-indexed struct array element from function",
			`package main
type S struct { a, b uint16 }
var arr [3]S
func get(i byte) S { return arr[i] }
func main() {
	arr[1].a = 100
	arr[1].b = 200
	s := get(1)
	println(s.a, s.b)
}`,
			"", "100 200\n",
		},
		{
			"struct equality at var-indexed array elements",
			`package main
type S struct { a, b uint16 }
func main() {
	var arr [3]S
	arr[0] = S{100, 200}
	arr[1] = S{100, 200}
	arr[2] = S{100, 201}
	i, j := byte(0), byte(1)
	if arr[i] == arr[j] { println("eq01") }
	j = 2
	if arr[i] != arr[j] { println("neq02") }
	if arr[i] == (S{100, 200}) { println("eqlit") }
}`,
			"", "eq01\nneq02\neqlit\n",
		},
		{
			"whole-struct copy between var-indexed array elements",
			`package main
type S struct { a, b uint16 }
func main() {
	var arr [3]S
	arr[0] = S{100, 200}
	i, j := byte(0), byte(1)
	arr[j] = arr[i]
	println(arr[1].a, arr[1].b)
}`,
			"", "100 200\n",
		},
		{
			"sub-array equality on var-indexed 2D array",
			`package main
func main() {
	m := [3][3]byte{{1, 2, 3}, {4, 5, 6}, {1, 2, 3}}
	i, j := byte(0), byte(2)
	if m[i] == m[j] { println("eq") }
	j = 1
	if m[i] != m[j] { println("neq") }
}`,
			"", "eq\nneq\n",
		},
		{
			"multi-byte slice field read and write at var-indexed struct array",
			`package main
type S struct { xs []uint16 }
func main() {
	var arr [2]S
	arr[1].xs = []uint16{10, 20, 30}
	i := byte(1)
	arr[i].xs[1] = 999
	for j, v := range arr[i].xs {
		if j > 0 { print(" ") }
		print(v)
	}
	println()
}`,
			"", "10 999 30\n",
		},
		{
			"address-of through chained selector on var-indexed struct array",
			`package main
type Inner struct { val uint16 }
type Outer struct { inner Inner }
func bump(p *uint16) { *p += 2 }
func main() {
	var arr [3]Outer
	arr[1].inner.val = 100
	i := byte(1)
	bump(&arr[i].inner.val)
	println(arr[1].inner.val)
}`,
			"", "102\n",
		},
		{
			"address-of slice element of slice field at var-indexed struct",
			`package main
type S struct { xs []uint16 }
func bump(p *uint16) { *p += 10 }
func main() {
	var arr [2]S
	arr[1].xs = []uint16{100, 200, 300}
	i := byte(1)
	bump(&arr[i].xs[1])
	for j, v := range arr[i].xs {
		if j > 0 { print(" ") }
		print(v)
	}
	println()
}`,
			"", "100 210 300\n",
		},
		{
			"multi-byte slice field via struct pointer",
			`package main
type S struct { xs []uint16 }
func sum(s *S) uint16 {
	var t uint16
	for _, v := range s.xs { t += v }
	return t
}
func main() {
	x := S{xs: []uint16{100, 200, 300}}
	println(sum(&x))
}`,
			"", "600\n",
		},
		{
			"chained array-field index on var-indexed struct array",
			`package main
type Cell struct { val uint16 }
type Row struct { cells [3]Cell }
func main() {
	var rows [2]Row
	rows[0].cells[1].val = 100
	rows[1].cells[2].val = 200
	i, j, k, l := byte(0), byte(1), byte(1), byte(2)
	println(rows[i].cells[j].val + rows[k].cells[l].val)
}`,
			"", "300\n",
		},
		{
			"range index-only over slice field",
			`package main
type S struct { xs []uint16 }
func main() {
	x := S{xs: []uint16{100, 200, 300}}
	for i := range x.xs { x.xs[i] += 10 }
	for i, v := range x.xs {
		if i > 0 { print(" ") }
		print(v)
	}
	println()
}`,
			"", "110 210 310\n",
		},
		{
			"range over pointer-to-array",
			`package main
func bump(a *[5]uint16) {
	for i := range a { a[i] += 1 }
}
func main() {
	arr := [5]uint16{100, 200, 300, 400, 500}
	bump(&arr)
	for i, v := range arr {
		if i > 0 { print(" ") }
		print(v)
	}
	println()
}`,
			"", "101 201 301 401 501\n",
		},
		{
			"equality of chained struct fields",
			`package main
type Inner struct { val uint16 }
type Outer struct { a, b Inner }
func main() {
	var arr [3]Outer
	arr[0].a = Inner{100}
	arr[0].b = Inner{100}
	arr[1].a = Inner{200}
	arr[1].b = Inner{300}
	i := byte(0)
	if arr[i].a == arr[i].b { println("eq") }
	i = 1
	if arr[i].a != arr[i].b { println("neq") }
}`,
			"", "eq\nneq\n",
		},
		{
			"chained struct field copy",
			`package main
type Inner struct { a, b uint16 }
type Outer struct { x, y Inner }
func main() {
	var arr [3]Outer
	arr[1].x = Inner{100, 200}
	i := byte(1)
	arr[i].y = arr[i].x
	println(arr[1].x.a, arr[1].x.b)
	println(arr[1].y.a, arr[1].y.b)
}`,
			"", "100 200\n100 200\n",
		},
		{
			"parallel swap of string fields on chained var-indexed struct",
			`package main
type Inner struct { a, b string }
type Outer struct { inner Inner }
func main() {
	var arr [3]Outer
	arr[1].inner.a = "hello"
	arr[1].inner.b = "world"
	i := byte(1)
	arr[i].inner.a, arr[i].inner.b = arr[i].inner.b, arr[i].inner.a
	println(arr[1].inner.a)
	println(arr[1].inner.b)
}`,
			"", "world\nhello\n",
		},
		{
			"array-literal assignment to multi-byte array field",
			`package main
type S struct { vals [3]uint16 }
func main() {
	var s S
	s.vals = [3]uint16{1000, 2000, 3000}
	println(s.vals[0], s.vals[1], s.vals[2])
}`,
			"", "1000 2000 3000\n",
		},
		{
			"multi-byte array field copy and equality",
			`package main
type S struct { vals [3]uint16 }
func main() {
	var arr [3]S
	arr[0].vals = [3]uint16{100, 200, 300}
	i, j := byte(0), byte(1)
	arr[j].vals = arr[i].vals
	if arr[i].vals == arr[j].vals { println("eq") }
	arr[j].vals[2] = 9999
	if arr[i].vals != arr[j].vals { println("neq") }
}`,
			"", "eq\nneq\n",
		},
		{
			"multi-return with multi-byte slice param",
			`package main
func minmax(a []uint64) (uint64, uint64) {
	mn := a[0]
	mx := a[0]
	for _, v := range a {
		if v < mn { mn = v }
		if v > mx { mx = v }
	}
	return mn, mx
}
func main() {
	mn, mx := minmax([]uint64{8000000000, 100, 15000000000000000000, 2000})
	println(mn, mx)
}`,
			"", "100 15000000000000000000\n",
		},
		{
			"multi-byte pointer param",
			`package main
func inc16(p *uint16) { *p++ }
func add32(p *uint32, v uint32) { *p += v }
func dbl64(p *uint64) { *p *= 2 }
func main() {
	a := uint16(65534)
	inc16(&a)
	println(a)
	inc16(&a)
	println(a)
	b := uint32(99999)
	add32(&b, uint32(1))
	println(b)
	c := uint64(8000000000)
	dbl64(&c)
	println(c)
}`,
			"", "65535\n0\n100000\n16000000000\n",
		},
		// --- Strings ---
		{
			"string variable len index range print",
			`package main
func main() {
	s := "hello"
	print(len(s))
	print(" ")
	print(s[0])
	print(" ")
	for i, c := range s {
		if i > 0 { print(",") }
		print(c)
	}
	print(" ")
	println(s)
}`,
			"", "5 104 104,101,108,108,111 hello\n",
		},
		{
			"var string declaration",
			`package main
func main() {
	x := byte(5)
	var s string
	if x > 3 {
		s = "big"
	} else {
		s = "small"
	}
	println(s)
	if s+"!" == "big!" { println("yes") }
}`,
			"", "big\nyes\n",
		},
		{
			"string equality and inequality",
			`package main
func main() {
	a := "hello"
	b := "hello"
	c := "world"
	if a == b { print("eq ") }
	if a == c { print("BAD ") }
	if a != c { print("ne") }
}`,
			"", "eq ne",
		},
		{
			"string concatenation",
			`package main
func main() {
	a := "hello"
	b := " world"
	c := a + b
	println(c)
	println(len(c))
}`,
			"", "hello world\n11\n",
		},
		{
			"string concat with literal operand",
			`package main
func main() {
	s := "hi"
	println(s + "a")
	println(s + "b")
}`,
			"", "hia\nhib\n",
		},
		{
			"string lexicographic ordering",
			`package main
func main() {
	a := "abc"
	b := "abd"
	c := "abc"
	d := "ab"
	if a < b { print("1 ") }
	if b < a { print("X ") }
	if a > d { print("2 ") }
	if a <= c { print("3 ") }
	if a >= c { print("4") }
}`,
			"", "1 2 3 4",
		},
		{
			"string compound assign",
			`package main
func main() {
	s := "hello"
	s += " world"
	println(s)
	s += "!"
	println(s)
}`,
			"", "hello world\nhello world!\n",
		},
		{
			"string function param and return",
			`package main
func greet(name string) string {
	return "hi " + name
}
func main() {
	println(greet("alice"))
	s := "bob"
	println(greet(s))
}`,
			"", "hi alice\nhi bob\n",
		},
		{
			"string and byte-slice conversion",
			`package main
func main() {
	s := "hello"
	b := []byte(s)
	print(b[0])
	print(" ")
	bs := []byte{'h', 'i'}
	t := string(bs)
	println(t)
}`,
			"", "104 hi\n",
		},
		{
			"string and byte-slice conversion of literals and concat",
			`package main
func main() {
	t := []byte("hello"); t[0] = 'X'; println(string(t))
	u := string("world"); println(u)
	a := "foo"; b := "bar"
	v := []byte(a + b); v[0] = 'Y'; println(string(v))
}`,
			"", "Xello\nworld\nYoobar\n",
		},
		{
			"string compare with literal operand",
			`package main
func main() {
	a := "abc"
	if a == "abc" { print("1 ") }
	if a != "abd" { print("2 ") }
	if a < "abd" { print("3 ") }
	if "" < a { print("4 ") }
	if a > "" { print("5") }
}`,
			"", "1 2 3 4 5",
		},
		{
			"empty string operations",
			`package main
func main() {
	a := ""
	println(len(a))
	if a == "" { print("eq ") }
	if a < "x" { print("lt") }
	println()
}`,
			"", "0\neq lt\n",
		},
		{
			"string field in struct",
			`package main
type P struct {
	name string
	age  byte
}
func main() {
	p := P{name: "alice", age: 30}
	println(p.name)
	println(p.age)
}`,
			"", "alice\n30\n",
		},
		{
			"byte slice field in struct",
			`package main
type P struct {
	xs []byte
	n  byte
}
func main() {
	p := P{xs: []byte{1, 2, 3, 4, 5}, n: 7}
	println(p.xs[0], p.xs[2], p.xs[4], p.n)
	p.xs = append(p.xs, byte(99))
	println(p.xs[5], len(p.xs))
	p.xs[0] = 100
	println(p.xs[0])
}`,
			"", "1 3 5 7\n99 6\n100\n",
		},
		{
			"uint16 slice field in struct",
			`package main
type S struct { xs []uint16 }
func main() {
	s := S{xs: []uint16{100, 40000, 60000}}
	println(s.xs[0], s.xs[1], s.xs[2])
	s.xs[1] = 50000
	s.xs = append(s.xs, 99)
	println(s.xs[1], s.xs[3], len(s.xs))
}`,
			"", "100 40000 60000\n50000 99 4\n",
		},
		{
			"struct slice field in struct",
			`package main
type Q struct{ x byte }
type S struct { ps []Q }
func main() {
	s := S{ps: []Q{{x: 1}, {x: 2}, {x: 3}}}
	println(s.ps[0].x, s.ps[1].x, s.ps[2].x)
}`,
			"", "1 2 3\n",
		},
		{
			"slice field copy bind",
			`package main
type S struct { vals []uint16 }
func main() {
	s := S{vals: []uint16{100, 200}}
	t := s.vals
	println(len(t), t[0], t[1])
}`,
			"", "2 100 200\n",
		},
		{
			"struct slice field write through index",
			`package main
type Q struct{ x byte }
func main() {
	ps := []Q{{x: 1}, {x: 2}, {x: 3}}
	ps[1].x = 99
	println(ps[0].x, ps[1].x, ps[2].x)
}`,
			"", "1 99 3\n",
		},
		{
			"slice of slice field in struct",
			`package main
type Q struct{ rows [][]byte }
func main() {
	q := Q{}
	q.rows = append(q.rows, []byte{10, 20})
	q.rows = append(q.rows, []byte{30, 40, 50})
	println(len(q.rows), q.rows[0][1], q.rows[1][2])
}`,
			"", "2 20 50\n",
		},
		{
			"append struct literal to slice",
			`package main
type Q struct{ x byte }
func main() {
	s := []Q{{x: 1}, {x: 2}}
	s = append(s, Q{x: 99})
	println(s[0].x, s[1].x, s[2].x, len(s))
}`,
			"", "1 2 99 3\n",
		},
		{
			"string field compare equality",
			`package main
type P struct { name string }
func main() {
	p := P{name: "abc"}
	q := P{name: "abc"}
	r := P{name: "xyz"}
	if p.name == q.name { println("eq") }
	if p.name != r.name { println("ne") }
	if p.name == "abc" { println("lit") }
}`,
			"", "eq\nne\nlit\n",
		},
		{
			"string field concat and lex compare",
			`package main
type P struct { name string }
func main() {
	p := P{name: "foo"}
	q := P{name: "bar"}
	println(p.name + q.name)
	println(p.name + "!")
	if q.name < p.name { println("q<p") }
}`,
			"", "foobar\nfoo!\nq<p\n",
		},
		{
			"declare string variable from struct field",
			`package main
type P struct { name string }
func main() {
	p := P{name: "hello"}
	t := p.name
	println(t)
	println(len(t))
}`,
			"", "hello\n5\n",
		},
		{
			"string field in struct array variable index",
			`package main
type P struct { name string }
func main() {
	ps := [3]P{{name: "foo"}, {name: "bar"}, {name: "baz"}}
	for i := 0; i < 3; i++ {
		println(ps[i].name)
	}
}`,
			"", "foo\nbar\nbaz\n",
		},
		{
			"string field compound assign",
			`package main
type P struct { name string }
func main() {
	p := P{name: "foo"}
	p.name += "bar"
	println(p.name)
}`,
			"", "foobar\n",
		},
		{
			"string field via pointer-to-struct",
			`package main
type P struct { name string }
func main() {
	p := P{name: "old"}
	pp := &p
	println(pp.name)
	pp.name = "new"
	println(p.name)
	pp.name += "!"
	println(p.name)
}`,
			"", "old\nnew\nnew!\n",
		},
		{
			"string field in slice of structs",
			`package main
type P struct { name string }
func main() {
	ps := make([]P, 3)
	ps[0].name = "a"
	ps[1].name = "b"
	ps[2].name = "c"
	for i := 0; i < len(ps); i++ {
		println(ps[i].name)
	}
}`,
			"", "a\nb\nc\n",
		},
		{
			"concat with string field via array index",
			`package main
type P struct { name string }
func main() {
	items := [3]P{{name: "a"}, {name: "b"}, {name: "c"}}
	for i := 0; i < 3; i++ {
		println("- " + items[i].name)
	}
}`,
			"", "- a\n- b\n- c\n",
		},
		{
			"concat with string field via chained selector",
			`package main
type Inner struct { tag string }
type Outer struct { inner Inner }
func main() {
	o := Outer{inner: Inner{tag: "hello"}}
	println(">> " + o.inner.tag)
	if o.inner.tag == "hello" { println("eq") }
}`,
			"", ">> hello\neq\n",
		},
		{
			"slice literal of struct literals with string field",
			`package main
type P struct { name string }
func main() {
	ps := []P{{name: "foo"}, {name: "bar"}}
	for i := 0; i < len(ps); i++ {
		println(ps[i].name)
	}
}`,
			"", "foo\nbar\n",
		},
		{
			"struct with []string field",
			`package main
type S struct { items []string }
func main() {
	s := S{items: []string{"alpha", "beta", "gamma"}}
	for i := 0; i < len(s.items); i++ {
		println(s.items[i])
	}
}`,
			"", "alpha\nbeta\ngamma\n",
		},
		{
			"slice string field",
			`package main
type P struct { name string }
func main() {
	p := P{name: "hello world"}
	println(p.name[0:5])
	println(p.name[6:])
	println(p.name[:5])
}`,
			"", "hello\nworld\nhello\n",
		},
		{
			"three way string concatenation",
			`package main
func main() {
	a := "foo"
	b := "bar"
	c := "baz"
	println(a + b + c)
	println(a + "-" + b + "-" + c)
}`,
			"", "foobarbaz\nfoo-bar-baz\n",
		},
		{
			"slice expression in string compare",
			`package main
func main() {
	s := "hello"
	for i := 0; i < len(s); i++ {
		if s[i:i+1] == "l" {
			println(i)
		}
	}
}`,
			"", "2\n3\n",
		},
		{
			"byte to string conversion",
			`package main
func main() {
	t := string(byte('A'))
	println(t)
	println(len(t))
}`,
			"", "A\n1\n",
		},
		{
			"byte to string accumulator loop",
			`package main
func main() {
	r := ""
	for i := byte(0); i < 5; i++ {
		r += string(byte('a') + i)
	}
	println(r)
}`,
			"", "abcde\n",
		},
		{
			"switch on string tag",
			`package main
func main() {
	s := "hello"
	switch s {
	case "world":
		println("world")
	case "hello":
		println("hi")
	default:
		println("other")
	}
}`,
			"", "hi\n",
		},
		{
			"string const concatenation",
			`package main
const HELLO = "hello"
const WORLD = "world"
const GREETING = HELLO + ", " + WORLD
func main() {
	println(HELLO)
	println(GREETING)
}`,
			"", "hello\nhello, world\n",
		},
		{
			"parallel swap of string variables",
			`package main
func main() {
	a := "alpha"
	b := "beta"
	a, b = b, a
	println(a, b)
}`,
			"", "beta alpha\n",
		},
		{
			"parallel swap of string fields",
			`package main
type T struct { name string }
func swap(p *T, q *T) { p.name, q.name = q.name, p.name }
func main() {
	a := T{name: "alice"}
	b := T{name: "bob"}
	swap(&a, &b)
	println(a.name, b.name)
}`,
			"", "bob alice\n",
		},
		{
			"parallel swap of string field via value-base struct",
			`package main
type T struct { s string }
func main() {
	a := T{s: "alpha"}
	b := T{s: "beta"}
	a.s, b.s = b.s, a.s
	println(a.s, b.s)
}`,
			"", "beta alpha\n",
		},
		{
			"parallel swap of slice-of-strings elements",
			`package main
func main() {
	a := []string{"alpha", "beta", "gamma"}
	a[0], a[2] = a[2], a[0]
	println(a[0], a[1], a[2])
}`,
			"", "gamma beta alpha\n",
		},
		{
			"parallel swap of slice-of-strings via variable index",
			`package main
func main() {
	a := []string{"x", "y", "z", "w"}
	for i := byte(0); i < 2; i++ {
		j := byte(3) - i
		a[i], a[j] = a[j], a[i]
	}
	println(a[0], a[1], a[2], a[3])
}`,
			"", "w z y x\n",
		},
		{
			"parallel swap of array-of-strings elements",
			`package main
func main() {
	a := [3]string{"alpha", "beta", "gamma"}
	a[0], a[2] = a[2], a[0]
	for i := byte(0); i < 1; i++ {
		j := byte(2) - i
		a[i], a[j] = a[j], a[i]
	}
	for i := 0; i < 3; i++ { println(a[i]) }
}`,
			"", "alpha\nbeta\ngamma\n",
		},
		{
			"parallel assign with string literal RHS",
			`package main
func main() {
	a := []string{"x", "y"}
	a[0], a[1] = "first", "second"
	println(a[0], a[1])
}`,
			"", "first second\n",
		},
		{
			"range over slice literal of strings",
			`package main
func main() {
	for _, s := range []string{"foo", "bar", "baz"} {
		switch s {
		case "foo", "bar":
			println(s, "small")
		case "baz":
			println(s, "medium")
		default:
			println(s, "other")
		}
	}
}`,
			"", "foo small\nbar small\nbaz medium\n",
		},
		{
			"function multi-return with string and byte",
			`package main
func split() (string, byte) { return "head", 42 }
func main() {
	s, n := split()
	println(s)
	print(n)
}`,
			"", "head\n42",
		},
		{
			"function multi-return two strings",
			`package main
func pair() (string, string) { return "head", "tail" }
func main() {
	a, b := pair()
	println(a, b)
	println(a + "-" + b)
}`,
			"", "head tail\nhead-tail\n",
		},
		{
			"range over string literal",
			`package main
func main() {
	for i, c := range "abc" {
		print(byte(i))
		putchar(':')
		putchar(c)
		putchar('\n')
	}
}`,
			"", "0:a\n1:b\n2:c\n",
		},
		{
			"byte var to string in concat",
			`package main
func main() {
	a := "X"
	x := byte('Y')
	t := a + string(x)
	println(t)
}`,
			"", "XY\n",
		},
		{
			"string of slice element in concat",
			`package main
func main() {
	s := "abcde"
	a := string(s[0])
	b := string(s[2])
	t := a + b
	println(t)
	println(string(s[0]) + string(s[2]) + string(s[4]))
	r := ""
	for i := byte(0); i < byte(len(s)); i++ {
		r += string(s[i])
	}
	println(r)
}`,
			"", "ac\nace\nabcde\n",
		},
		{
			"struct equality with string field",
			`package main
type P struct {
	name string
	age  byte
}
func main() {
	a := P{name: "x", age: 1}
	b := P{name: "x", age: 1}
	c := P{name: "y", age: 1}
	d := P{name: "x", age: 2}
	if a == b { println("ab eq") }
	if a != c { println("ac ne") }
	if a != d { println("ad ne") }
}`,
			"", "ab eq\nac ne\nad ne\n",
		},
		{
			"slice of strings literal and indexing",
			`package main
func main() {
	s := []string{"alpha", "beta", "gamma"}
	for i := 0; i < len(s); i++ {
		println(s[i])
	}
}`,
			"", "alpha\nbeta\ngamma\n",
		},
		{
			"slice of strings make and assign",
			`package main
func main() {
	s := make([]string, 3)
	s[0] = "first"
	s[1] = "second"
	s[2] = "third"
	for i := 0; i < 3; i++ {
		println(s[i])
	}
}`,
			"", "first\nsecond\nthird\n",
		},
		{
			"slice of strings range with key value",
			`package main
func main() {
	s := []string{"a", "b", "c"}
	for i, v := range s {
		print(i)
		print(":")
		println(v)
	}
}`,
			"", "0:a\n1:b\n2:c\n",
		},
		{
			"slice of strings append and modify",
			`package main
func main() {
	s := []string{"foo", "bar"}
	s = append(s, "baz")
	s[0] = "FOO"
	for _, v := range s {
		println(v)
	}
}`,
			"", "FOO\nbar\nbaz\n",
		},
		{
			"slice of strings compare and concat element",
			`package main
func main() {
	s := []string{"hi", "world"}
	if s[0] == "hi" { println("eq") }
	println(s[0] + " " + s[1])
}`,
			"", "eq\nhi world\n",
		},
		{
			"slice of strings from function return",
			`package main
func makeList() []string {
	return []string{"x", "y", "z"}
}
func main() {
	for _, v := range makeList() {
		println(v)
	}
}`,
			"", "x\ny\nz\n",
		},
		{
			"array of strings literal indexing and range",
			`package main
func main() {
	a := [3]string{"alpha", "beta", "gamma"}
	for i := 0; i < 3; i++ {
		println(a[i])
	}
	for _, v := range a {
		println(v)
	}
}`,
			"", "alpha\nbeta\ngamma\nalpha\nbeta\ngamma\n",
		},
		{
			"array of strings element assign and compare",
			`package main
func main() {
	a := [3]string{"a", "b", "c"}
	a[0] = "ALPHA"
	if a[1] == "b" { println("eq") }
	println(a[0])
	println(a[0] + "/" + a[2])
}`,
			"", "eq\nALPHA\nALPHA/c\n",
		},
		{
			"array of strings function parameter",
			`package main
func printArr(a [3]string) {
	for i := 0; i < 3; i++ {
		println(a[i])
	}
}
func main() {
	a := [3]string{"alpha", "beta", "gamma"}
	printArr(a)
}`,
			"", "alpha\nbeta\ngamma\n",
		},
		{
			"array of byte slices function parameter",
			`package main
func sum(p [3][]byte) byte {
	var s byte
	for i := 0; i < 3; i++ {
		for j := 0; j < len(p[i]); j++ {
			s += p[i][j]
		}
	}
	return s
}
func main() {
	var a [3][]byte
	a[0] = []byte{1, 2}
	a[1] = []byte{3, 4, 5}
	a[2] = []byte{6}
	print(sum(a))
}`,
			"", "21",
		},
		{
			"array of byte slices",
			`package main
func main() {
	a := [3][]byte{{'h', 'i'}, {'g', 'o'}, {'b', 'y'}}
	for i := 0; i < 3; i++ {
		println(string(a[i]))
		println(len(a[i]))
	}
}`,
			"", "hi\n2\ngo\n2\nby\n2\n",
		},
		{
			"array of byte slices element assign",
			`package main
func main() {
	var a [3][]byte
	a[0] = []byte{'h', 'i'}
	a[1] = []byte{'b', 'y', 'e'}
	a[2] = []byte{'!', '!'}
	for i := byte(0); i < 3; i++ {
		println(string(a[i]))
	}
}`,
			"", "hi\nbye\n!!\n",
		},
		{
			"nested struct equality with string field",
			`package main
type Inner struct { name string }
type Outer struct {
	a Inner
	b byte
}
func main() {
	x := Outer{a: Inner{name: "foo"}, b: 1}
	y := Outer{a: Inner{name: "foo"}, b: 1}
	z := Outer{a: Inner{name: "bar"}, b: 1}
	if x == y { println("xy eq") }
	if x != z { println("xz ne") }
}`,
			"", "xy eq\nxz ne\n",
		},
		{
			"string const in len and slice",
			`package main
const LONG = "abcdefghijklmnop"
func main() {
	println(len(LONG))
	for i := 0; i < len(LONG); i += 4 {
		println(LONG[i : i+4])
	}
}`,
			"", "16\nabcd\nefgh\nijkl\nmnop\n",
		},
		{
			"function returning string in concat chain",
			`package main
func repeat(s string, n byte) string {
	r := ""
	for i := byte(0); i < n; i++ {
		r += s
	}
	return r
}
func main() {
	println(repeat("X", 4) + "-" + repeat("Y", 2))
}`,
			"", "XXXX-YY\n",
		},
		{
			"defer with string concat argument",
			`package main
func main() {
	s := "hello"
	defer println(s + "!")
	s = "world"
	println("first")
}`,
			"", "first\nhello!\n",
		},
		{
			"named string return with bare return",
			`package main
func greet() (msg string) {
	msg = "hello"
	msg += " "
	msg += "world"
	return
}
func main() {
	println(greet())
}`,
			"", "hello world\n",
		},
		{
			"parenthesized string concat",
			`package main
func main() {
	a := "1"
	b := "2"
	c := "3"
	println(a + (b + c))
	println((a + b) + c)
	println((a + b) + (b + c))
}`,
			"", "123\n123\n1223\n",
		},
		{
			"slice expression on function call result",
			`package main
func makeS() string { return "hello world" }
func main() {
	println(makeS()[0:5])
	println(makeS()[6:])
	println(makeS()[3:8])
}`,
			"", "hello\nworld\nlo wo\n",
		},
		{
			"println multiple string arguments",
			`package main
func main() {
	a := "hello"
	b := "world"
	println(a, b)
	println(a, b, "!")
}`,
			"", "hello world\nhello world !\n",
		},
		{
			"string slicing assignment via field",
			`package main
type T struct { name string }
func main() {
	p := T{name: "hello"}
	for byte(len(p.name)) > 1 {
		p.name = p.name[1:]
		println(p.name)
	}
}`,
			"", "ello\nllo\nlo\no\n",
		},
		{
			"empty string slice via len",
			`package main
func main() {
	s := "hello"
	t := s[len(s):]
	println(len(t))
	println("[" + t + "]")
}`,
			"", "0\n[]\n",
		},
		{
			"string last char via len-1",
			`package main
func main() {
	s := "hello"
	println(s[len(s)-1])
	println(s[len(s)-2])
}`,
			"", "111\n108\n",
		},
		{
			"println string of byte slice keeps slice semantics",
			`package main
func main() {
	bs := []byte{'h', 'i'}
	println(string(bs))
	bs = append(bs, '!')
	println(string(bs))
}`,
			"", "hi\nhi!\n",
		},
		{
			"string const indexed by variable in loop",
			`package main
const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
func main() {
	for i := byte(0); i < 26; i++ {
		putchar(alpha[i])
	}
	println()
}`,
			"", "ABCDEFGHIJKLMNOPQRSTUVWXYZ\n",
		},
		{
			"discarded concat in loop reclaims heap",
			`package main
func main() {
	for i := byte(0); i < 30; i++ {
		_ = "Hello, " + "World!"
		print(i)
		print(" ")
	}
	println()
}`,
			"", "0 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 \n",
		},
		{
			"printed concat in loop reclaims heap",
			`package main
func main() {
	for i := byte(0); i < 30; i++ {
		print("XY" + "ZW")
	}
	println()
}`,
			"", "XYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZWXYZW\n",
		},
		{
			"range over temp string in loop reclaims heap",
			`package main
func main() {
	for i := byte(0); i < 30; i++ {
		n := byte(0)
		for range "HelloWorld" + "Goodbye" {
			n++
		}
		print(n)
		print(" ")
	}
	println()
}`,
			"", "17 17 17 17 17 17 17 17 17 17 17 17 17 17 17 17 17 17 17 17 17 17 17 17 17 17 17 17 17 17 \n",
		},
		{
			"string field write at var-indexed struct array",
			`package main
type S struct { name string }
func main() {
	var arr [3]S
	i := byte(1)
	arr[i].name = "hello"
	println(arr[0].name, arr[1].name, arr[2].name)
}`,
			"", " hello \n",
		},
		// --- Pointers ---
		{
			"pointer read",
			`package main
func main() {
	x := byte(42)
	p := &x
	print(*p)
}`,
			"", "42",
		},
		{
			"pointer write",
			`package main
func main() {
	x := byte(10)
	p := &x
	*p = 99
	print(x)
}`,
			"", "99",
		},
		{
			"pointer inc dec",
			`package main
func main() {
	x := byte(10)
	p := &x
	*p++
	*p++
	*p--
	print(x)
}`,
			"", "11",
		},
		{
			"pointer swap via function",
			`package main
func swap(a, b *byte) {
	t := *a
	*a = *b
	*b = t
}
func main() {
	x := byte(10)
	y := byte(20)
	swap(&x, &y)
	println(x, y)
}`,
			"", "20 10\n",
		},
		{
			"pointer alias to struct",
			`package main
type P struct{ x, y, z byte }
func main() {
	p := P{x: 3, y: 5, z: 7}
	pp := &p
	q := pp
	q.x = 99
	println(p.x, p.y, p.z)
	println(q.x, q.y, q.z)
}`,
			"", "99 5 7\n99 5 7\n",
		},
		{
			"deref pointer to struct define",
			`package main
type P struct{ x, y byte }
func main() {
	p := P{x: 3, y: 5}
	pp := &p
	q := *pp
	q.x = 99
	println(p.x, q.x)
}`,
			"", "3 99\n",
		},
		{
			"selector on parenthesized deref",
			`package main
type P struct{ x, y byte }
func main() {
	p := P{x: 3, y: 5}
	pp := &p
	println((*pp).x, (*pp).y)
	(*pp).x = 99
	println(p.x)
}`,
			"", "3 5\n99\n",
		},
		{
			"pointer to array element",
			`package main
func main() {
	a := [3]byte{10, 20, 30}
	p := &a[1]
	*p = 99
	print(a[1])
}`,
			"", "99",
		},
		{
			"pointer to array variable index",
			`package main
func main() {
	a := [3]byte{10, 20, 30}
	for i := byte(0); i < 3; i++ {
		p := &a[i]
		*p *= 2
	}
	println(a[0], a[1], a[2])
}`,
			"", "20 40 60\n",
		},
		{
			"pointer to uintN array element write widens narrower literal",
			`package main
func setAll(p *[3]uint32) {
	for i := byte(0); i < 3; i++ {
		p[i] = 1000
	}
}
func main() {
	var a [3]uint32
	setAll(&a)
	println(a[0], a[1], a[2])
}`,
			"", "1000 1000 1000\n",
		},
		{
			"pointer to struct field",
			`package main
type P struct { x byte; y byte }
func main() {
	p := P{x: 3, y: 7}
	px := &p.x
	*px = 42
	print(p.x)
}`,
			"", "42",
		},
		{
			"struct pointer read and write",
			`package main
type P struct { x byte; y byte }
func main() {
	p := P{x: 3, y: 7}
	ptr := &p
	print(ptr.x + ptr.y)
	ptr.x = 10
	ptr.y = 20
	print(" ")
	print(p.x + p.y)
}`,
			"", "10 30",
		},
		{
			"pointer-typed struct field read and write",
			`package main
type Inner struct { v byte }
type Outer struct { id byte; p *Inner; tag byte }
func main() {
	in := Inner{v: 42}
	out := Outer{id: 7, p: &in, tag: 99}
	println(out.id, out.tag)
	println(out.p.v)
	out.p.v = 100
	println(in.v, out.p.v)
}`,
			"", "7 99\n42\n100 100\n",
		},
		{
			"pointer-typed struct field in slice element",
			`package main
type N struct { v byte }
type W struct { p *N; tag byte }
func main() {
	a := N{v: 10}
	b := N{v: 20}
	s := []W{W{p: &a, tag: 1}, W{p: &b, tag: 2}}
	println(s[0].p.v, s[0].tag)
	println(s[1].p.v, s[1].tag)
	s[0].p.v = 99
	println(a.v)
}`,
			"", "10 1\n20 2\n99\n",
		},
		{
			"pointer-typed struct field in array element",
			`package main
type N struct { v byte }
type W struct { p *N }
func main() {
	a := N{v: 1}
	b := N{v: 2}
	arr := [2]W{W{p: &a}, W{p: &b}}
	for _, w := range arr {
		print(w.p.v); print(" ")
	}
	println()
}`,
			"", "1 2 \n",
		},
		{
			"method call through pointer-typed struct field",
			`package main
type N struct { v byte }
func (n *N) inc() { n.v++ }
type W struct { p *N }
func main() {
	x := N{v: 10}
	w := W{p: &x}
	w.p.inc()
	w.p.inc()
	println(x.v)
}`,
			"", "12\n",
		},
		{
			"nested struct field via pointer",
			`package main
type Inner struct { v byte; w uint16 }
type Mid struct { d Inner }
type Outer struct { a byte; m Mid }
func main() {
	o := Outer{a: 1, m: Mid{d: Inner{v: 3, w: 30000}}}
	po := &o
	x := po.m.d.v
	y := po.m.d.w
	println(x, y)
	po.m.d.v = 99
	po.m.d.w = 50000
	println(o.m.d.v, o.m.d.w)
}`,
			"", "3 30000\n99 50000\n",
		},
		{
			"nested struct string field via pointer",
			`package main
type Inner struct { s string }
type Outer struct { a byte; q Inner }
func setVia(p *Outer, s string) { p.q.s = s }
func main() {
	o := Outer{a: 1, q: Inner{s: "hello"}}
	po := &o
	println(po.q.s)
	po.q.s = "world"
	println(o.q.s)
	setVia(po, "everyone")
	println(o.q.s)
}`,
			"", "hello\nworld\neveryone\n",
		},
		{
			"len and cap via pointer",
			`package main
func main() {
	a := [5]byte{1, 2, 3, 4, 5}
	ptr := &a
	println(len(ptr), len(*ptr), cap(ptr))
}`,
			"", "5 5 5\n",
		},
		{
			"pointer array index",
			`package main
func main() {
	a := [4]byte{10, 20, 30, 40}
	ptr := &a
	s := byte(0)
	for i := byte(0); i < 4; i++ {
		s += ptr[i]
	}
	print(s)
}`,
			"", "100",
		},
		{
			"pointer 2d array index",
			`package main
func main() {
	a := [2][3]byte{{1, 2, 3}, {4, 5, 6}}
	ptr := &a
	println(ptr[0][0], ptr[1][2])
}`,
			"", "1 6\n",
		},
		{
			"pointer 2d array write",
			`package main
func main() {
	a := [2][3]byte{{0,0,0},{0,0,0}}
	ptr := &a
	for i := byte(0); i < 2; i++ {
		for j := byte(0); j < 3; j++ {
			ptr[i][j] = i*3 + j + 1
		}
	}
	println(a[0][0], a[1][2])
}`,
			"", "1 6\n",
		},
		{
			"pointer array of structs field read",
			`package main
type P struct { x byte; y byte }
func main() {
	a := [3]P{{x: 10, y: 11}, {x: 20, y: 21}, {x: 30, y: 31}}
	ptr := &a
	for i := byte(0); i < 3; i++ {
		if i > 0 { print(" ") }
		print(ptr[i].x)
	}
}`,
			"", "10 20 30",
		},
		{
			"pointer struct array field read",
			`package main
type S struct { data [3]byte; n byte }
func main() {
	s := S{data: [3]byte{10, 20, 30}, n: 3}
	ptr := &s
	println(ptr.data[0], ptr.data[2])
}`,
			"", "10 30\n",
		},
		{
			"pointer array of structs field write",
			`package main
type P struct { x byte; y byte }
func main() {
	a := [3]P{{x: 0, y: 0}, {x: 0, y: 0}, {x: 0, y: 0}}
	ptr := &a
	for i := byte(0); i < 3; i++ {
		ptr[i].x = i + 1
		ptr[i].y = (i + 1) * 10
	}
	println(a[0].x, a[2].y)
}`,
			"", "1 30\n",
		},
		{
			"pointer struct array field write",
			`package main
type S struct { data [4]byte }
func main() {
	s := S{data: [4]byte{0, 0, 0, 0}}
	ptr := &s
	for i := byte(0); i < 4; i++ {
		ptr.data[i] = (i + 1) * 10
	}
	println(s.data[0], s.data[3])
}`,
			"", "10 40\n",
		},
		{
			"pointer array write",
			`package main
func main() {
	a := [4]byte{10, 20, 30, 40}
	ptr := &a
	for i := byte(0); i < 4; i++ {
		ptr[i] = ptr[i] * 2
	}
	for i := byte(0); i < 4; i++ {
		if i > 0 { print(" ") }
		print(a[i])
	}
}`,
			"", "20 40 60 80",
		},
		{
			"pointer array write via function",
			`package main
func zero(ptr *[3]byte) {
	for i := byte(0); i < byte(len(*ptr)); i++ {
		ptr[i] = 0
	}
}
func main() {
	a := [3]byte{1, 2, 3}
	print(a[0])
	print(a[1])
	print(a[2])
	print(" ")
	zero(&a)
	print(a[0])
	print(a[1])
	print(a[2])
}`,
			"", "123 000",
		},
		{
			"typed struct pointer parameter",
			`package main
type P struct { x byte; y byte }
func inc(ptr *P) {
	ptr.x++
	ptr.y++
}
func main() {
	p := P{x: 3, y: 7}
	inc(&p)
	println(p.x, p.y)
}`,
			"", "4 8\n",
		},
		{
			"typed array pointer parameter with len",
			`package main
func sum(ptr *[4]byte) byte {
	s := byte(0)
	for i := byte(0); i < byte(len(ptr)); i++ {
		s += ptr[i]
	}
	return s
}
func main() {
	a := [4]byte{10, 20, 30, 40}
	print(sum(&a))
}`,
			"", "100",
		},
		{
			"pointer return from function",
			`package main
func first(a *[3]byte) *byte {
	return &a[0]
}
func main() {
	a := [3]byte{10, 20, 30}
	p := first(&a)
	print(*p)
}`,
			"", "10",
		},
		{
			"deref function result",
			`package main
func getptr(p *byte) *byte { return p }
func main() {
	x := byte(42)
	p := &x
	print(*getptr(p))
}`,
			"", "42",
		},
		{
			"pointer 2d array read variable inner index",
			`package main
func main() {
	a := [2][3]byte{{10, 20, 30}, {40, 50, 60}}
	p := &a
	j := byte(2)
	println(p[0][j], p[1][j])
}`,
			"", "30 60\n",
		},
		{
			"pointer 2d array write and read variable inner index",
			`package main
func main() {
	var a [2][3]byte
	p := &a
	j := byte(1)
	p[0][j] = 42
	print(p[0][j])
}`,
			"", "42",
		},
		{
			"pointer 2d array write variable inner index",
			`package main
func main() {
	var a [2][3]byte
	p := &a
	j := byte(1)
	p[1][j] = 99
	print(p[1][j])
}`,
			"", "99",
		},
		{
			"pointer 2d array read zero inner index",
			`package main
func main() {
	a := [2][3]byte{{10, 20, 30}, {40, 50, 60}}
	p := &a
	print(p[1][0])
}`,
			"", "40",
		},
		{
			"pointer 2d array write zero inner index",
			`package main
func main() {
	var a [2][3]byte
	p := &a
	p[0][0] = 77
	print(p[0][0])
}`,
			"", "77",
		},
		{
			"pointer struct array field variable index",
			`package main
type S struct { data [3]byte; len byte }
func main() {
	s := S{data: [3]byte{10, 20, 30}, len: 3}
	p := &s
	i := byte(1)
	print(p.data[i])
}`,
			"", "20",
		},
		{
			"pointer 2d array write then read both variable",
			`package main
func main() {
	var a [3][2]byte
	p := &a
	i := byte(1)
	j := byte(0)
	p[i][j] = 55
	print(p[i][j])
}`,
			"", "55",
		},
		{
			"pointer 2d array multiple writes and reads",
			`package main
func main() {
	var a [2][2]byte
	p := &a
	i := byte(0)
	j := byte(1)
	p[i][j] = 10
	p[j][i] = 20
	println(p[i][j], p[j][i])
}`,
			"", "10 20\n",
		},
		{
			"pointer array write then read same index",
			`package main
func main() {
	var a [4]byte
	p := &a
	i := byte(2)
	p[i] = 77
	print(p[i])
}`,
			"", "77",
		},
		{
			"pointer struct field write then read via variable index",
			`package main
type S struct { data [3]byte; n byte }
func main() {
	var s S
	p := &s
	i := byte(2)
	p.data[i] = 33
	print(p.data[i])
}`,
			"", "33",
		},
		{
			"pointer array accumulate via variable index",
			`package main
func main() {
	var a [3]byte
	p := &a
	for i := byte(0); i < 3; i++ {
		p[i] = (i + 1) * 10
	}
	s := byte(0)
	for i := byte(0); i < 3; i++ {
		s += p[i]
	}
	print(s)
}`,
			"", "60",
		},
		{
			"pointer struct field inc dec",
			`package main
type P struct { x, y byte }
func main() {
	p := P{x: 10, y: 20}
	ptr := &p
	ptr.x++
	ptr.y--
	println(ptr.x, ptr.y)
}`,
			"", "11 19\n",
		},
		{
			"pointer of composite literal",
			`package main
type P struct { x, y byte }
func main() {
	p := &P{x: 1, y: 2}
	println(p.x, p.y)
	p.x = 10
	println(p.x)
}`,
			"", "1 2\n10\n",
		},
		{
			"pointer deref decrement",
			`package main
func main() {
	x := byte(10)
	p := &x
	*p--
	print(x)
}`,
			"", "9",
		},
		{
			"parallel swap via deref",
			`package main
func main() {
	var x byte = 1
	var y byte = 2
	px := &x
	py := &y
	*px, *py = *py, *px
	println(x, y)
}`,
			"", "2 1\n",
		},
		{
			"pointer array index decrement",
			`package main
func main() {
	a := [3]byte{10, 20, 30}
	p := &a
	p[1]--
	print(p[1])
}`,
			"", "19",
		},
		{
			"pointer array fill in loop",
			`package main
func fill(p *[5]byte) {
	for i := range byte(5) {
		p[i] = (i + 1) * (i + 1)
	}
}
func main() {
	var a [5]byte
	fill(&a)
	for _, v := range a {
		print(v); print(" ")
	}
	println()
}`,
			"", "1 4 9 16 25 \n",
		},
		{
			"pointer comparison",
			`package main
func main() {
	x, y := byte(0), byte(1)
	p, q := &x, &y
	if p == p { print("Y") } else { print("N") }
	if p == q { print("Y") } else { print("N") }
	if q == q { print("Y") } else { print("N") }
}`,
			"", "YNY",
		},
		{
			"pointer nil comparison",
			`package main
func main() {
	var p *byte
	if p == nil { print("Y") } else { print("N") }
	x := byte(42)
	p = &x
	if p != nil { print("Y") } else { print("N") }
}`,
			"", "YY",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, err := CompileSource(tt.src)
			if err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			if err := Interpret(code, strings.NewReader(tt.input), &out); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != tt.output {
				t.Errorf("got %q, want %q", got, tt.output)
			}
		})
	}
}

func TestCompileError(t *testing.T) {
	tests := []struct {
		name string
		src  string
		err  string
	}{
		{
			"no main",
			`package main
func f() {}`,
			"no main function found",
		},
		{
			"zero-length array local var",
			`package main
func main() { var a [0]byte; _ = a }`,
			"input.go:2:21: zero-length arrays are not supported",
		},
		{
			"zero-length array struct field",
			`package main
type T struct { pad [0]byte; v byte }
func main() { _ = T{v: 1} }`,
			"input.go:2:21: zero-length arrays are not supported",
		},
		{
			"zero-length array return",
			`package main
func f() [0]byte { return [0]byte{} }
func main() { _ = f() }`,
			"input.go:2:10: zero-length arrays are not supported",
		},
		{
			"zero-length array param",
			`package main
func f(p [0]byte) {}
func main() { var a [0]byte; f(a) }`,
			"input.go:2:10: zero-length arrays are not supported",
		},
		{
			"zero-length array via const",
			`package main
const N = 0
func main() { var a [N]byte; _ = a }`,
			"input.go:3:21: zero-length arrays are not supported",
		},
		{
			"zero-length array global var typed",
			`package main
var arr [0]byte
func main() { _ = arr }`,
			"input.go:2:9: zero-length arrays are not supported",
		},
		{
			"zero-length array global var with literal",
			`package main
var arr = [0]byte{}
func main() { _ = arr }`,
			"input.go:2:11: zero-length arrays are not supported",
		},
		{
			"zero-length array global var with ellipsis",
			`package main
var arr = [...]byte{}
func main() { _ = arr }`,
			"input.go:2:11: zero-length arrays are not supported",
		},
		{
			"integer overflow",
			`package main
func main() { putchar(256) }`,
			"cannot use uint16 as argument to putchar, use byte() to truncate",
		},
		{
			"undefined variable: x",
			`package main
func main() { putchar(x) }`,
			"undefined variable: x",
		},
		{
			"global accessed from recursive function rejected",
			`package main
var counter uint16
func f(n byte) {
	if n == 0 { return }
	counter++
	f(n - 1)
}
func main() { f(3) }`,
			"global variable counter is not accessible from recursive function",
		},
		{
			"unsupported function in expression: unknown",
			`package main
func main() { foo() }`,
			"unsupported function: foo",
		},
		{
			"import not supported",
			`package main
import "fmt"
func main() { fmt.Println("hello") }`,
			"input.go:2:8: imports are not supported",
		},
		{
			"wrong argument count",
			`package main
func f(x byte) byte { return x }
func main() { f(1, 2) }`,
			"function f expects 1 arguments, got 2",
		},
		{
			"array out of bounds",
			`package main
func main() {
	a := [3]byte{1, 2, 3}
	putchar(a[3])
}`,
			"array index 3 out of bounds [0:3]",
		},
		{
			"break outside loop",
			`package main
func main() { break }`,
			"break outside loop",
		},
		{
			"continue outside loop",
			`package main
func main() { continue }`,
			"continue outside loop",
		},
		{
			"undefined array",
			`package main
func main() {
	putchar(a[0])
}`,
			"undefined variable: a",
		},
		{
			"string literal",
			`package main
func main() {
	x := "hello"
	putchar(x)
}`,
			"cannot use slice as argument to putchar",
		},
		{
			"range over struct",
			`package main
type X struct { x, y byte }
func main() {
	i := X{4, 5}
	for j := range i { println(j) }
}`,
			"cannot range over struct: i",
		},
		{
			"range over pointer",
			`package main
type X struct { x, y byte }
func main() {
	i := X{4, 5}
	for j := range &i { println(j) }
}`,
			"cannot range over pointer expression",
		},
		{
			"too many locals in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a0, a1, a2, a3, a4, a5, a6, a7, a8, a9 := n, n, n, n, n, n, n, n, n, n
	b0, b1, b2, b3, b4, b5, b6, b7, b8, b9 := n, n, n, n, n, n, n, n, n, n
	c0, c1, c2, c3, c4, c5, c6, c7, c8, c9 := n, n, n, n, n, n, n, n, n, n
	d0, d1, d2, d3, d4, d5, d6, d7, d8, d9 := n, n, n, n, n, n, n, n, n, n
	e0, e1, e2, e3, e4, e5, e6, e7, e8, e9 := n, n, n, n, n, n, n, n, n, n
	g0, g1, g2, g3, g4, g5, g6, g7, g8, g9 := n, n, n, n, n, n, n, n, n, n
	s := f(n - 1)
	return s + a0 + a1 + a2 + a3 + a4 + a5 + a6 + a7 + a8 + a9 +
		b0 + b1 + b2 + b3 + b4 + b5 + b6 + b7 + b8 + b9 +
		c0 + c1 + c2 + c3 + c4 + c5 + c6 + c7 + c8 + c9 +
		d0 + d1 + d2 + d3 + d4 + d5 + d6 + d7 + d8 + d9 +
		e0 + e1 + e2 + e3 + e4 + e5 + e6 + e7 + e8 + e9 +
		g0 + g1 + g2 + g3 + g4 + g5 + g6 + g7 + g8 + g9 
}
func main() { putchar(f(1)) }`,
			"too many local variables in recursive function",
		},
		{
			"unsupported expression statement in recursive function",
			`package main
func main() {
	x := byte(1)
	x + 1
}`,
			"unsupported expression statement",
		},
		{
			"wrong package name",
			`package notmain
func main() { }`,
			"input.go: expected package main, got package notmain",
		},
		{
			"recursive unsupported stmt",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	go println(a)
	return a
}
func main() { putchar(f(1)) }`,
			"unsupported statement in recursive function: *ast.GoStmt",
		},
		{
			"recursive call as statement error",
			`package main
func g(n byte) byte {
	if n == 0 { return 0 }
	return g(n - 1)
}
func f(n byte) byte {
	if n == 0 { return 0 }
	g(n)
	r := f(n - 1)
	return r
}
func main() { print(f(1)) }`,
			"unsupported recursive call as statement: g",
		},
		{
			"unsupported expression statement in recursive",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	n + 1
	r := f(n - 1)
	return r
}
func main() { print(f(1)) }`,
			"unsupported expression statement in recursive function",
		},
		{
			"uint64 return in recursive function",
			`package main
func f(n byte) uint64 {
	if n == 0 { return 0 }
	r := f(n - 1)
	return r
}
func main() { print(f(1)) }`,
			"uint64 return type in recursive function f is not supported",
		},
		{
			"uint64 local in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	x := uint64(100)
	r := f(n - 1)
	return byte(x) + r
}
func main() { print(f(2)) }`,
			"uint64 local x in recursive function is not supported",
		},
		{
			"uint64 param in recursive function",
			`package main
func f(n uint64) byte {
	if n == 0 { return 0 }
	r := f(n - 1)
	return r + byte(1)
}
func main() { print(f(2)) }`,
			"uint64 parameter n in recursive function f is not supported",
		},
		{
			"pointer param in general recursion",
			`package main
func f(p *byte, n byte) byte {
	if n == 0 { return *p }
	r := f(p, n - 1)
	return r
}
func main() { x := byte(7); print(f(&x, 3)) }`,
			"pointer parameter p in recursive function f is not supported",
		},
		{
			"struct param in recursive function",
			`package main
type P struct { x, y byte }
func f(p P, n byte) byte {
	if n == 0 { return p.x + p.y }
	r := f(p, n - 1)
	return r
}
func main() { print(f(P{1, 2}, 2)) }`,
			"struct parameter p in recursive function f is not supported",
		},
		{
			"recursive method on struct receiver",
			`package main
type Counter struct { val byte }
func (c Counter) sum(n byte) uint16 {
	if n == 0 { return uint16(c.val) }
	return c.sum(n-1) + uint16(n)
}
func main() { print(Counter{val: 5}.sum(3)) }`,
			"struct parameter c in recursive function Counter.sum is not supported",
		},
		{
			"array param in recursive function",
			`package main
func f(a [3]byte, n byte) byte {
	if n == 0 { return a[0] + a[1] + a[2] }
	r := f(a, n - 1)
	return r
}
func main() { print(f([3]byte{1, 2, 3}, 2)) }`,
			"array parameter a in recursive function f is not supported",
		},
		{
			"struct return in recursive function",
			`package main
type P struct { x, y byte }
func f(n byte) P {
	if n == 0 { return P{1, 2} }
	r := f(n - 1)
	return r
}
func main() { print(f(2).x) }`,
			"struct return type in recursive function f is not supported",
		},
		{
			"array return in recursive function",
			`package main
func f(n byte) [3]byte {
	if n == 0 { return [3]byte{1, 2, 3} }
	r := f(n - 1)
	return r
}
func main() { print(f(2)[0]) }`,
			"array return type in recursive function f is not supported",
		},
		{
			"array local in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := [3]byte{1, 2, 3}
	return a[0] + f(n - 1)
}
func main() { print(f(2)) }`,
			"array usage in recursive function is not supported",
		},
		{
			"struct local in recursive function",
			`package main
type P struct { x, y byte }
func f(n byte) byte {
	if n == 0 { return 0 }
	var p P
	p.x = n
	return p.x + f(n - 1)
}
func main() { print(f(2)) }`,
			"struct usage in recursive function is not supported",
		},
		{
			"label on non-loop in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
myLabel:
	x := byte(5)
	_ = x
	r := f(n - 1)
	return r
}
func main() { print(f(2)) }`,
			"label myLabel on non-loop statement is not supported in recursive functions",
		},
		{
			"label nested inside block in non-recursive function",
			`package main
func main() {
	x := byte(0)
	if x == 0 {
inner:
		putchar(byte('A'))
		_ = inner
	}
	goto inner
}`,
			"label inner must be at the function-body top level",
		},
		{
			"goto undefined label",
			`package main
func main() { goto missing }`,
			"goto target missing is not a top-level label of the enclosing function",
		},
		{
			"goto in tail-recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
loop:
	if n > 5 { n--; goto loop }
	return f(n - 1)
}
func main() { print(f(3)) }`,
			"goto in tail-recursive function f is not supported",
		},
		{
			"defer of non-builtin in recursive function",
			`package main
func g(x byte) { _ = x }
func f(n byte) byte {
	if n == 0 { return 0 }
	defer g(n)
	r := f(n - 1)
	return r
}
func main() { print(f(2)) }`,
			"unsupported defer call in recursive function: g",
		},
		{
			"void function in expression in recursive function",
			`package main
func g(x byte) {}
func f(n byte) byte {
	if n == 0 { return 0 }
	x := g(n)
	r := f(n - 1)
	return r + x
}
func main() { print(f(2)) }`,
			"function g has no return value",
		},
		{
			"range with value in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	for i, v := range byte(5) {
		_ = i; _ = v
	}
	r := f(n - 1)
	return r
}
func main() { print(f(2)) }`,
			"range with value in recursive function is not supported (no array locals)",
		},
		{
			"undefined variable in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	_ = x
	r := f(n - 1)
	return r
}
func main() { print(f(2)) }`,
			"undefined variable in recursive function: x",
		},
		{
			"function literal call in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	x := func(y byte) byte { return y + 1 }(n)
	r := f(n - 1)
	return r + x
}
func main() { print(f(2)) }`,
			"unsupported call in recursive function: *ast.FuncLit",
		},
		{
			"function literal call as statement in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	(func() { _ = byte(0) })()
	r := f(n - 1)
	return r
}
func main() { print(f(2)) }`,
			"unsupported call in recursive function",
		},
		{
			"parse error",
			`package main
func main() { `,
			"input.go:2:15: expected '}', found 'EOF'",
		},
		{
			"composite literal out of bounds",
			`package main
func main() {
	a := [2]byte{0: 1, 5: 2}
	putchar(a[0])
}`,
			"array index 5 out of bounds [0:2]",
		},
		{
			"array index out of bounds write",
			`package main
func main() {
	a := [3]byte{1, 2, 3}
	a[3] = 4
}`,
			"array index 3 out of bounds [0:3]",
		},
		{
			"multi return array out of bounds",
			`package main
func f() (byte, byte) { return 1, 2 }
func main() {
	var a [2]byte
	a[0], a[2] = f()
}`,
			"array index 2 out of bounds [0:2]",
		},
		{
			"multi return undefined array",
			`package main
func f() (byte, byte) { return 1, 2 }
func main() {
	x, b[0] = f()
	_ = x
}`,
			"undefined variable: x",
		},
		{
			"deref assign undefined variable",
			`package main
func main() {
	*x = 1
}`,
			"undefined variable: x",
		},
		{
			"max wrong args",
			`package main
func main() { print(max(byte(1))) }`,
			"max() expects at least 2 arguments",
		},
		{
			"cap wrong args",
			`package main
func main() { print(cap(1, 2)) }`,
			"cap() expects 1 argument",
		},
		{
			"cap on non-array",
			`package main
func main() { x := byte(1); print(cap(x)) }`,
			"cap() argument must be an array",
		},
		{
			"len wrong args",
			`package main
func main() { print(len(1, 2)) }`,
			"len() expects 1 argument",
		},
		{
			"byte wrong args",
			`package main
func main() { putchar(byte(1, 2)) }`,
			"byte() expects 1 argument",
		},
		{
			"len on non-array",
			`package main
func main() { x := byte(1); print(len(x)) }`,
			"len() argument must be an array",
		},
		{
			"putchar no args",
			`package main
func main() { putchar() }`,
			"putchar expects 1 argument, got 0",
		},
		{
			"getchar wrong args",
			`package main
func main() { x := getchar(1) }`,
			"getchar expects 0 arguments",
		},
		{
			"void function in expression",
			`package main
func f() { }
func main() { x := f() }`,
			"function f has no return value",
		},
		{
			"unknown function in expression",
			`package main
func main() { x := unknown() }`,
			"unsupported function in expression: unknown",
		},
		{
			"clear wrong arg count",
			`package main
func main() { clear() }`,
			"clear expects 1 argument",
		},
		{
			"clear non-slice args",
			`package main
func main() { x := byte(1); clear(x) }`,
			"clear expects a slice argument",
		},
		{
			"copy wrong arg count",
			`package main
func main() { copy() }`,
			"copy expects 2 arguments",
		},
		{
			"copy non-slice args",
			`package main
func main() { x := byte(1); y := byte(2); copy(x, y) }`,
			"copy expects slice arguments",
		},
		{
			"struct field assign to non-struct",
			`package main
func main() {
	x := byte(1)
	x.y = 2
}`,
			"undefined struct in field assignment",
		},
		{
			"min wrong args",
			`package main
func main() { print(min(byte(1))) }`,
			"min() expects at least 2 arguments",
		},
		{
			"putchar wrong args",
			`package main
func main() { putchar(1, 2) }`,
			"putchar expects 1 argument, got 2",
		},
		{
			"unsupported call in expression",
			`package main
func main() { putchar(foo()) }`,
			"unsupported function in expression: foo",
		},
		{
			"too many stack slots",
			`package main
func main() {
	var a [255]byte
	var b [2]byte
	a[0] = 1
	b[0] = 2
}`,
			"too many variables: 259 stack slots (max 255)",
		},
		{
			"void function in recursive function",
			`package main
func g() {}
func f(n byte) byte {
	if n == 0 { return 0 }
	a := g()
	r := f(n - 1)
	return r
}
func main() { print(f(1)) }`,
			"function g has no return value",
		},
		{
			"recursive undefined var",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := f(n - 1)
	return a + x
}
func main() { putchar(f(1)) }`,
			"undefined variable in recursive function: x",
		},
		{
			"function no return value in expression",
			`package main
func f() { }
func main() { putchar(f()) }`,
			"function f has no return value",
		},
		{
			"getchar wrong args in expression",
			`package main
func main() { putchar(getchar(1)) }`,
			"getchar expects 0 arguments",
		},
		{
			"len wrong args in expression",
			`package main
func main() { putchar(len(1, 2)) }`,
			"len() expects 1 argument",
		},
		{
			"len non-array",
			`package main
func main() {
	x := byte(1)
	putchar(len(x))
}`,
			"len() argument must be an array",
		},
		{
			"defer in loop",
			`package main
func main() {
	for i := range 3 {
		defer putchar(i)
	}
}`,
			"defer inside a loop is not supported",
		},
		{
			"const uint32 out of range",
			`package main
const x uint32 = 4294967296
func main() {}`,
			"const x: value 4294967296 out of uint32 range (0-4294967295)",
		},
		{
			"const byte negative wraps to out-of-range",
			`package main
const x byte = byte(0) - 1
func main() {}`,
			"const x: value 18446744073709551615 out of byte range (0-255)",
		},
		{
			"const uint8 negative wraps to out-of-range",
			`package main
const x uint8 = uint8(0) - 1
func main() {}`,
			"const x: value 18446744073709551615 out of byte range (0-255)",
		},
		{
			"const division by zero",
			`package main
const x = 10 / 0
func main() {}`,
			"const x: division by zero in constant expression",
		},
		{
			"const modulo by zero",
			`package main
const x = 10 % 0
func main() {}`,
			"const x: modulo by zero in constant expression",
		},
		{
			"const with unsupported unary expression",
			`package main
const x byte = +5
func main() {}`,
			"const x: unsupported constant expression",
		},
		{
			"duplicate struct type",
			`package main
type P struct{ x byte }
type P struct{ y byte }
func main() {}`,
			"duplicate type: P",
		},
		{
			"unknown struct field",
			`package main
type Point struct { x byte; y byte }
func main() {
	p := Point{1, 2}
	print(p.z)
}`,
			"unknown field z in struct Point",
		},
		{
			"slice field with unknown element ident",
			`package main
type S struct{ f []T }
func main() { var s S; _ = s }`,
			"unknown field type: T",
		},
		{
			"slice field with unsupported element shape",
			`package main
type Foo struct{ v byte }
type S struct{ f []*Foo }
func main() { var s S; _ = s }`,
			"unknown field type: *Foo",
		},
		{
			"wider int in slice literal",
			`package main
func main() { a := []uint16{uint32(1000)}; _ = a }`,
			"cannot use uint32 value in []uint16 literal, use explicit conversion",
		},
		{
			"wider int in array literal",
			`package main
func main() { a := [1]uint16{uint32(1000)}; _ = a }`,
			"cannot use uint32 value in []uint16 literal, use explicit conversion",
		},
		{
			"struct argument undefined",
			`package main
type Point struct { x byte; y byte }
func f(p Point) byte { return p.x }
func main() {
	print(f(q))
}`,
			"undefined variable: q",
		},
		{
			"recursive unsupported nested if both",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	if n%3 == 0 {
		if n%2 == 0 {
			return f(n-1)
		} else {
			return f(n-2)
		}
	}
	return f(n-1) + 1
}
func main() { print(f(1)) }`,
			"unsupported recursive call pattern in then-branch of f",
		},
		{
			"mutual recursion",
			`package main
func even(n byte) byte { if n == 0 { return 1 }; return odd(n-1) }
func odd(n byte) byte { if n == 0 { return 0 }; return even(n-1) }
func main() { print(even(4)) }`,
			"mutual recursion is not supported: even -> odd -> even",
		},
		{
			"mutual recursion 4-cycle",
			`package main
func a(n byte) byte { if n == 0 { return 0 }; return b(n-1) }
func b(n byte) byte { if n == 0 { return 0 }; return c(n-1) }
func c(n byte) byte { if n == 0 { return 0 }; return d(n-1) }
func d(n byte) byte { if n == 0 { return 0 }; return a(n-1) }
func main() { print(a(4)) }`,
			"mutual recursion is not supported: a -> b -> c -> d -> a",
		},
		{
			"byte conversion wrong args",
			`package main
func main() { putchar(byte(1, 2)) }`,
			"byte() expects 1 argument",
		},
		{
			"address of literal",
			`package main
func main() { putchar(&1) }`,
			"cannot take address of *ast.BasicLit",
		},
		{
			"use struct as byte",
			`package main
type P struct { x byte }
func main() {
	p := P{1}
	putchar(p)
}`,
			"cannot use struct P as byte value",
		},
		{
			"use array as byte",
			`package main
func main() {
	a := [3]byte{1, 2, 3}
	putchar(a)
}`,
			"cannot use array as byte value",
		},
		{
			"string constant in byte context",
			`package main
const msg = "hello"
func main() { putchar(msg) }`,
			"string constant msg can only be used with print/println",
		},
		{
			"array index out of bounds in struct array field",
			`package main
type S struct { data [2]byte }
func main() {
	s := S{data: [2]byte{1, 2}}
	print(s.data[3])
}`,
			"array index 3 out of bounds [0:2]",
		},
		{
			"array nesting too deep",
			`package main
func main() {
	var a [2][3][4][5]byte
	a[0][0][0][0] = 1
}`,
			"array nesting deeper than 3 levels is not supported",
		},
		{
			"string literal in expression",
			`package main
func main() {
	x := "hello"
	putchar(x)
}`,
			"cannot use slice as argument to putchar",
		},
		{
			"uint64 putchar",
			`package main
func main() { putchar(5000000000) }`,
			"cannot use uint64 as argument to putchar, use byte() to truncate",
		},
		{
			"uint16 assign to byte variable",
			`package main
func main() { v := 300; _ = v }`,
			"cannot assign wider integer to byte variable, use explicit conversion",
		},
		{
			"uint16 putchar",
			`package main
func main() { var x uint16 = 1; putchar(x) }`,
			"cannot use uint16 as argument to putchar, use byte() to truncate",
		},
		{
			"uint16 return from byte function",
			`package main
func f() byte { var x uint16 = 1; return x }
func main() { println(f()) }`,
			"cannot return wider integer from byte-returning function, use byte() to truncate",
		},
		{
			"uint16 mixed type",
			`package main
func main() { var x uint16 = 1; y := byte(2); println(x + y) }`,
			"mismatched integer sizes in x + y, use explicit conversion",
		},
		{
			"uint16 array element assigned byte var",
			`package main
func main() { var a [3]uint16; b := byte(5); a[0] = b; println(a[0]) }`,
			"mismatched integer sizes in element assignment, use explicit conversion",
		},
		{
			"make literal size exceeds 255",
			`package main
func main() { a := make([]byte, 1000); println(len(a)) }`,
			"make size 1000 (* elemSize 1 = 1000 cells) exceeds the 255-slot ceiling",
		},
		{
			"make uint16 size",
			`package main
func main() { var n uint16 = 5; a := make([]byte, n); println(len(a)) }`,
			"make size must be byte (got uint16), use byte() to truncate",
		},
		{
			"byte array literal element overflow",
			`package main
func main() { a := [3]byte{1, 2, 300}; println(a[2]) }`,
			"cannot use uint16 value in []byte literal, use byte() to truncate",
		},
		{
			"byte arg literal overflow",
			`package main
func f(x byte) { println(x) }
func main() { f(256) }`,
			"cannot pass uint16 value to byte parameter x, use byte() to truncate",
		},
		{
			"uint16 array index",
			`package main
func main() { a := [3]byte{1, 2, 3}; var i uint16 = 0; println(a[i]) }`,
			"cannot use multi-byte integer as array index, use byte() to truncate",
		},
		{
			"uint16 struct array index",
			`package main
type Point struct { x byte; y byte }
func main() { a := [2]Point{Point{1, 2}, Point{3, 4}}; var i uint16 = 0; println(a[i].x) }`,
			"cannot use multi-byte integer as array index, use byte() to truncate",
		},
		{
			"unsupported function in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	r := f(n - 1)
	return r + unknown(n)
}
func main() { print(f(1)) }`,
			"unsupported function in recursive function: unknown",
		},
		{
			"recursive function called in recursive function",
			`package main
func g(n byte) byte {
	if n == 0 { return 0 }
	return g(n - 1) + 1
}
func f(n byte) byte {
	if n == 0 { return 0 }
	r := f(n - 1)
	return r + g(n)
}
func main() { print(f(1)) }`,
			"unsupported recursive function in recursive function: g",
		},
		{
			"unsupported defer in recursive",
			`package main
func double(x byte) byte { return x * 2 }
func f(n byte) byte {
	if n == 0 { return 0 }
	defer double(n)
	r := f(n - 1)
	return r
}
func main() { print(f(1)) }`,
			"unsupported defer call in recursive function: double",
		},
		{
			"slices in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	s := make([]byte, 1)
	s[0] = n
	r := f(n - 1)
	return s[0] + r
}
func main() { print(f(1)) }`,
			"slice usage in recursive function is not supported",
		},
		{
			"slice parameter in recursive function",
			`package main
func gen(n byte, acc []byte) []byte {
	if n == 0 { return acc }
	return gen(n - 1, append(acc, n))
}
func main() { _ = gen(3, []byte{}) }`,
			"slice parameter acc in recursive function gen is not supported",
		},
		{
			"slice nesting too deep",
			`package main
func main() {
	var s [][][]byte
	_ = s
}`,
			"slice nesting deeper than 2 levels is not supported",
		},
		{
			"slice nesting too deep make",
			`package main
func main() {
	s := make([][][]byte, 1)
	_ = s
}`,
			"slice nesting deeper than 2 levels is not supported",
		},
		{
			"assign non-slice to slice variable",
			`package main
func main() {
	var s []byte
	s = byte(1)
}`,
			"unsupported slice expression: *ast.CallExpr",
		},
		{
			"field not an array in struct array field index",
			`package main
type S struct { x byte }
func main() {
	s := S{x: 1}
	p := &s
	print(p.x[0])
}`,
			"cannot index non-array expression",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CompileSource(tt.src)
			if err == nil {
				t.Fatal("expected error")
			}
			if err.Error() != tt.err {
				t.Errorf("got error %q, want %q", err, tt.err)
			}
		})
	}
}

func TestMultiFile(t *testing.T) {
	tests := []struct {
		name   string
		srcs   []string
		input  string
		output string
	}{
		{
			"cross-file function call",
			[]string{
				`package main
func main() { putchar(double(36)) }`,
				`package main
func double(x byte) byte { return x + x }`,
			},
			"", "H",
		},
		{
			"cross-file multiple functions",
			[]string{
				`package main
func main() { putchar(add(30, 42)) }`,
				`package main
func add(a, b byte) byte { return a + b }`,
			},
			"", "H",
		},
		{
			"three files",
			[]string{
				`package main
func main() { putchar(inc(double(35))) }`,
				`package main
func double(x byte) byte { return x + x }`,
				`package main
func inc(x byte) byte { return x + 1 }`,
			},
			"", "G", // double(35)=70, inc(70)=71='G'
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, err := compileSources(tt.srcs...)
			if err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			if err := Interpret(code, strings.NewReader(tt.input), &out); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != tt.output {
				t.Errorf("got %q, want %q", got, tt.output)
			}
		})
	}
}

func TestMultiFileError(t *testing.T) {
	tests := []struct {
		name string
		srcs []string
		err  string
	}{
		{
			"duplicate function",
			[]string{
				`package main
func main() {}`,
				`package main
func main() {}`,
			},
			"duplicate function: main",
		},
		{
			"wrong package name in second file",
			[]string{
				`package main
func main() {}`,
				`package util
func helper() {}`,
			},
			"file1.go: expected package main, got package util",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := compileSources(tt.srcs...)
			if err == nil {
				t.Fatal("expected error")
			}
			if err.Error() != tt.err {
				t.Errorf("got error %q, want %q", err, tt.err)
			}
		})
	}
}

func compileSources(srcs ...string) (string, error) {
	fset := token.NewFileSet()
	var files []*ast.File
	for i, src := range srcs {
		file, err := parser.ParseFile(fset, fmt.Sprintf("file%d.go", i), src, 0)
		if err != nil {
			return "", err
		}
		files = append(files, file)
	}
	return compile(files, fset, false)
}

func TestCompileFile(t *testing.T) {
	f, err := os.CreateTemp("", name+"-*.go")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	if _, err := f.WriteString("package main\nfunc main() { putchar(72) }\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	code, err := Compile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := Interpret(code, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "H" {
		t.Errorf("got %q, want %q", got, "H")
	}
}

func TestCompileFileError(t *testing.T) {
	_, err := Compile(filepath.Join(t.TempDir(), "nonexistent.go"))
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestCompileSourceError(t *testing.T) {
	_, err := CompileSource("not valid go")
	if err == nil {
		t.Fatal("expected error for invalid source")
	}
}

func TestGenerateDebug(t *testing.T) {
	src := `package main
func main() { putchar(72) }`
	file, fset, err := ParseSource(src)
	if err != nil {
		t.Fatal(err)
	}
	info, err := Analyze([]*ast.File{file}, fset)
	if err != nil {
		t.Fatal(err)
	}
	prog, err := Lower(info)
	if err != nil {
		t.Fatal(err)
	}
	code := Generate(prog, true)
	if !strings.Contains(code, "# ") {
		t.Error("expected debug comments in output")
	}
	// Verify the debug output is valid BF (comments are non-BF chars).
	var out strings.Builder
	if err := Interpret(code, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "H" {
		t.Errorf("got %q, want %q", got, "H")
	}
}

func TestTestdata(t *testing.T) {
	tests := []struct {
		file   string
		suffix string
	}{
		{"testdata/hello.go", "Hello, World!\n"},
		{"testdata/fizzbuzz.go", "FizzBuzz\n91\n92\nFizz\n94\nBuzz\nFizz\n97\n98\nFizz\nBuzz\n"},
		{"testdata/primes.go", "2 3 5 7 11 13 17 19 23 29 31 37 41 43 47 53 59 61 67 71 73 79 83 89 97\n"},
		{"testdata/factorial.go", "1! = 1\n2! = 2\n3! = 6\n4! = 24\n5! = 120\n"},
		{"testdata/fibonacci.go", "fib(7) = 13\nfib(8) = 21\nfib(9) = 34\nfib(10) = 55\n"},
		{"testdata/collatz.go", "13: 9\n14: 17\n15: 17\n16: 4\n17: 12\n18: 20\n19: 20\n20: 7\n"},
		{"testdata/ackermann.go", "1 2 3 4\n2 3 4 5\n3 5 7 9\n5 13 29 61\n"},
		{"testdata/bubblesort.go", "1 2 3 4 5\n"},
		{"testdata/quicksort.go", "result: 0 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15\n"},
		{"testdata/sieve.go", "2 3 5 7 11 13 17 19 23 29 31 37 41 43 47\n"},
		{"testdata/pascal.go", "1 12 66 220 495 792 924 792 495 220 66 12 1\n"},
		{"testdata/roman.go", "1999 = MCMXCIX\n3888 = MMMDCCCLXXXVIII\n"},
	}
	for _, tt := range tests {
		t.Run(filepath.Base(tt.file), func(t *testing.T) {
			code, err := Compile(tt.file)
			if err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			if err := Interpret(code, strings.NewReader(""), &out); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(out.String(), tt.suffix) {
				t.Errorf("got %q, want suffix %q", out.String(), tt.suffix)
			}
		})
	}
}
