package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"s3gw/internal/backend"
	"s3gw/internal/sig"
	"s3gw/internal/store"
)

type objectFixture struct {
	Key        string   `json:"key"`
	Kind       string   `json:"kind"`
	Size       int64    `json:"size"`
	SHA256     string   `json:"sha256"`
	File       string   `json:"file"`
	Parts      int      `json:"parts,omitempty"`
	VersionIDs []string `json:"version_ids,omitempty"`
}

type sessionPartFixture struct {
	PartNumber int    `json:"part_number"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
}

type sessionFixture struct {
	UploadID   string               `json:"upload_id"`
	Key        string               `json:"key"`
	AgeSeconds int64                `json:"age_seconds"`
	Parts      []sessionPartFixture `json:"parts"`
}

type manifest struct {
	Seed               int64            `json:"seed"`
	Bucket             string           `json:"bucket"`
	SmallObjects       int              `json:"small_objects"`
	MultipartObjects   int              `json:"multipart_objects"`
	TotalObjectBytes   int64            `json:"total_object_bytes"`
	IncompleteSessions int              `json:"incomplete_sessions"`
	IncompleteBytes    int64            `json:"incomplete_bytes"`
	V2Requests         int              `json:"v2_requests"`
	V4Requests         int              `json:"v4_requests"`
	Objects            []objectFixture  `json:"objects"`
	Sessions           []sessionFixture `json:"sessions"`
}

type rng struct{ state uint64 }

func newRNG(seed int64) *rng {
	s := uint64(seed)
	if s == 0 {
		s = 0x9e3779b97f4a7c15
	}
	return &rng{state: s}
}

func (r *rng) next() uint64 {
	x := r.state
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	r.state = x
	return x
}

func (r *rng) inRange(min, max int64) int64 {
	if max <= min {
		return min
	}
	return min + int64(r.next()%uint64(max-min+1))
}

func (r *rng) fill(b []byte) {
	for i := 0; i < len(b); {
		v := r.next()
		for j := 0; j < 8 && i < len(b); j++ {
			b[i] = byte(v)
			v >>= 8
			i++
		}
	}
}

type loader struct {
	endpoint string
	bucket   string
	access   string
	secret   string
	region   string
	client   *http.Client
	v2Count  int
	v4Count  int
}

func main() {
	logger := log.New(os.Stdout, "samplegen ", log.LstdFlags)
	out := flag.String("out", "samples", "fixture output directory")
	seed := flag.Int64("seed", 301, "deterministic seed")
	bucket := flag.String("bucket", "s3gw-samples", "bucket name")
	endpoint := flag.String("endpoint", "", "gateway endpoint; when set, objects are loaded over signed HTTP")
	dataFile := flag.String("data", "", "gateway SQLite path; when set, 80 aged sessions are seeded directly")
	backendURL := flag.String("backend", "http://127.0.0.1:9000", "MinIO endpoint for direct session seeding")
	backendAccess := flag.String("backend-access-key", "minioadmin", "MinIO access key")
	backendSecret := flag.String("backend-secret-key", "minioadmin", "MinIO secret key")
	accessKey := flag.String("access-key", "gateway", "gateway access key")
	secretKey := flag.String("secret-key", "gateway-secret", "gateway secret key")
	region := flag.String("region", "us-east-1", "signing region")
	sessionAge := flag.Duration("session-age", 2*time.Hour, "age assigned to incomplete sessions")
	verify := flag.Bool("verify", false, "verify every loaded version, list-versions output, status and metrics")
	verifyOnly := flag.Bool("verify-only", false, "read manifest from -out and verify existing data without loading")
	flag.Parse()

	m := manifest{}
	if *verifyOnly {
		b, err := os.ReadFile(filepath.Join(*out, "manifest.json"))
		if err != nil {
			logger.Fatal(err)
		}
		if err := json.Unmarshal(b, &m); err != nil {
			logger.Fatal(err)
		}
		*bucket = m.Bucket
	} else {
		if err := os.MkdirAll(*out, 0o755); err != nil {
			logger.Fatal(err)
		}
		r := newRNG(*seed)
		m = manifest{Seed: *seed, Bucket: *bucket, SmallObjects: 90, MultipartObjects: 30, IncompleteSessions: 80}
		if err := generateFixtures(*out, r, sessionAge, &m); err != nil {
			logger.Fatal(err)
		}
	}

	if *endpoint != "" && !*verifyOnly {
		l := &loader{
			endpoint: strings.TrimRight(*endpoint, "/"), bucket: *bucket,
			access: *accessKey, secret: *secretKey, region: *region,
			client: &http.Client{Timeout: 2 * time.Minute},
		}
		if err := l.loadObjects(*out, &m); err != nil {
			logger.Fatal(err)
		}
		m.V2Requests = l.v2Count
		m.V4Requests = l.v4Count
	} else if !*verifyOnly {
		m.V2Requests = 120
		m.V4Requests = 0
	}
	if *dataFile != "" && !*verifyOnly {
		if err := seedSessions(*dataFile, *backendURL, *backendAccess, *backendSecret, *bucket, *sessionAge, &m); err != nil {
			logger.Fatal(err)
		}
	}
	if *verify || *verifyOnly {
		if *endpoint == "" {
			logger.Fatal("-verify requires -endpoint")
		}
		summary, err := verifyLoaded(&loader{
			endpoint: strings.TrimRight(*endpoint, "/"), bucket: *bucket,
			access: *accessKey, secret: *secretKey, region: *region,
			client: &http.Client{Timeout: 2 * time.Minute},
		}, *out, &m)
		if err != nil {
			logger.Fatal(err)
		}
		b, _ := json.MarshalIndent(summary, "", "  ")
		logger.Printf("verification=%s", b)
	}
	manifestBytes, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "manifest.json"), append(manifestBytes, '\n'), 0o644); err != nil {
		logger.Fatal(err)
	}
	sessionBytes, _ := json.MarshalIndent(m.Sessions, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "sessions.json"), append(sessionBytes, '\n'), 0o644); err != nil {
		logger.Fatal(err)
	}
	logger.Printf("fixtures=%d objects=%d incomplete_sessions=%d object_bytes=%d incomplete_bytes=%d loaded_endpoint=%q seeded_sqlite=%q",
		len(m.Objects), len(m.Objects), len(m.Sessions), m.TotalObjectBytes, m.IncompleteBytes, *endpoint, *dataFile)
}

func verifyLoaded(l *loader, out string, m *manifest) (map[string]any, error) {
	versionTotal := 0
	for _, fixture := range m.Objects {
		if len(fixture.VersionIDs) == 0 {
			return nil, fmt.Errorf("%s has no version ids", fixture.Key)
		}
		firstID := fixture.VersionIDs[0]
		status, headers, err := l.do(http.MethodGet, l.objectPath(fixture.Key)+"?versionId="+url.QueryEscape(firstID), nil, nil, true)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("GET first version %s/%s status=%d body=%s", fixture.Key, firstID, status, headers.body)
		}
		if int64(len(headers.body)) != fixture.Size || sha256Hex(headers.body) != fixture.SHA256 {
			return nil, fmt.Errorf("first version content mismatch %s/%s", fixture.Key, firstID)
		}
		status, headers, err = l.do(http.MethodGet, l.objectPath(fixture.Key), nil, nil, false)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("GET current %s status=%d body=%s", fixture.Key, status, headers.body)
		}
		lastID := fixture.VersionIDs[len(fixture.VersionIDs)-1]
		if headers.Get("x-amz-version-id") != lastID {
			return nil, fmt.Errorf("current version mismatch %s: got=%s want=%s", fixture.Key, headers.Get("x-amz-version-id"), lastID)
		}
		if len(fixture.VersionIDs) == 1 && sha256Hex(headers.body) != fixture.SHA256 {
			return nil, fmt.Errorf("current content mismatch %s", fixture.Key)
		}
		if len(fixture.VersionIDs) > 1 && !bytes.HasPrefix(headers.body, []byte("v4-overwrite")) {
			return nil, fmt.Errorf("V4 overwrite content mismatch %s", fixture.Key)
		}
		versionTotal += len(fixture.VersionIDs)
	}
	status, headers, err := l.do(http.MethodGet, "/"+l.bucket+"?versions&max-keys=1000", nil, nil, false)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("list versions status=%d body=%s", status, headers.body)
	}
	listed := bytes.Count(headers.body, []byte("<Version>")) + bytes.Count(headers.body, []byte("<DeleteMarker>"))
	if listed != versionTotal {
		return nil, fmt.Errorf("list versions count=%d want=%d", listed, versionTotal)
	}
	resp, err := l.client.Get(l.endpoint + "/_gateway/status?bucket=" + url.QueryEscape(l.bucket))
	if err != nil {
		return nil, err
	}
	statusBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var statusJSON map[string]any
	if err := json.Unmarshal(statusBody, &statusJSON); err != nil {
		return nil, err
	}
	metricsResp, err := l.client.Get(l.endpoint + "/metrics")
	if err != nil {
		return nil, err
	}
	metricsBody, err := io.ReadAll(metricsResp.Body)
	metricsResp.Body.Close()
	if err != nil {
		return nil, err
	}
	for _, metric := range []string{
		"s3gw_signature_requests_total", "s3gw_versions_current", "s3gw_versions_historical",
		"s3gw_multipart_sessions_incomplete", "s3gw_multipart_bytes_incomplete",
		"s3gw_reaper_bytes_reclaimed_total", "s3gw_reaper_failures_total",
	} {
		if !bytes.Contains(metricsBody, []byte(metric)) {
			return nil, fmt.Errorf("metric %s missing", metric)
		}
	}
	return map[string]any{
		"objects": len(m.Objects), "sessions": len(m.Sessions),
		"versions_verified": versionTotal, "listed_versions": listed,
		"status": statusJSON,
	}, nil
}

func generateFixtures(out string, r *rng, sessionAge *time.Duration, m *manifest) error {
	if err := os.MkdirAll(filepath.Join(out, "objects"), 0o755); err != nil {
		return err
	}
	for i := 0; i < 90; i++ {
		size := r.inRange(1024, 256*1024)
		data := make([]byte, size)
		r.fill(data)
		key := fmt.Sprintf("small/object-%03d.bin", i)
		file := filepath.Join("objects", fmt.Sprintf("small-%03d.bin", i))
		if err := os.WriteFile(filepath.Join(out, file), data, 0o644); err != nil {
			return err
		}
		m.Objects = append(m.Objects, objectFixture{
			Key: key, Kind: "put", Size: size, SHA256: sha256Hex(data), File: file,
		})
		m.TotalObjectBytes += size
	}
	for i := 0; i < 30; i++ {
		size := r.inRange(1<<20, 4<<20)
		data := make([]byte, size)
		r.fill(data)
		parts := int((size + (512 << 10) - 1) / (512 << 10))
		if parts < 1 {
			parts = 1
		}
		key := fmt.Sprintf("large/multipart-%03d.bin", i)
		file := filepath.Join("objects", fmt.Sprintf("large-%03d.bin", i))
		if err := os.WriteFile(filepath.Join(out, file), data, 0o644); err != nil {
			return err
		}
		m.Objects = append(m.Objects, objectFixture{
			Key: key, Kind: "multipart", Size: size, SHA256: sha256Hex(data), File: file, Parts: parts,
		})
		m.TotalObjectBytes += size
	}
	for i := 0; i < 80; i++ {
		uploadID := sha256Hex([]byte(fmt.Sprintf("session-%d-%d", m.Seed, i)))[:32]
		partCount := 1 + int(r.next()%3)
		var parts []sessionPartFixture
		var total int64
		for p := 1; p <= partCount; p++ {
			size := r.inRange(32*1024, 128*1024)
			data := deterministicBytes(fmt.Sprintf("session-%d-%d-%d", m.Seed, i, p), size)
			parts = append(parts, sessionPartFixture{PartNumber: p, Size: size, SHA256: sha256Hex(data)})
			total += size
		}
		m.Sessions = append(m.Sessions, sessionFixture{
			UploadID: uploadID, Key: fmt.Sprintf("unfinished/session-%03d.bin", i),
			AgeSeconds: int64(*sessionAge / time.Second), Parts: parts,
		})
		m.IncompleteBytes += total
	}
	requests := struct {
		V2 []string `json:"v2"`
		V4 []string `json:"v4"`
	}{
		V2: []string{"90 V2 PutObject + 30 V2 multipart initiate/uploadPart/complete sequences"},
		V4: []string{"12 V4 PutObject overwrites with x-amz-storage-class and x-amz-acl"},
	}
	b, _ := json.MarshalIndent(requests, "", "  ")
	return os.WriteFile(filepath.Join(out, "requests.json"), append(b, '\n'), 0o644)
}

func (l *loader) loadObjects(out string, m *manifest) error {
	if err := l.createBucket(); err != nil {
		return err
	}
	for i := range m.Objects {
		fixture := &m.Objects[i]
		data, err := os.ReadFile(filepath.Join(out, fixture.File))
		if err != nil {
			return err
		}
		if fixture.Kind == "multipart" {
			id, err := l.putMultipart(fixture, data)
			if err != nil {
				return fmt.Errorf("%s: %w", fixture.Key, err)
			}
			fixture.VersionIDs = append(fixture.VersionIDs, id)
		} else {
			id, err := l.putObject(fixture.Key, data, "", false)
			if err != nil {
				return fmt.Errorf("%s: %w", fixture.Key, err)
			}
			fixture.VersionIDs = append(fixture.VersionIDs, id)
		}
	}
	for i := 0; i < 12; i++ {
		fixture := &m.Objects[i*7]
		data := []byte(fmt.Sprintf("v4-overwrite seed=%d index=%d", m.Seed, i))
		id, err := l.putObject(fixture.Key, data, "STANDARD", true)
		if err != nil {
			return err
		}
		fixture.VersionIDs = append(fixture.VersionIDs, id)
	}
	return nil
}

func (l *loader) createBucket() error {
	status, _, err := l.do(http.MethodPut, "/"+l.bucket, nil, nil, false)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusConflict {
		return fmt.Errorf("create bucket status %d", status)
	}
	return nil
}

func (l *loader) putObject(key string, data []byte, storageClass string, v4 bool) (string, error) {
	headers := map[string]string{"Content-Type": "application/octet-stream"}
	if storageClass != "" {
		headers["x-amz-storage-class"] = storageClass
		headers["x-amz-acl"] = "private"
	}
	status, respHeaders, err := l.do(http.MethodPut, l.objectPath(key), data, headers, v4)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("put status %d", status)
	}
	return respHeaders.Get("x-amz-version-id"), nil
}

func (l *loader) putMultipart(fixture *objectFixture, data []byte) (string, error) {
	status, headers, err := l.do(http.MethodPost, l.objectPath(fixture.Key)+"?uploads", nil,
		map[string]string{"Content-Type": "application/octet-stream", "x-amz-storage-class": "STANDARD", "x-amz-acl": "private"}, true)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("initiate status %d", status)
	}
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	_ = xml.Unmarshal(headers.body, &init)
	if init.UploadID == "" {
		return "", fmt.Errorf("missing upload id")
	}
	partSize := (len(data) + fixture.Parts - 1) / fixture.Parts
	var completed []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	}
	for p := 0; p < fixture.Parts; p++ {
		start := p * partSize
		end := start + partSize
		if end > len(data) {
			end = len(data)
		}
		status, h, err := l.do(http.MethodPut,
			l.objectPath(fixture.Key)+"?partNumber="+strconv.Itoa(p+1)+"&uploadId="+init.UploadID,
			data[start:end], nil, true)
		if err != nil {
			return "", err
		}
		if status != http.StatusOK {
			return "", fmt.Errorf("upload part status %d", status)
		}
		completed = append(completed, struct {
			PartNumber int    `xml:"PartNumber"`
			ETag       string `xml:"ETag"`
		}{PartNumber: p + 1, ETag: h.Get("ETag")})
	}
	payload, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []struct {
			PartNumber int    `xml:"PartNumber"`
			ETag       string `xml:"ETag"`
		} `xml:"Part"`
	}{Parts: completed})
	status, headers, err = l.do(http.MethodPost, l.objectPath(fixture.Key)+"?uploadId="+init.UploadID, payload, nil, false)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("complete status %d body=%s", status, headers.body)
	}
	return headers.Get("x-amz-version-id"), nil
}

type responseHeaders struct {
	http.Header
	body []byte
}

func (l *loader) do(method, path string, body []byte, headers map[string]string, v4 bool) (int, responseHeaders, error) {
	req, err := http.NewRequest(method, l.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return 0, responseHeaders{}, err
	}
	req.Host = req.URL.Host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if len(body) > 0 {
		sum := md5.Sum(body)
		req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	}
	if v4 {
		sig.SignV4Request(req, l.access, l.secret, l.region, body, time.Now().UTC())
		l.v4Count++
	} else {
		sig.SignV2Request(req, l.access, l.secret, time.Now().UTC())
		l.v2Count++
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return 0, responseHeaders{}, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, responseHeaders{Header: resp.Header.Clone(), body: respBody}, err
}

func (l *loader) objectPath(key string) string {
	segments := strings.Split(key, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	return "/" + url.PathEscape(l.bucket) + "/" + strings.Join(segments, "/")
}

func seedSessions(dbPath, backendURL, backendAccess, backendSecret, bucket string, age time.Duration, m *manifest) error {
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	be, err := backend.New(backendURL, backendAccess, backendSecret, "us-east-1")
	if err != nil {
		return err
	}
	ctx := context.Background()
	if _, err := st.EnsureBucket(ctx, bucket); err != nil {
		return err
	}
	if err := be.CreateBucket(ctx, bucket); err != nil {
		return err
	}
	for idx, fixture := range m.Sessions {
		existing, err := st.GetSession(ctx, fixture.UploadID)
		if err != nil {
			return err
		}
		if existing != nil {
			continue
		}
		ageSeconds := fixture.AgeSeconds
		if ageSeconds <= 0 {
			ageSeconds = int64(age / time.Second)
		}
		createdAt := time.Now().Add(-time.Duration(ageSeconds) * time.Second).UnixNano()
		session, err := st.CreateSessionAt(ctx, fixture.UploadID, bucket, fixture.Key,
			store.ObjectMetadata{ContentType: "application/octet-stream"}, "STANDARD", "", createdAt)
		if err != nil {
			return err
		}
		for _, part := range fixture.Parts {
			data := deterministicBytes(fmt.Sprintf("session-%d-%d-%d", m.Seed, idx, part.PartNumber), part.Size)
			etag := md5HexLocal(data)
			backendKey := store.UploadPartBackendKey(fixture.UploadID, part.PartNumber)
			if _, err := be.PutObject(ctx, bucket, backendKey, data, "application/octet-stream"); err != nil {
				return err
			}
			if err := st.PutPartAt(ctx, session.ID, part.PartNumber, etag, part.Size, backendKey, createdAt); err != nil {
				return err
			}
		}
	}
	return nil
}

func deterministicBytes(label string, size int64) []byte {
	out := make([]byte, size)
	var counter uint64
	for offset := 0; offset < len(out); {
		h := sha256.New()
		_, _ = io.WriteString(h, label)
		_, _ = io.WriteString(h, ":"+strconv.FormatUint(counter, 10))
		sum := h.Sum(nil)
		offset += copy(out[offset:], sum)
		counter++
	}
	return out
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func md5HexLocal(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}
