package sampletruth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"example.com/vectorcore"
)

type CurveSample struct {
	Segment int                `json:"segment"`
	Points  []vectorcore.Point `json:"points"`
}

type PathTruth struct {
	PathID          string            `json:"pathId"`
	AnalyticArea    int64             `json:"analyticArea"`
	BBox            vectorcore.Bounds `json:"bbox"`
	CurveSamples    []CurveSample     `json:"curveSamples"`
	FlattenedPoints int               `json:"flattenedPoints"`
}

type Sample struct {
	TimeIndex    int               `json:"timeIndex"`
	TimeTicks    int64             `json:"timeTicks"`
	BBox         vectorcore.Bounds `json:"bbox"`
	BitmapWidth  int               `json:"bitmapWidth"`
	BitmapHeight int               `json:"bitmapHeight"`
	PixelFile    string            `json:"pixelFile"`
	PixelSHA256  string            `json:"pixelSha256"`
	Paths        []PathTruth       `json:"paths"`
}

type File struct {
	SceneID       string      `json:"sceneId"`
	SourceVersion int         `json:"sourceVersion"`
	PixelEncoding string      `json:"pixelEncoding"`
	GeneratedBy   string      `json:"generatedBy"`
	Config        ConfigTruth `json:"config"`
	Samples       []Sample    `json:"samples"`
}

type ConfigTruth struct {
	Interpolation string           `json:"interpolation"`
	Tolerance     int64            `json:"tolerance"`
	Scale         vectorcore.Scale `json:"scale"`
	Grid          int              `json:"grid"`
	FillRule      string           `json:"fillRule"`
	Margin        int              `json:"margin"`
}

func ConfigFrom(cfg vectorcore.Config) ConfigTruth {
	return ConfigTruth{
		Interpolation: string(cfg.Interpolation), Tolerance: cfg.Tolerance, Scale: cfg.Scale,
		Grid: cfg.Grid, FillRule: string(cfg.FillRule), Margin: cfg.Margin,
	}
}

func (c ConfigTruth) ToConfig() vectorcore.Config {
	cfg := vectorcore.DefaultConfig()
	cfg.Interpolation = vectorcore.InterpMode(c.Interpolation)
	cfg.Tolerance = c.Tolerance
	cfg.Scale = c.Scale
	cfg.Grid = c.Grid
	cfg.FillRule = vectorcore.FillRule(c.FillRule)
	cfg.Margin = c.Margin
	return cfg
}

func PixelHash(pixels []byte) string {
	sum := sha256.Sum256(pixels)
	return hex.EncodeToString(sum[:])
}

func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	return &file, nil
}

func Save(path string, file *File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func ReadPixels(baseDir, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(baseDir, name))
}

func CompareFile(got *vectorcore.Bitmap, path string) (x, y, maxDelta int, ok bool, err error) {
	want, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, false, err
	}
	if got == nil {
		return 0, 0, 0, false, fmt.Errorf("nil bitmap")
	}
	if len(want) != got.Width*got.Height*4 {
		return 0, 0, 0, false, fmt.Errorf("pixel file has %d bytes, expected %d", len(want), got.Width*got.Height*4)
	}
	first := true
	for py := 0; py < got.Height; py++ {
		for px := 0; px < got.Width; px++ {
			i := (py*got.Width + px) * 4
			for ch := 0; ch < 4; ch++ {
				d := int(got.Pixels[i+ch]) - int(want[i+ch])
				if d < 0 {
					d = -d
				}
				if d > maxDelta {
					maxDelta = d
				}
				if d != 0 && first {
					x, y = px, py
					first = false
				}
			}
		}
	}
	return x, y, maxDelta, maxDelta == 0, nil
}
