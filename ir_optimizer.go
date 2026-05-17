package main

// OptimizeIR performs peephole optimizations on the IR before code generation.
func OptimizeIR(prog *Program) {
	optimizeBlock(prog.Main, map[Cell]bool{})
}

// optimizeBlock walks a block once, doing three cleanups in lockstep:
//
//  1. Constant folding -- tracks the last known constant value in
//     `known[c] byte`, drops a redundant `IRConst{c, v}` when `c` is
//     already at `v`.
//  2. Delta conversion -- replaces `IRConst{c, v}` with `IRAddI`/
//     `IRSubI` when the delta from the prior known value is smaller
//     than the literal (common in string-literal printing).
//  3. Fresh-zero elimination -- drops `IRZero{c}` when `c` has never
//     been written anywhere prior in the program. The BF tape starts
//     at 0 and the register cache has no entry for an untouched
//     cell, so the zero is a no-op. `everWritten` is shared across
//     recursive calls so writes in earlier branches or pre-marked
//     loop bodies keep later `IRZero`s where the cell may carry a
//     stale value or cache entry.
//
// `known` is reset at control-flow boundaries (`IRIf`/`IRLoop`/
// `IRDispatch`/unknown nodes) since the optimizer cannot tell which
// branch ran or what the loop did across iterations. `everWritten`
// is preserved. For loop and dispatch bodies, `markBlockDsts`
// pre-marks every write inside before the recursive walk, so a body-
// local IRZero used to reset a cell between iterations isn't
// mistaken for a fresh-cell zero on the first pass.
func optimizeBlock(block *IRBlock, everWritten map[Cell]bool) {
	known := map[Cell]byte{} // cell -> last known constant value
	out := make([]IRNode, 0, len(block.Nodes))

	for _, node := range block.Nodes {
		switch n := node.(type) {
		case *IRZero:
			if !everWritten[n.Dst] {
				// Fresh cell -- BF tape is 0 and the register cache has
				// no entry, so the zero-store is a no-op. Mark
				// `everWritten` so a later IRZero on this cell (after a
				// real write) isn't dropped as fresh.
				everWritten[n.Dst] = true
				known[n.Dst] = 0
				continue
			}
			out = append(out, n)
			known[n.Dst] = 0
		case *IRConst:
			if prev, ok := known[n.Dst]; ok {
				diff := int(n.Value) - int(prev)
				// Skip if same nonzero value. Don't skip zero -- the cell
				// may have been freed and reused, with stale data in the
				// register from a different cell.
				if diff == 0 && n.Value != 0 {
					continue
				}
				// Convert to IRAddI/IRSubI when the delta is smaller than
				// the absolute value.
				if prev != 0 && diff > 0 && diff < int(n.Value) {
					out = append(out, &IRAddI{Dst: n.Dst, Value: byte(diff)}) // #nosec G115
					everWritten[n.Dst] = true
					known[n.Dst] = n.Value
					continue
				}
				if prev != 0 && diff < 0 && -diff < int(prev) {
					out = append(out, &IRSubI{Dst: n.Dst, Value: byte(-diff)}) // #nosec G115
					everWritten[n.Dst] = true
					known[n.Dst] = n.Value
					continue
				}
			}
			out = append(out, n)
			everWritten[n.Dst] = true
			known[n.Dst] = n.Value
		case *IRMove:
			out = append(out, n)
			everWritten[n.Dst] = true
			everWritten[n.Src] = true // Move zeros Src
			if v, ok := known[n.Src]; ok {
				known[n.Dst] = v
			} else {
				delete(known, n.Dst)
			}
			known[n.Src] = 0
		case *IRCopy:
			out = append(out, n)
			everWritten[n.Dst] = true
			if v, ok := known[n.Src]; ok {
				known[n.Dst] = v
			} else {
				delete(known, n.Dst)
			}
		case *IRAddI:
			out = append(out, n)
			everWritten[n.Dst] = true
			if prev, ok := known[n.Dst]; ok {
				known[n.Dst] = prev + n.Value
			} else {
				delete(known, n.Dst)
			}
		case *IRSubI:
			out = append(out, n)
			everWritten[n.Dst] = true
			if prev, ok := known[n.Dst]; ok {
				known[n.Dst] = prev - n.Value
			} else {
				delete(known, n.Dst)
			}
		case irHasDst:
			out = append(out, n)
			everWritten[n.getDst()] = true
			delete(known, n.getDst())
		case *IRDivMod:
			out = append(out, n)
			everWritten[n.QuotDst] = true
			everWritten[n.RemDst] = true
			delete(known, n.QuotDst)
			delete(known, n.RemDst)
		case *IRIf:
			optimizeBlock(n.Then, everWritten)
			if n.Else != nil {
				optimizeBlock(n.Else, everWritten)
			}
			known = map[Cell]byte{}
			out = append(out, n)
		case *IRLoop:
			markBlockDsts(n.Body, everWritten)
			optimizeBlock(n.Body, everWritten)
			known = map[Cell]byte{}
			out = append(out, n)
		case *IRDispatch:
			for _, p := range n.Phases {
				markBlockDsts(p, everWritten)
				optimizeBlock(p, everWritten)
			}
			known = map[Cell]byte{}
			out = append(out, n)
		case *IRFree:
			out = append(out, n)
			delete(known, n.Cell)
		default:
			out = append(out, node)
		}
	}
	block.Nodes = out
}

// markBlockDsts recursively marks every cell written anywhere in
// `block` as ever-written. Used to pre-populate `everWritten` with
// writes from nested blocks the main scan hasn't visited yet, so a
// later `IRZero` on those cells isn't mistaken for a fresh-cell zero.
func markBlockDsts(block *IRBlock, everWritten map[Cell]bool) {
	for _, n := range block.Nodes {
		switch n := n.(type) {
		case irHasDst:
			everWritten[n.getDst()] = true
		case *IRMove:
			everWritten[n.Dst] = true
			everWritten[n.Src] = true
		case *IRDivMod:
			everWritten[n.QuotDst] = true
			everWritten[n.RemDst] = true
		case *IRIf:
			markBlockDsts(n.Then, everWritten)
			if n.Else != nil {
				markBlockDsts(n.Else, everWritten)
			}
		case *IRLoop:
			markBlockDsts(n.Body, everWritten)
		case *IRDispatch:
			for _, p := range n.Phases {
				markBlockDsts(p, everWritten)
			}
		}
	}
}
