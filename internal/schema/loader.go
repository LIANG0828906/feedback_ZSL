package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"occlusionlab/internal/model"
)

type projectFile struct {
	Meta        model.ProjectMeta `json:"meta"`
	ContentHash string            `json:"content_hash"`
}

// LoadProject reads the four-file deterministic project bundle and verifies
// the manifest before decoding semantic fields.
func LoadProject(dir string) (*model.Project, *model.AppError) {
	manifestBytes, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, model.IOError("manifest.json", "无法读取项目清单", err)
	}
	projectBytes, err := os.ReadFile(filepath.Join(dir, "project.json"))
	if err != nil {
		return nil, model.IOError("project.json", "无法读取项目元数据", err)
	}
	worldBytes, err := os.ReadFile(filepath.Join(dir, "world.json"))
	if err != nil {
		return nil, model.IOError("world.json", "无法读取世界文件", err)
	}
	truthBytes, err := os.ReadFile(filepath.Join(dir, "truth.json"))
	if err != nil {
		return nil, model.IOError("truth.json", "无法读取真值文件", err)
	}

	var manifest model.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, model.SchemaError(0, "manifest", "项目清单不是合法 JSON")
	}
	if manifest.SchemaVersion != model.SchemaVersion {
		return nil, &model.AppError{
			Code: model.CodeSchema, Field: "manifest.schema_version",
			Message: fmt.Sprintf("不支持的项目清单版本 %d", manifest.SchemaVersion),
		}
	}
	checks := []struct {
		field string
		want  string
		data  []byte
	}{
		{"manifest.project_sha256", manifest.ProjectHash, projectBytes},
		{"manifest.world_sha256", manifest.WorldHash, worldBytes},
		{"manifest.truth_sha256", manifest.TruthHash, truthBytes},
	}
	for _, check := range checks {
		if sha256Hex(check.data) != check.want {
			return nil, &model.AppError{
				Code: model.CodeSchema, Field: check.field,
				Message: "文件内容哈希与项目清单不一致",
			}
		}
	}

	if err := requireTopFields(projectBytes, []string{"meta", "content_hash"}, -1, "project"); err != nil {
		return nil, err
	}
	var pf projectFile
	if err := json.Unmarshal(projectBytes, &pf); err != nil {
		return nil, model.SchemaError(0, "project", "项目元数据不是合法 JSON")
	}
	if err := requireMetaFields(projectBytes); err != nil {
		return nil, err
	}

	if err := requireWorldFields(worldBytes); err != nil {
		return nil, err
	}
	var world model.World
	if err := json.Unmarshal(worldBytes, &world); err != nil {
		return nil, model.SchemaError(0, "world", "世界文件不是合法 JSON")
	}

	if err := requireTruthFields(truthBytes); err != nil {
		return nil, err
	}
	var truth model.Truth
	if err := json.Unmarshal(truthBytes, &truth); err != nil {
		return nil, model.SchemaError(0, "truth", "真值文件不是合法 JSON")
	}

	project := &model.Project{
		Meta:        pf.Meta,
		World:       world,
		Truth:       truth,
		ContentHash: pf.ContentHash,
	}
	if err := ValidateProject(project); err != nil {
		return nil, err
	}
	computed := model.ProjectContentHash(project.Meta, project.World, project.Truth)
	if computed != project.ContentHash || computed != manifest.ContentHash {
		return nil, &model.AppError{
			Code: model.CodeSchema, Field: "content_hash",
			Message: "项目内容哈希不匹配，真值或场景可能已被修改",
		}
	}
	return project, nil
}

func requireTopFields(raw []byte, fields []string, chunk int, prefix string) *model.AppError {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return model.SchemaError(chunk, prefix, "对象不是合法 JSON")
	}
	for _, field := range fields {
		if _, ok := obj[field]; !ok {
			return model.SchemaError(chunk, prefix+"."+field, "缺失必需字段")
		}
	}
	return nil
}

