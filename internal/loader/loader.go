package loader

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io/fs"
	"math"
	"os"
	"path/filepath"

	"integritykernel/internal/core"
)

type Frame struct {
	Meta           core.FrameMeta
	Image          *image.NRGBA
	Hash           string
	ActualChannels int
}

type Sequence struct {
	Dir      string
	Manifest core.Manifest
	Frames   []*Frame
	RootHash string
}

func Load(dir string) (*Sequence, error) {
	manifestPath := filepath.Join(dir, "truth.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, core.WithField(core.NewError(core.CodeDecode, "truth manifest missing: %s", manifestPath), "truth.json")
		}
		return nil, core.WithField(core.NewError(core.CodeDecode, "read truth manifest: %v", err), "truth.json")
	}
	var manifest core.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, core.WithField(core.NewError(core.CodeDecode, "decode truth manifest: %v", err), "truth.json")
	}
	if err := validateManifestShape(raw, &manifest); err != nil {
		return nil, err
	}

	if manifest.FrameCount < 1 {
		frame := 0
		return nil, &core.Error{Code: core.CodeMeta, Message: "frame_count must be positive", Frame: &frame, Field: "frame_count"}
	}
	if len(manifest.Frames) != manifest.FrameCount {
		frame := len(manifest.Frames)
		if manifest.FrameCount < frame {
			frame = manifest.FrameCount
		}
		return nil, &core.Error{
			Code:    core.CodeMeta,
			Message: "frame_count=" + itoa(manifest.FrameCount) + " does not match frames[] length=" + itoa(len(manifest.Frames)),
			Frame:   &frame, Field: "frame_count",
		}
	}

	seq := &Sequence{Dir: dir, Manifest: manifest}
	previousTimestamp := int64(math.MinInt64)
	rootHasher := sha256.New()

	for i, meta := range manifest.Frames {
		frameNumber := i
		if meta.Index != i {
			return nil, &core.Error{Code: core.CodeMeta, Message: "frame index does not match array position", Frame: &frameNumber, Field: "frames.index"}
		}
		if i > 0 && meta.TimestampMillis < previousTimestamp {
			return nil, &core.Error{Code: core.CodeMeta, Message: "timestamp is less than previous frame", Frame: &frameNumber, Field: "timestamp_millis"}
		}
		previousTimestamp = meta.TimestampMillis

		path := meta.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		img, channels, err := loadPNG(path)
		if err != nil {
			return nil, core.WithFrame(core.WithField(core.NewError(core.CodeDecode, "decode frame data: %v", err), "file"), i, "file")
		}
		if channels != manifest.Channels {
			return nil, &core.Error{Code: core.CodeMeta, Message: "PNG channel count does not match manifest", Frame: &i, Field: "channels"}
		}
		if img.Bounds().Dx() != manifest.Width {
			return nil, &core.Error{Code: core.CodeMeta, Message: "frame width does not match manifest", Frame: &i, Field: "width"}
		}
		if img.Bounds().Dy() != manifest.Height {
			return nil, &core.Error{Code: core.CodeMeta, Message: "frame height does not match manifest", Frame: &i, Field: "height"}
		}

		hash := hashNRGBA(img)
		seq.Frames = append(seq.Frames, &Frame{Meta: meta, Image: img, Hash: hash, ActualChannels: channels})
		rootHasher.Write([]byte(hash))
		previousTimestamp = meta.TimestampMillis
	}

	region := manifest.WatermarkRegion
	if !region.InFrame(manifest.Width, manifest.Height) {
		frame := 0
		return nil, &core.Error{Code: core.CodeMark, Message: "watermark region is outside frame or has invalid size", Frame: &frame, Field: "watermark_region"}
	}
	for pi, primitive := range manifest.Primitives {
		if !finiteRect(primitive.BBox) {
			frame := 0
			_ = pi
			return nil, &core.Error{Code: core.CodeMeta, Message: "primitive bounding box contains non-finite or invalid coordinates", Frame: &frame, Field: "primitives.bbox"}
		}
	}
	if len(manifest.Replacement.FrameMask) != manifest.FrameCount {
		frame := manifest.Replacement.StartFrame
		return nil, &core.Error{Code: core.CodeMeta, Message: "replacement frame_mask length does not match frame_count", Frame: &frame, Field: "replacement.frame_mask"}
	}

	seq.RootHash = hex.EncodeToString(rootHasher.Sum(nil))
	return seq, nil
}

