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
			"tail recursive struct return",
			`package main
type Point struct { x, y byte }
func walk(p Point, n byte) Point {
	if n == 0 { return p }
	return walk(Point{p.x + 1, p.y + 2}, n-1)
}
func main() {
	p := walk(Point{0, 0}, 5)
	print(p.x); print(" "); println(p.y)
}`,
			"", "5 10\n",
		},
		{
			"tail recursive array return",
			`package main
func walk(a [2]byte, n byte) [2]byte {
	if n == 0 { return a }
	return walk([2]byte{a[0] + 1, a[1] + 2}, n-1)
}
func main() {
	r := walk([2]byte{0, 0}, 3)
	print(r[0]); print(" "); println(r[1])
}`,
			"", "3 6\n",
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
			"struct return from recursive function",
			`package main
type Point struct { x byte; y byte }
func f(n byte) Point {
	if n == 0 { return Point{1, 1} }
	p := f(n - 1)
	return Point{p.x * 2, p.y + 1}
}
func main() { p := f(3); println(p.x, p.y) }`,
			"", "8 4\n",
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
			"range with value in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := [3]byte{n, n+1, n+2}
	s := byte(0)
	for _, v := range a {
		s += v
	}
	return s
}
func main() { print(f(2)) }`,
			"", "9",
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
			"array composite literal in recursive function",
			`package main
func f(n byte) byte {
	a := [3]byte{1, 2, 3}
	if n == 0 { return a[0] + a[1] + a[2] }
	b := f(n - 1)
	return b + a[0]
}
func main() { print(f(2)) }`,
			"", "8",
		},
		{
			"array index assign in recursive function",
			`package main
func f(n byte) byte {
	a := [3]byte{0, 0, 0}
	a[0] = n
	a[1] = n * 2
	if n == 0 { return 0 }
	b := f(n - 1)
	return b + a[0] + a[1]
}
func main() { print(f(3)) }`,
			"", "18",
		},
		{
			"var array in recursive function",
			`package main
func f(n byte) byte {
	var a [3]byte
	a[0] = n
	a[1] = n + 1
	a[2] = n + 2
	if n == 0 { return a[0] + a[1] + a[2] }
	b := f(n - 1)
	return b + a[2]
}
func main() { print(f(3)) }`,
			"", "15",
		},
		{
			"array inc/dec in recursive function",
			`package main
func f(n byte) byte {
	a := [2]byte{10, 20}
	a[0]++
	a[1]--
	if n == 0 { return a[0] + a[1] }
	b := f(n - 1)
	return b
}
func main() { print(f(1)) }`,
			"", "30",
		},
		{
			"array variable index read in recursive function",
			`package main
func f(n byte) byte {
	a := [4]byte{10, 20, 30, 40}
	if n == 0 { return a[0] }
	b := f(n - 1)
	return a[n] + b
}
func main() { print(f(3)) }`,
			"", "100",
		},
		{
			"array variable index write in recursive function",
			`package main
func f(n byte) byte {
	var a [3]byte
	a[n] = n * 10
	if n == 0 { return a[0] }
	b := f(n - 1)
	return a[n] + b
}
func main() { print(f(2)) }`,
			"", "30",
		},
		{
			"array len in recursive function",
			`package main
func f(n byte) byte {
	a := [5]byte{1, 2, 3, 4, 5}
	if n == 0 { return byte(len(a)) }
	b := f(n - 1)
	return b
}
func main() { print(f(1)) }`,
			"", "5",
		},
		{
			"array variable index inc/dec in recursive function",
			`package main
func f(n byte) byte {
	a := [3]byte{10, 20, 30}
	a[n]++
	if n == 0 { return a[0] }
	b := f(n - 1)
	return a[n] + b
}
func main() { print(f(2)) }`,
			"", "63",
		},
		{
			"array key-value literal in recursive function",
			`package main
func f(n byte) byte {
	a := [4]byte{1: 10, 3: 30}
	if n == 0 { return a[1] + a[3] }
	b := f(n - 1)
	return b
}
func main() { print(f(1)) }`,
			"", "40",
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
			"array return from recursive function",
			`package main
