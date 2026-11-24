package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"audiocal"
	"audiocal/device"
)

func main() {
	dataDir := flag.String("data", "data", "样本与检查点目录")
	port := flag.Int("port", 0, "设备服务端口，0=随机高位端口")
	keepServer := flag.Bool("keep-server", false, "演示结束后保留服务（默认关闭）")
	flag.Parse()

	must := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "失败:", err)
			os.Exit(1)
		}
	}

	// 1. 确定性样本。
	samples := audiocal.GenerateSamples(20261124, 3)
	manifestPath, err := audiocal.SaveManifest(*dataDir, "samples.json", samples)
	must(err)
	loaded, err := audiocal.LoadManifest(manifestPath)
	must(err)
	sample := loaded[0]
	fmt.Printf("== 加载测量记录 %s（%d 通道）==\n", sample.ID, len(sample.Channels))

	cfg := audiocal.DefaultFitConfig()

	// 2. 拟合。
	model, err := audiocal.Fit(sample, cfg)
	must(err)
	fmt.Printf("== 拟合完成，SSE=%.6f，通道数 %d ==\n", model.TotalSSE(), len(model.Channels))

	// 3. 三条导出路径，各自独立检查点目录。
	interactiveDir := filepath.Join(*dataDir, "checkpoints", "interactive")
	resumableDir := filepath.Join(*dataDir, "checkpoints", "resumable")
	fullDir := filepath.Join(*dataDir, "checkpoints", "full")
	removeAll(interactiveDir, resumableDir, fullDir)

	rInteractive, err := audiocal.ExportInteractive(model, audiocal.NewCheckpointStore(interactiveDir))
	must(err)

	// 断点恢复：先交互式提交前两段模拟“中断”，再走恢复路径。
	exp, err := audiocal.NewExporter(model, audiocal.NewCheckpointStore(resumableDir))
	must(err)
	_, total := exp.Progress()
	cut := 2
	if cut > total {
		cut = total
	}
	for i := 0; i < cut; i++ {
		_, err = exp.ExportSegment(i)
		must(err)
	}
	rResumable, err := audiocal.ExportResumable(model, audiocal.NewCheckpointStore(resumableDir))
	must(err)

	rFull, err := audiocal.ExportAllAtOnce(model, audiocal.NewCheckpointStore(fullDir))
	must(err)

	must(audiocal.CompareResults(rInteractive, rResumable))
	must(audiocal.CompareResults(rInteractive, rFull))
	n, hash := rInteractive.Firmware.ByteSummary()
	fmt.Printf("== 三条导出路径逐字节一致：%d 字节, sha256=%s, 提交序列 %v ==\n",
		n, hash[:16], rInteractive.CommitSequence)

	// 4. 启动本地回环模拟设备服务并下发回显。
	srv, err := device.Listen(*port)
	must(err)
	defer func() {
		if !*keepServer {
			_ = srv.Close()
		}
	}()
	client := device.NewClient(srv.Addr())
	resp, err := client.Upload(rInteractive.Bytes, model.Grid, model.SampleRate, 0.01, device.FaultNone)
	must(err)

	var reports []*audiocal.EchoReport
	for chID, applied := range resp.AppliedDB {
		rep, err := audiocal.CompareEcho(model, chID, applied, 0.01)
		must(err)
		reports = append(reports, rep)
	}
	fmt.Printf("== 回环回显校验通过：%d 通道，最大偏差 %.6f dB ==\n", len(reports), maxDelta(reports))

	// 5. 异常注入链路。
	faults := []struct {
		name string
		f    device.Fault
		want audiocal.ErrorCode
	}{
		{"固件版本拒收", device.FaultVersion, audiocal.CodeFW},
		{"时序窗越界", device.FaultWindow, audiocal.CodeWin},
		{"通道映射冲突", device.FaultMap, audiocal.CodeMap},
		{"段缺失", device.FaultSegment, audiocal.CodeSeg},
	}
	for _, tc := range faults {
		_, err := client.Upload(rInteractive.Bytes, model.Grid, model.SampleRate, 0.01, tc.f)
		be, ok := audiocal.AsBizError(err)
		if !ok || be.Code != tc.want {
			fmt.Fprintf(os.Stderr, "异常注入 %s 未返回预期码 %v, got %v\n", tc.name, tc.want, err)
			os.Exit(1)
		}
		fmt.Printf("== 异常注入 %-12s -> %s (%s) ==\n", tc.name, be.Code, be.Field)
	}

	// 回显漂移 -> E_ECHO。
	respDrift, err := client.Upload(rInteractive.Bytes, model.Grid, model.SampleRate, 0.01, device.FaultEchoDrift)
	must(err)
	for chID, applied := range respDrift.AppliedDB {
		_, err := audiocal.CompareEcho(model, chID, applied, 0.01)
		be, ok := audiocal.AsBizError(err)
		if !ok || be.Code != audiocal.CodeEcho {
			fmt.Fprintln(os.Stderr, "回显漂移未触发 E_ECHO")
			os.Exit(1)
		}
		fmt.Printf("== 回显漂移 -> E_ECHO, 首个超差点 %s, local=%.4f echo=%.4f ==\n",
			be.Field, be.Detail["local_db"], be.Detail["echo_db"])
		break
	}

	// 6. 面板。
	panel := audiocal.NewPanel(model, exp)
	panel.SetEcho(reports)
	fmt.Println()
	fmt.Print(panel.Render())
}

func maxDelta(reps []*audiocal.EchoReport) float64 {
	var m float64
	for _, r := range reps {
		if r.MaxDelta > m {
			m = r.MaxDelta
		}
	}
	return m
}

func removeAll(dirs ...string) {
	for _, d := range dirs {
		_ = os.RemoveAll(d)
	}
}