func requireNested(raw json.RawMessage, fields []string, chunk int, prefix string) *model.AppError {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return model.SchemaError(chunk, prefix, "对象不是合法 JSON")
	}
	for _, field := range fields {
		if _, ok := obj[field]; !ok {
			return model.SchemaError(chunk, prefix+"."+field, "缺失必需字段")
		}
	}
	return nil
}

func requireVec(raw json.RawMessage, chunk int, prefix string) *model.AppError {
	return requireNested(raw, []string{"x", "y", "z"}, chunk, prefix)
}

func requireAABB(raw json.RawMessage, chunk int, prefix string) *model.AppError {
	if err := requireNested(raw, []string{"min", "max"}, chunk, prefix); err != nil {
		return err
	}
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(raw, &obj)
	if err := requireVec(obj["min"], chunk, prefix+".min"); err != nil {
		return err
	}
	return requireVec(obj["max"], chunk, prefix+".max")
}

func requireMetaFields(projectBytes []byte) *model.AppError {
	var root map[string]json.RawMessage
	_ = json.Unmarshal(projectBytes, &root)
	meta := root["meta"]
	if err := requireNested(meta, []string{
		"schema_version", "generator_version", "seed", "title", "precision", "tolerance_px_q",
	}, 0, "meta"); err != nil {
		return err
	}
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(meta, &obj)
	return requireNested(obj["precision"], []string{
		"world_frac_bits", "basis_frac_bits", "screen_frac_bits", "param_frac_bits",
	}, 0, "meta.precision")
}

func requireWorldFields(worldBytes []byte) *model.AppError {
	if err := requireTopFields(worldBytes, []string{
		"world_bounds", "chunks", "occluders", "cameras",
	}, 0, "world"); err != nil {
		return err
	}
	var root map[string]json.RawMessage
	_ = json.Unmarshal(worldBytes, &root)
	if err := requireAABB(root["world_bounds"], 0, "world.world_bounds"); err != nil {
		return err
	}

	var chunks []json.RawMessage
	if err := json.Unmarshal(root["chunks"], &chunks); err != nil {
		return model.SchemaError(0, "world.chunks", "分块列表不是数组")
	}
	for i, raw := range chunks {
		if err := requireNested(raw, []string{
			"index", "label", "bounds", "anchor", "occluder_refs", "complexity", "load_cost", "color",
		}, i, fmt.Sprintf("chunks[%d]", i)); err != nil {
			return err
		}
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(raw, &obj)
		if err := requireAABB(obj["bounds"], i, fmt.Sprintf("chunks[%d].bounds", i)); err != nil {
			return err
		}
		if err := requireVec(obj["anchor"], i, fmt.Sprintf("chunks[%d].anchor", i)); err != nil {
			return err
		}
	}

	var occluders []json.RawMessage
	if err := json.Unmarshal(root["occluders"], &occluders); err != nil {
		return model.SchemaError(0, "world.occluders", "遮挡体列表不是数组")
	}
	for i, raw := range occluders {
		if err := requireNested(raw, []string{
			"id", "kind", "bounds", "source_chunk", "color",
		}, 0, fmt.Sprintf("occluders[%d]", i)); err != nil {
			return err
		}
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(raw, &obj)
		if err := requireAABB(obj["bounds"], 0, fmt.Sprintf("occluders[%d].bounds", i)); err != nil {
			return err
		}
	}

	var cameras []json.RawMessage
	if err := json.Unmarshal(root["cameras"], &cameras); err != nil {
		return model.SchemaError(0, "world.cameras", "相机轨迹不是数组")
	}
	for i, raw := range cameras {
		if err := requireNested(raw, []string{
			"window", "time", "position", "right", "up", "forward", "near", "far",
			"tan_half_x", "tan_half_y", "focal_x", "focal_y", "center_x", "center_y",
			"screen_width", "screen_height",
		}, 0, fmt.Sprintf("cameras[%d]", i)); err != nil {
			err.Window = i
			return err
		}
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(raw, &obj)
		for _, field := range []string{"position", "right", "up", "forward"} {
			if err := requireVec(obj[field], 0, fmt.Sprintf("cameras[%d].%s", i, field)); err != nil {
				err.Window = i
				return err
			}
		}
	}
	return nil
}

