package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"maps"
)

// === General recursion via stack-based CPU model ===
//
// Frame layout (slot indices):
//   0: phase (dispatch phase number)
//   1: retval (single-byte return; uintN returns flow through retReg)
//   2..2+P-1: parameters (one cell each; recursive params are scalar-only)
//   2+P..end: local variables (one cell for byte, intSize cells for uintN)
//
// The function body is split into "phases" at each recursive call site.
// A dispatch loop processes one phase per iteration, always operating on
// the topmost stack frame.
//
// Pointer, composite (struct, array, slice), and uint64 params/returns
// are rejected upfront -- the rec lowerer has no dereference path or
// composite layout. See `inlineCall` (lowerer.go) and `rejectComposites`
// / `collectIntLocals` (lowerer_rec.go) for the entry-point checks.

// recLocalInfo holds slot index plus per-local type metadata for a variable
// in a recursive function frame. Recursive functions are scalar-only, so
// the only metadata beyond `slot` is the multi-byte integer width.
type recLocalInfo struct {
	slot    int
	intSize int // 0 for byte; 2/4 for uint16/uint32
}

// recContext holds state for lowering a recursive function. Recursive
// functions are scalar-only: all params and locals occupy one frame
// slot each (or `intSize` cells for `uint16`/`uint32` locals and
// returns). Pointer, struct, array, slice, and `uint64` types are
// rejected upfront, so this struct does not track that metadata.
type recContext struct {
	funcName  string
	frameSize int
	slotPhase int                     // always 0 (dispatch phase number)
	slotRet   int                     // always 1 (single-byte return slot; multi-byte uintN returns flow through retReg)
	paramBase int                     // always 2 (params start here, one cell each)
	localBase int                     // 2 + len(params); locals and synthetic temps start here
	locals    map[string]recLocalInfo // name -> slot + intSize (also covers params, named returns, and synthetic temps like $cond/$switch)
	phases    []*IRBlock              // dispatch phases produced by buildPhases
	activeReg Cell                    // phase-temp cell holding the recursion depth counter
	retReg    Cell                    // phase-temp cell at retReg..retReg+retSize-1 carrying child return values
	retSize   int                     // number of return cells (1 for byte, 2 or 4 for uint16/uint32)

	dispatchPhase, dispatchPr    Cell     // dispatch-loop working state, reserved in the phase-temp area so
	dispatchFlag, dispatchActive Cell     // they don't compete with phase code for codegen's algo-temp pool.
	phaseCodeBase                int      // first phase-temp cell available to phase code (past dispatch's reserved 4)
	noRetFlag                    Cell     // phase temp: 1 if no return happened in this phase, 0 after return
	returnNames                  []string // named return value names (empty if unnamed)

	deferCaptureSlots []int      // pre-allocated frame slots for defer captures
	deferCaptureIdx   int        // index into deferCaptureSlots during lowering
	deferredCalls     []*IRBlock // IR blocks emitted before each return's frame pop
}

func (l *Lowerer) lowerGeneralRecursion(info *FuncInfo, argExprs []ast.Expr) ([]Cell, error) {
	// Compute frame layout.
	localNames := collectLocals(info.Body, info.Params)
	rc := &recContext{
		funcName:  info.Name,
		slotPhase: 0,
		slotRet:   1,
		paramBase: 2,
		localBase: 2 + len(info.Params),
		locals:    make(map[string]recLocalInfo),
	}
	paramSlot := rc.paramBase
	for i, name := range info.Params {
		// Params are scalars: byte (1 cell) or uint16/uint32 (intSize
		// cells). Pointer/composite/uint64/slice params are rejected at
		// inlineCall before reaching here.
		size, intSize := 1, 0
		if i < len(info.ParamTypes) {
			if n := info.ParamTypes[i].IntSize; n >= 2 {
				size, intSize = n, n
			}
		}
		rc.locals[name] = recLocalInfo{slot: paramSlot, intSize: intSize}
		paramSlot += size
	}
	rc.localBase = paramSlot
	for i, name := range localNames {
		rc.locals[name] = recLocalInfo{slot: rc.localBase + i}
	}
	rc.frameSize = rc.localBase + len(localNames)

	// Named return values are mapped to frame slots like locals. For
	// uintN named returns, allocate intSize cells so reads/writes through
	// the returned name span the full multi-byte value.
	rc.returnNames = info.ReturnNames
	for i, name := range info.ReturnNames {
		if _, exists := rc.locals[name]; exists {
			continue
		}
		intSize := 0
		if i < len(info.ReturnTypes) {
			if n := info.ReturnTypes[i].IntSize; n >= 2 {
				intSize = n
			}
		}
		rc.locals[name] = recLocalInfo{slot: rc.frameSize, intSize: intSize}
		rc.frameSize += max(intSize, 1)
	}

	if err := l.rejectComposites(info.Body); err != nil {
		return nil, err
	}
	if err := l.collectIntLocals(rc, info.Body); err != nil {
		return nil, err
	}

	// Allocate active and retval in the PHASE TEMP area (direct tape positions,
	// not stack slots). This avoids cache/storeToStack issues in the dispatch loop.
	// Reserve positions 25, 26 for these; phase code allocs start at 27.
	// uintN returns are the only multi-cell case; struct/array returns are
	// rejected before reaching this point.
	retSize := info.Returns
	if ri := info.SingleReturn(); ri.IntSize >= 2 {
		retSize = ri.IntSize
	}
	rc.retSize = retSize

	rc.activeReg = phaseTempBase  // tape position 25
	rc.retReg = phaseTempBase + 1 // tape position 26 (base of return area)

	// By default the dispatch loop's four working cells (phase, pr,
	// flag, activeTemp) come from the codegen's algo-temp pool --
	// close to the registers, so frame loads in phase code stay
	// cheap. Functions that use bitwise operators need the full
	// algo-temp pool for genBitwise's ~11-temp peak, so for them we
	// relocate the dispatch cells into the phase-temp area, freeing
	// the algo-temp pool at the cost of pushing phase code's frame
	// slots four positions higher (more `<>` navigation per access).
	rc.phaseCodeBase = phaseTempBase + 1 + retSize
	if hasBitwise(info.Body) {
		pos := rc.phaseCodeBase
		dispatchCells := make([]Cell, 0, 4)
		for len(dispatchCells) < 4 {
			// Skip highway markers and the codegen-reserved interleaved
			// algo-temp slots just below them (see currentAlgoTemps).
			if isMarkerOrAlgoTemp(pos) {
				pos++
				continue
			}
			dispatchCells = append(dispatchCells, Cell(pos))
			pos++
		}
		rc.dispatchPhase = dispatchCells[0]
		rc.dispatchPr = dispatchCells[1]
		rc.dispatchFlag = dispatchCells[2]
		rc.dispatchActive = dispatchCells[3]
		rc.phaseCodeBase = pos
	}

	// Evaluate arguments. Byte args produce 1 cell;
	// uint16/uint32 args produce intSize contiguous cells.
	type argCells struct {
		cells []Cell
		temps bool
	}
	args := make([]argCells, len(argExprs))
	for i, expr := range argExprs {
		r, err := l.lowerExpr(expr)
		if err != nil {
			return nil, err
		}
		if r.intSize >= 2 {
			cells := make([]Cell, r.intSize)
			for j := range r.intSize {
				cells[j] = r.cell + j
			}
			args[i] = argCells{cells, r.temp}
		} else {
			args[i] = argCells{[]Cell{r.cell}, r.temp}
		}
	}

	// Pre-allocate frame slots for defer captures. This must happen before
	// buildPhases so that all IRStoreFrame/IRLoadFrame use the final frameSize.
	ast.Inspect(info.Body, func(n ast.Node) bool {
		ds, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		for _, arg := range ds.Call.Args {
			if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				continue
			}
			rc.deferCaptureSlots = append(rc.deferCaptureSlots, rc.frameSize)
			rc.frameSize++
		}
		return true
	})

	// Build phases first - this may grow rc.frameSize (e.g. extractRecCalls
	// adds temp variables for inlined recursive functions).
	if err := l.buildPhases(rc, info); err != nil {
		return nil, err
	}

	// Emit: push initial frame (after buildPhases so frameSize is final).
	l.emit(&IRFramePush{Slots: rc.frameSize})

	// Store arguments into frame's parameter slots.
	paramSlot = rc.paramBase
	for _, arg := range args {
		for _, cell := range arg.cells {
			l.emit(&IRStoreFrame{Slot: paramSlot, Src: cell, FrameSize: rc.frameSize})
			paramSlot++
		}
		if arg.temps {
			for _, cell := range arg.cells {
				l.freeCell(cell)
			}
		}
	}

	// Store phase = 0
	phaseConst := l.allocCell()
	l.emit(&IRConst{Dst: phaseConst, Value: 0})
	l.emit(&IRStoreFrame{Slot: rc.slotPhase, Src: phaseConst, FrameSize: rc.frameSize})
	l.freeCell(phaseConst)

	// Set active = 1
	l.emit(&IRConst{Dst: rc.activeReg, Value: 1})

	// Emit the dispatch loop.
	l.emit(&IRDispatch{
		Active:     rc.activeReg,
		Phase:      rc.dispatchPhase,
		Pr:         rc.dispatchPr,
		Flag:       rc.dispatchFlag,
		ActiveTemp: rc.dispatchActive,
		FrameSize:  rc.frameSize,
		Phases:     rc.phases,
	})

	// After dispatch loop exits, retReg area holds the return value(s).
	// Multi-byte returns must land in contiguous cells since callers
	// (e.g., emitPrintInt, emitCopyOrMove for uintN destinations)
	// access them as base+k.
	retCells := make([]Cell, rc.retSize)
	if rc.retSize > 1 {
		base := l.allocCells(rc.retSize)
		for j := range rc.retSize {
			retCells[j] = base + j
			l.emit(&IRCopy{Dst: retCells[j], Src: rc.retReg + j})
		}
	} else {
		for j := range rc.retSize {
			retCells[j] = l.allocCell()
			l.emit(&IRCopy{Dst: retCells[j], Src: rc.retReg + j})
		}
	}

	// activeReg and retReg are phase temp positions, no need to free.

	return retCells, nil
}

