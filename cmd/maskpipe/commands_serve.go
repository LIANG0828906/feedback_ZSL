package main

import (
	"context"
	"flag"

	"maskpipe/internal/web"
)

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	common := bindCommon(fs)
	addr := fs.String("addr", "127.0.0.1:8088", "HTTP diagnostic interface address")
	dbPath := fs.String("db", "reports/diagnostic.db", "SQLite checkpoint path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	loaded, err := loadScene(common)
	if err != nil {
		return err
	}
	return web.Serve(ctx, web.Config{
		Addr: *addr, SampleDir: common.sampleDir, DBPath: *dbPath,
		SceneID: common.sceneID, Loaded: loaded,
	})
}
