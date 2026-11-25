package fogkernel

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

const (
	SchemaV1 = "fog.project/v1+flat-permille-mm"
	SchemaV2 = "fog.project/v2+tree-permyriad-m"
	SchemaV3 = "fog.project/v3+tree-permille-m"
)

type DecimalMeters string

func (d *DecimalMeters) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "\"") {
		var text string
		if err := json.Unmarshal(b, &text); err != nil {
			return err
		}
		*d = DecimalMeters(text)
		return nil
	}
	*d = DecimalMeters(s)
	return nil
}

func (d DecimalMeters) MarshalJSON() ([]byte, error) {
	s := strings.TrimSpace(string(d))
	if s == "" {
		return []byte("0"), nil
	}
	if _, err := DecimalMetersToMicro(s, "$.marshal"); err != nil {
		return nil, err
	}
	return []byte(s), nil
}

type envelope struct {
	Schema        string `json:"Schema"`
	SchemaVersion string `json:"schema_version"`
	APIVersion    string `json:"apiVersion"`
}

func DetectSchema(data []byte) (string, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return "", NewBizError(CodeSchemaField, "$", "invalid JSON envelope: %v", err)
	}
	nonEmpty := 0
	for _, v := range []string{env.Schema, env.SchemaVersion, env.APIVersion} {
		if v != "" {
			nonEmpty++
		}
	}
	if nonEmpty > 1 {
		return "", NewBizError(CodeSchemaCombination, "$", "document mixes multiple schema version envelopes").
			WithContext("Schema", env.Schema, "schema_version", env.SchemaVersion, "apiVersion", env.APIVersion)
	}
	switch {
	case env.Schema != "":
		if env.Schema != SchemaV1 {
			return "", NewBizError(CodeSchemaVersion, "$.Schema", "unrecognized schema/combination %q", env.Schema).
				WithContext("supported", []string{SchemaV1, SchemaV2, SchemaV3})
		}
		return SchemaV1, nil
	case env.SchemaVersion != "":
		if env.SchemaVersion != SchemaV2 {
			return "", NewBizError(CodeSchemaVersion, "$.schema_version", "unrecognized schema/combination %q", env.SchemaVersion).
				WithContext("supported", []string{SchemaV1, SchemaV2, SchemaV3})
		}
		return SchemaV2, nil
	case env.APIVersion != "":
		if env.APIVersion != SchemaV3 {
			return "", NewBizError(CodeSchemaVersion, "$.apiVersion", "unrecognized schema/combination %q", env.APIVersion).
				WithContext("supported", []string{SchemaV1, SchemaV2, SchemaV3})
		}
		return SchemaV3, nil
	default:
		return "", NewBizError(CodeSchemaVersion, "$", "document is missing Schema/schema_version/apiVersion")
	}
}

func LoadProject(data []byte) (*Scene, error) {
	version, err := DetectSchema(data)
	if err != nil {
		return nil, err
	}
	switch version {
	case SchemaV1:
		var doc dtoV1
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, NewBizError(CodeSchemaField, "$", "v1 decode failed: %v", err)
		}
		return buildV1(&doc)
	case SchemaV2:
		var doc dtoV2
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, NewBizError(CodeSchemaField, "$", "v2 decode failed: %v", err)
		}
		return buildV2(&doc)
	case SchemaV3:
		var doc dtoV3
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, NewBizError(CodeSchemaField, "$", "v3 decode failed: %v", err)
		}
		return buildV3(&doc)
	}
	return nil, NewBizError(CodeSchemaVersion, "$", "unsupported schema %q", version)
}

type dtoRecipeV1 struct {
	ChunkSizeMM             int64 `json:"ChunkSizeMM"`
	BlockSampleLimit        int   `json:"BlockSampleLimit"`
	TransmittanceToleranceQ int64 `json:"TransmittanceToleranceQ"`
	TerminationThresholdQ   int64 `json:"TerminationThresholdQ"`
	PreviewStride           int   `json:"PreviewStride"`
	PreviewErrorBoundQ      int64 `json:"PreviewErrorBoundQ"`
	DefaultWorkers          int   `json:"DefaultWorkers"`
	DefaultBuffer           int   `json:"DefaultBuffer"`
}

