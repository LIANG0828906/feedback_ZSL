package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"edgegateway/internal/model"
)

type options struct {
	tcpAddr    string
	quicAddr   string
	backendURL string
	duration   time.Duration
	seed       int64
	devices    int
	reportPath string
	stdout     bool
	limitProbe bool
}

type deviceSpec struct {
	index        int
	id           string
	ingress      string
	fps          int
	connectionID string
	initialAddr  string
	altAddr      string
	migrate      bool
	injectReject bool
}

type migrationLocal struct {
	from             string
	to               string
	at               time.Time
	ackBefore        []model.Range
	ackAfter         []model.Range
	ackThroughBefore int64
	ackThroughAfter  int64
}

type worker struct {
	spec        deviceSpec
	client      *deviceClient
	sessionID   string
	resumeToken string
	peerAddr    string

	success        int64
	rejectionResp  int64
	duplicates     int64
	rateLimited    int64
	timeouts       int64
	failedSeqs     map[int64]struct{}
	migrations     []migrationLocal
	lastError      string
	established    bool
	lastRateActive bool
}

type burstOutcome struct {
	ok            bool
	success       int64
	duplicates    int64
	rejectionResp int64
	rateLimited   int64
	timeouts      int64
	seq           int64
	lastRate      bool
}

type app struct {
	opts       options
	specs      []deviceSpec
	workers    []*worker
	start      time.Time
	latency    *latencyCollector
	timelineMu sync.Mutex
	timeline   []TimelineEvent
	http       *http.Client
}

type counters struct {
	success       int64
	rejectionResp int64
	duplicates    int64
	rateLimited   int64
	timeouts      int64
}

func main() {
	opts := options{}
	flag.StringVar(&opts.tcpAddr, "tcp", "127.0.0.1:8443", "gateway TCP ingress")
	flag.StringVar(&opts.quicAddr, "quic", "127.0.0.1:4433", "gateway QUIC ingress")
	flag.StringVar(&opts.backendURL, "backend", "http://127.0.0.1:18080", "backend management URL")
	flag.DurationVar(&opts.duration, "duration", 5*time.Second, "traffic duration")
	flag.Int64Var(&opts.seed, "seed", 309, "fixed generation seed")
	flag.IntVar(&opts.devices, "devices", 24, "number of simulated devices")
	flag.StringVar(&opts.reportPath, "report", "artifacts/scenario-report.json", "JSON report path")
	flag.BoolVar(&opts.stdout, "stdout", false, "also print JSON report")
	flag.BoolVar(&opts.limitProbe, "limit-probe", true, "probe the configured global session limit after the run")
	flag.Parse()
	if opts.devices <= 0 || opts.devices > 256 {
		fmt.Fprintln(os.Stderr, "devices must be in 1..256")
		os.Exit(2)
	}
	if opts.duration < time.Second {
		fmt.Fprintln(os.Stderr, "duration must be at least 1s")
		os.Exit(2)
	}
	a := newApp(opts)
	if err := a.run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "scenario:", err)
		os.Exit(1)
	}
}

func newApp(opts options) *app {
	specs := generateSpecs(opts.seed, opts.devices)
	workers := make([]*worker, len(specs))
	for i := range specs {
		workers[i] = &worker{
			spec: specs[i], peerAddr: specs[i].initialAddr,
			failedSeqs: make(map[int64]struct{}),
		}
	}
	a := &app{
		opts: opts, specs: specs, workers: workers,
		latency: newLatencyCollector(),
		http:    &http.Client{Timeout: 5 * time.Second},
	}
	workerRuntime = runtimeState{
		tcpAddr: opts.tcpAddr, quicAddr: opts.quicAddr, backendURL: opts.backendURL,
		seed: opts.seed, latency: a.latency, http: a.http, app: a,
	}
	return a
}

