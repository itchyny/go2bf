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

	// Defer context.
	deferredCalls []*IRBlock // deferred call blocks, emitted in LIFO order at return
}

// scope holds variable bindings for the current lexical scope.
type scope struct {
	vars     map[string]Cell
	consts   map[string]byte       // compile-time constants
	arrays   map[string]arrayInfo  // base cell and size
	structs  map[string]structInfo // base cell and field layout
	ptrType  map[string]string     // variable name -> pointed-to struct type name
	ptrArray map[string]arrayInfo  // variable name -> pointed-to array info
}

type arrayInfo struct {
	base     Cell
	size     int    // total cells (count * elemSize)
	count    int    // number of elements
	elemSize int    // cells per element (1 for byte, >1 for struct)
	elemType string // struct type name (empty for byte)
}

type structInfo struct {
	base Cell
	def  *StructDef // field names, offsets, size
}

type exprResult struct {
	cell      Cell
	temp      bool   // if true, the caller should free this cell via freeCell
	size      int    // total number of cells; 0 means 1 (scalar)
	elemSize  int    // element size for indexable results; 0 means not indexable
	elemCount int    // number of elements for indexable results
	elemType  string // struct type name for composite elements (empty for byte)
	typeName  string // struct type name of this result (empty for non-struct)
	isPtr     bool   // if true, cell is a pointer (slot index) for indirect access
	flatBase  Cell   // for flat-offset results: base of the original array
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

// structTypeSize returns the StructDef for a named struct type, or nil.
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
							if count, elemSize, elemType := l.arrayElementInfo(comp.Type); count > 0 {
								if _, exists := sc.arrays[id.Name]; !exists {
									if elemSize > 1 {
										l.defineStructArray(sc, id.Name, count, elemType, elemSize)
									} else {
										l.defineArray(sc, id.Name, count)
									}
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
						if count, elemSize, elemType := l.arrayElementInfo(vs.Type); count > 0 {
							if elemSize > 1 {
								l.defineStructArray(sc, name.Name, count, elemType, elemSize)
							} else {
								l.defineArray(sc, name.Name, count)
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

func (l *Lowerer) defineStructArray(sc *scope, name string, count int, elemType string, elemSize int) {
	total := count * elemSize
	base := l.allocCells(total)
	sc.arrays[name] = arrayInfo{base: base, size: total, count: count, elemSize: elemSize, elemType: elemType}
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
	count, elemSize, _ := l.arrayElementInfo(expr)
	return count * elemSize
}

// arrayElementInfo returns (count, elemSize, elemType) for an array type.
// For [N]byte: (N, 1, ""). For [N]StructType: (N, structSize, typeName).
// For [N][M]byte: (N, M, "").
func (l *Lowerer) arrayElementInfo(expr ast.Expr) (int, int, string) {
	at, ok := expr.(*ast.ArrayType)
	if !ok {
		return 0, 0, ""
	}
	count := arrayTypeSizePart(at.Len, l.result.ByteConsts)
	if count < 0 {
		return 0, 0, ""
	}
	// Check element type: byte, struct, or nested array.
	if id, ok := at.Elt.(*ast.Ident); ok {
		if def, ok := l.result.Structs[id.Name]; ok {
			return count, def.Size, id.Name
		}
	}
	// Nested array: [N][M]byte
	if innerAt, ok := at.Elt.(*ast.ArrayType); ok {
		innerSize := arrayTypeSizePart(innerAt.Len, l.result.ByteConsts)
		if innerSize > 0 {
			return count, innerSize, ""
		}
	}
	return count, 1, ""
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
		}
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
		if len(args) != 1 {
			return true, fmt.Errorf("putchar expects 1 argument, got %d", len(args))
		}
		r, err := lowerExpr(args[0])
		if err != nil {
			return true, err
		}
		if r.size > 0 {
			if r.typeName != "" {
				return true, fmt.Errorf("cannot use struct %s as byte value", r.typeName)
			}
			if r.elemCount > 0 {
				return true, fmt.Errorf("cannot use array as byte value")
			}
			return true, fmt.Errorf("cannot use composite value as byte")
		}
		l.emit(&IRPutc{Src: r.cell})
		if r.temp {
			l.freeCell(r.cell)
		}
		return true, nil
	case "print", "println":
		for i, arg := range args {
			if i > 0 && name == "println" {
				t := l.allocCell()
				l.emit(&IRConst{Dst: t, Value: ' '})
				l.emit(&IRPutc{Src: t})
				l.freeCell(t)
			}
			if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				s, _ := strconv.Unquote(lit.Value)
				t := l.allocCell()
				for _, b := range []byte(s) {
					l.emit(&IRConst{Dst: t, Value: b})
					l.emit(&IRPutc{Src: t})
				}
				l.freeCell(t)
			} else if id, ok := arg.(*ast.Ident); ok && l.lookupStringConst(id.Name) != "" {
				s := l.lookupStringConst(id.Name)
				t := l.allocCell()
				for _, b := range []byte(s) {
					l.emit(&IRConst{Dst: t, Value: b})
					l.emit(&IRPutc{Src: t})
				}
				l.freeCell(t)
			} else if call, ok := arg.(*ast.CallExpr); ok && len(call.Args) == 1 {
				if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "string" {
					// string(x) -- print as raw character.
					r, err := lowerExpr(call.Args[0])
					if err != nil {
						return true, err
					}
					l.emit(&IRPutc{Src: r.cell})
					if r.temp {
						l.freeCell(r.cell)
					}
				} else {
					r, err := lowerExpr(arg)
					if err != nil {
						return true, err
					}
					l.emitPrintByte(r.cell)
					if r.temp {
						l.freeCell(r.cell)
					}
				}
			} else {
				r, err := lowerExpr(arg)
				if err != nil {
					return true, err
				}
				l.emitPrintByte(r.cell)
				if r.temp {
					l.freeCell(r.cell)
				}
			}
		}
		if name == "println" {
			t := l.allocCell()
			l.emit(&IRConst{Dst: t, Value: '\n'})
			l.emit(&IRPutc{Src: t})
			l.freeCell(t)
		}
		return true, nil
	}
	return false, nil
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
		for i, name := range vs.Names {
			if i >= len(vs.Values) {
				// No initializer: zero the variable/array/struct.
				if ai, ok := l.lookupArray(name.Name); ok {
					for j := range ai.size {
						l.emit(&IRZero{Dst: ai.base + j})
					}
				} else if si, ok := l.lookupStruct(name.Name); ok {
					for j := range si.def.Size {
						l.emit(&IRZero{Dst: si.base + j})
					}
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
	// Composite literal: a = [N]byte{...} or p = Point{...}
	if comp, ok := rhs.(*ast.CompositeLit); ok {
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
				if srcAI.elemType != "" {
					l.defineStructArray(sc, name, srcAI.count, srcAI.elemType, srcAI.elemSize)
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
	// Resolve destination.
	dst, err := l.lowerExpr(&ast.Ident{Name: name})
	if err != nil {
		return err
	}
	if r.cell == dst.cell {
		if r.temp {
			l.freeCell(r.cell)
		}
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
				// Composite return: p := f() where f returns struct or array.
				if len(s.Lhs) == 1 && (info.ReturnType.ArraySize > 0 || info.ReturnType.StructType != "") {
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
	// Generic: evaluate base and value, write at index.
	base, err := l.lowerExpr(idx.X)
	if err != nil {
		return err
	}
	if base.elemCount == 0 {
		return fmt.Errorf("cannot index non-array expression")
	}
	// Composite literal RHS: struct or array literal.
	if comp, ok := rhs.(*ast.CompositeLit); ok {
		return l.lowerCompositeElemAssign(base, idx.Index, comp)
	}
	r, err := l.lowerExpr(rhs)
	if err != nil {
		return err
	}
	return l.writeInto(base, idx.Index, r)
}

// lowerCompositeElemAssign handles a[i] = CompositeLit where the RHS is
// a struct literal (Point{x: 1}) or array literal ([3]byte{1, 2, 3}).
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
	if base.isPtr {
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
	// Pointer-to-struct field inc/dec: ptr.x++ -> load, modify, store
	if id, ok := sel.X.(*ast.Ident); ok {
		if ptrDef, ok := l.lookupPtrType(id.Name); ok {
			ptrCell, err := l.lookupVar(id.Name)
			if err != nil {
				return err
			}
			idx := l.ptrOffset(ptrCell, ptrDef.Offsets[sel.Sel.Name])
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

// lowerRange handles `for i := range n` (Go 1.22+ range over integer).
// Desugars to: i = 0; for i < n { body; i++ }.
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

	// Check if ranging over an array: for i, v := range arr
	var valCell Cell
	var arr arrayInfo
	var hasVal bool
	if s.Value != nil {
		if id, ok := s.X.(*ast.Ident); ok {
			if ai, ok := l.lookupArray(id.Name); ok {
				arr = ai
				valID, ok := s.Value.(*ast.Ident)
				if !ok {
					return fmt.Errorf("unsupported range value: %T", s.Value)
				}
				valCell, _ = l.lookupVar(valID.Name)
				hasVal = true
			}
		}
	}

	// Evaluate the range limit.
	var limit exprResult
	var err error
	if hasVal {
		// Range over array: limit = len(arr)
		t := l.allocCell()
		l.emit(&IRConst{Dst: t, Value: byte(arr.size)}) // #nosec G115
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

	// For range over array: load v = a[i] at the start of each iteration.
	if hasVal {
		l.emitVariableIndexRead(arr, cell, valCell)
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
		if id, ok := result.(*ast.Ident); ok {
			if si, ok := l.lookupStruct(id.Name); ok {
				return l.returnComposite(si.base, si.def.Size)
			}
			if ai, ok := l.lookupArray(id.Name); ok {
				return l.returnComposite(ai.base, ai.size)
			}
		}
		// Handle return of composite literal: return [3]byte{...} or return Point{...}
		if comp, ok := result.(*ast.CompositeLit); ok {
			if size := l.arraySize(comp.Type); size > 0 {
				for i, elt := range comp.Elts {
					r, err := l.lowerExpr(elt)
					if err != nil {
						return err
					}
					l.emitCopyOrMove(l.returnDst[i], r)
				}
				return l.returnFinish()
			}
			if def := l.structDef(comp.Type); def != nil {
				if err := l.lowerStructValueTo(l.returnDst[0], def, comp); err != nil {
					return err
				}
				return l.returnFinish()
			}
		}
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
		if id, ok := arg.(*ast.Ident); ok && l.lookupStringConst(id.Name) != "" {
			// String constant: pass as a string literal for the deferred call.
			capturedArgs[i] = &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(l.lookupStringConst(id.Name))}
			continue
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
		def := l.structDef(comp.Type)
		if def == nil {
			return 0, 0, fmt.Errorf("unsupported composite literal argument")
		}
		base := l.allocCells(def.Size)
		for j := range def.Size {
			l.emit(&IRZero{Dst: base + j})
		}
		if err := l.lowerStructValueTo(base, def, comp); err != nil {
			return 0, 0, err
		}
		return base, def.Size, nil
	}
	r, err := l.lowerExpr(expr)
	if err != nil {
		return 0, 0, err
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
	for i, expr := range argExprs {
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
				if count, elemSize, elemType := l.arrayElementInfo(comp.Type); count > 0 && elemSize > 1 {
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
			if pt.ArraySize > 0 || pt.StructType != "" {
				var paramBase Cell
				var paramSize int
				if pt.ArraySize > 0 {
					sc := l.currentScope()
					if pt.ArrayElemSize > 1 {
						l.defineStructArray(sc, paramName, pt.ArrayCount, pt.ArrayElemType, pt.ArrayElemSize)
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
	if info.ReturnType.ArraySize > 0 {
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
	cell, err := lookupVar(e.Name)
	if err != nil {
		// Fall back to composite types.
		if si, ok := l.lookupStruct(e.Name); ok {
			return exprResult{cell: si.base, size: si.def.Size, elemSize: 1, elemCount: si.def.Size, typeName: si.def.Name}, nil
		}
		if ai, ok := l.lookupArray(e.Name); ok {
			return exprResult{cell: ai.base, size: ai.size, elemSize: ai.elemSize, elemCount: ai.count, elemType: ai.elemType}, nil
		}
		return exprResult{}, err
	}
	// Pointer-to-array: return as indexable pointer.
	if ptrAI, ok := l.lookupPtrArray(e.Name); ok {
		return exprResult{cell: cell, elemSize: ptrAI.elemSize, elemCount: ptrAI.count, elemType: ptrAI.elemType, isPtr: true}, nil
	}
	// Pointer-to-struct: return as indexable pointer (fields as byte offsets).
	if ptrDef, ok := l.lookupPtrType(e.Name); ok {
		return exprResult{cell: cell, size: ptrDef.Size, elemSize: 1, elemCount: ptrDef.Size, typeName: ptrDef.Name, isPtr: true}, nil
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
	idxR, err := l.lowerExpr(indexExpr)
	if err != nil {
		return 0, err
	}
	if elemSize > 1 {
		es := l.allocCell()
		l.emit(&IRConst{Dst: es, Value: byte(elemSize)}) // #nosec G115
		l.emit(&IRMul{Dst: idx, Src1: idxR.cell, Src2: es})
		l.freeCell(es)
	} else {
		l.emitCopyOrMove(idx, idxR)
	}
	l.emit(&IRAdd{Dst: idx, Src1: idx, Src2: ptr})
	if idxR.temp {
		l.freeCell(idxR.cell)
	}
	return idx, nil
}

// lowerAddressOf handles &x, &a[i], &p.x -- returns the stack slot index as a byte.
func (l *Lowerer) lowerAddressOf(expr ast.Expr) (exprResult, error) {
	switch e := expr.(type) {
	case *ast.Ident:
		// &struct or &array -- return base slot.
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
		// &a[i] -- compute slotOf(a.base) + i
		id, ok := e.X.(*ast.Ident)
		if !ok {
			return exprResult{}, fmt.Errorf("cannot take address of chained index expression")
		}
		ai, ok := l.lookupArray(id.Name)
		if !ok {
			return exprResult{}, fmt.Errorf("cannot take address of non-array index: %s", id.Name)
		}
		baseSlot := slotOf(ai.base)
		t := l.allocCell()
		if constIdx, ok := l.constValue(e.Index); ok {
			l.emit(&IRConst{Dst: t, Value: byte(baseSlot + constIdx)}) // #nosec G115
		} else {
			idxR, err := l.lowerExpr(e.Index)
			if err != nil {
				return exprResult{}, err
			}
			l.emit(&IRConst{Dst: t, Value: byte(baseSlot)}) // #nosec G115
			l.emit(&IRAdd{Dst: t, Src1: t, Src2: idxR.cell})
			if idxR.temp {
				l.freeCell(idxR.cell)
			}
		}
		return exprResult{cell: t, temp: true}, nil
	case *ast.SelectorExpr:
		// &p.x -- base slot + field offset
		r, err := l.lowerSelectorExpr(e)
		if err != nil {
			return exprResult{}, err
		}
		t := l.allocCell()
		l.emit(&IRConst{Dst: t, Value: byte(slotOf(r.cell))}) // #nosec G115
		return exprResult{cell: t, temp: true}, nil
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
		return exprResult{
			cell: retCells[0], temp: true, size: info.ReturnType.ArraySize,
			elemSize: 1, elemCount: info.ReturnType.ArraySize,
		}, nil
	}
	if info.ReturnType.StructType != "" {
		def := l.result.Structs[info.ReturnType.StructType]
		return exprResult{cell: retCells[0], temp: true, size: def.Size, typeName: info.ReturnType.StructType}, nil
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
		return exprResult{}, fmt.Errorf("cannot index non-array expression")
	}
	return l.indexInto(base, e.Index)
}

// indexInto indexes a composite result by the given expression.
// The base must have elemSize and elemCount set.
func (l *Lowerer) indexInto(base exprResult, indexExpr ast.Expr) (exprResult, error) {
	if base.isPtr {
		idx, err := l.ptrDynIndex(base.cell, indexExpr, base.elemSize)
		if err != nil {
			return exprResult{}, err
		}
		if base.elemSize == 1 {
			result := l.ptrLoad(idx)
			l.freeCell(idx)
			return exprResult{cell: result, temp: true}, nil
		}
		// Multi-byte element: return pointer to sub-array.
		return exprResult{cell: idx, temp: true, elemSize: 1, elemCount: base.elemSize, elemType: base.elemType, typeName: base.elemType, isPtr: true}, nil
	}

	// Flat-offset result: cell holds i*elemSize relative to flatBase.
	// Add the inner offset and do dynamic access on the flat array.
	if base.flatBase != 0 {
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
			r.elemSize = 1
			r.elemCount = base.elemSize
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
		r.elemSize = 1
		r.elemCount = base.elemSize
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
	if base.isPtr {
		idx, err := l.ptrDynIndex(base.cell, indexExpr, base.elemSize)
		if err != nil {
			return err
		}
		t := l.allocCell()
		l.emitCopyOrMove(t, val)
		l.ptrStore(idx, t)
		l.freeCell(t)
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
	baseIsPtr := false
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
				return exprResult{cell: idx, temp: true, elemSize: 1, elemCount: arrSize, isPtr: true}, nil
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
		baseIsPtr = inner.isPtr
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
	if baseIsPtr {
		idx := l.ptrOffset(base, offset)
		// Array field: return pointer for indexing.
		if arrSize := def.FieldArraySizes[e.Sel.Name]; arrSize > 0 {
			return exprResult{cell: idx, temp: true, elemSize: 1, elemCount: arrSize, isPtr: true}, nil
		}
		result := l.ptrLoad(idx)
		l.freeCell(idx)
		return exprResult{cell: result, temp: true}, nil
	}
	r := exprResult{cell: base + offset}
	if arrSize := def.FieldArraySizes[e.Sel.Name]; arrSize > 0 {
		r.size = arrSize
		r.elemSize = 1
		r.elemCount = arrSize
	} else if fieldType := def.FieldTypes[e.Sel.Name]; fieldType != "" {
		fieldDef := l.result.Structs[fieldType]
		r.size = fieldDef.Size
		r.elemSize = 1
		r.elemCount = fieldDef.Size
		r.typeName = fieldType
	}
	return r, nil
}
