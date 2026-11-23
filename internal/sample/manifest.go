package sample

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"maskpipeline/internal/model"
)

const (
	ManifestName  = "sample_manifest.json"
	ImageName     = "image.rgb"
	imageMagic    = "MASKIMG1"
	imageHdrSize  = 24
	SchemaVersion = 1
)

type Tolerances struct {
	RegionCount int     `json:"region_count"`
	CentroidPx  float64 `json:"centroid_px"`
	AreaRel     float64 `json:"area_rel"`
}

type ImageRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type TruthRef struct {
	RegionCount int    `json:"region_count"`
	MaskPath    string `json:"mask_path"`
	MaskSHA256  string `json:"mask_sha256"`
	Fingerprint string `json:"fingerprint"`
}

type RegionTruth struct {
	ID         int          `json:"id"`
	Name       string       `json:"name"`
	Seed       model.Point  `json:"seed"`
	RGB        model.RGB    `json:"rgb"`
	Area       int          `json:"area"`
	Centroid   model.PointF `json:"centroid"`
	BBox       model.Rect   `json:"bbox"`
	MaskPath   string       `json:"mask_path"`
	MaskSHA256 string       `json:"mask_sha256"`
}

type ParameterDefaults struct {
	ColorTolerance       float64 `json:"color_tolerance"`
	GradientThreshold    float64 `json:"gradient_threshold"`
	StructureElementSize int     `json:"structure_element_size"`
}

type Manifest struct {
	SchemaVersion     int               `json:"schema_version"`
	SampleID          string            `json:"sample_id"`
	Seed              int64             `json:"seed"`
	Width             int               `json:"width"`
	Height            int               `json:"height"`
	Channels          int               `json:"channels"`
	TileSize          int               `json:"tile_size"`
	PreviewDeadlineMS int               `json:"preview_deadline_ms"`
	AlgorithmVersion  string            `json:"algorithm_version"`
	Tolerances        Tolerances        `json:"tolerances"`
	DefaultParams     ParameterDefaults `json:"default_params"`
	Image             ImageRef          `json:"image"`
	Truth             TruthRef          `json:"truth"`
	Regions           []RegionTruth     `json:"regions"`
}

func (m Manifest) DefaultsWithSeeds() model.Params {
	seeds := make([]model.Point, 0, len(m.Regions))
	for _, r := range m.Regions {
		seeds = append(seeds, r.Seed)
	}
	return model.NormalizeParams(model.Params{
		ColorTolerance:       m.DefaultParams.ColorTolerance,
		GradientThreshold:    m.DefaultParams.GradientThreshold,
		StructureElementSize: m.DefaultParams.StructureElementSize,
		Seeds:                seeds,
	})
}

func (m Manifest) Fingerprint() string {
	view := m
	view.Truth.Fingerprint = ""
	return model.HashJSON(view)
}

func EncodeImage(im *model.Image) []byte {
	out := make([]byte, imageHdrSize+len(im.Pix))
	copy(out[:8], imageMagic)
	binary.BigEndian.PutUint32(out[8:12], uint32(im.Width))
	binary.BigEndian.PutUint32(out[12:16], uint32(im.Height))
	out[16] = uint8(im.Channels)
	copy(out[imageHdrSize:], im.Pix)
	return out
}

type imageHeader struct {
	Width    int
	Height   int
	Channels int
}

func decodeImageHeader(data []byte) (imageHeader, error) {
	if len(data) < imageHdrSize {
		return imageHeader{}, fmt.Errorf("image header truncated: got %d bytes, need %d", len(data), imageHdrSize)
	}
	if string(data[:8]) != imageMagic {
		return imageHeader{}, fmt.Errorf("bad image magic")
	}
	w := int(binary.BigEndian.Uint32(data[8:12]))
	h := int(binary.BigEndian.Uint32(data[12:16]))
	ch := int(data[16])
	if w <= 0 || h <= 0 || ch <= 0 || ch > 4 {
		return imageHeader{}, fmt.Errorf("invalid image header dimensions/channels")
	}
	return imageHeader{Width: w, Height: h, Channels: ch}, nil
}

func safeJoin(base, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty relative path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path not allowed: %s", rel)
	}
	cleaned := filepath.Clean(filepath.FromSlash(rel))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes sample directory: %s", rel)
	}
	full := filepath.Join(base, cleaned)
	check, err := filepath.Rel(base, full)
	if err != nil || check == ".." || strings.HasPrefix(check, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes sample directory: %s", rel)
	}
	return full, nil
}

func readFileRequired(path, field, op string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errorfDecode(op, field, "required file is missing: %s", filepath.Base(path))
		}
		return nil, errorfDecode(op, field, "cannot read required file %s: %v", filepath.Base(path), err)
	}
	return data, nil
}
