package gtex

import (
	"os"
	"path/filepath"
	"testing"

	"texlab/internal/apperr"
	"texlab/internal/model"
)

func TestWriteReadLayoutsAndDepths(t *testing.T) {
	for _, tc := range []struct {
		depth  int
		layout string
		max    int
	}{
		{8, "RGBA", 255}, {8, "BGRA", 255}, {16, "RGBA", 65535}, {16, "BGRA", 65535},
	} {
		im := model.NewImage(2, 2, tc.depth, 4, tc.layout, "straight")
		im.SetSemanticPixel(0, 0, [4]uint16{1, 2, 3, uint16(tc.max)})
		im.SetSemanticPixel(1, 0, [4]uint16{uint16(tc.max), 0, 7, 9})
		path := filepath.Join(t.TempDir(), "x.gtex")
		if err := Write(path, im); err != nil {
			t.Fatal(err)
		}
		got, header, err := Read(path)
		if err != nil {
			t.Fatal(err)
		}
		if header.Depth != tc.depth || header.Layout != tc.layout {
			t.Fatalf("header = %+v", header)
		}
		if got.SemanticPixel(0, 0) != [4]uint16{1, 2, 3, uint16(tc.max)} {
			t.Fatalf("pixel roundtrip failed: %v", got.SemanticPixel(0, 0))
		}
	}
}

func TestDecodeCorruption(t *testing.T) {
	im := model.NewImage(4, 4, 8, 4, "RGBA", "straight")
	for i := range im.Pixels {
		im.Pixels[i] = uint16((i * 7) % 256)
	}
	path := filepath.Join(t.TempDir(), "x.gtex")
	if err := Write(path, im); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	raw[len(raw)-1] ^= 0xff
	if _, _, err := Decode(raw); !apperr.IsCode(err, apperr.EDecode) {
		t.Fatalf("want E_DECODE, got %v", err)
	}
}
