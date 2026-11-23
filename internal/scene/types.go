package scene

import "occlbench/internal/core"

// 版本常量随算法 / 量化参数语义变化而提升；任何一项变化都会整体作废旧快照。
const (
	AlgorithmVersion = "algo-1.0.0"
	// QuantVersion 标识包围盒量化与栅格化的默认参数族。
	QuantVersion = "quant-1.0.0"
)

// World 一份确定性生成的场景工程。
type World struct {
	SchemaVersion string `json:"schemaVersion"`
	Seed          int64  `json:"seed"`
	Name          string `json:"name"`
	// Partitions 为逻辑分区编号 -> 该分区下的分块列表。
	Partitions  []Partition `json:"partitions"`
	Occluders   []Occluder  `json:"occluders"`
	Trajectory  Trajectory  `json:"trajectory"`
	GroundTruth GroundTruth `json:"groundTruth"`
}

// Partition 逻辑分区，内含若干分块；编号即其在 Partitions 中的下标。
type Partition struct {
	ID     int     `json:"id"`
	Name   string  `json:"name"`
	Chunks []Chunk `json:"chunks"`
}

// Chunk 流式加载的最小单位。
type Chunk struct {
	// ID 为全局唯一分块编号；LocalIndex 为其在所属分区内的下标。
	ID         int       `json:"id"`
	LocalIndex int       `json:"localIndex"`
	Partition  int       `json:"partition"`
	Box        core.AABB `json:"box"`
	// OccluderRefs 引用 World.Occluders 的下标。
	OccluderRefs []int   `json:"occluderRefs"`
	Complexity   int     `json:"complexity"`
	LoadCost     float64 `json:"loadCost"`
	MemoryCost   float64 `json:"memoryCost"`
	Color        string  `json:"color"`
}

// Occluder 遮挡体：样本场景使用轴对齐实体盒，便于确定性栅格化与求交。
type Occluder struct {
	ID  int       `json:"id"`
	Box core.AABB `json:"box"`
	// RefChunk 该遮挡体归属的分块，用于真值记录参考遮挡体。
	RefChunk int `json:"refChunk"`
}

// CameraSample 轨迹上的一个采样时刻。
type CameraSample struct {
	T          float64      `json:"t"`
	Pos        core.Vec3    `json:"pos"`
	Forward    core.Vec3    `json:"forward"`
	Up         core.Vec3    `json:"up"`
	FOVy       float64      `json:"fovYDeg"`
	Aspect     float64      `json:"aspect"`
	Near       float64      `json:"near"`
	Far        float64      `json:"far"`
	Projection *ScreenPoint `json:"projection,omitempty"`
}

// ScreenPoint 已知采样点的屏幕投影坐标（像素，原点左上）。
type ScreenPoint struct {
	ChunkID int     `json:"chunkId"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
}

// Trajectory 按逻辑时间单调排列的相机轨迹。
type Trajectory struct {
	Samples []CameraSample `json:"samples"`
}

// GroundTruth 真值清单。
type GroundTruth struct {
	// Visible[sampleIndex] 为该时刻应可见的分块 ID 集合。
	Visible [][]int `json:"visible"`
	// OccluderBasis[chunkID] 为该分块可见性判定所依据的参考遮挡体 ID（可空）。
	OccluderBasis map[int][]int `json:"occluderBasis"`
	// ScreenChecks 已知采样点屏幕投影坐标。
	ScreenChecks []ScreenCheck `json:"screenChecks"`
}

// ScreenCheck 单个屏幕投影真值点。
type ScreenCheck struct {
	SampleIndex int     `json:"sampleIndex"`
	ChunkID     int     `json:"chunkId"`
	X           float64 `json:"x"`
	Y           float64 `json:"y"`
}

// ChunkByID 建立分块 ID -> 分块指针的索引。
func (w *World) ChunkByID() map[int]*Chunk {
	m := make(map[int]*Chunk)
	for p := range w.Partitions {
		for c := range w.Partitions[p].Chunks {
			ch := &w.Partitions[p].Chunks[c]
			m[ch.ID] = ch
		}
	}
	return m
}

// ChunkCount 分块总数。
func (w *World) ChunkCount() int {
	n := 0
	for p := range w.Partitions {
		n += len(w.Partitions[p].Chunks)
	}
	return n
}

// AllChunks 按稳定顺序返回全部分块。
func (w *World) AllChunks() []Chunk {
	out := make([]Chunk, 0, w.ChunkCount())
	for p := range w.Partitions {
		out = append(out, w.Partitions[p].Chunks...)
	}
	return out
}