func f(n byte) [3]byte {
	if n == 0 { return [3]byte{1, 2, 3} }
	a := f(n - 1)
	return [3]byte{a[0] + 1, a[1] + 1, a[2] + 1}
}
func main() {
	a := f(2)
	print(a[0], a[1], a[2])
}`,
			"", "345",
		},
		{
			"array variable return from recursive function",
			`package main
func f(n byte) [2]byte {
	var a [2]byte
	a[0] = n
	a[1] = n * 2
	if n == 0 { return a }
	b := f(n - 1)
	a[0] = a[0] + b[0]
	return a
}
func main() {
	r := f(3)
	print(r[0], r[1])
}`,
			"", "66",
		},
		{
			"struct param in recursive function",
			`package main
type Point struct { x byte; y byte }
func scale(p Point, n byte) Point {
	if n == 0 { return Point{0, 0} }
	q := scale(p, n-1)
	return Point{q.x + p.x, q.y + p.y}
}
func main() {
	r := scale(Point{3, 4}, 3)
	print(r.x, r.y)
}`,
			"", "912",
		},
		{
			"struct literal arg in recursive call",
			`package main
type Point struct { x, y byte }
func f(p Point, n byte) byte {
	if n == 0 { return p.x + p.y }
	r := f(Point{p.x + 1, p.y + 2}, n - 1)
	return r
}
func main() { println(f(Point{0, 0}, 3)) }`,
			"", "9\n",
		},
		{
			"binary search recursive",
			`package main
func bsearch(a [8]byte, target, lo, hi byte) byte {
	if lo >= hi { return 255 }
	mid := (lo + hi) / 2
	if a[mid] == target { return mid }
	if a[mid] < target {
		return bsearch(a, target, mid+1, hi)
	}
	return bsearch(a, target, lo, mid)
}
func main() {
	a := [8]byte{2, 5, 8, 12, 16, 23, 38, 56}
	print(bsearch(a, 23, 0, 8))
	print(" ")
	print(bsearch(a, 2, 0, 8))
	print(" ")
	println(bsearch(a, 99, 0, 8))
}`,
			"", "5 0 255\n",
		},
		{
			"array equality in recursive function",
			`package main
func f(a, b [3]byte, n byte) byte {
	if n == 0 { return 0 }
	r := f(a, b, n - 1)
	if a == b { return r + 1 }
	return r
}
func main() {
	print(f([3]byte{1, 2, 3}, [3]byte{1, 2, 3}, 3))
	print(" ")
	println(f([3]byte{1, 2, 3}, [3]byte{1, 2, 4}, 3))
}`,
			"", "3 0\n",
		},
		{
			"array param in recursive function",
			`package main
func sum(a [5]byte, n byte) byte {
	if n == 0 { return a[0] }
	b := sum(a, n-1)
	return b + a[n]
}
func main() {
	a := [5]byte{10, 20, 30, 40, 50}
	print(sum(a, 4))
}`,
			"", "150",
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
			"array of structs in recursive function",
			`package main
type Point struct { x byte; y byte }
func f(n byte) byte {
	a := [2]Point{Point{1, 2}, Point{3, 4}}
	if n == 0 { return a[0].x + a[1].y }
	b := f(n - 1)
	return b + a[0].x
}
func main() { print(f(2)) }`,
			"", "7",
		},
		{
			"2d array in recursive function",
			`package main
func f(n byte) byte {
	a := [2][3]byte{{1, 2, 3}, {4, 5, 6}}
	if n == 0 { return a[0][1] + a[1][2] }
	b := f(n - 1)
	return b + a[0][0]
}
func main() { print(f(2)) }`,
			"", "10",
		},
		{
			"variable index array of structs in recursive function",
			`package main
type Point struct { x byte; y byte }
func f(n byte) byte {
	a := [3]Point{Point{1, 2}, Point{3, 4}, Point{5, 6}}
	if n == 0 { return a[0].x }
	b := f(n - 1)
	return b + a[n].x
}
func main() { print(f(2)) }`,
			"", "9",
		},
		{
			"variable index 2d array in recursive function",
			`package main
