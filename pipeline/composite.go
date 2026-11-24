package pipeline

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

const frameMagic = "VSPFRAME"

type Frame struct {
	Window     int       `json:"window"`
	Frame      int       `json:"frame"`
	Width      int       `json:"width"`
	Height     int       `json:"height"`
	Channels   int       `json:"channels"`
	ColorSpace string    `json:"color_space"`
	AlphaMode  string    `json:"alpha_mode"`
	TimeUnit   string    `json:"time_unit"`
	Timestamp  Rational  `json:"timestamp"`
	Pixels     []float32 `json:"-"`
}

type PixelSample struct {
	X    int        `json:"x"`
	Y    int        `json:"y"`
	RGBA [4]float64 `json:"rgba"`
}

type FrameSummary struct {
	Frame       int      `json:"frame"`
	Timestamp   Rational `json:"timestamp"`
	PixelCount  int      `json:"pixel_count"`
	MeanLuma    float64  `json:"mean_luma"`
	MinLuma     float64  `json:"min_luma"`
	MaxLuma     float64  `json:"max_luma"`
	MeanAlpha   float64  `json:"mean_alpha"`
	ContentHash string   `json:"content_hash"`
}

type CompositeConfig struct {
	MaxFrameBytes int64
}

func (c CompositeConfig) Validate(width, height, channels int) error {
	if width <= 0 || height <= 0 {
		return paraError(-1, "frame dimensions must be positive")
	}
	if channels != 3 && channels != 4 {
		return paraError(-1, "frame channels must be 3 or 4")
	}
	size := int64(width) * int64(height) * int64(channels) * 4
	if c.MaxFrameBytes <= 0 {
		return paraError(-1, "max frame bytes must be positive")
	}
	if size > c.MaxFrameBytes {
		return paraError(-1, "frame buffer %d exceeds configured limit %d", size, c.MaxFrameBytes)
	}
	return nil
}

// CompositeFrame executes one deterministic frame. The model is never mutated;
// per-frame instructions are applied to local copies of each layer.
func CompositeFrame(model *Model, frame int, cfg CompositeConfig) (*Frame, error) {
	if model == nil {
		return nil, paraError(-1, "model is nil")
	}
	if frame < 0 || frame >= model.FrameCount {
		return nil, paraError(frame, "frame %d is outside [0,%d)", frame, model.FrameCount)
	}
	if err := cfg.Validate(model.Canvas.Width, model.Canvas.Height, model.Canvas.Channels); err != nil {
		return nil, err
	}
	if model.Canvas.ColorSpace != ColorLinear {
		return nil, paraError(frame, "unsupported working color space %q", model.Canvas.ColorSpace)
	}
	ins := instructionForFrame(model, frame)
	for id, ov := range ins.Layers {
		if ov.Opacity != nil && (*ov.Opacity < 0 || *ov.Opacity > 1) {
			return nil, paraError(frame, "instruction opacity for layer %s is outside [0,1]", id)
		}
		if ov.Alpha != nil && (*ov.Alpha < 0 || *ov.Alpha > 1) {
			return nil, paraError(frame, "instruction alpha for layer %s is outside [0,1]", id)
		}
		if ov.Color != nil {
			for i, c := range *ov.Color {
				if c < 0 || c > 1 || math.IsNaN(c) {
					return nil, paraError(frame, "instruction color[%d] for layer %s is outside [0,1]", i, id)
				}
			}
		}
	}
	byID := make(map[string]Layer, len(model.Layers))
	for _, layer := range model.Layers {
		byID[layer.ID] = layer
	}
	w, h, ch := model.Canvas.Width, model.Canvas.Height, model.Canvas.Channels
	acc := make([]float32, w*h*4)
	for _, id := range model.Stack {
		base, ok := byID[id]
		if !ok {
			return nil, layerError(id, "stack references missing layer")
		}
		layer, visible := layerAtFrame(base, frame, ins)
		if !visible || layer.Opacity <= 0 || layer.Alpha <= 0 {
			continue
		}
		r, g, b, a := straightLinearSource(layer)
		if a <= 0 {
			continue
		}
		minX, minY, maxX, maxY := shapeBounds(layer.Shape, w, h)
		for y := minY; y < maxY; y++ {
			for x := minX; x < maxX; x++ {
				coverage := shapeCoverage(layer.Shape, x, y, w, h)
				if coverage <= 0 {
					continue
				}
				srcA := float32(clampFloat64(a*coverage, 0, 1))
				if srcA <= 0 {
					continue
				}
				idx := (y*w + x) * 4
				inv := 1 - srcA
				acc[idx] = float32(r)*srcA + acc[idx]*inv
				acc[idx+1] = float32(g)*srcA + acc[idx+1]*inv
				acc[idx+2] = float32(b)*srcA + acc[idx+2]*inv
				acc[idx+3] = srcA + acc[idx+3]*inv
			}
		}
	}
	pixels := make([]float32, w*h*ch)
	for p := 0; p < w*h; p++ {
		a := acc[p*4+3]
		if ch == 4 {
			if a > 0 {
				pixels[p*4] = acc[p*4] / a
				pixels[p*4+1] = acc[p*4+1] / a
				pixels[p*4+2] = acc[p*4+2] / a
			}
			pixels[p*4+3] = a
		} else {
			// Three-channel deliverables use premultiplied linear RGB; no
			// alpha channel is available to recover straight color.
			pixels[p*3] = acc[p*4]
			pixels[p*3+1] = acc[p*4+1]
			pixels[p*3+2] = acc[p*4+2]
		}
	}
	return &Frame{
		Frame: frame, Width: w, Height: h, Channels: ch,
		ColorSpace: ColorLinear, AlphaMode: AlphaStraight,
		TimeUnit: model.TimeBase.Unit, Timestamp: model.TimeBase.Timestamp(frame),
		Pixels: pixels,
	}, nil
}