func generateSpecs(seed int64, count int) []deviceSpec {
	rng := rand.New(rand.NewSource(seed))
	specs := make([]deviceSpec, 0, count)
	migrateN := count / 3
	if migrateN < 1 {
		migrateN = 1
	}
	for i := 0; i < count; i++ {
		ingress := model.IngressTCP
		base := 30
		alt := 40
		if i%2 == 0 {
			ingress = model.IngressQUIC
			base = 20
			alt = 50
		}
		id := fmt.Sprintf("dev-%03d", i+1)
		specs = append(specs, deviceSpec{
			index: i, id: id, ingress: ingress,
			fps:          20 + rng.Intn(101),
			connectionID: fmt.Sprintf("conn-%03d-%08x", i+1, rng.Uint32()),
			initialAddr:  fmt.Sprintf("10.%d.0.%d:41000", base, i+10),
			altAddr:      fmt.Sprintf("10.%d.0.%d:41000", alt, i+10),
			migrate:      i < migrateN,
			injectReject: i == 1 || i == 4,
		})
	}
	return specs
}

func (a *app) run(ctx context.Context) error {
	a.addEvent(time.Now(), "scenario_start", fmt.Sprintf("seed=%d devices=%d", a.opts.seed, a.opts.devices))
	ready := sync.WaitGroup{}
	for _, w := range a.workers {
		ready.Add(1)
		go func(w *worker) {
			defer ready.Done()
			if err := w.establish(ctx); err != nil {
				w.lastError = err.Error()
				a.addEvent(time.Now(), "establish_failed", w.spec.id+": "+err.Error())
				return
			}
			w.established = true
		}(w)
	}
	ready.Wait()
	established := 0
	for _, w := range a.workers {
		if w.established {
			established++
		}
	}
	if established == 0 {
		return fmt.Errorf("no device could establish a session")
	}
	a.addEvent(time.Now(), "sessions_established", fmt.Sprintf("count=%d", established))

	a.start = time.Now()
	var trafficWG sync.WaitGroup
	for _, w := range a.workers {
		if !w.established {
			continue
		}
		trafficWG.Add(1)
		go func(w *worker) {
			defer trafficWG.Done()
			w.runTraffic(ctx, a.start, a.opts.duration)
		}(w)
	}
	coordDone := make(chan struct{})
	go a.runCoordinator(ctx, coordDone)
	trafficWG.Wait()
	close(coordDone)
	elapsed := time.Since(a.start)
	a.addEvent(time.Now(), "traffic_finished", fmt.Sprintf("elapsed_ms=%d", elapsed.Milliseconds()))

	report := &Report{
		Schema: "edge.scenario.report.v1", Seed: a.opts.seed, GeneratedAt: time.Now(),
		DurationSeconds: elapsed.Seconds(),
		Window:          "Windows 11, Go 1.22, 2 logical CPUs, 1.25 GiB RAM",
		DeviceCount:     a.opts.devices, MigratedDevices: countMigrated(a.specs),
		Ingresses: map[string]IngressReport{
			model.IngressQUIC: {Protocols: map[string]int{}},
			model.IngressTCP:  {Protocols: map[string]int{}},
		},
		Notes: []string{
			"quic-go v0.42 disables QUIC transport migration; address changes rebuild transport while the signed session token preserves ack state.",
			"Rate limits are evaluated per session and per ingress; QUIC and TCP samples are never combined.",
			"Forward P99 is measured from fragment request start to gateway response on the generator.",
		},
	}
	for _, w := range a.workers {
		sr := w.collectSessionReport(ctx)
		report.Sessions = append(report.Sessions, sr)
	}
	sort.Slice(report.Sessions, func(i, j int) bool { return report.Sessions[i].DeviceID < report.Sessions[j].DeviceID })
	a.aggregate(report)
	report.Backend = a.collectBackendReport(ctx)
	report.SessionLimit = a.probeSessionLimit(ctx, report)
	report.Timeline = append([]TimelineEvent(nil), a.timeline...)
	sort.Slice(report.Timeline, func(i, j int) bool { return report.Timeline[i].At.Before(report.Timeline[j].At) })
	if err := writeReport(a.opts.reportPath, report, a.opts.stdout); err != nil {
		return err
	}
	fmt.Printf("report=%s sessions=%d migrations=%d p99_ms=%.2f\n",
		a.opts.reportPath, len(report.Sessions),
		report.Ingresses[model.IngressQUIC].MigrationCount+report.Ingresses[model.IngressTCP].MigrationCount,
		report.Ingresses[model.IngressQUIC].ForwardP99MS)
	for _, w := range a.workers {
		if w.client != nil {
			w.client.close()
		}
	}
	return nil
}