// hasBitwise reports whether a function body uses any bitwise operator
// (`&`, `|`, `^`, `&^`) or compound assign. The result decides whether
// lowerGeneralRecursion relocates the dispatch cells into the phase-temp
// area: with bitwise present we need the full algo-temp pool for
// genBitwise; otherwise we keep the cheaper algo-temp layout.
func hasBitwise(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		switch n := n.(type) {
		case *ast.BinaryExpr:
			switch n.Op {
			case token.AND, token.OR, token.XOR, token.AND_NOT:
				found = true
			}
		case *ast.AssignStmt:
			switch n.Tok {
			case token.AND_ASSIGN, token.OR_ASSIGN, token.XOR_ASSIGN, token.AND_NOT_ASSIGN:
				found = true
			}
		}
		return !found
	})
	return found
}

// rejectComposites scans the function body for any struct, array, or
// slice usage and returns an error if found. Recursive functions are
// scalar-only by design (params, returns, and locals).
func (l *Lowerer) rejectComposites(body *ast.BlockStmt) error {
	var err error
	ast.Inspect(body, func(n ast.Node) bool {
		if err != nil {
			return false
		}
		switch n := n.(type) {
		case *ast.ArrayType:
			// []T (slice) when Len is nil; [N]T (array) otherwise.
			kind := "array"
			if n.Len == nil {
				kind = "slice"
			}
			err = fmt.Errorf("%s usage in recursive function is not supported", kind)
		case *ast.StructType:
			// Anonymous struct: `struct{...}` type literal.
			err = fmt.Errorf("struct usage in recursive function is not supported")
		case *ast.Ident:
			// Named struct type used in a value context (var p Point,
			// Point{...}, etc.). The non-struct idents pass through.
			if l.structDef(n) != nil {
				err = fmt.Errorf("struct usage in recursive function is not supported")
			}
		}
		return true
	})
	return err
}

// collectIntLocals scans the body for uintN variable declarations
// (`var x uint16`, `x := uint16(...)`, `b := a` where a is uintN, etc.)
// and reallocates their frame slots to hold IntSize cells. Returns an
// error when a `uint64` local is found: the eight-cell layout would
// span the highway marker at position 32 once `sentinelFwd` is bumped,
// the same constraint that blocks `uint64` returns.
func (l *Lowerer) collectIntLocals(rc *recContext, body *ast.BlockStmt) error {
	seen := make(map[string]bool)
	var firstErr error
	register := func(name string, intSize int) {
		if seen[name] || intSize < 2 {
			return
		}
		if intSize >= 8 && firstErr == nil {
			firstErr = fmt.Errorf("uint64 local %s in recursive function is not supported", name)
			return
		}
		seen[name] = true
		rc.locals[name] = recLocalInfo{slot: rc.frameSize, intSize: intSize}
		rc.frameSize += intSize
	}
	intSizeOf := func(typ ast.Expr) int {
		if id, ok := typ.(*ast.Ident); ok {
			return intIdentSize(id.Name)
		}
		return 0
	}
	// inferExprIntSize walks an expression tree to determine its uintN
	// width: uintN(...) conversion is N; a uintN local lookup returns
	// its size; a call to a uintN-returning function picks up the
	// callee's return width; binary op of two uintN operands inherits
	// the width.
	var inferExprIntSize func(e ast.Expr) int
	inferExprIntSize = func(e ast.Expr) int {
		switch x := e.(type) {
		case *ast.ParenExpr:
			return inferExprIntSize(x.X)
		case *ast.CallExpr:
			if fn, ok := x.Fun.(*ast.Ident); ok {
				if size := intIdentSize(fn.Name); size >= 2 {
					return size
				}
				if info, ok := l.result.Funcs[fn.Name]; ok {
					if n := info.SingleReturn().IntSize; n >= 2 {
						return n
					}
				}
			}
		case *ast.Ident:
			if src, ok := rc.locals[x.Name]; ok && src.intSize >= 2 {
				return src.intSize
			}
		case *ast.BinaryExpr:
			if l := inferExprIntSize(x.X); l >= 2 {
				return l
			}
			return inferExprIntSize(x.Y)
		}
		return 0
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if firstErr != nil {
			return false
		}
		if decl, ok := n.(*ast.DeclStmt); ok {
			if gd, ok := decl.Decl.(*ast.GenDecl); ok {
				for _, spec := range gd.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						if size := intSizeOf(vs.Type); size >= 2 {
							for _, name := range vs.Names {
								register(name.Name, size)
							}
						}
					}
				}
			}
		}
		if assign, ok := n.(*ast.AssignStmt); ok && assign.Tok == token.DEFINE && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
			if id, ok := assign.Lhs[0].(*ast.Ident); ok {
				if size := inferExprIntSize(assign.Rhs[0]); size >= 2 {
					register(id.Name, size)
				}
			}
		}
		return true
	})
	return firstErr
}

// collectLocals finds all := variable names in the function body that aren't parameters.
func collectLocals(body *ast.BlockStmt, params []string) []string {
	paramSet := make(map[string]bool)
	for _, p := range params {
		paramSet[p] = true
	}
	seen := make(map[string]bool)
	var locals []string
	ast.Inspect(body, func(n ast.Node) bool {
		if assign, ok := n.(*ast.AssignStmt); ok && assign.Tok == token.DEFINE {
			for _, lhs := range assign.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					if id.Name != "_" && !paramSet[id.Name] && !seen[id.Name] {
						seen[id.Name] = true
						locals = append(locals, id.Name)
					}
				}
			}
		}
		// Switch with tag needs a $switch variable.
		if sw, ok := n.(*ast.SwitchStmt); ok && sw.Tag != nil {
			if !seen["$switch"] {
				seen["$switch"] = true
				locals = append(locals, "$switch")
			}
		}
		// Var declarations. Const declarations are skipped: they're
		// inlined at use sites via constBinding/intConstBinding in the
		// scope, not stored in a frame slot.
		if decl, ok := n.(*ast.DeclStmt); ok {
			if gd, ok := decl.Decl.(*ast.GenDecl); ok && gd.Tok != token.CONST {
				for _, spec := range gd.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, name := range vs.Names {
							if !paramSet[name.Name] && !seen[name.Name] {
								seen[name.Name] = true
								locals = append(locals, name.Name)
							}
						}
					}
				}
			}
		}
		// Range variables.
		if rs, ok := n.(*ast.RangeStmt); ok {
			if rs.Key != nil {
				if id, ok := rs.Key.(*ast.Ident); ok {
					if !paramSet[id.Name] && !seen[id.Name] {
						seen[id.Name] = true
						locals = append(locals, id.Name)
					}
				}
			}
			if rs.Value != nil {
				if id, ok := rs.Value.(*ast.Ident); ok {
					if !paramSet[id.Name] && !seen[id.Name] {
						seen[id.Name] = true
						locals = append(locals, id.Name)
					}
				}
			}
		}
		return true
	})
	return locals
}

