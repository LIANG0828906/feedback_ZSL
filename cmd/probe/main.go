package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"settlement/internal/api"
	"settlement/internal/bizcodes"
)

type checkReport struct {
	OK          bool     `json:"ok"`
	Checks      []string `json:"checks"`
	Errors      []string `json:"errors,omitempty"`
	DurationMS  int64    `json:"duration_ms"`
	GeneratedAt string   `json:"generated_at"`
}

type prober struct {
	client     api.SettlementLockClient
	metricsURL string
	report     checkReport
}

func main() {
	grpcAddress := flag.String("grpc", "127.0.0.1:17070", "coordinator gRPC address")
	httpAddress := flag.String("http", "127.0.0.1:19090", "coordinator HTTP address")
	outPath := flag.String("out", "artifacts/lock-probe-report.json", "JSON report path")
	flag.Parse()
	started := time.Now()
	rootCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(*grpcAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	p := &prober{
		client:     api.NewSettlementLockClient(conn),
		metricsURL: "http://" + *httpAddress + "/metrics",
	}
	p.report.GeneratedAt = started.Format(time.RFC3339)

	p.checkMutualExclusionAndIdempotency(rootCtx)
	p.checkLeaseFloor(rootCtx)
	p.checkWaitTimeout(rootCtx)
	p.checkExpiry(rootCtx)
	p.checkFIFO(rootCtx)

	p.report.DurationMS = time.Since(started).Milliseconds()
	p.report.OK = len(p.report.Errors) == 0
	data, _ := json.MarshalIndent(p.report, "", "  ")
	data = append(data, '\n')
	if *outPath != "" {
		if err := os.WriteFile(*outPath, data, 0o644); err != nil {
			log.Fatal(err)
		}
	}
	_, _ = os.Stdout.Write(data)
	if !p.report.OK {
		log.Fatalf("lock probes failed with %d error(s)", len(p.report.Errors))
	}
}

func (p *prober) pass(name string) {
	p.report.Checks = append(p.report.Checks, name)
}

func (p *prober) fail(format string, args ...any) {
	p.report.Errors = append(p.report.Errors, fmt.Sprintf(format, args...))
}

func (p *prober) checkMutualExclusionAndIdempotency(ctx context.Context) {
	batchID := uniqueBatch("mutex")
	first, err := p.acquire(ctx, batchID, "A", 6000, 0)
	if err != nil || !first.GetAcquired() {
		p.fail("%s: first acquire failed: acquired=%v code=%s err=%v", batchID, first.GetAcquired(), first.GetErrorCode(), err)
		return
	}
	p.pass("first holder acquires batch")
	p.checkHeldMetrics(ctx, batchID, "A")
	second, err := p.acquire(ctx, batchID, "B", 6000, 0)
	if err != nil || second.GetAcquired() || second.GetErrorCode() != bizcodes.LockHeld || second.GetCurrentHolderId() != "A" {
		p.fail("%s: competing acquire should be E_LOCK_HELD by A: acquired=%v code=%s holder=%s err=%v",
			batchID, second.GetAcquired(), second.GetErrorCode(), second.GetCurrentHolderId(), err)
	} else {
		p.pass("competing holder receives E_LOCK_HELD with current holder")
	}
	renewed, err := p.acquire(ctx, batchID, "A", 6000, 0)
	if err != nil || !renewed.GetAcquired() || !renewed.GetRenewed() ||
		renewed.GetExpiresAtUnixMs() < first.GetExpiresAtUnixMs() {
		p.fail("%s: same-holder acquire should renew: acquired=%v renewed=%v expiry=%d first=%d err=%v",
			batchID, renewed.GetAcquired(), renewed.GetRenewed(), renewed.GetExpiresAtUnixMs(), first.GetExpiresAtUnixMs(), err)
	} else {
		p.pass("same holder re-acquire refreshes lease")
	}
	submission := &api.SubmitResultRequest{
		BatchId: batchID, HolderId: "A", TaskCount: 1, TotalAmount: 42,
		ResultDigest: "probe-digest-a", ResultJson: `[{"task_id":"probe-task","amount":42}]`,
	}
	firstSubmit, err := p.client.SubmitResult(ctx, submission)
	if err != nil || !firstSubmit.GetAccepted() || firstSubmit.GetIdempotent() || firstSubmit.GetCommittedBy() != "A" {
		p.fail("%s: first submit failed: accepted=%v idempotent=%v by=%s code=%s err=%v",
			batchID, firstSubmit.GetAccepted(), firstSubmit.GetIdempotent(), firstSubmit.GetCommittedBy(), firstSubmit.GetErrorCode(), err)
	} else {
		p.pass("first holder commits result once")
	}
	replay, err := p.client.SubmitResult(ctx, submission)
	if err != nil || !replay.GetAccepted() || !replay.GetIdempotent() ||
		replay.GetCommittedBy() != firstSubmit.GetCommittedBy() ||
		replay.GetCommittedAtUnixMs() != firstSubmit.GetCommittedAtUnixMs() ||
		replay.GetResultDigest() != firstSubmit.GetResultDigest() {
		p.fail("%s: replay did not return first result: %+v err=%v", batchID, replay, err)
	} else {
		p.pass("duplicate submit returns the first committed result")
	}
	released, err := p.client.ReleaseLock(ctx, &api.ReleaseLockRequest{BatchId: batchID, HolderId: "A"})
	if err != nil || !released.GetReleased() {
		p.fail("%s: release failed: %+v err=%v", batchID, released, err)
	}
	afterRelease, err := p.client.SubmitResult(ctx, submission)
	if err != nil || afterRelease.GetErrorCode() != bizcodes.LockLost || afterRelease.GetAccepted() {
		p.fail("%s: submit after release should be E_LOCK_LOST: %+v err=%v", batchID, afterRelease, err)
	} else {
		p.pass("submit after lock loss is rejected with E_LOCK_LOST")
	}
	bAcquire, err := p.acquire(ctx, batchID, "B", 6000, 1000)
	if err != nil || !bAcquire.GetAcquired() {
		p.fail("%s: next holder acquire failed: %+v err=%v", batchID, bAcquire, err)
		return
	}
	different := &api.SubmitResultRequest{
		BatchId: batchID, HolderId: "B", TaskCount: 1, TotalAmount: 999,
		ResultDigest: "probe-digest-b", ResultJson: `[{"task_id":"probe-task","amount":999}]`,
	}
	canonical, err := p.client.SubmitResult(ctx, different)
	if err != nil || !canonical.GetAccepted() || canonical.GetCommittedBy() != "A" ||
		canonical.GetResultDigest() != firstSubmit.GetResultDigest() {
		p.fail("%s: replacement holder did not receive canonical first result: %+v err=%v", batchID, canonical, err)
	} else {
		p.pass("replacement holder receives canonical first result")
	}
	_, _ = p.client.ReleaseLock(ctx, &api.ReleaseLockRequest{BatchId: batchID, HolderId: "B"})
}

func (p *prober) checkHeldMetrics(ctx context.Context, batchID, holderID string) {
	metrics, err := p.fetchMetrics(ctx)
	if err != nil {
		p.fail("%s: fetch metrics while held: %v", batchID, err)
		return
	}
	heldNeedle := fmt.Sprintf("settlement_lock_held{batch=%q} 1", batchID)
	ownerNeedle := fmt.Sprintf("settlement_lock_owner_info{batch=%q,holder=%q} 1", batchID, holderID)
	remainingNeedle := fmt.Sprintf("settlement_lock_lease_remaining_seconds{batch=%q} ", batchID)
	if !strings.Contains(metrics, heldNeedle) ||
		!strings.Contains(metrics, ownerNeedle) ||
		!strings.Contains(metrics, remainingNeedle) {
		p.fail("%s: held/owner/remaining metrics missing", batchID)
		return
	}
	remaining := 0.0
	for _, line := range strings.Split(metrics, "\n") {
		if !strings.HasPrefix(line, remainingNeedle) {
			continue
		}
		remaining, _ = strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, remainingNeedle)), 64)
	}
	if remaining <= 0 {
		p.fail("%s: lease_remaining_seconds=%f, want > 0", batchID, remaining)
		return
	}
	p.pass("Prometheus exposes held, owner and positive remaining lease")
}