func validateManifestShape(raw []byte, m *core.Manifest) error {
	if m.SchemaVersion == "" {
		return core.WithField(core.NewError(core.CodeMeta, "schema_version is required"), "schema_version")
	}
	if m.Width <= 0 || m.Height <= 0 {
		frame := 0
		return &core.Error{Code: core.CodeMeta, Message: "width and height must be positive", Frame: &frame, Field: "width"}
	}
	if m.Channels != 3 && m.Channels != 4 {
		frame := 0
		return &core.Error{Code: core.CodeMeta, Message: "channels must be 3 or 4", Frame: &frame, Field: "channels"}
	}
	var rawMap map[string]any
	if err := json.Unmarshal(raw, &rawMap); err == nil {
		if err := validateFiniteCoordinates(rawMap); err != nil {
			return err
		}
	}
	return nil
}

func validateFiniteCoordinates(v any) error {
	switch t := v.(type) {
	case map[string]any:
		for key, value := range t {
			if key == "x" || key == "y" || key == "width" || key == "height" || key == "start_frame" || key == "end_frame" {
				if f, ok := value.(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
					frame := 0
					return &core.Error{Code: core.CodeMeta, Message: "coordinate is not finite", Frame: &frame, Field: key}
				}
			}
			if err := validateFiniteCoordinates(value); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range t {
			if err := validateFiniteCoordinates(item); err != nil {
				return err
			}
		}
	}
	return nil
}

func finiteRect(r core.Rect) bool {
	return finiteFloat(float64(r.X)) && finiteFloat(float64(r.Y)) &&
		finiteFloat(float64(r.Width)) && finiteFloat(float64(r.Height)) &&
		r.Width >= 0 && r.Height >= 0
}

func finiteFloat(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func loadPNG(path string) (*image.NRGBA, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	cfg, err := png.DecodeConfig(f)
	if err != nil {
		return nil, 0, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, 0, err
	}
	decoded, _, err := image.Decode(f)
	if err != nil {
		return nil, 0, err
	}
	channels := channelsForModel(cfg.ColorModel)
	if channels == 0 {
		channels = channelsForImage(decoded)
	}
	bounds := decoded.Bounds()
	img := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(img, img.Bounds(), decoded, bounds.Min, draw.Src)
	return img, channels, nil
}

func hashNRGBA(img *image.NRGBA) string {
	h := sha256.New()
	width := img.Rect.Dx()
	height := img.Rect.Dy()
	var header [16]byte
	header[0] = byte(width >> 24)
	header[1] = byte(width >> 16)
	header[2] = byte(width >> 8)
	header[3] = byte(width)
	header[4] = byte(height >> 24)
	header[5] = byte(height >> 16)
	header[6] = byte(height >> 8)
	header[7] = byte(height)
	header[8] = 4
	h.Write(header[:9])
	h.Write(img.Pix)
	return hex.EncodeToString(h.Sum(nil))
}

func channelsForModel(m color.Model) int {
	switch m {
	case color.NRGBAModel, color.RGBAModel:
		return 4
	case color.GrayModel, color.Gray16Model:
		return 1
	default:
		return 3
	}
}

func channelsForImage(img image.Image) int {
	switch img.(type) {
	case *image.NRGBA, *image.RGBA:
		return 4
	case *image.Gray, *image.Gray16:
		return 1
	default:
		return 3
	}
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
