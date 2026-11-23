// Package generator 按固定种子确定性产出场景工程、相机轨迹与真值清单。
// 真值由独立的射线采样参考实现计算，与剔除引擎的覆盖栅格实现相互独立，
// 用于交叉验证两条算法路径的一致性。
package generator

import (
	"math"

	"occlbench/internal/core"
	"occlbench/internal/scene"
)

// deterministicUnit 返回由 (seed, salt) 确定性派生的 [0,1) 伪随机值，
// 不持有可变状态，保证同种子跨进程/跨平台完全可复现。
func deterministicUnit(seed int64, salt int) float64 {
	h := uint64(seed)*1099511628211 + uint64(salt)*1469598103934665603 + 1469598103934665603
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	return float64(h>>11) / float64(1<<53)
}

// Config 生成配置。
type Config struct {
	Seed       int64
	Name       string
	Partitions int
	Cols       int
	Rows       int
	Samples    int
	Width      float64
	Depth      float64
	Height     float64
}

// DefaultConfig 默认确定性布局。
func DefaultConfig() Config {
	return Config{
		Seed: 42, Name: "deterministic-sample",
		Partitions: 4, Cols: 4, Rows: 4, Samples: 12,
		Width: 16, Depth: 16, Height: 4,
	}
}

// Generate 生成完整世界工程（含真值）。
func Generate(cfg Config) *scene.World {
	w := &scene.World{
		SchemaVersion: "occlbench/v1",
		Seed:          cfg.Seed,
		Name:          cfg.Name,
	}

	chunksPerPartition := (cfg.Cols * cfg.Rows) / cfg.Partitions
	chunkID := 0
	for p := 0; p < cfg.Partitions; p++ {
		part := scene.Partition{ID: p, Name: partitionName(p)}
		for k := 0; k < chunksPerPartition; k++ {
			idx := p*chunksPerPartition + k
			col := idx % cfg.Cols
			row := idx / cfg.Cols
			cw := cfg.Width / float64(cfg.Cols)
			cd := cfg.Depth / float64(cfg.Rows)
			box := core.AABB{
				Min: core.Vec3{X: float64(col) * cw, Y: 0, Z: float64(row) * cd},
				Max: core.Vec3{X: float64(col+1) * cw, Y: cfg.Height, Z: float64(row+1) * cd},
			}
			ch := scene.Chunk{
				ID: chunkID, LocalIndex: k, Partition: p, Box: box,
				Complexity:   200 + int(deterministicUnit(cfg.Seed, chunkID*3+0)*800),
				LoadCost:     math.Round((1+deterministicUnit(cfg.Seed, chunkID*3+1)*4)*100) / 100,
				MemoryCost:   math.Round((1+deterministicUnit(cfg.Seed, chunkID*3+2)*3)*100) / 100,
				Color:        palette[(p+k)%len(palette)],
				OccluderRefs: []int{},
			}
			part.Chunks = append(part.Chunks, ch)
			chunkID++
		}
		w.Partitions = append(w.Partitions, part)
	}

	buildOccluders(w, cfg)
	buildTrajectory(w, cfg)
	buildGroundTruth(w, cfg)
	return w
}

func partitionName(p int) string {
	return []string{"north", "east", "south", "west"}[p%4]
}

var palette = []string{"#4e79a7", "#f28e2b", "#59a14f", "#e15759", "#76b7b2", "#edc948", "#b07aa1", "#ff9da7"}

// buildOccluders 在每行分块交界处放置带缺口的实体墙（轴对齐盒）。
func buildOccluders(w *scene.World, cfg Config) {
	cd := cfg.Depth / float64(cfg.Rows)
	wallThickness := 0.5
	occID := 0
	// 行 1..Rows-1 各放一道墙，每道墙留一个缺口列。
	for row := 1; row < cfg.Rows; row++ {
		// 缺口列按行确定性轮换且互不相同，保证射线真值在规整栅格下精确，
		// 同时仍由固定种子驱动，可复现多套布局。
		gapCol := (row*3 + int(cfg.Seed%int64(cfg.Cols)) + cfg.Cols) % cfg.Cols
		z := float64(row)*cd - wallThickness/2
		for col := 0; col < cfg.Cols; col++ {
			if col == gapCol {
				continue
			}
			cw := cfg.Width / float64(cfg.Cols)
			box := core.AABB{
				Min: core.Vec3{X: float64(col) * cw, Y: 0, Z: z - wallThickness/2},
				Max: core.Vec3{X: float64(col+1) * cw, Y: cfg.Height, Z: z + wallThickness/2},
			}
			// 找到该墙所在格子对应的分块作为归属。
			refChunk := row*cfg.Cols + col
			oc := scene.Occluder{ID: occID, Box: box, RefChunk: refChunk}
			w.Occluders = append(w.Occluders, oc)
			attachRef(w, refChunk, occID)
			occID++
		}
	}
}

func attachRef(w *scene.World, chunkID, occID int) {
	for p := range w.Partitions {
		for c := range w.Partitions[p].Chunks {
			if w.Partitions[p].Chunks[c].ID == chunkID {
				w.Partitions[p].Chunks[c].OccluderRefs = append(
					w.Partitions[p].Chunks[c].OccluderRefs, occID)
				return
			}
		}
	}
}

func buildTrajectory(w *scene.World, cfg Config) {
	// 相机在第一行前方向 +Z 观察，沿 X 往返，保证视锥与墙体遮挡都被覆盖。
	for i := 0; i < cfg.Samples; i++ {
		phase := float64(i) / float64(cfg.Samples-1)
		x := phase * cfg.Width
		pos := core.Vec3{X: x, Y: 2.0, Z: -2.0}
		s := scene.CameraSample{
			T:       phase * 10,
			Pos:     pos,
			Forward: core.Vec3{X: 0, Y: -0.05, Z: 1}.Normalize(),
			Up:      core.Vec3{X: 0, Y: 1, Z: 0},
			FOVy:    60, Aspect: 16.0 / 9.0, Near: 0.1, Far: 100,
		}
		w.Trajectory.Samples = append(w.Trajectory.Samples, s)
	}
}
