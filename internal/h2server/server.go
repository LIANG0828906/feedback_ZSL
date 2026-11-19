package h2server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"feedback_zsl/internal/entry"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type Server struct {
	Handler         http.Handler
	TLSConfig       *tls.Config
	MaxHeaderBytes  int
	MaxBodyBytes    int
	IdleTimeout     time.Duration
	Logger          *log.Logger
	OnProtocolError func(streamID uint32, code, message string)

	nextConnID atomic.Uint64
}

func (s *Server) Serve(ln net.Listener) error {
	var delay time.Duration
	for {
		raw, err := ln.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if delay == 0 {
					delay = 5 * time.Millisecond
				} else {
					delay *= 2
				}
				if delay > time.Second {
					delay = time.Second
				}
				time.Sleep(delay)
				continue
			}
			return err
		}
		delay = 0
		go s.handleRawConn(raw)
	}
}

func (s *Server) handleRawConn(raw net.Conn) {
	cfg := s.TLSConfig.Clone()
	if len(cfg.NextProtos) == 0 {
		cfg.NextProtos = []string{http2.NextProtoTLS}
	}
	tc := tls.Server(raw, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = tc.Close()
		return
	}
	if tc.ConnectionState().NegotiatedProtocol != http2.NextProtoTLS {
		_ = tc.Close()
		return
	}
	c := &conn{
		srv:      s,
		raw:      tc,
		tlsState: tc.ConnectionState(),
		writeFrm: http2.NewFramer(tc, tc),
		streams:  make(map[uint32]*stream),
		pending:  make(map[uint32]http2.PriorityParam),
		rejected: make(map[uint32]bool),
		done:     make(chan struct{}),
		connID:   fmt.Sprintf("h2c-%d", s.nextConnID.Add(1)),
		remote:   tc.RemoteAddr().String(),
		flow:     newFlowControl(),
	}
	c.enc = hpack.NewEncoder(&c.encBuf)
	if s.IdleTimeout > 0 {
		_ = tc.SetReadDeadline(time.Now().Add(s.IdleTimeout))
	}
	c.serve()
}

type conn struct {
	srv        *Server
	raw        net.Conn
	tlsState   tls.ConnectionState
	readFrm    *http2.Framer
	writeFrm   *http2.Framer
	writeMu    sync.Mutex
	enc        *hpack.Encoder
	encBuf     bytes.Buffer
	streams    map[uint32]*stream
	pending    map[uint32]http2.PriorityParam
	rejected   map[uint32]bool
	done       chan struct{}
	doneOnce   sync.Once
	connID     string
	remote     string
	flow       *flowControl
	maxStream  uint32
	settingsMu sync.Mutex
	closed     atomic.Bool
}

type stream struct {
	id         uint32
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	method     string
	path       string
	scheme     string
	authority  string
	header     http.Header
	trailer    http.Header
	body       bytes.Buffer
	contentLen int64
	priority   http2.PriorityParam
	hasPrio    bool
	started    bool
	rejected   bool
	ended      bool
}

type protocolError struct {
	status  int
	code    string
	message string
}

