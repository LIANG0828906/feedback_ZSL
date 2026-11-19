package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"quota-coordinator/internal/config"
	"quota-coordinator/internal/model"
	"quota-coordinator/internal/store"
)

type Writer struct {
	cfg            config.Config
	store          *store.Store
	coordinatorURL string
	outputPath     string
	controlAddr    string
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup

	storageMu sync.Mutex
	file      *os.File
	offset    int64
	seen      map[string]struct{}

	statsMu sync.Mutex
	stats   Stats

	paused   atomic.Bool
	ackDelay atomic.Int64
	faultsMu sync.Mutex
	faults   map[string]time.Duration
	http     *http.Client
}

type Stats struct {
	State            string                 `json:"state"`
	Paused           bool                   `json:"paused"`
	WrittenBatches   uint64                 `json:"written_batches"`
	WrittenRecords   uint64                 `json:"written_records"`
	WrittenBytes     uint64                 `json:"written_bytes"`
	DuplicateBatches uint64                 `json:"duplicate_batches"`
	DuplicateRecords uint64                 `json:"duplicate_records"`
	AckRetries       uint64                 `json:"ack_retries"`
	WriteErrors      uint64                 `json:"write_errors"`
	LastError        string                 `json:"last_error,omitempty"`
	OutputFile       string                 `json:"output_file"`
	StartedAt        string                 `json:"started_at"`
	UpdatedAt        string                 `json:"updated_at"`
	Tenants          map[string]TenantStats `json:"tenants"`
}

type TenantStats struct {
	WrittenBatches   uint64 `json:"written_batches"`
	WrittenRecords   uint64 `json:"written_records"`
	WrittenBytes     uint64 `json:"written_bytes"`
	DuplicateBatches uint64 `json:"duplicate_batches"`
	AckRetries       uint64 `json:"ack_retries"`
}

type ackPayload struct {
	TenantID      string `json:"tenant_id"`
	BatchID       string `json:"batch_id"`
	Seq           int64  `json:"seq"`
	StorageOffset int64  `json:"storage_offset"`
}

