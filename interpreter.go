package main

import (
	"fmt"
	"io"
	"strings"
)

const tapeSize = 30000

// opcode represents a compiled Brainfuck instruction.
type opcode byte

const (
	opMove   opcode = iota // ptr += arg (negative for left)
	opAdd                  // tape[ptr] += byte(arg) (negative for subtract)
	opOut                  // output tape[ptr]
	opIn                   // input to tape[ptr]
	opOpen                 // if tape[ptr]==0 jump to arg
	opClose                // if tape[ptr]!=0 jump to arg
	opClear                // tape[ptr] = 0
	opMult                 // tape[ptr+arg] += tape[ptr] * factor
	opScan                 // scan for zero: ptr += arg until tape[ptr]==0
	opWalk                 // counter-walk: move ptr by arg*tape[ptr], tape[ptr]=0
	opDivMod               // divmod: [n,d,0,0,0,0] -> [0,d-n%d,n%d,n/d,0,0]
)

type instruction struct {
	op     opcode
	arg    int
	factor byte // for opMult: multiplication factor
}

// Interpret executes a Brainfuck program with the given input, writing output to w.
func Interpret(code string, input io.Reader, output io.Writer) error {
	ops, err := compileBF(code)
	if err != nil {
		return err
	}

	tape := make([]byte, tapeSize)
	ptr := 0
	n := len(ops)
	inputBuf := [1]byte{}

	for ip := 0; ip < n; ip++ {
		switch ops[ip].op {
		case opMove:
			ptr += ops[ip].arg
		case opAdd:
			tape[ptr] += byte(ops[ip].arg) // #nosec G115
		case opOut:
			_, _ = output.Write(tape[ptr : ptr+1])
		case opIn:
			nr, err := input.Read(inputBuf[:])
			if err != nil || nr == 0 {
				tape[ptr] = 0
			} else {
				tape[ptr] = inputBuf[0]
			}
		case opOpen:
			if tape[ptr] == 0 {
				ip = ops[ip].arg
			}
		case opClose:
			if tape[ptr] != 0 {
				ip = ops[ip].arg
			}
		case opClear:
			tape[ptr] = 0
		case opMult:
			if v := tape[ptr]; v != 0 {
				for ops[ip].op == opMult {
					tape[ptr+ops[ip].arg] += v * ops[ip].factor
					ip++
				}
				tape[ptr] = 0
			} else {
				for ops[ip].op == opMult {
					ip++
				}
			}
		case opScan:
			for tape[ptr] != 0 {
				ptr += ops[ip].arg
			}
		case opWalk:
			// Counter-walk: shift counter forward by stride, decrement each step.
			// Equivalent to [[>>>+<<<-]>>>-] for stride=3.
			stride := ops[ip].arg
			for tape[ptr] != 0 {
				tape[ptr+stride] += tape[ptr]
				tape[ptr] = 0
				ptr += stride
				tape[ptr]--
			}
		case opDivMod:
			// Native divmod: [n, d, 0, 0, 0, 0] -> [0, d-n%d, n%d, n/d, 0, 0]
			n, d := tape[ptr], tape[ptr+1]
			if d != 0 {
				tape[ptr+3] = n / d
				tape[ptr+2] = n % d
				tape[ptr+1] = d - n%d
			}
			tape[ptr] = 0
		}
	}
	return nil
}

// isMultLoop checks if the loop at ops[start] is a multiplication loop.
// Two patterns are recognized:
//   - sub-first: [sub(1) (move add)+ move]  e.g. [->>+>+++<<<]
//   - sub-last:  [(move add)+ move sub(1)]  e.g. [>>+>+++<<<-]
//
// In both cases, all moves must sum to zero (pointer returns to origin)
// and cell[0] is decremented by exactly 1 per iteration.
func isMultLoop(ops []instruction, start int) bool {
	end := ops[start].arg
	body := ops[start+1 : end]
	if len(body) < 3 {
		return false
	}
	// Determine sub(1) position: first or last instruction.
	var pairs []instruction
	if body[0].op == opAdd && body[0].arg == -1 {
		// Sub-first: [-  (move add)+ move]
		pairs = body[1:]
	} else if body[len(body)-1].op == opAdd && body[len(body)-1].arg == -1 {
		// Sub-last: [(move add)+ move  -]
		pairs = body[:len(body)-1]
	} else {
		return false
	}
	// pairs must be (move, add)+ followed by a final move back.
	if len(pairs) < 3 || len(pairs)%2 == 0 {
		return false
	}
	pos := 0
	for j := 0; j < len(pairs)-1; j += 2 {
		if pairs[j].op != opMove || pairs[j+1].op != opAdd {
			return false
		}
		pos += pairs[j].arg
	}
	last := pairs[len(pairs)-1]
	if last.op != opMove {
		return false
	}
	pos += last.arg
	return pos == 0
}

