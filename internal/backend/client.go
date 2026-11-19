package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"edgegateway/internal/model"
)

type ForwardRequest struct {
	Schema         string `json:"schema"`
	SessionID      string `json:"session_id"`
	DeviceID       string `json:"device_id"`
	Ingress        string `json:"ingress"`
	Seq            int64  `json:"seq"`
	Size           int    `json:"size"`
	DataBase64     string `json:"data_base64"`
	ChecksumSHA256 string `json:"checksum_sha256"`
}

type ForwardResponse struct {
	Accepted       bool          `json:"accepted"`
	Duplicate      bool          `json:"duplicate"`
	Rejected       bool          `json:"rejected"`
	Reason         string        `json:"reason"`
	Code           string        `json:"code"`
	RejectedSeq    *int64        `json:"rejected_seq"`
	AcceptedRanges []model.Range `json:"accepted_ranges"`
	AckThrough     int64         `json:"ack_through"`
}

type Client struct {
	base string
	hc   *http.Client
}

func New(baseURL string, timeout time.Duration) *Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   64,
		MaxConnsPerHost:       64,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: timeout,
		ForceAttemptHTTP2:     false,
	}
	return &Client{
		base: baseURL,
		hc: &http.Client{
			Transport: transport,
			Timeout:   timeout,
		},
	}
}

func (c *Client) Forward(ctx context.Context, req ForwardRequest) (ForwardResponse, error) {
	var out ForwardResponse
	b, err := json.Marshal(req)
	if err != nil {
		return out, fmt.Errorf("encode backend request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/fragments", bytes.NewReader(b))
	if err != nil {
		return out, fmt.Errorf("create backend request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, fmt.Errorf("read backend response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("backend status %d: %s", resp.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("decode backend response: %w", err)
	}
	if out.RejectedSeq != nil && *out.RejectedSeq != req.Seq {
		return out, errors.New("backend rejected unexpected fragment")
	}
	return out, nil
}

func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("backend health status %d", resp.StatusCode)
	}
	return nil
}
