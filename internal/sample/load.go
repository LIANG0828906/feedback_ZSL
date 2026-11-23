package sample

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"math"
	"path/filepath"

	"maskpipeline/internal/model"
)

type Sample struct {
	Dir              string
	Manifest         Manifest
	Image            *model.Image
	TruthLabels      []byte
	TruthStats       model.Stats
	ImageSHA256      string
	TruthFingerprint string
}

func (s *Sample) TruthLabelAt(x, y int) int {
	return int(s.TruthLabels[y*s.Manifest.Width+x])
}

func LoadManifest(dir string) (Manifest, error) {
	path, err := safeJoin(dir, ManifestName)
	if err != nil {
		return Manifest{}, errorfDecode("load_manifest", "manifest", "%v", err)
	}
	data, err := readFileRequired(path, "manifest", "load_manifest")
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, errorfDecode("load_manifest", "manifest", "invalid manifest JSON: %v", err)
	}
	if err := validateManifestShape(m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func Load(dir string) (*Sample, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}

	imgPath, err := safeJoin(dir, m.Image.Path)
	if err != nil {
		return nil, errorfDecode("load_sample", "image.path", "%v", err)
	}
	raw, err := readFileRequired(imgPath, "image", "load_sample")
	if err != nil {
		return nil, err
	}
	hdr, err := decodeImageHeader(raw)
	if err != nil {
		return nil, errorfDecode("load_sample", "image", "%v", err).WithDetail("path", m.Image.Path)
	}
	if hdr.Width != m.Width {
		return nil, errorfMeta("load_sample", "width", "image header width differs from manifest").
			WithDetail("manifest", m.Width).WithDetail("image", hdr.Width)
	}
	if hdr.Height != m.Height {
		return nil, errorfMeta("load_sample", "height", "image header height differs from manifest").
			WithDetail("manifest", m.Height).WithDetail("image", hdr.Height)
	}
	if hdr.Channels != m.Channels {
		return nil, errorfMeta("load_sample", "channels", "image header channels differs from manifest").
			WithDetail("manifest", m.Channels).WithDetail("image", hdr.Channels)
	}
	if m.Channels != 3 {
		return nil, errorfMeta("load_sample", "channels", "only 3-channel RGB samples are supported").
			WithDetail("manifest", m.Channels)
	}
	want := hdr.Width * hdr.Height * hdr.Channels
	if len(raw)-imageHdrSize != want {
		return nil, errorfDecode("load_sample", "image", "image payload is corrupted or truncated").
			WithDetail("want_bytes", want).
			WithDetail("got_bytes", len(raw)-imageHdrSize)
	}
	imageHash := model.HashBytes(raw)
	if imageHash != m.Image.SHA256 {
		return nil, errorfDecode("load_sample", "image.sha256", "image content hash mismatch").
			WithDetail("manifest", m.Image.SHA256).
			WithDetail("actual", imageHash)
	}

	labels := make([]byte, m.Width*m.Height)
	regions := make([]model.RegionStats, 0, len(m.Regions))
	for i, rt := range m.Regions {
		maskPath, err := safeJoin(dir, rt.MaskPath)
		if err != nil {
			return nil, errorfDecode("load_sample", fmt.Sprintf("regions[%d].mask_path", i), "%v", err)
		}
		maskData, err := readFileRequired(maskPath, fmt.Sprintf("regions[%d].mask", i), "load_sample")
		if err != nil {
			return nil, err
		}
		if got := model.HashBytes(maskData); got != rt.MaskSHA256 {
			return nil, errorfDecode("load_sample", fmt.Sprintf("regions[%d].mask_sha256", i), "truth mask hash mismatch").
				WithDetail("manifest", rt.MaskSHA256).
				WithDetail("actual", got)
		}
		img, err := png.Decode(bytes.NewReader(maskData))
		if err != nil {
			return nil, errorfDecode("load_sample", fmt.Sprintf("regions[%d].mask", i), "truth mask PNG is corrupted: %v", err)
		}
		if err := checkMaskBounds(img, m.Width, m.Height, fmt.Sprintf("regions[%d].mask", i)); err != nil {
			return nil, err
		}
		area := 0
		var sumX, sumY int64
		minX, minY, maxX, maxY := m.Width, m.Height, -1, -1
		for y := 0; y < m.Height; y++ {
			for x := 0; x < m.Width; x++ {
				r, _, _, _ := img.At(x, y).RGBA()
				if r <= 0x7fff {
					continue
				}
				idx := y*m.Width + x
				if labels[idx] != 0 {
					return nil, errorfMeta("load_sample", "regions", "truth region masks overlap").
						WithDetail("pixel", model.Point{X: x, Y: y})
				}
				labels[idx] = byte(rt.ID)
				area++
				sumX += int64(x)
				sumY += int64(y)
				if x < minX {
					minX = x
				}
				if y < minY {
					minY = y
				}
				if x > maxX {
					maxX = x
				}
				if y > maxY {
					maxY = y
				}
			}
		}
		if area == 0 {
			return nil, errorfMeta("load_sample", fmt.Sprintf("regions[%d].area", i), "truth region is empty")
		}
		actual := model.RegionStats{
			ID:       rt.ID,
			Seed:     rt.Seed,
			Name:     rt.Name,
			Area:     area,
			Centroid: model.PointF{X: float64(sumX) / float64(area), Y: float64(sumY) / float64(area)},
			BBox:     model.Rect{X: minX, Y: minY, W: maxX - minX + 1, H: maxY - minY + 1},
		}
		if err := compareTruthRegion(i, rt, actual); err != nil {
			return nil, err
		}
		if rt.Seed.X < 0 || rt.Seed.X >= m.Width || rt.Seed.Y < 0 || rt.Seed.Y >= m.Height {
			return nil, errorfMeta("load_sample", fmt.Sprintf("regions[%d].seed", i), "truth seed is outside the image")
		}
		if labels[rt.Seed.Y*m.Width+rt.Seed.X] != byte(rt.ID) {
			return nil, errorfMeta("load_sample", fmt.Sprintf("regions[%d].seed", i), "truth seed is not inside its reference mask")
		}
		regions = append(regions, actual)
	}

	combinedPath, err := safeJoin(dir, m.Truth.MaskPath)
	if err != nil {
		return nil, errorfDecode("load_sample", "truth.mask_path", "%v", err)
	}
	combinedData, err := readFileRequired(combinedPath, "truth.mask", "load_sample")
	if err != nil {
		return nil, err
	}
	if got := model.HashBytes(combinedData); got != m.Truth.MaskSHA256 {
		return nil, errorfDecode("load_sample", "truth.mask_sha256", "combined truth mask hash mismatch").
			WithDetail("manifest", m.Truth.MaskSHA256).
			WithDetail("actual", got)
	}
	combined, err := png.Decode(bytes.NewReader(combinedData))
	if err != nil {
		return nil, errorfDecode("load_sample", "truth.mask", "combined truth mask PNG is corrupted: %v", err)
	}
	if err := checkMaskBounds(combined, m.Width, m.Height, "truth.mask"); err != nil {
		return nil, err
	}
	for y := 0; y < m.Height; y++ {
		for x := 0; x < m.Width; x++ {
			gray := combined.At(x, y)
			r, _, _, _ := gray.RGBA()
			v := byte(r >> 8)
			if v != labels[y*m.Width+x] {
				return nil, errorfMeta("load_sample", "truth.mask", "combined truth mask disagrees with per-region masks").
					WithDetail("pixel", model.Point{X: x, Y: y}).
					WithDetail("combined", v).
					WithDetail("regions", labels[y*m.Width+x])
			}
		}
	}

	stats := model.StatsFromLabels(labels, m.Width, m.Height)
	if stats.RegionCount != m.Truth.RegionCount {
		return nil, errorfMeta("load_sample", "truth.region_count", "truth region count differs from decoded masks").
			WithDetail("manifest", m.Truth.RegionCount).
			WithDetail("decoded", stats.RegionCount)
	}
	for i := range stats.Regions {
		if i < len(regions) {
			stats.Regions[i].Seed = regions[i].Seed
			stats.Regions[i].Name = regions[i].Name
		}
	}
	fingerprint := m.Fingerprint()
	if fingerprint != m.Truth.Fingerprint {
		return nil, errorfMeta("load_sample", "truth.fingerprint", "truth fingerprint mismatch").
			WithDetail("manifest", m.Truth.Fingerprint).
			WithDetail("actual", fingerprint)
	}

	im := &model.Image{
		Width:    hdr.Width,
		Height:   hdr.Height,
		Channels: hdr.Channels,
		Pix:      append([]byte(nil), raw[imageHdrSize:]...),
	}
	return &Sample{
		Dir:              filepath.Clean(dir),
		Manifest:         m,
		Image:            im,
		TruthLabels:      labels,
		TruthStats:       stats,
		ImageSHA256:      imageHash,
		TruthFingerprint: fingerprint,
	}, nil
}

