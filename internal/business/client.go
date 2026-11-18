package business

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"configcenter/internal/model"
)

type ClientOptions struct {
	BaseURL       string
	Namespace     string
	ClientID      string
	PollTimeout   time.Duration
	ApplyDelay    time.Duration
	MaxPolls      int
	Logger        *log.Logger
	ReportTimeout time.Duration
}

type Client struct {
	options ClientOptions
	state   *State
	client  *http.Client
}

func NewClient(options ClientOptions) *Client {
	if options.PollTimeout <= 0 {
		options.PollTimeout = 10 * time.Second
	}
	if options.ReportTimeout <= 0 {
		options.ReportTimeout = 2 * time.Second
	}
	if options.Logger == nil {
		options.Logger = log.Default()
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = false
	return &Client{
		options: options,
		state:   NewState(),
		client: &http.Client{
			Transport: transport,
			Timeout:   options.PollTimeout + 5*time.Second,
		},
	}
}

func (c *Client) Run(ctx context.Context) error {
	baseURL := strings.TrimRight(c.options.BaseURL, "/")
	c.options.Logger.Printf(
		"READY client=%s namespace=%s base=%s",
		c.options.ClientID,
		c.options.Namespace,
		baseURL,
	)

	polls := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if c.options.MaxPolls > 0 && polls >= c.options.MaxPolls {
			return nil
		}
		polls++

		response, err := c.watch(ctx, baseURL, c.state.Version())
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.options.Logger.Printf("WATCH_ERROR client=%s error=%q", c.options.ClientID, err)
			if !sleepContext(ctx, 500*time.Millisecond) {
				return nil
			}
			continue
		}

		if c.options.ApplyDelay > 0 {
			if !sleepContext(ctx, c.options.ApplyDelay) {
				return nil
			}
		}
		result, err := c.state.Apply(response)
		if err != nil {
			return fmt.Errorf("client %s: %w", c.options.ClientID, err)
		}
		version, _, history, stale := c.state.Snapshot()
		if result.Stale {
			c.options.Logger.Printf(
				"STALE client=%s received_version=%d local_version=%d",
				c.options.ClientID,
				response.Version,
				version,
			)
		}
		if result.Applied {
			keys := append([]string(nil), result.Keys...)
			sort.Strings(keys)
			c.options.Logger.Printf(
				"APPLIED client=%s version=%d base=%d changes=%d keys=%s",
				c.options.ClientID,
				version,
				response.BaseVersion,
				len(keys),
				strings.Join(keys, ","),
			)
		}
		if err := c.report(ctx, baseURL, response.Version, version, history, stale); err != nil && ctx.Err() == nil {
			c.options.Logger.Printf("REPORT_ERROR client=%s error=%q", c.options.ClientID, err)
		}
	}
}

func (c *Client) watch(
	ctx context.Context,
	baseURL string,
	knownVersion int64,
) (model.WatchResponse, error) {
	query := url.Values{}
	query.Set("version", strconv.FormatInt(knownVersion, 10))
	query.Set("timeout", c.options.PollTimeout.String())
	query.Set("client_id", c.options.ClientID)
	requestURL := baseURL + "/v1/namespaces/" + url.PathEscape(c.options.Namespace) + "/watch?" + query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return model.WatchResponse{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Connection", "keep-alive")

	response, err := c.client.Do(request)
	if err != nil {
		return model.WatchResponse{}, err
	}
	defer response.Body.Close()
	if response.ProtoMajor != 1 || response.ProtoMinor != 1 {
		return model.WatchResponse{}, fmt.Errorf("long-poll used %s; HTTP/1.1 is required", response.Proto)
	}
	if response.StatusCode != http.StatusOK {
		var apiError model.ErrorResponse
		_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&apiError)
		if apiError.Code == "" {
			apiError.Code = "E_HTTP"
			apiError.Message = response.Status
		}
		return model.WatchResponse{}, fmt.Errorf("%s: %s", apiError.Code, apiError.Message)
	}

	var watchResponse model.WatchResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&watchResponse); err != nil {
		return model.WatchResponse{}, err
	}
	return watchResponse, nil
}

func (c *Client) report(
	ctx context.Context,
	baseURL string,
	lastResponseVersion int64,
	knownVersion int64,
	history []int64,
	stale uint64,
) error {
	report := model.ClientReport{
		ClientID:            c.options.ClientID,
		Namespace:           c.options.Namespace,
		KnownVersion:        knownVersion,
		AppliedVersion:      knownVersion,
		LastResponseVersion: lastResponseVersion,
		StaleResponses:      stale,
		AppliedHistory:      history,
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return err
	}
	reportCtx, cancel := context.WithTimeout(ctx, c.options.ReportTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(
		reportCtx,
		http.MethodPost,
		baseURL+"/v1/clients/report",
		bytes.NewReader(payload),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return fmt.Errorf("report returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
