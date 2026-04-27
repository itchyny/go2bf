package main

// OptimizeIR performs peephole optimizations on the IR before code generation.
func OptimizeIR(prog *Program) {
	optimizeBlock(prog.Main)
	eliminateDeadStores(prog.Main)
}

func optimizeBlock(block *IRBlock) {
	// Track known constant values for cells.
	known := map[Cell]byte{}
	out := make([]IRNode, 0, len(block.Nodes))
	for _, node := range block.Nodes {
		switch n := node.(type) {
		case *IRConst:
			if prev, ok := known[n.Dst]; ok {
				diff := int(n.Value) - int(prev)
				// Skip if same nonzero value. Don't skip zero - the cell may
				// have been freed and reused, with stale data in the register.
				if diff == 0 && n.Value != 0 {
					continue
				}
				if prev != 0 && diff > 0 && diff < int(n.Value) {
					out = append(out, &IRAddI{Dst: n.Dst, Value: byte(diff)}) // #nosec G115
					known[n.Dst] = n.Value
					continue
				}
				if prev != 0 && diff < 0 && -diff < int(prev) {
					out = append(out, &IRSubI{Dst: n.Dst, Value: byte(-diff)}) // #nosec G115
					known[n.Dst] = n.Value
					continue
				}
			}
			out = append(out, n)
			known[n.Dst] = n.Value
		case *IRZero:
			// Don't skip: the cell may have been freed and reused,
			// with stale data in the register from a different cell.
			out = append(out, n)
			known[n.Dst] = 0
		case *IRAddI:
			if prev, ok := known[n.Dst]; ok {
				known[n.Dst] = prev + n.Value
			} else {
				delete(known, n.Dst)
			}
			out = append(out, n)
		case *IRSubI:
			if prev, ok := known[n.Dst]; ok {
				known[n.Dst] = prev - n.Value
			} else {
				delete(known, n.Dst)
			}
			out = append(out, n)
		case *IRMove:
			// dst gets src's value, src becomes 0.
			if v, ok := known[n.Src]; ok {
				known[n.Dst] = v
			} else {
				delete(known, n.Dst)
			}
			known[n.Src] = 0
			out = append(out, n)
		case *IRCopy:
			if v, ok := known[n.Src]; ok {
				known[n.Dst] = v
			} else {
				delete(known, n.Dst)
			}
			out = append(out, n)
		case *IRIf:
			// Recurse into branches, then invalidate all knowledge
			// (we don't know which branch was taken).
			optimizeBlock(n.Then)
			if n.Else != nil {
				optimizeBlock(n.Else)
			}
			known = map[Cell]byte{}
			out = append(out, n)
		case *IRLoop:
			optimizeBlock(n.Body)
			known = map[Cell]byte{}
			out = append(out, n)
		case *IRDispatch:
			for _, phase := range n.Phases {
				optimizeBlock(phase)
			}
			known = map[Cell]byte{}
			out = append(out, n)
		case *IRFree:
			delete(known, n.Cell)
			out = append(out, n)
		default:
			// Any other node invalidates knowledge about its destination.
			invalidateNodeDst(known, node)
			out = append(out, n)
		}
	}
	block.Nodes = out
}

// eliminateDeadStores removes writes to cells that are overwritten before
// being read. A forward scan tracks the last write index for each cell;
// when a second write occurs with no intervening read or control flow,
// the first write is marked dead and removed.
func eliminateDeadStores(block *IRBlock) {
	// Recurse into sub-blocks (but not loop bodies -- all writes in a loop
	// body are potentially live due to re-execution).
	for _, node := range block.Nodes {
		if n, ok := node.(*IRIf); ok {
			eliminateDeadStores(n.Then)
			if n.Else != nil {
				eliminateDeadStores(n.Else)
			}
		}
	}

	written := map[Cell]int{} // cell -> index of last write
	dead := map[int]bool{}
	markDead := func(cell Cell) {
		if prev, ok := written[cell]; ok {
			dead[prev] = true
		}
	}
	markRead := func(cell Cell) {
		delete(written, cell)
	}
	clearAll := func() {
		written = map[Cell]int{}
	}

	for i, node := range block.Nodes {
		switch n := node.(type) {
		case *IRConst:
			markDead(n.Dst)
			written[n.Dst] = i
		case *IRZero:
			markDead(n.Dst)
			written[n.Dst] = i
		case *IRCopy:
			markRead(n.Src)
			markDead(n.Dst)
			written[n.Dst] = i
		case *IRMove:
			markRead(n.Src)
			markDead(n.Dst)
			written[n.Dst] = i
		case irBinaryOp:
			dst, src1, src2 := n.getDstSrc1Src2()
			markRead(src1)
			markRead(src2)
			markDead(dst)
			written[dst] = i
		case *IRDivMod:
			markRead(n.Src1)
			markRead(n.Src2)
			markDead(n.QuotDst)
			markDead(n.RemDst)
			written[n.QuotDst] = i
			written[n.RemDst] = i
		case *IRNot:
			markRead(n.Src)
			markDead(n.Dst)
			written[n.Dst] = i
		case *IRGetc:
			markDead(n.Dst)
			written[n.Dst] = i
		case *IRAddI:
			markRead(n.Dst)
		case *IRSubI:
			markRead(n.Dst)
		case *IRPutc:
			markRead(n.Src)
		case *IRDynLoad:
			markRead(n.Index)
			markDead(n.Dst)
			written[n.Dst] = i
		case *IRDynStore:
			markRead(n.Index)
			markRead(n.Src)
		case *IRFree:
			markDead(n.Cell)
			delete(written, n.Cell)
		default:
			clearAll()
		}
	}

	if len(dead) > 0 {
		out := block.Nodes[:0]
		for i, node := range block.Nodes {
			if !dead[i] {
				out = append(out, node)
			}
		}
		block.Nodes = out
	}
}

// invalidateNodeDst removes known values for cells written by the node.
func invalidateNodeDst(known map[Cell]byte, node IRNode) {
	switch n := node.(type) {
	case *IRAdd:
		delete(known, n.Dst)
	case *IRSub:
		delete(known, n.Dst)
	case *IRMul:
		delete(known, n.Dst)
	case *IRDiv:
		delete(known, n.Dst)
	case *IRMod:
		delete(known, n.Dst)
	case *IRDivMod:
		delete(known, n.QuotDst)
		delete(known, n.RemDst)
	case *IRCmp:
		delete(known, n.Dst)
	case *IRNot:
		delete(known, n.Dst)
	case *IRGetc:
		delete(known, n.Dst)
	case *IRDynLoad:
		delete(known, n.Dst)
	case *IRDynStore:
		// Writes to dynamic slot - can't track.
	case *IRLoadFrame:
		delete(known, n.Dst)
	case *IRFramePush, *IRFramePushDyn, *IRFramePop, *IRStoreFrame, *IRPutc:
		// No register destination to invalidate.
	}
}