// buildPhases splits the recursive function body into phases.
// Each recursive call site creates a phase boundary.
func (l *Lowerer) buildPhases(rc *recContext, info *FuncInfo) error {
	// Find all recursive call sites in the body.
	// We pre-process: rewrite `return fib(n-1) + fib(n-2)` into
	// `a := fib(n-1); b := fib(n-2); return a + b`
	// This is done by extracting recursive calls from expressions.
	stmts := info.Body.List

	// Split statements into segments separated by recursive calls.
	// Each segment becomes one phase.
	var currentPhaseStmts []ast.Stmt
	var callSites []recCallSite // info about each recursive call

	// flatStmts is the flattened list of statements after expanding if-statements
	// that contain recursive calls. We process it with an index so that newly
	// appended statements (from flattening) are also visited.
	flatStmts := append([]ast.Stmt{}, stmts...)

	for i := 0; i < len(flatStmts); i++ {
		stmt := flatStmts[i]
		calls := findRecursiveCalls(stmt, rc.funcName)
		if len(calls) == 0 {
			currentPhaseStmts = append(currentPhaseStmts, stmt)
			continue
		}
		// Try processing as a simple recursive statement (assign, return, etc.).
		newSites, tail, ok := processRecStmt(stmt, calls, rc, currentPhaseStmts, "")
		if ok {
			callSites = append(callSites, newSites...)
			currentPhaseStmts = tail
			continue
		}
		// Switch statement: desugar to if-else chain, then re-process.
		if sw, ok := stmt.(*ast.SwitchStmt); ok {
			var desugared []ast.Stmt
			if sw.Init != nil {
				desugared = append(desugared, sw.Init)
			}
			if sw.Tag != nil {
				// Store tag: $switch := tag
				desugared = append(desugared, &ast.AssignStmt{
					Lhs: []ast.Expr{ast.NewIdent("$switch")},
					Tok: token.DEFINE,
					Rhs: []ast.Expr{sw.Tag},
				})
				if _, exists := rc.locals["$switch"]; !exists {
					rc.locals["$switch"] = recLocalInfo{slot: rc.frameSize}
					rc.frameSize++
				}
			}
			tagName := ""
			if sw.Tag != nil {
				tagName = "$switch"
			}
			ifStmt := l.buildSwitchIf(sw.Body.List, tagName)
			if ifStmt != nil {
				desugared = append(desugared, ifStmt)
			}
			desugared = append(desugared, flatStmts[i+1:]...)
			flatStmts = append(flatStmts[:i], desugared...)
			i-- // re-process
			continue
		}
		// If statement containing recursive calls: flatten into the sequence.
		if ifStmt, ok := stmt.(*ast.IfStmt); ok {
			expanded, newSites, tail, err := flattenIfWithRecCalls(ifStmt, rc, currentPhaseStmts, flatStmts[i+1:])
			if err != nil {
				return err
			}
			if expanded != nil {
				// Single-branch case: replace remaining statements with expanded form.
				flatStmts = append(flatStmts[:i], expanded...)
				i-- // re-process from the current position
			} else {
				// Both-branches case: call sites created directly.
				callSites = append(callSites, newSites...)
				currentPhaseStmts = tail
				// Skip remaining statements (consumed by flattenIfWithRecCalls).
				i = len(flatStmts)
			}
			continue
		}
		// Fallback: unsupported pattern.
		return fmt.Errorf("unsupported recursive call pattern in %s", rc.funcName)
	}

	// Build phase IR blocks.
	// Phase 0 = preStmts of callSite 0 (if any), otherwise the whole body.
	// Phase K (for K>0) = continuation after callSite K-1.

	// For multi-cell uintN returns, result variables need N contiguous frame
	// slots. Re-allocate them at the end of the frame, overriding the single-
	// slot allocation from collectLocals.
	if rc.retSize > 1 {
		retIntSize := info.SingleReturn().IntSize
		allocated := make(map[string]bool)
		for _, cs := range callSites {
			info := rc.locals[cs.resultVar]
			if !allocated[cs.resultVar] {
				info.slot = rc.frameSize
				rc.frameSize += rc.retSize
				allocated[cs.resultVar] = true
			}
			info.intSize = retIntSize
			rc.locals[cs.resultVar] = info
		}
	}

	// Phase 0: code before first recursive call + push child frame.
	phase0, err := l.buildRecPhaseWithCall(rc, callSites[0].preStmts, callSites[0], 1)
	if err != nil {
		return err
	}
	rc.phases = append(rc.phases, phase0)

	// Phases 1..N-1: load child result, continue code, push next child.
	for i := 1; i < len(callSites); i++ {
		phase, err := l.buildRecPhaseWithCall(rc, callSites[i].preStmts, callSites[i], i+1)
		if err != nil {
			return err
		}
		// Prepend: load retReg into the result variable of callSite[i-1].
		prevSlot := rc.locals[callSites[i-1].resultVar].slot
		var loadRet []IRNode
		for j := range rc.retSize {
			loadRet = append(loadRet, &IRStoreFrame{Slot: prevSlot + j, Src: rc.retReg + j, FrameSize: rc.frameSize})
		}
		phase.Nodes = append(loadRet, phase.Nodes...)
		rc.phases = append(rc.phases, phase)
	}

	// Final phase: load last child's result, run remaining code, return.
	// Ensure the phase ends with a return (needed for void functions
	// where the tail may be empty).
	if len(currentPhaseStmts) == 0 || !endsWithReturn(currentPhaseStmts) {
		currentPhaseStmts = append(currentPhaseStmts, &ast.ReturnStmt{})
	}
	lastCallSite := callSites[len(callSites)-1]
	finalPhase, err := l.buildRecPhase(rc, currentPhaseStmts)
	if err != nil {
		return err
	}
	// Prepend: load retReg into last result variable.
	prevSlot := rc.locals[lastCallSite.resultVar].slot
	var loadRet []IRNode
	for j := range rc.retSize {
		loadRet = append(loadRet, &IRStoreFrame{Slot: prevSlot + j, Src: rc.retReg + j, FrameSize: rc.frameSize})
	}
	finalPhase.Nodes = append(loadRet, finalPhase.Nodes...)
	rc.phases = append(rc.phases, finalPhase)

	return nil
}

func endsWithReturn(stmts []ast.Stmt) bool {
	if len(stmts) == 0 {
		return false
	}
	_, ok := stmts[len(stmts)-1].(*ast.ReturnStmt)
	return ok
}

type recCallSite struct {
	argExprs  []ast.Expr
	resultVar string
	preStmts  []ast.Stmt
	condVar   string // if set, call is conditional on this frame variable being nonzero
}

// processRecStmt tries to process a single statement containing recursive calls
// into call sites. Returns (callSites, tailStmts, ok). If the statement pattern
// is not recognized, ok is false. condVar is passed through to call sites.
func processRecStmt(stmt ast.Stmt, calls []*ast.CallExpr, rc *recContext, preStmts []ast.Stmt, condVar string) ([]recCallSite, []ast.Stmt, bool) {
	// Direct assignment: a := f(n-1) where the entire RHS is the call.
	// (For `a := expr-containing-call`, the next branch wraps the call in a
	// temp and rewrites the RHS; this branch must only match the bare form.)
	if assign, ok := stmt.(*ast.AssignStmt); ok && len(calls) == 1 &&
		len(assign.Rhs) == 1 && assign.Rhs[0] == calls[0] {
		id, ok := assign.Lhs[0].(*ast.Ident)
		if !ok {
			return nil, nil, false
		}
		return []recCallSite{{
			argExprs:  calls[0].Args,
			resultVar: id.Name,
			preStmts:  preStmts,
			condVar:   condVar,
		}}, nil, true
	}
	// Assignment with expression containing recursive calls: x = f(n-1) + f(n-2)
	if assign, ok := stmt.(*ast.AssignStmt); ok && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
		if id, ok := assign.Lhs[0].(*ast.Ident); ok {
			extracted, resultExpr := extractRecCalls(assign.Rhs[0], rc)
			if len(extracted) > 0 {
				var sites []recCallSite
				cur := preStmts
				for _, ext := range extracted {
					sites = append(sites, recCallSite{
						argExprs:  ext.call.Args,
						resultVar: ext.tmpName,
						preStmts:  cur,
						condVar:   condVar,
					})
					cur = nil
				}
				tail := []ast.Stmt{&ast.AssignStmt{
					Lhs: []ast.Expr{ast.NewIdent(id.Name)},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{resultExpr},
				}}
				return sites, tail, true
			}
		}
	}
	// Return with direct recursive call: return f(n-1)
	if ret, ok := stmt.(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
		if call, isCall := ret.Results[0].(*ast.CallExpr); isCall && len(calls) == 1 && call == calls[0] {
			if _, exists := rc.locals["$tailret"]; !exists {
				rc.locals["$tailret"] = recLocalInfo{slot: rc.frameSize}
				rc.frameSize++
			}
			return []recCallSite{{
					argExprs:  calls[0].Args,
					resultVar: "$tailret",
					preStmts:  preStmts,
					condVar:   condVar,
				}}, []ast.Stmt{
					&ast.ReturnStmt{Results: []ast.Expr{ast.NewIdent("$tailret")}},
				}, true
		}
	}
	// Return with expression containing recursive calls: return f(n-1) + f(n-2)
	if ret, ok := stmt.(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
		extracted, resultExpr := extractRecCalls(ret.Results[0], rc)
		if len(extracted) > 0 {
			var sites []recCallSite
			cur := preStmts
			for _, ext := range extracted {
				sites = append(sites, recCallSite{
					argExprs:  ext.call.Args,
					resultVar: ext.tmpName,
					preStmts:  cur,
					condVar:   condVar,
				})
				cur = nil
			}
			tail := []ast.Stmt{&ast.ReturnStmt{Results: []ast.Expr{resultExpr}}}
			return sites, tail, true
		}
	}
	// Void recursive call: f(n-1) as a bare statement.
	if expr, ok := stmt.(*ast.ExprStmt); ok {
		if _, isCall := expr.X.(*ast.CallExpr); isCall && len(calls) == 1 {
			if _, exists := rc.locals["$void"]; !exists {
				rc.locals["$void"] = recLocalInfo{slot: rc.frameSize}
				rc.frameSize++
			}
			return []recCallSite{{
				argExprs:  calls[0].Args,
				resultVar: "$void",
				preStmts:  preStmts,
				condVar:   condVar,
			}}, []ast.Stmt{}, true
		}
	}
	return nil, nil, false
}

// flattenIfWithRecCalls rewrites an if-statement containing recursive calls.
//
// For single-branch cases (recursive calls in only one branch), it returns
// expanded statements (expanded != nil) for re-processing by the main loop.
//
// For both-branches cases, it directly creates call sites with condVar set
// and returns them (expanded == nil, callSites != nil).
func flattenIfWithRecCalls(ifStmt *ast.IfStmt, rc *recContext, preStmts, restStmts []ast.Stmt) (expanded []ast.Stmt, callSites []recCallSite, tail []ast.Stmt, err error) {
	thenCalls := findRecursiveCalls(ifStmt.Body, rc.funcName)
	var elseCalls []*ast.CallExpr
	if ifStmt.Else != nil {
		elseCalls = findRecursiveCalls(ifStmt.Else, rc.funcName)
	}
	restCalls := findRecursiveCallsInStmts(restStmts, rc.funcName)

	// Single-branch cases: return expanded statements for re-processing.
	if len(thenCalls) > 0 && len(elseCalls) == 0 && len(restCalls) == 0 {
		exp, err := flattenIfThenRec(ifStmt, restStmts)
		return exp, nil, nil, err
	}
	if len(thenCalls) == 0 && len(elseCalls) > 0 && len(restCalls) == 0 {
		exp, err := flattenIfElseRec(ifStmt, restStmts)
		return exp, nil, nil, err
	}

	// Both-branches case: create call sites directly with condVar.
	if len(thenCalls) > 0 && (len(elseCalls) > 0 || len(restCalls) > 0) {
		sites, tail, err := flattenIfBothRec(ifStmt, rc, preStmts, restStmts)
		return nil, sites, tail, err
	}
	return nil, nil, nil, fmt.Errorf("unsupported recursive call pattern in %s", rc.funcName)
}