func validateManifestShape(m Manifest) error {
	if m.SchemaVersion != SchemaVersion {
		return errorfMeta("load_manifest", "schema_version", "unsupported manifest schema").
			WithDetail("want", SchemaVersion).WithDetail("got", m.SchemaVersion)
	}
	if m.SampleID == "" {
		return errorfMeta("load_manifest", "sample_id", "sample id is required")
	}
	if m.Width <= 0 {
		return errorfMeta("load_manifest", "width", "width must be positive")
	}
	if m.Height <= 0 {
		return errorfMeta("load_manifest", "height", "height must be positive")
	}
	if m.Channels != 3 {
		return errorfMeta("load_manifest", "channels", "sample channels must be 3")
	}
	if m.TileSize <= 0 || m.TileSize > max(m.Width, m.Height) {
		return errorfMeta("load_manifest", "tile_size", "tile size is out of range")
	}
	if m.PreviewDeadlineMS <= 0 {
		return errorfMeta("load_manifest", "preview_deadline_ms", "preview deadline must be positive")
	}
	if m.Tolerances.RegionCount < 0 {
		return errorfMeta("load_manifest", "tolerances.region_count", "region count tolerance cannot be negative")
	}
	if m.Tolerances.CentroidPx < 0 || math.IsNaN(m.Tolerances.CentroidPx) || math.IsInf(m.Tolerances.CentroidPx, 0) {
		return errorfMeta("load_manifest", "tolerances.centroid_px", "centroid tolerance must be finite and non-negative")
	}
	if m.Tolerances.AreaRel < 0 || math.IsNaN(m.Tolerances.AreaRel) || math.IsInf(m.Tolerances.AreaRel, 0) {
		return errorfMeta("load_manifest", "tolerances.area_rel", "area tolerance must be finite and non-negative")
	}
	if m.AlgorithmVersion == "" {
		return errorfMeta("load_manifest", "algorithm_version", "algorithm version is required")
	}
	if len(m.Regions) == 0 {
		return errorfMeta("load_manifest", "regions", "at least one truth region is required")
	}
	if m.Truth.RegionCount != len(m.Regions) {
		return errorfMeta("load_manifest", "truth.region_count", "declared region count differs from region list").
			WithDetail("declared", m.Truth.RegionCount).WithDetail("regions", len(m.Regions))
	}
	if m.Image.Path == "" || m.Image.SHA256 == "" {
		return errorfMeta("load_manifest", "image", "image path and hash are required")
	}
	if m.Truth.MaskPath == "" || m.Truth.MaskSHA256 == "" {
		return errorfMeta("load_manifest", "truth.mask_path", "truth mask path and hash are required")
	}
	seen := make(map[int]bool, len(m.Regions))
	for i, r := range m.Regions {
		if r.ID <= 0 || r.ID > 255 {
			return errorfMeta("load_manifest", fmt.Sprintf("regions[%d].id", i), "region id must be in [1,255]")
		}
		if seen[r.ID] {
			return errorfMeta("load_manifest", fmt.Sprintf("regions[%d].id", i), "duplicate region id")
		}
		seen[r.ID] = true
	}
	return nil
}

