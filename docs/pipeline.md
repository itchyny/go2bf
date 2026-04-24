# Compiler Pipeline

`go2bf` compiles Go source code to Brainfuck through six stages:

```text
Go source -> Parse -> Analyze -> Lower ->
  Optimize IR -> Generate BF -> Optimize BF
```

## 1. Parse (`parser.go`)

Uses Go's `go/ast` parser to produce an AST. Multiple files are parsed
into a shared `token.FileSet`. All files must be `package main`.

## 2. Analyze (`analyzer.go`)

Walks the AST to build a function table (`FuncInfo` for each function):

- Collects function declarations with their parameters, return types,
  and body AST
- Parses parameter types including arrays (`[N]byte`), structs
  (`Point`), and pointer types (`*byte`, `*[N]byte`, `*Point`)
- Detects recursive functions via call graph traversal
- Detects mutual recursion cycles and reports the cycle path
- Identifies tail-recursive functions (single recursive call in tail
  position) for tail-call optimization
- Parses struct type definitions (`StructDef` with field names and sizes)
- Evaluates constant expressions (integer literals, character literals,
  `+`, `-`, `*`, `/`, `%`, `^` (complement), `iota` in const blocks,
  `byte()` conversion, string literal constants for `print`/`println`)
- Validates method receivers and desugars `func (p T) m()` to `T.m`

## 3. Lower (`lowerer.go`, `lowerer_rec.go`)

Converts the AST into a structured IR (see [`ir.md`](ir.md)).

- Allocates abstract **cells** for variables and temporaries
- Lowers expressions to register-transfer operations
  (e.g., `x + y` becomes `IRAdd{dst, cellX, cellY}`)
- Inlines non-recursive function calls at each call site, pushing a new
  scope for parameters and locals
- Lowers tail-recursive functions by rewriting them as loops
  (disabled when the function contains `defer`)
- Lowers general recursive functions via phase dispatch
  (see [`recursion.md`](recursion.md))
- Handles structs as contiguous cell ranges with compile-time field
  offsets (`p.x` becomes a direct cell access)
- Handles arrays with constant-index access (direct cell) and
  variable-index access (`IRDynLoad`/`IRDynStore`)
- Lowers composite types (arrays, structs) passed to/from functions
  by cell-by-cell copying
- Lowers `defer` by capturing arguments into cells and emitting deferred
  blocks before each return
- **Skip guard elimination**: the first statement in each block skips
  the `if !returnFlag` guard, since no preceding statement could have
  set the flag
- **Return flag elimination**: functions without `return` statements
  skip the `returnFlag` cell allocation entirely
- **Move semantics**: uses `IRMove` instead of `IRCopy` when the source
  is a temporary freed immediately after, and `IRZero` directly for
  array initialization
- **DivMod fusion**: adjacent `q := a/b; r := a%b` are fused
  into a single `IRDivMod` (both regular and recursive lowering)

## 4. Optimize IR (`ir_optimizer.go`)

Two passes on the IR before code generation:

- **Constant folding and delta conversion**: tracks known cell values;
  converts `IRConst` to `IRAddI`/`IRSubI` when the delta is smaller
  (common in string literal printing)
- **Dead store elimination**: removes writes to cells that are
  overwritten before being read, with tracking cleared at control flow
  boundaries

## 5. Generate BF (`codegen.go`)

Converts IR to Brainfuck using the CPU execution model:

- Maps abstract cells to stack slots (see [`tape.md`](tape.md))
- Manages a 5-register LRU cache to minimize stack traffic
  (see [`cache.md`](cache.md))
- **Neighbor register optimization**: uses adjacent free registers
  (distance 1) as temps for multiplication loops and copies
  instead of distant algorithm temps
- **Near temp allocation**: arithmetic on phase temps (positions
  25-39) allocates the closest free algorithm temp instead of the
  default low positions, reducing inner loop distance
- **Flush-only before if**: preserves register cache mappings when
  entering if-bodies, avoiding redundant stack reloads (disabled
  when the if-body contains `IRDynStore`)
- **Dead cell elimination**: `IRFree` nodes let the codegen drop
  dead registers without storing to stack, skipping flushes for
  temporaries at control flow boundaries
- Generates navigation code using highway markers
- **Highway routing in `moveTo`**: uses highway scans instead of
  direct movement when shorter (forward from near position 0, or
  backward from the sentinel area)
- Emits stack load/store via the breadcrumb technique (see [`stack.md`](stack.md))
- Generates dispatch loops for recursive functions (see [`recursion.md`](recursion.md))
- Uses multiplication loops for efficient constant setting
  (see [`codegen.md`](codegen.md))
- **Memory init skip**: strips highway marker setup and frame
  push when the stack is never accessed

## 6. Optimize BF (`optimizer.go`)

Peephole optimization on the raw Brainfuck string:

- Merges consecutive identical operations, cancels adjacent `+-`
  and `><` pairs
- Removes dead loops (`[-]` after another `[-]`, `[]` at start)
- Eliminates highway round-trips after guard scans
  (`[<<<]<<<<<<<<[<<<<<<<<]>>>>>>>>[>>>>>>>>]` -> `[<<<]`)
