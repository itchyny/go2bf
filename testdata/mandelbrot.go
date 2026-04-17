package main

// Signed fixed-point multiplication with scale 16 (16 units = 1.0).
// Double decomposition to avoid 8-bit overflow:
//
//	(a/16)*(b/16)*16 + (a/16)*(b%16) + (a%16)*(b/16) + (a%16)*(b%16)/16
func smul(a, b byte) byte {
	s := 0
	ma := a
	if a >= 128 {
		ma = 0 - a
		s = 1
	}
	mb := b
	if b >= 128 {
		mb = 0 - b
		s = s + 1
	}
	ah := ma / 16
	al := ma % 16
	bh := mb / 16
	bl := mb % 16
	r := ah*bh*16 + ah*bl + al*bh + al*bl/16
	if s == 1 {
		r = 0 - r
	}
	return r
}

func mandel(cr, ci byte) byte {
	zr := byte(0)
	zi := byte(0)
	n := byte(0)
	done := byte(0)
	for done == 0 {
		azr := zr
		if zr >= 128 {
			azr = 0 - zr
		}
		azi := zi
		if zi >= 128 {
			azi = 0 - zi
		}
		if azr > 32 {
			done = 1
		}
		if azi > 32 {
			done = 1
		}
		if done == 0 {
			zr2 := smul(zr, zr)
			zi2 := smul(zi, zi)
			zri := smul(zr, zi)
			zr = zr2 - zi2 + cr
			zi = zri + zri + ci
			n = n + 1
			if n >= 20 {
				done = 1
			}
		}
	}
	return n
}

func main() {
	// ci from -20 to 20 (21 rows), real -1.25 to 1.25
	for ci := byte(236); ci != 21; ci++ {
		// cr from -40 to 24 (65 columns), real -2.5 to 1.5
		for cr := byte(216); cr != 25; cr++ {
			n := mandel(cr, ci)
			if n >= 20 {
				print("#")
			} else if n >= 12 {
				print("%")
			} else if n >= 8 {
				print("+")
			} else if n >= 5 {
				print(":")
			} else if n >= 3 {
				print(".")
			} else {
				print(" ")
			}
		}
		print("\n")
	}
}
