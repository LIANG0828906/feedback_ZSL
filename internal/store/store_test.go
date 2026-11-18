package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/dualring-config/internal/model"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(duration)
}

func newTestStore(t *testing.T, clock *fakeClock, history int) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "config.db"), Options{
		CanaryPercent:     10,
		ErrorThreshold:    0.005,
		HistoryLimit:      history,
		ObservationPeriod: 500 * time.Millisecond,
		Now:               clock.Now,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCASConflictHistoryAndAudit(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := newTestStore(t, clock, 5)
	if _, err := s.Bootstrap(ctx, "payments", "seed", map[string]string{"timeout": "100ms"}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	const workers = 12
	var wg sync.WaitGroup
	var successes atomic.Int64
	var conflicts atomic.Int64
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.UpdateItem(ctx, "payments", "timeout", fmt.Sprintf("%dms", i),
				false, 1, fmt.Sprintf("worker-%02d", i), fmt.Sprintf("req-%02d", i))
			if err == nil {
				successes.Add(1)
				return
			}
			var biz *model.BusinessError
			if errors.As(err, &biz) && biz.Code == model.CodeConflict {
				conflicts.Add(1)
				return
			}
			t.Errorf("unexpected update error: %v", err)
		}(i)
	}
	close(start)
	wg.Wait()
	if successes.Load() != 1 || conflicts.Load() != workers-1 {
		t.Fatalf("CAS result: successes=%d conflicts=%d", successes.Load(), conflicts.Load())
	}

	record, err := s.ItemVersion(ctx, "payments", "timeout")
	if err != nil {
		t.Fatalf("item version: %v", err)
	}
	if record.Version != 2 {
		t.Fatalf("item version=%d want 2", record.Version)
	}
	history, err := s.History(ctx, "payments", "timeout", 20)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history length=%d want 2", len(history))
	}

	current := record.Version
	for i := 0; i < 7; i++ {
		value := fmt.Sprintf("v%d", i)
		result, err := s.UpdateItem(ctx, "payments", "timeout", value, false, current, "sequential", "")
		if err != nil {
			t.Fatalf("sequential update %d: %v", i, err)
		}
		current = result.ItemVersion
	}
	history, err = s.History(ctx, "payments", "timeout", 20)
	if err != nil {
		t.Fatalf("history after updates: %v", err)
	}
	if len(history) != 5 {
		t.Fatalf("history length=%d want retention of 5", len(history))
	}
	if history[0].Version != current {
		t.Fatalf("newest history version=%d want %d", history[0].Version, current)
	}

	deleted, err := s.UpdateItem(ctx, "payments", "timeout", "", true, current, "deleter", "")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	recreated, err := s.UpdateItem(ctx, "payments", "timeout", "back", false, deleted.ItemVersion,
		"creator", "")
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if recreated.ItemVersion != deleted.ItemVersion+1 {
		t.Fatalf("recreated version=%d want %d", recreated.ItemVersion, deleted.ItemVersion+1)
	}

	audits, err := s.ListAudit(ctx, "payments", 1000)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	var failedAudit, successAudit bool
	for _, entry := range audits {
		if entry.Operation == "config.update" && entry.StatusCode == model.CodeConflict {
			failedAudit = true
		}
		if entry.Operation == "config.update" && entry.StatusCode == model.CodeOK {
			successAudit = true
		}
	}
	if !failedAudit || !successAudit {
		t.Fatalf("audit outcomes missing: failed=%t success=%t", failedAudit, successAudit)
	}
}

