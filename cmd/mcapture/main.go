package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mcapture/internal/pipeline"
	"mcapture/internal/session"
	"mcapture/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	var err error
	switch cmd {
	case "generate":
		err = cmdGenerate(fs, os.Args[2:])
	case "run":
		err = cmdRun(fs, os.Args[2:])
	case "resume":
		err = cmdResume(fs, os.Args[2:])
	case "realtime":
		err = cmdRealtime(fs, os.Args[2:])
	case "verify":
		err = cmdVerify(fs, os.Args[2:])
	case "export":
		err = cmdExport(fs, os.Args[2:])
	case "inject":
		err = cmdInject(fs, os.Args[2:])
	case "web":
		err = cmdWeb(fs, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mcapture - 多通道采集联调桌面应用

用法:
  mcapture generate -dir <会话目录> [-seed N] [-channels N] [-shape pulse|chirp]
  mcapture run      -dir <会话目录> -work <工作目录> [-workers N] [-seg N] [-order F] [-halfwin N] [-tol F]
  mcapture realtime -dir <会话目录> -work <工作目录> [-workers N] [同上参数]
  mcapture resume   -dir <会话目录> -work <工作目录> [-workers N] [同上参数]
  mcapture verify   -dir <会话目录> -work <工作目录> [同上参数]
  mcapture export   -dir <会话目录> -work <工作目录> -out <导出目录>
  mcapture inject   -dir <输出会话目录> -fault chan|mark|clip [-marker N] [-gain F]
  mcapture web      -dir <会话目录> -work <工作目录> [-addr 127.0.0.1:8080]
`)
}

type runFlags struct {
	dir, work, out, addr, shape, fault string
	workers                            int
	seg                                int
	halfWin                            int
	seed, channels                     int
	marker                             int
	order, tol, gain                   float64
}

func bindRun(fs *flag.FlagSet, f *runFlags) {
	fs.StringVar(&f.dir, "dir", "", "会话目录")
	fs.StringVar(&f.work, "work", "", "工作目录(SQLite 检查点)")
	fs.StringVar(&f.out, "out", "", "导出目录")
	fs.StringVar(&f.addr, "addr", "127.0.0.1:8080", "回环监听地址")
	fs.IntVar(&f.workers, "workers", 3, "工作线程数")
	fs.IntVar(&f.seg, "seg", 0, "分段长度(采样点), 0 用清单默认")
	fs.IntVar(&f.halfWin, "halfwin", 16, "sinc 插值单边窗长")
	fs.Float64Var(&f.order, "order", 1.0, "sinc 插值阶数系数")
	fs.Float64Var(&f.tol, "tol", 0.25, "漂移核对容差(采样点)")
	fs.IntVar(&f.seed, "seed", 0, "生成种子")
	fs.IntVar(&f.channels, "channels", 4, "通道数")
	fs.StringVar(&f.shape, "shape", "pulse", "标记形状 pulse|chirp")
	fs.StringVar(&f.fault, "fault", "", "注入故障 chan|mark|clip")
	fs.IntVar(&f.marker, "marker", 3, "mark 故障的标记序号")
	fs.Float64Var(&f.gain, "gain", 4.0, "clip 故障增益")
}

func configFromFlags(sess *session.Session, f runFlags) pipeline.AlgoConfig {
	cfg := pipeline.DefaultConfig(sess.Manifest)
	cfg.MaxWorkers = f.workers
	cfg.HalfWindow = f.halfWin
	cfg.SincOrder = f.order
	cfg.Tolerance = f.tol
	if f.seg > 0 {
		cfg.SegmentSamples = int64(f.seg)
	}
	return cfg
}

func cmdGenerate(fs *flag.FlagSet, args []string) error {
	var f runFlags
	bindRun(fs, &f)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if f.dir == "" {
		return fmt.Errorf("generate 需要 -dir")
	}
	cfg := session.DefaultGenConfig()
	if f.seed != 0 {
		cfg.Seed = int64(f.seed)
	}
	cfg.Channels = f.channels
	cfg.MarkerShape = f.shape
	m, faults, err := session.Generate(f.dir, cfg)
	if err != nil {
		return err
	}
	fmt.Printf("generated session=%s channels=%d points/chan=%d markers=%d faults=%d dir=%s\n",
		m.SessionID, len(m.Channels_), m.PointsPerChan, len(m.RefChannel().Markers), len(faults), f.dir)
	return nil
}

func cmdRun(fs *flag.FlagSet, args []string) error {
	return executeMode(fs, args, pipeline.ModeFull)
}
func cmdRealtime(fs *flag.FlagSet, args []string) error {
	return executeMode(fs, args, pipeline.ModeRealtime)
}
func cmdResume(fs *flag.FlagSet, args []string) error {
	return executeMode(fs, args, pipeline.ModeResume)
}

func executeMode(fs *flag.FlagSet, args []string, mode pipeline.Mode) error {
	var f runFlags
	bindRun(fs, &f)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if f.dir == "" || f.work == "" {
		return fmt.Errorf("%s 需要 -dir 与 -work", mode)
	}
	sess, err := session.Load(f.dir)
	if err != nil {
		return err
	}
	cfg := configFromFlags(sess, f)
	res, st, err := pipeline.RunPath(context.Background(), sess, cfg,
		filepath.Join(f.work, "checkpoint.db"), mode, f.workers)
	if err != nil {
		return err
	}
	defer st.Close()
	committed, total := res.Progress[0], res.Progress[1]
	fmt.Printf("mode=%s committed=%d/%d commits=%d events=%d\n",
		mode, committed, total, len(res.CommitSeq), len(res.Events))
	return nil
}

func cmdVerify(fs *flag.FlagSet, args []string) error {
	var f runFlags
	bindRun(fs, &f)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if f.dir == "" || f.work == "" {
		return fmt.Errorf("verify 需要 -dir 与 -work")
	}
	sess, err := session.Load(f.dir)
	if err != nil {
		return err
	}
	cfg := configFromFlags(sess, f)

	fullDB := filepath.Join(f.work, "verify-full.db")
	rtDB := filepath.Join(f.work, "verify-realtime.db")
	resFull, st1, err := pipeline.RunPath(context.Background(), sess, cfg, fullDB, pipeline.ModeFull, f.workers)
	if err != nil {
		return err
	}
	st1.Close()
	resRT, st2, err := pipeline.RunPath(context.Background(), sess, cfg, rtDB, pipeline.ModeRealtime, 1)
	if err != nil {
		return err
	}
	st2.Close()
	// 续跑：先在独立库上跑一半再恢复（通过测试入口更彻底，CLI 做整库恢复演示）。
	resResume, st3, err := pipeline.RunPath(context.Background(), sess, cfg,
		filepath.Join(f.work, "verify-resume.db"), pipeline.ModeResume, f.workers)
	if err != nil {
		return err
	}
	st3.Close()

	if err := pipeline.CompareResults(sess, cfg, resFull, resRT); err != nil {
		return err
	}
	if err := pipeline.CompareResults(sess, cfg, resFull, resResume); err != nil {
		return err
	}
	fmt.Println("VERIFY OK: full / realtime / resume 三路逐样本一致，提交序列一致")
	return nil
}

func cmdExport(fs *flag.FlagSet, args []string) error {
	var f runFlags
	bindRun(fs, &f)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if f.dir == "" || f.work == "" || f.out == "" {
		return fmt.Errorf("export 需要 -dir -work -out")
	}
	sess, err := session.Load(f.dir)
	if err != nil {
		return err
	}
	cfg := configFromFlags(sess, f)
	res, st, err := pipeline.RunPath(context.Background(), sess, cfg,
		filepath.Join(f.work, "checkpoint.db"), pipeline.ModeFull, f.workers)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := pipeline.ExportMultiChannel(f.out, sess, res); err != nil {
		return err
	}
	fmt.Printf("exported %d channels to %s\n", len(sess.Channels), f.out)
	return nil
}

func cmdInject(fs *flag.FlagSet, args []string) error {
	var f runFlags
	bindRun(fs, &f)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if f.dir == "" || f.fault == "" {
		return fmt.Errorf("inject 需要 -dir 与 -fault")
	}
	cfg := session.DefaultGenConfig()
	cfg.Channels = f.channels
	cfg.MarkerShape = f.shape
	switch f.fault {
	case "chan":
		cfg.DropChannel = "ch1"
	case "mark":
		cfg.DropMarker = f.marker
	case "clip":
		cfg.ClipGain = f.gain
	default:
		return fmt.Errorf("未知故障类型 %q", f.fault)
	}
	m, faults, err := session.Generate(f.dir, cfg)
	if err != nil {
		return err
	}
	fmt.Printf("injected session=%s faults=%+v dir=%s\n", m.SessionID, faults, f.dir)
	return nil
}

func cmdWeb(fs *flag.FlagSet, args []string) error {
	var f runFlags
	bindRun(fs, &f)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if f.dir == "" || f.work == "" {
		return fmt.Errorf("web 需要 -dir 与 -work")
	}
	srv, err := web.NewServer(f.dir, f.work, f.addr)
	if err != nil {
		return err
	}
	fmt.Printf("诊断页面: http://%s/\n", f.addr)
	return srv.ListenAndServe()
}
