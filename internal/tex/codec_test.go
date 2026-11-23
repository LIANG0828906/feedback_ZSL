package tex

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestImageCodecRoundTripAndCorruption(t *testing.T) {
	img := mustImage(t, 2, 2, 16, LayoutBGRA, []uint16{
		1, 2, 3, 4,
		5, 6, 7, 8,
		9, 10, 11, 12,
		13, 14, 15, 16,
	})
	var buffer bytes.Buffer
	if err := EncodeImage(&buffer, img); err != nil {
		t.Fatalf("EncodeImage: %v", err)
	}
	decoded, err := DecodeImage(bytes.NewReader(buffer.Bytes()))
	if err != nil {
		t.Fatalf("DecodeImage: %v", err)
	}
	if decoded.ContentHash != img.ContentHash || len(decoded.Pixels) != len(img.Pixels) {
		t.Fatalf("decoded image differs from source")
	}
	for i := range img.Pixels {
		if decoded.Pixels[i] != img.Pixels[i] {
			t.Fatalf("decoded pixel %d differs", i)
		}
	}
	corrupt := append([]byte(nil), buffer.Bytes()...)
	corrupt[0] = 'X'
	_, err = DecodeImage(bytes.NewReader(corrupt))
	var texErr *Error
	if !errors.As(err, &texErr) || texErr.Code != CodeDecode {
		t.Fatalf("corrupt image should return E_DECODE, got %v", err)
	}
	truncated := buffer.Bytes()[:len(buffer.Bytes())-1]
	_, err = DecodeImage(bytes.NewReader(truncated))
	if !errors.As(err, &texErr) || texErr.Code != CodeDecode {
		t.Fatalf("truncated image should return E_DECODE, got %v", err)
	}
	withTrailingZero := append(append([]byte(nil), buffer.Bytes()...), 0)
	_, err = DecodeImage(bytes.NewReader(withTrailingZero))
	if !errors.As(err, &texErr) || texErr.Code != CodeDecode {
		t.Fatalf("trailing byte should return E_DECODE, got %v", err)
	}
}

func TestDecodeImageFileDetectsPNG(t *testing.T) {
	data := &Converted{
		Width:    2,
		Height:   1,
		BitDepth: 8,
		Layout:   LayoutRGB,
		Pixels:   []uint16{255, 0, 0, 0, 255, 0},
	}
	path := filepath.Join(t.TempDir(), "red-green.png")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := EncodePNG(file, data, false); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	img, err := DecodeImageFile(path)
	if err != nil {
		t.Fatalf("DecodeImageFile PNG: %v", err)
	}
	if img.Width != 2 || img.Height != 1 || img.Layout != LayoutRGBA || img.BitDepth != 8 {
		t.Fatalf("unexpected decoded PNG: %+v", img)
	}
	if img.Pixels[3] != 255 {
		t.Fatalf("opaque PNG alpha should be restored as 255, got %d", img.Pixels[3])
	}
}
