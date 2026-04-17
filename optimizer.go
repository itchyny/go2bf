package main

import "strings"

type tok struct {
	kind    byte   // 'v' for +/-, 'p' for >/<, '.', ',', '[', ']', 'c' for comment
	val     int    // delta (for 'v' and 'p')
	comment string // text (for 'c')
}

// Optimize performs peephole optimizations on Brainfuck code.
// It merges adjacent operations, cancels opposing pairs, and removes no-ops.
// Comments (non-Brainfuck text) are preserved in the output.
func Optimize(code string) string {
	tokens := tokenize(code)
	tokens = mergeTokens(tokens)
	tokens = eliminateDeadLoops(tokens)
	tokens = eliminateHighwayRoundTrips(tokens)
	return render(tokens)
}

// tokenize parses BF code into tokens, preserving comments.
func tokenize(code string) []tok {
	tokens := make([]tok, 0, len(code))
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch c {
		case '+':
			tokens = append(tokens, tok{kind: 'v', val: 1})
		case '-':
			tokens = append(tokens, tok{kind: 'v', val: -1})
		case '>':
			tokens = append(tokens, tok{kind: 'p', val: 1})
		case '<':
			tokens = append(tokens, tok{kind: 'p', val: -1})
		case '.', ',', '[', ']':
			tokens = append(tokens, tok{kind: c})
		default:
			// Collect comment text (non-Brainfuck characters).
			j := strings.IndexAny(code[i:], "><+-.,[]")
			if j < 0 {
				j = len(code)
			} else {
				j += i
			}
			tokens = append(tokens, tok{kind: 'c', comment: code[i:j]})
			i = j - 1
		}
	}
	return tokens
}

// mergeTokens merges adjacent same-kind tokens and removes zero-delta tokens.
func mergeTokens(tokens []tok) []tok {
	merged := make([]tok, 0, len(tokens))
	for _, t := range tokens {
		if (t.kind == 'v' || t.kind == 'p') && len(merged) > 0 &&
			merged[len(merged)-1].kind == t.kind {
			merged[len(merged)-1].val += t.val
		} else {
			merged = append(merged, t)
		}
	}
	return merged
}

// eliminateDeadLoops removes loops after [-] (cell is known to be 0).
// Handles chained dead loops: [-][+][>] -> [-] in a single pass.
func eliminateDeadLoops(tokens []tok) []tok {
	result := tokens[:0]
	for i := 0; i < len(tokens); i++ {
		result = append(result, tokens[i])
		// Detect [-][ pattern: clear followed by loop -> dead loop.
		// Use a loop to handle chains: after removing one dead loop,
		// [-] is still at the end of result so check again.
		for len(result) >= 3 &&
			result[len(result)-3].kind == '[' &&
			result[len(result)-2].kind == 'v' && result[len(result)-2].val == -1 &&
			result[len(result)-1].kind == ']' {
			// Check if next non-comment token is '['.
			j := i + 1
			for j < len(tokens) && tokens[j].kind == 'c' {
				j++
			}
			if j >= len(tokens) || tokens[j].kind != '[' {
				break
			}
			// Skip the dead loop: find matching ']'.
			depth := 1
			j++
			for j < len(tokens) && depth > 0 {
				switch tokens[j].kind {
				case '[':
					depth++
				case ']':
					depth--
				}
				j++
			}
			i = j - 1 // skip past dead loop
		}
	}
	return result
}

// eliminateHighwayRoundTrips removes no-op scan round-trips:
// ]<<<<<<<<[<<<<<<<<]>>>>>>>>[>>>>>>>>] -> ]
// The pattern p(-N)[p(-N)]p(N)[p(N)] is a no-op when preceded by ]
// (a guard/highway scan that landed on a zero cell). A comment may
// appear between the backward and forward scans.
func eliminateHighwayRoundTrips(tokens []tok) []tok {
	result := tokens[:0]
	for i := 0; i < len(tokens); i++ {
		result = append(result, tokens[i])
		if tokens[i].kind != ']' {
			continue
		}
		j := i + 1
		offset := 0
		if j+5 < len(tokens) && tokens[j+4].kind == 'c' {
			offset = 1
		}
		if j+7+offset <= len(tokens) {
			if v := tokens[j].val; tokens[j].kind == 'p' && v < 0 &&
				tokens[j+1].kind == '[' &&
				tokens[j+2].kind == 'p' && tokens[j+2].val == v &&
				tokens[j+3].kind == ']' &&
				tokens[j+4+offset].kind == 'p' && tokens[j+4+offset].val == -v &&
				tokens[j+5+offset].kind == '[' &&
				tokens[j+6+offset].kind == 'p' && tokens[j+6+offset].val == -v &&
				tokens[j+7+offset].kind == ']' {
				if offset == 1 {
					result = append(result, tokens[j+4])
				}
				i = j + 7 + offset
			}
		}
	}
	return result
}

// render reconstructs Brainfuck code from tokens.
func render(tokens []tok) string {
	var buf strings.Builder
	for _, t := range tokens {
		switch t.kind {
		case 'v':
			if t.val > 0 {
				buf.WriteString(strings.Repeat("+", t.val))
			} else {
				buf.WriteString(strings.Repeat("-", -t.val))
			}
		case 'p':
			if t.val > 0 {
				buf.WriteString(strings.Repeat(">", t.val))
			} else {
				buf.WriteString(strings.Repeat("<", -t.val))
			}
		case 'c':
			buf.WriteString(t.comment)
		default:
			buf.WriteByte(t.kind)
		}
	}
	return buf.String()
}
