package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"parkrelay/internal/relay"
)

func main() {
	cfg := relay.DefaultConfig()
	flag.StringVar(&cfg.BindIP, "bind-ip", cfg.BindIP, "loopback bind IP")
	flag.StringVar(&cfg.ControlUDP, "control-udp", cfg.ControlUDP, "UDP control listen address")
	flag.StringVar(&cfg.ControlTCP, "control-tcp", cfg.ControlTCP, "TCP control listen address")
	flag.StringVar(&cfg.HTTP, "http", cfg.HTTP, "HTTP status/metrics listen address")
	flag.StringVar(&cfg.DBPath, "db", cfg.DBPath, "SQLite database path")
	flag.IntVar(&cfg.PortMin, "port-min", cfg.PortMin, "minimum mapping port")
	flag.IntVar(&cfg.PortMax, "port-max", cfg.PortMax, "maximum mapping port")
	flag.DurationVar(&cfg.LeaseWindow, "lease-window", cfg.LeaseWindow, "gateway keepalive window")
	flag.DurationVar(&cfg.RecoveryCooldown, "cooldown", cfg.RecoveryCooldown, "released-port cooldown")
	flag.DurationVar(&cfg.InFlightDrain, "drain", cfg.InFlightDrain, "in-flight drain period")
	flag.DurationVar(&cfg.ProbeTimeout, "probe-timeout", cfg.ProbeTimeout, "NAT probe timeout")
	flag.DurationVar(&cfg.ProbeInterval, "probe-interval", cfg.ProbeInterval, "periodic probe interval")
	flag.StringVar(&cfg.ForwardUDP, "forward-udp", cfg.ForwardUDP, "optional UDP sink for verified mapping payloads")
	flag.StringVar(&cfg.ForwardTCP, "forward-tcp", cfg.ForwardTCP, "optional TCP sink for verified mapping payloads")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	r, err := relay.Start(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("relay started control_udp=%s control_tcp=%s http=%s db=%s\n",
		cfg.ControlUDP, cfg.ControlTCP, cfg.HTTP, cfg.DBPath)
	<-ctx.Done()
	start := time.Now()
	if err := r.Stop(); err != nil {
		log.Printf("stop: %v", err)
	}
	fmt.Printf("relay stopped in %s\n", time.Since(start).Round(time.Millisecond))
}
