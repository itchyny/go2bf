# Recursion

`go2bf` supports general recursion (including non-tail recursion like
`fib(n-1) + fib(n-2)` and nested recursive calls like
`ack(m-1, ack(m, n-1))`) via a phase dispatch model with explicit
stack frames.

## Tail Recursion

Tail-recursive functions (where the recursive call is the last operation)
are optimized into loops during lowering. No stack frames or dispatch
needed.

```go
func gcd(a, b byte) byte {
    if b == 0 { return a }
    return gcd(b, a%b)          // tail call -> becomes a loop
}
```

The lowerer rewrites this as:

```text
loop:
  if b == 0 { return a }
  a, b = b, a%b
  goto loop
```

Struct and array parameters are supported in tail calls.
`lowerTailCall` copies composite arguments to temporaries before
assigning to parameter cells, avoiding overwrites when the same
variable appears in both source and destination. Composite literal
arguments (e.g., `return walk(Point{p.x+1, p.y+2}, n-1)`) are
evaluated into temp cells before the parameter update.

When a tail-recursive function contains `defer`, the tail-call
optimization is disabled. The loop rewrite would lose per-call
`defer` semantics -- `defer` would only run once instead of once
per logical call. These functions fall back to general
recursion dispatch.

## General Recursion: Phase Dispatch

Non-tail-recursive functions use a phase dispatch model. The function
body is split into **phases** at each recursive call site. A dispatch
loop executes one phase per iteration on the topmost stack frame.

### Phase Splitting

```go
func fib(n byte) byte {
    if n <= 1 { return n }     // base case
    a := fib(n - 1)            // <- phase boundary (call site 0)
    b := fib(n - 2)            // <- phase boundary (call site 1)
    return a + b
}
```

This produces 3 phases:

| Phase | Statements | Ends with |
| ----- | ---------- | --------- |
| 0 | `if n <= 1 { return n }` | push child frame for `fib(n-1)` |
| 1 | load child result into `a` | push child frame for `fib(n-2)` |
| 2 | load child result into `b`; `return a + b` | pop frame |

### Frame Layout

Each recursive call gets its own stack frame with this slot layout:

```text
Slot 0:         phase number (which phase to execute next)
Slot 1:         return value (also receives child's result)
Slot 2..2+P-1:  parameters
Slot 2+P..end:  local variables
```

### Dispatch Loop

The generated dispatch loop:

```text
active = 1                          // initial call
while active > 0:
    phase = load frame slot 0       // read phase number
    if phase == 0: [phase 0 code]
    if phase == 1: [phase 1 code]
    if phase == 2: [phase 2 code]
    active = reload                 // may have changed
```

Phase matching uses a decrement chain: copy phase to a working register
`pr`, check `pr == 0` (matches phase 0), decrement `pr`, check again
(matches phase 1), etc. After a match, `pr` is set to a large value to
skip remaining checks.

### noRetFlag

Each phase starts with `noRetFlag = 1`. A `return` statement:

1. Stores result in `retReg`
2. Sets `noRetFlag = 0`
3. Pops frame
4. Decrements `active`

The call setup at the end of each phase is guarded by
`if noRetFlag { ... }`. If the base case returned in the `preStmts`
(`noRetFlag`=0), the child frame push is skipped. The dispatch loop sees
the decremented `active` and processes the parent frame's next phase.

```text
Phase 0:
  noRetFlag = 1
  [preStmts: if n <= 1 { return n }]    // may set noRetFlag=0
  if noRetFlag {                        // only if no return happened
    [evaluate args, store locals, set phase=1, active++, push child]
  }
```

### Return Value Passing

Child return values pass through `retReg`:

1. The child's `return` copies its result to `retReg` and pops frame
2. The dispatch loop re-enters the parent's next phase
3. The next phase's first instruction: `storeFrame(result_slot, retReg)`

## Recursion in If-Branches

### Single Branch

When recursive calls appear in only one branch of an if, the compiler
flattens the if-statement by inverting the condition. The non-recursive
branch becomes an early-return guard:

```go
// Original:
if n > 1 {
    a := f(n - 1)
    return a + 1
}
return n

// Flattened for phase splitting:
if !(n > 1) { return n }     // guard: sets noRetFlag=0 if taken
a := f(n - 1)                // now at top level
return a + 1
```

### Both Branches (Conditional Calls)

When both branches contain recursive calls:

```go
if n%2 == 0 {
    return f(n/2) + 1       // call A
}
return f(n*3+1) + 1         // call B
```

The condition is stored in a frame variable `$cond`. Call A becomes
conditional -- it only pushes a child frame when `$cond` is true.
When false, the phase number advances without pushing a child:

```text
Phase 0:
  preStmts: if n<=1 { return 0 }; $cond = (n%2 == 0)
  if noRetFlag:
    if $cond:
      [evaluate n/2, store locals, set phase=1, active++, push child]
    else:
      [store locals, set phase=1]     // no child push, no active change

Phase 1:
  load retReg into $rec_0
  if $cond: return $rec_0 + 1        // then-branch return
  // if $cond was false: falls through (noRetFlag stays 1)
  [evaluate n*3+1, store locals, set phase=2, active++, push child]

Phase 2:
  load retReg into $rec_1
  return $rec_1 + 1                  // else-branch return
```

