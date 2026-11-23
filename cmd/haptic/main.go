package main

import (
	"flag"
	"fmt"
	"os"

	"haptickernel/internal/device"
	"haptickernel/internal/diag"
)

func main() {
	dataDir := flag.String("data", "data", "确定性样本输出目录")
	cpDir := flag.String("checkpoints", "checkpoints", "文件检查点根目录")
	workers := flag.Int("workers", 3, "通道量化最大并行度")
	noServer := flag.Bool("no-http", false, "只跑验证不启动诊断界面")
	miss := flag.String("fault-miss", "6", "注入缺包的 seq 列表")
	dup := flag.String("fault-dup", "11", "注入重发的 seq 列表")
	timeout := flag.String("fault-timeout", "17", "注入超时的 seq 列表")
	flag.Parse()

	state := diag.NewState()
	fmt.Println(">> [1/4] 生成确定性样本并校验，执行三路径量化流水线 ...")
	if err := state.Bootstrap(*dataDir, *cpDir, *workers); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}
	snap := state.Snapshot()
	fmt.Printf("    事件数=%d 通道数=%d 实时路径包数=%d 采样点=%d\n",
		len(snap.Events), snap.Manifest.ChannelCount,
		len(snap.Realtime.Packets), snap.Realtime.SampleTotal)

	fmt.Println(">> [2/4] 三路径逐采样/逐包比对 ...")
	if snap.CompareReport.Passed {
		fmt.Println("    PASS: 实时 / 断点恢复 / 全量重放 逐采样点与逐包完全一致")
		for _, p := range snap.CompareReport.PathOrder {
			fmt.Printf("      - %-8s 采样点=%d 包数=%d\n",
				p, snap.CompareReport.SampleTotals[p], snap.CompareReport.PacketCounts[p])
		}
	} else {
		fmt.Println("    FAIL E_DIFF:", snap.CompareErr)
	}
	fmt.Printf("    串行 vs 并行(workers=%d) identical=%v 背压=%d\n",
		snap.SerialVsParallel.Workers, snap.SerialVsParallel.Identical,
		len(snap.SerialVsParallel.Backpressure))

	fmt.Println(">> [3/4] 启动回环模拟设备并注入异常 ...")
	fault := device.FaultSpec{
		MissSeqs:    parseList(*miss),
		DupSeqs:     parseList(*dup),
		TimeoutSeqs: parseList(*timeout),
	}
	if err := state.RunDeviceLoop(fault); err != nil {
		fmt.Fprintln(os.Stderr, "设备回环失败:", err)
		os.Exit(1)
	}
	snap = state.Snapshot()
	fmt.Printf("    设备=%s 对齐包数=%d 异常数=%d\n",
		snap.DeviceURL, snap.AlignReport.AlignedCount, len(snap.AlignReport.Anomalies))
	for _, a := range snap.AlignReport.Anomalies {
		fmt.Printf("      - [%s/%s] seq=%d channel=%d time(us)=%d %s\n",
			a.Code, a.Side, a.Seq, a.Channel, a.LogicalTime, a.Reason)
	}

	fmt.Println(">> [4/4] 失效传播（编辑首个事件优先级） ...")
	reused, invalid := 0, 0
	for _, e := range snap.Invalidations {
		if e.Reused {
			reused++
		} else {
			invalid++
		}
	}
	fmt.Printf("    复用缓存包=%d 失效重算包=%d（清单顺序与提交顺序一致）\n", reused, invalid)

	if *noServer {
		fmt.Println("    --no-http 指定，跳过诊断界面。")
		return
	}
	ui := diag.NewHTTPServer(state)
	addr, err := ui.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "诊断界面启动失败:", err)
		os.Exit(1)
	}
	fmt.Printf("\n诊断界面已启动（仅回环）: %s\n按 Ctrl+C 退出。\n", addr)
	select {}
}

func parseList(s string) []int {
	out := []int{}
	cur := 0
	has := false
	for _, ch := range s {
		if ch == ',' {
			if has {
				out = append(out, cur)
			}
			cur, has = 0, false
			continue
		}
		if ch >= '0' && ch <= '9' {
			cur = cur*10 + int(ch-'0')
			has = true
		}
	}
	if has {
		out = append(out, cur)
	}
	return out
}
