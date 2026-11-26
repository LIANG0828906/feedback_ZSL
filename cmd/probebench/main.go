package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	lp "lightprobe"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "verify", "demo":
		verifyCmd(os.Args[2:])
	case "serve":
		serveCmd(os.Args[2:])
	case "crash":
		crashCmd(os.Args[2:])
	case "resume":
		resumeCmd(os.Args[2:])
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Println(`probebench commands:
  verify [--dir .probebench/acceptance]  run deterministic acceptance checks
  serve  [--addr 127.0.0.1:8088] [--dir .probebench/workbench]
  crash  [--dir .probebench/manual]      commit room-a then SIGKILL this process
  resume [--dir .probebench/manual]      resume from checkpoint and compare full rebuild`)
}

func verifyCmd(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fs.String("dir", ".probebench/acceptance", "checkpoint/run directory")
	_ = fs.Parse(args)

	report, err := lp.RunAcceptance(*dir)
	if err != nil {
		fatal("acceptance failed", err)
	}
	printAcceptance(report)
	if !report.AllConsistent {
		if report.FullDiff != nil {
			fatalCode(report.FullDiff)
		}
		fatalCode(report.ResumeDiff)
	}
	if len(report.BoundViolations) != 0 {
		fmt.Printf("[FAIL] error upper-bound violations: %v\n", report.BoundViolations)
		os.Exit(1)
	}
	fmt.Println("[PASS] schema, reconstruction, interpolation, bounds, graph and recovery acceptance")
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8088", "listen address")
	dir := fs.String("dir", ".probebench/workbench", "checkpoint/run directory")
	_ = fs.Parse(args)
	fmt.Printf("LightProbe workbench: http://%s\n", *addr)
	if err := lp.ServeWorkbench(*addr, *dir); err != nil {
		fatal("workbench stopped", err)
	}
}

func crashCmd(args []string) {
	fs := flag.NewFlagSet("crash", flag.ExitOnError)
	dir := fs.String("dir", ".probebench/manual", "checkpoint directory")
	_ = fs.Parse(args)
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(*dir, "resume.lpjson")
	scene, err := lp.GenerateScene(lp.DefaultSeed, lp.DefaultParams())
	if err != nil {
		fatal("generate", err)
	}
	pr, err := lp.RunPipeline(scene, lp.RunOptions{
		Label: "before-sigkill", CheckpointPath: path, StopAfterCommits: 1,
		Pool: lp.PoolConfig{Workers: 1, BufferLimit: 2, QueueBudget: 1},
	})
	if err != nil {
		fatal("crash precondition", err)
	}
	fmt.Printf("checkpoint committed order=%v path=%s\n", pr.CommitOrder, path)
	fmt.Println("simulating hard termination with SIGKILL; no exit report is promised")
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
}

func resumeCmd(args []string) {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	dir := fs.String("dir", ".probebench/manual", "checkpoint directory")
	_ = fs.Parse(args)
	path := filepath.Join(*dir, "resume.lpjson")
	scene, err := lp.GenerateScene(lp.DefaultSeed, lp.DefaultParams())
	if err != nil {
		fatal("generate", err)
	}
	resumed, err := lp.RunPipeline(scene, lp.RunOptions{
		Label: "resume", CheckpointPath: path,
		Pool: lp.PoolConfig{Workers: 4, BufferLimit: 8, QueueBudget: 16},
	})
	if err != nil {
		fatal("resume", err)
	}
	fullPath := filepath.Join(*dir, "full.lpjson")
	full, err := lp.RunPipeline(scene, lp.RunOptions{
		Label: "full", CheckpointPath: fullPath,
		Pool: lp.PoolConfig{Workers: 8, BufferLimit: 16, QueueBudget: 64},
	})
	if err != nil {
		fatal("full rebuild", err)
	}
	if diff := lp.DiffPipelines(full, resumed); diff != nil {
		fatalCode(diff)
	}
	maxActual, maxUpper, violations := lp.CheckErrorBounds(full)
	if len(violations) != 0 {
		fmt.Printf("[FAIL] bound violations: %v\n", violations)
		os.Exit(1)
	}
	fmt.Printf("[PASS] resumed order=%v restored=%v\n", resumed.CommitOrder, resumed.RestoredRooms)
	fmt.Printf("[PASS] resume/full grid, pixels, uncertainty and commit order match; actual=%.3e upper=%.3e\n", maxActual, maxUpper)
}

func printAcceptance(a *lp.AcceptanceReport) {
	s := a.Scene
	fmt.Println("== LightProbe deterministic acceptance ==")
	fmt.Printf("seed=%d rooms=%d probes=%d directives=%d queries=%d pixels=%d\n",
		s.Seed, len(s.Rooms), len(s.Probes), len(s.Directives), len(s.Queries), len(s.Pixels))
	fmt.Printf("versions=%s param=%s fixed_scale=%d\n",
		lp.AlgorithmVersion, s.Params.ParamVersion, s.Params.FixedScale)
	fmt.Printf("commit_order=%v\n", a.Full.CommitOrder)
	fmt.Printf("realtime_pool workers=%d peak=%d waits=%d wait_total=%.3fms\n",
		a.Realtime.Queue.Workers, a.Realtime.Queue.PeakActiveWorkers,
		a.Realtime.Queue.WaitCount, a.Realtime.Queue.TotalWaitMS)
	fmt.Printf("resume_restored=%v stale=%d order=%v\n",
		a.Resume.RestoredRooms, len(a.Resume.StaleRooms), a.Resume.CommitOrder)
	fmt.Printf("error_bounds actual_max=%.6e accumulated_upper_max=%.6e violations=%d\n",
		a.BoundMaxActual, a.BoundMaxUpper, len(a.BoundViolations))
	if a.FullDiff != nil {
		fmt.Printf("[%s] %s object=%s field=%s\n", a.FullDiff.Code, a.FullDiff.Message, a.FullDiff.ObjectID, a.FullDiff.Field)
	}
	if a.ResumeDiff != nil {
		fmt.Printf("[%s] %s object=%s field=%s\n", a.ResumeDiff.Code, a.ResumeDiff.Message, a.ResumeDiff.ObjectID, a.ResumeDiff.Field)
	}
}

func fatal(prefix string, err error) {
	if de, ok := lp.AsDomainError(err); ok {
		fmt.Fprintf(os.Stderr, "%s: [%s] object=%s field=%s: %s\n", prefix, de.Code, de.ObjectID, de.Field, de.Message)
	} else {
		fmt.Fprintf(os.Stderr, "%s: %v\n", prefix, err)
	}
	os.Exit(1)
}

func fatalCode(de *lp.Error) {
	fmt.Fprintf(os.Stderr, "[%s] object=%s field=%s: %s\n", de.Code, de.ObjectID, de.Field, de.Message)
	os.Exit(1)
}
