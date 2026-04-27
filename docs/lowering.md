# Lowering

The lowerer (`lowerer.go`) converts Go AST into structured IR. This is
the largest compiler stage, handling all Go language features.

## Function Inlining

`go2bf` has no runtime function call mechanism for non-recursive functions.
Instead, every call site is **inlined**: the function body is duplicated
at each call site with parameters copied from arguments and return values
copied to the caller.

```go
func double(x byte) byte { return x + x }
func main() { println(double(3)) }
```

Becomes (conceptually):

```text
// At the call site of double(3):
param_x = 3               // copy argument
ret = param_x + param_x   // inlined body
// ret is used by println
```

This means the same function compiled into different call sites may
produce different IR (the optimizer sees different contexts).

## Variable Allocation

Each Go variable maps to one or more abstract cells:

- `byte` variable: 1 cell
- `[N]byte` array: N contiguous cells
- Struct: contiguous cells (one per `byte` field)
- Slice: 3 cells (`ptr`, `len`, `cap`)
- Pointer: 1 cell (stack slot index)

Temporaries for intermediate expression results are allocated and freed
as needed. The allocator reuses freed cell IDs.

## Expression Results

`exprResult` is the return type of `lowerExpr` and related functions.
It carries the cell, ownership, and composite/indexing metadata:

| Field | Description |
| ----- | ------- |
| `cell` | cell ID (or pointer slot for `isPointer` results) |
| `temp` | if true, caller must free via `freeCell` |
| `size` | total cells; 0 means scalar (1 cell) |
| `elemSize` | element size for indexable results; 0 = not indexable |
| `elemCount` | number of elements for indexable results |
| `elemType` | struct type name for composite array elements |
| `elemSlice` | true if elements are slices (`[][]byte`) |
| `elemPtrType` | struct type for pointer elements (`[]*Point`) |
| `innerElemSize` | for nested arrays: cells per inner element (0 if flat) |
| `typeName` | struct type name of this result (for field resolution) |
| `isPointer` | cell is a pointer (stack slot index) for indirect access |
| `flatBase` | for flat-offset results: base of the original array |
| `lenCell` | runtime length cell for slices (0 if compile-time) |
| `capCell` | runtime capacity cell for slices (0 if not applicable) |

`lowerExpr` returns composite results for arrays (`elemSize`/`elemCount`
set), structs (`size`/`typeName` set), pointer-to-array variables
(`isPointer`/`elemType` set), and pointer-to-struct variables
(`isPointer`/`typeName` set). Function calls returning arrays or structs
also return composite results with proper metadata.

`typeName` enables field resolution without re-inspecting the AST:
`lowerSelectorExpr` and `lowerFieldAssign` use `base.typeName` to
look up the `StructDef` directly. `elemType` propagates through
`indexInto` so that `a[i].x` resolves the struct type from the
indexed result. This allows chained access on any expression
(e.g., `f().x`, `f()[i].x`, `f().data[i]`).

### Variable Initialization

Simple assignments (`x = expr`, `x := expr`) and `var x = expr`
declarations share a unified path through `lowerVarInit`, which
handles composite literals, pointer tracking, and composite variable
copies. For composite RHS (struct/array variables), `lowerExpr`
returns a multi-cell result and `emitCopyOrMove` handles the
cell-by-cell transfer.

### Field Assignment

`lowerFieldAssign` resolves the base via `lowerExpr(sel.X)`, then
dispatches by result type:

- **Pointer** (`isPointer`): `ptrOffset` + `ptrStore`
- **Nested struct**: `lowerStructValueTo` for composite field writes
- **Direct/flat-offset**: `writeInto` with the field offset as a
  constant index expression

## Structs

Structs are lowered as contiguous cell ranges with compile-time field
offsets:

```go
type Point struct { x byte; y byte }
p := Point{x: 3, y: 5}
```

`p` occupies 2 cells: `p.x = cell[base]`, `p.y = cell[base+1]`.
Field access is a direct cell reference (no runtime offset computation).

### Nested Structs

Fields can be other structs:

```go
type Rect struct { min Point; max Point }
```

`Rect` occupies 4 cells: `min.x`, `min.y`, `max.x`, `max.y`.
Chained access `r.min.x` is resolved recursively at compile time.

### Struct Fields with Arrays