func f(n byte) byte {
	a := [3][2]byte{{10, 20}, {30, 40}, {50, 60}}
	if n == 0 { return a[0][0] }
	b := f(n - 1)
	return b + a[n][1]
}
func main() { print(f(2)) }`,
			"", "110",
		},
		{
			"variable outer index 2d array in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	var m [3][2]byte
	m[n-1][0] = n
	m[n-1][1] = n * 10
	r := f(n - 1)
	return m[n-1][0] + m[n-1][1] + r
}
func main() { println(f(3)) }`,
			"", "66\n",
		},
		{
			"variable inner index 2d array in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	var m [2][3]byte
	j := n - 1
	m[0][j] = n
	m[1][j] = n * 2
	r := f(n - 1)
	return m[0][j] + m[1][j] + r
}
func main() { println(f(3)) }`,
			"", "18\n",
		},
		{
			"both variable index 2d array in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	var m [3][3]byte
	i := n - 1
	j := n - 1
	m[i][j] = n
	r := f(n - 1)
	return m[i][j] + r
}
func main() { println(f(3)) }`,
			"", "6\n",
		},
		{
			"2d array assign in recursive function",
			`package main
func f(n byte) byte {
	var a [2][2]byte
	a[0][0] = n
	a[0][1] = n + 1
	a[1][0] = n + 2
	a[1][1] = n + 3
	if n == 0 { return a[0][0] + a[1][1] }
	b := f(n - 1)
	return b + a[0][1]
}
func main() { print(f(2)) }`,
			"", "8",
		},
		{
			"nested struct literal in recursive function",
			`package main
type Point struct { x byte; y byte }
type Rect struct { min Point; max Point }
func f(n byte) byte {
	r := Rect{min: Point{1, 2}, max: Point{n, n + 1}}
	if n <= 2 { return r.max.x }
	a := f(n - 1)
	return a + r.min.x
}
func main() { print(f(4)) }`,
			"", "4",
		},
		{
			"method call in recursive function",
			`package main
type Point struct { x byte; y byte }
func (p Point) sum() byte { return p.x + p.y }
func f(n byte) byte {
	p := Point{n, n + 1}
	if n == 0 { return p.sum() }
	a := f(n - 1)
	return a + p.sum()
}
func main() { print(f(3)) }`,
			"", "16",
		},
		{
			"array literal as recursive call argument",
			`package main
func f(a [3]byte, n byte) byte {
	if n == 0 { return a[0] + a[1] + a[2] }
	a[0]++
	b := f(a, n - 1)
	return b
}
func main() { print(f([3]byte{10, 20, 30}, 3)) }`,
			"", "63",
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
			"named return with recursive expression assign",
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
			"struct field assign in recursive function",
			`package main
type P struct { x byte; y byte }
func f(n byte) byte {
	if n == 0 { return 0 }
	var p P
	p.x = n
	p.y = n + 1
	return p.x + p.y + f(n-1)
}
func main() { print(f(3)) }`,
			"", "15",
		},
		{
			"nested struct field assign in recursive function",
			`package main
type Inner struct { x byte; y byte }
type Outer struct { a Inner; b byte }
func f(n byte) byte {
	if n == 0 { return 0 }
	var o Outer
	o.a.x = n
	o.a.y = n * 2
	o.b = n * 3
	return o.a.x + o.a.y + o.b + f(n-1)
}
func main() { print(f(2)) }`,
			"", "18",
		},
		{
			"struct field inc dec in recursive function",
			`package main
type P struct { x byte; y byte }
func f(n byte) byte {
	if n == 0 { return 0 }
	var p P
	p.x = n
	p.x++
	p.y = n
	p.y--
	return p.x + p.y + f(n-1)
}
func main() { print(f(2)) }`,
			"", "6",
		},
		{
			"struct field compound assign in recursive function",
			`package main
type P struct { x byte; y byte }
func f(n byte) byte {
	if n == 0 { return 0 }
	var p P
	p.x = 1
	p.x += n
	return p.x + f(n-1)
}
func main() { print(f(3)) }`,
			"", "9",
		},
		{
			"recursive with chained array index",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	var a [2][2]byte
	a[0][0] = n
	a[1][1] = n + 1
	return a[0][0] + a[1][1] + f(n-1)
}
func main() { print(f(2)) }`,
			"", "8",
		},
		{
			"recursive with struct composite literal",
			`package main