type dtoCameraV1 struct {
	LogicalTime   int64    `json:"LogicalTime"`
	PositionMM    [3]int64 `json:"PositionMM"`
	YawMilliDeg   int64    `json:"YawMilliDeg"`
	PitchMilliDeg int64    `json:"PitchMilliDeg"`
	FovYMilliDeg  int64    `json:"FovYMilliDeg"`
	AspectQ       int64    `json:"AspectQ"`
}

type dtoVolumeV1 struct {
	ID                 string   `json:"ID"`
	BoundsMinMM        [3]int64 `json:"BoundsMinMM"`
	BoundsMaxMM        [3]int64 `json:"BoundsMaxMM"`
	VoxelSizeMM        int64    `json:"VoxelSizeMM"`
	Size               [3]int   `json:"Size"`
	DensityValues      []int    `json:"DensityValues"`
	DensityMax         int      `json:"DensityMax"`
	SigmaAPermille     int64    `json:"SigmaAPermille"`
	SigmaSPermille     int64    `json:"SigmaSPermille"`
	AnisotropyPermille int64    `json:"AnisotropyPermille"`
}

type dtoV1 struct {
	Schema       string        `json:"Schema"`
	ProjectID    string        `json:"ProjectID"`
	Seed         int64         `json:"Seed"`
	Recipe       dtoRecipeV1   `json:"Recipe"`
	LightVectorQ [3]int64      `json:"LightVectorQ"`
	Cameras      []dtoCameraV1 `json:"Cameras"`
	Volumes      []dtoVolumeV1 `json:"Volumes"`
}

type dtoRecipeM struct {
	ChunkSizeM              DecimalMeters `json:"chunk_size_m"`
	BlockSampleLimit        int           `json:"block_sample_limit"`
	TransmittanceToleranceQ int64         `json:"transmittance_tolerance_q"`
	TerminationThresholdQ   int64         `json:"termination_threshold_q"`
	PreviewStride           int           `json:"preview_stride"`
	PreviewErrorBoundQ      int64         `json:"preview_error_bound_q"`
	DefaultWorkers          int           `json:"default_workers"`
	DefaultBuffer           int           `json:"default_buffer"`
}

type dtoCameraV2 struct {
	LogicalTime   int64            `json:"logical_time"`
	PositionM     [3]DecimalMeters `json:"position_m"`
	YawMilliDeg   int64            `json:"yaw_milli_deg"`
	PitchMilliDeg int64            `json:"pitch_milli_deg"`
	FovYMilliDeg  int64            `json:"fov_y_milli_deg"`
	AspectQ       int64            `json:"aspect_q"`
}

type dtoBlockV2 struct {
	BlockOrigin [3]int `json:"block_origin"`
	Size        [3]int `json:"size"`
	Values      []int  `json:"values"`
}

type dtoGridV2 struct {
	Dimensions [3]int       `json:"dimensions"`
	DensityMax int          `json:"density_max"`
	Blocks     []dtoBlockV2 `json:"blocks"`
}

type dtoPhaseV2 struct {
	Type string `json:"type"`
}

type dtoVolumeV2 struct {
	ID                  string           `json:"media_id"`
	BoundsMinM          [3]DecimalMeters `json:"aabb_min_m"`
	BoundsMaxM          [3]DecimalMeters `json:"aabb_max_m"`
	VoxelSizeM          DecimalMeters    `json:"voxel_size_m"`
	VoxelGrid           dtoGridV2        `json:"voxel_grid"`
	SigmaAPermyriad     int64            `json:"sigma_a_permyriad"`
	SigmaSPermyriad     int64            `json:"sigma_s_permyriad"`
	AnisotropyPermyriad int64            `json:"anisotropy_permyriad"`
	PhaseFunction       *dtoPhaseV2      `json:"phase_function"`
}

