package scene

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
)

type Vec3 [3]float64

func (v Vec3) Add(o Vec3) Vec3      { return Vec3{v[0] + o[0], v[1] + o[1], v[2] + o[2]} }
func (v Vec3) Sub(o Vec3) Vec3      { return Vec3{v[0] - o[0], v[1] - o[1], v[2] - o[2]} }
func (v Vec3) Scale(s float64) Vec3 { return Vec3{v[0] * s, v[1] * s, v[2] * s} }

type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func (r Rect) Contains(x, y int) bool {
	return x >= r.X && y >= r.Y && x < r.X+r.W && y < r.Y+r.H
}

func (r Rect) Intersects(o Rect) bool {
	return r.X < o.X+o.W && o.X < r.X+r.W && r.Y < o.Y+o.H && o.Y < r.Y+r.H
}

func (r Rect) Expand(n int) Rect {
	return Rect{X: r.X - n, Y: r.Y - n, W: r.W + 2*n, H: r.H + 2*n}
}

type CameraResponse struct {
	InverseGamma     float64 `json:"inverseGamma"`
	FullWellRadiance float64 `json:"fullWellRadiance"`
	BlackLevel       float64 `json:"blackLevel"`
}

type Exposure struct {
	DurationUS int64    `json:"durationUS"`
	Codes      []uint16 `json:"codes"`
}

type Band struct {
	ID          string     `json:"id"`
	CenterNM    float64    `json:"centerNM"`
	ResponseXYZ Vec3       `json:"responseXYZ"`
	OffsetX     float64    `json:"offsetX"`
	OffsetY     float64    `json:"offsetY"`
	Exposures   []Exposure `json:"exposures"`
	Detail      []float64  `json:"detail"`
}

type RenderSettings struct {
	InterpolationOrder    int     `json:"interpolationOrder"`
	MaxRegistrationOffset float64 `json:"maxRegistrationOffset"`
	FusionPower           float64 `json:"fusionPower"`
	FusionStrength        float64 `json:"fusionStrength"`
	GamutIntent           string  `json:"gamutIntent"`
	TargetGamut           string  `json:"targetGamut"`
	DetailGain            float64 `json:"detailGain"`
}

func DefaultSettings() RenderSettings {
	return RenderSettings{
		InterpolationOrder:    1,
		MaxRegistrationOffset: 4.0,
		FusionPower:           2.0,
		FusionStrength:        4.0,
		GamutIntent:           "relative-colorimetric",
		TargetGamut:           "sRGB",
		DetailGain:            1.0,
	}
}

func (s RenderSettings) Validate() error {
	if s.InterpolationOrder < 0 || s.InterpolationOrder > 5 {
		return fmt.Errorf("interpolationOrder must be between 0 and 5")
	}
	if s.MaxRegistrationOffset <= 0 || s.MaxRegistrationOffset > 64 {
		return fmt.Errorf("maxRegistrationOffset must be in (0,64]")
	}
	if s.FusionPower <= 0 || s.FusionPower > 16 {
		return fmt.Errorf("fusionPower must be in (0,16]")
	}
	if s.FusionStrength <= 0 || s.FusionStrength > 32 {
		return fmt.Errorf("fusionStrength must be in (0,32]")
	}
	switch s.GamutIntent {
	case "relative-colorimetric", "perceptual", "saturation":
	default:
		return fmt.Errorf("unsupported gamut intent %q", s.GamutIntent)
	}
	if s.TargetGamut != "" && s.TargetGamut != "sRGB" {
		return fmt.Errorf("unsupported target gamut %q", s.TargetGamut)
	}
	if s.DetailGain < 0 || s.DetailGain > 4 {
		return fmt.Errorf("detailGain must be in [0,4]")
	}
	return nil
}

type Patch struct {
	Name              string          `json:"name"`
	Rect              Rect            `json:"rect"`
	BandRadiance      []float64       `json:"bandRadiance"`
	ExpectedLinearRGB Vec3            `json:"expectedLinearRGB"`
	ExpectedByIntent  map[string]Vec3 `json:"expectedByIntent,omitempty"`
	RawLinearRGB      Vec3            `json:"rawLinearRGB"`
	OutOfGamut        bool            `json:"outOfGamut"`
}

type RegistrationTruth struct {
	Reference string  `json:"reference"`
	Band      string  `json:"band"`
	OffsetX   float64 `json:"offsetX"`
	OffsetY   float64 `json:"offsetY"`
}