func (p *prober) fetchMetrics(ctx context.Context) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.metricsURL, nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metrics status %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	return string(body), err
}

func (p *prober) checkLeaseFloor(ctx context.Context) {
	batchID := uniqueBatch("lease-floor")
	acquired, err := p.acquire(ctx, batchID, "A", 6000, 0)
	if err != nil || !acquired.GetAcquired() {
		p.fail("%s: setup acquire failed: %+v err=%v", batchID, acquired, err)
		return
	}
	before, err := p.client.GetStatus(ctx, &api.GetStatusRequest{BatchId: batchID})
	if err != nil {
		p.fail("%s: status before short lease failed: %v", batchID, err)
		return
	}
	short, err := p.acquire(ctx, batchID, "A", 1000, 0)
	if err != nil || short.GetErrorCode() != bizcodes.LeaseTooShort {
		p.fail("%s: short acquire should be E_LEASE_TOO_SHORT: %+v err=%v", batchID, short, err)
	}
	renewShort, err := p.client.RenewLock(ctx, &api.RenewLockRequest{
		BatchId: batchID, HolderId: "A", LeaseTtlMs: 1500,
	})
	if err != nil || renewShort.GetErrorCode() != bizcodes.LeaseTooShort {
		p.fail("%s: short renew should be E_LEASE_TOO_SHORT: %+v err=%v", batchID, renewShort, err)
	}
	after, err := p.client.GetStatus(ctx, &api.GetStatusRequest{BatchId: batchID})
	if err != nil || !after.GetStatus().GetHeld() || after.GetStatus().GetHolderId() != "A" ||
		after.GetStatus().GetExpiresAtUnixMs() != before.GetStatus().GetExpiresAtUnixMs() {
		p.fail("%s: short lease rejection changed state: before=%+v after=%+v err=%v",
			batchID, before.GetStatus(), after.GetStatus(), err)
	} else {
		p.pass("lease below 3s is rejected without changing existing lock")
	}
	_, _ = p.client.ReleaseLock(ctx, &api.ReleaseLockRequest{BatchId: batchID, HolderId: "A"})
}

