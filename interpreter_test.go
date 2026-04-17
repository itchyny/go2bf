package main

import (
	"strings"
	"testing"
)

func TestInterpret(t *testing.T) {
	tests := []struct {
		name   string
		code   string
		input  string
		output string
	}{
		{"hello", "+++++++++[>++++++++>+++++++++++>+++++<<<-]>.>++.+++++++..+++.>-.------------.<<+++++++++++++++.>.+++.------.--------.>+.", "", "Hello, World!"},
		{"cat", ",[.,]", "abc", "abc"},
		// opClear: [-]
		{"clear", "+++++[-].", "", "\x00"},
		// opScan: [>] scan right for zero
		{"scan right", "+>+>+>+>[-<<<<].", "", "\x00"},
		// opScan: [<] scan left for zero
		{"scan left", ">>>>+>+>+>+<<<<<<<[>>>>].", "", "\x00"},
		// opMult: [->>+<<] multiply
		{"mult loop", "+++++[->>++<<]>>.", "", "\n"}, // 5*2=10='\n'
		// opMult with value=0 (skip mult body)
		{"mult zero", "[->>++<<]>>.", "", "\x00"},
		// opMult multi-target: [->>+>+++<<<]
		{"mult multi", "+++[->>+>+++<<<]>>.>.", "", "\x03\x09"}, // 3*1=3, 3*3=9
		// opMult sub-last: [>+<-] (codegen pattern)
		{"mult sub last", "+++++[>++<-]>.", "", "\n"}, // 5*2=10='\n'
		// opMult sub-last multi-target: [>>+>+++<<<-]
		{"mult sub last multi", "+++[>>+>+++<<<-]>>.>.", "", "\x03\x09"},
		// Destructive copy (recognized as mult loop)
		{"copy loop", "+[->+<]>.", "", "\x01"},
		// Loops that are not multiplication loops
		{"loop first not sub", "++[>+<-]>.", "", "\x02"},       // body[0] is move, not sub(1)
		{"loop inner even", "[->+>+].", "", "\x00"},            // inner length even (no trailing move)
		{"loop inner has output", "+[-.>+<].", "", "\x00\x00"}, // inner pair starts with output, not move
		{"loop inner has dot", "[->.<+].", "", "\x00"},         // inner pair has output, not add
		// isMultLoop: last element not opMove
		{"loop last not move", "[->+.].", "", "\x00"},
		// opDivMod: native divmod on 6 cells
		{"divmod", "++++++++++++++++>+++++<" + divModCode + ">>.>.", "", "\x01\x03"}, // 16/5: rem=1, quot=3
		// Input at EOF returns 0
		{"input eof", ",.", "", "\x00"},
		// Nested loops
		{"nested loop", "+++[>++[>+<-]<-]>>.", "", "\x06"}, // 3*2=6
		// [+] clears (wrapping)
		{"clear plus", "+++++[+].", "", "\x00"},
		// opWalk: [[>>>+<<<-]>>>-] counter-walk
		{"walk", "+++[[>>>+<<<-]>>>-].", "", "\x00"},
		// Walk with stride 2: [[>>+<<-]>>-]
		{"walk stride 2", "++[[>>+<<-]>>-].", "", "\x00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			err := Interpret(tt.code, strings.NewReader(tt.input), &out)
			if err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != tt.output {
				t.Errorf("got %q, want %q", got, tt.output)
			}
		})
	}
}

func TestIsWalkLoop(t *testing.T) {
	// Helper: parse BF to ops with jump table.
	parse := func(code string) []instruction {
		ops, err := compileBF(code)
		if err != nil {
			t.Fatal(err)
		}
		return ops
	}
	// Walk patterns produce opWalk in the output.
	for _, tt := range []struct {
		name string
		code string
		want bool
	}{
		{"stride 3", "[[>>>+<<<-]>>>-]", true},
		{"stride 2", "[[>>+<<-]>>-]", true},
		{"stride 1", "[[>+<-]>-]", true},
		// Negative cases: these should NOT produce opWalk.
		{"too short", "[>-]", false},
		// 8 ops inside outer loop, but start+1 is opMove not opOpen (line 177)
		{"no inner loop", "[>+>+>+>+]", false},
		// Inner loop with wrong add direction (line 184)
		{"wrong inner add", "[[>>>-<<<+]>>>-]", false},
		// Inner loop with wrong move-back stride (line 187)
		{"wrong inner move back", "[[>>>+<<-]>>>-]", false},
		// Inner loop with wrong final add (line 190)
		{"wrong inner final add", "[[>>>+<<<+]>>>-]", false},
		// Wrong outer move stride (line 192)
		{"wrong outer move", "[[>>>+<<<-]>>-]", false},
		// Wrong outer add (line 195)
		{"wrong outer add", "[[>>>+<<<-]>>>+]", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ops := parse(tt.code)
			hasWalk := false
			for _, op := range ops {
				if op.op == opWalk {
					hasWalk = true
					break
				}
			}
			if hasWalk != tt.want {
				t.Errorf("got walk=%v, want %v", hasWalk, tt.want)
			}
		})
	}
}

func TestInterpretError(t *testing.T) {
	tests := []struct {
		name string
		code string
		err  string
	}{
		{"unmatched close", "]", "unmatched ']'"},
		{"unmatched open", "[", "unmatched '['"},
		{"nested unmatched", "[[[]", "unmatched '['"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			err := Interpret(tt.code, strings.NewReader(""), &out)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.err) {
				t.Errorf("got error %q, want it to contain %q", err, tt.err)
			}
		})
	}
}
