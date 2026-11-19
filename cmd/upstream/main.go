package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"feedback_zsl/internal/fixture"
)

func main() {
	var cfg fixture.Config
	flag.StringVar(&cfg.H1Addr, "h1", "127.0.0.1:18081", "HTTP/1.1 listen address")
	flag.StringVar(&cfg.H2Addr, "h2", "127.0.0.1:18082", "TLS HTTP/2 listen address")
	flag.StringVar(&cfg.H2CAddr, "h2c", "127.0.0.1:18083", "h2c listen address")
	flag.StringVar(&cfg.CertFile, "cert", "", "TLS certificate file; generated in memory when empty")
	flag.StringVar(&cfg.KeyFile, "key", "", "TLS private key file; generated in memory when empty")
	flag.Parse()
	set, err := fixture.Start(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"status": "listening",
		"addrs":  set.Addrs,
	})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	set.Close()
}
