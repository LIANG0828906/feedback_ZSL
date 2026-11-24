package main

import (
	"flag"

	"storyboardstudio/internal/app"
)

func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "HTTP listen address")
	samples := fs.String("samples", "samples", "sample directory to load")
	db := fs.String("db", "data/storyboard.db", "SQLite database path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return app.Serve(*addr, *samples, *db)
}