func checkMaskBounds(img image.Image, width, height int, field string) error {
	b := img.Bounds()
	if b.Dx() != width {
		return errorfMeta("load_sample", field, "truth mask width differs from manifest").
			WithDetail("manifest", width).WithDetail("mask", b.Dx())
	}
	if b.Dy() != height {
		return errorfMeta("load_sample", field, "truth mask height differs from manifest").
			WithDetail("manifest", height).WithDetail("mask", b.Dy())
	}
	return nil
}

func compareTruthRegion(index int, declared RegionTruth, actual model.RegionStats) error {
	if declared.Area != actual.Area {
		return errorfMeta("load_sample", fmt.Sprintf("regions[%d].area", index), "declared truth area differs from mask").
			WithDetail("manifest", declared.Area).WithDetail("actual", actual.Area)
	}
	if declared.BBox != actual.BBox {
		return errorfMeta("load_sample", fmt.Sprintf("regions[%d].bbox", index), "declared truth bbox differs from mask").
			WithDetail("manifest", declared.BBox).WithDetail("actual", actual.BBox)
	}
	const eps = 1e-9
	if math.Abs(declared.Centroid.X-actual.Centroid.X) > eps || math.Abs(declared.Centroid.Y-actual.Centroid.Y) > eps {
		return errorfMeta("load_sample", fmt.Sprintf("regions[%d].centroid", index), "declared truth centroid differs from mask").
			WithDetail("manifest", declared.Centroid).WithDetail("actual", actual.Centroid)
	}
	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