func (c *conn) serve() {
	defer c.close()
	c.readFrm = http2.NewFramer(c.raw, c.raw)
	c.readFrm.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	max := c.srv.MaxHeaderBytes
	if max <= 0 {
		max = 32 << 10
	}
	c.readFrm.SetMaxReadFrameSize(16 << 20)
	// Decode up to 4x the application limit so oversized-but-bounded header
	// blocks can receive a stream-local 431 instead of a connection failure.
	c.readFrm.MaxHeaderListSize = uint32(max * 4)

	if err := c.sendSettings(); err != nil {
		return
	}
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(c.raw, preface); err != nil || string(preface) != http2.ClientPreface {
		return
	}
	for {
		f, err := c.readFrm.ReadFrame()
		if err != nil {
			if c.handleReadError(err) {
				return
			}
			continue
		}
		if c.srv.IdleTimeout > 0 {
			_ = c.raw.SetReadDeadline(time.Now().Add(c.srv.IdleTimeout))
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if err := c.handleSettings(f); err != nil {
				return
			}
		case *http2.MetaHeadersFrame:
			c.handleHeaders(f)
		case *http2.DataFrame:
			c.handleData(f)
		case *http2.PriorityFrame:
			c.handlePriority(f)
		case *http2.RSTStreamFrame:
			c.handleReset(f)
		case *http2.WindowUpdateFrame:
			c.flow.add(f.StreamID, int64(f.Increment))
		case *http2.PingFrame:
			if !f.IsAck() {
				_ = c.writeControl(func(fr *http2.Framer) error {
					return fr.WritePing(true, f.Data)
				})
			}
		case *http2.GoAwayFrame:
			return
		}
	}
}

func (c *conn) sendSettings() error {
	maxHeader := c.srv.MaxHeaderBytes
	if maxHeader <= 0 {
		maxHeader = 32 << 10
	}
	return c.writeControl(func(fr *http2.Framer) error {
		return fr.WriteSettings(
			http2.Setting{ID: http2.SettingMaxConcurrentStreams, Val: 256},
			http2.Setting{ID: http2.SettingInitialWindowSize, Val: 1 << 20},
			http2.Setting{ID: http2.SettingMaxHeaderListSize, Val: uint32(maxHeader)},
		)
	})
}

func (c *conn) handleSettings(f *http2.SettingsFrame) error {
	if f.IsAck() {
		return nil
	}
	var initial int64 = -1
	iterErr := f.ForeachSetting(func(s http2.Setting) error {
		switch s.ID {
		case http2.SettingInitialWindowSize:
			initial = int64(s.Val)
		case http2.SettingHeaderTableSize:
			c.writeMu.Lock()
			c.enc.SetMaxDynamicTableSize(s.Val)
			c.writeMu.Unlock()
		}
		return nil
	})
	if iterErr != nil {
		return iterErr
	}
	if initial >= 0 {
		c.flow.setInitialWindow(initial)
	}
	return c.writeControl(func(fr *http2.Framer) error { return fr.WriteSettingsAck() })
}

func (c *conn) handlePriority(f *http2.PriorityFrame) {
	if f.StreamDep == f.StreamID {
		c.protocolReject(f.StreamID, http.StatusBadRequest, "PROTOCOL_INVALID_HEADER", "priority stream cannot depend on itself")
		return
	}
	s := c.streams[f.StreamID]
	if s == nil {
		c.pending[f.StreamID] = f.PriorityParam
		return
	}
	s.mu.Lock()
	if !s.started {
		s.priority = f.PriorityParam
		s.hasPrio = true
	}
	s.mu.Unlock()
}

func (c *conn) handleReset(f *http2.RSTStreamFrame) {
	s := c.streams[f.StreamID]
	if s == nil {
		return
	}
	s.mu.Lock()
	s.rejected = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
}

