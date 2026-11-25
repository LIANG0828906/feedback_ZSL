// Package domain contains the on-disk scene schema and deterministic hashing
// helpers shared by the generator, renderer, and verifier.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
)

const SchemaVersion = 1

// Scene is the deterministic screen-space geometry and material manifest.
type Scene struct {
	SchemaVersion int              `json:"schema_version"`
	Seed          int64            `json:"seed"`
	Width         int              `json:"width"`
	Height        int              `json:"height"`
	Background    [3]float64       `json:"background"`
	Meshes        []Mesh           `json:"meshes"`
	Objects       []ObjectSpectrum `json:"objects"`
}

// Mesh is a screen-space triangle mesh. Every vertex owns five material
// channels: albedo (3), roughness (1), and metallic (1).
type Mesh struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	VertexCount      int      `json:"vertex_count"`
	MaterialChannels int      `json:"material_channels"`
	Vertices         []Vertex `json:"vertices"`
	Indices          []int    `json:"indices"`
}

type Vertex struct {
	Position  [3]float64 `json:"position"`
	Normal    [3]float64 `json:"normal"`
	Albedo    [3]float64 `json:"albedo"`
	Roughness float64    `json:"roughness"`
	Metallic  float64    `json:"metallic"`
}

type ObjectSpectrum struct {
	MeshID       string  `json:"mesh_id"`
	BasisBits    int     `json:"basis_bits"`
	BasisSigned  bool    `json:"basis_signed"`
	Coefficients []int64 `json:"coefficients"`
}

// SpectralData contains every wavelength-dependent table. Every table uses the
// exact same wavelength sampling as Wavelengths.
type SpectralData struct {
	SchemaVersion int             `json:"schema_version"`
	Wavelengths   []float64       `json:"wavelengths"`
	CMF           ColorMatching   `json:"cmf"`
	WhitePoint    [3]float64      `json:"white_point"`
	Gamut         Gamut           `json:"gamut"`
	Basis         []BasisFunction `json:"basis"`
	Lights        []Light         `json:"lights"`
}

type ColorMatching struct {
	X []float64 `json:"x"`
	Y []float64 `json:"y"`
	Z []float64 `json:"z"`
}

type Gamut struct {
	Name     string        `json:"name"`
	RGBToXYZ [3][3]float64 `json:"rgb_to_xyz"`
	XYZToRGB [3][3]float64 `json:"xyz_to_rgb"`
}

type BasisFunction struct {
	ID     string    `json:"id"`
	Values []float64 `json:"values"`
}

type Light struct {
	ID        string     `json:"id"`
	Direction [3]float64 `json:"direction"`
	Intensity []float64  `json:"intensity"`
}

// Quantization declares the fixed-point units used during integration.
type Quantization struct {
	AlbedoBits    int `json:"albedo_bits"`
	RoughnessBits int `json:"roughness_bits"`
	MetallicBits  int `json:"metallic_bits"`
	BasisBits     int `json:"basis_bits"`
}

func DefaultQuantization() Quantization {
	return Quantization{AlbedoBits: 8, RoughnessBits: 8, MetallicBits: 8, BasisBits: 12}
}

// GamutInjection is a deterministic test hook for the E_GAMUT path. It is
// intentionally part of the serialized parameter snapshot so injected tests
// still obey version isolation.
type GamutInjection struct {
	X       int     `json:"x"`
	Y       int     `json:"y"`
	Channel string  `json:"channel"`
	Value   float64 `json:"value"`
}

// RenderConfig is a complete, immutable parameter snapshot.
type RenderConfig struct {
	WavelengthSteps      int             `json:"wavelength_steps"`
	Integration          string          `json:"integration"`
	TileSize             int             `json:"tile_size"`
	MaxWorkers           int             `json:"max_workers"`
	PerThreadPixelBuffer int             `json:"per_thread_pixel_buffer"`
	QueueBudget          int             `json:"queue_budget"`
	IndirectBounces      int             `json:"indirect_bounces"`
	Exposure             float64         `json:"exposure"`
	ToneCurve            string          `json:"tone_curve"`
	Gamma                float64         `json:"gamma"`
	SeamThreshold        float64         `json:"seam_threshold"`
	Quantization         Quantization    `json:"quantization"`
	PreviewTolerance     float64         `json:"preview_tolerance"`
	ColorTolerance       float64         `json:"color_tolerance"`
	MaxRetries           int             `json:"max_retries"`
	InjectedGamut        *GamutInjection `json:"injected_gamut,omitempty"`
}

