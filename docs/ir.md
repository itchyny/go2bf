# Intermediate Representation

The IR is a structured, tree-shaped representation between the Go AST
and Brainfuck output. It operates on abstract **cells** (stack slot IDs)
rather than tape positions.

## Design Principles

- **Register-transfer style**: most operations take explicit source and
  destination cells (e.g., `IRAdd{Dst, Src1, Src2}`).
- **Structured control flow**: `IRIf`, `IRLoop`, and `IRBlock` form a
  tree. No gotos or flat label-based control flow.
- **No memory model**: cells are abstract identifiers. The code generator
  decides whether a cell lives in a register or on the stack.

## Node Categories

### Constants and Movement

| Node | Semantics |
| ------ | ----------- |
| `IRZero{Dst}` | dst = 0 |
| `IRConst{Dst, Value}` | dst = constant |
| `IRMove{Dst, Src}` | dst = src; src = 0 |
| `IRCopy{Dst, Src}` | dst = src (src preserved) |

### Arithmetic

| Node | Semantics |
| ------ | ----------- |
| `IRAddI{Dst, Value}` | dst += constant |
| `IRSubI{Dst, Value}` | dst -= constant |
| `IRAdd{Dst, Src1, Src2}` | dst = src1 + src2 |
| `IRSub{Dst, Src1, Src2}` | dst = src1 - src2 |
| `IRMul{Dst, Src1, Src2}` | dst = src1 * src2 |
| `IRDiv{Dst, Src1, Src2}` | dst = src1 / src2 |
| `IRMod{Dst, Src1, Src2}` | dst = src1 % src2 |
| `IRDivMod{QuotDst, RemDst, Src1, Src2}` | quotient and remainder |

`IRDivMod` is a fused operation that computes both quotient and
remainder. The lowerer emits it when adjacent `/` and `%` use the same
operands (e.g., `q, r := a/b, a%b`).

### Bitwise

| Node | Semantics |
| ------ | ----------- |
| `IRAnd{Dst, Src1, Src2}` | dst = src1 & src2 |
| `IROr{Dst, Src1, Src2}` | dst = src1 \| src2 |
| `IRXor{Dst, Src1, Src2}` | dst = src1 ^ src2 |

These are implemented via bit decomposition at the codegen level
(see [`codegen.md`](codegen.md)).

### Comparison and Logic

| Node | Semantics |
| ------ | ----------- |
| `IRCmp{Dst, Src1, Src2, Op}` | dst = (src1 op src2) ? 1 : 0 |
| `IRNot{Dst, Src}` | dst = (src == 0) ? 1 : 0 |

`CmpOp` can be: `CmpEq`, `CmpNeq`, `CmpLt`, `CmpLe`, `CmpGt`, `CmpGe`.

### Control Flow

| Node | Semantics |
| ------ | ----------- |
| `IRIf{Cond, Then, Else}` | conditional (Else may be nil) |
| `IRLoop{Cond, Body}` | while cond != 0 |
| `IRBlock{Nodes}` | sequence of nodes |

When `Else` is nil, `IRIf` uses `emitIf` (consumes cond). When `Else`
is non-nil, it uses `emitIfElse` (preserves cond).

### I/O

| Node | Semantics |
| ------ | ----------- |
| `IRPutc{Src}` | output byte (BF `.`) |
| `IRGetc{Dst}` | input byte (BF `,`) |

### Dynamic Memory (Arrays)

| Node | Semantics |
| ------ | ----------- |
| `IRDynLoad{Dst, Base, Idx}` | dst = stack[base + idx] |
| `IRDynStore{Base, Idx, Src}` | stack[base + idx] = src |

Used for variable-indexed array access. The codegen uses the
counter-walk technique (see [`stack.md`](stack.md)) to navigate to the target slot
at runtime.

### Stack Frame (Recursion)

