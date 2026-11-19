package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"runtime"
)

// iconState selects the tray icon.
type iconState int

const (
	iconIdle    iconState = iota // up to date
	iconBusy                     // uploading / files waiting
	iconPaused                   // paused by the user
	iconOffline                  // server unreachable
	iconProblem                  // needs attention (no key, no folders, ...)
)

var iconColors = map[iconState]color.RGBA{
	iconIdle:    {0x1f, 0x9d, 0x7a, 0xff}, // green
	iconBusy:    {0x2f, 0x6f, 0xde, 0xff}, // blue
	iconPaused:  {0x8a, 0x8f, 0x98, 0xff}, // grey
	iconOffline: {0x5b, 0x63, 0x70, 0xff}, // dark grey
	iconProblem: {0xd9, 0x77, 0x06, 0xff}, // amber
}

// iconBytes returns the icon in the format the platform's tray expects:
// ICO on Windows, PNG elsewhere.
func iconBytes(s iconState) []byte {
	if runtime.GOOS == "windows" {
		return ico(s, 16, 20, 24, 32, 48)
	}
	return pngBytes(render(s, 64))
}

// AppIconICO is the application icon for the Windows executable, at the
// sizes Explorer and the taskbar use.
func AppIconICO() []byte { return ico(iconIdle, 16, 20, 24, 32, 40, 48, 64, 256) }

// render draws the icon at size×size with 4×4 supersampling for smooth edges.
// Shapes are defined on a 0..1 unit square.
func render(s iconState, size int) *image.NRGBA {
	bg := iconColors[s]
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var bgHits, fgHits int
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) / float64(size)
					y := (float64(py) + (float64(sy)+0.5)/ss) / float64(size)
					if !roundedRect(x, y, 0.04, 0.04, 0.96, 0.96, 0.22) {
						continue
					}
					if glyph(s, x, y) {
						fgHits++
					} else {
						bgHits++
					}
				}
			}
			total := float64(ss * ss)
			a := float64(bgHits+fgHits) / total
			if a == 0 {
				continue
			}
			// Mix background and white glyph by coverage.
			f := float64(fgHits) / float64(bgHits+fgHits)
			mix := func(c uint8) uint8 { return uint8(float64(c)*(1-f) + 255*f + 0.5) }
			img.SetNRGBA(px, py, color.NRGBA{mix(bg.R), mix(bg.G), mix(bg.B), uint8(a*255 + 0.5)})
		}
	}
	return img
}

func glyph(s iconState, x, y float64) bool {
	switch s {
	case iconPaused:
		return rect(x, y, 0.32, 0.27, 0.44, 0.73) || rect(x, y, 0.56, 0.27, 0.68, 0.73)
	case iconProblem:
		return rect(x, y, 0.44, 0.22, 0.56, 0.60) || rect(x, y, 0.44, 0.68, 0.56, 0.79)
	default: // up arrow over a base line: "upload"
		arrowHead := y >= 0.20 && y <= 0.50 && absf(x-0.5) <= (y-0.20)*0.95
		shaft := rect(x, y, 0.43, 0.48, 0.57, 0.68)
		base := rect(x, y, 0.27, 0.73, 0.73, 0.81)
		g := arrowHead || shaft || base
		if s == iconOffline {
			// Diagonal slash through the glyph area.
			if absf((x-0.5)-(y-0.5)) < 0.06 && rect(x, y, 0.2, 0.2, 0.8, 0.8) {
				return true
			}
		}
		return g
	}
}

func rect(x, y, x0, y0, x1, y1 float64) bool { return x >= x0 && x <= x1 && y >= y0 && y <= y1 }

func roundedRect(x, y, x0, y0, x1, y1, r float64) bool {
	if !rect(x, y, x0, y0, x1, y1) {
		return false
	}
	cx := clamp(x, x0+r, x1-r)
	cy := clamp(y, y0+r, y1-r)
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func pngBytes(img image.Image) []byte {
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

// ico packs PNG images of the given sizes into a .ico file (PNG-in-ICO is
// supported since Windows Vista).
func ico(s iconState, sizes ...int) []byte {
	var imgs [][]byte
	for _, sz := range sizes {
		imgs = append(imgs, pngBytes(render(s, sz)))
	}
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, [3]uint16{0, 1, uint16(len(imgs))})
	offset := 6 + 16*len(imgs)
	for i, sz := range sizes {
		dim := uint8(sz)
		if sz >= 256 {
			dim = 0
		}
		b.Write([]byte{dim, dim, 0, 0})
		binary.Write(&b, binary.LittleEndian, [2]uint16{1, 32})
		binary.Write(&b, binary.LittleEndian, [2]uint32{uint32(len(imgs[i])), uint32(offset)})
		offset += len(imgs[i])
	}
	for _, p := range imgs {
		b.Write(p)
	}
	return b.Bytes()
}
