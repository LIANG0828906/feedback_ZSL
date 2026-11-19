package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

type Scenario struct {
	Seed             int64            `json:"seed"`
	DurationMS       int              `json:"duration_ms"`
	TickMS           int              `json:"tick_ms"`
	MaxBatchRecords  int64            `json:"max_batch_records"`
	PauseWriterFirst bool             `json:"pause_writer_first"`
	WarmupTenant     string           `json:"warmup_tenant"`
	WarmupRecords    int64            `json:"warmup_records"`
	AlignToSecond    bool             `json:"align_to_second"`
	Target           string           `json:"target"`
	CoordinatorURL   string           `json:"coordinator_url"`
	WriterControlURL string           `json:"writer_control_url"`
	TenantRates      map[string]int64 `json:"tenant_rates"`
	Burst            *Burst           `json:"burst,omitempty"`
	Faults           []Fault          `json:"faults,omitempty"`
	QuotaUpdates     []QuotaUpdate    `json:"quota_updates,omitempty"`
	Plan             []PlanStep       `json:"plan,omitempty"`
}

type PlanStep struct {
	AtMS       int    `json:"at_ms"`
	TenantID   string `json:"tenant_id"`
	Records    int64  `json:"records"`
	Batches    int    `json:"batches"`
	IntervalMS int    `json:"interval_ms,omitempty"`
}

type Burst struct {
	TenantID   string  `json:"tenant_id"`
	StartMS    int     `json:"start_ms"`
	DurationMS int     `json:"duration_ms"`
	Multiplier float64 `json:"multiplier"`
}

type Fault struct {
	AtMS       int    `json:"at_ms"`
	Kind       string `json:"kind"`
	DurationMS int    `json:"duration_ms"`
	DelayMS    int64  `json:"delay_ms,omitempty"`
	TenantID   string `json:"tenant_id,omitempty"`
}

type QuotaUpdate struct {
	AtMS           int    `json:"at_ms"`
	TenantID       string `json:"tenant_id"`
	LogsPerSecond  *int64 `json:"logs_per_second,omitempty"`
	BytesPerSecond *int64 `json:"bytes_per_second,omitempty"`
}

type tenantCounters struct {
	Offered  int64 `json:"offered_records"`
	Accepted int64 `json:"accepted_records"`
	Dropped  int64 `json:"dropped_records"`
	Requests int64 `json:"requests"`
	Errors   int64 `json:"transport_errors"`
}

type Summary struct {
	SchemaVersion int                       `json:"schema_version"`
	Seed          int64                     `json:"seed"`
	Target        string                    `json:"target"`
	StartedAt     string                    `json:"started_at"`
	FinishedAt    string                    `json:"finished_at"`
	ElapsedMS     int64                     `json:"elapsed_ms"`
	Ticks         int                       `json:"ticks"`
	Tenants       map[string]tenantCounters `json:"tenants"`
	StatusCodes   map[string]int64          `json:"status_codes"`
}

type generator struct {
	scenario  Scenario
	client    *http.Client
	start     time.Time
	loadStart time.Time
	counters  map[string]*atomicTenantCounter
	statuses  sync.Map
}

type atomicTenantCounter struct {
	Offered  atomic.Int64
	Accepted atomic.Int64
	Dropped  atomic.Int64
	Requests atomic.Int64
	Errors   atomic.Int64
}

type ingestResponse struct {
	Code     string `json:"code"`
	Accepted int64  `json:"accepted_records"`
	Dropped  int64  `json:"dropped_records"`
}

func main() {
	scenarioPath := flag.String("scenario", "config/scenario.json", "scenario JSON path")
	target := flag.String("target", "", "override OTLP/HTTP target")
	output := flag.String("summary", "run/loadgen-summary.json", "write generator summary JSON")
	flag.Parse()
	data, err := os.ReadFile(*scenarioPath)
	if err != nil {
		log.Fatalf("read scenario: %v", err)
	}
	var scenario Scenario
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&scenario); err != nil {
		log.Fatalf("decode scenario: %v", err)
	}
	if *target != "" {
		scenario.Target = *target
	}
	if scenario.TickMS <= 0 {
		scenario.TickMS = 100
	}
	if scenario.DurationMS <= 0 {
		scenario.DurationMS = 6000
	}
	if len(scenario.TenantRates) == 0 {
		log.Fatal("scenario.tenant_rates is required")
	}
	if scenario.Target == "" {
		log.Fatal("scenario.target is required")
	}
	g := &generator{
		scenario: scenario,
		client:   &http.Client{Timeout: 30 * time.Second},
		counters: make(map[string]*atomicTenantCounter),
	}
	for tenant := range scenario.TenantRates {
		g.counters[tenant] = &atomicTenantCounter{}
	}
	summary := g.run()
	raw, _ := json.MarshalIndent(summary, "", "  ")
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepathDir(*output), 0o755); err != nil {
		log.Fatalf("create summary directory: %v", err)
	}
	if err := os.WriteFile(*output, raw, 0o644); err != nil {
		log.Fatalf("write summary: %v", err)
	}
	log.Printf("offered=%d accepted(direct)=%d dropped(direct)=%d", summaryTotal(summary, func(c tenantCounters) int64 { return c.Offered }), summaryTotal(summary, func(c tenantCounters) int64 { return c.Accepted }), summaryTotal(summary, func(c tenantCounters) int64 { return c.Dropped }))
}