type dtoV2 struct {
	SchemaVersion string        `json:"schema_version"`
	ProjectID     string        `json:"project_id"`
	Seed          int64         `json:"seed"`
	Recipe        dtoRecipeM    `json:"recipe"`
	LightVectorQ  [3]int64      `json:"light_vector_q"`
	CameraTracks  []dtoCameraV2 `json:"camera_tracks"`
	Media         []dtoVolumeV2 `json:"media"`
}

type dtoRecipeV3 struct {
	ChunkSizeM              DecimalMeters `json:"chunkSizeM"`
	BlockSampleLimit        int           `json:"blockSampleLimit"`
	TransmittanceToleranceQ int64         `json:"transmittanceToleranceQ"`
	TerminationThresholdQ   int64         `json:"terminationThresholdQ"`
	PreviewStride           int           `json:"previewStride"`
	PreviewErrorBoundQ      int64         `json:"previewErrorBoundQ"`
	DefaultWorkers          int           `json:"defaultWorkers"`
	DefaultBuffer           int           `json:"defaultBuffer"`
}

type dtoCameraV3 struct {
	LogicalTime   int64            `json:"logicalTime"`
	PositionM     [3]DecimalMeters `json:"positionM"`
	YawMilliDeg   int64            `json:"yawMilliDeg"`
	PitchMilliDeg int64            `json:"pitchMilliDeg"`
	FovYMilliDeg  int64            `json:"fovYMilliDeg"`
	AspectQ       int64            `json:"aspectQ"`
}

type dtoBlockV3 struct {
	BlockOrigin [3]int `json:"blockOrigin"`
	Size        [3]int `json:"size"`
	Values      []int  `json:"values"`
}

type dtoGridV3 struct {
	Dimensions [3]int       `json:"dimensions"`
	DensityMax int          `json:"densityMax"`
	Blocks     []dtoBlockV3 `json:"blocks"`
}

type dtoPhaseV3 struct {
	Type string `json:"type"`
}

type treeBlock struct {
	Origin [3]int
	Size   [3]int
	Values []int
}

type dtoVolumeV3 struct {
	ID                 string           `json:"volumeId"`
	BoundsMinM         [3]DecimalMeters `json:"boundsMinM"`
	BoundsMaxM         [3]DecimalMeters `json:"boundsMaxM"`
	VoxelSizeM         DecimalMeters    `json:"voxelSizeM"`
	VoxelTree          dtoGridV3        `json:"voxelTree"`
	SigmaAPermille     int64            `json:"sigmaAPermille"`
	SigmaSPermille     int64            `json:"sigmaSPermille"`
	AnisotropyPermille int64            `json:"anisotropyPermille"`
	PhaseFunction      *dtoPhaseV3      `json:"phaseFunction"`
}

type dtoV3 struct {
	APIVersion   string        `json:"apiVersion"`
	ProjectID    string        `json:"projectId"`
	Seed         int64         `json:"seed"`
	Recipe       dtoRecipeV3   `json:"recipe"`
	LightVectorQ [3]int64      `json:"lightVectorQ"`
	Cameras      []dtoCameraV3 `json:"cameras"`
	Volumes      []dtoVolumeV3 `json:"volumes"`
}

func vecMM(a [3]int64, path string) (Vec3I, error) {
	x, err := MMToMicro(a[0], path+".x")
	if err != nil {
		return Vec3I{}, err
	}
	y, err := MMToMicro(a[1], path+".y")
	if err != nil {
		return Vec3I{}, err
	}
	z, err := MMToMicro(a[2], path+".z")
	if err != nil {
		return Vec3I{}, err
	}
	return Vec3I{X: x, Y: y, Z: z}, nil
}

