package h2server

import (
	"bytes"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"feedback_zsl/internal/entry"
	"feedback_zsl/internal/tlscert"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func startServer(t *testing.T, handler http.Handler) (net.Listener, *Server) {
	t.Helper()
	cert, err := tlscert.Generate([]string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Handler:        handler,
		TLSConfig:      &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2"}},
		MaxHeaderBytes: 4096,
		MaxBodyBytes:   1 << 20,
	}
	go func() { _ = srv.Serve(ln) }()
	return ln, srv
}

func dialH2(t *testing.T, addr string) (*tls.Conn, *http2.Framer) {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(conn, conn)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	return conn, fr
}

func encodeHeaders(t *testing.T, enc *hpack.Encoder, buf *bytes.Buffer, fields ...hpack.HeaderField) []byte {
	t.Helper()
	buf.Reset()
	for _, field := range fields {
		if err := enc.WriteField(field); err != nil {
			t.Fatal(err)
		}
	}
	return append([]byte(nil), buf.Bytes()...)
}

func TestPriorityFrameIsExposedToHandler(t *testing.T) {
	got := make(chan entry.Meta, 1)
	ln, _ := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta, _ := entry.FromContext(r.Context())
		got <- meta
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ln.Close()
	conn, fr := dialH2(t, ln.Addr().String())
	defer conn.Close()

	if err := fr.WritePriority(1, http2.PriorityParam{Weight: 255, StreamDep: 0}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	block := encodeHeaders(t, enc, &buf,
		hpack.HeaderField{Name: ":method", Value: "GET"},
		hpack.HeaderField{Name: ":scheme", Value: "https"},
		hpack.HeaderField{Name: ":path", Value: "/priority"},
		hpack.HeaderField{Name: ":authority", Value: "gateway.local"},
	)
	if err := fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID: 1, BlockFragment: block, EndHeaders: true, EndStream: true,
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case meta := <-got:
		if !meta.H2Priority.Set || meta.H2Priority.Weight != 255 {
			t.Fatalf("priority metadata = %#v", meta.H2Priority)
		}
		if meta.StreamID != "1" {
			t.Fatalf("stream id = %s", meta.StreamID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not observe the request")
	}
}

func TestBadStreamDoesNotBreakFollowingStream(t *testing.T) {
	var calls atomic.Int64
	ln, _ := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ln.Close()
	conn, fr := dialH2(t, ln.Addr().String())
	defer conn.Close()

	// Consume the server preface and acknowledge its SETTINGS.
	srvFrame, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := srvFrame.(*http2.SettingsFrame); !ok {
		t.Fatalf("first server frame = %T", srvFrame)
	}
	if err := fr.WriteSettingsAck(); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	missingPath := encodeHeaders(t, enc, &buf,
		hpack.HeaderField{Name: ":method", Value: "GET"},
		hpack.HeaderField{Name: ":scheme", Value: "https"},
		hpack.HeaderField{Name: ":authority", Value: "gateway.local"},
	)
	if err := fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID: 1, BlockFragment: missingPath, EndHeaders: true, EndStream: true,
	}); err != nil {
		t.Fatal(err)
	}
	normal := encodeHeaders(t, enc, &buf,
		hpack.HeaderField{Name: ":method", Value: "GET"},
		hpack.HeaderField{Name: ":scheme", Value: "https"},
		hpack.HeaderField{Name: ":path", Value: "/ok"},
		hpack.HeaderField{Name: ":authority", Value: "gateway.local"},
	)
	if err := fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID: 3, BlockFragment: normal, EndHeaders: true, EndStream: true,
	}); err != nil {
		t.Fatal(err)
	}

	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	statuses := map[uint32]string{}
	bodies := map[uint32][]byte{}
	ended := map[uint32]bool{}
	deadline := time.Now().Add(3 * time.Second)
	for !(ended[1] && ended[3]) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for both streams: status=%v ended=%v", statuses, ended)
		}
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		switch f := f.(type) {
		case *http2.MetaHeadersFrame:
			statuses[f.StreamID] = f.PseudoValue("status")
		case *http2.DataFrame:
			bodies[f.StreamID] = append(bodies[f.StreamID], f.Data()...)
			if f.StreamEnded() {
				ended[f.StreamID] = true
			}
		case *http2.RSTStreamFrame:
			t.Fatalf("unexpected RST_STREAM for %d: %v", f.StreamID, f.ErrCode)
		}
	}
	if statuses[1] != "400" {
		t.Fatalf("bad stream status = %q, body=%s", statuses[1], bodies[1])
	}
	if statuses[3] != "200" || string(bodies[3]) != "ok" {
		t.Fatalf("good stream status=%q body=%q", statuses[3], bodies[3])
	}
	if calls.Load() != 1 {
		t.Fatalf("handler calls = %d, want 1", calls.Load())
	}
}

func TestOversizedHeaderReturns431OnThatStream(t *testing.T) {
	ln, _ := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ln.Close()
	conn, fr := dialH2(t, ln.Addr().String())
	defer conn.Close()
	// Read the server preface so a write buffer cannot hide a protocol failure.
	_, _ = fr.ReadFrame()

	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	block := encodeHeaders(t, enc, &buf,
		hpack.HeaderField{Name: ":method", Value: "GET"},
		hpack.HeaderField{Name: ":scheme", Value: "https"},
		hpack.HeaderField{Name: ":path", Value: "/huge"},
		hpack.HeaderField{Name: ":authority", Value: "gateway.local"},
		hpack.HeaderField{Name: "x-huge", Value: strings.Repeat("a", 8192)},
	)
	if err := fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID: 1, BlockFragment: block, EndHeaders: true, EndStream: true,
	}); err != nil {
		t.Fatal(err)
	}
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for 431 response")
		}
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if mh, ok := f.(*http2.MetaHeadersFrame); ok && mh.StreamID == 1 {
			if got := mh.PseudoValue("status"); got != "431" {
				t.Fatalf("status = %s, want 431", got)
			}
			return
		}
	}
}

func TestIllegalTrailerIsRejectedBeforeDispatch(t *testing.T) {
	var calls atomic.Int64
	ln, _ := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ln.Close()
	conn, fr := dialH2(t, ln.Addr().String())
	defer conn.Close()
	_, _ = fr.ReadFrame()
	_ = fr.WriteSettingsAck()

	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	initial := encodeHeaders(t, enc, &buf,
		hpack.HeaderField{Name: ":method", Value: "GET"},
		hpack.HeaderField{Name: ":scheme", Value: "https"},
		hpack.HeaderField{Name: ":path", Value: "/trailer"},
		hpack.HeaderField{Name: ":authority", Value: "gateway.local"},
	)
	if err := fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID: 1, BlockFragment: initial, EndHeaders: true,
	}); err != nil {
		t.Fatal(err)
	}
	trailer := encodeHeaders(t, enc, &buf,
		hpack.HeaderField{Name: "content-length", Value: "0"},
	)
	if err := fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID: 1, BlockFragment: trailer, EndHeaders: true, EndStream: true,
	}); err != nil {
		t.Fatal(err)
	}
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for trailer rejection")
		}
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if mh, ok := f.(*http2.MetaHeadersFrame); ok && mh.StreamID == 1 {
			if got := mh.PseudoValue("status"); got != "400" {
				t.Fatalf("status = %s, want 400", got)
			}
			if calls.Load() != 0 {
				t.Fatal("handler was invoked for illegal trailers")
			}
			return
		}
	}
}
