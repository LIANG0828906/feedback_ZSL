// demo 是可重复演示管线：固定种子生成样本，跑实时/恢复/全量三条路径，
// 执行四类异常注入，并用独立复核工具核对持久化快照与真值。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"replaykernel/internal/engine"
	"replaykernel/internal/gen"
	"replaykernel/internal/model"
	"replaykernel/internal/store"
	"replaykernel/internal/verify"
)

func main() {
	var (
		seed      = flag.Int64("seed", 20261124, "固定主种子")
		sampleDir = flag.String("samples", "samples", "样本输出相对目录")
		dbDir     = flag.String("dbdir", "db", "SQLite 输出相对目录")
		logPath   = flag.String("log", filepath.Join("logs", "demo.log"), "日志相对路径")
	)
	flag.Parse()

	logger, closer := mustLogger(*logPath)
	defer closer()

	params := model.DefaultParams()
	fmt.Println("== 回合制对战回放验证内核 · 可重复演示 ==")
	fmt.Printf("seed=%d window=%d parallel=%d buffer=%d\n\n",
		*seed, params.WindowSize, params.MaxParallel, params.WorkerBufferCap)

	// 1) 固定种子生成多规模对局：数十到数万回合。
	specs := []gen.MatchSpec{
		{MatchID: "m_small", Turns: 60, Units: 4},
		{MatchID: "m_medium", Turns: 800, Units: 6},
		{MatchID: "m_large", Turns: 20000, Units: 8},
	}
	fmt.Println("[1/6] 生成确定性样本与真值 ...")
	hashes, err := gen.Generate(gen.Spec{
		OutDir: *sampleDir, Seed: *seed, MatchSpecs: specs,
	})
	must(err)
	for _, ms := range specs {
		logger.Printf("generated match=%s turns=%d units=%d replay_hash=%s",
			ms.MatchID, ms.Turns, ms.Units, hashes[ms.MatchID])
	}

	// 2) 三路径一致性：对每局分别落库 live / recovery / full。
	fmt.Println("[2/6] 实时结算 / 断点恢复 / 一次性全量重放 ...")
	for _, ms := range specs {
		runThreePaths(ms, *sampleDir, *dbDir, params, logger)
	}

	// 3) 独立复核：仅凭 SQLite 快照 + 真值指纹。
	fmt.Println("[3/6] 独立复核（不调用结算主路径）...")
	for _, ms := range specs {
		truth := loadTruth(filepath.Join(*sampleDir, "truth", ms.MatchID+".truth.json"))
		for _, mode := range []string{"live", "recovery", "full"} {
			dbPath := filepath.Join(*dbDir, ms.MatchID+"_"+mode+".db")
			rep, err := verify.Verify(dbPath, truth)
			must(err)
			status := "OK"
			if !rep.OK {
				status = fmt.Sprintf("VIOLATION first=%+v", rep.First)
			}
			fmt.Printf("  verify %-8s %-9s turns_checked=%-6d %s\n", ms.MatchID, mode, rep.Checked, status)
			logger.Printf("verify match=%s mode=%s ok=%v checked=%d", ms.MatchID, mode, rep.OK, rep.Checked)
		}
	}

	// 4) 异常注入链路。
	fmt.Println("[4/6] 四类异常注入链路 ...")
	runInjections(*sampleDir, *dbDir, *seed, params, logger)

	// 5) 失败窗口重试（不污染其他窗口）。
	fmt.Println("[5/6] 失败窗口隔离与重试 ...")
	runStageRetry(*sampleDir, *dbDir, params, logger)

	// 6) 输出自适应决策记录。
	fmt.Println("[6/6] 自适应算法决策 ...")
	for _, ms := range specs {
		st, err := store.Open(filepath.Join(*dbDir, ms.MatchID+"_live.db"))
		must(err)
		d, err := st.LoadDecision(ms.MatchID)
		must(err)
		_ = st.Close()
		fmt.Printf("  %-8s algo=%-20s window=%d basis=%s\n", ms.MatchID, d.Algorithm, d.WindowSize, d.Basis)
		logger.Printf("decision match=%s algo=%s window=%d basis=%s", ms.MatchID, d.Algorithm, d.WindowSize, d.Basis)
	}
	fmt.Println("\n演示完成。日志:", *logPath)
}

