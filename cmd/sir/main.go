// sir 是空间声学脉冲响应分析工作台的命令行入口。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"spatialir/internal/checkpoint"
	"spatialir/internal/compare"
	"spatialir/internal/generator"
	"spatialir/internal/loader"
	"spatialir/internal/model"
	"spatialir/internal/panel"
	"spatialir/internal/pipeline"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = cmdGenerate(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "export":
		err = cmdExport(os.Args[2:])
	case "faultdemo":
		err = cmdFaultDemo(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		if ae := model.AsAppError(err); ae != nil {
			data, _ := json.MarshalIndent(ae, "", "  ")
			fmt.Fprintln(os.Stderr, string(data))
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `sir - spatial impulse response workbench

usage:
  sir generate --out data                 生成内置确定性样本集
  sir run --dir data --sample stereo_48k --mode realtime [--pause N]
  sir verify --dir data --sample stereo_48k  三路径一致性 + 异常码 + 断点恢复
  sir serve --dir data --addr :8080       内置可视化面板
  sir export --report report.json --out out.json
  sir faultdemo --dir data --sample stereo_48k --kind chan|order|clip|frame|bound
`)
}

func commonFlags(fs *flag.FlagSet) (*string, *string, *model.FrameParams) {
	dir := fs.String("dir", "data", "样本目录")
	sample := fs.String("sample", "stereo_48k", "样本名")
	p := &model.FrameParams{}
	fs.IntVar(&p.FrameLength, "frame-length", 1024, "帧长（样本）")
	fs.IntVar(&p.FrameHop, "frame-hop", 512, "帧移（样本）")
	fs.IntVar(&p.WindowLen, "window-len", 768, "估计窗长（样本）")
	fs.IntVar(&p.ConvWindow, "conv-window", 64, "卷积窗长（样本）")
	fs.IntVar(&p.RenderBlock, "render-block", 512, "渲染块大小（样本）")
	fs.IntVar(&p.InterpOrder, "interp-order", 1, "插值阶数 0/1/3")
	fs.Float64Var(&p.BoundScale, "bound-scale", 1.0, "误差上界收紧系数")
	fs.IntVar(&p.MaxWorkers, "workers", 4, "最大并行度")
	fs.IntVar(&p.BufferCap, "buffer-cap", 4096, "每线程采样缓冲上限")
	return dir, sample, p
}

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "data", "输出目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, spec := range generator.BuiltinSpecs() {
		dir := filepath.Join(*out, spec.Name)
		m, _, _, err := generator.Generate(spec, dir)
		if err != nil {
			return err
		}
		fmt.Printf("generated %s: %d channels, %d samples, %d Hz -> %s\n",
			spec.Name, m.Layout.Channels, m.LengthSamples, m.SampleRate, dir)
	}
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	dir, sample, params := commonFlags(fs)
	mode := fs.String("mode", "realtime", "realtime|resume|replay")
	pause := fs.Int("pause", 0, "提交 N 帧后暂停（断点续跑演示）")
	reset := fs.Bool("reset", false, "运行前清空检查点")
	reportPath := fs.String("report", "", "报告 JSON 输出路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sampleDir := filepath.Join(*dir, *sample)
	ds, err := loader.Load(sampleDir, *sample+"_manifest.json")
	if err != nil {
		return err
	}
	if err := loader.ValidateParams(*params, ds.Manifest.LengthSamples); err != nil {
		return err
	}
	store, err := checkpoint.Open(filepath.Join(sampleDir, *sample+".db"))
	if err != nil {
		return err
	}
	defer store.Close()
	if *reset {
		if err := store.Reset(); err != nil {
			return err
		}
	}
	eng := pipeline.NewEngine(ds, *params)
	res, err := eng.Run(context.Background(), pipeline.Options{
		Dataset: ds, Params: *params, Store: store,
		Mode: pipeline.RunMode(*mode), PauseAt: *pause,
	})
	if err != nil {
		return err
	}
	fmt.Printf("mode=%s committed=%d/%d pool_events=%d\n",
		res.Report.Mode, res.Report.Committed, res.Report.Frames, len(res.PoolEvents))
	if *reportPath != "" {
		return writeJSON(*reportPath, res.Report)
	}
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir, sample, params := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runVerify(*dir, *sample, *params)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dir := fs.String("dir", "data", "样本目录")
	addr := fs.String("addr", "127.0.0.1:8080", "监听地址")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return panel.Serve(*dir, *addr)
}

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	report := fs.String("report", "", "输入报告 JSON")
	out := fs.String("out", "", "导出路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw, err := os.ReadFile(*report)
	if err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	return writeJSON(*out, v)
}

