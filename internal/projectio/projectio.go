package projectio

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"terrainpipeline/internal/model"
)

type manifestFile struct {
	SchemaVersion *string                `json:"schema_version"`
	Seed          *int64                 `json:"seed"`
	Params        *model.BakeParams      `json:"params"`
	ParamVersion  string                 `json:"param_version"`
	Layers        []model.Layer          `json:"layers"`
	Blocks        []blockDTO             `json:"blocks"`
	Seams         []model.SeamConstraint `json:"seams"`
	Truth         model.TruthSet         `json:"truth"`
	ManifestHash  string                 `json:"manifest_hash"`
}

type blockDTO struct {
	ID            *string  `json:"id"`
	NX            *int     `json:"nx"`
	NZ            *int     `json:"nz"`
	DX            *float64 `json:"dx"`
	DZ            *float64 `json:"dz"`
	OriginX       float64  `json:"origin_x"`
	OriginZ       float64  `json:"origin_z"`
	ElevationFile string   `json:"elevation_file"`
	WeightsFile   string   `json:"weights_file"`
}

type elevationFile struct {
	BlockID string    `json:"block_id"`
	NX      int       `json:"nx"`
	NZ      int       `json:"nz"`
	Values  []float64 `json:"values"`
}

type weightsFile struct {
	BlockID  string   `json:"block_id"`
	LayerIDs []string `json:"layer_ids"`
	Quantum  int      `json:"quantum"`
	Values   [][]int  `json:"values"`
}

