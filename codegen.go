package main

import (
	"fmt"
	"slices"
	"strings"
)

// CPU execution model with highway markers for fast navigation.
// Tape layout, sentinel positions, and the bump-on-overflow scheme
// for phase temps are documented in docs/tape.md.

// Tape positions for registers. Interleaved with algo temps at 3, 6 so
// every register has at least one distance-1 neighbor.
var regPos = [5]int{1, 2, 4, 5, 7}

// Algo temp positions (skip markers and sentinels).
var algoTempPositions = []int{
	3, 6, // group 0 (interleaved with registers)
	9, 10, 11, 12, 13, 14, 15, // group 1
	17, 18, 19, 20, 21, 22, 23, // group 2
}

const (
	numRegs          = 5
	sentinelBack     = 0  // backward sentinel
	highwayStride    = 8  // [>>>>>>>>] stride
	phaseTempBase    = 25 // first phase temp
	maxSentinelBumps = 8  // max bumps to sentinelFwd before giving up
)

// sentinelFwd is the forward sentinel position (always 0 on the tape).
// Cells with IDs 0..sentinelFwd map directly to those tape positions
// and form the fixed CPU area (registers, algo temps, phase temps);
// the first stack slot has cell ID sentinelFwd+1 and lives further
// up the tape past two pad cells (see stackValuePos).
//
// It's a var rather than a const because the compile driver bumps it
// by highwayStride (8) when the default phase-temp pool overflows
// during recursive lowering -- the larger pool covers programs that
// would otherwise hit "too many local variables in recursive function".
var sentinelFwd = 24

// Generator converts IR to Brainfuck code using a register + stack model.
// 5 registers at positions 1,2,4,5,7 (interleaved with algo temps at 3,6 for
// neighbor optimization), highway markers at 8,16,24,32 for fast [>>>>>>>>] scanning.
type Generator struct {
	buf       strings.Builder
	pos       int
	temps     posAllocator
	frameSize int
	cache     regCache
	code      bool
	debug     bool
	stackUsed bool
}

// posAllocator manages a set of non-contiguous tape positions.
type posAllocator struct {
	positions []int
	inUse     []bool
}

func newPosAllocator(positions []int) posAllocator {
	return posAllocator{
		positions: positions,
		inUse:     make([]bool, len(positions)),
	}
}

func (pa *posAllocator) alloc() int {
	for i, u := range pa.inUse {
		if !u {
			pa.inUse[i] = true
			return pa.positions[i]
		}
	}
	panic("out of temporary cells")
}

// allocNear allocates a temp near the given operand positions.
// When any operand is a phase temp (>= phaseTempBase), picks the closest
// free position. Otherwise uses default sequential allocation.
func (pa *posAllocator) allocNear(positions ...int) int {
	target := -1
	for _, p := range positions {
		if p >= phaseTempBase {
			target = p
			break
		}
	}
	if target < 0 {
		return pa.alloc()
	}
	best := -1
	bestDist := 1 << 30
	for i, p := range pa.positions {
		if !pa.inUse[i] {
			d := abs(target - p)
			if d < bestDist {
				bestDist = d
				best = i
			}
		}
	}
	if best < 0 {
		panic("out of temporary cells")
	}
	pa.inUse[best] = true
	return pa.positions[best]
}

func (pa *posAllocator) free(pos int) {
	for i, p := range pa.positions {
		if p == pos {
			pa.inUse[i] = false
			return
		}
	}
}

func (pa *posAllocator) allocConsecutive(n int) int {
	var c, q int
	for i, p := range pa.positions {
		if pa.inUse[i] || i > 0 && p != q+1 {
			c = 0
		}
		if !pa.inUse[i] {
			if c++; c >= n {
				s := i - n + 1
				for j := s; j <= i; j++ {
					pa.inUse[j] = true
				}
				return pa.positions[s]
			}
		}
		q = p
	}
	panic("cannot allocate consecutive cells")
}

func (pa *posAllocator) freeConsecutive(base, n int) {
	for i, p := range pa.positions {
		if p == base {
			for j := range n {
				pa.inUse[i+j] = false
			}
			return
		}
	}
}

// regCache manages the registers.
type regCache struct {
	regs [numRegs]regEntry
	next int
	gen  *Generator
}

type regEntry struct {
	slot  int
	dirty bool
}

func (rc *regCache) ensure(slot int, avoid []int) int {
	for i := range rc.regs {
		if rc.regs[i].slot == slot {
			return regPos[i]
		}
	}
	idx := rc.allocIdx(avoid)
	rc.gen.loadFromStack(regPos[idx], slot)
	rc.regs[idx] = regEntry{slot: slot}
	return regPos[idx]
}

func (rc *regCache) assign(slot int, avoid []int) int {
	for i := range rc.regs {
		if rc.regs[i].slot == slot {
			rc.regs[i].dirty = true
			return regPos[i]
		}
	}
	idx := rc.allocIdx(avoid)
	rc.regs[idx] = regEntry{slot: slot, dirty: true}
	return regPos[idx]
}

func (rc *regCache) allocIdx(avoid []int) int {
	isAvoided := func(idx int) bool {
		return slices.Contains(avoid, regPos[idx])
	}
	for i := range rc.regs {
		if rc.regs[i].slot < 0 && !isAvoided(i) {
			return i
		}
	}
	for range rc.regs {
		r := rc.next
		rc.next = (rc.next + 1) % numRegs
		if !isAvoided(r) {
			rc.evict(r)
			return r
		}
	}
	panic("no available register")
}

// allocNeighbor returns the position of an empty register adjacent to pos,
// or -1 if none. The caller must call freeNeighbor when done.
func (rc *regCache) allocNeighbor(pos int) int {
	for i := range rc.regs {
		if rc.regs[i].slot < 0 && abs(regPos[i]-pos) == 1 {
			rc.regs[i].slot = -2 // mark reserved
			return regPos[i]
		}
	}
	return -1
}

func (rc *regCache) freeNeighbor(pos int) {
	for i := range rc.regs {
		if regPos[i] == pos && rc.regs[i].slot == -2 {
			rc.regs[i].slot = -1
			return
		}
	}
}

func (rc *regCache) evict(idx int) {
	if rc.regs[idx].slot >= 0 && rc.regs[idx].dirty {
		rc.gen.storeToStack(rc.regs[idx].slot, regPos[idx])
	}
	rc.regs[idx] = regEntry{slot: -1}
}

func (rc *regCache) markDirty(pos int) {
	for i, p := range regPos {
		if p == pos {
			rc.regs[i].dirty = true
			return
		}
	}
}

func (rc *regCache) flush() {
	for i := range rc.regs {
		if rc.regs[i].slot >= 0 && rc.regs[i].dirty {
			rc.gen.storeToStack(rc.regs[i].slot, regPos[i])
			rc.regs[i].dirty = false
		}
	}
}

func (rc *regCache) invalidate() {
	for i := range rc.regs {
		rc.regs[i] = regEntry{slot: -1}
	}
}

