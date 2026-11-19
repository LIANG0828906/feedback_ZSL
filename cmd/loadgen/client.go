package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"

	"edgegateway/internal/model"
)

type sessionEnvelope struct {
	OK          bool           `json:"ok"`
	Session     *model.Session `json:"session"`
	SessionID   string         `json:"session_id"`
	ResumeToken string         `json:"resume_token"`
	Reused      bool           `json:"reused"`
	Migrated    bool           `json:"migrated"`
	Code        string         `json:"code"`
	Error       string         `json:"error"`
}

type feedbackView struct {
	Limited      bool    `json:"limited"`
	Source       string  `json:"source"`
	Reason       string  `json:"reason"`
	Basis        string  `json:"basis"`
	RatePerSec   float64 `json:"rate_limit_per_sec"`
	Frozen       bool    `json:"frozen_during_migration"`
	RetryAfterMS int64   `json:"retry_after_ms"`
}

type fragmentReply struct {
	OK                 bool          `json:"ok"`
	Code               string        `json:"code"`
	BusinessErrorCode  string        `json:"business_error_code"`
	Error              string        `json:"error"`
	SessionID          string        `json:"session_id"`
	Seq                int64         `json:"seq"`
	Accepted           bool          `json:"accepted"`
	Duplicate          bool          `json:"duplicate"`
	RejectedSeq        *int64        `json:"rejected_seq"`
	RejectReason       string        `json:"reject_reason"`
	AcceptedRanges     []model.Range `json:"accepted_ranges"`
	AckThrough         int64         `json:"ack_through"`
	RateLimitActive    bool          `json:"rate_limit_active"`
	RateLimitPerSec    float64       `json:"rate_limit_per_sec"`
	BackendBacklog     int           `json:"backend_backlog"`
	BackendBacklogPeak int           `json:"backend_backlog_peak"`
	SendWindowUsed     int           `json:"send_window_used"`
	SendWindowTotal    int           `json:"send_window_total"`
	ProcessingMS       float64       `json:"processing_ms"`
	RetryAfterMS       int64         `json:"retry_after_ms"`
	Feedback           *feedbackView `json:"feedback"`
}

type sessionDetailReply struct {
	Session    *model.Session        `json:"session"`
	Migrations []model.Migration     `json:"migrations"`
	Feedback   []model.FeedbackEvent `json:"feedback"`
	Rejections []model.Rejection     `json:"rejections"`
}

type deviceClient struct {
	ingress    string
	baseURL    string
	httpClient *http.Client
	closeFn    func()
	mu         sync.Mutex
	protocol   string
}

func newDeviceClient(ingress, tcpAddr, quicAddr string) (*deviceClient, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true, // local self-signed gateway certificate
		MinVersion:         tls.VersionTLS12,
	}
	switch ingress {
	case model.IngressQUIC:
		tlsCfg.MinVersion = tls.VersionTLS13
		rt := &http3.RoundTripper{
			TLSClientConfig: tlsCfg,
			QuicConfig: &quic.Config{
				MaxIdleTimeout:        30 * time.Second,
				KeepAlivePeriod:       5 * time.Second,
				MaxIncomingStreams:    64,
				MaxIncomingUniStreams: 16,
			},
		}
		return &deviceClient{
			ingress: ingress, baseURL: "https://" + quicAddr,
			httpClient: &http.Client{Transport: rt, Timeout: 5 * time.Second},
			closeFn:    func() { _ = rt.Close() },
		}, nil
	case model.IngressTCP:
		tr := &http.Transport{
			TLSClientConfig:     tlsCfg,
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        2,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     60 * time.Second,
		}
		_ = http2.ConfigureTransport(tr)
		return &deviceClient{
			ingress: ingress, baseURL: "https://" + tcpAddr,
			httpClient: &http.Client{Transport: tr, Timeout: 5 * time.Second},
			closeFn:    func() { tr.CloseIdleConnections() },
		}, nil
	default:
		return nil, fmt.Errorf("unknown ingress %q", ingress)
	}
}

func (c *deviceClient) close() {
	if c.closeFn != nil {
		c.closeFn()
	}
}

func (c *deviceClient) doJSON(ctx context.Context, method, path string, body any, peerAddr string) (int, []byte, time.Duration, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if peerAddr != "" {
		req.Header.Set("X-Edge-Peer-Address", peerAddr)
	}
	start := time.Now()
	resp, err := c.httpClient.Do(req)
	latency := time.Since(start)
	if err != nil {
		return 0, nil, latency, err
	}
	defer resp.Body.Close()
	c.mu.Lock()
	c.protocol = resp.Proto
	c.mu.Unlock()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return resp.StatusCode, nil, latency, err
	}
	return resp.StatusCode, raw, latency, nil
}

func (c *deviceClient) negotiatedProtocol() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.protocol
}

func decodeReply(raw []byte) (fragmentReply, error) {
	var out fragmentReply
	err := json.Unmarshal(raw, &out)
	return out, err
}
