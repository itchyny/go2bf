package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"math"
	"slices"
	"strconv"
)

// Lowerer converts a Go AST into the IR representation.
type Lowerer struct {
	result    *AnalysisResult
	fset      *token.FileSet
	nodes     []IRNode
	nextCell  Cell
	freeCells []Cell
	scopes    []scope

	// Return context for inlined functions.
	returnDst  []Cell // cells where return values should be written
	returnFlag Cell   // 1 after a return statement
	inFunc     bool   // true when inside an inlined function body

	// Tail-call context.
	tailCallFunc string // name of the function being tail-call optimized
	tailCallFlag Cell   // set to 1 to trigger tail-call loop restart

	// Recursive phase context.
	recFrameSize int   // frame size for the current recursive function (0 if not in phase)
	recAllocErr  error // set when phase temp allocation overflows

	// Loop break/continue context.
	loopSkipFlag  Cell // 1 after break or continue (skip remaining body stmts)
	loopBreakFlag Cell // 1 after break (skip post/condition, exit loop)
	loopDepth     int  // nesting depth of for/range loops

	// Heap allocator for slices.
	heapPtr Cell // cell holding the next free stack slot index

	// Defer context.
	deferredCalls []*IRBlock // deferred call blocks, emitted in LIFO order at return
}

// scope holds variable bindings for the current lexical scope.
type scope map[string]binding

// binding is a tagged-union of what a name in scope can be. byteBinding
// covers ordinary byte vars and pointer cells (which are bytes holding a
// slot index, optionally annotated with the pointee's type metadata).
type binding interface{ isBinding() }

type byteBinding struct {
	cell       Cell
	ptrType    string     // non-empty if this var is a pointer-to-struct
	ptrArray   *arrayInfo // non-nil if this var is a pointer-to-array
	ptrIntSize int        // non-zero if this var is a pointer-to-uintN
}
type intBinding struct {
	base Cell
	size int // 2, 4, or 8
}
type arrayBinding struct{ info arrayInfo }
type structBinding struct{ info structInfo }
type sliceBinding struct{ info sliceInfo }
type constBinding struct{ value byte }
type intConstBinding struct {
	value uint64
	size  int // 2, 4, or 8
}

func (*byteBinding) isBinding()     {}
func (*intBinding) isBinding()      {}
func (*arrayBinding) isBinding()    {}
func (*structBinding) isBinding()   {}
func (*sliceBinding) isBinding()    {}
func (*constBinding) isBinding()    {}
func (*intConstBinding) isBinding() {}

// byteBindingFor returns the *byteBinding for name. If a non-byte binding
// already exists (e.g. struct/array/slice), returns nil so callers skip
// the annotation -- composites can't also be pointers.
// Otherwise, creates a fresh byteBinding if none exists.
func (sc scope) byteBindingFor(name string) *byteBinding {
	if existing, ok := sc[name]; ok {
		b, ok := existing.(*byteBinding)
		if !ok {
			return nil
		}
		return b
	}
	b := &byteBinding{}
	sc[name] = b
	return b
}

// has reports whether name is bound to any kind in this scope.
func (sc scope) has(name string) bool {
	_, ok := sc[name]
	return ok
}

func (sc scope) hasArray(name string) bool {
	_, ok := sc[name].(*arrayBinding)
	return ok
}

func (sc scope) hasStruct(name string) bool {
	_, ok := sc[name].(*structBinding)
	return ok
}

func (sc scope) hasSlice(name string) bool {
	_, ok := sc[name].(*sliceBinding)
	return ok
}

func (sc scope) hasIntCell(name string) bool {
	_, ok := sc[name].(*intBinding)
	return ok
}

func (sc scope) hasVar(name string) bool {
	_, ok := sc[name].(*byteBinding)
	return ok
}

// defineByte binds a fresh byte var (or pointer cell) in this scope.
// If a byteBinding already exists (from a prior annotation), reuse it so
// pointer metadata is preserved.
func (sc scope) defineByte(name string, c Cell) {
	if b := sc.byteBindingFor(name); b != nil {
		b.cell = c
	}
}

// annotatePtrType marks an existing byte var as a pointer-to-struct.
func (sc scope) annotatePtrType(name, typeName string) {
	if b := sc.byteBindingFor(name); b != nil {
		b.ptrType = typeName
	}
}

// annotatePtrArray marks an existing byte var as a pointer-to-array.
func (sc scope) annotatePtrArray(name string, ai arrayInfo) {
	if b := sc.byteBindingFor(name); b != nil {
		b.ptrArray = &ai
	}
}

// annotatePtrIntSize marks an existing byte var as a pointer-to-uintN.
func (sc scope) annotatePtrIntSize(name string, n int) {
	if b := sc.byteBindingFor(name); b != nil {
		b.ptrIntSize = n
	}
}

type arrayInfo struct {
	base             Cell
	size             int    // total cells (count * elemSize)
	count            int    // number of elements
	elemSize         int    // cells per element (1 for byte, >1 for struct)
	elemType         string // struct type name (empty for byte)
	elemIntSize      int    // >1 if elements are multi-byte integers (uint16/uint32/uint64)
	elemSlice        bool   // true if elements are slices ([N]string, [N][]byte)
	innerElemSize    int    // for nested arrays: cells per inner element (0 if flat)
	innerElemIntSize int    // for nested arrays: >1 if inner elements are multi-byte ints
}

type structInfo struct {
	base Cell
	def  *StructDef // field names, offsets, size
}

type sliceInfo struct {
	ptr         Cell   // cell holding stack slot index of first element
	len         Cell   // cell holding current length
	cap         Cell   // cell holding capacity
	elemSize    int    // cells per element (1 for byte)
	elemType    string // struct type name (empty for byte)
	elemSlice   bool   // true if element is a slice ([][]byte)
	elemPtrType string // struct type for pointer elements ([]*Point)
	elemIntSize int    // >1 for slices of multi-byte integers (uint16/uint32/uint64)
}

type exprResult struct {
	cell             Cell
	temp             bool   // if true, the caller should free this cell via freeCell
	size             int    // total number of cells; 0 means 1 (scalar)
	intSize          int    // >1 for multi-byte integers (2, 4, or 8)
	typeName         string // struct type name of this result (empty for non-struct)
	elemSize         int    // element size for indexable results; 0 means not indexable
	elemCount        int    // number of elements for indexable results
	elemType         string // struct type name for composite elements (empty for byte)
	elemIntSize      int    // >1 if this is an indexable array/slice of multi-byte ints
	elemSlice        bool   // true if elements are slices ([][]byte)
	elemPtrType      string // struct type for pointer elements ([]*Point)
	innerElemSize    int    // for nested arrays: cells per inner element (0 if flat)
	innerElemIntSize int    // for nested arrays: >1 if inner elements are multi-byte ints
	isPointer        bool   // cell is a slot index for indirect access (pointer-to-struct/array, or a 3-cell slice header where lenCell/capCell carry the length and capacity)
	ptrIntSize       int    // >1 if this pointer targets a multi-byte integer
	flatBase         Cell   // for flat-offset results: base of the original array
	lenCell          Cell   // runtime length cell (0 if compile-time elemCount)
	capCell          Cell   // runtime capacity cell (0 if not applicable)
}

// cellCount returns the number of cells in this result (1 for scalars).
func (r exprResult) cellCount() int {
	return max(r.size, 1)
}

// Lower converts the analyzed AST to an IR program.
func Lower(result *AnalysisResult) (*Program, error) {
	l := &Lowerer{
		result:   result,
		fset:     result.fset,
		nextCell: numFixed,
	}

	// Reserve slot 0 for heapPtr so that no user variable occupies slot 0.
	// This makes pointer value 0 a reliable nil sentinel.
	l.heapPtr = l.allocCell()
	l.pushScope()

	// Load top-level constants into scope.
	sc := l.currentScope()
	for name, v := range result.ByteConsts {
		sc[name] = &constBinding{value: v}
	}
	for name, v := range result.IntConsts {
		sc[name] = &intConstBinding{value: v, size: result.IntConstSize[name]}
	}

	// Set up return flag only if the body contains return statements.
	info := result.Funcs["main"]
	if hasReturn(info.Body) {
		l.returnFlag = l.allocCell()
		l.emit(&IRZero{Dst: l.returnFlag})
	}
	l.inFunc = true

	if err := l.lowerStmts(info.Body.List); err != nil {
		return nil, err
	}
	l.emitDeferred()

	l.inFunc = false
	if l.returnFlag != 0 {
		l.freeCell(l.returnFlag)
		l.returnFlag = 0
	}
	l.popScope()

	// Initialize heap pointer for slices (after all cells allocated).
	if l.heapPtr != 0 {
		heapStart := byte(slotOf(l.nextCell)) // #nosec G115
		initNodes := []IRNode{
			&IRConst{Dst: l.heapPtr, Value: heapStart},
		}
		l.nodes = append(initNodes, l.nodes...)
	}

	return &Program{
		Main:      &IRBlock{Nodes: l.nodes},
		CellsUsed: l.nextCell,
	}, nil
}

// Cell allocation.

func (l *Lowerer) allocCell() Cell {
	if n := len(l.freeCells); n > 0 {
		c := l.freeCells[n-1]
		l.freeCells = l.freeCells[:n-1]
		return c
	}
	c := l.nextCell
	l.nextCell++
	// Skip highway marker positions (multiples of highwayStride) during phase temp allocation.
	if l.recFrameSize > 0 && c > 0 && c%highwayStride == 0 && c < sentinelFwd {
		c = l.nextCell
		l.nextCell++
	}
	// In recursive phase lowering, cells 27-39 are phase temps. If we run out,
	// report an error - the function has too many local variables for a single phase.
	if l.recFrameSize > 0 && c >= sentinelFwd {
		l.recAllocErr = fmt.Errorf("too many local variables in recursive function")
	}
	return c
}

func (l *Lowerer) allocCells(n int) Cell {
	base := l.nextCell
	l.nextCell += n
	return base
}

func (l *Lowerer) freeCell(c Cell) {
	l.emit(&IRFree{Cell: c})
	l.freeCells = append(l.freeCells, c)
}

func (l *Lowerer) freeCellRange(base Cell, n int) {
	for i := range n {
		l.freeCell(base + i)
	}
}

func (l *Lowerer) emit(node IRNode) {
	l.nodes = append(l.nodes, node)
}

// emitCopyOrMove emits IRMove if the source is a temp (destructive, smaller Brainfuck),
// or IRCopy if the source must be preserved. Frees the temp if applicable.
// For composite results (size > 1), copies/moves all cells.
func (l *Lowerer) emitCopyOrMove(dst Cell, r exprResult) {
	n := r.cellCount()
	if r.cell == dst {
		if r.temp {
			l.freeCellRange(r.cell, n)
		}
		return
	}
	for j := range n {
		if r.temp {
			l.emit(&IRMove{Dst: dst + j, Src: r.cell + j})
		} else {
			l.emit(&IRCopy{Dst: dst + j, Src: r.cell + j})
		}
	}
	if r.temp {
		l.freeCellRange(r.cell, n)
	}
}

// ensureTemp makes sure the expression result is in a temp cell that can be
// consumed by destructive operations. If it's already temp, returns as-is.
// If it's a variable (non-temp), copies it to a new temp cell.
// Handles composite results (size > 1) by copying all cells.
func (l *Lowerer) ensureTemp(r exprResult) exprResult {
	if r.temp {
		return r
	}
	n := r.cellCount()
	if n == 1 {
		t := l.allocCell()
		l.emit(&IRCopy{Dst: t, Src: r.cell})
		return exprResult{cell: t, temp: true}
	}
	base := l.allocCells(n)
	for j := range n {
		l.emit(&IRCopy{Dst: base + j, Src: r.cell + j})
	}
	return exprResult{cell: base, temp: true, size: r.size}
}

// Scope management.

func (l *Lowerer) pushScope() {
	l.scopes = append(l.scopes, make(scope))
}

func (l *Lowerer) popScope() {
	l.scopes = l.scopes[:len(l.scopes)-1]
}

func (l *Lowerer) currentScope() scope {
	return l.scopes[len(l.scopes)-1]
}

func (l *Lowerer) defineVar(name string) Cell {
	c := l.allocCell()
	sc := l.currentScope()
	sc.defineByte(name, c)
	sc[name] = &byteBinding{cell: c}
	return c
}

func (l *Lowerer) lookupConst(name string) (byte, bool) {
	if b, ok := l.lookupBinding(name).(*constBinding); ok {
		return b.value, true
	}
	return 0, false
}

// allByteConsts returns a merged map of top-level and all scope byte constants.
func (l *Lowerer) allByteConsts() map[string]byte {
	m := make(map[string]byte, len(l.result.ByteConsts))
	maps.Copy(m, l.result.ByteConsts)
	for _, sc := range l.scopes {
		for name, b := range sc {
			if c, ok := b.(*constBinding); ok {
				m[name] = c.value
			}
		}
	}
	return m
}

func (l *Lowerer) lookupStringConst(name string) string {
	return l.result.StringConsts[name]
}

// lookupBinding finds the innermost-scope binding for a name, or nil.
func (l *Lowerer) lookupBinding(name string) binding {
	for i := len(l.scopes) - 1; i >= 0; i-- {
		if b, ok := l.scopes[i][name]; ok {
			return b
		}
	}
	return nil
}

func (l *Lowerer) lookupVar(name string) (Cell, error) {
	if name == "_" {
		// Blank identifier: allocate a disposable cell.
		return l.allocCell(), nil
	}
	switch b := l.lookupBinding(name).(type) {
	case *byteBinding:
		return b.cell, nil
	case *arrayBinding:
		return 0, fmt.Errorf("cannot use array %s as byte value", name)
	case *structBinding:
		return 0, fmt.Errorf("cannot use struct %s as byte value", name)
	default:
		return 0, fmt.Errorf("undefined variable: %s", name)
	}
}

func (l *Lowerer) lookupArray(name string) (arrayInfo, bool) {
	if b, ok := l.lookupBinding(name).(*arrayBinding); ok {
		return b.info, true
	}
	return arrayInfo{}, false
}

func (l *Lowerer) lookupStruct(name string) (structInfo, bool) {
	if b, ok := l.lookupBinding(name).(*structBinding); ok {
		return b.info, true
	}
	return structInfo{}, false
}

func (l *Lowerer) lookupPtrType(name string) (*StructDef, bool) {
	if b, ok := l.lookupBinding(name).(*byteBinding); ok && b.ptrType != "" {
		if def, ok := l.result.Structs[b.ptrType]; ok {
			return def, true
		}
	}
	return nil, false
}

func (l *Lowerer) lookupPtrArray(name string) (arrayInfo, bool) {
	if b, ok := l.lookupBinding(name).(*byteBinding); ok && b.ptrArray != nil {
		return *b.ptrArray, true
	}
	return arrayInfo{}, false
}

func (l *Lowerer) lookupSlice(name string) (sliceInfo, bool) {
	if b, ok := l.lookupBinding(name).(*sliceBinding); ok {
		return b.info, true
	}
	return sliceInfo{}, false
}

func (l *Lowerer) defineSlice(sc scope, name string, elemSize int,
	elemType string, elemSlice bool, elemPtrType string, elemIntSize int) sliceInfo {
	si := sliceInfo{
		ptr: l.allocCell(), len: l.allocCell(), cap: l.allocCell(),
		elemSize: elemSize, elemType: elemType, elemSlice: elemSlice,
		elemPtrType: elemPtrType, elemIntSize: elemIntSize,
	}
	sc[name] = &sliceBinding{info: si}
	return si
}

func (l *Lowerer) lookupIntConst(name string) (uint64, int, bool) {
	if b, ok := l.lookupBinding(name).(*intConstBinding); ok {
		return b.value, b.size, true
	}
	return 0, 0, false
}

func (l *Lowerer) lookupPtrIntSize(name string) int {
	if b, ok := l.lookupBinding(name).(*byteBinding); ok {
		return b.ptrIntSize
	}
	return 0
}

func (l *Lowerer) lookupIntCell(name string) (Cell, bool) {
	if b, ok := l.lookupBinding(name).(*intBinding); ok {
		return b.base, true
	}
	return 0, false
}

func (l *Lowerer) lookupIntVarSize(name string) int {
	if b, ok := l.lookupBinding(name).(*intBinding); ok {
		return b.size
	}
	return 2
}

func (l *Lowerer) defineIntVar(sc scope, name string, size int) Cell {
	base := l.allocCells(size)
	sc[name] = &intBinding{base: base, size: size}
	return base
}

// exprInvolvesInt checks if an expression produces a multi-byte integer result.
func (l *Lowerer) exprInvolvesInt(expr ast.Expr, sc scope) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		if _, ok := l.lookupIntCell(e.Name); ok {
			return true
		}
		if _, _, ok := l.lookupIntConst(e.Name); ok {
			return true
		}
	case *ast.CallExpr:
		if fn, ok := e.Fun.(*ast.Ident); ok {
			switch fn.Name {
			case "uint16", "uint32", "uint64":
				return true
			}
			if info, ok := l.result.Funcs[fn.Name]; ok && info.ReturnType.IntSize >= 2 {
				return true
			}
		}
	case *ast.BinaryExpr:
		return l.exprInvolvesInt(e.X, sc) || l.exprInvolvesInt(e.Y, sc)
	case *ast.ParenExpr:
		return l.exprInvolvesInt(e.X, sc)
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return false // &x produces a pointer, not a multi-byte integer
		}
		return l.exprInvolvesInt(e.X, sc)
	case *ast.SelectorExpr:
		typeName := l.resolveExprTypeName(e.X)
		if def, ok := l.result.Structs[typeName]; ok {
			return def.FieldIntSizes[e.Sel.Name] >= 2
		}
	case *ast.StarExpr:
		// *p where p is a pointer to a multi-byte integer
		if id, ok := e.X.(*ast.Ident); ok {
			if _, ok := l.lookupIntCell(id.Name); ok {
				return true
			}
			return l.lookupPtrIntSize(id.Name) >= 2
		}
	case *ast.IndexExpr:
		// a[i] or s[i] where a/s holds multi-byte integer elements.
		if id, ok := e.X.(*ast.Ident); ok {
			if ai, ok := l.lookupArray(id.Name); ok && ai.elemIntSize >= 2 {
				return true
			}
			if si, ok := l.lookupSlice(id.Name); ok && si.elemIntSize >= 2 {
				return true
			}
		}
	}
	return false
}

// exprIntSize returns the multi-byte integer size of an expression (2, 4, or 8),
// or 2 as a default for callers that have already confirmed the result is multi-byte.
func (l *Lowerer) exprIntSize(expr ast.Expr, sc scope) int {
	switch e := expr.(type) {
	case *ast.Ident:
		if _, ok := l.lookupIntCell(e.Name); ok {
			return l.lookupIntVarSize(e.Name)
		}
		if _, n, ok := l.lookupIntConst(e.Name); ok {
			return n
		}
	case *ast.CallExpr:
		if fn, ok := e.Fun.(*ast.Ident); ok {
			if n := intIdentSize(fn.Name); n > 0 {
				return n
			}
			if info, ok := l.result.Funcs[fn.Name]; ok && info.ReturnType.IntSize >= 2 {
				return info.ReturnType.IntSize
			}
		}
	case *ast.BinaryExpr:
		ls := l.exprIntSize(e.X, sc)
		rs := l.exprIntSize(e.Y, sc)
		return max(ls, rs)
	case *ast.ParenExpr:
		return l.exprIntSize(e.X, sc)
	case *ast.UnaryExpr:
		if e.Op != token.AND {
			return l.exprIntSize(e.X, sc)
		}
	case *ast.SelectorExpr:
		typeName := l.resolveExprTypeName(e.X)
		if def, ok := l.result.Structs[typeName]; ok {
			if n := def.FieldIntSizes[e.Sel.Name]; n >= 2 {
				return n
			}
		}
	case *ast.StarExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			if n := l.lookupPtrIntSize(id.Name); n >= 2 {
				return n
			}
		}
	case *ast.IndexExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			if ai, ok := l.lookupArray(id.Name); ok && ai.elemIntSize >= 2 {
				return ai.elemIntSize
			}
			if si, ok := l.lookupSlice(id.Name); ok && si.elemIntSize >= 2 {
				return si.elemIntSize
			}
		}
	}
	return 2
}

// isSliceType returns true if the type expression is a slice ([]T).
func isSliceType(expr ast.Expr) bool {
	at, ok := expr.(*ast.ArrayType)
	return ok && at.Len == nil
}

// sliceElemInfo returns layout info for a slice type:
// elemSize, elemType, isSliceOfSlice, ptrType, elemIntSize.
// ptrType is non-empty for pointer-to-struct elements ([]*Point).
// elemIntSize is set (2/4/8) for slices of multi-byte integers.
func (l *Lowerer) sliceElemInfo(expr ast.Expr) (int, string, bool, string, int) {
	at, ok := expr.(*ast.ArrayType)
	if !ok || at.Len != nil {
		return 1, "", false, "", 0
	}
	if id, ok := at.Elt.(*ast.Ident); ok {
		if def, ok := l.result.Structs[id.Name]; ok {
			return def.Size, id.Name, false, "", 0
		}
		if n := intIdentSize(id.Name); n > 0 {
			return n, "", false, "", n
		}
		// `[]string` is equivalent to `[][]byte`: each element is a
		// 3-cell slice header.
		if id.Name == "string" {
			return 3, "", true, "", 0
		}
	}
	if size := arrayTypeSize(at.Elt); size > 0 {
		return size, "", false, "", 0
	}
	if isSliceType(at.Elt) {
		return 3, "", true, "", 0
	}
	// Pointer-to-struct: []*Point
	if star, ok := at.Elt.(*ast.StarExpr); ok {
		if id, ok := star.X.(*ast.Ident); ok {
			if _, ok := l.result.Structs[id.Name]; ok {
				return 1, "", false, id.Name, 0
			}
		}
	}
	return 1, "", false, "", 0
}

func (l *Lowerer) allocSliceInfo() sliceInfo {
	return sliceInfo{
		ptr: l.allocCell(), len: l.allocCell(), cap: l.allocCell(),
	}
}

func (l *Lowerer) freeSliceInfo(si sliceInfo) {
	l.freeCell(si.ptr)
	l.freeCell(si.len)
	l.freeCell(si.cap)
}

// lowerSliceExpr evaluates a slice-producing expression into a
// temporary sliceInfo. The caller must free the result with freeSliceInfo.
func (l *Lowerer) lowerSliceExpr(expr ast.Expr) (sliceInfo, error) {
	switch e := expr.(type) {
	case *ast.CallExpr:
		if fn, ok := e.Fun.(*ast.Ident); ok {
			if fn.Name == "make" && len(e.Args) >= 2 && isSliceType(e.Args[0]) {
				return l.evalSliceMake(e.Args[0], e.Args[1:])
			}
			if fn.Name == "append" && len(e.Args) >= 2 {
				si, err := l.lowerSliceExpr(e.Args[0])
				if err != nil {
					return sliceInfo{}, err
				}
				if e.Ellipsis.IsValid() && len(e.Args) == 2 {
					if err := l.lowerSliceAppendSpread(si, e.Args[1]); err != nil {
						return sliceInfo{}, err
					}
				} else {
					for _, arg := range e.Args[1:] {
						if err := l.lowerSliceAppend(si, arg); err != nil {
							return sliceInfo{}, err
						}
					}
				}
				return si, nil
			}
			// string(bs) -- copy a byte slice as a string-typed slice.
			if fn.Name == "string" && len(e.Args) == 1 && l.isStringExpr(e.Args[0]) {
				return l.copyStringSlice(e.Args[0])
			}
			// string(byteExpr) -- 1-char string from a byte value.
			if fn.Name == "string" && len(e.Args) == 1 {
				return l.evalByteToString(e.Args[0])
			}
		}
		// []byte(s) -- copy a string into a fresh byte slice.
		if at, ok := e.Fun.(*ast.ArrayType); ok && at.Len == nil && len(e.Args) == 1 {
			if id, ok := at.Elt.(*ast.Ident); ok && id.Name == "byte" && l.isStringExpr(e.Args[0]) {
				return l.copyStringSlice(e.Args[0])
			}
		}
	case *ast.CompositeLit:
		if isSliceType(e.Type) {
			return l.evalSliceLiteral(e)
		}
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			return l.evalStringLiteral(e)
		}
	}
	// Fallback: evaluate via lowerExpr and convert to sliceInfo.
	r, err := l.lowerExpr(expr)
	if err != nil {
		return sliceInfo{}, err
	}
	if r.lenCell == 0 {
		// nil produces a zero slice header.
		if id, ok := expr.(*ast.Ident); ok && id.Name == "nil" {
			tmp := l.allocSliceInfo()
			l.emit(&IRZero{Dst: tmp.ptr})
			l.emit(&IRZero{Dst: tmp.len})
			l.emit(&IRZero{Dst: tmp.cap})
			return tmp, nil
		}
		return sliceInfo{}, fmt.Errorf("unsupported slice expression: %T", expr)
	}
	// If the result already owns its 3 header cells, hand them over as a
	// sliceInfo with no extra IR -- avoids the alloc + IRCopy round-trip
	// that would otherwise leak the original cells.
	if r.temp {
		return sliceInfo{
			ptr: r.cell, len: r.lenCell, cap: r.capCell,
			elemSize: r.elemSize, elemType: r.elemType, elemSlice: r.elemSlice,
			elemPtrType: r.elemPtrType, elemIntSize: r.elemIntSize,
		}, nil
	}
	// Borrowed cells (e.g. a slice ident): copy header values into temps so
	// the returned sliceInfo can outlive the source.
	tmp := l.allocSliceInfo()
	tmp.elemSize = r.elemSize
	tmp.elemType = r.elemType
	tmp.elemSlice = r.elemSlice
	tmp.elemPtrType = r.elemPtrType
	tmp.elemIntSize = r.elemIntSize
	l.emit(&IRCopy{Dst: tmp.ptr, Src: r.cell})
	l.emit(&IRCopy{Dst: tmp.len, Src: r.lenCell})
	l.emit(&IRCopy{Dst: tmp.cap, Src: r.capCell})
	return tmp, nil
}

// lowerSliceAssign evaluates a slice RHS and copies to the destination header.
func (l *Lowerer) lowerSliceAssign(si sliceInfo, rhs ast.Expr) error {
	// Optimize s = append(s, v...): append directly to s.
	if call, ok := rhs.(*ast.CallExpr); ok {
		if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "append" && len(call.Args) >= 2 {
			if srcID, ok := call.Args[0].(*ast.Ident); ok {
				if src, ok := l.lookupSlice(srcID.Name); ok && src.ptr == si.ptr {
					if call.Ellipsis.IsValid() && len(call.Args) == 2 {
						return l.lowerSliceAppendSpread(si, call.Args[1])
					}
					for _, arg := range call.Args[1:] {
						if err := l.lowerSliceAppend(si, arg); err != nil {
							return err
						}
					}
					return nil
				}
			}
		}
	}
	tmp, err := l.lowerSliceExpr(rhs)
	if err != nil {
		return err
	}
	l.emit(&IRMove{Dst: si.ptr, Src: tmp.ptr})
	l.emit(&IRMove{Dst: si.len, Src: tmp.len})
	l.emit(&IRMove{Dst: si.cap, Src: tmp.cap})
	l.freeSliceInfo(tmp)
	return nil
}

func (l *Lowerer) evalSliceMake(typeExpr ast.Expr, args []ast.Expr) (sliceInfo, error) {
	if sliceNestingDepth(typeExpr) > 2 {
		return sliceInfo{}, fmt.Errorf("slice nesting deeper than 2 levels is not supported")
	}
	si := l.allocSliceInfo()
	es, et, esl, ept, eis := l.sliceElemInfo(typeExpr)
	si.elemSize = es
	si.elemType = et
	si.elemSlice = esl
	si.elemPtrType = ept
	si.elemIntSize = eis
	if err := l.lowerSliceMake(si, args); err != nil {
		return sliceInfo{}, err
	}
	return si, nil
}

func (l *Lowerer) lowerSliceMake(si sliceInfo, args []ast.Expr) error {
	lenR, err := l.lowerExpr(args[0])
	if err != nil {
		return err
	}
	var capR exprResult
	if len(args) >= 2 {
		capR, err = l.lowerExpr(args[1])
		if err != nil {
			return err
		}
	} else {
		capR = lenR
	}
	l.emitCopyOrMove(si.len, lenR)
	if len(args) >= 2 {
		l.emitCopyOrMove(si.cap, capR)
	} else {
		l.emit(&IRCopy{Dst: si.cap, Src: si.len})
	}
	// Allocate backing array: ptr = heapPtr; heapPtr += cap * elemSize.
	l.emit(&IRCopy{Dst: si.ptr, Src: l.heapPtr})
	t := l.allocCell()
	l.emit(&IRCopy{Dst: t, Src: si.cap})
	if si.elemSize > 1 {
		es := l.allocCell()
		l.emit(&IRConst{Dst: es, Value: byte(si.elemSize)}) // #nosec G115
		l.emit(&IRMul{Dst: t, Src1: t, Src2: es})
		l.freeCell(es)
	}
	l.emit(&IRAdd{Dst: l.heapPtr, Src1: l.heapPtr, Src2: t})
	l.freeCell(t)
	capArg := args[0]
	if len(args) >= 2 {
		capArg = args[1]
	}
	if constCap, ok := l.constValue(capArg); ok {
		slots := constCap * max(si.elemSize, 1)
		if slots > 0 {
			l.emit(&IRFramePush{Slots: slots})
		}
	} else {
		pushSize := l.allocCell()
		l.emit(&IRCopy{Dst: pushSize, Src: si.cap})
		if si.elemSize > 1 {
			es := l.allocCell()
			l.emit(&IRConst{Dst: es, Value: byte(si.elemSize)}) // #nosec G115
			l.emit(&IRMul{Dst: pushSize, Src1: pushSize, Src2: es})
			l.freeCell(es)
		}
		l.emit(&IRFramePushDyn{Size: pushSize})
		l.freeCell(pushSize)
	}
	return nil
}

// evalByteToString lowers `string(b)` for a byte-valued expression to a
// fresh 1-byte heap-backed slice. Used for both `t := string(byte('A'))`
// declarations and string-shaped operands inside a `+` chain.
func (l *Lowerer) evalByteToString(arg ast.Expr) (sliceInfo, error) {
	r, err := l.lowerExpr(arg)
	if err != nil {
		return sliceInfo{}, err
	}
	si := l.allocSliceInfo()
	si.elemSize = 1
	l.emit(&IRConst{Dst: si.len, Value: 1})
	l.emit(&IRConst{Dst: si.cap, Value: 1})
	l.emit(&IRCopy{Dst: si.ptr, Src: l.heapPtr})
	l.emit(&IRAddI{Dst: l.heapPtr, Value: 1})
	l.emit(&IRFramePush{Slots: 1})
	valCell := l.allocCell()
	l.emitCopyOrMove(valCell, r)
	l.ptrStore(si.ptr, valCell)
	l.freeCell(valCell)
	return si, nil
}

// copyStringSlice copies a string-producing expr into a fresh
// heap-backed byte slice. Used for string(bs) and []byte(s) so the
// new variable has independent storage.
func (l *Lowerer) copyStringSlice(expr ast.Expr) (sliceInfo, error) {
	si := l.allocSliceInfo()
	si.elemSize = 1
	l.emit(&IRZero{Dst: si.len})

	// Literal: cap and bytes are compile-time known; no source to resolve.
	if s, ok := l.stringLiteralValue(expr); ok {
		l.emit(&IRConst{Dst: si.cap, Value: byte(len(s))}) // #nosec G115
		l.pushHeapRegion(si)
		l.appendLiteralBytes(si, s)
		return si, nil
	}

	// Non-literal: resolve once, then copy len/bytes from the same slice
	// header. Resolving twice (once for cap, once for append) would
	// re-materialize heap-allocating operands like a `+` chain.
	src, srcTemp, err := l.resolveStringSlice(expr)
	if err != nil {
		return sliceInfo{}, err
	}
	l.emit(&IRCopy{Dst: si.cap, Src: src.len})
	l.pushHeapRegion(si)
	l.appendBytesFromSlice(si, src)
	if srcTemp {
		l.freeSliceInfo(src)
	}
	return si, nil
}

// pushHeapRegion allocates si.cap stack slots starting at the current
// heap pointer, sets si.ptr to that base, and bumps the heap pointer.
// Common epilogue for evalStringLiteral / copyStringSlice / lowerStringConcat.
func (l *Lowerer) pushHeapRegion(si sliceInfo) {
	l.emit(&IRCopy{Dst: si.ptr, Src: l.heapPtr})
	push := l.allocCell()
	l.emit(&IRCopy{Dst: push, Src: si.cap})
	l.emit(&IRAdd{Dst: l.heapPtr, Src1: l.heapPtr, Src2: push})
	l.emit(&IRFramePushDyn{Size: push})
	l.freeCell(push)
}

// evalStringLiteral lowers a string literal as a []byte slice -- backing
// array on the heap, ptr/len/cap set up like a make([]byte, N).
func (l *Lowerer) evalStringLiteral(lit *ast.BasicLit) (sliceInfo, error) {
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return sliceInfo{}, err
	}
	si := l.allocSliceInfo()
	si.elemSize = 1
	n := len(s)
	l.emit(&IRConst{Dst: si.len, Value: byte(n)}) // #nosec G115
	l.emit(&IRConst{Dst: si.cap, Value: byte(n)}) // #nosec G115
	l.emit(&IRCopy{Dst: si.ptr, Src: l.heapPtr})
	t := l.allocCell()
	l.emit(&IRConst{Dst: t, Value: byte(n)}) // #nosec G115
	l.emit(&IRAdd{Dst: l.heapPtr, Src1: l.heapPtr, Src2: t})
	l.freeCell(t)
	if n > 0 {
		l.emit(&IRFramePush{Slots: n})
	}
	for i, b := range []byte(s) {
		idx := l.allocCell()
		l.emit(&IRCopy{Dst: idx, Src: si.ptr})
		if i > 0 {
			l.emit(&IRAddI{Dst: idx, Value: byte(i)}) // #nosec G115
		}
		val := l.allocCell()
		l.emit(&IRConst{Dst: val, Value: b})
		l.ptrStore(idx, val)
		l.freeCell(val)
		l.freeCell(idx)
	}
	return si, nil
}

func (l *Lowerer) evalSliceLiteral(comp *ast.CompositeLit) (sliceInfo, error) {
	si := l.allocSliceInfo()
	es, et, esl, ept, eis := l.sliceElemInfo(comp.Type)
	si.elemSize = es
	si.elemType = et
	si.elemSlice = esl
	si.elemPtrType = ept
	si.elemIntSize = eis
	n := len(comp.Elts)
	l.emit(&IRConst{Dst: si.len, Value: byte(n)}) // #nosec G115
	l.emit(&IRConst{Dst: si.cap, Value: byte(n)}) // #nosec G115
	l.emit(&IRCopy{Dst: si.ptr, Src: l.heapPtr})
	t := l.allocCell()
	l.emit(&IRConst{Dst: t, Value: byte(n * max(si.elemSize, 1))}) // #nosec G115
	l.emit(&IRAdd{Dst: l.heapPtr, Src1: l.heapPtr, Src2: t})
	l.freeCell(t)
	if n > 0 {
		l.emit(&IRFramePush{Slots: n * max(si.elemSize, 1)})
	}
	// Element type for inferring the type of typeless inner composite
	// literals: []P{{name: "a"}} -> {name: "a"} gets Type = P.
	var elemTypeExpr ast.Expr
	if at, ok := comp.Type.(*ast.ArrayType); ok {
		elemTypeExpr = at.Elt
	}
	for i, elt := range comp.Elts {
		// Infer type for typeless inner composite literals.
		if cl, ok := elt.(*ast.CompositeLit); ok && cl.Type == nil && elemTypeExpr != nil {
			elt = &ast.CompositeLit{Type: elemTypeExpr, Elts: cl.Elts}
		}
		idx := l.allocCell()
		l.emit(&IRCopy{Dst: idx, Src: si.ptr})
		l.emit(&IRAddI{Dst: idx, Value: byte(i * max(es, 1))}) // #nosec G115
		if si.elemIntSize >= 2 {
			r, err := l.lowerExpr(elt)
			if err != nil {
				return sliceInfo{}, err
			}
			if r.intSize > si.elemIntSize {
				if r.temp {
					l.freeCellRange(r.cell, r.cellCount())
				}
				return sliceInfo{}, fmt.Errorf(
					"cannot use uint%d value in []uint%d literal, use explicit conversion",
					r.intSize*8, si.elemIntSize*8)
			}
			srcN := max(r.intSize, 1)
			for j := range si.elemIntSize {
				if j < srcN {
					t := l.allocCell()
					l.emit(&IRCopy{Dst: t, Src: r.cell + j})
					l.ptrStore(idx, t)
					l.freeCell(t)
				} else {
					t := l.allocCell()
					l.emit(&IRZero{Dst: t})
					l.ptrStore(idx, t)
					l.freeCell(t)
				}
				if j < si.elemIntSize-1 {
					l.emit(&IRAddI{Dst: idx, Value: 1})
				}
			}
			if r.temp {
				l.freeCellRange(r.cell, r.cellCount())
			}
		} else if si.elemSlice {
			// Slice-of-slice: each element is itself a slice. Evaluate the inner
			// slice and store its 3-cell header.
			inner, err := l.lowerSliceExpr(elt)
			if err != nil {
				return sliceInfo{}, err
			}
			t := l.allocCell()
			l.emit(&IRCopy{Dst: t, Src: inner.ptr})
			l.ptrStore(idx, t)
			l.emit(&IRAddI{Dst: idx, Value: 1})
			l.emit(&IRCopy{Dst: t, Src: inner.len})
			l.ptrStore(idx, t)
			l.emit(&IRAddI{Dst: idx, Value: 1})
			l.emit(&IRCopy{Dst: t, Src: inner.cap})
			l.ptrStore(idx, t)
			l.freeCell(t)
			l.freeSliceInfo(inner)
		} else if es > 1 || si.elemType != "" {
			// Struct or multi-cell element: resolve struct/array literal.
			base, size, err := l.resolveStructArg(elt)
			if err != nil {
				return sliceInfo{}, err
			}
			for j := range size {
				l.ptrStore(idx, base+j)
				if j < size-1 {
					l.emit(&IRAddI{Dst: idx, Value: 1})
				}
			}
			l.freeCellRange(base, size)
		} else {
			r, err := l.lowerExpr(elt)
			if err != nil {
				return sliceInfo{}, err
			}
			t := l.allocCell()
			l.emitCopyOrMove(t, r)
			l.ptrStore(idx, t)
			l.freeCell(t)
		}
		l.freeCell(idx)
	}
	return si, nil
}

// evalSliceExpr handles s[low:high] or a[low:high].
func (l *Lowerer) evalSliceExpr(se *ast.SliceExpr) (sliceInfo, error) {
	si := l.allocSliceInfo()
	switch x := se.X.(type) {
	case *ast.Ident:
		if src, ok := l.lookupSlice(x.Name); ok {
			si.elemSize = src.elemSize
			si.elemType = src.elemType
			si.elemSlice = src.elemSlice
			si.elemPtrType = src.elemPtrType
			si.elemIntSize = src.elemIntSize
		} else if ai, ok := l.lookupArray(x.Name); ok {
			si.elemSize = max(ai.elemSize, 1)
			si.elemType = ai.elemType
			si.elemIntSize = ai.elemIntSize
		} else if l.lookupStringConst(x.Name) != "" {
			si.elemSize = 1
		}
	case *ast.SelectorExpr:
		// p.name[low:high] -- slicing a string-typed struct field.
		if l.isStringSelector(x) {
			si.elemSize = 1
		}
	default:
		// Any other string-shaped expression base (e.g. f()[0:5]).
		if l.isStringExpr(se.X) {
			si.elemSize = 1
		}
	}
	if err := l.lowerSliceFromSliceExpr(si, se); err != nil {
		l.freeSliceInfo(si)
		return sliceInfo{}, err
	}
	return si, nil
}

func (l *Lowerer) lowerSliceFromSliceExpr(si sliceInfo, se *ast.SliceExpr) error {
	// Slicing a string-typed struct field: p.name[low:high].
	if sel, ok := se.X.(*ast.SelectorExpr); ok {
		src, srcTemp, err := l.resolveStringSlice(sel)
		if err != nil {
			return fmt.Errorf("unsupported slice expression: %v", err)
		}
		if err := l.lowerSliceFromSrcSliceInfo(si, src, se); err != nil {
			if srcTemp {
				l.freeSliceInfo(src)
			}
			return err
		}
		if srcTemp {
			l.freeSliceInfo(src)
		}
		return nil
	}
	// Slicing any other string-shaped expression (e.g. f()[0:5]).
	if _, ok := se.X.(*ast.Ident); !ok && l.isStringExpr(se.X) {
		src, srcTemp, err := l.resolveStringSlice(se.X)
		if err != nil {
			return fmt.Errorf("unsupported slice expression: %v", err)
		}
		if err := l.lowerSliceFromSrcSliceInfo(si, src, se); err != nil {
			if srcTemp {
				l.freeSliceInfo(src)
			}
			return err
		}
		if srcTemp {
			l.freeSliceInfo(src)
		}
		return nil
	}
	id, ok := se.X.(*ast.Ident)
	if !ok {
		return fmt.Errorf("unsupported slice expression")
	}
	// Slicing a string constant: materialize and reslice.
	if l.lookupStringConst(id.Name) != "" {
		src, srcTemp, err := l.resolveStringSlice(id)
		if err != nil {
			return err
		}
		if err := l.lowerSliceFromSrcSliceInfo(si, src, se); err != nil {
			if srcTemp {
				l.freeSliceInfo(src)
			}
			return err
		}
		if srcTemp {
			l.freeSliceInfo(src)
		}
		return nil
	}
	// Slice from array: s = a[low:high]
	if ai, ok := l.lookupArray(id.Name); ok {
		baseSlot := ai.base - numFixed
		var low, high int
		if se.Low != nil {
			v, ok := l.constValue(se.Low)
			if !ok {
				return fmt.Errorf("slice bounds must be constant for arrays")
			}
			low = v
		}
		if se.High != nil {
			v, ok := l.constValue(se.High)
			if !ok {
				return fmt.Errorf("slice bounds must be constant for arrays")
			}
			high = v
		} else {
			high = ai.count
		}
		capVal := ai.count - low
		if se.Max != nil {
			v, ok := l.constValue(se.Max)
			if !ok {
				return fmt.Errorf("slice bounds must be constant for arrays")
			}
			capVal = v - low
		}
		es := max(ai.elemSize, 1)
		l.emit(&IRConst{Dst: si.ptr, Value: byte(baseSlot + low*es)}) // #nosec G115
		l.emit(&IRConst{Dst: si.len, Value: byte(high - low)})        // #nosec G115
		l.emit(&IRConst{Dst: si.cap, Value: byte(capVal)})            // #nosec G115
		return nil
	}
	// Reslice: s = t[low:high]
	src, ok := l.lookupSlice(id.Name)
	if !ok {
		return fmt.Errorf("unsupported slice expression base: %s", id.Name)
	}
	return l.lowerSliceFromSrcSliceInfo(si, src, se)
}

// lowerSliceFromSrcSliceInfo emits the bounds arithmetic for `si = src[low:high:max]`.
// Both operands carry full sliceInfo; src must be a valid live header.
func (l *Lowerer) lowerSliceFromSrcSliceInfo(si, src sliceInfo, se *ast.SliceExpr) error {
	sameSlice := si.ptr == src.ptr
	if se.Low == nil && se.High == nil {
		if !sameSlice {
			l.emit(&IRCopy{Dst: si.ptr, Src: src.ptr})
			l.emit(&IRCopy{Dst: si.len, Src: src.len})
			l.emit(&IRCopy{Dst: si.cap, Src: src.cap})
		}
		return nil
	}
	if se.Low != nil {
		lowR, err := l.lowerExpr(se.Low)
		if err != nil {
			return err
		}
		if se.High != nil {
			highR, err := l.lowerExpr(se.High)
			if err != nil {
				return err
			}
			l.emit(&IRSub{Dst: si.len, Src1: highR.cell, Src2: lowR.cell})
			if highR.temp {
				l.freeCell(highR.cell)
			}
		} else {
			l.emit(&IRSub{Dst: si.len, Src1: src.len, Src2: lowR.cell})
		}
		if se.Max != nil {
			maxR, err := l.lowerExpr(se.Max)
			if err != nil {
				return err
			}
			l.emit(&IRSub{Dst: si.cap, Src1: maxR.cell, Src2: lowR.cell})
			if maxR.temp {
				l.freeCell(maxR.cell)
			}
		} else {
			l.emit(&IRSub{Dst: si.cap, Src1: src.cap, Src2: lowR.cell})
		}
		ptrOff := l.allocCell()
		l.emit(&IRCopy{Dst: ptrOff, Src: lowR.cell})
		if src.elemSize > 1 {
			es := l.allocCell()
			l.emit(&IRConst{Dst: es, Value: byte(src.elemSize)}) // #nosec G115
			l.emit(&IRMul{Dst: ptrOff, Src1: ptrOff, Src2: es})
			l.freeCell(es)
		}
		l.emit(&IRAdd{Dst: si.ptr, Src1: src.ptr, Src2: ptrOff})
		l.freeCell(ptrOff)
		if lowR.temp {
			l.freeCell(lowR.cell)
		}
	} else {
		if !sameSlice {
			l.emit(&IRCopy{Dst: si.ptr, Src: src.ptr})
		}
		if se.Max != nil {
			maxR, err := l.lowerExpr(se.Max)
			if err != nil {
				return err
			}
			l.emitCopyOrMove(si.cap, maxR)
		} else if !sameSlice {
			l.emit(&IRCopy{Dst: si.cap, Src: src.cap})
		}
		if se.High != nil {
			highR, err := l.lowerExpr(se.High)
			if err != nil {
				return err
			}
			l.emitCopyOrMove(si.len, highR)
		} else if !sameSlice {
			l.emit(&IRCopy{Dst: si.len, Src: src.len})
		}
	}
	return nil
}

// lowerSliceAppend handles s = append(s, val).
func (l *Lowerer) lowerSliceAppend(si sliceInfo, valArg ast.Expr) error {
	es := max(si.elemSize, 1)
	// Evaluate the value to append.
	var valBase Cell
	if si.elemSlice {
		// Slice-of-slices: evaluate inner slice header.
		inner, err := l.lowerSliceExpr(valArg)
		if err != nil {
			return err
		}
		valBase = l.allocCells(3)
		l.emit(&IRCopy{Dst: valBase, Src: inner.ptr})
		l.emit(&IRCopy{Dst: valBase + 1, Src: inner.len})
		l.emit(&IRCopy{Dst: valBase + 2, Src: inner.cap})
		l.freeSliceInfo(inner)
	} else if es > 1 {
		// Multi-cell element: resolve struct arg (handles composite literals).
		base, size, err := l.resolveStructArg(valArg)
		if err != nil {
			return err
		}
		if size != es {
			return fmt.Errorf("append element size mismatch: got %d, want %d", size, es)
		}
		// Copy into temp cells to avoid freeing permanent struct cells
		// and to snapshot the values before conditional branches.
		valBase = l.allocCells(es)
		for j := range es {
			l.emit(&IRCopy{Dst: valBase + j, Src: base + j})
		}
	} else {
		val, err := l.lowerExpr(valArg)
		if err != nil {
			return err
		}
		valBase = l.ensureTemp(val).cell
	}

	// emitStoreVal stores elemSize cells from valBase at the given heap address.
	emitStoreVal := func(addr Cell) {
		for j := range es {
			l.ptrStore(addr, valBase+j)
			if j < es-1 {
				l.emit(&IRAddI{Dst: addr, Value: 1})
			}
		}
	}
	// emitAddr computes ptr + len * elemSize into a new temp cell.
	emitAddr := func() Cell {
		addr := l.allocCell()
		if es == 1 {
			l.emit(&IRAdd{Dst: addr, Src1: si.ptr, Src2: si.len})
		} else {
			t := l.allocCell()
			l.emit(&IRConst{Dst: t, Value: byte(es)}) // #nosec G115
			l.emit(&IRMul{Dst: addr, Src1: si.len, Src2: t})
			l.freeCell(t)
			l.emit(&IRAdd{Dst: addr, Src1: si.ptr, Src2: addr})
		}
		return addr
	}

	// Compare len < cap.
	cond := l.allocCell()
	l.emit(&IRCmp{Op: CmpLt, Dst: cond, Src1: si.len, Src2: si.cap})

	// Fast path: has room.
	saved := l.nodes
	l.nodes = nil
	addr := emitAddr()
	emitStoreVal(addr)
	l.freeCell(addr)
	l.emit(&IRAddI{Dst: si.len, Value: 1})
	fastNodes := l.nodes

	// Slow path: reallocate.
	l.nodes = nil
	// Compute newCap = cap * 2 (min 1 if cap == 0). newCap counts elements.
	newCap := l.allocCell()
	capIsZero := l.allocCell()
	zero := l.allocCell()
	l.emit(&IRZero{Dst: zero})
	l.emit(&IRCmp{Op: CmpEq, Dst: capIsZero, Src1: si.cap, Src2: zero})
	l.freeCell(zero)
	savedInner := l.nodes
	l.nodes = nil
	l.emit(&IRConst{Dst: newCap, Value: 1})
	thenNodes := l.nodes
	l.nodes = nil
	l.emit(&IRAdd{Dst: newCap, Src1: si.cap, Src2: si.cap})
	elseNodes := l.nodes
	l.nodes = savedInner
	l.emit(&IRIf{Cond: capIsZero, Then: &IRBlock{Nodes: thenNodes}, Else: &IRBlock{Nodes: elseNodes}})
	l.freeCell(capIsZero)

	// newCapCells = newCap * elemSize (total cells for allocation).
	// capCells = cap * elemSize, lenCells = len * elemSize.
	emitMulES := func(dst, src Cell) {
		if es == 1 {
			l.emit(&IRCopy{Dst: dst, Src: src})
		} else {
			t := l.allocCell()
			l.emit(&IRConst{Dst: t, Value: byte(es)}) // #nosec G115
			l.emit(&IRMul{Dst: dst, Src1: src, Src2: t})
			l.freeCell(t)
		}
	}
	newCapCells := l.allocCell()
	emitMulES(newCapCells, newCap)

	// Check if backing array is at heap top: ptr + cap * elemSize == heapPtr.
	// If so, extend in-place (no copy needed).
	ptrCopy := l.allocCell()
	l.emit(&IRCopy{Dst: ptrCopy, Src: si.ptr})
	capCells := l.allocCell()
	emitMulES(capCells, si.cap)
	endPtr := l.allocCell()
	l.emit(&IRAdd{Dst: endPtr, Src1: ptrCopy, Src2: capCells})
	l.freeCell(ptrCopy)
	l.freeCell(capCells)
	atEnd := l.allocCell()
	l.emit(&IRCmp{Op: CmpEq, Dst: atEnd, Src1: endPtr, Src2: l.heapPtr})
	l.freeCell(endPtr)

	// In-place extend: bump heapPtr, push extra guards, no copy.
	savedExtend := l.nodes
	l.nodes = nil
	oldCapCells := l.allocCell()
	emitMulES(oldCapCells, si.cap)
	extraCells := l.allocCell()
	l.emit(&IRSub{Dst: extraCells, Src1: newCapCells, Src2: oldCapCells})
	l.freeCell(oldCapCells)
	l.emit(&IRAdd{Dst: l.heapPtr, Src1: l.heapPtr, Src2: extraCells})
	pushExt := l.allocCell()
	l.emit(&IRCopy{Dst: pushExt, Src: extraCells})
	l.emit(&IRFramePushDyn{Size: pushExt})
	l.freeCell(pushExt)
	l.freeCell(extraCells)
	l.emit(&IRCopy{Dst: si.cap, Src: newCap})
	extAddr := emitAddr()
	emitStoreVal(extAddr)
	l.freeCell(extAddr)
	l.emit(&IRAddI{Dst: si.len, Value: 1})
	extendNodes := l.nodes

	// Full reallocation: allocate newCapCells, copy lenCells, store new element.
	l.nodes = nil
	newPtr := l.allocCell()
	l.emit(&IRCopy{Dst: newPtr, Src: l.heapPtr})
	t := l.allocCell()
	l.emit(&IRCopy{Dst: t, Src: newCapCells})
	l.emit(&IRAdd{Dst: l.heapPtr, Src1: l.heapPtr, Src2: t})
	l.freeCell(t)
	pushSize := l.allocCell()
	l.emit(&IRCopy{Dst: pushSize, Src: newCapCells})
	l.emit(&IRFramePushDyn{Size: pushSize})
	l.freeCell(pushSize)
	// Copy len * elemSize cells from old to new.
	lenCells := l.allocCell()
	emitMulES(lenCells, si.len)
	counter := l.allocCell()
	l.emit(&IRZero{Dst: counter})
	loopCond := l.allocCell()
	l.emit(&IRCmp{Op: CmpLt, Dst: loopCond, Src1: counter, Src2: lenCells})
	loopSaved := l.nodes
	l.nodes = nil
	srcAddr := l.allocCell()
	l.emit(&IRAdd{Dst: srcAddr, Src1: si.ptr, Src2: counter})
	tmpVal := l.ptrLoad(srcAddr)
	l.freeCell(srcAddr)
	dstAddr := l.allocCell()
	l.emit(&IRAdd{Dst: dstAddr, Src1: newPtr, Src2: counter})
	l.ptrStore(dstAddr, tmpVal)
	l.freeCell(dstAddr)
	l.freeCell(tmpVal)
	l.emit(&IRAddI{Dst: counter, Value: 1})
	l.emit(&IRCmp{Op: CmpLt, Dst: loopCond, Src1: counter, Src2: lenCells})
	loopBody := &IRBlock{Nodes: l.nodes}
	l.nodes = loopSaved
	l.emit(&IRLoop{Cond: loopCond, Body: loopBody})
	l.freeCell(counter)
	l.freeCell(loopCond)
	l.freeCell(lenCells)
	// Store new element at ptr + len * elemSize.
	l.emit(&IRCopy{Dst: si.ptr, Src: newPtr})
	l.freeCell(newPtr)
	storeAddr := emitAddr()
	emitStoreVal(storeAddr)
	l.freeCell(storeAddr)
	l.emit(&IRCopy{Dst: si.cap, Src: newCap})
	l.freeCell(newCap)
	l.freeCell(newCapCells)
	l.emit(&IRAddI{Dst: si.len, Value: 1})
	reallocNodes := l.nodes

	l.nodes = savedExtend
	l.emit(&IRIf{Cond: atEnd, Then: &IRBlock{Nodes: extendNodes}, Else: &IRBlock{Nodes: reallocNodes}})
	l.freeCell(atEnd)
	slowNodes := l.nodes

	l.nodes = saved
	l.emit(&IRIf{Cond: cond, Then: &IRBlock{Nodes: fastNodes}, Else: &IRBlock{Nodes: slowNodes}})
	for j := range es {
		l.freeCell(valBase + j)
	}
	return nil
}

// lowerSliceAppendSpread handles s = append(s, t...) by ensuring
// capacity and copying elements from t to s.
func (l *Lowerer) lowerSliceAppendSpread(si sliceInfo, srcExpr ast.Expr) error {
	srcID, ok := srcExpr.(*ast.Ident)
	if !ok {
		return fmt.Errorf("append spread requires a slice identifier")
	}
	src, ok := l.lookupSlice(srcID.Name)
	if !ok {
		return fmt.Errorf("append spread requires a slice argument")
	}
	es := max(si.elemSize, 1)
	// Compute needed = len(dst) + len(src). If needed > cap, reallocate.
	needed := l.allocCell()
	l.emit(&IRAdd{Dst: needed, Src1: si.len, Src2: src.len})
	growCond := l.allocCell()
	l.emit(&IRCmp{Op: CmpGt, Dst: growCond, Src1: needed, Src2: si.cap})
	savedGrow := l.nodes
	l.nodes = nil
	// Reallocate: newCap = needed, allocate, copy old, update header.
	newPtr := l.allocCell()
	l.emit(&IRCopy{Dst: newPtr, Src: l.heapPtr})
	pushSize := l.allocCell()
	t := l.allocCell()
	l.emit(&IRConst{Dst: t, Value: byte(es)}) // #nosec G115
	l.emit(&IRMul{Dst: pushSize, Src1: needed, Src2: t})
	l.freeCell(t)
	l.emit(&IRAdd{Dst: l.heapPtr, Src1: l.heapPtr, Src2: pushSize})
	l.emit(&IRFramePushDyn{Size: pushSize})
	l.freeCell(pushSize)
	// Copy old elements: len * es cells.
	oldCells := l.allocCell()
	t = l.allocCell()
	l.emit(&IRConst{Dst: t, Value: byte(es)}) // #nosec G115
	l.emit(&IRMul{Dst: oldCells, Src1: si.len, Src2: t})
	l.freeCell(t)
	counter := l.allocCell()
	l.emit(&IRZero{Dst: counter})
	copyCond := l.allocCell()
	l.emit(&IRCmp{Op: CmpLt, Dst: copyCond, Src1: counter, Src2: oldCells})
	savedCopy := l.nodes
	l.nodes = nil
	sAddr := l.allocCell()
	l.emit(&IRAdd{Dst: sAddr, Src1: si.ptr, Src2: counter})
	v := l.ptrLoad(sAddr)
	l.freeCell(sAddr)
	dAddr := l.allocCell()
	l.emit(&IRAdd{Dst: dAddr, Src1: newPtr, Src2: counter})
	l.ptrStore(dAddr, v)
	l.freeCell(v)
	l.freeCell(dAddr)
	l.emit(&IRAddI{Dst: counter, Value: 1})
	l.emit(&IRCmp{Op: CmpLt, Dst: copyCond, Src1: counter, Src2: oldCells})
	copyBody := &IRBlock{Nodes: l.nodes}
	l.nodes = savedCopy
	l.emit(&IRLoop{Cond: copyCond, Body: copyBody})
	l.freeCell(counter)
	l.freeCell(copyCond)
	l.freeCell(oldCells)
	l.emit(&IRCopy{Dst: si.ptr, Src: newPtr})
	l.emit(&IRCopy{Dst: si.cap, Src: needed})
	l.freeCell(newPtr)
	growNodes := l.nodes
	l.nodes = savedGrow
	l.emit(&IRIf{Cond: growCond, Then: &IRBlock{Nodes: growNodes}})
	l.freeCell(growCond)
	l.freeCell(needed)
	// Copy src elements to dst[len*es..].
	counter = l.allocCell()
	l.emit(&IRZero{Dst: counter})
	srcCells := l.allocCell()
	t = l.allocCell()
	l.emit(&IRConst{Dst: t, Value: byte(es)}) // #nosec G115
	l.emit(&IRMul{Dst: srcCells, Src1: src.len, Src2: t})
	l.freeCell(t)
	cond := l.allocCell()
	l.emit(&IRCmp{Op: CmpLt, Dst: cond, Src1: counter, Src2: srcCells})
	saved := l.nodes
	l.nodes = nil
	sAddr = l.allocCell()
	l.emit(&IRAdd{Dst: sAddr, Src1: src.ptr, Src2: counter})
	v = l.ptrLoad(sAddr)
	l.freeCell(sAddr)
	// dst offset = len * es + counter
	dstOff := l.allocCell()
	t = l.allocCell()
	l.emit(&IRConst{Dst: t, Value: byte(es)}) // #nosec G115
	l.emit(&IRMul{Dst: dstOff, Src1: si.len, Src2: t})
	l.freeCell(t)
	l.emit(&IRAdd{Dst: dstOff, Src1: dstOff, Src2: counter})
	dAddr = l.allocCell()
	l.emit(&IRAdd{Dst: dAddr, Src1: si.ptr, Src2: dstOff})
	l.freeCell(dstOff)
	l.ptrStore(dAddr, v)
	l.freeCell(v)
	l.freeCell(dAddr)
	l.emit(&IRAddI{Dst: counter, Value: 1})
	l.emit(&IRCmp{Op: CmpLt, Dst: cond, Src1: counter, Src2: srcCells})
	body := &IRBlock{Nodes: l.nodes}
	l.nodes = saved
	l.emit(&IRLoop{Cond: cond, Body: body})
	l.freeCell(counter)
	l.freeCell(srcCells)
	l.freeCell(cond)
	// Update len.
	l.emit(&IRAdd{Dst: si.len, Src1: si.len, Src2: src.len})
	return nil
}

// structDef returns the StructDef for a named struct type, or nil.
func (l *Lowerer) structDef(expr ast.Expr) *StructDef {
	id, ok := expr.(*ast.Ident)
	if !ok {
		return nil
	}
	if def, ok := l.result.Structs[id.Name]; ok {
		return def
	}
	return nil
}

// declareFromAssign allocates cells for variables introduced by `:=`.
func (l *Lowerer) declareFromAssign(s *ast.AssignStmt) {
	if s.Tok != token.DEFINE {
		return
	}
	sc := l.currentScope()

	// Multi-return: x, y := f() where f returns multiple values.
	if len(s.Rhs) == 1 && len(s.Lhs) > 1 {
		if call, ok := s.Rhs[0].(*ast.CallExpr); ok {
			if fn, ok := call.Fun.(*ast.Ident); ok {
				if info, ok := l.result.Funcs[fn.Name]; ok && len(info.ReturnSizes) == len(s.Lhs) {
					for i, lhs := range s.Lhs {
						lid, ok := lhs.(*ast.Ident)
						if !ok || lid.Name == "_" {
							continue
						}
						if i < len(info.ReturnSizes) && info.ReturnSizes[i] == 3 {
							if !sc.hasSlice(lid.Name) {
								l.defineSlice(sc, lid.Name, 1, "", false, "", 0)
							}
						} else if i < len(info.ReturnSizes) && info.ReturnSizes[i] >= 2 {
							if !sc.hasIntCell(lid.Name) {
								l.defineIntVar(sc, lid.Name, info.ReturnSizes[i])
							}
						} else if !sc.hasVar(lid.Name) {
							sc.defineByte(lid.Name, l.allocCell())
						}
					}
					return
				}
			}
		}
	}
	for i, lhs := range s.Lhs {
		id, ok := lhs.(*ast.Ident)
		if !ok || id.Name == "_" {
			continue
		}
		// Skip if already bound (e.g. parameters in the inlined-call scope).
		if sc.has(id.Name) {
			continue
		}
		// s := "hello" -- string literal lowers to []byte slice.
		if i < len(s.Rhs) {
			if lit, ok := s.Rhs[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				l.defineSlice(sc, id.Name, 1, "", false, "", 0)
				continue
			}
		}
		// c := a + b where both are string-like -- string concat.
		if i < len(s.Rhs) {
			if bin, ok := s.Rhs[i].(*ast.BinaryExpr); ok && bin.Op == token.ADD {
				if l.isStringExpr(bin.X) && l.isStringExpr(bin.Y) {
					l.defineSlice(sc, id.Name, 1, "", false, "", 0)
					continue
				}
			}
		}
		// t := string(bs) or b := []byte(s) -- string/byte-slice cast.
		// t := string(byteExpr) -- 1-char string from a byte value.
		if i < len(s.Rhs) {
			if call, ok := s.Rhs[i].(*ast.CallExpr); ok && len(call.Args) == 1 {
				stringCast := false
				byteToString := false
				if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "string" {
					stringCast = true
					if !l.isStringExpr(call.Args[0]) {
						byteToString = true
					}
				} else if at, ok := call.Fun.(*ast.ArrayType); ok && at.Len == nil {
					if eid, ok := at.Elt.(*ast.Ident); ok && eid.Name == "byte" {
						stringCast = true
					}
				}
				if (stringCast && l.isStringExpr(call.Args[0])) || byteToString {
					l.defineSlice(sc, id.Name, 1, "", false, "", 0)
					continue
				}
			}
		}
		// Check for composite literal: a := [N]byte{...} or p := Point{...}
		if i < len(s.Rhs) {
			if comp, ok := s.Rhs[i].(*ast.CompositeLit); ok {
				if count, elemSize, elemType, eis, esl, ies, ieis := l.arrayElementInfo(comp.Type); count > 0 {
					if !sc.hasArray(id.Name) {
						if elemSize > 1 {
							l.defineStructArray(sc, id.Name, count, elemSize, elemType, eis, esl, ies, ieis)
						} else {
							l.defineArray(sc, id.Name, count)
						}
					}
					continue
				}
				if isSliceType(comp.Type) {
					if !sc.hasSlice(id.Name) {
						es, et, esl, ept, eis := l.sliceElemInfo(comp.Type)
						l.defineSlice(sc, id.Name, es, et, esl, ept, eis)
					}
					continue
				}
				if def := l.structDef(comp.Type); def != nil {
					if !sc.hasStruct(id.Name) {
						l.defineStruct(sc, id.Name, def)
					}
					continue
				}
			}
		}
		// s := make([]byte, n) or s := append(...)
		if i < len(s.Rhs) {
			if call, ok := s.Rhs[i].(*ast.CallExpr); ok {
				if fn, ok := call.Fun.(*ast.Ident); ok {
					if (fn.Name == "uint16" || fn.Name == "uint32" || fn.Name == "uint64") && len(call.Args) == 1 {
						if !sc.hasIntCell(id.Name) {
							l.defineIntVar(sc, id.Name, intIdentSize(fn.Name))
						}
						continue
					}
					if fn.Name == "make" && len(call.Args) >= 2 && isSliceType(call.Args[0]) {
						if !sc.hasSlice(id.Name) {
							es, et, esl, ept, eis := l.sliceElemInfo(call.Args[0])
							l.defineSlice(sc, id.Name, es, et, esl, ept, eis)
						}
						continue
					}
					if fn.Name == "append" && len(call.Args) >= 2 {
						if !sc.hasSlice(id.Name) {
							// append(s, ...) where s is a known slice.
							if srcID, ok := call.Args[0].(*ast.Ident); ok {
								if sc.hasSlice(srcID.Name) {
									es, et, esl, ept, eis := l.sliceElemInfo(call.Args[0])
									l.defineSlice(sc, id.Name, es, et, esl, ept, eis)
									continue
								}
							}
							// append(make(...), ...) or append([]byte{...}, ...).
							if innerCall, ok := call.Args[0].(*ast.CallExpr); ok {
								if innerFn, ok := innerCall.Fun.(*ast.Ident); ok && innerFn.Name == "make" && len(innerCall.Args) >= 2 && isSliceType(innerCall.Args[0]) {
									es, et, esl, ept, eis := l.sliceElemInfo(innerCall.Args[0])
									l.defineSlice(sc, id.Name, es, et, esl, ept, eis)
									continue
								}
							}
							if comp, ok := call.Args[0].(*ast.CompositeLit); ok && isSliceType(comp.Type) {
								es, et, esl, ept, eis := l.sliceElemInfo(comp.Type)
								l.defineSlice(sc, id.Name, es, et, esl, ept, eis)
								continue
							}
						}
					}
				}
			}
		}
		// s := a[1:3] or s := t[:]
		if i < len(s.Rhs) {
			if se, ok := s.Rhs[i].(*ast.SliceExpr); ok {
				if !sc.hasSlice(id.Name) {
					es, et, esl, ept, eis := 1, "", false, "", 0
					if srcID, ok := se.X.(*ast.Ident); ok {
						if src, ok := l.lookupSlice(srcID.Name); ok {
							es, et, esl, ept, eis = src.elemSize, src.elemType, src.elemSlice, src.elemPtrType, src.elemIntSize
						} else if ai, ok := l.lookupArray(srcID.Name); ok {
							es, et, eis = max(ai.elemSize, 1), ai.elemType, ai.elemIntSize
						}
					}
					l.defineSlice(sc, id.Name, es, et, esl, ept, eis)
				}
				continue
			}
		}
		// inner := s[i] where s is [][]byte, []P, [N][M]byte, [N]P, or [N]uintN
		if i < len(s.Rhs) {
			if idxExpr, ok := s.Rhs[i].(*ast.IndexExpr); ok {
				if arrID, ok := idxExpr.X.(*ast.Ident); ok {
					if si, ok := l.lookupSlice(arrID.Name); ok {
						if si.elemSlice {
							if !sc.hasSlice(id.Name) {
								l.defineSlice(sc, id.Name, 1, "", false, "", 0)
							}
							continue
						}
						if si.elemIntSize >= 2 {
							if !sc.hasIntCell(id.Name) {
								l.defineIntVar(sc, id.Name, si.elemIntSize)
							}
							continue
						}
						if si.elemType != "" {
							if !sc.hasStruct(id.Name) {
								def := l.result.Structs[si.elemType]
								l.defineStruct(sc, id.Name, def)
							}
							continue
						}
					}
					if ai, ok := l.lookupArray(arrID.Name); ok && ai.elemSize > 1 {
						if ai.elemIntSize >= 2 {
							// [N]uintN -> uintN
							if !sc.hasIntCell(id.Name) {
								l.defineIntVar(sc, id.Name, ai.elemIntSize)
							}
						} else if ai.elemType != "" {
							// [N]Point -> Point
							if !sc.hasStruct(id.Name) {
								def := l.result.Structs[ai.elemType]
								l.defineStruct(sc, id.Name, def)
							}
						} else {
							// [N][M]byte -> [M]byte
							if !sc.hasArray(id.Name) {
								l.defineArray(sc, id.Name, ai.elemSize)
							}
						}
						continue
					}
				}
			}
		}
		// s := t where t is a slice
		if i < len(s.Rhs) {
			if rhsID, ok := s.Rhs[i].(*ast.Ident); ok {
				if src, ok := l.lookupSlice(rhsID.Name); ok {
					if !sc.hasSlice(id.Name) {
						l.defineSlice(sc, id.Name, src.elemSize, src.elemType, src.elemSlice, src.elemPtrType, src.elemIntSize)
					}
					continue
				}
			}
		}
		// t := p.name where p.name is a string-typed struct field
		// (direct, pointer, or chained access).
		if i < len(s.Rhs) {
			if sel, ok := s.Rhs[i].(*ast.SelectorExpr); ok {
				if l.isStringSelector(sel) {
					if !sc.hasSlice(id.Name) {
						l.defineSlice(sc, id.Name, 1, "", false, "", 0)
					}
					continue
				}
			}
		}
		// s := f() where f returns a slice
		if i < len(s.Rhs) {
			if call, ok := s.Rhs[i].(*ast.CallExpr); ok {
				if fn, ok := call.Fun.(*ast.Ident); ok {
					if info, ok := l.result.Funcs[fn.Name]; ok && info.ReturnType.IsSlice {
						if !sc.hasSlice(id.Name) {
							es := max(info.ReturnType.SliceElemSize, 1)
							l.defineSlice(sc, id.Name, es, info.ReturnType.SliceElemType, info.ReturnType.SliceElemSlice, "", info.ReturnType.SliceElemIntSize)
						}
						continue
					}
				}
			}
		}
		// p := a[i] where a is [N]Struct
		if i < len(s.Rhs) {
			if idx, ok := s.Rhs[i].(*ast.IndexExpr); ok {
				if arrID, ok := idx.X.(*ast.Ident); ok {
					if ai, ok := l.lookupArray(arrID.Name); ok && ai.elemType != "" {
						if def, ok := l.result.Structs[ai.elemType]; ok {
							if !sc.hasStruct(id.Name) {
								l.defineStruct(sc, id.Name, def)
							}
							continue
						}
					}
				}
			}
		}
		if !sc.hasVar(id.Name) {
			if !sc.hasIntCell(id.Name) {
				if n := 0; i < len(s.Rhs) && l.exprInvolvesInt(s.Rhs[i], sc) {
					n = l.exprIntSize(s.Rhs[i], sc)
					l.defineIntVar(sc, id.Name, n)
				} else {
					sc.defineByte(id.Name, l.allocCell())
				}
			}
		}
		// Track &var for pointer type info. Annotations come AFTER allocation
		// so byteBindingFor returns the just-defined binding instead of
		// creating a stub.
		if i < len(s.Rhs) {
			if unary, ok := s.Rhs[i].(*ast.UnaryExpr); ok && unary.Op == token.AND {
				if rhsID, ok := unary.X.(*ast.Ident); ok {
					if _, ok := l.lookupIntCell(rhsID.Name); ok {
						sc.annotatePtrIntSize(id.Name, l.lookupIntVarSize(rhsID.Name))
					}
					if si, ok := l.lookupStruct(rhsID.Name); ok {
						sc.annotatePtrType(id.Name, si.def.Name)
					}
				}
				if sel, ok := unary.X.(*ast.SelectorExpr); ok {
					typeName := l.resolveExprTypeName(sel.X)
					if def, ok := l.result.Structs[typeName]; ok {
						if n := def.FieldIntSizes[sel.Sel.Name]; n >= 2 {
							sc.annotatePtrIntSize(id.Name, n)
						}
					}
				}
			}
		}
	}
}

// declareFromRange allocates the Key and Value bindings of a range stmt.
func (l *Lowerer) declareFromRange(s *ast.RangeStmt) {
	sc := l.currentScope()

	if s.Key != nil {
		if id, ok := s.Key.(*ast.Ident); ok {
			if !sc.hasVar(id.Name) {
				if !sc.hasIntCell(id.Name) {
					if l.exprInvolvesInt(s.X, sc) {
						n := l.exprIntSize(s.X, sc)
						l.defineIntVar(sc, id.Name, n)
					} else {
						sc.defineByte(id.Name, l.allocCell())
					}
				}
			}
		}
	}
	if s.Value != nil {
		if id, ok := s.Value.(*ast.Ident); ok {
			if sc.has(id.Name) {
				return
			}
			// Multi-byte int element: allocate v as an intVar. If the same
			// name was already defined as an intCell at a smaller width,
			// reject -- our flat scope can't hold both. Same name reused
			// for a wider element would silently truncate.
			var n int
			if rangeID, ok := s.X.(*ast.Ident); ok {
				if ai, ok := l.lookupArray(rangeID.Name); ok && ai.elemIntSize >= 2 {
					n = ai.elemIntSize
				} else if si, ok := l.lookupSlice(rangeID.Name); ok && si.elemIntSize >= 2 {
					n = si.elemIntSize
				}
			} else if sel, ok := s.X.(*ast.SelectorExpr); ok {
				// `for _, v := range s.vals` where s is struct, vals is multi-byte int array.
				if structID, ok := sel.X.(*ast.Ident); ok {
					if si, ok := l.lookupStruct(structID.Name); ok {
						if eis := si.def.FieldArrayElemIntSize[sel.Sel.Name]; eis >= 2 {
							n = eis
						}
					}
				}
			}
			if n >= 2 {
				if !sc.hasIntCell(id.Name) {
					if !sc.hasVar(id.Name) {
						l.defineIntVar(sc, id.Name, n)
					}
				}
				return
			}
			if !sc.hasVar(id.Name) {
				// Struct/pointer slice range: allocate appropriately.
				var rangeElemType string
				if rangeID, ok := s.X.(*ast.Ident); ok {
					if si, ok := l.lookupSlice(rangeID.Name); ok {
						if si.elemSlice {
							// `[][]byte` / `[]string`: each element is a 3-cell slice header.
							l.defineSlice(sc, id.Name, 1, "", false, "", 0)
							return
						}
						if si.elemType != "" {
							rangeElemType = si.elemType
						} else if si.elemPtrType != "" {
							sc.defineByte(id.Name, l.allocCell())
							sc.annotatePtrType(id.Name, si.elemPtrType)
							return
						}
					}
					if ai, ok := l.lookupArray(rangeID.Name); ok {
						if ai.elemSlice {
							// `[N]string`: each element is a 3-cell slice header.
							l.defineSlice(sc, id.Name, 1, "", false, "", 0)
							return
						}
						if ai.elemType != "" {
							rangeElemType = ai.elemType
						}
					}
				}
				if rangeElemType == "" {
					if call, ok := s.X.(*ast.CallExpr); ok {
						if fn, ok := call.Fun.(*ast.Ident); ok {
							if info, ok := l.result.Funcs[fn.Name]; ok && info.ReturnType.IsSlice {
								if info.ReturnType.SliceElemSlice {
									l.defineSlice(sc, id.Name, 1, "", false, "", 0)
									return
								}
								rangeElemType = info.ReturnType.SliceElemType
							}
						}
					}
				}
				if rangeElemType == "" {
					if se, ok := s.X.(*ast.SliceExpr); ok {
						if srcID, ok := se.X.(*ast.Ident); ok {
							if si, ok := l.lookupSlice(srcID.Name); ok {
								rangeElemType = si.elemType
							}
						}
					}
				}
				if comp, ok := s.X.(*ast.CompositeLit); ok {
					if at, ok := comp.Type.(*ast.ArrayType); ok && at.Len == nil {
						if eid, ok := at.Elt.(*ast.Ident); ok && eid.Name == "string" {
							l.defineSlice(sc, id.Name, 1, "", false, "", 0)
							return
						}
						if eltAt, ok := at.Elt.(*ast.ArrayType); ok && eltAt.Len == nil {
							l.defineSlice(sc, id.Name, 1, "", false, "", 0)
							return
						}
					}
				}
				if rangeElemType != "" {
					def := l.result.Structs[rangeElemType]
					l.defineStruct(sc, id.Name, def)
					return
				}
				sc.defineByte(id.Name, l.allocCell())
			}
		}
	}
}

// declareFromDecl allocates cells for variables introduced by `var`,
// and registers local consts and types.
func (l *Lowerer) declareFromDecl(s *ast.DeclStmt) {
	sc := l.currentScope()

	gd, ok := s.Decl.(*ast.GenDecl)
	if !ok {
		return
	}
	if gd.Tok == token.CONST {
		// Register local consts so subsequent declarations can reference them.
		// Errors are caught again during lowerDecl.
		_ = l.lowerLocalConsts(gd)
		return
	}
	if gd.Tok == token.TYPE {
		// Register local types so subsequent variable declarations can reference them.
		// Errors are caught again during lowerDecl.
		_ = l.lowerLocalTypes(gd)
		return
	}
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for _, name := range vs.Names {
			if sc.has(name.Name) {
				continue
			}
			{
				if count, elemSize, elemType, eis, esl, ies, ieis := l.arrayElementInfo(vs.Type); count > 0 {
					if elemSize > 1 {
						l.defineStructArray(sc, name.Name, count, elemSize, elemType, eis, esl, ies, ieis)
					} else {
						l.defineArray(sc, name.Name, count)
					}
				} else if n := intTypeSize(vs.Type); n >= 2 {
					if !sc.hasIntCell(name.Name) {
						l.defineIntVar(sc, name.Name, n)
					}
				} else if isSliceType(vs.Type) {
					if !sc.hasSlice(name.Name) {
						es, et, esl, ept, eis := l.sliceElemInfo(vs.Type)
						l.defineSlice(sc, name.Name, es, et, esl, ept, eis)
					}
				} else if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "string" {
					// var s string -- backed by a 3-cell []byte slice header.
					if !sc.hasSlice(name.Name) {
						l.defineSlice(sc, name.Name, 1, "", false, "", 0)
					}
				} else if def := l.structDef(vs.Type); def != nil {
					if !sc.hasStruct(name.Name) {
						l.defineStruct(sc, name.Name, def)
					}
				} else {
					sc.defineByte(name.Name, l.allocCell())
				}
			}
		}
	}
}

// lowerStructLit handles p := Point{x: 1, y: 2}.
func (l *Lowerer) lowerStructLit(name string, comp *ast.CompositeLit, def *StructDef) error {
	si, ok := l.lookupStruct(name)
	if !ok {
		return fmt.Errorf("undefined struct variable: %s", name)
	}
	return l.lowerStructValueTo(si.base, def, comp)
}

// lowerStructValueTo lowers a struct value (literal or variable) into cells
// starting at base.
func (l *Lowerer) lowerStructValueTo(base Cell, def *StructDef, valExpr ast.Expr) error {
	switch v := valExpr.(type) {
	case *ast.CompositeLit:
		for j, elt := range v.Elts {
			var fieldName string
			var off int
			var ve ast.Expr
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				fieldName = kv.Key.(*ast.Ident).Name
				off = def.Offsets[fieldName]
				ve = kv.Value
			} else {
				fieldName = def.Fields[j]
				off = def.Offsets[fieldName]
				ve = elt
			}
			// Nested struct field: recurse.
			if nestedType := def.FieldTypes[fieldName]; nestedType != "" {
				nestedDef := l.result.Structs[nestedType]
				if err := l.lowerStructValueTo(base+off, nestedDef, ve); err != nil {
					return err
				}
				continue
			}
			// String field: build a slice header at base+off.
			if def.FieldStrings[fieldName] {
				si, isTemp, err := l.resolveStringSlice(ve)
				if err != nil {
					return err
				}
				l.emit(&IRCopy{Dst: base + off, Src: si.ptr})
				l.emit(&IRCopy{Dst: base + off + 1, Src: si.len})
				l.emit(&IRCopy{Dst: base + off + 2, Src: si.cap})
				if isTemp {
					l.freeSliceInfo(si)
				}
				continue
			}
			// Array field: lower each element.
			if arrSize := def.FieldArraySizes[fieldName]; arrSize > 0 {
				if comp, ok := ve.(*ast.CompositeLit); ok {
					for k, innerElt := range comp.Elts {
						r, err := l.lowerExpr(innerElt)
						if err != nil {
							return err
						}
						l.emitCopyOrMove(base+off+k, r)
					}
					continue
				}
			}
			r, err := l.lowerExpr(ve)
			if err != nil {
				return err
			}
			l.emitCopyOrMove(base+off, r)
		}
	default:
		r, err := l.lowerExpr(valExpr)
		if err != nil {
			return err
		}
		l.emitCopyOrMove(base, r)
	}
	return nil
}

// lowerCompositeLit handles a := [N]byte{v0, v1, ...}.
func (l *Lowerer) lowerCompositeLit(name string, comp *ast.CompositeLit) error {
	arr, _ := l.lookupArray(name)
	return l.lowerCompositeLitInto(arr, comp)
}

func (l *Lowerer) lowerCompositeLitInto(arr arrayInfo, comp *ast.CompositeLit) error {
	// Zero all cells - needed when re-entering a loop or reassigning.
	for i := range arr.size {
		l.emit(&IRZero{Dst: arr.base + i})
	}
	idx := 0
	for _, elt := range comp.Elts {
		var valExpr ast.Expr
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			// Keyed element: {0: 'a', 2: 'c'}
			key, ok := l.constValue(kv.Key)
			if !ok {
				return fmt.Errorf("array index must be a constant")
			}
			idx = key
			valExpr = kv.Value
		} else {
			valExpr = elt
		}
		if idx >= arr.count {
			return fmt.Errorf("array index %d out of bounds [0:%d]", idx, arr.count)
		}
		if arr.elemType != "" {
			elemDef := l.result.Structs[arr.elemType]
			base := arr.base + idx*arr.elemSize
			if err := l.lowerStructValueTo(base, elemDef, valExpr); err != nil {
				return err
			}
			idx++
			continue
		}
		// Multi-byte int element: write each value (possibly zero-extended) to
		// the elemIntSize-cell slot. The array was zeroed above, so for narrower
		// values only the low bytes need to be set.
		if arr.elemIntSize >= 2 {
			base := arr.base + idx*arr.elemSize
			r, err := l.lowerExpr(valExpr)
			if err != nil {
				return err
			}
			if r.intSize > arr.elemIntSize {
				if r.temp {
					l.freeCellRange(r.cell, r.cellCount())
				}
				return fmt.Errorf(
					"cannot use uint%d value in []uint%d literal, use explicit conversion",
					r.intSize*8, arr.elemIntSize*8)
			}
			l.emitCopyOrMove(base, r)
			idx++
			continue
		}
		// Array of slices ([N]string, [N][]byte): store each element as a
		// 3-cell slice header.
		if arr.elemSlice {
			// Typeless inner composite literal -- e.g. `[N][]byte{{'h','i'},...}`
			// where the element type is implied. Materialize via the slice
			// literal path so the bytes go on the heap.
			if cl, ok := valExpr.(*ast.CompositeLit); ok && cl.Type == nil {
				typed := &ast.CompositeLit{
					Type: &ast.ArrayType{Elt: &ast.Ident{Name: "byte"}},
					Elts: cl.Elts,
				}
				si, err := l.evalSliceLiteral(typed)
				if err != nil {
					return err
				}
				base := arr.base + idx*arr.elemSize
				l.emit(&IRCopy{Dst: base, Src: si.ptr})
				l.emit(&IRCopy{Dst: base + 1, Src: si.len})
				l.emit(&IRCopy{Dst: base + 2, Src: si.cap})
				l.freeSliceInfo(si)
				idx++
				continue
			}
			src, srcTemp, err := l.resolveStringSlice(valExpr)
			if err != nil {
				return err
			}
			base := arr.base + idx*arr.elemSize
			l.emit(&IRCopy{Dst: base, Src: src.ptr})
			l.emit(&IRCopy{Dst: base + 1, Src: src.len})
			l.emit(&IRCopy{Dst: base + 2, Src: src.cap})
			if srcTemp {
				l.freeSliceInfo(src)
			}
			idx++
			continue
		}
		// Array-of-arrays: inner composite literal.
		if arr.elemSize > 1 && arr.elemType == "" {
			comp, ok := valExpr.(*ast.CompositeLit)
			if !ok {
				return fmt.Errorf("array-of-array element must be a literal")
			}
			base := arr.base + idx*arr.elemSize
			for j, innerElt := range comp.Elts {
				r, err := l.lowerExpr(innerElt)
				if err != nil {
					return err
				}
				l.emitCopyOrMove(base+j, r)
			}
			idx++
			continue
		}
		r, err := l.lowerExpr(valExpr)
		if err != nil {
			return err
		}
		l.emitCopyOrMove(arr.base+idx, r)
		idx++
	}
	return nil
}

func (l *Lowerer) defineStruct(sc scope, name string, def *StructDef) {
	base := l.allocCells(def.Size)
	si := structInfo{base: base, def: def}
	sc[name] = &structBinding{info: si}
}

func (l *Lowerer) defineArray(sc scope, name string, size int) {
	base := l.allocCells(size)
	ai := arrayInfo{base: base, size: size, count: size, elemSize: 1}
	sc[name] = &arrayBinding{info: ai}
}

func (l *Lowerer) defineStructArray(sc scope, name string, count, elemSize int,
	elemType string, elemIntSize int, elemSlice bool, innerElemSize, innerElemIntSize int) {
	total := count * elemSize
	base := l.allocCells(total)
	ai := arrayInfo{base: base, size: total, count: count,
		elemSize: elemSize, elemType: elemType, elemIntSize: elemIntSize, elemSlice: elemSlice,
		innerElemSize: innerElemSize, innerElemIntSize: innerElemIntSize}
	sc[name] = &arrayBinding{info: ai}
}

// arrayTypeSize returns N for [N]byte types, 0 for non-array types.
func arrayTypeSize(expr ast.Expr) int {
	at, ok := expr.(*ast.ArrayType)
	if !ok {
		return 0
	}
	n := arrayTypeSizePart(at.Len, nil)
	if n < 0 {
		return 0
	}
	return n
}

func (l *Lowerer) arraySize(expr ast.Expr) int {
	count, elemSize, _, _, _, _, _ := l.arrayElementInfo(expr)
	return count * elemSize
}

// arrayElementInfo returns array layout info. For [N]byte: count=N, elemSize=1.
// For [N]Point: count=N, elemSize=structSize, elemType="Point". For nested
// arrays the inner element size is reported via innerElemSize. For multi-byte
// int elements ([N]uint16/uint32/uint64), elemIntSize is set to the byte width.
// For nested multi-byte int arrays ([N][M]uintN), innerElemIntSize tracks the
// innermost element width so chained indexing can materialize correctly.
// For [N]string, elemSlice is true and elemSize is 3 (per slice header).
// Return-value order matches the field order in arrayInfo.
func (l *Lowerer) arrayElementInfo(expr ast.Expr) (count, elemSize int,
	elemType string, elemIntSize int, elemSlice bool, innerElemSize, innerElemIntSize int) {
	at, ok := expr.(*ast.ArrayType)
	if !ok {
		return 0, 0, "", 0, false, 0, 0
	}
	count = arrayTypeSizePart(at.Len, l.allByteConsts())
	if count < 0 {
		return 0, 0, "", 0, false, 0, 0
	}
	if id, ok := at.Elt.(*ast.Ident); ok {
		if def, ok := l.result.Structs[id.Name]; ok {
			return count, def.Size, id.Name, 0, false, 0, 0
		}
		if n := intIdentSize(id.Name); n > 0 {
			return count, n, "", n, false, 0, 0
		}
		if id.Name == "string" {
			return count, 3, "", 0, true, 0, 0
		}
	}
	if eltAt, ok := at.Elt.(*ast.ArrayType); ok {
		// `[N][]byte` (or any slice-typed element): each element is a
		// 3-cell slice header.
		if eltAt.Len == nil {
			return count, 3, "", 0, true, 0, 0
		}
		innerCount, innerES, innerET, innerEIS, _, _, _ := l.arrayElementInfo(at.Elt)
		if innerCount > 0 {
			return count, innerCount * innerES, innerET, 0, false, innerES, innerEIS
		}
	}
	return count, 1, "", 0, false, 0, 0
}

func sliceNestingDepth(expr ast.Expr) int {
	depth := 0
	for {
		at, ok := expr.(*ast.ArrayType)
		if !ok || at.Len != nil {
			return depth
		}
		depth++
		expr = at.Elt
	}
}

func arrayNestingDepth(expr ast.Expr) int {
	depth := 0
	for {
		at, ok := expr.(*ast.ArrayType)
		if !ok || at.Len == nil {
			return depth
		}
		depth++
		expr = at.Elt
	}
}

// arrayTypeSizePart extracts the length from an array length expression.
// Returns -1 if the expression is not a valid array length.
func arrayTypeSizePart(lenExpr ast.Expr, consts map[string]byte) int {
	if lit, ok := lenExpr.(*ast.BasicLit); ok && lit.Kind == token.INT {
		n, err := strconv.Atoi(lit.Value)
		if err != nil {
			return -1
		}
		return n
	}
	if id, ok := lenExpr.(*ast.Ident); ok && consts != nil {
		if val, ok := consts[id.Name]; ok {
			return int(val)
		}
	}
	return -1
}

// Statement lowering.

func (l *Lowerer) lowerStmts(stmts []ast.Stmt) error {
	for i := 0; i < len(stmts); i++ {
		guarded := i > 0 && (l.returnFlag != 0 || l.loopSkipFlag != 0)
		// Fuse adjacent div/mod assignments: q := x/y; r := x%y -> IRDivMod.
		if i+1 < len(stmts) {
			mark := len(l.nodes)
			if fused, err := l.tryLowerDivModAssign(stmts[i], stmts[i+1]); err != nil {
				return err
			} else if fused {
				if guarded {
					l.wrapNodesInGuard(mark)
				}
				i++
				continue
			}
		}
		if guarded {
			if err := l.lowerStmtGuarded(stmts[i]); err != nil {
				return err
			}
		} else {
			if err := l.lowerStmt(stmts[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// hasReturn reports whether a block contains any return statements.
func hasReturn(block *ast.BlockStmt) bool {
	found := false
	ast.Inspect(block, func(n ast.Node) bool {
		if _, ok := n.(*ast.ReturnStmt); ok {
			found = true
		}
		return !found
	})
	return found
}

// wrapNodesInGuard wraps all IR nodes emitted since mark in a skip guard.
func (l *Lowerer) wrapNodesInGuard(mark int) {
	body := slices.Clone(l.nodes[mark:])
	l.nodes = l.nodes[:mark]
	guard := l.emitSkipGuard()
	l.emit(&IRIf{Cond: guard, Then: &IRBlock{Nodes: body}})
	l.freeCell(guard)
}

func (l *Lowerer) tryLowerDivModAssign(a, b ast.Stmt) (bool, error) {
	return l.tryLowerDivModAssignWith(a, b, l.lowerExpr,
		func(id *ast.Ident, tok token.Token) (Cell, error) {
			if base, ok := l.lookupIntCell(id.Name); ok {
				return base, nil
			}
			return l.lookupOrDefineVar(id, tok)
		},
	)
}

// tryLowerDivModAssignWith detects adjacent div/mod assignments and fuses
// them into a single IRDivMod. The lowerExpr and lookupDst callbacks allow
// both regular and recursive lowerers to share this logic.
func (l *Lowerer) tryLowerDivModAssignWith(
	a, b ast.Stmt,
	lowerExpr func(ast.Expr) (exprResult, error),
	lookupDst func(*ast.Ident, token.Token) (Cell, error),
) (bool, error) {
	aAssign, aOk := a.(*ast.AssignStmt)
	bAssign, bOk := b.(*ast.AssignStmt)
	if !aOk || !bOk || len(aAssign.Lhs) != 1 || len(bAssign.Lhs) != 1 ||
		len(aAssign.Rhs) != 1 || len(bAssign.Rhs) != 1 {
		return false, nil
	}
	// Pre-allocate LHS cells; tryLowerDivModAssign bypasses lowerAssign.
	l.declareFromAssign(aAssign)
	l.declareFromAssign(bAssign)
	aBin, aIsBin := aAssign.Rhs[0].(*ast.BinaryExpr)
	bBin, bIsBin := bAssign.Rhs[0].(*ast.BinaryExpr)
	if !aIsBin || !bIsBin {
		return false, nil
	}
	var divBin, modBin *ast.BinaryExpr
	var divLHS, modLHS ast.Expr
	if aBin.Op == token.QUO && bBin.Op == token.REM {
		divBin, modBin = aBin, bBin
		divLHS, modLHS = aAssign.Lhs[0], bAssign.Lhs[0]
	} else if aBin.Op == token.REM && bBin.Op == token.QUO {
		modBin, divBin = aBin, bBin
		modLHS, divLHS = aAssign.Lhs[0], bAssign.Lhs[0]
	} else {
		return false, nil
	}
	if !sameExpr(divBin.X, modBin.X) || !sameExpr(divBin.Y, modBin.Y) {
		return false, nil
	}
	src1, err := lowerExpr(divBin.X)
	if err != nil {
		return false, err
	}
	src2, err := lowerExpr(divBin.Y)
	if err != nil {
		return false, err
	}
	src2 = l.ensureTemp(src2)
	divID, ok := divLHS.(*ast.Ident)
	if !ok {
		return false, nil
	}
	modID, ok := modLHS.(*ast.Ident)
	if !ok {
		return false, nil
	}
	quotDst, err := lookupDst(divID, aAssign.Tok)
	if err != nil {
		return false, err
	}
	remDst, err := lookupDst(modID, bAssign.Tok)
	if err != nil {
		return false, err
	}
	// Multi-byte integer divmod: compute both quotient and remainder in one pass.
	if src1.intSize >= 2 {
		n := src1.intSize
		l.emitDivModIntFused(quotDst, remDst, src1.cell, src2.cell, n)
		if src1.temp {
			l.freeCellRange(src1.cell, n)
		}
		if src2.temp {
			l.freeCellRange(src2.cell, n)
		}
		return true, nil
	}
	l.emit(&IRDivMod{QuotDst: quotDst, RemDst: remDst, Src1: src1.cell, Src2: src2.cell})
	if src1.temp {
		l.freeCell(src1.cell)
	}
	l.freeCell(src2.cell)
	return true, nil
}

func sameExpr(a, b ast.Expr) bool {
	aID, aOk := a.(*ast.Ident)
	bID, bOk := b.(*ast.Ident)
	if aOk && bOk {
		return aID.Name == bID.Name
	}
	aLit, aOk := a.(*ast.BasicLit)
	bLit, bOk := b.(*ast.BasicLit)
	if aOk && bOk {
		return aLit.Value == bLit.Value
	}
	return false
}

func (l *Lowerer) lookupOrDefineVar(id *ast.Ident, tok token.Token) (Cell, error) {
	if tok == token.DEFINE {
		return l.defineVar(id.Name), nil
	}
	return l.lookupVar(id.Name)
}

// emitSkipGuard allocates a guard cell that is 1 when neither returnFlag
// nor loopSkipFlag is set, and 0 otherwise. Caller must free the cell.
func (l *Lowerer) emitSkipGuard() Cell {
	guard := l.allocCell()
	if l.loopSkipFlag != 0 && l.returnFlag != 0 {
		temp := l.allocCell()
		l.emit(&IRAdd{Dst: temp, Src1: l.returnFlag, Src2: l.loopSkipFlag})
		l.emit(&IRNot{Dst: guard, Src: temp})
		l.freeCell(temp)
	} else if l.loopSkipFlag != 0 {
		l.emit(&IRNot{Dst: guard, Src: l.loopSkipFlag})
	} else {
		l.emit(&IRNot{Dst: guard, Src: l.returnFlag})
	}
	return guard
}

// lowerStmtGuarded wraps a statement in a guard that skips if
// returnFlag or loopSkipFlag is set.
func (l *Lowerer) lowerStmtGuarded(stmt ast.Stmt) error {
	guard := l.emitSkipGuard()

	saved := l.nodes
	l.nodes = nil
	if err := l.lowerStmt(stmt); err != nil {
		return err
	}
	body := &IRBlock{Nodes: l.nodes}
	l.nodes = saved

	l.emit(&IRIf{Cond: guard, Then: body})
	l.freeCell(guard)
	return nil
}

func (l *Lowerer) lowerStmt(stmt ast.Stmt) error {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		return l.lowerExprStmt(s)
	case *ast.DeclStmt:
		return l.lowerDecl(s)
	case *ast.AssignStmt:
		return l.lowerAssign(s)
	case *ast.IncDecStmt:
		return l.lowerIncDec(s)
	case *ast.IfStmt:
		return l.lowerIf(s)
	case *ast.SwitchStmt:
		return l.lowerSwitch(s)
	case *ast.ForStmt:
		return l.lowerFor(s)
	case *ast.RangeStmt:
		return l.lowerRange(s)
	case *ast.BranchStmt:
		return l.lowerBranch(s)
	case *ast.BlockStmt:
		l.pushScope()
		err := l.lowerStmts(s.List)
		l.popScope()
		return err
	case *ast.ReturnStmt:
		return l.lowerReturn(s)
	case *ast.DeferStmt:
		return l.lowerDefer(s)
	default:
		return fmt.Errorf("unsupported statement: %T", stmt)
	}
}

func (l *Lowerer) lowerExprStmt(s *ast.ExprStmt) error {
	call, ok := s.X.(*ast.CallExpr)
	if !ok {
		return fmt.Errorf("unsupported expression statement")
	}
	return l.lowerCallStmt(call)
}

func (l *Lowerer) lowerCallStmt(call *ast.CallExpr) error {
	funcName, receiver := l.resolveCall(call)
	if funcName == "" {
		return fmt.Errorf("unsupported function call")
	}

	if handled, err := l.lowerBuiltinCall(funcName, call.Args, l.lowerExpr); handled {
		return err
	}

	// User-defined function or method call (as statement, discard return values).
	info, ok := l.result.Funcs[funcName]
	if !ok {
		return fmt.Errorf("unsupported function: %s", funcName)
	}
	args := l.prependReceiver(receiver, info, call.Args)
	retCells, err := l.inlineCall(info, args)
	if err != nil {
		return err
	}
	for _, c := range retCells {
		l.freeCell(c)
	}
	return nil
}

// prependReceiver returns args with the method receiver prepended. If the
// method has a pointer receiver and the supplied expression is a value-typed
// struct, the receiver is implicitly wrapped with `&` so the inlined body
// sees a pointer (matching Go's auto-address-of semantics on method calls).
func (l *Lowerer) prependReceiver(receiver ast.Expr, info *FuncInfo, args []ast.Expr) []ast.Expr {
	if receiver == nil {
		return args
	}
	if len(info.ParamTypes) > 0 && info.ParamTypes[0].IsPointer && info.ParamTypes[0].PtrStructType != "" {
		if !l.isPointerReceiver(receiver) {
			receiver = &ast.UnaryExpr{Op: token.AND, X: receiver}
		}
	}
	return append([]ast.Expr{receiver}, args...)
}

// isPointerReceiver reports whether the receiver expression already evaluates
// to a struct pointer (so &-wrapping should be skipped).
func (l *Lowerer) isPointerReceiver(receiver ast.Expr) bool {
	if id, ok := receiver.(*ast.Ident); ok {
		if _, ok := l.lookupPtrType(id.Name); ok {
			return true
		}
	}
	return false
}

// resolveCall returns the function name and optional receiver for a call expression.
// For regular calls f(args), returns ("f", nil).
// For method calls p.method(args), returns ("Point.method", receiverExpr).
func (l *Lowerer) resolveCall(call *ast.CallExpr) (string, ast.Expr) {
	if id, ok := call.Fun.(*ast.Ident); ok {
		return id.Name, nil
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if typeName := l.resolveExprTypeName(sel.X); typeName != "" {
			return typeName + "." + sel.Sel.Name, sel.X
		}
	}
	return "", nil
}

// resolveExprTypeName returns the struct type name of an expression
// without evaluating it, or "" if unknown.
func (l *Lowerer) resolveExprTypeName(expr ast.Expr) string {
	switch x := expr.(type) {
	case *ast.Ident:
		if si, ok := l.lookupStruct(x.Name); ok {
			return si.def.Name
		}
		if ptrDef, ok := l.lookupPtrType(x.Name); ok {
			return ptrDef.Name
		}
	case *ast.IndexExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			if ai, ok := l.lookupArray(id.Name); ok && ai.elemType != "" {
				return ai.elemType
			}
			if ptrAI, ok := l.lookupPtrArray(id.Name); ok && ptrAI.elemType != "" {
				return ptrAI.elemType
			}
			if si, ok := l.lookupSlice(id.Name); ok {
				if si.elemType != "" {
					return si.elemType
				}
				if si.elemPtrType != "" {
					return si.elemPtrType
				}
			}
		}
		// Nested index: a[i][j] -> resolve a[i]'s type.
		return l.resolveExprTypeName(x.X)
	case *ast.CallExpr:
		funcName, _ := l.resolveCall(x)
		if info, ok := l.result.Funcs[funcName]; ok {
			return info.ReturnType.StructType
		}
	case *ast.SelectorExpr:
		if parentType := l.resolveExprTypeName(x.X); parentType != "" {
			if def, ok := l.result.Structs[parentType]; ok {
				return def.FieldTypes[x.Sel.Name]
			}
		}
	}
	return ""
}

// lowerBuiltinCall handles putchar, print, println calls.
// Returns (true, err) if handled, (false, nil) if not a builtin.
// The lowerExpr parameter allows both Lowerer and recLowerer to share this logic.
func (l *Lowerer) lowerBuiltinCall(name string, args []ast.Expr, lowerExpr func(ast.Expr) (exprResult, error)) (bool, error) {
	switch name {
	case "putchar":
		return true, l.lowerPutchar(args, lowerExpr)
	case "print", "println":
		return true, l.lowerPrint(name, args, lowerExpr)
	case "clear":
		return true, l.lowerClear(args)
	case "copy":
		return true, l.lowerCopy(args)
	}
	return false, nil
}

func (l *Lowerer) lowerPutchar(args []ast.Expr, lowerExpr func(ast.Expr) (exprResult, error)) error {
	if len(args) != 1 {
		return fmt.Errorf("putchar expects 1 argument, got %d", len(args))
	}
	if id, ok := args[0].(*ast.Ident); ok {
		if _, ok := l.lookupSlice(id.Name); ok {
			return fmt.Errorf("cannot use slice as argument to putchar")
		}
		if l.lookupStringConst(id.Name) != "" {
			return fmt.Errorf("string constant %s can only be used with print/println", id.Name)
		}
	}
	r, err := lowerExpr(args[0])
	if err != nil {
		return err
	}
	if r.intSize >= 2 {
		if r.temp {
			l.freeCellRange(r.cell, r.intSize)
		}
		return fmt.Errorf("cannot use uint%d as argument to putchar, use byte() to truncate", r.intSize*8)
	}
	if r.size > 0 {
		if r.typeName != "" {
			return fmt.Errorf("cannot use struct %s as byte value", r.typeName)
		}
		if r.elemCount > 0 {
			return fmt.Errorf("cannot use array as byte value")
		}
		return fmt.Errorf("cannot use composite value as byte")
	}
	l.emit(&IRPutc{Src: r.cell})
	if r.temp {
		l.freeCell(r.cell)
	}
	return nil
}

func (l *Lowerer) lowerPrint(name string, args []ast.Expr, lowerExpr func(ast.Expr) (exprResult, error)) error {
	// Expand multi-return function call: println(f()) -> println(r0, r1, ...)
	if len(args) == 1 {
		if call, ok := args[0].(*ast.CallExpr); ok {
			funcName, receiver := l.resolveCall(call)
			if info, ok := l.result.Funcs[funcName]; ok && len(info.ReturnSizes) > 1 {
				callArgs := l.prependReceiver(receiver, info, call.Args)
				retCells, err := l.inlineCall(info, callArgs)
				if err != nil {
					return err
				}
				off := 0
				for i, sz := range info.ReturnSizes {
					if i > 0 && name == "println" {
						t := l.allocCell()
						l.emit(&IRConst{Dst: t, Value: ' '})
						l.emit(&IRPutc{Src: t})
						l.freeCell(t)
					}
					if sz >= 2 {
						l.emitPrintInt(retCells[off], sz)
					} else {
						l.emitPrintByte(retCells[off])
					}
					for j := range sz {
						l.freeCell(retCells[off+j])
					}
					off += sz
				}
				if name == "println" {
					t := l.allocCell()
					l.emit(&IRConst{Dst: t, Value: '\n'})
					l.emit(&IRPutc{Src: t})
					l.freeCell(t)
				}
				return nil
			}
		}
	}
	for i, arg := range args {
		if i > 0 && name == "println" {
			t := l.allocCell()
			l.emit(&IRConst{Dst: t, Value: ' '})
			l.emit(&IRPutc{Src: t})
			l.freeCell(t)
		}
		if s := l.resolveStringArg(arg); s != "" {
			t := l.allocCell()
			for _, b := range []byte(s) {
				l.emit(&IRConst{Dst: t, Value: b})
				l.emit(&IRPutc{Src: t})
			}
			l.freeCell(t)
			continue
		}
		// print(s) where s is a []byte slice (e.g. from a string literal).
		if id, ok := arg.(*ast.Ident); ok {
			if si, ok := l.lookupSlice(id.Name); ok && si.elemSize == 1 &&
				si.elemType == "" && si.elemIntSize == 0 && !si.elemSlice {
				l.emitPrintBytes(si)
				continue
			}
		}
		// string(x) where x is a byte value -- print as raw character.
		// string(bs) where bs is already a string-shaped expression is
		// an identity cast; keep slice semantics so emitPrintBytes runs
		// over the full content below.
		rawChar := false
		if call, ok := arg.(*ast.CallExpr); ok && len(call.Args) == 1 {
			if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "string" {
				if l.isStringExpr(call.Args[0]) {
					arg = call.Args[0]
				} else {
					arg = call.Args[0]
					rawChar = true
				}
			}
		}
		r, err := lowerExpr(arg)
		if err != nil {
			return err
		}
		// String-like slice expression result.
		if !rawChar && r.isPointer && r.elemSize == 1 && r.elemType == "" && r.lenCell != 0 {
			l.emitPrintBytes(sliceInfo{ptr: r.cell, len: r.lenCell, cap: r.capCell, elemSize: 1})
			if r.temp {
				l.freeCell(r.cell)
				l.freeCell(r.lenCell)
				l.freeCell(r.capCell)
			}
			continue
		}
		if rawChar {
			l.emit(&IRPutc{Src: r.cell})
		} else if r.intSize >= 2 {
			l.emitPrintInt(r.cell, r.intSize)
		} else {
			l.emitPrintByte(r.cell)
		}
		if r.temp {
			l.freeCellRange(r.cell, max(r.intSize, 1))
		}
	}
	if name == "println" {
		t := l.allocCell()
		l.emit(&IRConst{Dst: t, Value: '\n'})
		l.emit(&IRPutc{Src: t})
		l.freeCell(t)
	}
	return nil
}

// emitPrintBytes emits IR to write each byte of a []byte slice as a raw
// character (used when printing a string variable).
func (l *Lowerer) emitPrintBytes(si sliceInfo) {
	cnt := l.allocCell()
	l.emit(&IRCopy{Dst: cnt, Src: si.len})
	idx := l.allocCell()
	l.emit(&IRCopy{Dst: idx, Src: si.ptr})
	saved := l.nodes
	l.nodes = nil
	val := l.ptrLoad(idx)
	l.emit(&IRPutc{Src: val})
	l.freeCell(val)
	l.emit(&IRAddI{Dst: idx, Value: 1})
	l.emit(&IRSubI{Dst: cnt, Value: 1})
	body := &IRBlock{Nodes: l.nodes}
	l.nodes = saved
	l.emit(&IRLoop{Cond: cnt, Body: body})
	l.freeCell(idx)
	l.freeCell(cnt)
}

func (l *Lowerer) resolveStringArg(expr ast.Expr) string {
	if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		s, _ := strconv.Unquote(lit.Value)
		return s
	}
	if id, ok := expr.(*ast.Ident); ok {
		return l.lookupStringConst(id.Name)
	}
	return ""
}

// emitPrintByte emits IR to print a byte value as a decimal number (0-255).
// Algorithm: extract digits via two divmod operations, suppress leading zeros.
//  1. divmod(src, 100) -> hundreds digit, remainder
//  2. divmod(remainder, 10) -> tens digit, ones digit
//  3. Print hundreds if nonzero; tens if hundreds was printed or tens nonzero; ones always.
//
// Uses 4 cells: d (divisor/condition), q (quotient), r (remainder), s (flag).
func (l *Lowerer) emitPrintByte(src Cell) {
	d := l.allocCell()
	q := l.allocCell()
	r := l.allocCell()

	// Step 1: split into front digits and ones digit.
	l.emit(&IRConst{Dst: d, Value: 10})
	l.emit(&IRDivMod{QuotDst: q, RemDst: r, Src1: src, Src2: d})
	// q = src / 10 (0-25), r = src % 10 (ones digit)

	// Step 2: if front digits exist, print hundreds and tens.
	// Build the then-block: divmod front by 10, print hundreds if nonzero, print tens.
	h := l.allocCell()
	t := l.allocCell()
	d2 := l.allocCell()
	hCond := l.allocCell()
	l.emit(&IRCopy{Dst: d, Src: q})
	l.emit(&IRIf{Cond: d, Then: &IRBlock{Nodes: []IRNode{
		&IRConst{Dst: d2, Value: 10},
		&IRDivMod{QuotDst: h, RemDst: t, Src1: q, Src2: d2},
		&IRCopy{Dst: hCond, Src: h},
		&IRIf{Cond: hCond, Then: &IRBlock{Nodes: []IRNode{
			&IRAddI{Dst: h, Value: 48},
			&IRPutc{Src: h},
		}}},
		&IRAddI{Dst: t, Value: 48},
		&IRPutc{Src: t},
	}}})
	l.freeCell(h)
	l.freeCell(t)
	l.freeCell(d2)
	l.freeCell(hCond)

	// Step 3: ones digit (always printed).
	l.emit(&IRAddI{Dst: r, Value: 48})
	l.emit(&IRPutc{Src: r})

	l.freeCell(d)
	l.freeCell(q)
	l.freeCell(r)
}

// emitPrintInt prints an n-byte unsigned integer using algebraic digit
// decomposition. Each byte is decomposed into 3 decimal digits (hundreds,
// tens, ones) via DivMod-by-10. The contributions are combined using the
// known decimal coefficients of 256^k, then carries are normalized.
func (l *Lowerer) emitPrintInt(base Cell, n int) {
	nd := numDecDigits(n)
	// Allocate accumulator digits. allocCells bumps the cell counter
	// past nextCell, so it cannot alias base[0..n-1]; bCopy below pulls
	// from the free list which never contains base's cells while the
	// caller still holds the input live.
	acc := l.allocCells(nd)
	for i := range nd {
		l.emit(&IRZero{Dst: acc + i})
	}
	ten := l.allocCell()
	l.emit(&IRConst{Dst: ten, Value: 10})

	for k := range n {
		// Decompose base[k] into o (ones), t (tens), h (hundreds).
		bCopy := l.allocCell()
		l.emit(&IRCopy{Dst: bCopy, Src: base + k})
		o := l.allocCell()
		q := l.allocCell()
		l.emit(&IRDivMod{Src1: bCopy, Src2: ten, QuotDst: q, RemDst: o})
		l.freeCell(bCopy)
		t := l.allocCell()
		h := l.allocCell()
		l.emit(&IRDivMod{Src1: q, Src2: ten, QuotDst: h, RemDst: t})
		l.freeCell(q)

		// Get coefficients for 256^k.
		digits := decimalDigits(k)
		// Add contributions: for digit_type j (0=o, 1=t, 2=h) at digit_value,
		// add coeff * digit_value to acc[d] where coeff = digits[d-j].
		for j, dv := range []Cell{o, t, h} {
			for d := range nd {
				ci := d - j
				if ci < 0 || ci >= len(digits) || digits[ci] == 0 {
					continue
				}
				coeff := digits[ci]
				if coeff == 1 {
					l.emit(&IRAdd{Dst: acc + d, Src1: acc + d, Src2: dv})
				} else {
					c := l.allocCell()
					l.emit(&IRConst{Dst: c, Value: byte(coeff)}) // #nosec G115
					prod := l.allocCell()
					l.emit(&IRMul{Dst: prod, Src1: dv, Src2: c})
					l.freeCell(c)
					l.emit(&IRAdd{Dst: acc + d, Src1: acc + d, Src2: prod})
					l.freeCell(prod)
				}
			}
		}
		l.freeCell(o)
		l.freeCell(t)
		l.freeCell(h)

		// Normalize carries: divmod each touched digit by 10. Byte k's
		// contributions reach at most acc[len(digits)+1]; higher digits
		// are still zero, so normalizing them is wasted. For k=0 the
		// contributions are u/t/h themselves (each already < 10), so
		// normalization is a no-op and skipped entirely. The last byte
		// normalizes through acc[nd-2] so the leading digit receives
		// its final carry.
		if k > 0 {
			limit := len(digits) + 1
			if k == n-1 || limit > nd-1 {
				limit = nd - 1
			}
			for d := 0; d < limit; d++ {
				carry := l.allocCell()
				rem := l.allocCell()
				l.emit(&IRDivMod{Src1: acc + d, Src2: ten, QuotDst: carry, RemDst: rem})
				l.emit(&IRMove{Dst: acc + d, Src: rem})
				l.freeCell(rem)
				l.emit(&IRAdd{Dst: acc + d + 1, Src1: acc + d + 1, Src2: carry})
				l.freeCell(carry)
			}
		}
	}
	l.freeCell(ten)

	// Print digits from most significant to least, suppressing leading zeros.
	started := l.allocCell()
	l.emit(&IRZero{Dst: started})
	for d := nd - 1; d >= 1; d-- {
		dCond := l.allocCell()
		l.emit(&IRCopy{Dst: dCond, Src: acc + d})
		l.emit(&IRIf{Cond: dCond, Then: &IRBlock{Nodes: []IRNode{
			&IRConst{Dst: started, Value: 1},
		}}})
		l.freeCell(dCond)
		sCond := l.allocCell()
		l.emit(&IRCopy{Dst: sCond, Src: started})
		l.emit(&IRIf{Cond: sCond, Then: &IRBlock{Nodes: []IRNode{
			&IRAddI{Dst: acc + d, Value: '0'},
			&IRPutc{Src: acc + d},
		}}})
		l.freeCell(sCond)
	}
	// Ones digit: always print.
	l.emit(&IRAddI{Dst: acc, Value: '0'})
	l.emit(&IRPutc{Src: acc})
	l.freeCell(started)
	l.freeCellRange(acc, nd)
}

// numDecDigits returns the number of decimal digits needed for an n-byte unsigned integer.
func numDecDigits(n int) int {
	switch n {
	case 1:
		return 3
	case 2:
		return 5
	case 4:
		return 10
	case 8:
		return 20
	}
	return 3 * n
}

// decimalDigits returns the decimal digits of 256^k, least significant first.
func decimalDigits(k int) []int {
	v := 1
	for range k {
		v *= 256
	}
	var digits []int
	for v > 0 {
		digits = append(digits, v%10)
		v /= 10
	}
	if len(digits) == 0 {
		digits = []int{0}
	}
	return digits
}

func (l *Lowerer) lowerClear(args []ast.Expr) error {
	if len(args) != 1 {
		return fmt.Errorf("clear expects 1 argument")
	}
	r, err := l.lowerExpr(args[0])
	if err != nil || r.lenCell == 0 {
		return fmt.Errorf("clear expects a slice argument")
	}
	es := max(r.elemSize, 1)
	counter := l.allocCell()
	l.emit(&IRZero{Dst: counter})
	limit := l.allocCell()
	t := l.allocCell()
	l.emit(&IRConst{Dst: t, Value: byte(es)}) // #nosec G115
	l.emit(&IRMul{Dst: limit, Src1: r.lenCell, Src2: t})
	l.freeCell(t)
	cond := l.allocCell()
	l.emit(&IRCmp{Op: CmpLt, Dst: cond, Src1: counter, Src2: limit})
	saved := l.nodes
	l.nodes = nil
	addr := l.allocCell()
	l.emit(&IRAdd{Dst: addr, Src1: r.cell, Src2: counter})
	zero := l.allocCell()
	l.emit(&IRZero{Dst: zero})
	l.ptrStore(addr, zero)
	l.freeCell(zero)
	l.freeCell(addr)
	l.emit(&IRAddI{Dst: counter, Value: 1})
	l.emit(&IRCmp{Op: CmpLt, Dst: cond, Src1: counter, Src2: limit})
	body := &IRBlock{Nodes: l.nodes}
	l.nodes = saved
	l.emit(&IRLoop{Cond: cond, Body: body})
	l.freeCell(counter)
	l.freeCell(limit)
	l.freeCell(cond)
	return nil
}

// emitCopyLoop emits a loop copying `limit` bytes between two pointer-based
// addresses. If forward is true, copies i=0..limit-1; otherwise i=limit-1..0.
func (l *Lowerer) emitCopyLoop(dstPtr, srcPtr, limit Cell, forward bool) {
	counter := l.allocCell()
	cond := l.allocCell()
	if forward {
		l.emit(&IRZero{Dst: counter})
		l.emit(&IRCmp{Op: CmpLt, Dst: cond, Src1: counter, Src2: limit})
	} else {
		l.emit(&IRCopy{Dst: counter, Src: limit})
		l.emit(&IRCmp{Op: CmpLt, Dst: cond, Src1: counter, Src2: limit}) // nonzero check
		// counter > 0 iff limit > 0; rewrite as "counter != 0".
		l.emit(&IRCopy{Dst: cond, Src: counter})
	}
	saved := l.nodes
	l.nodes = nil
	if !forward {
		l.emit(&IRSubI{Dst: counter, Value: 1})
	}
	srcAddr := l.allocCell()
	l.emit(&IRAdd{Dst: srcAddr, Src1: srcPtr, Src2: counter})
	val := l.ptrLoad(srcAddr)
	l.freeCell(srcAddr)
	dstAddr := l.allocCell()
	l.emit(&IRAdd{Dst: dstAddr, Src1: dstPtr, Src2: counter})
	l.ptrStore(dstAddr, val)
	l.freeCell(val)
	l.freeCell(dstAddr)
	if forward {
		l.emit(&IRAddI{Dst: counter, Value: 1})
		l.emit(&IRCmp{Op: CmpLt, Dst: cond, Src1: counter, Src2: limit})
	} else {
		l.emit(&IRCopy{Dst: cond, Src: counter})
	}
	body := &IRBlock{Nodes: l.nodes}
	l.nodes = saved
	l.emit(&IRLoop{Cond: cond, Body: body})
	l.freeCell(counter)
	l.freeCell(cond)
}

// emitCopy performs the copy operation and returns the cell holding
// the number of elements copied (min(len(dst), len(src))).
func (l *Lowerer) emitCopy(dst, src exprResult) Cell {
	es := max(dst.elemSize, 1)
	// n = min(len(dst), len(src))
	n := l.allocCell()
	cmpCell := l.allocCell()
	l.emit(&IRCmp{Op: CmpLeq, Dst: cmpCell, Src1: dst.lenCell, Src2: src.lenCell})
	savedCopy := l.nodes
	l.nodes = nil
	l.emit(&IRCopy{Dst: n, Src: dst.lenCell})
	thenNodes := l.nodes
	l.nodes = nil
	l.emit(&IRCopy{Dst: n, Src: src.lenCell})
	elseNodes := l.nodes
	l.nodes = savedCopy
	l.emit(&IRIf{Cond: cmpCell, Then: &IRBlock{Nodes: thenNodes}, Else: &IRBlock{Nodes: elseNodes}})
	l.freeCell(cmpCell)
	// limit = n * elemSize (total bytes to copy)
	limit := l.allocCell()
	t := l.allocCell()
	l.emit(&IRConst{Dst: t, Value: byte(es)}) // #nosec G115
	l.emit(&IRMul{Dst: limit, Src1: n, Src2: t})
	l.freeCell(t)
	// When slices overlap with dst after src, copy backwards to avoid
	// overwriting source data. Check dst.ptr >= src.ptr at runtime.
	overlap := l.allocCell()
	l.emit(&IRCmp{Op: CmpGeq, Dst: overlap, Src1: dst.cell, Src2: src.cell})

	// Forward copy: for i := 0; i < limit; i++
	savedFwd := l.nodes
	l.nodes = nil
	l.emitCopyLoop(dst.cell, src.cell, limit, true)
	fwdNodes := l.nodes

	// Backward copy: for i := limit-1; i >= 0; i--
	l.nodes = nil
	l.emitCopyLoop(dst.cell, src.cell, limit, false)
	bwdNodes := l.nodes

	l.nodes = savedFwd
	l.emit(&IRIf{Cond: overlap, Then: &IRBlock{Nodes: bwdNodes}, Else: &IRBlock{Nodes: fwdNodes}})
	l.freeCell(limit)
	return n
}

func (l *Lowerer) lowerCopy(args []ast.Expr) error {
	if len(args) != 2 {
		return fmt.Errorf("copy expects 2 arguments")
	}
	dst, err := l.lowerExpr(args[0])
	if err != nil || dst.lenCell == 0 {
		return fmt.Errorf("copy expects slice arguments")
	}
	src, err := l.lowerExpr(args[1])
	if err != nil || src.lenCell == 0 {
		return fmt.Errorf("copy expects slice arguments")
	}
	n := l.emitCopy(dst, src)
	l.freeCell(n)
	return nil
}

func (l *Lowerer) lowerLocalConsts(gd *ast.GenDecl) error {
	sc := l.currentScope()
	allConsts := l.allByteConsts()
	iota := 0
	var lastExprs []ast.Expr
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		if len(vs.Values) > 0 {
			lastExprs = vs.Values
		}
		for i, name := range vs.Names {
			if i < len(lastExprs) {
				if s, ok := evalStringConstExpr(lastExprs[i], l.result.StringConsts); ok {
					l.result.StringConsts[name.Name] = s
					continue
				}
				val, err := evalConstExpr(lastExprs[i], iota, allConsts)
				if err != nil {
					return fmt.Errorf("const %s: %w", name.Name, err)
				}
				size, err := classifyIntConst(name.Name, val, intTypeSize(vs.Type))
				if err != nil {
					return err
				}
				if size > 1 {
					sc[name.Name] = &intConstBinding{value: uint64(val), size: size} // #nosec G115
				} else {
					sc[name.Name] = &constBinding{value: byte(val)} // #nosec G115
					allConsts[name.Name] = byte(val)                // #nosec G115
				}
			}
		}
		iota++
	}
	return nil
}

func (l *Lowerer) lowerLocalTypes(gd *ast.GenDecl) error {
	for _, spec := range gd.Specs {
		ts, ok := spec.(*ast.TypeSpec)
		if !ok {
			continue
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return fmt.Errorf("unsupported local type: only struct types are supported")
		}
		def := &StructDef{
			Name:               ts.Name.Name,
			Offsets:            make(map[string]int),
			FieldTypes:         make(map[string]string),
			FieldArraySizes:    make(map[string]int),
			FieldInnerSizes:    make(map[string]int),
			FieldArrayElemType: make(map[string]string),
			FieldIntSizes:      make(map[string]int),
			FieldStrings:       make(map[string]bool),
		}
		offset := 0
		for _, field := range st.Fields.List {
			fieldSize := 1
			fieldType := ""
			fieldArraySize := 0
			fieldArrayElemType := ""
			fieldIsString := false
			if id, ok := field.Type.(*ast.Ident); ok {
				if nested, ok := l.result.Structs[id.Name]; ok {
					fieldSize = nested.Size
					fieldType = id.Name
				} else if n := intIdentSize(id.Name); n > 0 {
					fieldSize = n
				} else if id.Name == "string" {
					fieldSize = 3
					fieldIsString = true
				}
			} else if at, ok := field.Type.(*ast.ArrayType); ok && at.Len != nil {
				if arrSize, ies := arrayFieldInfo(field.Type); arrSize > 0 {
					fieldSize = arrSize
					fieldArraySize = arrSize
					if ies > 0 {
						for _, name := range field.Names {
							def.FieldInnerSizes[name.Name] = ies
						}
					}
					if eltID, ok := at.Elt.(*ast.Ident); ok {
						if _, ok := l.result.Structs[eltID.Name]; ok {
							fieldArrayElemType = eltID.Name
						}
					}
				}
			}
			for _, name := range field.Names {
				def.Fields = append(def.Fields, name.Name)
				def.Offsets[name.Name] = offset
				if fieldType != "" {
					def.FieldTypes[name.Name] = fieldType
				}
				if fieldArraySize > 0 {
					def.FieldArraySizes[name.Name] = fieldArraySize
				}
				if fieldArrayElemType != "" {
					def.FieldArrayElemType[name.Name] = fieldArrayElemType
				}
				if fieldIsString {
					def.FieldStrings[name.Name] = true
				} else if fieldSize >= 2 && fieldType == "" && fieldArraySize == 0 {
					def.FieldIntSizes[name.Name] = fieldSize
				}
				offset += fieldSize
			}
		}
		def.Size = offset
		if _, exists := l.result.Structs[def.Name]; !exists {
			l.result.Structs[def.Name] = def
		}
	}
	return nil
}

func (l *Lowerer) lowerDecl(s *ast.DeclStmt) error {
	l.declareFromDecl(s)
	gd, ok := s.Decl.(*ast.GenDecl)
	if !ok {
		return fmt.Errorf("unsupported declaration")
	}
	if gd.Tok == token.CONST {
		return l.lowerLocalConsts(gd)
	}
	if gd.Tok == token.TYPE {
		return l.lowerLocalTypes(gd)
	}
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		if vs.Type != nil && arrayNestingDepth(vs.Type) > 3 {
			return fmt.Errorf("array nesting deeper than 3 levels is not supported")
		}
		if vs.Type != nil && sliceNestingDepth(vs.Type) > 2 {
			return fmt.Errorf("slice nesting deeper than 2 levels is not supported")
		}
		for i, name := range vs.Names {
			if i >= len(vs.Values) {
				// No initializer: zero the variable/array/struct/slice.
				if ai, ok := l.lookupArray(name.Name); ok {
					for j := range ai.size {
						l.emit(&IRZero{Dst: ai.base + j})
					}
				} else if si, ok := l.lookupStruct(name.Name); ok {
					for j := range si.def.Size {
						l.emit(&IRZero{Dst: si.base + j})
					}
				} else if base, ok := l.lookupIntCell(name.Name); ok {
					l.emit(&IRZero{Dst: base})
					l.emit(&IRZero{Dst: base + 1})
				} else if si, ok := l.lookupSlice(name.Name); ok {
					l.emit(&IRZero{Dst: si.ptr})
					l.emit(&IRZero{Dst: si.len})
					l.emit(&IRZero{Dst: si.cap})
				} else if cell, err := l.lookupVar(name.Name); err == nil {
					l.emit(&IRZero{Dst: cell})
				}
				continue
			}
			if err := l.lowerVarInit(name.Name, vs.Values[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// lowerVarInit handles `name = rhs` where rhs can be a composite literal,
// a composite variable, or a scalar expression.
func (l *Lowerer) lowerVarInit(name string, rhs ast.Expr) error {
	// Slice assignment: s = make([]byte, n) or s = append(s, v) or s = expr
	if si, ok := l.lookupSlice(name); ok {
		return l.lowerSliceAssign(si, rhs)
	}
	// Multi-byte integer assignment.
	if base, ok := l.lookupIntCell(name); ok {
		n := l.lookupIntVarSize(name)
		maxVal := uint64(1)<<(n*8) - 1
		// Handle integer literal directly.
		if lit, ok := rhs.(*ast.BasicLit); ok && lit.Kind == token.INT {
			val, err := strconv.ParseUint(lit.Value, 0, 64)
			if err != nil {
				return err
			}
			if val > maxVal {
				return fmt.Errorf("integer literal %d out of uint%d range (0-%d)", val, n*8, maxVal)
			}
			for j := range n {
				l.emit(&IRConst{Dst: base + j, Value: byte(val >> (j * 8))}) // #nosec G115
			}
			return nil
		}
		r, err := l.lowerExpr(rhs)
		if err != nil {
			return err
		}
		if r.intSize >= 2 {
			l.emitCopyOrMove(base, exprResult{cell: r.cell, temp: r.temp, size: r.intSize})
			return nil
		}
		// byte -> multi-byte: zero-extend.
		l.emitCopyOrMove(base, r)
		for j := 1; j < n; j++ {
			l.emit(&IRZero{Dst: base + j})
		}
		return nil
	}
	// Composite literal: a = [N]byte{...} or p = Point{...}
	if comp, ok := rhs.(*ast.CompositeLit); ok {
		if comp.Type != nil && arrayNestingDepth(comp.Type) > 3 {
			return fmt.Errorf("array nesting deeper than 3 levels is not supported")
		}
		size := l.arraySize(comp.Type)
		if size > 0 {
			return l.lowerCompositeLit(name, comp)
		}
		if size == 0 && comp.Type != nil {
			if _, ok := comp.Type.(*ast.ArrayType); ok {
				return nil // [0]byte{} -- no-op
			}
		}
		if def := l.structDef(comp.Type); def != nil {
			return l.lowerStructLit(name, comp, def)
		}
	}
	// Track pointer-to-struct/array type: ptr = &myStruct or ptr = &myArray
	if unary, ok := rhs.(*ast.UnaryExpr); ok && unary.Op == token.AND {
		if rhsID, ok := unary.X.(*ast.Ident); ok {
			if si, ok := l.lookupStruct(rhsID.Name); ok {
				l.currentScope().annotatePtrType(name, si.def.Name)
			}
			if ai, ok := l.lookupArray(rhsID.Name); ok {
				l.currentScope().annotatePtrArray(name, ai)
			}
		}
		if comp, ok := unary.X.(*ast.CompositeLit); ok {
			if def := l.structDef(comp.Type); def != nil {
				l.currentScope().annotatePtrType(name, def.Name)
			}
		}
	}
	// Composite variable copy: b = a where a is array or struct.
	// Must define the LHS as composite if it's a := declaration.
	if rhsID, ok := rhs.(*ast.Ident); ok {
		if srcSI, ok := l.lookupStruct(rhsID.Name); ok {
			sc := l.currentScope()
			delete(sc, name)
			if !sc.hasStruct(name) {
				l.defineStruct(sc, name, srcSI.def)
			}
		} else if srcAI, ok := l.lookupArray(rhsID.Name); ok {
			sc := l.currentScope()
			delete(sc, name)
			if !sc.hasArray(name) {
				if srcAI.elemSize > 1 || srcAI.elemType != "" {
					l.defineStructArray(sc, name, srcAI.count, srcAI.elemSize, srcAI.elemType,
						srcAI.elemIntSize, srcAI.elemSlice, srcAI.innerElemSize, srcAI.innerElemIntSize)
				} else {
					l.defineArray(sc, name, srcAI.size)
				}
			}
		}
	}
	r, err := l.lowerExpr(rhs)
	if err != nil {
		return err
	}
	// Track pointer type from expression result (function returns, etc.).
	if r.ptrIntSize >= 2 {
		l.currentScope().annotatePtrIntSize(name, r.ptrIntSize)
	}
	if r.isPointer {
		sc := l.currentScope()
		if r.typeName != "" {
			sc.annotatePtrType(name, r.typeName)
		} else if r.elemCount > 0 && r.elemCount != 255 {
			sc.annotatePtrArray(name, arrayInfo{
				size: r.elemCount, count: r.elemCount,
				elemSize: max(r.elemSize, 1), elemType: r.elemType,
			})
		}
	}
	// Resolve destination and copy.
	dst, err := l.lowerExpr(&ast.Ident{Name: name})
	if err != nil {
		return err
	}
	return l.assignResult(dst, r)
}

// assignResult copies an expression result to a destination.
func (l *Lowerer) assignResult(dst, r exprResult) error {
	// Reject assigning wider integer to narrower variable.
	if r.intSize >= 2 && dst.intSize < 2 && dst.size <= 1 {
		if r.temp {
			l.freeCellRange(r.cell, r.intSize)
		}
		return fmt.Errorf("cannot assign wider integer to byte variable, use explicit conversion")
	}
	if r.cell == dst.cell {
		if r.temp {
			l.freeCell(r.cell)
		}
		return nil
	}
	// Flat-offset result: materialize by reading each element from the flat array.
	if r.flatBase != 0 && dst.size > 1 {
		totalSize := r.elemCount * r.elemSize
		flatArr := arrayInfo{base: r.flatBase, size: totalSize, count: totalSize, elemSize: 1}
		n := min(r.elemCount, dst.size)
		dsts := make([]Cell, n)
		for j := range n {
			dsts[j] = dst.cell + Cell(j) // #nosec G115
		}
		l.loadConsecutiveViaIndex(flatArr, r.cell, dsts)
		l.freeCell(r.cell)
		return nil
	}
	// Pointer-based composite: materialize by loading each cell.
	if r.isPointer && r.elemCount > 1 && dst.size > 1 {
		for j := range min(r.elemCount, dst.size) {
			val := l.ptrLoad(r.cell)
			l.emit(&IRMove{Dst: dst.cell + j, Src: val})
			l.freeCell(val)
			if j < r.elemCount-1 {
				l.emit(&IRAddI{Dst: r.cell, Value: 1})
			}
		}
		l.freeCell(r.cell)
		return nil
	}
	l.emitCopyOrMove(dst.cell, r)
	return nil
}

// assignOp maps assignment operation tokens to binary operators.
var assignOp = map[token.Token]token.Token{
	token.ADD_ASSIGN:     token.ADD,
	token.SUB_ASSIGN:     token.SUB,
	token.MUL_ASSIGN:     token.MUL,
	token.QUO_ASSIGN:     token.QUO,
	token.REM_ASSIGN:     token.REM,
	token.AND_ASSIGN:     token.AND,
	token.OR_ASSIGN:      token.OR,
	token.XOR_ASSIGN:     token.XOR,
	token.SHL_ASSIGN:     token.SHL,
	token.SHR_ASSIGN:     token.SHR,
	token.AND_NOT_ASSIGN: token.AND_NOT,
}

func (l *Lowerer) lowerAssign(s *ast.AssignStmt) error {
	l.declareFromAssign(s)
	// Desugar assignment operations: x += y -> x = x + y
	if op, ok := assignOp[s.Tok]; ok && len(s.Lhs) == 1 && len(s.Rhs) == 1 {
		s = &ast.AssignStmt{
			Lhs: s.Lhs,
			Tok: token.ASSIGN,
			Rhs: []ast.Expr{&ast.BinaryExpr{X: s.Lhs[0], Op: op, Y: s.Rhs[0]}},
		}
	}

	// Handle function/method return assignments.
	if len(s.Rhs) == 1 {
		if call, ok := s.Rhs[0].(*ast.CallExpr); ok {
			funcName, receiver := l.resolveCall(call)
			if info, ok := l.result.Funcs[funcName]; ok {
				args := l.prependReceiver(receiver, info, call.Args)
				// Multi-return: q, r := divmod(a, b) or a[0], a[1] = divmod(a, b)
				if len(info.ReturnSizes) == len(s.Lhs) && len(info.ReturnSizes) > 1 {
					return l.lowerMultiReturnAssign(s, info, args)
				}
				// Composite return: p := f() where f returns struct, array, or slice.
				if len(s.Lhs) == 1 && !info.ReturnType.IsPointer &&
					(info.ReturnType.ArraySize > 0 || info.ReturnType.StructType != "" || info.ReturnType.IsSlice) {
					return l.lowerCompositeReturnAssign(s.Lhs[0], info, args)
				}
			}
		}
	}

	// For multiple assignments (e.g., a, b = b, a), evaluate all RHS first
	// into temporaries, then assign to LHS. This ensures correct swap semantics.
	if len(s.Lhs) > 1 && len(s.Lhs) == len(s.Rhs) {
		type rhsValue struct {
			cell  Cell
			size  int // 1 for byte, >1 for composite
			isStr bool
			str   sliceInfo
		}
		rhsVals := make([]rhsValue, len(s.Rhs))
		for i, rhs := range s.Rhs {
			r, err := l.lowerExpr(rhs)
			if err != nil {
				// String/slice literal as RHS: materialize to a slice header.
				if si, sliceErr := l.lowerSliceExpr(rhs); sliceErr == nil {
					rhsVals[i] = rhsValue{isStr: true, str: si}
					continue
				}
				return err
			}
			if r.lenCell != 0 {
				// String/slice header: snapshot all 3 cells.
				ptrT := l.allocCell()
				lenT := l.allocCell()
				capT := l.allocCell()
				l.emit(&IRCopy{Dst: ptrT, Src: r.cell})
				l.emit(&IRCopy{Dst: lenT, Src: r.lenCell})
				l.emit(&IRCopy{Dst: capT, Src: r.capCell})
				if r.temp {
					l.freeCell(r.cell)
					l.freeCell(r.lenCell)
					l.freeCell(r.capCell)
				}
				rhsVals[i] = rhsValue{isStr: true, str: sliceInfo{ptr: ptrT, len: lenT, cap: capT, elemSize: 1}}
				continue
			}
			r = l.ensureTemp(r)
			rhsVals[i] = rhsValue{cell: r.cell, size: r.cellCount()}
		}
		// Assign to all LHS.
		for i, lhs := range s.Lhs {
			rv := rhsVals[i]
			if rv.isStr {
				if err := l.assignStringHeader(lhs, rv.str); err != nil {
					return err
				}
				l.freeCell(rv.str.ptr)
				l.freeCell(rv.str.len)
				l.freeCell(rv.str.cap)
				continue
			}
			val := exprResult{cell: rv.cell, temp: true, size: rv.size}
			switch t := lhs.(type) {
			case *ast.IndexExpr:
				base, err := l.lowerExpr(t.X)
				if err != nil {
					return err
				}
				if base.elemCount > 0 {
					if err := l.writeInto(base, t.Index, val); err != nil {
						return err
					}
					continue
				}
				return fmt.Errorf("cannot index non-array expression")
			case *ast.SelectorExpr:
				if err := l.assignFieldFromCell(t, rv.cell); err != nil {
					return err
				}
				l.freeCell(rv.cell)
				continue
			case *ast.StarExpr:
				if err := l.lowerDerefAssignFromCell(t.X, rv.cell); err != nil {
					return err
				}
				l.freeCell(rv.cell)
				continue
			}
			dst, err := l.lowerExpr(lhs)
			if err != nil {
				return err
			}
			l.emitCopyOrMove(dst.cell, val)
		}
		return nil
	}

	for i, lhs := range s.Lhs {
		rhs := s.Rhs[i]
		switch target := lhs.(type) {
		case *ast.IndexExpr:
			return l.lowerArrayAssign(target, rhs)
		case *ast.StarExpr:
			return l.lowerDerefAssign(target.X, rhs)
		case *ast.SelectorExpr:
			return l.lowerFieldAssign(target, rhs)
		case *ast.Ident:
			if err := l.lowerVarInit(target.Name, rhs); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported assignment target: %T", lhs)
		}
	}
	return nil
}

func (l *Lowerer) lowerMultiReturnAssign(s *ast.AssignStmt, info *FuncInfo, args []ast.Expr) error {
	retCells, err := l.inlineCall(info, args)
	if err != nil {
		return err
	}
	off := 0
	for i, lhs := range s.Lhs {
		n := 1
		if i < len(info.ReturnSizes) {
			n = info.ReturnSizes[i]
		}
		switch target := lhs.(type) {
		case *ast.Ident:
			if n == 3 {
				if si, ok := l.lookupSlice(target.Name); ok {
					l.emit(&IRMove{Dst: si.ptr, Src: retCells[off]})
					l.emit(&IRMove{Dst: si.len, Src: retCells[off+1]})
					l.emit(&IRMove{Dst: si.cap, Src: retCells[off+2]})
				}
			} else if n >= 2 {
				if base, ok := l.lookupIntCell(target.Name); ok {
					for j := range n {
						l.emit(&IRMove{Dst: base + j, Src: retCells[off+j]})
					}
				}
			} else {
				cell, err := l.lookupVar(target.Name)
				if err != nil {
					return err
				}
				l.emit(&IRMove{Dst: cell, Src: retCells[off]})
			}
		case *ast.IndexExpr:
			base, err := l.lowerExpr(target.X)
			if err != nil {
				return err
			}
			if err := l.writeInto(base, target.Index, exprResult{cell: retCells[off]}); err != nil {
				return err
			}
			l.freeCell(retCells[off])
		case *ast.SelectorExpr:
			r, err := l.lowerSelectorExpr(target)
			if err != nil {
				return err
			}
			for j := range n {
				l.emit(&IRMove{Dst: r.cell + j, Src: retCells[off+j]})
			}
		default:
			return fmt.Errorf("unsupported assignment target")
		}
		off += n
	}
	return nil
}

func (l *Lowerer) lowerArrayAssign(idx *ast.IndexExpr, rhs ast.Expr) error {
	base, err := l.lowerExpr(idx.X)
	if err != nil {
		return err
	}
	if base.elemCount == 0 {
		depth := 0
		for x := ast.Expr(idx); ; depth++ {
			if ie, ok := x.(*ast.IndexExpr); ok {
				x = ie.X
			} else {
				break
			}
		}
		if depth > 3 {
			return fmt.Errorf("array nesting deeper than 3 levels is not supported")
		}
		return fmt.Errorf("cannot index non-array expression")
	}
	// Slice element write: s[i] = t, s[i] = make(...), s[i] = []byte{...}.
	if base.elemSlice {
		if base.isPointer {
			inner, err := l.lowerSliceExpr(rhs)
			if err != nil {
				return err
			}
			addr, err := l.ptrDynIndex(base.cell, idx.Index, 3)
			if err != nil {
				return err
			}
			l.storeSliceHeader(addr, inner)
			return nil
		}
		// Array of slices (`[N]string`, `[N][]byte`): write 3 cells at
		// base.cell + i*3 directly. Constant index hits an in-place
		// IRCopy; variable index uses storeConsecutiveViaIndex.
		src, srcTemp, err := l.resolveStringSlice(rhs)
		if err != nil {
			return err
		}
		if constIdx, ok := l.constValue(idx.Index); ok {
			if constIdx >= base.elemCount {
				return fmt.Errorf("array index %d out of bounds [0:%d]", constIdx, base.elemCount)
			}
			dst := base.cell + Cell(constIdx*3) // #nosec G115
			l.emit(&IRCopy{Dst: dst, Src: src.ptr})
			l.emit(&IRCopy{Dst: dst + 1, Src: src.len})
			l.emit(&IRCopy{Dst: dst + 2, Src: src.cap})
		} else {
			flatArr := arrayInfo{base: base.cell, size: base.elemCount * 3, count: base.elemCount * 3, elemSize: 1}
			idxR, err := l.lowerExpr(idx.Index)
			if err != nil {
				return err
			}
			es := l.allocCell()
			l.emit(&IRConst{Dst: es, Value: 3})
			flatIdx := l.allocCell()
			l.emit(&IRMul{Dst: flatIdx, Src1: idxR.cell, Src2: es})
			l.freeCell(es)
			if idxR.temp {
				l.freeCell(idxR.cell)
			}
			l.storeConsecutiveViaIndex(flatArr, flatIdx, []Cell{src.ptr, src.len, src.cap})
			l.freeCell(flatIdx)
		}
		if srcTemp {
			l.freeSliceInfo(src)
		}
		return nil
	}
	if comp, ok := rhs.(*ast.CompositeLit); ok {
		return l.lowerCompositeElemAssign(base, idx.Index, comp)
	}
	r, err := l.lowerExpr(rhs)
	if err != nil {
		return err
	}
	return l.writeInto(base, idx.Index, r)
}

// storeSliceHeader stores a 3-cell slice header (ptr, len, cap) at the
// given address via pointer writes. Frees addr and inner after storing.
func (l *Lowerer) storeSliceHeader(addr Cell, inner sliceInfo) {
	t := l.allocCell()
	l.emit(&IRCopy{Dst: t, Src: inner.ptr})
	l.ptrStore(addr, t)
	l.emit(&IRAddI{Dst: addr, Value: 1})
	l.emit(&IRCopy{Dst: t, Src: inner.len})
	l.ptrStore(addr, t)
	l.emit(&IRAddI{Dst: addr, Value: 1})
	l.emit(&IRCopy{Dst: t, Src: inner.cap})
	l.ptrStore(addr, t)
	l.freeCell(t)
	l.freeCell(addr)
	l.freeSliceInfo(inner)
}

// lowerCompositeElemAssign handles a[i] = CompositeLit where the RHS is
// a struct literal (Point{x: 1}) or array literal ([3]byte{1, 2, 3}).
// Slice literals are handled by the elemSlice path in lowerArrayAssign.
func (l *Lowerer) lowerCompositeElemAssign(base exprResult, indexExpr ast.Expr, comp *ast.CompositeLit) error {
	// Determine how to lower the literal: struct or array.
	lowerLitInto := func(dst Cell) error {
		if def := l.structDef(comp.Type); def != nil {
			return l.lowerStructValueTo(dst, def, comp)
		}
		subArr := arrayInfo{base: dst, size: base.elemSize, count: base.elemSize, elemSize: 1}
		return l.lowerCompositeLitInto(subArr, comp)
	}
	// Constant index: write directly into the element.
	if constIdx, ok := l.constValue(indexExpr); ok {
		if constIdx >= base.elemCount {
			return fmt.Errorf("array index %d out of bounds [0:%d]", constIdx, base.elemCount)
		}
		if base.isPointer {
			// Pointer-based: evaluate literal into temps, then store via pointer.
			valBase := l.allocCells(base.elemSize)
			for j := range base.elemSize {
				l.emit(&IRZero{Dst: valBase + j})
			}
			if err := lowerLitInto(valBase); err != nil {
				return err
			}
			addr, err := l.ptrDynIndex(base.cell, indexExpr, base.elemSize)
			if err != nil {
				return err
			}
			for j := range base.elemSize {
				l.ptrStore(addr, valBase+j)
				l.freeCell(valBase + j)
				if j < base.elemSize-1 {
					l.emit(&IRAddI{Dst: addr, Value: 1})
				}
			}
			l.freeCell(addr)
			return nil
		}
		return lowerLitInto(base.cell + constIdx*base.elemSize)
	}
	// Variable index: evaluate into temp cells, then dynamic store.
	valBase := l.allocCells(base.elemSize)
	for j := range base.elemSize {
		l.emit(&IRZero{Dst: valBase + j})
	}
	if err := lowerLitInto(valBase); err != nil {
		return err
	}
	if base.isPointer {
		// Pointer-based: store via ptrStore at ptr + index * elemSize.
		addr, err := l.ptrDynIndex(base.cell, indexExpr, base.elemSize)
		if err != nil {
			return err
		}
		for j := range base.elemSize {
			l.ptrStore(addr, valBase+j)
			l.freeCell(valBase + j)
			if j < base.elemSize-1 {
				l.emit(&IRAddI{Dst: addr, Value: 1})
			}
		}
		l.freeCell(addr)
		return nil
	}
	ai := arrayInfo{
		base: base.cell, size: base.elemCount * base.elemSize,
		count: base.elemCount, elemSize: base.elemSize,
	}
	baseOffset, err := l.lowerCompositeVarIndex(ai, indexExpr)
	if err != nil {
		return err
	}
	flatArr := flatArrayOf(ai)
	srcs := make([]Cell, base.elemSize)
	for j := range base.elemSize {
		srcs[j] = valBase + Cell(j) // #nosec G115
	}
	l.storeConsecutiveViaIndex(flatArr, baseOffset.cell, srcs)
	for j := range base.elemSize {
		l.freeCell(valBase + Cell(j)) // #nosec G115
	}
	l.freeCell(baseOffset.cell)
	return nil
}

// lowerCompositeReturnAssign handles p := f() where f returns
// a struct or array. Inlines the call and moves return cells to
// the composite variable's base.
func (l *Lowerer) lowerCompositeReturnAssign(lhs ast.Expr, info *FuncInfo, args []ast.Expr) error {
	// Index expression: s[i] = f() where f returns struct/array.
	if idx, ok := lhs.(*ast.IndexExpr); ok {
		retCells, err := l.inlineCall(info, args)
		if err != nil {
			return err
		}
		base, err := l.lowerExpr(idx.X)
		if err != nil {
			return err
		}
		retSize := info.Returns
		if info.ReturnType.StructType != "" {
			retSize = l.result.Structs[info.ReturnType.StructType].Size
		} else if info.ReturnType.ArraySize > 0 {
			retSize = info.ReturnType.ArraySize
		}
		val := exprResult{cell: retCells[0], temp: true, size: retSize}
		return l.writeInto(base, idx.Index, val)
	}
	id, ok := lhs.(*ast.Ident)
	if !ok {
		return fmt.Errorf("unsupported assignment target for composite return")
	}
	retCells, err := l.inlineCall(info, args)
	if err != nil {
		return err
	}
	// Find or define the composite variable and get its base.
	// Remove any scalar cell that may have been allocated already,
	// since the variable is actually a composite.
	var base Cell
	if info.ReturnType.IsSlice {
		sc := l.currentScope()
		delete(sc, id.Name)
		es := max(info.ReturnType.SliceElemSize, 1)
		et := info.ReturnType.SliceElemType
		if si, ok := l.lookupSlice(id.Name); ok {
			l.emit(&IRMove{Dst: si.ptr, Src: retCells[0]})
			l.emit(&IRMove{Dst: si.len, Src: retCells[1]})
			l.emit(&IRMove{Dst: si.cap, Src: retCells[2]})
		} else {
			newSI := l.defineSlice(sc, id.Name, es, et, info.ReturnType.SliceElemSlice, "", info.ReturnType.SliceElemIntSize)
			l.emit(&IRMove{Dst: newSI.ptr, Src: retCells[0]})
			l.emit(&IRMove{Dst: newSI.len, Src: retCells[1]})
			l.emit(&IRMove{Dst: newSI.cap, Src: retCells[2]})
		}
		return nil
	}
	if info.ReturnType.StructType != "" {
		def := l.result.Structs[info.ReturnType.StructType]
		sc := l.currentScope()
		delete(sc, id.Name)
		if !sc.hasStruct(id.Name) {
			l.defineStruct(sc, id.Name, def)
		}
		si, _ := l.lookupStruct(id.Name)
		base = si.base
	} else {
		size := info.ReturnType.ArraySize
		sc := l.currentScope()
		delete(sc, id.Name)
		if !sc.hasArray(id.Name) {
			l.defineArray(sc, id.Name, size)
		}
		ai, _ := l.lookupArray(id.Name)
		base = ai.base
	}
	for j := range len(retCells) {
		l.emit(&IRMove{Dst: base + j, Src: retCells[j]})
	}
	return nil
}

func (l *Lowerer) lowerFieldAssign(sel *ast.SelectorExpr, rhs ast.Expr) error {
	// Resolve the base (struct, array element, or pointer).
	base, err := l.lowerExpr(sel.X)
	if err != nil {
		return err
	}
	if base.typeName == "" {
		return fmt.Errorf("undefined struct in field assignment")
	}
	def := l.result.Structs[base.typeName]
	offset := def.Offsets[sel.Sel.Name]
	if base.isPointer {
		// String field via pointer: write 3 cells (ptr, len, cap).
		if def.FieldStrings[sel.Sel.Name] {
			si, isTemp, err := l.resolveStringSlice(rhs)
			if err != nil {
				return err
			}
			l.storeStringHeaderViaPtr(l.ptrOffset(base.cell, offset), si)
			if isTemp {
				l.freeSliceInfo(si)
			}
			return nil
		}
		// Pointer write: compute slot = ptr + offset, then store.
		slot := l.ptrOffset(base.cell, offset)
		val, err := l.lowerExpr(rhs)
		if err != nil {
			return err
		}
		if intSize := def.FieldIntSizes[sel.Sel.Name]; intSize >= 2 {
			srcs := make([]Cell, intSize)
			for j := range intSize {
				srcs[j] = val.cell + Cell(j) // #nosec G115
			}
			l.storeConsecutiveViaPtr(slot, srcs)
			if val.temp {
				l.freeCellRange(val.cell, intSize)
			}
			return nil
		}
		t := l.allocCell()
		l.emitCopyOrMove(t, val)
		l.ptrStore(slot, t)
		l.freeCell(t)
		l.freeCell(slot)
		return nil
	}
	// Check if the field is a nested struct type.
	if fieldType := def.FieldTypes[sel.Sel.Name]; fieldType != "" {
		fieldDef := l.result.Structs[fieldType]
		return l.lowerStructValueTo(base.cell+offset, fieldDef, rhs)
	}
	// String field: copy ptr/len/cap (3 cells).
	if def.FieldStrings[sel.Sel.Name] {
		si, isTemp, err := l.resolveStringSlice(rhs)
		if err != nil {
			return err
		}
		l.emit(&IRCopy{Dst: base.cell + offset, Src: si.ptr})
		l.emit(&IRCopy{Dst: base.cell + offset + 1, Src: si.len})
		l.emit(&IRCopy{Dst: base.cell + offset + 2, Src: si.cap})
		if isTemp {
			l.freeSliceInfo(si)
		}
		return nil
	}
	// Multi-byte int field: copy N cells.
	if intSize := def.FieldIntSizes[sel.Sel.Name]; intSize >= 2 {
		val, err := l.lowerExpr(rhs)
		if err != nil {
			return err
		}
		if base.isPointer {
			slot := l.ptrOffset(base.cell, offset)
			for j := range intSize {
				t := l.allocCell()
				if val.temp {
					l.emit(&IRMove{Dst: t, Src: val.cell + j})
				} else {
					l.emit(&IRCopy{Dst: t, Src: val.cell + j})
				}
				l.ptrStore(slot, t)
				l.freeCell(t)
				if j < intSize-1 {
					l.emit(&IRAddI{Dst: slot, Value: 1})
				}
			}
			l.freeCell(slot)
			return nil
		}
		// Variable-index struct array element: base.cell holds i*elemSize
		// relative to base.flatBase. Write N cells via dynamic-index store.
		if base.flatBase != 0 {
			totalSize := base.elemCount * base.elemSize
			flatArr := arrayInfo{base: base.flatBase, size: totalSize, count: totalSize, elemSize: 1}
			for j := range intSize {
				idxCell := l.allocCell()
				l.emit(&IRCopy{Dst: idxCell, Src: base.cell})
				if off := offset + j; off > 0 {
					l.emit(&IRAddI{Dst: idxCell, Value: byte(off)}) // #nosec G115
				}
				l.emitVariableIndexWrite(flatArr, idxCell, val.cell+j)
				l.freeCell(idxCell)
			}
			if val.temp {
				l.freeCellRange(val.cell, intSize)
			}
			if base.temp {
				l.freeCell(base.cell)
			}
			return nil
		}
		dst := base.cell + offset
		l.emitCopyOrMove(dst, exprResult{cell: val.cell, temp: val.temp, size: intSize})
		return nil
	}
	// Direct or flat-offset write via writeInto.
	val, err := l.lowerExpr(rhs)
	if err != nil {
		return err
	}
	offsetExpr := &ast.BasicLit{Kind: token.INT, Value: strconv.Itoa(offset)}
	return l.writeInto(base, offsetExpr, val)
}

// assignStringHeader writes a 3-cell string header (snapshotted in src)
// into the LHS slot. Used by parallel-assign to swap string-shaped values.
func (l *Lowerer) assignStringHeader(lhs ast.Expr, src sliceInfo) error {
	switch t := lhs.(type) {
	case *ast.SelectorExpr:
		base, err := l.lowerExpr(t.X)
		if err != nil {
			return err
		}
		if base.typeName == "" {
			return fmt.Errorf("undefined struct in field assignment")
		}
		def := l.result.Structs[base.typeName]
		if !def.FieldStrings[t.Sel.Name] {
			return fmt.Errorf("expected string field, got %s", t.Sel.Name)
		}
		offset := def.Offsets[t.Sel.Name]
		if base.isPointer {
			l.storeStringHeaderViaPtr(l.ptrOffset(base.cell, offset), src)
			return nil
		}
		l.emit(&IRCopy{Dst: base.cell + offset, Src: src.ptr})
		l.emit(&IRCopy{Dst: base.cell + offset + 1, Src: src.len})
		l.emit(&IRCopy{Dst: base.cell + offset + 2, Src: src.cap})
		return nil
	case *ast.Ident:
		si, ok := l.lookupSlice(t.Name)
		if !ok {
			return fmt.Errorf("expected string variable: %s", t.Name)
		}
		l.emit(&IRCopy{Dst: si.ptr, Src: src.ptr})
		l.emit(&IRCopy{Dst: si.len, Src: src.len})
		l.emit(&IRCopy{Dst: si.cap, Src: src.cap})
		return nil
	case *ast.IndexExpr:
		base, err := l.lowerExpr(t.X)
		if err != nil {
			return err
		}
		if !base.elemSlice {
			return fmt.Errorf("expected slice-of-strings target")
		}
		if base.isPointer {
			addr, err := l.ptrDynIndex(base.cell, t.Index, 3)
			if err != nil {
				return err
			}
			tmp := l.allocCell()
			l.emit(&IRCopy{Dst: tmp, Src: src.ptr})
			l.ptrStore(addr, tmp)
			l.emit(&IRAddI{Dst: addr, Value: 1})
			l.emit(&IRCopy{Dst: tmp, Src: src.len})
			l.ptrStore(addr, tmp)
			l.emit(&IRAddI{Dst: addr, Value: 1})
			l.emit(&IRCopy{Dst: tmp, Src: src.cap})
			l.ptrStore(addr, tmp)
			l.freeCell(tmp)
			l.freeCell(addr)
			return nil
		}
		if constIdx, ok := l.constValue(t.Index); ok {
			if constIdx >= base.elemCount {
				return fmt.Errorf("array index %d out of bounds [0:%d]", constIdx, base.elemCount)
			}
			dst := base.cell + Cell(constIdx*3) // #nosec G115
			l.emit(&IRCopy{Dst: dst, Src: src.ptr})
			l.emit(&IRCopy{Dst: dst + 1, Src: src.len})
			l.emit(&IRCopy{Dst: dst + 2, Src: src.cap})
			return nil
		}
		flatArr := arrayInfo{base: base.cell, size: base.elemCount * 3, count: base.elemCount * 3, elemSize: 1}
		idxR, err := l.lowerExpr(t.Index)
		if err != nil {
			return err
		}
		es := l.allocCell()
		l.emit(&IRConst{Dst: es, Value: 3})
		flatIdx := l.allocCell()
		l.emit(&IRMul{Dst: flatIdx, Src1: idxR.cell, Src2: es})
		l.freeCell(es)
		if idxR.temp {
			l.freeCell(idxR.cell)
		}
		l.storeConsecutiveViaIndex(flatArr, flatIdx, []Cell{src.ptr, src.len, src.cap})
		l.freeCell(flatIdx)
		return nil
	}
	return fmt.Errorf("unsupported parallel string-assign target: %T", lhs)
}

// assignFieldFromCell writes a single byte cell into a struct field's slot.
// Used by parallel-assign for scalar RHS values targeting struct fields.
func (l *Lowerer) assignFieldFromCell(sel *ast.SelectorExpr, src Cell) error {
	base, err := l.lowerExpr(sel.X)
	if err != nil {
		return err
	}
	if base.typeName == "" {
		return fmt.Errorf("undefined struct in field assignment")
	}
	def := l.result.Structs[base.typeName]
	offset := def.Offsets[sel.Sel.Name]
	if base.isPointer {
		slot := l.ptrOffset(base.cell, offset)
		t := l.allocCell()
		l.emit(&IRMove{Dst: t, Src: src})
		l.ptrStore(slot, t)
		l.freeCell(t)
		l.freeCell(slot)
		return nil
	}
	l.emit(&IRMove{Dst: base.cell + offset, Src: src})
	return nil
}

// lowerDerefAssignFromCell writes a single byte to *p, where p is the inner
// pointer expression of a StarExpr LHS. Used by parallel-assign.
func (l *Lowerer) lowerDerefAssignFromCell(ptrExpr ast.Expr, src Cell) error {
	p, err := l.lowerExpr(ptrExpr)
	if err != nil {
		return err
	}
	t := l.allocCell()
	l.emit(&IRMove{Dst: t, Src: src})
	l.ptrStore(p.cell, t)
	l.freeCell(t)
	return nil
}

// flatArrayOf returns a flat (elemSize=1) view of a composite array,
// for use with `emitVariableIndexRead`/`emitVariableIndexWrite`.
func flatArrayOf(ai arrayInfo) arrayInfo {
	return arrayInfo{base: ai.base, size: ai.size, count: ai.size, elemSize: 1}
}

// lowerCompositeVarIndex computes i * elemSize as a flat offset temp cell.
// The caller must add the field/inner offset and use dynamic load/store.
func (l *Lowerer) lowerCompositeVarIndex(ai arrayInfo, indexExpr ast.Expr) (exprResult, error) {
	indexR, err := l.lowerExpr(indexExpr)
	if err != nil {
		return exprResult{}, err
	}
	if indexR.intSize >= 2 {
		if indexR.temp {
			l.freeCellRange(indexR.cell, indexR.intSize)
		}
		return exprResult{}, fmt.Errorf("cannot use multi-byte integer as array index, use byte() to truncate")
	}
	es := l.allocCell()
	l.emit(&IRConst{Dst: es, Value: byte(ai.elemSize)}) // #nosec G115
	flatIdx := l.allocCell()
	l.emit(&IRMul{Dst: flatIdx, Src1: indexR.cell, Src2: es})
	l.freeCell(es)
	if indexR.temp {
		l.freeCell(indexR.cell)
	}
	return exprResult{cell: flatIdx, temp: true}, nil
}

// addFlatOffset adds an offset expression to a flat index cell.
// Returns the (possibly new) cell holding the combined offset.
// If the offset is constant, adds in-place; otherwise allocates a new cell.
func (l *Lowerer) addFlatOffset(flatIdx Cell, offsetExpr ast.Expr) (Cell, error) {
	if constOff, ok := l.constValue(offsetExpr); ok {
		if constOff > 0 {
			l.emit(&IRAddI{Dst: flatIdx, Value: byte(constOff)}) // #nosec G115
		}
		return flatIdx, nil
	}
	offR, err := l.lowerExpr(offsetExpr)
	if err != nil {
		return 0, err
	}
	t := l.allocCell()
	l.emit(&IRAdd{Dst: t, Src1: flatIdx, Src2: offR.cell})
	l.freeCell(flatIdx)
	if offR.temp {
		l.freeCell(offR.cell)
	}
	return t, nil
}

// constValue returns the constant integer value of an expression, if it is one.
func (l *Lowerer) constValue(expr ast.Expr) (int, bool) {
	if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.INT {
		val, err := strconv.Atoi(lit.Value)
		if err != nil {
			return 0, false
		}
		return val, true
	}
	if id, ok := expr.(*ast.Ident); ok {
		if val, ok := l.lookupConst(id.Name); ok {
			return int(val), true
		}
	}
	return 0, false
}

// emitVariableIndexRead uses a counter-walk to load from a dynamic array index.
func (l *Lowerer) emitVariableIndexRead(arr arrayInfo, indexCell, result Cell) {
	baseSlot := slotOf(arr.base)
	l.emit(&IRDynLoad{Dst: result, BaseSlot: baseSlot, Index: indexCell})
}

// emitVariableIndexWrite uses a counter-walk to store to a dynamic array index.
func (l *Lowerer) emitVariableIndexWrite(arr arrayInfo, indexCell, valCell Cell) {
	baseSlot := slotOf(arr.base)
	l.emit(&IRDynStore{BaseSlot: baseSlot, Index: indexCell, Src: valCell})
}

func (l *Lowerer) lowerIncDec(s *ast.IncDecStmt) error {
	var cell Cell
	switch x := s.X.(type) {
	case *ast.Ident:
		// Multi-byte integer inc/dec.
		if base, ok := l.lookupIntCell(x.Name); ok {
			n := l.lookupIntVarSize(x.Name)
			if s.Tok == token.INC {
				l.emitIncInt(base, n)
			} else {
				l.emitDecInt(base, n)
			}
			return nil
		}
		c, err := l.lookupVar(x.Name)
		if err != nil {
			return err
		}
		cell = c
	case *ast.IndexExpr:
		r, err := l.lowerIndexExpr(x)
		if err != nil {
			return err
		}
		if r.temp {
			// Variable or pointer index: read-modify-write.
			if s.Tok == token.INC {
				l.emit(&IRAddI{Dst: r.cell, Value: 1})
			} else {
				l.emit(&IRSubI{Dst: r.cell, Value: 1})
			}
			base, err := l.lowerExpr(x.X)
			if err != nil {
				return err
			}
			return l.writeInto(base, x.Index, r)
		}
		cell = r.cell
	case *ast.SelectorExpr:
		return l.lowerFieldIncDec(x, s.Tok)
	case *ast.StarExpr:
		return l.lowerDerefIncDec(x.X, s.Tok)
	default:
		return fmt.Errorf("unsupported inc/dec target: %T", s.X)
	}
	if s.Tok == token.INC {
		l.emit(&IRAddI{Dst: cell, Value: 1})
	} else {
		l.emit(&IRSubI{Dst: cell, Value: 1})
	}
	return nil
}

// emitIncInt increments an n-byte integer in place with carry chain.
func (l *Lowerer) emitIncInt(base Cell, n int) {
	// Increment byte 0. If it wrapped to 0, carry to byte 1, etc.
	carry := l.allocCell()
	l.emit(&IRAddI{Dst: base, Value: 1})
	l.emit(&IRCopy{Dst: carry, Src: base})
	l.emit(&IRNot{Dst: carry, Src: carry}) // carry = (base[0] == 0)
	for j := 1; j < n; j++ {
		cond := l.allocCell()
		l.emit(&IRCopy{Dst: cond, Src: carry})
		l.emit(&IRIf{Cond: cond, Then: &IRBlock{Nodes: []IRNode{
			&IRAddI{Dst: base + j, Value: 1},
		}}})
		l.freeCell(cond)
		if j < n-1 {
			// carry = carry AND (base[j] == 0)
			isZero := l.allocCell()
			l.emit(&IRCopy{Dst: isZero, Src: base + j})
			l.emit(&IRNot{Dst: isZero, Src: isZero})
			l.emit(&IRMul{Dst: carry, Src1: carry, Src2: isZero})
			l.freeCell(isZero)
		}
	}
	l.freeCell(carry)
}

// emitDecInt decrements an n-byte integer in place with borrow chain.
func (l *Lowerer) emitDecInt(base Cell, n int) {
	// Decrement byte 0. If it was 0 (wrapped to 255), borrow from byte 1, etc.
	borrow := l.allocCell()
	l.emit(&IRCopy{Dst: borrow, Src: base})
	l.emit(&IRNot{Dst: borrow, Src: borrow}) // borrow = (base[0] == 0)
	l.emit(&IRSubI{Dst: base, Value: 1})
	for j := 1; j < n; j++ {
		cond := l.allocCell()
		l.emit(&IRCopy{Dst: cond, Src: borrow})
		if j < n-1 {
			// Check if this byte is also 0 before decrement (will chain borrow).
			isZero := l.allocCell()
			l.emit(&IRCopy{Dst: isZero, Src: base + j})
			l.emit(&IRNot{Dst: isZero, Src: isZero})
			l.emit(&IRMul{Dst: borrow, Src1: borrow, Src2: isZero})
			l.freeCell(isZero)
		}
		l.emit(&IRIf{Cond: cond, Then: &IRBlock{Nodes: []IRNode{
			&IRSubI{Dst: base + j, Value: 1},
		}}})
		l.freeCell(cond)
	}
	l.freeCell(borrow)
}

// lowerDerefIncDec handles *p++ / *p--: load, modify, store via dynamic access.
func (l *Lowerer) lowerDerefIncDec(ptr ast.Expr, tok token.Token) error {
	p, err := l.lowerExpr(ptr)
	if err != nil {
		return err
	}
	if n := p.ptrIntSize; n >= 2 {
		idx := l.allocCell()
		l.emit(&IRCopy{Dst: idx, Src: p.cell})
		tmp := l.allocCells(n)
		for j := range n {
			val := l.ptrLoad(idx)
			l.emit(&IRMove{Dst: tmp + j, Src: val})
			l.freeCell(val)
			if j < n-1 {
				l.emit(&IRAddI{Dst: idx, Value: 1})
			}
		}
		if tok == token.INC {
			l.emitIncInt(tmp, n)
		} else {
			l.emitDecInt(tmp, n)
		}
		// Store back (idx still points to last byte).
		for j := n - 1; j >= 0; j-- {
			l.ptrStore(idx, tmp+j)
			if j > 0 {
				l.emit(&IRSubI{Dst: idx, Value: 1})
			}
		}
		l.freeCellRange(tmp, n)
		l.freeCell(idx)
		return nil
	}
	t := l.ptrLoad(p.cell)
	if tok == token.INC {
		l.emit(&IRAddI{Dst: t, Value: 1})
	} else {
		l.emit(&IRSubI{Dst: t, Value: 1})
	}
	l.ptrStore(p.cell, t)
	l.freeCell(t)
	if p.temp {
		l.freeCell(p.cell)
	}
	return nil
}

// lowerFieldIncDec handles p.x++ / p.x-- and a[i].x++ / a[i].x--.
func (l *Lowerer) lowerFieldIncDec(sel *ast.SelectorExpr, tok token.Token) error {
	// Variable-indexed struct array field: a[i].x++
	if idx, ok := sel.X.(*ast.IndexExpr); ok {
		if id, ok := idx.X.(*ast.Ident); ok {
			ai, ok := l.lookupArray(id.Name)
			if ok && ai.elemType != "" {
				if _, isConst := l.constValue(idx.Index); !isConst {
					elemDef := l.result.Structs[ai.elemType]
					offset := elemDef.Offsets[sel.Sel.Name]
					baseOffset, err := l.lowerCompositeVarIndex(ai, idx.Index)
					if err != nil {
						return err
					}
					l.emit(&IRAddI{Dst: baseOffset.cell, Value: byte(offset)}) // #nosec G115
					flatArr := flatArrayOf(ai)
					val := l.allocCell()
					l.emitVariableIndexRead(flatArr, baseOffset.cell, val)
					if tok == token.INC {
						l.emit(&IRAddI{Dst: val, Value: 1})
					} else {
						l.emit(&IRSubI{Dst: val, Value: 1})
					}
					l.emitVariableIndexWrite(flatArr, baseOffset.cell, val)
					l.freeCell(val)
					l.freeCell(baseOffset.cell)
					return nil
				}
			}
		}
	}
	// Pointer-based field inc/dec: ptr.x++, s[i].x++ -> load, modify, store
	base, err := l.lowerExpr(sel.X)
	if err == nil && base.isPointer && base.typeName != "" {
		def := l.result.Structs[base.typeName]
		offset := def.Offsets[sel.Sel.Name]
		if n := def.FieldIntSizes[sel.Sel.Name]; n >= 2 {
			idx := l.allocCell()
			l.emit(&IRCopy{Dst: idx, Src: base.cell})
			l.emit(&IRAddI{Dst: idx, Value: byte(offset)}) // #nosec G115
			tmp := l.allocCells(n)
			for j := range n {
				val := l.ptrLoad(idx)
				l.emit(&IRMove{Dst: tmp + j, Src: val})
				l.freeCell(val)
				if j < n-1 {
					l.emit(&IRAddI{Dst: idx, Value: 1})
				}
			}
			if tok == token.INC {
				l.emitIncInt(tmp, n)
			} else {
				l.emitDecInt(tmp, n)
			}
			for j := n - 1; j >= 0; j-- {
				l.ptrStore(idx, tmp+j)
				if j > 0 {
					l.emit(&IRSubI{Dst: idx, Value: 1})
				}
			}
			l.freeCellRange(tmp, n)
			l.freeCell(idx)
			return nil
		}
		idx := l.ptrOffset(base.cell, offset)
		val := l.ptrLoad(idx)
		if tok == token.INC {
			l.emit(&IRAddI{Dst: val, Value: 1})
		} else {
			l.emit(&IRSubI{Dst: val, Value: 1})
		}
		l.ptrStore(idx, val)
		l.freeCell(val)
		l.freeCell(idx)
		return nil
	}
	r, err := l.lowerSelectorExpr(sel)
	if err != nil {
		return err
	}
	if n := r.intSize; n >= 2 {
		if tok == token.INC {
			l.emitIncInt(r.cell, n)
		} else {
			l.emitDecInt(r.cell, n)
		}
		return nil
	}
	if tok == token.INC {
		l.emit(&IRAddI{Dst: r.cell, Value: 1})
	} else {
		l.emit(&IRSubI{Dst: r.cell, Value: 1})
	}
	return nil
}

func (l *Lowerer) lowerIf(s *ast.IfStmt) error {
	if s.Init != nil {
		if err := l.lowerStmt(s.Init); err != nil {
			return err
		}
	}
	cond, err := l.lowerExpr(s.Cond)
	if err != nil {
		return err
	}

	saved := l.nodes
	l.nodes = nil
	if err := l.lowerStmts(s.Body.List); err != nil {
		return err
	}
	thenBlock := &IRBlock{Nodes: l.nodes}

	var elseBlock *IRBlock
	if s.Else != nil {
		l.nodes = nil
		switch e := s.Else.(type) {
		case *ast.BlockStmt:
			if err := l.lowerStmts(e.List); err != nil {
				return err
			}
		case *ast.IfStmt:
			if err := l.lowerIf(e); err != nil {
				return err
			}
		}
		elseBlock = &IRBlock{Nodes: l.nodes}
	}
	l.nodes = saved

	l.emit(&IRIf{Cond: cond.cell, Then: thenBlock, Else: elseBlock})
	if cond.temp {
		l.freeCell(cond.cell)
	}
	return nil
}

// lowerSwitch converts a switch statement to an if-else if chain.
func (l *Lowerer) lowerSwitch(s *ast.SwitchStmt) error {
	if s.Init != nil {
		if err := l.lowerStmt(s.Init); err != nil {
			return err
		}
	}

	// Convert to an if-else if chain and lower that.
	var tagName string
	if s.Tag != nil {
		tagName = "$switch"
		// String tag: store the slice header so case `s == "lit"` compares content.
		if l.isStringExpr(s.Tag) {
			sc := l.currentScope()
			tagSI := l.defineSlice(sc, tagName, 1, "", false, "", 0)
			src, srcTemp, err := l.resolveStringSlice(s.Tag)
			if err != nil {
				return err
			}
			l.emit(&IRCopy{Dst: tagSI.ptr, Src: src.ptr})
			l.emit(&IRCopy{Dst: tagSI.len, Src: src.len})
			l.emit(&IRCopy{Dst: tagSI.cap, Src: src.cap})
			if srcTemp {
				l.freeSliceInfo(src)
			}
		} else {
			// Store tag in a temp variable so case comparisons can reference it.
			r, err := l.lowerExpr(s.Tag)
			if err != nil {
				return err
			}
			if r.intSize >= 2 {
				sc := l.currentScope()
				base := l.defineIntVar(sc, tagName, r.intSize)
				l.emitCopyOrMove(base, exprResult{cell: r.cell, temp: r.temp, size: r.intSize})
			} else {
				tagCell := l.allocCell()
				l.currentScope().defineByte(tagName, tagCell)
				l.emitCopyOrMove(tagCell, r)
			}
		}
	}

	ifStmt := l.buildSwitchIf(s.Body.List, tagName)
	if ifStmt != nil {
		return l.lowerIf(ifStmt)
	}
	return nil
}

// buildSwitchIf converts case clauses into a nested *ast.IfStmt chain.
// tagName is the variable name holding the switch tag ("" for bool switch).
func (*Lowerer) buildSwitchIf(clauses []ast.Stmt, tagName string) *ast.IfStmt {
	type caseEntry struct {
		values []ast.Expr // nil for default
		body   []ast.Stmt
	}
	// Preserve original clause order (including default position) for fallthrough.
	entries := make([]caseEntry, len(clauses))
	defaultIdx := -1
	for i, clause := range clauses {
		cc := clause.(*ast.CaseClause)
		entries[i] = caseEntry{values: cc.List, body: cc.Body}
		if cc.List == nil {
			defaultIdx = i
		}
	}

	// Handle fallthrough: process in reverse so chained fallthroughs resolve.
	for i := len(entries) - 1; i >= 0; i-- {
		if hasFallthrough(entries[i].body) {
			entries[i].body = stripFallthrough(entries[i].body)
			if i+1 < len(entries) {
				entries[i].body = append(entries[i].body, entries[i+1].body...)
			}
		}
	}

	// Separate default from cases.
	var defaultBody []ast.Stmt
	var cases []caseEntry
	for i, e := range entries {
		if i == defaultIdx {
			defaultBody = e.body
		} else {
			cases = append(cases, e)
		}
	}

	if len(cases) == 0 {
		if defaultBody != nil {
			return &ast.IfStmt{
				Cond: &ast.BasicLit{Kind: token.INT, Value: "1"},
				Body: &ast.BlockStmt{List: defaultBody},
			}
		}
		return nil
	}

	var elseStmt ast.Stmt
	if defaultBody != nil {
		elseStmt = &ast.BlockStmt{List: defaultBody}
	}

	for i := len(cases) - 1; i >= 0; i-- {
		c := cases[i]
		var cond ast.Expr
		for _, val := range c.values {
			var match ast.Expr
			if tagName != "" {
				match = &ast.BinaryExpr{
					X:  ast.NewIdent(tagName),
					Op: token.EQL,
					Y:  val,
				}
			} else {
				match = val
			}
			if cond == nil {
				cond = match
			} else {
				cond = &ast.BinaryExpr{X: cond, Op: token.LOR, Y: match}
			}
		}
		elseStmt = &ast.IfStmt{
			Cond: cond,
			Body: &ast.BlockStmt{List: c.body},
			Else: elseStmt,
		}
	}

	if ifNode, ok := elseStmt.(*ast.IfStmt); ok {
		return ifNode
	}
	return nil
}

func hasFallthrough(stmts []ast.Stmt) bool {
	if len(stmts) == 0 {
		return false
	}
	last, ok := stmts[len(stmts)-1].(*ast.BranchStmt)
	return ok && last.Tok == token.FALLTHROUGH
}

func stripFallthrough(stmts []ast.Stmt) []ast.Stmt {
	if len(stmts) == 0 {
		return stmts
	}
	return stmts[:len(stmts)-1]
}

func (l *Lowerer) lowerFor(s *ast.ForStmt) error {
	l.pushScope()
	defer l.popScope()
	if s.Init != nil {
		if err := l.lowerStmt(s.Init); err != nil {
			return err
		}
	}

	condCell := l.allocCell()
	if s.Cond != nil {
		if err := l.emitCondTo(condCell, s.Cond); err != nil {
			return err
		}
	} else {
		l.emit(&IRConst{Dst: condCell, Value: 1})
	}

	// Set up break/continue flags for this loop.
	outerSkip := l.loopSkipFlag
	outerBreak := l.loopBreakFlag
	l.loopSkipFlag = l.allocCell()
	l.loopBreakFlag = l.allocCell()

	saved := l.nodes
	l.nodes = nil

	// Reset flags at start of each iteration.
	l.emit(&IRZero{Dst: l.loopSkipFlag})
	l.emit(&IRZero{Dst: l.loopBreakFlag})
	l.loopDepth++

	// Body statements (guarded by skipFlag).
	if err := l.lowerStmts(s.Body.List); err != nil {
		return err
	}

	l.loopDepth--
	// After body: guard post and condition with !breakFlag.
	// Continue skips body but post/condition still run.
	// Break skips everything.
	l.emit(&IRZero{Dst: l.loopSkipFlag}) // clear skip for continue
	breakGuard := l.allocCell()
	l.emit(&IRNot{Dst: breakGuard, Src: l.loopBreakFlag})

	guardedSaved := l.nodes
	l.nodes = nil
	if s.Post != nil {
		if err := l.lowerStmt(s.Post); err != nil {
			return err
		}
	}
	if s.Cond != nil {
		if err := l.emitCondTo(condCell, s.Cond); err != nil {
			return err
		}
	} else {
		l.emit(&IRConst{Dst: condCell, Value: 1})
	}
	postCondBlock := &IRBlock{Nodes: l.nodes}
	l.nodes = guardedSaved
	l.emit(&IRIf{Cond: breakGuard, Then: postCondBlock})
	l.freeCell(breakGuard)

	// If break or return, exit loop.
	if l.loopBreakFlag != 0 {
		l.emit(&IRIf{
			Cond: l.loopBreakFlag,
			Then: &IRBlock{Nodes: []IRNode{&IRZero{Dst: condCell}}},
		})
	}
	if l.inFunc && l.returnFlag != 0 {
		l.emit(&IRIf{
			Cond: l.returnFlag,
			Then: &IRBlock{Nodes: []IRNode{&IRZero{Dst: condCell}}},
		})
	}

	body := &IRBlock{Nodes: l.nodes}
	l.nodes = saved

	l.emit(&IRLoop{Cond: condCell, Body: body})

	l.freeCell(l.loopBreakFlag)
	l.freeCell(l.loopSkipFlag)
	l.loopSkipFlag = outerSkip
	l.loopBreakFlag = outerBreak
	l.freeCell(condCell)
	return nil
}

func (l *Lowerer) emitCondTo(dst Cell, expr ast.Expr) error {
	r, err := l.lowerExpr(expr)
	if err != nil {
		return err
	}
	l.emitCopyOrMove(dst, r)
	return nil
}

func (l *Lowerer) lowerRange(s *ast.RangeStmt) error {
	l.pushScope()
	defer l.popScope()
	l.declareFromRange(s)
	var cell Cell
	var counterIntSize int // 0 for byte, >= 2 for multi-byte integers
	if s.Key != nil {
		id, ok := s.Key.(*ast.Ident)
		if !ok {
			return fmt.Errorf("unsupported range key: %T", s.Key)
		}
		if base, ok := l.lookupIntCell(id.Name); ok {
			cell = base
			counterIntSize = l.lookupIntVarSize(id.Name)
		} else {
			var err error
			cell, err = l.lookupVar(id.Name)
			if err != nil {
				return err
			}
		}
	} else {
		// No loop variable: allocate a hidden counter.
		// Check if range expression is multi-byte to size the counter.
		if l.exprInvolvesInt(s.X, l.currentScope()) {
			n := l.exprIntSize(s.X, l.currentScope())
			counterIntSize = n
			cell = l.allocCells(n)
			defer l.freeCellRange(cell, n)
		} else {
			cell = l.allocCell()
			defer l.freeCell(cell)
		}
	}

	// Check if ranging over an array or slice: for i, v := range x
	var valCell Cell
	var valSliceCells []Cell // for slice-element range values: [ptr, len, cap]
	var rangeBase exprResult
	var hasVal bool
	if s.Value != nil {
		r, err := l.lowerExpr(s.X)
		if err != nil {
			// String/slice literal as range source: materialize to a slice header
			// and synthesize a pointer-shape exprResult that mirrors the
			// element layout of the materialized slice.
			if si, sliceErr := l.lowerSliceExpr(s.X); sliceErr == nil {
				r = exprResult{
					cell: si.ptr, lenCell: si.len, capCell: si.cap,
					elemSize: max(si.elemSize, 1), elemCount: 255,
					elemSlice: si.elemSlice, isPointer: true,
				}
				err = nil
			}
		}
		if err == nil && r.elemCount > 0 {
			rangeBase = r
			hasVal = true
			valID, ok := s.Value.(*ast.Ident)
			if !ok {
				return fmt.Errorf("unsupported range value: %T", s.Value)
			}
			if si, ok := l.lookupStruct(valID.Name); ok {
				valCell = si.base
			} else if base, ok := l.lookupIntCell(valID.Name); ok {
				valCell = base
			} else if si, ok := l.lookupSlice(valID.Name); ok {
				// `[]string` / `[][]byte` element: v is bound to a 3-cell
				// slice header whose cells need not be contiguous.
				valSliceCells = []Cell{si.ptr, si.len, si.cap}
			} else {
				valCell, _ = l.lookupVar(valID.Name)
			}
		}
	} else if id, ok := s.X.(*ast.Ident); ok {
		// Plain `for range slice` / `for range array` uses len as the iteration
		// count. Pre-evaluate the source so the limit logic below picks up
		// lenCell or elemCount.
		if _, ok := l.lookupSlice(id.Name); ok {
			r, err := l.lowerExpr(s.X)
			if err == nil {
				rangeBase = r
			}
		} else if _, ok := l.lookupArray(id.Name); ok {
			r, err := l.lowerExpr(s.X)
			if err == nil {
				rangeBase = r
			}
		}
	}

	// Evaluate the range limit.
	var limit exprResult
	var err error
	if rangeBase.lenCell != 0 {
		t := l.allocCell()
		l.emit(&IRCopy{Dst: t, Src: rangeBase.lenCell})
		limit = exprResult{cell: t, temp: true}
	} else if rangeBase.elemCount > 0 && rangeBase.elemCount != 255 {
		t := l.allocCell()
		l.emit(&IRConst{Dst: t, Value: byte(rangeBase.elemCount)}) // #nosec G115
		limit = exprResult{cell: t, temp: true}
	} else {
		limit, err = l.lowerExpr(s.X)
		if err != nil {
			return err
		}
	}
	// i = 0
	if counterIntSize >= 2 {
		for j := range counterIntSize {
			l.emit(&IRZero{Dst: cell + j})
		}
	} else {
		l.emit(&IRZero{Dst: cell})
	}
	// Desugar to for loop: condition is i < limit.
	condCell := l.allocCell()
	if counterIntSize >= 2 {
		l.emitCmpLtInt(condCell, cell, limit.cell, counterIntSize)
	} else {
		l.emit(&IRCmp{Op: CmpLt, Dst: condCell, Src1: cell, Src2: limit.cell})
	}

	outerSkip := l.loopSkipFlag
	outerBreak := l.loopBreakFlag
	l.loopSkipFlag = l.allocCell()
	l.loopBreakFlag = l.allocCell()

	saved := l.nodes
	l.nodes = nil

	l.emit(&IRZero{Dst: l.loopSkipFlag})
	l.emit(&IRZero{Dst: l.loopBreakFlag})
	l.loopDepth++

	// For range over array/slice: load v = x[i] at the start of each iteration.
	if hasVal {
		if rangeBase.isPointer {
			es := rangeBase.elemSize
			idx := l.allocCell()
			if es == 1 {
				l.emit(&IRAdd{Dst: idx, Src1: rangeBase.cell, Src2: cell})
			} else {
				t := l.allocCell()
				l.emit(&IRConst{Dst: t, Value: byte(es)}) // #nosec G115
				l.emit(&IRMul{Dst: idx, Src1: cell, Src2: t})
				l.freeCell(t)
				l.emit(&IRAdd{Dst: idx, Src1: rangeBase.cell, Src2: idx})
			}
			for j := range es {
				result := l.ptrLoad(idx)
				dst := valCell + Cell(j) // #nosec G115
				if valSliceCells != nil {
					dst = valSliceCells[j]
				}
				l.emit(&IRMove{Dst: dst, Src: result})
				l.freeCell(result)
				if j < es-1 {
					l.emit(&IRAddI{Dst: idx, Value: 1})
				}
			}
			l.freeCell(idx)
		} else if rangeBase.elemSize > 1 {
			// Multi-cell element (uint16/uint32/uint64, struct, or nested array).
			// Read elemSize bytes per iteration via flat indexing into the array.
			es := rangeBase.elemSize
			ai := arrayInfo{base: rangeBase.cell, size: rangeBase.elemCount * es, count: rangeBase.elemCount * es, elemSize: 1}
			esCell := l.allocCell()
			l.emit(&IRConst{Dst: esCell, Value: byte(es)}) // #nosec G115
			flatIdx := l.allocCell()
			l.emit(&IRMul{Dst: flatIdx, Src1: cell, Src2: esCell})
			l.freeCell(esCell)
			dsts := make([]Cell, es)
			for j := range es {
				if valSliceCells != nil {
					dsts[j] = valSliceCells[j]
				} else {
					dsts[j] = valCell + Cell(j) // #nosec G115
				}
			}
			l.loadConsecutiveViaIndex(ai, flatIdx, dsts)
			l.freeCell(flatIdx)
		} else {
			ai := arrayInfo{base: rangeBase.cell, size: rangeBase.elemCount, count: rangeBase.elemCount, elemSize: 1}
			l.emitVariableIndexRead(ai, cell, valCell)
		}
	}

	if err := l.lowerStmts(s.Body.List); err != nil {
		return err
	}

	l.loopDepth--
	// Clear skipFlag for continue.
	l.emit(&IRZero{Dst: l.loopSkipFlag})

	// Post: i++ (guarded by !breakFlag).
	breakGuard := l.allocCell()
	l.emit(&IRNot{Dst: breakGuard, Src: l.loopBreakFlag})
	guardedSaved := l.nodes
	l.nodes = nil
	if counterIntSize >= 2 {
		l.emitIncInt(cell, counterIntSize)
		l.emitCmpLtInt(condCell, cell, limit.cell, counterIntSize)
	} else {
		l.emit(&IRAddI{Dst: cell, Value: 1})
		l.emit(&IRCmp{Op: CmpLt, Dst: condCell, Src1: cell, Src2: limit.cell})
	}
	postBlock := &IRBlock{Nodes: l.nodes}
	l.nodes = guardedSaved
	l.emit(&IRIf{Cond: breakGuard, Then: postBlock})
	l.freeCell(breakGuard)

	// Exit on break or return.
	l.emit(&IRIf{
		Cond: l.loopBreakFlag,
		Then: &IRBlock{Nodes: []IRNode{&IRZero{Dst: condCell}}},
	})
	if l.inFunc && l.returnFlag != 0 {
		l.emit(&IRIf{
			Cond: l.returnFlag,
			Then: &IRBlock{Nodes: []IRNode{&IRZero{Dst: condCell}}},
		})
	}

	body := &IRBlock{Nodes: l.nodes}
	l.nodes = saved

	l.emit(&IRLoop{Cond: condCell, Body: body})

	l.freeCell(l.loopBreakFlag)
	l.freeCell(l.loopSkipFlag)
	l.loopSkipFlag = outerSkip
	l.loopBreakFlag = outerBreak
	l.freeCell(condCell)
	if limit.temp {
		l.freeCellRange(limit.cell, max(limit.intSize, 1))
	}
	return nil
}

func (l *Lowerer) lowerBranch(s *ast.BranchStmt) error {
	switch s.Tok {
	case token.BREAK:
		if l.loopSkipFlag == 0 {
			return fmt.Errorf("break outside loop")
		}
		l.emit(&IRConst{Dst: l.loopSkipFlag, Value: 1})
		l.emit(&IRConst{Dst: l.loopBreakFlag, Value: 1})
		return nil
	case token.CONTINUE:
		if l.loopSkipFlag == 0 {
			return fmt.Errorf("continue outside loop")
		}
		l.emit(&IRConst{Dst: l.loopSkipFlag, Value: 1})
		return nil
	default:
		return fmt.Errorf("unsupported branch statement: %s", s.Tok)
	}
}

func (l *Lowerer) lowerReturn(s *ast.ReturnStmt) error {
	if !l.inFunc {
		return fmt.Errorf("return outside function")
	}

	// Check for tail call: return f(args...) where f is the current tail-call target.
	if l.tailCallFunc != "" && len(s.Results) == 1 {
		if call, ok := s.Results[0].(*ast.CallExpr); ok {
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == l.tailCallFunc {
				return l.lowerTailCall(call)
			}
		}
	}

	// Bare return: named return values are already in returnDst cells.
	if len(s.Results) == 0 {
		return l.returnFinish()
	}

	if len(s.Results) == 1 {
		result := s.Results[0]
		// Struct/array variable.
		if id, ok := result.(*ast.Ident); ok {
			if si, ok := l.lookupStruct(id.Name); ok {
				return l.returnComposite(si.base, si.def.Size)
			}
			if ai, ok := l.lookupArray(id.Name); ok {
				return l.returnComposite(ai.base, ai.size)
			}
		}
		// Slice, composite literal, or scalar via lowerExpr.
		r, err := l.lowerExpr(result)
		if err != nil {
			// Slice composite literal: return []byte{...}.
			if si, err := l.lowerSliceExpr(result); err == nil {
				l.emitCopyOrMove(l.returnDst[0], exprResult{cell: si.ptr, temp: true})
				l.emitCopyOrMove(l.returnDst[1], exprResult{cell: si.len, temp: true})
				l.emitCopyOrMove(l.returnDst[2], exprResult{cell: si.cap, temp: true})
				return l.returnFinish()
			}
			// Struct/array composite literal.
			base, size, err := l.resolveStructArg(result)
			if err != nil {
				return err
			}
			return l.returnComposite(base, size)
		}
		if r.lenCell != 0 {
			l.emitCopyOrMove(l.returnDst[0], exprResult{cell: r.cell, temp: r.temp})
			l.emitCopyOrMove(l.returnDst[1], exprResult{cell: r.lenCell, temp: r.temp})
			l.emitCopyOrMove(l.returnDst[2], exprResult{cell: r.capCell, temp: r.temp})
			return l.returnFinish()
		}
		if r.intSize >= 2 && len(l.returnDst) < 2 {
			return fmt.Errorf("cannot return wider integer from byte-returning function, use byte() to truncate")
		}
		l.emitCopyOrMove(l.returnDst[0], r)
		return l.returnFinish()
	}
	// Fuse return a/b, a%b or return a%b, a/b into IRDivMod.
	if len(s.Results) == 2 {
		if fused, err := l.tryReturnDivMod(s.Results); err != nil {
			return err
		} else if fused {
			return l.returnFinish()
		}
	}
	off := 0
	for _, expr := range s.Results {
		r, err := l.lowerExpr(expr)
		if err != nil {
			// String/slice literal in return position: write the 3-cell header.
			si, sliceErr := l.lowerSliceExpr(expr)
			if sliceErr != nil {
				return err
			}
			l.emitCopyOrMove(l.returnDst[off], exprResult{cell: si.ptr, temp: true})
			l.emitCopyOrMove(l.returnDst[off+1], exprResult{cell: si.len, temp: true})
			l.emitCopyOrMove(l.returnDst[off+2], exprResult{cell: si.cap, temp: true})
			off += 3
			continue
		}
		if r.lenCell != 0 {
			// String/slice variable or expression: copy 3 cells.
			l.emitCopyOrMove(l.returnDst[off], exprResult{cell: r.cell, temp: r.temp})
			l.emitCopyOrMove(l.returnDst[off+1], exprResult{cell: r.lenCell, temp: r.temp})
			l.emitCopyOrMove(l.returnDst[off+2], exprResult{cell: r.capCell, temp: r.temp})
			off += 3
			continue
		}
		l.emitCopyOrMove(l.returnDst[off], r)
		n := 1
		if r.intSize >= 2 {
			n = r.intSize
		}
		off += n
	}
	return l.returnFinish()
}

func (l *Lowerer) lowerTailCall(call *ast.CallExpr) error {
	info := l.result.Funcs[l.tailCallFunc]

	// Evaluate all arguments into temporaries first. For composite args
	// (struct/array), resolve their base cell and size, then copy to
	// temps to avoid overwriting source params during assignment.
	type argVal struct {
		cell Cell
		base Cell // non-zero for composite args
		size int
	}
	vals := make([]argVal, len(call.Args))
	for i, arg := range call.Args {
		if i < len(info.ParamTypes) {
			pt := info.ParamTypes[i]
			if pt.StructType != "" {
				base, size, err := l.resolveStructArg(arg)
				if err != nil {
					return err
				}
				tmp := l.allocCells(size)
				for j := range size {
					l.emit(&IRCopy{Dst: tmp + j, Src: base + j})
				}
				vals[i] = argVal{base: tmp, size: size}
				continue
			}
			if pt.ArraySize > 0 {
				if id, ok := arg.(*ast.Ident); ok {
					if ai, ok := l.lookupArray(id.Name); ok {
						tmp := l.allocCells(ai.size)
						for j := range ai.size {
							l.emit(&IRCopy{Dst: tmp + j, Src: ai.base + j})
						}
						vals[i] = argVal{base: tmp, size: ai.size}
						continue
					}
				}
				if comp, ok := arg.(*ast.CompositeLit); ok {
					size := l.arraySize(comp.Type)
					base := l.allocCells(size)
					for j := range size {
						l.emit(&IRZero{Dst: base + j})
					}
					arr := arrayInfo{base: base, size: size, count: size, elemSize: 1}
					if err := l.lowerCompositeLitInto(arr, comp); err != nil {
						return err
					}
					vals[i] = argVal{base: base, size: size}
					continue
				}
			}
		}
		r, err := l.lowerExpr(arg)
		if err != nil {
			return err
		}
		vals[i] = argVal{cell: l.ensureTemp(r).cell}
	}

	// Move evaluated values to param cells.
	for i, paramName := range info.Params {
		if vals[i].size > 0 {
			// Composite param: look up by struct or array base.
			var paramBase Cell
			if si, ok := l.lookupStruct(paramName); ok {
				paramBase = si.base
			} else if ai, ok := l.lookupArray(paramName); ok {
				paramBase = ai.base
			}
			for j := range vals[i].size {
				l.emit(&IRMove{Dst: paramBase + j, Src: vals[i].base + j})
			}
		} else {
			paramCell, _ := l.lookupVar(paramName)
			l.emit(&IRMove{Dst: paramCell, Src: vals[i].cell})
		}
	}

	// Signal the tail-call loop to restart.
	l.emit(&IRConst{Dst: l.tailCallFlag, Value: 1})
	l.emit(&IRConst{Dst: l.returnFlag, Value: 1}) // skip remaining stmts
	return nil
}

func (l *Lowerer) tryReturnDivMod(results []ast.Expr) (bool, error) {
	a, aOk := results[0].(*ast.BinaryExpr)
	b, bOk := results[1].(*ast.BinaryExpr)
	if !aOk || !bOk {
		return false, nil
	}
	var divExpr, modExpr *ast.BinaryExpr
	var quotIdx, remIdx int
	if a.Op == token.QUO && b.Op == token.REM {
		divExpr, modExpr = a, b
		quotIdx, remIdx = 0, 1
	} else if a.Op == token.REM && b.Op == token.QUO {
		modExpr, divExpr = a, b
		remIdx, quotIdx = 0, 1
	} else {
		return false, nil
	}
	if !sameExpr(divExpr.X, modExpr.X) || !sameExpr(divExpr.Y, modExpr.Y) {
		return false, nil
	}
	src1, err := l.lowerExpr(divExpr.X)
	if err != nil {
		return false, err
	}
	src2, err := l.lowerExpr(divExpr.Y)
	if err != nil {
		return false, err
	}
	src2 = l.ensureTemp(src2)
	if src1.intSize >= 2 {
		// Multi-byte: compute offsets into returnDst by return sizes.
		quotOff, remOff := 0, src1.intSize
		if quotIdx > remIdx {
			quotOff, remOff = src1.intSize, 0
		}
		l.emitDivModIntFused(l.returnDst[quotOff], l.returnDst[remOff], src1.cell, src2.cell, src1.intSize)
	} else {
		l.emit(&IRDivMod{QuotDst: l.returnDst[quotIdx], RemDst: l.returnDst[remIdx], Src1: src1.cell, Src2: src2.cell})
	}
	if src1.temp {
		l.freeCell(src1.cell)
	}
	l.freeCell(src2.cell)
	return true, nil
}

func (l *Lowerer) returnComposite(base Cell, size int) error {
	for j := range size {
		l.emit(&IRMove{Dst: l.returnDst[j], Src: base + j})
	}
	return l.returnFinish()
}

func (l *Lowerer) returnFinish() error {
	l.emit(&IRConst{Dst: l.returnFlag, Value: 1})
	if l.tailCallFlag != 0 {
		l.emit(&IRZero{Dst: l.tailCallFlag})
	}
	return nil
}

func (l *Lowerer) lowerDefer(s *ast.DeferStmt) error {
	if l.loopDepth > 0 {
		return fmt.Errorf("defer inside a loop is not supported")
	}

	// Go semantics: defer args are evaluated immediately and captured.
	// A flag cell tracks whether the defer was registered (for defers inside if/switch).
	// Use a fresh cell (not recycled) so the stack slot starts at 0 from BF initialization.
	// Non-matching branches never write to this slot, so it stays 0 (defer skipped).
	flag := l.allocCells(1)
	l.emit(&IRConst{Dst: flag, Value: 1})

	capturedArgs := make([]ast.Expr, len(s.Call.Args))
	for i, arg := range s.Call.Args {
		if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			capturedArgs[i] = arg
			continue
		}
		// String constant: pass as a string literal for the deferred call.
		if id, ok := arg.(*ast.Ident); ok && l.lookupStringConst(id.Name) != "" {
			capturedArgs[i] = &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(l.lookupStringConst(id.Name))}
			continue
		}
		// Slice argument: capture the 3-cell header.
		if id, ok := arg.(*ast.Ident); ok {
			if si, ok := l.lookupSlice(id.Name); ok {
				name := fmt.Sprintf("$defer_%d_%d", len(l.deferredCalls), i)
				sc := l.currentScope()
				capSI := l.defineSlice(sc, name, si.elemSize, si.elemType, si.elemSlice, si.elemPtrType, si.elemIntSize)
				l.emit(&IRCopy{Dst: capSI.ptr, Src: si.ptr})
				l.emit(&IRCopy{Dst: capSI.len, Src: si.len})
				l.emit(&IRCopy{Dst: capSI.cap, Src: si.cap})
				capturedArgs[i] = ast.NewIdent(name)
				continue
			}
		}
		r, err := l.lowerExpr(arg)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("$defer_%d_%d", len(l.deferredCalls), i)
		sc := l.currentScope()
		// String-shaped result (e.g. `s + "!"`): capture the 3-cell header.
		if r.lenCell != 0 && r.elemSize == 1 && r.elemType == "" && r.elemIntSize == 0 && !r.elemSlice {
			capSI := l.defineSlice(sc, name, 1, "", false, "", 0)
			l.emit(&IRCopy{Dst: capSI.ptr, Src: r.cell})
			l.emit(&IRCopy{Dst: capSI.len, Src: r.lenCell})
			l.emit(&IRCopy{Dst: capSI.cap, Src: r.capCell})
			if r.temp {
				l.freeCell(r.cell)
				l.freeCell(r.lenCell)
				l.freeCell(r.capCell)
			}
			capturedArgs[i] = ast.NewIdent(name)
			continue
		}
		if r.intSize >= 2 {
			base := l.allocCells(r.intSize)
			l.emitCopyOrMove(base, exprResult{cell: r.cell, temp: r.temp, size: r.intSize})
			sc[name] = &intBinding{base: base, size: r.intSize}
		} else {
			cell := l.allocCell()
			l.emitCopyOrMove(cell, r)
			sc.defineByte(name, cell)
		}
		capturedArgs[i] = ast.NewIdent(name)
	}
	// Build the deferred call wrapped in an IRIf guard on the flag.
	deferCall := &ast.CallExpr{
		Fun:  s.Call.Fun,
		Args: capturedArgs,
	}
	saved := l.nodes
	l.nodes = nil
	if err := l.lowerCallStmt(deferCall); err != nil {
		l.nodes = saved
		return err
	}
	body := &IRBlock{Nodes: l.nodes}
	l.nodes = saved
	// The deferred block checks the flag before executing.
	block := &IRBlock{Nodes: []IRNode{&IRIf{Cond: flag, Then: body}}}
	l.deferredCalls = append(l.deferredCalls, block)
	return nil
}

// emitDeferred emits all deferred calls in LIFO order.
func (l *Lowerer) emitDeferred() {
	for i := len(l.deferredCalls) - 1; i >= 0; i-- {
		for _, node := range l.deferredCalls[i].Nodes {
			l.emit(node)
		}
	}
}

// resolveStructArg evaluates a struct argument expression, returning
// the base cell and size. Handles variables, indexed elements,
// composite literals, and function calls.
func (l *Lowerer) resolveStructArg(expr ast.Expr) (Cell, int, error) {
	// Composite literal: must be handled before lowerExpr.
	if comp, ok := expr.(*ast.CompositeLit); ok {
		if def := l.structDef(comp.Type); def != nil {
			base := l.allocCells(def.Size)
			for j := range def.Size {
				l.emit(&IRZero{Dst: base + j})
			}
			if err := l.lowerStructValueTo(base, def, comp); err != nil {
				return 0, 0, err
			}
			return base, def.Size, nil
		}
		if size := l.arraySize(comp.Type); size > 0 {
			base := l.allocCells(size)
			arr := arrayInfo{base: base, size: size, count: size, elemSize: 1}
			if err := l.lowerCompositeLitInto(arr, comp); err != nil {
				return 0, 0, err
			}
			return base, size, nil
		}
		return 0, 0, fmt.Errorf("unsupported composite literal argument")
	}
	r, err := l.lowerExpr(expr)
	if err != nil {
		return 0, 0, err
	}
	// Pointer-based composite: materialize into contiguous temp cells.
	if r.isPointer && r.elemCount > 1 && !r.elemSlice {
		base := l.allocCells(r.elemCount)
		for j := range r.elemCount {
			val := l.ptrLoad(r.cell)
			l.emit(&IRMove{Dst: base + j, Src: val})
			l.freeCell(val)
			if j < r.elemCount-1 {
				l.emit(&IRAddI{Dst: r.cell, Value: 1})
			}
		}
		l.freeCell(r.cell)
		return base, r.elemCount, nil
	}
	return r.cell, r.cellCount(), nil
}

func (l *Lowerer) inlineCall(info *FuncInfo, argExprs []ast.Expr) ([]Cell, error) {
	if len(argExprs) != len(info.Params) {
		return nil, fmt.Errorf("function %s expects %d arguments, got %d", info.Name, len(info.Params), len(argExprs))
	}
	if info.IsRecursive || info.IsTailRec {
		for _, pt := range info.ParamTypes {
			if pt.IntSize >= 2 {
				return nil, fmt.Errorf("multi-byte integer parameters are not supported in recursive function %s", info.Name)
			}
		}
		if info.ReturnType.IntSize >= 2 {
			return nil, fmt.Errorf("multi-byte integer return values are not supported in recursive function %s", info.Name)
		}
	}
	if info.IsRecursive && !info.IsTailRec {
		return l.lowerGeneralRecursion(info, argExprs)
	}

	// Evaluate all arguments before pushScope.
	args := make([]exprResult, len(argExprs))
	sliceArgs := map[int]sliceInfo{}
	for i, expr := range argExprs {
		// Slice argument: evaluate to sliceInfo for later param copy.
		if i < len(info.ParamTypes) && info.ParamTypes[i].IsSlice {
			tmp, err := l.lowerSliceExpr(expr)
			if err != nil {
				return nil, err
			}
			sliceArgs[i] = tmp
			continue
		}
		// Composite literal: lower into temp cells (lowerExpr doesn't handle these).
		if comp, ok := expr.(*ast.CompositeLit); ok {
			if def := l.structDef(comp.Type); def != nil {
				base := l.allocCells(def.Size)
				for j := range def.Size {
					l.emit(&IRZero{Dst: base + j})
				}
				if err := l.lowerStructValueTo(base, def, comp); err != nil {
					return nil, err
				}
				args[i] = exprResult{cell: base, temp: true, size: def.Size}
				continue
			}
			size := l.arraySize(comp.Type)
			if size > 0 {
				base := l.allocCells(size)
				arr := arrayInfo{base: base, size: size, count: size, elemSize: 1}
				if count, elemSize, elemType, eis, esl, ies, ieis := l.arrayElementInfo(comp.Type); count > 0 && elemSize > 1 {
					arr = arrayInfo{base: base, size: size, count: count, elemSize: elemSize,
						elemType: elemType, elemIntSize: eis, elemSlice: esl, innerElemSize: ies, innerElemIntSize: ieis}
				}
				if err := l.lowerCompositeLitInto(arr, comp); err != nil {
					return nil, err
				}
				args[i] = exprResult{cell: base, temp: true, size: size}
				continue
			}
		}
		r, err := l.lowerExpr(expr)
		if err != nil {
			return nil, err
		}
		// Pointer-based composite: materialize into contiguous temp cells, unless
		// the parameter itself wants a pointer (e.g. `setName(pg, ...)` where
		// `setName` takes `*Greeter`). In the pointer-param case the byte cell
		// holding the slot index is exactly what the callee expects.
		paramWantsPointer := i < len(info.ParamTypes) && info.ParamTypes[i].IsPointer
		if r.isPointer && r.elemCount >= 1 && r.typeName != "" && !r.elemSlice && !paramWantsPointer {
			base := l.allocCells(r.elemCount)
			// Copy r.cell into a temp index so the loop can bump it without
			// corrupting the source variable (e.g. a `*T` ident may be reused
			// by later code).
			idx := l.allocCell()
			l.emit(&IRCopy{Dst: idx, Src: r.cell})
			for j := range r.elemCount {
				val := l.ptrLoad(idx)
				l.emit(&IRMove{Dst: base + j, Src: val})
				l.freeCell(val)
				if j < r.elemCount-1 {
					l.emit(&IRAddI{Dst: idx, Value: 1})
				}
			}
			l.freeCell(idx)
			if r.temp {
				l.freeCell(r.cell)
			}
			r = exprResult{cell: base, temp: true, size: r.elemCount, typeName: r.typeName}
		}
		// Flat-offset result: materialize into contiguous temp cells.
		if r.flatBase != 0 {
			totalSize := r.elemCount * r.elemSize
			flatArr := arrayInfo{base: r.flatBase, size: totalSize, count: totalSize, elemSize: 1}
			n := r.elemCount
			base := l.allocCells(n)
			dsts := make([]Cell, n)
			for j := range n {
				dsts[j] = base + Cell(j) // #nosec G115
			}
			l.loadConsecutiveViaIndex(flatArr, r.cell, dsts)
			l.freeCell(r.cell)
			r = exprResult{cell: base, temp: true, size: n}
		}
		args[i] = r
	}

	// Push a new scope for the function.
	l.pushScope()

	// Allocate parameter cells and copy arguments.
	for i, paramName := range info.Params {
		if i < len(info.ParamTypes) {
			pt := info.ParamTypes[i]
			if pt.IntSize >= 2 {
				n := pt.IntSize
				sc := l.currentScope()
				base := l.defineIntVar(sc, paramName, n)
				if args[i].intSize >= 2 {
					for j := range n {
						l.emit(&IRCopy{Dst: base + j, Src: args[i].cell + j})
					}
				} else {
					l.emitCopyOrMove(base, args[i])
					args[i].temp = false // already freed by emitCopyOrMove
					for j := 1; j < n; j++ {
						l.emit(&IRZero{Dst: base + j})
					}
				}
				continue
			}
			if pt.IsSlice {
				if inner, ok := sliceArgs[i]; ok {
					sc := l.currentScope()
					paramSI := l.defineSlice(sc, paramName, inner.elemSize, inner.elemType,
						inner.elemSlice, inner.elemPtrType, inner.elemIntSize)
					l.emit(&IRMove{Dst: paramSI.ptr, Src: inner.ptr})
					l.emit(&IRMove{Dst: paramSI.len, Src: inner.len})
					l.emit(&IRMove{Dst: paramSI.cap, Src: inner.cap})
					l.freeSliceInfo(inner)
					continue
				}
			}
			if pt.ArraySize > 0 || pt.StructType != "" {
				var paramBase Cell
				var paramSize int
				if pt.ArraySize > 0 {
					sc := l.currentScope()
					if pt.ArrayElemSize > 1 {
						l.defineStructArray(sc, paramName, pt.ArrayCount, pt.ArrayElemSize, pt.ArrayElemType, pt.ArrayElemIntSize, pt.ArrayElemSlice, 0, 0)
					} else {
						l.defineArray(sc, paramName, pt.ArraySize)
					}
					paramAI, _ := l.lookupArray(paramName)
					paramBase = paramAI.base
					paramSize = pt.ArraySize
				} else {
					def := l.result.Structs[pt.StructType]
					sc := l.currentScope()
					l.defineStruct(sc, paramName, def)
					paramSI, _ := l.lookupStruct(paramName)
					paramBase = paramSI.base
					paramSize = def.Size
				}
				for j := range paramSize {
					l.emit(&IRCopy{Dst: paramBase + j, Src: args[i].cell + j})
				}
				continue
			}
			// Pointer parameter: register pointer type info.
			if pt.IsPointer {
				cell := l.defineVar(paramName)
				l.emit(&IRCopy{Dst: cell, Src: args[i].cell})
				sc := l.currentScope()
				if pt.PtrArrayInfo != nil {
					sc.annotatePtrArray(paramName, arrayInfo{
						size:     pt.PtrArrayInfo.ArraySize,
						count:    pt.PtrArrayInfo.ArrayCount,
						elemSize: pt.PtrArrayInfo.ArrayElemSize,
						elemType: pt.PtrArrayInfo.ArrayElemType,
					})
				}
				if pt.PtrStructType != "" {
					sc.annotatePtrType(paramName, pt.PtrStructType)
				}
				if pt.PtrIntSize >= 2 {
					sc.annotatePtrIntSize(paramName, pt.PtrIntSize)
				}
				continue
			}
		}
		cell := l.defineVar(paramName)
		l.emit(&IRCopy{Dst: cell, Src: args[i].cell})
	}
	for i := range args {
		if args[i].temp {
			l.freeCell(args[i].cell)
		}
	}

	// Allocate return value cells.
	// For composite return types (struct/array), allocate contiguous cells.
	retSize := info.Returns
	if info.ReturnType.IsSlice {
		retSize = 3 // ptr, len, cap
	} else if info.ReturnType.IntSize >= 2 {
		retSize = info.ReturnType.IntSize
	} else if info.ReturnType.ArraySize > 0 && !info.ReturnType.IsPointer {
		retSize = info.ReturnType.ArraySize
	} else if info.ReturnType.StructType != "" && !info.ReturnType.IsPointer {
		retSize = l.result.Structs[info.ReturnType.StructType].Size
	}
	retCells := make([]Cell, retSize)
	if retSize > 1 {
		base := l.allocCells(retSize)
		for i := range retCells {
			retCells[i] = base + i
			l.emit(&IRZero{Dst: retCells[i]})
		}
	} else {
		for i := range retCells {
			retCells[i] = l.allocCell()
			l.emit(&IRZero{Dst: retCells[i]})
		}
	}

	// Register named return variables as aliases for the return cells.
	if len(info.ReturnNames) > 0 {
		sc := l.currentScope()
		if info.ReturnType.IntSize >= 2 && len(info.ReturnNames) == 1 {
			n := info.ReturnNames[0]
			sc[n] = &intBinding{base: retCells[0], size: info.ReturnType.IntSize}
		} else if info.ReturnType.IsSlice && len(info.ReturnNames) == 1 {
			n := info.ReturnNames[0]
			sc[n] = &sliceBinding{info: sliceInfo{
				ptr: retCells[0], len: retCells[1], cap: retCells[2],
				elemSize:    max(info.ReturnType.SliceElemSize, 1),
				elemType:    info.ReturnType.SliceElemType,
				elemSlice:   info.ReturnType.SliceElemSlice,
				elemIntSize: info.ReturnType.SliceElemIntSize,
			}}
		} else {
			for i, name := range info.ReturnNames {
				if i < len(retCells) {
					sc.defineByte(name, retCells[i])
				}
			}
		}
	}

	// Set up return context.
	savedRetDst := l.returnDst
	savedRetFlag := l.returnFlag
	savedInFunc := l.inFunc
	savedTailFunc := l.tailCallFunc
	savedTailFlag := l.tailCallFlag

	l.returnDst = retCells
	if hasReturn(info.Body) {
		l.returnFlag = l.allocCell()
		l.emit(&IRZero{Dst: l.returnFlag})
	} else {
		l.returnFlag = 0
	}
	l.inFunc = true
	savedDefers := l.deferredCalls
	l.deferredCalls = nil

	if info.IsTailRec {
		l.lowerTailRecFunc(info)
	} else {
		if err := l.lowerStmts(info.Body.List); err != nil {
			return nil, err
		}
	}
	l.emitDeferred()

	// Restore context.
	if l.returnFlag != 0 {
		l.freeCell(l.returnFlag)
	}
	l.returnDst = savedRetDst
	l.returnFlag = savedRetFlag
	l.inFunc = savedInFunc
	l.tailCallFunc = savedTailFunc
	l.tailCallFlag = savedTailFlag
	l.deferredCalls = savedDefers

	l.popScope()
	return retCells, nil
}

// lowerTailRecFunc lowers a tail-recursive function by converting to a loop.
func (l *Lowerer) lowerTailRecFunc(info *FuncInfo) {
	// Allocate a tail-call flag.
	tcFlag := l.allocCell()
	l.emit(&IRConst{Dst: tcFlag, Value: 1})

	// Set up tail-call context.
	l.tailCallFunc = info.Name
	l.tailCallFlag = tcFlag

	// Build the loop body.
	saved := l.nodes
	l.nodes = nil
	l.emit(&IRZero{Dst: l.returnFlag}) // reset return flag each iteration
	_ = l.lowerStmts(info.Body.List)
	body := &IRBlock{Nodes: l.nodes}
	l.nodes = saved

	l.emit(&IRLoop{Cond: tcFlag, Body: body})

	l.tailCallFunc = ""
	l.tailCallFlag = 0
	l.freeCell(tcFlag)
}

// Expression lowering.

func (l *Lowerer) lowerExpr(expr ast.Expr) (exprResult, error) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return l.lowerLiteral(e)
	case *ast.Ident:
		return l.lowerIdent(e, l.lookupVar)
	case *ast.ParenExpr:
		return l.lowerExpr(e.X)
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return l.lowerAddressOf(e.X)
		}
		return l.lowerUnary(e, l.lowerExpr)
	case *ast.StarExpr:
		return l.lowerDeref(e.X)
	case *ast.BinaryExpr:
		return l.lowerBinary(e, l.lowerExpr)
	case *ast.CallExpr:
		return l.lowerCallExpr(e)
	case *ast.IndexExpr:
		return l.lowerIndexExpr(e)
	case *ast.SelectorExpr:
		return l.lowerSelectorExpr(e)
	case *ast.SliceExpr:
		si, err := l.evalSliceExpr(e)
		if err != nil {
			return exprResult{}, err
		}
		return exprResult{cell: si.ptr, temp: true, elemSize: si.elemSize,
			elemCount: 255, elemType: si.elemType, elemIntSize: si.elemIntSize,
			elemSlice: si.elemSlice, elemPtrType: si.elemPtrType,
			isPointer: true, lenCell: si.len, capCell: si.cap}, nil
	default:
		return exprResult{}, fmt.Errorf("unsupported expression: %T", expr)
	}
}

func (l *Lowerer) lowerLiteral(e *ast.BasicLit) (exprResult, error) {
	switch e.Kind {
	case token.INT:
		val, err := strconv.ParseUint(e.Value, 0, 64)
		if err != nil {
			return exprResult{}, err
		}
		n := 1
		switch {
		case val > math.MaxUint32:
			n = 8
		case val > math.MaxUint16:
			n = 4
		case val > math.MaxUint8:
			n = 2
		}
		if n == 1 {
			t := l.allocCell()
			l.emit(&IRConst{Dst: t, Value: byte(val)}) // #nosec G115
			return exprResult{cell: t, temp: true}, nil
		}
		base := l.allocCells(n)
		for j := range n {
			l.emit(&IRConst{Dst: base + j, Value: byte(val >> (j * 8))}) // #nosec G115
		}
		return exprResult{cell: base, temp: true, size: n, intSize: n}, nil
	case token.CHAR:
		s, err := strconv.Unquote(e.Value)
		if err != nil {
			return exprResult{}, err
		}
		t := l.allocCell()
		l.emit(&IRConst{Dst: t, Value: s[0]})
		return exprResult{cell: t, temp: true}, nil
	case token.STRING:
		// String literal in expression context: not directly supported as a value.
		return exprResult{}, fmt.Errorf("string literals can only be used with print/println")
	default:
		return exprResult{}, fmt.Errorf("unsupported literal kind: %s", e.Kind)
	}
}

func (l *Lowerer) lowerIdent(e *ast.Ident, lookupVar func(string) (int, error)) (exprResult, error) {
	if s := l.lookupStringConst(e.Name); s != "" {
		// Materialize the constant as a fresh heap-backed slice so it can
		// flow into len/index/slice/concat. The 3-cell header is temp; the
		// caller is expected to free via the lenCell/capCell pattern.
		lit := &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(s)}
		si, err := l.evalStringLiteral(lit)
		if err != nil {
			return exprResult{}, err
		}
		return exprResult{cell: si.ptr, temp: true, elemSize: 1, elemCount: 255,
			isPointer: true, lenCell: si.len, capCell: si.cap}, nil
	}
	if val, ok := l.lookupConst(e.Name); ok {
		t := l.allocCell()
		l.emit(&IRConst{Dst: t, Value: val})
		return exprResult{cell: t, temp: true}, nil
	}
	if val, intSize, ok := l.lookupIntConst(e.Name); ok {
		base := l.allocCells(intSize)
		for j := range intSize {
			l.emit(&IRConst{Dst: base + j, Value: byte(val >> (j * 8))}) // #nosec G115
		}
		return exprResult{cell: base, temp: true, size: intSize, intSize: intSize}, nil
	}
	if e.Name == "nil" {
		t := l.allocCell()
		l.emit(&IRZero{Dst: t})
		return exprResult{cell: t, temp: true}, nil
	}
	cell, err := lookupVar(e.Name)
	if err != nil {
		// Fall back to composite types.
		if base, ok := l.lookupIntCell(e.Name); ok {
			n := l.lookupIntVarSize(e.Name)
			return exprResult{cell: base, size: n, intSize: n}, nil
		}
		if si, ok := l.lookupStruct(e.Name); ok {
			return exprResult{cell: si.base, size: si.def.Size, elemSize: 1,
				elemCount: si.def.Size, typeName: si.def.Name}, nil
		}
		if ai, ok := l.lookupArray(e.Name); ok {
			return exprResult{cell: ai.base, size: ai.size, elemSize: ai.elemSize,
				elemCount: ai.count, elemType: ai.elemType, elemIntSize: ai.elemIntSize,
				elemSlice: ai.elemSlice, innerElemSize: ai.innerElemSize, innerElemIntSize: ai.innerElemIntSize}, nil
		}
		if si, ok := l.lookupSlice(e.Name); ok {
			return exprResult{cell: si.ptr, elemSize: si.elemSize,
				elemCount: 255, elemType: si.elemType, elemIntSize: si.elemIntSize,
				elemSlice: si.elemSlice, elemPtrType: si.elemPtrType,
				isPointer: true, lenCell: si.len, capCell: si.cap}, nil
		}
		return exprResult{}, err
	}
	// Pointer-to-array: return as indexable pointer.
	if ptrAI, ok := l.lookupPtrArray(e.Name); ok {
		return exprResult{cell: cell, elemSize: ptrAI.elemSize,
			elemCount: ptrAI.count, elemType: ptrAI.elemType, isPointer: true}, nil
	}
	// Pointer-to-struct: return as indexable pointer (fields as byte offsets).
	if ptrDef, ok := l.lookupPtrType(e.Name); ok {
		return exprResult{cell: cell, size: ptrDef.Size, elemSize: 1,
			elemCount: ptrDef.Size, typeName: ptrDef.Name, isPointer: true}, nil
	}
	if n := l.lookupPtrIntSize(e.Name); n >= 2 {
		return exprResult{cell: cell, ptrIntSize: n}, nil
	}
	return exprResult{cell: cell}, nil
}

// lowerDerefAssign handles *p = val -- writes val to the stack slot whose index is in p.
func (l *Lowerer) lowerDerefAssign(ptr, rhs ast.Expr) error {
	p, err := l.lowerExpr(ptr)
	if err != nil {
		return err
	}
	r, err := l.lowerExpr(rhs)
	if err != nil {
		return err
	}
	if n := p.ptrIntSize; n >= 2 && r.intSize >= 2 {
		l.lowerDerefAssignInt(p.cell, n, r)
		return nil
	}
	t := l.allocCell()
	l.emitCopyOrMove(t, r)
	l.ptrStore(p.cell, t)
	l.freeCell(t)
	if p.temp {
		l.freeCell(p.cell)
	}
	return nil
}

// lowerDerefAssignInt handles *p = val for multi-byte integer pointers.
func (l *Lowerer) lowerDerefAssignInt(pCell Cell, ptrIntSize int, r exprResult) {
	idx := l.allocCell()
	l.emit(&IRCopy{Dst: idx, Src: pCell})
	for j := range ptrIntSize {
		t := l.allocCell()
		if r.temp {
			l.emit(&IRMove{Dst: t, Src: r.cell + j})
		} else {
			l.emit(&IRCopy{Dst: t, Src: r.cell + j})
		}
		l.ptrStore(idx, t)
		l.freeCell(t)
		if j < ptrIntSize-1 {
			l.emit(&IRAddI{Dst: idx, Value: 1})
		}
	}
	l.freeCell(idx)
}

// ptrLoad reads a byte from the stack slot whose index is in idx.
func (l *Lowerer) ptrLoad(idx Cell) Cell {
	result := l.allocCell()
	l.emit(&IRDynLoad{Dst: result, BaseSlot: 0, Index: idx})
	return result
}

// ptrStore writes val to the stack slot whose index is in idx.
func (l *Lowerer) ptrStore(idx, val Cell) {
	l.emit(&IRDynStore{BaseSlot: 0, Index: idx, Src: val})
}

// ptrOffset returns a temp cell holding ptr + offset.
func (l *Lowerer) ptrOffset(ptr Cell, offset int) Cell {
	idx := l.allocCell()
	l.emit(&IRCopy{Dst: idx, Src: ptr})
	if offset > 0 {
		l.emit(&IRAddI{Dst: idx, Value: byte(offset)}) // #nosec G115
	}
	return idx
}

// loadConsecutiveViaPtr loads n consecutive bytes from heap[*idx],
// heap[*idx+1], ..., heap[*idx+n-1] into the cells dsts[0..n-1]. idx
// is bumped in place and freed.
func (l *Lowerer) loadConsecutiveViaPtr(idx Cell, dsts []Cell) {
	for j := range len(dsts) {
		v := l.ptrLoad(idx)
		l.emit(&IRMove{Dst: dsts[j], Src: v})
		l.freeCell(v)
		if j < len(dsts)-1 {
			l.emit(&IRAddI{Dst: idx, Value: 1})
		}
	}
	l.freeCell(idx)
}

// loadStringHeaderViaPtr loads ptr/len/cap from three consecutive heap
// slots starting at *idx into a fresh sliceInfo and frees idx. The idx
// cell is mutated in place. Returns an exprResult shaped like a
// string-producing expression so callers (string-field reads via a
// pointer, struct-array variable index) can return it directly.
func (l *Lowerer) loadStringHeaderViaPtr(idx Cell) exprResult {
	si := l.allocSliceInfo()
	l.loadConsecutiveViaPtr(idx, []Cell{si.ptr, si.len, si.cap})
	return exprResult{cell: si.ptr, temp: true, elemSize: 1, elemCount: 255,
		isPointer: true, lenCell: si.len, capCell: si.cap}
}

// loadMultiByteIntViaPtr loads n bytes from heap[*idx]..heap[*idx+n-1]
// into a fresh contiguous block. idx is bumped in place and freed.
// Returns an int-shaped exprResult ready for a multi-byte arithmetic
// or comparison helper.
func (l *Lowerer) loadMultiByteIntViaPtr(idx Cell, n int) exprResult {
	base := l.allocCells(n)
	dsts := make([]Cell, n)
	for j := range n {
		dsts[j] = base + Cell(j) // #nosec G115
	}
	l.loadConsecutiveViaPtr(idx, dsts)
	return exprResult{cell: base, temp: true, size: n, intSize: n}
}

// loadConsecutiveViaIndex reads len(dsts) bytes from a flat-array
// element at rowIdx + j for j in 0..len(dsts)-1, into dsts[j].
// rowIdx is read but not freed -- caller manages its lifetime.
func (l *Lowerer) loadConsecutiveViaIndex(flatArr arrayInfo, rowIdx Cell, dsts []Cell) {
	for j := range len(dsts) {
		idxCell := l.allocCell()
		l.emit(&IRCopy{Dst: idxCell, Src: rowIdx})
		if j > 0 {
			l.emit(&IRAddI{Dst: idxCell, Value: byte(j)}) // #nosec G115
		}
		l.emitVariableIndexRead(flatArr, idxCell, dsts[j])
		l.freeCell(idxCell)
	}
}

// storeConsecutiveViaIndex writes len(srcs) bytes into a flat-array
// element at rowIdx + j for j in 0..len(srcs)-1, taking srcs[j].
// rowIdx is read but not freed -- caller manages its lifetime.
func (l *Lowerer) storeConsecutiveViaIndex(flatArr arrayInfo, rowIdx Cell, srcs []Cell) {
	for j := range len(srcs) {
		idxCell := l.allocCell()
		l.emit(&IRCopy{Dst: idxCell, Src: rowIdx})
		if j > 0 {
			l.emit(&IRAddI{Dst: idxCell, Value: byte(j)}) // #nosec G115
		}
		l.emitVariableIndexWrite(flatArr, idxCell, srcs[j])
		l.freeCell(idxCell)
	}
}

// storeConsecutiveViaPtr writes the values of srcs to consecutive heap
// slots starting at *slot. slot is bumped in place and freed. Sources
// are read non-destructively (IRCopy); use a per-cell IRMove if the
// caller wants to consume the sources.
func (l *Lowerer) storeConsecutiveViaPtr(slot Cell, srcs []Cell) {
	for j := range len(srcs) {
		t := l.allocCell()
		l.emit(&IRCopy{Dst: t, Src: srcs[j]})
		l.ptrStore(slot, t)
		l.freeCell(t)
		if j < len(srcs)-1 {
			l.emit(&IRAddI{Dst: slot, Value: 1})
		}
	}
	l.freeCell(slot)
}

// storeStringHeaderViaPtr writes the three header cells of src to three
// consecutive heap slots starting at *slot. The slot cell is mutated
// in place and freed.
func (l *Lowerer) storeStringHeaderViaPtr(slot Cell, src sliceInfo) {
	l.storeConsecutiveViaPtr(slot, []Cell{src.ptr, src.len, src.cap})
}

// ptrDynIndex returns a temp cell holding ptr + indexExpr * elemSize.
func (l *Lowerer) ptrDynIndex(ptr Cell, indexExpr ast.Expr, elemSize int) (Cell, error) {
	idx := l.allocCell()
	if constI, ok := l.constValue(indexExpr); ok {
		l.emit(&IRCopy{Dst: idx, Src: ptr})
		if constI*elemSize > 0 {
			l.emit(&IRAddI{Dst: idx, Value: byte(constI * elemSize)}) // #nosec G115
		}
		return idx, nil
	}
	// Pre-evaluate the index expression into idx. This stores the
	// result to idx's stack slot, preventing register cache conflicts
	// when ptr is later loaded for the addition.
	idxR, err := l.lowerExpr(indexExpr)
	if err != nil {
		return 0, err
	}
	if idxR.intSize >= 2 {
		if idxR.temp {
			l.freeCellRange(idxR.cell, idxR.intSize)
		}
		return 0, fmt.Errorf("cannot use multi-byte integer as array index, use byte() to truncate")
	}
	l.emitCopyOrMove(idx, idxR)
	if elemSize > 1 {
		es := l.allocCell()
		l.emit(&IRConst{Dst: es, Value: byte(elemSize)}) // #nosec G115
		l.emit(&IRMul{Dst: idx, Src1: idx, Src2: es})
		l.freeCell(es)
	}
	// Copy ptr to a fresh temp and add, ensuring ptr's value is
	// loaded from stack AFTER expression evaluation (not stale in cache).
	ptrTemp := l.allocCell()
	l.emit(&IRCopy{Dst: ptrTemp, Src: ptr})
	l.emit(&IRAdd{Dst: idx, Src1: idx, Src2: ptrTemp})
	l.freeCell(ptrTemp)
	// Note: idxR.cell is already freed by emitCopyOrMove above.
	return idx, nil
}

// lowerAddressOf handles &x, &a[i], &p.x -- returns the stack slot index as a byte.
func (l *Lowerer) lowerAddressOf(expr ast.Expr) (exprResult, error) {
	switch e := expr.(type) {
	case *ast.Ident:
		if si, ok := l.lookupStruct(e.Name); ok {
			t := l.allocCell()
			l.emit(&IRConst{Dst: t, Value: byte(slotOf(si.base))}) // #nosec G115
			return exprResult{cell: t, temp: true}, nil
		}
		if ai, ok := l.lookupArray(e.Name); ok {
			t := l.allocCell()
			l.emit(&IRConst{Dst: t, Value: byte(slotOf(ai.base))}) // #nosec G115
			return exprResult{cell: t, temp: true}, nil
		}
		if base, ok := l.lookupIntCell(e.Name); ok {
			t := l.allocCell()
			l.emit(&IRConst{Dst: t, Value: byte(slotOf(base))}) // #nosec G115
			return exprResult{cell: t, temp: true, ptrIntSize: l.lookupIntVarSize(e.Name)}, nil
		}
		cell, err := l.lookupVar(e.Name)
		if err != nil {
			return exprResult{}, err
		}
		t := l.allocCell()
		l.emit(&IRConst{Dst: t, Value: byte(slotOf(cell))}) // #nosec G115
		return exprResult{cell: t, temp: true}, nil
	case *ast.IndexExpr:
		id, ok := e.X.(*ast.Ident)
		if !ok {
			return exprResult{}, fmt.Errorf("cannot take address of chained index expression")
		}
		// &s[i] on slice: return ptr + i * elemSize (heap slot index).
		if si, ok := l.lookupSlice(id.Name); ok {
			idx, err := l.ptrDynIndex(si.ptr, e.Index, max(si.elemSize, 1))
			if err != nil {
				return exprResult{}, err
			}
			r := exprResult{cell: idx, temp: true}
			if si.elemType != "" {
				r.isPointer = true
				r.typeName = si.elemType
			}
			if si.elemIntSize >= 2 {
				r.ptrIntSize = si.elemIntSize
			}
			return r, nil
		}
		// &p[i] for pointer-to-array param: ptrDynIndex computes
		// *p + i*elemSize, where *p holds the caller's array base.
		if ptrAI, ok := l.lookupPtrArray(id.Name); ok {
			cell, err := l.lookupVar(id.Name)
			if err != nil {
				return exprResult{}, err
			}
			idx, err := l.ptrDynIndex(cell, e.Index, max(ptrAI.elemSize, 1))
			if err != nil {
				return exprResult{}, err
			}
			r := exprResult{cell: idx, temp: true}
			if ptrAI.elemType != "" {
				r.isPointer = true
				r.typeName = ptrAI.elemType
			}
			return r, nil
		}
		// &a[i] -- compute slotOf(a.base) + i
		ai, ok := l.lookupArray(id.Name)
		if !ok {
			return exprResult{}, fmt.Errorf("cannot take address of non-array index: %s", id.Name)
		}
		baseSlot := slotOf(ai.base)
		es := max(ai.elemSize, 1)
		t := l.allocCell()
		if constIdx, ok := l.constValue(e.Index); ok {
			l.emit(&IRConst{Dst: t, Value: byte(baseSlot + constIdx*es)}) // #nosec G115
		} else {
			idxR, err := l.lowerExpr(e.Index)
			if err != nil {
				return exprResult{}, err
			}
			scaled := idxR.cell
			scaledTemp := idxR.temp
			if es > 1 {
				if !scaledTemp {
					// Don't clobber a bound variable by multiplying in place.
					t := l.allocCell()
					l.emit(&IRCopy{Dst: t, Src: scaled})
					scaled = t
					scaledTemp = true
				}
				esCell := l.allocCell()
				l.emit(&IRConst{Dst: esCell, Value: byte(es)}) // #nosec G115
				l.emit(&IRMul{Dst: scaled, Src1: scaled, Src2: esCell})
				l.freeCell(esCell)
			}
			l.emit(&IRConst{Dst: t, Value: byte(baseSlot)}) // #nosec G115
			l.emit(&IRAdd{Dst: t, Src1: t, Src2: scaled})
			if scaledTemp {
				l.freeCell(scaled)
			}
		}
		r := exprResult{cell: t, temp: true}
		if ai.elemType != "" {
			r.isPointer = true
			r.typeName = ai.elemType
		}
		if ai.elemIntSize >= 2 {
			r.ptrIntSize = ai.elemIntSize
		}
		return r, nil
	case *ast.SelectorExpr:
		// &p.x -- base slot + field offset
		r, err := l.lowerSelectorExpr(e)
		if err != nil {
			return exprResult{}, err
		}
		t := l.allocCell()
		l.emit(&IRConst{Dst: t, Value: byte(slotOf(r.cell))}) // #nosec G115
		res := exprResult{cell: t, temp: true}
		if r.intSize >= 2 {
			res.ptrIntSize = r.intSize
		}
		return res, nil
	case *ast.CompositeLit:
		// &Point{x: 1, y: 2} -- lower into cells, return pointer.
		if def := l.structDef(e.Type); def != nil {
			base := l.allocCells(def.Size)
			for j := range def.Size {
				l.emit(&IRZero{Dst: base + j})
			}
			if err := l.lowerStructValueTo(base, def, e); err != nil {
				return exprResult{}, err
			}
			t := l.allocCell()
			l.emit(&IRConst{Dst: t, Value: byte(slotOf(base))}) // #nosec G115
			return exprResult{cell: t, temp: true}, nil
		}
		if size := l.arraySize(e.Type); size > 0 {
			base := l.allocCells(size)
			arr := arrayInfo{base: base, size: size, count: size, elemSize: 1}
			if err := l.lowerCompositeLitInto(arr, e); err != nil {
				return exprResult{}, err
			}
			t := l.allocCell()
			l.emit(&IRConst{Dst: t, Value: byte(slotOf(base))}) // #nosec G115
			return exprResult{cell: t, temp: true}, nil
		}
		return exprResult{}, fmt.Errorf("cannot take address of %T", expr)
	default:
		return exprResult{}, fmt.Errorf("cannot take address of %T", expr)
	}
}

// lowerDeref handles *p -- reads from the stack slot whose index is in p.
func (l *Lowerer) lowerDeref(expr ast.Expr) (exprResult, error) {
	r, err := l.lowerExpr(expr)
	if err != nil {
		return exprResult{}, err
	}
	if n := r.ptrIntSize; n >= 2 {
		base := l.allocCells(n)
		idx := l.allocCell()
		l.emit(&IRCopy{Dst: idx, Src: r.cell})
		for j := range n {
			val := l.ptrLoad(idx)
			l.emit(&IRMove{Dst: base + j, Src: val})
			l.freeCell(val)
			if j < n-1 {
				l.emit(&IRAddI{Dst: idx, Value: 1})
			}
		}
		l.freeCell(idx)
		return exprResult{cell: base, temp: true, size: n, intSize: n}, nil
	}
	result := l.ptrLoad(r.cell)
	if r.temp {
		l.freeCell(r.cell)
	}
	return exprResult{cell: result, temp: true}, nil
}

func (l *Lowerer) lowerUnary(e *ast.UnaryExpr, lowerExpr func(ast.Expr) (exprResult, error)) (exprResult, error) {
	operand, err := lowerExpr(e.X)
	if err != nil {
		return exprResult{}, err
	}
	// Multi-byte integer unary operations.
	if operand.intSize >= 2 {
		return l.lowerUnaryInt(e.Op, operand)
	}
	t := l.allocCell()
	switch e.Op {
	case token.NOT:
		l.emit(&IRNot{Dst: t, Src: operand.cell})
	case token.SUB:
		zero := l.allocCell()
		l.emit(&IRConst{Dst: zero, Value: 0})
		l.emit(&IRSub{Dst: t, Src1: zero, Src2: operand.cell})
		l.freeCell(zero)
	case token.XOR:
		// ^x = 255 - x (bitwise complement for byte)
		byteMax := l.allocCell()
		l.emit(&IRConst{Dst: byteMax, Value: 255})
		l.emit(&IRSub{Dst: t, Src1: byteMax, Src2: operand.cell})
		l.freeCell(byteMax)
	default:
		l.freeCell(t)
		return exprResult{}, fmt.Errorf("unsupported unary operator: %s", e.Op)
	}
	if operand.temp {
		l.freeCell(operand.cell)
	}
	return exprResult{cell: t, temp: true}, nil
}

// lowerUnaryInt handles unary operations on multi-byte integers.
func (l *Lowerer) lowerUnaryInt(op token.Token, operand exprResult) (exprResult, error) {
	n := operand.intSize
	r := l.allocCells(n)
	switch op {
	case token.SUB:
		byteMax := l.allocCell()
		l.emit(&IRConst{Dst: byteMax, Value: 255})
		for j := range n {
			l.emit(&IRSub{Dst: r + j, Src1: byteMax, Src2: operand.cell + j})
		}
		l.freeCell(byteMax)
		l.emitIncInt(r, n)
	case token.XOR:
		byteMax := l.allocCell()
		l.emit(&IRConst{Dst: byteMax, Value: 255})
		for j := range n {
			l.emit(&IRSub{Dst: r + j, Src1: byteMax, Src2: operand.cell + j})
		}
		l.freeCell(byteMax)
	default:
		l.freeCellRange(r, n)
		return exprResult{}, fmt.Errorf("unsupported unary operator for uint%d: %s", n*8, op)
	}
	if operand.temp {
		l.freeCellRange(operand.cell, n)
	}
	return exprResult{cell: r, temp: true, size: n, intSize: n}, nil
}

func (l *Lowerer) lowerBinary(e *ast.BinaryExpr, lowerExpr func(ast.Expr) (exprResult, error)) (exprResult, error) {
	if e.Op == token.LAND || e.Op == token.LOR {
		return l.lowerLogical(e, lowerExpr)
	}

	// Handle array/struct equality comparison.
	if e.Op == token.EQL || e.Op == token.NEQ {
		if r, ok, err := l.lowerCompositeCompare(e); ok {
			return r, err
		}
		if r, ok, err := l.lowerSliceCompare(e); ok {
			return r, err
		}
	}
	// String lexicographic ordering.
	if e.Op == token.LSS || e.Op == token.GTR || e.Op == token.LEQ || e.Op == token.GEQ {
		if r, ok, err := l.lowerSliceLexCompare(e); ok {
			return r, err
		}
	}
	// String concatenation.
	if e.Op == token.ADD {
		if r, ok, err := l.lowerStringConcat(e); ok {
			return r, err
		}
	}

	left, err := lowerExpr(e.X)
	if err != nil {
		return exprResult{}, err
	}
	right, err := lowerExpr(e.Y)
	if err != nil {
		return exprResult{}, err
	}
	// Multi-byte integer binary operations.
	if left.intSize >= 2 || right.intSize >= 2 {
		if e.Op != token.SHL && e.Op != token.SHR {
			if left.intSize != right.intSize {
				return exprResult{}, fmt.Errorf("mismatched integer sizes in %s, use explicit conversion", e.Op)
			}
		}
		return l.lowerBinaryInt(e.Op, left, right)
	}
	t := l.allocCell()
	switch e.Op {
	case token.ADD:
		l.emit(&IRAdd{Dst: t, Src1: left.cell, Src2: right.cell})
	case token.SUB:
		l.emit(&IRSub{Dst: t, Src1: left.cell, Src2: right.cell})
	case token.MUL:
		l.emit(&IRMul{Dst: t, Src1: left.cell, Src2: right.cell})
	case token.QUO:
		l.emit(&IRDiv{Dst: t, Src1: left.cell, Src2: right.cell})
	case token.REM:
		l.emit(&IRMod{Dst: t, Src1: left.cell, Src2: right.cell})
	case token.AND:
		l.emit(&IRAnd{Dst: t, Src1: left.cell, Src2: right.cell})
	case token.OR:
		l.emit(&IROr{Dst: t, Src1: left.cell, Src2: right.cell})
	case token.XOR:
		l.emit(&IRXor{Dst: t, Src1: left.cell, Src2: right.cell})
	case token.AND_NOT:
		// a &^ b = a & (^b) = a & (255 - b)
		comp := l.allocCell()
		byteMax := l.allocCell()
		l.emit(&IRConst{Dst: byteMax, Value: 255})
		l.emit(&IRSub{Dst: comp, Src1: byteMax, Src2: right.cell})
		l.freeCell(byteMax)
		l.emit(&IRAnd{Dst: t, Src1: left.cell, Src2: comp})
		l.freeCell(comp)
	case token.SHL:
		// x << n = x * (2^n)
		pow := l.allocCell()
		l.emit(&IRConst{Dst: pow, Value: 1})
		cnt := l.allocCell()
		l.emitCopyOrMove(cnt, right)
		right.temp = false // consumed by emitCopyOrMove
		l.emit(&IRLoop{Cond: cnt, Body: &IRBlock{Nodes: []IRNode{
			&IRAdd{Dst: pow, Src1: pow, Src2: pow},
			&IRSubI{Dst: cnt, Value: 1},
		}}})
		l.emit(&IRMul{Dst: t, Src1: left.cell, Src2: pow})
		l.freeCell(pow)
		l.freeCell(cnt)
	case token.SHR:
		// x >> n = x / (2^n)
		pow := l.allocCell()
		l.emit(&IRConst{Dst: pow, Value: 1})
		cnt := l.allocCell()
		l.emitCopyOrMove(cnt, right)
		right.temp = false // consumed by emitCopyOrMove
		l.emit(&IRLoop{Cond: cnt, Body: &IRBlock{Nodes: []IRNode{
			&IRAdd{Dst: pow, Src1: pow, Src2: pow},
			&IRSubI{Dst: cnt, Value: 1},
		}}})
		l.emit(&IRDiv{Dst: t, Src1: left.cell, Src2: pow})
		l.freeCell(pow)
		l.freeCell(cnt)
	case token.EQL:
		l.emit(&IRCmp{Op: CmpEq, Dst: t, Src1: left.cell, Src2: right.cell})
	case token.NEQ:
		l.emit(&IRCmp{Op: CmpNeq, Dst: t, Src1: left.cell, Src2: right.cell})
	case token.LSS:
		l.emit(&IRCmp{Op: CmpLt, Dst: t, Src1: left.cell, Src2: right.cell})
	case token.GTR:
		l.emit(&IRCmp{Op: CmpGt, Dst: t, Src1: left.cell, Src2: right.cell})
	case token.LEQ:
		l.emit(&IRCmp{Op: CmpLeq, Dst: t, Src1: left.cell, Src2: right.cell})
	case token.GEQ:
		l.emit(&IRCmp{Op: CmpGeq, Dst: t, Src1: left.cell, Src2: right.cell})
	default:
		l.freeCell(t)
		return exprResult{}, fmt.Errorf("unsupported binary operator: %s", e.Op)
	}
	if left.temp {
		l.freeCell(left.cell)
	}
	if right.temp {
		l.freeCell(right.cell)
	}
	return exprResult{cell: t, temp: true}, nil
}

func (l *Lowerer) lowerLogical(e *ast.BinaryExpr, lowerExpr func(ast.Expr) (exprResult, error)) (exprResult, error) {
	left, err := lowerExpr(e.X)
	if err != nil {
		return exprResult{}, err
	}
	result := l.allocCell()
	saved := l.nodes

	// Block that evaluates the right operand into result.
	l.nodes = nil
	right, err := lowerExpr(e.Y)
	if err != nil {
		return exprResult{}, err
	}
	l.emitCopyOrMove(result, right)
	rightBlock := &IRBlock{Nodes: l.nodes}

	// Block that sets the short-circuit value (0 for LAND, 1 for LOR).
	var shortVal byte
	if e.Op == token.LOR {
		shortVal = 1
	}
	l.nodes = nil
	l.emit(&IRConst{Dst: result, Value: shortVal})
	shortBlock := &IRBlock{Nodes: l.nodes}

	// LAND: if left then right else 0. LOR: if left then 1 else right.
	l.nodes = saved
	thenBlock, elseBlock := rightBlock, shortBlock
	if e.Op == token.LOR {
		thenBlock, elseBlock = shortBlock, rightBlock
	}
	l.emit(&IRIf{Cond: left.cell, Then: thenBlock, Else: elseBlock})

	if left.temp {
		l.freeCell(left.cell)
	}
	return exprResult{cell: result, temp: true}, nil
}

// isByteSliceIdent reports whether expr is an identifier bound as a
// []byte slice (the type produced by string literals).
// stringLen returns (Cell, isConst, value) for the length of a string-like
// operand: byte-slice ident -> (si.len, false, _); string literal or
// string-const ident -> (_, true, byteCount).
func (l *Lowerer) isStringExpr(expr ast.Expr) bool {
	if paren, ok := expr.(*ast.ParenExpr); ok {
		return l.isStringExpr(paren.X)
	}
	if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		return true
	}
	if sel, ok := expr.(*ast.SelectorExpr); ok {
		return l.isStringSelector(sel)
	}
	if bin, ok := expr.(*ast.BinaryExpr); ok && bin.Op == token.ADD {
		return l.isStringExpr(bin.X) && l.isStringExpr(bin.Y)
	}
	if se, ok := expr.(*ast.SliceExpr); ok {
		return l.isStringExpr(se.X)
	}
	if ix, ok := expr.(*ast.IndexExpr); ok {
		// `s[i]` where s is `[]string` / `[N]string` (a slice or array
		// with byte-slice elements) yields a string-shaped header.
		if id, ok := ix.X.(*ast.Ident); ok {
			if si, ok := l.lookupSlice(id.Name); ok {
				return si.elemSlice
			}
			if ai, ok := l.lookupArray(id.Name); ok {
				return ai.elemSlice
			}
		}
	}
	if call, ok := expr.(*ast.CallExpr); ok {
		if fn, ok := call.Fun.(*ast.Ident); ok {
			if fn.Name == "string" && len(call.Args) == 1 {
				return true
			}
			// User-defined function returning a string.
			if info, ok := l.result.Funcs[fn.Name]; ok {
				rt := info.ReturnType
				if rt.IsSlice && rt.SliceElemSize <= 1 && rt.SliceElemType == "" && rt.SliceElemIntSize == 0 {
					return true
				}
			}
		}
		if at, ok := call.Fun.(*ast.ArrayType); ok && at.Len == nil && len(call.Args) == 1 {
			if id, ok := at.Elt.(*ast.Ident); ok && id.Name == "byte" {
				return l.isStringExpr(call.Args[0])
			}
		}
	}
	id, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	if l.lookupStringConst(id.Name) != "" {
		return true
	}
	si, ok := l.lookupSlice(id.Name)
	if !ok {
		return false
	}
	return si.elemSize == 1 && si.elemType == "" && !si.elemSlice && si.elemIntSize == 0
}

// isStringSelector reports whether sel selects a string-typed struct
// field. Handles direct local struct bindings, pointer-to-struct
// idents, struct-array element access (`items[i].name`), and chained
// selectors (`outer.inner.name`).
func (l *Lowerer) isStringSelector(sel *ast.SelectorExpr) bool {
	def := l.selectorStructDef(sel.X)
	if def == nil {
		return false
	}
	return def.FieldStrings[sel.Sel.Name]
}

// selectorStructDef resolves the static struct type of a selector
// receiver expression without emitting any IR. Returns nil if the
// receiver isn't a known struct.
func (l *Lowerer) selectorStructDef(expr ast.Expr) *StructDef {
	switch x := expr.(type) {
	case *ast.Ident:
		if sb, ok := l.lookupStruct(x.Name); ok {
			return sb.def
		}
		if def, ok := l.lookupPtrType(x.Name); ok {
			return def
		}
	case *ast.IndexExpr:
		// Direct ident base: items[i] for items of slice/array type.
		if id, ok := x.X.(*ast.Ident); ok {
			if si, ok := l.lookupSlice(id.Name); ok && si.elemType != "" {
				return l.result.Structs[si.elemType]
			}
			if ai, ok := l.lookupArray(id.Name); ok && ai.elemType != "" {
				return l.result.Structs[ai.elemType]
			}
			return nil
		}
		// Field-of-struct base: a.items[i] for items of type [N]Item.
		if sel, ok := x.X.(*ast.SelectorExpr); ok {
			outer := l.selectorStructDef(sel.X)
			if outer == nil {
				return nil
			}
			if t := outer.FieldArrayElemType[sel.Sel.Name]; t != "" {
				return l.result.Structs[t]
			}
		}
	case *ast.SelectorExpr:
		// Chained: r.inner.field -- look up the type of r.inner.
		outer := l.selectorStructDef(x.X)
		if outer == nil {
			return nil
		}
		if t := outer.FieldTypes[x.Sel.Name]; t != "" {
			return l.result.Structs[t]
		}
	}
	return nil
}

// selectorStringField returns a sliceInfo built from the field's three
// header cells if sel selects a string-typed struct field of a local
// struct binding. The cells are not freshly allocated, so the caller
// must not freeSliceInfo the returned value. Returns false for
// pointer-accessed fields (use resolveStringSlice's fallback instead).
func (l *Lowerer) selectorStringField(sel *ast.SelectorExpr) (sliceInfo, bool) {
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return sliceInfo{}, false
	}
	sb, ok := l.lookupStruct(id.Name)
	if !ok {
		return sliceInfo{}, false
	}
	if !sb.def.FieldStrings[sel.Sel.Name] {
		return sliceInfo{}, false
	}
	off := Cell(sb.def.Offsets[sel.Sel.Name]) // #nosec G115
	return sliceInfo{
		ptr:      sb.base + off,
		len:      sb.base + off + 1,
		cap:      sb.base + off + 2,
		elemSize: 1,
	}, true
}

// resolveStringSlice returns the sliceInfo for a string-like operand. For
// idents bound as byte slices it returns the existing slice; for literals
// (or string-const idents) it materializes a fresh heap-backed slice and
// the caller must free it via freeSliceInfo when done.
func (l *Lowerer) resolveStringSlice(expr ast.Expr) (sliceInfo, bool, error) {
	if id, ok := expr.(*ast.Ident); ok {
		if si, ok := l.lookupSlice(id.Name); ok &&
			si.elemSize == 1 && si.elemType == "" && !si.elemSlice && si.elemIntSize == 0 {
			return si, false, nil
		}
	}
	if sel, ok := expr.(*ast.SelectorExpr); ok {
		if si, ok := l.selectorStringField(sel); ok {
			return si, false, nil
		}
	}
	if s, ok := l.stringLiteralValue(expr); ok {
		lit := &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(s)}
		si, err := l.evalStringLiteral(lit)
		return si, true, err
	}
	// Fallback: anything else string-shaped (e.g. `a + b`, function call,
	// indexed slice). Materialize via lowerSliceExpr.
	si, err := l.lowerSliceExpr(expr)
	if err != nil {
		return sliceInfo{}, false, err
	}
	return si, true, nil
}

// stringLiteralValue returns the literal byte content if expr is a string
// literal or a string-const ident. Otherwise returns "", false.
func (l *Lowerer) stringLiteralValue(expr ast.Expr) (string, bool) {
	if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		s, err := strconv.Unquote(lit.Value)
		return s, err == nil
	}
	if id, ok := expr.(*ast.Ident); ok {
		if s := l.lookupStringConst(id.Name); s != "" {
			return s, true
		}
	}
	return "", false
}

// appendBytesFromSlice copies src.len bytes from src.ptr into dst at
// offset dst.len, then bumps dst.len. Caller must ensure
// dst.cap >= dst.len + src.len.
func (l *Lowerer) appendBytesFromSlice(dst, src sliceInfo) {
	counter := l.allocCell()
	l.emit(&IRZero{Dst: counter})
	cond := l.allocCell()
	l.emit(&IRCmp{Op: CmpLt, Dst: cond, Src1: counter, Src2: src.len})
	saved := l.nodes
	l.nodes = nil
	sAddr := l.allocCell()
	l.emit(&IRAdd{Dst: sAddr, Src1: src.ptr, Src2: counter})
	v := l.ptrLoad(sAddr)
	l.freeCell(sAddr)
	dAddr := l.allocCell()
	l.emit(&IRAdd{Dst: dAddr, Src1: dst.ptr, Src2: dst.len})
	l.ptrStore(dAddr, v)
	l.freeCell(v)
	l.freeCell(dAddr)
	l.emit(&IRAddI{Dst: dst.len, Value: 1})
	l.emit(&IRAddI{Dst: counter, Value: 1})
	l.emit(&IRCmp{Op: CmpLt, Dst: cond, Src1: counter, Src2: src.len})
	body := &IRBlock{Nodes: l.nodes}
	l.nodes = saved
	l.emit(&IRLoop{Cond: cond, Body: body})
	l.freeCell(cond)
	l.freeCell(counter)
}

// lowerStringConcat handles `+` between two string-like operands. The
// new slice is pre-sized to len(a)+len(b) so the appends don't trigger
// reallocation.
func (l *Lowerer) lowerStringConcat(e *ast.BinaryExpr) (exprResult, bool, error) {
	if !l.isStringExpr(e.X) || !l.isStringExpr(e.Y) {
		return exprResult{}, false, nil
	}

	// Resolve each operand exactly once. Materializing operands like
	// `string(byteExpr)` emits heap-allocating IR; calling resolveStringSlice
	// twice would double-allocate. Literals fold to compile-time lengths.
	type concatOperand struct {
		literal   string
		isLiteral bool
		si        sliceInfo
		isTemp    bool
	}
	var ops [2]concatOperand
	for i, x := range [2]ast.Expr{e.X, e.Y} {
		if s, ok := l.stringLiteralValue(x); ok {
			ops[i] = concatOperand{literal: s, isLiteral: true}
			continue
		}
		src, srcTemp, err := l.resolveStringSlice(x)
		if err != nil {
			return exprResult{}, false, err
		}
		ops[i] = concatOperand{si: src, isTemp: srcTemp}
	}

	si := l.allocSliceInfo()
	si.elemSize = 1
	l.emit(&IRZero{Dst: si.len})

	totalConst := 0
	for _, op := range ops {
		if op.isLiteral {
			totalConst += len(op.literal)
		}
	}
	if totalConst > 0 {
		l.emit(&IRConst{Dst: si.cap, Value: byte(totalConst)}) // #nosec G115
	} else {
		l.emit(&IRZero{Dst: si.cap})
	}
	for _, op := range ops {
		if !op.isLiteral {
			l.emit(&IRAdd{Dst: si.cap, Src1: si.cap, Src2: op.si.len})
		}
	}

	l.pushHeapRegion(si)

	for _, op := range ops {
		if op.isLiteral {
			l.appendLiteralBytes(si, op.literal)
		} else {
			l.appendBytesFromSlice(si, op.si)
		}
	}
	for _, op := range ops {
		if op.isTemp {
			l.freeSliceInfo(op.si)
		}
	}

	return exprResult{cell: si.ptr, temp: true, elemSize: 1, elemCount: 255,
		isPointer: true, lenCell: si.len, capCell: si.cap}, true, nil
}

// appendLiteralBytes inlines a string literal into si byte-by-byte at offset si.len.
// The caller must ensure si.cap >= si.len + len(s).
func (l *Lowerer) appendLiteralBytes(si sliceInfo, s string) {
	for _, b := range []byte(s) {
		addr := l.allocCell()
		l.emit(&IRAdd{Dst: addr, Src1: si.ptr, Src2: si.len})
		valCell := l.allocCell()
		l.emit(&IRConst{Dst: valCell, Value: b})
		l.ptrStore(addr, valCell)
		l.freeCell(valCell)
		l.freeCell(addr)
		l.emit(&IRAddI{Dst: si.len, Value: 1})
	}
}

// lowerSliceLexCompare handles `<`, `>`, `<=`, `>=` for two byte-slice
// operands. Walks bytes from index 0; at the first non-equal pair sets
// the result and stops. If all bytes match up to min(len(a), len(b)),
// the lengths are compared instead.
func (l *Lowerer) lowerSliceLexCompare(e *ast.BinaryExpr) (exprResult, bool, error) {
	if !l.isStringExpr(e.X) || !l.isStringExpr(e.Y) {
		return exprResult{}, false, nil
	}
	switch e.Op {
	case token.LSS, token.GTR, token.LEQ, token.GEQ:
	default:
		return exprResult{}, false, nil
	}
	lSI, lTemp, err := l.resolveStringSlice(e.X)
	if err != nil {
		return exprResult{}, false, err
	}
	rSI, rTemp, err := l.resolveStringSlice(e.Y)
	if err != nil {
		return exprResult{}, false, err
	}
	defer func() {
		if lTemp {
			l.freeSliceInfo(lSI)
		}
		if rTemp {
			l.freeSliceInfo(rSI)
		}
	}()

	cmpOp := CmpLt
	if e.Op == token.GTR || e.Op == token.GEQ {
		cmpOp = CmpGt
	}

	result := l.allocCell()
	l.emit(&IRZero{Dst: result})
	done := l.allocCell()
	l.emit(&IRZero{Dst: done})

	// minLen = min(len(a), len(b))
	minLen := l.allocCell()
	tmp := l.allocCell()
	l.emit(&IRCmp{Op: CmpLt, Dst: tmp, Src1: lSI.len, Src2: rSI.len})
	l.emit(&IRCopy{Dst: minLen, Src: rSI.len})
	l.emit(&IRIf{Cond: tmp, Then: &IRBlock{Nodes: []IRNode{
		&IRCopy{Dst: minLen, Src: lSI.len},
	}}})
	l.freeCell(tmp)

	li := l.allocCell()
	l.emit(&IRCopy{Dst: li, Src: lSI.ptr})
	ri := l.allocCell()
	l.emit(&IRCopy{Dst: ri, Src: rSI.ptr})
	cnt := l.allocCell()
	l.emit(&IRCopy{Dst: cnt, Src: minLen})

	// Loop body: if !done, compare av,bv; on mismatch set result+done.
	saved := l.nodes
	l.nodes = nil
	nd := l.allocCell()
	l.emit(&IRNot{Dst: nd, Src: done})
	innerSaved := l.nodes
	l.nodes = nil
	av := l.ptrLoad(li)
	bv := l.ptrLoad(ri)
	eq := l.allocCell()
	l.emit(&IRCmp{Op: CmpEq, Dst: eq, Src1: av, Src2: bv})
	notEq := l.allocCell()
	l.emit(&IRNot{Dst: notEq, Src: eq})
	l.emit(&IRIf{Cond: notEq, Then: &IRBlock{Nodes: []IRNode{
		&IRCmp{Op: cmpOp, Dst: result, Src1: av, Src2: bv},
		&IRConst{Dst: done, Value: 1},
	}}})
	l.freeCell(eq)
	l.freeCell(av)
	l.freeCell(bv)
	innerBody := &IRBlock{Nodes: l.nodes}
	l.nodes = innerSaved
	l.emit(&IRIf{Cond: nd, Then: innerBody})
	l.emit(&IRAddI{Dst: li, Value: 1})
	l.emit(&IRAddI{Dst: ri, Value: 1})
	l.emit(&IRSubI{Dst: cnt, Value: 1})
	body := &IRBlock{Nodes: l.nodes}
	l.nodes = saved
	l.emit(&IRLoop{Cond: cnt, Body: body})

	// All bytes equal up to minLen: result depends on length comparison.
	tailNd := l.allocCell()
	l.emit(&IRNot{Dst: tailNd, Src: done})
	tailSaved := l.nodes
	l.nodes = nil
	l.emit(&IRCmp{Op: cmpOp, Dst: result, Src1: lSI.len, Src2: rSI.len})
	if e.Op == token.LEQ || e.Op == token.GEQ {
		// Equal lengths also yield true.
		eqLen := l.allocCell()
		l.emit(&IRCmp{Op: CmpEq, Dst: eqLen, Src1: lSI.len, Src2: rSI.len})
		l.emit(&IRIf{Cond: eqLen, Then: &IRBlock{Nodes: []IRNode{
			&IRConst{Dst: result, Value: 1},
		}}})
		l.freeCell(eqLen)
	}
	tailBody := &IRBlock{Nodes: l.nodes}
	l.nodes = tailSaved
	l.emit(&IRIf{Cond: tailNd, Then: tailBody})

	l.freeCell(li)
	l.freeCell(ri)
	l.freeCell(cnt)
	l.freeCell(minLen)
	l.freeCell(done)

	return exprResult{cell: result, temp: true}, true, nil
}

// lowerSliceCompare handles `==` / `!=` for two string-like operands.
func (l *Lowerer) lowerSliceCompare(e *ast.BinaryExpr) (exprResult, bool, error) {
	if !l.isStringExpr(e.X) || !l.isStringExpr(e.Y) {
		return exprResult{}, false, nil
	}
	lSI, lTemp, err := l.resolveStringSlice(e.X)
	if err != nil {
		return exprResult{}, false, err
	}
	rSI, rTemp, err := l.resolveStringSlice(e.Y)
	if err != nil {
		return exprResult{}, false, err
	}
	defer func() {
		if lTemp {
			l.freeSliceInfo(lSI)
		}
		if rTemp {
			l.freeSliceInfo(rSI)
		}
	}()

	result := l.allocCell()
	l.emit(&IRCmp{Op: CmpEq, Dst: result, Src1: lSI.len, Src2: rSI.len})

	cnt := l.allocCell()
	l.emit(&IRCopy{Dst: cnt, Src: lSI.len})
	li := l.allocCell()
	l.emit(&IRCopy{Dst: li, Src: lSI.ptr})
	ri := l.allocCell()
	l.emit(&IRCopy{Dst: ri, Src: rSI.ptr})

	saved := l.nodes
	l.nodes = nil
	lv := l.ptrLoad(li)
	rv := l.ptrLoad(ri)
	eq := l.allocCell()
	l.emit(&IRCmp{Op: CmpEq, Dst: eq, Src1: lv, Src2: rv})
	l.freeCell(lv)
	l.freeCell(rv)
	l.emit(&IRMul{Dst: result, Src1: result, Src2: eq})
	l.freeCell(eq)
	l.emit(&IRAddI{Dst: li, Value: 1})
	l.emit(&IRAddI{Dst: ri, Value: 1})
	l.emit(&IRSubI{Dst: cnt, Value: 1})
	body := &IRBlock{Nodes: l.nodes}
	l.nodes = saved

	// Skip loop if lengths differ (result already 0).
	cond := l.allocCell()
	l.emit(&IRCopy{Dst: cond, Src: result})
	l.emit(&IRIf{Cond: cond, Then: &IRBlock{Nodes: []IRNode{
		&IRLoop{Cond: cnt, Body: body},
	}}})
	l.freeCell(cond)

	l.freeCell(li)
	l.freeCell(ri)
	l.freeCell(cnt)

	if e.Op == token.NEQ {
		notR := l.allocCell()
		l.emit(&IRNot{Dst: notR, Src: result})
		l.freeCell(result)
		return exprResult{cell: notR, temp: true}, true, nil
	}
	return exprResult{cell: result, temp: true}, true, nil
}

// lowerCompositeCompare handles == and != for arrays and structs.
// Returns (result, true, nil) if handled, or (_, false, nil) if not composite.
func (l *Lowerer) lowerCompositeCompare(e *ast.BinaryExpr) (exprResult, bool, error) {
	lBase, lSize, lTemp, lDef := l.resolveCompositeOperand(e.X)
	rBase, rSize, rTemp, rDef := l.resolveCompositeOperand(e.Y)
	if lSize < 0 || rSize < 0 || lSize != rSize {
		// Free any temps allocated for composite literals.
		if lTemp > 0 {
			l.freeCellRange(lBase, lTemp)
		}
		if rTemp > 0 {
			l.freeCellRange(rBase, rTemp)
		}
		return exprResult{}, false, nil
	}

	// Compare element-wise with short-circuit: start with result = 1,
	// then for each pair, only compare if result is still 1.
	result := l.allocCell()
	l.emit(&IRConst{Dst: result, Value: 1})
	if lDef != nil && rDef != nil && lDef == rDef {
		l.emitStructCompare(result, lBase, rBase, lDef)
	} else {
		for i := range lSize {
			cond := l.allocCell()
			l.emit(&IRCopy{Dst: cond, Src: result})
			l.emit(&IRIf{Cond: cond, Then: &IRBlock{Nodes: []IRNode{
				&IRCmp{Op: CmpEq, Dst: result, Src1: lBase + i, Src2: rBase + i},
			}}})
			l.freeCell(cond)
		}
	}

	// Free temps from composite literals.
	if lTemp > 0 {
		l.freeCellRange(lBase, lTemp)
	}
	if rTemp > 0 {
		l.freeCellRange(rBase, rTemp)
	}

	if e.Op == token.NEQ {
		notResult := l.allocCell()
		l.emit(&IRNot{Dst: notResult, Src: result})
		l.freeCell(result)
		return exprResult{cell: notResult, temp: true}, true, nil
	}
	return exprResult{cell: result, temp: true}, true, nil
}

// emitStructCompare ANDs `result` with field-aware equality for two
// structs of the same definition. String fields compare by content;
// every other field compares cell-by-cell. Each field guards on `result`
// so an early mismatch short-circuits the rest.
func (l *Lowerer) emitStructCompare(result, lBase, rBase Cell, def *StructDef) {
	for _, name := range def.Fields {
		offset := Cell(def.Offsets[name]) // #nosec G115
		cond := l.allocCell()
		l.emit(&IRCopy{Dst: cond, Src: result})
		saved := l.nodes
		l.nodes = nil
		if def.FieldStrings[name] {
			lSI := sliceInfo{ptr: lBase + offset, len: lBase + offset + 1, cap: lBase + offset + 2, elemSize: 1}
			rSI := sliceInfo{ptr: rBase + offset, len: rBase + offset + 1, cap: rBase + offset + 2, elemSize: 1}
			l.emitStringEq(result, lSI, rSI)
		} else if t := def.FieldTypes[name]; t != "" {
			// Nested struct: recurse so its string subfields compare by content.
			l.emitStructCompare(result, lBase+offset, rBase+offset, l.result.Structs[t])
		} else {
			fieldSize := 1
			if n := def.FieldIntSizes[name]; n >= 2 {
				fieldSize = n
			} else if n := def.FieldArraySizes[name]; n > 0 {
				fieldSize = n
			}
			for j := range fieldSize {
				inner := l.allocCell()
				l.emit(&IRCopy{Dst: inner, Src: result})
				l.emit(&IRIf{Cond: inner, Then: &IRBlock{Nodes: []IRNode{
					&IRCmp{Op: CmpEq, Dst: result, Src1: lBase + offset + Cell(j), Src2: rBase + offset + Cell(j)}, // #nosec G115
				}}})
				l.freeCell(inner)
			}
		}
		body := &IRBlock{Nodes: l.nodes}
		l.nodes = saved
		l.emit(&IRIf{Cond: cond, Then: body})
		l.freeCell(cond)
	}
}

// emitStringEq sets result to (result && content-equal(lSI, rSI)).
// Implements the same logic as lowerSliceCompare but without the
// outer alloc/return so callers can inline it under a guard.
func (l *Lowerer) emitStringEq(result Cell, lSI, rSI sliceInfo) {
	lenEq := l.allocCell()
	l.emit(&IRCmp{Op: CmpEq, Dst: lenEq, Src1: lSI.len, Src2: rSI.len})
	l.emit(&IRMul{Dst: result, Src1: result, Src2: lenEq})
	l.freeCell(lenEq)

	cnt := l.allocCell()
	l.emit(&IRCopy{Dst: cnt, Src: lSI.len})
	li := l.allocCell()
	l.emit(&IRCopy{Dst: li, Src: lSI.ptr})
	ri := l.allocCell()
	l.emit(&IRCopy{Dst: ri, Src: rSI.ptr})

	saved := l.nodes
	l.nodes = nil
	lv := l.ptrLoad(li)
	rv := l.ptrLoad(ri)
	eq := l.allocCell()
	l.emit(&IRCmp{Op: CmpEq, Dst: eq, Src1: lv, Src2: rv})
	l.freeCell(lv)
	l.freeCell(rv)
	l.emit(&IRMul{Dst: result, Src1: result, Src2: eq})
	l.freeCell(eq)
	l.emit(&IRAddI{Dst: li, Value: 1})
	l.emit(&IRAddI{Dst: ri, Value: 1})
	l.emit(&IRSubI{Dst: cnt, Value: 1})
	body := &IRBlock{Nodes: l.nodes}
	l.nodes = saved

	cond := l.allocCell()
	l.emit(&IRCopy{Dst: cond, Src: result})
	l.emit(&IRIf{Cond: cond, Then: &IRBlock{Nodes: []IRNode{
		&IRLoop{Cond: cnt, Body: body},
	}}})
	l.freeCell(cond)
	l.freeCell(li)
	l.freeCell(ri)
	l.freeCell(cnt)
}

// resolveCompositeOperand resolves a comparison operand to (base, size, tempSize, structDef).
// Returns size = -1 if the operand is not a composite type. structDef is
// non-nil only for struct-typed operands.
// tempSize > 0 means tempSize cells starting at base were allocated and need freeing.
func (l *Lowerer) resolveCompositeOperand(expr ast.Expr) (Cell, int, int, *StructDef) {
	if id, ok := expr.(*ast.Ident); ok {
		if ai, ok := l.lookupArray(id.Name); ok {
			return ai.base, ai.size, 0, nil
		}
		if si, ok := l.lookupStruct(id.Name); ok {
			return si.base, si.def.Size, 0, si.def
		}
	}
	if comp, ok := expr.(*ast.CompositeLit); ok {
		if def := l.structDef(comp.Type); def != nil {
			base := l.allocCells(def.Size)
			// Zero all cells first.
			for j := range def.Size {
				l.emit(&IRZero{Dst: base + j})
			}
			// Lower the literal into temp cells.
			if err := l.lowerStructValueTo(base, def, comp); err != nil {
				return 0, 0, 0, nil
			}
			return base, def.Size, def.Size, def
		}
	}
	return 0, -1, 0, nil
}

// lowerBinaryInt handles binary operations on multi-byte integer values.
func (l *Lowerer) lowerBinaryInt(op token.Token, left, right exprResult) (exprResult, error) {
	n := left.intSize
	r := l.allocCells(n)

	switch op {
	case token.ADD:
		l.emitAddInt(r, left.cell, right.cell, n)
	case token.SUB:
		l.emitSubInt(r, left.cell, right.cell, n)
	case token.MUL:
		l.emitMulInt(r, left.cell, right.cell, n)
	case token.QUO:
		l.emitDivModInt(r, left.cell, right.cell, n, false)
	case token.REM:
		l.emitDivModInt(r, left.cell, right.cell, n, true)
	case token.AND:
		for j := range n {
			l.emit(&IRAnd{Dst: r + j, Src1: left.cell + j, Src2: right.cell + j})
		}
	case token.OR:
		for j := range n {
			l.emit(&IROr{Dst: r + j, Src1: left.cell + j, Src2: right.cell + j})
		}
	case token.XOR:
		for j := range n {
			l.emit(&IRXor{Dst: r + j, Src1: left.cell + j, Src2: right.cell + j})
		}
	case token.AND_NOT:
		byteMax := l.allocCell()
		l.emit(&IRConst{Dst: byteMax, Value: 255})
		for j := range n {
			comp := l.allocCell()
			l.emit(&IRSub{Dst: comp, Src1: byteMax, Src2: right.cell + j})
			l.emit(&IRAnd{Dst: r + j, Src1: left.cell + j, Src2: comp})
			l.freeCell(comp)
		}
		l.freeCell(byteMax)
	case token.SHL:
		if right.intSize >= 2 {
			l.freeCellRange(r, n)
			return exprResult{}, fmt.Errorf("shift count must be byte, not uint%d", right.intSize*8)
		}
		l.emitShiftInt(r, left.cell, right.cell, n, false)
	case token.SHR:
		if right.intSize >= 2 {
			l.freeCellRange(r, n)
			return exprResult{}, fmt.Errorf("shift count must be byte, not uint%d", right.intSize*8)
		}
		l.emitShiftInt(r, left.cell, right.cell, n, true)
	case token.EQL, token.NEQ, token.LSS, token.GTR, token.LEQ, token.GEQ:
		l.freeCellRange(r+1, n-1) // comparisons return 1 cell
		l.emitCmpInt(r, op, left.cell, right.cell, n)
		if left.temp {
			l.freeCellRange(left.cell, n)
		}
		if right.temp {
			l.freeCellRange(right.cell, n)
		}
		return exprResult{cell: r, temp: true}, nil
	default:
		l.freeCellRange(r, n)
		return exprResult{}, fmt.Errorf("unsupported uint%d operator: %s", n*8, op)
	}

	if left.temp {
		l.freeCellRange(left.cell, left.cellCount())
	}
	if right.temp {
		l.freeCellRange(right.cell, right.cellCount())
	}
	return exprResult{cell: r, temp: true, size: n, intSize: n}, nil
}

// emitAddInt computes r = a + b for n-byte integers with carry chain.
func (l *Lowerer) emitAddInt(r, a, b Cell, n int) {
	// General N-byte add with chained carry.
	carry := l.allocCell()
	l.emit(&IRZero{Dst: carry})
	for j := range n {
		// r[j] = a[j] + b[j]
		l.emit(&IRAdd{Dst: r + j, Src1: a + j, Src2: b + j})
		// r[j] += carry from previous byte
		old := l.allocCell()
		l.emit(&IRCopy{Dst: old, Src: r + j})
		cond := l.allocCell()
		l.emit(&IRCopy{Dst: cond, Src: carry})
		l.emit(&IRIf{Cond: cond, Then: &IRBlock{Nodes: []IRNode{
			&IRAddI{Dst: r + j, Value: 1},
		}}})
		l.freeCell(cond)
		if j < n-1 {
			// Compute new carry: (r[j] < a[j]) OR (carry was 1 AND r[j] == old,
			// meaning the +1 wrapped). Simplified: carry = (r[j] < a[j]) OR
			// (old carry AND r[j] == 0 after adding carry).
			// Easier: carry = (a[j] + b[j] overflowed) OR (adding carry overflowed)
			c1 := l.allocCell()
			l.emit(&IRCmp{Op: CmpLt, Dst: c1, Src1: old, Src2: a + j}) // a+b overflowed
			c2 := l.allocCell()
			l.emit(&IRCopy{Dst: c2, Src: r + j})
			l.emit(&IRNot{Dst: c2, Src: c2})               // r[j]==0 means carry addition wrapped
			l.emit(&IRMul{Dst: c2, Src1: c2, Src2: carry}) // only if carry was set
			// carry = c1 OR c2
			combined := l.allocCell()
			l.emit(&IRAdd{Dst: combined, Src1: c1, Src2: c2})
			prod := l.allocCell()
			l.emit(&IRMul{Dst: prod, Src1: c1, Src2: c2})
			l.emit(&IRSub{Dst: carry, Src1: combined, Src2: prod})
			l.freeCell(prod)
			l.freeCell(combined)
			l.freeCell(c2)
			l.freeCell(c1)
		}
		l.freeCell(old)
	}
	l.freeCell(carry)
}

// emitSubInt computes r = a - b for n-byte integers with borrow chain.
func (l *Lowerer) emitSubInt(r, a, b Cell, n int) {
	// General N-byte subtraction with chained borrow.
	borrow := l.allocCell()
	l.emit(&IRCmp{Op: CmpLt, Dst: borrow, Src1: a, Src2: b})
	l.emit(&IRSub{Dst: r, Src1: a, Src2: b})
	for j := 1; j < n; j++ {
		l.emit(&IRSub{Dst: r + j, Src1: a + j, Src2: b + j})
		// Apply borrow from previous byte.
		cond := l.allocCell()
		l.emit(&IRCopy{Dst: cond, Src: borrow})
		if j < n-1 {
			// Before decrementing, check if r[j] is 0 (will wrap on borrow).
			isZero := l.allocCell()
			l.emit(&IRCopy{Dst: isZero, Src: r + j})
			l.emit(&IRNot{Dst: isZero, Src: isZero})
			// newBorrow = borrow AND (r[j] == 0) -- borrow chains if r[j] wraps.
			newBorrow := l.allocCell()
			l.emit(&IRMul{Dst: newBorrow, Src1: borrow, Src2: isZero})
			l.freeCell(isZero)
			// Also check if a[j] < b[j] for a fresh borrow (independent of chain).
			freshBorrow := l.allocCell()
			l.emit(&IRCmp{Op: CmpLt, Dst: freshBorrow, Src1: a + j, Src2: b + j})
			// Combined borrow = newBorrow OR freshBorrow.
			// OR via: a|b = a + b - a*b (for 0/1 values).
			combined := l.allocCell()
			l.emit(&IRAdd{Dst: combined, Src1: newBorrow, Src2: freshBorrow})
			prod := l.allocCell()
			l.emit(&IRMul{Dst: prod, Src1: newBorrow, Src2: freshBorrow})
			l.emit(&IRSub{Dst: borrow, Src1: combined, Src2: prod})
			l.freeCell(prod)
			l.freeCell(combined)
			l.freeCell(freshBorrow)
			l.freeCell(newBorrow)
		}
		l.emit(&IRIf{Cond: cond, Then: &IRBlock{Nodes: []IRNode{
			&IRSubI{Dst: r + j, Value: 1},
		}}})
		l.freeCell(cond)
	}
	l.freeCell(borrow)
}

// emitMulInt multiplies two n-byte integers into an n-byte result.
// For each byte b[k] of the multiplier, add a << (k*8) repeated b[k] times.
func (l *Lowerer) emitMulInt(r, a, b Cell, n int) {
	for j := range n {
		l.emit(&IRZero{Dst: r + j})
	}
	// Schoolbook multiplication: for each byte pair (a[i], b[j]),
	// add a[i] to r[i+j] exactly b[j] times with carry propagation.
	for j := range n {
		for i := range n {
			if i+j >= n {
				break // overflow beyond result width
			}
			cnt := l.allocCell()
			l.emit(&IRCopy{Dst: cnt, Src: b + j})
			saved := l.nodes
			l.nodes = nil
			// r[i+j] += a[i], with carry to r[i+j+1..].
			old := l.allocCell()
			l.emit(&IRCopy{Dst: old, Src: r + i + j})
			tmp := l.allocCell()
			l.emit(&IRAdd{Dst: tmp, Src1: r + i + j, Src2: a + i})
			l.emit(&IRMove{Dst: r + i + j, Src: tmp})
			l.freeCell(tmp)
			if i+j < n-1 {
				carry := l.allocCell()
				l.emit(&IRCmp{Op: CmpLt, Dst: carry, Src1: r + i + j, Src2: old})
				// Propagate carry through higher bytes. Inside the IF body
				// the carry is known to be 1, so the next-higher carry is
				// just `r[k] == 0` after the increment -- no need to AND
				// with the old carry. IRIf zeroes its cond, so we pass
				// carry directly and replace it with newCarry each step.
				for k := i + j + 1; k < n; k++ {
					if k < n-1 {
						newCarry := l.allocCell()
						l.emit(&IRZero{Dst: newCarry})
						l.emit(&IRIf{Cond: carry, Then: &IRBlock{Nodes: []IRNode{
							&IRAddI{Dst: r + k, Value: 1},
							&IRNot{Dst: newCarry, Src: r + k},
						}}})
						l.freeCell(carry)
						carry = newCarry
					} else {
						l.emit(&IRIf{Cond: carry, Then: &IRBlock{Nodes: []IRNode{
							&IRAddI{Dst: r + k, Value: 1},
						}}})
					}
				}
				l.freeCell(carry)
			}
			l.freeCell(old)
			l.emit(&IRSubI{Dst: cnt, Value: 1})
			body := &IRBlock{Nodes: l.nodes}
			l.nodes = saved
			l.emit(&IRLoop{Cond: cnt, Body: body})
			l.freeCell(cnt)
		}
	}
}

// emitDivModInt computes a / b (or a % b) for n-byte integers by delegating
// to emitDivModIntFused and discarding the unused output.
func (l *Lowerer) emitDivModInt(r, a, b Cell, n int, isMod bool) {
	discard := l.allocCells(n)
	if isMod {
		l.emitDivModIntFused(discard, r, a, b, n)
	} else {
		l.emitDivModIntFused(r, discard, a, b, n)
	}
	l.freeCellRange(discard, n)
}

// emitDivModIntFused computes both a / b and a % b for n-byte integers using
// bit-by-bit schoolbook long division. The quotient and remainder are computed
// in a single pass over 8*n iterations regardless of the input values, which
// is much faster than repeated subtraction when the quotient is large.
//
// Algorithm: a 2n-byte combined register RQ starts as (R=0, Q=a). Each
// iteration shifts RQ left by one bit. The bit shifted out of R is held as
// `over`. After the shift, if over is set or R >= b, then R -= b (mod 2^(8n))
// and the new low bit of Q is set to 1. After 8*n iterations, R holds the
// remainder and Q holds the quotient.
func (l *Lowerer) emitDivModIntFused(quotDst, remDst, a, b Cell, n int) {
	// Allocate Q and R contiguously as a single 2n-byte buffer (Q low, R high)
	// so the combined left-shift can walk the cells in a single pass.
	combined := l.allocCells(2 * n)
	quot := combined
	rem := combined + n
	for j := range n {
		l.emit(&IRCopy{Dst: quot + j, Src: a + j})
		l.emit(&IRZero{Dst: rem + j})
	}
	counter := l.allocCell()
	l.emit(&IRConst{Dst: counter, Value: byte(8 * n)}) // #nosec G115
	saved := l.nodes
	l.nodes = nil
	// Save high bit of R[n-1] before the shift discards it.
	over := l.allocCell()
	c128 := l.allocCell()
	l.emit(&IRConst{Dst: c128, Value: 128})
	l.emit(&IRCmp{Op: CmpGeq, Dst: over, Src1: rem + n - 1, Src2: c128})
	l.freeCell(c128)
	// Shift the combined 2n-byte RQ register left by one bit.
	l.emitShiftLeftIntByOne(combined, 2*n)
	// should = over OR (R >= b). If true, R -= b and Q[0] |= 1.
	cmp := l.allocCell()
	l.emitCmpGeqInt(cmp, rem, b, n)
	sum := l.allocCell()
	prod := l.allocCell()
	l.emit(&IRAdd{Dst: sum, Src1: over, Src2: cmp})
	l.emit(&IRMul{Dst: prod, Src1: over, Src2: cmp})
	should := l.allocCell()
	l.emit(&IRSub{Dst: should, Src1: sum, Src2: prod})
	l.freeCell(prod)
	l.freeCell(sum)
	l.freeCell(cmp)
	l.freeCell(over)
	thenSaved := l.nodes
	l.nodes = nil
	l.emitSubIntInPlace(rem, b, n)
	l.emit(&IRAddI{Dst: quot, Value: 1})
	thenBlock := &IRBlock{Nodes: l.nodes}
	l.nodes = thenSaved
	l.emit(&IRIf{Cond: should, Then: thenBlock})
	l.freeCell(should)
	l.emit(&IRSubI{Dst: counter, Value: 1})
	body := &IRBlock{Nodes: l.nodes}
	l.nodes = saved
	l.emit(&IRLoop{Cond: counter, Body: body})
	l.freeCell(counter)
	for j := range n {
		l.emit(&IRMove{Dst: quotDst + j, Src: quot + j})
		l.emit(&IRMove{Dst: remDst + j, Src: rem + j})
	}
	l.freeCellRange(combined, 2*n)
}

// emitSubIntInPlace subtracts b from a in place for n-byte integers.
func (l *Lowerer) emitSubIntInPlace(a, b Cell, n int) {
	// General N-byte in-place subtraction with chained borrow.
	borrow := l.allocCell()
	l.emit(&IRCmp{Op: CmpLt, Dst: borrow, Src1: a, Src2: b})
	newVal := l.allocCell()
	l.emit(&IRSub{Dst: newVal, Src1: a, Src2: b})
	l.emit(&IRMove{Dst: a, Src: newVal})
	l.freeCell(newVal)
	for j := 1; j < n; j++ {
		// Save a[j] before modification for borrow detection.
		old := l.allocCell()
		l.emit(&IRCopy{Dst: old, Src: a + j})
		// a[j] -= b[j]
		nv := l.allocCell()
		l.emit(&IRSub{Dst: nv, Src1: a + j, Src2: b + j})
		l.emit(&IRMove{Dst: a + j, Src: nv})
		l.freeCell(nv)
		// Apply borrow from previous byte.
		cond := l.allocCell()
		l.emit(&IRCopy{Dst: cond, Src: borrow})
		l.emit(&IRIf{Cond: cond, Then: &IRBlock{Nodes: []IRNode{
			&IRSubI{Dst: a + j, Value: 1},
		}}})
		l.freeCell(cond)
		if j < n-1 {
			// Compute new borrow: (old < b[j]) OR (borrow AND a[j] wrapped to 255).
			freshBorrow := l.allocCell()
			l.emit(&IRCmp{Op: CmpLt, Dst: freshBorrow, Src1: old, Src2: b + j})
			// Check if borrow-subtract wrapped (a[j]==255 and borrow was set).
			c255 := l.allocCell()
			l.emit(&IRConst{Dst: c255, Value: 255})
			wrapped := l.allocCell()
			l.emit(&IRCmp{Op: CmpEq, Dst: wrapped, Src1: a + j, Src2: c255})
			l.freeCell(c255)
			l.emit(&IRMul{Dst: wrapped, Src1: wrapped, Src2: borrow})
			// borrow = freshBorrow OR wrapped
			combined := l.allocCell()
			l.emit(&IRAdd{Dst: combined, Src1: freshBorrow, Src2: wrapped})
			prod := l.allocCell()
			l.emit(&IRMul{Dst: prod, Src1: freshBorrow, Src2: wrapped})
			l.emit(&IRSub{Dst: borrow, Src1: combined, Src2: prod})
			l.freeCell(prod)
			l.freeCell(combined)
			l.freeCell(wrapped)
			l.freeCell(freshBorrow)
		}
		l.freeCell(old)
	}
	l.freeCell(borrow)
}

// emitShiftInt shifts an n-byte integer left (right=false) or right (right=true)
// by cnt bits (cnt is a byte cell). Splits cnt into a whole-byte shift and a
// sub-byte bit shift via divmod by 8, running the cheap whole-byte loop first
// and the bit-by-bit loop only for the remainder. This is much faster than 8*N
// single-bit shifts when cnt is large (e.g., uint64 << 56 takes 7 byte-shifts
// instead of 56 bit-shifts).
func (l *Lowerer) emitShiftInt(r, a, cnt Cell, n int, right bool) {
	for j := range n {
		l.emit(&IRCopy{Dst: r + j, Src: a + j})
	}
	byteCount := l.allocCell()
	bitCount := l.allocCell()
	eight := l.allocCell()
	l.emit(&IRConst{Dst: eight, Value: 8})
	l.emit(&IRDivMod{Src1: cnt, Src2: eight, QuotDst: byteCount, RemDst: bitCount})
	l.freeCell(eight)

	main := l.nodes
	l.nodes = nil
	if right {
		l.emitShiftRightIntByByte(r, n)
	} else {
		l.emitShiftLeftIntByByte(r, n)
	}
	l.emit(&IRSubI{Dst: byteCount, Value: 1})
	byteBody := &IRBlock{Nodes: l.nodes}
	l.nodes = main
	l.emit(&IRLoop{Cond: byteCount, Body: byteBody})
	l.freeCell(byteCount)

	main = l.nodes
	l.nodes = nil
	if right {
		l.emitShiftRightIntByOne(r, n)
	} else {
		l.emitShiftLeftIntByOne(r, n)
	}
	l.emit(&IRSubI{Dst: bitCount, Value: 1})
	bitBody := &IRBlock{Nodes: l.nodes}
	l.nodes = main
	l.emit(&IRLoop{Cond: bitCount, Body: bitBody})
	l.freeCell(bitCount)
}

// emitShiftLeftIntByByte shifts an n-byte little-endian integer left by 8 bits
// (one whole byte) in place. The high byte is discarded; the low byte becomes 0.
func (l *Lowerer) emitShiftLeftIntByByte(a Cell, n int) {
	// Walk high to low so each byte is read before being overwritten by the
	// next-lower one.
	for j := n - 1; j > 0; j-- {
		l.emit(&IRMove{Dst: a + j, Src: a + j - 1})
	}
	l.emit(&IRZero{Dst: a})
}

// emitShiftLeftIntByOne shifts an n-byte little-endian integer left by one bit
// in place. The bit shifted out of the high byte is discarded.
func (l *Lowerer) emitShiftLeftIntByOne(a Cell, n int) {
	// Walk from high byte to low so each byte is read for its outgoing carry
	// before being doubled.
	for j := n - 1; j >= 0; j-- {
		if j > 0 {
			carry := l.allocCell()
			c128 := l.allocCell()
			l.emit(&IRConst{Dst: c128, Value: 128})
			l.emit(&IRCmp{Op: CmpGeq, Dst: carry, Src1: a + j - 1, Src2: c128})
			l.freeCell(c128)
			newVal := l.allocCell()
			l.emit(&IRAdd{Dst: newVal, Src1: a + j, Src2: a + j})
			l.emit(&IRMove{Dst: a + j, Src: newVal})
			l.freeCell(newVal)
			l.emit(&IRIf{Cond: carry, Then: &IRBlock{Nodes: []IRNode{
				&IRAddI{Dst: a + j, Value: 1},
			}}})
			l.freeCell(carry)
		} else {
			newVal := l.allocCell()
			l.emit(&IRAdd{Dst: newVal, Src1: a, Src2: a})
			l.emit(&IRMove{Dst: a, Src: newVal})
			l.freeCell(newVal)
		}
	}
}

// emitShiftRightIntByByte shifts an n-byte little-endian integer right by 8 bits
// (one whole byte) in place. The low byte is discarded; the high byte becomes 0.
func (l *Lowerer) emitShiftRightIntByByte(a Cell, n int) {
	// Walk low to high so each byte is read before being overwritten by the
	// next-higher one.
	for j := 0; j < n-1; j++ {
		l.emit(&IRMove{Dst: a + j, Src: a + j + 1})
	}
	l.emit(&IRZero{Dst: a + n - 1})
}

// emitShiftRightIntByOne shifts an n-byte little-endian integer right by one bit
// in place. The bit shifted out of the low byte is discarded.
func (l *Lowerer) emitShiftRightIntByOne(r Cell, n int) {
	for j := range n {
		if j < n-1 {
			// carry = low bit of r[j+1] (will become high bit of r[j] after shift)
			carry := l.allocCell()
			one := l.allocCell()
			l.emit(&IRConst{Dst: one, Value: 1})
			l.emit(&IRAnd{Dst: carry, Src1: r + j + 1, Src2: one})
			l.freeCell(one)
			newVal := l.allocCell()
			two := l.allocCell()
			l.emit(&IRConst{Dst: two, Value: 2})
			l.emit(&IRDivMod{Src1: r + j, Src2: two, QuotDst: newVal, RemDst: two})
			l.emit(&IRMove{Dst: r + j, Src: newVal})
			l.freeCell(newVal)
			l.freeCell(two)
			l.emit(&IRIf{Cond: carry, Then: &IRBlock{Nodes: []IRNode{
				&IRAddI{Dst: r + j, Value: 128},
			}}})
			l.freeCell(carry)
		} else {
			newVal := l.allocCell()
			two := l.allocCell()
			l.emit(&IRConst{Dst: two, Value: 2})
			l.emit(&IRDivMod{Src1: r + j, Src2: two, QuotDst: newVal, RemDst: two})
			l.emit(&IRMove{Dst: r + j, Src: newVal})
			l.freeCell(newVal)
			l.freeCell(two)
		}
	}
}

// emitCmpInt compares two n-byte integers. Writes 0 or 1 to dst.
func (l *Lowerer) emitCmpInt(dst Cell, op token.Token, a, b Cell, n int) {
	switch op {
	case token.EQL:
		l.emit(&IRConst{Dst: dst, Value: 1})
		for j := range n {
			eq := l.allocCell()
			l.emit(&IRCmp{Op: CmpEq, Dst: eq, Src1: a + j, Src2: b + j})
			l.emit(&IRMul{Dst: dst, Src1: dst, Src2: eq})
			l.freeCell(eq)
		}
	case token.NEQ:
		l.emit(&IRConst{Dst: dst, Value: 1})
		for j := range n {
			eq := l.allocCell()
			l.emit(&IRCmp{Op: CmpEq, Dst: eq, Src1: a + j, Src2: b + j})
			l.emit(&IRMul{Dst: dst, Src1: dst, Src2: eq})
			l.freeCell(eq)
		}
		l.emit(&IRNot{Dst: dst, Src: dst})
	case token.LSS:
		l.emitCmpLtInt(dst, a, b, n)
	case token.GTR:
		l.emitCmpLtInt(dst, b, a, n)
	case token.LEQ:
		l.emitCmpGeqInt(dst, b, a, n) // a <= b iff b >= a
	case token.GEQ:
		l.emitCmpGeqInt(dst, a, b, n)
	}
}

// emitCmpLtInt: dst = (a < b) for n-byte integers.
func (l *Lowerer) emitCmpLtInt(dst, a, b Cell, n int) {
	l.emitCmpOrderInt(dst, a, b, n, CmpLt, 0)
}

// emitCmpGeqInt: dst = (a >= b) for n-byte integers.
func (l *Lowerer) emitCmpGeqInt(dst, a, b Cell, n int) {
	l.emitCmpOrderInt(dst, a, b, n, CmpGeq, 1)
}

// emitCmpOrderInt computes dst = (a `op` b) for n-byte integers, where op is
// CmpLt or CmpGeq. Walks bytes high to low; once the first non-equal pair is
// found, sets dst to that pair's comparison and skips the rest via a runtime
// `done` flag. Sequential per-byte IRIfs (rather than recursive nesting) keep
// the codegen's live-cell pressure low for wider widths like uint64.
//
// initVal is the value of dst when all bytes are equal: 1 for CmpGeq, 0 for CmpLt.
func (l *Lowerer) emitCmpOrderInt(dst, a, b Cell, n int, op CmpOp, initVal byte) {
	if n == 1 {
		l.emit(&IRCmp{Op: op, Dst: dst, Src1: a, Src2: b})
		return
	}
	l.emit(&IRConst{Dst: dst, Value: initVal})
	done := l.allocCell()
	l.emit(&IRZero{Dst: done})
	for j := n - 1; j >= 0; j-- {
		// cond = !done AND a[j] != b[j]
		notDone := l.allocCell()
		l.emit(&IRCopy{Dst: notDone, Src: done})
		l.emit(&IRNot{Dst: notDone, Src: notDone})
		eq := l.allocCell()
		l.emit(&IRCmp{Op: CmpEq, Dst: eq, Src1: a + j, Src2: b + j})
		l.emit(&IRNot{Dst: eq, Src: eq})
		cond := l.allocCell()
		l.emit(&IRMul{Dst: cond, Src1: notDone, Src2: eq})
		l.freeCell(eq)
		l.freeCell(notDone)

		saved := l.nodes
		l.nodes = nil
		l.emit(&IRCmp{Op: op, Dst: dst, Src1: a + j, Src2: b + j})
		l.emit(&IRConst{Dst: done, Value: 1})
		body := &IRBlock{Nodes: l.nodes}
		l.nodes = saved
		l.emit(&IRIf{Cond: cond, Then: body})
		l.freeCell(cond)
	}
	l.freeCell(done)
}

func (l *Lowerer) lowerCallExpr(call *ast.CallExpr) (exprResult, error) {
	if r, ok, err := l.lowerCallExprWith(call, l.lowerExpr); ok {
		return r, err
	}
	funcName, receiver := l.resolveCall(call)
	if funcName == "" {
		return exprResult{}, fmt.Errorf("unsupported function call expression")
	}
	info, ok := l.result.Funcs[funcName]
	if !ok {
		return exprResult{}, fmt.Errorf("unsupported function in expression: %s", funcName)
	}
	if info.Returns == 0 {
		return exprResult{}, fmt.Errorf("function %s has no return value", funcName)
	}
	args := l.prependReceiver(receiver, info, call.Args)
	retCells, err := l.inlineCall(info, args)
	if err != nil {
		return exprResult{}, err
	}
	// Composite return: return all cells with array/struct metadata.
	if info.ReturnType.ArraySize > 0 {
		if info.ReturnType.IsPointer {
			return exprResult{
				cell: retCells[0], temp: true, isPointer: true,
				elemSize: 1, elemCount: info.ReturnType.ArraySize,
			}, nil
		}
		return exprResult{
			cell: retCells[0], temp: true, size: info.ReturnType.ArraySize,
			elemSize: 1, elemCount: info.ReturnType.ArraySize,
		}, nil
	}
	if info.ReturnType.StructType != "" {
		if info.ReturnType.IsPointer {
			return exprResult{cell: retCells[0], temp: true, isPointer: true, typeName: info.ReturnType.StructType}, nil
		}
		def := l.result.Structs[info.ReturnType.StructType]
		return exprResult{cell: retCells[0], temp: true, size: def.Size, typeName: info.ReturnType.StructType}, nil
	}
	if n := info.ReturnType.IntSize; n >= 2 {
		return exprResult{cell: retCells[0], temp: true, size: n, intSize: n}, nil
	}
	if info.ReturnType.IsSlice {
		return exprResult{
			cell: retCells[0], temp: true,
			elemSize:    max(info.ReturnType.SliceElemSize, 1),
			elemCount:   255,
			elemType:    info.ReturnType.SliceElemType,
			elemIntSize: info.ReturnType.SliceElemIntSize,
			elemSlice:   info.ReturnType.SliceElemSlice,
			isPointer:   true,
			lenCell:     retCells[1],
			capCell:     retCells[2],
		}, nil
	}
	// Scalar return.
	for i := 1; i < len(retCells); i++ {
		l.freeCell(retCells[i])
	}
	return exprResult{cell: retCells[0], temp: true}, nil
}

func (l *Lowerer) lowerCallExprWith(call *ast.CallExpr, lowerExpr func(ast.Expr) (exprResult, error)) (exprResult, bool, error) {
	fn, ok := call.Fun.(*ast.Ident)
	if !ok {
		return exprResult{}, false, nil
	}
	switch fn.Name {
	case "byte", "uint8":
		if len(call.Args) != 1 {
			return exprResult{}, true, fmt.Errorf("%s() expects 1 argument", fn.Name)
		}
		r, err := lowerExpr(call.Args[0])
		if err != nil {
			return exprResult{}, true, err
		}
		// Multi-byte int -> byte: truncate to low byte.
		if r.intSize > 1 {
			if r.temp {
				for j := 1; j < r.intSize; j++ {
					l.freeCell(r.cell + j)
				}
			}
			return exprResult{cell: r.cell, temp: r.temp}, true, nil
		}
		return r, true, err
	case "uint16", "uint32", "uint64":
		n := intIdentSize(fn.Name)
		if len(call.Args) != 1 {
			return exprResult{}, true, fmt.Errorf("%s() expects 1 argument", fn.Name)
		}
		// Handle integer literal directly.
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.INT {
			val, err := strconv.ParseUint(lit.Value, 0, 64)
			if err != nil {
				return exprResult{}, true, err
			}
			maxVal := uint64(1)<<(n*8) - 1
			if val > maxVal {
				return exprResult{}, true, fmt.Errorf("integer literal %d out of %s range (0-%d)", val, fn.Name, maxVal)
			}
			base := l.allocCells(n)
			for j := range n {
				l.emit(&IRConst{Dst: base + j, Value: byte(val >> (j * 8))}) // #nosec G115
			}
			return exprResult{cell: base, temp: true, size: n, intSize: n}, true, nil
		}
		r, err := lowerExpr(call.Args[0])
		if err != nil {
			return exprResult{}, true, err
		}
		// Truncate wider integer.
		if r.intSize >= n {
			if r.intSize > n && r.temp {
				for j := n; j < r.intSize; j++ {
					l.freeCell(r.cell + j)
				}
			}
			return exprResult{cell: r.cell, temp: r.temp, size: n, intSize: n}, true, nil
		}
		// Zero-extend smaller integer.
		base := l.allocCells(n)
		srcSize := max(r.intSize, 1)
		for j := range srcSize {
			if r.temp {
				l.emit(&IRMove{Dst: base + j, Src: r.cell + j})
			} else {
				l.emit(&IRCopy{Dst: base + j, Src: r.cell + j})
			}
		}
		for j := srcSize; j < n; j++ {
			l.emit(&IRZero{Dst: base + j})
		}
		if r.temp {
			// IRMove already zeroed source cells; free them.
			// For byte (intSize 0), free 1 cell. For wider, free all.
			l.freeCellRange(r.cell, srcSize)
		}
		return exprResult{cell: base, temp: true, size: n, intSize: n}, true, nil
	case "len", "cap":
		if len(call.Args) != 1 {
			return exprResult{}, true, fmt.Errorf("%s() expects 1 argument", fn.Name)
		}
		arg := call.Args[0]
		if star, ok := arg.(*ast.StarExpr); ok {
			arg = star.X // len(*ptr) -> len(ptr)
		}
		r, err := lowerExpr(arg)
		if err != nil {
			return exprResult{}, true, err
		}
		// Slice: len/cap are runtime values from the header.
		if r.lenCell != 0 {
			t := l.allocCell()
			if fn.Name == "len" {
				l.emit(&IRCopy{Dst: t, Src: r.lenCell})
			} else {
				l.emit(&IRCopy{Dst: t, Src: r.capCell})
			}
			if r.temp {
				l.freeCell(r.cell)
				l.freeCell(r.lenCell)
				l.freeCell(r.capCell)
			}
			return exprResult{cell: t, temp: true}, true, nil
		}
		if r.elemCount == 0 {
			return exprResult{}, true, fmt.Errorf("%s() argument must be an array", fn.Name)
		}
		if r.temp {
			l.freeCellRange(r.cell, r.cellCount())
		}
		t := l.allocCell()
		l.emit(&IRConst{Dst: t, Value: byte(r.elemCount)}) // #nosec G115
		return exprResult{cell: t, temp: true}, true, nil
	case "copy":
		if len(call.Args) != 2 {
			return exprResult{}, true, fmt.Errorf("copy() expects 2 arguments")
		}
		dst, err := lowerExpr(call.Args[0])
		if err != nil || dst.lenCell == 0 {
			return exprResult{}, true, fmt.Errorf("copy expects slice arguments")
		}
		src, err := lowerExpr(call.Args[1])
		if err != nil || src.lenCell == 0 {
			return exprResult{}, true, fmt.Errorf("copy expects slice arguments")
		}
		n := l.emitCopy(dst, src)
		return exprResult{cell: n, temp: true}, true, nil
	case "min", "max":
		if len(call.Args) < 2 {
			return exprResult{}, true, fmt.Errorf("%s() expects at least 2 arguments", fn.Name)
		}
		cmpOp := CmpLeq
		if fn.Name == "max" {
			cmpOp = CmpGeq
		}
		r, err := lowerExpr(call.Args[0])
		if err != nil {
			return exprResult{}, true, err
		}
		// Multi-byte path: keep an N-cell running result and use N-byte compare.
		if r.intSize >= 2 {
			n := r.intSize
			t := l.allocCells(n)
			l.emitCopyOrMove(t, r)
			for _, arg := range call.Args[1:] {
				r, err := lowerExpr(arg)
				if err != nil {
					return exprResult{}, true, err
				}
				if r.intSize != n {
					if r.temp {
						l.freeCellRange(r.cell, r.cellCount())
					}
					l.freeCellRange(t, n)
					return exprResult{}, true, fmt.Errorf("%s: mismatched integer sizes", fn.Name)
				}
				cond := l.allocCell()
				if cmpOp == CmpLeq {
					l.emitCmpGeqInt(cond, t, r.cell, n) // r <= t iff t >= r
				} else {
					l.emitCmpLtInt(cond, t, r.cell, n) // r >= t iff t < r
				}
				body := []IRNode{}
				for j := range n {
					body = append(body, &IRCopy{Dst: t + j, Src: r.cell + j})
				}
				l.emit(&IRIf{Cond: cond, Then: &IRBlock{Nodes: body}})
				l.freeCell(cond)
				if r.temp {
					l.freeCellRange(r.cell, n)
				}
			}
			return exprResult{cell: t, temp: true, size: n, intSize: n}, true, nil
		}
		t := l.allocCell()
		l.emitCopyOrMove(t, r)
		for _, arg := range call.Args[1:] {
			r, err := lowerExpr(arg)
			if err != nil {
				return exprResult{}, true, err
			}
			cond := l.allocCell()
			l.emit(&IRCmp{Op: cmpOp, Dst: cond, Src1: r.cell, Src2: t})
			l.emit(&IRIf{Cond: cond, Then: &IRBlock{Nodes: []IRNode{
				&IRCopy{Dst: t, Src: r.cell},
			}}})
			l.freeCell(cond)
			if r.temp {
				l.freeCell(r.cell)
			}
		}
		return exprResult{cell: t, temp: true}, true, nil
	case "getchar":
		if len(call.Args) != 0 {
			return exprResult{}, true, fmt.Errorf("getchar expects 0 arguments")
		}
		t := l.allocCell()
		l.emit(&IRGetc{Dst: t})
		return exprResult{cell: t, temp: true}, true, nil
	default:
		return exprResult{}, false, nil
	}
}

func (l *Lowerer) lowerIndexExpr(e *ast.IndexExpr) (exprResult, error) {
	// Evaluate the base expression and index into it.
	base, err := l.lowerExpr(e.X)
	if err != nil {
		return exprResult{}, err
	}
	if base.elemCount == 0 {
		depth := 0
		for x := ast.Expr(e); ; depth++ {
			if idx, ok := x.(*ast.IndexExpr); ok {
				x = idx.X
			} else {
				break
			}
		}
		if depth > 3 {
			return exprResult{}, fmt.Errorf("array nesting deeper than 3 levels is not supported")
		}
		return exprResult{}, fmt.Errorf("cannot index non-array expression")
	}
	return l.indexInto(base, e.Index)
}

// indexInto indexes a composite result by the given expression.
// The base must have elemSize and elemCount set.
func (l *Lowerer) indexInto(base exprResult, indexExpr ast.Expr) (exprResult, error) {
	if base.isPointer {
		idx, err := l.ptrDynIndex(base.cell, indexExpr, base.elemSize)
		if err != nil {
			return exprResult{}, err
		}
		if base.elemSize == 1 {
			result := l.ptrLoad(idx)
			l.freeCell(idx)
			r := exprResult{cell: result, temp: true}
			if base.elemPtrType != "" {
				r.isPointer = true
				r.typeName = base.elemPtrType
				def := l.result.Structs[base.elemPtrType]
				r.elemSize = 1
				r.elemCount = def.Size
			} else if base.elemType != "" {
				// Size-1 struct element: the loaded byte IS the only field's
				// value. typeName lets .field selectors resolve.
				r.typeName = base.elemType
			}
			return r, nil
		}
		if base.elemSlice {
			// Slice-of-slices: load inner 3-cell header (ptr, len, cap).
			inner := l.allocSliceInfo()
			tmpPtr := l.ptrLoad(idx)
			l.emit(&IRMove{Dst: inner.ptr, Src: tmpPtr})
			l.freeCell(tmpPtr)
			l.emit(&IRAddI{Dst: idx, Value: 1})
			tmpLen := l.ptrLoad(idx)
			l.emit(&IRMove{Dst: inner.len, Src: tmpLen})
			l.freeCell(tmpLen)
			l.emit(&IRAddI{Dst: idx, Value: 1})
			tmpCap := l.ptrLoad(idx)
			l.emit(&IRMove{Dst: inner.cap, Src: tmpCap})
			l.freeCell(tmpCap)
			l.freeCell(idx)
			return exprResult{cell: inner.ptr, temp: true, elemSize: 1,
				elemCount: 255, isPointer: true, lenCell: inner.len, capCell: inner.cap}, nil
		}
		// Multi-byte int element: materialize into a temp by loading N bytes.
		if base.elemIntSize >= 2 {
			n := base.elemIntSize
			dst := l.allocCells(n)
			for j := range n {
				val := l.ptrLoad(idx)
				l.emit(&IRMove{Dst: dst + j, Src: val})
				l.freeCell(val)
				if j < n-1 {
					l.emit(&IRAddI{Dst: idx, Value: 1})
				}
			}
			l.freeCell(idx)
			return exprResult{cell: dst, temp: true, size: n, intSize: n}, nil
		}
		// Multi-byte struct element: return pointer to sub-array.
		return exprResult{cell: idx, temp: true, size: base.elemSize, elemSize: 1,
			elemCount: base.elemSize, elemType: base.elemType, typeName: base.elemType, isPointer: true}, nil
	}

	// Flat-offset result: cell holds i*elemSize relative to flatBase.
	if base.flatBase != 0 {
		if base.elemSize > 1 {
			// Nested: compute deeper flat offset.
			es := l.allocCell()
			l.emit(&IRConst{Dst: es, Value: byte(base.elemSize)}) // #nosec G115
			idxR, err := l.lowerExpr(indexExpr)
			if err != nil {
				return exprResult{}, err
			}
			t := l.allocCell()
			l.emit(&IRMul{Dst: t, Src1: idxR.cell, Src2: es})
			l.freeCell(es)
			if idxR.temp {
				l.freeCell(idxR.cell)
			}
			l.emit(&IRAdd{Dst: base.cell, Src1: base.cell, Src2: t})
			l.freeCell(t)
			return exprResult{cell: base.cell, temp: true, elemSize: 1,
				elemCount: base.elemSize, elemType: base.elemType,
				typeName: base.elemType, flatBase: base.flatBase}, nil
		}
		// Scalar access on flat array.
		totalSize := base.elemCount * base.elemSize
		flatArr := arrayInfo{base: base.flatBase, size: totalSize, count: totalSize, elemSize: 1}
		flatIdx, err := l.addFlatOffset(base.cell, indexExpr)
		if err != nil {
			return exprResult{}, err
		}
		result := l.allocCell()
		l.emitVariableIndexRead(flatArr, flatIdx, result)
		l.freeCell(flatIdx)
		return exprResult{cell: result, temp: true}, nil
	}

	// Constant index: direct cell access.
	if constIdx, ok := l.constValue(indexExpr); ok {
		if constIdx >= base.elemCount {
			return exprResult{}, fmt.Errorf("array index %d out of bounds [0:%d]", constIdx, base.elemCount)
		}
		cell := base.cell + constIdx*base.elemSize
		// Multi-byte int element: return a non-temp uint16/uint32/uint64 view.
		if base.elemIntSize >= 2 {
			return exprResult{cell: cell, size: base.elemIntSize, intSize: base.elemIntSize}, nil
		}
		// `[N]string` (or [N][]byte) element: return as string-shaped header.
		if base.elemSlice {
			return exprResult{cell: cell, elemSize: 1, elemCount: 255,
				isPointer: true, lenCell: cell + 1, capCell: cell + 2}, nil
		}
		r := exprResult{cell: cell, typeName: base.elemType}
		if base.elemSize > 1 {
			r.size = base.elemSize
			if base.innerElemSize > 0 {
				// Nested array: preserve inner structure.
				r.elemSize = base.innerElemSize
				r.elemCount = base.elemSize / base.innerElemSize
				r.elemType = base.elemType
				if base.innerElemIntSize >= 2 {
					r.elemIntSize = base.innerElemIntSize
				}
			} else {
				r.elemSize = 1
				r.elemCount = base.elemSize
			}
		}
		return r, nil
	}

	// Variable index on composite array: return flat offset i*elemSize
	// with sub-array info for chained indexing.
	ai := arrayInfo{
		base: base.cell, size: base.elemCount * base.elemSize,
		count: base.elemCount, elemSize: base.elemSize,
	}
	// Multi-byte int element with variable index: materialize into a temp
	// uint16/uint32/uint64 by reading N consecutive bytes from the flat array.
	if base.elemIntSize >= 2 {
		flatIdx, err := l.lowerCompositeVarIndex(ai, indexExpr)
		if err != nil {
			return exprResult{}, err
		}
		flatArr := arrayInfo{base: base.cell, size: ai.size, count: ai.size, elemSize: 1}
		dst := l.allocCells(base.elemIntSize)
		dsts := make([]Cell, base.elemIntSize)
		for j := range base.elemIntSize {
			dsts[j] = dst + Cell(j) // #nosec G115
		}
		l.loadConsecutiveViaIndex(flatArr, flatIdx.cell, dsts)
		l.freeCell(flatIdx.cell)
		return exprResult{cell: dst, temp: true, size: base.elemIntSize, intSize: base.elemIntSize}, nil
	}
	// `[N]string` / `[N][]byte` element with variable index: load 3 cells
	// (ptr/len/cap) into a fresh sliceInfo.
	if base.elemSlice {
		flatIdx, err := l.lowerCompositeVarIndex(ai, indexExpr)
		if err != nil {
			return exprResult{}, err
		}
		flatArr := arrayInfo{base: base.cell, size: ai.size, count: ai.size, elemSize: 1}
		si := l.allocSliceInfo()
		l.loadConsecutiveViaIndex(flatArr, flatIdx.cell, []Cell{si.ptr, si.len, si.cap})
		l.freeCell(flatIdx.cell)
		return exprResult{cell: si.ptr, temp: true, elemSize: 1, elemCount: 255,
			isPointer: true, lenCell: si.len, capCell: si.cap}, nil
	}
	if base.elemSize > 1 {
		r, err := l.lowerCompositeVarIndex(ai, indexExpr)
		if err != nil {
			return exprResult{}, err
		}
		if base.innerElemSize > 0 {
			r.elemSize = base.innerElemSize
			r.elemCount = base.elemSize / base.innerElemSize

			r.elemType = base.elemType
		} else {
			r.elemSize = 1
			r.elemCount = base.elemSize
		}
		r.typeName = base.elemType
		r.flatBase = base.cell
		return r, nil
	}
	// Variable index on scalar array: dynamic read.
	indexResult, err := l.lowerExpr(indexExpr)
	if err != nil {
		return exprResult{}, err
	}
	if indexResult.intSize >= 2 {
		if indexResult.temp {
			l.freeCellRange(indexResult.cell, indexResult.intSize)
		}
		return exprResult{}, fmt.Errorf("cannot use multi-byte integer as array index, use byte() to truncate")
	}
	result := l.allocCell()
	l.emitVariableIndexRead(ai, indexResult.cell, result)
	if indexResult.temp {
		l.freeCell(indexResult.cell)
	}
	return exprResult{cell: result, temp: true}, nil
}

// writeInto writes val into a composite at the given index.
// The base must have elemSize and elemCount set.
func (l *Lowerer) writeInto(base exprResult, indexExpr ast.Expr, val exprResult) error {
	if base.isPointer {
		idx, err := l.ptrDynIndex(base.cell, indexExpr, base.elemSize)
		if err != nil {
			return err
		}
		if val.isPointer && val.size > 1 {
			// Multi-cell pointer-to-pointer copy: read from val, write to idx.
			for j := range val.elemCount {
				t := l.ptrLoad(val.cell)
				l.ptrStore(idx, t)
				l.freeCell(t)
				if j < val.elemCount-1 {
					l.emit(&IRAddI{Dst: val.cell, Value: 1})
					l.emit(&IRAddI{Dst: idx, Value: 1})
				}
			}
			l.freeCell(val.cell)
		} else if val.size > 1 {
			// Multi-cell direct struct: write each cell via pointer store.
			for j := range val.size {
				l.ptrStore(idx, val.cell+j)
				if j < val.size-1 {
					l.emit(&IRAddI{Dst: idx, Value: 1})
				}
			}
		} else {
			t := l.allocCell()
			l.emitCopyOrMove(t, val)
			l.ptrStore(idx, t)
			l.freeCell(t)
		}
		l.freeCell(idx)
		return nil
	}
	// Flat-offset result: add inner offset and dynamic write.
	if base.flatBase != 0 {
		totalSize := base.elemCount * base.elemSize
		flatArr := arrayInfo{base: base.flatBase, size: totalSize, count: totalSize, elemSize: 1}
		flatIdx, err := l.addFlatOffset(base.cell, indexExpr)
		if err != nil {
			return err
		}
		l.emitVariableIndexWrite(flatArr, flatIdx, val.cell)
		if val.temp {
			l.freeCell(val.cell)
		}
		l.freeCell(flatIdx)
		return nil
	}
	// Constant index: direct cell write.
	if constIdx, ok := l.constValue(indexExpr); ok {
		if constIdx >= base.elemCount {
			return fmt.Errorf("array index %d out of bounds [0:%d]", constIdx, base.elemCount)
		}
		l.emitCopyOrMove(base.cell+constIdx*base.elemSize, val)
		return nil
	}
	// Variable index: dynamic write.
	ai := arrayInfo{
		base: base.cell, size: base.elemCount * base.elemSize,
		count: base.elemCount, elemSize: base.elemSize,
	}
	// Multi-byte int element: write N bytes via dynamic stores at sequential offsets.
	if base.elemIntSize >= 2 {
		flatIdx, err := l.lowerCompositeVarIndex(ai, indexExpr)
		if err != nil {
			return err
		}
		srcs := make([]Cell, base.elemIntSize)
		for j := range base.elemIntSize {
			srcs[j] = val.cell + Cell(j) // #nosec G115
		}
		l.storeConsecutiveViaIndex(ai, flatIdx.cell, srcs)
		l.freeCell(flatIdx.cell)
		if val.temp {
			l.freeCellRange(val.cell, val.cellCount())
		}
		return nil
	}
	indexResult, err := l.lowerExpr(indexExpr)
	if err != nil {
		return err
	}
	l.emitVariableIndexWrite(ai, indexResult.cell, val.cell)
	if val.temp {
		l.freeCell(val.cell)
	}
	if indexResult.temp {
		l.freeCell(indexResult.cell)
	}
	return nil
}

func (l *Lowerer) lowerSelectorExpr(e *ast.SelectorExpr) (exprResult, error) {
	// Resolve the base: a variable, a chained selector, or an array element.
	var base Cell
	var def *StructDef
	baseIsPointer := false
	switch x := e.X.(type) {
	case *ast.Ident:
		si, ok := l.lookupStruct(x.Name)
		if ok {
			base = si.base
			def = si.def
		} else if ptrDef, ok := l.lookupPtrType(x.Name); ok {
			// Pointer-to-struct: ptr.x -> load at *ptr + offset
			ptrCell, err := l.lookupVar(x.Name)
			if err != nil {
				return exprResult{}, err
			}
			idx := l.ptrOffset(ptrCell, ptrDef.Offsets[e.Sel.Name])
			// Array field: return pointer for indexing.
			if arrSize := ptrDef.FieldArraySizes[e.Sel.Name]; arrSize > 0 {
				return exprResult{cell: idx, temp: true, elemSize: 1, elemCount: arrSize, isPointer: true}, nil
			}
			// Multi-byte int field: load N cells.
			if n := ptrDef.FieldIntSizes[e.Sel.Name]; n >= 2 {
				return l.loadMultiByteIntViaPtr(idx, n), nil
			}
			// String field: load 3 cells (ptr, len, cap) into a fresh sliceInfo.
			if ptrDef.FieldStrings[e.Sel.Name] {
				return l.loadStringHeaderViaPtr(idx), nil
			}
			result := l.ptrLoad(idx)
			l.freeCell(idx)
			return exprResult{cell: result, temp: true}, nil
		} else {
			return exprResult{}, fmt.Errorf("undefined struct: %s", x.Name)
		}
	case *ast.SelectorExpr:
		// Chained: r.min.x -> resolve r.min first.
		inner, err := l.lowerSelectorExpr(x)
		if err != nil {
			return exprResult{}, err
		}
		base = inner.cell
		baseIsPointer = inner.isPointer
		if inner.typeName == "" {
			return exprResult{}, fmt.Errorf("field %s is not a struct", x.Sel.Name)
		}
		def = l.result.Structs[inner.typeName]
	case *ast.IndexExpr:
		inner, err := l.lowerExpr(x)
		if err != nil {
			return exprResult{}, err
		}
		if inner.typeName == "" {
			return exprResult{}, fmt.Errorf("indexed expression does not have struct elements")
		}
		def = l.result.Structs[inner.typeName]
		// Size-1 struct from a slice/array index: inner.cell is a temp
		// holding the only byte. The single field IS that byte.
		if !inner.isPointer && inner.flatBase == 0 && def.Size == 1 {
			if _, ok := def.Offsets[e.Sel.Name]; !ok {
				return exprResult{}, fmt.Errorf("unknown field %s in struct %s", e.Sel.Name, def.Name)
			}
			return exprResult{cell: inner.cell, temp: inner.temp}, nil
		}
		if inner.flatBase != 0 {
			// Variable index: flat offset + fieldOffset, dynamic load.
			offset, ok := def.Offsets[e.Sel.Name]
			if !ok {
				return exprResult{}, fmt.Errorf("unknown field %s in struct %s", e.Sel.Name, def.Name)
			}
			l.emit(&IRAddI{Dst: inner.cell, Value: byte(offset)}) // #nosec G115
			totalSize := inner.elemCount * inner.elemSize
			flatArr := arrayInfo{base: inner.flatBase, size: totalSize, count: totalSize, elemSize: 1}
			if n := def.FieldIntSizes[e.Sel.Name]; n >= 2 {
				base := l.allocCells(n)
				dsts := make([]Cell, n)
				for j := range n {
					dsts[j] = base + Cell(j) // #nosec G115
				}
				l.loadConsecutiveViaIndex(flatArr, inner.cell, dsts)
				l.freeCell(inner.cell)
				return exprResult{cell: base, temp: true, size: n, intSize: n}, nil
			}
			if def.FieldStrings[e.Sel.Name] {
				si := l.allocSliceInfo()
				l.loadConsecutiveViaIndex(flatArr, inner.cell, []Cell{si.ptr, si.len, si.cap})
				l.freeCell(inner.cell)
				return exprResult{cell: si.ptr, temp: true, elemSize: 1, elemCount: 255,
					isPointer: true, lenCell: si.len, capCell: si.cap}, nil
			}
			result := l.allocCell()
			l.emitVariableIndexRead(flatArr, inner.cell, result)
			l.freeCell(inner.cell)
			return exprResult{cell: result, temp: true}, nil
		}
		base = inner.cell
		baseIsPointer = inner.isPointer
	default:
		// Generic: evaluate e.X and resolve struct type.
		inner, err := l.lowerExpr(e.X)
		if err != nil {
			return exprResult{}, err
		}
		if inner.typeName == "" {
			return exprResult{}, fmt.Errorf("unsupported selector expression")
		}
		def = l.result.Structs[inner.typeName]
		base = inner.cell
	}
	offset, ok := def.Offsets[e.Sel.Name]
	if !ok {
		return exprResult{}, fmt.Errorf("unknown field %s in struct %s", e.Sel.Name, def.Name)
	}
	if baseIsPointer {
		idx := l.ptrOffset(base, offset)
		// Array field: return pointer for indexing.
		if arrSize := def.FieldArraySizes[e.Sel.Name]; arrSize > 0 {
			return exprResult{cell: idx, temp: true, elemSize: 1, elemCount: arrSize, isPointer: true}, nil
		}
		// Nested struct field: return pointer with struct type.
		if fieldType := def.FieldTypes[e.Sel.Name]; fieldType != "" {
			fieldDef := l.result.Structs[fieldType]
			return exprResult{cell: idx, temp: true, size: fieldDef.Size, elemSize: 1,
				elemCount: fieldDef.Size, typeName: fieldType, isPointer: true}, nil
		}
		// Multi-byte int field: load N cells.
		if n := def.FieldIntSizes[e.Sel.Name]; n >= 2 {
			return l.loadMultiByteIntViaPtr(idx, n), nil
		}
		// String field: load 3 cells (ptr, len, cap) into a fresh sliceInfo.
		if def.FieldStrings[e.Sel.Name] {
			return l.loadStringHeaderViaPtr(idx), nil
		}
		result := l.ptrLoad(idx)
		l.freeCell(idx)
		return exprResult{cell: result, temp: true}, nil
	}
	r := exprResult{cell: base + offset}
	if arrSize := def.FieldArraySizes[e.Sel.Name]; arrSize > 0 {
		r.size = arrSize
		if eis := def.FieldArrayElemIntSize[e.Sel.Name]; eis >= 2 {
			r.elemSize = eis
			r.elemCount = arrSize / eis
			r.elemIntSize = eis
		} else if ies := def.FieldInnerSizes[e.Sel.Name]; ies > 0 {
			r.elemSize = ies
			r.elemCount = arrSize / ies
		} else {
			r.elemSize = 1
			r.elemCount = arrSize
		}
	} else if fieldType := def.FieldTypes[e.Sel.Name]; fieldType != "" {
		fieldDef := l.result.Structs[fieldType]
		r.size = fieldDef.Size
		r.elemSize = 1
		r.elemCount = fieldDef.Size
		r.typeName = fieldType
	} else if intSize := def.FieldIntSizes[e.Sel.Name]; intSize >= 2 {
		r.size = intSize
		r.intSize = intSize
	} else if def.FieldStrings[e.Sel.Name] {
		// String field: 3-cell slice header at base+offset.
		r.elemSize = 1
		r.elemCount = 255
		r.isPointer = true
		r.lenCell = base + offset + 1
		r.capCell = base + offset + 2
	}
	return r, nil
}
