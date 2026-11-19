package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"quota-coordinator/internal/model"
)

var (
	ErrReceiptNotFound = errors.New("receipt not found")
)

type Store struct {
	rdb    *redis.Client
	prefix string
}

func New(addr, password string, db int, prefix string) *Store {
	return &Store{
		rdb: redis.NewClient(&redis.Options{
			Addr:         addr,
			Password:     password,
			DB:           db,
			PoolSize:     16,
			MinIdleConns: 2,
		}),
		prefix: prefix,
	}
}

func (s *Store) Close() error {
	return s.rdb.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.rdb.Ping(ctx).Err()
}

func (s *Store) key(parts ...string) string {
	out := s.prefix
	for i, part := range parts {
		if i > 0 {
			out += ":"
		}
		out += part
	}
	return out
}

func (s *Store) batchKey(batchID string) string {
	sum := sha256.Sum256([]byte(batchID))
	return hex.EncodeToString(sum[:16])
}

func (s *Store) QueueKey(tenant string) string {
	return s.key("queue", tenant)
}

func (s *Store) ProcessingKey(tenant string) string {
	return s.key("processing", tenant)
}

func (s *Store) Seen(ctx context.Context, tenant, batchID string) (int64, bool, error) {
	value, err := s.rdb.HGet(ctx, s.key("seen", tenant), batchID).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return value, true, nil
}

func (s *Store) NextSeq(ctx context.Context, tenant string) (int64, error) {
	return s.rdb.Incr(ctx, s.key("seq", tenant)).Result()
}

var enqueueScript = redis.NewScript(`
if redis.call('HEXISTS', KEYS[1], ARGV[1]) == 1 then
  return {0, tonumber(redis.call('HGET', KEYS[1], ARGV[1]))}
end
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
redis.call('LPUSH', KEYS[2], ARGV[3])
redis.call('HSET', KEYS[3],
  'tenant_id', ARGV[4],
  'batch_id', ARGV[1],
  'seq', ARGV[2],
  'record_count', ARGV[5],
  'bytes', ARGV[6],
  'enqueued_at', ARGV[7],
  'acknowledged', 'false')
return {1, tonumber(ARGV[2])}
`)

func (s *Store) Enqueue(ctx context.Context, tenant, batchID string, seq int64, job model.QueueJob) (int64, bool, error) {
	job.Seq = seq
	raw, err := json.Marshal(job)
	if err != nil {
		return 0, false, err
	}
	result, err := enqueueScript.Run(ctx, s.rdb,
		[]string{
			s.key("seen", tenant),
			s.QueueKey(tenant),
			s.key("receipt", tenant, s.batchKey(batchID)),
		},
		batchID,
		strconv.FormatInt(seq, 10),
		string(raw),
		tenant,
		strconv.FormatInt(job.Count, 10),
		strconv.FormatInt(job.Bytes, 10),
		job.Enqueued,
	).Slice()
	if err != nil {
		return 0, false, err
	}
	if len(result) != 2 {
		return 0, false, fmt.Errorf("unexpected enqueue result %v", result)
	}
	created, _ := result[0].(int64)
	actualSeq, _ := result[1].(int64)
	return actualSeq, created == 0, nil
}

func (s *Store) Receipt(ctx context.Context, tenant, batchID string) (model.Receipt, error) {
	values, err := s.rdb.HGetAll(ctx, s.key("receipt", tenant, s.batchKey(batchID))).Result()
	if err != nil {
		return model.Receipt{}, err
	}
	if len(values) == 0 {
		return model.Receipt{}, ErrReceiptNotFound
	}
	return receiptFromHash(values), nil
}

func receiptFromHash(values map[string]string) model.Receipt {
	seq, _ := strconv.ParseInt(values["seq"], 10, 64)
	count, _ := strconv.ParseInt(values["record_count"], 10, 64)
	bytesValue, _ := strconv.ParseInt(values["bytes"], 10, 64)
	offset, _ := strconv.ParseInt(values["storage_offset"], 10, 64)
	return model.Receipt{
		TenantID:       values["tenant_id"],
		BatchID:        values["batch_id"],
		Seq:            seq,
		RecordCount:    count,
		Bytes:          bytesValue,
		EnqueuedAt:     values["enqueued_at"],
		Acknowledged:   values["acknowledged"] == "true",
		AcknowledgedAt: values["acknowledged_at"],
		StorageOffset:  offset,
	}
}

var ackScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
  return 0
end
redis.call('HSET', KEYS[1],
  'acknowledged', 'true',
  'acknowledged_at', ARGV[1],
  'storage_offset', ARGV[2])
local cursorSeq = tonumber(redis.call('HGET', KEYS[2], 'last_seq') or '0')
local seq = tonumber(ARGV[3])
if seq >= cursorSeq then
  redis.call('HSET', KEYS[2],
    'last_seq', ARGV[3],
    'last_batch_id', ARGV[4],
    'acknowledged_at', ARGV[1],
    'records_total', ARGV[5],
    'bytes_total', ARGV[6])
