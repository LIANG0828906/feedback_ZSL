package volume

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"volumelab/pkg/model"
)

const ManifestFormat = "volumelab.manifest/v1"

type Manifest struct {
	Format               string       `json:"format"`
	Seed                 int64        `json:"seed"`
	PrecisionToleranceMM float64      `json:"precisionToleranceMM"`
	Volumes              []VolumeDecl `json:"volumes"`
	ManifestPath         string       `json:"-"`
	ManifestHash         string       `json:"-"`
}

type VolumeDecl struct {
	ID         string            `json:"id"`
	File       string            `json:"file"`
	AxisOrder  string            `json:"axisOrder"`
	Dimensions []int             `json:"dimensions"`
	Spacing    []*float64        `json:"spacing"`
	Slope      *float64          `json:"slope"`
	Intercept  *float64          `json:"intercept"`
	LayerCount *int              `json:"layerCount"`
	SHA256     string            `json:"sha256"`
	GrayRange  []*float64        `json:"grayRange"`
	Structures []model.Structure `json:"structures"`
}

type ResolvedSpec struct {
	ID           string
	FilePath     string
	AxisOrder    string
	Dimensions   [3]int
	Spacing      [3]float64
	Slope        float64
	Intercept    float64
	LayerCount   int
	SHA256       string
	GrayRange    [2]float64
	Structures   []model.Structure
	ToleranceMM  float64
	ManifestPath string
	ManifestHash string
}

func LoadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &model.AppError{Code: "E_IO", Message: "cannot read manifest", Field: "manifestPath", Details: map[string]any{"path": path, "cause": err.Error()}}
	}
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, &model.AppError{Code: "E_DECODE", Message: "manifest is not valid JSON", Field: "manifest", Details: map[string]any{"cause": err.Error()}}
	}
	if m.Format != ManifestFormat {
		return nil, &model.AppError{Code: "E_VERSION", Message: "unsupported manifest format", Field: "format", Actual: m.Format, Allowed: ManifestFormat}
	}
	if m.PrecisionToleranceMM <= 0 {
		return nil, &model.AppError{Code: "E_CAL", Message: "precision tolerance must be positive", Field: "precisionToleranceMM", Actual: m.PrecisionToleranceMM, Allowed: "> 0"}
	}
	if len(m.Volumes) == 0 {
		return nil, &model.AppError{Code: "E_CAL", Message: "manifest contains no volumes", Field: "volumes"}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	m.ManifestPath = abs
	m.ManifestHash = hex.EncodeToString(sum[:])
	return &m, nil
}