type P struct { x byte; y byte }
func f(n byte) byte {
	if n == 0 { return 0 }
	p := P{x: n, y: n + 1}
	return p.x + p.y + f(n-1)
}
func main() { print(f(2)) }`,
			"", "8",
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
			"recursive with struct field inc dec",
			`package main
type P struct { x byte }
func f(n byte) byte {
	if n == 0 { return 0 }
	var p P
	p.x = n
	p.x--
	return p.x + f(n-1)
}
func main() { print(f(4)) }`,
			"", "6",
		},
		{
			"recursive with array variable index and inc",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	var a [3]byte
	i := n % 3
	a[i] = n
	return a[i] + f(n-1)
}
func main() { print(f(4)) }`,
			"", "10",
		},
		{
			"recursive with array not equal",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := [2]byte{n, n + 1}
	b := [2]byte{n, n + 2}
	if a != b { return 1 + f(n-1) }
	return f(n-1)
}
func main() { print(f(3)) }`,
			"", "3",
		},
		{
			"recursive with struct array field access",
			`package main
type P struct { x, y byte }
func f(n byte) byte {
	if n == 0 { return 0 }
	var a [2]P
	a[0].x = n
	a[0].y = n + 1
	return a[0].x + a[0].y + f(n-1)
}
func main() { print(f(2)) }`,
			"", "8",
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
			"array copy in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := [3]byte{n, n+1, n+2}
	b := a
	return b[0] + b[1] + b[2] + f(n-1)
}
func main() { print(f(2)) }`,
			"", "15",
		},
		{
			"struct copy in recursive function",
			`package main
type P struct { x byte; y byte }
func f(n byte) byte {
	if n == 0 { return 0 }
	var a P
	a.x = n
	a.y = n + 1
	b := a
	return b.x + b.y + f(n-1)
}
func main() { print(f(3)) }`,
			"", "15",
		},
		{
			"dynamic struct field assign in recursive function",
			`package main
type P struct { x byte; y byte }
func f(n byte) byte {
	if n == 0 { return 0 }
	var a [3]P
	i := n - 1
	a[i].x = n
	a[i].y = n * 2
	return a[i].x + a[i].y + f(n-1)
}
func main() { print(f(3)) }`,
			"", "18",
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
			"nested struct field in recursive function",
			`package main
type Inner struct { x byte; y byte }
type Outer struct { a Inner; b byte }
func f(n byte) byte {
	if n == 0 { return 0 }
	var o Outer
	o.a.x = n
	o.a.y = n + 1
	o.b = n + 2
	return o.a.x + o.a.y + o.b + f(n-1)
}
func main() { print(f(2)) }`,
			"", "15",
		},
		{
			"2d array constant index assign in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	var a [2][3]byte
	a[0][1] = n
	a[1][2] = n + 1
	return a[0][1] + a[1][2] + f(n-1)
}
func main() { print(f(2)) }`,
			"", "8",
		},
		{
			"variable index array assign in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	var a [4]byte
	i := n - 1
	a[i] = n * 10
	return a[i] + f(n-1)
}
func main() { print(f(3)) }`,
			"", "60",
		},
		{
			"struct literal return in recursive function",
			`package main
type P struct { x byte; y byte }
func f(n byte) P {
	if n == 0 { return P{x: 0, y: 0} }
	p := f(n - 1)
	return P{x: p.x + n, y: p.y + n*2}
}
func main() {
	r := f(3)
	println(r.x, r.y)
}`,
			"", "6 12\n",
		},
		{
			"array literal return in recursive function",
			`package main
func f(n byte) [2]byte {
	if n == 0 { return [2]byte{0, 0} }
	a := f(n - 1)
	return [2]byte{a[0] + n, a[1] + 1}
}
func main() {
	r := f(3)
	println(r[0], r[1])
}`,
			"", "6 3\n",
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
			"nested struct read in recursive function",
			`package main
type Inner struct { x byte }
type Outer struct { a Inner }
func f(n byte) byte {
	if n == 0 { return 0 }
	var o Outer
	o.a.x = n
	return o.a.x + f(n-1)
}
func main() { print(f(3)) }`,
			"", "6",
		},
		{
			"struct field dec in recursive function",
			`package main
type P struct { x byte; y byte }
func f(n byte) byte {
	if n == 0 { return 0 }
	var p P
	p.x = n
	p.x--
	return p.x + f(n-1)
}
func main() { print(f(3)) }`,
			"", "3",
		},
		{
			"array dec in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	var a [2]byte
	a[0] = n
	a[0]--
	return a[0] + f(n-1)
}
func main() { print(f(3)) }`,
			"", "3",
		},
		{
			"2d array read variable outer index in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	var a [3][2]byte
	a[0][0] = 10
	a[1][0] = 20
	a[2][0] = 30
	i := n - 1
	return a[i][0] + f(n-1)
}
func main() { print(f(3)) }`,
			"", "60",
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
			"array literal return with key-value in recursive function",
			`package main
func f(n byte) [2]byte {
	if n == 0 { return [2]byte{0: 0, 1: 0} }
	a := f(n - 1)
	return [2]byte{0: a[0] + n, 1: a[1] + 1}
}
func main() {
	r := f(3)
	println(r[0], r[1])
}`,
			"", "6 3\n",
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
			"inline call with array param in recursive function",
			`package main
func sum(a [3]byte) byte { return a[0] + a[1] + a[2] }
func f(a [3]byte, n byte) byte {
	if n == 0 { return 0 }
	r := f(a, n - 1)
	return sum(a) + r
}
func main() { println(f([3]byte{1, 2, 3}, 2)) }`,
			"", "12\n",
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
			"divmod fusion in recursive function",
			`package main
func digitSum(n byte) byte {
	if n < 10 { return n }
	q := n / 10
	r := n % 10
	return r + digitSum(q)
}
func main() { println(digitSum(199)) }`,
			"", "19\n",
		},
		{
			"switch with tag in recursive function",
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
			"nested struct field read in recursive function",
			`package main
type Inner struct { x byte }
type Outer struct { a Inner }
func f(n byte) byte {
	if n == 0 { return 0 }
	o := Outer{Inner{n}}
	return o.a.x + f(n - 1)
}
func main() { println(f(4)) }`,
			"", "10\n",
		},
		{
			"nested struct field inc dec in recursive function",
			`package main
type Inner struct { x byte }
type Outer struct { a Inner }
func f(n byte) byte {
	if n == 0 { return 0 }
	o := Outer{Inner{n}}
	o.a.x++
	r := f(n - 1)
	return o.a.x + r
}
func main() { println(f(3)) }`,
			"", "9\n",
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
			"chained index assign in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	var m [2][2]byte
	m[0][0] = n
	m[0][1] = n + 1
	m[1][0] = n + 2
	m[1][1] = n + 3
	r := f(n - 1)
	return m[0][0] + m[1][1] + r
}
func main() { println(f(2)) }`,
			"", "12\n",
		},
		{
			"2D array assign in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	var m [2][2]byte
	m[0][0] = n
	m[1][1] = n * 2
	return m[0][0] + m[1][1] + f(n - 1)
}
func main() { println(f(3)) }`,
			"", "18\n",
		},
		{
			"zero length array in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := [0]byte{}
	_ = a
	r := f(n - 1)
	return r + n
}
func main() { println(f(3)) }`,
			"", "6\n",
		},
		{
			"array inc dec in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	var a [3]byte
	a[0] = n
	a[0]++
	a[1] = n
	a[1]--
	return a[0] + a[1] + f(n - 1)
}
func main() { println(f(3)) }`,
			"", "12\n",
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
			"struct field assign sum in recursive function",
			`package main
type Point struct { x, y byte }
func f(n byte) byte {
	if n == 0 { return 0 }
	var p Point
	p.x = n
	p.y = n * 2
	return p.x + p.y + f(n - 1)
}
func main() { println(f(3)) }`,
			"", "18\n",
		},
		{
			"method call as statement in recursive function",
			`package main
type Point struct { x, y byte }
func (p Point) show() {
	print(p.x); print(","); print(p.y); print(" ")
}
func f(n byte) byte {
	if n == 0 { return 0 }
	p := Point{n, n * 2}
	p.show()
	r := f(n - 1)
	return r
}
func main() { f(3) }`,
			"", "3,6 2,4 1,2 ",
		},
		{
			"range with value in recursive function",
			`package main
func f(a [4]byte, n byte) byte {
	if n == 0 { return 0 }
	s := byte(0)
	for _, v := range a {
		s += v
	}
	r := f(a, n - 1)
	return s + r
}
func main() { println(f([4]byte{1, 2, 3, 4}, 2)) }`,
			"", "20\n",
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
			"recursive 2d array both variable indices",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	var a [3][3]byte
	i := n % 3
	j := (n + 1) % 3
	a[i][j] = n
	return a[i][j] + f(n-1)
}
func main() { print(f(4)) }`,
			"", "10",
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
			"recursive nested struct field access",
			`package main
