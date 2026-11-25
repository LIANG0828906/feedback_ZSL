package maskqa

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
)

func EncodePNGBase64(img image.Image) (string, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func DecodePNGBase64(s string) (image.Image, []byte, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, nil, schemaError("", "image_png_base64", "invalid base64: %v", err)
	}
	img, err := DecodeImageBytes(raw)
	return img, raw, err
}

// DecodeImageBytes decodes PNG bytes and maps truncation/corruption to E_DECODE.
func DecodeImageBytes(raw []byte) (image.Image, error) {
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, &Error{Code: CodeDecode, Field: "image_png", Path: "image_png", Message: "image decode failed", Cause: err}
	}
	return img, nil
}

func EncodeMaskPNG(m *Mask) ([]byte, error) {
	img := image.NewGray(image.Rect(0, 0, m.Width, m.Height))
	for i, v := range m.Pix {
		if v != 0 {
			img.Pix[i] = 255
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func EncodeMaskBase64(m *Mask) (string, error) {
	b, err := EncodeMaskPNG(m)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func DecodeMaskPNG(raw []byte) (*Mask, error) {
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, &Error{Code: CodeDecode, Field: "candidate_mask_png", Path: "candidate_mask_png", Message: "mask png decode failed", Cause: err}
	}
	b := img.Bounds()
	mask := NewMask(b.Dx(), b.Dy())
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			g := color.GrayModel.Convert(img.At(x, y)).(color.Gray)
			mask.Set(x-b.Min.X, y-b.Min.Y, g.Y >= 128)
		}
	}
	return mask, nil
}

func DecodeMaskBase64(s string) (*Mask, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, schemaError("", "candidate_mask_png_base64", "invalid base64: %v", err)
	}
	return DecodeMaskPNG(raw)
}

// EncodeMaskPGM is a dependency-free diagnostic encoding useful in tests/CLI.
func EncodeMaskPGM(m *Mask) []byte {
	var buf bytes.Buffer
	buf.WriteString("P5\n")
	buf.WriteString(itoa(m.Width))
	buf.WriteByte(' ')
	buf.WriteString(itoa(m.Height))
	buf.WriteString("\n255\n")
	for _, v := range m.Pix {
		if v != 0 {
			buf.WriteByte(255)
		} else {
			buf.WriteByte(0)
		}
	}
	return buf.Bytes()
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
