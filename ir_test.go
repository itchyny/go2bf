package main

import "testing"

func TestIRNode(t *testing.T) {
	// Verify all IR node types satisfy the IRNode interface.
	nodes := []IRNode{
		&IRBlock{},
		&IRZero{Dst: 1},
		&IRConst{Dst: 1, Value: 42},
		&IRMove{Dst: 1, Src: 2},
		&IRCopy{Dst: 1, Src: 2},
		&IRAddI{Dst: 1, Value: 1},
		&IRSubI{Dst: 1, Value: 1},
		&IRAdd{Dst: 1, Src1: 2, Src2: 3},
		&IRSub{Dst: 1, Src1: 2, Src2: 3},
		&IRMul{Dst: 1, Src1: 2, Src2: 3},
		&IRDiv{Dst: 1, Src1: 2, Src2: 3},
		&IRMod{Dst: 1, Src1: 2, Src2: 3},
		&IRDivMod{QuotDst: 1, RemDst: 2, Src1: 3, Src2: 4},
		&IRAnd{Dst: 1, Src1: 2, Src2: 3},
		&IROr{Dst: 1, Src1: 2, Src2: 3},
		&IRXor{Dst: 1, Src1: 2, Src2: 3},
		&IRCmp{Op: CmpEq, Dst: 1, Src1: 2, Src2: 3},
		&IRNot{Dst: 1, Src: 2},
		&IRIf{Cond: 1, Then: &IRBlock{}},
		&IRLoop{Cond: 1, Body: &IRBlock{}},
		&IRPutc{Src: 1},
		&IRGetc{Dst: 1},
		&IRDynLoad{Dst: 1, BaseSlot: 0, Index: 2},
		&IRDynStore{BaseSlot: 0, Index: 1, Src: 2},
		&IRFramePush{Slots: 8},
		&IRFramePop{Slots: 8},
		&IRFramePushDyn{Size: 1},
		&IRFramePopDyn{Size: 1},
		&IRLoadFrame{Dst: 1, Slot: 0, FrameSize: 8},
		&IRStoreFrame{Slot: 0, Src: 1, FrameSize: 8},
		&IRDispatch{Active: 1, FrameSize: 8, Phases: nil},
		&IRFree{Cell: 1},
	}
	for _, n := range nodes {
		n.irNode()
	}
	if expected := 32; len(nodes) != expected {
		t.Errorf("expected %d IR node types, got %d", expected, len(nodes))
	}
}

func TestIRBinaryOp(t *testing.T) {
	ops := []irBinaryOp{
		&IRAdd{Dst: 10, Src1: 20, Src2: 30},
		&IRSub{Dst: 11, Src1: 21, Src2: 31},
		&IRMul{Dst: 12, Src1: 22, Src2: 32},
		&IRDiv{Dst: 13, Src1: 23, Src2: 33},
		&IRMod{Dst: 14, Src1: 24, Src2: 34},
		&IRAnd{Dst: 15, Src1: 25, Src2: 35},
		&IROr{Dst: 16, Src1: 26, Src2: 36},
		&IRXor{Dst: 17, Src1: 27, Src2: 37},
		&IRCmp{Op: CmpLt, Dst: 18, Src1: 28, Src2: 38},
	}
	for _, op := range ops {
		dst, src1, src2 := op.getDstSrc1Src2()
		if dst == 0 || src1 == 0 || src2 == 0 {
			t.Errorf("unexpected zero field in %T", op)
		}
		if src1 != dst+10 || src2 != dst+20 {
			t.Errorf("unexpected fields in %T: %d %d %d",
				op, dst, src1, src2)
		}
	}
}
