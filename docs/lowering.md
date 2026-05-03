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

- `byte` (`uint8`) variable: 1 cell
- `[N]byte` array: N contiguous cells
- Struct: contiguous cells (per field size)
- Slice: 3 cells (`ptr`, `len`, `cap`); backing array on the heap
- Pointer: 1 cell (stack slot index)
- `uint16`/`uint32`/`uint64` variable: 2/4/8 contiguous cells
- `[N]uint16`/`uint32`/`uint64` array: `N * width` contiguous cells

Temporaries for intermediate expression results are allocated and freed
as needed. The allocator reuses freed cell IDs.

Local cell allocation is lazy: when `lowerAssign` / `lowerDecl` /
`lowerRange` first encounter a declaration, `declareFromAssign` /
`declareFromDecl` / `declareFromRange` allocate the cells in the
current scope. A `scope` is a `map[string]binding`, where `binding`
is a tagged union (`*byteBinding`, `*intBinding`, `*arrayBinding`,
`*structBinding`, `*sliceBinding`, `*constBinding`,
`*intConstBinding`). `lowerFor` and `lowerRange` push their own scope
so loop variables and consecutive same-name loops don't collide.

### Variable Initialization

Simple assignments (`x = expr`, `x := expr`) and `var x = expr`
declarations share a unified path through `lowerVarInit`, which
handles composite literals, pointer tracking, and composite variable
copies. For composite RHS (struct/array variables), `lowerExpr`
returns a multi-cell result and `emitCopyOrMove` handles the
cell-by-cell transfer. For flat-offset results (e.g., `p := a[i]`
on a struct array), `assignResult` materializes the data by reading
each element from the flat array via `emitVariableIndexRead`.

### Local Declarations

`lowerDecl` dispatches by declaration kind:

- **`const`**: `lowerLocalConsts` evaluates expressions (including
  `iota` and references to earlier consts) and registers values in
  the current scope's `consts` map. These are visible to
  `arrayTypeSizePart` for use as array sizes.
- **`type`**: `lowerLocalTypes` parses struct definitions and
  registers them in `result.Structs`, identical to top-level types.
- **`var`**: falls through to `lowerVarInit`.

`const` declarations bind into the current scope (`constBinding` for
byte, `intConstBinding` for `uintN`), so they respect lexical
shadowing -- an inner `x := byte(1)` overrides an outer
`const x = 5`. `type` declarations register in `l.result.Structs`
since struct types are package-level after analysis.

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
cells via a counted loop and returns the number of elements
copied. Both arguments can be any slice expression (variable,
reslice, array slice). When slices overlap, the copy direction
is chosen at runtime (`dst.ptr >= src.ptr` copies backwards)
to preserve correctness.

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
functions returning struct slices. `declareFromAssign`
detects struct slice range values, `tmp := s[i]`
patterns, and `row := grid[i]` on 2D arrays or struct
arrays to allocate appropriately-sized variables.

## Strings

Strings are represented as `[]byte` slices, so all the slice
machinery (heap allocation, dynamic length, indexing, range,
`append`) is reused. A string literal `"hello"` lowers via
`evalStringLiteral` to a fresh heap-backed slice with the
bytes pre-stored.

- `s := "hello"` -- `declareFromAssign` recognizes the literal
  and registers `s` as a slice; `lowerSliceExpr` dispatches on
  the literal kind and runs `evalStringLiteral`.
- `var s string` -- `declareFromDecl` treats `string` as a
  3-cell slice header so a subsequent `s = "..."` lands in the
  slice-assign path.
- `len(s)`, `s[i]`, `for i, c := range s` -- ordinary slice ops.
- `print(s)` / `println(s)` -- the print path detects a byte-
  slice argument (or a string-shaped exprResult) and runs
  `emitPrintBytes`, which loops over `len` writing each byte
  with `IRPutc`. Multiple args are joined with spaces.
- `s == t` / `s != t` -- `lowerSliceCompare` initializes
  `result = (len(a) == len(b))`, then loops byte-by-byte ANDing
  per-byte equality into `result`. The loop is wrapped in
  `IRIf{Cond: result}` so unequal-length operands skip it.