func vecMeters(a [3]DecimalMeters, path string) (Vec3I, error) {
	x, err := DecimalMetersToMicro(string(a[0]), path+".x")
	if err != nil {
		return Vec3I{}, err
	}
	y, err := DecimalMetersToMicro(string(a[1]), path+".y")
	if err != nil {
		return Vec3I{}, err
	}
	z, err := DecimalMetersToMicro(string(a[2]), path+".z")
	if err != nil {
		return Vec3I{}, err
	}
	return Vec3I{X: x, Y: y, Z: z}, nil
}

func flatDensity(size [3]int, values []int, maxValue int, path string) (DensityField, error) {
	field, err := emptyDensityField(size, maxValue, path+".size")
	if err != nil {
		return DensityField{}, err
	}
	n := len(field.Values)
	if len(values) != n {
		return DensityField{}, NewBizError(CodeDensityShape, path+".values", "flat density length %d does not match dimensions product %d", len(values), n).
			WithContext("actual_values", len(values), "required_values", n, "dimensions", size)
	}
	for i, v := range values {
		if v < 0 || v > field.Max {
			return DensityField{}, NewBizError(CodeDensityRange, fmt.Sprintf("%s.values[%d]", path, i), "voxel value %d outside [0,%d]", v, field.Max).
				WithContext("value", v, "min", 0, "max", field.Max, "index", i)
		}
	}
	field.Values = append([]int(nil), values...)
	return field, nil
}

const MaxDensityVoxels = 16_000_000

func emptyDensityField(size [3]int, maxValue int, path string) (DensityField, error) {
	if size[0] <= 0 || size[1] <= 0 || size[2] <= 0 {
		return DensityField{}, NewBizError(CodeDensityShape, path, "density dimensions must be positive, got %v", size)
	}
	n := int64(1)
	for _, d := range size {
		if d > 0 && n > math.MaxInt64/int64(d) {
			return DensityField{}, NewBizError(CodeDensityShape, path, "density dimension product overflows int64: %v", size)
		}
		n *= int64(d)
	}
	if n > MaxDensityVoxels {
		return DensityField{}, NewBizError(CodeDensityShape, path, "density grid %d voxels exceeds safe local limit %d", n, MaxDensityVoxels).
			WithContext("required_values", n, "limit", MaxDensityVoxels, "dimensions", size)
	}
	if maxValue <= 0 {
		maxValue = 255
	}
	return DensityField{
		Size:   Vec3I{X: int64(size[0]), Y: int64(size[1]), Z: int64(size[2])},
		Values: make([]int, int(n)),
		Max:    maxValue,
	}, nil
}

func flattenTree(dims [3]int, maxValue int, blocks []dtoBlockV2, path string) (DensityField, error) {
	gb := make([]treeBlock, 0, len(blocks))
	for _, b := range blocks {
		gb = append(gb, treeBlock{b.BlockOrigin, b.Size, b.Values})
	}
	return flattenGenericTree(dims, maxValue, gb, path)
}

func flattenTreeV3(dims [3]int, maxValue int, blocks []dtoBlockV3, path string) (DensityField, error) {
	gb := make([]treeBlock, 0, len(blocks))
	for _, b := range blocks {
		gb = append(gb, treeBlock{b.BlockOrigin, b.Size, b.Values})
	}
	return flattenGenericTree(dims, maxValue, gb, path)
}