func countMigrated(specs []deviceSpec) int {
	n := 0
	for _, s := range specs {
		if s.migrate {
			n++
		}
	}
	return n
}

func (a *app) runCoordinator(ctx context.Context, done <-chan struct{}) {
	timer := time.NewTimer(time.Duration(float64(a.opts.duration) * 0.15))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-done:
		return
	case <-timer.C:
	}
	delayMS := int64(140)
	durationMS := int64(float64(a.opts.duration/time.Millisecond) * 0.32)
	if durationMS < 600 {
		durationMS = 600
	}
	if err := a.postAdmin(ctx, "/admin/delay", map[string]any{
		"delay_ms": delayMS, "duration_ms": durationMS,
	}); err != nil {
		a.addEvent(time.Now(), "delay_injection_failed", err.Error())
		return
	}
	a.addEvent(time.Now(), "backend_delay_injected", fmt.Sprintf("delay_ms=%d duration_ms=%d", delayMS, durationMS))
}

func (w *worker) establish(ctx context.Context) error {
	client, err := newDeviceClient(w.spec.ingress, w.optsTCP(), w.optsQUIC())
	if err != nil {
		return err
	}
	w.client = client
	body := map[string]any{
		"device_id": w.spec.id, "connection_id": w.spec.connectionID,
	}
	status, raw, _, err := client.doJSON(ctx, http.MethodPost, "/v1/sessions", body, w.peerAddr)
	if err != nil {
		return err
	}
	var env sessionEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	if status != http.StatusOK || env.Session == nil {
		return fmt.Errorf("establish status=%d code=%s error=%s", status, env.Code, env.Error)
	}
	w.sessionID = env.SessionID
	if w.sessionID == "" && env.Session != nil {
		w.sessionID = env.Session.ID
	}
	w.resumeToken = env.ResumeToken
	return nil
}

func (w *worker) optsTCP() string {
	// The parameters are substituted by app when workers are initialized.
	return workerRuntime.tcpAddr
}

func (w *worker) optsQUIC() string {
	return workerRuntime.quicAddr
}

func (w *worker) runTraffic(ctx context.Context, start time.Time, duration time.Duration) {
	period := time.Second / time.Duration(w.spec.fps)
	next := start
	deadline := start.Add(duration)
	seq := int64(0)
	nextMigration := 0
	injectedReject := false
	burstDone := false
	migrationAt := []float64{0.40, 0.68}
	for time.Now().Before(deadline) {
		now := time.Now()
		if now.Before(next) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(next)):
			}
		}
		now = time.Now()
		if !now.Before(deadline) {
			break
		}
		progress := now.Sub(start).Seconds() / duration.Seconds()
		if w.spec.migrate && nextMigration < len(migrationAt) && progress >= migrationAt[nextMigration] {
			to := w.spec.initialAddr
			if nextMigration == 0 {
				to = w.spec.altAddr
			}
			w.migrate(ctx, to)
			nextMigration++
		}
		if w.spec.injectReject && !injectedReject && progress >= 0.52 {
			if err := workerRuntime.postAdmin(ctx, "/admin/reject", map[string]any{
				"device_ids": []string{w.spec.id}, "seqs": []int64{seq},
				"duration_ms": 300, "reason": "deterministic_fragment_reject",
			}); err == nil {
				workerRuntime.addEvent(time.Now(), "fragment_reject_injected",
					fmt.Sprintf("device=%s seq=%d", w.spec.id, seq))
			}
			injectedReject = true
		}
		if !burstDone && (w.spec.index == 10 || w.spec.index == 11) && progress >= 0.72 && !w.lastRateActive {
			w.burstFragments(ctx, seq, 6, deadline)
			seq += 6
			burstDone = true
			next = next.Add(6 * period)
			if next.Before(time.Now()) {
				next = time.Now().Add(period)
			}
			continue
		}
		if !w.sendFragment(ctx, seq, deadline) {
			break
		}
		seq++
		next = next.Add(period)
		if next.Before(time.Now()) {
			next = time.Now().Add(period)
		}
	}
}

