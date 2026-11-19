package gateway

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"edgegateway/internal/model"
	"edgegateway/internal/store"
)

type fragmentResult struct {
	status int
	body   map[string]any
}

type fragmentCall struct {
	done   chan struct{}
	result fragmentResult
}

type sessionRuntime struct {
	mu             sync.Mutex
	id             string
	lastAddr       string
	lastHost       string
	inflight       map[int64]*fragmentCall
	blocked        bool
	blockedSeq     int64
	completed      bool
	migrationUntil int64
	ackRanges      []model.Range
	backlog        int
	backlogPeak    int
	sendWindowPeak int
	rtts           []float64
	rateActive     bool
	rate           float64
	tokens         float64
	lastRefill     time.Time
	clean          int
}

type startOutcome struct {
	call       *fragmentCall
	wait       *fragmentCall
	status     int
	code       string
	message    string
	retryAfter time.Duration
	duplicate  bool
}

type runtimeStats struct {
	RTTSamples     []float64
	RTTMedianMS    float64
	BacklogPeak    int
	SendWindowPeak int
	Backlog        int
	SendWindow     int
	RateActive     bool
	Rate           float64
	RetryAfter     time.Duration
	Migration      bool
}

func (g *Gateway) runtime(sess *model.Session) *sessionRuntime {
	g.mu.Lock()
	defer g.mu.Unlock()
	rt := g.rt[sess.ID]
	if rt == nil {
		rt = &sessionRuntime{
			id: sess.ID, lastAddr: sess.LastAddr, lastHost: sess.LastAddrHost,
			inflight: make(map[int64]*fragmentCall), rateActive: sess.LimitActive,
			rate: sess.RateLimitPerSec, lastRefill: time.Now(),
			blocked: sess.Blocked, blockedSeq: sess.BlockedSeq,
			completed:      sess.Completed || !sess.Active,
			migrationUntil: sess.MigrationUntilUnix,
			ackRanges:      append([]model.Range(nil), sess.AckRanges...),
		}
		if rt.rateActive {
			rt.tokens = 0
		} else {
			rt.tokens = 1
		}
		g.rt[sess.ID] = rt
	}
	return rt
}

func (g *Gateway) forgetRuntime(id string) {
	g.mu.Lock()
	delete(g.rt, id)
	g.mu.Unlock()
}

func (rt *sessionRuntime) mergeSession(sess *model.Session) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.mergeSessionLocked(sess)
}

func (rt *sessionRuntime) mergeSessionLocked(sess *model.Session) {
	if sess.Blocked {
		rt.blocked, rt.blockedSeq = true, sess.BlockedSeq
	}
	if sess.Completed || !sess.Active {
		rt.completed = true
	}
	if sess.MigrationUntilUnix > rt.migrationUntil {
		rt.migrationUntil = sess.MigrationUntilUnix
	}
	rt.ackRanges = model.MergeRanges(rt.ackRanges, sess.AckRanges)
}

// applySession is used after the current request commits authoritative state.
// Unlike mergeSession it may clear a rejection block after a successful retry.
func (rt *sessionRuntime) applySession(sess *model.Session) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.blocked, rt.blockedSeq = sess.Blocked, sess.BlockedSeq
	rt.completed = sess.Completed || !sess.Active
	if sess.MigrationUntilUnix > rt.migrationUntil {
		rt.migrationUntil = sess.MigrationUntilUnix
	}
	rt.ackRanges = model.MergeRanges(rt.ackRanges, sess.AckRanges)
}

func (rt *sessionRuntime) syncLimit(active bool, rate float64) {
	if rt.rateActive == active && (active == false || rt.rate == rate) {
		return
	}
	rt.rateActive = active
	rt.rate = rate
	if active {
		rt.tokens = 0
	} else {
		rt.tokens = 1
	}
	rt.clean = 0
	rt.lastRefill = time.Now()
}