// flattenIfThenRec handles the case where recursive calls are only in the
// then-branch. The else-branch (which has no recursive calls) becomes an
// early-return guard under the inverted condition, then the then-body's
// statements are spliced in, followed by the remaining statements.
func flattenIfThenRec(ifStmt *ast.IfStmt, restStmts []ast.Stmt) ([]ast.Stmt, error) {
	var result []ast.Stmt

	if ifStmt.Init != nil {
		result = append(result, ifStmt.Init)
	}

	// The guard must contain a return to set noRetFlag=0 when the condition
	// is false. Use the else-branch if it has one, or the restStmts.
	var guardBody []ast.Stmt
	if ifStmt.Else != nil {
		switch e := ifStmt.Else.(type) {
		case *ast.BlockStmt:
			guardBody = append(guardBody, e.List...)
		case *ast.IfStmt:
			guardBody = append(guardBody, e)
		}
	}
	guardBody = append(guardBody, restStmts...)

	invertedCond := &ast.UnaryExpr{Op: token.NOT, X: &ast.ParenExpr{X: ifStmt.Cond}}
	guard := &ast.IfStmt{
		Cond: invertedCond,
		Body: &ast.BlockStmt{List: guardBody},
	}
	result = append(result, guard)

	// Splice the then-body statements.
	result = append(result, ifStmt.Body.List...)

	return result, nil
}

// flattenIfElseRec handles the case where recursive calls are only in the
// else-branch. The then-branch and remaining statements become an early-return
// guard under the original condition, followed by the else-body's statements.
func flattenIfElseRec(ifStmt *ast.IfStmt, restStmts []ast.Stmt) ([]ast.Stmt, error) {
	var result []ast.Stmt

	if ifStmt.Init != nil {
		result = append(result, ifStmt.Init)
	}

	// Guard: if cond { then-body; rest }
	guardBody := append([]ast.Stmt{}, ifStmt.Body.List...)
	guardBody = append(guardBody, restStmts...)
	guard := &ast.IfStmt{
		Cond: ifStmt.Cond,
		Body: &ast.BlockStmt{List: guardBody},
	}
	result = append(result, guard)

	// Splice the else-body statements.
	switch e := ifStmt.Else.(type) {
	case *ast.BlockStmt:
		result = append(result, e.List...)
	case *ast.IfStmt:
		result = append(result, e)
	}

	return result, nil
}

// flattenIfBothRec handles if-statements where both branches (or then + fallthrough)
// contain recursive calls. It stores the condition in a frame variable and creates
// call sites with condVar so the then-branch calls are conditional.
//
// For: if cond { return f(a)+1 } else { return f(b)+2 }
// Produces call sites:
//   - $rec_0 := f(a) [condVar=$cond, preStmts includes "$cond := cond"]
//   - $rec_1 := f(b) [unconditional, preStmts includes "if $cond { return $rec_0+1 }"]
//
// And tail: [return $rec_1 + 2]
func flattenIfBothRec(ifStmt *ast.IfStmt, rc *recContext, preStmts, restStmts []ast.Stmt) ([]recCallSite, []ast.Stmt, error) {
	// Allocate frame variable for the condition.
	condVar := fmt.Sprintf("$ifcond_%d", rc.frameSize)
	if _, exists := rc.locals[condVar]; !exists {
		rc.locals[condVar] = recLocalInfo{slot: rc.frameSize}
		rc.frameSize++
	}

	// Build preStmts: existing preStmts + init + $cond := cond
	var pre []ast.Stmt
	pre = append(pre, preStmts...)
	if ifStmt.Init != nil {
		pre = append(pre, ifStmt.Init)
	}
	pre = append(pre, &ast.AssignStmt{
		Lhs: []ast.Expr{ast.NewIdent(condVar)},
		Tok: token.DEFINE,
		Rhs: []ast.Expr{ifStmt.Cond},
	})

	// Process then-body: extract call sites with condVar.
	var allSites []recCallSite
	currentPre := pre
	for _, stmt := range ifStmt.Body.List {
		calls := findRecursiveCalls(stmt, rc.funcName)
		if len(calls) == 0 {
			currentPre = append(currentPre, stmt)
			continue
		}
		sites, tail, ok := processRecStmt(stmt, calls, rc, currentPre, condVar)
		if !ok {
			return nil, nil, fmt.Errorf("unsupported recursive call pattern in then-branch of %s", rc.funcName)
		}
		allSites = append(allSites, sites...)
		currentPre = tail
	}

	// After then-body call sites, add a guard: if $cond { <then-tail> }
	// This returns when cond was true (setting noRetFlag=0, skipping else calls).
	// When cond is false, it falls through to the else-branch calls.
	if len(currentPre) > 0 {
		// Wrap remaining then-body statements in: if $cond { stmts }
		guardedThen := &ast.IfStmt{
			Cond: ast.NewIdent(condVar),
			Body: &ast.BlockStmt{List: currentPre},
		}
		currentPre = []ast.Stmt{guardedThen}
	}

	// Process else-body (or restStmts for then+fallthrough case).
	var elseStmts []ast.Stmt
	if ifStmt.Else != nil {
		switch e := ifStmt.Else.(type) {
		case *ast.BlockStmt:
			elseStmts = e.List
		case *ast.IfStmt:
			elseStmts = []ast.Stmt{e}
		}
	}
	elseStmts = append(elseStmts, restStmts...)

	for _, stmt := range elseStmts {
		calls := findRecursiveCalls(stmt, rc.funcName)
		if len(calls) == 0 {
			currentPre = append(currentPre, stmt)
			continue
		}
		sites, tail, ok := processRecStmt(stmt, calls, rc, currentPre, "")
		if ok {
			allSites = append(allSites, sites...)
			currentPre = tail
			continue
		}
		// Nested if-else-if with recursive calls (e.g., from switch desugaring).
		if nestedIf, ok := stmt.(*ast.IfStmt); ok {
			nestedSites, nestedTail, err := flattenIfBothRec(nestedIf, rc, currentPre, nil)
			if err != nil {
				return nil, nil, err
			}
			allSites = append(allSites, nestedSites...)
			currentPre = nestedTail
			continue
		}
		return nil, nil, fmt.Errorf("unsupported recursive call pattern in else-branch of %s", rc.funcName)
	}

	return allSites, currentPre, nil
}

func findRecursiveCallsInStmts(stmts []ast.Stmt, funcName string) []*ast.CallExpr {
	var calls []*ast.CallExpr
	for _, s := range stmts {
		calls = append(calls, findRecursiveCalls(s, funcName)...)
	}
	return calls
}

type extractedCall struct {
	call    *ast.CallExpr
	tmpName string
}

