package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"example.com/dualring-config/internal/clientstate"
	"example.com/dualring-config/internal/model"
)

type options struct {
	server         string
	namespace      string
	instances      int
	instancePrefix string
	poll           time.Duration
	pollCount      int
	duration       time.Duration
	seed           int64
	fault5xxRate   float64
	networkErrRate float64
	latencyProb    float64
	latencyMin     time.Duration
	latencyMax     time.Duration
	applyErrorRate float64
	stateDB        string
	reportPath     string
	logPath        string
	readWorkers    int
	readInterval   time.Duration
	readMinItems   int
	requestTimeout time.Duration
	maxProcs       int
	memoryLimitMB  int64
}

type instance struct {
	index int
	id    string
	opts  *options
	rng   *rand.Rand
	local *clientstate.Store
	http  *http.Client

	snapshot         clientstate.Snapshot
	hasSnapshot      bool
	online           bool
	offline          bool
	offlineFallbacks int64
	pendingOffline   bool
	successes        int64
	errors           int64
	countersRelease  string
	pullCount        int64
	successfulPulls  int64
	reportCount      int64
	digestFailures   int64
	lastError        string
}

type pollFailure struct {
	status    int
	message   string
	transport bool
}

func (e *pollFailure) Error() string {
	return e.message
}

