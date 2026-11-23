package tex

import (
	"errors"
	"testing"
)

func mustImage(t *testing.T, width, height, depth int, layout Layout, pixels []uint16) *Image {
	t.Helper()
	img, err := NewImage("test", width, height, depth, layout, pixels)
	if err != nil {
		t.Fatalf("NewImage: %v", err)
	}
	return img
}

func requireErrorCode(t *testing.T, err error, code string) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	var texErr *Error
	if !errors.As(err, &texErr) {
		t.Fatalf("expected *tex.Error, got %T: %v", err, err)
	}
	if texErr.Code != code {
		t.Fatalf("expected code %s, got %s (%v)", code, texErr.Code, err)
	}
	return texErr
}

func TestDescriptorValidationErrors(t *testing.T) {
	valid := Descriptor{
		ID:             "valid",
		SourceLayout:   LayoutRGB,
		TargetLayout:   LayoutRGBA,
		TargetBitDepth: 8,
		Permutation:    []int{0, 1, 2},
		Fill:           FillRepeatAlpha,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid descriptor rejected: %v", err)
	}

	tests := []struct {
		name  string
		edit  func(*Descriptor)
		code  string
		field string
		index *int
	}{
		{"missing depth", func(d *Descriptor) { d.TargetBitDepth = 0 }, CodePara, "target_bit_depth", nil},
		{"bad depth", func(d *Descriptor) { d.TargetBitDepth = 12 }, CodePara, "target_bit_depth", nil},
		{"missing source layout", func(d *Descriptor) { d.SourceLayout = "" }, CodePara, "source_layout", nil},
		{"bad fill", func(d *Descriptor) { d.Fill = "mirror" }, CodePara, "fill", nil},
		{"repeat alpha without alpha", func(d *Descriptor) { d.TargetLayout = LayoutRGB }, CodePara, "fill", nil},
		{"duplicate channel", func(d *Descriptor) { d.Permutation = []int{0, 0, 2} }, CodeLayout, "permutation", intPtr(0)},
		{"out of range channel", func(d *Descriptor) { d.Permutation = []int{0, 1, 9} }, CodeLayout, "permutation", intPtr(9)},
		{"wrong permutation length", func(d *Descriptor) { d.Permutation = []int{0, 1} }, CodeLayout, "permutation", intPtr(-1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			desc := valid
			desc.Permutation = append([]int(nil), valid.Permutation...)
			tc.edit(&desc)
			err := desc.Validate()
			texErr := requireErrorCode(t, err, tc.code)
			if texErr.Field != tc.field {
				t.Fatalf("field=%q, want %q", texErr.Field, tc.field)
			}
			if tc.index != nil {
				if texErr.ChannelIndex == nil || *texErr.ChannelIndex != *tc.index {
					t.Fatalf("channel_index=%v, want %d", texErr.ChannelIndex, *tc.index)
				}
			}
		})
	}
}

func intPtr(value int) *int {
	return &value
}

func TestConvertRGBToBGRA16AndQuantization(t *testing.T) {
	img := mustImage(t, 2, 1, 8, LayoutRGB, []uint16{10, 20, 30, 200, 100, 50})
	desc := Descriptor{
		SourceLayout:   LayoutRGB,
		TargetLayout:   LayoutBGRA,
		TargetBitDepth: 16,
		Permutation:    []int{2, 1, 0},
		Fill:           FillZero,
	}
	result, err := ConvertFull(img, desc, ConvertOptions{})
	if err != nil {
		t.Fatalf("ConvertFull: %v", err)
	}
	want := []uint16{30 * 257, 20 * 257, 10 * 257, 0, 50 * 257, 100 * 257, 200 * 257, 0}
	if len(result.Pixels) != len(want) {
		t.Fatalf("pixel count=%d, want %d", len(result.Pixels), len(want))
	}
	for i := range want {
		if result.Pixels[i] != want[i] {
			t.Fatalf("pixel[%d]=%d, want %d", i, result.Pixels[i], want[i])
		}
	}
	if result.Width != img.Width || result.Height != img.Height || result.BitDepth != 16 || result.Layout != LayoutBGRA {
		t.Fatalf("unexpected output shape: %+v", result)
	}
	if result.QuantStats.MaxAbs != 0 || result.QuantStats.NonZero != 0 {
		t.Fatalf("8->16 quantization should be exact, got %+v", result.QuantStats)
	}
}

func TestDescriptorEnumNormalization(t *testing.T) {
	img := mustImage(t, 1, 1, 8, LayoutRGB, []uint16{1, 2, 3})
	desc := Descriptor{
		SourceLayout:   "RGB",
		TargetLayout:   "RGBA",
		TargetBitDepth: 8,
		Permutation:    []int{0, 1, 2},
		Fill:           "REPEAT_ALPHA",
	}
	result, err := ConvertFull(img, desc, ConvertOptions{})
	if err != nil {
		t.Fatalf("uppercase descriptor should normalize: %v", err)
	}
	if result.Layout != LayoutRGBA || result.Pixels[3] != 3 {
		t.Fatalf("uppercase descriptor conversion failed: %+v", result.Pixels)
	}
}

