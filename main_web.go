//go:build !wails_desktop

package main

import (
	"flag"
	"log"

	"storyboardstudio/internal/app"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "HTTP workbench address")
	samples := flag.String("samples", "samples", "sample directory")
	db := flag.String("db", "data/storyboard.db", "SQLite checkpoint database")
	flag.Parse()
	if err := app.Serve(*addr, *samples, *db); err != nil {
		log.Fatal(err)
	}
}
