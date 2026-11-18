package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"archive/internal/model"
)

var ErrRedisNotFound = errors.New("redis key not found")

type Redis struct {
	client    *goredis.Client
	namespace string
	version   string
}

func OpenRedis(ctx context.Context, addr, password string, db int, namespace string) (*Redis, error) {
	client := goredis.NewClient(&goredis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		PoolSize:     16,
	})
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("redis ping %s: %w", addr, err)
	}
	version := ""
	if info, err := client.Info(ctx, "server").Result(); err == nil {
		for _, line := range strings.Split(info, "\n") {
			if strings.HasPrefix(line, "redis_version:") {
				version = strings.TrimSpace(strings.TrimPrefix(line, "redis_version:"))
				break
			}
		}
	}
	return &Redis{client: client, namespace: strings.TrimRight(namespace, ":"), version: version}, nil
}

func (r *Redis) Close() error { return r.client.Close() }

func (r *Redis) Version() string { return r.version }

func (r *Redis) Ping(ctx context.Context) error { return r.client.Ping(ctx).Err() }

func (r *Redis) key(parts ...string) string {
	return r.namespace + ":" + strings.Join(parts, ":")
}

func (r *Redis) ObjectKey(id string) string { return r.key("object", id) }
func (r *Redis) StripeKey(id string) string { return r.key("stripe", id) }
func (r *Redis) DirsKey() string            { return r.key("dirs") }
func (r *Redis) LastReconcileKey() string   { return r.key("reconcile", "last") }