- `s + t` -- `lowerStringConcat` resolves both operands once
  (literals fold to compile-time lengths, idents/selectors
  return their existing slice header, callers like
  `string(byteExpr)` materialize via `evalByteToString`),
  pre-sizes `cap = sum(lens)`, allocates the destination
  region, and walks operands a second time emitting either
  `appendLiteralBytes` (inline `IRConst`+`ptrStore` per byte)
  or `appendBytesFromSlice` (loop using the resolved
  `sliceInfo`). Resolving once avoids double heap allocation
  when an operand is itself a heap-allocating expression.
  Chained `a + b + c` and parenthesized `a + (b + c)` both
  dispatch here.
- `s += t` -- the existing `+=` desugar (`s = s + t`) and
  the slice-assign path handle this; no new code.
- `s < t` / `s > t` / `s <= t` / `s >= t` -- `lowerSliceLexCompare`
  walks bytes from index 0 over `min(len(a), len(b))`. At the
  first non-equal pair it sets the result via `IRCmp` and
  flips a `done` flag so subsequent iterations short-circuit.
  If all bytes match, the result falls through to a length
  comparison; for `<=`/`>=`, equal lengths set the result to 1.
- `func f(s string) string` -- the analyzer treats `string`
  as a slice param/return with `SliceElemSize=1`, reusing all
  the existing slice param/return plumbing. Single named
  string returns (`func g() (msg string)`) bind `msg` as a
  `sliceBinding` whose ptr/len/cap alias the return cells, so
  bare `return` works.
- `string(bs)` and `[]byte(s)` -- both lower via
  `copyStringSlice`: resolve the source once, copy its `len`
  into the dst cap, push a heap region via `pushHeapRegion`,
  then `appendBytesFromSlice` to copy the bytes. The new
  slice has independent storage so mutations to either side
  don't affect the other.
- `string(byteExpr)` -- `evalByteToString` allocates a fresh
  1-byte heap-backed slice and stores the byte value at the
  pointer cell. Recognized in `declareFromAssign` (`t :=
  string(byte('A'))`), in `lowerSliceExpr`'s CallExpr branch
  (so concat operands materialize), and via `isStringExpr`'s
  CallExpr case so the surrounding `+` chain dispatches
  correctly.
- `s[low:high]` on any string-shaped base --
  `evalSliceExpr` / `lowerSliceFromSliceExpr` accept idents,
  `p.name` selectors, string-const idents (`LONG[i:i+4]`),
  and arbitrary string-shaped expressions like
  `makeS()[0:5]`. Non-Ident bases are routed through
  `resolveStringSlice` to produce a temporary `sliceInfo`,
  then `lowerSliceFromSrcSliceInfo` emits the bounds
  arithmetic.
- `switch s { case "lit": ... }` -- `lowerSwitch` detects a
  string-typed tag and stores it as a slice header so the
  generated `tag == "lit"` chain dispatches to
  `lowerSliceCompare`. Without this the tag would be forced
  into a single byte cell and string literals would error.
- String constants -- `evalStringConstExpr` folds string-
  typed constant expressions at compile time: literals,
  references to other string consts, and `+` chains thereof.
  Both the analyzer's package-level loop and the lowerer's
  local-const loop call it. Using a string const in a
  slice/`len`/concat context goes through `lowerIdent`'s
  string-const branch, which materializes a fresh heap-backed
  slice on demand. The `putchar` guard preserves the original
  "string constant X can only be used with print/println"
  error for byte-only contexts.
- `defer println(s + "!")` -- `lowerDefer` captures string-
  shaped argument expressions (those whose lowered result has
  `lenCell != 0` and byte-slice shape) into a fresh slice
  binding by copying ptr/len/cap, so the eventual deferred
  call sees the value at defer time rather than a stale byte
  cell.
