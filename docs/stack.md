# Stack

All Go variables, temporaries, and function parameters live on a stack
that grows rightward from position 43 on the Brainfuck tape.

## Slot Layout

Each stack slot occupies 3 consecutive cells:

```text
[guard | value | zero]
```

- **guard**: 1 if the slot is allocated, 0 if free.
- **value**: the byte value stored in this slot (0-255).
- **zero**: always 0 at rest. Used as a temporary during load
  operations (see "Zero Cell Trick") and as a carrier during
  counter-walk navigation (see "Counter-Walk").

### Example

Three allocated slots starting at position 43:

```text
Pos:  40  41  42  43  44  45  46  47  48  49  50  51  52 ...
      0  pad pad   1  v0   0   1  v1   0   1  v2   0   0 ...
      ^sentinel    ^slot 0     ^slot 1     ^slot 2     ^end
```

### Guard Column Scanning

The sentinel at position 40 is always 0. Guard cells form a
column at positions 43, 46, 49, 52, ... (stride 3). To scan
to the stack end: `>>>[>>>]` -- first `>>>` moves past the
sentinel to the first guard (position 43), then `[>>>]` scans
the guard column and stops at the first 0 (the unallocated
slot past the last allocated one).

## Cell Allocation

The lowerer assigns each variable an abstract **cell** ID starting at
`numFixed` (41). Cell N maps to stack slot `N - numFixed` and tape
position `sentinelFwd + 4 + 3 * slot`.

Cells are allocated sequentially and freed when temporaries go out of
scope. The allocator reuses freed cells (free list). For contiguous
allocations (arrays, structs), `allocCells(n)` reserves N consecutive
cell IDs.

## Frame Push and Pop

`genFramePush(slots)` allocates a new frame by scanning to the stack
end and setting guard=1 for each new slot. For frames larger than 3
slots, a counter loop avoids unrolling:

```text
>>>[>>>]              scan to stack end (from sentinel)
<N                      set counter = N at the last zero cell
[>+<-[>>>+<<<-]>>>]     loop: set guard, shift counter forward
<<                      back to last guard column
[<<<]                   scan guard column back to sentinel
<<<<<<<<                move to highway marker at position 32
[<<<<<<<<]              scan highway back to position 0
```

`genFramePop(slots)` reverses this: scans to the stack end and clears
guards (1 -> 0) walking backward.

## Loading a Value

Loading transfers a stack value to a register byte-by-byte. Each byte
requires a round-trip between the register area and the stack slot.

### Direct Path (slots 0-6)

For nearby slots (within 24 positions of the sentinel), the codegen
navigates directly via absolute position:

```text
clear(rp)                  clear destination register
moveTo(value_cell)         navigate to the value cell
[                          while value > 0:
  ->+<<                      value--; zero++; move to guard column
  [<<<]                      scan guard column to sentinel
  <<<<<<<<[<<<<<<<<]         highway back to home
  rp+                        increment register
  [>>>>>>>>]                 highway to sentinel
  >>>[>>>]>                  scan guard column to value cell
]
>[<+>-]                    restore: move zero cell back to value
<<                         back to guard column
[<<<]                      scan guard column to sentinel
<<<<<<<<[<<<<<<<<]         highway back to home
```

### Breadcrumb Path (slots 7+)

For distant slots, direct navigation would require knowing the absolute
position. Instead, the **breadcrumb technique** marks the target slot by
setting its guard to 0:

```text
1. Navigate to the target slot's guard cell.
2. guard-- (1 -> 0). This is the breadcrumb.
```

Now the guard column has exactly one 0 between the sentinel and the
stack end. This creates two complementary scan behaviors:

- `[<<<]` scans backward through the guard column, stopping at the
  first zero cell. Note that `[<<<]` would stop at the breadcrumb
  guard (0) if the pointer lands on it. The implementation avoids
  this: `homeFromBreadcrumb()` first does `<<<` to step past the
  breadcrumb to the previous guard (which is 1), then calls `[<<<]`
  which scans past all remaining guards to the sentinel (position 40).

- `>>>[>>>]` from the sentinel scans forward and stops at the first
  guard=0, which is the breadcrumb.

This pair enables reliable round-trips:

```text
clear(rp)
navigate to target guard      (compile-time known slot)
-                            set breadcrumb (guard: 1 -> 0)
>                            move to value cell
[                            while value > 0:
  ->+                          value--; zero++
  <<                           move to guard (breadcrumb, =0)
  <<<                          step past breadcrumb to previous guard
  [<<<]                        scan to sentinel
  <<<<<<<<[<<<<<<<<]           scan to home
  rp+                          increment register
  >>>>>>>>[>>>>>>>>]           scan to sentinel
  >>>[>>>]                     scan to breadcrumb (guard=0)
  >                            move to value cell
]
>[<+>-]                      restore: move zero to value
<<+                          restore guard (0 -> 1, remove breadcrumb)
[<<<]<<<<<<<<[<<<<<<<<]      scan home
```

`moveTo` uses highway scans when shorter than direct movement:
forward via `>>>>>>>>[>>>>>>>>]` from near position 0, or backward
via `<<<<<<<<[<<<<<<<<]` from the sentinel area. This reduces
navigation cost for dynamic array access (the padding cell at
position 42 is reached via the sentinel instead of 42 `>` characters).

The BF optimizer eliminates highway round-trips: when `[<<<]`
(guard scan to sentinel) is immediately followed by
`<<<<<<<<[<<<<<<<<]>>>>>>>>[>>>>>>>>]` (home then back to sentinel),
the round-trip is removed since it is a no-op.

### Zero Cell Trick

The zero cell (third cell of each slot) is always 0. During load, it
acts as a temporary:

1. Decrement value, increment zero: `->+`
2. Navigate home, increment register
3. Navigate back to value cell
4. Repeat until value is 0
5. Locally restore value from zero: `>[<+>-]`

The restore is a single adjacent-cell move (no navigation). Without the
zero cell, we would need a second full round-trip to restore the value
from a separate temp.

## Storing a Value

Storing follows the reverse pattern: navigate to the slot, clear the old
value, then transfer the register value byte-by-byte with round-trips.
A temp cell in the register area preserves the register value (since the
copy loop consumes it). After the loop, the register is restored from
the temp via a local `move`.

## Counter-Walk (Dynamic Slot Access)

For variable-indexed array access (`a[i]`), the slot index is only known
at runtime. The **counter-walk** technique uses a runtime counter to
traverse the zero column:

```text
1. Copy the runtime index to position 41 (adjacent to sentinel).
2. Add base_slot to position 41 (compile-time constant).
3. Move the distance from position 41 to position 42 (padding cell).
4. Counter-walk from position 42:
   [[>>>+<<<-]>>>-]           walk loop:
                                shift counter to next zero cell,
                                advance 3 positions,
                                decrement counter
5. >                          advance to the target guard cell
```

Each iteration of the inner loop moves the counter value from the
current zero cell to the next one (`[>>>+<<<-]`). The outer loop
advances the pointer by 3 (`>>>`) and decrements the counter (`-`).
When the counter reaches 0, the pointer is at the last walked zero
cell. A final `>` advances to the target slot's guard cell.

After the walk, the standard breadcrumb load/store handles the actual
data transfer.

### Walk Distance

The distance value is `base_slot + index`. Position 42 is the padding
cell. The first walk step moves the counter to position 45 (slot 0's
zero cell). After D steps, the pointer is at slot D-1's zero cell.
The final `>` advances to slot D's guard cell, which is the target
`base_slot + index`.
