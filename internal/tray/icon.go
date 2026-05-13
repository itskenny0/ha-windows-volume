package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
)

// buildIcon returns ICO-format bytes containing a single 32×32 PNG-encoded
// image of a speaker glyph in `fg` on transparent background.
//
// Windows accepts PNG-embedded ICOs since Vista; the systray library on
// Windows hands these straight to LoadIconFromBytes.
func buildIcon(fg color.RGBA, withSlash bool) []byte {
	const size = 32
	img := image.NewRGBA(image.Rect(0, 0, size, size))

	// Glyph: a trapezoidal speaker body on the left, three concentric arcs
	// on the right. The whole thing fits in a 28x28 bbox centered in 32x32.
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if inSpeakerGlyph(x, y) {
				img.SetRGBA(x, y, fg)
			} else if !withSlash && inSoundArcs(x, y) {
				img.SetRGBA(x, y, fg)
			}
		}
	}
	if withSlash {
		// Draw a thick diagonal red slash for the "disconnected" state.
		slash := color.RGBA{R: 0xff, G: 0x44, B: 0x44, A: 0xff}
		for i := 0; i < size; i++ {
			for d := -2; d <= 2; d++ {
				x, y := i+d, size-1-i
				if x >= 0 && x < size && y >= 0 && y < size {
					img.SetRGBA(x, y, slash)
				}
			}
		}
	}

	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, img)

	// ICONDIR (6 bytes) + ICONDIRENTRY (16 bytes) + PNG payload.
	var ico bytes.Buffer
	// ICONDIR: reserved, type=1 (ICO), count=1.
	binary.Write(&ico, binary.LittleEndian, uint16(0))
	binary.Write(&ico, binary.LittleEndian, uint16(1))
	binary.Write(&ico, binary.LittleEndian, uint16(1))
	// ICONDIRENTRY: width, height (0 for 256+), colorCount, reserved,
	// planes, bitCount, bytesInRes, imageOffset.
	binary.Write(&ico, binary.LittleEndian, uint8(size))
	binary.Write(&ico, binary.LittleEndian, uint8(size))
	binary.Write(&ico, binary.LittleEndian, uint8(0))
	binary.Write(&ico, binary.LittleEndian, uint8(0))
	binary.Write(&ico, binary.LittleEndian, uint16(1))
	binary.Write(&ico, binary.LittleEndian, uint16(32))
	binary.Write(&ico, binary.LittleEndian, uint32(pngBuf.Len()))
	binary.Write(&ico, binary.LittleEndian, uint32(22)) // 6 + 16
	ico.Write(pngBuf.Bytes())
	return ico.Bytes()
}

// inSpeakerGlyph: a stubby cone/trapezoid speaker on the left third.
// Coords are in 0..31; we keep the math arithmetic so we don't ship a
// bitmap.
func inSpeakerGlyph(x, y int) bool {
	// rear rectangle: x in [4,10], y in [12,19]
	if x >= 4 && x <= 10 && y >= 12 && y <= 19 {
		return true
	}
	// flaring cone: triangle from (10,7) to (18,4) to (18,27) to (10,24).
	// Approximate with two linear bounds in x∈[10,18].
	if x >= 10 && x <= 18 {
		// top edge: y = 7 + (4-7)*(x-10)/8 = 7 - 3*(x-10)/8
		top := 7 - (3*(x-10))/8
		// bottom edge: y = 24 + (27-24)*(x-10)/8 = 24 + 3*(x-10)/8
		bot := 24 + (3*(x-10))/8
		if y >= top && y <= bot {
			return true
		}
	}
	return false
}

// inSoundArcs draws three nested arcs on the right of the speaker.
func inSoundArcs(x, y int) bool {
	cx, cy := 18, 16
	dx, dy := x-cx, y-cy
	// We only care about the right half.
	if dx < 2 {
		return false
	}
	d2 := dx*dx + dy*dy
	hits := func(r int) bool { return d2 >= (r-1)*(r-1) && d2 <= r*r }
	return hits(6) || hits(10) || hits(14)
}
