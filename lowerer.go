package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"maps"
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
	scopes    []*scope

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
type scope struct {
	vars     map[string]Cell
	consts   map[string]byte       // compile-time constants
	arrays   map[string]arrayInfo  // base cell and size
	structs  map[string]structInfo // base cell and field layout
	slices   map[string]sliceInfo  // slice header (ptr, len, cap)
	ptrType  map[string]string     // variable name -> pointed-to struct type name
	ptrArray map[string]arrayInfo  // variable name -> pointed-to array info
}

// sliceInfo holds the 3-cell header for a slice variable.
type sliceInfo struct {
	ptr         Cell   // cell holding stack slot index of first element
	len         Cell   // cell holding current length
	cap         Cell   // cell holding capacity
	elemSize    int    // cells per element (1 for byte)
	elemType    string // struct type name (empty for byte)
	elemSlice   bool   // true if element is a slice ([][]byte)
	elemPtrType string // struct type for pointer elements ([]*Point)
}

type arrayInfo struct {
	base          Cell
	size          int    // total cells (count * elemSize)
	count         int    // number of elements
	elemSize      int    // cells per element (1 for byte, >1 for struct)
	elemType      string // struct type name (empty for byte)
	innerElemSize int    // for nested arrays: cells per inner element (0 if flat)
}

type structInfo struct {
	base Cell
	def  *StructDef // field names, offsets, size
}

type exprResult struct {
	cell          Cell
	temp          bool   // if true, the caller should free this cell via freeCell
	size          int    // total number of cells; 0 means 1 (scalar)
	elemSize      int    // element size for indexable results; 0 means not indexable
	elemCount     int    // number of elements for indexable results
	elemType      string // struct type name for composite elements (empty for byte)
	elemSlice     bool   // true if elements are slices ([][]byte)
	elemPtrType   string // struct type for pointer elements ([]*Point)
	innerElemSize int    // for nested arrays: cells per inner element (0 if flat)
	typeName      string // struct type name of this result (empty for non-struct)
	isPointer     bool   // if true, cell is a pointer (slot index) for indirect access
	flatBase      Cell   // for flat-offset results: base of the original array
	lenCell       Cell   // runtime length cell (0 if compile-time elemCount)
	capCell       Cell   // runtime capacity cell (0 if not applicable)
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

	info := result.Funcs["main"]
	// Reserve slot 0 for heapPtr so that no user variable occupies slot 0.
	// This makes pointer value 0 a reliable nil sentinel.
	l.heapPtr = l.allocCell()
	l.pushScope()
	// Load top-level constants into scope.
	maps.Copy(l.currentScope().consts, result.ByteConsts)
	l.scanAndAllocLocals(info.Body)

	// Set up return flag only if the body contains return statements.
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
	if n == 1 {
		if r.cell == dst {
			if r.temp {
				l.freeCell(r.cell)
			}
			return
		}
		if r.temp {
			l.emit(&IRMove{Dst: dst, Src: r.cell})
			l.freeCell(r.cell)
		} else {
			l.emit(&IRCopy{Dst: dst, Src: r.cell})
		}
		return
	}
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
	l.scopes = append(l.scopes, &scope{
		vars:     make(map[string]Cell),
		consts:   make(map[string]byte),
		arrays:   make(map[string]arrayInfo),
		structs:  make(map[string]structInfo),
		slices:   make(map[string]sliceInfo),
		ptrType:  make(map[string]string),
		ptrArray: make(map[string]arrayInfo),
	})
}

func (l *Lowerer) popScope() {
	l.scopes = l.scopes[:len(l.scopes)-1]
}

func (l *Lowerer) currentScope() *scope {
	return l.scopes[len(l.scopes)-1]
}

func (l *Lowerer) defineVar(name string) Cell {
	c := l.allocCell()
	l.currentScope().vars[name] = c
	return c
}

func (l *Lowerer) lookupConst(name string) (byte, bool) {
	for i := len(l.scopes) - 1; i >= 0; i-- {
		if v, ok := l.scopes[i].consts[name]; ok {
			return v, true
		}
	}
	return 0, false
}

func (l *Lowerer) lookupStringConst(name string) string {
	return l.result.StringConsts[name]
}

func (l *Lowerer) lookupVar(name string) (Cell, error) {
	if name == "_" {
		// Blank identifier: allocate a disposable cell.
		return l.allocCell(), nil
	}
	for i := len(l.scopes) - 1; i >= 0; i-- {
		if c, ok := l.scopes[i].vars[name]; ok {
			return c, nil
		}
	}
	if _, ok := l.lookupArray(name); ok {
		return 0, fmt.Errorf("cannot use array %s as byte value", name)
	}
	if _, ok := l.lookupStruct(name); ok {
		return 0, fmt.Errorf("cannot use struct %s as byte value", name)
	}
	return 0, fmt.Errorf("undefined variable: %s", name)
}

func (l *Lowerer) lookupArray(name string) (arrayInfo, bool) {
	for i := len(l.scopes) - 1; i >= 0; i-- {
		if info, ok := l.scopes[i].arrays[name]; ok {
			return info, true
		}
	}
	return arrayInfo{}, false
}

func (l *Lowerer) lookupStruct(name string) (structInfo, bool) {
	for i := len(l.scopes) - 1; i >= 0; i-- {
		if info, ok := l.scopes[i].structs[name]; ok {
			return info, true
		}
	}
	return structInfo{}, false
}

func (l *Lowerer) lookupPtrType(name string) (*StructDef, bool) {
	for i := len(l.scopes) - 1; i >= 0; i-- {
		if typeName, ok := l.scopes[i].ptrType[name]; ok {
			if def, ok := l.result.Structs[typeName]; ok {
				return def, true
			}
		}
	}
	return nil, false
}

