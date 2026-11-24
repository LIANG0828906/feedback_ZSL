package spectralrender

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

const (
	AlgorithmVersion = "spectralrender/2"
	sceneSchema      = 1
	truthSchema      = 1
)

// RGB is a three-component linear-light value. XYZ uses the same shape.
type RGB [3]float64

func (v RGB) Add(o RGB) RGB { return RGB{v[0] + o[0], v[1] + o[1], v[2] + o[2]} }
func (v RGB) Mul(s float64) RGB {
	return RGB{v[0] * s, v[1] * s, v[2] * s}
}
func (v RGB) MulComponent(o RGB) RGB {
	return RGB{v[0] * o[0], v[1] * o[1], v[2] * o[2]}
}
func (v RGB) Max() float64 { return math.Max(v[0], math.Max(v[1], v[2])) }
func (v RGB) Min() float64 { return math.Min(v[0], math.Min(v[1], v[2])) }
func (v RGB) IsFinite() bool {
	return isFinite(v[0]) && isFinite(v[1]) && isFinite(v[2])
}

type Quantization struct {
	VertexBits      int   `json:"vertex_bits"`
	VertexUnit      int64 `json:"vertex_unit"`
	CoefficientBits int   `json:"coefficient_bits"`
	CoefficientUnit int64 `json:"coefficient_unit"`
}

type EffectiveQuantization struct {
	VertexBits      int   `json:"vertex_bits"`
	VertexUnit      int64 `json:"vertex_unit"`
	CoefficientBits int   `json:"coefficient_bits"`
	CoefficientUnit int64 `json:"coefficient_unit"`
}

type CMF struct {
	X []float64 `json:"x"`
	Y []float64 `json:"y"`
	Z []float64 `json:"z"`
}

type Spectrum struct {
	StartNM     float64       `json:"start_nm"`
	EndNM       float64       `json:"end_nm"`
	StepNM      float64       `json:"step_nm"`
	Wavelengths []float64     `json:"wavelengths"`
	CMF         CMF           `json:"cmf"`
	WhitePoint  [3]float64    `json:"white_point_xyz"`
	XYZToRGB    [3][3]float64 `json:"xyz_to_rgb"`
	TargetGamut string        `json:"target_gamut"`
}

type BasisFunction struct {
	Name   string    `json:"name"`
	Values []float64 `json:"values"`
}

type Material struct {
	ID              string          `json:"id"`
	Basis           []BasisFunction `json:"basis"`
	Coefficients    []int64         `json:"coefficients"`
	CoefficientBits int             `json:"coefficient_bits"`
	CoefficientUnit int64           `json:"coefficient_unit"`
}

type Vertex struct {
	Position  [3]float64 `json:"position"`
	Normal    [3]float64 `json:"normal"`
	Albedo    [3]int64   `json:"albedo"`
	Roughness int64      `json:"roughness"`
	Metallic  int64      `json:"metallic"`
}

type Mesh struct {
	ID         string   `json:"id"`
	MaterialID string   `json:"material_id"`
	Vertices   []Vertex `json:"vertices"`
	Triangles  [][3]int `json:"triangles"`
}

type Light struct {
	ID        string     `json:"id"`
	Direction [3]float64 `json:"direction"`
	Intensity []float64  `json:"intensity"`
}

type Scene struct {
	SchemaVersion int          `json:"schema_version"`
	Seed          int64        `json:"seed"`
	Width         int          `json:"width"`
	Height        int          `json:"height"`
	Quantization  Quantization `json:"quantization"`
	Spectrum      Spectrum     `json:"spectrum"`
	Materials     []Material   `json:"materials"`
	Meshes        []Mesh       `json:"meshes"`
	Lights        []Light      `json:"lights"`
	BackgroundRGB RGB          `json:"background_linear_rgb"`
}

type TruthReference struct {
	SampleCount    int     `json:"sample_count"`
	Integration    string  `json:"integration"`
	Bounces        int     `json:"bounces"`
	IndirectWeight float64 `json:"indirect_weight"`
}

type TruthPixel struct {
	X                 int        `json:"x"`
	Y                 int        `json:"y"`
	LinearRGB         RGB        `json:"linear_rgb"`
	Tolerance         float64    `json:"tolerance"`
	PeakRadiance      float64    `json:"peak_radiance"`
	PeakTolerance     float64    `json:"peak_tolerance"`
	ChromaticityXY    [2]float64 `json:"chromaticity_xy"`
	ChromaticityBound float64    `json:"chromaticity_bound"`
}