func TestRepeatAlphaAndInverseMapping(t *testing.T) {
	img := mustImage(t, 1, 1, 8, LayoutRGB, []uint16{12, 34, 56})
	desc := Descriptor{
		SourceLayout:   LayoutRGB,
		TargetLayout:   LayoutRGBA,
		TargetBitDepth: 8,
		Permutation:    []int{0, 1, 2},
		Fill:           FillRepeatAlpha,
	}
	result, err := ConvertFull(img, desc, ConvertOptions{})
	if err != nil {
		t.Fatalf("ConvertFull: %v", err)
	}
	if result.Pixels[3] != 56 {
		t.Fatalf("repeat alpha=%d, want nearest source channel B=56", result.Pixels[3])
	}
	if result.Mapping.AlphaSource == nil || result.Mapping.AlphaSource.Index != 2 || result.Mapping.AlphaSource.Name != "B" {
		t.Fatalf("alpha source relation=%+v, want B(src[2])", result.Mapping.AlphaSource)
	}
	recovered, err := result.Mapping.SourceFromTarget()
	if err != nil {
		t.Fatalf("SourceFromTarget: %v", err)
	}
	want := []int{0, 1, 2}
	for i := range want {
		if recovered[i] != want[i] {
			t.Fatalf("source channel %d comes from target %d, want %d", i, recovered[i], want[i])
		}
	}
}

func TestChannelDropReturnsLoss(t *testing.T) {
	img := mustImage(t, 1, 1, 8, LayoutRGBA, []uint16{1, 2, 3, 4})
	desc := Descriptor{
		SourceLayout:   LayoutRGBA,
		TargetLayout:   LayoutRGB,
		TargetBitDepth: 8,
		Permutation:    []int{0, 1, 2, 3},
		Fill:           FillZero,
	}
	result, err := ConvertFull(img, desc, ConvertOptions{})
	if err != nil {
		t.Fatalf("ConvertFull: %v", err)
	}
	if len(result.Mapping.Dropped) != 1 || result.Mapping.Dropped[0].Index != 3 || result.Mapping.Dropped[0].Name != "A" {
		t.Fatalf("dropped channels=%+v, want A(src[3])", result.Mapping.Dropped)
	}
	_, err = result.Mapping.SourceFromTarget()
	texErr := requireErrorCode(t, err, CodeLoss)
	if len(texErr.Dropped) != 1 || texErr.Dropped[0].Name != "A" {
		t.Fatalf("E_LOSS dropped=%+v, want A", texErr.Dropped)
	}
}

func TestQuantizationErrorGrowsWhenBitDepthDrops(t *testing.T) {
	values := []uint16{0, 1, 127, 128, 32767, 32768, 65434, 65535}
	img16 := mustImage(t, len(values), 1, 16, LayoutGray, values)
	desc8 := Descriptor{SourceLayout: LayoutGray, TargetLayout: LayoutGray, TargetBitDepth: 8, Permutation: []int{0}, Fill: FillZero}
	result8, err := ConvertFull(img16, desc8, ConvertOptions{})
	if err != nil {
		t.Fatalf("ConvertFull 16->8: %v", err)
	}
	if result8.QuantStats.MeanAbs <= 0 || result8.QuantStats.MaxAbs == 0 {
		t.Fatalf("16->8 should record quantization error, got %+v", result8.QuantStats)
	}
	values8 := make([]uint16, len(values))
	for i, value := range values {
		values8[i] = value % 256
	}
	img8 := mustImage(t, len(values8), 1, 8, LayoutGray, values8)
	descSame := desc8
	result8Same, err := ConvertFull(img8, descSame, ConvertOptions{})
	if err != nil {
		t.Fatalf("ConvertFull 8->8: %v", err)
	}
	if result8Same.QuantStats.MeanAbs != 0 {
		t.Fatalf("8->8 should have no quantization error, got %+v", result8Same.QuantStats)
	}
	if result8.QuantStats.MeanAbs <= result8Same.QuantStats.MeanAbs {
		t.Fatalf("16->8 mean error %f should exceed 8->8 %f", result8.QuantStats.MeanAbs, result8Same.QuantStats.MeanAbs)
	}
}

func TestHistogramAndDimensions(t *testing.T) {
	img := mustImage(t, 2, 2, 8, LayoutRGB, []uint16{
		0, 1, 2,
		3, 4, 5,
		0, 1, 2,
		3, 4, 5,
	})
	desc := Descriptor{SourceLayout: LayoutRGB, TargetLayout: LayoutBGR, TargetBitDepth: 8, Permutation: []int{2, 1, 0}, Fill: FillZero}
	result, err := ConvertFull(img, desc, ConvertOptions{})
	if err != nil {
		t.Fatalf("ConvertFull: %v", err)
	}
	if result.Width != 2 || result.Height != 2 || len(result.Pixels) != 12 {
		t.Fatalf("unexpected dimensions/samples: %dx%d samples=%d", result.Width, result.Height, len(result.Pixels))
	}
	if len(result.Histogram.Counts) != 3 {
		t.Fatalf("histogram channels=%d, want 3", len(result.Histogram.Counts))
	}
	for c, counts := range result.Histogram.Counts {
		var total uint64
		for _, count := range counts {
			total += count
		}
		if total != 4 {
			t.Fatalf("channel %d histogram total=%d, want 4", c, total)
		}
	}
	if result.Pixels[0] != 2 || result.Pixels[2] != 0 {
		t.Fatalf("BGR reorder failed: %v", result.Pixels[:3])
	}
}
