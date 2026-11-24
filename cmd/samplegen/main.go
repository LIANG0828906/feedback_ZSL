package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"audiocal"
)

func main() {
	dir := flag.String("dir", "data", "样本输出目录")
	name := flag.String("name", "samples.json", "样本清单文件名")
	seed := flag.Int64("seed", 20261124, "确定性种子")
	n := flag.Int("n", 3, "生成测量记录条数")
	flag.Parse()

	samples := audiocal.GenerateSamples(*seed, *n)
	path, err := audiocal.SaveManifest(*dir, *name, samples)
	if err != nil {
		fmt.Fprintln(os.Stderr, "样本生成失败:", err)
		os.Exit(1)
	}
	abs, _ := filepath.Abs(path)
	fmt.Printf("已生成 %d 条确定性测量记录 -> %s\n", len(samples), abs)
	for _, s := range samples {
		fmt.Printf("  %s: %d 通道, IR 长度 %d, 目标频点 %d, 种子 %d\n",
			s.ID, len(s.Channels), s.IRLength, len(s.Target), s.Seed)
		for _, ch := range s.Channels {
			fmt.Printf("    通道 %d: 直达延迟 %.4fms 早期反射能量 %.5f 残差上界点数 %d\n",
				ch.ChannelID, ch.Truth.DirectDelaySec*1000, ch.Truth.EarlyEnergy,
				len(ch.Truth.ResidualUpperDB))
		}
	}
}
