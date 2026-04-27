package main

// Cell is an abstract tape cell identifier used in the IR.
// Cells 0..numFixed-1 are reserved for codegen scratch work.
// Cells numFixed+ are allocated by the lowerer for variables and temps.
type Cell = int

// IRNode is a node in the structured IR.
type IRNode interface {
	irNode()
}

// irBinaryOp is an IR node with Dst, Src1, Src2 fields.
type irBinaryOp interface {
	IRNode
	getDstSrc1Src2() (Cell, Cell, Cell)
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

// IRConst sets a cell to a constant byte value.
type IRConst struct {
	Dst   Cell
	Value byte
}

func (*IRConst) irNode() {}

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

// IRAddI adds a constant to a cell in place.
type IRAddI struct {
	Dst   Cell
	Value byte
}

func (*IRAddI) irNode() {}

// IRSubI subtracts a constant from a cell in place.
type IRSubI struct {
	Dst   Cell
	Value byte
}

func (*IRSubI) irNode() {}

// IRAdd sets dst = src1 + src2.
type IRAdd struct {
	Dst, Src1, Src2 Cell
}

func (*IRAdd) irNode() {}

func (n *IRAdd) getDstSrc1Src2() (Cell, Cell, Cell) {
	return n.Dst, n.Src1, n.Src2
}

// IRSub sets dst = src1 - src2.
type IRSub struct {
	Dst, Src1, Src2 Cell
}

func (*IRSub) irNode() {}

func (n *IRSub) getDstSrc1Src2() (Cell, Cell, Cell) {
	return n.Dst, n.Src1, n.Src2
}

// IRMul sets dst = src1 * src2.
type IRMul struct {
	Dst, Src1, Src2 Cell
}

func (*IRMul) irNode() {}

func (n *IRMul) getDstSrc1Src2() (Cell, Cell, Cell) {
	return n.Dst, n.Src1, n.Src2
}

// IRDiv sets dst = src1 / src2.
type IRDiv struct {
	Dst, Src1, Src2 Cell
}

func (*IRDiv) irNode() {}

func (n *IRDiv) getDstSrc1Src2() (Cell, Cell, Cell) {
	return n.Dst, n.Src1, n.Src2
}

// IRMod sets dst = src1 % src2.
type IRMod struct {
	Dst, Src1, Src2 Cell
}

func (*IRMod) irNode() {}

func (n *IRMod) getDstSrc1Src2() (Cell, Cell, Cell) {
	return n.Dst, n.Src1, n.Src2
}

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

func (n *IRAnd) getDstSrc1Src2() (Cell, Cell, Cell) {
	return n.Dst, n.Src1, n.Src2
}

// IROr sets dst = src1 | src2 (bitwise OR).
type IROr struct {
	Dst, Src1, Src2 Cell
}

func (*IROr) irNode() {}

func (n *IROr) getDstSrc1Src2() (Cell, Cell, Cell) {
	return n.Dst, n.Src1, n.Src2
}

// IRXor sets dst = src1 ^ src2 (bitwise XOR).
type IRXor struct {
	Dst, Src1, Src2 Cell
}

func (*IRXor) irNode() {}

func (n *IRXor) getDstSrc1Src2() (Cell, Cell, Cell) {
	return n.Dst, n.Src1, n.Src2
}

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

func (n *IRCmp) getDstSrc1Src2() (Cell, Cell, Cell) {
	return n.Dst, n.Src1, n.Src2
}

// IRNot computes logical not: dst = (src == 0) ? 1 : 0.
type IRNot struct {
	Dst Cell
	Src Cell
}

func (*IRNot) irNode() {}

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

// IRDynLoad loads a value from a dynamically indexed array slot.
type IRDynLoad struct {
	Dst      Cell
	BaseSlot int
	Index    Cell
}

func (*IRDynLoad) irNode() {}

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

// IRFramePushDyn allocates stack slots with a runtime-determined count.
// The Size cell holds the number of slots to push.
type IRFramePushDyn struct {
	Size Cell
}

func (*IRFramePushDyn) irNode() {}

// IRFramePop deallocates the top stack frame.
type IRFramePop struct {
	Slots int
}

func (*IRFramePop) irNode() {}

// IRLoadFrame loads a value from the current frame's slot into a register cell.
type IRLoadFrame struct {
	Dst       Cell
	Slot      int
	FrameSize int
}

func (*IRLoadFrame) irNode() {}

// IRStoreFrame stores a register cell's value into the current frame's slot.
type IRStoreFrame struct {
	Slot      int
	Src       Cell
	FrameSize int
}

func (*IRStoreFrame) irNode() {}

// IRDispatch is a phase dispatch loop for recursive functions.
type IRDispatch struct {
	Active    Cell
	FrameSize int
	Phases    []*IRBlock
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