When `$cond` is false: phase 0 advances to phase 1 without pushing a
child (active unchanged). The dispatch loop immediately executes phase 1
on the same frame. The `if $cond { return }` is skipped, falling through
to call B.

When `$cond` is true: phase 0 pushes a child, which eventually returns.
Phase 1 loads the result, and `if $cond { return }` executes, setting
`noRetFlag`=0. Call B is skipped.

## Recursion in Switch

Switch statements containing recursive calls are desugared to if-else
chains before phase splitting. The tag expression (if any) is stored
in a frame variable `$switch`, and each case becomes an equality check:

```go
switch n {
case 0: return 1
case 1: return 2
default: return f(n-1) + f(n-2)
}
```

Becomes:

```text
$switch := n
if $switch == 0 { return 1 }
else if $switch == 1 { return 2 }
else { return f(n-1) + f(n-2) }
```

The resulting if-else chain is then handled by the existing
if-branch flattening machinery.

## Loops in Recursive Functions

`for` and `range` loops work inside recursive function phases.
The `recLowerer` implements its own `lowerFor` and `lowerRange` that
use the `recLowerer`'s expression/statement handlers (frame-aware
variable access) while maintaining the loop control flow structure.

Key implementation detail: at the end of each loop body iteration,
`storeAllLocals` writes all loaded phase temp cells back to their
frame slots. This ensures the next iteration's `IRLoadFrame`
instructions read the updated values.

`break` works via the same `breakGuard` + `condCell` clearing
mechanism as the regular lowerer -- when break fires, the else
branch of the `breakGuard` if-else clears `condCell` to 0, causing
the `IRLoop` to exit.

`continue` works by wrapping each loop body statement in a skip guard
(`if !loopSkipFlag { ... }`). To ensure correct variable values across
guarded blocks, `lowerLoopBody` pre-loads all referenced variables
from the frame before the guarded loop, then stores and reloads
between each guarded statement.

`break label` and `continue label` are also supported in recursive
functions. The recursive lowerer pushes a frame onto the same
`loopFrames` stack used by the regular lowerer (it embeds `*Lowerer`),
so `emitLabeledBranch` walks across both regular and recursive frames
uniformly. `recLowerer.lowerLabeledStmt` exists only so the inner
loop is dispatched via `recLowerer.lowerStmt` rather than the embedded
base lowerer.

## Phase Temps

Recursive dispatch uses phase temp cells (tape positions 25-39) instead
of stack slots for dispatch control variables:

| Position | Purpose |
| -------- | ------- |
| 25 | `activeReg` - recursion depth counter |
| 26 | `retReg` - return value transfer |
| 27-31, 33-39 | available for phase code (`noRetFlag`, `condVar`, etc.) |

Using fixed tape positions avoids cache/stack conflicts during the
dispatch loop, since the dispatch code itself reads and writes frame
slots.

## Deferred Calls

`defer` in recursive functions stores captured arguments in dedicated
frame slots. Before each `return` (and frame pop), the deferred IR
blocks are emitted in LIFO order.

## Frame-Based Indexing

The `recLowerer` uses frame-based indexing helpers that parallel
the regular lowerer's `indexInto`/`writeInto`:

- **`recIndexInto(baseSlot, count, indexExpr)`** -- reads a scalar
  from a frame array. Constant index emits `IRLoadFrame` directly;
  variable index uses an if-cascade via `emitFrameIndexRead`.
- **`recWriteInto(baseSlot, count, indexExpr, val)`** -- writes a
  scalar to a frame array. Same dispatch as `recIndexInto`.
- **`recFieldIndexInto(baseSlot, info, offset, indexExpr)`** --
  reads a struct field from a frame struct array (adds field offset
  to `i*elemSize`).
- **`recFieldWriteInto(baseSlot, info, offset, indexExpr, val)`** --
  writes a struct field in a frame struct array.

These replace per-call-site constant/variable dispatch and are used
by `lowerIndexExpr`, `lowerArrayAssign`, `lowerArrayIncDec`,
`lowerSelectorExpr`, and `lowerFieldAssign`.

The `compositeSize(name)` helper resolves the total frame slot count
for a variable by checking the `arrayInfo` and `structType` fields on
its `recLocalInfo` entry in `recContext.locals`. It is used by
`lowerRecVarInit` and `lowerDecl` to avoid duplicated map lookups.

## Supported Features

Most Go features supported in regular functions also work in
recursive functions. Key differences from regular lowering:

- **Variable-index arrays** use an if-cascade (checking each
  possible index) instead of `IRDynLoad`/`IRDynStore`, since
  frame slots are not directly addressable by the stack
  counter-walk.
- **Composite equality** (`a == b`, `p != q`) loads each
  element pair from the frame before comparing.
- **Non-recursive function calls** (expressions and
  statements, including methods) are inlined with struct
  and array parameters loaded from the frame.
- **`for i, v := range arr`** resolves the range limit at
  compile time from the frame's array metadata.
- **`divmod` fusion** applies in recursive phases, matching
  the regular lowerer.

### Limitations

- Recursive calls inside `for` loops are not supported.
- Pointers are not supported in recursive functions.
- Slices are not supported in recursive functions, including
  slice parameters, slice return types, and slice locals.