func findRecursiveCalls(node ast.Node, funcName string) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		if ident.Name == funcName {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

// extractRecCalls extracts recursive calls from an expression, replacing them
// with temporary variable references. Returns the extracted calls and the
// modified expression.
func extractRecCalls(expr ast.Expr, rc *recContext) ([]extractedCall, ast.Expr) {
	var extracted []extractedCall
	counter := 0

	var rewrite func(e ast.Expr) ast.Expr
	rewrite = func(e ast.Expr) ast.Expr {
		if call, ok := e.(*ast.CallExpr); ok {
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == rc.funcName {
				// Rewrite arguments first to extract nested recursive calls.
				newArgs := make([]ast.Expr, len(call.Args))
				for i, arg := range call.Args {
					newArgs[i] = rewrite(arg)
				}
				tmpName := fmt.Sprintf("$recursive_%d", counter)
				counter++
				if _, exists := rc.locals[tmpName]; !exists {
					rc.locals[tmpName] = recLocalInfo{slot: rc.frameSize}
					rc.frameSize++
				}
				extracted = append(extracted, extractedCall{
					call:    &ast.CallExpr{Fun: call.Fun, Args: newArgs},
					tmpName: tmpName,
				})
				return ast.NewIdent(tmpName)
			}
			// Non-recursive call: rewrite arguments.
			newArgs := make([]ast.Expr, len(call.Args))
			for i, arg := range call.Args {
				newArgs[i] = rewrite(arg)
			}
			return &ast.CallExpr{Fun: call.Fun, Args: newArgs, Lparen: call.Lparen, Rparen: call.Rparen}
		}
		if bin, ok := e.(*ast.BinaryExpr); ok {
			return &ast.BinaryExpr{
				X:     rewrite(bin.X),
				OpPos: bin.OpPos,
				Op:    bin.Op,
				Y:     rewrite(bin.Y),
			}
		}
		if paren, ok := e.(*ast.ParenExpr); ok {
			return &ast.ParenExpr{
				Lparen: paren.Lparen,
				X:      rewrite(paren.X),
				Rparen: paren.Rparen,
			}
		}
		if unary, ok := e.(*ast.UnaryExpr); ok {
			return &ast.UnaryExpr{
				OpPos: unary.OpPos,
				Op:    unary.Op,
				X:     rewrite(unary.X),
			}
		}
		return e
	}

	result := rewrite(expr)
	return extracted, result
}

// enterPhase saves the parent emission state and redirects allocation to
// the phase-temp range. The returned function restores the saved state;
// callers should `defer` it.
func (l *Lowerer) enterPhase(rc *recContext) func() {
	savedNodes := l.nodes
	savedNext := l.nextCell
	savedFree := l.freeCells
	l.nodes = nil
	// Phase code uses cells starting past activeReg, retReg, and the
	// four dispatch-reserved cells -- phaseCodeBase already accounts
	// for highway-marker skipping done at allocation time.
	l.nextCell = Cell(rc.phaseCodeBase)
	l.freeCells = nil
	l.recFrameSize = rc.frameSize
	l.recAllocErr = nil
	return func() {
		l.nodes = savedNodes
		l.nextCell = savedNext
		l.freeCells = savedFree
		l.recFrameSize = 0
	}
}

// buildRecPhase builds an IR block for a phase that ends with a return (pop frame).
func (l *Lowerer) buildRecPhase(rc *recContext, stmts []ast.Stmt) (*IRBlock, error) {
	defer l.enterPhase(rc)()

	// Allocate noRetFlag so lowerStmts can guard statements after a return.
	rc.noRetFlag = l.allocCell()
	l.emit(&IRConst{Dst: rc.noRetFlag, Value: 1})

	rl := l.newRecLowerer(rc)
	if err := rl.lowerStmts(stmts); err != nil {
		return nil, err
	}
	if l.recAllocErr != nil {
		return nil, l.recAllocErr
	}
	return &IRBlock{Nodes: l.nodes}, nil
}

// buildRecPhaseWithCall builds a phase that ends by pushing a child frame.
func (l *Lowerer) buildRecPhaseWithCall(rc *recContext, stmts []ast.Stmt, call recCallSite, nextPhase int) (*IRBlock, error) {
	defer l.enterPhase(rc)()

	// Allocate a "did not return" flag. Set to 1 at phase start.
	// lowerReturn clears it. The call setup checks it instead of activeReg.
	rc.noRetFlag = l.allocCell()
	l.emit(&IRConst{Dst: rc.noRetFlag, Value: 1})

	rl := l.newRecLowerer(rc)
	// Lower the pre-call statements (may include base case returns).
	if err := rl.lowerStmts(stmts); err != nil {
		return nil, err
	}
	guardNodes := l.nodes

	// Save loadedMap before arg evaluation. Arg evaluation may load
	// variables (e.g., n in f(n-1)) that add to loadedMap. The skip
	// branch must not store these -- they weren't loaded in the skip path.
	savedLoadedMap := maps.Clone(rl.loadedMap)

	// Build call setup code on a fresh list.
	l.nodes = nil

	// Evaluate call arguments. Byte args produce 1 cell; uint16/uint32
	// args produce intSize contiguous cells. Pointer/struct/array params
	// are rejected at inlineCall.
	type argValue struct {
		cells []Cell
	}
	info := rl.result.Funcs[rc.funcName]
	argVals := make([]argValue, len(call.argExprs))
	for i, expr := range call.argExprs {
		r, err := rl.lowerExpr(expr)
		if err != nil {
			return nil, err
		}
		intSize := 0
		if i < len(info.ParamTypes) {
			intSize = info.ParamTypes[i].IntSize
		}
		if intSize >= 2 {
			cells := make([]Cell, intSize)
			for j := range intSize {
				cells[j] = r.cell + j
			}
			argVals[i] = argValue{cells}
		} else {
			argVals[i] = argValue{[]Cell{r.cell}}
		}
	}

	// Store all modified locals back to the frame before pushing child.
	rl.storeAllLocals()

	// Set continuation phase in current frame.
	phaseConst := l.allocCell()
	l.emit(&IRConst{Dst: phaseConst, Value: byte(nextPhase)}) // #nosec G115 -- nextPhase < 256
	l.emit(&IRStoreFrame{Slot: rc.slotPhase, Src: phaseConst, FrameSize: rc.frameSize})
	l.freeCell(phaseConst)

	// Increment active (depth counter) for child frame.
	l.emit(&IRAddI{Dst: rc.activeReg, Value: 1})

	// Push child frame.
	l.emit(&IRFramePush{Slots: rc.frameSize})

	// Store args into child frame's param slots.
	paramSlot := rc.paramBase
	for _, av := range argVals {
		for _, cell := range av.cells {
			l.emit(&IRStoreFrame{Slot: paramSlot, Src: cell, FrameSize: rc.frameSize})
			paramSlot++
		}
	}

	// Store phase = 0 in child frame.
	zeroConst := l.allocCell()
	l.emit(&IRConst{Dst: zeroConst, Value: 0})
	l.emit(&IRStoreFrame{Slot: rc.slotPhase, Src: zeroConst, FrameSize: rc.frameSize})
	l.freeCell(zeroConst)

	callNodes := l.nodes

	if l.recAllocErr != nil {
		return nil, l.recAllocErr
	}

	// For conditional calls: when the condition is false, skip the call
	// by just advancing the phase without pushing a child frame.
	if call.condVar != "" {
		// Load condVar from the frame. It may have been set in an earlier phase.
		condSlot := rc.locals[call.condVar].slot
		condCell, ok := rl.loadedMap[condSlot]
		if !ok {
			condCell = l.allocCell()
			l.emit(&IRLoadFrame{Dst: condCell, Slot: condSlot, FrameSize: rc.frameSize})
			rl.loadedMap[condSlot] = condCell
		}

		// Build skip branch: just set phase = nextPhase.
		// Use savedLoadedMap to avoid storing variables loaded during arg evaluation.
		l.nodes = nil
		skipRL := &recLowerer{Lowerer: l, rc: rc, loadedMap: maps.Clone(savedLoadedMap)}
		skipRL.storeAllLocals()
		skipPhaseConst := l.allocCell()
		l.emit(&IRConst{Dst: skipPhaseConst, Value: byte(nextPhase)}) // #nosec G115
		l.emit(&IRStoreFrame{Slot: rc.slotPhase, Src: skipPhaseConst, FrameSize: rc.frameSize})
		l.freeCell(skipPhaseConst)
		skipNodes := l.nodes

		// Wrap: if condVar { callNodes } else { skipNodes }
		l.nodes = nil
		callNodes = []IRNode{&IRIf{
			Cond: condCell,
			Then: &IRBlock{Nodes: callNodes},
			Else: &IRBlock{Nodes: skipNodes},
		}}
	}

	// Combine: guard nodes + if(noRetFlag) { call nodes }
	// noRetFlag is 1 if the pre-stmts didn't return, 0 if they did.
	allNodes := make([]IRNode, len(guardNodes))
	copy(allNodes, guardNodes)
	if len(callNodes) > 0 {
		allNodes = append(allNodes, &IRIf{
			Cond: rc.noRetFlag,
			Then: &IRBlock{Nodes: callNodes},
		})
	}
	return &IRBlock{Nodes: allNodes}, nil
}

// recLowerer is a specialized lowerer for recursive function phases.
// It uses register cells only and emits load/store for frame access.
type recLowerer struct {
	*Lowerer
	rc        *recContext
	loadedMap map[int]Cell // slot -> register cell (cache)
}

func (l *Lowerer) newRecLowerer(rc *recContext) *recLowerer {
	return &recLowerer{
		Lowerer:   l,
		rc:        rc,
		loadedMap: make(map[int]Cell),
	}
}

// lookupVar overrides the default to load from the frame. For uintN
// locals, allocates intSize contiguous cells and loads each from the
// frame; loadedMap caches the base cell.
func (rl *recLowerer) lookupVar(name string) (Cell, error) {
	if name == "_" {
		return rl.allocCell(), nil
	}
	info, ok := rl.rc.locals[name]
	if !ok {
		return 0, fmt.Errorf("undefined variable in recursive function: %s", name)
	}
	slot := info.slot
	if reg, ok := rl.loadedMap[slot]; ok {
		return reg, nil
	}
	if info.intSize >= 2 {
		base := rl.allocCells(info.intSize)
		rl.loadFrame(base, slot, info.intSize)
		rl.loadedMap[slot] = base
		return base, nil
	}
	reg := rl.allocCell()
	rl.loadFrame(reg, slot, 1)
	rl.loadedMap[slot] = reg
	return reg, nil
}

// slotSize returns the cell count for a frame slot. Most slots are 1
// cell; uint16/uint32 locals occupy intSize contiguous cells.
func (rl *recLowerer) slotSize(slot int) int {
	for _, info := range rl.rc.locals {
		if info.slot == slot && info.intSize >= 2 {
			return info.intSize
		}
	}
	return 1
}

// captureBlock redirects emit to a fresh node list while fn runs, then
// restores the prior list and returns the captured block. Emission is
// restored even when fn errors.
func (rl *recLowerer) captureBlock(fn func() error) (*IRBlock, error) {
	saved := rl.nodes
	rl.nodes = nil
	err := fn()
	block := &IRBlock{Nodes: rl.nodes}
	rl.nodes = saved
	return block, err
}

// loadFrame emits n IRLoadFrame nodes copying frame slots [slot, slot+n)
// into cells [dst, dst+n).
func (rl *recLowerer) loadFrame(dst Cell, slot, n int) {
	for j := range n {
		rl.emit(&IRLoadFrame{Dst: dst + j, Slot: slot + j, FrameSize: rl.rc.frameSize})
	}
}

// storeFrame emits n IRStoreFrame nodes copying cells [src, src+n) into
// frame slots [slot, slot+n).
func (rl *recLowerer) storeFrame(slot int, src Cell, n int) {
	for j := range n {
		rl.emit(&IRStoreFrame{Slot: slot + j, Src: src + j, FrameSize: rl.rc.frameSize})
	}
}

// reloadAllLocals re-reads all cached locals from the frame into their
// existing phase temp cells.
func (rl *recLowerer) reloadAllLocals() {
	for slot, reg := range rl.loadedMap {
		rl.loadFrame(reg, slot, rl.slotSize(slot))
	}
}

// storeAllLocals writes all loaded (and potentially modified) locals back to the frame.
func (rl *recLowerer) storeAllLocals() {
	for slot, reg := range rl.loadedMap {
		rl.storeFrame(slot, reg, rl.slotSize(slot))
	}
}

// lowerStmts processes statements within a recursive phase.
// Each statement is guarded by noRetFlag so that statements after a return
// inside an if-without-else are skipped.
func (rl *recLowerer) lowerStmts(stmts []ast.Stmt) error {
	for i, stmt := range stmts {
		if i == 0 {
			// First statement: noRetFlag is always 1, no guard needed.
			if err := rl.lowerStmt(stmt); err != nil {
				return err
			}
			continue
		}
		// Wrap in IRIf with empty else to skip if a prior return happened.
		// The else block must be non-nil so emitIfElse is used (preserves noRetFlag).
		body, err := rl.captureBlock(func() error { return rl.lowerStmt(stmt) })
		if err != nil {
			return err
		}
		rl.emit(&IRIf{Cond: rl.rc.noRetFlag, Then: body, Else: &IRBlock{}})
	}
	return nil
}

func (rl *recLowerer) lowerStmt(stmt ast.Stmt) error {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		return rl.lowerExprStmt(s)
	case *ast.DeclStmt:
		return rl.lowerDecl(s)
	case *ast.AssignStmt:
		return rl.lowerAssign(s)
	case *ast.IncDecStmt:
		return rl.lowerIncDec(s)
	case *ast.IfStmt:
		return rl.lowerIf(s)
	case *ast.SwitchStmt:
		return rl.lowerSwitch(s)
	case *ast.ForStmt:
		return rl.lowerFor(s)
	case *ast.RangeStmt:
		return rl.lowerRange(s)
	case *ast.BranchStmt:
		return rl.lowerBranch(s)
	case *ast.LabeledStmt:
		return rl.lowerLabeledStmt(s)
	case *ast.ReturnStmt:
		return rl.lowerReturn(s)
	case *ast.DeferStmt:
		return rl.lowerDefer(s)
	default:
		return fmt.Errorf("unsupported statement in recursive function: %T", stmt)
	}
}