Fields can be arrays:

```go
type Vec struct { data [3]byte; len byte }
```

`Vec` occupies 4 cells. `v.data[i]` resolves the field offset at
compile time, then indexes into the array.

### Multi-Assignment and Swap

Parallel assignment (`a, b = b, a`) evaluates all RHS values into
temporaries via `lowerExpr` + `ensureTemp`, then assigns to LHS.
Both `lowerExpr` and `ensureTemp` handle composite results
(struct/array variables return multi-cell results with `size` set),
so no special-casing is needed for composite swap.

### Method Receivers

Method receivers are desugared: `func (p Point) sum() byte` becomes a
function `Point.sum` with `p` as the first parameter.

Method calls resolve the receiver's struct type via
`resolveExprTypeName`, which walks the AST without evaluating.
This supports method calls on any expression that produces a struct:
variables (`p.sum()`), array elements (`a[i].sum()`), function
returns (`makePoint(1, 2).sum()`), and chained methods
(`p.scale(3).sum()`). The receiver expression is evaluated via
`lowerExpr` and passed as the first argument to the inlined method.

## Arrays

Constant-indexed access (`a[3]`) is a direct cell reference at
`base + 3`. No runtime computation needed.

Variable-indexed access (`a[i]`) cannot use a direct cell reference
because the index is not known at compile time. The lowerer emits
`IRDynLoad`/`IRDynStore`, which the codegen implements via
counter-walk (see [`stack.md`](stack.md)).

### Unified Indexing

All index operations (read and write) go through two central functions:

- **`indexInto(base, indexExpr)`** -- reads from a composite at the
  given index. Handles direct arrays, pointer arrays, flat-offset
  chained access, constant indices, and variable indices.
- **`writeInto(base, indexExpr, val)`** -- writes to a composite at
  the given index. Same dispatch as `indexInto`.

`lowerIndexExpr` evaluates `e.X` via `lowerExpr` (which returns
composite metadata), then calls `indexInto`. `lowerArrayAssign`
does the same with `writeInto`. This replaces per-type dispatch
(previously separate functions for pointer arrays, chained indices,
selector bases, etc.).

For pointer-based access (`isPointer` results), `indexInto`/`writeInto`
use `ptrDynIndex` + `ptrLoad`/`ptrStore`. For variable-index
composite arrays, `indexInto` returns a flat-offset result with
`flatBase` set, so the next level of indexing knows the original
array base for dynamic access.

### Arrays of Structs

`[N]Point` occupies `N * sizeof(Point)` contiguous cells. Constant
index `a[3].x` is a direct cell access at `base + 3*2 + 0`. Variable
index `a[i].x` computes a flat index `i * elemSize + fieldOffset`
and uses a single dynamic load/store. Whole-struct assignment
`a[i] = Point{...}` with variable index evaluates the struct into
temp cells and dynamic-stores each field.

### Arrays of Arrays

`[N][M]byte` occupies `N * M` contiguous cells. `a[i][j]` is
handled by recursive evaluation: `lowerExpr(a[i])` returns a
composite result (with `elemSize=1, elemCount=M` for constant `i`,
or a flat-offset result with `flatBase` for variable `i`), then
`indexInto` handles `[j]`.

Up to 3 levels of nesting are supported (`[N][M][K]byte`).
The `innerElemSize` field in `arrayInfo` and `exprResult` tracks
the sub-element size. When `indexInto` returns a composite
sub-element, it uses `innerElemSize` to set the next level's
`elemSize` and `elemCount`. Nested struct arrays (`[N][M]Point`)
propagate `elemType` through all levels.

Struct fields may also contain nested arrays. `FieldInnerSizes`
in `StructDef` stores the inner element size for nested array
fields (e.g., `data [2][3]byte` has inner size 3).

Variable-index reads and writes use `IRDynLoad`/`IRDynStore`, which
compute a walk distance (`base + index`) and navigate to the target
slot via counter-walk (see [`stack.md`](stack.md)). This avoids
generating code proportional to the array size.

Array parameters and returns are passed by cell-by-cell copying.
For a function `func f(a [3]byte)`, the caller copies 3 cells into
the function's parameter slots.

## Slices

