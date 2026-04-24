# Tape Layout

The Brainfuck tape is organized into a fixed-position CPU area and
a dynamic stack area.

## Overview

```text
Position:  0  1  2  3  4  5  6  7  8  9 10 11 12 13 14 15
Content:   0  R  R  T  R  R  T  R  1  [ algorithm temps ]
           ^                       ^
           backward sentinel       highway marker

Position: 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30 31
Content:   1  [ algorithm temps ]  1 [   phase temps    ]
           ^                       ^
           marker                  marker

Position: 32 33 34 35 36 37 38 39 40 41 42 43 44 45 46 ...
Content:   1 [   phase temps    ]  0 [pad] [  stack...  ]
           ^                       ^
           marker                  forward sentinel

R = register (positions 1,2,4,5,7)
T = algorithm temp (positions 3,6, interleaved with registers)
```

## Regions

### Sentinels (positions 0 and 40)

Always 0. Provide stopping points for highway scans:

- `[<<<<<<<<]` from any marker stops at position 0 (backward sentinel).
- `[>>>>>>>>]` from any marker stops at position 40 (forward sentinel).

Position 40 also serves as the head of the guard column for the stack
(see [`stack.md`](stack.md)).

### Registers (positions 1, 2, 4, 5, 7)

Five general-purpose registers for active computation. The register
cache (see [`cache.md`](cache.md)) maps abstract cell IDs to these positions.
Interleaved with algorithm temps at positions 3 and 6 so that every
register has at least one adjacent neighbor for the neighbor register
optimization (see [`codegen.md`](codegen.md)).

### Algorithm temps (positions 3, 6, 9-15, 17-23)

Scratch cells for codegen primitives. `copy` needs a temp cell, comparison
needs flags, `divmod` needs 6 consecutive cells. Managed by a position
allocator (`alloc`/`free`). Positions 3 and 6 are between registers,
enabling distance-1 neighbor allocation. There are 16 algorithm temp
positions in total.

### Highway markers (positions 8, 16, 24, 32)

Always 1. Placed at stride-8 intervals, these enable O(1) navigation
between the CPU area and the stack sentinel. The stride-8 scan
`[>>>>>>>>]`/`[<<<<<<<<]` jumps across markers and stops only at a
sentinel (value 0).

### Phase temps (positions 25-31, 33-39)

Reserved for recursive dispatch code (see [`recursion.md`](recursion.md)). Separated
from algorithm temps so dispatch code doesn't interfere with the register
cache.

- Position 25: `activeReg` (recursion depth counter)
- Position 26: `retReg` (return value transfer between phases)
- Positions 27-31, 33-39: available for phase code (`noRetFlag`,
  condition variables, argument preparation). Marker at 32 is skipped.

### Padding (positions 41-42)

Used by the counter-walk technique for dynamic array access
(see [`stack.md`](stack.md)). Position 41 holds the walk distance (index +
base slot), which is then moved to position 42 (the padding cell)
for the counter-walk loop.

### Stack (position 43+)

Variable storage. Each variable occupies a 3-cell slot. See [`stack.md`](stack.md)
for the full stack design.

## Highway Navigation

The highway system enables the codegen to navigate between the register
area (near position 0) and any stack slot without knowing the absolute
tape position at compile time.

- **Home to sentinel**: move to the nearest marker
  (round up to next multiple of 8), then `[>>>>>>>>]`.
- **Sentinel to home**: move to position 32 (last marker before
  sentinel), then `[<<<<<<<<]`.
- **Sentinel to stack**: `>>>[>>>]` scans the guard column
  (see [`stack.md`](stack.md)).
- **Stack to sentinel**: `[<<<]` scans backward through the guard
  column to position 40.

All navigation is position-independent: the same BF code works
regardless of how many stack slots are allocated.
