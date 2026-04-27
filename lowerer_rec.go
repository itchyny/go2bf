package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"strconv"
)

// === General recursion via stack-based CPU model ===
//
// Frame layout (slot indices):
//   0: phase (dispatch phase number)
//   1: retval (return value, also receives child's return value)
//   2..2+P-1: parameters
//   2+P..2+P+L-1: local variables
//
// The function body is split into "phases" at each recursive call site.
// A dispatch loop processes one phase per iteration, always operating on
// the topmost stack frame.

// recArrayInfo holds metadata for an array variable in a recursive function frame.
type recArrayInfo struct {
	size     int    // total cells
	count    int    // number of elements
	elemSize int    // cells per element
	elemType string // struct type name (empty for byte or nested array)
}

// recContext holds state for lowering a recursive function.
type recContext struct {
	funcName         string
	frameSize        int
	slotPhase        int // always 0
	slotRet          int // always 1
	paramBase        int // always 2
	localBase        int
	localMap         map[string]int // variable name -> slot index
	phases           []*IRBlock
	activeReg        Cell                    // register cell for dispatch loop control
	retReg           Cell                    // base register cell for passing return values
	retSize          int                     // number of return cells (1 for byte, N for struct/array)
	noRetFlag        Cell                    // phase temp: 1 if no return happened in this phase, 0 after return
	returnNames      []string                // named return value names (empty if unnamed)
	localStructTypes map[string]string       // variable name -> struct type name
	localArrayInfo   map[string]recArrayInfo // variable name -> array metadata

	deferCaptureSlots []int // pre-allocated frame slots for defer captures
	deferCaptureIdx   int   // index into deferCaptureSlots during lowering
	// Deferred calls: IR blocks to emit before each return's frame pop.
	deferredCalls []*IRBlock
}

func (l *Lowerer) lowerGeneralRecursion(info *FuncInfo, argExprs []ast.Expr) ([]Cell, error) {
	// Compute frame layout.
	localNames := collectLocals(info.Body, info.Params)
	rc := &recContext{
		funcName:         info.Name,
		slotPhase:        0,
		slotRet:          1,
		paramBase:        2,
		localBase:        2 + len(info.Params),
		localMap:         make(map[string]int),
		localStructTypes: make(map[string]string),
		localArrayInfo:   make(map[string]recArrayInfo),
	}
	paramSlot := rc.paramBase
	for i, name := range info.Params {
		rc.localMap[name] = paramSlot
		paramSize := 1
		if i < len(info.ParamTypes) {
			pt := info.ParamTypes[i]
			if pt.StructType != "" {
				paramSize = l.result.Structs[pt.StructType].Size
				rc.localStructTypes[pt.Name] = pt.StructType
			} else if pt.ArraySize > 0 {
				paramSize = pt.ArraySize
				rc.localArrayInfo[pt.Name] = recArrayInfo{
					size: pt.ArraySize, count: pt.ArrayCount,
					elemSize: pt.ArrayElemSize, elemType: pt.ArrayElemType,
				}
			}
		}
		paramSlot += paramSize
	}
	rc.localBase = paramSlot
	for i, name := range localNames {
		rc.localMap[name] = rc.localBase + i
	}
	rc.frameSize = rc.localBase + len(localNames)

	// Named return values are mapped to frame slots like locals.
	rc.returnNames = info.ReturnNames
	for _, name := range info.ReturnNames {
		if _, exists := rc.localMap[name]; !exists {
			rc.localMap[name] = rc.frameSize
			rc.frameSize++
		}
	}

	// Scan for array locals and reallocate multi-slot frame space.
	l.collectArrayLocals(rc, info.Body)

	// Slices are not supported in recursive functions.
	if hasSliceUsage(info.Body) {
		return nil, fmt.Errorf("slices in recursive functions are not supported")
	}

	// Allocate active and retval in the PHASE TEMP area (direct tape positions,
	// not stack slots). This avoids cache/storeToStack issues in the dispatch loop.
	// Reserve positions 25, 26 for these; phase code allocs start at 27.
	// Compute return size for struct/array returns.
	retSize := info.Returns
	if retSize == 0 {
		retSize = 0 // void function
	} else if info.ReturnType.StructType != "" {
		retSize = l.result.Structs[info.ReturnType.StructType].Size
	} else if info.ReturnType.ArraySize > 0 {
		retSize = info.ReturnType.ArraySize
	}
	rc.retSize = retSize

	rc.activeReg = phaseTempBase  // tape position 25
	rc.retReg = phaseTempBase + 1 // tape position 26 (base of return area)

	// Evaluate arguments. Composite (struct/array) arguments are resolved
	// to their base cells and stored cell-by-cell.
	type argCells struct {
		cells []Cell
		temps bool
	}
	args := make([]argCells, len(argExprs))
	for i, expr := range argExprs {
		if i < len(info.ParamTypes) {
			pt := info.ParamTypes[i]
			if pt.StructType != "" {
				base, size, err := l.resolveStructArg(expr)
				if err != nil {
					return nil, err
				}
				cells := make([]Cell, size)
				for j := range size {
					cells[j] = base + j
				}
				args[i] = argCells{cells, false}
				continue
			}
			if pt.ArraySize > 0 {
				if id, ok := expr.(*ast.Ident); ok {
					ai, ok := l.lookupArray(id.Name)
					if ok {
						cells := make([]Cell, ai.size)
						for j := range ai.size {
							cells[j] = ai.base + j
						}
						args[i] = argCells{cells, false}
						continue
					}
				}
				if comp, ok := expr.(*ast.CompositeLit); ok {
					size := l.arraySize(comp.Type)
					base := l.allocCells(size)
					arr := arrayInfo{base: base, size: size, count: size, elemSize: 1}
					if count, elemSize, elemType, _ := l.arrayElementInfo(comp.Type); count > 0 && elemSize > 1 {
						arr = arrayInfo{base: base, size: size, count: count, elemSize: elemSize, elemType: elemType}
					}
					for j := range size {
						l.emit(&IRZero{Dst: base + j})
					}
					if err := l.lowerCompositeLitInto(arr, comp); err != nil {
						return nil, err
					}
					cells := make([]Cell, size)
					for j := range size {
						cells[j] = base + j
					}
					args[i] = argCells{cells, false}
					continue
				}
			}
		}
		r, err := l.lowerExpr(expr)
		if err != nil {
			return nil, err
		}
		args[i] = argCells{[]Cell{r.cell}, r.temp}
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
	// adds temp variables for inlined recursive expressions).
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
		Active:    rc.activeReg,
		FrameSize: rc.frameSize,
		Phases:    rc.phases,
	})

	// After dispatch loop exits, retReg area holds the return value(s).
	retCells := make([]Cell, rc.retSize)
	for j := range rc.retSize {
		retCells[j] = l.allocCell()
		l.emit(&IRCopy{Dst: retCells[j], Src: rc.retReg + j})
	}

	// activeReg and retReg are phase temp positions, no need to free.

	return retCells, nil
}

// hasSliceUsage checks if a block contains any slice type reference.
func hasSliceUsage(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		if at, ok := n.(*ast.ArrayType); ok && at.Len == nil {
			found = true
			return false
		}
		return true
	})
	return found
}

