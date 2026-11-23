package persist

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"occlbench/internal/culling"
	"occlbench/internal/scene"
)

// Identity 快照身份指纹。任一维度变化都必须整体作废旧快照。
type Identity struct {
	ContentHash    string  `json:"contentHash"`
	TrajectoryFP   string  `json:"trajectoryFp"`
	QuantVersion   string  `json:"quantVersion"`
	AlgoVersion    string  `json:"algoVersion"`
	GridResolution int     `json:"gridResolution"`
	SampleStride   int     `json:"sampleStride"`
	Slots          int     `json:"slots"`
	Memory         float64 `json:"memory"`
	WindowSize     int     `json:"windowSize"`
}

// ContentHash 对场景世界内容（分块、遮挡体、真值）求稳定哈希。
func ContentHash(w *scene.World) string {
	core := struct {
		Seed       int64             `json:"seed"`
		Partitions []scene.Partition `json:"partitions"`
		Occluders  []scene.Occluder  `json:"occluders"`
		Truth      scene.GroundTruth `json:"groundTruth"`
	}{Seed: w.Seed, Partitions: w.Partitions, Occluders: w.Occluders, Truth: w.GroundTruth}
	return stableHash(core)
}

// TrajectoryFingerprint 对相机轨迹求稳定哈希。
func TrajectoryFingerprint(w *scene.World) string {
	return stableHash(w.Trajectory)
}

// BuildIdentity 组装完整快照身份。
func BuildIdentity(w *scene.World, params culling.Params, slots int, memory float64, windowSize int) Identity {
	return Identity{
		ContentHash:    ContentHash(w),
		TrajectoryFP:   TrajectoryFingerprint(w),
		QuantVersion:   params.Quant.Version,
		AlgoVersion:    scene.AlgorithmVersion,
		GridResolution: params.GridResolution,
		SampleStride:   params.SampleStride,
		Slots:          slots,
		Memory:         memory,
		WindowSize:     windowSize,
	}
}

func stableHash(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "hash-error"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Match 判断两个身份是否完全一致；任意字段不同即视为版本失配。
func (a Identity) Match(b Identity) bool {
	return a.ContentHash == b.ContentHash &&
		a.TrajectoryFP == b.TrajectoryFP &&
		a.QuantVersion == b.QuantVersion &&
		a.AlgoVersion == b.AlgoVersion &&
		a.GridResolution == b.GridResolution &&
		a.SampleStride == b.SampleStride &&
		a.Slots == b.Slots &&
		a.Memory == b.Memory &&
		a.WindowSize == b.WindowSize
}

// DiffFields 返回失配字段名列表，用于诊断与差异定位。
func (a Identity) DiffFields(b Identity) []string {
	diffs := []string{}
	add := func(name string, x, y any) {
		if x != y {
			diffs = append(diffs, name)
		}
	}
	add("contentHash", a.ContentHash, b.ContentHash)
	add("trajectoryFp", a.TrajectoryFP, b.TrajectoryFP)
	add("quantVersion", a.QuantVersion, b.QuantVersion)
	add("algoVersion", a.AlgoVersion, b.AlgoVersion)
	add("gridResolution", a.GridResolution, b.GridResolution)
	add("sampleStride", a.SampleStride, b.SampleStride)
	add("slots", a.Slots, b.Slots)
	add("memory", a.Memory, b.Memory)
	add("windowSize", a.WindowSize, b.WindowSize)
	return diffs
}