func flattenGenericTree(dims [3]int, maxValue int, blocks []treeBlock, path string) (DensityField, error) {
	field, err := emptyDensityField(dims, maxValue, path+".dimensions")
	if err != nil {
		return DensityField{}, err
	}
	filled := make([]bool, len(field.Values))
	for bi, block := range blocks {
		bp := fmt.Sprintf("%s.blocks[%d]", path, bi)
		for axis := 0; axis < 3; axis++ {
			if block.Origin[axis] < 0 || block.Size[axis] <= 0 ||
				block.Size[axis] > dims[axis] ||
				block.Origin[axis] > dims[axis]-block.Size[axis] {
				return DensityField{}, NewBizError(CodeDensityShape, bp, "block origin/size %v/%v outside grid %v", block.Origin, block.Size, dims)
			}
		}
		need := block.Size[0] * block.Size[1] * block.Size[2]
		if len(block.Values) != need {
			return DensityField{}, NewBizError(CodeDensityShape, bp+".values", "block has %d values, need %d", len(block.Values), need).
				WithContext("actual_values", len(block.Values), "required_values", need)
		}
		k := 0
		for z := block.Origin[2]; z < block.Origin[2]+block.Size[2]; z++ {
			for y := block.Origin[1]; y < block.Origin[1]+block.Size[1]; y++ {
				for x := block.Origin[0]; x < block.Origin[0]+block.Size[0]; x++ {
					idx := field.Index(x, y, z)
					if filled[idx] {
						return DensityField{}, NewBizError(CodeDensityShape, bp, "overlapping voxel block at (%d,%d,%d)", x, y, z)
					}
					v := block.Values[k]
					if v < 0 || v > field.Max {
						return DensityField{}, NewBizError(CodeDensityRange, fmt.Sprintf("%s.values[%d]", bp, k), "voxel value %d outside [0,%d]", v, field.Max)
					}
					field.Values[idx] = v
					filled[idx] = true
					k++
				}
			}
		}
	}
	for i, ok := range filled {
		if !ok {
			return DensityField{}, NewBizError(CodeDensityShape, path+".blocks", "voxel tree does not cover grid; first missing flat index %d", i).
				WithContext("missing_index", i, "dimensions", dims, "block_count", len(blocks))
		}
	}
	return field, nil
}

func validateAABBDensity(id string, mn, mx Vec3I, voxelMicro int64, density DensityField, path string) error {
	if voxelMicro <= 0 {
		return NewBizError(CodeDensityShape, path+".voxel_size", "voxel size must be positive")
	}
	size := Vec3I{X: mx.X - mn.X, Y: mx.Y - mn.Y, Z: mx.Z - mn.Z}
	expected := Vec3I{X: density.Size.X * voxelMicro, Y: density.Size.Y * voxelMicro, Z: density.Size.Z * voxelMicro}
	if size.X <= 0 || size.Y <= 0 || size.Z <= 0 {
		return NewBizError(CodeDensityShape, path+".bounds", "AABB max must be greater than min")
	}
	if size != expected {
		return NewBizError(CodeDensityShape, path, "density grid %v*voxel %d does not match AABB size %v", density.Size, voxelMicro, size).
			WithContext("medium_id", id, "aabb_size_micro", size, "grid_size_micro", expected, "dimensions", density.Size)
	}
	return nil
}

func buildRecipeV1(r *dtoRecipeV1) (Recipe, error) {
	chunk, err := MMToMicro(r.ChunkSizeMM, "$.Recipe.ChunkSizeMM")
	if err != nil {
		return Recipe{}, err
	}
	return makeRecipe(chunk, r.BlockSampleLimit, r.TransmittanceToleranceQ, r.TerminationThresholdQ, r.PreviewStride, r.PreviewErrorBoundQ, r.DefaultWorkers, r.DefaultBuffer)
}

func buildRecipeM(chunk DecimalMeters, limit int, tol, thresholdQ int64, stride int, boundQ int64, workers, buffer int, path string) (Recipe, error) {
	micro, err := DecimalMetersToMicro(string(chunk), path)
	if err != nil {
		return Recipe{}, err
	}
	return makeRecipe(micro, limit, tol, thresholdQ, stride, boundQ, workers, buffer)
}

