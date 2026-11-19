package gateway

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"s3gw/internal/backend"
	"s3gw/internal/config"
	"s3gw/internal/monitor"
	"s3gw/internal/sig"
	"s3gw/internal/store"
)

const (
	testAccess = "gateway"
	testSecret = "gateway-secret"
)

type fakeS3 struct {
	mu     sync.RWMutex
	bucket map[string]map[string][]byte
	writes atomic.Int64
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	t.Helper()
	f := &fakeS3{bucket: map[string]map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	return f, server
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPut && key == "" {
		if _, ok := f.bucket[bucket]; !ok {
			f.bucket[bucket] = map[string][]byte{}
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method == http.MethodDelete && key == "" {
		delete(f.bucket, bucket)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	objects := f.bucket[bucket]
	if objects == nil {
		writeBackendError(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		objects[key] = body
		f.writes.Add(1)
		sum := md5.Sum(body)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet, http.MethodHead:
		body, ok := objects[key]
		if !ok {
			writeBackendError(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		sum := md5.Sum(body)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		w.Header().Set("Content-Length", intString(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	case http.MethodDelete:
		delete(objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeBackendError(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func writeBackendError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "<Error><Code>"+code+"</Code><Message>"+code+"</Message></Error>")
}

func intString(n int) string {
	return strconv.Itoa(n)
}

type testEnv struct {
	server *httptest.Server
	gw     *Server
	store  *store.Store
	fake   *fakeS3
}

func newTestEnv(t *testing.T, retention time.Duration) *testEnv {
	t.Helper()
	fake, beServer := newFakeS3(t)
	st, err := store.Open(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	be, err := backend.New(beServer.URL, "minioadmin", "minioadmin", "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Backend: beServer.URL, Region: "us-east-1", AccessKey: testAccess, SecretKey: testSecret,
		PartRetention: retention, ReaperInterval: time.Second, MaxObjectBytes: 64 << 20, MaxListKeys: 1000,
	}
	gw := New(cfg, st, be, monitor.New(), log.New(io.Discard, "", 0))
	server := httptest.NewServer(gw)
	t.Cleanup(server.Close)
	return &testEnv{server: server, gw: gw, store: st, fake: fake}
}

func (e *testEnv) request(t *testing.T, method, path string, body []byte, headers map[string]string, version string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Host = req.URL.Host
	switch version {
	case "v2":
		sig.SignV2Request(req, testAccess, testSecret, time.Now().UTC())
	case "v4":
		sig.SignV4Request(req, testAccess, testSecret, "us-east-1", body, time.Now().UTC())
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func requireStatus(t *testing.T, resp *http.Response, want int) []byte {
	t.Helper()
	body := readAll(t, resp)
	if resp.StatusCode != want {
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, want, body)
	}
	return body
}

func createBucket(t *testing.T, e *testEnv, bucket string) {
	t.Helper()
	resp := e.request(t, http.MethodPut, "/"+bucket, nil, nil, "v2")
	requireStatus(t, resp, http.StatusOK)
}

func TestVersioningAndLocationDiscovery(t *testing.T) {
	e := newTestEnv(t, time.Hour)
	createBucket(t, e, "discovery-bucket")
	config := []byte(`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`)
	resp := e.request(t, http.MethodPut, "/discovery-bucket?versioning", config, nil, "v4")
	requireStatus(t, resp, http.StatusOK)
	resp = e.request(t, http.MethodGet, "/discovery-bucket?versioning", nil, nil, "v2")
	body := requireStatus(t, resp, http.StatusOK)
	if !bytes.Contains(body, []byte("<Status>Enabled</Status>")) {
		t.Fatalf("unexpected versioning response: %s", body)
	}
	resp = e.request(t, http.MethodGet, "/discovery-bucket?location", nil, nil, "v4")
	body = requireStatus(t, resp, http.StatusOK)
	if !bytes.Contains(body, []byte("us-east-1")) {
		t.Fatalf("unexpected location response: %s", body)
	}
}

func TestDualSignaturesVersionsDeleteAndList(t *testing.T) {
	e := newTestEnv(t, time.Hour)
	createBucket(t, e, "demo-bucket")

	v4Body := []byte("written by the new client")
	resp := e.request(t, http.MethodPut, "/demo-bucket/team/object.txt", v4Body, map[string]string{
		"Content-Type":        "text/plain",
		"x-amz-storage-class": "STANDARD",
		"x-amz-acl":           "private",
		"x-amz-meta-origin":   "v4",
	}, "v4")
	requireStatus(t, resp, http.StatusOK)
	v4ID := resp.Header.Get("x-amz-version-id")

	v2Body := []byte("written by an old client without acl/storage-class")
	resp = e.request(t, http.MethodPut, "/demo-bucket/team/object.txt", v2Body, map[string]string{
		"Content-Type":      "text/plain",
		"x-amz-meta-origin": "v2",
	}, "v2")
	requireStatus(t, resp, http.StatusOK)
	v2ID := resp.Header.Get("x-amz-version-id")
	if v4ID == "" || v2ID == "" || v4ID == v2ID || v2ID <= v4ID {
		t.Fatalf("unexpected version ids v4=%q v2=%q", v4ID, v2ID)
	}

	resp = e.request(t, http.MethodGet, "/demo-bucket/team/object.txt", nil, nil, "v4")
	if got := string(requireStatus(t, resp, http.StatusOK)); got != string(v2Body) {
		t.Fatalf("current body=%q", got)
	}
	resp = e.request(t, http.MethodGet, "/demo-bucket/team/object.txt?versionId="+v4ID, nil, nil, "v2")
	if got := string(requireStatus(t, resp, http.StatusOK)); got != string(v4Body) {
		t.Fatalf("historical body=%q", got)
	}
	resp = e.request(t, http.MethodGet, "/demo-bucket/team/object.txt?versionId=v999", nil, nil, "v4")
	body := requireStatus(t, resp, http.StatusNotFound)
	if !bytes.Contains(body, []byte("<Code>NoSuchVersion</Code>")) {
		t.Fatalf("expected NoSuchVersion: %s", body)
	}

	resp = e.request(t, http.MethodDelete, "/demo-bucket/team/object.txt?versionId="+v4ID, nil, nil, "v4")
	requireStatus(t, resp, http.StatusNoContent)
	resp = e.request(t, http.MethodDelete, "/demo-bucket/team/object.txt?versionId="+v4ID, nil, nil, "v4")
	requireStatus(t, resp, http.StatusNoContent)

	resp = e.request(t, http.MethodGet, "/demo-bucket?versions&prefix=team/", nil, nil, "v4")
	body = requireStatus(t, resp, http.StatusOK)
	if bytes.Count(body, []byte("<DeleteMarker>")) != 0 {
		t.Fatalf("duplicate delete of a version created a marker: %s", body)
	}
	if !bytes.Contains(body, []byte(v2ID)) || bytes.Contains(body, []byte(v4ID)) {
		t.Fatalf("version chain is incorrect: %s", body)
	}

	resp = e.request(t, http.MethodDelete, "/demo-bucket/team/object.txt", nil, nil, "v2")
	requireStatus(t, resp, http.StatusNoContent)
	markerID := resp.Header.Get("x-amz-version-id")
	if markerID == "" {
		t.Fatal("missing delete marker version id")
	}
	resp = e.request(t, http.MethodGet, "/demo-bucket/team/object.txt", nil, nil, "v4")
	body = requireStatus(t, resp, http.StatusNotFound)
	if !bytes.Contains(body, []byte("<Code>NoSuchKey</Code>")) {
		t.Fatalf("expected NoSuchKey, got %s", body)
	}

	resp = e.request(t, http.MethodGet, "/demo-bucket?versions&prefix=team/", nil, nil, "v2")
	body = requireStatus(t, resp, http.StatusOK)
	if !bytes.Contains(body, []byte(markerID)) || !bytes.Contains(body, []byte("<DeleteMarker>")) {
		t.Fatalf("delete marker missing from list: %s", body)
	}
}

func TestRejectsUnsignedAndBadSignaturesBeforeForwarding(t *testing.T) {
	e := newTestEnv(t, time.Hour)
	createBucket(t, e, "secure-bucket")
	before := e.fake.writes.Load()

	resp := e.request(t, http.MethodPut, "/secure-bucket/anonymous", []byte("no signature"), nil, "")
	body := requireStatus(t, resp, http.StatusForbidden)
	if !bytes.Contains(body, []byte("<Code>AccessDenied</Code>")) {
		t.Fatalf("unexpected unsigned error: %s", body)
	}
	if e.fake.writes.Load() != before {
		t.Fatal("unsigned request reached the backend")
	}

	req, _ := http.NewRequest(http.MethodPut, e.server.URL+"/secure-bucket/bad", strings.NewReader("bad"))
	req.Host = req.URL.Host
	sig.SignV2Request(req, testAccess, testSecret, time.Now().UTC())
	req.Header.Set("Authorization", "AWS gateway:AAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	badResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body = requireStatus(t, badResp, http.StatusForbidden)
	if !bytes.Contains(body, []byte("<Code>SignatureDoesNotMatch</Code>")) {
		t.Fatalf("unexpected bad signature error: %s", body)
	}
	if e.fake.writes.Load() != before {
		t.Fatal("bad signature request reached the backend")
	}

	req, _ = http.NewRequest(http.MethodPut, e.server.URL+"/secure-bucket/missing", strings.NewReader("x"))
	req.Host = req.URL.Host
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=gateway/20240101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00")
	missingResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body = requireStatus(t, missingResp, http.StatusBadRequest)
	if !bytes.Contains(body, []byte("MissingSecurityHeader")) {
		t.Fatalf("unexpected missing header error: %s", body)
	}
}

func TestConcurrentWritesReceiveUniqueRetrievableVersions(t *testing.T) {
	e := newTestEnv(t, time.Hour)
	createBucket(t, e, "race-bucket")
	const n = 24
	type result struct {
		id   string
		body string
		err  error
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := []byte("body-" + intString(i))
			req, err := http.NewRequest(http.MethodPut, e.server.URL+"/race-bucket/same-key", bytes.NewReader(body))
			if err != nil {
				results <- result{err: err}
				return
			}
			req.Host = req.URL.Host
			if i%2 == 0 {
				sig.SignV4Request(req, testAccess, testSecret, "us-east-1", body, time.Now().UTC())
			} else {
				sig.SignV2Request(req, testAccess, testSecret, time.Now().UTC())
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			results <- result{id: resp.Header.Get("x-amz-version-id"), body: string(body)}
		}(i)
	}
	wg.Wait()
	close(results)
	seen := map[string]string{}
	var maxID string
	for r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.id == "" {
			t.Fatal("empty version id")
		}
		if prev, ok := seen[r.id]; ok {
			t.Fatalf("duplicate version id %s for %q and %q", r.id, prev, r.body)
		}
		seen[r.id] = r.body
		if r.id > maxID {
			maxID = r.id
		}
	}
	if len(seen) != n {
		t.Fatalf("got %d unique versions", len(seen))
	}
	for id, body := range seen {
		resp := e.request(t, http.MethodGet, "/race-bucket/same-key?versionId="+id, nil, nil, "v4")
		if got := string(requireStatus(t, resp, http.StatusOK)); got != body {
			t.Fatalf("version %s body=%q want %q", id, got, body)
		}
	}
	resp := e.request(t, http.MethodGet, "/race-bucket/same-key", nil, nil, "v2")
	resp.Body.Close()
	if resp.Header.Get("x-amz-version-id") != maxID {
		t.Fatalf("current version=%s want max=%s", resp.Header.Get("x-amz-version-id"), maxID)
	}
}

type uploadResult struct {
	UploadID string
}

func initiate(t *testing.T, e *testEnv, bucket, key string) string {
	t.Helper()
	resp := e.request(t, http.MethodPost, "/"+bucket+"/"+key+"?uploads", nil, map[string]string{
		"Content-Type": "application/octet-stream",
	}, "v4")
	body := requireStatus(t, resp, http.StatusOK)
	var parsed initiateMultipartUploadResult
	if err := xml.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.UploadID == "" {
		t.Fatalf("no upload id: %s", body)
	}
	return parsed.UploadID
}

func uploadPart(t *testing.T, e *testEnv, bucket, key, uploadID string, number int, body []byte) string {
	t.Helper()
	resp := e.request(t, http.MethodPut, "/"+bucket+"/"+key+"?partNumber="+intString(number)+"&uploadId="+uploadID,
		body, nil, "v2")
	requireStatus(t, resp, http.StatusOK)
	return resp.Header.Get("ETag")
}

func completeUpload(t *testing.T, e *testEnv, bucket, key, uploadID string, parts ...completePart) *http.Response {
	t.Helper()
	payload, _ := xml.Marshal(completeMultipartUpload{Parts: parts})
	return e.request(t, http.MethodPost, "/"+bucket+"/"+key+"?uploadId="+uploadID, payload, nil, "v4")
}

func TestMultipartCompleteAbortAndRetry(t *testing.T) {
	e := newTestEnv(t, time.Hour)
	createBucket(t, e, "multi-bucket")
	uploadID := initiate(t, e, "multi-bucket", "large.bin")
	part1 := bytes.Repeat([]byte("a"), 1024)
	part2 := append(bytes.Repeat([]byte("b"), 512), []byte("tail")...)
	etag1 := uploadPart(t, e, "multi-bucket", "large.bin", uploadID, 1, part1)
	etag2 := uploadPart(t, e, "multi-bucket", "large.bin", uploadID, 2, part2)

	resp := completeUpload(t, e, "multi-bucket", "large.bin", uploadID,
		completePart{PartNumber: 1, ETag: etag1},
		completePart{PartNumber: 3, ETag: `"deadbeef"`})
	body := requireStatus(t, resp, http.StatusBadRequest)
	if !bytes.Contains(body, []byte("<Code>InvalidPart</Code>")) {
		t.Fatalf("expected InvalidPart: %s", body)
	}
	etag3 := uploadPart(t, e, "multi-bucket", "large.bin", uploadID, 3, []byte("part-three"))
	resp = completeUpload(t, e, "multi-bucket", "large.bin", uploadID,
		completePart{PartNumber: 1, ETag: etag1},
		completePart{PartNumber: 2, ETag: etag2},
		completePart{PartNumber: 3, ETag: etag3})
	body = requireStatus(t, resp, http.StatusOK)
	versionID := resp.Header.Get("x-amz-version-id")
	if versionID == "" {
		t.Fatalf("complete did not return version id: %s", body)
	}
	expected := append(append(append([]byte{}, part1...), part2...), []byte("part-three")...)
	resp = e.request(t, http.MethodGet, "/multi-bucket/large.bin?versionId="+versionID, nil, nil, "v4")
	if got := requireStatus(t, resp, http.StatusOK); !bytes.Equal(got, expected) {
		t.Fatalf("multipart object mismatch len=%d want=%d", len(got), len(expected))
	}

	abortID := initiate(t, e, "multi-bucket", "aborted.bin")
	uploadPart(t, e, "multi-bucket", "aborted.bin", abortID, 1, []byte("will be removed"))
	resp = e.request(t, http.MethodDelete, "/multi-bucket/aborted.bin?uploadId="+abortID, nil, nil, "v4")
	requireStatus(t, resp, http.StatusNoContent)
	e.fake.mu.RLock()
	defer e.fake.mu.RUnlock()
	for key := range e.fake.bucket["multi-bucket"] {
		if strings.Contains(key, abortID) {
			t.Fatalf("aborted upload still occupies backend key %s", key)
		}
	}
}

func TestReaperOnlyRemovesExpiredUploadsAndKeepsCommittedObject(t *testing.T) {
	e := newTestEnv(t, time.Hour)
	createBucket(t, e, "reap-bucket")
	committed := []byte("must survive multipart reaping")
	resp := e.request(t, http.MethodPut, "/reap-bucket/committed.txt", committed, nil, "v4")
	requireStatus(t, resp, http.StatusOK)
	committedVersion := resp.Header.Get("x-amz-version-id")

	ctx := context.Background()
	uploadID := "0123456789abcdef0123456789abcdef"
	session, err := e.store.CreateSessionAt(ctx, uploadID, "reap-bucket", "old.bin",
		store.ObjectMetadata{ContentType: "application/octet-stream"}, "STANDARD", "", time.Now().Add(-2*time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	partBody := bytes.Repeat([]byte("z"), 4096)
	partKey := store.UploadPartBackendKey(uploadID, 1)
	if _, err := e.gw.backend.PutObject(ctx, "reap-bucket", partKey, partBody, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.PutPartAt(ctx, session.ID, 1, md5Hex(partBody), int64(len(partBody)), partKey, time.Now().Add(-2*time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}

	result := e.gw.ReapOnce(ctx)
	if result.ReclaimedSessions != 1 || result.ReclaimedBytes != int64(len(partBody)) {
		t.Fatalf("unexpected reap result: %+v", result)
	}
	e.fake.mu.RLock()
	_, stillThere := e.fake.bucket["reap-bucket"][partKey]
	e.fake.mu.RUnlock()
	if stillThere {
		t.Fatal("expired part still exists in backend")
	}
	resp = e.request(t, http.MethodGet, "/reap-bucket/committed.txt?versionId="+committedVersion, nil, nil, "v2")
	if got := requireStatus(t, resp, http.StatusOK); !bytes.Equal(got, committed) {
		t.Fatalf("committed object changed after reaping: %q", got)
	}
	resp = completeUpload(t, e, "reap-bucket", "old.bin", uploadID,
		completePart{PartNumber: 1, ETag: quotedETag(md5Hex(partBody))})
	body := requireStatus(t, resp, http.StatusNotFound)
	if !bytes.Contains(body, []byte("NoSuchUpload")) {
		t.Fatalf("reaped upload was still completable: %s", body)
	}

	statusResp, err := http.Get(e.server.URL + "/_gateway/status?bucket=reap-bucket")
	if err != nil {
		t.Fatal(err)
	}
	statusBody := requireStatus(t, statusResp, http.StatusOK)
	var status map[string]any
	if err := json.Unmarshal(statusBody, &status); err != nil {
		t.Fatal(err)
	}
	if status["oldest_incomplete_session_age_seconds"].(float64) != 0 {
		t.Fatalf("unexpected status: %s", statusBody)
	}
	reclaims := status["recent_reclaims"].([]any)
	if len(reclaims) != 1 {
		t.Fatalf("reclaim ledger missing: %s", statusBody)
	}

	metricsResp, err := http.Get(e.server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metricsBody := string(requireStatus(t, metricsResp, http.StatusOK))
	for _, name := range []string{
		"s3gw_signature_requests_total", "s3gw_signature_failures_total",
		"s3gw_versions_current", "s3gw_multipart_sessions_incomplete",
		"s3gw_reaper_bytes_reclaimed_total", "s3gw_reaper_failures_total",
	} {
		if !strings.Contains(metricsBody, name) {
			t.Fatalf("metric %s missing:\n%s", name, metricsBody)
		}
	}
}

func TestPresignedV2AndV4(t *testing.T) {
	e := newTestEnv(t, time.Hour)
	createBucket(t, e, "presign-bucket")

	req, _ := http.NewRequest(http.MethodPut, e.server.URL+"/presign-bucket/v2.txt", strings.NewReader("v2 pre"))
	req.Host = req.URL.Host
	sig.SignV2Presigned(req, testAccess, testSecret, time.Now().Add(time.Minute))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, resp, http.StatusOK)

	req, _ = http.NewRequest(http.MethodPut, e.server.URL+"/presign-bucket/v4.txt", strings.NewReader("v4 pre"))
	req.Host = req.URL.Host
	sig.SignV4Presigned(req, testAccess, testSecret, "us-east-1", time.Minute, "UNSIGNED-PAYLOAD")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, resp, http.StatusOK)
}