func main() {
	var opts options
	flag.StringVar(&opts.server, "server", "http://127.0.0.1:8080", "config center base URL")
	flag.StringVar(&opts.namespace, "namespace", "payments", "configuration namespace")
	flag.IntVar(&opts.instances, "instances", 24, "number of simulated instances")
	flag.StringVar(&opts.instancePrefix, "instance-prefix", "sim", "instance ID prefix")
	flag.DurationVar(&opts.poll, "poll", 10*time.Second, "polling interval")
	flag.IntVar(&opts.pollCount, "poll-count", 0, "fixed polls per instance; 0 uses duration")
	flag.DurationVar(&opts.duration, "duration", 60*time.Second, "simulation duration; 0 runs until interrupt")
	flag.Int64Var(&opts.seed, "seed", 263, "deterministic fault-injection seed")
	flag.Float64Var(&opts.fault5xxRate, "fault-5xx-rate", 0.03, "probability of an injected 5xx response per poll")
	flag.Float64Var(&opts.networkErrRate, "network-error-rate", 0.02, "probability of an injected network failure per poll")
	flag.Float64Var(&opts.latencyProb, "latency-probability", 0.30, "probability of injected 200ms-2s latency per poll")
	flag.DurationVar(&opts.latencyMin, "latency-min", 200*time.Millisecond, "minimum injected network latency")
	flag.DurationVar(&opts.latencyMax, "latency-max", 2*time.Second, "maximum injected network latency")
	flag.Float64Var(&opts.applyErrorRate, "apply-error-rate", 0.0,
		"fallback per-poll configuration-effectiveness error probability")
	flag.StringVar(&opts.stateDB, "state-db", "data/simclient.db", "local SQLite snapshot database")
	flag.StringVar(&opts.reportPath, "report", "reports/simulation-report.json", "JSON report output path")
	flag.StringVar(&opts.logPath, "log", "logs/simclient.log", "simulator log path")
	flag.IntVar(&opts.readWorkers, "read-workers", 6, "concurrent consistency probe workers")
	flag.DurationVar(&opts.readInterval, "read-interval", 200*time.Millisecond, "consistency probe interval")
	flag.IntVar(&opts.readMinItems, "read-min-items", 1, "minimum item count for a consistency probe")
	flag.DurationVar(&opts.requestTimeout, "request-timeout", 10*time.Second, "per-request timeout")
	flag.IntVar(&opts.maxProcs, "maxprocs", 0, "GOMAXPROCS override; 0 keeps the environment value")
	flag.Int64Var(&opts.memoryLimitMB, "memory-limit-mb", 384, "Go heap soft limit in MiB; 0 disables")
	flag.Parse()

	if err := validateOptions(opts); err != nil {
		fmt.Fprintln(os.Stderr, "simclient:", err)
		os.Exit(2)
	}
	if opts.maxProcs > 0 {
		runtime.GOMAXPROCS(opts.maxProcs)
	}
	if opts.memoryLimitMB > 0 {
		debug.SetMemoryLimit(opts.memoryLimitMB << 20)
	}

	logger, closeLog, err := openLogger(opts.logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "simclient:", err)
		os.Exit(1)
	}
	defer closeLog()

	local, err := clientstate.Open(opts.stateDB)
	if err != nil {
		logger.Fatalf("open local snapshot store: %v", err)
	}
	defer local.Close()

	httpClient := &http.Client{Timeout: opts.requestTimeout}
	instances := make([]*instance, opts.instances)
	for i := range instances {
		instances[i] = &instance{
			index: i,
			id:    fmt.Sprintf("%s-%02d", opts.instancePrefix, i),
			opts:  &opts,
			rng:   rand.New(rand.NewSource(opts.seed + int64(i)*7919)),
			local: local,
			http:  httpClient,
		}
		loaded, ok, err := local.Load(context.Background(), instances[i].id, opts.namespace)
		if err != nil {
			logger.Fatalf("load snapshot for %s: %v", instances[i].id, err)
		}
		if ok {
			instances[i].snapshot = loaded
			instances[i].hasSnapshot = true
			instances[i].countersRelease = loaded.ReleaseID
			logger.Printf("%s restored local snapshot version=%d ring=%s", loaded.InstanceID, loaded.Version, loaded.Ring)
		}
	}

	started := time.Now().UTC()
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx := signalCtx
	var cancel context.CancelFunc
	if opts.duration > 0 {
		ctx, cancel = context.WithTimeout(signalCtx, opts.duration)
		defer cancel()
	}

	probes := newConsistencyProbe(opts)
	probeCtx, cancelProbes := context.WithCancel(ctx)
	defer cancelProbes()
	var probeWG sync.WaitGroup
	for i := 0; i < opts.readWorkers; i++ {
		probeWG.Add(1)
		go func(worker int) {
			defer probeWG.Done()
			probes.run(probeCtx, worker, httpClient)
		}(i)
	}
	var clientWG sync.WaitGroup
	for _, inst := range instances {
		clientWG.Add(1)
		go func(inst *instance) {
			defer clientWG.Done()
			inst.run(ctx, logger)
		}(inst)
	}
	clientWG.Wait()
	cancelProbes()
	probeWG.Wait()
	finished := time.Now().UTC()

	report := buildReport(opts, instances, probes.snapshot(), started, finished, httpClient)
	if err := writeReport(opts.reportPath, report); err != nil {
		logger.Fatalf("write report: %v", err)
	}
	logger.Printf("report written path=%s instances=%d rollbacks=%d offline_fallbacks=%d consistency=%t p95_ms=%.3f",
		opts.reportPath, len(report.Instances), report.RollbackCount,
		report.OfflineFallbackTriggers, report.ReadConsistency.Passed,
		report.ReadConsistency.P95MS)
	fmt.Printf("report: %s\n", opts.reportPath)
}

func validateOptions(opts options) error {
	if opts.instances <= 0 {
		return errors.New("instances must be positive")
	}
	if opts.poll <= 0 {
		return errors.New("poll must be positive")
	}
	if opts.pollCount < 0 {
		return errors.New("poll-count cannot be negative")
	}
	if opts.fault5xxRate < 0 || opts.fault5xxRate > 1 ||
		opts.networkErrRate < 0 || opts.networkErrRate > 1 ||
		opts.fault5xxRate+opts.networkErrRate > 1 {
		return errors.New("fault probabilities must be between 0 and 1 and sum to at most 1")
	}
	if opts.latencyProb < 0 || opts.latencyProb > 1 {
		return errors.New("latency probability must be between 0 and 1")
	}
	if opts.latencyMin <= 0 || opts.latencyMax < opts.latencyMin {
		return errors.New("invalid latency range")
	}
	if opts.applyErrorRate < 0 || opts.applyErrorRate > 1 {
		return errors.New("apply error rate must be between 0 and 1")
	}
	if opts.readWorkers < 0 || opts.readInterval <= 0 {
		return errors.New("invalid consistency probe settings")
	}
	return nil
}

