package codec

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"mediaforge/internal/domain"
	"mediaforge/internal/errs"
	"mediaforge/internal/mfxfmt"
)

func TestFineAndAdaptivePathsAreByteIdentical(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "source.png")
	img := image.NewNRGBA(image.Rect(0, 0, 97, 61))
	for y := 0; y < 61; y++ {
		for x := 0; x < 97; x++ {
			i := y*img.Stride + x*4
			img.Pix[i] = byte(x*7 + y*3)
			img.Pix[i+1] = byte(x*11 + y*5)
			img.Pix[i+2] = byte(x*13 + y*17)
			img.Pix[i+3] = byte(210 + (x+y)%46)
		}
	}
	f, err := os.Create(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	wavPath := filepath.Join(dir, "source.wav")
	samples := make([]int16, 2048*2)
	for i := range samples {
		samples[i] = int16((i*37)%60000 - 30000)
	}
	wav, err := EncodeWAVPCM16(44100, 2, samples)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wavPath, wav, 0o644); err != nil {
		t.Fatal(err)
	}
	recipe := domain.Recipe{
		TargetWidth: 53, TargetHeight: 41, ImageChannels: 4, ImageBitDepth: 10,
		ChromaSampling: "4:2:0", AudioSampleRate: 48000, AudioBitDepth: 16,
		MetadataPolicy: "preserve",
	}
	in := Input{
		ImagePath: imagePath, AudioPath: wavPath, ImageChannels: 4, AudioChannels: 2,
		ImageMetadata: map[string]string{"title": "hero", "author": "test"},
		AudioMetadata: map[string]string{"audio.title": "pulse", "license": "CC0"},
	}
	fine1, err := Transcode(in, recipe, Options{Path: PathFine})
	if err != nil {
		t.Fatal(err)
	}
	fine2, err := Transcode(in, recipe, Options{Path: PathFine})
	if err != nil {
		t.Fatal(err)
	}
	adaptive, err := Transcode(in, recipe, Options{
		Path: PathAdaptive, BlockRows: 7, DownsampleFactor: 4,
		MaxPixelsPerWorker: 10000, MaxSamplesPerWorker: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fine1.Bytes, fine2.Bytes) {
		t.Fatal("repeated fine transcodes are not byte-identical")
	}
	if !bytes.Equal(fine1.Bytes, adaptive.Bytes) {
		t.Fatal("fine and adaptive paths are not byte-identical")
	}
	if adaptive.Preview == nil || adaptive.Preview.Downsample != 4 ||
		adaptive.Preview.Width != (53+3)/4 || adaptive.Preview.Height != (41+3)/4 {
		t.Fatalf("adaptive preview metadata is missing or wrong: %+v", adaptive.Preview)
	}
	if fine1.Preview != nil {
		t.Fatal("fine path should not emit a preview")
	}
	if fine1.Header.Width != 53 || fine1.Header.Height != 41 || fine1.Header.ImageChannels != 4 {
		t.Fatalf("unexpected header: %+v", fine1.Header)
	}
	if fine1.Header.AudioSamples != 2229 {
		t.Fatalf("audio sample count = %d, want 2229", fine1.Header.AudioSamples)
	}
	if _, err := CompareProducts(fine1.Bytes, adaptive.Bytes, domain.Consistency{
		RequireByteIdentical: true,
	}); err != nil {
		t.Fatalf("compare products: %v", err)
	}
	pkg, err := mfxfmt.Parse(fine1.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Header.MetadataFields[0] != "audio.title" || pkg.Header.MetadataFields[3] != "title" {
		t.Fatalf("metadata fields are not canonical: %v", pkg.Header.MetadataFields)
	}
}

func TestCompareProductsReturnsEScale(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "source.png")
	img := image.NewNRGBA(image.Rect(0, 0, 24, 16))
	for i := range img.Pix {
		img.Pix[i] = byte(i % 251)
	}
	f, _ := os.Create(imagePath)
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	f.Close()
	samples := make([]int16, 400)
	wav, _ := EncodeWAVPCM16(8000, 1, samples)
	wavPath := filepath.Join(dir, "source.wav")
	if err := os.WriteFile(wavPath, wav, 0o644); err != nil {
		t.Fatal(err)
	}
	recipe := domain.Recipe{
		TargetWidth: 24, TargetHeight: 16, ImageChannels: 4, ImageBitDepth: 8,
		ChromaSampling: "4:4:4", AudioSampleRate: 8000, AudioBitDepth: 16,
		MetadataPolicy: "strip",
	}
	a, err := Transcode(Input{ImagePath: imagePath, AudioPath: wavPath, ImageChannels: 4, AudioChannels: 1},
		recipe, Options{Path: PathFine})
	if err != nil {
		t.Fatal(err)
	}
	alt := recipe
	alt.TargetWidth = 25
	b, err := Transcode(Input{ImagePath: imagePath, AudioPath: wavPath, ImageChannels: 4, AudioChannels: 1},
		alt, Options{Path: PathFine})
	if err != nil {
		t.Fatal(err)
	}
	_, err = CompareProducts(a.Bytes, b.Bytes, domain.Consistency{RequireByteIdentical: true})
	if errs.CodeOf(err) != errs.EScale {
		t.Fatalf("code = %s, want E_SCALE (%v)", errs.CodeOf(err), err)
	}
}
