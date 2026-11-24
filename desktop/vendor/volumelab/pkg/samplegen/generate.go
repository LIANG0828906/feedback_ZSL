package samplegen

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"volumelab/pkg/model"
	"volumelab/pkg/volume"
)

const DefaultManifestName = "manifest.json"

type Options struct {
	OutputDir    string
	Seed         int64
	ManifestName string
}

type Result struct {
	OutputDir    string                `json:"outputDir"`
	ManifestPath string                `json:"manifestPath"`
	Seed         int64                 `json:"seed"`
	Volumes      []model.VolumeSummary `json:"volumes"`
}

type volumeRecipe struct {
	id         string
	fileName   string
	axisOrder  string
	shape      [3]int
	spacing    [3]float64
	slope      float64
	intercept  float64
	grayRange  [2]float64
	structures []model.Structure
}

func DefaultRecipes() []volumeRecipe {
	return []volumeRecipe{
		{
			id:        "vol_a",
			fileName:  "vol_a.vlm",
			axisOrder: "XYZ",
			shape:     [3]int{64, 56, 40},
			spacing:   [3]float64{1.0, 1.2, 1.5},
			slope:     1.0,
			intercept: -1000,
			grayRange: [2]float64{-1000, 1600},
			structures: []model.Structure{
				sphere("sphere_a", "球体 A", model.Vec3{22, 24, 24}, 8),
				tube("tube_a", "管状结构 A", model.Vec3{10, 44, 8}, model.Vec3{54, 44, 44}, 4),
				box("box_a", "矩形块 A", model.Vec3{40, 8, 10}, model.Vec3{58, 24, 36}),
			},
		},
		{
			id:        "vol_b",
			fileName:  "vol_b.vlm",
			axisOrder: "ZYX",
			shape:     [3]int{48, 52, 36},
			spacing:   [3]float64{1.25, 1.25, 2.0},
			slope:     0.5,
			intercept: -500,
			grayRange: [2]float64{-500, 900},
			structures: []model.Structure{
				sphere("sphere_b", "球体 B", model.Vec3{22, 20, 22}, 7.5),
				tube("tube_b", "管状结构 B", model.Vec3{8, 42, 12}, model.Vec3{48, 42, 56}, 3.75),
				box("box_b", "矩形块 B", model.Vec3{30, 6, 8}, model.Vec3{52, 20, 30}),
			},
		},
	}
}

func Generate(opts Options) (*Result, error) {
	if opts.OutputDir == "" {
		return nil, &model.AppError{Code: "E_ARG", Message: "output directory is required", Field: "outputDir"}
	}
	if opts.Seed == 0 {
		opts.Seed = 42
	}
	if opts.ManifestName == "" {
		opts.ManifestName = DefaultManifestName
	}
	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		return nil, &model.AppError{Code: "E_IO", Message: "cannot create sample output directory", Field: "outputDir", Details: map[string]any{"cause": err.Error()}}
	}
	manifest := volume.Manifest{
		Format:               volume.ManifestFormat,
		Seed:                 opts.Seed,
		PrecisionToleranceMM: 2.5,
	}
	summaries := make([]model.VolumeSummary, 0, len(DefaultRecipes()))
	for _, recipe := range DefaultRecipes() {
		decl, summary, err := generateVolume(opts.OutputDir, opts.Seed, recipe)
		if err != nil {
			return nil, err
		}
		manifest.Volumes = append(manifest.Volumes, decl)
		summaries = append(summaries, summary)
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	manifestPath := filepath.Join(opts.OutputDir, opts.ManifestName)
	if err := writeFileAtomic(manifestPath, encoded, 0o644); err != nil {
		return nil, &model.AppError{Code: "E_IO", Message: "cannot write manifest", Details: map[string]any{"cause": err.Error()}}
	}
	return &Result{
		OutputDir:    opts.OutputDir,
		ManifestPath: manifestPath,
		Seed:         opts.Seed,
		Volumes:      summaries,
	}, nil
}