type Inner struct { v byte }
type Outer struct { i Inner }
func f(n byte) byte {
	if n == 0 { return 0 }
	var o Outer
	o.i.v = n
	return o.i.v + f(n-1)
}
func main() { print(f(3)) }`,
			"", "6",
		},
		{
			"recursive struct array variable index field assign",
			`package main
type P struct { x, y byte }
func f(n byte) byte {
	if n == 0 { return 0 }
	var a [2]P
	i := n % 2
	a[i].x = n
	a[i].y = n + 1
	return a[i].x + a[i].y + f(n-1)
}
func main() { print(f(3)) }`,
			"", "15",
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
			"defer with array access in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	var a [2]byte
	a[0] = n
	defer print(a[0])
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
			"zero length array",
			`package main
func main() {
	a := [0]byte{}
	_ = a
	print("Y")
}`,
			"", "Y",
		},
		{
			"zero length array equality",
			`package main
func main() {
	a := [0]byte{}
	b := [0]byte{}
	if a == b { print("Y") } else { print("N") }
}`,
			"", "Y",
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
		print(a[i][0]); print(","); print(a[i][1]); print(" ")
	}
	println()
}`,
			"", "1,10 2,20 3,30 \n",
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
			"array of structs variable index struct assign",
			`package main
type Point struct { x, y byte }
func main() {
	var pts [3]Point
	for i := range byte(3) {
		pts[i] = Point{i + 1, (i + 1) * 2}
	}
	for i := range byte(3) {
		print(pts[i].x); print(","); print(pts[i].y); print(" ")
	}
	println()
}`,
			"", "1,2 2,4 3,6 \n",
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
			"slice of structs read write",
			`package main
type Point struct { x, y byte }
func main() {
	s := make([]Point, 3)
	s[0].x = 1; s[0].y = 2
	s[1].x = 3; s[1].y = 4
	s[2].x = 5; s[2].y = 6
	for i := range byte(3) {
		if i > 0 { print(" ") }
		print(s[i].x); print(","); print(s[i].y)
	}
}`,
			"", "1,2 3,4 5,6",
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
			"integer overflow",
			`package main
func main() { putchar(256) }`,
			"integer literal 256 out of byte range",
		},
		{
			"undefined variable",
			`package main
func main() { putchar(x) }`,
			"undefined variable: x",
		},
		{
			"unsupported function",
			`package main
func main() { foo() }`,
			"unsupported function: foo",
		},
		{
			"import not supported",
			`package main
import "fmt"
func main() { fmt.Println("hello") }`,
			"imports are not supported",
		},
		{
			"wrong argument count",
			`package main
func f(x byte) byte { return x }
func main() { f(1, 2) }`,
			"expects 1 arguments, got 2",
		},
		{
			"array out of bounds",
			`package main
func main() {
	a := [3]byte{1, 2, 3}
	putchar(a[3])
}`,
			"array index 3 out of bounds",
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
			"undefined",
		},
		{
			"string literal",
			`package main
func main() {
	x := "hello"
	putchar(x)
}`,
			"string literals can only be used with print/println",
		},
		{
			"too many locals in recursive function",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	a := n; b := n; c := n; d := n
	e := n; g := n; h := n; i := n
	j := n
	s := f(n - 1)
	return s + a + b + c + d + e + g + h + i + j
}
func main() { putchar(f(1)) }`,
			"too many local variables in recursive function",
		},
		{
			"unsupported expression statement",
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
			"expected package main",
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
			"unsupported statement in recursive function",
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
			"unsupported recursive call as statement",
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
			"unsupported expression statement",
		},
		{
			"parse error",
			`package main
