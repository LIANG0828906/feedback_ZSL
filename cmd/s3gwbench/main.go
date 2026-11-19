package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"s3gw/internal/sig"
)

type sample struct {
	key      string
	duration time.Duration
	status   int
}

type bench struct {
	endpoint string
	bucket   string
	access   string
	secret   string
	region   string
	client   *http.Client
}

func main() {
	logger := log.New(io.Discard, "", 0)
	endpoint := flag.String("endpoint", "http://127.0.0.1:9100", "gateway endpoint")
	bucket := flag.String("bucket", "s3gw-bench", "benchmark bucket")
	access := flag.String("access-key", "gateway", "access key")
	secret := flag.String("secret-key", "gateway-secret", "secret key")
	region := flag.String("region", "us-east-1", "signing region")
	count := flag.Int("requests", 200, "small object request count")
	size := flag.Int("size", 256*1024, "small object size")
	concurrency := flag.Int("concurrency", 8, "concurrent workers")
	flag.Parse()
	if *size <= 0 || *count <= 0 || *concurrency <= 0 {
		logger.Fatal("requests, size and concurrency must be positive")
	}
	b := &bench{
		endpoint: strings.TrimRight(*endpoint, "/"),
		bucket:   *bucket, access: *access, secret: *secret, region: *region,
		client: &http.Client{
			Timeout: 2 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns: 64, MaxIdleConnsPerHost: 64, MaxConnsPerHost: 64,
			},
		},
	}
	if status, _, err := b.do(http.MethodPut, "/"+url.PathEscape(b.bucket), nil, nil, "v2"); err != nil || (status != 200 && status != 409) {
		log.Fatalf("create bucket status=%d err=%v", status, err)
	}
	run := randomHex(6)
	keys := make([]string, *count)
	for i := range keys {
		keys[i] = fmt.Sprintf("bench/%s/object-%04d.bin", run, i)
	}
	payload := bytes.Repeat([]byte("x"), *size)
	puts := parallelSamples(keys, *concurrency, func(key string) sample {
		started := time.Now()
		status, _, err := b.do(http.MethodPut, b.objectPath(key), payload, map[string]string{"Content-Type": "application/octet-stream"}, "v2")
		if err != nil {
			status = 0
		}
		return sample{key: key, duration: time.Since(started), status: status}
	})
	gets := parallelSamples(keys, *concurrency, func(key string) sample {
		started := time.Now()
		status, _, err := b.do(http.MethodGet, b.objectPath(key), nil, nil, "v2")
		if err != nil {
			status = 0
		}
		return sample{key: key, duration: time.Since(started), status: status}
	})
	multipartStarted := time.Now()
	multiStatus, multiDuration, multiVersion, err := b.multipartFourMiB(run)
	if err != nil {
		log.Fatalf("multipart: %v", err)
	}
	firstGetStarted := time.Now()
	getStatus, getHeaders, getErr := b.do(http.MethodGet, b.objectPath("bench/"+run+"/multipart-4m.bin"), nil, nil, "v4")
	firstGetDuration := time.Since(firstGetStarted)
	if getErr != nil || getStatus != 200 {
		log.Fatalf("multipart first GET status=%d err=%v", getStatus, getErr)
	}
	out := map[string]any{
		"put": map[string]any{"p50_ms": millis(p50(puts)), "p95_ms": millis(p95(puts)), "p99_ms": millis(p99(puts)), "errors": failures(puts)},
		"get": map[string]any{"p50_ms": millis(p50(gets)), "p95_ms": millis(p95(gets)), "p99_ms": millis(p99(gets)), "errors": failures(gets)},
		"multipart": map[string]any{
			"status": multiStatus, "complete_ms": millis(multiDuration), "first_get_ms": millis(firstGetDuration),
			"first_get_status": getStatus, "version_id": multiVersion,
			"first_get_bytes": len(getHeaders.body), "total_ms": millis(time.Since(multipartStarted)),
		},
	}
	enc := json.NewEncoder(log.Writer())
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func parallelSamples(keys []string, concurrency int, fn func(string) sample) []sample {
	jobs := make(chan string)
	results := make(chan sample, len(keys))
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for key := range jobs {
				results <- fn(key)
			}
		}()
	}
	for _, key := range keys {
		jobs <- key
	}
	close(jobs)
	wg.Wait()
	close(results)
	out := make([]sample, 0, len(keys))
	for result := range results {
		out = append(out, result)
	}
	return out
}