func straightLinearSource(layer Layer) (r, g, b, a float64) {
	r, g, b = layer.Color[0], layer.Color[1], layer.Color[2]
	a = layer.Alpha
	if layer.AlphaMode == AlphaPremultiplied && a > 0 {
		r /= a
		g /= a
		b /= a
	}
	if layer.ColorEncoding == ColorSRGB {
		r = srgbToLinear(r)
		g = srgbToLinear(g)
		b = srgbToLinear(b)
	}
	a *= layer.Opacity
	return clampFloat64(r, 0, 1), clampFloat64(g, 0, 1), clampFloat64(b, 0, 1), clampFloat64(a, 0, 1)
}

func srgbToLinear(v float64) float64 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

func linearToSRGB(v float64) float64 {
	v = clampFloat64(v, 0, 1)
	if v <= 0.0031308 {
		return v * 12.92
	}
	return 1.055*math.Pow(v, 1/2.4) - 0.055
}

func clampFloat64(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func shapeBounds(shape Shape, width, height int) (minX, minY, maxX, maxY int) {
	switch shape.Type {
	case ShapeRect, ShapeEllipse:
		minX = int(math.Floor(shape.Rect[0]))
		minY = int(math.Floor(shape.Rect[1]))
		maxX = int(math.Ceil(shape.Rect[0] + shape.Rect[2]))
		maxY = int(math.Ceil(shape.Rect[1] + shape.Rect[3]))
		minX = clamp(minX, 0, width)
		minY = clamp(minY, 0, height)
		maxX = clamp(maxX, 0, width)
		maxY = clamp(maxY, 0, height)
	default:
		return 0, 0, width, height
	}
	return minX, minY, maxX, maxY
}

func shapeCoverage(shape Shape, x, y, width, height int) float64 {
	switch shape.Type {
	case ShapeFull:
		return 1
	case ShapeRect:
		if float64(x) >= shape.Rect[0] && float64(x+1) <= shape.Rect[0]+shape.Rect[2] &&
			float64(y) >= shape.Rect[1] && float64(y+1) <= shape.Rect[1]+shape.Rect[3] {
			return 1
		}
		return 0
	case ShapeEllipse:
		rx := shape.Rect[2] / 2
		ry := shape.Rect[3] / 2
		if rx <= 0 || ry <= 0 {
			return 0
		}
		cx := shape.Rect[0] + rx
		cy := shape.Rect[1] + ry
		dx := (float64(x) + 0.5 - cx) / rx
		dy := (float64(y) + 0.5 - cy) / ry
		if dx*dx+dy*dy <= 1 {
			return 1
		}
		return 0
	default:
		return 0
	}
}

func (f *Frame) Sample(x, y int) (PixelSample, error) {
	if f == nil {
		return PixelSample{}, paraError(-1, "frame is nil")
	}
	if x < 0 || x >= f.Width || y < 0 || y >= f.Height {
		return PixelSample{}, paraError(-1, "sample (%d,%d) outside %dx%d frame", x, y, f.Width, f.Height)
	}
	idx := (y*f.Width + x) * f.Channels
	out := PixelSample{X: x, Y: y, RGBA: [4]float64{0, 0, 0, 1}}
	if f.Channels == 4 {
		out.RGBA = [4]float64{float64(f.Pixels[idx]), float64(f.Pixels[idx+1]), float64(f.Pixels[idx+2]), float64(f.Pixels[idx+3])}
	} else {
		out.RGBA = [4]float64{float64(f.Pixels[idx]), float64(f.Pixels[idx+1]), float64(f.Pixels[idx+2]), 1}
	}
	return out, nil
}

// EncodeFrame returns a stable little-endian raw frame representation used for
// cache files and content hashes.
func EncodeFrame(f *Frame) []byte {
	var buf bytes.Buffer
	buf.WriteString(frameMagic)
	buf.WriteByte(1)
	_ = binary.Write(&buf, binary.LittleEndian, int32(f.Frame))
	_ = binary.Write(&buf, binary.LittleEndian, int32(f.Width))
	_ = binary.Write(&buf, binary.LittleEndian, int32(f.Height))
	_ = binary.Write(&buf, binary.LittleEndian, int32(f.Channels))
	_ = binary.Write(&buf, binary.LittleEndian, f.Timestamp.Num)
	_ = binary.Write(&buf, binary.LittleEndian, f.Timestamp.Den)
	buf.WriteString(f.TimeUnit)
	buf.WriteByte(0)
	_ = binary.Write(&buf, binary.LittleEndian, f.Pixels)
	return buf.Bytes()
}

func DecodeFrame(data []byte) (*Frame, error) {
	reader := bytes.NewReader(data)
	magic := make([]byte, len(frameMagic))
	if _, err := reader.Read(magic); err != nil || string(magic) != frameMagic {
		return nil, errCode(CodeDecode, "invalid frame magic")
	}
	version, err := reader.ReadByte()
	if err != nil || version != 1 {
		return nil, errCode(CodeDecode, "unsupported frame encoding version")
	}
	var frame, width, height, channels int32
	for _, target := range []*int32{&frame, &width, &height, &channels} {
		if err := binary.Read(reader, binary.LittleEndian, target); err != nil {
			return nil, errCode(CodeDecode, "truncated frame header: %v", err)
		}
	}
	var timestamp Rational
	if err := binary.Read(reader, binary.LittleEndian, &timestamp.Num); err != nil {
		return nil, errCode(CodeDecode, "truncated frame timestamp")
	}
	if err := binary.Read(reader, binary.LittleEndian, &timestamp.Den); err != nil {
		return nil, errCode(CodeDecode, "truncated frame timestamp")
	}
	var unit []byte
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return nil, errCode(CodeDecode, "truncated frame time unit")
		}
		if b == 0 {
			break
		}
		unit = append(unit, b)
		if len(unit) > 64 {
			return nil, errCode(CodeDecode, "frame time unit is too long")
		}
	}
	if width <= 0 || height <= 0 || (channels != 3 && channels != 4) {
		return nil, errCode(CodeDecode, "invalid frame dimensions")
	}
	count := int(width) * int(height) * int(channels)
	if count < 0 || count > 1<<30 {
		return nil, errCode(CodeDecode, "frame dimensions exceed decoder limit")
	}
	pixels := make([]float32, count)
	if err := binary.Read(reader, binary.LittleEndian, pixels); err != nil {
		return nil, errCode(CodeDecode, "truncated pixel payload: %v", err)
	}
	if reader.Len() != 0 {
		return nil, errCode(CodeDecode, "trailing bytes in frame payload")
	}
	return &Frame{
		Frame: int(frame), Width: int(width), Height: int(height), Channels: int(channels),
		ColorSpace: ColorLinear, AlphaMode: AlphaStraight, TimeUnit: string(unit),
		Timestamp: timestamp, Pixels: pixels,
	}, nil
}