func main() { `,
			"expected",
		},
		{
			"composite literal out of bounds",
			`package main
func main() {
	a := [2]byte{0: 1, 5: 2}
	putchar(a[0])
}`,
			"array index 5 out of bounds",
		},
		{
			"array index out of bounds write",
			`package main
func main() {
	a := [3]byte{1, 2, 3}
	a[3] = 4
}`,
			"out of bounds",
		},
		{
			"multi return array out of bounds",
			`package main
func f() (byte, byte) { return 1, 2 }
func main() {
	var a [2]byte
	a[0], a[2] = f()
}`,
			"out of bounds",
		},
		{
			"multi return undefined array",
			`package main
func f() (byte, byte) { return 1, 2 }
func main() {
	x, b[0] = f()
	_ = x
}`,
			"undefined",
		},
		{
			"deref assign undefined variable",
			`package main
func main() {
	*x = 1
}`,
			"undefined variable",
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
			"must be an array",
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
			"must be an array",
		},
		{
			"putchar no args",
			`package main
func main() { putchar() }`,
			"putchar expects 1 argument",
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
			"no return value",
		},
		{
			"unknown function in expression",
			`package main
func main() { x := unknown() }`,
			"unsupported function",
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
			"undefined struct",
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
			"putchar expects 1 argument",
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
			"too many variables",
		},
		{
			"void function in recursive expression",
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
			"undefined variable in recursive function",
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
			"len()",
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
			"const out of range",
			`package main
const x = 256
func main() {}`,
			"const x: value 256 out of byte range (0-255)",
		},
		{
			"const division by zero",
			`package main
const x = 10 / 0
func main() {}`,
			"division by zero in constant expression",
		},
		{
			"const modulo by zero",
			`package main
const x = 10 % 0
func main() {}`,
			"modulo by zero in constant expression",
		},
		{
			"unknown struct field",
			`package main
type Point struct { x byte; y byte }
func main() {
	p := Point{1, 2}
	print(p.z)
}`,
			"unknown field z",
		},
		{
			"struct argument undefined",
			`package main
type Point struct { x byte; y byte }
func f(p Point) byte { return p.x }
func main() {
	print(f(q))
}`,
			"undefined",
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
			"unsupported recursive call pattern",
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
			"cannot take address",
		},
		{
			"use struct as byte",
			`package main
type P struct { x byte }
func main() {
	p := P{1}
	putchar(p)
}`,
			"cannot use struct",
		},
		{
			"use array as byte",
			`package main
func main() {
	a := [3]byte{1, 2, 3}
	putchar(a)
}`,
			"cannot use array",
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
			"out of bounds",
		},
		{
			"array nesting too deep",
			`package main
func main() {
	var a [2][3][4][5]byte
	a[0][0][0][0] = 1
}`,
			"nesting deeper than 3",
		},
		{
			"string literal in expression",
			`package main
func main() {
	x := "hello"
	putchar(x)
}`,
			"string literals can only be used with print/println",
		},
		{
			"integer literal out of byte range",
			`package main
func main() { putchar(300) }`,
			"out of byte range",
		},
		{
			"unsupported call in recursive expression",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	r := f(n - 1)
	return r + unknown(n)
}
func main() { print(f(1)) }`,
			"unsupported call in recursive expression",
		},
		{
			"unsupported function call in recursive",
			`package main
func f(n byte) byte {
	if n == 0 { return 0 }
	unknown()
	r := f(n - 1)
	return r
}
func main() { print(f(1)) }`,
			"unsupported function in recursive function",
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
			"unsupported defer call in recursive function",
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
			"slices in recursive functions are not supported",
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
			"unsupported slice expression",
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
			if !strings.Contains(err.Error(), tt.err) {
				t.Errorf("got error %q, want it to contain %q", err, tt.err)
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
			"expected package main, got package util",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := compileSources(tt.srcs...)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.err) {
				t.Errorf("got error %q, want it to contain %q", err, tt.err)
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
