package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"feedback_zsl/internal/config"
	"feedback_zsl/internal/entry"
	"feedback_zsl/internal/h2server"
	"feedback_zsl/internal/proxy"
	"feedback_zsl/internal/tlscert"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

const exitConfigInvalid = 78

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	code := run()
	os.Exit(code)
}

func run() int {
	var (
		configPath = flag.String("config", "configs/gateway.json", "path to the gateway JSON configuration")
		check      = flag.Bool("check", false, "validate configuration and exit without listening")
	)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		writeStartupError(err)
		return exitConfigInvalid
	}
	if *check {
		out, _ := json.Marshal(map[string]any{
			"status":   "valid",
			"revision": cfg.Revision,
			"path":     *configPath,
		})
		fmt.Println(string(out))
		return 0
	}

	p, err := proxy.NewWithConfig(cfg, log.Default())
	if err != nil {
		writeStartupError(err)
		return exitConfigInvalid
	}
	cert, err := tlscert.LoadOrGenerate(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		writeStartupError(&config.ValidationError{Code: config.ErrorInvalidTLS, Message: err.Error()})
		return exitConfigInvalid
	}

	h2ln, err := net.Listen("tcp", cfg.ListenH2)
	if err != nil {
		writeStartupError(fmt.Errorf("listen HTTP/2 on %s: %w", cfg.ListenH2, err))
		return 1
	}
	udpAddr, err := net.ResolveUDPAddr("udp", cfg.ListenH3)
	if err != nil {
		_ = h2ln.Close()
		writeStartupError(fmt.Errorf("resolve HTTP/3 address: %w", err))
		return 1
	}
	h3pc, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		_ = h2ln.Close()
		writeStartupError(fmt.Errorf("listen HTTP/3 on %s: %w", cfg.ListenH3, err))
		return 1
	}
	adminLn, err := net.Listen("tcp", cfg.AdminListen)
	if err != nil {
		_ = h2ln.Close()
		_ = h3pc.Close()
		writeStartupError(fmt.Errorf("listen admin on %s: %w", cfg.AdminListen, err))
		return 1
	}
	p.SetBoundAddresses(h2ln.Addr().String(), h3pc.LocalAddr().String(), adminLn.Addr().String())

	h2TLS := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2"},
	}
	h3TLS := http3.ConfigureTLSConfig(&tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	})
	h2srv := &h2server.Server{
		Handler:        p,
		TLSConfig:      h2TLS,
		MaxHeaderBytes: cfg.Limits.MaxHeaderBytes,
		MaxBodyBytes:   cfg.Limits.MaxRequestBytes,
		IdleTimeout:    cfg.Timeouts.IdleConnection.Duration(),
		Logger:         log.Default(),
		OnProtocolError: func(streamID uint32, code, message string) {
			p.ProtocolError("h2", fmt.Sprintf("%d", streamID), code, message)
		},
	}
	h3srv := &http3.Server{
		Addr:           cfg.ListenH3,
		TLSConfig:      h3TLS,
		Handler:        &proxy.H3Handler{Next: p},
		MaxHeaderBytes: max(256<<10, cfg.Limits.MaxHeaderBytes*4),
		QUICConfig: &quic.Config{
			HandshakeIdleTimeout:  5 * time.Second,
			MaxIdleTimeout:        maxDuration(30*time.Second, cfg.Timeouts.IdleConnection.Duration()),
			MaxIncomingStreams:    256,
			MaxIncomingUniStreams: 16,
		},
		ConnContext: func(ctx context.Context, c quic.Connection) context.Context {
			id := fmt.Sprintf("%s", c.RemoteAddr())
			if tracing, ok := ctx.Value(quic.ConnectionTracingKey).(quic.ConnectionTracingID); ok {
				id = fmt.Sprintf("%d", tracing)
			}
			return entry.WithConnectionInfo(ctx, &entry.ConnectionInfo{
				ID:         "h3-" + id,
				RemoteAddr: c.RemoteAddr().String(),
			})
		},
	}
	adminSrv := &http.Server{
		Handler:           p.AdminHandler(),
		ReadHeaderTimeout: 3 * time.Second,
	}

	errCh := make(chan error, 3)
	go func() {
		if err := h2srv.Serve(h2ln); err != nil && !errors.Is(err, net.ErrClosed) {
			errCh <- fmt.Errorf("HTTP/2 server: %w", err)
		}
	}()
	go func() {
		if err := h3srv.Serve(h3pc); err != nil && !errors.Is(err, net.ErrClosed) {
			errCh <- fmt.Errorf("HTTP/3 server: %w", err)
		}
	}()
	go func() {
		if err := adminSrv.Serve(adminLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("admin server: %w", err)
		}
	}()
	p.Start()

	startup := map[string]any{
		"status":          "listening",
		"config_revision": cfg.Revision,
		"http2_tcp":       h2ln.Addr().String(),
		"http3_udp":       h3pc.LocalAddr().String(),
		"admin":           adminLn.Addr().String(),
	}
	_ = json.NewEncoder(os.Stdout).Encode(startup)

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, os.Interrupt)
	setupReloadSignals(sigCh)

	exitCode := 0
selectLoop:
	for {
		select {
		case sig := <-sigCh:
			if isReloadSignal(sig) {
				if err := p.Reload(); err != nil {
					log.Printf("configuration reload rejected: %v", err)
				}
				continue
			}
			break selectLoop
		case err := <-errCh:
			log.Printf("server error: %v", err)
			exitCode = 1
			break selectLoop
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Timeouts.Shutdown.Duration())
	defer cancel()
	_ = h3srv.Close()
	_ = h2ln.Close()
	_ = adminSrv.Shutdown(shutdownCtx)
	p.Shutdown(shutdownCtx)
	if exitCode == 0 {
		log.Printf("gateway stopped cleanly")
	}
	return exitCode
}

func writeStartupError(err error) {
	code := "STARTUP_FAILED"
	var ve *config.ValidationError
	if errors.As(err, &ve) {
		code = ve.Code
	}
	_ = json.NewEncoder(os.Stderr).Encode(map[string]string{
		"code":    code,
		"message": err.Error(),
	})
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
