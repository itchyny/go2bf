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

func TestIRHasDst(t *testing.T) {
	ops := []irHasDst{
		&IRZero{Dst: 10},
		&IRConst{Dst: 11},
		&IRCopy{Dst: 12},
		&IRAddI{Dst: 13},
		&IRSubI{Dst: 14},
		&IRAdd{Dst: 15},
		&IRSub{Dst: 16},
		&IRMul{Dst: 17},
		&IRDiv{Dst: 18},
		&IRMod{Dst: 19},
		&IRAnd{Dst: 20},
		&IROr{Dst: 21},
		&IRXor{Dst: 22},
		&IRCmp{Dst: 23},
		&IRNot{Dst: 24},
		&IRGetc{Dst: 25},
		&IRDynLoad{Dst: 26},
		&IRLoadFrame{Dst: 27},
	}
	for i, op := range ops {
		if got, want := op.getDst(), Cell(10+i); got != want {
			t.Errorf("%T.getDst() = %d, want %d", op, got, want)
		}
	}
}