func (c *conn) handleHeaders(f *http2.MetaHeadersFrame) {
	id := f.StreamID
	if id == 0 || id%2 == 0 {
		c.goAway(http2.ErrCodeProtocol, "client opened an invalid stream")
		return
	}
	if s := c.streams[id]; s != nil {
		c.handleTrailers(s, f)
		return
	}
	if id <= c.maxStream {
		c.goAway(http2.ErrCodeProtocol, "stream id moved backwards")
		return
	}
	c.maxStream = id

	s := &stream{
		id:      id,
		header:  make(http.Header),
		trailer: make(http.Header),
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if p, ok := c.pending[id]; ok {
		s.priority, s.hasPrio = p, true
		delete(c.pending, id)
	}
	if f.HasPriority() {
		s.priority, s.hasPrio = f.Priority, true
	}
	c.streams[id] = s

	if f.Truncated {
		c.protocolReject(id, http.StatusRequestHeaderFieldsTooLarge, "PROTOCOL_HEADER_TOO_LARGE", "header list exceeded configured limit")
		return
	}
	parts, perr := c.validateRequestHeaders(f)
	if perr != nil {
		c.protocolReject(id, perr.status, perr.code, perr.message)
		return
	}
	s.mu.Lock()
	s.method = parts.method
	s.path = parts.path
	s.scheme = parts.scheme
	s.authority = parts.authority
	s.header = parts.header
	s.contentLen = parts.contentLength
	s.mu.Unlock()
	if f.StreamEnded() {
		c.dispatch(s)
	}
}

type requestParts struct {
	method        string
	path          string
	scheme        string
	authority     string
	header        http.Header
	contentLength int64
}

func (c *conn) validateRequestHeaders(f *http2.MetaHeadersFrame) (*requestParts, *protocolError) {
	parts := &requestParts{header: make(http.Header), contentLength: -1}
	size := 0
	var sawMethod, sawPath, sawScheme, sawAuthority bool
	for _, hf := range f.Fields {
		size += len(hf.Name) + len(hf.Value) + 32
		if hf.IsPseudo() {
			if sawMethod || sawPath || sawScheme || sawAuthority {
				// Duplicate and pseudo-order errors are normally caught by x/net/http2.
			}
			switch hf.Name {
			case ":method":
				if sawMethod {
					return nil, badRequest("PROTOCOL_INVALID_HEADER", "duplicate :method")
				}
				sawMethod = true
				parts.method = hf.Value
			case ":path":
				if sawPath {
					return nil, badRequest("PROTOCOL_INVALID_HEADER", "duplicate :path")
				}
				sawPath = true
				parts.path = hf.Value
			case ":scheme":
				if sawScheme {
					return nil, badRequest("PROTOCOL_INVALID_HEADER", "duplicate :scheme")
				}
				sawScheme = true
				parts.scheme = hf.Value
			case ":authority":
				if sawAuthority {
					return nil, badRequest("PROTOCOL_INVALID_HEADER", "duplicate :authority")
				}
				sawAuthority = true
				parts.authority = hf.Value
			case ":protocol":
				return nil, &protocolError{status: http.StatusBadRequest, code: "PROTOCOL_INVALID_HEADER", message: ":protocol is not supported"}
			default:
				return nil, badRequest("PROTOCOL_INVALID_HEADER", "unknown pseudo header "+hf.Name)
			}
			continue
		}
		name := strings.ToLower(hf.Name)
		if name != hf.Name {
			return nil, badRequest("PROTOCOL_INVALID_HEADER", "HTTP/2 header names must be lowercase")
		}
		if invalidConnectionHeader(name, hf.Value) {
			return nil, badRequest("PROTOCOL_INVALID_HEADER", "connection-specific header is not allowed: "+name)
		}
		parts.header.Add(textproto.CanonicalMIMEHeaderKey(name), hf.Value)
	}
	if size > c.maxHeaderBytes() {
		return nil, &protocolError{status: http.StatusRequestHeaderFieldsTooLarge, code: "PROTOCOL_HEADER_TOO_LARGE", message: "header list exceeded configured limit"}
	}
	if parts.method == "" {
		return nil, badRequest("PROTOCOL_INVALID_REQUEST", "missing :method")
	}
	if parts.path == "" && parts.method != http.MethodConnect {
		return nil, badRequest("PROTOCOL_INVALID_REQUEST", "missing :path")
	}
	if parts.scheme == "" && parts.method != http.MethodConnect {
		return nil, badRequest("PROTOCOL_INVALID_REQUEST", "missing :scheme")
	}
	if parts.authority == "" {
		parts.authority = parts.header.Get("Host")
	}
	if parts.authority == "" && parts.method != http.MethodConnect {
		return nil, badRequest("PROTOCOL_MISSING_REQUIRED_HEADER", "missing :authority")
	}
	if parts.method == http.MethodConnect && parts.path == "" {
		parts.path = "/"
	}
	for _, required := range c.requiredHeaders() {
		if required == "host" {
			if parts.authority == "" {
				return nil, badRequest("PROTOCOL_MISSING_REQUIRED_HEADER", "missing required header host")
			}
			continue
		}
		if parts.header.Get(required) == "" {
			return nil, badRequest("PROTOCOL_MISSING_REQUIRED_HEADER", "missing required header "+required)
		}
	}
	if cl := parts.header.Get("Content-Length"); cl != "" {
		v, err := strconv.ParseInt(cl, 10, 64)
		if err != nil || v < 0 {
			return nil, badRequest("PROTOCOL_INVALID_HEADER", "invalid Content-Length")
		}
		parts.contentLength = v
	}
	return parts, nil
}

func (c *conn) handleTrailers(s *stream, f *http2.MetaHeadersFrame) {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if started {
		c.protocolReject(s.id, http.StatusBadRequest, "PROTOCOL_INVALID_TRAILER", "trailers arrived after request dispatch")
		return
	}
	if !f.StreamEnded() {
		c.protocolReject(s.id, http.StatusBadRequest, "PROTOCOL_INVALID_TRAILER", "trailers must end the stream")
		return
	}
	if f.Truncated {
		c.protocolReject(s.id, http.StatusRequestHeaderFieldsTooLarge, "PROTOCOL_HEADER_TOO_LARGE", "trailer list exceeded configured limit")
		return
	}
	for _, hf := range f.Fields {
		if hf.IsPseudo() {
			c.protocolReject(s.id, http.StatusBadRequest, "PROTOCOL_INVALID_TRAILER", "pseudo header in trailers")
			return
		}
		name := strings.ToLower(hf.Name)
		if name != hf.Name || invalidTrailerHeader(name, hf.Value) {
			c.protocolReject(s.id, http.StatusBadRequest, "PROTOCOL_INVALID_TRAILER", "forbidden trailer field "+name)
			return
		}
		s.trailer.Add(textproto.CanonicalMIMEHeaderKey(name), hf.Value)
	}
	c.dispatch(s)
}

func (c *conn) handleData(f *http2.DataFrame) {
	s := c.streams[f.StreamID]
	if s == nil || c.rejected[f.StreamID] {
		c.sendReset(f.StreamID, http2.ErrCodeStreamClosed)
		return
	}
	s.mu.Lock()
	rejected := s.rejected
	s.mu.Unlock()
	if rejected {
		return
	}
	data := f.Data()
	if max := c.srv.MaxBodyBytes; max > 0 && s.body.Len()+len(data) > max {
		c.protocolReject(s.id, http.StatusRequestEntityTooLarge, "REQUEST_BODY_TOO_LARGE", "request body exceeded configured limit")
		return
	}
	_, _ = s.body.Write(data)
	if len(data) > 0 {
		_ = c.writeControl(func(fr *http2.Framer) error {
			if err := fr.WriteWindowUpdate(0, uint32(len(data))); err != nil {
				return err
			}
			return fr.WriteWindowUpdate(s.id, uint32(len(data)))
		})
	}
	if f.StreamEnded() {
		s.mu.Lock()
		if s.contentLen >= 0 && int64(s.body.Len()) != s.contentLen {
			s.mu.Unlock()
			c.protocolReject(s.id, http.StatusBadRequest, "PROTOCOL_INVALID_HEADER", "DATA length does not match Content-Length")
			return
		}
		s.mu.Unlock()
		c.dispatch(s)
	}
}

func (c *conn) dispatch(s *stream) {
	s.mu.Lock()
	if s.started || s.rejected {
		s.mu.Unlock()
		return
	}
	s.started = true
	method := s.method
	path := s.path
	scheme := s.scheme
	authority := s.authority
	header := cloneHeader(s.header)
	trailer := cloneHeader(s.trailer)
	body := append([]byte(nil), s.body.Bytes()...)
	prio := s.priority
	hasPrio := s.hasPrio
	s.mu.Unlock()

	u, err := parseRequestURL(scheme, authority, path)
	if err != nil {
		c.protocolReject(s.id, http.StatusBadRequest, "PROTOCOL_INVALID_REQUEST", err.Error())
		return
	}
	req := &http.Request{
		Method:        method,
		URL:           u,
		Proto:         "HTTP/2.0",
		ProtoMajor:    2,
		ProtoMinor:    0,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Host:          authority,
		RequestURI:    path,
		RemoteAddr:    c.remote,
		TLS:           &c.tlsState,
		Trailer:       trailer,
	}
	meta := entry.Meta{
		Protocol:     "h2",
		StreamID:     strconv.FormatUint(uint64(s.id), 10),
		ConnectionID: c.connID,
		RemoteAddr:   c.remote,
	}
	if hasPrio {
		meta.H2Priority = entry.H2Priority{
			Weight:    prio.Weight,
			StreamDep: prio.StreamDep,
			Exclusive: prio.Exclusive,
			Set:       true,
		}
	}
	req = req.WithContext(entry.WithMeta(s.ctx, meta))
	rw := &responseWriter{c: c, id: s.id, ctx: s.ctx, header: make(http.Header)}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				if p != http.ErrAbortHandler && c.srv.Logger != nil {
					c.srv.Logger.Printf("h2 handler panic on stream %d: %v", s.id, p)
				}
				s.cancel()
				rw.abort()
				return
			}
			rw.finish()
		}()
		if c.srv.Handler == nil {
			http.NotFound(rw, req)
			return
		}
		c.srv.Handler.ServeHTTP(rw, req)
	}()
}

