package compose

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"

	"virtualstudio/model"
)

type Heatmap struct {
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	MaxDiff    uint8   `json:"max_diff"`
	MeanDiff   float64 `json:"mean_diff"`
	Mismatches int     `json:"mismatches"`
	PixelDiff  []uint8 `json:"-"`
	PNG        []byte  `json:"-"`
}

func FramePNG(frame model.Frame) ([]byte, error) {
	if frame.Width <= 0 || frame.Height <= 0 || frame.Channels != 4 {
		return nil, model.NewError(model.CodeParam, "frame", "invalid frame dimensions")
	}
	if len(frame.RGBA) != frame.Width*frame.Height*4 {
		return nil, model.NewError(model.CodeDecode, "frame.rgba", "length mismatch")
	}
	img := image.NewNRGBA(image.Rect(0, 0, frame.Width, frame.Height))
	copy(img.Pix, frame.RGBA)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, model.WrapError(model.CodeIO, "png", "encode frame", err)
	}
	return buf.Bytes(), nil
}

func DiffHeatmap(a, b model.Frame) (*Heatmap, error) {
	if a.Width != b.Width || a.Height != b.Height || a.Channels != 4 || b.Channels != 4 {
		return nil, model.NewError(model.CodeCompare, "frames", "frame dimensions differ")
	}
	if len(a.RGBA) != a.Width*a.Height*4 || len(b.RGBA) != b.Width*b.Height*4 {
		return nil, model.NewError(model.CodeDecode, "frames.rgba", "pixel buffer length mismatch")
	}
	hm := &Heatmap{Width: a.Width, Height: a.Height, PixelDiff: make([]uint8, a.Width*a.Height)}
	var sum uint64
	for p := 0; p < a.Width*a.Height; p++ {
		off := p * 4
		var d uint8
		for c := 0; c < 4; c++ {
			delta := int(a.RGBA[off+c]) - int(b.RGBA[off+c])
			if delta < 0 {
				delta = -delta
			}
			if uint8(delta) > d {
				d = uint8(delta)
			}
		}
		hm.PixelDiff[p] = d
		sum += uint64(d)
		if d > 0 {
			hm.Mismatches++
		}
		if d > hm.MaxDiff {
			hm.MaxDiff = d
		}
	}
	hm.MeanDiff = float64(sum) / float64(len(hm.PixelDiff))
	img := image.NewNRGBA(image.Rect(0, 0, hm.Width, hm.Height))
	for y := 0; y < hm.Height; y++ {
		for x := 0; x < hm.Width; x++ {
			d := hm.PixelDiff[y*hm.Width+x]
			// Heat ramp: black -> red -> yellow -> white.
			t := float64(d) / 255.0
			r := uint8(math.Round(255 * clamp01(t*2)))
			g := uint8(math.Round(255 * clamp01(t*2-1)))
			bv := uint8(math.Round(255 * clamp01(t*2-1)))
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: bv, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, model.WrapError(model.CodeIO, "heatmap.png", "encode heatmap", err)
	}
	hm.PNG = buf.Bytes()
	return hm, nil
}

func ASCII(frame model.Frame, maxWidth, maxHeight int) string {
	if frame.Width <= 0 || frame.Height <= 0 || frame.Channels != 4 || maxWidth <= 0 || maxHeight <= 0 {
		return ""
	}
	ramp := []byte(" .:-=+*#%@")
	sx := int(math.Ceil(float64(frame.Width) / float64(maxWidth)))
	sy := int(math.Ceil(float64(frame.Height) / float64(maxHeight)))
	if sx < 1 {
		sx = 1
	}
	if sy < 1 {
		sy = 1
	}
	var buf bytes.Buffer
	for y := 0; y < frame.Height; y += sy {
		for x := 0; x < frame.Width; x += sx {
			off := (y*frame.Width + x) * 4
			luma := 0.2126*float64(frame.RGBA[off]) + 0.7152*float64(frame.RGBA[off+1]) + 0.0722*float64(frame.RGBA[off+2])
			idx := int(luma / 256.0 * float64(len(ramp)))
			if idx >= len(ramp) {
				idx = len(ramp) - 1
			}
			buf.WriteByte(ramp[idx])
		}
		buf.WriteByte('\n')
	}
	return buf.String()
}

func ASCIIHeatmap(hm *Heatmap, maxWidth, maxHeight int) string {
	if hm == nil || maxWidth <= 0 || maxHeight <= 0 {
		return ""
	}
	ramp := []byte(" .:-=+*#%@")
	sx := int(math.Ceil(float64(hm.Width) / float64(maxWidth)))
	sy := int(math.Ceil(float64(hm.Height) / float64(maxHeight)))
	if sx < 1 {
		sx = 1
	}
	if sy < 1 {
		sy = 1
	}
	var buf bytes.Buffer
	for y := 0; y < hm.Height; y += sy {
		for x := 0; x < hm.Width; x += sx {
			d := hm.PixelDiff[y*hm.Width+x]
			idx := int(float64(d) / 256.0 * float64(len(ramp)))
			if idx >= len(ramp) {
				idx = len(ramp) - 1
			}
			buf.WriteByte(ramp[idx])
		}
		buf.WriteByte('\n')
	}
	return buf.String()
}

func DescribeDiff(hm *Heatmap) string {
	if hm == nil {
		return "no diff"
	}
	return fmt.Sprintf("max=%d mean=%.4f mismatches=%d", hm.MaxDiff, hm.MeanDiff, hm.Mismatches)
}
