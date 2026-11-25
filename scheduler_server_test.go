package dressbatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParallelValidationBoundedAndOrdered(t *testing.T) {
	project := testProject(t)
	result, _ := testPack(t, project)
	var active int32
	var maxActive int32
	validator := func(project *Project, batch Batch) error {
		current := atomic.AddInt32(&active, 1)
		for {
			previous := atomic.LoadInt32(&maxActive)
			if current <= previous || atomic.CompareAndSwapInt32(&maxActive, previous, current) {
				break
			}
		}
		time.Sleep(8 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return ValidatePlannedBatch(project, batch)
	}
	report, err := RunBatchValidationPool(context.Background(), project, result.Batches, PoolConfig{
		MaxWorkers:               2,
		CandidateBufferPerWorker: 2,
		QueueBudget:              32,
	}, validator)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != len(result.Batches) {
		t.Fatalf("result count mismatch")
	}
	for i, item := range report.Results {
		if item.BatchID != result.Batches[i].ID || !item.Valid {
			t.Fatalf("unordered or invalid result: %+v", report.Results)
		}
	}
	if maxActive := atomic.LoadInt32(&maxActive); maxActive > 2 {
		t.Fatalf("parallelism exceeded configured bound: %d", maxActive)
	}
}

func TestSchedulerRecordsQueueWait(t *testing.T) {
	project := testProject(t)
	result, _ := testPack(t, project)
	batches := append([]Batch(nil), result.Batches...)
	for len(batches) < 5 {
		batches = append(batches, result.Batches...)
	}
	for i := range batches {
		batches[i].ID = "bench-" + string(rune('a'+i))
	}
	validator := func(project *Project, batch Batch) error {
		time.Sleep(12 * time.Millisecond)
		return nil
	}
	queueBudget := 0
	for _, batch := range batches {
		if len(batch.PartIDs) > queueBudget {
			queueBudget = len(batch.PartIDs)
		}
	}
	report, err := RunBatchValidationPool(context.Background(), project, batches, PoolConfig{
		MaxWorkers:               1,
		CandidateBufferPerWorker: 1,
		QueueBudget:              queueBudget,
	}, validator)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.QueueEvents) == 0 {
		t.Fatal("expected recorded queue wait events")
	}
	reasons := map[string]bool{}
	for _, event := range report.QueueEvents {
		reasons[event.Reason] = true
		if event.WaitMillis < 0 {
			t.Fatalf("negative wait duration: %+v", event)
		}
	}
	if !reasons["queue_part_budget"] && !reasons["candidate_buffer_full"] {
		t.Fatalf("unexpected queue reasons: %+v", report.QueueEvents)
	}
}

func TestSchedulerFailureIsolationAndRetry(t *testing.T) {
	project := testProject(t)
	result, _ := testPack(t, project)
	var failedOnce int32
	validator := func(project *Project, batch Batch) error {
		if batch.ID == result.Batches[1].ID && atomic.CompareAndSwapInt32(&failedOnce, 0, 1) {
			return newError(CodeScheduler, "batch:"+batch.ID, "injected", "injected failure")
		}
		return ValidatePlannedBatch(project, batch)
	}
	config := PoolConfig{MaxWorkers: 2, CandidateBufferPerWorker: 2, QueueBudget: 64}
	report, err := RunBatchValidationPool(context.Background(), project, result.Batches, config, validator)
	if err != nil {
		t.Fatal(err)
	}
	if report.Results[1].Valid {
		t.Fatal("expected injected failure")
	}
	if !report.Results[0].Valid || !report.Results[2].Valid {
		t.Fatal("failure polluted other batches")
	}
	retried, err := RetryFailedBatches(context.Background(), project, result.Batches, report, config, ValidatePlannedBatch)
	if err != nil {
		t.Fatal(err)
	}
	if !retried.Results[1].Valid || retried.Results[1].Attempts != 2 {
		t.Fatalf("retry did not recover failed batch: %+v", retried.Results[1])
	}
	if !retried.Results[0].Valid || retried.Results[0].Attempts != 1 {
		t.Fatal("successful batch was modified during retry")
	}
}

func TestSchedulerPanicIsolated(t *testing.T) {
	project := testProject(t)
	result, _ := testPack(t, project)
	validator := func(project *Project, batch Batch) error {
		if batch.ID == result.Batches[1].ID {
			panic("boom")
		}
		return ValidatePlannedBatch(project, batch)
	}
	report, err := RunBatchValidationPool(context.Background(), project, result.Batches, PoolConfig{
		MaxWorkers:               2,
		CandidateBufferPerWorker: 2,
		QueueBudget:              64,
	}, validator)
	if err != nil {
		t.Fatal(err)
	}
	if report.Results[1].Valid || report.Results[1].Error == nil {
		t.Fatal("panic was not isolated as a failed batch")
	}
	if !report.Results[0].Valid || !report.Results[2].Valid {
		t.Fatal("panic polluted other batches")
	}
}

func TestDiagnosticHandler(t *testing.T) {
	project := testProject(t)
	result, _ := testPack(t, project)
	state, err := BuildDiagnosticState(project, result)
	if err != nil {
		t.Fatal(err)
	}
	state.Truth, _ = VerifyTruth(project)
	state.Scheduler, _ = ValidateBatchesWithPool(context.Background(), project, result.Batches, PoolConfig{
		MaxWorkers:               2,
		CandidateBufferPerWorker: 2,
		QueueBudget:              32,
	})
	handler, err := NewDiagnosticHandler(state)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("page status %d", response.StatusCode)
	}
	bodyBytes, _ := io.ReadAll(response.Body)
	body := string(bodyBytes)
	for _, expected := range []string{"批次划分", "失效传播", "并行调度", "b0001"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("page missing %q", expected)
		}
	}

	apiResponse, err := http.Get(server.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer apiResponse.Body.Close()
	var decoded DiagnosticState
	if err := json.NewDecoder(apiResponse.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ProjectID != project.ID || len(decoded.Result.Batches) == 0 {
		t.Fatalf("unexpected state: %+v", decoded)
	}
}