- String fields in structs -- the analyzer records each
  string-typed field in `StructDef.FieldStrings` and reserves
  3 cells (ptr, len, cap) at the field's offset.
  `lowerStructValueTo` initializes the field by resolving the
  RHS via `resolveStringSlice` and copying the three header
  cells. `lowerSelectorExpr` returns an `isPointer` result with
  `lenCell`/`capCell` pointing into the struct so reads, range,
  print, and length all reuse the slice machinery.
  `lowerFieldAssign` mirrors the literal init path for
  `p.name = expr`. The same path covers pointer access
  (`pp.name`, `pp.name = expr`, `pp.name += expr`): the
  pointer-read branch in `lowerSelectorExpr` calls
  `loadStringHeaderViaPtr`, and the pointer-write branch in
  `lowerFieldAssign` calls `storeStringHeaderViaPtr`. Both
  are thin wrappers around the generic
  `loadConsecutiveViaPtr` / `storeConsecutiveViaPtr` helpers
  that walk three consecutive heap slots. Variable-index
  struct-array access (`ps[i].name`) uses the index-based
  twin `loadConsecutiveViaIndex`, which copies a row index
  cell per byte and dispatches through `emitVariableIndexRead`.
  Multi-byte int fields share the same plumbing via
  `loadMultiByteIntViaPtr` (pointer access) and
  `storeConsecutiveViaPtr` (pointer write).
- Struct equality with string fields -- `lowerCompositeCompare`
  walks fields rather than cells when both operands are the
  same struct type. `emitStructCompare` dispatches per field:
  string fields call `emitStringEq` (the inline form of
  `lowerSliceCompare`), nested struct fields recurse, and
  every other field compares cell-by-cell under a result
  guard so an early mismatch short-circuits the rest. Without
  this, `P{name: "x"} == P{name: "x"}` compared ptr cells and
  always returned false; nested cases like
  `Outer{a: Inner{name: "x"}}` would have failed similarly.
- Slice literals of struct literals -- `evalSliceLiteral`
  infers types for typeless inner literals
  (`[]P{{name: "x"}}`) by setting their `Type` field from
  `comp.Type.(*ast.ArrayType).Elt`. The dispatch routes any
  struct-typed element (`elemType != ""`) through
  `resolveStructArg`. Size-1 structs from a slice index are
  handled in `lowerSelectorExpr`'s `IndexExpr` branch: when
  the indexed result is a temp byte from a size-1 struct, the
  only field IS that byte, so we return it directly with
  `temp` propagated.

`isStringExpr` and `stringLiteralValue` are the predicates
that classify "string-producing" expressions. `isStringExpr`
handles: string literals, string-const idents, byte-slice
idents, string-typed struct field selectors (via
`isStringSelector`, which covers both direct local structs
and pointer-to-struct), `BinaryExpr` ADD whose operands are
both string-shaped (so chains compose), `SliceExpr` whose
base is string-shaped, `CallExpr` to `string(...)` /
`[]byte(...)` / a user-defined function returning a byte
slice, and `ParenExpr` (unwrapped recursively).
`resolveStringSlice` falls through to `lowerSliceExpr` for
non-Ident/non-Selector/non-literal string-shaped expressions
(e.g. the BinaryExpr produced by the `+=` desugar), so all
paths converge to a `sliceInfo`.

`[]string`, `[N]string`, `[][]byte`, and `[N][]byte` are all
supported. Each element is a 3-cell `[]byte` slice header.
`sliceElemInfo` and `arrayElementInfo` recognise both
`Ident "string"` and `*ast.ArrayType` (without a length) as
slice-element types and return `elemSlice=true` (a new bool
on both `sliceInfo` and `arrayInfo`). `lowerCompositeLitInto`'s
`elemSlice` branch also infers the element type for typeless
inner literals (`[N][]byte{{'h','i'},...}`) by wrapping each
element in a synthetic `[]byte{...}` and routing it through
`evalSliceLiteral`.