func (rl *recLowerer) lowerExprStmt(s *ast.ExprStmt) error {
	call, ok := s.X.(*ast.CallExpr)
	if !ok {
		return fmt.Errorf("unsupported expression statement in recursive function")
	}
	return rl.lowerCallStmt(call)
}

func (rl *recLowerer) lowerCallStmt(call *ast.CallExpr) error {
	funcName, _ := rl.resolveCall(call)
	if funcName == "" {
		return fmt.Errorf("unsupported call in recursive function")
	}
	if handled, err := rl.lowerBuiltinCall(funcName, call.Args, rl.lowerExpr); handled {
		return err
	}
	info, ok := rl.result.Funcs[funcName]
	if !ok {
		return fmt.Errorf("unsupported function in recursive function: %s", funcName)
	}
	if info.IsRecursive {
		return fmt.Errorf("unsupported recursive call as statement: %s", funcName)
	}
	retCells, err := rl.inlineCallInRec(info, call.Args)
	if err != nil {
		return err
	}
	for _, c := range retCells {
		rl.freeCell(c)
	}
	return nil
}

// inlineCallInRec inlines a non-recursive function call within a recursive
// function phase. Similar to Lowerer.inlineCall but uses rl.lowerExpr for
// argument evaluation and rl.Lowerer.lowerStmts for the inlined body.
func (rl *recLowerer) inlineCallInRec(info *FuncInfo, args []ast.Expr) ([]Cell, error) {
	// All args are scalars: composite args would require a frame-resident
	// composite local as the source, and those are rejected upfront.
	results := make([]exprResult, len(args))
	for i, arg := range args {
		r, err := rl.lowerExpr(arg)
		if err != nil {
			return nil, err
		}
		results[i] = r
	}
	rl.pushScope()
	defer rl.popScope()
	sc := rl.currentScope()
	for j, name := range info.Params {
		intSize := 0
		if j < len(info.ParamTypes) {
			intSize = info.ParamTypes[j].IntSize
		}
		if intSize >= 2 {
			base := rl.allocCells(intSize)
			for k := range intSize {
				rl.emit(&IRCopy{Dst: base + k, Src: results[j].cell + k})
			}
			sc[name] = &intBinding{base: base, size: intSize}
			continue
		}
		cell := rl.allocCell()
		rl.emit(&IRCopy{Dst: cell, Src: results[j].cell})
		sc.defineByte(name, cell)
	}
	retCells := rl.allocReturnCells(info)
	err := rl.runInlinedFunc(info, retCells, func() error {
		return rl.Lowerer.lowerStmts(info.Body.List)
	})
	if err != nil {
		return nil, err
	}
	return retCells, nil
}

// runInlinedFunc swaps in the inline-call return context (returnDst,
// returnFlag, inFunc), invokes body, and restores. body typically lowers
// the inlined function's statements.
func (rl *recLowerer) runInlinedFunc(info *FuncInfo, retCells []Cell, body func() error) error {
	savedRetDst := rl.returnDst
	savedRetFlag := rl.returnFlag
	savedInFunc := rl.inFunc
	defer func() {
		if rl.returnFlag != 0 {
			rl.freeCell(rl.returnFlag)
		}
		rl.returnDst = savedRetDst
		rl.returnFlag = savedRetFlag
		rl.inFunc = savedInFunc
	}()
	rl.returnDst = retCells
	if hasReturn(info.Body) {
		rl.returnFlag = rl.allocCell()
		rl.emit(&IRZero{Dst: rl.returnFlag})
	} else {
		rl.returnFlag = 0
	}
	rl.inFunc = true
	return body()
}

func (rl *recLowerer) lowerDecl(s *ast.DeclStmt) error {
	gd, ok := s.Decl.(*ast.GenDecl)
	if !ok {
		return fmt.Errorf("unsupported declaration in recursive function")
	}
	if gd.Tok == token.CONST {
		return rl.lowerLocalConsts(gd)
	}
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range vs.Names {
			cell, err := rl.lookupVar(name.Name)
			if err != nil {
				return err
			}
			if i < len(vs.Values) {
				r, err := rl.lowerExpr(vs.Values[i])
				if err != nil {
					return err
				}
				rl.emitCopyOrMove(cell, r)
			} else {
				size := max(rl.rc.locals[name.Name].intSize, 1)
				for j := range size {
					rl.emit(&IRZero{Dst: cell + j})
				}
			}
		}
	}
	return nil
}

func (rl *recLowerer) lowerAssign(s *ast.AssignStmt) error {
	// Desugar assignment operations: x += y -> x = x + y
	if op, ok := assignOp[s.Tok]; ok && len(s.Lhs) == 1 && len(s.Rhs) == 1 {
		s = &ast.AssignStmt{
			Lhs: s.Lhs,
			Tok: token.ASSIGN,
			Rhs: []ast.Expr{&ast.BinaryExpr{X: s.Lhs[0], Op: op, Y: s.Rhs[0]}},
		}
	}

	// Multi-return: q, r := divmod(a, b)
	if len(s.Lhs) > 1 && len(s.Rhs) == 1 {
		if call, ok := s.Rhs[0].(*ast.CallExpr); ok {
			if fn, ok := call.Fun.(*ast.Ident); ok {
				if info, ok := rl.result.Funcs[fn.Name]; ok && info.Returns == len(s.Lhs) && !info.IsRecursive {
					retCells, err := rl.inlineCallInRec(info, call.Args)
					if err != nil {
						return err
					}
					for i, lhs := range s.Lhs {
						id, ok := lhs.(*ast.Ident)
						if !ok {
							return fmt.Errorf("unsupported multi-return target in recursive function")
						}
						cell, err := rl.lookupVar(id.Name)
						if err != nil {
							return err
						}
						rl.emit(&IRMove{Dst: cell, Src: retCells[i]})
					}
					return nil
				}
			}
		}
	}

	// For multiple assignments (e.g., a, b = b, a), evaluate all RHS first
	// into temporaries to ensure correct swap semantics.
	if len(s.Lhs) > 1 && len(s.Lhs) == len(s.Rhs) {
		rhsCells := make([]exprResult, len(s.Rhs))
		for i, rhs := range s.Rhs {
			r, err := rl.lowerExpr(rhs)
			if err != nil {
				return err
			}
			rhsCells[i] = rl.ensureTemp(r)
		}
		for i, lhs := range s.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok {
				return fmt.Errorf("unsupported multi-assignment target in recursive function")
			}
			cell, err := rl.lookupVar(id.Name)
			if err != nil {
				return err
			}
			rl.emitCopyOrMove(cell, rhsCells[i])
		}
		return nil
	}

	for i, lhs := range s.Lhs {
		rhs := s.Rhs[i]
		id, ok := lhs.(*ast.Ident)
		if !ok {
			return fmt.Errorf("unsupported assignment target in recursive function: %T", lhs)
		}
		if err := rl.lowerRecVarInit(id.Name, rhs); err != nil {
			return err
		}
	}
	return nil
}

// lowerRecVarInit handles name = rhs in recursive functions, including
// scalar assignments (composites are rejected upfront).
func (rl *recLowerer) lowerRecVarInit(name string, rhs ast.Expr) error {
	cell, err := rl.lookupVar(name)
	if err != nil {
		return err
	}
	r, err := rl.lowerExpr(rhs)
	if err != nil {
		return err
	}
	rl.emitCopyOrMove(cell, r)
	return nil
}