// cmdFaultDemo 通过输入或参数注入四类异常与 E_BOUND，验证错误码链路。
// 注入完全由清单/参数驱动，不修改任何源码。
func cmdFaultDemo(args []string) error {
	fs := flag.NewFlagSet("faultdemo", flag.ContinueOnError)
	dir, sample, params := commonFlags(fs)
	kind := fs.String("kind", "frame", "chan|order|clip|frame|bound")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sampleDir := filepath.Join(*dir, *sample)
	manifestPath := filepath.Join(sampleDir, *sample+"_manifest.json")
	manifest, err := generator.ReadManifest(manifestPath)
	if err != nil {
		return err
	}
	switch *kind {
	case "chan":
		manifest.Faults = model.FaultSpec{MissingChannel: manifest.Layout.Names[0]}
	case "order":
		manifest.Faults = model.FaultSpec{OrderFrame: 1, OrderChannel: manifest.Layout.Names[0]}
	case "clip":
		manifest.Faults = model.FaultSpec{
			ClipFrame: 1, ClipChannel: manifest.Layout.Names[0], ClipGain: 5,
		}
	case "frame":
		manifest.Faults = model.FaultSpec{BadFrames: []int{1}}
	case "bound":
		params.BoundScale = 1e-9
	default:
		return model.NewError(model.CodePara, "unknown fault kind").WithField("kind")
	}
	if err := writeJSON(manifestPath, manifest); err != nil {
		return err
	}
	if *kind == "chan" {
		_, err = loader.Load(sampleDir, *sample+"_manifest.json")
		return err
	}
	rt, err := verifyReports(*dir, *sample, *params)
	if err != nil {
		return err
	}
	want := map[string]model.ErrorCode{
		"order": model.CodeOrder, "clip": model.CodeClip,
		"frame": model.CodeFrame, "bound": model.CodeBound,
	}[*kind]
	for _, s := range rt.Report.FrameReports {
		if s.FailCode == want {
			data, _ := json.MarshalIndent(s, "", "  ")
			fmt.Println(string(data))
			return nil
		}
	}
	return model.NewError(want, "fault frame not observed")
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// runVerify 是验收主流程的库内实现，供 CLI 与集成测试共用。
func runVerify(dir, sample string, params model.FrameParams) error {
	rt, err := verifyReports(dir, sample, params)
	if err != nil {
		return err
	}
	fmt.Printf("verify OK: %d frames, three paths sample-identical, committed=%d\n",
		rt.Report.Frames, rt.Report.Committed)
	return nil
}

// verifyReports 执行三条路径并逐样本比对，返回实时路径完整报告（含失败帧）。
func verifyReports(dir, sample string, params model.FrameParams) (*pipeline.Result, error) {
	sampleDir := filepath.Join(dir, sample)
	ds, err := loader.Load(sampleDir, sample+"_manifest.json")
	if err != nil {
		return nil, err
	}
	if err := loader.ValidateParams(params, ds.Manifest.LengthSamples); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(sampleDir, sample+"_verify.db")
	_ = os.Remove(dbPath)
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	eng := pipeline.NewEngine(ds, params)
	rt, err := eng.Run(context.Background(), pipeline.Options{
		Dataset: ds, Params: params, Store: store, Mode: pipeline.ModeRealtime})
	if err != nil {
		return nil, err
	}
	// 断点恢复：另起引擎模拟进程重启，只跑剩余帧（此处全量已提交，验证冻结复用）。
	eng2 := pipeline.NewEngine(ds, params)
	rs, err := eng2.Run(context.Background(), pipeline.Options{
		Dataset: ds, Params: params, Store: store, Mode: pipeline.ModeResume})
	if err != nil {
		return nil, err
	}
	if d := compare.Compare(rt.Report, rs.Report, eng.Frames()); d != nil {
		return nil, compare.AsError(d)
	}
	// 存在失败帧时检查点不完整，replay 语义上不适用；
	// 仅全提交样本才执行基于持久化参数的全量复算比对。
	if rt.Report.Committed != rt.Report.Frames {
		return rt, nil
	}
	// 全量复算：仅基于已持久化参数重建。
	eng3 := pipeline.NewEngine(ds, params)
	rp, err := eng3.Run(context.Background(), pipeline.Options{
		Dataset: ds, Params: params, Store: store, Mode: pipeline.ModeReplay})
	if err != nil {
		return nil, err
	}
	if d := compare.Compare(rt.Report, rp.Report, eng.Frames()); d != nil {
		return nil, compare.AsError(d)
	}
	return rt, nil
}