For slices, the existing slice-of-slices machinery handles
`[]string` directly. For arrays, `lowerCompositeLitInto`
gained an `elemSlice` branch that resolves each element via
`resolveStringSlice` and copies the three header cells into
the array slot, and `lowerArrayAssign` handles `a[i] = "x"`
both for constant index (in-place IRCopy) and variable
index (storeConsecutiveViaIndex via a flat array).
`indexInto`'s constant-index path returns a string-shaped
exprResult pointing at the three cells (no copy needed,
since they're addressable). Variable-index reads use
`loadConsecutiveViaIndex` into a fresh `sliceInfo`.

Range over `[]string` / `[N]string` declares the value
binding as a `sliceBinding` (its three header cells need not
be contiguous in the cell pool), and `lowerRange` writes the
loaded ptr/len/cap into the binding's actual cells via the
`valSliceCells` override. The print path's `string(s[i])`
shortcut sees the indexed string-shape via the new
`IndexExpr` branch in `isStringExpr`, so `println(string(s[i]))`
and `println(s[i])` both go through `emitPrintBytes`.

Range over a string or slice **literal** (`for i, c := range
"abc"`, `for _, s := range []string{"a","b"}`) takes the
`lowerSliceExpr` fallback in `lowerRange`: when `lowerExpr`
on the source fails or returns a non-iterable shape, the
materialized `sliceInfo` is wrapped into a synthetic pointer-
shape `exprResult` that mirrors the slice's `elemSize` and
`elemSlice` flag so the iteration logic dispatches correctly.
`declareFromRange` picks up the same case for slice-of-strings
and slice-of-byte-slices composite-literal sources.

## Multi-return tuples with strings

`func f() (string, byte)` and similar shapes thread a
3-cell slice header through one or more return slots. The
analyzer's per-field return-type detection recognises `string`
identifiers and sets `ReturnSizes[i] = 3`, so the call site
allocates the right total cell count. `lowerReturn`'s multi-
return loop tries `lowerExpr` first, falls back to
`lowerSliceExpr` for string/slice literals, and writes 3 cells
when the result has `lenCell != 0`. `lowerMultiReturnAssign`
mirrors the pattern on the LHS: when `ReturnSizes[i] == 3`
the target is taken to be a string variable and its three
header cells receive the return slot's three cells.
`declareFromAssign` defines the LHS as a `sliceBinding` for
the same condition.

## Parallel assignment with strings

`a, b = b, a`, `p.name, q.name = q.name, p.name`, and
`s[i], s[j] = s[j], s[i]` all go through one path. Each RHS
is lowered, and if the result has `lenCell != 0` (string or
slice header), all three cells (ptr, len, cap) are snapshotted
into fresh temps before any LHS write. Otherwise the
single-cell snapshot is taken as before. The LHS dispatch
then routes by node type:

- `*ast.IndexExpr` byte/scalar: `writeInto` (existing path).
- `*ast.SelectorExpr` byte/scalar: `assignFieldFromCell`,
  which mirrors `lowerFieldAssign`'s pointer-base and
  value-base writes for byte fields.
- `*ast.StarExpr` byte: `lowerDerefAssignFromCell`, a thin
  helper around `ptrStore`.
- 3-cell snapshots: `assignStringHeader`, which writes the
  ptr/len/cap triple either directly (value-base struct field,
  string variable), via `storeStringHeaderViaPtr` (pointer-base
  struct field), via three direct `IRCopy`s (constant array
  index), via inline `ptrStore`s (pointer-based slice element),
  or via `storeConsecutiveViaIndex` (variable-index array
  element).

The pre-fix path lowered the LHS via `lowerExpr`, which for
struct-field selectors *reads* the field into a temp; the
"write" then went into that temp and was discarded, so the
swap appeared to do nothing. For string fields the situation
was worse since only the ptr cell was snapshotted in the
first place.

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

## Multi-byte Integers

A multi-byte integer variable occupies N contiguous cells
in little-endian order (low byte at `cell`, high byte at
`cell+N-1`). `uint16` uses 2 cells, `uint32` uses 4,
and `uint64` uses 8. All operations are decomposed into
byte-level IR at lowering time -- no codegen changes needed.

`exprResult.intSize` tracks the integer width (2, 4, or 8).
`lowerBinary` checks `intSize` and dispatches to
`lowerBinaryInt` for all multi-byte operations. uint16,
uint32, and uint64 share the same generalized N-byte code
paths. Mixed types are a compile error; explicit casts
required.

### Arithmetic

- **Add/Sub**: N-byte carry/borrow chain via
  `emitAddInt`/`emitSubInt`
- **Mul**: `emitMulInt` -- schoolbook byte-pair multiply;
  for each `(a[i], b[j])`, adds `a[i]` to `r[i+j]`
  exactly `b[j]` times with single-byte carry
- **Div/Mod**: `emitDivModInt` -- bit-by-bit schoolbook
  long division. A combined 2N-byte register starts as
  `(R=0, Q=a)`. Each of `8*N` iterations shifts the
  register left by one bit; if the discarded high bit
  was set or `R >= b`, then `R -= b` and the new low bit
  of `Q` is set. After the loop, `Q` holds the quotient
  and `R` the remainder. Runtime is independent of input
  values, vs. `O(quotient)` for repeated subtraction.
- **Shift**: `emitShiftInt` (left or right via a flag) --
  splits the count into whole-byte and sub-byte parts via
  divmod by 8, runs the cheap whole-byte shift loop first,
  then a bit-by-bit loop with inter-byte carry propagation
  for the remainder. `uint64 << 56` takes 7 byte-shifts
  instead of 56 bit-shifts.

### Comparison and print

Comparison (`emitCmpGeqInt`/`emitCmpLtInt`) walks bytes
high-to-low, sets a runtime `done` flag on the first
non-equal byte pair, and skips remaining iterations.
The flat sequential structure (vs. an earlier deeply-
nested if-else) keeps the codegen's live-cell pressure
low at uint64 width.

Print uses algebraic digit decomposition
(`emitPrintInt`) to avoid multi-byte arithmetic
entirely. The algorithm:

1. Each input byte is decomposed into 3 decimal digits
   (hundreds, tens, ones) via two single-byte DivMod-by-10
   operations.
2. The contributions of each digit to the output decimal
   positions are computed using precomputed coefficients
   of `256^k`. Since `256 = 2*100 + 5*10 + 6`, byte k's
   ones digit contributes `6*o` to the output ones,
   `5*o` to the output tens, `2*o` to the output hundreds,
   and so on for each power of 256.
3. After processing each byte, carries are normalized
   across accumulator digits via DivMod-by-10. Byte k=0
   skips normalization (its contributions are o/t/h
   directly, each already < 10). Subsequent bytes only
   normalize through `len(decimalDigits(k))+1` -- the
   highest digit byte k can touch -- since higher
   accumulators are still zero. The last byte normalizes
   the full range so the leading digit receives its carry.
4. Leading zeros are suppressed during output.

The output digit count is `numDecDigits(N)`: 5 for
`uint16`, 10 for `uint32`, 20 for `uint64`. Coefficients
for each `256^k` come from `decimalDigits(k)`
(`k = 0..N-1`), computed at compile time and consumed
when accumulating each input byte.

This is much more efficient than repeated subtraction of
powers of 10, which requires up to `value/power` iterations
of multi-byte comparison and subtraction. The algebraic
approach uses only single-byte operations with bounded
iteration counts.

### Type rules

Both operands in a binary expression must have the same
`intSize`. Use `uint16()`, `uint32()`, `uint64()`, or
`byte()` to convert. Shift counts are always byte.

Integer literals > 255 produce `uint16` results;
literals > 65535 produce `uint32`; literals > 2^32 - 1
produce `uint64`. The same magnitude-based promotion
applies to untyped constants via `classifyIntConst`.
Assigning a wider integer to a narrower variable is a
compile error. `byte()`, `uint16()`, and `uint32()` are
the explicit truncation paths.

Truncation guards emit errors for: assigning wider
integers to narrower variables, `putchar` with non-byte,
`return` type mismatch, and wider integers as
array/slice indices.

### Function parameters and returns

Function parameters and returns of `uint16`/`uint32`/`uint64`
occupy N cells. `inlineCall` copies all N cells for
multi-byte params and zero-extends smaller args.
`info.ReturnSizes` tracks per-return-value cell counts
for multi-return functions. `info.Returns` equals the
total cell count across all return values.

Multi-byte integers are not supported in recursive
functions (tail-call or general recursion) -- the
phase dispatch and frame push/pop mechanisms only
handle byte-sized cells. Using uint16/uint32/uint64 in
recursive functions may crash at runtime.

Typed `*uint16`/`*uint32`/`*uint64` pointer parameters
are supported. The analyzer records the pointed-to width
in `ParamInfo.PtrIntSize`, and `inlineCall` registers it
in the param scope's `ptrIntSize` map so deref reads
(`*p`), writes (`*p = v`), increment/decrement (`*p++`),
and compound assignment (`*p += v`) all use the
multi-byte pointer paths.

### Struct fields

Struct fields of `uint16`/`uint32`/`uint64` occupy N cells
at their offset within the struct. `FieldIntSizes` in
`StructDef` tracks which fields are multi-byte.
Field read (`p.val`), write (`p.val = x`), increment
(`p.val++`), and compound assignment (`p.val += x`)
all handle multi-byte fields through both direct and
pointer-based access paths.

### Arrays and slices of multi-byte integers

`[N]uintN` arrays and `[]uintN` slices are supported.
The element width is tracked via `arrayInfo.elemIntSize`
and `sliceInfo.elemIntSize` (set by `arrayElementInfo`
and `sliceElemInfo`), and propagated into `exprResult`
when the array/slice is read.

For arrays, indexing reads (`a[i]`) materialize an
N-byte temp by emitting `IRDynLoad` at sequential
offsets, and writes (`a[i] = v`) emit `IRDynStore` at
those same offsets. Constant-index reads are direct
N-cell views into the underlying flat array.
Composite literals (`[N]uint16{100, 200, 300}`)
zero-extend smaller-typed elements as needed.

For slices, the same pattern applies but uses
`ptrLoad`/`ptrStore` over the slice's backing pointer.
`append`, `make`, range, and slice-literal init all use
the `elemSize`-stride that was already in place for
struct slices; multi-byte int slices are just the
struct-slice path with `elemIntSize > 0` triggering a
materialization step at the read boundary so the result
is typed as a `uintN` rather than a 2/4/8-byte sub-array.

### Nested composites

Three forms of nested multi-byte composites are supported:

- **Nested arrays** `[N][M]uintN`: `arrayInfo` and
  `exprResult` carry an extra `innerElemIntSize` field
  populated by `arrayElementInfo`'s recursive case. When
  `indexInto` resolves the outer index it propagates this
  as the sub-array's `elemIntSize`, so chained `a[i][j]`
  reads/writes hit the multi-byte path at the inner level.
- **Struct fields of multi-byte int arrays**
  (`type S struct { vals [N]uintN }`): tracked via
  `StructDef.FieldArrayElemIntSize`. `arrayFieldInfo`
  computes total cells as `N*elemBytes` so the struct
  allocates the full size; `lowerSelectorExpr` propagates
  the element width onto the field-access result.
- **Range over `[N]Struct{multi-byte fields}`**: the
  array fallback in `lowerRange` uses `rangeBase.elemSize`
  (rather than a hardcoded 1) and reads `elemSize` cells
  per iteration via dynamic loads at sequential offsets.
  The struct value lives in `valCell..valCell+elemSize-1`,
  and field access on it goes through the regular struct-
  field path (which already handles multi-byte fields).

`[][]uintN` (slice-of-slice of multi-byte) and
`[][N]uintN` (slice of multi-byte array) are **not**
supported. The outer's `s[i]` returns a sub-slice or
sub-array view through `indexInto`'s pointer paths,
which hardcode `elemSize=1` on the materialized result.
Tracking the element-of-element width through these
runtime-materialized views would need additional fields
and is deferred until needed. Workaround: use a 2D fixed
array `[N][M]uintN` or build the outer slice via `make`
+ per-index assignment instead of literal init.

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
When `n` is `uint16`/`uint32`/`uint64`, the loop counter
and limit use multi-byte comparison (`emitCmpLtInt`) and
increment (`emitIncInt`).

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
