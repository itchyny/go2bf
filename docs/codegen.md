# Code Generation Primitives

The code generator translates IR nodes into Brainfuck using a set of
reusable primitives. All primitives operate on tape positions (registers
or temp cells), not abstract stack cell IDs.

## Constant Setting

`set(cell, value)` clears the cell and adds the value. For values
above 12, uses a multiplication loop to reduce code size:

```text
value = 100: mulFactors(100) = (10, 10, 0)
BF: [-]++++++++++[>++++++++++<-]
     ^clear ^counter=10 ^loop: add 10 to target, dec counter
```

`mulFactors(n)` finds `a, b, r` where `a*b+r = n` and `a+b+r` is
minimized. Examples:

| Value | Factors | BF length |
| ----- | ------- | --------- |
| 10 | direct | 10 (`++++++++++`) |
| 48 | `6*8` | ~18 |
| 65 | `8*8+1` | ~21 |
| 100 | `10*10` | ~24 |
| 255 | `15*17` | ~36 |

## Data Movement

**move(dst, src)**: `dst += src; src = 0`. Single loop, no temp needed.

**subtract(dst, src)**: `dst -= src; src = 0`. Same as `move` but
subtracts.

**copy(dst, src, temp)**: non-destructive copy using a temp cell.

```text
src[dst+ temp+ src-]    move src to dst and temp
temp[src+ temp-]        restore src from temp
```

Copy requires a temp cell and two loops (move + restore), making it
roughly twice as expensive as a destructive move. The lowerer uses
`IRMove` instead of `IRCopy` when the source is a temporary that
will be freed immediately after (e.g., expression results, return
value cells, composite returns going out of scope). The register
cache minimizes how often copies are needed (see
[`cache.md`](cache.md)).

## Arithmetic

**Add**: `dst = src1 + src2`. Copies `src1` to `dst`, then moves a
copy of `src2` into `dst` via `move`.

```text
copy(dst, src1, temp)
copy(t, src2, temp)
move(dst, t)
```

**Subtract**: same structure, but `subtract` instead of `move`.

**Multiply**: nested loops. The outer loop decrements one operand; the
inner loop adds the other operand to `dst` (via copy to preserve it).

```text
// dst = a * b (a is consumed, b preserved via temp)
a[              // for each unit of a:
  copy(t, b)    //   t = b
  move(dst, t)  //   dst += t
  a-            //   a--
]
```

**DivMod**: uses 6 consecutive temp cells `[n, d, 0, 0, 0, 0]` and
the BF algorithm `[->-[>+>>]>[+[-<+>]>+>>]<<<<<]`:

```text
Before: [n,     d,   0,   0, 0, 0]
Result: [0, d-n%d, n%d, n/d, 0, 0]
```

The outer loop decrements n (cell 0) each iteration. The inner loop
decrements the divisor copy (cell 1). When the divisor copy reaches
0, the quotient (cell 3) is incremented and the divisor copy is
reloaded from cell 4. The remainder accumulates in cell 2.
The operands are first copied from registers to the temp area,
and results are moved back to the destination registers after.

## Comparison

**Not**: `dst = (src == 0) ? 1 : 0`.

```text
dst[-]+ src[dst- src[-]] // set dst=1, decr if src!=0
```

**Less-than** (`a < b`): copies a, then loops b times. Each iteration
checks if the copy of a is exhausted. If a runs out first, a < b.

**Less-or-equal**: same loop structure with the result inverted.

**Equality**: `a == b` is computed as `not(a - b)`. The subtraction
wraps at 0/255, but `not` only cares whether the result is zero.

## Control Flow

`emitIf(cond, body)`: executes body once if `cond` is nonzero.
Consumes `cond` (sets it to 0).

```text
cond[ body cond[-] ]
```

This is a BF loop that executes at most once: the body clears `cond`
at the end, so the loop exits.

`emitIfElse(cond, then, else)`: preserves `cond`. Uses two temp cells:

```text
copy(t, cond, u)    // t = cond
u+                  // u = 1 (else flag)
t[                  // if cond != 0:
  then()            //   execute then-branch
  u-                //   clear else flag
  t[-]              //   exit loop
]
u[                  // if else flag still set:
  else()            //   execute else-branch
  u-                //   exit loop
]
```

**while(cond, body)**: standard BF loop.

```text
cond[ body cond ]
```

## Bitwise Operations

Brainfuck has no bitwise primitives. `go2bf` implements AND, OR, XOR,
and shifts via bit decomposition:

1. Decompose both operands into 8 bits using repeated `divmod-by-2`
2. Apply the bitwise operation on each bit pair (0/1 arithmetic)
3. Reconstruct the result byte from 8 bits

The `divmod-by-2` uses an optimized toggle loop instead of the general
`divmod` algorithm:

```text
// Toggle divmod-by-2: q = n/2, r = n%2
r[-]              // r = 0
n[                // while n > 0:
  emitIfElse(r,
    { q+ r- },   // r was 1: increment quotient, clear r
    { r+ })      // r was 0: set r
  n-
]
```

Each iteration toggles r between 0 and 1, incrementing q every other
step. This avoids the 6-cell temp area and counter-based algorithm
that the general `divmod` requires.

## String Literals

String constants in `print`/`println` are emitted as sequences of
`IRConst` + `IRPutc`. The IR optimizer's delta conversion often reduces
these to `IRAddI`/`IRSubI` chains:

```text
// print("Hi!")
IRConst{t, 72}   // 'H'
IRPutc{t}
IRAddI{t, 33}    // 72+33=105='i' (delta, not full const)
IRPutc{t}
IRSubI{t, 72}    // 105-72=33='!'
IRPutc{t}
```