func generateVolume(outDir string, seed int64, recipe volumeRecipe) (volume.VolumeDecl, model.VolumeSummary, error) {
	count, ok := volume.VoxelCount(recipe.shape)
	if !ok {
		return volume.VolumeDecl{}, model.VolumeSummary{}, fmt.Errorf("invalid shape for %s", recipe.id)
	}
	data := make([]int16, int(count))
	storageDims, _ := volume.StorageDimensions(recipe.axisOrder, recipe.shape)
	for z := 0; z < recipe.shape[2]; z++ {
		for y := 0; y < recipe.shape[1]; y++ {
			for x := 0; x < recipe.shape[0]; x++ {
				point := model.Vec3{
					float64(x) * recipe.spacing[0],
					float64(y) * recipe.spacing[1],
					float64(z) * recipe.spacing[2],
				}
				raw := int32(texture(seed, x, y, z, 5))
				for _, structure := range recipe.structures {
					if structureContains(structure, point) {
						raw = structureBase(structure.Type) + int32(texture(seed, x, y, z, 13))
					}
				}
				offset, _ := volume.StorageOffset(recipe.axisOrder, storageDims, [3]int{x, y, z})
				data[offset] = int16(raw)
			}
		}
	}
	header, err := volume.EncodeHeader(volume.FileHeader{
		Version:      volume.FileVersion,
		AxisOrder:    recipe.axisOrder,
		StorageDims:  [3]uint32{uint32(storageDims[0]), uint32(storageDims[1]), uint32(storageDims[2])},
		PhysicalDims: [3]uint32{uint32(recipe.shape[0]), uint32(recipe.shape[1]), uint32(recipe.shape[2])},
		Spacing:      recipe.spacing,
		Slope:        recipe.slope,
		Intercept:    recipe.intercept,
		LayerCount:   uint32(recipe.shape[2]),
		DataBytes:    count * volume.BytesPerVoxel,
	})
	if err != nil {
		return volume.VolumeDecl{}, model.VolumeSummary{}, err
	}
	payload := make([]byte, count*volume.BytesPerVoxel)
	for i, value := range data {
		binary.LittleEndian.PutUint16(payload[i*2:i*2+2], uint16(value))
	}
	fileBytes := append(header, payload...)
	hash := sha256.Sum256(fileBytes)
	hashString := hex.EncodeToString(hash[:])
	filePath := filepath.Join(outDir, recipe.fileName)
	if err := writeFileAtomic(filePath, fileBytes, 0o644); err != nil {
		return volume.VolumeDecl{}, model.VolumeSummary{}, &model.AppError{Code: "E_IO", Message: "cannot write volume file", Details: map[string]any{"cause": err.Error()}}
	}
	f := func(v float64) *float64 { return &v }
	i := func(v int) *int { return &v }
	decl := volume.VolumeDecl{
		ID:         recipe.id,
		File:       recipe.fileName,
		AxisOrder:  recipe.axisOrder,
		Dimensions: []int{recipe.shape[0], recipe.shape[1], recipe.shape[2]},
		Spacing:    []*float64{f(recipe.spacing[0]), f(recipe.spacing[1]), f(recipe.spacing[2])},
		Slope:      f(recipe.slope),
		Intercept:  f(recipe.intercept),
		LayerCount: i(recipe.shape[2]),
		SHA256:     hashString,
		GrayRange:  []*float64{f(recipe.grayRange[0]), f(recipe.grayRange[1])},
		Structures: recipe.structures,
	}
	summary := model.VolumeSummary{
		ID:                   recipe.id,
		File:                 filePath,
		AxisOrder:            recipe.axisOrder,
		Dimensions:           recipe.shape,
		Spacing:              recipe.spacing,
		Slope:                recipe.slope,
		Intercept:            recipe.intercept,
		LayerCount:           recipe.shape[2],
		GrayRange:            recipe.grayRange,
		ContentHash:          hashString,
		Structures:           recipe.structures,
		PrecisionToleranceMM: 2.5,
	}
	return decl, summary, nil
}

func texture(seed int64, x, y, z, amplitude int) int {
	h := uint64(seed)*0x9E3779B97F4A7C15 +
		uint64(x)*0xBF58476D1CE4E5B9 +
		uint64(y)*0x94D049BB133111EB +
		uint64(z)*0xD6E8FEB86659FD93
	h ^= h >> 30
	h *= 0xBF58476D1CE4E5B9
	h ^= h >> 27
	h *= 0x94D049BB133111EB
	h ^= h >> 31
	return int(h%uint64(amplitude)) - amplitude/2
}

func structureBase(kind string) int32 {
	switch kind {
	case "sphere":
		return 2500
	case "tube":
		return 1800
	case "box":
		return 1200
	default:
		return 800
	}
}

func structureContains(s model.Structure, p model.Vec3) bool {
	switch s.Type {
	case "sphere":
		return s.Center != nil && s.Radius != nil && p.Sub(*s.Center).Norm() <= *s.Radius
	case "tube":
		if s.Start == nil || s.End == nil || s.Radius == nil {
			return false
		}
		axis := s.End.Sub(*s.Start)
		denom := axis.Dot(axis)
		if denom == 0 {
			return false
		}
		t := p.Sub(*s.Start).Dot(axis) / denom
		if t < 0 || t > 1 {
			return false
		}
		projection := s.Start.Add(axis.Mul(t))
		return p.Sub(projection).Norm() <= *s.Radius
	case "box":
		return s.Min != nil && s.Max != nil &&
			p[0] >= s.Min[0] && p[0] <= s.Max[0] &&
			p[1] >= s.Min[1] && p[1] <= s.Max[1] &&
			p[2] >= s.Min[2] && p[2] <= s.Max[2]
	default:
		return false
	}
}

func sphere(id, label string, center model.Vec3, radius float64) model.Structure {
	min := center.Sub(model.Vec3{radius, radius, radius})
	max := center.Add(model.Vec3{radius, radius, radius})
	return model.Structure{
		ID: id, Type: "sphere", Label: label,
		Center: &center, Radius: &radius,
		BoundingBox: [2]model.Vec3{min, max},
	}
}

func tube(id, label string, start, end model.Vec3, radius float64) model.Structure {
	min := model.Vec3{
		math.Min(start[0], end[0]) - radius,
		math.Min(start[1], end[1]) - radius,
		math.Min(start[2], end[2]) - radius,
	}
	max := model.Vec3{
		math.Max(start[0], end[0]) + radius,
		math.Max(start[1], end[1]) + radius,
		math.Max(start[2], end[2]) + radius,
	}
	return model.Structure{
		ID: id, Type: "tube", Label: label,
		Start: &start, End: &end, Radius: &radius,
		BoundingBox: [2]model.Vec3{min, max},
	}
}

func box(id, label string, min, max model.Vec3) model.Structure {
	return model.Structure{
		ID: id, Type: "box", Label: label,
		Min: &min, Max: &max,
		BoundingBox: [2]model.Vec3{min, max},
	}
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+strings.TrimSpace(filepath.Base(path))+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