func runThreePaths(ms gen.MatchSpec, sampleDir, dbDir string, params model.RunParams, logger *log.Logger) {
	replay, raw := loadReplay(filepath.Join(sampleDir, "replays", ms.MatchID+".json"))
	truth := loadTruth(filepath.Join(sampleDir, "truth", ms.MatchID+".truth.json"))
	truthHash := engine.HashTruth(truth)

	paths := []struct {
		mode engine.RunMode
		file string
	}{
		{engine.ModeLive, "live"},
		{engine.ModeRecovery, "recovery"},
		{engine.ModeFull, "full"},
	}
	stores := map[string]*store.Store{}
	defer func() {
		for _, st := range stores {
			_ = st.Close()
		}
	}()

	for _, p := range paths {
		dbPath := filepath.Join(dbDir, ms.MatchID+"_"+p.file+".db")
		removeIfExists(dbPath)
		switch p.mode {
		case engine.ModeRecovery:
			// 用 live 的磁盘副本模拟“进程重启后”的检查点，再走恢复续跑。
			must(store.SnapshotFile(
				filepath.Join(dbDir, ms.MatchID+"_live.db"), dbPath))
			st, err := store.Open(dbPath)
			must(err)
			stores[p.file] = st
			runner := engine.NewRunner(raw, truthHash, params)
			res, err := runner.RunRecovery(replay, st)
			must(err)
			committed, total := progressOf(st, ms.MatchID)
			fmt.Printf("  %-8s %-9s committed=%d/%d recovered=%v invalidated=%v\n",
				ms.MatchID, p.file, committed, total, res.Recovered, res.Invalidated)
			logger.Printf("path match=%s mode=%s committed=%d/%d recovered=%v invalidated=%v",
				ms.MatchID, p.file, committed, total, res.Recovered, res.Invalidated)
		case engine.ModeFull:
			st, err := store.Open(dbPath)
			must(err)
			stores[p.file] = st
			runner := engine.NewRunner(raw, truthHash, params)
			res, err := runner.RunFullReplay(replay, st)
			must(err)
			committed, total := progressOf(st, ms.MatchID)
			fmt.Printf("  %-8s %-9s committed=%d/%d recovered=%v invalidated=%v\n",
				ms.MatchID, p.file, committed, total, res.Recovered, res.Invalidated)
			logger.Printf("path match=%s mode=%s committed=%d/%d recovered=%v invalidated=%v",
				ms.MatchID, p.file, committed, total, res.Recovered, res.Invalidated)
		default:
			st, err := store.Open(dbPath)
			must(err)
			stores[p.file] = st
			runner := engine.NewRunner(raw, truthHash, params)
			res, err := runner.RunLive(replay, st)
			must(err)
			committed, total := progressOf(st, ms.MatchID)
			fmt.Printf("  %-8s %-9s committed=%d/%d recovered=%v invalidated=%v\n",
				ms.MatchID, p.file, committed, total, res.Recovered, res.Invalidated)
			logger.Printf("path match=%s mode=%s committed=%d/%d recovered=%v invalidated=%v",
				ms.MatchID, p.file, committed, total, res.Recovered, res.Invalidated)
		}
	}

	// 三路径两两比对，全部只读持久化快照。
	must(engine.CompareStores(ms.MatchID, stores["live"], stores["recovery"]))
	must(engine.CompareStores(ms.MatchID, stores["live"], stores["full"]))
	fmt.Printf("  %-8s three-path compare: IDENTICAL\n", ms.MatchID)
	logger.Printf("compare match=%s live=recovery=full identical", ms.MatchID)
}