// isWalkLoop checks if the loop at ops[start] is a counter-walk pattern:
//
//	[[>>>+<<<-]>>>-]
//
// In raw ops (before optimization):
//
//	opOpen opOpen opMove(S) opAdd(1) opMove(-S) opAdd(-1) opClose opMove(S) opAdd(-1) opClose
//
// The inner loop shifts the counter forward by S, the outer loop advances
// the pointer by S and decrements the counter. Net effect: ptr += S * counter.
func isWalkLoop(ops []instruction, start int) bool {
	if ops[start].op != opOpen {
		return false
	}
	// Expect exactly 9 instructions between outer [ and ].
	if ops[start].arg != start+9 {
		return false
	}
	// Inner loop: [opMove(S) opAdd(1) opMove(-S) opAdd(-1)]
	if ops[start+1].op != opOpen || ops[start+1].arg != start+6 ||
		ops[start+2].op != opMove || ops[start+2].arg == 0 ||
		ops[start+3].op != opAdd || ops[start+3].arg != 1 ||
		ops[start+4].op != opMove || ops[start+4].arg != -ops[start+2].arg ||
		ops[start+5].op != opAdd || ops[start+5].arg != -1 ||
		ops[start+6].op != opClose {
		return false
	}
	// After inner loop: opMove(S) opAdd(-1)
	if ops[start+7].op != opMove || ops[start+7].arg != ops[start+2].arg ||
		ops[start+8].op != opAdd || ops[start+8].arg != -1 {
		return false
	}
	return true
}

func compileBF(code string) ([]instruction, error) {
	// Parse and merge runs.
	var ops []instruction
	for i := 0; i < len(code); i++ {
		// Detect divmod loop at raw string level before parsing.
		if strings.HasPrefix(code[i:], divModCode) {
			ops = append(ops, instruction{op: opDivMod})
			i += len(divModCode) - 1
			continue
		}
		c := code[i]
		switch c {
		case '>', '<', '+', '-':
			count := 1
			for i+1 < len(code) && code[i+1] == c {
				count++
				i++
			}
			switch c {
			case '>':
				ops = append(ops, instruction{op: opMove, arg: count})
			case '<':
				ops = append(ops, instruction{op: opMove, arg: -count})
			case '+':
				ops = append(ops, instruction{op: opAdd, arg: count})
			case '-':
				ops = append(ops, instruction{op: opAdd, arg: -count})
			}
		case '.':
			ops = append(ops, instruction{op: opOut})
		case ',':
			ops = append(ops, instruction{op: opIn})
		case '[':
			ops = append(ops, instruction{op: opOpen})
		case ']':
			ops = append(ops, instruction{op: opClose})
		}
	}

	// Build bracket jump table.
	stack := make([]int, 0, 64)
	for i := range ops {
		switch ops[i].op {
		case opOpen:
			stack = append(stack, i)
		case opClose:
			if len(stack) == 0 {
				return nil, fmt.Errorf("unmatched ']'")
			}
			open := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			ops[open].arg = i
			ops[i].arg = open
		}
	}
	if len(stack) != 0 {
		return nil, fmt.Errorf("unmatched '['")
	}

	// Peephole: replace common patterns.
	var opt []instruction
	for i := 0; i < len(ops); i++ {
		// [-] or [+] -> clear
		if ops[i].op == opOpen && ops[i].arg == i+2 {
			inner := ops[i+1]
			if inner.op == opAdd && (inner.arg == -1 || inner.arg == 1) {
				opt = append(opt, instruction{op: opClear})
				i += 2
				continue
			}
		}
		// Multiplication loop -> sequence of opMult + opClear.
		// Handles both sub-first [->>+<<<] and sub-last [>>+<<<-] patterns.
		if ops[i].op == opOpen && isMultLoop(ops, i) {
			body := ops[i+1 : ops[i].arg]
			// Skip the sub(1) to get (move, add)+ pairs.
			var pairs []instruction
			if body[0].op == opAdd && body[0].arg == -1 {
				pairs = body[1 : len(body)-1] // sub-first: skip first sub and last move
			} else {
				pairs = body[:len(body)-2] // sub-last: skip last move and sub
			}
			pos := 0
			var mults []instruction
			for j := 0; j < len(pairs); j += 2 {
				pos += pairs[j].arg            // opMove
				factor := byte(pairs[j+1].arg) // #nosec G115 -- opAdd arg
				mults = append(mults, instruction{op: opMult, arg: pos, factor: factor})
			}
			opt = append(opt, mults...)
			opt = append(opt, instruction{op: opClear})
			i = ops[i].arg
			continue
		}
		// [>>>] -> scan right with stride
		if ops[i].op == opOpen && ops[i].arg == i+2 &&
			ops[i+1].op == opMove {
			opt = append(opt, instruction{op: opScan, arg: ops[i+1].arg})
			i += 2
			continue
		}
		// Counter-walk: [[>>>+<<<-]>>>-] -> opWalk{stride}
		// Walks ptr forward by stride * counter steps, zeroing the counter.
		if isWalkLoop(ops, i) {
			stride := ops[i+2].arg // opMove stride from inner loop
			opt = append(opt, instruction{op: opWalk, arg: stride})
			i = ops[i].arg
			continue
		}
		opt = append(opt, ops[i])
	}

	// Rebuild jump table after optimization.
	stack = stack[:0]
	for i := range opt {
		switch opt[i].op {
		case opOpen:
			stack = append(stack, i)
		case opClose:
			open := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			opt[open].arg = i
			opt[i].arg = open
		}
	}

	return opt, nil
}