func FrameHash(f *Frame) string {
	sum := sha256.Sum256(EncodeFrame(f))
	return hex.EncodeToString(sum[:])
}

func SummarizeFrame(f *Frame) FrameSummary {
	n := f.Width * f.Height
	meanLuma, minLuma, maxLuma, meanAlpha := 0.0, math.Inf(1), math.Inf(-1), 0.0
	if n == 0 {
		minLuma, maxLuma = 0, 0
	}
	for p := 0; p < n; p++ {
		idx := p * f.Channels
		r, g, b := float64(f.Pixels[idx]), float64(f.Pixels[idx+1]), float64(f.Pixels[idx+2])
		luma := 0.2126*r + 0.7152*g + 0.0722*b
		meanLuma += luma
		if luma < minLuma {
			minLuma = luma
		}
		if luma > maxLuma {
			maxLuma = luma
		}
		if f.Channels == 4 {
			meanAlpha += float64(f.Pixels[idx+3])
		} else {
			meanAlpha++
		}
	}
	if n > 0 {
		meanLuma /= float64(n)
		meanAlpha /= float64(n)
	}
	return FrameSummary{
		Frame: f.Frame, Timestamp: f.Timestamp, PixelCount: n,
		MeanLuma: meanLuma, MinLuma: minLuma, MaxLuma: maxLuma, MeanAlpha: meanAlpha,
		ContentHash: FrameHash(f),
	}
}

