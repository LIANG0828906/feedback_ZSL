package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"enginelab.local/internal/verify"
	"enginelab.local/internal/workbench"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "verify":
			fs := flag.NewFlagSet("verify", flag.ExitOnError)
			out := fs.String("out", "artifacts", "artifact output directory")
			_ = fs.Parse(os.Args[2:])
			report, err := verify.Run(context.Background(), *out)
			if err != nil {
				fmt.Fprintf(os.Stderr, "verify failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("verification ok=%v checks=%d injections=%v artifacts=%s\n", report.OK, len(report.Checks), report.InjectionCodes, *out)
			if !report.OK {
				for _, c := range report.Checks {
					if !c.OK {
						fmt.Printf("FAIL %s: %s\n", c.Name, c.Detail)
					}
				}
				os.Exit(2)
			}
			return
		case "serve", "":
			os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
		case "-h", "--help", "help":
			usage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
			usage()
			os.Exit(2)
		}
	}

	addr := flag.String("addr", "127.0.0.1:8088", "HTTP listen address")
	dbPath := flag.String("db", "artifacts/workbench.sqlite", "SQLite checkpoint path")
	seed := flag.Int64("seed", 20261127, "deterministic generator seed")
	flag.Parse()

	svc, err := workbench.NewService(workbench.Config{Seed: *seed, DBPath: *dbPath, ResetDB: true, Budget: workbench.DefaultBudget()})
	if err != nil {
		fmt.Fprintf(os.Stderr, "init workbench failed: %v\n", err)
		os.Exit(1)
	}
	defer svc.Close()

	server := &http.Server{Addr: *addr, Handler: workbench.NewHandler(svc), ReadHeaderTimeout: 5 * time.Second}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		fmt.Printf("enginelab workbench: http://%s (sqlite=%s seed=%d)\n", *addr, svc.DBPath(), *seed)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			os.Exit(1)
		}
	}()

	received := <-sigCh
	fmt.Printf("\nreceived catchable signal %s; draining HTTP and writing clean-shutdown marker\n", received)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = svc.MarkGracefulShutdown(shutdownCtx, received.String())
	if err := server.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(os.Stderr, "graceful shutdown failed: %v\n", err)
	}
}

func usage() {
	fmt.Println(`enginelab commands:
  serve [-addr 127.0.0.1:8088] [-db artifacts/workbench.sqlite] [-seed 20261127]
  verify [-out artifacts]

SIGINT/SIGTERM/SIGHUP are handled as graceful shutdown.
SIGKILL is not catchable; SQLite global commit pointer recovery is covered by tests.`)
}
