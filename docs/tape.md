# Tape Layout

The Brainfuck tape is organized into a fixed-position CPU area and
a dynamic stack area. The forward sentinel position is **dynamic**:
it sits at position 24 by default, and the compile driver bumps it
by `highwayStride` (8) at a time when a recursive function's
phase-temp pool overflows. Each bump promotes the previous sentinel
position into a marker and extends the phase temp range by one
stride.

## Default Layout (`sentinelFwd = 24`)

Non-recursive code and tail-call-optimized recursion compile against
this layout. The phase-temp pool is empty -- no dispatch
infrastructure is allocated unless the program needs it.

```text
Position:  0  1  2  3  4  5  6  7  8  9 10 11 12 13 14 15
Content:   0  R  R  T  R  R  T  R  1  [ algorithm temps ]
           ^                       ^
           backward sentinel       highway marker

Position: 16 17 18 19 20 21 22 23 24 25 26 27 28 ...
Content:   1 [  algorithm temps ]  0 [pad] [ stack... ]
           ^                       ^
           marker                  forward sentinel

R = register (positions 1,2,4,5,7)
T = algorithm temp (positions 3,6, interleaved with registers)
```

## Bumped Layout (`sentinelFwd = 32, 40, 48, ...`)

When `Lower` reports `errTooManyLocalsInRec`, the compile driver
adds `highwayStride` to `sentinelFwd` and re-lowers, capped at
`maxSentinelBumps` (8) bumps -- so `sentinelFwd` can grow from 24
up to 88, exposing up to 56 phase-temp cells (positions 25-87
minus the seven markers in between). Each bump promotes the
previous sentinel position into a marker and pushes the sentinel
further out, opening a new phase-temp segment.

After two bumps (`sentinelFwd = 40`):

```text
Position:  0  1 .. 23 24 25 26 27 28 29 30 31
Content:   0  R ..  T  1 [  phase temps  ]  T
                       ^ marker             ^ algorithm temp

Position: 32 33 34 35 36 37 38 39 40 41 42 43 44 ...
Content:   1 [  phase temps  ]  T  0 [pad] [stack...]
           ^ marker             ^  ^ forward sentinel
                                algorithm temp
```

Phase temps occupy positions `phaseTempBase = 25` through `sentinelFwd-1`,
skipping any markers (multiples of `highwayStride = 8`) that fall in
that range AND the position just below each marker (reserved as an
interleaved algo-temp slot; see Algorithm temps above). The pad cells
and stack always begin two cells past the forward sentinel.

## Regions

### Sentinels (positions 0 and `sentinelFwd`)

Always 0. Provide stopping points for highway scans:

- `[<<<<<<<<]` from any marker stops at position 0 (backward sentinel).
- `[>>>>>>>>]` from any marker stops at the forward sentinel.

The forward sentinel doubles as the head of the guard column for
the stack (see [`stack.md`](stack.md)).

### Registers (positions 1, 2, 4, 5, 7)

Five general-purpose registers for active computation. The register
cache (see [`cache.md`](cache.md)) maps abstract cell IDs to these positions.
Interleaved with algorithm temps at positions 3 and 6 so that every
register has at least one adjacent neighbor for the neighbor register
optimization (see [`codegen.md`](codegen.md)).

### Algorithm temps (positions 3, 6, 9-15, 17-23, interleaved)

Scratch cells for codegen primitives. `copy` needs a temp cell, comparison
needs flags, `divmod` needs 6 consecutive cells. Managed by a position
allocator (`alloc`/`free`). Positions 3 and 6 are between registers,
enabling distance-1 neighbor allocation. There are 16 fixed algorithm
temp positions in total.

When `sentinelFwd` grows past the default 24, the position just below
each new marker is also added to the algo-temp pool (positions 31, 39,
47, 55, ... up to but not below `sentinelFwd`). These interleaved
slots sit in the middle of the phase-temp range, so a `copy` /
`add` / `move` between two high phase-temp operands can pick a scratch
two or three cells away instead of walking 25+ cells back to position
23. The lowerer's `isMarkerOrAlgoTemp` check keeps these positions
free of phase-code allocations. `allocTemp` uses `allocNear(operand)`
to pick the closest free slot, so the interleaved positions naturally
get chosen when an operand is in the phase-temp range.

### Highway markers (positions 8, 16, ...)

Always 1. Placed at stride-8 intervals between the backward sentinel
and the forward sentinel, these enable O(1) navigation between the CPU
area and the stack sentinel. The stride-8 scan `[>>>>>>>>]`/`[<<<<<<<<]`
jumps across markers and stops only at a sentinel (value 0). Default
layout has markers at 8 and 16; each bump adds a marker at the previous
sentinel position (24, 32, 40, ...).

The memory-init pass sets these cells to 1. For up to four markers it
emits `>>>>>>>>+` per marker (unrolled). For five or more markers it
uses a walking-counter loop -- the same technique `genFramePush` uses,
just at stride 8 -- which has constant source size and leaves the
cursor one cell before the forward sentinel, shortening the subsequent
`moveToSentinel` to a single `>`.

### Phase temps (`phaseTempBase` through `sentinelFwd-1`, skipping markers and algorithm temps)

Reserved for recursive dispatch code (see [`recursion.md`](recursion.md)).
Separated from algorithm temps so dispatch code doesn't interfere
with the register cache. Empty in the default layout (`sentinelFwd = 24`)
since `phaseTempBase = 25` is past the sentinel; populated only when the
compile driver bumps `sentinelFwd` to fit a recursive function.

When present:

- Position 25: `activeReg` (recursion depth counter)
- Position 26: `retReg` (return value transfer between phases; spans
  `retSize` cells for uintN returns)
- Positions past `retReg`, skipping markers (32, 40, ...) AND the
  interleaved algo-temp slot just below each marker (31, 39, 47, ...):
  available for phase code (`noRetFlag`, condition variables,
  argument preparation, and -- for bitwise-using functions -- the
  four dispatch-loop working cells).

### Padding (two cells past `sentinelFwd`)

Used by the counter-walk technique for dynamic array access
(see [`stack.md`](stack.md)). The first cell past the forward sentinel
holds the walk distance (index + base slot), which is then moved to
the second pad cell for the counter-walk loop.

### Stack (`sentinelFwd+3` onward)

Variable storage. Each variable occupies a 3-cell slot. See
[`stack.md`](stack.md) for the full stack design.

## Highway Navigation

The highway system enables the codegen to navigate between the register
area (near position 0) and any stack slot without knowing the absolute
tape position at compile time.

- **Home to sentinel**: move to the nearest marker
  (round up to next multiple of `highwayStride`), then `[>>>>>>>>]`.
- **Sentinel to home**: move to the last marker before the forward
  sentinel, then `[<<<<<<<<]`.
- **Sentinel to stack**: `>>>[>>>]` scans the guard column
  (see [`stack.md`](stack.md)).
- **Stack to sentinel**: `[<<<]` scans backward through the guard
  column to the forward sentinel.

All navigation is position-independent: the same BF code works
regardless of how many stack slots are allocated, and regardless of
whether the layout has been bumped.
