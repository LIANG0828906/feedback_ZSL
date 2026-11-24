package yuv

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"yuvench/errs"
)

func LoadManifest(path string) (*Manifest, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, &errs.Error{Code: errs.CodeIO, Message: "read manifest", Cause: err.Error()}
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, nil, &errs.Error{Code: errs.CodePlane, Message: "parse manifest", Field: "manifest", Cause: err.Error()}
	}
	if manifest.SchemaVersion == "" {
		return nil, nil, &errs.Error{Code: errs.CodePlane, Message: "missing manifest field", Field: "schemaVersion"}
	}
	if manifest.SchemaVersion != SchemaVersion {
		return nil, nil, &errs.Error{Code: errs.CodePlane, Message: "unsupported manifest schema", Field: "schemaVersion", Expected: SchemaVersion, Actual: manifest.SchemaVersion}
	}
	if len(manifest.Images) == 0 {
		return nil, nil, &errs.Error{Code: errs.CodePlane, Message: "manifest has no images", Field: "images"}
	}
	return &manifest, data, nil
}

func FindImage(manifest *Manifest, name string) (*ImageMeta, error) {
	if manifest == nil {
		return nil, errs.New(errs.CodePlane, "manifest is nil")
	}
	if name == "" {
		return &manifest.Images[0], nil
	}
	for i := range manifest.Images {
		if manifest.Images[i].Name == name {
			return &manifest.Images[i], nil
		}
	}
	return nil, &errs.Error{Code: errs.CodePlane, Message: "image not found in manifest", Field: "image", Actual: name}
}

func LoadImage(manifestPath, imageName string) (*Image, error) {
	manifest, manifestBytes, err := LoadManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	meta, err := FindImage(manifest, imageName)
	if err != nil {
		return nil, err
	}
	if err := validateMeta(meta); err != nil {
		return nil, err
	}
	baseDir := filepath.Dir(manifestPath)
	planes := make(map[string]*Plane, 3)
	hashes := make(map[string]string, 3)
	for _, name := range []string{"Y", "U", "V"} {
		spec, ok := meta.Planes[name]
		if !ok {
			return nil, &errs.Error{Code: errs.CodePlane, Message: "missing plane metadata", Field: "planes." + name, Plane: name}
		}
		plane, err := loadPlane(baseDir, *meta, name, spec)
		if err != nil {
			return nil, err
		}
		planes[name] = plane
		hashes[name] = plane.ContentHash()
	}

	inputHash := HashBytes(
		[]byte(SchemaVersion),
		[]byte(meta.Name),
		[]byte(fmt.Sprintf("%d:%d:%d:%d:%s:%s:%s:%s:%d:%d", meta.Width, meta.Height, meta.BitDepth, meta.Channels, meta.ChromaAlignment, meta.Upsample, meta.Endianness, meta.Quantization.Range, meta.Quantization.Min, meta.Quantization.Max)),
		planes["Y"].EncodeRaw(),
		planes["U"].EncodeRaw(),
		planes["V"].EncodeRaw(),
	)
	chromaVersion := HashString(fmt.Sprintf("%s|%s|%s|chroma-upsample-v1", meta.ChromaAlignment, meta.Upsample, SchemaVersion))
	return &Image{
		Meta:          *meta,
		ManifestPath:  manifestPath,
		ManifestHash:  HashBytes(manifestBytes),
		InputHash:     inputHash,
		Y:             planes["Y"],
		U:             planes["U"],
		V:             planes["V"],
		PlaneHashes:   hashes,
		ChromaVersion: chromaVersion,
	}, nil
}