| Node | Semantics |
| ------ | ----------- |
| `IRFramePush{Slots}` | allocate a new frame on the stack |
| `IRFramePop{Slots}` | deallocate the topmost frame |
| `IRFramePushDyn{Size}` | allocate runtime-determined slots |
| `IRFramePopDyn{Size}` | deallocate runtime-determined slots |
| `IRLoadFrame{Dst, Slot, FrameSize}` | load from the topmost frame |
| `IRStoreFrame{Slot, Src, FrameSize}` | store to the topmost frame |
| `IRDispatch{Active, FrameSize, Phases}` | recursive dispatch loop |

These are used exclusively by the recursive function lowerer
(see [`recursion.md`](recursion.md)). `IRDispatch` contains the entire dispatch loop
structure, with each phase as an `IRBlock`.

### Lifetime

| Node | Semantics |
| ------ | ----------- |
| `IRFree{Cell}` | mark cell as dead (value no longer needed) |

Emitted by the lowerer when `freeCell` is called. The codegen drops
the register without storing to stack, preventing dead temporaries
from being flushed at control flow boundaries.

## Cell Allocation

The lowerer maintains a cell counter starting at `numFixed` (41). Each
`allocCell()` returns the next ID. `freeCell()` returns a cell to the
free list for reuse. `allocCells(n)` allocates N contiguous cells (used
for arrays and structs).

Cells below `numFixed` are phase temp cells (used directly as tape
positions, not stack slots). The codegen distinguishes these with
`isReg(cell)`.

The maximum number of stack slots is 255 (enforced by the compiler).
This limits the total number of live variables and temporaries.

## IR Optimization

The IR optimizer (`ir_optimizer.go`) is a single walk that does three
cleanups in lockstep on each block.

### Constant Folding and Delta Conversion

`known[c] byte` tracks the last constant value the optimizer has put
in cell `c`:

| Optimization | Condition | Result |
| ------------- | ----------- | -------- |
| Skip redundant const | cell holds the same nonzero value | drop `IRConst` |
| Delta add | cell holds V, target is V+D, D < target | emit `IRAddI{D}` |
| Delta sub | cell holds V, target is V-D, D < V | emit `IRSubI{D}` |

Note: `IRConst{c, 0}` is never skipped via this path even when the
cell is known to be 0, because the lowerer reuses freed cells whose
register positions may hold stale data from the codegen's register
cache. The fresh-zero pass below handles untouched cells separately.

Delta conversion is particularly effective for string literal printing,
where consecutive characters often differ by small amounts
(e.g., "fib(" -> 'f', 'i'=f+3, 'b'=i-7, '('=b-58).

`known` is cleared at control-flow boundaries (`IRIf`, `IRLoop`,
`IRDispatch`, unknown nodes) since the optimizer cannot track which
branch executed at runtime.

### Fresh Zero Elimination

In the same walk, an `IRZero{c}` is dropped when `c` has never been
written anywhere prior in the program. The BF tape starts at 0 and
the register cache has no entry for an untouched cell, so the zero
is a no-op.

`everWritten[c]` -- whether `c` has ever been a `Dst` anywhere in
the program. Shared across recursive calls so writes in earlier
branches or pre-marked loop bodies keep later `IRZero`s where the
cell may carry a stale value or cache entry.

For each node `optimizeBlock` updates `everWritten[c] = true` on
every `Dst` it sees (including a *dropped* `IRZero{c}`, so the next
`IRZero{c}` against a now-touched cell stays). On `IRZero{c}`:

```text
IRZero{c}:
  !everWritten[c]   ->   drop the IRZero entirely (don't emit);
                          mark everWritten[c] = true, known[c] = 0
  everWritten[c]    ->   emit, known[c] = 0
```

For `IRLoop` and `IRDispatch`, `markBlockDsts` pre-marks every cell
written anywhere inside the body before the recursive walk visits
it. Without this, a body-local `IRZero{c}` used to reset `c`
between iterations would be the *first* `Dst` the walk sees and
would drop -- breaking iterations after the first.