func main() {
	configPath := flag.String("config", "config/coordinator.json", "path to shared JSON config")
	output := flag.String("output", "run/storage/logs.jsonl", "final JSON Lines output path")
	coordinatorURL := flag.String("coordinator", "http://127.0.0.1:8080", "coordinator base URL")
	controlAddr := flag.String("control", "127.0.0.1:8081", "writer control/statistics address")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(*output), 0o755); err != nil {
		log.Fatalf("create output directory: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	st := store.New(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB, cfg.Redis.Prefix)
	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := st.Ping(pingCtx); err != nil {
		pingCancel()
		log.Fatalf("redis ping: %v", err)
	}
	pingCancel()

	writer := &Writer{
		cfg:            cfg,
		store:          st,
		coordinatorURL: strings.TrimRight(*coordinatorURL, "/"),
		outputPath:     *output,
		controlAddr:    *controlAddr,
		ctx:            ctx,
		cancel:         cancel,
		seen:           make(map[string]struct{}),
		faults:         make(map[string]time.Duration),
		http:           &http.Client{Timeout: 3 * time.Second},
	}
	writer.stats = Stats{
		State:      "starting",
		OutputFile: filepath.Base(*output),
		StartedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		Tenants:    make(map[string]TenantStats),
	}
	for _, tenant := range cfg.TenantIDs() {
		writer.stats.Tenants[tenant] = TenantStats{}
	}
	if err := writer.openAndScan(); err != nil {
		log.Fatalf("open output: %v", err)
	}
	if err := writer.requeueProcessing(); err != nil {
		log.Fatalf("requeue processing jobs: %v", err)
	}
	writer.setStats(func(stats *Stats) {
		stats.State = "running"
	})
	writer.startControl()
	for _, tenant := range cfg.TenantIDs() {
		writer.wg.Add(1)
		go writer.consumeTenant(tenant)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	sig := <-signals
	log.Printf("received %s, shutting down writer", sig)
	writer.cancel()
	writer.wg.Wait()
	writer.setStats(func(stats *Stats) { stats.State = "stopped" })
	_ = writer.file.Sync()
	_ = writer.file.Close()
	_ = st.Close()
}

func (w *Writer) openAndScan() error {
	file, err := os.OpenFile(w.outputPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if info.Size() == 0 {
		w.file = file
		return nil
	}
	last := make([]byte, 1)
	if _, err := file.ReadAt(last, info.Size()-1); err != nil {
		_ = file.Close()
		return err
	}
	if last[0] != '\n' {
		lastNewline, err := findLastNewline(file, info.Size())
		if err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Truncate(lastNewline); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
		if _, err := file.Seek(0, io.SeekEnd); err != nil {
			_ = file.Close()
			return err
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	type scannedBatch struct {
		id          string
		tenant      string
		expected    int64
		startOffset int64
		rows        uint64
		bytes       uint64
		indices     map[int64]struct{}
	}
	var current *scannedBatch
	var totalBatches, totalRecords, totalBytes uint64
	var lineOffset int64
	truncateOffset := int64(-1)
	commit := func() {
		if current == nil {
			return
		}
		complete := current.expected < 0 || (int64(current.rows) == current.expected &&
			(len(current.indices) == 0 || int64(len(current.indices)) == current.expected))
		if !complete {
			if truncateOffset < 0 {
				truncateOffset = current.startOffset
			}
			return
		}
		if _, exists := w.seen[current.id]; exists {
			return
		}
		w.seen[current.id] = struct{}{}
		totalBatches++
		totalRecords += current.rows
		totalBytes += current.bytes
		tenantStats := w.stats.Tenants[current.tenant]
		tenantStats.WrittenBatches++
		tenantStats.WrittenRecords += current.rows
		tenantStats.WrittenBytes += current.bytes
		w.stats.Tenants[current.tenant] = tenantStats
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		lineStart := lineOffset
		lineOffset += int64(len(line) + 1)
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			continue
		}
		batchID, _ := row["_batch_id"].(string)
		tenant, _ := row["_tenant_id"].(string)
		if batchID == "" {
			continue
		}
		if current == nil || current.id != batchID {
			commit()
			current = &scannedBatch{
				id:          batchID,
				tenant:      tenant,
				expected:    -1,
				startOffset: lineStart,
				indices:     make(map[int64]struct{}),
			}
		}
		current.rows++
		current.bytes += uint64(len(line) + 1)
		if expected, ok := numberAsInt64(row["_batch_record_count"]); ok {
			current.expected = expected
		}
		if index, ok := numberAsInt64(row["_batch_record_index"]); ok {
			current.indices[index] = struct{}{}
		}
	}
	commit()
	if err := scanner.Err(); err != nil {
		_ = file.Close()
		return err
	}
	if truncateOffset >= 0 {
		if err := file.Truncate(truncateOffset); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
		lineOffset = truncateOffset
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		_ = file.Close()
		return err
	}
	w.file = file
	w.offset = lineOffset
	w.stats.WrittenBatches = totalBatches
	w.stats.WrittenRecords = totalRecords
	w.stats.WrittenBytes = totalBytes
	log.Printf("resumed output %s: batches=%d records=%d bytes=%d", filepath.Base(w.outputPath), totalBatches, totalRecords, totalBytes)
	return nil
}

func numberAsInt64(value any) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Int64()
		return parsed, err == nil
	case float64:
		return int64(number), true
	case int64:
		return number, true
	default:
		return 0, false
	}
}

func findLastNewline(file *os.File, size int64) (int64, error) {
	const chunk = 64 * 1024
	buf := make([]byte, chunk)
	for end := size; end > 0; {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		n, err := file.ReadAt(buf[:end-start], start)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		for i := n - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				return start + int64(i) + 1, nil
			}
		}
		end = start
	}
	return 0, nil
}

func (w *Writer) requeueProcessing() error {
	for _, tenant := range w.cfg.TenantIDs() {
		moved, err := w.store.RequeueProcessing(w.ctx, tenant)
		if err != nil {
			return err
		}
		if moved > 0 {
			log.Printf("requeued %d unconfirmed batches for %s", moved, tenant)
		}
	}
	return nil
}

