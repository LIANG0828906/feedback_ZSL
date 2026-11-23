package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"loudnessworkbench/apperr"
	"loudnessworkbench/workbench"
)

func commandServe(args []string) error {
	flags := newFlagSet("serve")
	dataDir := flags.String("data", "demo", "generated data directory")
	addr := flags.String("addr", "127.0.0.1:8080", "local listen address")
	version := flags.String("version", "v3", "default source project version")
	if err := flags.Parse(args); err != nil {
		return err
	}
	server, err := workbench.NewServer(*dataDir, *version)
	if err != nil {
		return err
	}
	httpServer := &http.Server{Addr: *addr, Handler: server.Handler()}
	fmt.Printf("loudness workbench listening on http://%s (data: %s)\n", *addr, *dataDir)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return apperr.Wrap(apperr.CodeInternal, "workbench server failed", err)
	}
	_ = os.Stdout.Sync()
	return nil
}
