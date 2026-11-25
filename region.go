package ocrflow

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"image"
)

// RegionHash hashes the raw RGBA bytes of one text-line region without any
// floating-point or image-format dependence.
func RegionHash(shot *Screenshot, r Rect) string {
	if shot == nil || shot.RGBA == nil {
		return HashString("nil-region")
	}
	bounds := shot.RGBA.Bounds()
	rr := rectIntersection(r, Rect{MinX: bounds.Min.X, MinY: bounds.Min.Y, MaxX: bounds.Max.X, MaxY: bounds.Max.Y})
	h := sha256.New()
	_, _ = h.Write([]byte("region-rgba-v1\n"))
	var dims [8]byte
	binary.BigEndian.PutUint32(dims[0:4], uint32(rr.Width()))
	binary.BigEndian.PutUint32(dims[4:8], uint32(rr.Height()))
	_, _ = h.Write(dims[:])
	for y := rr.MinY; y < rr.MaxY; y++ {
		start := y*shot.RGBA.Stride + rr.MinX*4
		end := y*shot.RGBA.Stride + rr.MaxX*4
		_, _ = h.Write(shot.RGBA.Pix[start:end])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ReplaceRegionPixels replaces one line region and refreshes the screenshot
// content hash. The replacement is supplied by the caller/injection layer.
func ReplaceRegionPixels(shot *Screenshot, lineNo int, replacement *image.RGBA) error {
	if shot == nil || replacement == nil {
		return decodeError("region", nil, "screenshot and replacement are required")
	}
	var r Rect
	found := false
	for _, line := range shot.Lines {
		if line.LineNo == lineNo {
			r = line.Quad.Rect()
			found = true
			break
		}
	}
	if !found {
		return paramError("line_no", shot.Lines, "line %d not found in truth", lineNo)
	}
	r = Rect{MinX: r.MinX - 2, MinY: r.MinY - 2, MaxX: r.MaxX + 2, MaxY: r.MaxY + 2}
	for y := 0; y < replacement.Bounds().Dy() && r.MinY+y < shot.Height; y++ {
		for x := 0; x < replacement.Bounds().Dx() && r.MinX+x < shot.Width; x++ {
			shot.RGBA.SetRGBA(r.MinX+x, r.MinY+y, replacement.RGBAAt(x, y))
		}
	}
	shot.ContentHash = HashRGBA(shot.RGBA)
	return nil
}

// ToggleBackgroundBit is a deterministic in-place region mutation used by the
// precise invalidation demonstration. It changes raw content without changing
// visible OCR class.
func ToggleBackgroundBit(shot *Screenshot, det *DetectionResult, lineNo int) (Point, bool) {
	for _, line := range det.Lines {
		if line.LineNo != lineNo {
			continue
		}
		for y := line.Box.MinY; y < line.Box.MaxY; y++ {
			for x := line.Box.MinX; x < line.Box.MaxX; x++ {
				idx := (y*shot.Width + x) * 4
				if idx+3 >= len(shot.RGBA.Pix) {
					continue
				}
				gray := (299*int(shot.RGBA.Pix[idx]) + 587*int(shot.RGBA.Pix[idx+1]) + 114*int(shot.RGBA.Pix[idx+2])) / 1000
				if gray < 90 {
					shot.RGBA.Pix[idx+2] ^= 0x01
					shot.ContentHash = HashRGBA(shot.RGBA)
					return Point{X: x, Y: y}, true
				}
			}
		}
	}
	return Point{}, false
}