func WriteSample(dir string, project *model.Project) error {
	if err := os.MkdirAll(filepath.Join(dir, "elevations"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "weights"), 0o755); err != nil {
		return err
	}

	manifest := manifestFile{
		SchemaVersion: &project.SchemaVersion,
		Seed:          &project.Seed,
		Params:        &project.Params,
		ParamVersion:  project.ParamVersion,
		Layers:        project.Layers,
		Seams:         project.Seams,
		Truth:         project.Truth,
		ManifestHash:  project.ManifestHash,
	}
	for _, b := range project.Blocks {
		ef := filepath.ToSlash(filepath.Join("elevations", b.ID+".json"))
		wf := filepath.ToSlash(filepath.Join("weights", b.ID+".json"))
		id, nx, nz, dx, dz := b.ID, b.NX, b.NZ, b.DX, b.DZ
		manifest.Blocks = append(manifest.Blocks, blockDTO{
			ID: &id, NX: &nx, NZ: &nz, DX: &dx, DZ: &dz,
			OriginX: b.OriginX, OriginZ: b.OriginZ,
			ElevationFile: ef, WeightsFile: wf,
		})
		elev := elevationFile{BlockID: b.ID, NX: b.NX, NZ: b.NZ, Values: b.Elevation}
		if err := writeJSON(filepath.Join(dir, ef), elev); err != nil {
			return err
		}
		layerIDs := make([]string, 0, len(project.Layers))
		for _, l := range project.Layers {
			layerIDs = append(layerIDs, l.ID)
		}
		weights := make([][]int, len(b.Weights))
		for i, px := range b.Weights {
			weights[i] = make([]int, len(px))
			for j, w := range px {
				weights[i][j] = int(w)
			}
		}
		wfile := weightsFile{BlockID: b.ID, LayerIDs: layerIDs, Quantum: 255, Values: weights}
		if err := writeJSON(filepath.Join(dir, wf), wfile); err != nil {
			return err
		}
	}
	return writeJSON(filepath.Join(dir, "project.json"), manifest)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWrite(path, data)
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func LoadProject(dir string) (*model.Project, error) {
	manifestPath := filepath.Join(dir, "project.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, &model.Error{Code: model.CodeIO, Field: "project.json", Message: err.Error()}
	}
	var manifest manifestFile
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, model.NewError(model.CodeSchema, "", "project.json", "JSON 解析失败: %v", err)
	}
	if manifest.SchemaVersion == nil || *manifest.SchemaVersion == "" {
		return nil, model.NewError(model.CodeSchema, "", "schema_version", "字段缺失")
	}
	if manifest.Seed == nil {
		return nil, model.NewError(model.CodeSchema, "", "seed", "字段缺失")
	}
	if manifest.Params == nil {
		return nil, model.NewError(model.CodeSchema, "", "params", "字段缺失")
	}
	if len(manifest.Layers) == 0 {
		return nil, model.NewError(model.CodeSchema, "", "layers", "至少需要一个材质层")
	}
	for i, layer := range manifest.Layers {
		if layer.ID == "" {
			return nil, model.NewError(model.CodeSchema, "", fmt.Sprintf("layers[%d].id", i), "材质层 ID 缺失")
		}
		if !isFinite(layer.Strength) || !isFinite(layer.Frequency) {
			return nil, model.NewError(model.CodeSchema, layer.ID, fmt.Sprintf("layers[%d]", i), "材质参数必须为有限数")
		}
	}
	var rawTop map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawTop); err != nil {
		return nil, model.NewError(model.CodeSchema, "", "project.json", "JSON 对象解析失败: %v", err)
	}
	if _, ok := rawTop["truth"]; !ok {
		return nil, model.NewError(model.CodeSchema, "", "truth", "字段缺失")
	}
	params := manifest.Params.WithDefaults()
	if params.Differential != "central" && params.Differential != "forward" && params.Differential != "sobel" {
		return nil, model.NewError(model.CodeSchema, "", "params.differential", "不支持的差分算子 %q", params.Differential)
	}
	if params.HeightScale <= 0 {
		return nil, model.NewError(model.CodeSchema, "", "params.height_scale", "必须为正数")
	}

	project := &model.Project{
		SchemaVersion: *manifest.SchemaVersion,
		Seed:          *manifest.Seed,
		Params:        params,
		ParamVersion:  manifest.ParamVersion,
		Layers:        manifest.Layers,
		Seams:         manifest.Seams,
		Truth:         manifest.Truth,
		ManifestHash:  manifest.ManifestHash,
	}
	if project.ParamVersion == "" {
		project.ParamVersion = params.ParameterVersion()
	}

	seen := map[string]bool{}
	for i := range manifest.Blocks {
		d := &manifest.Blocks[i]
		if d.ID == nil || *d.ID == "" {
			return nil, model.NewError(model.CodeSchema, "", fmt.Sprintf("blocks[%d].id", i), "字段缺失或为空")
		}
		id := *d.ID
		if seen[id] {
			return nil, model.NewError(model.CodeSchema, id, "blocks.id", "块标识重复")
		}
		seen[id] = true
		if d.NX == nil {
			return nil, model.NewError(model.CodeSchema, id, "blocks.nx", "字段缺失")
		}
		if d.NZ == nil {
			return nil, model.NewError(model.CodeSchema, id, "blocks.nz", "字段缺失")
		}
		if *d.NX < 2 || *d.NZ < 2 {
			return nil, model.NewError(model.CodeSchema, id, "blocks.nx", "nx/nz 必须至少为 2")
		}
		if d.DX == nil || *d.DX <= 0 {
			return nil, model.NewError(model.CodeSchema, id, "blocks.dx", "字段缺失或非正数")
		}
		if d.DZ == nil || *d.DZ <= 0 {
			return nil, model.NewError(model.CodeSchema, id, "blocks.dz", "字段缺失或非正数")
		}
		if d.ElevationFile == "" {
			return nil, model.NewError(model.CodeSchema, id, "blocks.elevation_file", "字段缺失")
		}
		if d.WeightsFile == "" {
			return nil, model.NewError(model.CodeSchema, id, "blocks.weights_file", "字段缺失")
		}

		block := &model.Block{
			ID: id, NX: *d.NX, NZ: *d.NZ, DX: *d.DX, DZ: *d.DZ,
			OriginX: d.OriginX, OriginZ: d.OriginZ,
		}
		if err := loadElevation(dir, d.ElevationFile, block); err != nil {
			return nil, err
		}
		if err := loadWeights(dir, d.WeightsFile, project.Layers, block); err != nil {
			return nil, err
		}
		project.Blocks = append(project.Blocks, block)
	}

	if err := validateSeams(project); err != nil {
		return nil, err
	}
	if err := validateTruth(project); err != nil {
		return nil, err
	}
	return project, nil
}

