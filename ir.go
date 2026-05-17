package main

// Cell is an abstract tape cell identifier used in the IR.
// Cells 0..sentinelFwd are reserved for codegen scratch work.
// Cells sentinelFwd+1 and beyond are allocated by the lowerer for
// variables and temps (stack slots).
type Cell = int

// IRNode is a node in the structured IR.
type IRNode interface {
	irNode()
}

// irHasDst is an IR node that writes to a single cell.
type irHasDst interface {
	IRNode
	getDst() Cell
}

// IRBlock is a sequence of IR nodes.
type IRBlock struct {
	Nodes []IRNode
}

func (*IRBlock) irNode() {}

// IRZero clears a cell to 0.
type IRZero struct {
	Dst Cell
}

func (*IRZero) irNode() {}

func (n *IRZero) getDst() Cell { return n.Dst }

// IRConst sets a cell to a constant byte value.
type IRConst struct {
	Dst   Cell
	Value byte
}

func (*IRConst) irNode() {}

func (n *IRConst) getDst() Cell { return n.Dst }

// IRMove moves src to dst (destructive, src becomes 0).
type IRMove struct {
	Dst Cell
	Src Cell
}

func (*IRMove) irNode() {}

// IRCopy copies src to dst (non-destructive, src preserved).
type IRCopy struct {
	Dst Cell
	Src Cell
}

func (*IRCopy) irNode() {}

func (n *IRCopy) getDst() Cell { return n.Dst }

// IRAddI adds a constant to a cell in place.
type IRAddI struct {
	Dst   Cell
	Value byte
}

func (*IRAddI) irNode() {}

func (n *IRAddI) getDst() Cell { return n.Dst }

// IRSubI subtracts a constant from a cell in place.
type IRSubI struct {
	Dst   Cell
	Value byte
}

func (*IRSubI) irNode() {}

func (n *IRSubI) getDst() Cell { return n.Dst }

// IRAdd sets dst = src1 + src2.
type IRAdd struct {
	Dst, Src1, Src2 Cell
}

func (*IRAdd) irNode() {}

func (n *IRAdd) getDst() Cell { return n.Dst }

// IRSub sets dst = src1 - src2.
type IRSub struct {
	Dst, Src1, Src2 Cell
}

func (*IRSub) irNode() {}

func (n *IRSub) getDst() Cell { return n.Dst }

// IRMul sets dst = src1 * src2.
type IRMul struct {
	Dst, Src1, Src2 Cell
}

func (*IRMul) irNode() {}

func (n *IRMul) getDst() Cell { return n.Dst }

// IRDiv sets dst = src1 / src2.
type IRDiv struct {
	Dst, Src1, Src2 Cell
}

func (*IRDiv) irNode() {}

func (n *IRDiv) getDst() Cell { return n.Dst }

// IRMod sets dst = src1 % src2.
type IRMod struct {
	Dst, Src1, Src2 Cell
}

func (*IRMod) irNode() {}

func (n *IRMod) getDst() Cell { return n.Dst }

// IRDivMod computes both quotient and remainder in one operation.
type IRDivMod struct {
	QuotDst, RemDst, Src1, Src2 Cell
}

func (*IRDivMod) irNode() {}

// IRAnd sets dst = src1 & src2 (bitwise AND).
type IRAnd struct {
	Dst, Src1, Src2 Cell
}

func (*IRAnd) irNode() {}

func (n *IRAnd) getDst() Cell { return n.Dst }

// IROr sets dst = src1 | src2 (bitwise OR).
type IROr struct {
	Dst, Src1, Src2 Cell
}

func (*IROr) irNode() {}

func (n *IROr) getDst() Cell { return n.Dst }

// IRXor sets dst = src1 ^ src2 (bitwise XOR).
type IRXor struct {
	Dst, Src1, Src2 Cell
}

func (*IRXor) irNode() {}

func (n *IRXor) getDst() Cell { return n.Dst }

// CmpOp is a comparison operation.
type CmpOp int

const (
	// CmpEq is the == comparison.
	CmpEq CmpOp = iota
	// CmpNeq is the != comparison.
	CmpNeq
	// CmpLt is the < comparison.
	CmpLt
	// CmpGt is the > comparison.
	CmpGt
	// CmpLeq is the <= comparison.
	CmpLeq
	// CmpGeq is the >= comparison.
	CmpGeq
)

