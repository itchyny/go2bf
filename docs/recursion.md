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

Recursive functions are scalar-only -- byte, `uint16`, and
`uint32` for params, locals, and returns (no pointers, no
composites). For multi-byte (uintN) params, `lowerTailCall`
evaluates each arg into an `intSize`-cell temp block before
the parameter update, avoiding overwrites when the same
variable appears in both source and destination of the
assignment (e.g., `return f(a + uint16(1), b)` would otherwise
clobber `a` while computing `b`).

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
Slot 1:         return value (single-byte; uintN returns flow through retReg)
Slot 2..:       parameters (1 cell per byte param,
                intSize cells per uint16/uint32 param)
Then:           local variables (same per-cell rules)
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
(`noRetFlag = 0`), the child frame push is skipped. The dispatch loop sees
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
`noRetFlag = 0`. Call B is skipped.

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

Recursive dispatch uses phase temp cells (tape positions starting at
`phaseTempBase = 25`, up to but not including `sentinelFwd`, skipping
highway markers AND the position just below each marker -- those are
reserved as interleaved algo-temp slots; see
[`tape.md`](tape.md)) instead of stack slots for dispatch control variables:

| Position | Purpose |
| -------- | ------- |
| 25 | `activeReg` - recursion depth counter |
| 26..26+retSize-1 | `retReg` - return value transfer |
| next 4 (bitwise only) | `genDispatch`'s working state (`phase`, `pr`, `flag`, `activeTemp`) |
| then onward | available for phase code (`noRetFlag`, `condVar`, etc.) |

`genDispatch`'s 4 working-state cells normally come from the codegen's
algo-temp pool, close to the registers so frame loads in phase code
stay cheap. But `genBitwise` peaks at ~11 algo temps and the dispatch
loop holds 4 -- the combination overflows when nested under a few
if-guards. So `lowerGeneralRecursion` checks `hasBitwise(body)` and
*only when bitwise is present* reserves 4 phase-temp cells for the
dispatch loop, freeing the full algo-temp pool. `IRDispatch.Phase` /
`Pr` / `Flag` / `ActiveTemp` carry the assigned positions to
`genDispatch`; when they're zero (the non-bitwise case), `genDispatch`
falls back to allocating from the algo-temp pool. Functions that
don't use bitwise keep the cheaper layout -- phase code's frame
loads sit four positions lower and the per-iteration `<>` navigation
is correspondingly smaller (5-13% fewer BF bytes on the existing
recursive testdata).

Using fixed tape positions avoids cache/stack conflicts during the
dispatch loop, since the dispatch code itself reads and writes frame
slots.

The phase-temp area is **dynamic**: `sentinelFwd` defaults to 24,
which means the area is empty -- programs that don't compile recursive
functions pay nothing for the dispatch infrastructure. When a
recursive function is lowered, the compile driver bumps `sentinelFwd`
by `highwayStride = 8` (re-running `Lower`) until the per-phase
allocation fits, capped at 5 strides. Each bump promotes the
previous sentinel position into a marker and opens a new phase-temp
segment. If lowering still overflows at the cap, the compile fails
with `errTooManyLocalsInRec`. See [`tape.md`](tape.md) for the
geometry.

## Deferred Calls

`defer` in recursive functions stores captured arguments in dedicated
frame slots. Before each `return` (and frame pop), the deferred IR
blocks are emitted in LIFO order.

Only `putchar`, `print`, and `println` are accepted as deferred calls
(other targets are rejected upfront). The replay block synthesizes one
`$defer_arg_N` entry in `rc.locals` per non-string argument -- each
entry binds a synthetic name to the captured frame slot. The replay
then delegates to `lowerBuiltinCall` with these synthetic names as
args, which routes back through `rl.lowerExpr` and the normal frame
load path. After the call lowers, the synthetic entries are removed
from `rc.locals` and the cells freed (via `IRFree` emitted into the
replay block, so the runtime free is part of the deferred call).
This delegation reuses the regular print/println separator + newline
+ string-literal expansion logic instead of re-implementing it.

## Supported Features

Recursive functions are scalar-only. Allowed types for parameters,
locals, and return values: `byte`, `uint16`, `uint32`. Pointers
(`*byte`, `*uintN`), composite types (struct, array, slice,
pointer-to-struct), `uint64`, mutual recursion, and recursive calls
inside `for` loops are rejected at compile time. Tail and general
recursion share the same scalar-only rule. The rejections live in
two places:

- **`inlineCall`** (`lowerer.go`) screens parameter and return
  types when the callee is recursive.
- **`rejectComposites`** (`lowerer_rec.go`) walks the body and
  flags any `var x [N]T`, `var p Struct`, `:= [N]T{...}`,
  `:= Struct{...}`, or any `[]T` slice expression before phase
  splitting.

Within those constraints, most Go features work the same as in
non-recursive functions:

- **Non-recursive function calls** (expressions and statements,
  including builtins) are inlined.
- **`for` loops** with byte counters work, including
  `break`/`continue` (with labels).

`divmod` fusion (folding adjacent `q := x/y; r := x%y` into a
single `IRDivMod`) does **not** apply inside recursive phases --
fusion has to be re-checked across the noRetFlag guard wrapper
on each statement, which adds complexity for a minor codegen win
in a code path that is already a small fraction of recursive bodies.
The two regular operations (`IRDiv` + `IRMod`) are emitted instead.

## Multi-Byte Integers

`uint16` and `uint32` are supported as recursive parameters,
locals, and return values. The same lowering pieces handle both
widths via the `intSize` field on `recLocalInfo`.

### Frame Slot Allocation

**Params.** `lowerGeneralRecursion`'s param loop reads
`info.ParamTypes[i].IntSize`: byte params occupy 1
frame slot (`intSize = 0`), `uint16`/`uint32` params occupy
`intSize` consecutive slots. `paramSlot` advances by the param's
slot count rather than always +1.

**Locals.** `collectIntLocals` re-registers any local whose `:=`
RHS is a `uintN(...)` conversion (or a binary op / ident that
infers to `uintN`). Each multi-byte local gets `intSize`
consecutive frame slots starting at the current `rc.frameSize`.
The earlier single-slot entry from `collectLocals` becomes an
unused orphan; this is wasteful by one slot per multi-byte local
but keeps the slot-allocation pass simple.

**Return result vars.** For a uintN return, `buildPhases`
re-allocates the result variable of each call site (`r` in
`r := f(...)`) at the end of the frame with `retSize` slots.

### Argument Passing

When the recursive function is called, the arg-evaluation loops
(in `lowerGeneralRecursion` for the initial push, and in
`buildRecPhaseWithCall` for nested phase pushes) read
`ParamTypes[i].IntSize` and gather `intSize` contiguous cells per
uintN arg. The frame-store loop then walks `paramSlot` by the
arg's cell count so the bytes land in the child frame's
multi-slot param region.