func (r *Redis) InitDirs(ctx context.Context, now time.Time) error {
	key := r.DirsKey()
	pipe := r.client.TxPipeline()
	pipe.HSetNX(ctx, key, "epoch", 0)
	for i := 0; i < model.BlockCount; i++ {
		pipe.HSetNX(ctx, key, fmt.Sprintf("online.%d", i), 1)
		pipe.HSetNX(ctx, key, fmt.Sprintf("reason.%d", i), "")
		pipe.HSetNX(ctx, key, fmt.Sprintf("updated.%d", i), now.UTC().Format(time.RFC3339Nano))
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (r *Redis) GetDirs(ctx context.Context) (model.DirSet, error) {
	raw, err := r.client.HGetAll(ctx, r.DirsKey()).Result()
	if err != nil {
		return model.DirSet{}, err
	}
	if len(raw) == 0 {
		return model.DirSet{}, ErrRedisNotFound
	}
	var out model.DirSet
	out.Epoch, _ = strconv.ParseInt(raw["epoch"], 10, 64)
	for i := 0; i < model.BlockCount; i++ {
		online := raw[fmt.Sprintf("online.%d", i)] != "0"
		out.Dirs[i] = model.DirState{
			Index:     i,
			Online:    online,
			Reason:    raw[fmt.Sprintf("reason.%d", i)],
			UpdatedAt: parseTime(raw[fmt.Sprintf("updated.%d", i)]),
		}
	}
	return out, nil
}

var setDirScript = goredis.NewScript(`
redis.call("HSET", KEYS[1],
  "online."..ARGV[1], ARGV[2],
  "reason."..ARGV[1], ARGV[3],
  "updated."..ARGV[1], ARGV[4])
return redis.call("HINCRBY", KEYS[1], "epoch", 1)
`)

func (r *Redis) SetDirOnline(ctx context.Context, index int, online bool, reason string, now time.Time) (int64, error) {
	onlineVal := "0"
	if online {
		onlineVal = "1"
	}
	epoch, err := setDirScript.Run(ctx, r.client, []string{r.DirsKey()},
		index, onlineVal, reason, now.UTC().Format(time.RFC3339Nano)).Int64()
	return epoch, err
}

func (r *Redis) LayoutEpoch(ctx context.Context) (int64, error) {
	raw, err := r.client.HGet(ctx, r.DirsKey(), "epoch").Result()
	if errors.Is(err, goredis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, _ := strconv.ParseInt(raw, 10, 64)
	return n, nil
}

func (r *Redis) ObjectExists(ctx context.Context, id string) (bool, error) {
	return r.client.SIsMember(ctx, r.key("objects"), id).Result()
}

func (r *Redis) SaveObject(ctx context.Context, obj model.ObjectMeta, stripes []model.StripeMeta) error {
	pipe := r.client.TxPipeline()
	for i := range stripes {
		stripe := &stripes[i]
		stripe.NormalizeBlocks()
		pipe.HSet(ctx, r.StripeKey(stripe.ID), stripeHash(stripe))
		pipe.SAdd(ctx, r.key("stripes"), stripe.ID)
	}
	pipe.HSet(ctx, r.ObjectKey(obj.ID), objectHash(obj))
	pipe.SAdd(ctx, r.key("objects"), obj.ID)
	_, err := pipe.Exec(ctx)
	return err
}

func objectHash(obj model.ObjectMeta) map[string]any {
	stripes, _ := json.Marshal(obj.Stripes)
	return map[string]any{
		"object_id":    obj.ID,
		"size":         obj.Size,
		"stripe_size":  obj.StripeSize,
		"data_sha256":  obj.DataSHA256,
		"created_at":   obj.CreatedAt.UTC().Format(time.RFC3339Nano),
		"seed":         obj.Seed,
		"stripe_count": len(obj.Stripes),
		"stripes":      string(stripes),
	}
}

func stripeHash(s *model.StripeMeta) map[string]any {
	blocks, _ := json.Marshal(s.Blocks)
	missing, _ := json.Marshal(s.MissingBlocks)
	return map[string]any{
		"stripe_id":             s.ID,
		"object_id":             s.ObjectID,
		"stripe_index":          s.Index,
		"offset_start":          s.OffsetStart,
		"offset_end":            s.OffsetEnd,
		"length":                s.Length,
		"shard_length":          s.ShardLength,
		"data_sha256":           s.DataSHA256,
		"status":                s.Status,
		"available_blocks":      s.AvailableBlocks,
		"missing_blocks":        string(missing),
		"degraded_reason":       s.DegradedReason,
		"created_at":            s.CreatedAt.UTC().Format(time.RFC3339Nano),
		"last_read_at":          formatTime(s.LastReadAt),
		"read_count":            s.ReadCount,
		"repair_epoch":          s.RepairEpoch,
		"repair_state":          s.RepairState,
		"repair_job_id":         s.RepairJobID,
		"repair_cooldown_until": formatTime(s.CooldownUntil),
		"last_error":            s.LastError,
		"blocks":                string(blocks),
	}
}

func (r *Redis) GetObject(ctx context.Context, id string) (model.ObjectMeta, error) {
	raw, err := r.client.HGetAll(ctx, r.ObjectKey(id)).Result()
	if err != nil {
		return model.ObjectMeta{}, err
	}
	if len(raw) == 0 {
		return model.ObjectMeta{}, ErrRedisNotFound
	}
	return parseObject(raw)
}

func parseObject(raw map[string]string) (model.ObjectMeta, error) {
	var obj model.ObjectMeta
	obj.ID = raw["object_id"]
	obj.Size, _ = strconv.ParseInt(raw["size"], 10, 64)
	obj.StripeSize, _ = strconv.ParseInt(raw["stripe_size"], 10, 64)
	obj.DataSHA256 = raw["data_sha256"]
	obj.CreatedAt = parseTime(raw["created_at"])
	obj.Seed, _ = strconv.ParseInt(raw["seed"], 10, 64)
	if s := raw["stripes"]; s != "" {
		if err := json.Unmarshal([]byte(s), &obj.Stripes); err != nil {
			return obj, err
		}
	}
	if obj.ID == "" {
		return obj, ErrRedisNotFound
	}
	return obj, nil
}

func (r *Redis) GetStripe(ctx context.Context, id string) (model.StripeMeta, error) {
	raw, err := r.client.HGetAll(ctx, r.StripeKey(id)).Result()
	if err != nil {
		return model.StripeMeta{}, err
	}
	if len(raw) == 0 {
		return model.StripeMeta{}, ErrRedisNotFound
	}
	return parseStripe(raw)
}

func parseStripe(raw map[string]string) (model.StripeMeta, error) {
	var s model.StripeMeta
	s.ID = raw["stripe_id"]
	s.ObjectID = raw["object_id"]
	s.Index, _ = strconv.Atoi(raw["stripe_index"])
	s.OffsetStart, _ = strconv.ParseInt(raw["offset_start"], 10, 64)
	s.OffsetEnd, _ = strconv.ParseInt(raw["offset_end"], 10, 64)
	s.Length, _ = strconv.ParseInt(raw["length"], 10, 64)
	s.ShardLength, _ = strconv.ParseInt(raw["shard_length"], 10, 64)
	s.DataSHA256 = raw["data_sha256"]
	s.Status = raw["status"]
	s.AvailableBlocks, _ = strconv.Atoi(raw["available_blocks"])
	if v := raw["missing_blocks"]; v != "" {
		_ = json.Unmarshal([]byte(v), &s.MissingBlocks)
	}
	s.DegradedReason = raw["degraded_reason"]
	s.CreatedAt = parseTime(raw["created_at"])
	s.LastReadAt = parseTime(raw["last_read_at"])
	s.ReadCount, _ = strconv.ParseInt(raw["read_count"], 10, 64)
	s.RepairEpoch, _ = strconv.ParseInt(raw["repair_epoch"], 10, 64)
	s.RepairState = raw["repair_state"]
	s.RepairJobID = raw["repair_job_id"]
	s.CooldownUntil = parseTime(raw["repair_cooldown_until"])
	s.LastError = raw["last_error"]
	if v := raw["blocks"]; v != "" {
		if err := json.Unmarshal([]byte(v), &s.Blocks); err != nil {
			return s, err
		}
	}
	s.NormalizeBlocks()
	if s.ID == "" {
		return s, ErrRedisNotFound
	}
	return s, nil
}

func (r *Redis) GetStripes(ctx context.Context, ids []string) ([]model.StripeMeta, error) {
	out := make([]model.StripeMeta, 0, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	pipe := r.client.Pipeline()
	cmds := make([]*goredis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, r.StripeKey(id))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, goredis.Nil) {
		return nil, err
	}
	for i, cmd := range cmds {
		raw, err := cmd.Result()
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("stripe %s: %w", ids[i], ErrRedisNotFound)
		}
		s, err := parseStripe(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (r *Redis) ListStripeIDs(ctx context.Context) ([]string, error) {
	ids, err := r.client.SMembers(ctx, r.key("stripes")).Result()
	if err != nil {
		return nil, err
	}
	sort.Strings(ids)
	return ids, nil
}

func (r *Redis) ListAllStripes(ctx context.Context) ([]model.StripeMeta, error) {
	ids, err := r.ListStripeIDs(ctx)
	if err != nil {
		return nil, err
	}
	return r.GetStripes(ctx, ids)
}

func (r *Redis) ListObjects(ctx context.Context) ([]model.ObjectMeta, error) {
	ids, err := r.client.SMembers(ctx, r.key("objects")).Result()
	if err != nil {
		return nil, err
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return []model.ObjectMeta{}, nil
	}
	pipe := r.client.Pipeline()
	cmds := make([]*goredis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, r.ObjectKey(id))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, goredis.Nil) {
		return nil, err
	}
	out := make([]model.ObjectMeta, 0, len(ids))
	for _, cmd := range cmds {
		obj, err := parseObject(cmd.Val())
		if err != nil {
			return nil, err
		}
		out = append(out, obj)
	}
	return out, nil
}

func (r *Redis) UpdateStripeState(ctx context.Context, s *model.StripeMeta) error {
	return r.UpdateStripeStates(ctx, []*model.StripeMeta{s})
}

func (r *Redis) UpdateStripeStates(ctx context.Context, stripes []*model.StripeMeta) error {
	if len(stripes) == 0 {
		return nil
	}
	pipe := r.client.Pipeline()
	for _, s := range stripes {
		missing, _ := json.Marshal(s.MissingBlocks)
		pipe.HSet(ctx, r.StripeKey(s.ID), map[string]any{
			"status":                s.Status,
			"available_blocks":      s.AvailableBlocks,
			"missing_blocks":        string(missing),
			"degraded_reason":       s.DegradedReason,
			"repair_state":          s.RepairState,
			"repair_job_id":         s.RepairJobID,
			"repair_cooldown_until": formatTime(s.CooldownUntil),
			"last_error":            s.LastError,
		})
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (r *Redis) CommitRepair(ctx context.Context, s *model.StripeMeta) error {
	s.NormalizeBlocks()
	missing, _ := json.Marshal(s.MissingBlocks)
	blocks, _ := json.Marshal(s.Blocks)
	err := r.client.HSet(ctx, r.StripeKey(s.ID), map[string]any{
		"blocks":                string(blocks),
		"status":                s.Status,
		"available_blocks":      s.AvailableBlocks,
		"missing_blocks":        string(missing),
		"degraded_reason":       s.DegradedReason,
		"repair_epoch":          s.RepairEpoch,
		"repair_state":          "",
		"repair_job_id":         "",
		"repair_cooldown_until": "",
		"last_error":            "",
	}).Err()
	return err
}

func (r *Redis) PersistStripe(ctx context.Context, s *model.StripeMeta) error {
	s.NormalizeBlocks()
	missing, _ := json.Marshal(s.MissingBlocks)
	blocks, _ := json.Marshal(s.Blocks)
	return r.client.HSet(ctx, r.StripeKey(s.ID), map[string]any{
		"blocks":           string(blocks),
		"status":           s.Status,
		"available_blocks": s.AvailableBlocks,
		"missing_blocks":   string(missing),
		"degraded_reason":  s.DegradedReason,
	}).Err()
}

func (r *Redis) SetRepairState(ctx context.Context, stripeID, state, jobID, lastErr string) error {
	return r.client.HSet(ctx, r.StripeKey(stripeID), map[string]any{
		"repair_state":  state,
		"repair_job_id": jobID,
		"last_error":    lastErr,
	}).Err()
}

func (r *Redis) SetRepairFailure(ctx context.Context, stripeID, lastErr string, cooldownUntil time.Time) error {
	return r.client.HSet(ctx, r.StripeKey(stripeID), map[string]any{
		"repair_state":          "",
		"repair_job_id":         "",
		"repair_cooldown_until": cooldownUntil.UTC().Format(time.RFC3339Nano),
		"last_error":            lastErr,
	}).Err()
}

func (r *Redis) SetSkip(ctx context.Context, stripeID, reason string, now time.Time) error {
	return r.client.HSet(ctx, r.StripeKey(stripeID), map[string]any{
		"last_skip_reason": reason,
		"last_skip_at":     now.UTC().Format(time.RFC3339Nano),
	}).Err()
}

func (r *Redis) TouchStripeReads(ctx context.Context, ids []string, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	pipe := r.client.Pipeline()
	ts := now.UTC().Format(time.RFC3339Nano)
	for _, id := range ids {
		pipe.HSet(ctx, r.StripeKey(id), "last_read_at", ts)
		pipe.HIncrBy(ctx, r.StripeKey(id), "read_count", 1)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (r *Redis) AcquireLock(ctx context.Context, name, token string, ttl time.Duration) (bool, error) {
	return r.client.SetNX(ctx, r.key("lock", name), token, ttl).Result()
}

var releaseLockScript = goredis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

func (r *Redis) ReleaseLock(ctx context.Context, name, token string) error {
	return releaseLockScript.Run(ctx, r.client, []string{r.key("lock", name)}, token).Err()
}

func (r *Redis) SetLastReconciliation(ctx context.Context, payload []byte) error {
	return r.client.Set(ctx, r.LastReconcileKey(), payload, 0).Err()
}

func (r *Redis) GetLastReconciliation(ctx context.Context) ([]byte, error) {
	b, err := r.client.Get(ctx, r.LastReconcileKey()).Bytes()
	if errors.Is(err, goredis.Nil) {
		return nil, ErrRedisNotFound
	}
	return b, err
}

func (r *Redis) ResetNamespace(ctx context.Context) error {
	var cursor uint64
	pattern := r.namespace + ":*"
	for {
		keys, next, err := r.client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := r.client.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, raw)
	return t
}