func runInjections(sampleDir, dbDir string, seed int64, params model.RunParams, logger *log.Logger) {
	base, _ := loadReplay(filepath.Join(sampleDir, "replays", "m_small.json"))
	turn := 21
	injectSpecs := []gen.InjectSpec{
		{MatchID: "f_decode", Kind: "decode"},
		{MatchID: "f_order", Kind: "order", Turn: turn, Window: (turn - 1) / params.WindowSize},
		{MatchID: "f_conflict", Kind: "conflict", Turn: turn, Window: (turn - 1) / params.WindowSize},
		{MatchID: "f_stage", Kind: "stage", Turn: turn, Window: (turn - 1) / params.WindowSize},
	}
	must(gen.BuildInjectSamples(base, sampleDir, injectSpecs))

	for _, sp := range injectSpecs {
		path := filepath.Join(sampleDir, "faults", sp.MatchID+".json")
		raw, err := os.ReadFile(path)
		must(err)
		replay, loadErr := engine.LoadReplay(raw)

		switch sp.Kind {
		case "decode", "order":
			code := errorCode(loadErr)
			fmt.Printf("  %-11s window=%d -> %s  detail=%v\n", sp.MatchID, sp.Window, code, loadErr)
			logger.Printf("injection %s window=%d code=%s err=%v", sp.MatchID, sp.Window, code, loadErr)
			continue
		default:
			must(loadErr)
		}

		dbPath := filepath.Join(dbDir, sp.MatchID+".db")
		removeIfExists(dbPath)
		st, err := store.Open(dbPath)
		must(err)
		runner := engine.NewRunner(raw, "fault-truth-placeholder", params)
		_, runErr := runner.RunLive(replay, st)
		_ = st.Close()
		code := errorCode(runErr)
		fmt.Printf("  %-11s window=%d turn=%d -> %s  detail=%v\n",
			sp.MatchID, sp.Window, sp.Turn, code, runErr)
		logger.Printf("injection %s window=%d turn=%d code=%s err=%v",
			sp.MatchID, sp.Window, sp.Turn, code, runErr)
	}
}

func runStageRetry(sampleDir, dbDir string, params model.RunParams, logger *log.Logger) {
	raw, err := os.ReadFile(filepath.Join(sampleDir, "faults", "f_stage.json"))
	must(err)
	replay, err := engine.LoadReplay(raw)
	must(err)

	dbPath := filepath.Join(dbDir, "f_stage_retry.db")
	removeIfExists(dbPath)
	st, err := store.Open(dbPath)
	must(err)
	defer st.Close()

	runner := engine.NewRunner(raw, "fault-truth-placeholder", params)
	_, firstErr := runner.RunLive(replay, st)
	fmt.Printf("  first run  -> %s (其他窗口不受影响)\n", errorCode(firstErr))
	logger.Printf("stage retry first err=%v", firstErr)

	// 修复回放：清除注入标记后重试失败窗口。
	for i := range replay.Turns {
		for j := range replay.Turns[i].Intents {
			replay.Turns[i].Intents[j].InjectStageFail = false
		}
	}
	fixedRaw, err := json.Marshal(replay)
	must(err)
	failWindow := (21 - 1) / params.WindowSize
	repairRunner := engine.NewRunner(fixedRaw, "fault-truth-placeholder", params)
	report, err := repairRunner.RetryFailedWindow(replay, st, failWindow)
	must(err)
	fmt.Printf("  retry window=%d -> committed turns=%d\n", failWindow, len(report.Snapshots))
	logger.Printf("stage retry window=%d committed snapshots=%d", failWindow, len(report.Snapshots))
}

func progressOf(st *store.Store, matchID string) (int, int) {
	windows, err := st.LoadWindows(matchID)
	must(err)
	committed := 0
	for _, w := range windows {
		if w.Status == string(model.WindowCommitted) {
			committed++
		}
	}
	return committed, len(windows)
}

func loadReplay(path string) (*model.Replay, []byte) {
	raw, err := os.ReadFile(path)
	must(err)
	r, err := engine.LoadReplay(raw)
	must(err)
	return r, raw
}

func loadTruth(path string) *model.MatchTruth {
	raw, err := os.ReadFile(path)
	must(err)
	var t model.MatchTruth
	must(json.Unmarshal(raw, &t))
	return &t
}

func errorCode(err error) string {
	if err == nil {
		return "OK"
	}
	if ke, ok := model.AsKernelError(err); ok {
		return ke.Code
	}
	return err.Error()
}

func removeIfExists(path string) {
	if _, err := os.Stat(path); err == nil {
		_ = os.Remove(path)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
}

func mustLogger(path string) (*log.Logger, func()) {
	must(os.MkdirAll(filepath.Dir(path), 0o755))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	must(err)
	return log.New(f, "", log.LstdFlags), func() { _ = f.Close() }
}

func must(err error) {
	if err != nil {
		log.Fatalf("fatal: %v", err)
	}
}
