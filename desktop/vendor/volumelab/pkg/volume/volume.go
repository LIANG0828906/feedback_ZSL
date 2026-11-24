package volume

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"volumelab/pkg/model"
)

type Volume struct {
	ID           string
	FilePath     string
	AxisOrder    string
	Shape        [3]int
	StorageDims  [3]int
	Spacing      [3]float64
	Slope        float64
	Intercept    float64
	LayerCount   int
	GrayRange    [2]float64
	Data         []int16
	ContentHash  string
	Structures   []model.Structure
	ManifestPath string
	ManifestHash string
	ToleranceMM  float64
}

func Load(spec ResolvedSpec) (*Volume, error) {
	rawFile, err := os.ReadFile(spec.FilePath)
	if err != nil {
		return nil, &model.AppError{Code: "E_DECODE", Message: "volume file cannot be read", Actual: spec.FilePath, Details: map[string]any{"cause": err.Error()}}
	}
	header, err := DecodeHeader(rawFile)
	if err != nil {
		return nil, err
	}
	if header.AxisOrder != spec.AxisOrder {
		return nil, &model.AppError{
			Code:    "E_AXIS",
			Message: "axisOrder in manifest and volume header conflict",
			Axis:    model.AxisName(model.AxisX),
			Actual:  map[string]any{"manifest": spec.AxisOrder, "header": header.AxisOrder},
			Allowed: spec.AxisOrder,
		}
	}
	for i := 0; i < 3; i++ {
		if int(header.PhysicalDims[i]) != spec.Dimensions[i] {
			return nil, &model.AppError{
				Code:    "E_AXIS",
				Message: "declared physical dimension conflicts with volume header",
				Axis:    model.AxisName(i),
				Actual:  map[string]any{"manifest": spec.Dimensions[i], "header": header.PhysicalDims[i]},
			}
		}
	}
	expectedStorage, _ := StorageDimensions(spec.AxisOrder, spec.Dimensions)
	for i := 0; i < 3; i++ {
		if int(header.StorageDims[i]) != expectedStorage[i] {
			axis := model.AxisName(model.AxisX)
			for pos := 0; pos < 3; pos++ {
				if expectedStorage[pos] != int(header.StorageDims[pos]) {
					a, _ := model.AxisFromName(string(spec.AxisOrder[pos]))
					axis = model.AxisName(a)
					break
				}
			}
			return nil, &model.AppError{
				Code:    "E_AXIS",
				Message: "axisOrder does not match actual storage dimensions",
				Axis:    axis,
				Actual:  map[string]any{"storageDims": header.StorageDims, "expected": expectedStorage, "axisOrder": spec.AxisOrder},
			}
		}
	}
	if header.LayerCount != uint32(spec.LayerCount) {
		return nil, &model.AppError{Code: "E_AXIS", Message: "layerCount conflicts with volume header", Axis: "Z", Actual: header.LayerCount, Allowed: spec.LayerCount}
	}
	for i := 0; i < 3; i++ {
		if math.Abs(header.Spacing[i]-spec.Spacing[i]) > 1e-9 {
			return nil, &model.AppError{Code: "E_CAL", Message: "spacing in manifest and header conflict", Field: "spacing." + model.AxisName(i), Actual: header.Spacing[i], Allowed: spec.Spacing[i]}
		}
	}
	if math.Abs(header.Slope-spec.Slope) > 1e-9 {
		return nil, &model.AppError{Code: "E_CAL", Message: "slope in manifest and header conflict", Field: "slope", Actual: header.Slope, Allowed: spec.Slope}
	}
	if math.Abs(header.Intercept-spec.Intercept) > 1e-9 {
		return nil, &model.AppError{Code: "E_CAL", Message: "intercept in manifest and header conflict", Field: "intercept", Actual: header.Intercept, Allowed: spec.Intercept}
	}
	count, ok := VoxelCount(spec.Dimensions)
	if !ok {
		return nil, &model.AppError{Code: "E_DECODE", Message: "volume dimensions overflow address space", Actual: spec.Dimensions}
	}
	expectedPayload := count * BytesPerVoxel
	if header.DataBytes != expectedPayload {
		return nil, &model.AppError{Code: "E_DECODE", Message: "header payload size is inconsistent with dimensions", Actual: header.DataBytes, Allowed: expectedPayload}
	}
	if uint64(len(rawFile)) < uint64(HeaderSize)+expectedPayload {
		return nil, &model.AppError{
			Code:    "E_DECODE",
			Message: "volume data is truncated",
			Actual:  len(rawFile),
			Allowed: fmtSize(uint64(HeaderSize) + expectedPayload),
		}
	}
	if uint64(len(rawFile)) > uint64(HeaderSize)+expectedPayload {
		return nil, &model.AppError{Code: "E_DECODE", Message: "volume file has trailing bytes", Actual: len(rawFile), Allowed: fmtSize(uint64(HeaderSize) + expectedPayload)}
	}
	sum := sha256.Sum256(rawFile)
	contentHash := hex.EncodeToString(sum[:])
	if spec.SHA256 != "" && spec.SHA256 != contentHash {
		return nil, &model.AppError{Code: "E_DECODE", Message: "volume content hash does not match manifest", Field: "sha256", Actual: contentHash, Allowed: spec.SHA256}
	}
	payload := rawFile[HeaderSize:]
	data := make([]int16, count)
	for i := range data {
		data[i] = int16(binary.LittleEndian.Uint16(payload[i*2 : i*2+2]))
	}
	v := &Volume{
		ID:           spec.ID,
		FilePath:     filepath.Clean(spec.FilePath),
		AxisOrder:    spec.AxisOrder,
		Shape:        spec.Dimensions,
		StorageDims:  expectedStorage,
		Spacing:      spec.Spacing,
		Slope:        spec.Slope,
		Intercept:    spec.Intercept,
		LayerCount:   spec.LayerCount,
		GrayRange:    spec.GrayRange,
		Data:         data,
		ContentHash:  contentHash,
		Structures:   spec.Structures,
		ManifestPath: spec.ManifestPath,
		ManifestHash: spec.ManifestHash,
		ToleranceMM:  spec.ToleranceMM,
	}
	return v, nil
}