func (p *prober) checkWaitTimeout(ctx context.Context) {
	batchID := uniqueBatch("wait")
	acquired, err := p.acquire(ctx, batchID, "A", 6000, 0)
	if err != nil || !acquired.GetAcquired() {
		p.fail("%s: setup acquire failed: %+v err=%v", batchID, acquired, err)
		return
	}
	started := time.Now()
	waiting, err := p.acquire(ctx, batchID, "B", 6000, 300)
	elapsed := time.Since(started)
	if err != nil || waiting.GetErrorCode() != bizcodes.WaitTimeout || elapsed < 250*time.Millisecond {
		p.fail("%s: wait should time out with E_WAIT_TIMEOUT: code=%s elapsed=%s err=%v",
			batchID, waiting.GetErrorCode(), elapsed, err)
	} else {
		p.pass("waiting acquire returns E_WAIT_TIMEOUT")
	}
	if metrics, metricsErr := p.fetchMetrics(ctx); metricsErr == nil {
		needle := fmt.Sprintf("settlement_lock_wait_timeouts_total{batch=%q} 1", batchID)
		if !strings.Contains(metrics, needle) {
			p.fail("%s: wait timeout metric missing", batchID)
		} else {
			p.pass("wait timeout is visible in Prometheus metrics")
		}
	} else {
		p.fail("%s: fetch metrics after timeout: %v", batchID, metricsErr)
	}
	_, _ = p.client.ReleaseLock(ctx, &api.ReleaseLockRequest{BatchId: batchID, HolderId: "A"})
}

