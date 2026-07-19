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

		// Dead loop elimination: any `]` leaves the current cell 0, so a loop
		// immediately after it is dead -- unless a pointer move or value
		// change in between makes the cell nonzero.
		{"clear dead loop", "[-][+++]", "[-]"},
		{"clear dead nested loop", "[-][[+>]<-]", "[-]"},
		{"clear no dead loop", "[-]+[+]", "[-]+[+]"},
		{"any loop-end marks dead loop", "[+][>+<]", "[+]"},
		{"dead loop after plain loop-end", "[>][-]", "[>]"},
		{"loop-end then move keeps clear", "[>]<[-]", "[>]<[-]"},
		{"chained dead loops", "[-][+][>]", "[-]"},
		{"dead loop comment gap", "[-]# note\n[+++]", "[-]"},

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