func (rc *regCache) flushAndInvalidate() {
	rc.flush()
	rc.invalidate()
}

// flushCell writes a specific cell back to stack if it's dirty in the cache.
func (rc *regCache) flushCell(cell int) {
	slot := slotOf(cell)
	for i := range rc.regs {
		if rc.regs[i].slot == slot && rc.regs[i].dirty {
			rc.gen.storeToStack(slot, regPos[i])
			rc.regs[i].dirty = false
			return
		}
	}
}

// dropCell removes a cell from the register cache without storing to stack.
// Used when a cell is freed (IRFree): the value is dead, so no writeback needed.
func (rc *regCache) dropCell(cell int) {
	if isReg(cell) {
		return
	}
	slot := slotOf(cell)
	for i := range rc.regs {
		if rc.regs[i].slot == slot {
			rc.regs[i] = regEntry{slot: -1}
			return
		}
	}
}

// Stack helpers.

func slotOf(cell int) int {
	return cell - (sentinelFwd + 1)
}

func isReg(cell int) bool {
	return cell <= sentinelFwd
}

func stackValuePos(slot int) int {
	return sentinelFwd + 4 + 3*slot
}

// Generate produces Brainfuck code. If debug is true, comments are emitted.
func Generate(prog *Program, debug bool) string {
	numSlots := prog.CellsUsed - (sentinelFwd + 1)
	g := &Generator{
		temps:     newPosAllocator(algoTempPositions),
		frameSize: numSlots,
		debug:     debug,
	}
	g.cache.gen = g
	for i := range g.cache.regs {
		g.cache.regs[i].slot = -1
	}

	g.genHighwayMarkers()
	if numSlots > 0 {
		g.genFramePush(numSlots)
	}
	initEnd := g.buf.Len()

	g.genNode(prog.Main)

	if !g.stackUsed {
		return strings.TrimSpace(g.buf.String()[initEnd:])
	}
	return g.buf.String()
}

// Brainfuck primitives.

func (g *Generator) emit(s string) {
	g.buf.WriteString(s)
	g.code = true
}

func (g *Generator) comment(format string, args ...any) {
	if g.debug {
		if g.code {
			g.code = false
			g.buf.WriteString("\n")
		}
		g.buf.WriteString("# ")
		fmt.Fprintf(&g.buf, format, args...)
		g.buf.WriteString("\n")
	}
}

func (g *Generator) moveTo(cell int) {
	diff := cell - g.pos
	if diff > 0 {
		// When moving forward from near position 0, check if going to the
		// forward sentinel via highway then backward is shorter.
		if g.pos < highwayStride {
			marker := highwayStride
			cost := (marker - g.pos) + len(scanFwd) + abs(sentinelFwd-cell)
			if cost < diff {
				g.moveToSentinel()
				g.moveTo(cell)
				return
			}
		}
		g.emit(strings.Repeat(">", diff))
	} else if diff < 0 {
		// When moving backward from the sentinel area, check if using the
		// highway backward to position 0 then forward is shorter.
		if g.pos >= sentinelFwd {
			marker := sentinelFwd - highwayStride
			cost := (g.pos - marker) + len(scanBack) + cell
			if cost < -diff {
				g.backToHome()
				g.moveTo(cell)
				return
			}
		}
		g.emit(strings.Repeat("<", -diff))
	}
	g.pos = cell
}

func (g *Generator) clear(cell int) {
	g.moveTo(cell)
	g.emit("[-]")
}

func (g *Generator) incr(cell int) {
	g.moveTo(cell)
	g.emit("+")
}

func (g *Generator) decr(cell int) {
	g.moveTo(cell)
	g.emit("-")
}

// set val to cell using multiplication when beneficial.
func (g *Generator) set(cell int, val byte) {
	g.clear(cell)
	g.emitAdd(cell, int(val))
}

// emitAdd adds v to cell using multiplication when beneficial.
func (g *Generator) emitAdd(cell, v int) {
	g.emitDelta(cell, v, "+")
}

// emitSub subtracts v from cell using multiplication when beneficial.
func (g *Generator) emitSub(cell, v int) {
	g.emitDelta(cell, v, "-")
}

// allocTemp allocates a temporary cell near the given position.
// Prefers an adjacent free register (distance 1), then the backward
// sentinel (position 0) for register 1, then falls back to algorithm temps.
// Only safe for local operations that don't navigate the highway
// (e.g., multiplication loops, copy). Not safe for storeToStack which
// uses [<<<<<<<<] scans that require position 0 to be 0.
func (g *Generator) allocTemp(pos int) (int, func()) {
	if t := g.cache.allocNeighbor(pos); t >= 0 {
		return t, func() { g.cache.freeNeighbor(t) }
	}
	if pos == 1 {
		return sentinelBack, func() {}
	}
	t := g.temps.alloc()
	return t, func() { g.temps.free(t) }
}

// mulFactors finds a, b, r such that a*b+r == n and a+b+r is minimized.
func mulFactors(n int) (int, int, int) {
	ma, mb, mr := 0, 0, n
	for a := 2; a*a <= n; a++ {
		b := n / a
		r := n - a*b
		if a+b+r < ma+mb+mr {
			ma, mb, mr = a, b, r
		}
	}
	return ma, mb, mr
}

// emitDelta applies op ("+" or "-") v times to cell.
// For v > 12, uses a multiplication loop: t[cell op*b; t-] with a*b+r = v.
// Falls back to direct repeat if the loop would be larger.
func (g *Generator) emitDelta(cell, v int, op string) {
	if v <= 0 {
		return
	}
	if v <= 12 {
		g.moveTo(cell)
		g.emit(strings.Repeat(op, v))
		return
	}
	ma, mb, mr := mulFactors(v)
	t, freeT := g.allocTemp(cell)
	dist := abs(cell - t)
	// BF for the multiplication loop:
	//   moveTo(t)  [-]  a++  [  moveTo(cell)  b++  moveTo(t)  -]  r++
	cost := dist + 3 + ma + 1 + dist + mb + dist + 2 + mr
	if mr > 0 {
		cost += dist // moveTo(cell) for remainder
	}
	if cost >= v {
		freeT()
		g.moveTo(cell)
		g.emit(strings.Repeat(op, v))
		return
	}
	g.clear(t)
	g.emit(strings.Repeat("+", ma))
	g.while(t, func() {
		g.moveTo(cell)
		g.emit(strings.Repeat(op, mb))
		g.decr(t)
	})
	freeT()
	if mr > 0 {
		g.moveTo(cell)
		g.emit(strings.Repeat(op, mr))
	}
}

// move src to dst: dst += src, src = 0.
func (g *Generator) move(dst, src int) {
	g.while(src, func() {
		g.incr(dst)
		g.decr(src)
	})
}

// subtract src from dst: dst -= src, src = 0.
func (g *Generator) subtract(dst, src int) {
	g.while(src, func() {
		g.decr(dst)
		g.decr(src)
	})
}

