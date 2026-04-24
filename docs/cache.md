# Register Cache

The code generator uses a 5-register LRU cache to minimize expensive
stack round-trips. Most IR operations work on registers; the cache
transparently loads from and stores to the stack as needed.

## Registers

Five registers at tape positions 1, 2, 4, 5, 7. Interleaved with
algorithm temps at positions 3 and 6 so that every register has at
least one adjacent neighbor (distance 1) for the neighbor register
optimization.

## Cache Operations

### ensureReg(cell)

Ensures a cell's value is in a register. If already cached, returns the
register position. Otherwise, evicts the least recently used register
and loads the cell from its stack slot.

Used for source operands: the value must be in a register for BF
operations to access it.

### assignReg(cell, avoid)

Assigns a register for writing. If the cell is already cached, marks it
dirty and returns it. Otherwise, allocates a free register (or evicts
one), avoiding the specified registers. The `avoid` list prevents
clobbering live source operands.

Used for destination operands: the register will be written to, so it
is marked dirty immediately.

### Example

For `IRAdd{Dst: c, Src1: a, Src2: b}`:

```text
r1 = ensureReg(a, nil)       // load a into a register (or find cached)
r2 = ensureReg(b, [r1])      // load b, avoiding eviction of r1
r3 = assignReg(c, [r1, r2])  // assign output register, avoiding sources
// generate BF: r3 = r1 + r2
```

The avoid list on the second `ensureReg` prevents evicting the first
operand's register when all registers are occupied. Without this, LRU
eviction could pick r1, making both operands alias the same register.

### Eviction

When all registers are in use, the allocator cycles through registers
(round-robin via `next` counter). Dirty registers are written back to
their stack slots before reuse. Clean registers are simply discarded.

### Flush and Invalidate

- `flush()`: writes all dirty registers back to stack, preserving
  cache mappings. Used before `IRIf` -- the cache remains valid
  whether or not the body executes (see "Flush-Only Before If" in
  [`codegen.md`](codegen.md)).
- `flushAndInvalidate()`: writes dirty registers back and clears all
  cache entries. Used at `IRLoop`, `IRDispatch`, `IRDynLoad`,
  `IRDynStore`, and at the end of if/else branches.
- `dropCell()`: removes a single cell from the cache without
  write back. Used by `IRFree` when a cell is dead.

## Why 5 Registers

A binary operation `dst = src1 op src2` needs 3 registers. Comparison
and `divmod` need additional temps. With 5 registers, most operations
complete without eviction. Fewer registers would cause thrashing on
common patterns like `a = b + c; d = a * e`.

## Dirty Tracking

A register is marked dirty when written to (`assignReg`). Only dirty
registers need write back on eviction. This avoids unnecessary stack
stores for values that were only read.

Example: `if x > 0 { ... }` loads `x` into a register to evaluate the
condition. If `x` isn't modified, the register can be reused later
without writing `x` back to the stack.

## Cache Coherence

The cache is invalidated after any operation that makes register contents
potentially stale:

- After `IRIf`/`IRLoop`/`IRBlock`: the branch may have modified stack
  slots that are cached in registers
- After `IRDispatch`: recursive calls modify the stack extensively
- After `IRDynStore`: dynamic writes could hit any slot, so all cached
  values are suspect

Individual cells can be flushed with `flushCell(cell)` when a specific
stack slot must be up-to-date (e.g., before a dynamic load that might
read the same slot).