func (l *Lowerer) lookupPtrArray(name string) (arrayInfo, bool) {
	for i := len(l.scopes) - 1; i >= 0; i-- {
		if ai, ok := l.scopes[i].ptrArray[name]; ok {
			return ai, true
		}
	}
	return arrayInfo{}, false
}

func (l *Lowerer) lookupSlice(name string) (sliceInfo, bool) {
	for i := len(l.scopes) - 1; i >= 0; i-- {
		if si, ok := l.scopes[i].slices[name]; ok {
			return si, true
		}
	}
	return sliceInfo{}, false
}

func (l *Lowerer) defineSlice(sc *scope, name string, elemSize int, elemType string, elemSlice bool, elemPtrType ...string) sliceInfo {
	si := sliceInfo{
		ptr: l.allocCell(), len: l.allocCell(), cap: l.allocCell(),
		elemSize: elemSize, elemType: elemType, elemSlice: elemSlice,
	}
	if len(elemPtrType) > 0 {
		si.elemPtrType = elemPtrType[0]
	}
	sc.slices[name] = si
	return si
}

// isSliceType returns true if the type expression is a slice ([]T).
func isSliceType(expr ast.Expr) bool {
	at, ok := expr.(*ast.ArrayType)
	return ok && at.Len == nil
}