func (g *generator) run() Summary {
	g.start = time.Now().UTC()
	if g.scenario.PauseWriterFirst {
		g.postJSON(g.scenario.WriterControlURL+"/admin/pause", map[string]any{"paused": true})
		time.Sleep(150 * time.Millisecond)
	}
	if g.scenario.WarmupRecords > 0 && g.scenario.WarmupTenant != "" {
		g.sendBatch(g.scenario.WarmupTenant, g.scenario.WarmupRecords, 0, -1, 0, 0)
		time.Sleep(350 * time.Millisecond)
	}
	g.loadStart = g.start
	if g.scenario.AlignToSecond {
		now := time.Now()
		g.loadStart = time.Unix(now.Unix()+1, 50*time.Millisecond.Nanoseconds())
	}
	faultDone := make(chan struct{})
	go func() {
		defer close(faultDone)
		g.runScheduledControls(g.loadStart)
	}()
	ticks := g.scenario.DurationMS / g.scenario.TickMS
	if len(g.scenario.Plan) > 0 {
		ticks = len(g.scenario.Plan)
		g.runPlan()
	} else {
		accumulators := make(map[string]int64, len(g.scenario.TenantRates))
		for tick := 0; tick < ticks; tick++ {
			elapsedMS := tick * g.scenario.TickMS
			var wg sync.WaitGroup
			for _, tenant := range sortedKeys(g.scenario.TenantRates) {
				count := g.recordsForTick(tenant, accumulators, elapsedMS)
				if count <= 0 {
					continue
				}
				maxRecords := g.scenario.MaxBatchRecords
				if maxRecords <= 0 || maxRecords > count {
					maxRecords = count
				}
				for offset := int64(0); offset < count; offset += maxRecords {
					part := maxRecords
					if offset+part > count {
						part = count - offset
					}
					wg.Add(1)
					go func(tenant string, part, offset int64, tick int, shard int) {
						defer wg.Done()
						g.sendBatch(tenant, part, offset, tick, shard, elapsedMS)
					}(tenant, part, offset, tick, int(offset/maxRecords))
				}
			}
			wg.Wait()
			targetTime := g.loadStart.Add(time.Duration((tick+1)*g.scenario.TickMS) * time.Millisecond)
			sleepFor := time.Until(targetTime)
			if sleepFor > 0 {
				time.Sleep(sleepFor)
			}
		}
	}
	<-faultDone
	g.clearControls()
	finished := time.Now().UTC()
	summary := Summary{
		SchemaVersion: 1,
		Seed:          g.scenario.Seed,
		Target:        g.scenario.Target,
		StartedAt:     g.start.Format(time.RFC3339Nano),
		FinishedAt:    finished.Format(time.RFC3339Nano),
		ElapsedMS:     finished.Sub(g.start).Milliseconds(),
		Ticks:         ticks,
		Tenants:       make(map[string]tenantCounters, len(g.counters)),
		StatusCodes:   make(map[string]int64),
	}
	for tenant, counter := range g.counters {
		summary.Tenants[tenant] = tenantCounters{
			Offered:  counter.Offered.Load(),
			Accepted: counter.Accepted.Load(),
			Dropped:  counter.Dropped.Load(),
			Requests: counter.Requests.Load(),
			Errors:   counter.Errors.Load(),
		}
	}
	g.statuses.Range(func(key, value any) bool {
		summary.StatusCodes[key.(string)] = value.(*atomic.Int64).Load()
		return true
	})
	return summary
}