func (c *conn) protocolReject(id uint32, status int, code, message string) {
	if id == 0 {
		c.goAway(http2.ErrCodeProtocol, message)
		return
	}
	if c.rejected[id] {
		return
	}
	c.rejected[id] = true
	if s := c.streams[id]; s != nil {
		s.mu.Lock()
		s.rejected = true
		if s.cancel != nil {
			s.cancel()
		}
		s.mu.Unlock()
	}
	if c.srv.OnProtocolError != nil {
		c.srv.OnProtocolError(id, code, message)
	}
	_ = c.writeJSONResponse(id, status, code, message)
}

func (c *conn) writeJSONResponse(id uint32, status int, code, message string) error {
	body, _ := json.Marshal(map[string]string{
		"code":      code,
		"message":   message,
		"stream_id": strconv.FormatUint(uint64(id), 10),
	})
	fields := []hpack.HeaderField{
		{Name: ":status", Value: strconv.Itoa(status)},
		{Name: "content-type", Value: "application/json"},
		{Name: "content-length", Value: strconv.Itoa(len(body))},
		{Name: "x-gateway-protocol-error", Value: code},
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.encBuf.Reset()
	for _, f := range fields {
		if err := c.enc.WriteField(f); err != nil {
			return err
		}
	}
	if err := c.writeFrm.WriteHeaders(http2.HeadersFrameParam{
		StreamID: id, BlockFragment: c.encBuf.Bytes(), EndHeaders: true,
	}); err != nil {
		return err
	}
	return c.writeFrm.WriteData(id, true, body)
}

func (c *conn) maxHeaderBytes() int {
	if c.srv.MaxHeaderBytes > 0 {
		return c.srv.MaxHeaderBytes
	}
	return 32 << 10
}

func (c *conn) requiredHeaders() []string {
	if c.srv.Handler == nil {
		return []string{"host"}
	}
	if rh, ok := c.srv.Handler.(interface{ RequiredHeaders() []string }); ok {
		return rh.RequiredHeaders()
	}
	return []string{"host"}
}

func (c *conn) handleReadError(err error) bool {
	var se http2.StreamError
	if errors.As(err, &se) {
		c.protocolReject(se.StreamID, http.StatusBadRequest, "PROTOCOL_INVALID_HEADER", se.Error())
		return false
	}
	if c.srv.Logger != nil && !errors.Is(err, io.EOF) {
		c.srv.Logger.Printf("h2 connection %s read error: %v", c.connID, err)
	}
	return true
}

func (c *conn) goAway(code http2.ErrCode, debug string) {
	_ = c.writeControl(func(fr *http2.Framer) error { return fr.WriteGoAway(c.maxStream, code, []byte(debug)) })
}

func (c *conn) sendReset(id uint32, code http2.ErrCode) {
	_ = c.writeControl(func(fr *http2.Framer) error { return fr.WriteRSTStream(id, code) })
}

func (c *conn) writeControl(fn func(*http2.Framer) error) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return fn(c.writeFrm)
}