func (w *worker) sendFragment(ctx context.Context, seq int64, deadline time.Time) bool {
	body := w.fragmentBody(seq)
	retryDeadline := deadline.Add(2 * time.Second)
	for attempt := 0; attempt < 32; attempt++ {
		status, raw, latency, err := w.client.doJSON(ctx, http.MethodPost, "/v1/fragments", body, w.peerAddr)
		if err != nil {
			w.lastError = err.Error()
			if time.Now().After(retryDeadline) {
				w.markFailed(seq)
				return false
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		var reply fragmentReply
		if err := json.Unmarshal(raw, &reply); err != nil {
			w.lastError = "decode response: " + err.Error()
			fmt.Fprintf(os.Stderr, "decode %s seq=%d: %v body=%s\n", w.spec.id, seq, err, string(raw))
			time.Sleep(20 * time.Millisecond)
			continue
		}
		procMS := reply.ProcessingMS
		if procMS <= 0 {
			procMS = float64(latency.Microseconds()) / 1000
		}
		workerRuntime.latency.add(w.spec.ingress, procMS)
		if status == http.StatusOK && reply.Accepted {
			w.success++
			w.lastRateActive = reply.RateLimitActive
			if reply.Duplicate {
				w.duplicates++
			}
			return true
		}
		switch {
		case status == http.StatusUnprocessableEntity || reply.Code == model.CodeBackendRejected:
			w.rejectionResp++
			w.markFailed(seq)
			time.Sleep(340 * time.Millisecond)
		case status == http.StatusTooManyRequests:
			w.rateLimited++
			w.lastRateActive = true
			wait := time.Duration(reply.RetryAfterMS) * time.Millisecond
			if wait <= 0 {
				wait = 50 * time.Millisecond
			}
			time.Sleep(wait)
		case status == http.StatusConflict:
			time.Sleep(20 * time.Millisecond)
		case status == http.StatusGatewayTimeout:
			w.timeouts++
			w.markFailed(seq)
			time.Sleep(50 * time.Millisecond)
		default:
			w.lastError = fmt.Sprintf("status=%d code=%s error=%s", status, reply.Code, reply.Error)
			w.markFailed(seq)
			time.Sleep(30 * time.Millisecond)
		}
		if time.Now().After(retryDeadline) {
			return false
		}
	}
	w.markFailed(seq)
	return false
}

func (w *worker) fragmentBody(seq int64) map[string]any {
	size := fragmentSize(workerRuntime.seed, w.spec.id, seq)
	payload := makeFragment(workerRuntime.seed, w.spec.id, seq, size)
	return map[string]any{
		"schema": "edge.fragment.v1", "session_id": w.sessionID,
		"device_id": w.spec.id, "connection_id": w.spec.connectionID,
		"seq": seq, "size": size,
		"data_base64": base64.StdEncoding.EncodeToString(payload),
	}
}

func (w *worker) burstFragments(ctx context.Context, startSeq int64, count int, deadline time.Time) {
	results := make(chan burstOutcome, count)
	for i := 0; i < count; i++ {
		go func(seq int64) {
			out := burstOutcome{seq: seq}
			body := w.fragmentBody(seq)
			retryDeadline := deadline.Add(2 * time.Second)
			for attempt := 0; attempt < 12; attempt++ {
				status, raw, latency, err := w.client.doJSON(ctx, http.MethodPost, "/v1/fragments", body, w.peerAddr)
				if err != nil {
					if time.Now().After(retryDeadline) {
						results <- out
						return
					}
					time.Sleep(15 * time.Millisecond)
					continue
				}
				var reply fragmentReply
				if json.Unmarshal(raw, &reply) != nil {
					results <- out
					return
				}
				procMS := reply.ProcessingMS
				if procMS <= 0 {
					procMS = float64(latency.Microseconds()) / 1000
				}
				workerRuntime.latency.add(w.spec.ingress, procMS)
				out.lastRate = reply.RateLimitActive
				if status == http.StatusOK && reply.Accepted {
					out.ok = true
					out.success = 1
					if reply.Duplicate {
						out.duplicates = 1
					}
					results <- out
					return
				}
				switch {
				case status == http.StatusTooManyRequests:
					out.rateLimited++
					wait := time.Duration(reply.RetryAfterMS) * time.Millisecond
					if wait <= 0 {
						wait = 20 * time.Millisecond
					}
					time.Sleep(wait)
				case status == http.StatusUnprocessableEntity:
					out.rejectionResp++
					time.Sleep(50 * time.Millisecond)
				case status == http.StatusGatewayTimeout:
					out.timeouts++
					time.Sleep(30 * time.Millisecond)
				default:
					time.Sleep(20 * time.Millisecond)
				}
				if time.Now().After(retryDeadline) {
					break
				}
			}
			results <- out
		}(startSeq + int64(i))
	}
	for i := 0; i < count; i++ {
		out := <-results
		w.success += out.success
		w.duplicates += out.duplicates
		w.rejectionResp += out.rejectionResp
		w.rateLimited += out.rateLimited
		w.timeouts += out.timeouts
		if !out.ok {
			w.markFailed(out.seq)
		}
		if out.lastRate {
			w.lastRateActive = true
		}
	}
	workerRuntime.addEvent(time.Now(), "burst_sent",
		fmt.Sprintf("device=%s ingress=%s first_seq=%d count=%d", w.spec.id, w.spec.ingress, startSeq, count))
}

func (w *worker) markFailed(seq int64) {
	w.failedSeqs[seq] = struct{}{}
}

func (w *worker) migrate(ctx context.Context, to string) {
	before, err := w.getSession(ctx)
	if err != nil {
		w.lastError = "pre-migration query: " + err.Error()
	}
	from := w.peerAddr
	ackBefore := []model.Range(nil)
	ackThroughBefore := int64(-1)
	if before != nil {
		ackBefore = append(ackBefore, before.AckRanges...)
		ackThroughBefore = before.AckThrough
	}
	if w.client != nil {
		w.client.close()
	}
	client, err := newDeviceClient(w.spec.ingress, workerRuntime.tcpAddr, workerRuntime.quicAddr)
	if err != nil {
		w.lastError = "new transport: " + err.Error()
		return
	}
	w.client = client
	w.peerAddr = to
	status, raw, _, err := client.doJSON(ctx, http.MethodPost, "/v1/sessions/attach", map[string]any{
		"device_id": w.spec.id, "resume_token": w.resumeToken,
	}, w.peerAddr)
	if err != nil {
		w.lastError = "attach: " + err.Error()
		return
	}
	var env sessionEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		w.lastError = "decode attach: " + err.Error()
		return
	}
	if status != http.StatusOK || env.Session == nil {
		w.lastError = fmt.Sprintf("attach status=%d code=%s error=%s", status, env.Code, env.Error)
		return
	}
	ackAfter := append([]model.Range(nil), env.Session.AckRanges...)
	m := migrationLocal{
		from: from, to: to, at: time.Now(), ackBefore: ackBefore, ackAfter: ackAfter,
		ackThroughBefore: ackThroughBefore, ackThroughAfter: env.Session.AckThrough,
	}
	w.migrations = append(w.migrations, m)
	preserved := rangesSubset(ackBefore, ackAfter) && env.Session.AckThrough >= ackThroughBefore
	workerRuntime.addEvent(time.Now(), "session_migrated",
		fmt.Sprintf("device=%s ingress=%s from=%s to=%s ack_preserved=%t transport_rebuilt=true",
			w.spec.id, w.spec.ingress, from, to, preserved))
}

func (w *worker) getSession(ctx context.Context) (*model.Session, error) {
	status, raw, _, err := w.client.doJSON(ctx, http.MethodGet, "/v1/sessions/"+w.sessionID, nil, w.peerAddr)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("session query status %d", status)
	}
	var detail sessionDetailReply
	if err := json.Unmarshal(raw, &detail); err != nil {
		return nil, err
	}
	return detail.Session, nil
}