func loadElevation(dir, rel string, block *model.Block) error {
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return model.NewError(model.CodeIO, block.ID, "elevation_file", "读取 %s 失败: %v", rel, err)
	}
	var ef elevationFile
	if err := json.Unmarshal(data, &ef); err != nil {
		return model.NewError(model.CodeSchema, block.ID, "elevation.values", "JSON 解析失败: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return model.NewError(model.CodeSchema, block.ID, "elevation", "JSON 对象解析失败: %v", err)
	}
	if _, ok := raw["values"]; !ok {
		return model.NewError(model.CodeSchema, block.ID, "elevation.values", "字段缺失")
	}
	if ef.BlockID != "" && ef.BlockID != block.ID {
		return model.NewError(model.CodeSchema, block.ID, "elevation.block_id", "文件声明块 %q 与清单不一致", ef.BlockID)
	}
	want := block.NX * block.NZ
	if len(ef.Values) != want || ef.NX != block.NX || ef.NZ != block.NZ {
		return model.NewError(model.CodeSize, block.ID, "elevation", "清单声明 %dx%d=%d，文件声明 %dx%d 且采样数 %d", block.NX, block.NZ, want, ef.NX, ef.NZ, len(ef.Values))
	}
	for _, v := range ef.Values {
		if !isFinite(v) {
			return model.NewError(model.CodeSchema, block.ID, "elevation.values", "高程必须为有限数")
		}
	}
	block.Elevation = append([]float64(nil), ef.Values...)
	return nil
}

func loadWeights(dir, rel string, layers []model.Layer, block *model.Block) error {
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return model.NewError(model.CodeIO, block.ID, "weights_file", "读取 %s 失败: %v", rel, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return model.NewError(model.CodeSchema, block.ID, "weights.values", "JSON 解析失败: %v", err)
	}
	if _, ok := raw["values"]; !ok {
		return model.NewError(model.CodeSchema, block.ID, "weights.values", "字段缺失")
	}
	var wf weightsFile
	if err := json.Unmarshal(data, &wf); err != nil {
		return model.NewError(model.CodeSchema, block.ID, "weights.values", "权重必须为量化整型: %v", err)
	}
	if wf.Quantum != 0 && wf.Quantum != 255 {
		return model.NewError(model.CodeSchema, block.ID, "weights.quantum", "仅支持 255 量化级，得到 %d", wf.Quantum)
	}
	if len(wf.LayerIDs) != len(layers) {
		return model.NewError(model.CodeSchema, block.ID, "weights.layer_ids", "层数量 %d 与工程 %d 不一致", len(wf.LayerIDs), len(layers))
	}
	for i, layer := range layers {
		if i >= len(wf.LayerIDs) || wf.LayerIDs[i] != layer.ID {
			return model.NewError(model.CodeSchema, block.ID, "weights.layer_ids", "第 %d 层应为 %q", i, layer.ID)
		}
	}
	wantPixels := block.NX * block.NZ
	if len(wf.Values) != wantPixels {
		return model.NewError(model.CodeSize, block.ID, "weights.values", "像素权重行数 %d 与高程像素数 %d 不符", len(wf.Values), wantPixels)
	}
	block.Weights = make([][]uint8, wantPixels)
	for p, row := range wf.Values {
		if len(row) != len(layers) {
			return model.NewError(model.CodeSize, block.ID, fmt.Sprintf("weights.values[%d]", p), "层权重数 %d 应为 %d", len(row), len(layers))
		}
		sum := 0
		out := make([]uint8, len(layers))
		for k, v := range row {
			if v < 0 || v > 255 {
				return model.NewError(model.CodeSchema, block.ID, fmt.Sprintf("weights.values[%d][%d]", p, k), "量化权重 %d 超出 0..255", v)
			}
			out[k] = uint8(v)
			sum += v
		}
		if sum != 255 {
			return model.NewError(model.CodeSchema, block.ID, fmt.Sprintf("weights.values[%d]", p), "量化权重和为 %d，必须等于 255", sum)
		}
		block.Weights[p] = out
	}
	return nil
}