// copy src to dst non-destructively, using temp as a buffer.
// After: dst = src (preserved), temp = 0.
func (g *Generator) copy(dst, src, temp int) {
	g.clear(dst)
	g.clear(temp)
	g.while(src, func() {
		g.incr(dst)
		g.incr(temp)
		g.decr(src)
	})
	g.move(src, temp)
}

// Navigation.

var (
	scanFwd  = "[" + strings.Repeat(">", highwayStride) + "]"
	scanBack = "[" + strings.Repeat("<", highwayStride) + "]"
)

// moveToSentinel navigates to the forward sentinel (position 40).
// Moves to the nearest highway marker, then scans forward with [>>>>>>>>].
// If already past the last marker, moves directly.
func (g *Generator) moveToSentinel() {
	marker := max(((g.pos+highwayStride-1)/highwayStride)*highwayStride, highwayStride)
	g.moveTo(marker)
	if marker < sentinelFwd {
		g.emit(scanFwd)
	}
	g.pos = sentinelFwd
}

// backToHome navigates to the backward sentinel (position 0).
// Moves to the nearest highway marker, then scans backward with [<<<<<<<<].
// If already before the first marker, moves directly.
func (g *Generator) backToHome() {
	marker := min((g.pos/highwayStride)*highwayStride, sentinelFwd-highwayStride)
	g.moveTo(marker)
	if marker > sentinelBack {
		g.emit(scanBack)
	}
	g.pos = sentinelBack
}

// backToSentinel scans backward through the guard column to the sentinel.
// The pointer must be on a guard column cell before calling.
func (g *Generator) backToSentinel() {
	g.emit("[<<<]")
	g.pos = sentinelFwd
}

// homeFromBreadcrumb navigates from the breadcrumb guard (=0) to position 0.
// Skips past the breadcrumb guard with <<< to reach the previous guard column,
// then navigates home.
func (g *Generator) homeFromBreadcrumb() {
	g.emit("<<<")
	g.backToSentinel()
	g.backToHome()
}

// goToBreadcrumb navigates from position 0 to the first guard=0 cell.
// When a breadcrumb is set, this is the breadcrumb slot's guard.
// When no breadcrumb is set, this is the stack end (first unallocated slot).
func (g *Generator) goToBreadcrumb() {
	g.moveToSentinel()
	g.emit(">>>[>>>]")
}

// restoreBreadcrumb restores the guard and navigates home.
func (g *Generator) restoreBreadcrumb() {
	g.goToBreadcrumb()
	g.emit("+")
	g.backToSentinel()
	g.backToHome()
}

// Control flow.

// emitIfElse executes thenFn if cond != 0, elseFn otherwise. Preserves cond.
// Uses 2 temps: copies cond to t, sets u=1 as the else flag.
// Then-branch decrements u (exactly 1, so becomes 0);
// else-branch runs if u is still 1.
func (g *Generator) emitIfElse(cond int, thenFn, elseFn func()) {
	t := g.temps.alloc()
	u := g.temps.alloc()
	g.copy(t, cond, u)
	g.incr(u)
	g.emitIf(t, func() {
		thenFn()
		g.decr(u)
	})
	g.temps.free(t)
	g.while(u, func() {
		elseFn()
		g.decr(u)
	})
	g.temps.free(u)
}

// emitIf executes bodyFn if cond != 0, consuming cond (sets it to 0).
func (g *Generator) emitIf(cond int, bodyFn func()) {
	g.while(cond, func() {
		bodyFn()
		g.clear(cond)
	})
}

// while executes bodyFn repeatedly while cond != 0.
func (g *Generator) while(cond int, bodyFn func()) {
	g.moveTo(cond)
	g.emit("[")
	bodyFn()
	g.moveTo(cond)
	g.emit("]")
}

// Register cache helpers.

func (g *Generator) ensureReg(cell int, avoid []int) int {
	if isReg(cell) {
		return cell
	}
	return g.cache.ensure(slotOf(cell), avoid)
}

func (g *Generator) assignReg(cell int, avoid []int) int {
	if isReg(cell) {
		return cell
	}
	return g.cache.assign(slotOf(cell), avoid)
}

func (g *Generator) markDirty(cell, rp int) {
	if !isReg(cell) {
		g.cache.markDirty(rp)
	}
}

// evictConsumed removes a consumed source from the cache without writeback.
func (g *Generator) evictConsumed(cell int) {
	if isReg(cell) {
		return
	}
	for i := range g.cache.regs {
		if g.cache.regs[i].slot == slotOf(cell) {
			g.cache.regs[i] = regEntry{slot: -1}
			return
		}
	}
}

// IR node dispatch.