func (g *generator) recordsForTick(tenant string, accumulators map[string]int64, elapsedMS int) int64 {
	rate := g.scenario.TenantRates[tenant]
	multiplier := 1.0
	if burst := g.scenario.Burst; burst != nil && burst.TenantID == tenant &&
		elapsedMS >= burst.StartMS && elapsedMS < burst.StartMS+burst.DurationMS {
		multiplier = burst.Multiplier
	}
	accumulators[tenant] += int64(float64(rate) * multiplier * float64(g.scenario.TickMS))
	count := accumulators[tenant] / 1000
	accumulators[tenant] -= count * 1000
	if count < 0 {
		return 0
	}
	return count
}

func (g *generator) sendBatch(tenant string, count, offset int64, tick, shard, elapsedMS int) {
	batchID := fmt.Sprintf("%s-%06d-%03d-%d", tenant, tick, shard, g.scenario.Seed)
	g.sendNamedBatch(tenant, count, int64(tick)*100000+offset, elapsedMS, batchID)
}

func (g *generator) sendNamedBatch(tenant string, count, recordBase int64, elapsedMS int, batchID string) {
	counter := g.counters[tenant]
	if counter == nil {
		counter = &atomicTenantCounter{}
		g.counters[tenant] = counter
	}
	counter.Offered.Add(count)
	counter.Requests.Add(1)
	payload := g.buildOTLP(tenant, count, recordBase, elapsedMS, batchID)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, g.scenario.Target, bytes.NewReader(payload))
	if err != nil {
		counter.Errors.Add(1)
		return
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Batch-ID", batchID)
	req.Header.Set("X-Request-ID", "loadgen-"+batchID)
	resp, err := g.client.Do(req)
	if err != nil {
		counter.Errors.Add(1)
		g.incrementStatus("transport_error")
		return
	}
	defer resp.Body.Close()
	g.incrementStatus(strconv.Itoa(resp.StatusCode))
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var result ingestResponse
		if json.Unmarshal(body, &result) == nil {
			counter.Accepted.Add(result.Accepted)
			counter.Dropped.Add(result.Dropped)
		}
	}
}

func (g *generator) buildOTLP(tenant string, count, recordBase int64, elapsedMS int, batchID string) []byte {
	seed := stableSeed(g.scenario.Seed, tenant)
	rng := rand.New(rand.NewSource(seed + recordBase))
	records := make([]*logspb.LogRecord, 0, count)
	baseTime := g.start.Add(time.Duration(elapsedMS) * time.Millisecond)
	for i := int64(0); i < count; i++ {
		size := 1024 + rng.Intn(7*1024+1)
		body := deterministicBody(rng, size)
		sequence := recordBase + i
		recordID := fmt.Sprintf("%s-r%08d", tenant, sequence)
		records = append(records, &logspb.LogRecord{
			TimeUnixNano:   uint64(baseTime.Add(time.Duration(i) * time.Microsecond).UnixNano()),
			SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_INFO,
			SeverityText:   "INFO",
			Body:           &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
			Attributes: []*commonpb.KeyValue{
				stringAttribute("record.id", recordID),
				stringAttribute("batch.id", batchID),
				{Key: "sequence", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: sequence}}},
			},
		})
	}
	payload := &collectorlogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{
			{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{
						stringAttribute("tenant.id", tenant),
						stringAttribute("service.name", "deterministic-loadgen"),
					},
				},
				ScopeLogs: []*logspb.ScopeLogs{
					{
						Scope:      &commonpb.InstrumentationScope{Name: "uid296.loadgen", Version: "1"},
						LogRecords: records,
					},
				},
			},
		},
	}
	raw, _ := proto.Marshal(payload)
	return raw
}

func stringAttribute(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}},
	}
}

func deterministicBody(rng *rand.Rand, size int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var b strings.Builder
	b.Grow(size)
	for b.Len() < size {
		b.WriteByte(alphabet[rng.Intn(len(alphabet))])
	}
	return b.String()
}

func stableSeed(seed int64, tenant string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(tenant))
	return seed ^ int64(h.Sum64())
}

func (g *generator) runPlan() {
	recordBases := make(map[string]int64, len(g.scenario.TenantRates))
	for stepIndex, step := range g.scenario.Plan {
		if step.TenantID == "" || step.Records <= 0 || step.Batches <= 0 {
			continue
		}
		if wait := time.Until(g.loadStart.Add(time.Duration(step.AtMS) * time.Millisecond)); wait > 0 {
			time.Sleep(wait)
		}
		for batch := 0; batch < step.Batches; batch++ {
			recordBase := recordBases[step.TenantID]
			recordBases[step.TenantID] += step.Records
			batchID := fmt.Sprintf("plan-%s-%03d-%03d-%d", step.TenantID, stepIndex, batch, g.scenario.Seed)
			elapsed := int(time.Since(g.loadStart) / time.Millisecond)
			if elapsed < 0 {
				elapsed = 0
			}
			g.sendNamedBatch(step.TenantID, step.Records, recordBase, elapsed, batchID)
			if step.IntervalMS > 0 && batch+1 < step.Batches {
				time.Sleep(time.Duration(step.IntervalMS) * time.Millisecond)
			}
		}
	}
}

