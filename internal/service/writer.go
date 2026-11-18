package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"archive/internal/codec"
	"archive/internal/model"
)

var objectIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type WriteResult struct {
	ObjectID         string `json:"object_id"`
	Size             int64  `json:"size"`
	DataSHA256       string `json:"data_sha256"`
	StripeSize       int64  `json:"stripe_size"`
	StripeCount      int    `json:"stripe_count"`
	BlocksWritten    int    `json:"blocks_written"`
	DegradedStripes  int    `json:"degraded_stripes"`
	Unrecoverable    int    `json:"unrecoverable_stripes"`
	ApproxStoredByte int64  `json:"approx_stored_bytes"`
	CreatedAt        string `json:"created_at"`
}

func (s *Service) WriteObject(ctx context.Context, objectID string, data []byte, seedValue int64) (WriteResult, error) {
	if objectID == "" {
		objectID = "obj-" + randomID()
	}
	if !objectIDPattern.MatchString(objectID) {
		return WriteResult{}, NewAppError("INVALID_OBJECT_ID", "object_id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}")
	}
	if int64(len(data)) > s.cfg.MaxObjectBytes {
		return WriteResult{}, NewAppError("OBJECT_TOO_LARGE", fmt.Sprintf("object exceeds max_object_bytes=%d", s.cfg.MaxObjectBytes))
	}
	lockToken := randomID()
	locked, err := s.redis.AcquireLock(ctx, "object:"+objectID, lockToken, 5*time.Minute)
	if err != nil {
		return WriteResult{}, err
	}
	if !locked {
		return WriteResult{}, NewAppError("OBJECT_WRITE_BUSY", "another writer is creating this object")
	}
	defer func() { _ = s.redis.ReleaseLock(context.Background(), "object:"+objectID, lockToken) }()

	exists, err := s.redis.ObjectExists(ctx, objectID)
	if err != nil {
		return WriteResult{}, err
	}
	if exists {
		return WriteResult{}, NewAppError("OBJECT_EXISTS", "object already exists")
	}
	dirs, err := s.dirs(ctx)
	if err != nil {
		return WriteResult{}, err
	}
	onlineCount := 0
	for i := 0; i < model.BlockCount; i++ {
		if dirs.IsOnline(i) {
			onlineCount++
		}
	}
	if onlineCount < model.DataBlocks {
		return WriteResult{}, NewAppError("INSUFFICIENT_WRITE_TARGETS", fmt.Sprintf("only %d block directories online; at least %d required", onlineCount, model.DataBlocks))
	}

	created := time.Now().UTC()
	obj := model.ObjectMeta{
		ID:         objectID,
		Size:       int64(len(data)),
		StripeSize: s.cfg.StripeSize,
		DataSHA256: codec.SHA256(data),
		CreatedAt:  created,
		Seed:       seedValue,
	}
	if len(data) == 0 {
		s.mu.Lock()
		err := s.redis.SaveObject(ctx, obj, nil)
		s.mu.Unlock()
		if err != nil {
			return WriteResult{}, err
		}
		return WriteResult{ObjectID: objectID, Size: 0, DataSHA256: obj.DataSHA256, StripeSize: s.cfg.StripeSize,
			CreatedAt: created.Format(time.RFC3339Nano)}, nil
	}

	stripeCount := int((int64(len(data)) + s.cfg.StripeSize - 1) / s.cfg.StripeSize)
	obj.Stripes = make([]string, 0, stripeCount)
	stripes := make([]model.StripeMeta, 0, stripeCount)
	written := 0
	degraded := 0
	unrecoverable := 0
	var storedBytes int64
	writtenRels := make([]string, 0, stripeCount*model.BlockCount)

	rollback := func() {
		for _, rel := range writtenRels {
			s.removeRel(rel)
		}
	}

	for i := 0; i < stripeCount; i++ {
		start := int64(i) * s.cfg.StripeSize
		end := start + s.cfg.StripeSize
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		chunk := data[start:end]
		shards, shardLen, err := s.codec.Encode(chunk)
		if err != nil {
			rollback()
			return WriteResult{}, err
		}
		stripeID := fmt.Sprintf("%s-s%06d", objectID, i)
		st := model.StripeMeta{
			ID:          stripeID,
			ObjectID:    objectID,
			Index:       i,
			OffsetStart: start,
			OffsetEnd:   end,
			Length:      end - start,
			ShardLength: shardLen,
			DataSHA256:  codec.SHA256(chunk),
			CreatedAt:   created,
			Blocks:      make([]model.BlockMeta, model.BlockCount),
		}
		for bi := 0; bi < model.BlockCount; bi++ {
			block := model.BlockMeta{Index: bi}
			if dirs.IsOnline(bi) {
				rel := primaryRel(bi, stripeID)
				if err := s.writeAtomic(rel, shards[bi]); err != nil {
					rollback()
					return WriteResult{}, fmt.Errorf("write stripe %s block %d: %w", stripeID, bi, err)
				}
				writtenRels = append(writtenRels, rel)
				written++
				block.PrimaryPath = rel
				block.PrimaryLength = int64(len(shards[bi]))
				block.PrimarySHA256 = codec.SHA256(shards[bi])
				block.PrimaryExists = true
			}
			st.Blocks[bi] = block
		}
		st.Refresh(dirs)
		storedBytes += st.ShardLength * model.BlockCount
		if st.Status == model.StatusDegraded {
			degraded++
		}
		if st.Status == model.StatusUnrecoverable {
			unrecoverable++
		}
		obj.Stripes = append(obj.Stripes, stripeID)
		stripes = append(stripes, st)
	}
	if unrecoverable > 0 {
		rollback()
		return WriteResult{}, NewAppError("INSUFFICIENT_WRITE_TARGETS", "at least one stripe would be unrecoverable")
	}

	s.mu.Lock()
	currentDirs, err := s.dirs(ctx)
	if err != nil {
		s.mu.Unlock()
		rollback()
		return WriteResult{}, err
	}
	degraded, unrecoverable = 0, 0
	for i := range stripes {
		stripes[i].Refresh(currentDirs)
		switch stripes[i].Status {
		case model.StatusDegraded:
			degraded++
		case model.StatusUnrecoverable:
			unrecoverable++
		}
	}
	if unrecoverable > 0 {
		s.mu.Unlock()
		rollback()
		return WriteResult{}, NewAppError("INSUFFICIENT_WRITE_TARGETS", "a block directory went offline during object write")
	}
	err = s.redis.SaveObject(ctx, obj, stripes)
	s.mu.Unlock()
	if err != nil {
		rollback()
		return WriteResult{}, err
	}
	if degraded > 0 {
		records := make([]model.DegradationRecord, 0, degraded)
		for i := range stripes {
			if stripes[i].Status != model.StatusDegraded {
				continue
			}
			details, _ := json.Marshal(map[string]any{
				"missing_blocks": stripes[i].MissingBlocks,
				"available":      stripes[i].AvailableBlocks,
				"reason":         stripes[i].DegradedReason,
			})
			records = append(records, model.DegradationRecord{
				StripeID:        stripes[i].ID,
				ObjectID:        objectID,
				DirIndex:        -1,
				Action:          "write",
				Reason:          "write_with_offline_dir",
				AvailableBlocks: stripes[i].AvailableBlocks,
				Status:          stripes[i].Status,
				Details:         string(details),
			})
		}
		details, _ := json.Marshal(map[string]any{
			"object_id": objectID, "degraded_stripes": degraded, "bytes": len(data),
		})
		if err := s.db.RecordDirEvent(ctx, "write", -1, "write_with_offline_dir", records, string(details)); err != nil {
			s.log.Printf("audit write degradation for %s: %v", objectID, err)
		}
	}
	s.WakeRepair()
	return WriteResult{
		ObjectID:         objectID,
		Size:             int64(len(data)),
		DataSHA256:       obj.DataSHA256,
		StripeSize:       s.cfg.StripeSize,
		StripeCount:      stripeCount,
		BlocksWritten:    written,
		DegradedStripes:  degraded,
		Unrecoverable:    unrecoverable,
		ApproxStoredByte: storedBytes,
		CreatedAt:        created.Format(time.RFC3339Nano),
	}, nil
}
