package proxy

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"testing"
	"time"

	"feedback_zsl/internal/config"
	"feedback_zsl/internal/entry"
	"feedback_zsl/internal/fixture"
	"feedback_zsl/internal/h2server"
	"feedback_zsl/internal/loadgen"
	"feedback_zsl/internal/tlscert"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

func TestDualStackResponseEquivalence(t *testing.T) {
	fx, err := fixture.Start(fixture.Config{
		H1Addr: "127.0.0.1:0", H2Addr: "127.0.0.1:0", H2CAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fx.Close()

	cfg := testConfig(
		"http://"+fx.Addrs["http1"],
		"http://"+fx.Addrs["http1"],
	)
	cfg.Upstreams = []config.UpstreamConfig{
		{ID: "content-h1", URL: "http://" + fx.Addrs["http1"], Protocol: "http1", MaxConnections: 2, MaxInflight: 2},
		{ID: "content-h2", URL: "https://" + fx.Addrs["http2"], Protocol: "http2", TLSInsecure: true, MaxConnections: 2, MaxInflight: 2},
		{ID: "content-h2c", URL: "http://" + fx.Addrs["h2c"], Protocol: "h2c", MaxConnections: 2, MaxInflight: 2},
	}
	cfg.Routes = []config.RouteConfig{{
		ID: "shared", PathPrefix: "/api/shared", Upstreams: []string{"content-h1", "content-h2", "content-h2c"},
	}}
	cfg.RequiredHdrs = []string{"x-required"}
	p, err := NewWithConfig(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())

	cert, err := tlscert.Generate([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	h2ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer h2ln.Close()
	h2srv := &h2server.Server{
		Handler: p, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2"}},
		MaxHeaderBytes: 16 << 10, MaxBodyBytes: 1 << 20,
	}
	go func() { _ = h2srv.Serve(h2ln) }()

	h3pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer h3pc.Close()
	h3srv := &http3.Server{
		TLSConfig:  http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}),
		Handler:    &H3Handler{Next: p},
		QUICConfig: &quic.Config{MaxIdleTimeout: 5 * time.Second},
		ConnContext: func(ctx context.Context, c quic.Connection) context.Context {
			return entry.WithConnectionInfo(ctx, &entry.ConnectionInfo{
				ID: "test-h3", RemoteAddr: c.RemoteAddr().String(),
			})
		},
	}
	go func() { _ = h3srv.Serve(h3pc) }()
	defer h3srv.Close()
	p.Start()

	h2 := loadgen.NewH2Client(true)
	defer h2.Close()
	h3, err := loadgen.NewH3Client("127.0.0.1", true)
	if err != nil {
		t.Fatal(err)
	}
	defer h3.Close()
	tpl := loadgen.Template{
		Method: http.MethodPost, Path: "/api/shared/echo?dual=1", Priority: "high",
		Headers: map[string]string{"Content-Type": "application/json", "X-Test-Key": "dual", "X-Required": "yes"},
		Body:    `{"dual":true}`,
	}
	r2 := h2.Do(context.Background(), "https://"+h2ln.Addr().String(), tpl, "dual-request", "")
	r3 := h3.Do(context.Background(), "https://"+h3pc.LocalAddr().String(), tpl, "dual-request", "")
	if r2.Err != nil || r3.Err != nil {
		t.Fatalf("h2 err=%v h3 err=%v", r2.Err, r3.Err)
	}
	if r2.Status != http.StatusOK || r3.Status != http.StatusOK {
		t.Fatalf("statuses h2=%d h3=%d", r2.Status, r3.Status)
	}
	if r2.BodyHash != r3.BodyHash {
		t.Fatalf("response bodies differ: h2=%s h3=%s", r2.BodyHash, r3.BodyHash)
	}
	if r2.Upstream != r3.Upstream {
		t.Fatalf("upstreams differ: h2=%s h3=%s", r2.Upstream, r3.Upstream)
	}

	missing := loadgen.Template{Method: http.MethodGet, Path: "/api/shared/missing", Priority: "normal"}
	m2 := h2.Do(context.Background(), "https://"+h2ln.Addr().String(), missing, "missing-h2", "")
	m3 := h3.Do(context.Background(), "https://"+h3pc.LocalAddr().String(), missing, "missing-h3", "")
	if m2.Status != http.StatusBadRequest || m3.Status != http.StatusBadRequest {
		t.Fatalf("missing header statuses h2=%d h3=%d", m2.Status, m3.Status)
	}
	if m2.Code != CodeProtocolMissingHeader || m3.Code != CodeProtocolMissingHeader {
		t.Fatalf("missing header codes h2=%s h3=%s", m2.Code, m3.Code)
	}
}