func validateMeta(meta *ImageMeta) error {
	if meta == nil {
		return &errs.Error{Code: errs.CodePlane, Message: "missing image metadata", Field: "images"}
	}
	if meta.Name == "" {
		return &errs.Error{Code: errs.CodePlane, Message: "missing image name", Field: "name"}
	}
	if meta.Width <= 0 {
		return &errs.Error{Code: errs.CodePlane, Message: "missing or invalid image width", Field: "width", Plane: "Y", Actual: meta.Width}
	}
	if meta.Height <= 0 {
		return &errs.Error{Code: errs.CodePlane, Message: "missing or invalid image height", Field: "height", Plane: "Y", Actual: meta.Height}
	}
	if meta.Width%2 != 0 || meta.Height%2 != 0 {
		return &errs.Error{Code: errs.CodePlane, Message: "YUV 4:2:0 luma dimensions must be even", Field: "width,height", Plane: "Y", Actual: fmt.Sprintf("%dx%d", meta.Width, meta.Height)}
	}
	if meta.BitDepth != 8 && meta.BitDepth != 10 && meta.BitDepth != 12 && meta.BitDepth != 16 {
		return &errs.Error{Code: errs.CodePlane, Message: "unsupported bit depth", Field: "bitDepth", Expected: "8,10,12,16", Actual: meta.BitDepth}
	}
	if meta.Channels == 0 {
		meta.Channels = 3
	}
	if meta.Channels != 3 {
		return &errs.Error{Code: errs.CodePlane, Message: "YUV image must declare three channels", Field: "channels", Expected: 3, Actual: meta.Channels}
	}
	if meta.ChromaAlignment == "" {
		return &errs.Error{Code: errs.CodePlane, Message: "missing chroma alignment", Field: "chromaAlignment"}
	}
	if meta.ChromaAlignment != "left" && meta.ChromaAlignment != "center" {
		return &errs.Error{Code: errs.CodePlane, Message: "unsupported chroma alignment", Field: "chromaAlignment", Expected: "left,center", Actual: meta.ChromaAlignment}
	}
	if meta.Upsample == "" {
		return &errs.Error{Code: errs.CodePlane, Message: "missing chroma upsampling rule", Field: "upsample"}
	}
	if meta.Upsample != "nearest" && meta.Upsample != "bilinear" {
		return &errs.Error{Code: errs.CodePlane, Message: "unsupported chroma upsampling rule", Field: "upsample", Expected: "nearest,bilinear", Actual: meta.Upsample}
	}
	if meta.Endianness == "" {
		meta.Endianness = "little"
	}
	if meta.Endianness != "little" && meta.Endianness != "big" {
		return &errs.Error{Code: errs.CodePlane, Message: "unsupported endianness", Field: "endianness", Expected: "little,big", Actual: meta.Endianness}
	}
	if meta.Layout != "" && meta.Layout != "yuv420p" && meta.Layout != "planar" {
		return &errs.Error{Code: errs.CodePlane, Message: "unsupported plane layout", Field: "layout", Expected: "yuv420p,planar", Actual: meta.Layout}
	}
	if err := validateQuantization(*meta); err != nil {
		return err
	}
	if len(meta.Planes) != 3 {
		return &errs.Error{Code: errs.CodePlane, Message: "expected exactly Y, U and V planes", Field: "planes", Actual: len(meta.Planes)}
	}
	for _, block := range meta.Blocks {
		if block.Width <= 0 || block.Height <= 0 || block.X < 0 || block.Y < 0 || block.X+block.Width > meta.Width || block.Y+block.Height > meta.Height {
			return &errs.Error{Code: errs.CodePlane, Message: "truth block is outside image", Field: "blocks", Plane: "Y", Actual: block.Name}
		}
		if block.CenterX < block.X || block.CenterX >= block.X+block.Width || block.CenterY < block.Y || block.CenterY >= block.Y+block.Height {
			return &errs.Error{Code: errs.CodePlane, Message: "truth block center is outside block", Field: "blocks.center", Actual: block.Name}
		}
	}
	for _, sample := range meta.Samples {
		if sample.X < 0 || sample.Y < 0 || sample.X >= meta.Width || sample.Y >= meta.Height {
			return &errs.Error{Code: errs.CodePlane, Message: "truth sample is outside image", Field: "samples", Actual: fmt.Sprintf("%d,%d", sample.X, sample.Y)}
		}
		if sample.ChromaX < 0 || sample.ChromaY < 0 || sample.ChromaX >= meta.Width/2 || sample.ChromaY >= meta.Height/2 {
			return &errs.Error{Code: errs.CodePlane, Message: "truth chroma sample is outside chroma plane", Field: "samples.chroma", Actual: fmt.Sprintf("%d,%d", sample.ChromaX, sample.ChromaY)}
		}
	}
	return nil
}

func validateQuantization(meta ImageMeta) error {
	q := meta.Quantization
	if q.Range == "" {
		return &errs.Error{Code: errs.CodePlane, Message: "missing quantization range", Field: "quantization.range"}
	}
	if q.Min < 0 || q.Max < 0 {
		return &errs.Error{Code: errs.CodePlane, Message: "quantization bounds must be non-negative", Field: "quantization.min,max", Actual: fmt.Sprintf("%d..%d", q.Min, q.Max)}
	}
	maxCode := int(meta.MaxCode())
	scale := 1 << max(0, meta.BitDepth-8)
	switch q.Range {
	case "full":
		if q.Min != 0 || q.Max != maxCode {
			return &errs.Error{Code: errs.CodePlane, Message: "full-range quantization does not match bit depth", Field: "quantization.max", Expected: fmt.Sprintf("%d..%d", 0, maxCode), Actual: fmt.Sprintf("%d..%d", q.Min, q.Max)}
		}
	case "video":
		if q.Min != 16*scale || q.Max != 240*scale {
			return &errs.Error{Code: errs.CodePlane, Message: "video-range quantization does not match bit depth", Field: "quantization.max", Expected: fmt.Sprintf("%d..%d", 16*scale, 240*scale), Actual: fmt.Sprintf("%d..%d", q.Min, q.Max)}
		}
	default:
		return &errs.Error{Code: errs.CodePlane, Message: "unsupported quantization range", Field: "quantization.range", Expected: "full,video", Actual: q.Range}
	}
	return nil
}

