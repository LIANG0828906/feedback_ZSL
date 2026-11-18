package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"archive/internal/codec"
	"archive/internal/config"
	"archive/internal/model"
	"archive/internal/store"
)

type AppError struct {
	Code            string
	Message         string
	StripeID        string
	ObjectID        string
	MissingBlocks   []int
	AvailableBlocks int
}

func (e *AppError) Error() string {
	if e.StripeID != "" {
		return fmt.Sprintf("%s: %s (stripe=%s missing=%v available=%d)", e.Code, e.Message, e.StripeID, e.MissingBlocks, e.AvailableBlocks)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func NewAppError(code, message string) *AppError {
	return &AppError{Code: code, Message: message}
}

type Service struct {
	cfg       config.Config
	dataRoot  string
	dbPath    string
	redis     *store.Redis
	db        *store.SQLite
	codec     *codec.Codec
	log       *log.Logger
	startedAt time.Time

	mu         sync.RWMutex
	metaMu     sync.Mutex
	repairSem  chan struct{}
	wake       chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	active     atomic.Int32
	skipMu     sync.Mutex
	skipRecord map[string]time.Time
}

func New(ctx context.Context, cfg config.Config, logger *log.Logger) (*Service, error) {
	if logger == nil {
		logger = log.New(os.Stdout, "archive ", log.LstdFlags|log.LUTC)
	}
	dataRoot, err := cfg.AbsDataRoot()
	if err != nil {
		return nil, err
	}
	dbPath, err := cfg.AbsSQLitePath()
	if err != nil {
		return nil, err
	}
	if err := ensureLayout(dataRoot); err != nil {
		return nil, err
	}
	c, err := codec.New()
	if err != nil {
		return nil, err
	}
	r, err := store.OpenRedis(ctx, cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB, cfg.RedisNamespace)
	if err != nil {
		return nil, err
	}
	db, err := store.OpenSQLite(dbPath)
	if err != nil {
		r.Close()
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s := &Service{
		cfg:        cfg,
		dataRoot:   dataRoot,
		dbPath:     dbPath,
		redis:      r,
		db:         db,
		codec:      c,
		log:        logger,
		startedAt:  time.Now().UTC(),
		repairSem:  make(chan struct{}, cfg.RepairConcurrency),
		wake:       make(chan struct{}, 1),
		ctx:        runCtx,
		cancel:     cancel,
		skipRecord: make(map[string]time.Time),
	}
	if err := r.InitDirs(ctx, time.Now()); err != nil {
		s.Close()
		return nil, err
	}
	s.log.Printf("started data_root=%s redis=%s db=%s redis_version=%s", dataRoot, cfg.RedisAddr, dbPath, r.Version())
	return s, nil
}

func (s *Service) Close() {
	s.cancel()
	s.wg.Wait()
	if s.redis != nil {
		_ = s.redis.Close()
	}
	if s.db != nil {
		_ = s.db.Close()
	}
}

func (s *Service) Start() {
	if done, err := s.applyCompletedRepairs(s.ctx); err != nil {
		s.log.Printf("apply completed repairs: %v", err)
	} else if done > 0 {
		s.log.Printf("applied %d completed repair metadata records", done)
	}
	if s.cfg.AutoRepair {
		s.wg.Add(1)
		go s.repairLoop()
	}
	s.wg.Add(1)
	go s.resumeLoop()
}

func (s *Service) WakeRepair() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) Config() config.Config { return s.cfg }
func (s *Service) StartedAt() time.Time  { return s.startedAt }
func (s *Service) RedisVersion() string  { return s.redis.Version() }
func (s *Service) DBPath() string        { return s.dbPath }

func (s *Service) Today(now time.Time) string { return s.cfg.Today(now) }

func ensureLayout(root string) error {
	dirs := []string{"tmp"}
	for i := 0; i < model.BlockCount; i++ {
		dirs = append(dirs, filepath.Join("blocks", fmt.Sprintf("block-%d", i)))
		dirs = append(dirs, filepath.Join("standby", fmt.Sprintf("block-%d", i)))
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	return nil
}

func primaryRel(index int, stripeID string) string {
	return filepath.ToSlash(filepath.Join("blocks", fmt.Sprintf("block-%d", index), stripeID, fmt.Sprintf("%d.blk", index)))
}

func standbyRel(index int, stripeID string) string {
	return filepath.ToSlash(filepath.Join("standby", fmt.Sprintf("block-%d", index), stripeID, fmt.Sprintf("%d.blk", index)))
}

func (s *Service) resolveRel(rel string) (string, error) {
	if rel == "" {
		return "", errors.New("empty relative path")
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe relative path %q", rel)
	}
	full := filepath.Join(s.dataRoot, clean)
	if full != s.dataRoot && !strings.HasPrefix(full, s.dataRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes data root: %q", rel)
	}
	return full, nil
}

func (s *Service) writeAtomic(rel string, data []byte) error {
	full, err := s.resolveRel(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(full), ".staging-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(full)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (s *Service) removeRel(rel string) {
	if full, err := s.resolveRel(rel); err == nil {
		_ = os.Remove(full)
	}
}

func (s *Service) readRel(rel string) ([]byte, error) {
	full, err := s.resolveRel(rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

func (s *Service) fileLength(rel string) (int64, error) {
	full, err := s.resolveRel(rel)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(full)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func (s *Service) dirs(ctx context.Context) (model.DirSet, error) {
	d, err := s.redis.GetDirs(ctx)
	if errors.Is(err, store.ErrRedisNotFound) {
		if err := s.redis.InitDirs(ctx, time.Now()); err != nil {
			return model.DirSet{}, err
		}
		return s.redis.GetDirs(ctx)
	}
	return d, err
}

func randomID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

type DirOperationResult struct {
	Dir              int    `json:"dir"`
	Name             string `json:"name"`
	Online           bool   `json:"online"`
	Reason           string `json:"reason,omitempty"`
	LayoutEpoch      int64  `json:"layout_epoch"`
	AffectedStripes  int    `json:"affected_stripes"`
	DegradedStripes  int    `json:"degraded_stripes"`
	Unrecoverable    int    `json:"unrecoverable_stripes"`
	AuditRowsWritten int    `json:"audit_rows_written"`
}

func (s *Service) SetDir(ctx context.Context, index int, online bool, reason string) (DirOperationResult, error) {
	if index < 0 || index >= model.BlockCount {
		return DirOperationResult{}, NewAppError("INVALID_DIR", "block directory must be between 0 and 5")
	}
	if reason == "" {
		if online {
			reason = "operator_restore"
		} else {
			reason = "operator_offline"
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	epoch, err := s.redis.SetDirOnline(ctx, index, online, reason, time.Now())
	if err != nil {
		return DirOperationResult{}, err
	}
	dirs, err := s.dirs(ctx)
	if err != nil {
		return DirOperationResult{}, err
	}
	stripes, err := s.redis.ListAllStripes(ctx)
	if err != nil {
		return DirOperationResult{}, err
	}
	action := "offline"
	if online {
		action = "online"
	}
	degraded, unrecoverable := 0, 0
	records := make([]model.DegradationRecord, 0, len(stripes))
	updates := make([]*model.StripeMeta, 0, len(stripes))
	for i := range stripes {
		st := &stripes[i]
		st.Refresh(dirs)
		switch st.Status {
		case model.StatusDegraded:
			degraded++
		case model.StatusUnrecoverable:
			unrecoverable++
		}
		st.RepairState = ""
		st.RepairJobID = ""
		updates = append(updates, st)
		detail, _ := jsonMarshal(map[string]any{
			"dir":              index,
			"online":           online,
			"missing_blocks":   st.MissingBlocks,
			"available_blocks": st.AvailableBlocks,
		})
		records = append(records, model.DegradationRecord{
			StripeID:        st.ID,
			ObjectID:        st.ObjectID,
			DirIndex:        index,
			Action:          action,
			Reason:          reason,
			AvailableBlocks: st.AvailableBlocks,
			Status:          st.Status,
			Details:         detail,
		})
	}
	if err := s.redis.UpdateStripeStates(ctx, updates); err != nil {
		return DirOperationResult{}, err
	}
	details, _ := jsonMarshal(map[string]any{
		"dir": index, "online": online, "reason": reason, "layout_epoch": epoch,
		"affected_stripes": len(stripes), "degraded_stripes": degraded, "unrecoverable_stripes": unrecoverable,
	})
	if err := s.db.RecordDirEvent(ctx, action, index, reason, records, details); err != nil {
		return DirOperationResult{}, err
	}
	s.WakeRepair()
	return DirOperationResult{
		Dir:              index,
		Name:             fmt.Sprintf("block-%d", index),
		Online:           online,
		Reason:           reason,
		LayoutEpoch:      epoch,
		AffectedStripes:  len(stripes),
		DegradedStripes:  degraded,
		Unrecoverable:    unrecoverable,
		AuditRowsWritten: len(records),
	}, nil
}

func (s *Service) OfflineDir(ctx context.Context, index int, reason string) (DirOperationResult, error) {
	return s.SetDir(ctx, index, false, reason)
}

func (s *Service) OnlineDir(ctx context.Context, index int, reason string) (DirOperationResult, error) {
	return s.SetDir(ctx, index, true, reason)
}

func (s *Service) DirStates(ctx context.Context) (model.DirSet, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dirs(ctx)
}

func (s *Service) ObjectMeta(ctx context.Context, id string) (model.ObjectMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	obj, err := s.redis.GetObject(ctx, id)
	if errors.Is(err, store.ErrRedisNotFound) {
		return obj, NewAppError("OBJECT_NOT_FOUND", "object does not exist")
	}
	return obj, err
}

func (s *Service) Quota(ctx context.Context) (model.QuotaUsage, error) {
	return s.db.QuotaUsage(ctx, s.Today(time.Now()), s.cfg.WritebackQuotaBytes)
}

func (s *Service) RunRepairOnce(ctx context.Context) (ScheduleSummary, error) {
	return s.scheduleOnce(ctx)
}

func (s *Service) AuditStripe(ctx context.Context, stripeID string) (store.AuditResult, error) {
	return s.db.AuditForStripe(ctx, stripeID)
}

func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