func (g *Generator) genNode(node IRNode) {
	switch n := node.(type) {
	case *IRZero:
		reg := g.assignReg(n.Dst, nil)
		g.comment("zero r%d", reg)
		g.clear(reg)
		g.markDirty(n.Dst, reg)
	case *IRConst:
		reg := g.assignReg(n.Dst, nil)
		g.comment("const r%d %d", reg, n.Value)
		g.set(reg, n.Value)
		g.markDirty(n.Dst, reg)
	case *IRMove:
		src := g.ensureReg(n.Src, nil)
		dst := g.assignReg(n.Dst, []int{src})
		if src != dst {
			g.comment("move r%d r%d", dst, src)
			g.clear(dst)
			g.move(dst, src)
		}
		g.markDirty(n.Dst, dst)
		if !isReg(n.Src) {
			for i := range g.cache.regs {
				if g.cache.regs[i].slot == slotOf(n.Src) {
					g.cache.regs[i] = regEntry{slot: -1}
					break
				}
			}
		}
	case *IRCopy:
		src := g.ensureReg(n.Src, nil)
		dst := g.assignReg(n.Dst, []int{src})
		if src != dst {
			g.comment("copy r%d r%d", dst, src)
			ct, freeT := g.allocTemp(src)
			g.copy(dst, src, ct)
			freeT()
		}
		g.markDirty(n.Dst, dst)
	case *IRAddI:
		reg := g.ensureReg(n.Dst, nil)
		g.comment("addi r%d %d", reg, n.Value)
		g.emitAdd(reg, int(n.Value))
		g.markDirty(n.Dst, reg)
	case *IRSubI:
		reg := g.ensureReg(n.Dst, nil)
		g.comment("subi r%d %d", reg, n.Value)
		g.emitSub(reg, int(n.Value))
		g.markDirty(n.Dst, reg)
	case *IRAdd:
		r1 := g.ensureReg(n.Src1, nil)
		r2 := g.ensureReg(n.Src2, []int{r1})
		rd := g.assignReg(n.Dst, []int{r1, r2})
		g.comment("add r%d r%d r%d", rd, r1, r2)
		g.genAdd(rd, r1, r2)
		g.markDirty(n.Dst, rd)
	case *IRSub:
		r1 := g.ensureReg(n.Src1, nil)
		r2 := g.ensureReg(n.Src2, []int{r1})
		rd := g.assignReg(n.Dst, []int{r1, r2})
		g.comment("sub r%d r%d r%d", rd, r1, r2)
		g.genSub(rd, r1, r2)
		g.markDirty(n.Dst, rd)
	case *IRMul:
		r1 := g.ensureReg(n.Src1, nil)
		r2 := g.ensureReg(n.Src2, []int{r1})
		rd := g.assignReg(n.Dst, []int{r1, r2})
		g.comment("mul r%d r%d r%d", rd, r1, r2)
		g.genMul(rd, r1, r2)
		g.markDirty(n.Dst, rd)
	case *IRDiv:
		r1 := g.ensureReg(n.Src1, nil)
		r2 := g.ensureReg(n.Src2, []int{r1})
		rd := g.assignReg(n.Dst, []int{r1, r2})
		g.comment("div r%d r%d r%d", rd, r1, r2)
		g.genDiv(rd, r1, r2)
		g.markDirty(n.Dst, rd)
	case *IRMod:
		r1 := g.ensureReg(n.Src1, nil)
		r2 := g.ensureReg(n.Src2, []int{r1})
		rd := g.assignReg(n.Dst, []int{r1, r2})
		g.comment("mod r%d r%d r%d", rd, r1, r2)
		g.genMod(rd, r1, r2)
		g.markDirty(n.Dst, rd)
	case *IRDivMod:
		r1 := g.ensureReg(n.Src1, nil)
		r2 := g.ensureReg(n.Src2, []int{r1})
		rq := g.assignReg(n.QuotDst, []int{r1, r2})
		rr := g.assignReg(n.RemDst, []int{r1, r2, rq})
		g.comment("divmod r%d r%d r%d r%d", rq, rr, r1, r2)
		g.genDivMod(rq, rr, r1, r2)
		g.markDirty(n.QuotDst, rq)
		g.markDirty(n.RemDst, rr)
	case *IRAnd:
		r1 := g.ensureReg(n.Src1, nil)
		r2 := g.ensureReg(n.Src2, []int{r1})
		rd := g.assignReg(n.Dst, []int{r1, r2})
		g.comment("and r%d r%d r%d", rd, r1, r2)
		g.genBitwise(rd, r1, r2, bitwiseAND)
		g.markDirty(n.Dst, rd)
	case *IROr:
		r1 := g.ensureReg(n.Src1, nil)
		r2 := g.ensureReg(n.Src2, []int{r1})
		rd := g.assignReg(n.Dst, []int{r1, r2})
		g.comment("or r%d r%d r%d", rd, r1, r2)
		g.genBitwise(rd, r1, r2, bitwiseOR)
		g.markDirty(n.Dst, rd)
	case *IRXor:
		r1 := g.ensureReg(n.Src1, nil)
		r2 := g.ensureReg(n.Src2, []int{r1})
		rd := g.assignReg(n.Dst, []int{r1, r2})
		g.comment("xor r%d r%d r%d", rd, r1, r2)
		g.genBitwise(rd, r1, r2, bitwiseXOR)
		g.markDirty(n.Dst, rd)
	case *IRCmp:
		r1 := g.ensureReg(n.Src1, nil)
		r2 := g.ensureReg(n.Src2, []int{r1})
		var consumedCell int
		switch n.Op {
		case CmpGt, CmpLeq:
			consumedCell = n.Src1
		default:
			consumedCell = n.Src2
		}
		if !isReg(consumedCell) {
			g.cache.flushCell(consumedCell)
		} else {
			src := r2
			if n.Op == CmpGt || n.Op == CmpLeq {
				src = r1
			}
			t := g.temps.allocNear(src)
			ct := g.temps.allocNear(src, t)
			switch n.Op {
			case CmpGt, CmpLeq:
				g.copy(t, r1, ct)
				r1 = t
			default:
				g.copy(t, r2, ct)
				r2 = t
			}
			g.temps.free(ct)
			defer g.temps.free(t)
		}
		rd := g.assignReg(n.Dst, []int{r1, r2})
		g.genCmp(n.Op, rd, r1, r2)
		g.markDirty(n.Dst, rd)
		g.evictConsumed(consumedCell)
	case *IRNot:
		rs := g.ensureReg(n.Src, nil)
		rd := g.assignReg(n.Dst, []int{rs})
		g.comment("not r%d r%d", rd, rs)
		g.genNot(rd, rs)
		g.markDirty(n.Dst, rd)
	case *IRIf:
		cond := g.ensureReg(n.Cond, nil)
		// Flush-only (preserve cache mappings) when the if-body has no
		// IRDynStore. After flush, all registers are clean and match their
		// stack slots. If the body doesn't execute, the cache is still valid.
		// If it does, flushAndInvalidate at the end clears it.
		//
		// When the body contains IRDynStore, we must invalidate. DynStore
		// writes to a runtime-determined stack slot via counter-walk. The
		// cache cannot track which slot was written. After the DynStore,
		// ensureReg may load a cell into a register that previously held
		// a different value for the same slot. On the next flush, this
		// stale value overwrites the DynStore'd value.
		if blockHasDynStore(n.Then) || blockHasDynStore(n.Else) {
			g.cache.flushAndInvalidate()
		} else {
			g.cache.flush()
		}
		g.comment("if r%d {", cond)
		if n.Else == nil {
			g.emitIf(cond, func() {
				g.genNode(n.Then)
				g.cache.flushAndInvalidate()
			})
		} else {
			g.emitIfElse(cond, func() {
				g.genNode(n.Then)
				g.cache.flushAndInvalidate()
			}, func() {
				g.comment("} else {")
				g.genNode(n.Else)
				g.cache.flushAndInvalidate()
			})
		}
		g.comment("}")
	case *IRLoop:
		cond := g.ensureReg(n.Cond, nil)
		condSlot := slotOf(n.Cond)
		g.cache.flushAndInvalidate()
		g.while(cond, func() {
			g.genNode(n.Body)
			g.cache.flushAndInvalidate()
			if !isReg(n.Cond) {
				g.loadFromStack(cond, condSlot)
			}
		})
	case *IRPutc:
		reg := g.ensureReg(n.Src, nil)
		g.comment("putc r%d", reg)
		g.moveTo(reg)
		g.emit(".")
	case *IRGetc:
		reg := g.assignReg(n.Dst, nil)
		g.comment("getc r%d", reg)
		g.moveTo(reg)
		g.emit(",")
		g.markDirty(n.Dst, reg)
	case *IRDynLoad:
		idx := g.ensureReg(n.Index, nil)
		g.cache.flushAndInvalidate()
		result := regPos[0]
		if result == idx {
			result = regPos[1]
		}
		g.genDynLoad(result, n.BaseSlot, idx)
		g.storeToStack(slotOf(n.Dst), result)
	case *IRDynStore:
		idx := g.ensureReg(n.Index, nil)
		src := g.ensureReg(n.Src, []int{idx})
		g.cache.flushAndInvalidate()
		g.genDynStore(n.BaseSlot, idx, src)
	case *IRBlock:
		for _, node := range n.Nodes {
			g.genNode(node)
		}
	case *IRFramePush:
		g.cache.flushAndInvalidate()
		g.genFramePush(n.Slots)
	case *IRFramePushDyn:
		src := g.ensureReg(n.Size, nil)
		g.cache.flushAndInvalidate()
		g.genFramePushDyn(src)
	case *IRFramePop:
		g.cache.flushAndInvalidate()
		g.genFramePop(n.Slots)
	case *IRLoadFrame:
		dst := g.ensureReg(n.Dst, nil)
		g.genLoadFrame(dst, n.Slot, n.FrameSize)
	case *IRStoreFrame:
		src := g.ensureReg(n.Src, nil)
		g.genStoreFrame(n.Slot, src, n.FrameSize)
	case *IRDispatch:
		g.cache.flushAndInvalidate()
		g.genDispatch(n)
	case *IRFree:
		g.cache.dropCell(n.Cell)
	}
}

