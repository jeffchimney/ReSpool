package server

import (
	"bytes"
	_ "embed"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

// markMaskPNG is the "foreground" group of mark.svg exported on the full
// transparent page at 600×600, so generated covers use the same mark.
//
//go:embed mark-mask.png
var markMaskPNG []byte

var markMask = func() *image.Alpha {
	mark, err := png.Decode(bytes.NewReader(markMaskPNG))
	if err != nil {
		panic(err)
	}
	mask := image.NewAlpha(mark.Bounds())
	draw.Draw(mask, mask.Bounds(), mark, mark.Bounds().Min, draw.Src)
	return mask
}()

// Spool's four tag colours, which it uses to give each show a stable accent.
var tagColors = []color.RGBA{
	{0x27, 0x43, 0x9B, 0xFF}, // blue
	{0x3C, 0x60, 0x5E, 0xFF}, // green
	{0x96, 0x42, 0x3E, 0xFF}, // rust
	{0xC9, 0x88, 0x35, 0xFF}, // amber
}

// tagColor picks a colour the way Spool's Theme.tagColor(for:) does: a 32-bit
// FNV-1a hash of the seed, modulo the palette.
func tagColor(seed string) color.RGBA {
	h := uint32(2166136261)
	for i := 0; i < len(seed); i++ {
		h ^= uint32(seed[i])
		h *= 16777619
	}
	return tagColors[h%uint32(len(tagColors))]
}

// placeholderCover draws artwork for books that have none: Spool's cassette
// and an RSS glyph in white on a gradient of the book's tag colour.
func placeholderCover(seed string) []byte {
	const size = 600
	base := tagColor(seed)
	white := color.RGBA{255, 255, 255, 255}
	top := lerp(base, white, 0.16)
	bottom := lerp(base, color.RGBA{0, 0, 0, 255}, 0.12)

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			px := lerp(top, bottom, (float64(x)+float64(y))/(2*size))
			if a := markMask.AlphaAt(x, y).A; a > 0 {
				px = lerp(px, white, 0.94*float64(a)/255)
			}
			img.SetRGBA(x, y, px)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func lerp(a, b color.RGBA, t float64) color.RGBA {
	mix := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t + 0.5) }
	return color.RGBA{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B), 255}
}
