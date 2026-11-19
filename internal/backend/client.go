package backend

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Client struct {
	endpoint *url.URL
	access   string
	secret   string
	region   string
	http     *http.Client
}

type BackendError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *BackendError) Error() string {
	return fmt.Sprintf("backend %d %s: %s", e.StatusCode, e.Code, e.Message)
}

func New(endpoint, access, secret, region string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("backend endpoint must use http or https")
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		MaxConnsPerHost:       64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &Client{
		endpoint: u,
		access:   access,
		secret:   secret,
		region:   region,
		http:     &http.Client{Transport: transport},
	}, nil
}

func (c *Client) endpointURL(bucket, key string) *url.URL {
	u := *c.endpoint
	path := strings.TrimRight(c.endpoint.Path, "/")
	if bucket != "" {
		path += "/" + escapePathSegment(bucket)
	}
	if key != "" {
		for _, segment := range strings.Split(key, "/") {
			path += "/" + escapePathSegment(segment)
		}
	}
	if path == "" {
		path = "/"
	}
	u.Path = ""
	u.RawPath = ""
	decoded, _ := url.PathUnescape(path)
	u.Path = decoded
	u.RawPath = path
	return &u
}

func escapePathSegment(s string) string {
	e := url.PathEscape(s)
	e = strings.ReplaceAll(e, "+", "%20")
	return e
}

func (c *Client) do(ctx context.Context, method, bucket, key string, body []byte, headers http.Header) (*http.Response, error) {
	u := c.endpointURL(bucket, key)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		req.Header = headers.Clone()
	}
	payloadHash := sha256Hex(body)
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Host", u.Host)
	req.Host = u.Host
	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	if value := req.Header.Get("Content-Type"); value != "" {
		signedHeaders = append(signedHeaders, "content-type")
	}
	sort.Strings(signedHeaders)
	canonicalHeaders := canonicalHeaders(req, signedHeaders)
	canonicalRequest := strings.Join([]string{
		method,
		u.EscapedPath(),
		"",
		canonicalHeaders,
		strings.Join(signedHeaders, ";"),
		payloadHash,
	}, "\n")
	scope := strings.Join([]string{dateStamp, c.region, "s3", "aws4_request"}, "/")
	hash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(hash[:])}, "\n")
	keyBytes := backendSigningKey(c.secret, dateStamp, c.region, "s3")
	signature := hex.EncodeToString(backendHMAC(keyBytes, []byte(stringToSign)))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.access, scope, strings.Join(signedHeaders, ";"), signature))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		be := decodeError(resp)
		return nil, be
	}
	return resp, nil
}

func canonicalHeaders(req *http.Request, names []string) string {
	var b strings.Builder
	for _, name := range names {
		var values []string
		if name == "host" {
			values = []string{req.Host}
		} else {
			values = req.Header.Values(name)
		}
		for i := range values {
			values[i] = strings.Join(strings.Fields(strings.TrimSpace(values[i])), " ")
		}
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(strings.Join(values, ","))
		b.WriteByte('\n')
	}
	return b.String()
}

func backendSigningKey(secret, date, region, service string) []byte {
	kDate := backendHMAC([]byte("AWS4"+secret), []byte(date))
	kRegion := backendHMAC(kDate, []byte(region))
	kService := backendHMAC(kRegion, []byte(service))
	return backendHMAC(kService, []byte("aws4_request"))
}

func backendHMAC(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(data)
	return h.Sum(nil)
}

func sha256Hex(body []byte) string {
	if body == nil {
		body = []byte{}
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

type backendErrorXML struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func decodeError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e backendErrorXML
	_ = xml.Unmarshal(b, &e)
	if e.Code == "" {
		e.Code = http.StatusText(resp.StatusCode)
	}
	return &BackendError{StatusCode: resp.StatusCode, Code: e.Code, Message: e.Message}
}

func IsNotFound(err error) bool {
	var be *BackendError
	if ok := asBackendError(err, &be); ok {
		return be.StatusCode == http.StatusNotFound || be.Code == "NoSuchKey" || be.Code == "NoSuchBucket"
	}
	return false
}

func asBackendError(err error, target **BackendError) bool {
	for err != nil {
		if be, ok := err.(*BackendError); ok {
			*target = be
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return false
}

func (c *Client) CreateBucket(ctx context.Context, bucket string) error {
	resp, err := c.do(ctx, http.MethodPut, bucket, "", nil, nil)
	if err != nil {
		var be *BackendError
		if asBackendError(err, &be) && (be.Code == "BucketAlreadyOwnedByYou" || be.Code == "BucketAlreadyExists") {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) DeleteBucket(ctx context.Context, bucket string) error {
	resp, err := c.do(ctx, http.MethodDelete, bucket, "", nil, nil)
	if err != nil && !IsNotFound(err) {
		return err
	}
	if resp != nil {
		resp.Body.Close()
	}
	return nil
}

func (c *Client) PutObject(ctx context.Context, bucket, key string, body []byte, contentType string) (string, error) {
	headers := make(http.Header)
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	resp, err := c.do(ctx, http.MethodPut, bucket, key, body, headers)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	return etag, nil
}

func (c *Client) OpenObject(ctx context.Context, bucket, key string) (*http.Response, error) {
	return c.do(ctx, http.MethodGet, bucket, key, nil, nil)
}

func (c *Client) GetObject(ctx context.Context, bucket, key string) ([]byte, error) {
	resp, err := c.OpenObject(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *Client) HeadObject(ctx context.Context, bucket, key string) (int64, string, error) {
	resp, err := c.do(ctx, http.MethodHead, bucket, key, nil, nil)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	return resp.ContentLength, strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

func (c *Client) DeleteObject(ctx context.Context, bucket, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, bucket, key, nil, nil)
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}