A slice variable occupies 3 cells (`ptr`, `len`, `cap`) plus
compile-time metadata (`elemSize`, `elemType`, `elemSlice`,
`elemPtrType`). Slice operations reuse the pointer
infrastructure (`ptrDynIndex`, `ptrLoad`, `ptrStore`) for
indexed access. For struct slices, `s[i].x` computes
`ptr + i * elemSize + fieldOffset`. For pointer slices
(`[]*Point`), `elemPtrType` tracks the pointed-to struct
type; `s[i]` loads the pointer and tags the result with
`isPointer` and `typeName` so field access and method calls
dispatch through the pointer-to-struct path.

`lowerSliceExpr` evaluates any slice-producing expression
(`make`, `append`, literals, slice expressions, variables,
function calls) into a temporary 3-cell header, separating
evaluation from assignment. Nested expressions like
`append(make([]byte, n), v)` work via recursive evaluation.

Backing arrays are allocated from a bump allocator
(`heapPtr` cell). The heap starts after all statically
allocated cells. `heapPtr` is always the first cell
allocated (slot 0), reserving slot 0 so that no user
variable occupies it. This makes pointer value 0 a
reliable nil sentinel for both slices and pointers.
`s == nil` and `p == nil` compare the cell against 0.
Each allocation pushes guard slots via `IRFramePush`
(constant sizes) or `IRFramePushDyn` (variable sizes), and
bumps `heapPtr` by `cap * elemSize` cells. `var r []byte`
initializes all header cells to 0 (`nil`).
Appending to a `nil` slice triggers full reallocation.

`append(s, v)` checks `len < cap`. If room, stores the
value at `ptr + len * elemSize` and increments `len`. If
full, doubles the capacity (or sets it to 1 if zero).
For struct slices, `resolveStructArg` evaluates the value
into temp cells. For `[][]byte`, `lowerSliceExpr` evaluates
the inner header. When `ptr + cap * elemSize == heapPtr`
(backing array at heap top), the array is extended in-place.
Otherwise, a new array is allocated, old elements are copied
via a counted loop, and the old array is leaked.
Variadic `append(s, a, b, c)` emits multiple single-element
appends. `append(s, t...)` ensures capacity, then bulk
copies `len(t) * elemSize` cells from source to destination.

`copy(dst, src)` copies `min(len(dst), len(src)) * elemSize`
cells via a counted loop. Both arguments can be any slice
expression (variable, reslice, array slice).

`clear(s)` zeroes `len(s) * elemSize` cells via a counted
loop starting at `ptr`.

Three-index slicing `s[low:high:max]` sets `cap = max - low`
instead of inheriting the source capacity.

Composite literals (`[]Point{P{1,2}, P{3,4}}`) use
`resolveStructArg` per element, storing each at
`ptr + i * elemSize` via `ptrStore`.

`len(m[i])` and `cap(m[i])` for `[][]byte` inner slices
are handled by loading the inner header via
`loadSliceElement` in the `len`/`cap` handler.

Composite slices (`[]Point`, `[][N]byte`, `[][]byte`)
use `elemSize > 1`. For `[][]byte`, each element is a
3-cell inner header; `indexInto` detects `elemSlice` and
loads it automatically. Element-to-element copy
(`s[0] = s[1]`) loads and stores each field via
`ptrLoad`/`ptrStore`. Pointer-based struct results
(e.g., `s[0]` for `[]Point`) are materialized into
contiguous temp cells when passed as function arguments
or assigned to local struct variables.

Reslicing (`s[i:j]`) propagates `elemSize`, `elemType`,
and `elemSlice` from the source. The analyzer stores
`SliceElemSize` and `SliceElemType` in `ReturnInfo` for
functions returning struct slices. `scanAndAllocLocals`
detects struct slice range values and `tmp := s[i]`
patterns to allocate struct-sized variables.

## Pointers

Pointers are byte values holding stack slot indices. `&x` emits
`IRConst` with the compile-time slot index. `&a[i]` with variable
index computes `slotOf(a.base) + i` at runtime. `&s[i]` for slices
computes `ptr + i * elemSize` via `ptrDynIndex`. `&Point{x: 1}`
lowers the composite literal into cells and returns the slot index.
Dereference (`*p`) uses `IRDynLoad{BaseSlot: 0, Index: p}` and
`IRDynStore` for writes. `nil` is lowered as 0. There is no
bounds checking on pointer values: out-of-range dereferences
silently read/write arbitrary stack slots.