func (w *worker) collectSessionReport(ctx context.Context) SessionReport {
	sr := SessionReport{
		DeviceID: w.spec.id, Ingress: w.spec.ingress, ConfiguredFPS: w.spec.fps,
		SuccessFragments: w.success, FailedFragments: int64(len(w.failedSeqs)),
		RejectionResponses: w.rejectionResp, DuplicateResponses: w.duplicates,
		RateLimitedRequests: w.rateLimited, BackendTimeouts: w.timeouts,
	}
	if w.client != nil {
		sr.Protocol = w.client.negotiatedProtocol()
	}
	if w.sessionID == "" {
		return sr
	}
	sr.ID = w.sessionID
	status, raw, _, err := w.client.doJSON(ctx, http.MethodGet, "/v1/sessions/"+w.sessionID, nil, w.peerAddr)
	if err != nil || status != http.StatusOK {
		return sr
	}
	var detail sessionDetailReply
	if err := json.Unmarshal(raw, &detail); err != nil || detail.Session == nil {
		return sr
	}
	sess := detail.Session
	sr.Active = sess.Active
	sr.MigrationCount = sess.MigrationCount
	sr.SuccessFragments = sess.SuccessCount
	sr.RejectionResponses = sess.RejectionCount
	sr.BackendBacklogPeak = sess.BacklogPeak
	sr.SendWindowPeak = sess.SendWindowPeak
	sr.BackendTimeouts = sess.TimeoutCount
	sr.FinalAckThrough = sess.AckThrough
	sr.AcceptedRanges = sess.AckRanges
	sr.Rejections = detail.Rejections
	for _, ev := range detail.Feedback {
		if ev.Source != "backend_backpressure" {
			continue
		}
		sr.RateLimitEvents = append(sr.RateLimitEvents, RateLimitEventReport{
			At: time.Unix(0, ev.AtUnix), Source: ev.Source, Basis: ev.Basis,
			Reason: ev.Reason, Backlog: ev.Backlog, RTTMedianMS: ev.RTTMedianMS,
			PriorRate: ev.PriorRate, NewRate: ev.NewRate,
		})
	}
	sort.Slice(detail.Migrations, func(i, j int) bool { return detail.Migrations[i].ID < detail.Migrations[j].ID })
	allPreserved := len(detail.Migrations) > 0
	for i, gm := range detail.Migrations {
		preserved := rangesSubset(gm.AckRanges, sess.AckRanges) && sess.AckThrough >= gm.AckThrough
		allPreserved = allPreserved && preserved
		mr := MigrationReport{
			FromAddress: gm.FromAddr, ToAddress: gm.ToAddr, At: time.Unix(0, gm.AtUnix),
			AckThroughBefore: gm.AckThrough, AckThroughAfter: sess.AckThrough,
			AckRangesPreserved: preserved, PreservedLimit: gm.PreservedLimited,
			PreservedRate: gm.PreservedRate, PreservedReason: gm.PreservedReason,
			Trigger: gm.Trigger, TransportRebuilt: true,
		}
		if i < len(w.migrations) {
			local := w.migrations[i]
			mr.LocalAckBefore = local.ackBefore
			mr.LocalAckAfter = local.ackAfter
			mr.AckThroughBefore = local.ackThroughBefore
			mr.AckThroughAfter = local.ackThroughAfter
			mr.AckRangesPreserved = mr.AckRangesPreserved && rangesSubset(local.ackBefore, local.ackAfter)
		}
		sr.Migrations = append(sr.Migrations, mr)
	}
	sr.AckPreservedAcrossAllMigrations = allPreserved
	if len(detail.Migrations) == 0 {
		sr.AckPreservedAcrossAllMigrations = true
	}
	return sr
}