func DefaultConfig() RenderConfig {
	return RenderConfig{
		WavelengthSteps:      17,
		Integration:          "trapezoid",
		TileSize:             32,
		MaxWorkers:           3,
		PerThreadPixelBuffer: 2048,
		QueueBudget:          6,
		IndirectBounces:      1,
		Exposure:             0,
		ToneCurve:            "aces",
		Gamma:                2.2,
		SeamThreshold:        0.08,
		Quantization:         DefaultQuantization(),
		PreviewTolerance:     1e-9,
		ColorTolerance:       2e-4,
		MaxRetries:           2,
	}
}

type Truth struct {
	SceneHash    string       `json:"scene_hash"`
	Source       string       `json:"source"`
	Tolerance    float64      `json:"tolerance"`
	Points       []TruthPoint `json:"points"`
	PeakRadiance float64      `json:"peak_radiance"`
}

type TruthPoint struct {
	X                    int        `json:"x"`
	Y                    int        `json:"y"`
	ObjectID             string     `json:"object_id"`
	ExpectedLinearRGB    [3]float64 `json:"expected_linear_rgb"`
	ExpectedPeakRadiance float64    `json:"expected_peak_radiance"`
	ExpectedChromaticity [2]float64 `json:"expected_chromaticity"`
}

type SampleBundle struct {
	Scene        *Scene
	Spectral     *SpectralData
	Truth        *Truth
	Config       *RenderConfig
	SceneHash    string
	SpectralHash string
	ContentHash  string
}

// CanonicalHash hashes the canonical JSON encoding of v.
func CanonicalHash(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func MustHash(v any) string {
	h, err := CanonicalHash(v)
	if err != nil {
		panic(err)
	}
	return h
}

func (s *SampleBundle) FinishHashes() error {
	sh, err := CanonicalHash(s.Scene)
	if err != nil {
		return err
	}
	ph, err := CanonicalHash(s.Spectral)
	if err != nil {
		return err
	}
	s.SceneHash = sh
	s.SpectralHash = ph
	s.ContentHash = MustHash(struct {
		Scene    string `json:"scene"`
		Spectral string `json:"spectral"`
	}{sh, ph})
	return nil
}

func (q Quantization) Signature() string {
	return MustHash(q)
}

// SamplingVersion includes everything that can change a spectral accumulation
// independently of scene content.
func SamplingVersion(s *SpectralData, c RenderConfig) string {
	sh, _ := CanonicalHash(s)
	return MustHash(struct {
		Spectral     string       `json:"spectral"`
		Steps        int          `json:"steps"`
		Integration  string       `json:"integration"`
		Quantization Quantization `json:"quantization"`
		Indirect     int          `json:"indirect_bounces"`
	}{
		Spectral:     sh,
		Steps:        c.WavelengthSteps,
		Integration:  c.Integration,
		Quantization: c.Quantization,
		Indirect:     c.IndirectBounces,
	})
}

func ConfigVersion(c RenderConfig) string {
	return MustHash(c)
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func ObjectContentHash(scene *Scene, obj ObjectSpectrum) string {
	var mesh *Mesh
	for i := range scene.Meshes {
		if scene.Meshes[i].ID == obj.MeshID {
			mesh = &scene.Meshes[i]
			break
		}
	}
	return MustHash(struct {
		Mesh   *Mesh          `json:"mesh"`
		Object ObjectSpectrum `json:"object"`
	}{mesh, obj})
}

func ObjectMaterialKey(scene *Scene, obj ObjectSpectrum, q Quantization) string {
	return hashText(ObjectContentHash(scene, obj) + "|" + q.Signature())
}

func Clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(0, math.Min(1, v))
}

func Luminance(rgb [3]float64) float64 {
	return 0.2126*rgb[0] + 0.7152*rgb[1] + 0.0722*rgb[2]
}

func (t *Truth) Pointer() *TruthPoint {
	if len(t.Points) == 0 {
		return nil
	}
	return &t.Points[0]
}

func (s *Scene) String() string {
	return fmt.Sprintf("scene(%dx%d, %d meshes)", s.Width, s.Height, len(s.Meshes))
}
