package iconcut

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Bundle struct {
	Dir       string
	Manifest  *Manifest
	Source    *Image
	Reference *LabelMask
}

func jsonMarshalIndent(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

func LoadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, NewError(EDecode, path, "read manifest: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, NewError(EMeta, "manifest", "invalid JSON: %v", err)
	}
	if err := ValidateManifest(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

func ValidateManifest(m *Manifest) error {
	if m == nil {
		return NewError(EMeta, "manifest", "manifest is nil")
	}
	if m.SchemaVersion == "" {
		return NewError(EMeta, "schema_version", "must be declared")
	}
	if m.ImageID == "" {
		return NewError(EMeta, "image_id", "must be declared")
	}
	if m.Source.W <= 0 || m.Source.H <= 0 {
		return NewError(EMeta, "source", "source width and height must be positive")
	}
	if m.Corrected.W <= 0 || m.Corrected.H <= 0 {
		return NewError(EMeta, "corrected", "corrected width and height must be positive")
	}
	if m.Channels != 3 {
		return WithDetails(NewError(EMeta, "channels", "only 3-channel RGB samples are supported"), map[string]any{"actual": m.Channels, "expected": 3})
	}
	if m.Geometry.Source.W == 0 {
		m.Geometry.Source = m.Source
	}
	if m.Geometry.Corrected.W == 0 {
		m.Geometry.Corrected = m.Corrected
	}
	if m.Geometry.Source != m.Source {
		return NewError(EMeta, "geometry.source", "must equal source size")
	}
	if m.Geometry.Corrected != m.Corrected {
		return NewError(EMeta, "geometry.corrected", "must equal corrected size")
	}
	if m.Declared.Precision.CoordScale <= 0 {
		m.Declared.Precision.CoordScale = 16
	}
	if m.Geometry.CoordScale <= 0 {
		m.Geometry.CoordScale = m.Declared.Precision.CoordScale
	}
	if m.Geometry.CoordScale != m.Declared.Precision.CoordScale {
		return NewError(EMeta, "geometry.coord_scale", "must equal declared.precision.coord_scale")
	}
	if err := ValidateGeometry(m.Geometry); err != nil {
		return err
	}
	for i, c := range m.Controls {
		if !finite(c.Source.X) || !finite(c.Source.Y) {
			return NewError(EMeta, fmt.Sprintf("controls[%d].source", i), "coordinates must be finite")
		}
		if !finite(c.Corrected.X) || !finite(c.Corrected.Y) {
			return NewError(EMeta, fmt.Sprintf("controls[%d].corrected", i), "coordinates must be finite")
		}
	}
	d := m.Declared
	if d.TileWidth <= 0 {
		return NewError(EMeta, "declared.tile_width", "must be positive")
	}
	if d.TileHeight <= 0 {
		return NewError(EMeta, "declared.tile_height", "must be positive")
	}
	if m.Corrected.W%d.TileWidth != 0 {
		return WithDetails(NewError(EMeta, "declared.tile_width", "tile_width must divide corrected width"), map[string]any{
			"tile_width": d.TileWidth, "corrected_w": m.Corrected.W,
		})
	}
	if m.Corrected.H%d.TileHeight != 0 {
		return WithDetails(NewError(EMeta, "declared.tile_height", "tile_height must divide corrected height"), map[string]any{
			"tile_height": d.TileHeight, "corrected_h": m.Corrected.H,
		})
	}
	if d.InterpOrder < 0 || d.InterpOrder > 2 {
		return NewError(EMeta, "declared.interp_order", "must be in [0,2]")
	}
	if d.BandPX < 0 {
		return NewError(EMeta, "declared.band_px", "must be non-negative")
	}
	if err := ValidateSegmentParams(d.Segment); err != nil {
		return err
	}
	if err := ValidateBudget(d.Budget); err != nil {
		return err
	}
	for i, s := range d.Segment.SeedPoints {
		if s.X < 0 || s.Y < 0 || s.X >= m.Corrected.W || s.Y >= m.Corrected.H {
			return NewError(EMeta, fmt.Sprintf("declared.segment.seed_points[%d]", i), "seed is outside corrected canvas")
		}
	}
	return nil
}

func LoadBundle(dir string) (*Bundle, error) {
	m, err := LoadManifest(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	src, err := ReadPNG(filepath.Join(dir, "source.png"))
	if err != nil {
		return nil, err
	}
	if src.W != m.Source.W || src.H != m.Source.H {
		return nil, WithDetails(NewError(EMeta, "source", "decoded source dimensions do not match manifest"), map[string]any{
			"decoded": Size{W: src.W, H: src.H}, "declared": m.Source,
		})
	}
	if src.Channels != m.Channels {
		return nil, NewError(EMeta, "channels", "decoded channel count does not match manifest")
	}
	refPath := filepath.Join(dir, "reference.png")
	var ref *LabelMask
	if _, err := os.Stat(refPath); err == nil {
		ref, err = ReadLabelPNG(refPath)
		if err != nil {
			return nil, err
		}
		if ref.W != m.Corrected.W || ref.H != m.Corrected.H {
			return nil, WithDetails(NewError(EMeta, "reference", "reference dimensions do not match corrected canvas"), map[string]any{
				"decoded": Size{W: ref.W, H: ref.H}, "declared": m.Corrected,
			})
		}
	}
	return &Bundle{Dir: dir, Manifest: m, Source: src, Reference: ref}, nil
}

func (b *Bundle) TruthFingerprint() string {
	if b.Reference == nil {
		return ""
	}
	return TruthFingerprint(b.Manifest, b.Reference)
}