func (a *app) aggregate(report *Report) {
	for _, s := range report.Sessions {
		ir := report.Ingresses[s.Ingress]
		if ir.RateLimitEvents == 0 {
			ir.AllLimitsCausedByBackend = true
		}
		ir.SessionCount++
		if s.Protocol != "" {
			ir.Protocols[s.Protocol]++
		}
		ir.MigrationCount += s.MigrationCount
		ir.SuccessFragments += s.SuccessFragments
		ir.FailedFragments += s.FailedFragments
		ir.RejectionResponses += s.RejectionResponses
		ir.DuplicateResponses += s.DuplicateResponses
		ir.RateLimitedRequests += s.RateLimitedRequests
		ir.BackendTimeoutCount += s.BackendTimeouts
		if s.BackendBacklogPeak > ir.BackendBacklogPeak {
			ir.BackendBacklogPeak = s.BackendBacklogPeak
		}
		if s.MigrationCount > 0 {
			ir.SessionsWithMigration++
			if s.AckPreservedAcrossAllMigrations {
				ir.MigrationAckPreservedSessions++
			}
			for _, m := range s.Migrations {
				if m.PreservedLimit {
					ir.MigrationsWithPreservedLimit++
				}
			}
		}
		if len(s.RateLimitEvents) > 0 {
			ir.RateLimitedSessions++
		}
		for _, ev := range s.RateLimitEvents {
			ir.RateLimitEvents++
			if ev.Source != "backend_backpressure" || ev.Basis == "" {
				ir.AllLimitsCausedByBackend = false
			}
		}
		report.Ingresses[s.Ingress] = ir
	}
	for ingress, ir := range report.Ingresses {
		if ir.SessionsWithMigration > 0 {
			ir.MigrationAckPreservedRatio = float64(ir.MigrationAckPreservedSessions) / float64(ir.SessionsWithMigration)
		}
		if ir.MigrationCount > 0 {
			ir.MigrationLimitPreservedRatio = float64(ir.MigrationsWithPreservedLimit) / float64(ir.MigrationCount)
		}
		ir.ForwardP99MS, ir.ForwardSamples = a.latency.p99(ingress)
		report.Ingresses[ingress] = ir
	}
}