type TruthBounds struct {
	LinearRGBAbs    float64 `json:"linear_rgb_abs"`
	PeakRadianceRel float64 `json:"peak_radiance_rel"`
	ChromaticityXY  float64 `json:"chromaticity_xy"`
}

type TruthManifest struct {
	SchemaVersion int            `json:"schema_version"`
	SceneHash     string         `json:"scene_hash"`
	Reference     TruthReference `json:"reference"`
	Pixels        []TruthPixel   `json:"pixels"`
	Bounds        TruthBounds    `json:"bounds"`
}

type SampleManifest struct {
	SchemaVersion int    `json:"schema_version"`
	Generator     string `json:"generator"`
	Seed          int64  `json:"seed"`
	SceneFile     string `json:"scene_file"`
	CMFFile       string `json:"cmf_file"`
	TruthFile     string `json:"truth_file"`
	SceneHash     string `json:"scene_hash"`
	CMFHash       string `json:"cmf_hash"`
	TruthHash     string `json:"truth_hash"`
}

type CMFDocument struct {
	SchemaVersion int        `json:"schema_version"`
	StartNM       float64    `json:"start_nm"`
	EndNM         float64    `json:"end_nm"`
	StepNM        float64    `json:"step_nm"`
	Wavelengths   []float64  `json:"wavelengths"`
	CMF           CMF        `json:"cmf"`
	WhitePoint    [3]float64 `json:"white_point_xyz"`
	TargetGamut   string     `json:"target_gamut"`
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func finiteRGB(v RGB) bool { return v.IsFinite() }

func normalized(v [3]float64) [3]float64 {
	n := math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
	if n == 0 || !isFinite(n) {
		return [3]float64{0, 0, 1}
	}
	return [3]float64{v[0] / n, v[1] / n, v[2] / n}
}

func dot(a, b [3]float64) float64 {
	return a[0]*b[0] + a[1]*b[1] + a[2]*b[2]
}

func hashBytes(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		var size [8]byte
		n := uint64(len(p))
		for i := 7; i >= 0; i-- {
			size[i] = byte(n)
			n >>= 8
		}
		h.Write(size[:])
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sha256HexBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hashString(parts ...string) string {
	bs := make([][]byte, len(parts))
	for i := range parts {
		bs[i] = []byte(parts[i])
	}
	return hashBytes(bs...)
}

func HashScene(scene *Scene) string {
	b, err := json.Marshal(scene)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func HashTruth(truth *TruthManifest) string {
	b, err := json.Marshal(truth)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func interpolateTable(xs, ys []float64, x float64) float64 {
	if len(xs) == 0 || len(ys) == 0 {
		return 0
	}
	if len(xs) == 1 {
		return ys[0]
	}
	if x <= xs[0] {
		return ys[0]
	}
	if x >= xs[len(xs)-1] {
		return ys[len(ys)-1]
	}
	i := sort.Search(len(xs), func(i int) bool { return xs[i] >= x })
	if i == 0 {
		return ys[0]
	}
	if xs[i] == x {
		return ys[i]
	}
	t := (x - xs[i-1]) / (xs[i] - xs[i-1])
	return ys[i-1] + t*(ys[i]-ys[i-1])
}

func roundToUnit(v float64, unit int64) int64 {
	if unit <= 0 {
		return 0
	}
	return int64(math.Round(v * float64(unit)))
}

func quantizedUnit(bits int) int64 {
	if bits <= 0 || bits >= 62 {
		return 0
	}
	// Power-of-two grids are nested: every coarser grid point remains
	// representable at the next bit depth. This makes nearest-quantization
	// error monotonic as bit depth increases.
	return int64(1) << bits
}

func matrixVector(m [3][3]float64, v [3]float64) [3]float64 {
	return [3]float64{
		m[0][0]*v[0] + m[0][1]*v[1] + m[0][2]*v[2],
		m[1][0]*v[0] + m[1][1]*v[1] + m[1][2]*v[2],
		m[2][0]*v[0] + m[2][1]*v[1] + m[2][2]*v[2],
	}
}

func formatFloat(v float64) string {
	return fmt.Sprintf("%.17g", v)
}
