package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"edgegateway/internal/backend"
	"edgegateway/internal/config"
	"edgegateway/internal/metrics"
	"edgegateway/internal/model"
	"edgegateway/internal/store"
)

type fakeBackend struct {
	mu          sync.Mutex
	accepted    map[string]map[int64]bool
	ranges      map[string][]model.Range
	calls       map[string]int
	rejectOnce  map[string]int64
	delayDevice map[string]time.Duration
}

func (f *fakeBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req backend.ForwardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f.mu.Lock()
	f.calls[req.SessionID]++
	delay := f.delayDevice[req.DeviceID]
	rejectSeq, hasReject := f.rejectOnce[req.SessionID]
	reject := hasReject && rejectSeq == req.Seq
	if reject {
		delete(f.rejectOnce, req.SessionID)
	}
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if reject {
		writeJSON(w, map[string]any{
			"accepted": false, "rejected": true, "reason": "injected",
			"code": "TEST_REJECT", "rejected_seq": req.Seq,
			"accepted_ranges": f.ranges[req.SessionID], "ack_through": model.AckThrough(f.ranges[req.SessionID]),
		})
		return
	}
	if f.accepted[req.SessionID] == nil {
		f.accepted[req.SessionID] = make(map[int64]bool)
	}
	duplicate := f.accepted[req.SessionID][req.Seq]
	if !duplicate {
		f.accepted[req.SessionID][req.Seq] = true
		f.ranges[req.SessionID] = model.MergeRange(f.ranges[req.SessionID], req.Seq)
	}
	writeJSON(w, map[string]any{
		"accepted": true, "duplicate": duplicate, "rejected": false,
		"accepted_ranges": f.ranges[req.SessionID],
		"ack_through":     model.AckThrough(f.ranges[req.SessionID]),
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newGatewayTest(t *testing.T, mutate func(*config.Config)) (*Gateway, *httptest.Server, *fakeBackend, *store.Store) {
	t.Helper()
	fb := &fakeBackend{
		accepted:    make(map[string]map[int64]bool),
		ranges:      make(map[string][]model.Range),
		calls:       make(map[string]int),
		rejectOnce:  make(map[string]int64),
		delayDevice: make(map[string]time.Duration),
	}
	backendServer := httptest.NewServer(fb)
	t.Cleanup(backendServer.Close)
	cfg := config.Default()
	cfg.DBPath = t.TempDir() + "/edge.db"
	cfg.BackendURL = backendServer.URL
	cfg.AllowPeerOverride = true
	cfg.BackendTimeoutMS = 3000
	cfg.MaxSessions = 12
	cfg.FeedbackRecoveryN = 2
	if mutate != nil {
		mutate(&cfg)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	secret := bytes.Repeat([]byte{7}, 32)
	gw := New(cfg, st, backend.New(cfg.BackendURL, cfg.BackendTimeout()), metrics.New(st), secret,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	server := httptest.NewServer(gw.Handler(model.IngressTCP))
	t.Cleanup(server.Close)
	return gw, server, fb, st
}

func requestJSON(t *testing.T, client *http.Client, method, url string, body any, peer string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if peer != "" {
		req.Header.Set("X-Edge-Peer-Address", peer)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func createSession(t *testing.T, client *http.Client, base, device, peer string) string {
	t.Helper()
	status, body := requestJSON(t, client, http.MethodPost, base+"/v1/sessions", map[string]any{
		"device_id": device, "connection_id": "conn-" + device,
	}, peer)
	if status != http.StatusOK {
		t.Fatalf("create session status=%d body=%#v", status, body)
	}
	sessionID, _ := body["session_id"].(string)
	if sessionID == "" {
		t.Fatalf("missing session_id: %#v", body)
	}
	return sessionID
}

func testFragmentBody(sessionID, device string, seq int64) map[string]any {
	return map[string]any{
		"session_id": sessionID, "device_id": device, "seq": seq,
		"data_base64": "YWJj", "size": 3,
	}
}

func TestDuplicateEstablishmentAndSameFragment(t *testing.T) {
	_, server, fb, st := newGatewayTest(t, nil)
	client := server.Client()
	first := createSession(t, client, server.URL, "dev-a", "10.1.0.1:1000")
	second := createSession(t, client, server.URL, "dev-a", "10.1.0.1:1001")
	if first != second {
		t.Fatalf("duplicate establishment created a second session: %s %s", first, second)
	}
	status, body := requestJSON(t, client, http.MethodPost, server.URL+"/v1/fragments",
		testFragmentBody(first, "dev-a", 0), "10.1.0.1:1000")
	if status != 200 || body["accepted"] != true {
		t.Fatalf("fragment status=%d body=%#v", status, body)
	}
	status, body = requestJSON(t, client, http.MethodPost, server.URL+"/v1/fragments",
		testFragmentBody(first, "dev-a", 0), "10.1.0.1:1000")
	if status != 200 || body["duplicate"] != true {
		t.Fatalf("duplicate fragment status=%d body=%#v", status, body)
	}
	if fb.calls[first] != 1 {
		t.Fatalf("backend calls = %d, want 1", fb.calls[first])
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body := requestJSON(t, client, http.MethodPost, server.URL+"/v1/fragments",
				testFragmentBody(first, "dev-a", 1), "10.1.0.1:1000")
			if status != 200 || body["accepted"] != true {
				t.Errorf("concurrent same fragment status=%d body=%#v", status, body)
			}
		}()
	}
	wg.Wait()
	if fb.calls[first] != 2 {
		t.Fatalf("same sequence forwarded %d times, want 2 total (seq 0 and 1)", fb.calls[first])
	}
	sess, err := st.GetSession(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if sess.SuccessCount != 2 || sess.AckThrough != 1 {
		t.Fatalf("success=%d ack=%d", sess.SuccessCount, sess.AckThrough)
	}
}

func TestRejectionPausesAndRetryCountsOnce(t *testing.T) {
	_, server, fb, st := newGatewayTest(t, func(c *config.Config) {
		c.RTTThresholdMS = 1000
		c.RateLimitPerSec = 1
	})
	client := server.Client()
	id := createSession(t, client, server.URL, "dev-r", "10.2.0.1:1000")
	status, _ := requestJSON(t, client, http.MethodPost, server.URL+"/v1/fragments", testFragmentBody(id, "dev-r", 0), "10.2.0.1:1000")
	if status != 200 {
		t.Fatalf("seq0 status=%d", status)
	}
	fb.mu.Lock()
	fb.rejectOnce[id] = 1
	fb.mu.Unlock()
	status, body := requestJSON(t, client, http.MethodPost, server.URL+"/v1/fragments", testFragmentBody(id, "dev-r", 1), "10.2.0.1:1000")
	if status != http.StatusUnprocessableEntity || body["rejected_seq"].(float64) != 1 {
		t.Fatalf("rejection status=%d body=%#v", status, body)
	}
	callsBefore := fb.calls[id]
	status, body = requestJSON(t, client, http.MethodPost, server.URL+"/v1/fragments", testFragmentBody(id, "dev-r", 2), "10.2.0.1:1000")
	if status != http.StatusConflict || body["code"] != model.CodeSessionPaused {
		t.Fatalf("paused status=%d body=%#v", status, body)
	}
	if fb.calls[id] != callsBefore {
		t.Fatal("paused subsequent fragment was sent to backend")
	}
	status, body = requestJSON(t, client, http.MethodPost, server.URL+"/v1/fragments", testFragmentBody(id, "dev-r", 1), "10.2.0.1:1000")
	if status != 200 || body["accepted"] != true {
		t.Fatalf("retry status=%d body=%#v", status, body)
	}
	status, _ = requestJSON(t, client, http.MethodPost, server.URL+"/v1/fragments", testFragmentBody(id, "dev-r", 2), "10.2.0.1:1000")
	if status != 200 {
		t.Fatalf("post-recovery status=%d", status)
	}
	sess, _ := st.GetSession(context.Background(), id)
	if sess.SuccessCount != 3 || sess.FailureCount != 1 || sess.RejectionCount != 1 || sess.AckThrough != 2 {
		t.Fatalf("counters success=%d failure=%d rejection=%d ack=%d", sess.SuccessCount, sess.FailureCount, sess.RejectionCount, sess.AckThrough)
	}
	rejections, _ := st.Rejections(context.Background(), id, 10)
	if len(rejections) != 1 || rejections[0].RecoveredAt == 0 {
		t.Fatalf("rejection recovery not recorded: %#v", rejections)
	}
}

func TestFeedbackIsPerSessionAndSurvivesMigration(t *testing.T) {
	_, server, fb, st := newGatewayTest(t, func(c *config.Config) {
		c.RTTThresholdMS = 50
		c.BacklogThreshold = 100
		c.RateLimitPerSec = 9
	})
	client := server.Client()
	a := createSession(t, client, server.URL, "dev-a", "10.3.0.1:1000")
	b := createSession(t, client, server.URL, "dev-b", "10.3.0.2:1000")
	fb.mu.Lock()
	fb.delayDevice["dev-a"] = 95 * time.Millisecond
	fb.mu.Unlock()
	status, body := requestJSON(t, client, http.MethodPost, server.URL+"/v1/fragments", testFragmentBody(a, "dev-a", 0), "10.3.0.1:1000")
	if status != 200 || body["rate_limit_active"] != true {
		t.Fatalf("A status=%d body=%#v", status, body)
	}
	status, body = requestJSON(t, client, http.MethodPost, server.URL+"/v1/fragments", testFragmentBody(b, "dev-b", 0), "10.3.0.2:1000")
	if status != 200 || body["rate_limit_active"] != false {
		t.Fatalf("B inherited A feedback: status=%d body=%#v", status, body)
	}
	status, body = requestJSON(t, client, http.MethodPost, server.URL+"/v1/fragments", testFragmentBody(a, "dev-a", 1), "10.3.0.1:1000")
	if status != http.StatusTooManyRequests || body["code"] != model.CodeRateLimited {
		t.Fatalf("A was not throttled: status=%d body=%#v", status, body)
	}
	// The throttled request still records the address migration. Its limit must
	// remain active, rather than being reset by the reconnect.
	status, _ = requestJSON(t, client, http.MethodPost, server.URL+"/v1/fragments", testFragmentBody(a, "dev-a", 1), "10.9.9.9:1000")
	if status != http.StatusTooManyRequests {
		t.Fatalf("limit reset after migration: status=%d", status)
	}
	sess, _ := st.GetSession(context.Background(), a)
	if sess.MigrationCount != 1 || !sess.LimitActive || sess.RateLimitPerSec != 9 {
		t.Fatalf("migration/limit state: %#v", sess)
	}
	migs, _ := st.Migrations(context.Background(), a)
	if len(migs) != 1 || !migs[0].PreservedLimited || migs[0].PreservedRate != 9 {
		t.Fatalf("migration record: %#v", migs)
	}
}

func TestBacklogThresholdTriggersFeedback(t *testing.T) {
	_, server, fb, st := newGatewayTest(t, func(c *config.Config) {
		c.RTTThresholdMS = 1000
		c.BacklogThreshold = 4
		c.MaxSessionInflight = 8
		c.RateLimitPerSec = 7
	})
	client := server.Client()
	id := createSession(t, client, server.URL, "dev-c", "10.4.0.1:1000")
	fb.mu.Lock()
	fb.delayDevice["dev-c"] = 120 * time.Millisecond
	fb.mu.Unlock()
	var wg sync.WaitGroup
	var conflicts atomic.Int64
	for seq := int64(0); seq < 6; seq++ {
		wg.Add(1)
		go func(seq int64) {
			defer wg.Done()
			status, _ := requestJSON(t, client, http.MethodPost, server.URL+"/v1/fragments", testFragmentBody(id, "dev-c", seq), "10.4.0.1:1000")
			if status == http.StatusConflict {
				conflicts.Add(1)
			}
		}(seq)
	}
	wg.Wait()
	sess, _ := st.GetSession(context.Background(), id)
	if sess.BacklogPeak < 5 || sess.SendWindowPeak < 5 {
		t.Fatalf("backlog peak=%d window peak=%d", sess.BacklogPeak, sess.SendWindowPeak)
	}
	events, _ := st.Feedback(context.Background(), id, 20)
	found := false
	for _, ev := range events {
		if ev.Source == "backend_backpressure" && ev.Basis == "backend_backlog" && ev.Backlog > 4 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no backlog feedback event: %#v", events)
	}
}
