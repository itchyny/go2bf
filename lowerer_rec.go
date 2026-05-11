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

// recLocalInfo holds slot index plus per-local type metadata for a variable
// in a recursive function frame. A single map[name]recLocalInfo replaces
// what used to be parallel slot/type maps, mirroring the analyzer's
// StructDef.Field map[string]FieldInfo pattern.
type recLocalInfo struct {
	slot int
}

// recContext holds state for lowering a recursive function.
type recContext struct {
	funcName    string
	frameSize   int
	slotPhase   int // always 0
	slotRet     int // always 1
	paramBase   int // always 2
	localBase   int
	locals      map[string]recLocalInfo // name -> slot + type metadata (incl. synthetic temps)
	phases      []*IRBlock
	activeReg   Cell     // register cell for dispatch loop control
	retReg      Cell     // base register cell for passing return values
	retSize     int      // number of return cells (1 for byte, N for struct/array)
	noRetFlag   Cell     // phase temp: 1 if no return happened in this phase, 0 after return
	returnNames []string // named return value names (empty if unnamed)

	deferCaptureSlots []int // pre-allocated frame slots for defer captures
	deferCaptureIdx   int   // index into deferCaptureSlots during lowering
	// Deferred calls: IR blocks to emit before each return's frame pop.
	deferredCalls []*IRBlock
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
	for _, name := range info.Params {
		rc.locals[name] = recLocalInfo{slot: paramSlot}
		paramSlot += 1
	}
	rc.localBase = paramSlot
	for i, name := range localNames {
		rc.locals[name] = recLocalInfo{slot: rc.localBase + i}
	}
	rc.frameSize = rc.localBase + len(localNames)

	// Named return values are mapped to frame slots like locals.
	rc.returnNames = info.ReturnNames
	for _, name := range info.ReturnNames {
		if _, exists := rc.locals[name]; !exists {
			rc.locals[name] = recLocalInfo{slot: rc.frameSize}
			rc.frameSize++
		}
	}

	if err := l.rejectComposites(info.Body); err != nil {
		return nil, err
	}

	// Allocate active and retval in the PHASE TEMP area (direct tape positions,
	// not stack slots). This avoids cache/storeToStack issues in the dispatch loop.
	// Reserve positions 25, 26 for these; phase code allocs start at 27.
	rc.retSize = info.Returns

	rc.activeReg = phaseTempBase  // tape position 25
	rc.retReg = phaseTempBase + 1 // tape position 26 (base of return area)

	// Evaluate arguments.
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

// rejectComposites scans the function body for any struct, array, slice
// usage, or bitwise operator and returns an error if found. Recursive
// functions are scalar-only by design (params, returns, and locals).
// Bitwise ops are rejected because their codegen (decompose-into-bits
// with two `genMul`s per bit) peaks at ~11 algo temps; the dispatch
// loop holds 4 of the 16 in the pool, so the combination overflows
// "out of temporary cells" deep in codegen otherwise.
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
		case *ast.BinaryExpr:
			switch n.Op {
			case token.AND, token.OR, token.XOR, token.AND_NOT:
				err = fmt.Errorf("bitwise operator %s in recursive function is not supported", n.Op)
			}
		case *ast.AssignStmt:
			switch n.Tok {
			case token.AND_ASSIGN, token.OR_ASSIGN, token.XOR_ASSIGN, token.AND_NOT_ASSIGN:
				err = fmt.Errorf("bitwise assignment %s in recursive function is not supported", n.Tok)
			}
		}
		return true
	})
	return err
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

	if rc.retSize > 1 {
		allocated := make(map[string]bool)
		for _, cs := range callSites {
			info := rc.locals[cs.resultVar]
			if !allocated[cs.resultVar] {
				info.slot = rc.frameSize
				rc.frameSize += rc.retSize
				allocated[cs.resultVar] = true
			}
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

	// Evaluate call arguments. Pointer/struct/array params
	// are rejected at inlineCall.
	type argValue struct {
		cells []Cell
	}
	argVals := make([]argValue, len(call.argExprs))
	for i, expr := range call.argExprs {
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
	info, ok := rl.rc.locals[name]
	if !ok {
		return 0, fmt.Errorf("undefined variable in recursive function: %s", name)
	}
	slot := info.slot
	if reg, ok := rl.loadedMap[slot]; ok {
		return reg, nil
	}
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
	sc := rl.currentScope()
	for j, name := range info.Params {
		cell := rl.allocCell()
		rl.emit(&IRCopy{Dst: cell, Src: results[j].cell})
		sc.defineByte(name, cell)
	}
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
			funcName, _ := rl.resolveCall(call)
			if info, ok := rl.result.Funcs[funcName]; ok && info.Returns == len(s.Lhs) && !info.IsRecursive {
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

func (rl *recLowerer) lowerLabeledStmt(s *ast.LabeledStmt) error {
	switch s.Stmt.(type) {
	case *ast.ForStmt, *ast.RangeStmt:
	default:
		return fmt.Errorf("label %s on non-loop statement is not supported", s.Label.Name)
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
	saved := rl.nodes
	rl.nodes = nil
	rl.emit(&IRZero{Dst: rl.loopSkipFlag})
	rl.emit(&IRZero{Dst: rl.loopBreakFlag})
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
			return fmt.Errorf("unsupported multi-cell return in recursive function")
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
			slot := rl.rc.locals[name].slot
			if reg, ok := rl.loadedMap[slot]; ok {
				rl.emit(&IRStoreFrame{Slot: slot, Src: reg, FrameSize: rl.rc.frameSize})
			}
		}
		for i, name := range rl.rc.returnNames {
			slot := rl.rc.locals[name].slot
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
	// Capture non-string arguments into frame slots (not in rc.locals,
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
