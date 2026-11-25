package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"terrainkernel/internal/kerr"
	"terrainkernel/internal/model"
)

func quote(s string) string {
	return strconv.Quote(s)
}

func ProbeVersion(data []byte) (int, error) {
	var probe struct {
		FormatVersion  *int `json:"format_version"`
		FormatVer      *int `json:"formatVer"`
		FormatVersionP *int `json:"FormatVersion"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&probe); err != nil {
		return 0, kerr.New(kerr.CodeSchema, "$", "invalid JSON: "+err.Error())
	}
	versions := make([]int, 0, 3)
	if probe.FormatVersion != nil {
		versions = append(versions, *probe.FormatVersion)
	}
	if probe.FormatVer != nil {
		versions = append(versions, *probe.FormatVer)
	}
	if probe.FormatVersionP != nil {
		versions = append(versions, *probe.FormatVersionP)
	}
	if len(versions) == 0 {
		return 0, kerr.New(kerr.CodeVersion, "format_version", "missing version discriminator")
	}
	v := versions[0]
	for _, other := range versions[1:] {
		if other != v {
			return 0, kerr.New(kerr.CodeVersion, "format_version", "conflicting version discriminators")
		}
	}
	if v < 1 || v > 3 {
		return 0, kerr.New(kerr.CodeVersion, versionPath(v), fmt.Sprintf("unknown version %d", v))
	}
	return v, nil
}

func versionPath(v int) string {
	return "format_version"
}

func Load(data []byte) (*model.Project, error) {
	version, err := ProbeVersion(data)
	if err != nil {
		return nil, err
	}
	switch version {
	case 1:
		return loadV1(data)
	case 2:
		return loadV2(data)
	case 3:
		return loadV3(data)
	default:
		return nil, kerr.New(kerr.CodeVersion, "format_version", fmt.Sprintf("unknown version %d", version))
	}
}

func LoadFile(path string) (*model.Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Load(data)
}

func requireString(value, path string) error {
	if value == "" {
		return kerr.New(kerr.CodeSchema, path, "required field is empty")
	}
	return nil
}

func requirePositive(value int, path string) error {
	if value <= 0 {
		return kerr.New(kerr.CodeSchema, path, "required positive integer")
	}
	return nil
}

type fieldStyle int

const (
	styleV1 fieldStyle = iota
	styleV2
	styleV3
	styleInternal
)

func projectFields(style fieldStyle) (projectID, normalization, cols, rows, size string) {
	switch style {
	case styleV1:
		return "project_id", "normalization", "chunk_cols", "chunk_rows", "chunk_size_px"
	case styleV2:
		return "projectId", "normalization", "chunkCols", "chunkRows", "chunkSizePx"
	case styleV3:
		return "ProjectID", "Normalization", "ChunkCols", "ChunkRows", "ChunkSizePx"
	default:
		return "project_id", "normalization", "chunk_cols", "chunk_rows", "chunk_size_px"
	}
}

func layerFields(style fieldStyle) (id, curve, noise string) {
	switch style {
	case styleV2:
		return "layerId", "curvePoints", "noise"
	case styleV3:
		return "LayerID", "CurvePoints", "Noise"
	default:
		return "layer_id", "curve_points", "noise"
	}
}

func validateProjectHeader(projectID, normalization string, cols, rows, size int, style fieldStyle) error {
	idPath, normPath, colPath, rowPath, sizePath := projectFields(style)
	if err := requireString(projectID, idPath); err != nil {
		return err
	}
	if normalization != "global" && normalization != "per_layer" {
		return kerr.New(kerr.CodeSchema, normPath, "unrecognized normalization "+quote(normalization))
	}
	if err := requirePositive(cols, colPath); err != nil {
		return err
	}
	if err := requirePositive(rows, rowPath); err != nil {
		return err
	}
	if err := requirePositive(size, sizePath); err != nil {
		return err
	}
	return nil
}

func validateLayers(layers []model.Layer, style fieldStyle) error {
	idField, curveField, noiseField := layerFields(style)
	if len(layers) == 0 {
		return kerr.New(kerr.CodeSchema, "layers", "at least one layer required")
	}
	ids := make(map[string]struct{}, len(layers))
	for i := range layers {
		layer := &layers[i]
		if layer.ID == "" {
			return kerr.New(kerr.CodeSchema, layerPath(i, idField), "required field is empty")
		}
		if _, exists := ids[layer.ID]; exists {
			return kerr.New(kerr.CodeSchema, layerPath(i, idField), "duplicate layer id")
		}
		ids[layer.ID] = struct{}{}
		if layer.Curve == nil && layer.Expr == "" {
			return kerr.New(kerr.CodeSchema, layerPath(i, curveField), "missing curve")
		}
		if layer.Curve != nil {
			if len(layer.Curve.Points) == 0 {
				return kerr.New(kerr.CodeSchema, layerPath(i, curveField), "curve has no points")
			}
			lastX := layer.Curve.Points[0].X
			for j, pt := range layer.Curve.Points {
				if j > 0 && pt.X < lastX {
					return kerr.New(kerr.CodeSchema, fmt.Sprintf("%s.%s[%d].x", layerPath(i, ""), curveField, j), "curve x values must be non-decreasing")
				}
				if pt.Y < 0 {
					return kerr.New(kerr.CodeSchema, fmt.Sprintf("%s.%s[%d].y", layerPath(i, ""), curveField, j), "weight must be non-negative")
				}
				lastX = pt.X
			}
		}
		if layer.Noise != nil {
			if layer.Noise.Amplitude < 0 {
				return kerr.New(kerr.CodeSchema, layerPath(i, noiseField+".Amplitude"), "amplitude must be non-negative")
			}
			if layer.Noise.ScaleMM <= 0 {
				return kerr.New(kerr.CodeSchema, layerPath(i, noiseField+".ScaleM"), "scale must be positive")
			}
		}
	}
	return nil
}

func layerPath(index int, field string) string {
	base := "layers[" + strconv.Itoa(index) + "]"
	if field == "" {
		return base
	}
	return base + "." + field
}