type Truth struct {
	PatchTolerance float64             `json:"patchTolerance"`
	PeakRadiance   float64             `json:"peakRadiance"`
	Registration   []RegistrationTruth `json:"registration"`
}

type ConversionTrace struct {
	Action        string `json:"action"`
	FromVersion   int    `json:"fromVersion"`
	ToVersion     int    `json:"toVersion"`
	Path          string `json:"path"`
	OldValue      any    `json:"oldValue"`
	DefaultValue  any    `json:"defaultValue"`
	DefaultSource string `json:"defaultSource"`
}

type Scene struct {
	SourceVersion int               `json:"sourceVersion"`
	SchemaVersion int               `json:"schemaVersion"`
	SceneID       string            `json:"sceneID"`
	Name          string            `json:"name"`
	Width         int               `json:"width"`
	Height        int               `json:"height"`
	BitDepth      int               `json:"bitDepth"`
	PixelPitchUM  float64           `json:"pixelPitchUM"`
	WhitePoint    Vec3              `json:"whitePoint"`
	Response      CameraResponse    `json:"cameraResponse"`
	Settings      RenderSettings    `json:"settings"`
	Bands         []Band            `json:"bands"`
	Patches       []Patch           `json:"patches"`
	Truth         Truth             `json:"truth"`
	Present       map[string]bool   `json:"-"`
	Trace         []ConversionTrace `json:"-"`
}

func (s *Scene) Clone() *Scene {
	if s == nil {
		return nil
	}
	out := *s
	out.Bands = make([]Band, len(s.Bands))
	for i, b := range s.Bands {
		out.Bands[i] = b.Clone()
	}
	out.Patches = append([]Patch(nil), s.Patches...)
	for i := range out.Patches {
		out.Patches[i].BandRadiance = append([]float64(nil), s.Patches[i].BandRadiance...)
		if s.Patches[i].ExpectedByIntent != nil {
			out.Patches[i].ExpectedByIntent = make(map[string]Vec3, len(s.Patches[i].ExpectedByIntent))
			for k, v := range s.Patches[i].ExpectedByIntent {
				out.Patches[i].ExpectedByIntent[k] = v
			}
		}
	}
	out.Truth.Registration = append([]RegistrationTruth(nil), s.Truth.Registration...)
	out.Present = make(map[string]bool, len(s.Present))
	for k, v := range s.Present {
		out.Present[k] = v
	}
	out.Trace = append([]ConversionTrace(nil), s.Trace...)
	return &out
}

func (b Band) Clone() Band {
	out := b
	out.Exposures = make([]Exposure, len(b.Exposures))
	for i, e := range b.Exposures {
		out.Exposures[i] = Exposure{
			DurationUS: e.DurationUS,
			Codes:      append([]uint16(nil), e.Codes...),
		}
	}
	out.Detail = append([]float64(nil), b.Detail...)
	return out
}

func (s *Scene) Validate() error {
	if s.Width <= 0 || s.Height <= 0 {
		return fmt.Errorf("invalid dimensions %dx%d", s.Width, s.Height)
	}
	if s.BitDepth < 1 || s.BitDepth > 16 {
		return fmt.Errorf("unsupported bit depth %d", s.BitDepth)
	}
	if s.PixelPitchUM <= 0 {
		return fmt.Errorf("pixel pitch must be positive")
	}
	if s.WhitePoint[0] <= 0 || s.WhitePoint[1] <= 0 || s.WhitePoint[2] <= 0 {
		return fmt.Errorf("white point must be positive XYZ")
	}
	if len(s.Bands) < 2 {
		return fmt.Errorf("at least two spectral bands are required")
	}
	if err := s.Settings.Validate(); err != nil {
		return err
	}
	if s.Response.InverseGamma <= 0 || s.Response.FullWellRadiance <= 0 {
		return fmt.Errorf("camera response must have positive gamma and full well")
	}
	n := s.Width * s.Height
	maxCode := uint32(1)<<uint(s.BitDepth) - 1
	seen := map[string]bool{}
	for i, b := range s.Bands {
		if b.ID == "" {
			return fmt.Errorf("bands[%d].id is required", i)
		}
		if seen[b.ID] {
			return fmt.Errorf("duplicate band %q", b.ID)
		}
		seen[b.ID] = true
		if b.CenterNM <= 0 {
			return fmt.Errorf("band %s center wavelength must be positive", b.ID)
		}
		if len(b.Exposures) == 0 {
			return fmt.Errorf("band %s has no exposures", b.ID)
		}
		if len(b.Detail) != n {
			return fmt.Errorf("band %s detail length %d != %d", b.ID, len(b.Detail), n)
		}
		for j, e := range b.Exposures {
			if e.DurationUS <= 0 {
				return fmt.Errorf("band %s exposure %d has invalid duration", b.ID, j)
			}
			if len(e.Codes) != n {
				return fmt.Errorf("band %s exposure %d length %d != %d", b.ID, j, len(e.Codes), n)
			}
			for _, code := range e.Codes {
				if uint32(code) > maxCode {
					return fmt.Errorf("band %s exposure %d contains code %d above %d-bit maximum", b.ID, j, code, s.BitDepth)
				}
			}
		}
	}
	return nil
}