// blockHasDynStore checks if a block contains any IRDynStore node.
func blockHasDynStore(b *IRBlock) bool {
	if b == nil {
		return false
	}
	for _, node := range b.Nodes {
		switch n := node.(type) {
		case *IRDynStore:
			return true
		case *IRIf:
			if blockHasDynStore(n.Then) || blockHasDynStore(n.Else) {
				return true
			}
		case *IRLoop:
			if blockHasDynStore(n.Body) {
				return true
			}
		case *IRBlock:
			if blockHasDynStore(n) {
				return true
			}
		}
	}
	return false
}

// Arithmetic codegen.

// genAdd sets dst = src1 + src2. Skips one copy when dst == src1 or src2.
func (g *Generator) genAdd(dst, src1, src2 int) {
	t1 := g.temps.allocNear(dst, src1, src2)
	t2 := g.temps.allocNear(dst, src1, src2)
	defer g.temps.free(t1)
	defer g.temps.free(t2)
	switch dst {
	case src1:
		g.copy(t2, src2, t1)
		g.move(dst, t2)
	case src2:
		g.copy(t2, src1, t1)
		g.move(dst, t2)
	default:
		g.copy(dst, src1, t1)
		g.copy(t2, src2, t1)
		g.move(dst, t2)
	}
}

// genSub sets dst = src1 - src2. Skips one copy when dst == src1.
func (g *Generator) genSub(dst, src1, src2 int) {
	t1 := g.temps.allocNear(dst, src1, src2)
	t2 := g.temps.allocNear(dst, src1, src2)
	defer g.temps.free(t1)
	defer g.temps.free(t2)
	if dst == src1 {
		g.copy(t2, src2, t1)
		g.subtract(dst, t2)
	} else {
		g.copy(dst, src1, t1)
		g.copy(t2, src2, t1)
		g.subtract(dst, t2)
	}
}

// genMul sets dst = src1 * src2 using repeated addition.
func (g *Generator) genMul(dst, src1, src2 int) {
	x := g.temps.allocNear(dst, src1, src2)
	y := g.temps.allocNear(dst, src1, src2)
	t := g.temps.allocNear(dst, src1, src2)
	defer g.temps.free(x)
	defer g.temps.free(y)
	defer g.temps.free(t)
	g.copy(x, src1, t)
	g.clear(dst)
	g.while(x, func() {
		g.decr(x)
		g.copy(y, src2, t)
		g.move(dst, y)
	})
}

func (g *Generator) genDiv(dst, src1, src2 int) {
	g.genDivMod(dst, -1, src1, src2)
}

func (g *Generator) genMod(dst, src1, src2 int) {
	g.genDivMod(-1, dst, src1, src2)
}

// divModCode is the Brainfuck divmod algorithm on 6 consecutive cells:
//
//	[n, d, 0, 0, 0, 0] -> [0, d-n%d, n%d, n/d, 0, 0]
//
// Outer loop: decrement n (cell 0) each iteration.
// Inner: decrement d copy (cell 1). If d reaches 0,
// reset it from cell 4 and increment quotient (cell 3).
// When n reaches 0, remainder is in cell 2, quotient in cell 3.
const divModCode = "[->-[>+>>]>[+[-<+>]>+>>]<<<<<]"

// genDivMod computes quotDst = src1/src2 and remDst = src1%src2.
// Uses 6 consecutive algo temp cells and the divModCode BF algorithm.
// Either quotDst or remDst can be -1 to skip that result.
func (g *Generator) genDivMod(quotDst, remDst, src1, src2 int) {
	base := g.temps.allocConsecutive(6)
	defer g.temps.freeConsecutive(base, 6)
	// Copy operands to cells 0 and 1; clear cells 2-5.
	t := g.temps.alloc()
	g.copy(base, src1, t)   // cell 0 = n
	g.copy(base+1, src2, t) // cell 1 = d
	g.temps.free(t)
	for i := 2; i < 6; i++ {
		g.clear(base + i)
	}
	g.moveTo(base)
	g.emit(divModCode)
	// Extract results: cell 2 = n%d, cell 3 = n/d.
	if remDst >= 0 {
		g.clear(remDst)
		g.move(remDst, base+2)
	}
	if quotDst >= 0 {
		g.clear(quotDst)
		g.move(quotDst, base+3)
	}
}

// genDivMod2 computes quotient = src/2, remainder = src%2.
// Cheaper than generic divmod: uses a toggle-based loop that drains src
// one at a time, alternating between incrementing a toggle and the quotient.
// Consumes src (sets to 0).
func (g *Generator) genDivMod2(quotient, remainder, src int) {
	prev := g.temps.alloc()
	defer g.temps.free(prev)
	g.clear(quotient)
	g.clear(remainder)
	g.while(src, func() {
		g.decr(src)
		g.move(prev, remainder) // prev = old remainder, remainder = 0
		g.incr(remainder)       // remainder = 1 (assume odd)
		g.emitIf(prev, func() { // if old remainder was 1: pair complete
			g.incr(quotient)
			g.decr(remainder) // undo: remainder = 0 (even)
		})
	})
}

type bitwiseOp int

const (
	bitwiseAND bitwiseOp = iota
	bitwiseOR
	bitwiseXOR
)