func openLogger(path string) (*log.Logger, func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	writer := io.MultiWriter(os.Stderr, file)
	return log.New(writer, "simclient ", log.LstdFlags|log.Lmicroseconds|log.LUTC), func() {
		_ = file.Close()
	}, nil
}

func (inst *instance) run(ctx context.Context, logger *log.Logger) {
	for pollNumber := 0; ; pollNumber++ {
		if ctx.Err() != nil {
			return
		}
		inst.pollOnce(ctx, logger)
		if inst.opts.pollCount > 0 && pollNumber+1 >= inst.opts.pollCount {
			return
		}
		timer := time.NewTimer(inst.opts.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (inst *instance) pollOnce(ctx context.Context, logger *log.Logger) {
	inst.pullCount++
	delay := time.Duration(0)
	if inst.rng.Float64() < inst.opts.latencyProb {
		span := inst.opts.latencyMax - inst.opts.latencyMin
		delay = inst.opts.latencyMin + time.Duration(inst.rng.Int63n(int64(span)+1))
	}
	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	roll := inst.rng.Float64()
	if roll < inst.opts.fault5xxRate {
		inst.recordOffline(&pollFailure{status: http.StatusServiceUnavailable, message: "injected HTTP 503"})
		return
	}
	if roll < inst.opts.fault5xxRate+inst.opts.networkErrRate {
		inst.recordOffline(&pollFailure{
			status: 0, message: "injected network unreachable", transport: true,
		})
		return
	}

	pull, err := inst.pull(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		inst.recordOffline(err)
		return
	}
	if pull.Digest != model.DigestItems(pull.Items) {
		inst.digestFailures++
		inst.recordOffline(&pollFailure{
			status: 0, message: "response digest does not match the item set",
		})
		return
	}
	inst.online = true
	inst.offline = false
	inst.successfulPulls++
	changed := !inst.hasSnapshot ||
		inst.snapshot.Version != pull.Version ||
		inst.snapshot.Digest != pull.Digest ||
		inst.snapshot.Ring != pull.Ring ||
		inst.snapshot.ReleaseID != pull.ReleaseID
	newSnapshot := clientstate.Snapshot{
		InstanceID: inst.id,
		Namespace:  pull.Namespace,
		Version:    pull.Version,
		Ring:       pull.Ring,
		ReleaseID:  pull.ReleaseID,
		Digest:     pull.Digest,
		Items:      pull.Items,
		LoadedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}
	if changed {
		if err := inst.local.Save(ctx, newSnapshot); err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Printf("%s persist snapshot version=%d: %v", inst.id, pull.Version, err)
			inst.lastError = "persist snapshot: " + err.Error()
			inst.recordOffline(&pollFailure{message: "persist snapshot: " + err.Error()})
			return
		}
		inst.snapshot = newSnapshot
		inst.hasSnapshot = true
	}
	if !inst.hasSnapshot {
		inst.lastError = "successful pull was not persisted"
		return
	}
	inst.snapshot.Ring = pull.Ring
	inst.snapshot.ReleaseID = pull.ReleaseID

	if pull.Ring == model.RingCanary && pull.ReleaseID != "" {
		inst.observeEffectiveness(pull)
		inst.report(ctx, logger)
	}
}

func (inst *instance) pull(ctx context.Context) (model.PullResponse, error) {
	values := url.Values{}
	values.Set("instance_id", inst.id)
	values.Set("namespace", inst.opts.namespace)
	values.Set("local_version", strconv.FormatInt(inst.snapshot.Version, 10))
	endpoint := strings.TrimRight(inst.opts.server, "/") + "/v1/config/pull?" + values.Encode()
	var response model.PullResponse
	if err := doJSON(ctx, inst.http, http.MethodGet, endpoint, nil, &response); err != nil {
		return response, err
	}
	if response.Version <= 0 {
		return response, &pollFailure{status: http.StatusBadGateway, message: "server returned a non-positive version"}
	}
	if response.InstanceID != inst.id {
		return response, &pollFailure{status: http.StatusBadGateway, message: "server returned another instance ID"}
	}
	return response, nil
}

func (inst *instance) recordOffline(err error) {
	wasOffline := inst.offline
	inst.online = false
	inst.offline = true
	inst.lastError = err.Error()
	if inst.hasSnapshot && !wasOffline {
		inst.offlineFallbacks++
	}
	inst.pendingOffline = true
}

func (inst *instance) observeEffectiveness(pull model.PullResponse) {
	if inst.countersRelease != pull.ReleaseID {
		inst.countersRelease = pull.ReleaseID
		inst.successes = 0
		inst.errors = 0
	}
	rate := inst.opts.applyErrorRate
	for _, item := range pull.Items {
		if item.Key == "_sim.effective_error_rate" {
			if parsed, err := strconv.ParseFloat(item.Value, 64); err == nil && parsed >= 0 && parsed <= 1 {
				rate = parsed
			}
			break
		}
	}
	if inst.rng.Float64() < rate {
		inst.errors++
	} else {
		inst.successes++
	}
}

func (inst *instance) report(ctx context.Context, logger *log.Logger) {
	if inst.snapshot.ReleaseID == "" || inst.snapshot.Ring != model.RingCanary {
		return
	}
	offline := inst.pendingOffline
	payload := map[string]any{
		"instance_id":   inst.id,
		"success_count": inst.successes,
		"error_count":   inst.errors,
		"local_version": inst.snapshot.Version,
		"offline":       offline,
	}
	endpoint := strings.TrimRight(inst.opts.server, "/") + "/v1/releases/" +
		url.PathEscape(inst.snapshot.ReleaseID) + "/report"
	var response map[string]any
	if err := doJSON(ctx, inst.http, http.MethodPost, endpoint, payload, &response); err != nil {
		logger.Printf("%s release report failed: %v", inst.id, err)
		return
	}
	inst.reportCount++
	if offline {
		inst.pendingOffline = false
	}
}

func doJSON(
	ctx context.Context,
	client *http.Client,
	method, endpoint string,
	requestBody any,
	responseBody any,
) error {
	var body io.Reader
	if requestBody != nil {
		data, err := json.Marshal(requestBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var apiError struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &apiError)
		if apiError.Code == model.CodeNotCanary || apiError.Code == model.CodeInvalidTransition {
			return &apiErrorType{code: apiError.Code, message: apiError.Message}
		}
		return &pollFailure{
			status:  response.StatusCode,
			message: fmt.Sprintf("HTTP %d %s %s", response.StatusCode, apiError.Code, apiError.Message),
		}
	}
	if responseBody != nil {
		if err := json.Unmarshal(data, responseBody); err != nil {
			return err
		}
	}
	return nil
}

type apiErrorType struct {
	code    string
	message string
}

func (e *apiErrorType) Error() string {
	return e.code + ": " + e.message
}

type consistencyProbe struct {
	opts              options
	mu                sync.Mutex
	requests          int64
	httpFail          int64
	digestMismatch    int64
	duplicateKeys     int64
	invalidVersions   int64
	itemCountMismatch int64
	durations         []float64
}

func newConsistencyProbe(opts options) *consistencyProbe {
	return &consistencyProbe{opts: opts}
}

func (p *consistencyProbe) run(ctx context.Context, worker int, client *http.Client) {
	for {
		if ctx.Err() != nil {
			return
		}
		p.probe(ctx, client)
		timer := time.NewTimer(p.opts.readInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (p *consistencyProbe) probe(ctx context.Context, client *http.Client) {
	values := url.Values{}
	values.Set("namespace", p.opts.namespace)
	endpoint := strings.TrimRight(p.opts.server, "/") + "/v1/config?" + values.Encode()
	start := time.Now()
	var config model.ConfigSet
	err := doJSON(ctx, client, http.MethodGet, endpoint, nil, &config)
	elapsed := float64(time.Since(start).Microseconds()) / 1000.0

	if err != nil && ctx.Err() != nil {
		return
	}
	atomic.AddInt64(&p.requests, 1)
	p.mu.Lock()
	p.durations = append(p.durations, elapsed)
	p.mu.Unlock()
	if err != nil {
		atomic.AddInt64(&p.httpFail, 1)
		return
	}
	if config.Version <= 0 {
		atomic.AddInt64(&p.invalidVersions, 1)
	}
	if len(config.Items) < p.opts.readMinItems {
		atomic.AddInt64(&p.itemCountMismatch, 1)
	}
	seen := make(map[string]struct{}, len(config.Items))
	for _, item := range config.Items {
		if _, exists := seen[item.Key]; exists {
			atomic.AddInt64(&p.duplicateKeys, 1)
			break
		}
		seen[item.Key] = struct{}{}
	}
	if model.DigestItems(config.Items) != config.Digest {
		atomic.AddInt64(&p.digestMismatch, 1)
	}
}

type ReadConsistency struct {
	Workers           int     `json:"workers"`
	Requests          int64   `json:"requests"`
	HTTPFailures      int64   `json:"http_failures"`
	DigestMismatches  int64   `json:"digest_mismatches"`
	DuplicateKeyViews int64   `json:"duplicate_key_views"`
	InvalidVersions   int64   `json:"invalid_versions"`
	ItemCountMismatch int64   `json:"item_count_mismatches"`
	P95MS             float64 `json:"p95_ms"`
	MaxMS             float64 `json:"max_ms"`
	P95Within100MS    bool    `json:"p95_within_100ms"`
	Passed            bool    `json:"passed"`
}

type probeSnapshot struct {
	Requests          int64
	HTTPFailures      int64
	DigestMismatches  int64
	DuplicateKeyViews int64
	InvalidVersions   int64
	ItemCountMismatch int64
	P95MS             float64
	MaxMS             float64
}

func (p *consistencyProbe) snapshot() probeSnapshot {
	p.mu.Lock()
	durations := append([]float64(nil), p.durations...)
	p.mu.Unlock()
	sort.Float64s(durations)
	p95 := 0.0
	max := 0.0
	if len(durations) > 0 {
		index := int(float64(len(durations))*0.95+0.999999) - 1
		if index < 0 {
			index = 0
		}
		if index >= len(durations) {
			index = len(durations) - 1
		}
		p95 = durations[index]
		max = durations[len(durations)-1]
	}
	return probeSnapshot{
		Requests:          atomic.LoadInt64(&p.requests),
		HTTPFailures:      atomic.LoadInt64(&p.httpFail),
		DigestMismatches:  atomic.LoadInt64(&p.digestMismatch),
		DuplicateKeyViews: atomic.LoadInt64(&p.duplicateKeys),
		InvalidVersions:   atomic.LoadInt64(&p.invalidVersions),
		ItemCountMismatch: atomic.LoadInt64(&p.itemCountMismatch),
		P95MS:             p95,
		MaxMS:             max,
	}
}

type Report struct {
	SchemaVersion           string              `json:"schema_version"`
	StartedAt               string              `json:"started_at"`
	FinishedAt              string              `json:"finished_at"`
	DurationMS              int64               `json:"duration_ms"`
	Seed                    int64               `json:"seed"`
	Config                  SimulationConfig    `json:"simulation_config"`
	Instances               []InstanceReport    `json:"instances"`
	CanaryObservations      []CanaryObservation `json:"canary_observations"`
	LatestCanaryObservation *CanaryObservation  `json:"latest_canary_observation,omitempty"`
	RollbackCount           int                 `json:"rollback_count"`
	OfflineFallbackTriggers int64               `json:"offline_fallback_triggers"`
	ReadConsistency         ReadConsistency     `json:"read_consistency"`
	ReleaseQueryError       string              `json:"release_query_error"`
	Validation              Validation          `json:"validation"`
}

type SimulationConfig struct {
	Server             string  `json:"server"`
	Namespace          string  `json:"namespace"`
	Instances          int     `json:"instances"`
	PollIntervalMS     int64   `json:"poll_interval_ms"`
	PollCount          int     `json:"poll_count"`
	Fault5xxRate       float64 `json:"fault_5xx_rate"`
	NetworkErrorRate   float64 `json:"network_error_rate"`
	LatencyProbability float64 `json:"latency_probability"`
	LatencyMinMS       int64   `json:"latency_min_ms"`
	LatencyMaxMS       int64   `json:"latency_max_ms"`
	ReadWorkers        int     `json:"read_workers"`
	ReadIntervalMS     int64   `json:"read_interval_ms"`
	CanaryInstances    int     `json:"canary_instances"`
	FullInstances      int     `json:"full_instances"`
	ActualCanaryRatio  float64 `json:"actual_canary_ratio"`
}

type InstanceReport struct {
	InstanceID           string `json:"instance_id"`
	Ring                 string `json:"ring"`
	FinalLoadedVersion   int64  `json:"final_loaded_version"`
	FinalReleaseID       string `json:"final_release_id"`
	Online               bool   `json:"online"`
	UsingSnapshot        bool   `json:"using_snapshot"`
	OfflineFallbackCount int64  `json:"offline_fallback_count"`
	PullCount            int64  `json:"pull_count"`
	SuccessfulPulls      int64  `json:"successful_pulls"`
	SuccessfulReports    int64  `json:"successful_reports"`
	DigestFailures       int64  `json:"digest_failures"`
	LastError            string `json:"last_error"`
}

type CanaryObservation struct {
	ReleaseID      string  `json:"release_id"`
	State          string  `json:"state"`
	SuccessCount   int64   `json:"success_count"`
	ErrorCount     int64   `json:"error_count"`
	ErrorRate      float64 `json:"error_rate"`
	ErrorThreshold float64 `json:"error_threshold"`
	RolledBack     bool    `json:"rolled_back"`
	RollbackReason string  `json:"rollback_reason"`
}

type Validation struct {
	AllInstancesHaveVersion  bool `json:"all_instances_have_version"`
	ReadConsistencyPassed    bool `json:"read_consistency_passed"`
	ReadP95Within100MS       bool `json:"read_p95_within_100ms"`
	NoInstanceDigestFailures bool `json:"no_instance_digest_failures"`
}

func buildReport(
	opts options,
	instances []*instance,
	probes probeSnapshot,
	started, finished time.Time,
	client *http.Client,
) Report {
	report := Report{
		SchemaVersion:      "1.0",
		StartedAt:          started.Format(time.RFC3339Nano),
		FinishedAt:         finished.Format(time.RFC3339Nano),
		DurationMS:         finished.Sub(started).Milliseconds(),
		Seed:               opts.seed,
		CanaryObservations: []CanaryObservation{},
		Config: SimulationConfig{
			Server:             opts.server,
			Namespace:          opts.namespace,
			Instances:          opts.instances,
			PollIntervalMS:     opts.poll.Milliseconds(),
			PollCount:          opts.pollCount,
			Fault5xxRate:       opts.fault5xxRate,
			NetworkErrorRate:   opts.networkErrRate,
			LatencyProbability: opts.latencyProb,
			LatencyMinMS:       opts.latencyMin.Milliseconds(),
			LatencyMaxMS:       opts.latencyMax.Milliseconds(),
			ReadWorkers:        opts.readWorkers,
			ReadIntervalMS:     opts.readInterval.Milliseconds(),
		},
	}
	report.Instances = make([]InstanceReport, 0, len(instances))
	var canaryCount int
	var allVersions = true
	var noDigestFailures = true
	for _, inst := range instances {
		if inst.snapshot.Ring == model.RingCanary {
			canaryCount++
		}
		if inst.snapshot.Version <= 0 {
			allVersions = false
		}
		if inst.digestFailures > 0 {
			noDigestFailures = false
		}
		report.OfflineFallbackTriggers += inst.offlineFallbacks
		report.Instances = append(report.Instances, InstanceReport{
			InstanceID:           inst.id,
			Ring:                 inst.snapshot.Ring,
			FinalLoadedVersion:   inst.snapshot.Version,
			FinalReleaseID:       inst.snapshot.ReleaseID,
			Online:               inst.online,
			UsingSnapshot:        inst.hasSnapshot,
			OfflineFallbackCount: inst.offlineFallbacks,
			PullCount:            inst.pullCount,
			SuccessfulPulls:      inst.successfulPulls,
			SuccessfulReports:    inst.reportCount,
			DigestFailures:       inst.digestFailures,
			LastError:            inst.lastError,
		})
	}
	sort.Slice(report.Instances, func(i, j int) bool {
		return report.Instances[i].InstanceID < report.Instances[j].InstanceID
	})
	report.Config.CanaryInstances = canaryCount
	report.Config.FullInstances = len(instances) - canaryCount
	if len(instances) > 0 {
		report.Config.ActualCanaryRatio = float64(canaryCount) / float64(len(instances))
	}

	probeResult := ReadConsistency{
		Workers:           opts.readWorkers,
		Requests:          probes.Requests,
		HTTPFailures:      probes.HTTPFailures,
		DigestMismatches:  probes.DigestMismatches,
		DuplicateKeyViews: probes.DuplicateKeyViews,
		InvalidVersions:   probes.InvalidVersions,
		ItemCountMismatch: probes.ItemCountMismatch,
		P95MS:             probes.P95MS,
		MaxMS:             probes.MaxMS,
	}
	probeResult.P95Within100MS = probeResult.Requests > 0 && probeResult.P95MS <= 100
	probeResult.Passed = probeResult.Requests > 0 &&
		probeResult.P95Within100MS &&
		probeResult.HTTPFailures == 0 &&
		probeResult.DigestMismatches == 0 &&
		probeResult.DuplicateKeyViews == 0 &&
		probeResult.InvalidVersions == 0 &&
		probeResult.ItemCountMismatch == 0
	report.ReadConsistency = probeResult

	releases, err := queryReleases(client, opts)
	if err != nil {
		report.ReleaseQueryError = err.Error()
	} else {
		report.CanaryObservations = make([]CanaryObservation, 0, len(releases))
		for i := len(releases) - 1; i >= 0; i-- {
			release := releases[i]
			observation := CanaryObservation{
				ReleaseID:      release.ID,
				State:          release.State,
				SuccessCount:   release.SuccessCount,
				ErrorCount:     release.ErrorCount,
				ErrorRate:      release.ErrorRate,
				ErrorThreshold: release.ErrorThreshold,
				RolledBack:     release.State == model.ReleaseRolledBack,
				RollbackReason: release.RollbackReason,
			}
			if observation.RolledBack {
				report.RollbackCount++
			}
			report.CanaryObservations = append(report.CanaryObservations, observation)
		}
		if len(report.CanaryObservations) > 0 {
			latest := report.CanaryObservations[len(report.CanaryObservations)-1]
			report.LatestCanaryObservation = &latest
		}
	}

	report.Validation = Validation{
		AllInstancesHaveVersion:  allVersions,
		ReadConsistencyPassed:    probeResult.Passed,
		ReadP95Within100MS:       probeResult.P95Within100MS,
		NoInstanceDigestFailures: noDigestFailures,
	}
	return report
}

func queryReleases(client *http.Client, opts options) ([]model.Release, error) {
	values := url.Values{}
	values.Set("namespace", opts.namespace)
	values.Set("limit", "100")
	endpoint := strings.TrimRight(opts.server, "/") + "/v1/releases?" + values.Encode()
	var response struct {
		Releases []model.Release `json:"releases"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.requestTimeout)
	defer cancel()
	if err := doJSON(ctx, client, http.MethodGet, endpoint, nil, &response); err != nil {
		return nil, err
	}
	return response.Releases, nil
}

func writeReport(path string, report Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".simulation-report-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempName, 0o644); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}
