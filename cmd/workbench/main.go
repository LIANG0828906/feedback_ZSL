package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"maskqc"
)

func main() {
	workdir := flag.String("workdir", "artifacts/workbench", "diagnostic artifact directory")
	addr := flag.String("addr", "127.0.0.1:8765", "local HTTP diagnostic address")
	serve := flag.Bool("serve", false, "start the local HTTP diagnostic workbench")
	selfTest := flag.Bool("selftest", false, "run all deterministic checks and exit")
	flag.Parse()

	report, err := maskqc.BuildWorkbenchReport(*workdir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workbench build failed: %v\n", err)
		os.Exit(1)
	}
	if err := maskqc.WritePathReports(*workdir, report); err != nil {
		fmt.Fprintf(os.Stderr, "write reports failed: %v\n", err)
		os.Exit(1)
	}
	if err := maskqc.WriteGeneratedSchemaDocs(*workdir); err != nil {
		fmt.Fprintf(os.Stderr, "write schema docs failed: %v\n", err)
		os.Exit(1)
	}
	printConsole(report, *workdir)

	if *selfTest {
		if !report.AcceptanceOK {
			os.Exit(1)
		}
		return
	}

	shouldServe := *serve
	if !shouldServe && terminalInteractive() {
		fmt.Print("\n启动浏览器诊断工作台? [Y/n] ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		if line == "\n" || line == "y\n" || line == "Y\n" || line == "yes\n" {
			shouldServe = true
		}
	}
	if !shouldServe {
		fmt.Printf("\nJSON: %s/diagnostics.json\n复核: go run ./cmd/review -a %s/path-live.json -b %s/path-recovery.json\n界面: go run ./cmd/workbench --serve --addr %s\n", *workdir, *workdir, *workdir, *addr)
		if !report.AcceptanceOK {
			os.Exit(1)
		}
		return
	}

	srv := &http.Server{Addr: *addr, Handler: (&dashboardServer{report: report}).routes(), ReadHeaderTimeout: 5 * time.Second}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		fmt.Printf("\n诊断界面: http://%s\n独立复核: go run ./cmd/review -a %s/path-live.json -b %s/path-full.json\n按 Ctrl+C 停止。\n", *addr, *workdir, *workdir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			os.Exit(1)
		}
	}()
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func terminalInteractive() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func printConsole(r *maskqc.WorkbenchReport, workdir string) {
	fmt.Println("掩码质检诊断工作台 / Mask QC Workbench")
	fmt.Printf("seed=%d selected=%s samples=%d workdir=%s\n", r.Seed, r.SelectedSampleID, len(r.Samples), workdir)
	fmt.Println("\n[样本]")
	for _, s := range r.Samples {
		fmt.Printf("- %s %dx%d chunks=%d strokes=%d groups=%d truth_domains=%d schema=v1/v2/v3:%v/%v/%v v1_lossy=%d\n",
			s.ID, s.Width, s.Height, s.Chunks, s.Strokes, s.Groups, s.Domains, s.V1Loaded, s.V2Loaded, s.V3Loaded, s.V1Lossy)
	}
	fmt.Println("\n[检查]")
	for _, c := range r.Checks {
		status := "PASS"
		if !c.OK {
			status = "FAIL"
		}
		loc := ""
		if c.ChunkID >= 0 || c.StrokeID != "" {
			loc = fmt.Sprintf(" chunk=%d stroke=%s", c.ChunkID, c.StrokeID)
		}
		fmt.Printf("- %s %s %s%s :: %s\n", status, c.Name, c.Code, loc, c.Detail)
	}
	fmt.Println("\n[三路径]")
	tp := r.ThreePath
	fmt.Printf("- live/recovery/full consistency=%v; mismatch=%+v/%+v\n", tp.OK, tp.LiveVsRec, tp.LiveVsFull)
	if tp.Live != nil && tp.Live.Report != nil {
		fmt.Printf("- live domains=%d topology_error=%v\n", len(tp.Live.Report.Domains), tp.Live.TopologyError != nil)
	}
	fmt.Println("\n[编辑失效]")
	for _, s := range r.EditSteps {
		fmt.Printf("- #%d %s invalid=%v reusable=%v match=%v items=%d\n", s.Index, s.Description, s.Plan.InvalidChunks, s.Plan.ReusableChunks, s.ReuseMatchesPlan, len(s.Plan.Items))
	}
	fmt.Printf("\n[验收] acceptance_ok=%v\n", r.AcceptanceOK)
}