// genBitwise computes dst = src1 op src2 for bitwise AND, OR, XOR.
// Decomposes both operands into 8 bits via divmod-by-2, applies the
// operation per bit, and reassembles with powers of 2.
func (g *Generator) genBitwise(dst, src1, src2 int, op bitwiseOp) {
	a := g.temps.alloc()
	b := g.temps.alloc()
	bitA := g.temps.alloc()
	bitB := g.temps.alloc()
	prod := g.temps.alloc()
	weight := g.temps.alloc()
	ct := g.temps.alloc()
	defer func() {
		g.temps.free(a)
		g.temps.free(b)
		g.temps.free(bitA)
		g.temps.free(bitB)
		g.temps.free(prod)
		g.temps.free(weight)
		g.temps.free(ct)
	}()

	g.copy(a, src1, ct)
	g.copy(b, src2, ct)
	g.clear(dst)
	g.set(weight, 1)

	bit := g.temps.alloc()
	defer g.temps.free(bit)

	for range 8 {
		// Extract LSBs: bitA = a%2, a = a/2; bitB = b%2, b = b/2.
		tmp := g.temps.alloc()
		g.clear(tmp)
		g.move(tmp, a)
		g.genDivMod2(a, bitA, tmp)
		g.move(tmp, b)
		g.genDivMod2(b, bitB, tmp)
		g.temps.free(tmp)

		// Compute result bit using arithmetic on 0/1 values:
		//   AND: bitA * bitB
		//   OR:  bitA + bitB - bitA*bitB
		//   XOR: bitA + bitB - 2*bitA*bitB
		g.genMul(prod, bitA, bitB)
		switch op {
		case bitwiseAND:
			g.clear(bit)
			g.move(bit, prod)
		case bitwiseOR:
			g.genAdd(bit, bitA, bitB)
			g.genSub(bit, bit, prod)
		case bitwiseXOR:
			g.genAdd(bit, bitA, bitB)
			g.genSub(bit, bit, prod)
			g.genSub(bit, bit, prod)
		}

		// dst += weight * bit
		t2 := g.temps.alloc()
		g.genMul(t2, weight, bit)
		g.genAdd(dst, dst, t2)
		g.temps.free(t2)

		// weight *= 2
		g.genAdd(weight, weight, weight)
	}
}

// Comparison codegen.

// genEqual sets dst = (a == b) if eq is true, or (a != b) if false.
// Copies a, subtracts b, checks if result is zero. Consumes b.
func (g *Generator) genEqual(dst, a, b int, eq bool) {
	t := g.temps.allocNear(dst, a, b)
	ct := g.temps.allocNear(a, t)
	defer g.temps.free(t)
	defer g.temps.free(ct)
	g.copy(t, a, ct) // t = a (preserve a)
	g.subtract(t, b) // t = a - b (consumes b); t == 0 iff a == b
	if eq {
		g.set(dst, 1)                       // assume equal
		g.emitIf(t, func() { g.decr(dst) }) // if t != 0: not equal
	} else {
		g.clear(dst)                        // assume not equal
		g.emitIf(t, func() { g.incr(dst) }) // if t != 0: not equal
	}
}

// genOrder sets dst = (a >= b) if ge is true, or (a < b) if false.
// Copies a to x, then simultaneously decrements x and b. If x exhausts
// first, a < b. Consumes b.
func (g *Generator) genOrder(dst, a, b int, ge bool) {
	x := g.temps.allocNear(dst, a, b)
	t := g.temps.allocNear(dst, a, b)
	flag := g.temps.allocNear(dst, a, b)
	defer g.temps.free(x)
	defer g.temps.free(t)
	defer g.temps.free(flag)
	g.copy(x, a, t)
	if ge {
		g.set(dst, 1) // assume a >= b
	} else {
		g.clear(dst) // assume a >= b (will set to 1 if a < b)
	}
	g.while(b, func() {
		g.set(flag, 1)
		g.move(t, x)
		g.while(t, func() {
			g.decr(t)
			g.move(x, t)
			g.decr(b)
			g.clear(flag)
		})
		g.emitIf(flag, func() {
			if ge {
				g.decr(dst) // a < b: result = false
			} else {
				g.incr(dst) // a < b: result = true
			}
			g.clear(b)
		})
	})
}

var cmpOps = [...]string{"eq", "ne", "lt", "gt", "le", "ge"}

func (g *Generator) genCmp(op CmpOp, dst, src1, src2 int) {
	g.comment("cmp%s r%d r%d r%d", cmpOps[op], dst, src1, src2)
	switch op {
	case CmpEq:
		g.genEqual(dst, src1, src2, true)
	case CmpNeq:
		g.genEqual(dst, src1, src2, false)
	case CmpLt:
		g.genOrder(dst, src1, src2, false)
	case CmpGt:
		g.genOrder(dst, src2, src1, false)
	case CmpLeq:
		g.genOrder(dst, src2, src1, true)
	case CmpGeq:
		g.genOrder(dst, src1, src2, true)
	}
}

// genNot sets dst = !src (dst != src required).
// Result: dst = 1 if src was 0, dst = 0 otherwise.
func (g *Generator) genNot(dst, src int) {
	t := g.temps.allocNear(dst, src)
	ct := g.temps.allocNear(dst, src)
	g.copy(t, src, ct)
	g.temps.free(ct)
	g.set(dst, 1)
	g.while(t, func() {
		g.decr(dst)
		g.clear(t)
	})
	g.temps.free(t)
}

// Stack operations.

// loadFromStack copies a stack slot's value into register rp.
// Uses the adjacent zero cell as a temp to avoid a restore round-trip:
//  1. Copy loop: value[- >zero+ ... rp+ ... value] (each byte: value--, zero++)
//  2. Restore: >[<+>-] moves zero cell back to value (local, no navigation)
//
// For slot < 7: direct moveTo. For slot >= 7: breadcrumb technique.
func (g *Generator) loadFromStack(rp, slot int) {
	g.stackUsed = true
	g.comment("load r%d %%%d", rp, slot)
	g.clear(rp)
	if slot < 7 {
		// Direct path: navigate to value cell via moveTo.
		spos := stackValuePos(slot)
		g.moveToSentinel()
		g.moveTo(spos)
		// Copy loop: value--; zero++; back to guard; navigate home; rp++; return.
		g.emit("[->+<<")
		g.backToSentinel()
		g.backToHome()
		g.incr(rp)
		g.moveToSentinel()
		g.moveTo(spos)
		g.emit("]")
		// Restore value from zero cell (local move, no round-trip).
		g.emit(">[<+>-]<<")
	} else {
		// Breadcrumb path: set guard=0 to mark target, scan with [>>>].
		g.walkToSlotAndBreadcrumb(slot)
		// Copy loop: value--; zero++; back to guard; navigate home; rp++; return.
		g.emit(">[->+<<")
		g.homeFromBreadcrumb()
		g.incr(rp)
		g.goToBreadcrumb()
		g.emit(">]")
		// Restore value from zero, then restore guard.
		g.emit(">[<+>-]<<+")
	}
	g.backToSentinel()
	g.backToHome()
}

// storeToStack copies register rp's value into a stack slot.
//  1. Navigate to value cell and clear it.
//  2. Copy loop: rp[- t+ ... value+ ... rp] (each byte: rp--, t++, value++)
//  3. Restore rp from t via move.
func (g *Generator) storeToStack(slot, rp int) {
	g.stackUsed = true
	g.comment("store %%%d r%d", slot, rp)
	t := g.temps.alloc()
	defer g.temps.free(t)
	g.clear(t)
	if slot < 7 {
		// Direct path: navigate to value cell, clear it, return home.
		spos := stackValuePos(slot)
		g.moveToSentinel()
		g.moveTo(spos)
		g.emit("[-]<")
		g.backToSentinel()
		g.backToHome()
		// Copy loop: rp--; t++; navigate to value; value++; return.
		g.while(rp, func() {
			g.decr(rp)
			g.incr(t)
			g.moveToSentinel()
			g.moveTo(spos)
			g.emit("+<")
			g.backToSentinel()
			g.backToHome()
		})
	} else {
		// Breadcrumb path: set guard=0, navigate via [>>>] scanning.
		g.walkToSlotAndBreadcrumb(slot)
		g.emit(">[-]<") // move to value, clear, back to guard
		g.homeFromBreadcrumb()
		// Copy loop: rp--; t++; navigate to value via breadcrumb; value++; return.
		g.while(rp, func() {
			g.decr(rp)
			g.incr(t)
			g.goToBreadcrumb()
			g.emit(">+<")
			g.homeFromBreadcrumb()
		})
		g.restoreBreadcrumb()
	}
	// Restore rp from t
	g.move(rp, t)
}