func (c *conn) close() {
	c.doneOnce.Do(func() {
		c.closed.Store(true)
		close(c.done)
		c.writeMu.Lock()
		_ = c.raw.Close()
		c.writeMu.Unlock()
		for _, s := range c.streams {
			s.mu.Lock()
			if s.cancel != nil {
				s.cancel()
			}
			s.mu.Unlock()
		}
	})
}

type responseWriter struct {
	c      *conn
	id     uint32
	ctx    context.Context
	header http.Header
	mu     sync.Mutex
	status int
	wrote  bool
	ended  bool
}

func (w *responseWriter) Header() http.Header { return w.header }

func (w *responseWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wrote || w.ended {
		return
	}
	w.status = status
	w.wrote = true
	_ = w.writeHeaderLocked(false)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx != nil && w.ctx.Err() != nil {
		return 0, w.ctx.Err()
	}
	if w.ended {
		return 0, http.ErrHandlerTimeout
	}
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
		if err := w.writeHeaderLocked(false); err != nil {
			return 0, err
		}
	}
	written := 0
	for len(p) > 0 {
		n := w.c.flow.take(w.c.done, w.id, len(p))
		if n <= 0 {
			return written, context.Canceled
		}
		if err := w.c.writeControl(func(fr *http2.Framer) error {
			return fr.WriteData(w.id, false, p[:n])
		}); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (w *responseWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx != nil && w.ctx.Err() != nil {
		return
	}
	if !w.wrote && !w.ended {
		w.status = http.StatusOK
		w.wrote = true
		_ = w.writeHeaderLocked(false)
	}
}

func (w *responseWriter) finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended || w.c.closed.Load() || (w.ctx != nil && w.ctx.Err() != nil) {
		return
	}
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
		if err := w.writeHeaderLocked(false); err != nil {
			return
		}
	}
	trailers := collectTrailers(w.header)
	if len(trailers) > 0 {
		if err := w.writeTrailersLocked(trailers); err != nil {
			return
		}
		w.ended = true
		return
	}
	_ = w.c.writeControl(func(fr *http2.Framer) error {
		return fr.WriteData(w.id, true, nil)
	})
	w.ended = true
}