func (w *Writer) consumeTenant(tenant string) {
	defer w.wg.Done()
	for {
		if w.ctx.Err() != nil {
			return
		}
		w.waitTenantFault(tenant)
		raw, err := w.store.Move(w.ctx, tenant, time.Second)
		if errors.Is(err, context.Canceled) {
			return
		}
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			w.setStats(func(stats *Stats) {
				stats.LastError = "redis dequeue: " + err.Error()
			})
			time.Sleep(200 * time.Millisecond)
			continue
		}
		var job model.QueueJob
		if err := json.Unmarshal([]byte(raw), &job); err != nil {
			w.setStats(func(stats *Stats) {
				stats.LastError = "decode queue job: " + err.Error()
			})
			_ = w.store.RemoveProcessing(w.ctx, tenant, raw)
			continue
		}
		for {
			if w.ctx.Err() != nil {
				return
			}
			w.waitTenantFault(tenant)
			written, offset, err := w.writeBatch(job)
			if err != nil {
				w.setStats(func(stats *Stats) {
					stats.WriteErrors++
					stats.LastError = "write batch: " + err.Error()
				})
				time.Sleep(250 * time.Millisecond)
				continue
			}
			if written {
				w.setStats(func(stats *Stats) {
					stats.WrittenBatches++
					stats.WrittenRecords += uint64(job.Count)
					stats.WrittenBytes += uint64(job.Bytes)
					tenantStats := stats.Tenants[tenant]
					tenantStats.WrittenBatches++
					tenantStats.WrittenRecords += uint64(job.Count)
					tenantStats.WrittenBytes += uint64(job.Bytes)
					stats.Tenants[tenant] = tenantStats
					stats.LastError = ""
				})
			} else {
				w.setStats(func(stats *Stats) {
					stats.DuplicateBatches++
					stats.DuplicateRecords += uint64(job.Count)
					tenantStats := stats.Tenants[tenant]
					tenantStats.DuplicateBatches++
					stats.Tenants[tenant] = tenantStats
				})
			}
			if delay := time.Duration(w.ackDelay.Load()) * time.Millisecond; delay > 0 {
				select {
				case <-w.ctx.Done():
					return
				case <-time.After(delay):
				}
			}
			if err := w.ackWithRetry(job, offset); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				continue
			}
			_ = w.store.RemoveProcessing(w.ctx, tenant, raw)
			break
		}
	}
}

func (w *Writer) writeBatch(job model.QueueJob) (bool, int64, error) {
	w.storageMu.Lock()
	defer w.storageMu.Unlock()
	if _, duplicate := w.seen[job.BatchID]; duplicate {
		return false, w.offset, nil
	}
	var buffer bytes.Buffer
	for index, record := range job.Records {
		row := make(map[string]any)
		dec := json.NewDecoder(bytes.NewReader(record.Data))
		dec.UseNumber()
		if err := dec.Decode(&row); err != nil {
			return false, w.offset, fmt.Errorf("decode record %s: %w", record.RecordID, err)
		}
		row["_batch_id"] = job.BatchID
		row["_tenant_id"] = job.TenantID
		row["_record_id"] = record.RecordID
		row["_batch_record_index"] = index
		row["_batch_record_count"] = len(job.Records)
		line, err := json.Marshal(row)
		if err != nil {
			return false, w.offset, err
		}
		buffer.Write(line)
		buffer.WriteByte('\n')
	}
	if _, err := w.file.Write(buffer.Bytes()); err != nil {
		return false, w.offset, err
	}
	if err := w.file.Sync(); err != nil {
		return false, w.offset, err
	}
	startOffset := w.offset
	w.offset += int64(buffer.Len())
	w.seen[job.BatchID] = struct{}{}
	return true, startOffset, nil
}

func (w *Writer) ackWithRetry(job model.QueueJob, offset int64) error {
	body, _ := json.Marshal(ackPayload{
		TenantID:      job.TenantID,
		BatchID:       job.BatchID,
		Seq:           job.Seq,
		StorageOffset: offset,
	})
	delay := 50 * time.Millisecond
	for attempt := 0; ; attempt++ {
		if w.ctx.Err() != nil {
			return context.Canceled
		}
		req, err := http.NewRequestWithContext(w.ctx, http.MethodPost, w.coordinatorURL+"/v1/ack", bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Request-ID", "writer-"+job.BatchID)
			resp, requestErr := w.http.Do(req)
			if requestErr == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return nil
				}
				err = fmt.Errorf("ack returned HTTP %d", resp.StatusCode)
			} else {
				err = requestErr
			}
		}
		w.setStats(func(stats *Stats) {
			stats.AckRetries++
			tenantStats := stats.Tenants[job.TenantID]
			tenantStats.AckRetries++
			stats.Tenants[job.TenantID] = tenantStats
			stats.LastError = "ack: " + err.Error()
		})
		select {
		case <-w.ctx.Done():
			return context.Canceled
		case <-time.After(delay):
		}
		if delay < time.Second {
			delay *= 2
		}
	}
}

