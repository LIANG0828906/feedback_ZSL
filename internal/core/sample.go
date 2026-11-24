package core

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

type SampleSpec struct {
	Name        string `json:"name"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Channels    int    `json:"channels"`
	Seed        int64  `json:"seed"`
	Noisy       bool   `json:"noisy"`
	LowContrast bool   `json:"lowContrast"`
}

func DefaultSampleSpecs() []SampleSpec {
	return []SampleSpec{
		{Name: "gray-640-clean", Width: 640, Height: 480, Channels: 1, Seed: 42001},
		{Name: "rgb-960-noisy", Width: 960, Height: 640, Channels: 3, Seed: 42002, Noisy: true},
		{Name: "gray-1280-low-contrast", Width: 1280, Height: 800, Channels: 1, Seed: 42003, Noisy: true, LowContrast: true},
		{Name: "rgb-2048-wide", Width: 2048, Height: 1152, Channels: 3, Seed: 42004, Noisy: true},
	}
}

func GenerateSample(root string, spec SampleSpec) (LoadedSample, error) {
	if spec.Width < 32 || spec.Width > 8192 {
		return LoadedSample{}, NewError(CodeParam, "width", "sample width must be in [32,8192]")
	}
	if spec.Height < 32 || spec.Height > 8192 {
		return LoadedSample{}, NewError(CodeParam, "height", "sample height must be in [32,8192]")
	}
	if spec.Channels != 1 && spec.Channels != 3 {
		return LoadedSample{}, NewError(CodeParam, "channels", "sample channels must be 1 or 3")
	}
	if strings.TrimSpace(spec.Name) == "" {
		return LoadedSample{}, NewError(CodeParam, "name", "sample name is required")
	}
	dir := filepath.Join(root, spec.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return LoadedSample{}, WrapError(CodeCheckpoint, "sampleDir", "cannot create sample directory", err)
	}

	im, truth := renderSample(spec)
	if err := im.Validate(); err != nil {
		return LoadedSample{}, err
	}
	manifest := SampleManifest{
		Name:        spec.Name,
		Seed:        spec.Seed,
		Width:       spec.Width,
		Height:      spec.Height,
		Channels:    spec.Channels,
		Noisy:       spec.Noisy,
		LowContrast: spec.LowContrast,
		Fingerprint: FingerprintImage(im),
		ImageFile:   "image.png",
		Truth:       truth,
		CreatedBy:   "deterministic-local-generator/v1",
	}
	if err := writeImage(filepath.Join(dir, manifest.ImageFile), im); err != nil {
		return LoadedSample{}, err
	}

	refs, err := buildReferenceMasks(im, truth)
	if err != nil {
		return LoadedSample{}, err
	}
	for _, ref := range refs {
		dirname := "references"
		if err := os.MkdirAll(filepath.Join(dir, dirname), 0o755); err != nil {
			return LoadedSample{}, WrapError(CodeCheckpoint, "references", "cannot create reference directory", err)
		}
		ref.File = filepath.ToSlash(filepath.Join(dirname, ref.Stage+".png"))
		mask := Image{Width: im.Width, Height: im.Height, Channels: im.Channels, Pix: ref.mask}
		if err := writeImage(filepath.Join(dir, ref.File), mask); err != nil {
			return LoadedSample{}, err
		}
		ref.Fingerprint = FingerprintImage(mask)
		ref.Coverage = coverage(mask.Pix, mask.Width*mask.Height*mask.Channels)
		manifest.References = append(manifest.References, ref.ReferenceMask)
	}
	if err := writeJSON(filepath.Join(dir, "manifest.json"), manifest); err != nil {
		return LoadedSample{}, err
	}
	return LoadedSample{Dir: dir, Manifest: manifest, Image: im}, nil
}

func renderSample(spec SampleSpec) (Image, Truth) {
	im := NewImage(spec.Width, spec.Height, spec.Channels)
	base := 86
	if spec.LowContrast {
		base = 104
	}
	for c := 0; c < spec.Channels; c++ {
		for y := 0; y < spec.Height; y++ {
			for x := 0; x < spec.Width; x++ {
				im.Set(x, y, c, byte(base+c*3))
			}
		}
	}
	foreground := 205
	if spec.LowContrast {
		foreground = 126
	}
	minDim := float64(min(spec.Width, spec.Height))
	lineStroke := math.Max(1.6, minDim/240)
	lines := []LineTruth{
		{Channel: -1, X1: 0.12 * float64(spec.Width), Y1: 0.25 * float64(spec.Height), X2: 0.48 * float64(spec.Width), Y2: 0.69 * float64(spec.Height)},
		{Channel: -1, X1: 0.34 * float64(spec.Width), Y1: 0.79 * float64(spec.Height), X2: 0.79 * float64(spec.Width), Y2: 0.51 * float64(spec.Height)},
	}
	circle := CircleTruth{
		Channel:  -1,
		CX:       0.70 * float64(spec.Width),
		CY:       0.28 * float64(spec.Height),
		Diameter: 0.24 * minDim,
	}
	for c := 0; c < spec.Channels; c++ {
		tone := byte(foreground - c*7)
		for _, ln := range lines {
			drawLine(&im, c, ln.X1, ln.Y1, ln.X2, ln.Y2, lineStroke, tone)
		}
		radius := circle.Diameter / 2
		drawDisk(&im, c, circle.CX, circle.CY, radius, tone)
	}
	if spec.Noisy {
		amp := 7.0
		if spec.LowContrast {
			amp = 4.0
		}
		for y := 0; y < spec.Height; y++ {
			for x := 0; x < spec.Width; x++ {
				for c := 0; c < spec.Channels; c++ {
					n := deterministicNoise(spec.Seed, x, y, c, amp)
					v := int(im.At(x, y, c)) + n
					im.Set(x, y, c, byte(clamp(v, 0, 255)))
				}
			}
		}
	}
	truth := Truth{}
	for c := 0; c < spec.Channels; c++ {
		for _, ln := range lines {
			copied := ln
			copied.Channel = c
			truth.Lines = append(truth.Lines, copied)
		}
		copiedCircle := circle
		copiedCircle.Channel = c
		truth.Circles = append(truth.Circles, copiedCircle)
	}
	// The reference gradient pipeline supplies the declared edge coverage and
	// the five per-stage reference masks.
	params := DefaultParams()
	stages, err := ComputeStages(im, params)
	if err == nil && len(stages) > 0 {
		truth.EdgeCoverage = coverage(stages[0].Pix, stages[0].Width*stages[0].Height*stages[0].Channels)
	}
	return im, truth
}

type builtReference struct {
	ReferenceMask
	mask []byte
}

func buildReferenceMasks(im Image, truth Truth) ([]builtReference, error) {
	stages, err := ComputeStages(im, DefaultParams())
	if err != nil {
		return nil, err
	}
	refs := make([]builtReference, 0, len(stages))
	for _, st := range stages {
		refs = append(refs, builtReference{
			ReferenceMask: ReferenceMask{
				Stage:    st.Stage,
				Coverage: st.Coverage,
			},
			mask: st.Pix,
		})
	}
	_ = truth
	return refs, nil
}

func LoadSample(dir string) (LoadedSample, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return LoadedSample{}, WrapError(CodeDecode, "manifest", "cannot read manifest", err)
	}
	var manifest SampleManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return LoadedSample{}, WrapError(CodeDecode, "manifest", "manifest JSON is invalid", err)
	}
	imagePath := filepath.Join(dir, filepath.FromSlash(manifest.ImageFile))
	im, err := DecodePNGFile(imagePath)
	if err != nil {
		return LoadedSample{}, err
	}
	if im.Width != manifest.Width {
		return LoadedSample{}, NewError(CodeMeta, "width", fmt.Sprintf("decoded width %d does not match manifest width %d", im.Width, manifest.Width))
	}
	if im.Height != manifest.Height {
		return LoadedSample{}, NewError(CodeMeta, "height", fmt.Sprintf("decoded height %d does not match manifest height %d", im.Height, manifest.Height))
	}
	if im.Channels != manifest.Channels {
		return LoadedSample{}, NewError(CodeMeta, "channels", fmt.Sprintf("decoded channels %d does not match manifest channels %d", im.Channels, manifest.Channels))
	}
	if fp := FingerprintImage(im); fp != manifest.Fingerprint {
		return LoadedSample{}, &AppError{
			Code:    CodeMeta,
			Field:   "contentFingerprint",
			Message: "decoded pixel fingerprint does not match manifest",
			Details: map[string]any{"expected": manifest.Fingerprint, "actual": fp},
		}
	}
	return LoadedSample{Dir: dir, Manifest: manifest, Image: im}, nil
}

func ValidateReferences(sample LoadedSample) error {
	for _, ref := range sample.Manifest.References {
		im, err := DecodePNGFile(filepath.Join(sample.Dir, filepath.FromSlash(ref.File)))
		if err != nil {
			return err
		}
		if im.Width != sample.Image.Width {
			return NewError(CodeMeta, "reference.width", ref.Stage+" reference width mismatch")
		}
		if im.Height != sample.Image.Height {
			return NewError(CodeMeta, "reference.height", ref.Stage+" reference height mismatch")
		}
		if im.Channels != sample.Image.Channels {
			return NewError(CodeMeta, "reference.channels", ref.Stage+" reference channel count mismatch")
		}
		if got := FingerprintImage(im); got != ref.Fingerprint {
			return NewError(CodeMeta, "reference.fingerprint", ref.Stage+" reference fingerprint mismatch")
		}
	}
	return nil
}

func writeImage(path string, im Image) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return WrapError(CodeCheckpoint, "", "cannot create image", err)
	}
	if err := EncodePNG(f, im); err != nil {
		_ = f.Close()
		return WrapError(CodeDecode, "", "cannot encode PNG", err)
	}
	if err := f.Close(); err != nil {
		return WrapError(CodeCheckpoint, "", "cannot close image", err)
	}
	return os.Rename(tmp, path)
}

func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return WrapError(CodeCheckpoint, "", "cannot marshal JSON", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return WrapError(CodeCheckpoint, "", "cannot write JSON", err)
	}
	return os.Rename(tmp, path)
}

func drawLine(im *Image, channel int, x1, y1, x2, y2, halfWidth float64, tone byte) {
	dx, dy := x2-x1, y2-y1
	len2 := dx*dx + dy*dy
	if len2 == 0 {
		return
	}
	minX := max(0, int(math.Floor(math.Min(x1, x2)-halfWidth-1)))
	maxX := min(im.Width-1, int(math.Ceil(math.Max(x1, x2)+halfWidth+1)))
	minY := max(0, int(math.Floor(math.Min(y1, y2)-halfWidth-1)))
	maxY := min(im.Height-1, int(math.Ceil(math.Max(y1, y2)+halfWidth+1)))
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			t := ((px-x1)*dx + (py-y1)*dy) / len2
			t = math.Max(0, math.Min(1, t))
			qx, qy := x1+t*dx, y1+t*dy
			dist := math.Hypot(px-qx, py-qy)
			if dist <= halfWidth {
				im.Set(x, y, channel, tone)
			}
		}
	}
}

func drawDisk(im *Image, channel int, cx, cy, radius float64, tone byte) {
	minX := max(0, int(math.Floor(cx-radius-1)))
	maxX := min(im.Width-1, int(math.Ceil(cx+radius+1)))
	minY := max(0, int(math.Floor(cy-radius-1)))
	maxY := min(im.Height-1, int(math.Ceil(cy+radius+1)))
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			if math.Hypot(px-cx, py-cy) <= radius {
				im.Set(x, y, channel, tone)
			}
		}
	}
}

func deterministicNoise(seed int64, x, y, c int, amplitude float64) int {
	h := uint64(seed) + 0x9e3779b97f4a7c15
	h ^= uint64(x) * 0xbf58476d1ce4e5b9
	h ^= uint64(y) * 0x94d049bb133111eb
	h ^= uint64(c+1) * 0xd6e8feb86659fd93
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	h *= 0x94d049bb133111eb
	h ^= h >> 31
	unit := float64(h&0xffff) / 65535.0
	return int(math.Round((unit*2 - 1) * amplitude))
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func coverage(pix []byte, total int) float64 {
	if total == 0 {
		return 0
	}
	n := 0
	for _, v := range pix {
		if v != 0 {
			n++
		}
	}
	return float64(n) / float64(total)
}