func p50(samples []sample) time.Duration { return percentile(samples, 0.50) }
func p95(samples []sample) time.Duration { return percentile(samples, 0.95) }
func p99(samples []sample) time.Duration { return percentile(samples, 0.99) }

func percentile(samples []sample, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	durations := make([]time.Duration, len(samples))
	for i := range samples {
		durations[i] = samples[i].duration
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	idx := int(float64(len(durations)-1) * p)
	return durations[idx]
}

func failures(samples []sample) int {
	n := 0
	for _, s := range samples {
		if s.status != 200 {
			n++
		}
	}
	return n
}

func millis(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

func (b *bench) multipartFourMiB(run string) (int, time.Duration, string, error) {
	key := "bench/" + run + "/multipart-4m.bin"
	status, headers, err := b.do(http.MethodPost, b.objectPath(key)+"?uploads", nil,
		map[string]string{"Content-Type": "application/octet-stream", "x-amz-storage-class": "STANDARD"}, "v4")
	if err != nil || status != 200 {
		return status, 0, "", fmt.Errorf("initiate status=%d err=%v body=%s", status, err, headers.body)
	}
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(headers.body, &init); err != nil {
		return status, 0, "", err
	}
	var parts []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	}
	for i := 1; i <= 4; i++ {
		data := bytes.Repeat([]byte{byte('A' + i)}, 1024*1024)
		partStatus, partHeaders, err := b.do(http.MethodPut,
			b.objectPath(key)+"?partNumber="+strconv.Itoa(i)+"&uploadId="+url.QueryEscape(init.UploadID),
			data, nil, "v4")
		if err != nil || partStatus != 200 {
			return partStatus, 0, "", fmt.Errorf("part %d status=%d err=%v", i, partStatus, err)
		}
		parts = append(parts, struct {
			PartNumber int    `xml:"PartNumber"`
			ETag       string `xml:"ETag"`
		}{PartNumber: i, ETag: partHeaders.Get("ETag")})
	}
	completePayload, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []struct {
			PartNumber int    `xml:"PartNumber"`
			ETag       string `xml:"ETag"`
		} `xml:"Part"`
	}{Parts: parts})
	started := time.Now()
	completeStatus, completeHeaders, err := b.do(http.MethodPost,
		b.objectPath(key)+"?uploadId="+url.QueryEscape(init.UploadID), completePayload, nil, "v4")
	duration := time.Since(started)
	if err != nil || completeStatus != 200 {
		return completeStatus, duration, "", fmt.Errorf("complete status=%d err=%v body=%s", completeStatus, err, completeHeaders.body)
	}
	return completeStatus, duration, completeHeaders.Get("x-amz-version-id"), nil
}

type response struct {
	http.Header
	body []byte
}

func (b *bench) do(method, path string, body []byte, headers map[string]string, version string) (int, response, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, b.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return 0, response{}, err
	}
	req.Host = req.URL.Host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	switch version {
	case "v2":
		sig.SignV2Request(req, b.access, b.secret, time.Now().UTC())
	case "v4":
		sig.SignV4Request(req, b.access, b.secret, b.region, body, time.Now().UTC())
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, response{}, err
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, response{Header: resp.Header.Clone(), body: respBody}, readErr
}

func (b *bench) objectPath(key string) string {
	segments := strings.Split(key, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	return "/" + url.PathEscape(b.bucket) + "/" + strings.Join(segments, "/")
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
