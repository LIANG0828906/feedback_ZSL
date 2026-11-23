// 诊断工作台服务器入口：go run ./cmd/server -dir ./data/sample
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"occlbench/internal/culling"
	"occlbench/internal/httpapi"
	"occlbench/internal/persist"
	"occlbench/internal/scene"
	"occlbench/internal/streaming"
	"occlbench/internal/workbench"
)

func main() {
	dir := flag.String("dir", "data/sample", "场景工程目录")
	dbDir := flag.String("db", "data/checkpoint", "SQLite 检查点目录")
	addr := flag.String("addr", "127.0.0.1:8080", "本地回环监听地址")
	windowSize := flag.Int("window", 3, "每个窗口覆盖的采样时刻数")
	slots := flag.Int("slots", 3, "并发加载槽位")
	memory := flag.Float64("memory", 20, "内存预算")
	grid := flag.Int("grid", 64, "遮挡栅格分辨率")
	flag.Parse()

	world, err := scene.Load(*dir)
	if err != nil {
		log.Fatalf("加载场景工程失败: %v", err)
	}
	cfg := streaming.Config{
		WindowSize: *windowSize,
		Budget:     streaming.Budget{Slots: *slots, Memory: *memory},
		CullParams: culling.Params{GridResolution: *grid, SampleStride: 1, Quant: culling.DefaultQuantPrecision()},
	}

	store, err := persist.Open(*dbDir)
	if err != nil {
		log.Fatalf("打开检查点失败: %v", err)
	}
	defer store.Close()

	app, err := workbench.NewApp(world, cfg, store, *dbDir)
	if err != nil {
		log.Fatalf("初始化工作台失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	paths, diverge, _, verr := app.Verify(ctx)
	if verr != nil {
		log.Printf("三路径比对发现问题: %v", verr)
	} else {
		log.Printf("三路径一致性校验通过，窗口数=%d", len(paths[workbench.PathRealtime]))
	}
	if diverge != nil {
		log.Printf("首个差异: 窗口=%d 分块=%d 字段=%s", diverge.WindowIndex, diverge.ChunkID, diverge.Field)
	}

	srv := httpapi.New(app)
	log.Printf("诊断工作台已启动: http://%s/", *addr)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
	fmt.Println("server stopped")
}