func (rl *recLowerer) lowerIncDec(s *ast.IncDecStmt) error {
	id, ok := s.X.(*ast.Ident)
	if !ok {
		return fmt.Errorf("unsupported inc/dec target in recursive function: %T", s.X)
	}
	cell, err := rl.lookupVar(id.Name)
	if err != nil {
		return err
	}
	if li := rl.rc.locals[id.Name]; li.intSize >= 2 {
		if s.Tok == token.INC {
			rl.emitIncInt(cell, li.intSize)
		} else {
			rl.emitDecInt(cell, li.intSize)
		}
		return nil
	}
	if s.Tok == token.INC {
		rl.emit(&IRAddI{Dst: cell, Value: 1})
	} else {
		rl.emit(&IRSubI{Dst: cell, Value: 1})
	}
	return nil
}

func (rl *recLowerer) lowerIf(s *ast.IfStmt) error {
	if s.Init != nil {
		if err := rl.lowerStmt(s.Init); err != nil {
			return err
		}
	}
	cond, err := rl.lowerExpr(s.Cond)
	if err != nil {
		return err
	}
	// Save loadedMap before branches. Variables loaded inside one branch
	// may not be loaded at runtime when that branch is not taken, so the
	// else-branch must not reuse cells loaded only in the then-branch.
	// At the end of each branch that does NOT end in return, call
	// storeAllLocals to push cached-cell modifications back to the frame --
	// otherwise an `if cond { x = ... }` updates only a cached cell and
	// the modification is lost when the cache entry is dropped on branch
	// exit. Returns already pop the frame, so no store-back is needed (or
	// safe -- the frame is gone).
	savedLoadedMap := maps.Clone(rl.loadedMap)

	thenBlock, err := rl.captureBlock(func() error {
		if err := rl.lowerStmts(s.Body.List); err != nil {
			return err
		}
		if !endsWithReturn(s.Body.List) {
			rl.storeAllLocals()
		}
		return nil
	})
	if err != nil {
		return err
	}

	var elseBlock *IRBlock
	if s.Else != nil {
		rl.loadedMap = maps.Clone(savedLoadedMap)
		elseBlock, err = rl.captureBlock(func() error {
			var stmts []ast.Stmt
			switch e := s.Else.(type) {
			case *ast.BlockStmt:
				if err := rl.lowerStmts(e.List); err != nil {
					return err
				}
				stmts = e.List
			case *ast.IfStmt:
				if err := rl.lowerIf(e); err != nil {
					return err
				}
				stmts = []ast.Stmt{e}
			}
			if !endsWithReturn(stmts) {
				rl.storeAllLocals()
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	rl.loadedMap = savedLoadedMap

	rl.emit(&IRIf{Cond: cond.cell, Then: thenBlock, Else: elseBlock})
	if cond.temp {
		rl.freeCell(cond.cell)
	}
	return nil
}

func (rl *recLowerer) lowerSwitch(s *ast.SwitchStmt) error {
	if s.Init != nil {
		if err := rl.lowerStmt(s.Init); err != nil {
			return err
		}
	}
	var tagName string
	if s.Tag != nil {
		tagName = "$switch"
		tagCell, err := rl.lookupVar(tagName)
		if err != nil {
			return err
		}
		tagR, err := rl.lowerExpr(s.Tag)
		if err != nil {
			return err
		}
		rl.emitCopyOrMove(tagCell, tagR)
	}
	ifStmt := rl.buildSwitchIf(s.Body.List, tagName)
	if ifStmt != nil {
		return rl.lowerIf(ifStmt)
	}
	return nil
}

func (rl *recLowerer) lowerLabeledStmt(s *ast.LabeledStmt) error {
	switch s.Stmt.(type) {
	case *ast.ForStmt, *ast.RangeStmt:
	default:
		return fmt.Errorf("label %s on non-loop statement is not supported in recursive functions", s.Label.Name)
	}
	saved := rl.pendingLabel
	rl.pendingLabel = s.Label.Name
	err := rl.lowerStmt(s.Stmt)
	rl.pendingLabel = saved
	return err
}

func (rl *recLowerer) lowerFor(s *ast.ForStmt) error {
	if s.Init != nil {
		if err := rl.lowerStmt(s.Init); err != nil {
			return err
		}
	}
	condCell := rl.allocCell()
	if s.Cond != nil {
		if err := rl.emitCondTo(condCell, s.Cond); err != nil {
			return err
		}
	} else {
		rl.emit(&IRConst{Dst: condCell, Value: 1})
	}
	outerSkip := rl.loopSkipFlag
	outerBreak := rl.loopBreakFlag
	rl.loopSkipFlag = rl.allocCell()
	rl.loopBreakFlag = rl.allocCell()
	label := rl.pendingLabel
	rl.pendingLabel = ""
	rl.loopFrames = append(rl.loopFrames, loopFrame{
		label: label, skipFlag: rl.loopSkipFlag, breakFlag: rl.loopBreakFlag,
	})
	body, err := rl.captureBlock(func() error {
		rl.emit(&IRZero{Dst: rl.loopSkipFlag})
		rl.emit(&IRZero{Dst: rl.loopBreakFlag})
		if err := rl.lowerLoopBody(s.Body.List); err != nil {
			return err
		}
		rl.emit(&IRZero{Dst: rl.loopSkipFlag})
		// If break: clear condCell to exit loop. If not: run post + recalc cond.
		breakGuard := rl.allocCell()
		rl.emit(&IRNot{Dst: breakGuard, Src: rl.loopBreakFlag})
		postCondBlock, err := rl.captureBlock(func() error {
			if s.Post != nil {
				if err := rl.lowerStmt(s.Post); err != nil {
					return err
				}
			}
			if s.Cond != nil {
				return rl.emitCondTo(condCell, s.Cond)
			}
			rl.emit(&IRConst{Dst: condCell, Value: 1})
			return nil
		})
		if err != nil {
			return err
		}
		rl.emit(&IRIf{
			Cond: breakGuard,
			Then: postCondBlock,
			Else: &IRBlock{Nodes: []IRNode{&IRZero{Dst: condCell}}},
		})
		rl.freeCell(breakGuard)
		return nil
	})
	if err != nil {
		return err
	}
	rl.emit(&IRLoop{Cond: condCell, Body: body})
	rl.freeCell(rl.loopSkipFlag)
	rl.freeCell(rl.loopBreakFlag)
	rl.loopSkipFlag = outerSkip
	rl.loopBreakFlag = outerBreak
	rl.loopFrames = rl.loopFrames[:len(rl.loopFrames)-1]
	rl.freeCell(condCell)
	return nil
}

func (rl *recLowerer) lowerRange(s *ast.RangeStmt) error {
	var cell Cell
	if s.Key != nil {
		id, ok := s.Key.(*ast.Ident)
		if !ok {
			return fmt.Errorf("unsupported range key: %T", s.Key)
		}
		var err error
		cell, err = rl.lookupVar(id.Name)
		if err != nil {
			return err
		}
	} else {
		cell = rl.allocCell()
		defer rl.freeCell(cell)
	}
	// Range over a scalar limit: for i := range n. Range over arrays
	// requires array locals, which are rejected upfront.
	if s.Value != nil {
		return fmt.Errorf("range with value in recursive function is not supported (no array locals)")
	}
	limit, err := rl.lowerExpr(s.X)
	if err != nil {
		return err
	}
	rl.emit(&IRZero{Dst: cell})
	condCell := rl.allocCell()
	limitCopy := rl.allocCell()
	rl.emit(&IRCopy{Dst: limitCopy, Src: limit.cell})
	rl.emit(&IRCmp{Op: CmpLt, Dst: condCell, Src1: cell, Src2: limitCopy})
	outerSkip := rl.loopSkipFlag
	outerBreak := rl.loopBreakFlag
	rl.loopSkipFlag = rl.allocCell()
	rl.loopBreakFlag = rl.allocCell()
	label := rl.pendingLabel
	rl.pendingLabel = ""
	rl.loopFrames = append(rl.loopFrames, loopFrame{
		label: label, skipFlag: rl.loopSkipFlag, breakFlag: rl.loopBreakFlag,
	})
	body, err := rl.captureBlock(func() error {
		rl.emit(&IRZero{Dst: rl.loopSkipFlag})
		rl.emit(&IRZero{Dst: rl.loopBreakFlag})
		if err := rl.lowerLoopBody(s.Body.List); err != nil {
			return err
		}
		rl.emit(&IRZero{Dst: rl.loopSkipFlag})
		breakGuard := rl.allocCell()
		rl.emit(&IRNot{Dst: breakGuard, Src: rl.loopBreakFlag})
		postBlock, err := rl.captureBlock(func() error {
			rl.emit(&IRAddI{Dst: cell, Value: 1})
			rl.emit(&IRCopy{Dst: limitCopy, Src: limit.cell})
			rl.emit(&IRCmp{Op: CmpLt, Dst: condCell, Src1: cell, Src2: limitCopy})
			return nil
		})
		if err != nil {
			return err
		}
		rl.emit(&IRIf{
			Cond: breakGuard,
			Then: postBlock,
			Else: &IRBlock{Nodes: []IRNode{&IRZero{Dst: condCell}}},
		})
		rl.freeCell(breakGuard)
		return nil
	})
	if err != nil {
		return err
	}
	rl.emit(&IRLoop{Cond: condCell, Body: body})
	rl.freeCell(rl.loopSkipFlag)
	rl.freeCell(rl.loopBreakFlag)
	rl.loopSkipFlag = outerSkip
	rl.loopBreakFlag = outerBreak
	rl.loopFrames = rl.loopFrames[:len(rl.loopFrames)-1]
	if limit.temp {
		rl.freeCell(limit.cell)
	}
	rl.freeCell(limitCopy)
	rl.freeCell(condCell)
	return nil
}

// lowerLoopBody lowers loop body statements with per-statement skip guards
// for break/continue support.
func (rl *recLowerer) lowerLoopBody(stmts []ast.Stmt) error {
	rl.preloadVars(stmts)
	guard := rl.allocCell()
	for _, stmt := range stmts {
		rl.storeAllLocals()
		rl.reloadAllLocals()
		rl.emit(&IRNot{Dst: guard, Src: rl.loopSkipFlag})
		stmtBlock, err := rl.captureBlock(func() error { return rl.lowerStmt(stmt) })
		if err != nil {
			return err
		}
		rl.emit(&IRIf{Cond: guard, Then: stmtBlock})
	}
	rl.storeAllLocals()
	rl.freeCell(guard)
	return nil
}

// preloadVars loads all referenced variables from frame into loadedMap.
func (rl *recLowerer) preloadVars(stmts []ast.Stmt) {
	for _, stmt := range stmts {
		ast.Inspect(stmt, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				if _, exists := rl.rc.locals[id.Name]; exists {
					_, _ = rl.lookupVar(id.Name)
				}
			}
			return true
		})
	}
}

func (rl *recLowerer) emitCondTo(dst Cell, cond ast.Expr) error {
	r, err := rl.lowerExpr(cond)
	if err != nil {
		return err
	}
	rl.emitCopyOrMove(dst, r)
	return nil
}

func (rl *recLowerer) lowerReturn(s *ast.ReturnStmt) error {
	if len(s.Results) >= 1 {
		if rl.rc.retSize > 1 {
			// Multi-cell uintN return: lower the expression, copy N cells
			// to retReg. Struct/array returns are rejected upfront.
			r, err := rl.lowerExpr(s.Results[0])
			if err != nil {
				return err
			}
			if r.intSize != rl.rc.retSize {
				return fmt.Errorf("intSize mismatch in return: got %d, want %d", r.intSize, rl.rc.retSize)
			}
			for j := range rl.rc.retSize {
				rl.emit(&IRMove{Dst: rl.rc.retReg + j, Src: r.cell + j})
			}
			if r.temp {
				rl.freeCellRange(r.cell, rl.rc.retSize)
			}
		} else {
			r, err := rl.lowerExpr(s.Results[0])
			if err != nil {
				return err
			}
			rl.emitCopyOrMove(rl.rc.retReg, r)
		}
	} else if len(rl.rc.returnNames) > 0 {
		// Bare return with named return values: store cached cells to
		// frame first (they may have been modified), then load from frame
		// (if-bodies may have modified the frame via IRStoreFrame).
		// Each named return occupies 1 cell for byte or intSize cells for
		// uint16/uint32; retReg is laid out the same way.
		for _, name := range rl.rc.returnNames {
			li := rl.rc.locals[name]
			if reg, ok := rl.loadedMap[li.slot]; ok {
				rl.storeFrame(li.slot, reg, max(li.intSize, 1))
			}
		}
		off := 0
		for _, name := range rl.rc.returnNames {
			li := rl.rc.locals[name]
			size := max(li.intSize, 1)
			for j := range size {
				cell := rl.allocCell()
				rl.loadFrame(cell, li.slot+j, 1)
				rl.emit(&IRMove{Dst: rl.rc.retReg + off + j, Src: cell})
				rl.freeCell(cell)
			}
			off += size
		}
	}
	// Emit deferred calls before popping the frame.
	rl.emitDeferred()
	// Clear noRetFlag so the call setup is skipped for this phase.
	rl.emit(&IRZero{Dst: rl.rc.noRetFlag})
	// Pop frame and decrement active depth counter.
	rl.emit(&IRFramePop{Slots: rl.rc.frameSize})
	rl.emit(&IRSubI{Dst: rl.rc.activeReg, Value: 1})
	return nil
}

func (rl *recLowerer) lowerDefer(s *ast.DeferStmt) error {
	fn, ok := s.Call.Fun.(*ast.Ident)
	if !ok {
		return fmt.Errorf("unsupported defer call in recursive function")
	}
	switch fn.Name {
	case "putchar", "print", "println":
	default:
		return fmt.Errorf("unsupported defer call in recursive function: %s", fn.Name)
	}

	// Capture non-string args into pre-allocated frame slots. (Slots are
	// outside rc.locals so storeAllLocals won't overwrite them.) String
	// literals are emitted directly at replay time, no capture needed.
	var slots []int
	for _, arg := range s.Call.Args {
		if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			continue
		}
		slot := rl.rc.deferCaptureSlots[rl.rc.deferCaptureIdx]
		rl.rc.deferCaptureIdx++
		r, err := rl.lowerExpr(arg)
		if err != nil {
			return err
		}
		rl.emit(&IRStoreFrame{Slot: slot, Src: r.cell, FrameSize: rl.rc.frameSize})
		slots = append(slots, slot)
	}

	// Replay block: bind each captured frame slot to a synthetic name in
	// rc.locals so rl.lowerExpr's frame path loads the value lazily, then
	// delegate to the regular builtin lowering. The names are removed from
	// rc.locals after the call so they don't bleed into surrounding code.
	block, err := rl.captureBlock(func() error {
		replayArgs := make([]ast.Expr, 0, len(s.Call.Args))
		var addedNames []string
		ci := 0
		for _, arg := range s.Call.Args {
			if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				replayArgs = append(replayArgs, arg)
				continue
			}
			name := fmt.Sprintf("$defer_arg_%d", ci+1)
			rl.rc.locals[name] = recLocalInfo{slot: slots[ci]}
			addedNames = append(addedNames, name)
			replayArgs = append(replayArgs, ast.NewIdent(name))
			ci++
		}
		_, err := rl.lowerBuiltinCall(fn.Name, replayArgs, rl.lowerExpr)
		for _, name := range addedNames {
			if cell, ok := rl.loadedMap[rl.rc.locals[name].slot]; ok {
				rl.freeCell(cell)
				delete(rl.loadedMap, rl.rc.locals[name].slot)
			}
			delete(rl.rc.locals, name)
		}
		return err
	})
	if err != nil {
		return err
	}
	rl.rc.deferredCalls = append(rl.rc.deferredCalls, block)
	return nil
}

