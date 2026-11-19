package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"dnssim/internal/sim"
)

const version = "1.0.0"

func main() {
	if os.Getenv("GOMAXPROCS") == "" && runtime.NumCPU() > 2 {
		runtime.GOMAXPROCS(2)
	}
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "validate":
		err = cmdValidate(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "trace":
		err = cmdTrace(os.Args[2:])
	case "sample":
		err = cmdSample(os.Args[2:])
	case "version", "--version", "-version":
		fmt.Printf("dnssim %s\n", version)
	case "help", "--help", "-h":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		var be *sim.BusinessError
		if errors.As(err, &be) {
			writeJSON(os.Stdout, map[string]any{"ok": false, "error": be})
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `dnssim - deterministic DNS dependency-chain simulator

Usage:
  dnssim validate -config topology.json [-check-ports]
  dnssim serve    -config topology.json [-http 127.0.0.1:8080]
  dnssim trace    -config topology.json -site shanghai -domain www.example.test [-type A] [-pretty]
  dnssim sample   [-out sample-topology.json]

All simulated DNS listeners are loopback-only. Port 0 may be used in a
topology to let the operating system allocate a free high port.`)
}

func cmdValidate(args []string) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	configPath := flags.String("config", "", "topology JSON path")
	checkPorts := flags.Bool("check-ports", false, "bind and release every configured DNS port")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return &sim.BusinessError{Code: sim.CodeInvalidRequest, Message: "-config is required"}
	}
	cfg, err := sim.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	output := map[string]any{
		"ok": true, "schema_version": cfg.Version,
		"resolvers": len(cfg.Resolvers), "zones": len(cfg.Zones), "sites": len(cfg.Sites),
	}
	if *checkPorts {
		engine, err := sim.StartEngine(cfg)
		if err != nil {
			return err
		}
		output["addresses"] = engine.Addresses()
		engine.Close()
		output["ports_released"] = true
	}
	writeJSON(os.Stdout, output)
	return nil
}

func cmdServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := flags.String("config", "", "topology JSON path")
	httpAddr := flags.String("http", "127.0.0.1:8080", "HTTP API listen address")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return &sim.BusinessError{Code: sim.CodeInvalidRequest, Message: "-config is required"}
	}
	cfg, err := sim.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *httpAddr)
	if err != nil {
		return &sim.BusinessError{Code: sim.CodeBindFailed, Message: fmt.Sprintf("bind HTTP API: %v", err)}
	}
	service, err := sim.NewService(cfg)
	if err != nil {
		_ = listener.Close()
		return err
	}
	defer service.Close()

	shutdown := make(chan struct{}, 1)
	var apiShutdown sync.Once
	api := sim.NewAPI(service, func() {
		apiShutdown.Do(func() { shutdown <- struct{}{} })
	})
	server := &http.Server{
		Handler:           api.Handler(),
		ReadHeaderTimeout: 3 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(listener)
	}()
	writeJSON(os.Stdout, map[string]any{
		"status": "ready",
		"http":   listener.Addr().String(),
		"dns":    service.Engine().Addresses(),
	})

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-signalCtx.Done():
	case <-shutdown:
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
	return nil
}

func cmdTrace(args []string) error {
	flags := flag.NewFlagSet("trace", flag.ContinueOnError)
	configPath := flags.String("config", "", "topology JSON path")
	site := flags.String("site", "", "site id")
	domain := flags.String("domain", "", "domain to trace")
	recordType := flags.String("type", "A", "record type")
	pretty := flags.Bool("pretty", false, "indent JSON output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || *site == "" || *domain == "" {
		return &sim.BusinessError{Code: sim.CodeInvalidRequest, Message: "-config, -site, and -domain are required"}
	}
	cfg, err := sim.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	service, err := sim.NewService(cfg)
	if err != nil {
		return err
	}
	defer service.Close()
	result, err := service.Trace(context.Background(), sim.TraceRequest{
		Site: *site, Domain: *domain, Type: *recordType,
	})
	if err != nil {
		return err
	}
	writeResult(os.Stdout, result, *pretty)
	return nil
}

func cmdSample(args []string) error {
	flags := flag.NewFlagSet("sample", flag.ContinueOnError)
	out := flags.String("out", "", "write sample topology to this path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg := sim.SampleConfig()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if *out == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return &sim.BusinessError{Code: sim.CodeInternal, Message: "cannot write sample topology"}
	}
	writeJSON(os.Stdout, map[string]any{"ok": true, "written": true})
	return nil
}

func writeResult(w io.Writer, value any, pretty bool) {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if pretty {
		encoder.SetIndent("", "  ")
	}
	_ = encoder.Encode(value)
}

func writeJSON(w io.Writer, value any) {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}