func requireTruthFields(truthBytes []byte) *model.AppError {
	if err := requireTopFields(truthBytes, []string{
		"samples", "known_projections", "tolerance_px_q", "occlusion_rule", "projection_rule",
	}, 0, "truth"); err != nil {
		return err
	}
	var root map[string]json.RawMessage
	_ = json.Unmarshal(truthBytes, &root)
	var samples []json.RawMessage
	if err := json.Unmarshal(root["samples"], &samples); err != nil {
		return model.SchemaError(0, "truth.samples", "真值采样列表不是数组")
	}
	for i, raw := range samples {
		if err := requireNested(raw, []string{"window", "time", "chunks"}, 0, fmt.Sprintf("truth.samples[%d]", i)); err != nil {
			err.Window = i
			return err
		}
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(raw, &obj)
		var chunks []json.RawMessage
		if err := json.Unmarshal(obj["chunks"], &chunks); err != nil {
			return model.SchemaError(0, fmt.Sprintf("truth.samples[%d].chunks", i), "分块真值不是数组")
		}
		for j, chunkRaw := range chunks {
			if err := requireNested(chunkRaw, []string{
				"chunk", "visible", "reference_occluder", "reason", "uncertainty_margin_q",
			}, 0, fmt.Sprintf("truth.samples[%d].chunks[%d]", i, j)); err != nil {
				err.Window = i
				return err
			}
		}
	}
	var projections []json.RawMessage
	if err := json.Unmarshal(root["known_projections"], &projections); err != nil {
		return model.SchemaError(0, "truth.known_projections", "已知投影列表不是数组")
	}
	for i, raw := range projections {
		if err := requireNested(raw, []string{
			"id", "window", "chunk", "world", "expected_x_q", "expected_y_q", "note",
		}, 0, fmt.Sprintf("truth.known_projections[%d]", i)); err != nil {
			return err
		}
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(raw, &obj)
		if err := requireVec(obj["world"], 0, fmt.Sprintf("truth.known_projections[%d].world", i)); err != nil {
			return err
		}
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// WriteProject is used by the deterministic generator and keeps file bytes
// stable across platforms.
func WriteProject(dir string, project *model.Project) *model.AppError {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return model.IOError("project_dir", "无法创建项目目录", err)
	}
	project.ContentHash = model.ProjectContentHash(project.Meta, project.World, project.Truth)
	pf := projectFile{Meta: project.Meta, ContentHash: project.ContentHash}
	projectBytes, err := marshalIndented(pf)
	if err != nil {
		return model.InternalError(0, "无法编码项目元数据", err)
	}
	worldBytes, err := marshalIndented(project.World)
	if err != nil {
		return model.InternalError(0, "无法编码世界数据", err)
	}
	truthBytes, err := marshalIndented(project.Truth)
	if err != nil {
		return model.InternalError(0, "无法编码真值数据", err)
	}
	manifest := model.Manifest{
		SchemaVersion: model.SchemaVersion,
		ContentHash:   project.ContentHash,
		ProjectHash:   sha256Hex(projectBytes),
		WorldHash:     sha256Hex(worldBytes),
		TruthHash:     sha256Hex(truthBytes),
	}
	manifestBytes, err := marshalIndented(manifest)
	if err != nil {
		return model.InternalError(0, "无法编码项目清单", err)
	}
	files := []struct {
		name string
		data []byte
	}{
		{"project.json", projectBytes},
		{"world.json", worldBytes},
		{"truth.json", truthBytes},
		{"manifest.json", manifestBytes},
	}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(dir, file.name), file.data, 0o644); err != nil {
			return model.IOError(file.name, "无法写入项目文件", err)
		}
	}
	return nil
}

func marshalIndented(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func firstMissing(m map[string]json.RawMessage, fields []string) string {
	for _, field := range fields {
		if _, ok := m[field]; !ok {
			return field
		}
	}
	return ""
}

func fieldPath(parts ...string) string {
	return strings.Join(parts, ".")
}
