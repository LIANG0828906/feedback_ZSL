package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"settlement/internal/api"
	"settlement/internal/config"
)

const (
	releaseReasonReleased     = "released"
	releaseReasonLeaseExpired = "lease_expired"
	releaseReasonLockMissing  = "lock_missing"
)

var batchPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)

type lockRecord struct {
	BatchID          string `json:"batch_id"`
	HolderID         string `json:"holder_id"`
	Generation       string `json:"generation"`
	LeaseID          string `json:"lease_id"`
	LeaseTTLMs       int64  `json:"lease_ttl_ms"`
	ExpiresAtUnixMs  int64  `json:"expires_at_unix_ms"`
	AcquiredAtUnixMs int64  `json:"acquired_at_unix_ms"`
}

type releaseEvent struct {
	BatchID              string `json:"batch_id"`
	HolderID             string `json:"holder_id"`
	Generation           string `json:"generation"`
	LeaseID              string `json:"lease_id"`
	LeaseExpiresAtUnixMs int64  `json:"lease_expires_at_unix_ms"`
	ReleasedAtUnixMs     int64  `json:"released_at_unix_ms"`
	Reason               string `json:"reason"`
}

type committedRecord struct {
	BatchID         string `json:"batch_id"`
	HolderID        string `json:"holder_id"`
	TaskCount       int32  `json:"task_count"`
	TotalAmount     int64  `json:"total_amount"`
	ResultDigest    string `json:"result_digest"`
	ResultJSON      string `json:"result_json"`
	CommittedAtUnix int64  `json:"committed_at_unix_ms"`
}

type Service struct {
	api.UnimplementedSettlementLockServer

	cfg     config.Coordinator
	client  *clientv3.Client
	logger  *slog.Logger
	queues  *queueRegistry
	metrics *Metrics
}

func New(ctx context.Context, cfg config.Coordinator, logger *slog.Logger) (*Service, error) {
	if logger == nil {
		logger = slog.Default()
	}
	dialTimeout := time.Duration(cfg.EtcdDialTimeoutMs) * time.Millisecond
	if dialTimeout <= 0 {
		dialTimeout = 5 * time.Second
	}
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:            cfg.EtcdEndpoints,
		DialTimeout:          dialTimeout,
		DialKeepAliveTime:    2 * time.Second,
		DialKeepAliveTimeout: 2 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("connect etcd: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	if _, err := cli.Get(pingCtx, cfg.KeyPrefix+"/health", clientv3.WithLimit(1)); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("etcd health check: %w", err)
	}
	return &Service{
		cfg:     cfg,
		client:  cli,
		logger:  logger,
		queues:  newQueueRegistry(),
		metrics: NewMetrics(),
	}, nil
}

func (s *Service) Close() error {
	return s.client.Close()
}

func (s *Service) Start(ctx context.Context) {
	go s.watchLoop(ctx)
	go s.sweepLoop(ctx)
}

func (s *Service) key(parts ...string) string {
	if len(parts) == 0 {
		return s.cfg.KeyPrefix
	}
	return s.cfg.KeyPrefix + "/" + strings.Join(parts, "/")
}

func (s *Service) lockKey(batchID string) string {
	return s.key("locks", batchID)
}

func (s *Service) currentKey(batchID string) string {
	return s.key("current", batchID)
}

func (s *Service) resultKey(batchID string) string {
	return s.key("results", batchID)
}

func (s *Service) eventKey(batchID, generation string) string {
	return s.key("release-events", batchID, generation)
}

func validBatchID(batchID string) bool {
	return batchPattern.MatchString(batchID)
}

func marshalLock(lock lockRecord) string {
	data, _ := json.Marshal(lock)
	return string(data)
}

func decodeLock(value []byte) (lockRecord, error) {
	var lock lockRecord
	if err := json.Unmarshal(value, &lock); err != nil {
		return lock, err
	}
	if lock.BatchID == "" || lock.HolderID == "" || lock.Generation == "" {
		return lock, fmt.Errorf("incomplete lock record")
	}
	return lock, nil
}

func marshalRelease(event releaseEvent) string {
	data, _ := json.Marshal(event)
	return string(data)
}

func (s *Service) expectedBatches() []string {
	return config.ExpectedBatchIDs(s.cfg)
}

func sortedUnion(groups ...[]string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, group := range groups {
		for _, id := range group {
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func remainingSeconds(expiresAtUnixMs int64, now time.Time) float64 {
	remaining := time.Until(time.UnixMilli(expiresAtUnixMs))
	if expiresAtUnixMs <= 0 || remaining <= 0 {
		return 0
	}
	return remaining.Seconds()
}

func protoReleaseEvent(event releaseEvent) *api.ReleaseEvent {
	return &api.ReleaseEvent{
		BatchId:              event.BatchID,
		HolderId:             event.HolderID,
		Generation:           event.Generation,
		LeaseId:              event.LeaseID,
		LeaseExpiresAtUnixMs: event.LeaseExpiresAtUnixMs,
		ReleasedAtUnixMs:     event.ReleasedAtUnixMs,
		Reason:               event.Reason,
	}
}

type queueRegistry struct {
	mu     sync.Mutex
	queues map[string]*batchQueue
}

func newQueueRegistry() *queueRegistry {
	return &queueRegistry{queues: make(map[string]*batchQueue)}
}

func (r *queueRegistry) get(batchID string) *batchQueue {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.queues[batchID]
	if q == nil {
		q = &batchQueue{}
		r.queues[batchID] = q
	}
	return q
}

func (r *queueRegistry) signalAll() {
	r.mu.Lock()
	queues := make([]*batchQueue, 0, len(r.queues))
	for _, q := range r.queues {
		queues = append(queues, q)
	}
	r.mu.Unlock()
	for _, q := range queues {
		q.signalAll()
	}
}

type queueTicket struct {
	sequence uint64
	holderID string
	wake     chan struct{}
}

type batchQueue struct {
	mu      sync.Mutex
	waiters []*queueTicket
	next    uint64
}

func (q *batchQueue) enqueue(holderID string) *queueTicket {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.next++
	ticket := &queueTicket{
		sequence: q.next,
		holderID: holderID,
		wake:     make(chan struct{}, 1),
	}
	q.waiters = append(q.waiters, ticket)
	return ticket
}

func (q *batchQueue) isHead(ticket *queueTicket) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiters) > 0 && q.waiters[0] == ticket
}

func (q *batchQueue) remove(ticket *queueTicket) {
	q.mu.Lock()
	removed := false
	for i, candidate := range q.waiters {
		if candidate != ticket {
			continue
		}
		q.waiters = append(q.waiters[:i], q.waiters[i+1:]...)
		removed = true
		break
	}
	shouldSignal := removed && len(q.waiters) > 0
	q.mu.Unlock()
	if shouldSignal {
		q.signalAll()
	}
}

func (q *batchQueue) signalAll() {
	q.mu.Lock()
	waiters := append([]*queueTicket(nil), q.waiters...)
	q.mu.Unlock()
	for _, waiter := range waiters {
		select {
		case waiter.wake <- struct{}{}:
		default:
		}
	}
}
