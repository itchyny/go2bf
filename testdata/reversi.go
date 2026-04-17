package main

const (
	empty = 0
	black = 1
	white = 2
)

func opponent(p byte) byte {
	if p == black {
		return white
	}
	return black
}

func main() {
	var board [64]byte
	// Initial position: center 4 pieces.
	board[27] = white // d4
	board[28] = black // e4
	board[35] = black // d5
	board[36] = white // e5

	// Direction deltas. 255 = -1 mod 256.
	dx := [8]byte{255, 255, 255, 0, 0, 1, 1, 1}
	dy := [8]byte{255, 0, 1, 255, 1, 255, 0, 1}

	player := byte(black)
	passes := byte(0)

	for passes < 2 {
		// Check valid moves and count pieces.
		opp := opponent(player)
		valid := [64]byte{}
		hasMove := byte(0)
		bc := byte(0)
		wc := byte(0)
		for pos := byte(0); pos < 64; pos++ {
			if board[pos] == black {
				bc++
			} else if board[pos] == white {
				wc++
			} else {
				pr := pos / 8
				pc := pos % 8
				totalFlips := byte(0)
				for d := byte(0); d < 8; d++ {
					nr := pr + dx[d]
					nc := pc + dy[d]
					if nr < 8 && nc < 8 && board[nr*8+nc] == opp {
						count := byte(0)
						done := byte(0)
						for nr < 8 && nc < 8 && done == 0 {
							v := board[nr*8+nc]
							if v == opp {
								count++
								nr += dx[d]
								nc += dy[d]
							} else {
								if v == player && count > 0 {
									totalFlips += count
								}
								done = 1
							}
						}
					}
				}
				if totalFlips > 0 {
					valid[pos] = totalFlips
					hasMove = 1
				}
			}
		}

		// Print board with valid moves marked as *.
		println("  ABCDEFGH")
		for r := byte(0); r < 8; r++ {
			putchar('1' + r)
			putchar(' ')
			for c := byte(0); c < 8; c++ {
				pos := r*8 + c
				v := board[pos]
				if v == black {
					putchar('X')
				} else if v == white {
					putchar('O')
				} else if valid[pos] > 0 {
					putchar('*')
				} else {
					putchar('.')
				}
			}
			println()
		}

		print("X:")
		print(bc)
		print(" O:")
		println(wc)

		if hasMove == 0 {
			if player == black {
				println("X passes\n")
			} else {
				println("O passes\n")
			}
			passes++
			player = opponent(player)
			continue
		}
		passes = 0

		movePos := byte(255) // chosen move position

		if player == black {
			// Human turn: read and validate move.
			ok := byte(0)
			for ok == 0 && passes < 2 {
				print("X move: ")
				// Skip whitespace, read column letter (A-H).
				ch := getchar()
				for ch == ' ' || ch == '\n' || ch == '\r' {
					ch = getchar()
				}
				if ch == 0 || ch == 255 {
					passes = 2 // EOF: exit game
				} else {
					col := ch - 'A'
					if ch >= 'a' {
						col = ch - 'a'
					}
					// Skip whitespace, read row digit (1-8).
					ch = getchar()
					for ch == ' ' || ch == '\n' || ch == '\r' {
						ch = getchar()
					}
					row := ch - '1'
					if row < 8 && col < 8 {
						movePos = row*8 + col
						if valid[movePos] > 0 {
							ok = 1
							println()
						} else if board[movePos] == empty {
							putchar('A' + col)
							putchar('1' + row)
							println(" is invalid move")
						} else {
							putchar('A' + col)
							putchar('1' + row)
							println(" is not empty")
						}
					} else {
						putchar('A' + col)
						putchar('1' + row)
						println(" is out of bounds")
					}
				}
			}
		} else {
			// Computer turn: weighted strategy.
			bestScore := byte(0)
			for pos := byte(0); pos < 64; pos++ {
				if valid[pos] > 0 {
					pr := pos / 8
					pc := pos % 8
					// Nearest corner.
					cr := byte(0)
					if pr > 3 {
						cr = 7
					}
					cc := byte(0)
					if pc > 3 {
						cc = 7
					}
					cornerEmpty := board[cr*8+cc] == empty
					// Count empty neighbors (frontier metric: fewer = more stable).
					frontier := byte(0)
					for d := byte(0); d < 8; d++ {
						nr := pr + dx[d]
						nc := pc + dy[d]
						if nr < 8 && nc < 8 && board[nr*8+nc] == empty {
							frontier++
						}
					}
					interior := byte(8) - frontier
					// Position weight.
					isCorner := (pr == 0 || pr == 7) && (pc == 0 || pc == 7)
					isEdge := pr == 0 || pr == 7 || pc == 0 || pc == 7
					isXsq := (pr == 1 || pr == 6) && (pc == 1 || pc == 6)
					isCsq := ((pr == 0 || pr == 7) && (pc == 1 || pc == 6)) ||
						((pr == 1 || pr == 6) && (pc == 0 || pc == 7))
					w := byte(0)
					if isCorner {
						w = 120 + valid[pos]
					} else if isXsq {
						if cornerEmpty {
							w = 1
						} else {
							w = 40 + valid[pos]
						}
					} else if isCsq {
						if cornerEmpty {
							w = 2
						} else {
							w = 40 + valid[pos]
						}
					} else if isEdge {
						w = 25 + valid[pos] + interior
					} else {
						w = 10 + valid[pos] + interior
					}
					if w > bestScore {
						bestScore = w
						movePos = pos
					}
				}
			}
			print("O move: ")
			putchar('A' + movePos%8)
			putchar('1' + movePos/8)
			println()
			println()
		}

		// Apply move: place piece and flip.
		if movePos < 64 {
			board[movePos] = player
			pr := movePos / 8
			pc := movePos % 8
			for d := byte(0); d < 8; d++ {
				nr := pr + dx[d]
				nc := pc + dy[d]
				if nr < 8 && nc < 8 && board[nr*8+nc] == opp {
					count := byte(0)
					found := byte(0)
					done := byte(0)
					for nr < 8 && nc < 8 && done == 0 {
						v := board[nr*8+nc]
						if v == opp {
							count++
							nr += dx[d]
							nc += dy[d]
						} else {
							if v == player && count > 0 {
								found = count
							}
							done = 1
						}
					}
					if found > 0 {
						nr = pr + dx[d]
						nc = pc + dy[d]
						for found > 0 {
							board[nr*8+nc] = player
							nr += dx[d]
							nc += dy[d]
							found--
						}
					}
				}
			}
		}

		player = opponent(player)
	}

	// Final score.
	bc := byte(0)
	wc := byte(0)
	for i := range 64 {
		if board[i] == black {
			bc++
		} else if board[i] == white {
			wc++
		}
	}
	print("X:")
	print(bc)
	print(" O:")
	print(wc)
	if bc > wc {
		println(" - X wins!")
	} else if wc > bc {
		println(" - O wins!")
	} else {
		println(" - Draw!")
	}
}