Struct pointers (`ptr := &myStruct` or `ptr := &Point{...}`) are
tracked in the scope's `ptrType` map. `ptr.x` emits `IRDynLoad`
at `*ptr + fieldOffset`. Functions returning `*Point` set
`ReturnType.StructType` with `IsPointer`, so `lowerCallExpr`
tags the result for `ptrType` tracking by the caller.

Array pointers (`ptr := &myArray`) are tracked in `ptrArray`.
`lowerExpr(ptr)` returns an `exprResult` with `isPointer: true` and
the array's `elemSize`/`elemCount`. Functions returning `*[N]byte`
set `ReturnType.IsPointer` and `ReturnType.ArraySize` so that
`lowerCallExpr` tags the result as a pointer-to-array. All pointer
indexing goes through the generic `indexInto`/`writeInto` path,
which uses `ptrDynIndex` + `ptrLoad`/`ptrStore` for `isPointer`
results.

`ptr[i]` computes `*ptr + i * elemSize` and loads/stores via
`IRDynLoad`/`IRDynStore`. `ptr[i][j]` is handled by recursive
`indexInto`: the first level returns a pointer sub-array result,
the second level indexes into it.

Mixed access patterns are supported:

- `ptr[i].x` for array-of-structs pointers: `indexInto` returns
  a pointer sub-array, `lowerSelectorExpr` adds the field offset
- `ptr.data[i]` for struct-with-array pointers: `lowerSelectorExpr`
  returns an `isPointer` result with the field's array metadata,
  then `indexInto` handles the index

Pointer types are tracked both for local assignments (`ptr := &myStruct`)
and for typed pointer parameters (`func f(p *Point)`, `func f(a *[3]byte)`).
The analyzer parses `*ast.StarExpr` in function signatures to extract
pointer-to-struct and pointer-to-array type information, which the lowerer
registers in the scope's `ptrType`/`ptrArray` maps during inlining.

`len(ptr)`, `len(*ptr)`, and `cap(ptr)` for array pointers resolve to
the array length at compile time via the `ptrArray` map, matching Go's
semantics where these operations work on both arrays and array pointers.

## Defer

`defer` captures the function call arguments at defer-time
and executes the call at function return. Slice arguments
are captured as a 3-cell header copy (shared backing array).

### Non-recursive Functions

Each `defer` allocates cells for the captured arguments:

```go
func main() {
    x := byte(1)
    defer println(x)    // captures x=1
    x = 2
    defer println(x)    // captures x=2
    // At return: prints 2, then 1 (LIFO)
}
```

Lowered as:

```text
x = 1
defer_0_arg = x        // capture x=1
x = 2
defer_1_arg = x        // capture x=2
// ... at return:
println(defer_1_arg)    // LIFO: last defer first
println(defer_0_arg)
```

Conditional defers use a flag cell:

```go
if cond {
    defer f(x)          // only deferred if cond was true
}
```

Lowered as:

```text
// defer_flag is a fresh cell (0 from BF tape initialization)
if cond {
    defer_arg = x
    defer_flag = 1
}
// ... at return:
if defer_flag { f(defer_arg) }
```

The flag cell is allocated with `allocCells(1)` (a fresh cell, not
recycled). Since non-recursive code uses a single frame pushed once
at program start, the stack slot starts at 0 from BF tape
initialization. Non-matching branches never write to the slot, so
the flag stays 0.

### Recursive Functions

In recursive functions, deferred calls must be per-frame. Arguments are
stored in dedicated frame slots. Before each `return` (and frame pop),
the deferred IR blocks are emitted in LIFO order.

## Control Flow

### If/Else

`if cond { A } else { B }` becomes `IRIf{cond, A, B}`. When the else
branch is nil, `emitIf` (consuming) is used. When non-nil, `emitIfElse`
(preserving) is used so the condition value survives for the else check.

### For Loops

C-style `for init; cond; post { body }` becomes:

```text
[init]
IRLoop{cond, [body; post]}
```

`for range n` desugars to `for i := 0; i < n; i++`.

`for i, v := range a` over arrays uses `len(a)` as the
limit and loads `v = a[i]` at each iteration via
`emitVariableIndexRead`. For slices, `len(s)` is a runtime
value from the header. For struct slices, `v` is allocated
as a struct and loaded via `ptrLoad` (`elemSize` cells per
iteration).