func makeRecipe(chunkMicro int64, limit int, tolQ, thresholdQ int64, stride int, boundQ int64, workers, buffer int) (Recipe, error) {
	if chunkMicro <= 0 {
		return Recipe{}, NewBizError(CodeSchemaField, "$.recipe.chunk", "chunk size must be positive")
	}
	if limit <= 0 || limit > 4096 {
		return Recipe{}, NewBizError(CodeSchemaField, "$.recipe.blockSampleLimit", "block sample limit %d outside 1..4096", limit)
	}
	if tolQ <= 0 {
		tolQ = DefaultTolQ
	}
	if thresholdQ <= 0 || thresholdQ > TransmittanceQ {
		return Recipe{}, NewBizError(CodeSchemaField, "$.recipe.threshold", "termination threshold Q %d outside 1..%d", thresholdQ, TransmittanceQ)
	}
	if stride <= 0 {
		stride = 2
	}
	if boundQ <= 0 {
		boundQ = PreviewBoundQ
	}
	if workers < 0 || workers > MaxDeclaredWorkers || buffer < 0 || buffer > MaxDeclaredBuffer {
		return Recipe{}, NewBizError(CodeBudgetWorkers, "$.recipe.budget", "default budget out of declared range")
	}
	if workers == 0 {
		workers = 4
	}
	if buffer == 0 {
		buffer = 64
	}
	return Recipe{
		ChunkSizeMicro: chunkMicro, BlockSampleLimit: limit,
		TransmittanceToleranceQ: tolQ, TerminationThresholdQ: thresholdQ,
		PreviewStride: stride, PreviewErrorBoundQ: boundQ,
		DefaultWorkers: workers, DefaultBuffer: buffer,
	}, nil
}

func buildCamerasV1(in []dtoCameraV1) ([]CameraSample, error) {
	out := make([]CameraSample, 0, len(in))
	for i, c := range in {
		pos, err := vecMM(c.PositionMM, fmt.Sprintf("$.Cameras[%d].PositionMM", i))
		if err != nil {
			return nil, err
		}
		out = append(out, CameraSample{LogicalTime: c.LogicalTime, PositionMicro: pos, YawMilliDeg: c.YawMilliDeg, PitchMilliDeg: c.PitchMilliDeg, FovYMilliDeg: c.FovYMilliDeg, AspectQ: c.AspectQ})
	}
	return out, nil
}

func buildCamerasV2(in []dtoCameraV2) ([]CameraSample, error) {
	out := make([]CameraSample, 0, len(in))
	for i, c := range in {
		pos, err := vecMeters(c.PositionM, fmt.Sprintf("$.camera_tracks[%d].position_m", i))
		if err != nil {
			return nil, err
		}
		out = append(out, CameraSample{LogicalTime: c.LogicalTime, PositionMicro: pos, YawMilliDeg: c.YawMilliDeg, PitchMilliDeg: c.PitchMilliDeg, FovYMilliDeg: c.FovYMilliDeg, AspectQ: c.AspectQ})
	}
	return out, nil
}

func buildCamerasV3(in []dtoCameraV3) ([]CameraSample, error) {
	out := make([]CameraSample, 0, len(in))
	for i, c := range in {
		pos, err := vecMeters(c.PositionM, fmt.Sprintf("$.cameras[%d].positionM", i))
		if err != nil {
			return nil, err
		}
		out = append(out, CameraSample{LogicalTime: c.LogicalTime, PositionMicro: pos, YawMilliDeg: c.YawMilliDeg, PitchMilliDeg: c.PitchMilliDeg, FovYMilliDeg: c.FovYMilliDeg, AspectQ: c.AspectQ})
	}
	return out, nil
}

func phaseG(gQ int64, path string) (PhaseFunction, error) {
	if gQ < -CoeffScale || gQ > CoeffScale {
		return PhaseFunction{}, NewBizError(CodeSchemaField, path, "anisotropy Q %d outside [-%d,%d]", gQ, CoeffScale, CoeffScale)
	}
	return PhaseFunction{Type: "henyey-greenstein", GQ: gQ}, nil
}

func lightDir(v [3]int64) Vec3 {
	return Vec3{X: float64(v[0]) / float64(CoeffScale), Y: float64(v[1]) / float64(CoeffScale), Z: float64(v[2]) / float64(CoeffScale)}.Normalize()
}
