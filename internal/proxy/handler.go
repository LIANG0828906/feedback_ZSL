package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"time"

	"feedback_zsl/internal/entry"
	"feedback_zsl/internal/priority"
	"feedback_zsl/internal/upstream"
)

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt := p.acquireRuntime()
	if rt == nil {
		writeError(w, http.StatusServiceUnavailable, errorBody{Code: CodeGatewayInternal, Message: "proxy is shutting down"})
		return
	}
	defer rt.release()
	originalWriter := w
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	var buffered *bufferedResponse
	var idemEntry *idempotencyEntry
	if idempotencyKey != "" && !upstream.IsSafeRead(r.Method, 0) {
		buffered = newBufferedResponse(rt.cfg.Limits.MaxResponseBytes)
		w = buffered
		defer func() {
			if idemEntry != nil {
				status, header, body := buffered.result()
				idemEntry.complete(status, header, body)
			}
			buffered.flush(originalWriter)
		}()
	}

	meta, _ := entry.FromContext(r.Context())
	protocol := entry.ProtocolOf(r)
	mapping := p.mappingFor(rt, r, meta)
	p.metrics.EntryStart(protocol, mapping.Priority.Name)
	defer p.metrics.EntryFinish(protocol)

	requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
	if requestID == "" {
		requestID = fmt.Sprintf("%s-%d", protocol, time.Now().UnixNano())
	}
	w.Header().Set("X-Gateway-Request-ID", requestID)
	w.Header().Set("X-Gateway-Priority-Name", mapping.Priority.Name)

	migration, migrated := p.observeMigration(r, meta)
	if migrated {
		w.Header().Set("X-Gateway-Migration", "resumed")
	} else if migration != "" {
		w.Header().Set("X-Gateway-Migration", "initial")
	}
	migrationSuccess := false
	defer func() {
		if migration == "" {
			return
		}
		if migrationSuccess {
			p.metrics.Migration("succeeded")
		} else {
			p.metrics.Migration("failed")
		}
	}()

	body, tooLarge, err := readRequestBody(r, rt.cfg.Limits.MaxRequestBytes)
	if tooLarge {
		p.recordProtocolError(protocol, meta.StreamID, CodeRequestBodyTooLarge, "request body exceeded configured limit")
		p.metrics.EntryError(protocol, CodeRequestBodyTooLarge)
		writeError(w, http.StatusRequestEntityTooLarge, errorBody{
			Code: CodeRequestBodyTooLarge, Message: "request body exceeded configured limit", RequestID: requestID,
		})
		return
	}
	if err != nil {
		if r.Context().Err() != nil {
			p.metrics.EntryError(protocol, CodeDownstreamCanceled)
			return
		}
		p.metrics.EntryError(protocol, CodeProtocolInvalidRequest)
		writeError(w, http.StatusBadRequest, errorBody{Code: CodeProtocolInvalidRequest, Message: err.Error(), RequestID: requestID})
		return
	}
	if perr := validateForwardHeaders(r, rt.cfg.Limits.MaxHeaderBytes, rt.cfg.RequiredHdrs, body); perr != nil {
		p.recordProtocolError(protocol, meta.StreamID, perr.Code, perr.Message)
		p.metrics.EntryError(protocol, perr.Code)
		writeError(w, perr.Status, errorBody{Code: perr.Code, Message: perr.Message, RequestID: requestID})
		return
	}

	route := rt.match(r)
	if route == nil {
		writeError(w, http.StatusNotFound, errorBody{Code: CodeNoRoute, Message: "no configured route matches " + r.URL.Path, RequestID: requestID})
		return
	}
	if buffered != nil {
		key := rt.cfg.Revision + "|" + route.cfg.ID + "|" + r.Method + "|" + idempotencyKey
		entry, leader := rt.idem.begin(key)
		if !leader {
			buffered.abandoned = true
			response, ok := entry.wait(r.Context())
			if !ok {
				return
			}
			response.replay(originalWriter)
			return
		}
		idemEntry = entry
	}

	ctx := r.Context()
	var cancel context.CancelFunc
	if d := rt.cfg.Timeouts.Request.Duration(); d > 0 {
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	lease, waited, err := rt.sched.Acquire(ctx, mapping.Priority)
	if err != nil {
		if r.Context().Err() != nil {
			p.metrics.EntryError(protocol, CodeDownstreamCanceled)
			return
		}
		p.metrics.EntryError(protocol, CodeQueueTimeout)
		writeError(w, http.StatusGatewayTimeout, errorBody{
			Code: CodeQueueTimeout, Message: "priority queue wait exceeded request deadline", RequestID: requestID,
		})
		return
	}
	defer lease.Release()
	defer func() {
		p.metrics.ObservePriorityWait(protocol, mapping.Priority.Name, waited)
	}()

	excluded := make(map[string]bool)
	safeRead := upstream.IsSafeRead(r.Method, len(body))
	maxAttempts := rt.cfg.Retry.Max + 1
	if !safeRead {
		maxAttempts = 1
	}
	var lastErr error
	var lastPhase string
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if r.Context().Err() != nil {
			p.metrics.EntryError(protocol, CodeDownstreamCanceled)
			return
		}
		up := rt.pick(route, excluded)
		if up == nil {
			if len(excluded) == 0 {
				lastErr = errors.New("no healthy upstream candidates")
				lastPhase = "health_check"
				break
			}
			break
		}
		outReq, err := buildUpstreamRequest(r, up, body, ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, errorBody{Code: CodeGatewayInternal, Message: err.Error(), RequestID: requestID})
			return
		}
		resp, upstreamWait, err := up.RoundTrip(ctx, outReq, mapping.Priority)
		waited += upstreamWait
		w.Header().Set("X-Gateway-Priority-Wait-Ms", strconv.FormatFloat(float64(waited)/float64(time.Millisecond), 'f', 3, 64))
		if err != nil {
			lastErr = err
			lastPhase = upstream.Phase(err)
			excluded[up.ID()] = true
			if ctx.Err() != nil {
				if r.Context().Err() != nil {
					p.metrics.EntryError(protocol, CodeDownstreamCanceled)
					return
				}
				lastPhase = "request_deadline"
				break
			}
			if upstream.IsHealthFailure(err) {
				up.MarkFailure(err)
			}
			p.metrics.UpstreamError(up.ID(), lastPhase)
			if safeRead && attempt+1 < maxAttempts && rt.healthyCount(route, excluded) > 0 {
				reason := lastPhase
				p.metrics.Retry(up.ID(), reason)
				w.Header().Set("X-Gateway-Retries", strconv.Itoa(attempt+1))
				w.Header().Set("X-Gateway-Retry-Reason", reason)
				if backoff := rt.cfg.Retry.Backoff.Duration(); backoff > 0 {
					select {
					case <-time.After(backoff):
					case <-ctx.Done():
						return
					}
				}
				continue
			}
			break
		}
		migrationSuccess = true
		p.writeUpstreamResponse(w, r, resp, requestID, mapping.Priority.Name, attempt, up.ID())
		return
	}

	if r.Context().Err() != nil {
		p.metrics.EntryError(protocol, CodeDownstreamCanceled)
		return
	}
	code := CodeUpstreamReadFailed
	status := http.StatusBadGateway
	message := "no healthy upstream could serve the read request"
	if !safeRead {
		code = CodeUpstreamWriteAmbiguous
		message = "write request failed after dispatch; automatic retry is forbidden because upstream side effects are ambiguous"
	} else if len(excluded) == 0 {
		status = http.StatusServiceUnavailable
		code = CodeUpstreamUnavailable
		message = "all configured upstreams are unhealthy"
	}
	p.metrics.EntryError(protocol, code)
	writeError(w, status, errorBody{
		Code: code, Message: message, Phase: lastPhase, RequestID: requestID,
		Upstream: strings.Join(sortedKeys(excluded), ","),
	})
	_ = lastErr
}