func validateSeams(p *model.Project) error {
	for i := range p.Seams {
		s := &p.Seams[i]
		fieldPrefix := fmt.Sprintf("seams[%s]", orIndex(s.ID, i))
		if s.ID == "" {
			s.ID = fmt.Sprintf("SEAM-%02d", i+1)
		}
		if s.BlockA == "" {
			return model.NewError(model.CodeSchema, "", fieldPrefix+".block_a", "字段缺失")
		}
		if s.BlockB == "" {
			return model.NewError(model.CodeSchema, "", fieldPrefix+".block_b", "字段缺失")
		}
		a := p.BlockByID(s.BlockA)
		if a == nil {
			return model.NewError(model.CodeRef, s.BlockA, fieldPrefix+".block_a", "接缝 %s 引用的相邻块不存在", s.ID)
		}
		b := p.BlockByID(s.BlockB)
		if b == nil {
			return model.NewError(model.CodeRef, s.BlockB, fieldPrefix+".block_b", "接缝 %s 引用的相邻块不存在", s.ID)
		}
		if !validEdge(s.EdgeA) {
			return model.NewError(model.CodeSchema, a.ID, fieldPrefix+".edge_a", "非法边 %q", s.EdgeA)
		}
		if !validEdge(s.EdgeB) {
			return model.NewError(model.CodeSchema, b.ID, fieldPrefix+".edge_b", "非法边 %q", s.EdgeB)
		}
		lenA := edgeLength(a, s.EdgeA)
		lenB := edgeLength(b, s.EdgeB)
		if lenA != lenB {
			return model.NewError(model.CodeSize, a.ID, fieldPrefix+".edge", "共享边长度不一致: %s(%s)=%d, %s(%s)=%d", a.ID, s.EdgeA, lenA, b.ID, s.EdgeB, lenB)
		}
		if s.SampleCount != 0 && s.SampleCount != lenA {
			return model.NewError(model.CodeSize, s.ID, fieldPrefix+".sample_count", "声明采样数 %d 与共享边高程列长度 %d 不符", s.SampleCount, lenA)
		}
		s.SampleCount = lenA
		if s.ThresholdDeg <= 0 {
			s.ThresholdDeg = p.Params.SeamThresholdDeg
		}
	}
	return nil
}

func validateTruth(p *model.Project) error {
	for i, t := range p.Truth.Normals {
		b := p.BlockByID(t.BlockID)
		if b == nil {
			return model.NewError(model.CodeRef, t.BlockID, fmt.Sprintf("truth.normals[%d].block_id", i), "真值引用块不存在")
		}
		if !b.InBounds(t.I, t.J) {
			return model.NewError(model.CodeSize, b.ID, fmt.Sprintf("truth.normals[%d].pixel", i), "真值像素 (%d,%d) 超出 %dx%d", t.I, t.J, b.NX, b.NZ)
		}
	}
	for i, t := range p.Truth.Seams {
		found := false
		for _, s := range p.Seams {
			if s.ID == t.SeamID {
				found = true
				if t.Index < 0 || t.Index >= s.SampleCount {
					return model.NewError(model.CodeSize, s.BlockA, fmt.Sprintf("truth.seams[%d].index", i), "接缝真值索引 %d 超出 0..%d", t.Index, s.SampleCount-1)
				}
			}
		}
		if !found {
			return model.NewError(model.CodeRef, t.SeamID, fmt.Sprintf("truth.seams[%d].seam_id", i), "真值引用接缝不存在")
		}
	}
	return nil
}

func validEdge(e model.Edge) bool {
	switch e {
	case model.EdgeLeft, model.EdgeRight, model.EdgeTop, model.EdgeBottom:
		return true
	}
	return false
}

func edgeLength(b *model.Block, e model.Edge) int {
	if e == model.EdgeLeft || e == model.EdgeRight {
		return b.NZ
	}
	return b.NX
}

func orIndex(id string, i int) string {
	if id != "" {
		return id
	}
	return fmt.Sprintf("%d", i)
}

func isFinite(v float64) bool {
	return v == v && v < 1e308 && v > -1e308
}
