package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	ballisticgrid "ballisticgrid"
	"ballisticgrid/internal/diag"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "verify":
		verifyCmd(os.Args[2:])
	case "serve":
		serveCmd(os.Args[2:])
	case "crash-demo":
		crashCmd(os.Args[2:])
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: ballisticgrid <verify|serve|crash-demo> [flags]")
}

func verifyCmd(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fs.String("dir", "artifacts/run", "deterministic artifact directory")
	_ = fs.Parse(args)

	report, err := ballisticgrid.RunVerification(*dir)
	if err != nil {
		var ge *ballisticgrid.Error
		if errors.As(err, &ge) {
			fmt.Fprintf(os.Stderr, "verification failed: %s code=%s field=%s tile=%v\n", ge.Message, ge.Code, ge.Field, ge.Tile)
		} else {
			fmt.Fprintf(os.Stderr, "verification failed: %v\n", err)
		}
		os.Exit(1)
	}

	missing := []string{}
	expected := map[string]ballisticgrid.ErrorCode{
		"decode": ballisticgrid.CodeDecode, "duplicate": ballisticgrid.CodeDecode,
		"order": ballisticgrid.CodeOrder, "norm": ballisticgrid.CodeNorm,
		"block": ballisticgrid.CodeBlock, "bounce": ballisticgrid.CodeBounce,
	}
	for name, code := range expected {
		ge := report.Injections[name]
		if ge == nil || ge.Code != code {
			missing = append(missing, name+":"+string(code))
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "injection verification missing codes: %v\n", missing)
		os.Exit(1)
	}

	fmt.Printf("OK seed=%d tiles=%d recovered=%d reused=%d recomputed=%d poolPeak=%d waits=%d dir=%s\n",
		report.Seed, report.TileCount, report.RestartRecovered, report.IncrementalReused,
		report.IncrementalRecomputed, report.Pool.PeakParallel, len(report.Pool.Waits), *dir)
	fmt.Printf("summary=%s/summary.json db=%s/tiles.db\n", *dir, *dir)
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8088", "listen address")
	dir := fs.String("dir", "artifacts/run", "artifact directory containing summary.json")
	_ = fs.Parse(args)

	level := ballisticgrid.Generate(ballisticgrid.DefaultSeed)
	cam := ballisticgrid.DefaultCamera()
	versions := ballisticgrid.DefaultSemanticVersions()
	plan, err := ballisticgrid.EditObstacle(level, cam, &versions, "OB01", "restitution", func(o *ballisticgrid.Obstacle) {
		o.Restitution = 9000
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	scene, err := ballisticgrid.ProjectScene(level, cam)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dashboard := &diag.Dashboard{Level: level, Camera: cam, Scene: scene, Plan: plan}
	if data, err := os.ReadFile(*dir + "/summary.json"); err == nil {
		var report ballisticgrid.VerificationReport
		if json.Unmarshal(data, &report) == nil {
			dashboard.Report = &report
		}
	}
	server := &http.Server{Addr: *addr, Handler: dashboard.Handler(), ReadHeaderTimeout: 5 * time.Second}
	fmt.Printf("diagnostics listening on http://%s\n", *addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func crashCmd(args []string) {
	fs := flag.NewFlagSet("crash-demo", flag.ExitOnError)
	db := fs.String("db", "artifacts/run/crash-demo.db", "snapshot database path")
	commits := fs.Int("commits", 3, "number of tiles to commit before sleeping")
	_ = fs.Parse(args)

	level := ballisticgrid.Generate(ballisticgrid.DefaultSeed)
	cam := ballisticgrid.DefaultCamera()
	scene, err := ballisticgrid.ProjectScene(level, cam)
	if err != nil {
		panic(err)
	}
	store, err := ballisticgrid.OpenSnapshot(*db)
	if err != nil {
		panic(err)
	}
	versions := ballisticgrid.DefaultSemanticVersions()
	for i := 0; i < *commits && i < len(scene.Tiles); i++ {
		if err := store.CommitTile(scene.Tiles[i], cam, versions, scene.HitStats); err != nil {
			panic(err)
		}
		fmt.Printf("committed %s seq=%d\n", scene.Tiles[i].ID, scene.Tiles[i].Seq)
	}
	_ = store.Close()
	fmt.Println("committed boundary durable; send SIGKILL now; sleeping 10m")
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Minute)
	defer stop()
	<-ctx.Done()
}