### Switch

Switch statements are lowered as chained if-else:

```go
switch x {
case 1: A
case 2, 3: B
default: C
}
```

Becomes:

```text
if x == 1 { A }
else if x == 2 || x == 3 { B }
else { C }
```

`fallthrough` connects to the next case body unconditionally, skipping
its condition check. `switch` without a tag expression (`switch { ... }`)
uses the case expressions directly as conditions.

### Statement Guards

Brainfuck has no `goto` or early exit. When a `return` executes inside
an `if` block, subsequent statements in the function still exist on the
tape and would execute unconditionally. To prevent this, every statement
in a function body is wrapped in a **skip guard**:

```text
returnFlag = 0
guard = !returnFlag         // 1 initially
if guard { <statement 1> }
guard = !returnFlag         // still 1 if no return
if guard { <statement 2> }
...
```

When `return` fires, it sets `returnFlag = 1`. All subsequent guard
checks produce 0, skipping the remaining statements. The same mechanism
handles `break` and `continue` via `loopSkipFlag`.

The guard is computed by `emitSkipGuard`, which produces
`!returnFlag` (or `!(returnFlag + loopSkipFlag)` when inside a loop).
Each guard cell is allocated, used for one `if`, then freed.

The **first statement** in each block skips the guard when
`returnFlag` and `loopSkipFlag` are both active, since no
preceding sibling could have set either flag.

Functions whose body contains no `return` statements skip the
`returnFlag` allocation entirely. This saves one cell and
eliminates all return-flag guards in that function, since
`returnFlag == 0` makes every guard a no-op.

### Break and Continue

Implemented via flag cells. `break` sets a break flag that guards each
subsequent iteration. `continue` sets a continue flag that skips the
rest of the loop body.

## Move Semantics

`IRCopy` (non-destructive) generates two BF loops: move to `dst+temp`,
then restore `src` from `temp`. `IRMove` (destructive) generates one
loop. The lowerer uses `IRMove` instead of `IRCopy` when the source
value is no longer needed:

- **Temporary expression results**: `emitCopyOrMove` checks if the
  source is a temp cell and emits `IRMove` when safe.
- **Return value cells**: after copying function return values to
  their destination, the return cells are never reused.
- **Composite returns**: struct/array locals going out of scope at
  function return use `IRMove` to transfer to `returnDst`.

## Composite Comparisons

Array and struct equality (`==`, `!=`) are lowered as element-wise
comparisons with short-circuit evaluation:

```go
a == b  // where a, b are [3]byte
```

Becomes:

```text
result = 1
if result { result = (a[0] == b[0]) }
if result { result = (a[1] == b[1]) }
if result { result = (a[2] == b[2]) }
```

Each element is only compared if `result` is still 1 (all previous
pairs matched). On first mismatch, `result` becomes 0 and remaining
comparisons are skipped. For `!=`, the final result is inverted
with `IRNot`. Zero-length arrays (`[0]byte`) are always equal
(result stays 1).

## Named Returns

Named return values are allocated as regular variables. A bare `return`
uses them automatically:

```go
func f() (r byte) {
    r = 42
    return          // returns r=42
}
```

## DivMod Fusion

When adjacent statements compute both `a/b` and `a%b` with the same
operands, the lowerer emits a single `IRDivMod` instead of separate
`IRDiv` + `IRMod`. This is detected during statement lowering by
looking ahead at the next assignment.

```go
q := a / b
r := a % b
```

Without fusion, this generates two full `divmod` loops (the BF `divmod`
algorithm computes both quotient and remainder internally but discards
one). With fusion:

```text
// Without fusion:
IRDiv{q, a, b}     // computes a/b (internally does divmod, discards remainder)
IRMod{r, a, b}     // computes a%b (internally does divmod, discards quotient)

// With fusion:
IRDivMod{q, r, a, b}   // single divmod, keeps both results
```

The fusion also handles guarded patterns like:

```go
if b != 0 {
    q := a / b
    r := a % b
}
```

Return statements:

```go
return a / b, a % b     // fused into a single IRDivMod
```

The fusion also applies in recursive function phases
(`recLowerer.lowerStmts`), with proper `noRetFlag` guarding
for non-first statements.
