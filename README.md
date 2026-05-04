# go2bf

[![CI Status](https://github.com/itchyny/go2bf/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/itchyny/go2bf/actions?query=branch:main)
[![Go Report Card](https://goreportcard.com/badge/github.com/itchyny/go2bf)](https://goreportcard.com/report/github.com/itchyny/go2bf)
[![MIT License](https://img.shields.io/badge/license-MIT-blue.svg)](https://github.com/itchyny/go2bf/blob/main/LICENSE)
[![release](https://img.shields.io/github/release/itchyny/go2bf/all.svg)](https://github.com/itchyny/go2bf/releases)

Compile Go to Brainfuck!

## Examples

<details>
<summary>FizzBuzz</summary>

```go
package main

func main() {
    for i := byte(1); i <= 100; i++ {
        if i%15 == 0 {
            print("FizzBuzz")
        } else if i%3 == 0 {
            print("Fizz")
        } else if i%5 == 0 {
            print("Buzz")
        } else {
            print(i)
        }
        println()
    }
}
```

Compile to Brainfuck:

```text
 $ go2bf fizzbuzz.go
>>>>>>>>+>>>>>>>>+>>>>>>>>+>>>>>>>>+[>>>>>>>>]>>>[>>>]+++[<+++++>-]<[>+<-[>>>+<<
<-]>>>]<<[<<<]<<<<<<<<[<<<<<<<<]>[-]>>>>>[-]>[-]<<<<<<[>>>>>+>+<<<<<<-]>>>>>>[<<
<<<<+>>>>>>-]<<<<<[-]+>>>>[<<<<[-]>>>>[-]][-]>>[>>>>>>>>]>>>>>>>[-]<[<<<]<<<<<<<
...
```

Compile and run:

```text
 $ go2bf run fizzbuzz.go
1
2
Fizz
4
Buzz
Fizz
...
```

</details>

<details>
<summary>Recursive Fibonacci</summary>

```go
package main

func fib(n byte) byte {
    if n <= 1 {
        return n
    }
    return fib(n-1) + fib(n-2)
}

func main() {
    for i := byte(1); i <= 10; i++ {
        print("fib(")
        print(i)
        print(") = ")
        println(fib(i))
    }
}
```

```text
 $ go2bf run fibonacci.go
fib(1) = 1
fib(2) = 1
fib(3) = 2
fib(4) = 3
fib(5) = 5
fib(6) = 8
fib(7) = 13
fib(8) = 21
fib(9) = 34
fib(10) = 55
```

</details>

<details>
<summary>Structs with methods</summary>

```go
package main

type Point struct {
    x byte
    y byte
}

func (p Point) add(q Point) Point {
    return Point{x: p.x + q.x, y: p.y + q.y}
}

func main() {
    a := Point{x: 1, y: 2}
    b := Point{x: 3, y: 4}
    c := a.add(b)
    print("(")
    print(c.x)
    print(", ")
    print(c.y)
    println(")")
}
```

```text
 $ go2bf run point.go
(4, 6)
```

</details>

<details>
<summary>Reversi</summary>

You can play Reversi written in Brainfuck!

```text
 $ go2bf run testdata/reversi.go
  ABCDEFGH
1 ........
2 ........
3 ...*....
4 ..*OX...
5 ...XO*..
6 ....*...
7 ........
8 ........
X:2 O:2
X move: D3

  ABCDEFGH
1 ........
2 ........
3 ..*X*...
4 ...XX...
5 ..*XO...
6 ........
7 ........
8 ........
X:4 O:1
O move: E3
...
```

</details>

## Installation

```sh
go install github.com/itchyny/go2bf@latest
```

## Usage

```sh
# Compile Go to Brainfuck
go2bf source.go > output.bf

# Compile and run
go2bf run source.go

# Compile multiple files
go2bf run main.go helper.go

# Compile from stdin
echo 'package main ...' | go2bf -

# Compile with debug comments
go2bf -debug source.go
```

## How it works

The compiler pipeline:

1. **Parse** - Uses Go's `go/ast` parser to parse
   the source code
2. **Analyze** - Builds call graph, detects recursion
   and tail calls
3. **Lower** - Converts AST to a structured IR
   (intermediate representation)
4. **Optimize IR** - Constant folding, delta conversion,
   dead store elimination
5. **Generate** - Converts IR to Brainfuck using
   a register-cache CPU model with optimized register
   allocation and stack traffic reduction
6. **Optimize BF** - Peephole optimization on
   the generated Brainfuck code (merging, cancellation,
   dead loop elimination)

### Execution model

The generated Brainfuck uses a CPU-like execution model:

- **5 registers** at positions 1,2,4,5,7 interleaved
  with algorithm temps for neighbor optimization
- **Register cache** with LRU eviction
  (consecutive operations on same variables
  stay in registers, dead temporaries skipped)
- **Stride-8 highway** markers at positions
  8, 16, 24, 32 for fast tape navigation
- **Phase temps** at fixed tape positions 25-39
  for recursive dispatch computation
- **Stride-3 stack** with guard/value/zero cells
  for variable storage
- **Counter-walk technique** for navigating to
  stack slots via the zero column
- **Breadcrumb technique** for far stack access
  (guard=0 marks target slot)
- **Phase dispatch loop** for general recursion
  with dynamic stack frames

## Supported Go features

### Types and operators

- `byte` (`uint8`, 0-255), `uint16` (0-65535),
  `uint32` (0-4294967295), `uint64` (0-2^64-1)
- Arithmetic: `+`, `-`, `*`, `/`, `%`, `++`, `--`,
  `+=`, `-=`, `*=`, `/=`, `%=`, unary `-`
- Bitwise: `&`, `|`, `^`, `&^`, `<<`, `>>`, `^x`,
  `&=`, `|=`, `^=`, `<<=`, `>>=`
- Comparison: `==`, `!=`, `<`, `>`, `<=`, `>=`
  (including array and struct equality)
- Logical: `&&`, `||`, `!` (0 is false, nonzero is true)
- Type conversion: `byte(expr)`, `uintN(expr)`,
  `string(byte)` in `print`/`println`
- Constants: `const n = 10`, `const nl = '\n'`,
  `const msg = "hello"`, `const` blocks with `iota`

### Control flow

- `if`, `else if`, `else` statements
- `for` loops with `break` and `continue`
- `switch` statement on `byte`, `uintN` values (including
   multiple values per case, `default`, and `fallthrough`)

### Functions

- Parameters, return values, multiple return values,
  named return values
- Tail-call recursion optimization
- General recursion (via stack-based dispatch)
  including nested recursive calls (e.g., Ackermann function)
- `defer` for function calls
  (LIFO order; not supported inside loops)

### Arrays

- `[N]byte`, `[N]uintN`, `[N]Point`, `[N][M]byte`,
  `[N][M]uintN`, `[N][M]Point`, `[N][M][K]byte`
- Constant and variable indexing
- Composite literals: `[N]byte{...}`, `[N]byte{0: v}`
- `len(array)`, `cap(array)`, `for i, v := range a`
- Copy assignment, `a[i]++`, `a[i] += v`
- Pass to and return from functions

### Structs

- Top-level and function-local type definitions
- Fields: `byte`, `uintN`, struct, array, or nested array types
- Field access, nested field access (`p.a.x`)
- Composite literals, copy assignment
- `p.x++`, `p.x += v`, `a[i].x = v`, `s.vals[i] = v`
- Pass to and return from functions
- Value and pointer receivers (`func (v T) m()`, `func (p *T) m()`)

### Slices

- `[]byte`, `[]uintN`, `[]Point`, `[][N]byte`, `[][]byte`,
  `[]*byte`, `[]*Point`
- Composite literals: `[]byte{1, 2, 3}`,
  `[]Point{Point{1, 2}, Point{3, 4}}`
- `make([]byte, n)`, `make([]Point, n, cap)`
- Indexing: `s[i]`, `s[i].x` for struct slices
- `len(s)`, `cap(s)`, `for _, v := range s`
- `append(s, v)`, `append(s, a, b, c)`,
  `append(s, t...)` with automatic reallocation
- `copy(dst, src)`, `clear(s)`
- Array slicing: `a[i:j]`, `a[i:]`, `a[:j]`, `a[i:j:k]`, `a[:]`
- Reslicing: `s[i:j]`, `s[i:]`, `s[:j]`, `s[i:j:k]`
- `s == nil`, `s != nil` comparison
- Copy assignment, `s[i]++`, `s[i].x++`
- Pass to and return from functions
- Backed by a dynamic allocator with in-place growth
  when possible (old arrays not freed otherwise)

### Strings

- String variables backed by `[]byte` slices: `s := "hello"`,
  `var s string`, `s = "world"`, `print(s)`, `println(s)`
- `len(s)`, `s[i]`, `s[i:j]`, `s[i:]`, `s[:j]`
- Range over string (by byte, not rune): `for i, b := range s`
- Equality, lexicographic ordering: `s == t`, `s != t`,
  `s < t`, `s > t`, `s <= t`, `s >= t`
- Concatenation `s + t` and compound assign `s += t`
- Conversions: `[]byte(s)`, `string(bs)`, `string(byte('A'))`
- Function parameters and returns, including named string
  returns (`func g() (msg string)`) and multi-return tuples
  containing strings (`func f() (string, byte)`)
- `defer println(s + "!")`, `switch s { case "test": ... }`
- String constants and concatenations of them in `const` blocks
- String fields in structs, struct equality compares string content
- Slices and arrays of strings or byte slices:
  `[]string{"a", "b"}`, `[N]string{"a", "b", "c"}`,
  `[N][]byte{{'h','i'}, {'b','y','e'}}`, `make([]string, n)`

### Pointers

- `*byte`, `*uintN` pointers: `&x`, `*p`, `*p = v`, `*p++`, `*p--`
- `&myStruct`, `&myArray`, `&Point{x: 1, y: 2}`
- `&a[i]`, `&s[i]` -- address of array/slice elements
- `ptr.x` read/write for struct pointers (`ptr := &myStruct`)
- `ptr[i]` read/write, `ptr[i][j]` read/write for array pointers
- `ptr[i].x` read/write for array-of-structs pointers
- `ptr.data[i]` read/write for struct-with-array pointers
- `len(ptr)`, `len(*ptr)`, `cap(ptr)` for array pointers
- `ptr == nil`, `ptr != nil` comparison
- Typed pointer parameters: `func f(p *[N]byte)`,
  `func f(p *Point)`, `func f(p *uintN)`
- Pass pointers to functions for by-reference semantics

### Built-in functions

- `print`, `println` -- decimal output, string literals,
  multi-return expansion
- `len(x)`, `cap(x)` -- arrays, slices, pointers
- `make([]T, n)`, `make([]T, n, cap)` -- slices of byte,
  struct, or array types
- `append(s, v)`, `append(s, a, b, c)`,
  `append(s, t...)` -- with automatic reallocation
- `copy(dst, src)` -- copy slice elements
- `clear(s)` -- zero all slice elements
- `min(a, b, ...)`, `max(a, b, ...)` -- variadic

go2bf extensions:

- `putchar(byte)` -- raw byte output
- `getchar()` -- read a byte from stdin

## Limitations

- No `int`, signed integer types, floating-point number
  types, or complex number types.
- No import statements.
- No maps, interfaces, or channels.
- Array nesting up to `[N][M][K]byte`, `[N][M]Point`,
  or `[N][M]uintN`.
- Slice nesting up to `[][]byte`
  (`[][][]byte`, `[][]Point`, `[][]uintN`, `[][N]uintN`
  not supported).
- No `select`, `go`, or `goto` statements.
- No closures or function pointers.
- Recursive functions do not support recursive calls
  inside `for` loops, mutual recursion, multi-byte
  integers (`uintN`), pointers, or slices.
- Maximum 255 stack slots (variables + temporaries)
  per program. Slice backing arrays share this space;
  programs that allocate many or large slices at runtime
  may silently overflow with no error.
- The built-in Brainfuck interpreter uses a 30,000-cell
  tape with 8-bit wrapping cells.

## Motivation

More than a decade ago, a friend of mine was writing a compiler that
turned a high-level language into Brainfuck. He was a brilliant
programmer -- far beyond my level at the time -- but his ambition was
equally outsized: he wanted to compile Haskell all the way down to
Brainfuck. "Imagine losing a game of Reversi to a Brainfuck program," he
said with a grin. In the end, the Brainfuck code his compiler generated
was simply too slow even for a simple "Hello world" program, and the
Reversi dream remained just that -- a dream.

Years passed, and I grew as a programmer. I set out to build my own
Python-to-Brainfuck compiler written in Haskell, layering abstractions
one on top of another from raw Brainfuck primitives upward. I got as far
as 32-bit integer arithmetic on the 8-bit Brainfuck tape, but then hit a
wall: I could not figure out how to represent a stack. Without a stack,
there could be no function calls, no arrays -- no Reversi. I, too, set
the dream aside.

More years went by. At some point, an idea for implementing a stack on
the Brainfuck tape came to me, but the sheer amount of work it would
take kept me from ever starting. Then the age of AI arrived. With Claude,
I built the entire compiler in just three days -- from the first line of
code to a working Reversi game. A dream I had carried for over a decade,
realized in a long weekend. The compiler supports functions, recursion,
arrays, structs -- and yes, it runs Reversi (also written by Claude).

The day has come, old friend. You can now lose to Reversi written in Brainfuck.

## Bug Tracker

Report bug at [Issues - itchyny/go2bf - GitHub](https://github.com/itchyny/go2bf/issues).

## Author

itchyny (<https://github.com/itchyny>)

## License

This software is released under the MIT License, see LICENSE.