For tail recursion, `lowerTailCall` does the same: it copies
uintN args to a temp block first (so a self-reference like
`f(a+1, b)` doesn't overwrite `a` mid-assignment), then moves
the temp into the param's `intBinding.base..base+intSize-1`.

### Loaded Cell Cache

`recLowerer.lookupVar` allocates `intSize` contiguous cells via
`allocCells(intSize)` on first reference, emits an `IRLoadFrame`
per byte, and caches the base cell in `loadedMap[slot]`. Subsequent
references in the same phase reuse the cached cells.

`reloadAllLocals` and `storeAllLocals` walk `loadedMap` and call
`slotSize(slot)` per cached slot to decide how many cells to
load/store; uintN locals get `intSize` ops, byte locals get one.
Both helpers route through the shared `loadFrame`/`storeFrame`
range emitters (`emit n IRLoadFrame/IRStoreFrame nodes from a
contiguous range`), which are also used by `lookupVar` and the
named-return path in `lowerReturn`.

### Return Path

`retReg` is `phaseTempBase + 1` (default 26) and the return area
spans `retReg`..`retReg+retSize-1`. `lowerReturn`'s uintN branch
lowers the return expression, then emits an `IRMove` for each
byte from the result cells to the corresponding `retReg+j`.
After the dispatch loop exits, `lowerGeneralRecursion` allocates
`retSize` contiguous cells via `allocCells(retSize)` (not separate
`allocCell()` calls -- the free list does not guarantee
contiguity, and downstream consumers like `emitPrintInt` access
`base+k`).

### Ident Lookup Bypass

`recLowerer.lowerExpr` for `*ast.Ident` checks `rc.locals` first
and short-circuits to `lookupVar`, bypassing the base lowerer's
`lookupBinding`. Without this bypass, an outer-scope `intBinding`
for the same name (e.g., `x := f(...)` in the caller pre-allocates
`x` before lowering the recursive call) would shadow the frame
slot and the recursive function would read the caller's cells.

### Why `uint64` Is Rejected

For an 8-byte return, `retReg` would span positions 26..33. Once
`sentinelFwd` is bumped (to 40 or more), position 32 is a highway
marker and must stay 1. Writing the high byte of a return through
that cell corrupts navigation. The same constraint blocks
`uint64` locals (their cell layout has the same span issue) and
`uint64` parameters (the child-frame's argument-marshalling step
funnels through cells that hit the same boundary). `inlineCall`
rejects all three up front with distinct error messages.

## Allocation Across Highway Markers

`allocCells(n)` (used for multi-byte locals and for `retCells`
materialization) checks whether the requested range straddles a
highway marker. If so, it shifts the base past the marker and
re-attempts. If the shifted range still reaches `sentinelFwd`,
it sets `recAllocErr = errTooManyLocalsInRec`, which the compile
driver in `compile()` catches and retries `Lower` with a bumped
sentinel. Without the shift, a `uint16` local landing on cells 31
and 32 (with `sentinelFwd = 40`) would overwrite the marker.

## Helpers

A few shared helpers reduce boilerplate across the `recLowerer` body:

- `slotSize(slot)` -- 1 for byte slots, `intSize` for uintN slots.
  Used by reload/store/named-return paths.
- `loadFrame(dst, slot, n)` / `storeFrame(slot, src, n)` -- emit
  `n` `IRLoadFrame`/`IRStoreFrame` nodes for a contiguous range.
  Single helper used wherever a multi-cell frame transfer is needed.
- `captureBlock(fn) (*IRBlock, error)` -- swaps `rl.nodes` to a
  fresh list while `fn` runs, returns the captured block, restores
  the prior list (even on error). Replaces a manual save/restore
  pattern that previously appeared in 6 places and silently leaked
  on the error paths.
- `enterPhase(rc) func()` -- saves emission state and redirects
  allocation to the phase-temp range. Returns a `defer`-able
  restore. Used by both `buildRecPhase` and `buildRecPhaseWithCall`.
- `allocReturnCells(info)` -- allocates `retSize` zeroed cells
  contiguously when `retSize > 1`; non-contiguously otherwise.
  Shared with regular `inlineCall` since downstream consumers
  (`emitPrintInt`, `emitCopyOrMove` for uintN) require
  contiguity for multi-cell returns.
- `runInlinedFunc(info, retCells, body)` -- swaps in the
  inline-call return context (`returnDst`, `returnFlag`,
  `inFunc`) for `body`, restores afterward. Used by
  `inlineCallInRec` so its body shrinks to arg eval + param
  binding + delegation.

## Limitations

- Recursive calls inside `for` loops are not supported.
- Composite types (struct, array, slice, pointer-to-struct)
  are not supported as parameters, return values, or locals
  in recursive functions. The frame-slot indirection plus
  variable-index if-cascade made composite handling a large
  fraction of `recLowerer` with limited real-world use --
  iterative forms cover most patterns.
- Top-level (global) variables are not accessible from
  recursive functions. `recLowerer.lookupVar` only resolves
  frame-bound locals; references to globals raise a clear
  error so the user knows to refactor (pass globals as
  arguments, or convert the recursion to iteration).
- `uint64` parameters, returns, and locals are rejected;
  `uint16` and `uint32` work. See [Multi-Byte Integers](#multi-byte-integers)
  for why eight-cell layouts collide with highway markers.
- Calling another recursive function from inside a recursive
  function is not supported (only inlined non-recursive helpers).
  Mutual recursion is rejected upfront.
- Local variable shadowing (`x := ...; if cond { x := ...; ... }`)
  flattens to a single frame slot; both writes hit the outer
  slot. Honor the surrounding scope by using distinct names.
- Recursion depth is capped at 255 stack frames at runtime.
  `activeReg` is one byte; pushing the 256th frame wraps it to
  zero and the dispatch loop exits silently (the result is
  whatever `retReg` happened to hold). Linear recursion at
  exactly `f(255)` reaches this boundary because the base case
  needs the 256th frame.
- Switch with multiple non-default cases each containing a
  recursive call is rejected by the phase splitter as
  `unsupported recursive call pattern`. Single-case-recurses or
  default-recurses patterns do work.
