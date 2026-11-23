// 确定性样本生成器入口：go run ./cmd/generator -dir ./data/sample
package main

import (
	"flag"
	"fmt"

	"occlbench/internal/generator"
	"occlbench/internal/scene"
)

func main() {
	dir := flag.String("dir", "data/sample", "场景工程输出目录")
	seed := flag.Int64("seed", 42, "固定随机种子")
	flag.Parse()

	cfg := generator.DefaultConfig()
	cfg.Seed = *seed
	world := generator.Generate(cfg)
	if err := world.Validate(); err != nil {
		fmt.Println("生成工程校验失败:", err)
		return
	}
	if err := world.Save(*dir); err != nil {
		fmt.Println("落盘失败:", err)
		return
	}
	fmt.Printf("已生成确定性场景工程：%s（分区=%d 分块=%d 遮挡体=%d 采样=%d）\n",
		*dir, len(world.Partitions), world.ChunkCount(), len(world.Occluders), len(world.Trajectory.Samples))
	_ = scene.AlgorithmVersion
}