func (p *Proxy) mappingFor(rt *Runtime, r *http.Request, meta entry.Meta) priority.Mapping {
	if meta.Protocol == "h2" && meta.H2Priority.Set {
		return rt.mapper.FromH2Weight(meta.H2Priority.Weight, r.Header)
	}
	if m, ok := rt.mapper.FromHeaders(r.Header); ok {
		return m
	}
	return priority.Mapping{Priority: rt.mapper.Default(), Source: "default"}
}

func (p *Proxy) writeUpstreamResponse(w http.ResponseWriter, client *http.Request, resp *http.Response, requestID, priorityName string, retries int, upstreamID string) {
	defer resp.Body.Close()
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("X-Gateway-Upstream", upstreamID)
	w.Header().Set("X-Gateway-Retries", strconv.Itoa(retries))
	w.Header().Set("X-Gateway-Request-ID", requestID)
	w.Header().Set("X-Gateway-Priority-Name", priorityName)
	for k := range resp.Trailer {
		w.Header().Add("Trailer", k)
	}
	w.WriteHeader(resp.StatusCode)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	buf := make([]byte, 32<<10)
	_, err := io.CopyBuffer(w, resp.Body, buf)
	if err == nil {
		for k, values := range resp.Trailer {
			for _, v := range values {
				w.Header().Add(http.TrailerPrefix+k, v)
				w.Header().Add(k, v)
			}
		}
		return
	}
	if client.Context().Err() != nil {
		p.metrics.EntryError(entry.ProtocolOf(client), CodeDownstreamCanceled)
		return
	}
	p.metrics.UpstreamError(upstreamID, "response_body")
	p.metrics.EntryError(entry.ProtocolOf(client), CodeUpstreamResponseAborted)
}