// collectArrayLocals scans the function body for array variable declarations
// and reallocates their frame slots to hold multiple cells.
func (l *Lowerer) collectArrayLocals(rc *recContext, body *ast.BlockStmt) {
	seen := make(map[string]bool)
	register := func(name string, info recArrayInfo) {
		if seen[name] || info.size <= 1 {
			return
		}
		seen[name] = true
		rc.localMap[name] = rc.frameSize
		rc.frameSize += info.size
		rc.localArrayInfo[name] = info
	}
	ast.Inspect(body, func(n ast.Node) bool {
		// a := [N]byte{...} or a := f(...)
		if assign, ok := n.(*ast.AssignStmt); ok && assign.Tok == token.DEFINE && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
			if id, ok := assign.Lhs[0].(*ast.Ident); ok {
				if comp, ok := assign.Rhs[0].(*ast.CompositeLit); ok {
					count, elemSize, elemType, _ := l.arrayElementInfo(comp.Type)
					size := count * elemSize
					if size > 0 {
						register(id.Name, recArrayInfo{size, count, elemSize, elemType})
					} else if def := l.structDef(comp.Type); def != nil {
						// Struct composite literal: r := Rect{...}
						if !seen[id.Name] {
							seen[id.Name] = true
							rc.localMap[id.Name] = rc.frameSize
							rc.frameSize += def.Size
							rc.localStructTypes[id.Name] = def.Name
						}
					}
				}
				// b := a where a is an array or struct variable.
				if rhsID, ok := assign.Rhs[0].(*ast.Ident); ok {
					if srcInfo, ok := rc.localArrayInfo[rhsID.Name]; ok {
						register(id.Name, srcInfo)
					} else if srcType, ok := rc.localStructTypes[rhsID.Name]; ok {
						if def, ok := l.result.Structs[srcType]; ok {
							if !seen[id.Name] {
								seen[id.Name] = true
								rc.localMap[id.Name] = rc.frameSize
								rc.frameSize += def.Size
								rc.localStructTypes[id.Name] = srcType
							}
						}
					}
				}
				// a := f(...) where f returns an array.
				if call, ok := assign.Rhs[0].(*ast.CallExpr); ok {
					if fn, ok := call.Fun.(*ast.Ident); ok {
						if info, ok := l.result.Funcs[fn.Name]; ok && info.ReturnType.ArraySize > 0 {
							size := info.ReturnType.ArraySize
							register(id.Name, recArrayInfo{
								size: size, count: size, elemSize: 1,
							})
						}
					}
				}
			}
		}
		// var a [N]byte or var a [N]Point or var p StructType
		if decl, ok := n.(*ast.DeclStmt); ok {
			if gd, ok := decl.Decl.(*ast.GenDecl); ok {
				for _, spec := range gd.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						count, elemSize, elemType, _ := l.arrayElementInfo(vs.Type)
						size := count * elemSize
						if size > 0 {
							for _, name := range vs.Names {
								register(name.Name, recArrayInfo{size, count, elemSize, elemType})
							}
						} else if def := l.structDef(vs.Type); def != nil {
							for _, name := range vs.Names {
								if !seen[name.Name] {
									seen[name.Name] = true
									rc.localMap[name.Name] = rc.frameSize
									rc.frameSize += def.Size
									rc.localStructTypes[name.Name] = def.Name
								}
							}
						}
					}
				}
			}
		}
		return true
	})
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
		// Var declarations.
		if decl, ok := n.(*ast.DeclStmt); ok {
			if gd, ok := decl.Decl.(*ast.GenDecl); ok {
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
				if _, exists := rc.localMap["$switch"]; !exists {
					rc.localMap["$switch"] = rc.frameSize
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

	// For multi-cell returns, result variables need N contiguous frame slots.
	// Re-allocate them at the end of the frame, overriding any single-slot
	// allocation from collectLocals.
	if rc.retSize > 1 {
		retType := info.ReturnType.StructType
		allocated := make(map[string]bool)
		for _, cs := range callSites {
			if !allocated[cs.resultVar] {
				rc.localMap[cs.resultVar] = rc.frameSize
				rc.frameSize += rc.retSize
				allocated[cs.resultVar] = true
			}
			if retType != "" {
				rc.localStructTypes[cs.resultVar] = retType
			}
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
		prevSlot := rc.localMap[callSites[i-1].resultVar]
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
	prevSlot := rc.localMap[lastCallSite.resultVar]
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
	// Assignment: a := f(n-1)
	if assign, ok := stmt.(*ast.AssignStmt); ok && len(calls) == 1 {
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
			if _, exists := rc.localMap["$tailret"]; !exists {
				rc.localMap["$tailret"] = rc.frameSize
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
			if _, exists := rc.localMap["$void"]; !exists {
				rc.localMap["$void"] = rc.frameSize
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
	if _, exists := rc.localMap[condVar]; !exists {
		rc.localMap[condVar] = rc.frameSize
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
				if _, exists := rc.localMap[tmpName]; !exists {
					rc.localMap[tmpName] = rc.frameSize
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

// buildRecPhase builds an IR block for a phase that ends with a return (pop frame).
func (l *Lowerer) buildRecPhase(rc *recContext, stmts []ast.Stmt) (*IRBlock, error) {
	// Isolate: save parent state, redirect allocation to phase temp range.
	savedNodes := l.nodes
	savedNext := l.nextCell
	savedFree := l.freeCells
	l.nodes = nil
	// Phase code uses cells starting at phaseTempBase+2 (27).
	l.nextCell = phaseTempBase + 1 + rc.retSize
	l.freeCells = nil
	l.recFrameSize = rc.frameSize
	l.recAllocErr = nil

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
	result := &IRBlock{Nodes: l.nodes}
	l.recFrameSize = 0

	l.nodes = savedNodes
	l.nextCell = savedNext
	l.freeCells = savedFree
	return result, nil
}

// buildRecPhaseWithCall builds a phase that ends by pushing a child frame.
func (l *Lowerer) buildRecPhaseWithCall(rc *recContext, stmts []ast.Stmt, call recCallSite, nextPhase int) (*IRBlock, error) {
	// Isolate: save parent state, redirect allocation to phase temp range.
	savedNodes := l.nodes
	savedNext := l.nextCell
	savedFree := l.freeCells
	l.nodes = nil
	l.nextCell = phaseTempBase + 1 + rc.retSize // skip activeReg and retReg
	l.freeCells = nil
	l.recFrameSize = rc.frameSize
	l.recAllocErr = nil

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

	// Evaluate call arguments. Composite (struct/array) args are loaded
	// cell-by-cell from the frame.
	type argValue struct {
		cells []Cell
	}
	argVals := make([]argValue, len(call.argExprs))
	for i, expr := range call.argExprs {
		// Check for composite argument (struct or array variable).
		if id, ok := expr.(*ast.Ident); ok {
			if size := rl.compositeSize(id.Name); size > 0 {
				slot := rc.localMap[id.Name]
				cells := make([]Cell, size)
				for j := range size {
					cells[j] = l.allocCell()
					l.emit(&IRLoadFrame{Dst: cells[j], Slot: slot + j, FrameSize: rc.frameSize})
				}
				argVals[i] = argValue{cells}
				continue
			}
		}
		// Handle struct/array composite literal arguments.
		if comp, ok := expr.(*ast.CompositeLit); ok {
			if def := rl.structDef(comp.Type); def != nil {
				cells := make([]Cell, def.Size)
				for j := range def.Size {
					cells[j] = l.allocCell()
					l.emit(&IRZero{Dst: cells[j]})
				}
				for j, elt := range comp.Elts {
					off := j
					val := elt
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						off = def.Offsets[kv.Key.(*ast.Ident).Name]
						val = kv.Value
					} else {
						off = def.Offsets[def.Fields[j]]
					}
					r, err := rl.lowerExpr(val)
					if err != nil {
						return nil, err
					}
					l.emit(&IRCopy{Dst: cells[off], Src: r.cell})
					if r.temp {
						l.freeCell(r.cell)
					}
				}
				argVals[i] = argValue{cells}
				continue
			}
		}
		r, err := rl.lowerExpr(expr)
		if err != nil {
			return nil, err
		}
		argVals[i] = argValue{[]Cell{r.cell}}
	}

	// Store all modified locals back to the frame before pushing child.
	rl.storeAllLocals(rc)

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
		condSlot := rc.localMap[call.condVar]
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
		skipRL.storeAllLocals(rc)
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

	l.nodes = savedNodes
	l.nextCell = savedNext
	l.freeCells = savedFree
	l.recFrameSize = 0
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

// lookupVar overrides the default to load from the frame.
func (rl *recLowerer) lookupVar(name string) (Cell, error) {
	if name == "_" {
		return rl.allocCell(), nil
	}
	slot, ok := rl.rc.localMap[name]
	if !ok {
		return 0, fmt.Errorf("undefined variable in recursive function: %s", name)
	}
	// Check if already loaded.
	if reg, ok := rl.loadedMap[slot]; ok {
		return reg, nil
	}
	// Load from frame into a register.
	reg := rl.allocCell()
	rl.emit(&IRLoadFrame{Dst: reg, Slot: slot, FrameSize: rl.rc.frameSize})
	rl.loadedMap[slot] = reg
	return reg, nil
}

// reloadAllLocals re-reads all cached locals from the frame into their
// existing phase temp cells.
func (rl *recLowerer) reloadAllLocals(rc *recContext) {
	for slot, reg := range rl.loadedMap {
		rl.emit(&IRLoadFrame{Dst: reg, Slot: slot, FrameSize: rc.frameSize})
	}
}

// storeAllLocals writes all loaded (and potentially modified) locals back to the frame.
func (rl *recLowerer) storeAllLocals(rc *recContext) {
	for slot, reg := range rl.loadedMap {
		rl.emit(&IRStoreFrame{Slot: slot, Src: reg, FrameSize: rc.frameSize})
	}
}

// zeroFrameSlots zeroes n frame slots starting at baseSlot.
func (rl *recLowerer) zeroFrameSlots(baseSlot, n int) {
	zero := rl.allocCell()
	rl.emit(&IRConst{Dst: zero, Value: 0})
	for j := range n {
		rl.emit(&IRStoreFrame{Slot: baseSlot + j, Src: zero, FrameSize: rl.rc.frameSize})
	}
	rl.freeCell(zero)
}

// copyFrameSlots copies n frame slots from srcSlot to dstSlot
// using a temporary cell for each slot.
func (rl *recLowerer) copyFrameSlots(srcSlot, dstSlot, n int) {
	for j := range n {
		cell := rl.allocCell()
		rl.emit(&IRLoadFrame{Dst: cell, Slot: srcSlot + j, FrameSize: rl.rc.frameSize})
		rl.emit(&IRStoreFrame{Slot: dstSlot + j, Src: cell, FrameSize: rl.rc.frameSize})
		rl.freeCell(cell)
	}
}

// lowerStmts processes statements within a recursive phase.
// Each statement is guarded by noRetFlag so that statements after a return
// inside an if-without-else are skipped.
func (rl *recLowerer) lowerStmts(stmts []ast.Stmt) error {
	for i := 0; i < len(stmts); i++ {
		// Fuse adjacent div/mod assignments: q := x/y; r := x%y -> IRDivMod.
		if i+1 < len(stmts) {
			if i == 0 {
				if fused, err := rl.tryLowerDivModAssign(stmts[i], stmts[i+1]); err != nil {
					return err
				} else if fused {
					i++
					continue
				}
			} else {
				saved := rl.nodes
				rl.nodes = nil
				if fused, err := rl.tryLowerDivModAssign(stmts[i], stmts[i+1]); err != nil {
					return err
				} else if fused {
					body := &IRBlock{Nodes: rl.nodes}
					rl.nodes = saved
					rl.emit(&IRIf{Cond: rl.rc.noRetFlag, Then: body, Else: &IRBlock{}})
					i++
					continue
				}
				// Not fused: restore and fall through to normal handling.
				rl.nodes = saved
			}
		}
		if i == 0 {
			// First statement: noRetFlag is always 1, no guard needed.
			if err := rl.lowerStmt(stmts[i]); err != nil {
				return err
			}
			continue
		}
		// Wrap in IRIf with empty else to skip if a prior return happened.
		// The else block must be non-nil so emitIfElse is used (preserves noRetFlag).
		saved := rl.nodes
		rl.nodes = nil
		if err := rl.lowerStmt(stmts[i]); err != nil {
			return err
		}
		body := &IRBlock{Nodes: rl.nodes}
		rl.nodes = saved
		rl.emit(&IRIf{Cond: rl.rc.noRetFlag, Then: body, Else: &IRBlock{}})
	}
	return nil
}

func (rl *recLowerer) tryLowerDivModAssign(a, b ast.Stmt) (bool, error) {
	return rl.tryLowerDivModAssignWith(a, b, rl.lowerExpr,
		func(id *ast.Ident, _ token.Token) (Cell, error) {
			return rl.lookupVar(id.Name)
		},
	)
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
	funcName, receiver := rl.resolveRecCall(call)
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
	args := call.Args
	if receiver != nil {
		args = append([]ast.Expr{receiver}, args...)
	}
	retCells, err := rl.inlineCallInRec(info, args)
	if err != nil {
		return err
	}
	for _, c := range retCells {
		rl.freeCell(c)
	}
	return nil
}

// resolveRecCall returns the function name and optional receiver for a call
// in recursive context. Uses frame-based struct type lookup for method calls.
func (rl *recLowerer) resolveRecCall(call *ast.CallExpr) (string, ast.Expr) {
	if id, ok := call.Fun.(*ast.Ident); ok {
		return id.Name, nil
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if id, ok := sel.X.(*ast.Ident); ok {
			if structType, ok := rl.rc.localStructTypes[id.Name]; ok {
				return structType + "." + sel.Sel.Name, sel.X
			}
		}
	}
	return "", nil
}

// inlineCallInRec inlines a non-recursive function call within a recursive
// function phase. Similar to Lowerer.inlineCall but uses rl.lowerExpr for
// argument evaluation and rl.Lowerer.lowerStmts for the inlined body.
func (rl *recLowerer) inlineCallInRec(info *FuncInfo, args []ast.Expr) ([]Cell, error) {
	// Evaluate arguments, handling composite types specially.
	type argVal struct {
		cell Cell // scalar arg
		base Cell // composite arg base
		size int  // composite arg size (0 for scalar)
		def  *StructDef
	}
	vals := make([]argVal, len(args))
	for i, arg := range args {
		if i < len(info.ParamTypes) {
			pt := info.ParamTypes[i]
			if pt.StructType != "" {
				def := rl.result.Structs[pt.StructType]
				if id, ok := arg.(*ast.Ident); ok {
					if _, ok := rl.rc.localStructTypes[id.Name]; ok {
						baseSlot := rl.rc.localMap[id.Name]
						// Allocate consecutive cells, skipping highway markers.
						base := rl.nextCell
						rl.nextCell += def.Size
						for j := range def.Size {
							if (base+j) > 0 && (base+j)%highwayStride == 0 && (base+j) < sentinelFwd {
								base = rl.nextCell
								rl.nextCell += def.Size
								break
							}
						}
						for j := range def.Size {
							rl.emit(&IRLoadFrame{Dst: base + j, Slot: baseSlot + j, FrameSize: rl.rc.frameSize})
						}
						vals[i] = argVal{base: base, size: def.Size, def: def}
						continue
					}
				}
			}
			if pt.ArraySize > 0 {
				if id, ok := arg.(*ast.Ident); ok {
					if ai, ok := rl.rc.localArrayInfo[id.Name]; ok {
						baseSlot := rl.rc.localMap[id.Name]
						base := rl.allocCells(ai.size)
						for j := range ai.size {
							rl.emit(&IRLoadFrame{Dst: base + j, Slot: baseSlot + j, FrameSize: rl.rc.frameSize})
						}
						vals[i] = argVal{base: base, size: ai.size}
						continue
					}
				}
			}
		}
		r, err := rl.lowerExpr(arg)
		if err != nil {
			return nil, err
		}
		vals[i] = argVal{cell: r.cell}
	}
	rl.pushScope()
	sc := rl.currentScope()
	for j, name := range info.Params {
		if vals[j].size > 0 {
			if vals[j].def != nil {
				sc.structs[name] = structInfo{base: vals[j].base, def: vals[j].def}
			} else {
				rl.defineArray(sc, name, vals[j].size)
				paramAI, _ := rl.lookupArray(name)
				for k := range vals[j].size {
					rl.emit(&IRCopy{Dst: paramAI.base + k, Src: vals[j].base + k})
				}
				continue
			}
			sc.vars[name] = vals[j].base
		} else {
			cell := rl.allocCell()
			rl.emit(&IRCopy{Dst: cell, Src: vals[j].cell})
			sc.vars[name] = cell
		}
	}
	rl.scanAndAllocLocals(info.Body)
	retCells := make([]Cell, info.Returns)
	for j := range retCells {
		retCells[j] = rl.allocCell()
		rl.emit(&IRZero{Dst: retCells[j]})
	}
	savedRetDst := rl.returnDst
	savedRetFlag := rl.returnFlag
	savedInFunc := rl.inFunc
	rl.returnDst = retCells
	if hasReturn(info.Body) {
		rl.returnFlag = rl.allocCell()
		rl.emit(&IRZero{Dst: rl.returnFlag})
	} else {
		rl.returnFlag = 0
	}
	rl.inFunc = true
	err := rl.Lowerer.lowerStmts(info.Body.List)
	if rl.returnFlag != 0 {
		rl.freeCell(rl.returnFlag)
	}
	rl.returnDst = savedRetDst
	rl.returnFlag = savedRetFlag
	rl.inFunc = savedInFunc
	rl.popScope()
	if err != nil {
		return nil, err
	}
	return retCells, nil
}

func (rl *recLowerer) lowerDecl(s *ast.DeclStmt) error {
	gd, ok := s.Decl.(*ast.GenDecl)
	if !ok {
		return fmt.Errorf("unsupported declaration in recursive function")
	}
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range vs.Names {
			if size := rl.compositeSize(name.Name); size > 0 {
				rl.zeroFrameSlots(rl.rc.localMap[name.Name], size)
				continue
			}
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
				rl.emit(&IRZero{Dst: cell})
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
			funcName, receiver := rl.resolveRecCall(call)
			if info, ok := rl.result.Funcs[funcName]; ok && info.Returns == len(s.Lhs) && !info.IsRecursive {
				args := call.Args
				if receiver != nil {
					args = append([]ast.Expr{receiver}, args...)
				}
				retCells, err := rl.inlineCallInRec(info, args)
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
		switch target := lhs.(type) {
		case *ast.IndexExpr:
			return rl.lowerArrayAssign(target, rhs)
		case *ast.SelectorExpr:
			return rl.lowerFieldAssign(target, rhs)
		case *ast.Ident:
			if err := rl.lowerRecVarInit(target.Name, rhs); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported assignment target in recursive function")
		}
	}
	return nil
}

// lowerRecVarInit handles name = rhs in recursive functions, including
// composite literals, composite variable copies, and scalar assignments.
func (rl *recLowerer) lowerRecVarInit(name string, rhs ast.Expr) error {
	// Composite literal: a = [N]byte{...} or p = Point{...}
	if comp, ok := rhs.(*ast.CompositeLit); ok {
		if _, ok := rl.rc.localArrayInfo[name]; ok {
			return rl.lowerArrayCompositeLit(name, comp)
		}
		if structType, ok := rl.rc.localStructTypes[name]; ok {
			return rl.lowerStructCompositeLit(name, structType, comp)
		}
		if rl.arraySize(comp.Type) == 0 {
			if _, ok := comp.Type.(*ast.ArrayType); ok {
				return nil // [0]byte{} -- no-op
			}
		}
	}
	// Composite variable copy: b = a where a is array or struct.
	if rhsID, ok := rhs.(*ast.Ident); ok {
		if size := rl.compositeSize(rhsID.Name); size > 0 {
			rl.copyFrameSlots(rl.rc.localMap[rhsID.Name], rl.rc.localMap[name], size)
			return nil
		}
	}
	// Scalar assignment.
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

// compositeSize returns the total frame slot size for a composite variable,
// or 0 if the variable is a scalar.
func (rl *recLowerer) compositeSize(name string) int {
	if info, ok := rl.rc.localArrayInfo[name]; ok {
		return info.size
	}
	if structType, ok := rl.rc.localStructTypes[name]; ok {
		return rl.result.Structs[structType].Size
	}
	return 0
}

func (rl *recLowerer) lowerArrayAssign(idx *ast.IndexExpr, rhs ast.Expr) error {
	// Chained index: a[i][j] = val
	if innerIdx, ok := idx.X.(*ast.IndexExpr); ok {
		return rl.lowerChainedIndexAssign(innerIdx, idx.Index, rhs)
	}
	id, ok := idx.X.(*ast.Ident)
	if !ok {
		return fmt.Errorf("unsupported array access in recursive function")
	}
	baseSlot, info, err := rl.lookupArraySlot(id.Name)
	if err != nil {
		return err
	}
	r, err := rl.lowerExpr(rhs)
	if err != nil {
		return err
	}
	return rl.recWriteInto(baseSlot, info.count, idx.Index, r)
}

func (rl *recLowerer) lowerChainedIndexAssign(outerIdx *ast.IndexExpr, innerIndex, rhs ast.Expr) error {
	id, ok := outerIdx.X.(*ast.Ident)
	if !ok {
		return fmt.Errorf("unsupported chained array access in recursive function")
	}
	baseSlot, info, err := rl.lookupArraySlot(id.Name)
	if err != nil {
		return err
	}
	r, err := rl.lowerExpr(rhs)
	if err != nil {
		return err
	}
	// Constant outer: write into sub-array directly.
	if constI, ok := rl.constValue(outerIdx.Index); ok {
		return rl.recWriteInto(baseSlot+constI*info.elemSize, info.elemSize, innerIndex, r)
	}
	// Variable outer: if-cascade, then write inner within each.
	idxI, err := rl.lowerExpr(outerIdx.Index)
	if err != nil {
		return err
	}
	if constJ, ok := rl.constValue(innerIndex); ok {
		rl.emitIfCascade(info.count, idxI.cell, func(i int) {
			rl.emit(&IRStoreFrame{Slot: baseSlot + i*info.elemSize + constJ, Src: r.cell, FrameSize: rl.rc.frameSize})
		})
	} else {
		idxJ, err := rl.lowerExpr(innerIndex)
		if err != nil {
			return err
		}
		rl.emitIfCascade(info.count, idxI.cell, func(i int) {
			rl.emitFrameIndexWrite(baseSlot+i*info.elemSize, info.elemSize, idxJ.cell, r.cell)
		})
		if idxJ.temp {
			rl.freeCell(idxJ.cell)
		}
	}
	if idxI.temp {
		rl.freeCell(idxI.cell)
	}
	if r.temp {
		rl.freeCell(r.cell)
	}
	return nil
}

// lowerFieldAssign handles struct field assignment (p.x = val) in recursive functions.
// For a[i].x = val with variable index, uses an if-cascade to write the correct slot.
func (rl *recLowerer) lowerFieldAssign(sel *ast.SelectorExpr, rhs ast.Expr) error {
	// Handle a[i].x = val where a is an array of structs.
	if idx, ok := sel.X.(*ast.IndexExpr); ok {
		if id, ok := idx.X.(*ast.Ident); ok {
			baseSlot, info, err := rl.lookupArraySlot(id.Name)
			if err != nil {
				return err
			}
			if info.elemType == "" {
				return fmt.Errorf("array %s element is not a struct", id.Name)
			}
			def := rl.result.Structs[info.elemType]
			offset := def.Offsets[sel.Sel.Name]
			r, err := rl.lowerExpr(rhs)
			if err != nil {
				return err
			}
			r = rl.ensureTemp(r)
			return rl.recFieldWriteInto(baseSlot, info, offset, idx.Index, r)
		}
	}
	slot, err := rl.resolveFieldSlot(sel)
	if err != nil {
		return err
	}
	r, err := rl.lowerExpr(rhs)
	if err != nil {
		return err
	}
	r = rl.ensureTemp(r)
	return rl.recWriteInto(slot, 1, &ast.BasicLit{Kind: token.INT, Value: "0"}, r)
}

// resolveFieldSlot resolves a selector expression to a frame slot offset.
func (rl *recLowerer) resolveFieldSlot(sel *ast.SelectorExpr) (int, error) {
	switch x := sel.X.(type) {
	case *ast.Ident:
		structType, ok := rl.rc.localStructTypes[x.Name]
		if !ok {
			return 0, fmt.Errorf("variable %s is not a struct in recursive function", x.Name)
		}
		baseSlot := rl.rc.localMap[x.Name]
		def := rl.result.Structs[structType]
		offset, ok := def.Offsets[sel.Sel.Name]
		if !ok {
			return 0, fmt.Errorf("unknown field %s", sel.Sel.Name)
		}
		return baseSlot + offset, nil
	case *ast.SelectorExpr:
		innerSlot, err := rl.resolveFieldSlot(x)
		if err != nil {
			return 0, err
		}
		innerDef := rl.resolveRecFieldDef(x)
		if innerDef == nil {
			return 0, fmt.Errorf("field %s is not a struct", x.Sel.Name)
		}
		offset, ok := innerDef.Offsets[sel.Sel.Name]
		if !ok {
			return 0, fmt.Errorf("unknown field %s", sel.Sel.Name)
		}
		return innerSlot + offset, nil
	default:
		return 0, fmt.Errorf("unsupported selector target in recursive function")
	}
}

func (rl *recLowerer) lowerIncDec(s *ast.IncDecStmt) error {
	var cell Cell
	switch x := s.X.(type) {
	case *ast.Ident:
		c, err := rl.lookupVar(x.Name)
		if err != nil {
			return err
		}
		cell = c
	case *ast.IndexExpr:
		return rl.lowerArrayIncDec(x, s.Tok)
	case *ast.SelectorExpr:
		slot, err := rl.resolveFieldSlot(x)
		if err != nil {
			return err
		}
		r, err := rl.recIndexInto(slot, 1, &ast.BasicLit{Kind: token.INT, Value: "0"})
		if err != nil {
			return err
		}
		if s.Tok == token.INC {
			rl.emit(&IRAddI{Dst: r.cell, Value: 1})
		} else {
			rl.emit(&IRSubI{Dst: r.cell, Value: 1})
		}
		return rl.recWriteInto(slot, 1, &ast.BasicLit{Kind: token.INT, Value: "0"}, r)
	default:
		return fmt.Errorf("unsupported inc/dec target in recursive function")
	}
	if s.Tok == token.INC {
		rl.emit(&IRAddI{Dst: cell, Value: 1})
	} else {
		rl.emit(&IRSubI{Dst: cell, Value: 1})
	}
	return nil
}

func (rl *recLowerer) lowerArrayIncDec(idx *ast.IndexExpr, tok token.Token) error {
	id, ok := idx.X.(*ast.Ident)
	if !ok {
		return fmt.Errorf("unsupported array access in recursive function")
	}
	baseSlot, info, err := rl.lookupArraySlot(id.Name)
	if err != nil {
		return err
	}
	r, err := rl.recIndexInto(baseSlot, info.count, idx.Index)
	if err != nil {
		return err
	}
	if tok == token.INC {
		rl.emit(&IRAddI{Dst: r.cell, Value: 1})
	} else {
		rl.emit(&IRSubI{Dst: r.cell, Value: 1})
	}
	return rl.recWriteInto(baseSlot, info.count, idx.Index, r)
}

// lookupArraySlot returns the base frame slot and array info for an array variable.
func (rl *recLowerer) lookupArraySlot(name string) (int, recArrayInfo, error) {
	slot, ok := rl.rc.localMap[name]
	if !ok {
		return 0, recArrayInfo{}, fmt.Errorf("undefined variable in recursive function: %s", name)
	}
	info, ok := rl.rc.localArrayInfo[name]
	if !ok {
		return 0, recArrayInfo{}, fmt.Errorf("variable %s is not an array in recursive function", name)
	}
	return slot, info, nil
}

func (rl *recLowerer) lowerStructCompositeLit(name, structType string, comp *ast.CompositeLit) error {
	baseSlot := rl.rc.localMap[name]
	def := rl.result.Structs[structType]
	rl.zeroFrameSlots(baseSlot, def.Size)
	// Store field values.
	for j, elt := range comp.Elts {
		off := j
		val := elt
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			off = def.Offsets[kv.Key.(*ast.Ident).Name]
			val = kv.Value
		} else {
			off = def.Offsets[def.Fields[j]]
		}
		// Handle nested struct literal.
		if innerComp, ok := val.(*ast.CompositeLit); ok {
			if innerDef := rl.structDef(innerComp.Type); innerDef != nil {
				for k, field := range innerComp.Elts {
					innerOff := k
					fval := field
					if kv, ok := field.(*ast.KeyValueExpr); ok {
						innerOff = innerDef.Offsets[kv.Key.(*ast.Ident).Name]
						fval = kv.Value
					} else {
						innerOff = innerDef.Offsets[innerDef.Fields[k]]
					}
					r, err := rl.lowerExpr(fval)
					if err != nil {
						return err
					}
					rl.emit(&IRStoreFrame{Slot: baseSlot + off + innerOff, Src: r.cell, FrameSize: rl.rc.frameSize})
					if r.temp {
						rl.freeCell(r.cell)
					}
				}
				continue
			}
		}
		r, err := rl.lowerExpr(val)
		if err != nil {
			return err
		}
		rl.emit(&IRStoreFrame{Slot: baseSlot + off, Src: r.cell, FrameSize: rl.rc.frameSize})
		if r.temp {
			rl.freeCell(r.cell)
		}
	}
	return nil
}

func (rl *recLowerer) lowerArrayCompositeLit(name string, comp *ast.CompositeLit) error {
	baseSlot, info, err := rl.lookupArraySlot(name)
	if err != nil {
		return err
	}
	size := info.size
	rl.zeroFrameSlots(baseSlot, size)
	// Store element values.
	for j, elt := range comp.Elts {
		idx := j
		val := elt
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			idx, _ = rl.constValue(kv.Key)
			val = kv.Value
		}
		elemBase := baseSlot + idx*info.elemSize
		if info.elemSize == 1 {
			// Scalar element.
			r, err := rl.lowerExpr(val)
			if err != nil {
				return err
			}
			rl.emit(&IRStoreFrame{Slot: elemBase, Src: r.cell, FrameSize: rl.rc.frameSize})
			if r.temp {
				rl.freeCell(r.cell)
			}
		} else if innerComp, ok := val.(*ast.CompositeLit); ok {
			// Nested composite literal (struct or inner array).
			if def := rl.structDef(innerComp.Type); def != nil {
				// Struct element.
				for k, field := range innerComp.Elts {
					off := k
					fval := field
					if kv, ok := field.(*ast.KeyValueExpr); ok {
						off = def.Offsets[kv.Key.(*ast.Ident).Name]
						fval = kv.Value
					} else {
						off = def.Offsets[def.Fields[k]]
					}
					r, err := rl.lowerExpr(fval)
					if err != nil {
						return err
					}
					rl.emit(&IRStoreFrame{Slot: elemBase + off, Src: r.cell, FrameSize: rl.rc.frameSize})
					if r.temp {
						rl.freeCell(r.cell)
					}
				}
			} else {
				// Inner array.
				for k, inner := range innerComp.Elts {
					r, err := rl.lowerExpr(inner)
					if err != nil {
						return err
					}
					rl.emit(&IRStoreFrame{Slot: elemBase + k, Src: r.cell, FrameSize: rl.rc.frameSize})
					if r.temp {
						rl.freeCell(r.cell)
					}
				}
			}
		} else {
			return fmt.Errorf("unsupported composite element in recursive function")
		}
	}
	return nil
}

// emitIfCascade emits an if-cascade that checks idxCell against each
// index 0..size-1 and executes emitBody for the matching case.
func (rl *recLowerer) emitIfCascade(size int, idxCell Cell, emitBody func(j int)) {
	for j := range size {
		cmp := rl.allocCell()
		idxCopy := rl.allocCell()
		rl.emit(&IRCopy{Dst: idxCopy, Src: idxCell})
		rl.emit(&IRConst{Dst: cmp, Value: byte(j)}) // #nosec G115
		rl.emit(&IRCmp{Op: CmpEq, Dst: cmp, Src1: idxCopy, Src2: cmp})
		rl.freeCell(idxCopy)
		saved := rl.nodes
		rl.nodes = nil
		emitBody(j)
		thenBlock := &IRBlock{Nodes: rl.nodes}
		rl.nodes = saved
		rl.emit(&IRIf{Cond: cmp, Then: thenBlock})
		rl.freeCell(cmp)
	}
}

// emitFrameIndexRead emits an if-cascade to load a[idx] from frame slots.
// recIndexInto reads a scalar from a frame-based array at the given index.
func (rl *recLowerer) recIndexInto(baseSlot, count int, indexExpr ast.Expr) (exprResult, error) {
	if constIdx, ok := rl.constValue(indexExpr); ok {
		cell := rl.allocCell()
		rl.emit(&IRLoadFrame{Dst: cell, Slot: baseSlot + constIdx, FrameSize: rl.rc.frameSize})
		return exprResult{cell: cell, temp: true}, nil
	}
	idxR, err := rl.lowerExpr(indexExpr)
	if err != nil {
		return exprResult{}, err
	}
	result := rl.allocCell()
	rl.emit(&IRZero{Dst: result})
	rl.emitFrameIndexRead(baseSlot, count, idxR.cell, result)
	if idxR.temp {
		rl.freeCell(idxR.cell)
	}
	return exprResult{cell: result, temp: true}, nil
}

// recFieldIndexInto reads a struct field from a frame-based struct array.
// Computes baseSlot + i*elemSize + offset for constant i, or if-cascade for variable i.
func (rl *recLowerer) recFieldIndexInto(baseSlot int, info recArrayInfo, offset int, indexExpr ast.Expr) (exprResult, error) {
	if constIdx, ok := rl.constValue(indexExpr); ok {
		cell := rl.allocCell()
		rl.emit(&IRLoadFrame{Dst: cell, Slot: baseSlot + constIdx*info.elemSize + offset, FrameSize: rl.rc.frameSize})
		return exprResult{cell: cell, temp: true}, nil
	}
	idxR, err := rl.lowerExpr(indexExpr)
	if err != nil {
		return exprResult{}, err
	}
	result := rl.allocCell()
	rl.emit(&IRZero{Dst: result})
	rl.emitFrameCompositeRead(baseSlot, info, idxR.cell, offset, result)
	if idxR.temp {
		rl.freeCell(idxR.cell)
	}
	return exprResult{cell: result, temp: true}, nil
}

// recFieldWriteInto writes a scalar to a struct field in a frame-based struct array.
func (rl *recLowerer) recFieldWriteInto(baseSlot int, info recArrayInfo, offset int, indexExpr ast.Expr, val exprResult) error {
	if constIdx, ok := rl.constValue(indexExpr); ok {
		rl.emit(&IRStoreFrame{Slot: baseSlot + constIdx*info.elemSize + offset, Src: val.cell, FrameSize: rl.rc.frameSize})
	} else {
		idxR, err := rl.lowerExpr(indexExpr)
		if err != nil {
			return err
		}
		rl.emitFrameFieldWrite(baseSlot, info, offset, idxR.cell, val.cell)
		if idxR.temp {
			rl.freeCell(idxR.cell)
		}
	}
	if val.temp {
		rl.freeCell(val.cell)
	}
	return nil
}

// recWriteInto writes a scalar to a frame-based array at the given index.
func (rl *recLowerer) recWriteInto(baseSlot, count int, indexExpr ast.Expr, val exprResult) error {
	if constIdx, ok := rl.constValue(indexExpr); ok {
		rl.emit(&IRStoreFrame{Slot: baseSlot + constIdx, Src: val.cell, FrameSize: rl.rc.frameSize})
	} else {
		idxR, err := rl.lowerExpr(indexExpr)
		if err != nil {
			return err
		}
		rl.emitFrameIndexWrite(baseSlot, count, idxR.cell, val.cell)
		if idxR.temp {
			rl.freeCell(idxR.cell)
		}
	}
	if val.temp {
		rl.freeCell(val.cell)
	}
	return nil
}

func (rl *recLowerer) emitFrameIndexRead(baseSlot, size int, idxCell, result Cell) {
	rl.emitIfCascade(size, idxCell, func(j int) {
		rl.emit(&IRLoadFrame{Dst: result, Slot: baseSlot + j, FrameSize: rl.rc.frameSize})
	})
}

// emitFrameIndexWrite emits an if-cascade to store val to a[idx] in frame slots.
func (rl *recLowerer) emitFrameIndexWrite(baseSlot, size int, idxCell, val Cell) {
	rl.emitIfCascade(size, idxCell, func(j int) {
		rl.emit(&IRStoreFrame{Slot: baseSlot + j, Src: val, FrameSize: rl.rc.frameSize})
	})
}

// emitFrameFieldWrite stores val to a[idx].field using an if-cascade.
func (rl *recLowerer) emitFrameFieldWrite(baseSlot int, info recArrayInfo, fieldOffset int, idxCell, val Cell) {
	rl.emitIfCascade(info.count, idxCell, func(j int) {
		rl.emit(&IRStoreFrame{Slot: baseSlot + j*info.elemSize + fieldOffset, Src: val, FrameSize: rl.rc.frameSize})
	})
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
	savedLoadedMap := maps.Clone(rl.loadedMap)

	saved := rl.nodes
	rl.nodes = nil
	if err := rl.lowerStmts(s.Body.List); err != nil {
		return err
	}
	thenBlock := &IRBlock{Nodes: rl.nodes}

	var elseBlock *IRBlock
	if s.Else != nil {
		rl.loadedMap = maps.Clone(savedLoadedMap)
		rl.nodes = nil
		switch e := s.Else.(type) {
		case *ast.BlockStmt:
			if err := rl.lowerStmts(e.List); err != nil {
				return err
			}
		case *ast.IfStmt:
			if err := rl.lowerIf(e); err != nil {
				return err
			}
		}
		elseBlock = &IRBlock{Nodes: rl.nodes}
	}
	rl.nodes = saved
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
	saved := rl.nodes
	rl.nodes = nil
	rl.emit(&IRZero{Dst: rl.loopSkipFlag})
	rl.emit(&IRZero{Dst: rl.loopBreakFlag})
	if err := rl.lowerLoopBody(s.Body.List); err != nil {
		return err
	}
	rl.emit(&IRZero{Dst: rl.loopSkipFlag})
	// If break: clear condCell to exit loop. If not: run post + recalc cond.
	breakGuard := rl.allocCell()
	rl.emit(&IRNot{Dst: breakGuard, Src: rl.loopBreakFlag})
	guardedSaved := rl.nodes
	rl.nodes = nil
	if s.Post != nil {
		if err := rl.lowerStmt(s.Post); err != nil {
			return err
		}
	}
	if s.Cond != nil {
		if err := rl.emitCondTo(condCell, s.Cond); err != nil {
			return err
		}
	} else {
		rl.emit(&IRConst{Dst: condCell, Value: 1})
	}
	postCondBlock := &IRBlock{Nodes: rl.nodes}
	rl.nodes = guardedSaved
	rl.emit(&IRIf{
		Cond: breakGuard,
		Then: postCondBlock,
		Else: &IRBlock{Nodes: []IRNode{&IRZero{Dst: condCell}}},
	})
	rl.freeCell(breakGuard)
	body := &IRBlock{Nodes: rl.nodes}
	rl.nodes = saved
	rl.emit(&IRLoop{Cond: condCell, Body: body})
	rl.freeCell(rl.loopSkipFlag)
	rl.freeCell(rl.loopBreakFlag)
	rl.loopSkipFlag = outerSkip
	rl.loopBreakFlag = outerBreak
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
	// Check if ranging over an array: for i, v := range arr
	var valCell Cell
	var arrBaseSlot int
	var arrInfo recArrayInfo
	var hasVal bool
	if s.Value != nil {
		if valID, ok := s.Value.(*ast.Ident); ok {
			if arrID, ok := s.X.(*ast.Ident); ok {
				baseSlot, info, err := rl.lookupArraySlot(arrID.Name)
				if err == nil {
					arrBaseSlot = baseSlot
					arrInfo = info
					hasVal = true
					valCell, err = rl.lookupVar(valID.Name)
					if err != nil {
						return err
					}
				}
			}
		}
	}
	// Evaluate the range limit.
	var limit exprResult
	if hasVal {
		t := rl.allocCell()
		rl.emit(&IRConst{Dst: t, Value: byte(arrInfo.count)}) // #nosec G115
		limit = exprResult{cell: t, temp: true}
	} else {
		var err error
		limit, err = rl.lowerExpr(s.X)
		if err != nil {
			return err
		}
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
	saved := rl.nodes
	rl.nodes = nil
	rl.emit(&IRZero{Dst: rl.loopSkipFlag})
	rl.emit(&IRZero{Dst: rl.loopBreakFlag})
	// Load range value variable each iteration: v = array[key]
	if hasVal {
		rl.emit(&IRZero{Dst: valCell})
		rl.emitFrameIndexRead(arrBaseSlot, arrInfo.count, cell, valCell)
	}
	if err := rl.lowerLoopBody(s.Body.List); err != nil {
		return err
	}
	rl.emit(&IRZero{Dst: rl.loopSkipFlag})
	breakGuard := rl.allocCell()
	rl.emit(&IRNot{Dst: breakGuard, Src: rl.loopBreakFlag})
	guardedSaved := rl.nodes
	rl.nodes = nil
	rl.emit(&IRAddI{Dst: cell, Value: 1})
	rl.emit(&IRCopy{Dst: limitCopy, Src: limit.cell})
	rl.emit(&IRCmp{Op: CmpLt, Dst: condCell, Src1: cell, Src2: limitCopy})
	postBlock := &IRBlock{Nodes: rl.nodes}
	rl.nodes = guardedSaved
	rl.emit(&IRIf{
		Cond: breakGuard,
		Then: postBlock,
		Else: &IRBlock{Nodes: []IRNode{&IRZero{Dst: condCell}}},
	})
	rl.freeCell(breakGuard)
	body := &IRBlock{Nodes: rl.nodes}
	rl.nodes = saved
	rl.emit(&IRLoop{Cond: condCell, Body: body})
	rl.freeCell(rl.loopSkipFlag)
	rl.freeCell(rl.loopBreakFlag)
	rl.loopSkipFlag = outerSkip
	rl.loopBreakFlag = outerBreak
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
		rl.storeAllLocals(rl.rc)
		rl.reloadAllLocals(rl.rc)
		rl.emit(&IRNot{Dst: guard, Src: rl.loopSkipFlag})
		saved := rl.nodes
		rl.nodes = nil
		if err := rl.lowerStmt(stmt); err != nil {
			return err
		}
		stmtBlock := &IRBlock{Nodes: rl.nodes}
		rl.nodes = saved
		rl.emit(&IRIf{Cond: guard, Then: stmtBlock})
	}
	rl.storeAllLocals(rl.rc)
	rl.freeCell(guard)
	return nil
}

// preloadVars loads all referenced variables from frame into loadedMap.
func (rl *recLowerer) preloadVars(stmts []ast.Stmt) {
	for _, stmt := range stmts {
		ast.Inspect(stmt, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				if _, exists := rl.rc.localMap[id.Name]; exists {
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
			// Multi-cell return (struct/array).
			result := s.Results[0]
			if id, ok := result.(*ast.Ident); ok {
				// Variable: load each cell from frame to retReg area.
				slot, ok := rl.rc.localMap[id.Name]
				if !ok {
					return fmt.Errorf("undefined variable: %s", id.Name)
				}
				for j := range rl.rc.retSize {
					cell := rl.allocCell()
					rl.emit(&IRLoadFrame{Dst: cell, Slot: slot + j, FrameSize: rl.rc.frameSize})
					rl.emit(&IRMove{Dst: rl.rc.retReg + j, Src: cell})
					rl.freeCell(cell)
				}
			} else if comp, ok := result.(*ast.CompositeLit); ok {
				if def := rl.structDef(comp.Type); def != nil {
					// Struct literal: lower each field using recLowerer's lowerExpr.
					for j, elt := range comp.Elts {
						var off int
						var ve ast.Expr
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							off = def.Offsets[kv.Key.(*ast.Ident).Name]
							ve = kv.Value
						} else {
							off = def.Offsets[def.Fields[j]]
							ve = elt
						}
						r, err := rl.lowerExpr(ve)
						if err != nil {
							return err
						}
						rl.emitCopyOrMove(rl.rc.retReg+off, r)
					}
				} else if arrayTypeSize(comp.Type) > 0 {
					// Array literal: lower each element.
					for j, elt := range comp.Elts {
						idx := j
						val := elt
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							idx, _ = rl.constValue(kv.Key)
							val = kv.Value
						}
						r, err := rl.lowerExpr(val)
						if err != nil {
							return err
						}
						rl.emitCopyOrMove(rl.rc.retReg+idx, r)
					}
				} else {
					return fmt.Errorf("unsupported composite return in recursive function")
				}
			} else {
				return fmt.Errorf("unsupported multi-cell return in recursive function")
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
		for _, name := range rl.rc.returnNames {
			slot := rl.rc.localMap[name]
			if reg, ok := rl.loadedMap[slot]; ok {
				rl.emit(&IRStoreFrame{Slot: slot, Src: reg, FrameSize: rl.rc.frameSize})
			}
		}
		for i, name := range rl.rc.returnNames {
			slot := rl.rc.localMap[name]
			cell := rl.allocCell()
			rl.emit(&IRLoadFrame{Dst: cell, Slot: slot, FrameSize: rl.rc.frameSize})
			rl.emit(&IRMove{Dst: rl.rc.retReg + i, Src: cell})
			rl.freeCell(cell)
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
	// Capture non-string arguments into frame slots (not in localMap,
	// so storeAllLocals won't overwrite them).
	type capturedSlot struct {
		slot int
	}
	// Use pre-allocated frame slots for captures.
	var captures []capturedSlot
	for _, arg := range s.Call.Args {
		if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			continue
		}
		_ = arg
		slot := rl.rc.deferCaptureSlots[rl.rc.deferCaptureIdx]
		rl.rc.deferCaptureIdx++
		captures = append(captures, capturedSlot{slot: slot})
	}
	// Now evaluate args and store into the pre-allocated slots.
	ci := 0
	for _, arg := range s.Call.Args {
		if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			continue
		}
		r, err := rl.lowerExpr(arg)
		if err != nil {
			return err
		}
		rl.emit(&IRStoreFrame{Slot: captures[ci].slot, Src: r.cell, FrameSize: rl.rc.frameSize})
		ci++
	}

	// Build the deferred block using raw IR (no lookupVar).
	// At replay time, load captured values from frame and emit the call.
	fn, ok := s.Call.Fun.(*ast.Ident)
	if !ok {
		return fmt.Errorf("unsupported defer call in recursive function")
	}
	saved := rl.nodes
	rl.nodes = nil
	ci = 0
	switch fn.Name {
	case "putchar":
		cell := rl.allocCell()
		rl.emit(&IRLoadFrame{Dst: cell, Slot: captures[0].slot, FrameSize: rl.rc.frameSize})
		rl.emit(&IRPutc{Src: cell})
	case "print", "println":
		argIdx := 0
		for _, arg := range s.Call.Args {
			if argIdx > 0 && fn.Name == "println" {
				sp := rl.allocCell()
				rl.emit(&IRConst{Dst: sp, Value: ' '})
				rl.emit(&IRPutc{Src: sp})
				rl.freeCell(sp)
			}
			argIdx++
			if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				s, _ := strconv.Unquote(lit.Value)
				for _, b := range []byte(s) {
					cell := rl.allocCell()
					rl.emit(&IRConst{Dst: cell, Value: b})
					rl.emit(&IRPutc{Src: cell})
				}
			} else {
				cell := rl.allocCell()
				rl.emit(&IRLoadFrame{Dst: cell, Slot: captures[ci].slot, FrameSize: rl.rc.frameSize})
				rl.emitPrintByte(cell)
				ci++
			}
		}
		if fn.Name == "println" {
			cell := rl.allocCell()
			rl.emit(&IRConst{Dst: cell, Value: '\n'})
			rl.emit(&IRPutc{Src: cell})
		}
	default:
		rl.nodes = saved
		return fmt.Errorf("unsupported defer call in recursive function: %s", fn.Name)
	}
	block := &IRBlock{Nodes: rl.nodes}
	rl.nodes = saved
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

// lowerRecCompositeCompare handles == and != for frame-based arrays
// and structs in recursive functions. Loads elements from the frame
// and compares element-by-element.
func (rl *recLowerer) lowerRecCompositeCompare(e *ast.BinaryExpr) (exprResult, bool, error) {
	resolve := func(expr ast.Expr) (int, int, bool) {
		id, ok := expr.(*ast.Ident)
		if !ok {
			return 0, 0, false
		}
		if ai, ok := rl.rc.localArrayInfo[id.Name]; ok {
			return rl.rc.localMap[id.Name], ai.size, true
		}
		if st, ok := rl.rc.localStructTypes[id.Name]; ok {
			def := rl.result.Structs[st]
			return rl.rc.localMap[id.Name], def.Size, true
		}
		return 0, 0, false
	}
	lSlot, lSize, lOk := resolve(e.X)
	rSlot, rSize, rOk := resolve(e.Y)
	if !lOk || !rOk || lSize != rSize {
		return exprResult{}, false, nil
	}
	// Compare element-by-element: load each pair from frame.
	result := rl.allocCell()
	rl.emit(&IRConst{Dst: result, Value: 1})
	for i := range lSize {
		lCell := rl.allocCell()
		rCell := rl.allocCell()
		rl.emit(&IRLoadFrame{Dst: lCell, Slot: lSlot + i, FrameSize: rl.rc.frameSize})
		rl.emit(&IRLoadFrame{Dst: rCell, Slot: rSlot + i, FrameSize: rl.rc.frameSize})
		cond := rl.allocCell()
		rl.emit(&IRCopy{Dst: cond, Src: result})
		rl.emit(&IRIf{Cond: cond, Then: &IRBlock{Nodes: []IRNode{
			&IRCmp{Op: CmpEq, Dst: result, Src1: lCell, Src2: rCell},
		}}})
		rl.freeCell(cond)
		rl.freeCell(lCell)
		rl.freeCell(rCell)
	}
	if e.Op == token.NEQ {
		notResult := rl.allocCell()
		rl.emit(&IRNot{Dst: notResult, Src: result})
		rl.freeCell(result)
		return exprResult{cell: notResult, temp: true}, true, nil
	}
	return exprResult{cell: result, temp: true}, true, nil
}

// Expression lowering.

func (rl *recLowerer) lowerExpr(expr ast.Expr) (exprResult, error) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return rl.lowerLiteral(e)
	case *ast.Ident:
		return rl.lowerIdent(e, rl.lookupVar)
	case *ast.ParenExpr:
		return rl.lowerExpr(e.X)
	case *ast.UnaryExpr:
		return rl.lowerUnary(e, rl.lowerExpr)
	case *ast.BinaryExpr:
		if e.Op == token.EQL || e.Op == token.NEQ {
			if r, ok, err := rl.lowerRecCompositeCompare(e); ok {
				return r, err
			}
		}
		return rl.lowerBinary(e, rl.lowerExpr)
	case *ast.CallExpr:
		return rl.lowerCallExpr(e)
	case *ast.IndexExpr:
		return rl.lowerIndexExpr(e)
	case *ast.SelectorExpr:
		return rl.lowerSelectorExpr(e)
	default:
		return exprResult{}, fmt.Errorf("unsupported expression in recursive function: %T", expr)
	}
}

func (rl *recLowerer) lowerCallExpr(e *ast.CallExpr) (exprResult, error) {
	// Handle len() for frame-based arrays.
	if fn, ok := e.Fun.(*ast.Ident); ok && (fn.Name == "len" || fn.Name == "cap") && len(e.Args) == 1 {
		if id, ok := e.Args[0].(*ast.Ident); ok {
			if ai, ok := rl.rc.localArrayInfo[id.Name]; ok {
				t := rl.allocCell()
				rl.emit(&IRConst{Dst: t, Value: byte(ai.count)}) // #nosec G115
				return exprResult{cell: t, temp: true}, nil
			}
		}
	}
	if r, ok, err := rl.lowerCallExprWith(e, rl.lowerExpr); ok {
		return r, err
	}
	// Handle method calls: p.sum() -> Point.sum(p)
	if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
		if id, ok := sel.X.(*ast.Ident); ok {
			if structType, ok := rl.rc.localStructTypes[id.Name]; ok {
				funcName := structType + "." + sel.Sel.Name
				info, ok := rl.result.Funcs[funcName]
				if !ok {
					return exprResult{}, fmt.Errorf("undefined method: %s", funcName)
				}
				if info.Returns == 0 {
					return exprResult{}, fmt.Errorf("method %s has no return value", funcName)
				}
				args := append([]ast.Expr{sel.X}, e.Args...)
				retCells, err := rl.inlineCallInRec(info, args)
				if err != nil {
					return exprResult{}, err
				}
				for i := 1; i < len(retCells); i++ {
					rl.freeCell(retCells[i])
				}
				return exprResult{cell: retCells[0], temp: true}, nil
			}
		}
	}
	// Inline non-recursive user functions.
	if fn, ok := e.Fun.(*ast.Ident); ok {
		info, ok := rl.result.Funcs[fn.Name]
		if ok && !info.IsRecursive && info.Returns > 0 {
			retCells, err := rl.inlineCallInRec(info, e.Args)
			if err != nil {
				return exprResult{}, err
			}
			for i := 1; i < len(retCells); i++ {
				rl.freeCell(retCells[i])
			}
			return exprResult{cell: retCells[0], temp: true}, nil
		}
		if ok && info.Returns == 0 {
			return exprResult{}, fmt.Errorf("function %s has no return value", fn.Name)
		}
	}
	return exprResult{}, fmt.Errorf("unsupported call in recursive expression: %T", e.Fun)
}

func (rl *recLowerer) lowerIndexExpr(e *ast.IndexExpr) (exprResult, error) {
	// Chained index: a[i][j] -- outer index on composite element.
	if innerIdx, ok := e.X.(*ast.IndexExpr); ok {
		innerID, ok := innerIdx.X.(*ast.Ident)
		if !ok {
			return exprResult{}, fmt.Errorf("unsupported chained array access in recursive function")
		}
		baseSlot, info, err := rl.lookupArraySlot(innerID.Name)
		if err != nil {
			return exprResult{}, err
		}
		// Constant outer: index into sub-array directly.
		if constI, ok := rl.constValue(innerIdx.Index); ok {
			return rl.recIndexInto(baseSlot+constI*info.elemSize, info.elemSize, e.Index)
		}
		// Variable outer: if-cascade, then index inner within each.
		idxI, err := rl.lowerExpr(innerIdx.Index)
		if err != nil {
			return exprResult{}, err
		}
		result := rl.allocCell()
		rl.emit(&IRZero{Dst: result})
		if constJ, ok := rl.constValue(e.Index); ok {
			rl.emitFrameCompositeRead(baseSlot, info, idxI.cell, constJ, result)
		} else {
			idxJ, err := rl.lowerExpr(e.Index)
			if err != nil {
				return exprResult{}, err
			}
			rl.emitIfCascade(info.count, idxI.cell, func(i int) {
				rl.emitFrameIndexRead(baseSlot+i*info.elemSize, info.elemSize, idxJ.cell, result)
			})
			if idxJ.temp {
				rl.freeCell(idxJ.cell)
			}
		}
		if idxI.temp {
			rl.freeCell(idxI.cell)
		}
		return exprResult{cell: result, temp: true}, nil
	}
	id, ok := e.X.(*ast.Ident)
	if !ok {
		return exprResult{}, fmt.Errorf("unsupported array access in recursive function")
	}
	baseSlot, info, err := rl.lookupArraySlot(id.Name)
	if err != nil {
		return exprResult{}, err
	}
	// Constant index on composite element: return base slot for chained access.
	if info.elemSize > 1 {
		if constIdx, ok := rl.constValue(e.Index); ok {
			return exprResult{cell: baseSlot + constIdx*info.elemSize}, nil
		}
		return exprResult{}, fmt.Errorf("variable index on composite array not supported in recursive functions")
	}
	return rl.recIndexInto(baseSlot, info.count, e.Index)
}

// emitFrameCompositeRead emits an if-cascade to load a scalar from a composite
// array element with a variable outer index: a[n] at offset within the element.
// For each possible index i, checks if idxCell == i and loads from
// baseSlot + i*elemSize + offset.
func (rl *recLowerer) emitFrameCompositeRead(baseSlot int, info recArrayInfo, idxCell Cell, offset int, result Cell) {
	rl.emitIfCascade(info.count, idxCell, func(i int) {
		rl.emit(&IRLoadFrame{Dst: result, Slot: baseSlot + i*info.elemSize + offset, FrameSize: rl.rc.frameSize})
	})
}

// resolveRecFieldDef resolves the struct definition for a field in a selector,
// using the recursive function's localStructTypes.
func (rl *recLowerer) resolveRecFieldDef(e *ast.SelectorExpr) *StructDef {
	var parentDef *StructDef
	switch x := e.X.(type) {
	case *ast.Ident:
		structType, ok := rl.rc.localStructTypes[x.Name]
		if !ok {
			return nil
		}
		parentDef = rl.result.Structs[structType]
	case *ast.SelectorExpr:
		parentDef = rl.resolveRecFieldDef(x)
	}
	if parentDef == nil {
		return nil
	}
	fieldType := parentDef.FieldTypes[e.Sel.Name]
	if fieldType == "" {
		return nil
	}
	return rl.result.Structs[fieldType]
}

func (rl *recLowerer) lowerSelectorExpr(e *ast.SelectorExpr) (exprResult, error) {
	// Handle a[i].x -- array of structs field access.
	if idx, ok := e.X.(*ast.IndexExpr); ok {
		id, ok := idx.X.(*ast.Ident)
		if !ok {
			return exprResult{}, fmt.Errorf("unsupported array selector in recursive function")
		}
		baseSlot, info, err := rl.lookupArraySlot(id.Name)
		if err != nil {
			return exprResult{}, err
		}
		def := rl.result.Structs[info.elemType]
		offset := def.Offsets[e.Sel.Name]
		return rl.recFieldIndexInto(baseSlot, info, offset, idx.Index)
	}
	// Resolve the base: identifier or chained selector (nested struct).
	var baseSlot int
	var def *StructDef
	switch x := e.X.(type) {
	case *ast.Ident:
		structType, ok := rl.rc.localStructTypes[x.Name]
		if !ok {
			return exprResult{}, fmt.Errorf("variable %s is not a struct in recursive function", x.Name)
		}
		baseSlot = rl.rc.localMap[x.Name]
		def = rl.result.Structs[structType]
	case *ast.SelectorExpr:
		// Chained: r.min.x -> resolve r.min first to get base slot.
		inner, err := rl.lowerSelectorExpr(x)
		if err != nil {
			return exprResult{}, err
		}
		baseSlot = inner.cell
		// Find the struct type of the inner field by walking the type chain.
		innerDef := rl.resolveRecFieldDef(x)
		if innerDef == nil {
			return exprResult{}, fmt.Errorf("field %s is not a struct", x.Sel.Name)
		}
		def = innerDef
	default:
		return exprResult{}, fmt.Errorf("unsupported selector in recursive function")
	}
	if def == nil {
		return exprResult{}, fmt.Errorf("unsupported selector in recursive function")
	}
	offset, ok := def.Offsets[e.Sel.Name]
	if !ok {
		return exprResult{}, fmt.Errorf("unknown field %s in struct %s", e.Sel.Name, def.Name)
	}
	slot := baseSlot + offset
	// If the field is a nested struct, return the base slot for chained access.
	if _, isStruct := def.FieldTypes[e.Sel.Name]; isStruct {
		return exprResult{cell: slot}, nil
	}
	cell := rl.allocCell()
	rl.emit(&IRLoadFrame{Dst: cell, Slot: slot, FrameSize: rl.rc.frameSize})
	return exprResult{cell: cell, temp: true}, nil
}