func (rl *recLowerer) emitDeferred() {
	for i := len(rl.rc.deferredCalls) - 1; i >= 0; i-- {
		for _, node := range rl.rc.deferredCalls[i].Nodes {
			rl.emit(node)
		}
	}
}

// Expression lowering.

func (rl *recLowerer) lowerExpr(expr ast.Expr) (exprResult, error) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return rl.lowerLiteral(e)
	case *ast.Ident:
		// Recursive-frame locals are looked up via the frame, bypassing
		// the base lowerer's scope. The outer scope may contain stale
		// bindings for the same name (e.g., `x := f(...)` in main pre-
		// allocates an intBinding for x before the call lowers, leaking
		// into f's body), so we must not consult lookupBinding here.
		if li, ok := rl.rc.locals[e.Name]; ok {
			cell, err := rl.lookupVar(e.Name)
			if err != nil {
				return exprResult{}, err
			}
			if li.intSize >= 2 {
				return exprResult{cell: cell, exprShape: exprShape{size: li.intSize, intSize: li.intSize}}, nil
			}
			return exprResult{cell: cell}, nil
		}
		return rl.lowerIdent(e, rl.lookupVar)
	case *ast.ParenExpr:
		return rl.lowerExpr(e.X)
	case *ast.UnaryExpr:
		return rl.lowerUnary(e, rl.lowerExpr)
	case *ast.BinaryExpr:
		return rl.lowerBinary(e, rl.lowerExpr)
	case *ast.CallExpr:
		return rl.lowerCallExpr(e)
	default:
		return exprResult{}, fmt.Errorf("unsupported expression in recursive function: %T", expr)
	}
}

func (rl *recLowerer) lowerCallExpr(e *ast.CallExpr) (exprResult, error) {
	if r, ok, err := rl.lowerCallExprWith(e, rl.lowerExpr); ok {
		return r, err
	}
	// Inline non-recursive user functions.
	if fn, ok := e.Fun.(*ast.Ident); ok {
		info, ok := rl.result.Funcs[fn.Name]
		if ok && !info.IsRecursive && info.Returns > 0 {
			retCells, err := rl.inlineCallInRec(info, e.Args)
			if err != nil {
				return exprResult{}, err
			}
			ri := info.SingleReturn()
			if ri.IntSize >= 2 {
				// Multi-cell uintN return: cells must be contiguous so
				// downstream consumers can index base+k.
				return exprResult{cell: retCells[0], temp: true,
					exprShape: exprShape{size: ri.IntSize, intSize: ri.IntSize}}, nil
			}
			for i := 1; i < len(retCells); i++ {
				rl.freeCell(retCells[i])
			}
			return exprResult{cell: retCells[0], temp: true}, nil
		}
		if ok && info.Returns == 0 {
			return exprResult{}, fmt.Errorf("function %s has no return value", fn.Name)
		}
		if ok && info.IsRecursive {
			// Recursive (general or tail) functions cannot be called as
			// inline expressions inside a recursive function: the rec
			// lowerer has no way to set up a nested dispatch loop, and
			// tail-rec inlining needs the regular lowerer's tail-call
			// context which is not active here.
			return exprResult{}, fmt.Errorf(
				"unsupported recursive function in recursive function: %s", fn.Name)
		}
		return exprResult{}, fmt.Errorf("unsupported function in recursive function: %s", fn.Name)
	}
	return exprResult{}, fmt.Errorf("unsupported call in recursive function: %T", e.Fun)
}