func (w *responseWriter) abort() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended {
		return
	}
	w.ended = true
	if !w.wrote || w.status < 200 {
		_ = w.c.writeJSONResponse(w.id, http.StatusBadGateway, "GATEWAY_HANDLER_PANIC", "handler panicked")
		return
	}
	w.c.sendReset(w.id, http2.ErrCodeInternal)
}

func (w *responseWriter) writeHeaderLocked(endStream bool) error {
	fields := []hpack.HeaderField{{Name: ":status", Value: strconv.Itoa(w.status)}}
	keys := make([]string, 0, len(w.header))
	for k := range w.header {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		if strings.EqualFold(k, "Trailer") || isHopByHop(k) {
			continue
		}
		for _, v := range w.header[k] {
			fields = append(fields, hpack.HeaderField{Name: strings.ToLower(k), Value: v})
		}
	}
	w.c.writeMu.Lock()
	defer w.c.writeMu.Unlock()
	w.c.encBuf.Reset()
	for _, field := range fields {
		if err := w.c.enc.WriteField(field); err != nil {
			return err
		}
	}
	return w.c.writeFrm.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      w.id,
		BlockFragment: append([]byte(nil), w.c.encBuf.Bytes()...),
		EndStream:     endStream,
		EndHeaders:    true,
	})
}