func (s *Scene) MaxCode() float64 {
	return float64(uint32(1)<<uint(s.BitDepth) - 1)
}

func (s *Scene) BandIndex(id string) int {
	for i := range s.Bands {
		if s.Bands[i].ID == id {
			return i
		}
	}
	return -1
}

func (s *Scene) Fingerprint() string {
	h := sha256.New()
	writeString := func(v string) {
		var n [8]byte
		binary.LittleEndian.PutUint64(n[:], uint64(len(v)))
		h.Write(n[:])
		h.Write([]byte(v))
	}
	writeFloat := func(v float64) {
		var n [8]byte
		binary.LittleEndian.PutUint64(n[:], math.Float64bits(v))
		h.Write(n[:])
	}
	writeInt := func(v int64) {
		var n [8]byte
		binary.LittleEndian.PutUint64(n[:], uint64(v))
		h.Write(n[:])
	}
	writeString(s.SceneID)
	writeString(s.Name)
	writeInt(int64(s.Width))
	writeInt(int64(s.Height))
	writeInt(int64(s.BitDepth))
	writeFloat(s.PixelPitchUM)
	for _, v := range s.WhitePoint {
		writeFloat(v)
	}
	writeFloat(s.Response.InverseGamma)
	writeFloat(s.Response.FullWellRadiance)
	writeFloat(s.Response.BlackLevel)
	for _, b := range s.Bands {
		writeString(b.ID)
		writeFloat(b.CenterNM)
		for _, v := range b.ResponseXYZ {
			writeFloat(v)
		}
		writeFloat(b.OffsetX)
		writeFloat(b.OffsetY)
		for _, e := range b.Exposures {
			writeInt(e.DurationUS)
			for _, c := range e.Codes {
				h.Write([]byte{byte(c), byte(c >> 8)})
			}
		}
		for _, d := range b.Detail {
			writeFloat(d)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Scene) BandMetadataVersion() string {
	h := sha256.New()
	fmt.Fprintf(h, "%d/%d/%d/%.17g", s.Width, s.Height, s.BitDepth, s.PixelPitchUM)
	for _, v := range s.WhitePoint {
		fmt.Fprintf(h, "/%.17g", v)
	}
	for _, b := range s.Bands {
		fmt.Fprintf(h, "/%s/%.17g/%.17g/%.17g/%.17g/%.17g", b.ID, b.CenterNM, b.ResponseXYZ[0], b.ResponseXYZ[1], b.ResponseXYZ[2], b.OffsetX)
		fmt.Fprintf(h, "/%.17g", b.OffsetY)
		for _, e := range b.Exposures {
			fmt.Fprintf(h, "/%d", e.DurationUS)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s RenderSettings) Version() string {
	return fmt.Sprintf("%d/%.17g/%.17g/%.17g/%s/%s/%.17g",
		s.InterpolationOrder, s.MaxRegistrationOffset, s.FusionPower,
		s.FusionStrength, s.GamutIntent, s.TargetGamut, s.DetailGain)
}

func CompactFloat(v float64) string {
	s := fmt.Sprintf("%.8f", v)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "-0" {
		return "0"
	}
	return s
}

func SortedBandIDs(bands []Band) []string {
	ids := make([]string, len(bands))
	for i, b := range bands {
		ids[i] = b.ID
	}
	sort.Strings(ids)
	return ids
}