func (rt *sessionRuntime) observeAddress(ctx context.Context, st *store.Store, addr, host string, countMigration bool, window time.Duration, seq int64, trigger string) (*model.Session, *model.Migration, bool, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if addr == "" || (addr == rt.lastAddr && host == rt.lastHost) {
		return nil, nil, false, nil
	}
	sess, mig, err := st.ObserveAddress(ctx, rt.id, addr, host, countMigration, window, seq, trigger)
	if err != nil {
		return nil, nil, false, err
	}
	rt.lastAddr, rt.lastHost = sess.LastAddr, sess.LastAddrHost
	rt.mergeSessionLocked(sess)
	return sess, mig, mig != nil, nil
}

// beginFragment serializes same-sequence retries, while still allowing
// different sequences of one session to occupy the bounded send window.
func (g *Gateway) beginFragment(rt *sessionRuntime, seq int64) startOutcome {
	now := time.Now()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.refill(now)
	if call := rt.inflight[seq]; call != nil {
		return startOutcome{wait: call}
	}
	if model.Contains(rt.ackRanges, seq) {
		return startOutcome{duplicate: true}
	}
	if rt.completed {
		return startOutcome{status: 410, code: model.CodeSessionCompleted, message: "session is completed"}
	}
	if rt.blocked && seq != rt.blockedSeq {
		return startOutcome{status: 409, code: model.CodeSessionPaused, message: "session paused after backend rejection"}
	}
	if len(rt.inflight) >= g.cfg.MaxSessionInflight {
		return startOutcome{status: 429, code: model.CodeSendWindow, message: "session send window is exhausted"}
	}
	if rt.rateActive && rt.rate > 0 {
		if rt.tokens < 1 {
			wait := time.Duration((1 - rt.tokens) / rt.rate * float64(time.Second))
			if wait < time.Millisecond {
				wait = time.Millisecond
			}
			return startOutcome{status: 429, code: model.CodeRateLimited, message: "backend backpressure rate limit is active", retryAfter: wait}
		}
		rt.tokens--
	}
	call := &fragmentCall{done: make(chan struct{})}
	rt.inflight[seq] = call
	rt.backlog = len(rt.inflight)
	if rt.backlog > rt.backlogPeak {
		rt.backlogPeak = rt.backlog
	}
	if rt.backlog > rt.sendWindowPeak {
		rt.sendWindowPeak = rt.backlog
	}
	return startOutcome{call: call}
}

func (rt *sessionRuntime) refill(now time.Time) {
	if !rt.rateActive || rt.rate <= 0 {
		rt.tokens = 1
		rt.lastRefill = now
		return
	}
	if rt.lastRefill.IsZero() {
		rt.lastRefill = now
		return
	}
	elapsed := now.Sub(rt.lastRefill).Seconds()
	rt.tokens += elapsed * rt.rate
	if rt.tokens > 1 {
		rt.tokens = 1
	}
	rt.lastRefill = now
}

func (rt *sessionRuntime) finish(call *fragmentCall, seq int64, result fragmentResult) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.inflight[seq] == call {
		delete(rt.inflight, seq)
	}
	rt.backlog = len(rt.inflight)
	call.result = result
	close(call.done)
}

func (rt *sessionRuntime) addRTT(sample time.Duration) {
	rt.mu.Lock()
	rt.addRTTLocked(sample)
	rt.mu.Unlock()
}

func (rt *sessionRuntime) addRTTLocked(sample time.Duration) {
	if sample < 0 {
		return
	}
	rt.rtts = append(rt.rtts, float64(sample.Microseconds())/1000)
	if len(rt.rtts) > 16 {
		rt.rtts = append([]float64(nil), rt.rtts[len(rt.rtts)-16:]...)
	}
}

func (rt *sessionRuntime) statsLocked() ([]float64, float64, int, int) {
	samples := append([]float64(nil), rt.rtts...)
	recent := samples
	if len(recent) > 8 {
		recent = recent[len(recent)-8:]
	}
	return samples, medianFloat(recent), rt.backlog, rt.backlogPeak
}

