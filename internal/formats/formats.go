package formats

import (
	"os"
	"path/filepath"
	"strings"

	"strata/internal/model"
)

type LossyReport struct {
	Warnings       []model.Warning `json:"warnings"`
	DowngradeTrace []string        `json:"downgradeTrace"`
	ReadbackPoints int             `json:"readbackPoints"`
	ReadbackFaces  int             `json:"readbackFaces"`
}

func Versions() []model.VersionInfo {
	return []model.VersionInfo{
		{
			ID:                 model.VersionPLYASCII,
			Number:             1,
			DisplayName:        "PLY ASCII v1",
			Short:              "ASC",
			Format:             "PLY ascii 1.0",
			Encoding:           "ascii",
			File:               "scene-ascii-v1.ply",
			FieldOrder:         []string{"x", "y", "z", "nx", "ny", "nz", "red", "green", "blue", "intensity"},
			OptionalAttributes: []string{"normals", "colors", "intensity"},
			Quantization:       model.Quantization{Kind: "text-float"},
			HasNormals:         true,
			HasColors:          true,
			HasIntensity:       true,
		},
		{
			ID:                 model.VersionPLYBinary,
			Number:             1,
			DisplayName:        "PLY binary LE v1",
			Short:              "BIN",
			Format:             "PLY binary_little_endian 1.0",
			Encoding:           "binary_little_endian",
			File:               "scene-binary-v1.ply",
			FieldOrder:         []string{"z", "x", "y", "red", "green", "blue", "intensity"},
			OptionalAttributes: []string{"colors", "intensity"},
			Quantization:       model.Quantization{Kind: "float32"},
			HasColors:          true,
			HasIntensity:       true,
		},
		{
			ID:                 model.VersionPCBin,
			Number:             1,
			DisplayName:        "PCBin compact v1",
			Short:              "PCZ",
			Format:             "PCZ1 compact binary",
			Encoding:           "custom-compact",
			File:               "scene-compact-v1.pcbin",
			FieldOrder:         []string{"x", "y", "z", "intensity"},
			OptionalAttributes: []string{"intensity"},
			Quantization:       model.Quantization{Kind: "bbox-u16"},
			HasIntensity:       true,
		},
	}
}

func Version(id string) (model.VersionInfo, error) {
	for _, v := range Versions() {
		if v.ID == id {
			return v, nil
		}
	}
	return model.VersionInfo{}, model.NewError(model.EVER, "unknown point cloud version "+id, "header.version", 0, map[string]any{
		"version":   id,
		"supported": []string{model.VersionPLYASCII, model.VersionPLYBinary, model.VersionPCBin},
	})
}

func InferVersion(path string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".pcbin":
		return model.VersionPCBin, nil
	case ".ply":
		f, err := os.Open(path)
		if err != nil {
			return "", model.NewError(model.EDECODE, err.Error(), "file", 0, nil)
		}
		defer f.Close()
		buf := make([]byte, 128)
		n, _ := f.Read(buf)
		header := string(buf[:n])
		if strings.Contains(header, "binary_little_endian") {
			return model.VersionPLYBinary, nil
		}
		return model.VersionPLYASCII, nil
	default:
		return "", model.NewError(model.EVER, "cannot infer version from extension", "file.extension", 0, nil)
	}
}

func LoadFile(path, versionID string) (*model.Geometry, error) {
	if versionID == "" {
		var err error
		versionID, err = InferVersion(path)
		if err != nil {
			return nil, err
		}
	}
	switch versionID {
	case model.VersionPLYASCII:
		return loadPLY(path, model.VersionPLYASCII)
	case model.VersionPLYBinary:
		return loadPLY(path, model.VersionPLYBinary)
	case model.VersionPCBin:
		return loadPCBin(path)
	default:
		_, err := Version(versionID)
		return nil, err
	}
}

func WriteFile(path, versionID string, geometry *model.Geometry, mesh bool) (*LossyReport, error) {
	switch versionID {
	case model.VersionPLYASCII:
		return writePLY(path, model.VersionPLYASCII, geometry, mesh)
	case model.VersionPLYBinary:
		return writePLY(path, model.VersionPLYBinary, geometry, mesh)
	case model.VersionPCBin:
		return writePCBin(path, geometry, mesh)
	default:
		_, err := Version(versionID)
		return nil, err
	}
}