func LoadFromManifest(manifestPath, volumeID string) (*Manifest, *Volume, error) {
	m, err := LoadManifest(manifestPath)
	if err != nil {
		return nil, nil, err
	}
	spec, err := m.Resolve(volumeID)
	if err != nil {
		return m, nil, err
	}
	v, err := Load(spec)
	if err != nil {
		return m, nil, err
	}
	return m, v, nil
}

func (v *Volume) InBounds(voxel model.IVec3) bool {
	return voxel[0] >= 0 && voxel[0] < v.Shape[0] &&
		voxel[1] >= 0 && voxel[1] < v.Shape[1] &&
		voxel[2] >= 0 && voxel[2] < v.Shape[2]
}

func (v *Volume) RawAt(voxel model.IVec3) (int16, bool) {
	if !v.InBounds(voxel) {
		return 0, false
	}
	offset, ok := storageOffset(v.AxisOrder, v.StorageDims, [3]int(voxel))
	if !ok || offset < 0 || offset >= len(v.Data) {
		return 0, false
	}
	return v.Data[offset], true
}

func (v *Volume) Calibrated(raw int16) float64 {
	return float64(raw)*v.Slope + v.Intercept
}

func (v *Volume) WorldFromVoxel(voxel model.Vec3) model.Vec3 {
	return model.Vec3{voxel[0] * v.Spacing[0], voxel[1] * v.Spacing[1], voxel[2] * v.Spacing[2]}
}

func (v *Volume) VoxelFromWorld(world model.Vec3) model.Vec3 {
	return model.Vec3{world[0] / v.Spacing[0], world[1] / v.Spacing[1], world[2] / v.Spacing[2]}
}

func (v *Volume) WorldBounds() [2]model.Vec3 {
	return [2]model.Vec3{
		{0, 0, 0},
		{
			float64(v.Shape[0]-1) * v.Spacing[0],
			float64(v.Shape[1]-1) * v.Spacing[1],
			float64(v.Shape[2]-1) * v.Spacing[2],
		},
	}
}

func (v *Volume) Summary() model.VolumeSummary {
	return model.VolumeSummary{
		ID:                   v.ID,
		File:                 v.FilePath,
		AxisOrder:            v.AxisOrder,
		Dimensions:           v.Shape,
		Spacing:              v.Spacing,
		Slope:                v.Slope,
		Intercept:            v.Intercept,
		LayerCount:           v.LayerCount,
		GrayRange:            v.GrayRange,
		ContentHash:          v.ContentHash,
		Structures:           v.Structures,
		PrecisionToleranceMM: v.ToleranceMM,
	}
}

func (v *Volume) ValidateWorldPoint(point model.Vec3) *model.AppError {
	for axis := 0; axis < 3; axis++ {
		min := 0.0
		max := float64(v.Shape[axis]-1) * v.Spacing[axis]
		if point[axis] < min-1e-9 || point[axis] > max+1e-9 {
			return &model.AppError{
				Code:    "E_RANGE",
				Message: "world point is outside the volume",
				Axis:    model.AxisName(axis),
				Actual:  point[axis],
				Allowed: [2]float64{min, max},
			}
		}
	}
	return nil
}

func fmtSize(n uint64) string {
	return strconv.FormatUint(n, 10) + " bytes"
}