// sliceElemInfo returns (elemSize, elemType, isSliceOfSlice, ptrType) for a slice type.
// ptrType is non-empty for pointer-to-struct elements ([]*Point).
func (l *Lowerer) sliceElemInfo(expr ast.Expr) (int, string, bool, string) {
	at, ok := expr.(*ast.ArrayType)
	if !ok || at.Len != nil {
		return 1, "", false, ""
	}
	if id, ok := at.Elt.(*ast.Ident); ok {
		if def, ok := l.result.Structs[id.Name]; ok {
			return def.Size, id.Name, false, ""
		}
	}
	if size := arrayTypeSize(at.Elt); size > 0 {
		return size, "", false, ""
	}
	if isSliceType(at.Elt) {
		return 3, "", true, ""
	}
	// Pointer-to-struct: []*Point
	if star, ok := at.Elt.(*ast.StarExpr); ok {
		if id, ok := star.X.(*ast.Ident); ok {
			if _, ok := l.result.Structs[id.Name]; ok {
				return 1, "", false, id.Name
			}
		}
	}
	return 1, "", false, ""
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
		}
	case *ast.CompositeLit:
		if isSliceType(e.Type) {
			return l.evalSliceLiteral(e)
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
	// Copy to temp cells for mutable use.
	tmp := l.allocSliceInfo()
	tmp.elemSize = r.elemSize
	tmp.elemType = r.elemType
	tmp.elemSlice = r.elemSlice
	tmp.elemPtrType = r.elemPtrType
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
	es, et, esl, ept := l.sliceElemInfo(typeExpr)
	si.elemSize = es
	si.elemType = et
	si.elemSlice = esl
	si.elemPtrType = ept
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

func (l *Lowerer) evalSliceLiteral(comp *ast.CompositeLit) (sliceInfo, error) {
	si := l.allocSliceInfo()
	es, et, esl, ept := l.sliceElemInfo(comp.Type)
	si.elemSize = es
	si.elemType = et
	si.elemSlice = esl
	si.elemPtrType = ept
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
	for i, elt := range comp.Elts {
		idx := l.allocCell()
		l.emit(&IRCopy{Dst: idx, Src: si.ptr})
		l.emit(&IRAddI{Dst: idx, Value: byte(i * max(es, 1))}) // #nosec G115
		if es > 1 {
			// Multi-cell element: resolve struct/array literal.
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
	id, ok := se.X.(*ast.Ident)
	if !ok {
		return sliceInfo{}, fmt.Errorf("unsupported slice expression")
	}
	// Determine element metadata.
	si := l.allocSliceInfo()
	if src, ok := l.lookupSlice(id.Name); ok {
		si.elemSize = src.elemSize
		si.elemType = src.elemType
		si.elemSlice = src.elemSlice
		si.elemPtrType = src.elemPtrType
	} else if ai, ok := l.lookupArray(id.Name); ok {
		si.elemSize = 1
		_ = ai
	}
	if err := l.lowerSliceFromSliceExpr(si, se); err != nil {
		l.freeSliceInfo(si)
		return sliceInfo{}, err
	}
	return si, nil
}

func (l *Lowerer) lowerSliceFromSliceExpr(si sliceInfo, se *ast.SliceExpr) error {
	id, ok := se.X.(*ast.Ident)
	if !ok {
		return fmt.Errorf("unsupported slice expression")
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
		l.emit(&IRConst{Dst: si.ptr, Value: byte(baseSlot + low)}) // #nosec G115
		l.emit(&IRConst{Dst: si.len, Value: byte(high - low)})     // #nosec G115
		l.emit(&IRConst{Dst: si.cap, Value: byte(capVal)})         // #nosec G115
		return nil
	}
	// Reslice: s = t[low:high]
	src, ok := l.lookupSlice(id.Name)
	if !ok {
		return fmt.Errorf("unsupported slice expression base: %s", id.Name)
	}
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

// scanAndAllocLocals pre-scans a block for := declarations and var statements,
// allocating cells for all local variables.
func (l *Lowerer) scanAndAllocLocals(block *ast.BlockStmt) {
	sc := l.currentScope()
	ast.Inspect(block, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if s.Tok == token.DEFINE {
				for i, lhs := range s.Lhs {
					id, ok := lhs.(*ast.Ident)
					if !ok || id.Name == "_" {
						continue
					}
					// Check for composite literal: a := [N]byte{...} or p := Point{...}
					if i < len(s.Rhs) {
						if comp, ok := s.Rhs[i].(*ast.CompositeLit); ok {
							if count, elemSize, elemType, ies := l.arrayElementInfo(comp.Type); count > 0 {
								if _, exists := sc.arrays[id.Name]; !exists {
									if elemSize > 1 {
										l.defineStructArray(sc, id.Name, count, elemType, elemSize, ies)
									} else {
										l.defineArray(sc, id.Name, count)
									}
								}
								continue
							}
							if isSliceType(comp.Type) {
								if _, exists := sc.slices[id.Name]; !exists {
									es, et, esl, ept := l.sliceElemInfo(comp.Type)
									l.defineSlice(sc, id.Name, es, et, esl, ept)
								}
								continue
							}
							if def := l.structDef(comp.Type); def != nil {
								if _, exists := sc.structs[id.Name]; !exists {
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
								if fn.Name == "make" && len(call.Args) >= 2 && isSliceType(call.Args[0]) {
									if _, exists := sc.slices[id.Name]; !exists {
										es, et, esl, ept := l.sliceElemInfo(call.Args[0])
										l.defineSlice(sc, id.Name, es, et, esl, ept)
									}
									continue
								}
								if fn.Name == "append" && len(call.Args) >= 2 {
									if _, exists := sc.slices[id.Name]; !exists {
										// append(s, ...) where s is a known slice.
										if srcID, ok := call.Args[0].(*ast.Ident); ok {
											if _, exists := sc.slices[srcID.Name]; exists {
												es, et, esl, ept := l.sliceElemInfo(call.Args[0])
												l.defineSlice(sc, id.Name, es, et, esl, ept)
												continue
											}
										}
										// append(make(...), ...) or append([]byte{...}, ...).
										if innerCall, ok := call.Args[0].(*ast.CallExpr); ok {
											if innerFn, ok := innerCall.Fun.(*ast.Ident); ok && innerFn.Name == "make" && len(innerCall.Args) >= 2 && isSliceType(innerCall.Args[0]) {
												es, et, esl, ept := l.sliceElemInfo(innerCall.Args[0])
												l.defineSlice(sc, id.Name, es, et, esl, ept)
												continue
											}
										}
										if comp, ok := call.Args[0].(*ast.CompositeLit); ok && isSliceType(comp.Type) {
											es, et, esl, ept := l.sliceElemInfo(comp.Type)
											l.defineSlice(sc, id.Name, es, et, esl, ept)
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
							if _, exists := sc.slices[id.Name]; !exists {
								es, et, esl, ept := 1, "", false, ""
								if srcID, ok := se.X.(*ast.Ident); ok {
									if src, ok := sc.slices[srcID.Name]; ok {
										es, et, esl, ept = src.elemSize, src.elemType, src.elemSlice, src.elemPtrType
									}
								}
								l.defineSlice(sc, id.Name, es, et, esl, ept)
							}
							continue
						}
					}
					// inner := s[i] where s is [][]byte or []P
					if i < len(s.Rhs) {
						if idxExpr, ok := s.Rhs[i].(*ast.IndexExpr); ok {
							if arrID, ok := idxExpr.X.(*ast.Ident); ok {
								if si, ok := sc.slices[arrID.Name]; ok {
									if si.elemSlice {
										if _, exists := sc.slices[id.Name]; !exists {
											l.defineSlice(sc, id.Name, 1, "", false)
										}
										continue
									}
									if si.elemType != "" {
										if _, exists := sc.structs[id.Name]; !exists {
											def := l.result.Structs[si.elemType]
											l.defineStruct(sc, id.Name, def)
										}
										continue
									}
								}
							}
						}
					}
					// s := t where t is a slice
					if i < len(s.Rhs) {
						if rhsID, ok := s.Rhs[i].(*ast.Ident); ok {
							if src, ok := sc.slices[rhsID.Name]; ok {
								if _, exists := sc.slices[id.Name]; !exists {
									l.defineSlice(sc, id.Name, src.elemSize, src.elemType, src.elemSlice, src.elemPtrType)
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
									if _, exists := sc.slices[id.Name]; !exists {
										es := max(info.ReturnType.SliceElemSize, 1)
										l.defineSlice(sc, id.Name, es, info.ReturnType.SliceElemType, false)
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
								if ai, ok := sc.arrays[arrID.Name]; ok && ai.elemType != "" {
									if def, ok := l.result.Structs[ai.elemType]; ok {
										if _, exists := sc.structs[id.Name]; !exists {
											l.defineStruct(sc, id.Name, def)
										}
										continue
									}
								}
							}
						}
					}
					if _, exists := sc.vars[id.Name]; !exists {
						sc.vars[id.Name] = l.allocCell()
					}
				}
			}
		case *ast.RangeStmt:
			if s.Key != nil {
				if id, ok := s.Key.(*ast.Ident); ok {
					if _, exists := sc.vars[id.Name]; !exists {
						sc.vars[id.Name] = l.allocCell()
					}
				}
			}
			if s.Value != nil {
				if id, ok := s.Value.(*ast.Ident); ok {
					if _, exists := sc.vars[id.Name]; !exists {
						// Struct/pointer slice range: allocate appropriately.
						var rangeElemType string
						if rangeID, ok := s.X.(*ast.Ident); ok {
							if si, ok := sc.slices[rangeID.Name]; ok {
								if si.elemType != "" {
									rangeElemType = si.elemType
								} else if si.elemPtrType != "" {
									sc.vars[id.Name] = l.allocCell()
									sc.ptrType[id.Name] = si.elemPtrType
									break
								}
							}
						}
						if rangeElemType == "" {
							if call, ok := s.X.(*ast.CallExpr); ok {
								if fn, ok := call.Fun.(*ast.Ident); ok {
									if info, ok := l.result.Funcs[fn.Name]; ok && info.ReturnType.IsSlice {
										rangeElemType = info.ReturnType.SliceElemType
									}
								}
							}
						}
						if rangeElemType == "" {
							if se, ok := s.X.(*ast.SliceExpr); ok {
								if srcID, ok := se.X.(*ast.Ident); ok {
									if si, ok := sc.slices[srcID.Name]; ok {
										rangeElemType = si.elemType
									}
								}
							}
						}
						if rangeElemType != "" {
							def := l.result.Structs[rangeElemType]
							l.defineStruct(sc, id.Name, def)
							break
						}
						sc.vars[id.Name] = l.allocCell()
					}
				}
			}
		case *ast.DeclStmt:
			gd, ok := s.Decl.(*ast.GenDecl)
			if !ok {
				return true
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range vs.Names {
					if _, exists := sc.vars[name.Name]; !exists {
						if count, elemSize, elemType, ies := l.arrayElementInfo(vs.Type); count > 0 {
							if elemSize > 1 {
								l.defineStructArray(sc, name.Name, count, elemType, elemSize, ies)
							} else {
								l.defineArray(sc, name.Name, count)
							}
						} else if isSliceType(vs.Type) {
							if _, exists := sc.slices[name.Name]; !exists {
								es, et, esl, ept := l.sliceElemInfo(vs.Type)
								l.defineSlice(sc, name.Name, es, et, esl, ept)
							}
						} else if def := l.structDef(vs.Type); def != nil {
							if _, exists := sc.structs[name.Name]; !exists {
								l.defineStruct(sc, name.Name, def)
							}
						} else {
							sc.vars[name.Name] = l.allocCell()
						}
					}
				}
			}
		}
		return true
	})
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

func (l *Lowerer) defineStruct(sc *scope, name string, def *StructDef) {
	base := l.allocCells(def.Size)
	sc.structs[name] = structInfo{base: base, def: def}
}

func (l *Lowerer) defineArray(sc *scope, name string, size int) {
	base := l.allocCells(size)
	sc.arrays[name] = arrayInfo{base: base, size: size, count: size, elemSize: 1}
}

func (l *Lowerer) defineStructArray(sc *scope, name string, count int, elemType string, elemSize, innerElemSize int) {
	total := count * elemSize
	base := l.allocCells(total)
	sc.arrays[name] = arrayInfo{base: base, size: total, count: count, elemSize: elemSize, elemType: elemType, innerElemSize: innerElemSize}
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
	count, elemSize, _, _ := l.arrayElementInfo(expr)
	return count * elemSize
}

// arrayElementInfo returns (count, elemSize, elemType) for an array type.
// For [N]byte: (N, 1, ""). For [N]StructType: (N, structSize, typeName).
// arrayElementInfo returns (count, elemSize, elemType, innerElemSize) for an array type.
// For [N]byte: (N, 1, "", 0). For [N]Point: (N, pointSize, "Point", 0).
// For [N][M]byte: (N, M, "", 0). For [N][M][K]byte: (N, M*K, "", K).
// For [N][M]Point: (N, M*pointSize, "Point", pointSize).
func (l *Lowerer) arrayElementInfo(expr ast.Expr) (count, elemSize int, elemType string, innerElemSize int) {
	at, ok := expr.(*ast.ArrayType)
	if !ok {
		return 0, 0, "", 0
	}
	count = arrayTypeSizePart(at.Len, l.result.ByteConsts)
	if count < 0 {
		return 0, 0, "", 0
	}
	if id, ok := at.Elt.(*ast.Ident); ok {
		if def, ok := l.result.Structs[id.Name]; ok {
			return count, def.Size, id.Name, 0
		}
	}
	if _, ok := at.Elt.(*ast.ArrayType); ok {
		innerCount, innerES, innerET, _ := l.arrayElementInfo(at.Elt)
		if innerCount > 0 {
			return count, innerCount * innerES, innerET, innerES
		}
	}
	return count, 1, "", 0
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
		return l.lowerStmts(s.List)
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
	args := call.Args
	if receiver != nil {
		args = append([]ast.Expr{receiver}, args...)
	}
	retCells, err := l.inlineCall(info, args)
	if err != nil {
		return err
	}
	for _, c := range retCells {
		l.freeCell(c)
	}
	return nil
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
	r, err := lowerExpr(args[0])
	if err != nil {
		return err
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
		// string(x) -- print as raw character.
		rawChar := false
		if call, ok := arg.(*ast.CallExpr); ok && len(call.Args) == 1 {
			if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "string" {
				arg = call.Args[0]
				rawChar = true
			}
		}
		r, err := lowerExpr(arg)
		if err != nil {
			return err
		}
		if rawChar {
			l.emit(&IRPutc{Src: r.cell})
		} else {
			l.emitPrintByte(r.cell)
		}
		if r.temp {
			l.freeCell(r.cell)
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
	es := max(dst.elemSize, 1)
	// n = min(len(dst), len(src)) * elemSize
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
	limit := l.allocCell()
	t := l.allocCell()
	l.emit(&IRConst{Dst: t, Value: byte(es)}) // #nosec G115
	l.emit(&IRMul{Dst: limit, Src1: n, Src2: t})
	l.freeCell(t)
	l.freeCell(n)
	// Copy loop: for i := 0; i < limit; i++ { dst[ptr+i] = src[ptr+i] }
	counter := l.allocCell()
	l.emit(&IRZero{Dst: counter})
	cond := l.allocCell()
	l.emit(&IRCmp{Op: CmpLt, Dst: cond, Src1: counter, Src2: limit})
	saved := l.nodes
	l.nodes = nil
	srcAddr := l.allocCell()
	l.emit(&IRAdd{Dst: srcAddr, Src1: src.cell, Src2: counter})
	val := l.ptrLoad(srcAddr)
	l.freeCell(srcAddr)
	dstAddr := l.allocCell()
	l.emit(&IRAdd{Dst: dstAddr, Src1: dst.cell, Src2: counter})
	l.ptrStore(dstAddr, val)
	l.freeCell(val)
	l.freeCell(dstAddr)
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

func (l *Lowerer) lowerDecl(s *ast.DeclStmt) error {
	gd, ok := s.Decl.(*ast.GenDecl)
	if !ok {
		return fmt.Errorf("unsupported declaration")
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
				l.currentScope().ptrType[name] = si.def.Name
			}
			if ai, ok := l.lookupArray(rhsID.Name); ok {
				l.currentScope().ptrArray[name] = ai
			}
		}
		if comp, ok := unary.X.(*ast.CompositeLit); ok {
			if def := l.structDef(comp.Type); def != nil {
				l.currentScope().ptrType[name] = def.Name
			}
		}
	}
	// Composite variable copy: b = a where a is array or struct.
	// Must define the LHS as composite if it's a := declaration.
	if rhsID, ok := rhs.(*ast.Ident); ok {
		if srcSI, ok := l.lookupStruct(rhsID.Name); ok {
			sc := l.currentScope()
			delete(sc.vars, name)
			if _, exists := sc.structs[name]; !exists {
				l.defineStruct(sc, name, srcSI.def)
			}
		} else if srcAI, ok := l.lookupArray(rhsID.Name); ok {
			sc := l.currentScope()
			delete(sc.vars, name)
			if _, exists := sc.arrays[name]; !exists {
				if srcAI.elemSize > 1 || srcAI.elemType != "" {
					l.defineStructArray(sc, name, srcAI.count, srcAI.elemType, srcAI.elemSize, srcAI.innerElemSize)
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
	if r.isPointer {
		sc := l.currentScope()
		if r.typeName != "" {
			sc.ptrType[name] = r.typeName
		} else if r.elemCount > 0 && r.elemCount != 255 {
			sc.ptrArray[name] = arrayInfo{
				size: r.elemCount, count: r.elemCount,
				elemSize: max(r.elemSize, 1), elemType: r.elemType,
			}
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
	if r.cell == dst.cell {
		if r.temp {
			l.freeCell(r.cell)
		}
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
				args := call.Args
				if receiver != nil {
					args = append([]ast.Expr{receiver}, args...)
				}
				// Multi-return: q, r := divmod(a, b) or a[0], a[1] = divmod(a, b)
				if info.Returns == len(s.Lhs) && info.Returns > 1 {
					return l.lowerMultiReturnAssign(s, info, args)
				}
				// Composite return: p := f() where f returns struct, array, or slice.
				if len(s.Lhs) == 1 && !info.ReturnType.IsPointer && (info.ReturnType.ArraySize > 0 || info.ReturnType.StructType != "" || info.ReturnType.IsSlice) {
					return l.lowerCompositeReturnAssign(s.Lhs[0], info, args)
				}
			}
		}
	}

	// For multiple assignments (e.g., a, b = b, a), evaluate all RHS first
	// into temporaries, then assign to LHS. This ensures correct swap semantics.
	if len(s.Lhs) > 1 && len(s.Lhs) == len(s.Rhs) {
		type rhsValue struct {
			cell Cell
			size int // 1 for byte, >1 for composite
		}
		rhsVals := make([]rhsValue, len(s.Rhs))
		for i, rhs := range s.Rhs {
			r, err := l.lowerExpr(rhs)
			if err != nil {
				return err
			}
			r = l.ensureTemp(r)
			rhsVals[i] = rhsValue{r.cell, r.cellCount()}
		}
		// Assign to all LHS.
		for i, lhs := range s.Lhs {
			rv := rhsVals[i]
			val := exprResult{cell: rv.cell, temp: true, size: rv.size}
			if idx, ok := lhs.(*ast.IndexExpr); ok {
				base, err := l.lowerExpr(idx.X)
				if err != nil {
					return err
				}
				if base.elemCount > 0 {
					if err := l.writeInto(base, idx.Index, val); err != nil {
						return err
					}
					continue
				}
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
	for i, lhs := range s.Lhs {
		switch target := lhs.(type) {
		case *ast.Ident:
			cell, err := l.lookupVar(target.Name)
			if err != nil {
				return err
			}
			l.emit(&IRMove{Dst: cell, Src: retCells[i]})
		case *ast.IndexExpr:
			base, err := l.lowerExpr(target.X)
			if err != nil {
				return err
			}
			if err := l.writeInto(base, target.Index, exprResult{cell: retCells[i]}); err != nil {
				return err
			}
			l.freeCell(retCells[i])
		case *ast.SelectorExpr:
			r, err := l.lowerSelectorExpr(target)
			if err != nil {
				return err
			}
			l.emit(&IRMove{Dst: r.cell, Src: retCells[i]})
		default:
			return fmt.Errorf("unsupported assignment target")
		}
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
	for j := range base.elemSize {
		idxCell := l.allocCell()
		l.emit(&IRCopy{Dst: idxCell, Src: baseOffset.cell})
		l.emit(&IRAddI{Dst: idxCell, Value: byte(j)}) // #nosec G115
		l.emitVariableIndexWrite(flatArr, idxCell, valBase+j)
		l.freeCell(idxCell)
		l.freeCell(valBase + j)
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
	// Remove any scalar cell allocated by scanAndAllocLocals,
	// since the variable is actually a composite.
	var base Cell
	if info.ReturnType.IsSlice {
		sc := l.currentScope()
		delete(sc.vars, id.Name)
		es := max(info.ReturnType.SliceElemSize, 1)
		et := info.ReturnType.SliceElemType
		if si, ok := l.lookupSlice(id.Name); ok {
			l.emit(&IRMove{Dst: si.ptr, Src: retCells[0]})
			l.emit(&IRMove{Dst: si.len, Src: retCells[1]})
			l.emit(&IRMove{Dst: si.cap, Src: retCells[2]})
		} else {
			newSI := l.defineSlice(sc, id.Name, es, et, false)
			l.emit(&IRMove{Dst: newSI.ptr, Src: retCells[0]})
			l.emit(&IRMove{Dst: newSI.len, Src: retCells[1]})
			l.emit(&IRMove{Dst: newSI.cap, Src: retCells[2]})
		}
		return nil
	}
	if info.ReturnType.StructType != "" {
		def := l.result.Structs[info.ReturnType.StructType]
		sc := l.currentScope()
		delete(sc.vars, id.Name)
		if _, exists := sc.structs[id.Name]; !exists {
			l.defineStruct(sc, id.Name, def)
		}
		si, _ := l.lookupStruct(id.Name)
		base = si.base
	} else {
		size := info.ReturnType.ArraySize
		sc := l.currentScope()
		delete(sc.vars, id.Name)
		if _, exists := sc.arrays[id.Name]; !exists {
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
		// Pointer write: compute slot = ptr + offset, then store.
		slot := l.ptrOffset(base.cell, offset)
		val, err := l.lowerExpr(rhs)
		if err != nil {
			return err
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
	// Direct or flat-offset write via writeInto.
	val, err := l.lowerExpr(rhs)
	if err != nil {
		return err
	}
	offsetExpr := &ast.BasicLit{Kind: token.INT, Value: strconv.Itoa(offset)}
	return l.writeInto(base, offsetExpr, val)
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

// lowerDerefIncDec handles *p++ / *p--: load, modify, store via dynamic access.
func (l *Lowerer) lowerDerefIncDec(ptr ast.Expr, tok token.Token) error {
	p, err := l.lowerExpr(ptr)
	if err != nil {
		return err
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
		// Store tag in a temp variable so case comparisons can reference it.
		tagName = "$switch"
		tagCell := l.allocCell()
		l.currentScope().vars[tagName] = tagCell
		r, err := l.lowerExpr(s.Tag)
		if err != nil {
			return err
		}
		l.emitCopyOrMove(tagCell, r)
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
	var cell Cell
	if s.Key != nil {
		id, ok := s.Key.(*ast.Ident)
		if !ok {
			return fmt.Errorf("unsupported range key: %T", s.Key)
		}
		var err error
		cell, err = l.lookupVar(id.Name)
		if err != nil {
			return err
		}
	} else {
		// No loop variable: allocate a hidden counter.
		cell = l.allocCell()
		defer l.freeCell(cell)
	}

	// Check if ranging over an array or slice: for i, v := range x
	var valCell Cell
	var rangeBase exprResult
	var hasVal bool
	if s.Value != nil {
		r, err := l.lowerExpr(s.X)
		if err == nil && r.elemCount > 0 {
			rangeBase = r
			hasVal = true
			valID, ok := s.Value.(*ast.Ident)
			if !ok {
				return fmt.Errorf("unsupported range value: %T", s.Value)
			}
			if si, ok := l.lookupStruct(valID.Name); ok {
				valCell = si.base
			} else {
				valCell, _ = l.lookupVar(valID.Name)
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
	} else if hasVal {
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
	l.emit(&IRZero{Dst: cell})
	// Desugar to for loop: condition is i < limit.
	condCell := l.allocCell()
	l.emit(&IRCmp{Op: CmpLt, Dst: condCell, Src1: cell, Src2: limit.cell})

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
				l.emit(&IRMove{Dst: valCell + j, Src: result})
				l.freeCell(result)
				if j < es-1 {
					l.emit(&IRAddI{Dst: idx, Value: 1})
				}
			}
			l.freeCell(idx)
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
	l.emit(&IRAddI{Dst: cell, Value: 1})
	l.emit(&IRCmp{Op: CmpLt, Dst: condCell, Src1: cell, Src2: limit.cell})
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
		l.freeCell(limit.cell)
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
			l.emitCopyOrMove(l.returnDst[1], exprResult{cell: r.lenCell})
			l.emitCopyOrMove(l.returnDst[2], exprResult{cell: r.capCell})
			return l.returnFinish()
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
	for i, expr := range s.Results {
		r, err := l.lowerExpr(expr)
		if err != nil {
			return err
		}
		l.emitCopyOrMove(l.returnDst[i], r)
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
	l.emit(&IRDivMod{QuotDst: l.returnDst[quotIdx], RemDst: l.returnDst[remIdx], Src1: src1.cell, Src2: src2.cell})
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
				capSI := l.defineSlice(sc, name, si.elemSize, si.elemType, si.elemSlice, si.elemPtrType)
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
		cell := l.allocCell()
		l.emitCopyOrMove(cell, r)
		name := fmt.Sprintf("$defer_%d_%d", len(l.deferredCalls), i)
		l.defineVar(name)
		l.currentScope().vars[name] = cell
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
				if count, elemSize, elemType, _ := l.arrayElementInfo(comp.Type); count > 0 && elemSize > 1 {
					arr = arrayInfo{base: base, size: size, count: count, elemSize: elemSize, elemType: elemType}
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
			r = exprResult{cell: base, temp: true, size: r.elemCount, typeName: r.typeName}
		}
		// Flat-offset results: materialize into contiguous temp cells.
		if r.flatBase != 0 {
			totalSize := r.elemCount * r.elemSize
			flatArr := arrayInfo{base: r.flatBase, size: totalSize, count: totalSize, elemSize: 1}
			n := r.elemCount
			base := l.allocCells(n)
			for j := range n {
				idxCell := l.allocCell()
				l.emit(&IRCopy{Dst: idxCell, Src: r.cell})
				l.emit(&IRAddI{Dst: idxCell, Value: byte(j)}) // #nosec G115
				l.emitVariableIndexRead(flatArr, idxCell, base+j)
				l.freeCell(idxCell)
			}
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
			if pt.IsSlice {
				if inner, ok := sliceArgs[i]; ok {
					sc := l.currentScope()
					paramSI := l.defineSlice(sc, paramName, inner.elemSize, inner.elemType, inner.elemSlice, inner.elemPtrType)
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
						l.defineStructArray(sc, paramName, pt.ArrayCount, pt.ArrayElemType, pt.ArrayElemSize, 0)
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
					sc.ptrArray[paramName] = arrayInfo{
						size:     pt.PtrArrayInfo.ArraySize,
						count:    pt.PtrArrayInfo.ArrayCount,
						elemSize: pt.PtrArrayInfo.ArrayElemSize,
						elemType: pt.PtrArrayInfo.ArrayElemType,
					}
				}
				if pt.PtrStructType != "" {
					sc.ptrType[paramName] = pt.PtrStructType
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

	// Scan and allocate local variables.
	l.scanAndAllocLocals(info.Body)

	// Allocate return value cells.
	// For composite return types (struct/array), allocate contiguous cells.
	retSize := info.Returns
	if info.ReturnType.IsSlice {
		retSize = 3 // ptr, len, cap
	} else if info.ReturnType.ArraySize > 0 && !info.ReturnType.IsPointer {
		retSize = info.ReturnType.ArraySize
	} else if info.ReturnType.StructType != "" {
		retSize = l.result.Structs[info.ReturnType.StructType].Size
	}
	retCells := make([]Cell, retSize)
	if retSize > 1 && info.Returns == 1 {
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
		for i, name := range info.ReturnNames {
			if i < len(retCells) {
				sc.vars[name] = retCells[i]
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
		return exprResult{cell: si.ptr, temp: true, elemSize: si.elemSize, elemCount: 255, elemType: si.elemType, elemSlice: si.elemSlice, elemPtrType: si.elemPtrType, isPointer: true, lenCell: si.len, capCell: si.cap}, nil
	default:
		return exprResult{}, fmt.Errorf("unsupported expression: %T", expr)
	}
}

func (l *Lowerer) lowerLiteral(e *ast.BasicLit) (exprResult, error) {
	switch e.Kind {
	case token.INT:
		val, err := strconv.ParseInt(e.Value, 0, 64)
		if err != nil {
			return exprResult{}, err
		}
		if val < 0 || val > 255 {
			return exprResult{}, fmt.Errorf("integer literal %d out of byte range (0-255)", val)
		}
		t := l.allocCell()
		l.emit(&IRConst{Dst: t, Value: byte(val)})
		return exprResult{cell: t, temp: true}, nil
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
	if l.lookupStringConst(e.Name) != "" {
		return exprResult{}, fmt.Errorf("string constant %s can only be used with print/println", e.Name)
	}
	if val, ok := l.lookupConst(e.Name); ok {
		t := l.allocCell()
		l.emit(&IRConst{Dst: t, Value: val})
		return exprResult{cell: t, temp: true}, nil
	}
	if e.Name == "nil" {
		t := l.allocCell()
		l.emit(&IRZero{Dst: t})
		return exprResult{cell: t, temp: true}, nil
	}
	cell, err := lookupVar(e.Name)
	if err != nil {
		// Fall back to composite types.
		if si, ok := l.lookupStruct(e.Name); ok {
			return exprResult{cell: si.base, size: si.def.Size, elemSize: 1, elemCount: si.def.Size, typeName: si.def.Name}, nil
		}
		if ai, ok := l.lookupArray(e.Name); ok {
			return exprResult{cell: ai.base, size: ai.size, elemSize: ai.elemSize, elemCount: ai.count, elemType: ai.elemType, innerElemSize: ai.innerElemSize}, nil
		}
		if si, ok := l.lookupSlice(e.Name); ok {
			return exprResult{cell: si.ptr, elemSize: si.elemSize, elemCount: 255, elemType: si.elemType, elemSlice: si.elemSlice, elemPtrType: si.elemPtrType, isPointer: true, lenCell: si.len, capCell: si.cap}, nil
		}
		return exprResult{}, err
	}
	// Pointer-to-array: return as indexable pointer.
	if ptrAI, ok := l.lookupPtrArray(e.Name); ok {
		return exprResult{cell: cell, elemSize: ptrAI.elemSize, elemCount: ptrAI.count, elemType: ptrAI.elemType, isPointer: true}, nil
	}
	// Pointer-to-struct: return as indexable pointer (fields as byte offsets).
	if ptrDef, ok := l.lookupPtrType(e.Name); ok {
		return exprResult{cell: cell, size: ptrDef.Size, elemSize: 1, elemCount: ptrDef.Size, typeName: ptrDef.Name, isPointer: true}, nil
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
	t := l.allocCell()
	l.emitCopyOrMove(t, r)
	l.ptrStore(p.cell, t)
	l.freeCell(t)
	if p.temp {
		l.freeCell(p.cell)
	}
	return nil
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
			if es > 1 {
				esCell := l.allocCell()
				l.emit(&IRConst{Dst: esCell, Value: byte(es)}) // #nosec G115
				l.emit(&IRMul{Dst: idxR.cell, Src1: idxR.cell, Src2: esCell})
				l.freeCell(esCell)
			}
			l.emit(&IRConst{Dst: t, Value: byte(baseSlot)}) // #nosec G115
			l.emit(&IRAdd{Dst: t, Src1: t, Src2: idxR.cell})
			if idxR.temp {
				l.freeCell(idxR.cell)
			}
		}
		r := exprResult{cell: t, temp: true}
		if ai.elemType != "" {
			r.isPointer = true
			r.typeName = ai.elemType
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
		return exprResult{cell: t, temp: true}, nil
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
		max := l.allocCell()
		l.emit(&IRConst{Dst: max, Value: 255})
		l.emit(&IRSub{Dst: t, Src1: max, Src2: operand.cell})
		l.freeCell(max)
	default:
		l.freeCell(t)
		return exprResult{}, fmt.Errorf("unsupported unary operator: %s", e.Op)
	}
	if operand.temp {
		l.freeCell(operand.cell)
	}
	return exprResult{cell: t, temp: true}, nil
}

// lowerCompositeCompare handles == and != for arrays and structs.
// Returns (result, true, nil) if handled, or (_, false, nil) if not composite.
func (l *Lowerer) lowerCompositeCompare(e *ast.BinaryExpr) (exprResult, bool, error) {
	lBase, lSize, lTemp := l.resolveCompositeOperand(e.X)
	rBase, rSize, rTemp := l.resolveCompositeOperand(e.Y)
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
	size := lSize

	// Compare element-wise with short-circuit: start with result = 1,
	// then for each pair, only compare if result is still 1.
	// For [0]byte, result stays 1 (vacuously true).
	result := l.allocCell()
	l.emit(&IRConst{Dst: result, Value: 1})
	for i := range size {
		cond := l.allocCell()
		l.emit(&IRCopy{Dst: cond, Src: result})
		l.emit(&IRIf{Cond: cond, Then: &IRBlock{Nodes: []IRNode{
			&IRCmp{Op: CmpEq, Dst: result, Src1: lBase + i, Src2: rBase + i},
		}}})
		l.freeCell(cond)
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

// resolveCompositeOperand resolves a comparison operand to (base, size, tempSize).
// Returns size = -1 if the operand is not a composite type.
// tempSize > 0 means tempSize cells starting at base were allocated and need freeing.
func (l *Lowerer) resolveCompositeOperand(expr ast.Expr) (Cell, int, int) {
	if id, ok := expr.(*ast.Ident); ok {
		if ai, ok := l.lookupArray(id.Name); ok {
			return ai.base, ai.size, 0
		}
		if si, ok := l.lookupStruct(id.Name); ok {
			return si.base, si.def.Size, 0
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
				return 0, 0, 0
			}
			return base, def.Size, def.Size
		}
	}
	return 0, -1, 0
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
	}

	left, err := lowerExpr(e.X)
	if err != nil {
		return exprResult{}, err
	}
	right, err := lowerExpr(e.Y)
	if err != nil {
		return exprResult{}, err
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
		max := l.allocCell()
		l.emit(&IRConst{Dst: max, Value: 255})
		l.emit(&IRSub{Dst: comp, Src1: max, Src2: right.cell})
		l.freeCell(max)
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

	switch e.Op {
	case token.LAND:
		l.nodes = nil
		right, err := lowerExpr(e.Y)
		if err != nil {
			return exprResult{}, err
		}
		l.emitCopyOrMove(result, right)
		thenBlock := &IRBlock{Nodes: l.nodes}

		l.nodes = nil
		l.emit(&IRConst{Dst: result, Value: 0})
		elseBlock := &IRBlock{Nodes: l.nodes}

		l.nodes = saved
		l.emit(&IRIf{Cond: left.cell, Then: thenBlock, Else: elseBlock})

	case token.LOR:
		l.nodes = nil
		l.emit(&IRConst{Dst: result, Value: 1})
		thenBlock := &IRBlock{Nodes: l.nodes}

		l.nodes = nil
		right, err := lowerExpr(e.Y)
		if err != nil {
			return exprResult{}, err
		}
		l.emitCopyOrMove(result, right)
		elseBlock := &IRBlock{Nodes: l.nodes}

		l.nodes = saved
		l.emit(&IRIf{Cond: left.cell, Then: thenBlock, Else: elseBlock})
	}

	if left.temp {
		l.freeCell(left.cell)
	}
	return exprResult{cell: result, temp: true}, nil
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
	args := call.Args
	if receiver != nil {
		args = append([]ast.Expr{receiver}, args...)
	}
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
	if info.ReturnType.IsSlice {
		return exprResult{
			cell: retCells[0], temp: true, isPointer: true,
			elemSize:  max(info.ReturnType.SliceElemSize, 1),
			elemCount: 255, elemType: info.ReturnType.SliceElemType,
			lenCell: retCells[1], capCell: retCells[2],
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
	case "byte":
		if len(call.Args) != 1 {
			return exprResult{}, true, fmt.Errorf("byte() expects 1 argument")
		}
		r, err := lowerExpr(call.Args[0])
		return r, true, err
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
			return exprResult{cell: inner.ptr, temp: true, elemSize: 1, elemCount: 255, isPointer: true, lenCell: inner.len, capCell: inner.cap}, nil
		}
		// Multi-byte element: return pointer to sub-array.
		return exprResult{cell: idx, temp: true, size: base.elemSize, elemSize: 1, elemCount: base.elemSize, elemType: base.elemType, typeName: base.elemType, isPointer: true}, nil
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
			return exprResult{cell: base.cell, temp: true, elemSize: 1, elemCount: base.elemSize, elemType: base.elemType, typeName: base.elemType, flatBase: base.flatBase}, nil
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
		r := exprResult{cell: cell, typeName: base.elemType}
		if base.elemSize > 1 {
			r.size = base.elemSize
			if base.innerElemSize > 0 {
				// Nested array: preserve inner structure.
				r.elemSize = base.innerElemSize
				r.elemCount = base.elemSize / base.innerElemSize

				r.elemType = base.elemType
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
		if inner.flatBase != 0 {
			// Variable index: flat offset + fieldOffset, dynamic load.
			offset, ok := def.Offsets[e.Sel.Name]
			if !ok {
				return exprResult{}, fmt.Errorf("unknown field %s in struct %s", e.Sel.Name, def.Name)
			}
			l.emit(&IRAddI{Dst: inner.cell, Value: byte(offset)}) // #nosec G115
			totalSize := inner.elemCount * inner.elemSize
			flatArr := arrayInfo{base: inner.flatBase, size: totalSize, count: totalSize, elemSize: 1}
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
			return exprResult{cell: idx, temp: true, size: fieldDef.Size, elemSize: 1, elemCount: fieldDef.Size, typeName: fieldType, isPointer: true}, nil
		}
		result := l.ptrLoad(idx)
		l.freeCell(idx)
		return exprResult{cell: result, temp: true}, nil
	}
	r := exprResult{cell: base + offset}
	if arrSize := def.FieldArraySizes[e.Sel.Name]; arrSize > 0 {
		r.size = arrSize
		if ies := def.FieldInnerSizes[e.Sel.Name]; ies > 0 {
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
	}
	return r, nil
}
