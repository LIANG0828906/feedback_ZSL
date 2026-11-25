package channelpipe

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

func WritePNGPreview(path string, buffer *Buffer) error {
	if buffer == nil {
		return FieldError(CodeInvalid, "buffer", "cannot write preview of nil buffer")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return WrapError(CodeIO, err, path, "cannot create preview directory")
	}
	file, err := os.Create(path)
	if err != nil {
		return WrapError(CodeIO, err, path, "cannot create PNG preview")
	}
	defer file.Close()

	rect := image.Rect(0, 0, buffer.Width, buffer.Height)
	switch buffer.Depth {
	case 8:
		img := image.NewNRGBA(rect)
		for y := 0; y < buffer.Height; y++ {
			for x := 0; x < buffer.Width; x++ {
				r, g, b, a := previewColors(buffer, x, y, 255)
				img.SetNRGBA(x, y, color.NRGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: uint8(a)})
			}
		}
		if err := png.Encode(file, img); err != nil {
			return WrapError(CodeIO, err, path, "cannot encode 8-bit PNG preview")
		}
	case 16:
		img := image.NewNRGBA64(rect)
		for y := 0; y < buffer.Height; y++ {
			for x := 0; x < buffer.Width; x++ {
				r, g, b, a := previewColors(buffer, x, y, 65535)
				img.SetNRGBA64(x, y, color.NRGBA64{R: r, G: g, B: b, A: a})
			}
		}
		if err := png.Encode(file, img); err != nil {
			return WrapError(CodeIO, err, path, "cannot encode 16-bit PNG preview")
		}
	default:
		return FieldError(CodeInvalid, "depth", "PNG preview supports depth 8 or 16, got %d", buffer.Depth)
	}
	return nil
}

func previewColors(buffer *Buffer, x, y int, maxValue uint16) (uint16, uint16, uint16, uint16) {
	base := (y*buffer.Width + x) * buffer.ChannelCount()
	values := map[Channel]uint16{}
	for i, channel := range buffer.Channels {
		values[channel] = buffer.Pixels[base+i]
	}
	r, ok := values[ChannelR]
	if !ok {
		r = 0
	}
	g, ok := values[ChannelG]
	if !ok {
		g = 0
	}
	b, ok := values[ChannelB]
	if !ok {
		b = 0
	}
	a, ok := values[ChannelA]
	if !ok {
		a = maxValue
	}
	return r, g, b, a
}