## String Conversion

`string(byte)` in `print`/`println` arguments outputs the byte as a
raw character via `IRPutc`, bypassing `emitPrintByte`.
For example, `print(string(65))` outputs `A`.

## Decimal Number Printing

`emitPrintByte` prints a byte (0-255) as a decimal number with
leading zero suppression:

```text
divmod(src, 10) -> front, ones
if front != 0:
    divmod(front, 10) -> hundreds, tens
    if hundreds != 0:
        putchar(hundreds + '0')
    putchar(tens + '0')
putchar(ones + '0')
```

The second `divmod` is nested inside the first `if`, so single-digit
values (0-9) skip it entirely -- no `divmod`, just one `putchar`.
This reduces the number of `IRIf` nodes at the outer level from 2
to 1, saving a `flushAndInvalidate` per print call.

## Neighbor Register Optimization

Several operations need a temporary cell alongside a register in a
tight loop: `copy` (preserving the source via restore) and
`emitDelta` (multiplication loop counter). Each loop iteration
moves between the register and the temp, so the distance directly
affects code size.

Registers are at positions 1,2,4,5,7 with algorithm temps at 3
and 6 interleaved between them. This ensures every register has
at least one adjacent cell (distance 1) available as a temp.

The `allocTemp(near)` method checks if an adjacent register
(distance 1) is free via the register cache. If found, it uses that
register instead of a distant algorithm temp. For register 1
(position 1), the backward sentinel (position 0) can also serve as
a distance-1 temp for local operations that don't navigate the
highway.

For example, `const r1 72` (setting register 1 to 72 = 8\*9)
with temp at position 9 vs adjacent register 2:

```text
// Without neighbor (temp at position 9, distance 8):
[-]>>>>>>>>[-]++++++++[<<<<<<<<+++++++++>>>>>>>>-]

// With neighbor (r2 at position 2, distance 1):
[-]>[-]++++++++[<+++++++++>-]
```

The inner loop shrinks from 20 chars to 5 chars -- each iteration
saves 15 moves. This gives 1-3% total output size reduction.

## Near Temp Allocation

Arithmetic primitives (`genAdd`, `genSub`, `genMul`, `genNot`,
`genOrder`) allocate algorithm temps for their inner loops. By
default, temps are allocated sequentially from low positions (3, 6,
9, ...), which are near the registers (1-7).

In recursive dispatch phases, operands are phase temps at positions
25-39. The default allocation places temps 20+ positions away,
making each loop iteration expensive. `allocNear` detects phase
temp operands and picks the closest free algorithm temp (typically
21-23), reducing inner loop distance from ~20 to ~4.

This is not applied to the dispatch loop itself (`genDispatch`),
where temps are used in the outer while loop alongside the BF
pointer's home area -- low positions are faster there.

## Flush-Only Before If

Before entering an `IRIf` body, the codegen must ensure the stack
is consistent (dirty registers written back). Previously this used
`flushAndInvalidate`, which also cleared all cache mappings --
forcing the if-body to reload every value from the stack.

Now the codegen uses `flush()` (without invalidate) before the if
when it is safe. The cache mappings remain valid: registers hold the
same values as their stack slots. Inside the if-body, `ensureReg`
finds cached values and skips the reload. At the end of each branch,
`flushAndInvalidate` runs as before.

No `invalidate()` is needed after the if construct. If the body
didn't execute, the cache is still valid from the pre-flush state.
If the body did execute, its `flushAndInvalidate` already cleared
the cache. Either way, the state is correct for subsequent code.

This is safe because `IRIf`'s condition is consumed (set to 0)
by the BF `[body cond[-]]` pattern -- it is never re-checked.
The same technique does not apply to `IRLoop`, where the condition
is re-checked by `]` after every iteration.

**`IRDynStore` exception**: when the if-body contains `IRDynStore`
(variable-indexed array write), the codegen falls back to
`flushAndInvalidate`. `IRDynStore` writes to a runtime-determined
stack slot via counter-walk, bypassing the register cache. The
cache cannot track which slot was written. After the
`IRDynStore`, `ensureReg` may load a cell into a register
that previously held a different value for the same slot.
On the next `flush`, this stale register value overwrites
the stored value on the stack.

## Dead Cell Elimination

When the lowerer frees a temporary cell, it emits `IRFree{Cell}`.
The codegen's `dropCell` method removes the register from the cache
without storing to stack. This prevents dead temporaries from being
flushed at control flow boundaries (`flush`, `flushAndInvalidate`).

For example, `a + b * c` allocates a temporary for `b * c`:

```text
IRMul{t, b, c}     // t = b * c
IRAdd{dst, a, t}   // dst = a + t
IRFree{t}          // t is dead -- codegen drops the register
```

Without `IRFree`, the register holding `t` stays dirty in the cache.
At the next `IRIf` or `IRLoop`, `flush` writes it to stack even
though nothing will read it. With `IRFree`, the register is freed
immediately, saving one `storeToStack` call.

The IR optimizer also benefits: `IRFree` after a write marks the
write as a dead store, which `eliminateDeadStores` can remove.

This gives 5-20% output size reduction depending on how many
temporaries the program uses.

## Memory Initialization Skip

Programs that never access the stack (all values fit in
registers, no control flow triggers cache flushes) skip
memory initialization and frame push entirely. The codegen
emits the init code first, then generates the main code.
If no `storeToStack` or `loadFromStack` was called, the
init prefix is stripped from the output.

This saves ~78 bytes (26%) for register-only programs like
hello world, where the init and frame push would otherwise
dominate the output.