// walkToSlotAndBreadcrumb navigates to a compile-time known slot using the
// counter-walk technique, then sets the breadcrumb (guard: 1->0).
// Pointer ends at the guard cell.
func (g *Generator) walkToSlotAndBreadcrumb(slot int) {
	g.moveToSentinel()
	guardPos := sentinelFwd + 3 + 3*slot
	if guardPos-sentinelFwd <= 24 {
		g.moveTo(guardPos)
		g.emit("-") // guard: 1->0 (breadcrumb)
	} else {
		padCell := sentinelFwd + 2
		if slot+1 > 12 {
			ma, mb, mr := mulFactors(slot + 1)
			g.moveTo(padCell - 1)
			g.emit(strings.Repeat("+", ma))
			g.emit("[>")
			g.emit(strings.Repeat("+", mb))
			g.emit("<-]>")
			g.emit(strings.Repeat("+", mr))
		} else {
			g.moveTo(padCell)
			g.emit(strings.Repeat("+", slot+1))
		}
		g.emit("[[>>>+<<<-]>>>-]<<-")
	}
}

// emitSetViaMul sets the current cell to n using multiplication when n > 12.
// Uses the right-adjacent cell as a temporary counter (must be 0, restored to 0).
// Uses the cell one position to the left as a temp (must be 0).
// Pointer starts and ends at the target cell.
func (g *Generator) emitSetViaMul(n int) {
	if n <= 12 {
		g.emit(strings.Repeat("+", n))
		return
	}
	ma, mb, mr := mulFactors(n)
	g.emit(">")
	g.emit(strings.Repeat("+", ma))
	g.emit("[<")
	g.emit(strings.Repeat("+", mb))
	g.emit(">-]<")
	g.emit(strings.Repeat("+", mr))
}

// genHighwayMarkers sets every highway marker cell (multiples of
// highwayStride below sentinelFwd) to 1. For up to 4 markers the
// unrolled `>>>>>>>>+` per marker is shortest; for 5 or more, a
// walking-counter loop with stride highwayStride wins (the same
// technique genFramePush uses, just at stride 8 instead of 3).
func (g *Generator) genHighwayMarkers() {
	g.comment("initialize markers")
	if n := sentinelFwd/highwayStride - 1; n < 5 {
		for m := highwayStride; m < sentinelFwd; m += highwayStride {
			g.incr(m)
		}
	} else {
		g.emitAdd(highwayStride-1, n)
		rights := strings.Repeat(">", highwayStride)
		lefts := strings.Repeat("<", highwayStride)
		g.emit("[>+<-[" + rights + "+" + lefts + "-]" + rights + "]")
		g.pos = sentinelFwd - 1
	}
}

// Frame operations (recursion).

// genFramePush allocates a new stack frame with the given number of slots.
// Each slot is 3 cells: [guard=1 | value | zero=0].
// Scans to the stack end (first guard=0 via >>>[>>>]), then allocates
// by setting guard=1 and advancing. For slots > 3, uses a counter loop
// to avoid unrolling.
func (g *Generator) genFramePush(slots int) {
	g.comment("push frame #%d", slots)
	g.goToBreadcrumb() // scan to stack end (no breadcrumb set)
	if slots <= 3 {
		// Unrolled: set guard=1, skip value and zero cells.
		for range slots {
			g.emit("+>>>")
		}
		g.emit("<<<") // back to last guard column
	} else {
		// Counter loop: move to last zero cell, set counter, then walk forward.
		// Each iteration: guard=1, shift counter forward through stride-3 layout.
		g.emit("<")
		g.emitSetViaMul(slots)
		g.emit("[>+<-[>>>+<<<-]>>>]<<")
	}
	g.backToSentinel()
	g.backToHome()
}

// genFramePushDyn pushes a runtime-determined number of stack slots.
// The count is in register src. Loops: each iteration scans to stack
// end, sets one guard, returns home, decrements counter.
func (g *Generator) genFramePushDyn(src int) {
	g.comment("push frame dyn r%d", src)
	g.while(src, func() {
		g.goToBreadcrumb() // scan to stack end (no breadcrumb set)
		g.emit("+")        // set guard=1
		g.backToSentinel()
		g.backToHome()
		g.decr(src)
	})
}

// genFramePop deallocates the topmost stack frame.
// Clears guard cells (1->0) so >>>[>>>] no longer stops at them.
// Value and zero cells are left as-is (genFramePush overwrites them).
func (g *Generator) genFramePop(slots int) {
	g.comment("pop frame #%d", slots)
	g.goToBreadcrumb() // scan to stack end (no breadcrumb set)
	// Counter loop: set counter at last zero cell, walk backward.
	// Each iteration: clear value, decrement guard, shift counter left.
	g.emit("<")
	g.emitSetViaMul(slots)
	g.emit("[<[-]<->>[<<<+>>>-]<<<-]<<")
	g.backToSentinel()
	g.backToHome()
}

// genLoadFrame loads a value from the topmost stack frame into a register cell.
// Used by the recursive dispatch loop. Navigates to the frame's slot then
// backs up to the target slot.
// Uses the same zero-cell trick as loadFromStack to avoid a restore round-trip.
func (g *Generator) genLoadFrame(dst, slot, frameSize int) {
	g.comment("load frame r%d #%d", dst, slot)
	g.clear(dst)
	// Scan to stack end, then back up to target slot's guard.
	g.goToBreadcrumb() // no breadcrumb set, scans to stack end
	g.emit(strings.Repeat("<", 3*(frameSize-slot)))
	// Set breadcrumb (guard: 1->0), move to value cell.
	g.emit("->")
	// Copy loop: value--; zero++; back to guard; navigate home; dst++; return.
	g.emit("[->+<<")
	g.homeFromBreadcrumb()
	g.incr(dst)
	g.goToBreadcrumb()
	g.emit(">]")
	// Restore value from zero cell, restore guard.
	g.emit(">[<+>-]<<+")
	g.backToSentinel()
	g.backToHome()
}

