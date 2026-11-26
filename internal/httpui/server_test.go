package httpui_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"warmcache/internal/domain"
	"warmcache/internal/gen"
	"warmcache/internal/httpui"
)

type envelope struct {
	OK    bool             `json:"ok"`
	Data  json.RawMessage  `json:"data"`
	Error *domain.BizError `json:"error"`
}

func request(t *testing.T, handler http.Handler, method, path string, body any) (int, envelope, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(payload)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	response := recorder.Result()
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded envelope
	if strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("decode response %s: %v", raw, err)
		}
	}
	return response.StatusCode, decoded, raw
}

func TestPageAndProjectLoading(t *testing.T) {
	server, err := httpui.New(gen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	status, _, page := request(t, handler, http.MethodGet, "/", nil)
	if status != http.StatusOK || !bytes.Contains(page, []byte("warmcache / control surface")) {
		t.Fatalf("page status=%d", status)
	}
	status, envelope, raw := request(t, handler, http.MethodGet, "/api/projects", nil)
	if status != http.StatusOK || !envelope.OK {
		t.Fatalf("projects status=%d body=%s", status, raw)
	}
	var data struct {
		AllSemanticallyEqual bool `json:"all_semantically_equal"`
		Projects             []struct {
			Version      domain.SchemaVersion `json:"version"`
			SemanticHash string               `json:"semantic_hash"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		t.Fatal(err)
	}
	if !data.AllSemanticallyEqual || len(data.Projects) != 3 {
		t.Fatalf("unexpected project payload: %+v", data)
	}
	if data.Projects[0].SemanticHash != data.Projects[2].SemanticHash {
		t.Fatal("v1 and v3 semantic hashes differ")
	}
	for _, version := range []string{"v1", "v2", "v3"} {
		status, _, raw := request(t, handler, http.MethodGet, "/api/sample?version="+version, nil)
		if status != http.StatusOK || !bytes.Contains(raw, []byte("cache-warm.")) {
			t.Fatalf("sample %s status=%d", version, status)
		}
	}
}

func TestRunEditCheckpointAndInjectionOverHTTP(t *testing.T) {
	server, err := httpui.New(gen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	status, envelope, raw := request(t, handler, http.MethodPost, "/api/run", map[string]any{
		"version": "v2", "ticks": 30, "workers": 3,
	})
	if status != http.StatusOK || !envelope.OK {
		t.Fatalf("run status=%d body=%s", status, raw)
	}
	var runData struct {
		Result domain.RunResult `json:"result"`
	}
	if err := json.Unmarshal(envelope.Data, &runData); err != nil {
		t.Fatal(err)
	}
	if len(runData.Result.Timeline) != 30 || len(runData.Result.Leases) == 0 {
		t.Fatalf("unexpected run: %+v", runData.Result)
	}
	foundBackpressure := false
	for _, issue := range runData.Result.Backpressure {
		if issue.Code == domain.CodeBackpressureBuffer {
			foundBackpressure = true
		}
	}
	if !foundBackpressure {
		t.Fatal("expected observable buffer backpressure")
	}

	status, envelope, raw = request(t, handler, http.MethodPost, "/api/edit", map[string]any{
		"version": "v1", "ticks": 30, "workers": 3,
		"edits": []map[string]any{
			{"id": "e1", "kind": "bump_shard", "at_tick": 7, "shard_id": "shard-03"},
			{"id": "e2", "kind": "retire_node", "at_tick": 11, "node_id": "edge-b"},
			{"id": "e3", "kind": "set_budget", "at_tick": 14, "budget_milli": 150},
		},
	})
	if status != http.StatusOK || !envelope.OK {
		t.Fatalf("edit status=%d body=%s", status, raw)
	}
	var editData struct {
		MatchesFull bool             `json:"matches_full"`
		Result      domain.RunResult `json:"result"`
		Diff        struct {
			ChangedCount int `json:"changed_count"`
		} `json:"diff"`
	}
	if err := json.Unmarshal(envelope.Data, &editData); err != nil {
		t.Fatal(err)
	}
	if !editData.MatchesFull || editData.Result.ReusedTicks != 6 || editData.Diff.ChangedCount == 0 {
		t.Fatalf("incremental validation failed: matches=%v reused=%d", editData.MatchesFull, editData.Result.ReusedTicks)
	}

	status, envelope, raw = request(t, handler, http.MethodPost, "/api/checkpoint/compare", map[string]any{
		"version": "v3", "ticks": 30, "workers": 3, "checkpoint_tick": 17,
	})
	if status != http.StatusOK || !envelope.OK {
		t.Fatalf("checkpoint status=%d body=%s", status, raw)
	}
	var checkpointData struct {
		EqualAtCheckpoint  bool `json:"equal_at_checkpoint"`
		EqualAfterContinue bool `json:"equal_after_continue"`
	}
	if err := json.Unmarshal(envelope.Data, &checkpointData); err != nil {
		t.Fatal(err)
	}
	if !checkpointData.EqualAtCheckpoint || !checkpointData.EqualAfterContinue {
		t.Fatalf("checkpoint compare failed: %+v", checkpointData)
	}

	expected := map[string]string{
		"duplicate_shard":      domain.CodeDuplicateShard,
		"zero_lease":           domain.CodeZeroLease,
		"bad_bandwidth_scale":  domain.CodeBadBandwidthScale,
		"retired_node_lease":   domain.CodeRetiredNode,
		"thread_budget":        domain.CodeThreadBudgetInvalid,
		"buffer_budget":        domain.CodeBufferBudgetInvalid,
		"truncated_checkpoint": domain.CodeCheckpointTruncated,
		"snapshot_version":     domain.CodeSnapshotVersion,
		"schema_conflict":      domain.CodeSchemaConflict,
	}
	samplesBefore := map[string][]byte{}
	for _, version := range []string{"v1", "v2", "v3"} {
		_, _, raw := request(t, handler, http.MethodGet, "/api/sample?version="+version, nil)
		samplesBefore[version] = append([]byte(nil), raw...)
		for kind, code := range expected {
			if kind == "thread_budget" || kind == "buffer_budget" || kind == "truncated_checkpoint" || kind == "snapshot_version" {
				continue
			}
			status, envelope, response := request(t, handler, http.MethodPost, "/api/inject", map[string]any{
				"version": version, "kind": kind,
			})
			if status < 400 || envelope.Error == nil || envelope.Error.Code != code {
				t.Fatalf("inject %s/%s: status=%d code=%v body=%s expected=%s", version, kind, status, envelope.Error, response, code)
			}
			if len(envelope.Error.Context) == 0 {
				t.Fatalf("inject %s/%s missing diagnostic context", version, kind)
			}
		}
	}
	for kind, code := range expected {
		if kind != "thread_budget" && kind != "buffer_budget" && kind != "truncated_checkpoint" && kind != "snapshot_version" {
			continue
		}
		status, envelope, raw := request(t, handler, http.MethodPost, "/api/inject", map[string]any{
			"version": "v2", "kind": kind,
		})
		if status < 400 || envelope.Error == nil || envelope.Error.Code != code {
			t.Fatalf("inject v2/%s: status=%d code=%v body=%s expected=%s", kind, status, envelope.Error, raw, code)
		}
	}
	for version, before := range samplesBefore {
		_, _, after := request(t, handler, http.MethodGet, "/api/sample?version="+version, nil)
		if !bytes.Equal(before, after) {
			t.Fatalf("source sample %s was modified by injection", version)
		}
	}
}