func (g *generator) runScheduledControls(loadStart time.Time) {
	events := make([]controlEvent, 0, len(g.scenario.Faults)+len(g.scenario.QuotaUpdates))
	for _, fault := range g.scenario.Faults {
		events = append(events, controlEvent{at: fault.AtMS, apply: func() { g.applyFault(fault) }})
		if fault.DurationMS > 0 {
			clearFault := fault
			events = append(events, controlEvent{at: fault.AtMS + fault.DurationMS, apply: func() { g.clearFault(clearFault) }})
		}
	}
	for _, update := range g.scenario.QuotaUpdates {
		update := update
		events = append(events, controlEvent{at: update.AtMS, apply: func() { g.applyQuotaUpdate(update) }})
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].at < events[j].at })
	for _, event := range events {
		if wait := time.Until(loadStart.Add(time.Duration(event.at) * time.Millisecond)); wait > 0 {
			time.Sleep(wait)
		}
		event.apply()
	}
}

type controlEvent struct {
	at    int
	apply func()
}

func (g *generator) applyFault(fault Fault) {
	switch fault.Kind {
	case "writer_pause":
		g.postJSON(g.scenario.WriterControlURL+"/admin/pause", map[string]any{"paused": true})
	case "consumer_slow":
		g.postJSON(g.scenario.WriterControlURL+"/admin/fault", map[string]any{"tenant_id": fault.TenantID, "delay_ms": fault.DelayMS})
	case "redis_slow":
		g.postJSON(g.scenario.CoordinatorURL+"/v1/admin/fault", map[string]any{"redis_delay_ms": fault.DelayMS})
	default:
		log.Printf("unknown fault kind %q", fault.Kind)
	}
}

func (g *generator) clearFault(fault Fault) {
	switch fault.Kind {
	case "writer_pause":
		g.postJSON(g.scenario.WriterControlURL+"/admin/pause", map[string]any{"paused": false})
	case "consumer_slow":
		g.postJSON(g.scenario.WriterControlURL+"/admin/fault", map[string]any{"tenant_id": fault.TenantID, "delay_ms": 0})
	case "redis_slow":
		g.postJSON(g.scenario.CoordinatorURL+"/v1/admin/fault", map[string]any{"redis_delay_ms": 0})
	}
}

func (g *generator) applyQuotaUpdate(update QuotaUpdate) {
	body := make(map[string]any)
	if update.LogsPerSecond != nil {
		body["logs_per_second"] = *update.LogsPerSecond
	}
	if update.BytesPerSecond != nil {
		body["bytes_per_second"] = *update.BytesPerSecond
	}
	g.putJSON(g.scenario.CoordinatorURL+"/v1/tenants/"+update.TenantID+"/quota", body)
}

func (g *generator) clearControls() {
	g.postJSON(g.scenario.WriterControlURL+"/admin/pause", map[string]any{"paused": false})
	g.postJSON(g.scenario.WriterControlURL+"/admin/fault", map[string]any{"tenant_id": "*", "delay_ms": 0})
	g.postJSON(g.scenario.CoordinatorURL+"/v1/admin/fault", map[string]any{"redis_delay_ms": 0})
}

func (g *generator) postJSON(url string, value any) {
	raw, _ := json.Marshal(value)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		log.Printf("control POST %s failed: %v", url, err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("control POST %s returned %d", url, resp.StatusCode)
	}
}

func (g *generator) putJSON(url string, value any) {
	raw, _ := json.Marshal(value)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(raw))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		log.Printf("control PUT %s failed: %v", url, err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("control PUT %s returned %d", url, resp.StatusCode)
	}
}

func (g *generator) incrementStatus(code string) {
	value, _ := g.statuses.LoadOrStore(code, &atomic.Int64{})
	value.(*atomic.Int64).Add(1)
}

func sortedKeys(values map[string]int64) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func filepathDir(path string) string {
	index := strings.LastIndexByte(path, '/')
	if index < 0 {
		index = strings.LastIndexByte(path, '\\')
	}
	if index < 0 {
		return "."
	}
	return path[:index]
}

func summaryTotal(summary Summary, value func(tenantCounters) int64) int64 {
	var total int64
	for _, counter := range summary.Tenants {
		total += value(counter)
	}
	return total
}