// genStoreFrame stores a register cell's value into the topmost stack frame.
// Navigates to the frame's slot, sets breadcrumb, clears old value.
// Copy loop: src--; tmp++; navigate to value via breadcrumb; value++; return.
// After loop, restores src from tmp and restores the breadcrumb guard.
func (g *Generator) genStoreFrame(slot, src, frameSize int) {
	g.comment("store frame #%d r%d", slot, src)
	tmp := g.temps.allocNear(src)
	defer g.temps.free(tmp)
	g.clear(tmp)
	// Scan to stack end, back up to target slot's guard.
	g.goToBreadcrumb() // no breadcrumb set, scans to stack end
	g.emit(strings.Repeat("<", 3*(frameSize-slot)))
	// Set breadcrumb (guard: 1->0), clear old value, return.
	g.emit("->[-]<")
	g.homeFromBreadcrumb()
	// Copy loop: src--; tmp++; navigate to value via breadcrumb; value++; return.
	g.while(src, func() {
		g.decr(src)
		g.incr(tmp)
		g.goToBreadcrumb()
		g.emit(">+<")
		g.homeFromBreadcrumb()
	})
	// Restore src from tmp.
	g.move(src, tmp)
	// Restore breadcrumb.
	g.moveToSentinel()
	g.restoreBreadcrumb()
}

// Dynamic array access via counter-walk through the zero column.
// Walk distance D from padding cell (sentinelFwd+2=34) through zero column:
//   padding(34) -> slot0_zero(37) -> slot1_zero(40) -> ...
// After D steps: pointer at slot(D-1)'s zero cell.
// For array base B and index I: walk distance = B + I + 1.

// genDynLoad loads a value from a dynamically indexed array slot.
// Computes walk distance = baseSlot + index + 1, walks to the target via
// counter-walk, then uses the breadcrumb + zero cell trick to copy
// the value to dst without a restore round-trip.
func (g *Generator) genDynLoad(dst, baseSlot, idx int) {
	g.comment("load r%d @%d(r%d)", dst, baseSlot, idx)
	g.clear(dst)
	g.walkToDynSlot(baseSlot, idx)
	// Set breadcrumb (guard: 1->0), move to value.
	g.emit("->")
	// Copy loop: value--; zero++; back to guard; navigate home; dst++; return.
	g.emit("[->+<<")
	g.homeFromBreadcrumb()
	g.incr(dst)
	g.goToBreadcrumb()
	g.emit(">]")
	// Restore value from zero cell, restore guard.
	g.emit(">[<+>-]<<+")
	g.backToSentinel()
	g.backToHome()
}

// genDynStore stores a value to a dynamically indexed array slot.
// Same walk as genDynLoad. Clears old value, then copies src to
// the slot byte-by-byte via breadcrumb round-trips.
func (g *Generator) genDynStore(baseSlot, idx, src int) {
	g.comment("store @%d(r%d) r%d", baseSlot, idx, src)
	tmp := g.temps.alloc()
	defer g.temps.free(tmp)
	g.clear(tmp)
	g.walkToDynSlot(baseSlot, idx)
	// Set breadcrumb (guard: 1->0), move to value, clear old value.
	g.emit("->[-]<")
	g.homeFromBreadcrumb()
	// Copy loop: src--; tmp++; navigate to value via breadcrumb; value++; return.
	g.while(src, func() {
		g.decr(src)
		g.incr(tmp)
		g.goToBreadcrumb()
		g.emit(">+<")
		g.homeFromBreadcrumb()
	})
	// Restore src from tmp, restore breadcrumb.
	g.move(src, tmp)
	g.restoreBreadcrumb()
}

// walkToDynSlot computes walk distance = baseSlot + index and
// navigates to the target slot via counter-walk.
// Pointer ends at the target guard.
func (g *Generator) walkToDynSlot(baseSlot, idx int) {
	dist := g.temps.alloc()
	tmp := g.temps.alloc()
	g.copy(dist, idx, tmp)
	g.temps.free(tmp)
	g.emitAdd(dist, baseSlot)
	padCell := sentinelFwd + 2
	g.move(padCell, dist)
	g.temps.free(dist)
	g.moveToSentinel()
	g.moveTo(padCell)
	g.emit("[[>>>+<<<-]>>>-]>")
}

// Dispatch loop for recursion.

// genDispatch implements the phase dispatch loop for general recursion.
// Each recursive function is split into phases (one per call site).
// The dispatch loop reads the phase number from the topmost stack frame
// and executes the matching phase block. Phases push/pop child frames
// and set the next phase number before returning to the loop.
//
//	Structure: while active > 0 {
//	  phase = loadFrame(slot 0)
//	  if phase == 0 { phase0 code }
//	  if phase == 1 { phase1 code }
//	  ...
//	  active = reload
//	}
func (g *Generator) genDispatch(n *IRDispatch) {
	g.comment("dispatch {")
	// Two layouts: when the rec lowerer detected bitwise ops it
	// reserved four phase-temp cells (Phase/Pr/Flag/ActiveTemp) so
	// the algo-temp pool stays fully available to genBitwise.
	// Otherwise (the common case) we allocate the four from the
	// algo-temp pool here -- cheaper navigation, smaller BF.
	var phase, pr, flag, activeTemp int
	if n.Phase != 0 {
		phase = int(n.Phase)
		pr = int(n.Pr)
		flag = int(n.Flag)
		activeTemp = int(n.ActiveTemp)
	} else {
		phase = g.temps.alloc()
		pr = g.temps.alloc()
		flag = g.temps.alloc()
		activeTemp = g.temps.alloc()
		defer g.temps.free(phase)
		defer g.temps.free(pr)
		defer g.temps.free(flag)
		defer g.temps.free(activeTemp)
	}

	// Copy active counter to a temp (active is a phase-temp cell).
	ct := g.temps.alloc()
	g.copy(activeTemp, n.Active, ct)
	g.temps.free(ct)

	// Outer loop: while active > 0.
	g.while(activeTemp, func() {
		// Load the phase number from the topmost frame.
		g.genLoadFrame(phase, 0, n.FrameSize)
		ct := g.temps.alloc()
		g.copy(pr, phase, ct)
		g.temps.free(ct)

		// Phase matching: for each phase i, check if pr == 0 (meaning phase == i).
		// After each check, decrement pr to test the next phase.
		for i, phase := range n.Phases {
			g.comment("phase %d/%d {", i, len(n.Phases))
			// flag = (pr == 0), i.e., current phase matches.
			g.genNot(flag, pr)
			g.emitIf(flag, func() {
				g.genNode(phase)
				// If not the last phase, set pr to skip remaining checks.
				if i < len(n.Phases)-1 {
					g.set(pr, byte(len(n.Phases))) // #nosec G115 -- phases < 256
				}
			})

			// Decrement pr: if pr > 0, copy to temp, clear, decrement, copy back.
			ct2 := g.temps.alloc()
			ct3 := g.temps.alloc()
			g.copy(ct2, pr, ct3)
			g.temps.free(ct3)
			g.emitIf(ct2, func() {
				g.decr(pr)
			})
			g.temps.free(ct2)
			g.comment("}")
		}

		// Reload active counter for the next iteration (may have changed due to
		// child frame push/pop within the phase).
		g.cache.flushAndInvalidate()
		ct2 := g.temps.alloc()
		g.copy(activeTemp, n.Active, ct2)
		g.temps.free(ct2)
	})
	g.comment("}")
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
