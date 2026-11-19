package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"edgegateway/internal/backend"
	"edgegateway/internal/config"
	"edgegateway/internal/gateway"
	"edgegateway/internal/metrics"
	"edgegateway/internal/store"
)

func main() {
	var configPath string
	var initOnly bool
	flag.StringVar(&configPath, "config", "edgegateway.json", "path to JSON configuration")
	flag.BoolVar(&initOnly, "init", false, "write the default configuration and exit")
	flag.Parse()

	if initOnly {
		if err := config.WriteExample(configPath); err != nil {
			fmt.Fprintln(os.Stderr, "write example config:", err)
			os.Exit(1)
		}
		fmt.Println(configPath)
		return
	}
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		if err := config.WriteExample(configPath); err != nil {
			fmt.Fprintln(os.Stderr, "write default config:", err)
			os.Exit(1)
		}
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(1)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}

func run(cfg config.Config) error {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := ensureCertificate(cfg.CertFile, cfg.KeyFile); err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("load certificate: %w", err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	secret, err := st.Secret(context.Background())
	if err != nil {
		return fmt.Errorf("load resume secret: %w", err)
	}
	bc := backend.New(cfg.BackendURL, cfg.BackendTimeout())
	mr := metrics.New(st)
	gw := gateway.New(cfg, st, bc, mr, secret, log)

	udpAddr, err := net.ResolveUDPAddr("udp", cfg.UDPListen)
	if err != nil {
		return fmt.Errorf("resolve UDP listen address: %w", err)
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("listen QUIC on %s: %w", cfg.UDPListen, err)
	}
	defer udpConn.Close()

	tcpLn, err := net.Listen("tcp", cfg.TCPListen)
	if err != nil {
		return fmt.Errorf("listen TCP on %s: %w", cfg.TCPListen, err)
	}
	defer tcpLn.Close()

	h3TLS := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"h3"},
	}
	quicCfg := &quic.Config{
		MaxIdleTimeout:                 30 * time.Second,
		KeepAlivePeriod:                10 * time.Second,
		HandshakeIdleTimeout:           5 * time.Second,
		InitialStreamReceiveWindow:     128 * 1024,
		MaxStreamReceiveWindow:         512 * 1024,
		InitialConnectionReceiveWindow: 256 * 1024,
		MaxConnectionReceiveWindow:     2 * 1024 * 1024,
		MaxIncomingStreams:             32,
		MaxIncomingUniStreams:          16,
	}
	h3Server := &http3.Server{
		Addr:       cfg.UDPListen,
		TLSConfig:  h3TLS,
		QuicConfig: quicCfg,
		Handler:    gw.Handler("quic"),
		ConnContext: func(ctx context.Context, conn quic.Connection) context.Context {
			return gateway.WithQUICConnection(ctx, conn)
		},
		MaxHeaderBytes: 16 * 1024,
	}

	tcpTLS := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	tcpServer := &http.Server{
		Handler:           gw.Handler("tcp"),
		TLSConfig:         tcpTLS,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	var metricsServer *http.Server
	var metricsLn net.Listener
	if cfg.MetricsListen != "" {
		metricsLn, err = net.Listen("tcp", cfg.MetricsListen)
		if err != nil {
			return fmt.Errorf("listen metrics on %s: %w", cfg.MetricsListen, err)
		}
		defer metricsLn.Close()
		metricsServer = &http.Server{
			Handler:           gw.Handler("tcp"),
			ReadHeaderTimeout: 5 * time.Second,
		}
	}

	errCh := make(chan error, 2)
	go func() {
		log.Info("QUIC/HTTP3 listening", "addr", udpConn.LocalAddr().String())
		if err := h3Server.Serve(udpConn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("HTTP/3 server: %w", err)
		}
	}()
	go func() {
		log.Info("TCP/HTTP1.1+HTTP2 listening", "addr", tcpLn.Addr().String())
		if err := tcpServer.ServeTLS(tcpLn, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("TCP server: %w", err)
		}
	}()
	if metricsServer != nil {
		go func() {
			log.Info("Prometheus metrics listening", "addr", metricsLn.Addr().String())
			if err := metricsServer.Serve(metricsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("metrics server: %w", err)
			}
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		log.Info("shutdown requested", "signal", sig.String())
	case err := <-errCh:
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout())
	defer cancel()
	_ = tcpServer.Shutdown(ctx)
	if metricsServer != nil {
		_ = metricsServer.Shutdown(ctx)
	}
	_ = h3Server.CloseGracefully(cfg.ShutdownTimeout())
	return nil
}

func ensureCertificate(certPath, keyPath string) error {
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		return err
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "edge-gateway-local"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(5, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"), net.ParseIP("::1"),
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyPath, keyPEM, 0o600)
}