func buildUpstreamRequest(in *http.Request, up *upstream.Upstream, body []byte, ctx context.Context) (*http.Request, error) {
	u := *up.URL()
	u.Path = in.URL.Path
	u.RawPath = in.URL.RawPath
	u.RawQuery = in.URL.RawQuery
	h := cloneHeader(in.Header)
	removeHopByHop(h)
	h.Del("X-Gateway-Priority")
	h.Del("X-Gateway-Session-ID")
	if ip, _, err := net.SplitHostPort(in.RemoteAddr); err == nil && ip != "" {
		if prior := h.Get("X-Forwarded-For"); prior != "" {
			h.Set("X-Forwarded-For", prior+", "+ip)
		} else {
			h.Set("X-Forwarded-For", ip)
		}
	}
	h.Set("X-Forwarded-Proto", "https")
	h.Set("X-Gateway-Entry-Protocol", in.Proto)
	if h.Get("User-Agent") == "" {
		h.Set("User-Agent", "")
	}
	out := &http.Request{
		Method:        in.Method,
		URL:           &u,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Host:          in.Host,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		GetBody: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		},
		Trailer: cloneHeader(in.Trailer),
	}
	if in.ContentLength < 0 {
		out.ContentLength = int64(len(body))
	}
	return out.WithContext(ctx), nil
}

func readRequestBody(r *http.Request, max int) ([]byte, bool, error) {
	if r.Body == nil {
		return nil, false, nil
	}
	defer r.Body.Close()
	if max > 0 {
		data, err := io.ReadAll(io.LimitReader(r.Body, int64(max)+1))
		if err != nil {
			return nil, false, err
		}
		if len(data) > max {
			return nil, true, nil
		}
		return data, false, nil
	}
	data, err := io.ReadAll(r.Body)
	return data, false, err
}

type headerError struct {
	Status  int
	Code    string
	Message string
}