func (w *responseWriter) writeTrailersLocked(trailers http.Header) error {
	w.c.writeMu.Lock()
	defer w.c.writeMu.Unlock()
	w.c.encBuf.Reset()
	keys := make([]string, 0, len(trailers))
	for k := range trailers {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		for _, v := range trailers[k] {
			if err := w.c.enc.WriteField(hpack.HeaderField{Name: strings.ToLower(k), Value: v}); err != nil {
				return err
			}
		}
	}
	return w.c.writeFrm.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      w.id,
		BlockFragment: append([]byte(nil), w.c.encBuf.Bytes()...),
		EndStream:     true,
		EndHeaders:    true,
	})
}

func collectTrailers(h http.Header) http.Header {
	out := make(http.Header)
	for k, v := range h {
		if strings.HasPrefix(k, http.TrailerPrefix) {
			out[strings.TrimPrefix(k, http.TrailerPrefix)] = append([]string(nil), v...)
		}
	}
	return out
}

type flowControl struct {
	mu      sync.Mutex
	conn    int64
	initial int64
	streams map[uint32]int64
}

func newFlowControl() *flowControl {
	return &flowControl{conn: 65535, initial: 65535, streams: make(map[uint32]int64)}
}

func (f *flowControl) setInitialWindow(v int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delta := v - f.initial
	f.initial = v
	for id, n := range f.streams {
		f.streams[id] = n + delta
	}
}

func (f *flowControl) add(streamID uint32, n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if streamID == 0 {
		f.conn += n
		return
	}
	f.streams[streamID] += n
}

func (f *flowControl) take(done <-chan struct{}, streamID uint32, want int) int {
	const maxFrame = 16 << 10
	if want > maxFrame {
		want = maxFrame
	}
	for {
		f.mu.Lock()
		if _, ok := f.streams[streamID]; !ok {
			f.streams[streamID] = f.initial
		}
		avail := f.conn
		if sw := f.streams[streamID]; sw < avail {
			avail = sw
		}
		if avail > 0 {
			n := int64(want)
			if avail < n {
				n = avail
			}
			f.conn -= n
			f.streams[streamID] -= n
			f.mu.Unlock()
			return int(n)
		}
		f.mu.Unlock()
		select {
		case <-done:
			return 0
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func parseRequestURL(scheme, authority, path string) (*url.URL, error) {
	if path == "*" {
		return &url.URL{Scheme: scheme, Host: authority, Path: "*"}, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, errors.New(":path must start with /")
	}
	u, err := url.ParseRequestURI(path)
	if err != nil {
		return nil, err
	}
	u.Scheme = scheme
	u.Host = authority
	return u, nil
}

func badRequest(code, message string) *protocolError {
	return &protocolError{status: http.StatusBadRequest, code: code, message: message}
}

func invalidConnectionHeader(name, value string) bool {
	switch name {
	case "connection", "proxy-connection", "keep-alive", "upgrade", "transfer-encoding":
		return true
	case "te":
		return !strings.EqualFold(strings.TrimSpace(value), "trailers")
	}
	return false
}

func invalidTrailerHeader(name, value string) bool {
	switch name {
	case "connection", "proxy-connection", "keep-alive", "upgrade", "transfer-encoding",
		"content-length", "host", "trailer":
		return true
	case "te":
		return !strings.EqualFold(strings.TrimSpace(value), "trailers")
	}
	return false
}

func isHopByHop(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
