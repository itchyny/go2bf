package main

import "testing"

func TestOptimize(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		output string
	}{
		// Merging adjacent same-kind tokens.
		{"merge add", "+++--", "+"},
		{"merge sub", "---++", "-"},
		{"merge right", ">>><<", ">"},
		{"merge left", "<<<>>", "<"},

		// Cancellation (zero-delta removal).
		{"cancel add", "+-", ""},
		{"cancel move", "><", ""},
		{"cancel long add", "+++---", ""},
		{"cancel long move", ">><<", ""},

		// Dead loop elimination: [-][ ... ] -> [-]
		{"clear dead loop", "[-][+++]", "[-]"},
		{"clear dead nested loop", "[-][[+>]<-]", "[-]"},
		{"clear no dead loop", "[-]+[+]", "[-]+[+]"},

		// Comments preserved (block merging across comments).
		{"preserve comment", "++# comment\n++", "++# comment\n++"},
		{"comment only", "# hello\n", "# hello\n"},
		{"comment between cancel", "+# mid\n-", "+# mid\n-"},

		// I/O and brackets preserved.
		{"io preserved", ".,.,", ".,.,"},
		{"brackets preserved", "[->+<]", "[->+<]"},

		// Empty input.
		{"empty", "", ""},

		// No-op patterns.
		{"clear pattern", "[-]", "[-]"},
		{"add pattern", "[+]", "[+]"},

		// Complex: different kinds don't merge.
		{"different kinds", ">>>+++<<<---", ">>>+++<<<---"},

		// [+] wraps to clear but dead-loop detection only matches [-].
		{"clear plus no dead loop", "[+][>+<]", "[+][>+<]"},

		// Multiple dead loops chained: [-][ eliminates second loop,
		// then the result [-] triggers another elimination.
		{"chained dead loops", "[-][+][>]", "[-]"},

		// Dead loop with comment between clear and open bracket.
		// The comment is consumed as part of the dead loop body.
		{"dead loop comment gap", "[-]# note\n[+++]", "[-]"},

		// Highway round-trip elimination:
		// [<<<]<<<<<<<<[<<<<<<<<]>>>>>>>>[>>>>>>>>] -> [<<<]
		{
			"highway round-trip",
			"[<<<]<<<<<<<<[<<<<<<<<]>>>>>>>>[>>>>>>>>]",
			"[<<<]",
		},
		{
			"highway round-trip with comment",
			"[<<<]<<<<<<<<[<<<<<<<<]# nav\n>>>>>>>>[>>>>>>>>]",
			"[<<<]# nav\n",
		},
		{
			"highway round-trip no match without guard scan",
			"<<<<<<<<[<<<<<<<<]>>>>>>>>[>>>>>>>>]",
			"<<<<<<<<[<<<<<<<<]>>>>>>>>[>>>>>>>>]",
		},
		{
			"highway round-trip preserves surrounding",
			"+[<<<]<<<<<<<<[<<<<<<<<]>>>>>>>>[>>>>>>>>]-",
			"+[<<<]-",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Optimize(tt.input)
			if got != tt.output {
				t.Errorf("got %q, want %q", got, tt.output)
			}
		})
	}
}