func TestReleaseObservationPassAndFullPromotion(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := newTestStore(t, clock, 20)
	if _, err := s.Bootstrap(ctx, "payments", "seed", map[string]string{
		"timeout": "100ms",
		"limit":   "10",
	}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := s.SetClientRing(ctx, "canary-1", model.RingCanary, "test"); err != nil {
		t.Fatalf("set canary ring: %v", err)
	}
	if _, err := s.SetClientRing(ctx, "full-1", model.RingFull, "test"); err != nil {
		t.Fatalf("set full ring: %v", err)
	}
	headBefore, err := s.HeadConfig(ctx, "payments")
	if err != nil {
		t.Fatalf("head before: %v", err)
	}

	release, err := s.CreateRelease(ctx, ReleaseInput{
		Namespace:          "payments",
		Operator:           "releaser",
		ObservationSeconds: 1,
		Changes: []model.ItemChange{
			{Key: "timeout", Value: "250ms"},
		},
	})
	if err != nil {
		t.Fatalf("create release: %v", err)
	}
	if _, err := s.TransitionRelease(ctx, release.ID, "full", "releaser", ""); err == nil {
		t.Fatal("draft to full should fail")
	} else {
		var biz *model.BusinessError
		if !errors.As(err, &biz) || biz.Code != model.CodeInvalidTransition {
			t.Fatalf("draft to full error=%v", err)
		}
	}
	unchanged, err := s.GetRelease(ctx, release.ID)
	if err != nil {
		t.Fatalf("get release after invalid transition: %v", err)
	}
	if unchanged.State != model.ReleaseDraft {
		t.Fatalf("invalid transition changed state to %s", unchanged.State)
	}

	release, err = s.TransitionRelease(ctx, release.ID, "canary", "releaser", "")
	if err != nil {
		t.Fatalf("start canary: %v", err)
	}
	canaryPull, err := s.ResolvePull(ctx, "canary-1", "payments")
	if err != nil {
		t.Fatalf("canary pull: %v", err)
	}
	fullPull, err := s.ResolvePull(ctx, "full-1", "payments")
	if err != nil {
		t.Fatalf("full pull: %v", err)
	}
	if canaryPull.Version != release.CandidateSnapshot || canaryPull.ReleaseID != release.ID {
		t.Fatalf("canary served version=%d release=%s", canaryPull.Version, canaryPull.ReleaseID)
	}
	if fullPull.Version != headBefore.Version || fullPull.ReleaseID != "" {
		t.Fatalf("full ring served version=%d release=%s, want old head=%d",
			fullPull.Version, fullPull.ReleaseID, headBefore.Version)
	}
	if itemValue(canaryPull.Items, "timeout") != "250ms" ||
		itemValue(fullPull.Items, "timeout") != "100ms" {
		t.Fatal("ring configurations were mixed")
	}

	if _, err := s.TransitionRelease(ctx, release.ID, "full", "releaser", ""); err == nil {
		t.Fatal("full promotion during observation should fail")
	} else {
		var biz *model.BusinessError
		if !errors.As(err, &biz) || biz.Code != model.CodeObservationActive {
			t.Fatalf("early full transition error=%v", err)
		}
	}
	if _, err := s.ReportObservation(ctx, ObservationInput{
		ReleaseID:    release.ID,
		InstanceID:   "canary-1",
		Successes:    999,
		Errors:       1,
		LocalVersion: release.CandidateSnapshot,
	}); err != nil {
		t.Fatalf("report observation: %v", err)
	}
	clock.Advance(2 * time.Second)
	if changed, err := s.EvaluateDueReleases(ctx); err != nil || changed != 1 {
		t.Fatalf("evaluate: changed=%d err=%v", changed, err)
	}
	passed, err := s.GetRelease(ctx, release.ID)
	if err != nil {
		t.Fatalf("get passed release: %v", err)
	}
	if passed.State != model.ReleaseObservedPass {
		t.Fatalf("state=%s want observed_pass", passed.State)
	}
	if passed.ErrorCount != 1 || passed.SuccessCount != 999 {
		t.Fatalf("observation counts success=%d error=%d", passed.SuccessCount, passed.ErrorCount)
	}

	promoted, err := s.TransitionRelease(ctx, release.ID, "full", "releaser", "")
	if err != nil {
		t.Fatalf("promote full: %v", err)
	}
	if promoted.State != model.ReleaseFull {
		t.Fatalf("promoted state=%s", promoted.State)
	}
	fullPull, err = s.ResolvePull(ctx, "full-1", "payments")
	if err != nil {
		t.Fatalf("full pull after promotion: %v", err)
	}
	canaryPull, err = s.ResolvePull(ctx, "canary-1", "payments")
	if err != nil {
		t.Fatalf("canary pull after promotion: %v", err)
	}
	if fullPull.Version != release.CandidateSnapshot || canaryPull.Version != release.CandidateSnapshot {
		t.Fatalf("post-promotion versions canary=%d full=%d candidate=%d",
			canaryPull.Version, fullPull.Version, release.CandidateSnapshot)
	}
	if itemValue(fullPull.Items, "timeout") != "250ms" {
		t.Fatal("full ring did not receive the promoted value")
	}
	audits, err := s.ListAudit(ctx, "payments", 1000)
	if err != nil {
		t.Fatalf("audit after promotion: %v", err)
	}
	var itemAudit bool
	for _, entry := range audits {
		if entry.ReleaseID == release.ID && entry.Key == "timeout" &&
			entry.StatusCode == model.CodeOK && entry.OldDigest != "" && entry.NewDigest != "" {
			itemAudit = true
			break
		}
	}
	if !itemAudit {
		t.Fatal("full promotion did not audit the individual item change")
	}
}

func TestAutomaticRollbackAndCanaryFallback(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := newTestStore(t, clock, 20)
	if _, err := s.Bootstrap(ctx, "payments", "seed", map[string]string{
		"timeout": "100ms",
	}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := s.SetClientRing(ctx, "canary-1", model.RingCanary, "test"); err != nil {
		t.Fatalf("set canary ring: %v", err)
	}
	if _, err := s.SetClientRing(ctx, "full-1", model.RingFull, "test"); err != nil {
		t.Fatalf("set full ring: %v", err)
	}
	head, err := s.HeadConfig(ctx, "payments")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	release, err := s.CreateRelease(ctx, ReleaseInput{
		Namespace:          "payments",
		Operator:           "releaser",
		ObservationSeconds: 1,
		Changes: []model.ItemChange{
			{Key: "timeout", Value: "broken"},
		},
	})
	if err != nil {
		t.Fatalf("create release: %v", err)
	}
	if _, err := s.TransitionRelease(ctx, release.ID, "canary", "releaser", ""); err != nil {
		t.Fatalf("canary: %v", err)
	}
	if _, err := s.ReportObservation(ctx, ObservationInput{
		ReleaseID:    release.ID,
		InstanceID:   "canary-1",
		Successes:    0,
		Errors:       10,
		LocalVersion: release.CandidateSnapshot,
	}); err != nil {
		t.Fatalf("report failures: %v", err)
	}
	clock.Advance(2 * time.Second)
	if _, err := s.EvaluateDueReleases(ctx); err != nil {
		t.Fatalf("evaluate rollback: %v", err)
	}
	rolledBack, err := s.GetRelease(ctx, release.ID)
	if err != nil {
		t.Fatalf("get rollback: %v", err)
	}
	if rolledBack.State != model.ReleaseRolledBack {
		t.Fatalf("state=%s want rolled_back", rolledBack.State)
	}
	if rolledBack.RollbackReason == "" || rolledBack.ErrorRate <= 0.005 {
		t.Fatalf("rollback audit reason/rate missing: reason=%q rate=%f",
			rolledBack.RollbackReason, rolledBack.ErrorRate)
	}
	canaryPull, err := s.ResolvePull(ctx, "canary-1", "payments")
	if err != nil {
		t.Fatalf("canary pull after rollback: %v", err)
	}
	fullPull, err := s.ResolvePull(ctx, "full-1", "payments")
	if err != nil {
		t.Fatalf("full pull after rollback: %v", err)
	}
	for name, pull := range map[string]model.PullResponse{"canary": canaryPull, "full": fullPull} {
		if pull.Version != head.Version || itemValue(pull.Items, "timeout") != "100ms" {
			t.Fatalf("%s pull after rollback version=%d value=%q, want version=%d old value",
				name, pull.Version, itemValue(pull.Items, "timeout"), head.Version)
		}
	}
	audits, err := s.ListAudit(ctx, "payments", 100)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	var autoAudit bool
	for _, entry := range audits {
		if entry.ReleaseID == release.ID && entry.Operator == "system:auto-rollback" &&
			entry.StatusCode == model.CodeOK {
			autoAudit = true
		}
	}
	if !autoAudit {
		t.Fatal("automatic rollback audit record missing")
	}
}

func TestConcurrentReadConsistencyAndP95(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := newTestStore(t, clock, 20)
	const itemCount = 300
	values := make(map[string]string, itemCount)
	for i := 0; i < itemCount; i++ {
		values[fmt.Sprintf("param.%03d", i)] = fmt.Sprintf(`{"gen":0,"payload":"base-%03d"}`, i)
	}
	if _, err := s.Bootstrap(ctx, "payments", "seed", values); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	changes := make([]model.ItemChange, 0, itemCount)
	for i := 0; i < itemCount; i++ {
		changes = append(changes, model.ItemChange{
			Key:   fmt.Sprintf("param.%03d", i),
			Value: fmt.Sprintf(`{"gen":1,"payload":"next-%03d"}`, i),
		})
	}
	release, err := s.CreateRelease(ctx, ReleaseInput{
		Namespace:          "payments",
		Operator:           "releaser",
		ObservationSeconds: 1,
		Changes:            changes,
	})
	if err != nil {
		t.Fatalf("create release: %v", err)
	}
	if _, err := s.TransitionRelease(ctx, release.ID, "canary", "releaser", ""); err != nil {
		t.Fatalf("canary: %v", err)
	}
	clock.Advance(2 * time.Second)
	if _, err := s.EvaluateDueReleases(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var reads atomic.Int64
	var failure atomic.Value
	var latencyMu sync.Mutex
	var latencies []float64
	for worker := 0; worker < 6; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				select {
				case <-readCtx.Done():
					return
				default:
				}
				start := time.Now()
				config, err := s.HeadConfig(readCtx, "payments")
				elapsed := float64(time.Since(start).Microseconds()) / 1000
				latencyMu.Lock()
				latencies = append(latencies, elapsed)
				latencyMu.Unlock()
				if err != nil {
					failure.Store(fmt.Sprintf("head read: %v", err))
					return
				}
				if config.Digest != model.DigestItems(config.Items) {
					failure.Store("digest mismatch")
					return
				}
				generation := valueGeneration(itemValue(config.Items, "param.000"))
				for _, item := range config.Items {
					if valueGeneration(item.Value) != generation {
						failure.Store("mixed configuration generations in one read")
						return
					}
				}
				reads.Add(1)
			}
		}()
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := s.TransitionRelease(ctx, release.ID, "full", "releaser", ""); err != nil {
		cancel()
		wg.Wait()
		t.Fatalf("promote full: %v", err)
	}
	wg.Wait()
	cancel()
	if value := failure.Load(); value != nil {
		t.Fatalf("consistency failure: %s", value)
	}
	if reads.Load() != 1800 {
		t.Fatalf("reads=%d want 1800", reads.Load())
	}
	latencyMu.Lock()
	sort.Float64s(latencies)
	p95Index := int(float64(len(latencies))*0.95+0.999999) - 1
	if p95Index < 0 {
		p95Index = 0
	}
	p95 := latencies[p95Index]
	latencyMu.Unlock()
	if p95 > 100 {
		t.Fatalf("read p95=%.3fms exceeds 100ms", p95)
	}
}

func TestFullPromotionFailureLeavesNoPartialWrites(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := newTestStore(t, clock, 20)
	if _, err := s.Bootstrap(ctx, "payments", "seed", map[string]string{
		"timeout": "100ms",
		"limit":   "10",
	}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	head, err := s.HeadConfig(ctx, "payments")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	release, err := s.CreateRelease(ctx, ReleaseInput{
		Namespace:          "payments",
		Operator:           "releaser",
		ObservationSeconds: 1,
		Changes: []model.ItemChange{
			{Key: "timeout", Value: "broken"},
		},
	})
	if err != nil {
		t.Fatalf("create release: %v", err)
	}
	if _, err := s.TransitionRelease(ctx, release.ID, "canary", "releaser", ""); err != nil {
		t.Fatalf("canary: %v", err)
	}
	clock.Advance(2 * time.Second)
	if _, err := s.EvaluateDueReleases(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE snapshot_items SET item_version = 0
		 WHERE snapshot_id = ? AND key = 'timeout'`, release.CandidateSnapshot,
	); err != nil {
		t.Fatalf("corrupt candidate snapshot for failure test: %v", err)
	}
	if _, err := s.TransitionRelease(ctx, release.ID, "full", "releaser", ""); err == nil {
		t.Fatal("full promotion should fail on a non-increasing item version")
	}
	unchanged, err := s.GetRelease(ctx, release.ID)
	if err != nil {
		t.Fatalf("get release: %v", err)
	}
	if unchanged.State != model.ReleaseObservedPass {
		t.Fatalf("failed promotion changed state to %s", unchanged.State)
	}
	currentHead, err := s.HeadConfig(ctx, "payments")
	if err != nil {
		t.Fatalf("head after failed promotion: %v", err)
	}
	if currentHead.Version != head.Version || itemValue(currentHead.Items, "timeout") != "100ms" {
		t.Fatalf("failed promotion partially wrote head: version=%d timeout=%s",
			currentHead.Version, itemValue(currentHead.Items, "timeout"))
	}
	var fullAudits int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log
		 WHERE release_id = ? AND status_code = 'OK' AND details LIKE '%to=full%'`,
		release.ID,
	).Scan(&fullAudits); err != nil {
		t.Fatalf("count full audits: %v", err)
	}
	if fullAudits != 0 {
		t.Fatalf("failed promotion left %d successful full-transition audits", fullAudits)
	}
	var releaseHistory int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM config_item_versions WHERE release_id = ?`,
		release.ID,
	).Scan(&releaseHistory); err != nil {
		t.Fatalf("count release history: %v", err)
	}
	if releaseHistory != 0 {
		t.Fatalf("failed promotion left %d release history rows", releaseHistory)
	}
}

func itemValue(items []model.Item, key string) string {
	for _, item := range items {
		if item.Key == key {
			return item.Value
		}
	}
	return ""
}

func valueGeneration(value string) string {
	if len(value) < 8 {
		return ""
	}
	start := findSubstring(value, `"gen":`)
	if start < 0 {
		return ""
	}
	start += len(`"gen":`)
	end := start
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
	}
	return value[start:end]
}

func findSubstring(value, needle string) int {
	for i := 0; i+len(needle) <= len(value); i++ {
		if value[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