func (a *app) collectBackendReport(ctx context.Context) BackendReport {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.opts.backendURL, "/")+"/admin/state", nil)
	if err != nil {
		return BackendReport{}
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return BackendReport{}
	}
	defer resp.Body.Close()
	var m map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m) != nil {
		return BackendReport{}
	}
	return BackendReport{
		Requests: int64(num(m["requests"])), Accepted: int64(num(m["accepted"])),
		Rejected: int64(num(m["rejected"])), Duplicates: int64(num(m["duplicates"])),
		InFlightPeak: int(num(m["in_flight_peak"])), ActiveRejectRules: int(num(m["active_reject_rules"])),
	}
}

func (a *app) probeSessionLimit(ctx context.Context, report *Report) SessionLimitReport {
	out := SessionLimitReport{}
	// Get the gateway's configured limit and active count from the API.
	var probeClient *deviceClient
	for _, w := range a.workers {
		if w.established {
			probeClient = w.client
			break
		}
	}
	if probeClient == nil {
		return out
	}
	code, raw, _, err := probeClient.doJSON(ctx, http.MethodGet, "/v1/sessions", nil, "10.99.0.1:41000")
	if err == nil && code == http.StatusOK {
		var list struct {
			Count int `json:"count"`
		}
		_ = json.Unmarshal(raw, &list)
		out.ObservedActive = list.Count
	}
	if cfgStatus, cfgRaw, _, err := probeClient.doJSON(ctx, http.MethodGet, "/v1/config", nil, "10.99.0.1:41000"); err == nil && cfgStatus == 200 {
		var cfg struct {
			MaxSessions int `json:"max_sessions"`
		}
		_ = json.Unmarshal(cfgRaw, &cfg)
		out.ConfiguredMax = cfg.MaxSessions
	}
	out.Respected = out.ConfiguredMax == 0 || out.ObservedActive <= out.ConfiguredMax
	if !a.opts.limitProbe {
		return out
	}
	out.ProbeAttempted = true
	status, raw, _, err := probeClient.doJSON(ctx, http.MethodPost, "/v1/sessions", map[string]any{
		"device_id": "limit-probe", "connection_id": "limit-probe-conn",
	}, "10.99.0.1:41000")
	if err != nil {
		return out
	}
	out.OverLimitHTTPStatus = status
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &body)
	out.OverLimitBusinessCode = body.Code
	if status == http.StatusServiceUnavailable && body.Code == model.CodeSessionLimit {
		out.Respected = true
	} else if status == http.StatusOK {
		out.Respected = false
	}
	return out
}