func loadPlane(baseDir string, meta ImageMeta, name string, spec PlaneSpec) (*Plane, error) {
	expectedWidth := meta.Width
	expectedHeight := meta.Height
	if name != "Y" {
		expectedWidth /= 2
		expectedHeight /= 2
	}
	if spec.Width <= 0 || spec.Height <= 0 {
		return nil, &errs.Error{Code: errs.CodePlane, Message: "missing plane dimensions", Field: "planes." + name, Plane: name, Actual: fmt.Sprintf("%dx%d", spec.Width, spec.Height)}
	}
	if spec.Width != expectedWidth || spec.Height != expectedHeight {
		return nil, &errs.Error{Code: errs.CodePlane, Message: "plane dimensions do not match YUV 4:2:0 layout", Field: "planes." + name, Plane: name, Expected: fmt.Sprintf("%dx%d", expectedWidth, expectedHeight), Actual: fmt.Sprintf("%dx%d", spec.Width, spec.Height)}
	}
	if spec.Channels != 0 && spec.Channels != 1 {
		return nil, &errs.Error{Code: errs.CodePlane, Message: "plane must have one channel", Field: "planes." + name + ".channels", Plane: name, Expected: 1, Actual: spec.Channels}
	}
	if spec.BitDepth != 0 && spec.BitDepth != meta.BitDepth {
		return nil, &errs.Error{Code: errs.CodePlane, Message: "plane bit depth does not match image bit depth", Field: "planes." + name + ".bitDepth", Plane: name, Expected: meta.BitDepth, Actual: spec.BitDepth}
	}
	if strings.TrimSpace(spec.File) == "" {
		return nil, &errs.Error{Code: errs.CodePlane, Message: "missing plane file", Field: "planes." + name + ".file", Plane: name}
	}
	path := spec.File
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, &errs.Error{Code: errs.CodePlane, Message: "cannot read plane file", Field: "planes." + name + ".file", Plane: name, Actual: err.Error()}
	}
	bytesPerSample := 1
	if meta.BitDepth > 8 {
		bytesPerSample = 2
	}
	expectedBytes := expectedWidth * expectedHeight * bytesPerSample
	if info.Size() != int64(expectedBytes) {
		return nil, &errs.Error{Code: errs.CodePlane, Message: "plane file size mismatch", Field: "planes." + name, Plane: name, Expected: fmt.Sprintf("%dx%d bytes=%d", expectedWidth, expectedHeight, expectedBytes), Actual: fmt.Sprintf("%dx%d bytes=%d", spec.Width, spec.Height, info.Size())}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &errs.Error{Code: errs.CodeIO, Message: "read plane", Plane: name, Cause: err.Error()}
	}
	data := make([]uint16, expectedWidth*expectedHeight)
	for i := range data {
		if bytesPerSample == 1 {
			data[i] = uint16(raw[i])
		} else if meta.Endianness == "big" {
			data[i] = binary.BigEndian.Uint16(raw[i*2:])
		} else {
			data[i] = binary.LittleEndian.Uint16(raw[i*2:])
		}
		if data[i] > meta.MaxCode() {
			return nil, &errs.Error{Code: errs.CodePlane, Message: "sample exceeds bit-depth range", Field: "planes." + name, Plane: name, Expected: fmt.Sprintf("<=%d", meta.MaxCode()), Actual: data[i]}
		}
	}
	low, high := meta.PlaneLimits(name)
	return &Plane{
		Name:     name,
		Width:    expectedWidth,
		Height:   expectedHeight,
		Channels: 1,
		BitDepth: meta.BitDepth,
		MinCode:  low,
		MaxCode:  high,
		Data:     data,
	}, nil
}

// VerifyTruth checks the parsed manifest truth against the source planes.
func VerifyTruth(image *Image) error {
	if image == nil {
		return errs.New(errs.CodePlane, "image is nil")
	}
	for _, block := range image.Meta.Blocks {
		x, y := block.CenterX, block.CenterY
		u := image.U.Pixel(x/2, y/2)
		v := image.V.Pixel(x/2, y/2)
		if image.Y.Pixel(x, y) != block.YValue || u != block.UValue || v != block.VValue {
			return &errs.Error{Code: errs.CodePlane, Message: "block truth mismatch", Field: "blocks", Plane: "YUV", Actual: fmt.Sprintf("%s(%d,%d)", block.Name, x, y)}
		}
	}
	for _, sample := range image.Meta.Samples {
		if image.Y.Pixel(sample.X, sample.Y) != sample.YValue ||
			image.U.Pixel(sample.ChromaX, sample.ChromaY) != sample.UValue ||
			image.V.Pixel(sample.ChromaX, sample.ChromaY) != sample.VValue {
			return &errs.Error{Code: errs.CodePlane, Message: "sample truth mismatch", Field: "samples", Actual: fmt.Sprintf("%d,%d", sample.X, sample.Y)}
		}
	}
	return nil
}

func roundClamp(value float64, low, high uint16) uint16 {
	if math.IsNaN(value) || value <= float64(low) {
		return low
	}
	if value >= float64(high) {
		return high
	}
	return uint16(math.Round(value))
}