func validateForwardHeaders(r *http.Request, max int, required []string, body []byte) *headerError {
	if r.Method == "" {
		return &headerError{Status: http.StatusBadRequest, Code: CodeProtocolInvalidRequest, Message: "missing method"}
	}
	if r.URL == nil || r.URL.Path == "" {
		return &headerError{Status: http.StatusBadRequest, Code: CodeProtocolInvalidRequest, Message: "missing path"}
	}
	if r.Host == "" {
		return &headerError{Status: http.StatusBadRequest, Code: CodeProtocolMissingHeader, Message: "missing required request host"}
	}
	size := len(r.Host) + 32
	for k, values := range r.Header {
		if strings.Contains(k, ":") {
			return &headerError{Status: http.StatusBadRequest, Code: CodeProtocolInvalidHeader, Message: "pseudo header in regular header map"}
		}
		if invalidRequestHeader(k, values) {
			return &headerError{Status: http.StatusBadRequest, Code: CodeProtocolInvalidHeader, Message: "connection-specific header is not allowed: " + k}
		}
		for _, v := range values {
			size += len(k) + len(v) + 32
		}
	}
	if size > max {
		return &headerError{Status: http.StatusRequestHeaderFieldsTooLarge, Code: CodeProtocolHeaderTooLarge, Message: "header list exceeded configured limit"}
	}
	for _, h := range required {
		if strings.EqualFold(h, "host") {
			if r.Host == "" {
				return &headerError{Status: http.StatusBadRequest, Code: CodeProtocolMissingHeader, Message: "missing required header host"}
			}
			continue
		}
		if r.Header.Get(h) == "" {
			return &headerError{Status: http.StatusBadRequest, Code: CodeProtocolMissingHeader, Message: "missing required header " + h}
		}
	}
	if cl := r.Header.Get("Content-Length"); cl != "" {
		n, err := strconv.ParseInt(cl, 10, 64)
		if err != nil || n < 0 {
			return &headerError{Status: http.StatusBadRequest, Code: CodeProtocolInvalidHeader, Message: "invalid Content-Length"}
		}
		if n != int64(len(body)) {
			return &headerError{Status: http.StatusBadRequest, Code: CodeProtocolInvalidHeader, Message: "Content-Length does not match request body"}
		}
	}
	for k, values := range r.Trailer {
		if strings.Contains(k, ":") {
			return &headerError{Status: http.StatusBadRequest, Code: CodeProtocolInvalidTrailer, Message: "pseudo header in trailers"}
		}
		if invalidTrailer(k, values) {
			return &headerError{Status: http.StatusBadRequest, Code: CodeProtocolInvalidTrailer, Message: "forbidden trailer field " + k}
		}
	}
	return nil
}

func invalidRequestHeader(k string, values []string) bool {
	switch strings.ToLower(k) {
	case "connection", "proxy-connection", "keep-alive", "upgrade", "transfer-encoding":
		return true
	case "te":
		for _, v := range values {
			if !strings.EqualFold(strings.TrimSpace(v), "trailers") {
				return true
			}
		}
	}
	return false
}

func invalidTrailer(k string, values []string) bool {
	switch strings.ToLower(k) {
	case "connection", "proxy-connection", "keep-alive", "upgrade", "transfer-encoding",
		"content-length", "host", "trailer":
		return true
	case "te":
		for _, v := range values {
			if !strings.EqualFold(strings.TrimSpace(v), "trailers") {
				return true
			}
		}
	}
	return false
}

func copyResponseHeaders(dst, src http.Header) {
	for k, values := range src {
		if isHopByHop(k) {
			continue
		}
		dst.Del(k)
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}

func removeHopByHop(h http.Header) {
	for _, token := range strings.Split(h.Get("Connection"), ",") {
		if token = strings.TrimSpace(token); token != "" {
			h.Del(token)
		}
	}
	for k := range h {
		if isHopByHop(k) {
			h.Del(k)
		}
	}
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
		out[textproto.CanonicalMIMEHeaderKey(k)] = append([]string(nil), v...)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (p *Proxy) recordProtocolError(protocol, streamID, code, message string) {
	p.metrics.ProtocolError(protocol, streamID, code, message)
}