func (a *app) postAdmin(ctx context.Context, path string, body any) error {
	return workerRuntime.postAdmin(ctx, path, body)
}

func (a *app) addEvent(at time.Time, event, detail string) {
	a.timelineMu.Lock()
	a.timeline = append(a.timeline, TimelineEvent{At: at, Event: event, Detail: detail})
	a.timelineMu.Unlock()
}

func (w *worker) postAdmin(ctx context.Context, path string, body any) error {
	return workerRuntime.postAdmin(ctx, path, body)
}

func (r *runtimeState) postAdmin(ctx context.Context, path string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.backendURL, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("admin %s status=%d body=%s", path, resp.StatusCode, string(body))
	}
	return nil
}

func (r *runtimeState) addEvent(at time.Time, event, detail string) {
	if r.app != nil {
		r.app.addEvent(at, event, detail)
	}
}

type runtimeState struct {
	tcpAddr    string
	quicAddr   string
	backendURL string
	seed       int64
	latency    *latencyCollector
	http       *http.Client
	app        *app
}

var workerRuntime runtimeState

func fragmentSize(seed int64, device string, seq int64) int {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(seed))
	binary.BigEndian.PutUint64(b[8:], uint64(seq))
	h := sha256.Sum256(append([]byte(device+"|"), b[:]...))
	return 512 + int(binary.BigEndian.Uint32(h[:4])%3585)
}

func makeFragment(seed int64, device string, seq int64, size int) []byte {
	out := make([]byte, size)
	var off int
	var block uint32
	for off < size {
		var b [20]byte
		binary.BigEndian.PutUint64(b[:8], uint64(seed))
		binary.BigEndian.PutUint64(b[8:16], uint64(seq))
		binary.BigEndian.PutUint32(b[16:], block)
		h := sha256.Sum256(append([]byte(device+"|"), b[:]...))
		off += copy(out[off:], h[:])
		block++
	}
	return out
}

func rangesSubset(sub, super []model.Range) bool {
	for _, r := range sub {
		if r.Start < 0 {
			continue
		}
		covered := false
		for _, s := range super {
			if s.Start <= r.Start && s.End >= r.End {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}