func (f *Frame) PNG() ([]byte, error) {
	img := image.NewNRGBA64(image.Rect(0, 0, f.Width, f.Height))
	for y := 0; y < f.Height; y++ {
		for x := 0; x < f.Width; x++ {
			idx := (y*f.Width + x) * f.Channels
			r := linearToSRGB(float64(f.Pixels[idx]))
			g := linearToSRGB(float64(f.Pixels[idx+1]))
			b := linearToSRGB(float64(f.Pixels[idx+2]))
			a := 1.0
			if f.Channels == 4 {
				a = float64(f.Pixels[idx+3])
			}
			img.SetNRGBA64(x, y, color.NRGBA64{
				R: uint16(math.Round(r * 65535)),
				G: uint16(math.Round(g * 65535)),
				B: uint16(math.Round(b * 65535)),
				A: uint16(math.Round(a * 65535)),
			})
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, errCode(CodeDecode, "encode PNG: %v", err)
	}
	return buf.Bytes(), nil
}

type FrameStore struct {
	Dir string
}

func CanonicalModelBytes(model *Model) ([]byte, error) {
	if model == nil {
		return nil, paraError(-1, "model is nil")
	}
	cp := model.Clone()
	cp.Normalize()
	return json.Marshal(cp)
}

func LoadModelJSON(path string) (*Model, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errCode(CodeDecode, "read model state: %v", err)
	}
	var model Model
	if err := json.Unmarshal(raw, &model); err != nil {
		return nil, errCode(CodeDecode, "decode model state: %v", err)
	}
	model.Normalize()
	if err := model.Validate(); err != nil {
		return nil, err
	}
	return &model, nil
}

func WriteModelJSON(path string, model *Model) error {
	raw, err := CanonicalModelBytes(model)
	if err != nil {
		return err
	}
	return writeAtomic(path, append(raw, '\n'))
}

func ModelHash(model *Model) (string, error) {
	raw, err := CanonicalModelBytes(model)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
