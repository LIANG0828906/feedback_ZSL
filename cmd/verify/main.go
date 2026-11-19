package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"settlement/internal/api"
	"settlement/internal/storage"
	"settlement/internal/tasks"
)

type report struct {
	OK              bool                   `json:"ok"`
	ErrorCount      int                    `json:"error_count"`
	Errors          []string               `json:"errors,omitempty"`
	Committed       int                    `json:"committed_batches"`
	ExpectedBatches int                    `json:"expected_batches"`
	Persisted       int                    `json:"persisted_batches"`
	TaskRows        int                    `json:"settlement_task_rows"`
	ReleaseEvents   int                    `json:"release_events"`
	LeaseExpired    int                    `json:"lease_expired_releases"`
	Batches         []batchReport          `json:"batches"`
	Database        []storage.BatchSummary `json:"database"`
	GeneratedAt     string                 `json:"generated_at"`
}

type batchReport struct {
	BatchID      string `json:"batch_id"`
	Committed    bool   `json:"committed"`
	CommittedBy  string `json:"committed_by"`
	TaskCount    int32  `json:"task_count"`
	TotalAmount  int64  `json:"total_amount"`
	ResultDigest string `json:"result_digest"`
	DBTaskRows   int    `json:"db_task_rows"`
	DBDigest     string `json:"db_digest"`
}

func main() {
	grpcAddress := flag.String("grpc", "127.0.0.1:17070", "coordinator gRPC address")
	dbPath := flag.String("db", "data/settlement.db", "settlement SQLite database")
	seed := flag.Int64("seed", tasks.DefaultSeed, "deterministic generator seed")
	batchCount := flag.Int("batches", 20, "expected batch count")
	tasksPerBatch := flag.Int("tasks-per-batch", 100, "expected tasks per batch")
	outPath := flag.String("out", "artifacts/acceptance-report.json", "JSON report path")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(*grpcAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	client := api.NewSettlementLockClient(conn)
	statuses, err := client.ListStatus(ctx, &api.ListStatusRequest{})
	if err != nil {
		log.Fatal(err)
	}
	store, err := storage.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	summaries, err := store.Summaries(ctx)
	if err != nil {
		log.Fatal(err)
	}
	dbByBatch := make(map[string]storage.BatchSummary, len(summaries))
	for _, summary := range summaries {
		dbByBatch[summary.BatchID] = summary
	}
	expected := tasks.Generate(*seed, *batchCount, *tasksPerBatch)
	statusByBatch := make(map[string]*api.BatchStatus, len(statuses.GetStatuses()))
	for _, item := range statuses.GetStatuses() {
		statusByBatch[item.GetBatchId()] = item
	}

	rep := report{
		ExpectedBatches: *batchCount,
		Persisted:       len(summaries),
		Database:        summaries,
		GeneratedAt:     time.Now().Format(time.RFC3339),
	}
	addErr := func(format string, args ...any) {
		rep.Errors = append(rep.Errors, fmt.Sprintf(format, args...))
	}
	for _, batch := range expected {
		result, buildErr := tasks.BuildResult(batch)
		if buildErr != nil {
			addErr("%s: build deterministic result: %v", batch.ID, buildErr)
			continue
		}
		status := statusByBatch[batch.ID]
		dbItem, dbOK := dbByBatch[batch.ID]
		item := batchReport{BatchID: batch.ID}
		if status == nil {
			addErr("%s: coordinator status missing", batch.ID)
		} else {
			item.Committed = status.GetCommitted()
			item.CommittedBy = status.GetCommittedBy()
			item.TaskCount = status.GetTaskCount()
			item.TotalAmount = status.GetTotalAmount()
			item.ResultDigest = status.GetResultDigest()
			rep.ReleaseEvents += len(status.GetReleaseEvents())
			for _, event := range status.GetReleaseEvents() {
				if event.GetReason() == "lease_expired" {
					rep.LeaseExpired++
				}
			}
			if !status.GetCommitted() {
				addErr("%s: coordinator reports not committed", batch.ID)
			}
			if status.GetTaskCount() != int32(*tasksPerBatch) {
				addErr("%s: coordinator task_count=%d expected=%d", batch.ID, status.GetTaskCount(), *tasksPerBatch)
			}
			if status.GetTotalAmount() != result.TotalAmount {
				addErr("%s: coordinator total=%d expected=%d", batch.ID, status.GetTotalAmount(), result.TotalAmount)
			}
			if status.GetResultDigest() != result.Digest {
				addErr("%s: coordinator digest=%s expected=%s", batch.ID, status.GetResultDigest(), result.Digest)
			}
			if status.GetCommittedBy() == "" {
				addErr("%s: committed_by is empty", batch.ID)
			}
			rep.Committed++
		}
		if !dbOK {
			addErr("%s: SQLite settlement missing", batch.ID)
		} else {
			item.DBDigest = dbItem.ResultDigest
			item.DBTaskRows, _ = store.TaskCount(ctx, batch.ID)
			if dbItem.ResultDigest != result.Digest {
				addErr("%s: SQLite digest=%s expected=%s", batch.ID, dbItem.ResultDigest, result.Digest)
			}
			if dbItem.TotalAmount != result.TotalAmount {
				addErr("%s: SQLite total=%d expected=%d", batch.ID, dbItem.TotalAmount, result.TotalAmount)
			}
			if item.DBTaskRows != *tasksPerBatch {
				addErr("%s: SQLite task rows=%d expected=%d", batch.ID, item.DBTaskRows, *tasksPerBatch)
			}
			rep.TaskRows += item.DBTaskRows
		}
		rep.Batches = append(rep.Batches, item)
	}
	if len(summaries) != *batchCount {
		addErr("SQLite has %d settlement batches, expected %d", len(summaries), *batchCount)
	}
	if rep.TaskRows != *batchCount**tasksPerBatch {
		addErr("SQLite has %d settlement task rows, expected %d", rep.TaskRows, *batchCount**tasksPerBatch)
	}
	if integrity, err := store.IntegrityCheck(ctx); err != nil || integrity != "ok" {
		addErr("SQLite integrity check failed: result=%q error=%v", integrity, err)
	}
	rep.ErrorCount = len(rep.Errors)
	rep.OK = rep.ErrorCount == 0
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	data = append(data, '\n')
	if *outPath != "" {
		if err := os.WriteFile(*outPath, data, 0o644); err != nil {
			log.Fatal(err)
		}
	}
	_, _ = os.Stdout.Write(data)
	if !rep.OK {
		log.Fatalf("acceptance verification failed with %d error(s)", rep.ErrorCount)
	}
}