func (p *prober) checkExpiry(ctx context.Context) {
	batchID := uniqueBatch("expiry")
	acquired, err := p.acquire(ctx, batchID, "A", 3000, 0)
	if err != nil || !acquired.GetAcquired() {
		p.fail("%s: setup acquire failed: %+v err=%v", batchID, acquired, err)
		return
	}
	type result struct {
		resp *api.AcquireLockResponse
		err  error
	}
	resultCh := make(chan result, 1)
	waitStarted := time.Now()
	go func() {
		resp, callErr := p.acquire(ctx, batchID, "B", 3000, 5000)
		resultCh <- result{resp: resp, err: callErr}
	}()
	select {
	case got := <-resultCh:
		elapsed := time.Since(waitStarted)
		expectedAt := time.UnixMilli(acquired.GetExpiresAtUnixMs())
		readyDelay := time.Since(expectedAt)
		if got.err != nil || !got.resp.GetAcquired() {
			p.fail("%s: waiting holder did not acquire after expiry: %+v err=%v", batchID, got.resp, got.err)
			return
		}
		if readyDelay < 0 || readyDelay > time.Second {
			p.fail("%s: lock became available outside the 1s bound: delay=%s elapsed=%s", batchID, readyDelay, elapsed)
		} else {
			p.pass("expired lease becomes acquirable within one second")
		}
	case <-time.After(6500 * time.Millisecond):
		p.fail("%s: waiting holder did not acquire within expiry window", batchID)
		return
	}
	status, err := p.client.GetStatus(ctx, &api.GetStatusRequest{BatchId: batchID})
	if err != nil {
		p.fail("%s: status after expiry failed: %v", batchID, err)
		return
	}
	found := false
	for _, event := range status.GetStatus().GetReleaseEvents() {
		if event.GetHolderId() == "A" && event.GetReason() == "lease_expired" &&
			event.GetBatchId() == batchID && event.GetLeaseExpiresAtUnixMs() == acquired.GetExpiresAtUnixMs() {
			found = true
		}
	}
	if !found {
		p.fail("%s: missing lease_expired release event with original holder and expiry", batchID)
	} else {
		p.pass("lease expiry records original holder, batch, expiry and reason")
	}
	if metrics, metricsErr := p.fetchMetrics(ctx); metricsErr == nil {
		needle := fmt.Sprintf(
			"settlement_lock_release_events_total{batch=%q,reason=%q} 1",
			batchID, "lease_expired",
		)
		if !strings.Contains(metrics, needle) {
			p.fail("%s: lease-expiry release metric missing", batchID)
		} else {
			p.pass("lease-expiry release is visible in Prometheus metrics")
		}
	} else {
		p.fail("%s: fetch metrics after expiry: %v", batchID, metricsErr)
	}
	_, _ = p.client.ReleaseLock(ctx, &api.ReleaseLockRequest{BatchId: batchID, HolderId: "B"})
}

func (p *prober) checkFIFO(ctx context.Context) {
	batchID := uniqueBatch("fifo")
	acquired, err := p.acquire(ctx, batchID, "A", 8000, 0)
	if err != nil || !acquired.GetAcquired() {
		p.fail("%s: setup acquire failed: %+v err=%v", batchID, acquired, err)
		return
	}
	holders := []string{"B", "C", "D"}
	order := make(chan string, len(holders))
	var wg sync.WaitGroup
	for _, holder := range holders {
		wg.Add(1)
		go func(holderID string) {
			defer wg.Done()
			resp, callErr := p.acquire(ctx, batchID, holderID, 8000, 5000)
			if callErr != nil || !resp.GetAcquired() {
				order <- "ERROR:" + holderID + ":" + resp.GetErrorCode()
				return
			}
			order <- holderID
			_, _ = p.client.ReleaseLock(ctx, &api.ReleaseLockRequest{BatchId: batchID, HolderId: holderID})
		}(holder)
		time.Sleep(150 * time.Millisecond)
	}
	selfRenew, renewErr := p.acquire(ctx, batchID, "A", 8000, 0)
	if renewErr != nil || !selfRenew.GetAcquired() || !selfRenew.GetRenewed() {
		p.fail("%s: same-holder renewal must not queue behind waiters: %+v err=%v", batchID, selfRenew, renewErr)
	} else {
		p.pass("same-holder renewal succeeds while others wait")
	}
	time.Sleep(150 * time.Millisecond)
	_, _ = p.client.ReleaseLock(ctx, &api.ReleaseLockRequest{BatchId: batchID, HolderId: "A"})
	wg.Wait()
	close(order)
	var actual []string
	for holder := range order {
		actual = append(actual, holder)
	}
	if len(actual) != len(holders) {
		p.fail("%s: FIFO observed %d/%d outcomes: %v", batchID, len(actual), len(holders), actual)
		return
	}
	for index, holder := range holders {
		if actual[index] != holder {
			p.fail("%s: FIFO order=%v expected=%v", batchID, actual, holders)
			return
		}
	}
	p.pass("waiting acquirers are served in FIFO order")
}

func (p *prober) acquire(
	ctx context.Context,
	batchID, holderID string,
	ttlMs, waitMs int64,
) (*api.AcquireLockResponse, error) {
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(waitMs+4000)*time.Millisecond)
	defer cancel()
	return p.client.AcquireLock(callCtx, &api.AcquireLockRequest{
		BatchId: batchID, HolderId: holderID,
		LeaseTtlMs: ttlMs, WaitTimeoutMs: waitMs,
	})
}

func uniqueBatch(prefix string) string {
	return fmt.Sprintf("probe-%s-%d", prefix, time.Now().UnixNano())
}
