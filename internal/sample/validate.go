package sample

import (
	"semanticshading/internal/apierr"
	"semanticshading/internal/colormetric"
	"semanticshading/internal/model"
)

func ValidateManifest(m model.Manifest) error {
	if m.Width <= 0 {
		return apierr.New(apierr.ESchema, "width", "width must be positive, got %d", m.Width)
	}
	if m.Height <= 0 {
		return apierr.New(apierr.ESchema, "height", "height must be positive, got %d", m.Height)
	}
	if m.Channels != 3 && m.Channels != 4 {
		return apierr.New(apierr.ESchema, "channels", "channels must be 3 or 4, got %d", m.Channels)
	}
	if m.TileSize <= 0 {
		return apierr.New(apierr.ESchema, "tile_size", "tile_size must be positive, got %d", m.TileSize)
	}
	if m.Width%m.TileSize != 0 {
		return apierr.New(apierr.ESchema, "tile_size", "tile_size %d must divide width %d", m.TileSize, m.Width).
			WithDetail("width", m.Width)
	}
	if m.Height%m.TileSize != 0 {
		return apierr.New(apierr.ESchema, "tile_size", "tile_size %d must divide height %d", m.TileSize, m.Height).
			WithDetail("height", m.Height)
	}
	if m.QuantGear < 1 || m.QuantGear > 8 {
		return apierr.New(apierr.EPara, "quant_gear", "quant_gear must be in [1,8], got %d", m.QuantGear).
			WithDetail("valid_range", [2]int{1, 8})
	}
	expectedBits := QuantBitsForGear(m.QuantGear)
	if m.QuantBits != expectedBits {
		return apierr.New(apierr.ESchema, "quant_bits", "quant_bits %d does not match gear %d (expected %d)", m.QuantBits, m.QuantGear, expectedBits)
	}
	if len(m.Categories) == 0 {
		return apierr.New(apierr.ESchema, "categories", "categories must not be empty")
	}
	spec := colormetric.Spec{Name: m.Metric, WindowRadius: m.WindowRadius}
	if err := spec.Validate(); err != nil {
		return err
	}
	if m.BandWidth < 0 || m.BandWidth > m.TileSize {
		return apierr.New(apierr.EPara, "band_width", "band_width must be in [0,%d], got %d", m.TileSize, m.BandWidth)
	}
	if m.ParamVersion == "" || m.AlgoVersion == "" {
		return apierr.New(apierr.ESchema, "version", "param_version and algo_version are required")
	}
	return nil
}

func truncatedPixel(byteCount, width, channels int) (int, int) {
	pixelIndex := byteCount / channels
	if width <= 0 {
		return 0, 0
	}
	return pixelIndex % width, pixelIndex / width
}

func Load(m model.Manifest, ref, recon, labels []byte) (*Sample, error) {
	if err := ValidateManifest(m); err != nil {
		return nil, err
	}
	pixelCount := m.Width * m.Height
	expectedPixels := pixelCount * m.Channels
	if len(ref) < expectedPixels {
		x, y := truncatedPixel(len(ref), m.Width, m.Channels)
		return nil, apierr.New(apierr.EDecode, "ref_pixels", "reference texture truncated: have %d bytes, need %d", len(ref), expectedPixels).
			WithPixel(x, y).WithDetail("expected_bytes", expectedPixels)
	}
	if len(ref) > expectedPixels {
		return nil, apierr.New(apierr.EDecode, "ref_pixels", "reference texture has %d bytes, expected %d", len(ref), expectedPixels).
			WithPixel(m.Width-1, m.Height-1).WithDetail("expected_bytes", expectedPixels)
	}
	if len(recon) < expectedPixels {
		x, y := truncatedPixel(len(recon), m.Width, m.Channels)
		return nil, apierr.New(apierr.EDecode, "recon_pixels", "reconstructed texture truncated: have %d bytes, need %d", len(recon), expectedPixels).
			WithPixel(x, y).WithDetail("expected_bytes", expectedPixels)
	}
	if len(recon) > expectedPixels {
		return nil, apierr.New(apierr.EDecode, "recon_pixels", "reconstructed texture has %d bytes, expected %d", len(recon), expectedPixels).
			WithPixel(m.Width-1, m.Height-1).WithDetail("expected_bytes", expectedPixels)
	}
	if len(labels) < pixelCount {
		x := len(labels) % m.Width
		y := len(labels) / m.Width
		return nil, apierr.New(apierr.EDecode, "labels", "label map truncated: have %d bytes, need %d", len(labels), pixelCount).
			WithPixel(x, y).WithDetail("expected_bytes", pixelCount)
	}
	if len(labels) > pixelCount {
		return nil, apierr.New(apierr.EDecode, "labels", "label map has %d bytes, expected %d", len(labels), pixelCount).
			WithPixel(m.Width-1, m.Height-1)
	}
	for y := 0; y < m.Height; y++ {
		for x := 0; x < m.Width; x++ {
			label := int(labels[y*m.Width+x])
			if label < 0 || label >= len(m.Categories) {
				return nil, apierr.New(apierr.ELabel, "labels", "label value %d is outside declared enum [0,%d)", label, len(m.Categories)).
					WithPixel(x, y).WithDetail("valid_max", len(m.Categories)-1)
			}
		}
	}
	truth, err := buildTruth(m, ref, recon, labels)
	if err != nil {
		return nil, err
	}
	s := &Sample{Manifest: m, Ref: ref, Recon: recon, Labels: labels, Truth: truth, ManifestHash: model.CanonicalHash(m)}
	s.InputHash = model.BytesHash([]byte(s.ManifestHash), ref, recon, labels)
	return s, nil
}