func (w *Writer) waitTenantFault(tenant string) {
	for {
		if w.ctx.Err() != nil {
			return
		}
		delay := time.Duration(0)
		if w.paused.Load() {
			delay = 25 * time.Millisecond
		} else {
			w.faultsMu.Lock()
			if tenantDelay := w.faults[tenant]; tenantDelay > delay {
				delay = tenantDelay
			}
			if globalDelay := w.faults["*"]; globalDelay > delay {
				delay = globalDelay
			}
			w.faultsMu.Unlock()
		}
		if delay == 0 {
			return
		}
		select {
		case <-w.ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (w *Writer) startControl() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(rw http.ResponseWriter, r *http.Request) {
		writeControlJSON(rw, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("/stats", func(rw http.ResponseWriter, r *http.Request) {
		writeControlJSON(rw, http.StatusOK, w.snapshot())
	})
	mux.HandleFunc("/admin/pause", func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			writeControlJSON(rw, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		var body struct {
			Paused bool `json:"paused"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<20)).Decode(&body); err != nil {
			writeControlJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		w.paused.Store(body.Paused)
		w.setStats(func(stats *Stats) { stats.Paused = body.Paused })
		writeControlJSON(rw, http.StatusOK, map[string]any{"ok": true, "paused": body.Paused})
	})
	mux.HandleFunc("/admin/fault", func(rw http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.faultsMu.Lock()
			out := make(map[string]int64, len(w.faults))
			for tenant, delay := range w.faults {
				out[tenant] = delay.Milliseconds()
			}
			w.faultsMu.Unlock()
			writeControlJSON(rw, http.StatusOK, map[string]any{
				"paused":        w.paused.Load(),
				"ack_delay_ms":  w.ackDelay.Load(),
				"tenant_delays": out,
			})
		case http.MethodPost, http.MethodPut:
			var body struct {
				TenantID   string `json:"tenant_id"`
				DelayMS    int64  `json:"delay_ms"`
				AckDelayMS *int64 `json:"ack_delay_ms,omitempty"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<20)).Decode(&body); err != nil {
				writeControlJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			if body.DelayMS < 0 || body.DelayMS > 60000 {
				writeControlJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": "delay_ms must be between 0 and 60000"})
				return
			}
			if body.AckDelayMS != nil {
				if *body.AckDelayMS < 0 || *body.AckDelayMS > 60000 {
					writeControlJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": "ack_delay_ms must be between 0 and 60000"})
					return
				}
				w.ackDelay.Store(*body.AckDelayMS)
			}
			tenant := body.TenantID
			if tenant == "" {
				tenant = "*"
			}
			w.faultsMu.Lock()
			w.faults[tenant] = time.Duration(body.DelayMS) * time.Millisecond
			w.faultsMu.Unlock()
			writeControlJSON(rw, http.StatusOK, map[string]any{"ok": true, "tenant_id": tenant, "delay_ms": body.DelayMS})
		default:
			writeControlJSON(rw, http.StatusMethodNotAllowed, map[string]any{"ok": false})
		}
	})
	server := &http.Server{
		Addr:              w.controlAddr,
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		log.Printf("writer control/debug server listening on %s", w.controlAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("writer control server: %v", err)
		}
	}()
	go func() {
		<-w.ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
}

func (w *Writer) snapshot() Stats {
	w.statsMu.Lock()
	defer w.statsMu.Unlock()
	out := w.stats
	out.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	out.Paused = w.paused.Load()
	out.Tenants = make(map[string]TenantStats, len(w.stats.Tenants))
	keys := make([]string, 0, len(w.stats.Tenants))
	for key := range w.stats.Tenants {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out.Tenants[key] = w.stats.Tenants[key]
	}
	return out
}

func (w *Writer) setStats(update func(*Stats)) {
	w.statsMu.Lock()
	defer w.statsMu.Unlock()
	update(&w.stats)
	w.stats.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
}

func writeControlJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