// IRCmp compares two cells and writes 1 or 0 to dst.
type IRCmp struct {
	Op   CmpOp
	Dst  Cell
	Src1 Cell
	Src2 Cell
}

func (*IRCmp) irNode() {}

func (n *IRCmp) getDst() Cell { return n.Dst }

// IRNot computes logical not: dst = (src == 0) ? 1 : 0.
type IRNot struct {
	Dst Cell
	Src Cell
}

func (*IRNot) irNode() {}

func (n *IRNot) getDst() Cell { return n.Dst }

// IRIf is a structured if/else.
// Executes Then if Cond != 0, Else otherwise.
type IRIf struct {
	Cond Cell
	Then *IRBlock
	Else *IRBlock // nil if no else branch
}

func (*IRIf) irNode() {}

// IRLoop is a structured while loop.
// Loops while Cond != 0.
type IRLoop struct {
	Cond Cell
	Body *IRBlock
}

func (*IRLoop) irNode() {}

// IRPutc outputs the byte in src (Brainfuck '.').
type IRPutc struct {
	Src Cell
}

func (*IRPutc) irNode() {}

// IRGetc reads a byte into dst (Brainfuck ',').
type IRGetc struct {
	Dst Cell
}

func (*IRGetc) irNode() {}

func (n *IRGetc) getDst() Cell { return n.Dst }

// IRDynLoad loads a value from a dynamically indexed array slot.
type IRDynLoad struct {
	Dst      Cell
	BaseSlot int
	Index    Cell
}

func (*IRDynLoad) irNode() {}

func (n *IRDynLoad) getDst() Cell { return n.Dst }

// IRDynStore stores a value to a dynamically indexed array slot.
type IRDynStore struct {
	BaseSlot int
	Index    Cell
	Src      Cell
}

func (*IRDynStore) irNode() {}

// IRFramePush allocates a new stack frame with the given number of slots.
type IRFramePush struct {
	Slots int
}

func (*IRFramePush) irNode() {}

// IRFramePop deallocates the top stack frame.
type IRFramePop struct {
	Slots int
}

func (*IRFramePop) irNode() {}

// IRFramePushDyn allocates stack slots with a runtime-determined count.
// The Size cell holds the number of slots to push.
type IRFramePushDyn struct {
	Size Cell
}

func (*IRFramePushDyn) irNode() {}

// IRFramePopDyn deallocates a runtime-determined number of stack slots
// from the top. The Size cell holds the number of slots to pop.
type IRFramePopDyn struct {
	Size Cell
}

func (*IRFramePopDyn) irNode() {}

// IRLoadFrame loads a value from the current frame's slot into a register cell.
type IRLoadFrame struct {
	Dst       Cell
	Slot      int
	FrameSize int
}

func (*IRLoadFrame) irNode() {}

func (n *IRLoadFrame) getDst() Cell { return n.Dst }

// IRStoreFrame stores a register cell's value into the current frame's slot.
type IRStoreFrame struct {
	Slot      int
	Src       Cell
	FrameSize int
}

func (*IRStoreFrame) irNode() {}

// IRDispatch is a phase dispatch loop for recursive functions.
//
// Phase, Pr, Flag, ActiveTemp are phase-temp cells reserved by the
// rec lowerer for genDispatch's internal state. They're non-zero
// only when the function uses bitwise operators (which need the
// full algo-temp pool for genBitwise's ~11-temp peak); otherwise
// they're left zero and genDispatch allocates the four cells from
// the algo-temp pool itself -- closer to the registers.
type IRDispatch struct {
	Active                      Cell
	Phase, Pr, Flag, ActiveTemp Cell
	FrameSize                   int
	Phases                      []*IRBlock
}

func (*IRDispatch) irNode() {}

// IRFree marks a cell as dead. The codegen frees the register without
// storing to stack, preventing dead cells from being flushed.
type IRFree struct {
	Cell Cell
}

func (*IRFree) irNode() {}

// Program is the top-level IR representation.
type Program struct {
	Main      *IRBlock
	CellsUsed int
}