end
return 1
`)

type AckInput struct {
	TenantID      string
	BatchID       string
	Seq           int64
	AckedAt       time.Time
	StorageOffset int64
}

func (s *Store) Ack(ctx context.Context, in AckInput) (model.Cursor, error) {
	receipt, err := s.Receipt(ctx, in.TenantID, in.BatchID)
	if err != nil {
		return model.Cursor{}, err
	}
	cursorKey := s.key("cursor", in.TenantID)
	cursor, _ := s.Cursor(ctx, in.TenantID)
	recordsTotal := cursor.RecordsTotal
	bytesTotal := cursor.BytesTotal
	if !receipt.Acknowledged {
		recordsTotal += receipt.RecordCount
		bytesTotal += receipt.Bytes
	}
	ok, err := ackScript.Run(ctx, s.rdb,
		[]string{s.key("receipt", in.TenantID, s.batchKey(in.BatchID)), cursorKey},
		in.AckedAt.UTC().Format(time.RFC3339Nano),
		strconv.FormatInt(in.StorageOffset, 10),
		strconv.FormatInt(in.Seq, 10),
		in.BatchID,
		strconv.FormatInt(recordsTotal, 10),
		strconv.FormatInt(bytesTotal, 10),
	).Int64()
	if err != nil {
		return model.Cursor{}, err
	}
	if ok == 0 {
		return model.Cursor{}, ErrReceiptNotFound
	}
	return s.Cursor(ctx, in.TenantID)
}

func (s *Store) Cursor(ctx context.Context, tenant string) (model.Cursor, error) {
	values, err := s.rdb.HGetAll(ctx, s.key("cursor", tenant)).Result()
	if err != nil {
		return model.Cursor{}, err
	}
	seq, _ := strconv.ParseInt(values["last_seq"], 10, 64)
	records, _ := strconv.ParseInt(values["records_total"], 10, 64)
	bytesValue, _ := strconv.ParseInt(values["bytes_total"], 10, 64)
	return model.Cursor{
		TenantID:       tenant,
		LastSeq:        seq,
		LastBatchID:    values["last_batch_id"],
		AcknowledgedAt: values["acknowledged_at"],
		RecordsTotal:   records,
		BytesTotal:     bytesValue,
	}, nil
}

func (s *Store) Cursors(ctx context.Context, tenants []string) ([]model.Cursor, error) {
	out := make([]model.Cursor, 0, len(tenants))
	for _, tenant := range tenants {
		cursor, err := s.Cursor(ctx, tenant)
		if err != nil {
			return nil, err
		}
		out = append(out, cursor)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TenantID < out[j].TenantID })
	return out, nil
}

type QueueDepth struct {
	QueueBatches      int64
	ProcessingBatches int64
	TotalBatches      int64
}

func (s *Store) Depth(ctx context.Context, tenant string) (QueueDepth, error) {
	pipe := s.rdb.Pipeline()
	queue := pipe.LLen(ctx, s.QueueKey(tenant))
	processing := pipe.LLen(ctx, s.ProcessingKey(tenant))
	if _, err := pipe.Exec(ctx); err != nil {
		return QueueDepth{}, err
	}
	return QueueDepth{
		QueueBatches:      queue.Val(),
		ProcessingBatches: processing.Val(),
		TotalBatches:      queue.Val() + processing.Val(),
	}, nil
}

func (s *Store) LoadPending(ctx context.Context, tenant string) ([]model.QueueJob, error) {
	jobs := make(map[string]model.QueueJob)
	for _, key := range []string{s.ProcessingKey(tenant), s.QueueKey(tenant)} {
		raws, err := s.rdb.LRange(ctx, key, 0, -1).Result()
		if err != nil {
			return nil, err
		}
		for _, raw := range raws {
			var job model.QueueJob
			if err := json.Unmarshal([]byte(raw), &job); err != nil {
				continue
			}
			jobs[job.BatchID] = job
		}
	}
	out := make([]model.QueueJob, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, job)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seq == out[j].Seq {
			return out[i].BatchID < out[j].BatchID
		}
		return out[i].Seq < out[j].Seq
	})
	return out, nil
}

func (s *Store) RemoveProcessing(ctx context.Context, tenant, rawJob string) error {
	return s.rdb.LRem(ctx, s.ProcessingKey(tenant), 1, rawJob).Err()
}

var requeueScript = redis.NewScript(`
local processing = KEYS[1]
local queue = KEYS[2]
local moved = 0
while true do
  local job = redis.call('LPOP', processing)
  if not job then
    break
  end
  redis.call('RPUSH', queue, job)
  moved = moved + 1
end
return moved
`)

func (s *Store) RequeueProcessing(ctx context.Context, tenant string) (int64, error) {
	return requeueScript.Run(ctx, s.rdb, []string{s.ProcessingKey(tenant), s.QueueKey(tenant)}).Int64()
}

func (s *Store) Move(ctx context.Context, tenant string, timeout time.Duration) (string, error) {
	return s.rdb.BLMove(ctx, s.QueueKey(tenant), s.ProcessingKey(tenant), "RIGHT", "LEFT", timeout).Result()
}

func (s *Store) SetControl(ctx context.Context, name, value string) error {
	return s.rdb.Set(ctx, s.key("control", name), value, 0).Err()
}

func (s *Store) GetControl(ctx context.Context, name string) (string, error) {
	value, err := s.rdb.Get(ctx, s.key("control", name)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return value, err
}

func (s *Store) FlushAll(ctx context.Context) error {
	return s.rdb.FlushDB(ctx).Err()
}