func (m *Manifest) Resolve(volumeID string) (ResolvedSpec, error) {
	var spec ResolvedSpec
	if volumeID == "" && len(m.Volumes) > 0 {
		volumeID = m.Volumes[0].ID
	}
	var decl *VolumeDecl
	for i := range m.Volumes {
		if m.Volumes[i].ID == volumeID {
			decl = &m.Volumes[i]
			break
		}
	}
	if decl == nil {
		return spec, &model.AppError{Code: "E_NOT_FOUND", Message: "volume id is absent from manifest", Field: "volumeId", Actual: volumeID}
	}
	if len(decl.Dimensions) != 3 {
		return spec, &model.AppError{Code: "E_AXIS", Message: "dimensions must have exactly three entries", Axis: "X", Field: "dimensions", Actual: decl.Dimensions}
	}
	for i, n := range decl.Dimensions {
		if n <= 0 {
			return spec, &model.AppError{Code: "E_AXIS", Message: "dimension must be positive", Axis: model.AxisName(i), Field: fmt.Sprintf("dimensions.%s", model.AxisName(i)), Actual: n}
		}
		spec.Dimensions[i] = n
	}
	if len(decl.AxisOrder) != 3 {
		return spec, &model.AppError{Code: "E_AXIS", Message: "axisOrder must be a permutation of XYZ", Axis: "X", Field: "axisOrder", Actual: decl.AxisOrder, Allowed: "XYZ permutation"}
	}
	dims, ok := StorageDimensions(strings.ToUpper(decl.AxisOrder), spec.Dimensions)
	if !ok {
		return spec, &model.AppError{Code: "E_AXIS", Message: "axisOrder has duplicates or unknown axes", Field: "axisOrder", Actual: decl.AxisOrder, Allowed: "XYZ permutation"}
	}
	_ = dims
	spec.AxisOrder = strings.ToUpper(decl.AxisOrder)
	if len(decl.Spacing) != 3 {
		field := "spacing"
		if len(decl.Spacing) > 0 {
			field = fmt.Sprintf("spacing.%s", model.AxisName(len(decl.Spacing)-1))
		}
		return spec, &model.AppError{Code: "E_CAL", Message: "spacing must contain three values", Field: field}
	}
	for i := 0; i < 3; i++ {
		if decl.Spacing[i] == nil {
			return spec, &model.AppError{Code: "E_CAL", Message: "spacing field is missing", Field: fmt.Sprintf("spacing.%s", model.AxisName(i))}
		}
		v := *decl.Spacing[i]
		if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return spec, &model.AppError{Code: "E_CAL", Message: "spacing must be finite and positive", Field: fmt.Sprintf("spacing.%s", model.AxisName(i)), Actual: v, Allowed: "> 0"}
		}
		spec.Spacing[i] = v
	}
	if decl.Slope == nil {
		return spec, &model.AppError{Code: "E_CAL", Message: "slope field is missing", Field: "slope"}
	}
	if decl.Intercept == nil {
		return spec, &model.AppError{Code: "E_CAL", Message: "intercept field is missing", Field: "intercept"}
	}
	if math.IsNaN(*decl.Slope) || math.IsInf(*decl.Slope, 0) || *decl.Slope == 0 {
		return spec, &model.AppError{Code: "E_CAL", Message: "slope must be finite and non-zero", Field: "slope", Actual: *decl.Slope}
	}
	if math.IsNaN(*decl.Intercept) || math.IsInf(*decl.Intercept, 0) {
		return spec, &model.AppError{Code: "E_CAL", Message: "intercept must be finite", Field: "intercept", Actual: *decl.Intercept}
	}
	spec.Slope = *decl.Slope
	spec.Intercept = *decl.Intercept
	if decl.LayerCount == nil {
		return spec, &model.AppError{Code: "E_CAL", Message: "layerCount field is missing", Field: "layerCount"}
	}
	if *decl.LayerCount != spec.Dimensions[2] {
		return spec, &model.AppError{Code: "E_AXIS", Message: "layerCount conflicts with the Z dimension", Axis: "Z", Field: "layerCount", Actual: *decl.LayerCount, Allowed: spec.Dimensions[2]}
	}
	spec.LayerCount = *decl.LayerCount
	if len(decl.GrayRange) != 2 || decl.GrayRange[0] == nil || decl.GrayRange[1] == nil {
		return spec, &model.AppError{Code: "E_CAL", Message: "grayRange must contain two declared values", Field: "grayRange"}
	}
	spec.GrayRange = [2]float64{*decl.GrayRange[0], *decl.GrayRange[1]}
	if math.IsNaN(spec.GrayRange[0]) || math.IsNaN(spec.GrayRange[1]) || spec.GrayRange[1] <= spec.GrayRange[0] {
		return spec, &model.AppError{Code: "E_CAL", Message: "grayRange must be finite and increasing", Field: "grayRange", Actual: spec.GrayRange, Allowed: "[min,max] with min < max"}
	}
	if decl.File == "" {
		return spec, &model.AppError{Code: "E_CAL", Message: "volume file is missing", Field: "file"}
	}
	spec.ID = decl.ID
	spec.FilePath = decl.File
	if !filepath.IsAbs(spec.FilePath) {
		spec.FilePath = filepath.Join(filepath.Dir(m.ManifestPath), decl.File)
	}
	spec.SHA256 = strings.ToLower(strings.TrimSpace(decl.SHA256))
	spec.Structures = decl.Structures
	spec.ToleranceMM = m.PrecisionToleranceMM
	spec.ManifestPath = m.ManifestPath
	spec.ManifestHash = m.ManifestHash
	return spec, nil
}