func (rt *sessionRuntime) statistics() runtimeStats {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	samples, med, _, _ := rt.statsLocked()
	return runtimeStats{
		RTTSamples: samples, RTTMedianMS: med, BacklogPeak: rt.backlogPeak,
		SendWindowPeak: rt.sendWindowPeak,
		Backlog:        rt.backlog, SendWindow: len(rt.inflight),
		RateActive: rt.rateActive, Rate: rt.rate,
	}
}

// evaluateFeedback is called only by the owner of a completed backend request.
// During a migration window the existing limit is deliberately frozen; no
// other session's samples participate in this decision.
func (g *Gateway) evaluateFeedback(rt *sessionRuntime, sess *model.Session, timeout bool) (*model.FeedbackEvent, runtimeStats) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	samples, med, backlog, peak := rt.statsLocked()
	stats := runtimeStats{
		RTTSamples: samples, BacklogPeak: peak, SendWindowPeak: rt.sendWindowPeak, Backlog: backlog,
		SendWindow: len(rt.inflight), RateActive: rt.rateActive, Rate: rt.rate,
		RTTMedianMS: med,
	}
	if timeout {
		stats.Backlog = backlog
	}
	now := time.Now().UnixNano()
	migrationUntil := sess.MigrationUntilUnix
	if rt.migrationUntil > migrationUntil {
		migrationUntil = rt.migrationUntil
	}
	if now < migrationUntil {
		stats.Migration = true
		return nil, stats
	}
	overBacklog := backlog > g.cfg.BacklogThreshold
	overRTT := med > g.cfg.RTTThresholdMS
	if overBacklog || overRTT {
		rt.clean = 0
		if rt.rateActive && rt.rate == g.cfg.RateLimitPerSec {
			stats.RateActive, stats.Rate = true, rt.rate
			stats.RetryAfter = retryAfter(rt.rate, rt.tokens)
			return nil, stats
		}
		basis := feedbackBasis(overBacklog, overRTT)
		reason := "backend_backlog_threshold_exceeded"
		if overBacklog && overRTT {
			reason = "backend_backlog_and_rtt_threshold_exceeded"
		} else if overRTT {
			reason = "rtt_median_threshold_exceeded"
		}
		event, err := g.store.RecordFeedback(context.Background(), sess, true, reason, basis, g.cfg.RateLimitPerSec, backlog, med)
		if err != nil {
			g.logError("record feedback", "session", sess.ID, "error", err)
			return nil, stats
		}
		rt.syncLimit(true, g.cfg.RateLimitPerSec)
		rt.lastRefill = time.Now()
		stats.RateActive, stats.Rate = true, rt.rate
		stats.RetryAfter = retryAfter(rt.rate, rt.tokens)
		return event, stats
	}
	if !rt.rateActive {
		rt.clean = 0
		return nil, stats
	}
	rt.clean++
	if rt.clean < g.cfg.FeedbackRecoveryN {
		stats.RetryAfter = retryAfter(rt.rate, rt.tokens)
		return nil, stats
	}
	event, err := g.store.RecordFeedback(context.Background(), sess, false, "thresholds_clear", "rtt_and_backlog_below_threshold", 0, backlog, med)
	if err != nil {
		g.logError("record feedback recovery", "session", sess.ID, "error", err)
		return nil, stats
	}
	rt.syncLimit(false, 0)
	stats.RateActive, stats.Rate = false, 0
	return event, stats
}

func retryAfter(rate, tokens float64) time.Duration {
	if rate <= 0 {
		return 0
	}
	v := (1 - tokens) / rate
	if v <= 0 {
		return 0
	}
	return time.Duration(v * float64(time.Second))
}

func feedbackBasis(backlog, rtt bool) string {
	switch {
	case backlog && rtt:
		return "rtt_median_and_backlog"
	case rtt:
		return "rtt_median"
	default:
		return "backend_backlog"
	}
}

func medianFloat(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	cp := append([]float64(nil), v...)
	sort.Float64s(cp)
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}

var errMissingSession = errors.New("missing session")
