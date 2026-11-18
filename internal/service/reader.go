package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"archive/internal/codec"
	"archive/internal/model"
	"archive/internal/store"
)

type ReadResult struct {
	ObjectID             string `json:"object_id"`
	Size                 int64  `json:"size"`
	Start                int64  `json:"start"`
	End                  int64  `json:"end"`
	BytesReturned        int    `json:"bytes_returned"`
	StripesTouched       int    `json:"stripes_touched"`
	Degraded             bool   `json:"degraded"`
	ReconstructedBlocks  int    `json:"reconstructed_blocks"`
	ReconstructedStripes int    `json:"reconstructed_stripes"`
	MissingBlocks        []int  `json:"missing_blocks,omitempty"`
	Bytes                []byte `json:"-"`
}

type stripeRead struct {
	data                []byte
	reconstructed       []int
	usedStandby         []int
	missing             []int
	invalidPrimary      []int
	invalidBackup       []int
	availableBeforeRead int
}

func (s *Service) ReadRange(ctx context.Context, objectID string, start, end int64) (ReadResult, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		res, retry, err := s.readRangeOnce(ctx, objectID, start, end)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !retry {
			return ReadResult{}, err
		}
	}
	if lastErr == nil {
		lastErr = NewAppError("LAYOUT_CHANGED", "layout changed during read")
	}
	return ReadResult{}, lastErr
}

func (s *Service) readRangeOnce(ctx context.Context, objectID string, start, end int64) (ReadResult, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	obj, err := s.redis.GetObject(ctx, objectID)
	if err != nil {
		if errors.Is(err, store.ErrRedisNotFound) {
			return ReadResult{}, false, NewAppError("OBJECT_NOT_FOUND", "object does not exist")
		}
		return ReadResult{}, false, err
	}
	if start < 0 || end < start || end >= obj.Size {
		return ReadResult{}, false, NewAppError("INVALID_RANGE", fmt.Sprintf("range %d-%d is outside object size %d", start, end, obj.Size))
	}
	if obj.Size == 0 {
		return ReadResult{ObjectID: obj.ID, Size: 0, Start: 0, End: -1, Bytes: []byte{}}, false, nil
	}
	dirs, err := s.dirs(ctx)
	if err != nil {
		return ReadResult{}, false, err
	}
	startEpoch := dirs.Epoch
	stripes, err := s.redis.GetStripes(ctx, obj.Stripes)
	if err != nil {
		if errors.Is(err, store.ErrRedisNotFound) {
			return ReadResult{}, false, NewAppError("STRIPE_METADATA_MISSING", "one or more stripe metadata records are missing")
		}
		return ReadResult{}, false, err
	}

	result := ReadResult{
		ObjectID: obj.ID,
		Size:     obj.Size,
		Start:    start,
		End:      end,
		Bytes:    make([]byte, 0, end-start+1),
	}
	degradedStripes := make([]model.StripeMeta, 0)
	for i := range stripes {
		st := &stripes[i]
		if st.OffsetEnd <= start || st.OffsetStart > end {
			continue
		}
		st.Refresh(dirs)
		sr, err := s.readStripe(ctx, st, dirs)
		if err != nil {
			return ReadResult{}, false, err
		}
		s.markInvalidBlocks(ctx, st, dirs, sr.invalidPrimary, sr.invalidBackup)
		from := start - st.OffsetStart
		if from < 0 {
			from = 0
		}
		to := end - st.OffsetStart + 1
		if to > st.Length {
			to = st.Length
		}
		if from > to || to > int64(len(sr.data)) {
			return ReadResult{}, false, NewAppError("STRIPE_RANGE_ERROR", "stripe data range is inconsistent")
		}
		result.Bytes = append(result.Bytes, sr.data[from:to]...)
		result.StripesTouched++
		if len(sr.reconstructed) > 0 {
			result.ReconstructedBlocks += len(sr.reconstructed)
			result.ReconstructedStripes++
		}
		result.MissingBlocks = append(result.MissingBlocks, sr.missing...)
		if len(sr.reconstructed) > 0 || len(sr.usedStandby) > 0 || st.Status != model.StatusHealthy {
			result.Degraded = true
			degradedStripes = append(degradedStripes, *st)
		}
		for _, bi := range sr.invalidPrimary {
			details, _ := json.Marshal(map[string]any{"site": "primary", "block_index": bi})
			_ = s.db.RecordBlockMismatch(ctx, st.ID, st.ObjectID, bi, "primary_block_digest_mismatch", string(details))
		}
		for _, bi := range sr.invalidBackup {
			details, _ := json.Marshal(map[string]any{"site": "standby", "block_index": bi})
			_ = s.db.RecordBlockMismatch(ctx, st.ID, st.ObjectID, bi, "standby_block_digest_mismatch", string(details))
		}
	}
	if int64(len(result.Bytes)) != end-start+1 {
		return ReadResult{}, false, NewAppError("RANGE_LENGTH_MISMATCH", "internal range assembly mismatch")
	}
	epochAfter, err := s.redis.LayoutEpoch(ctx)
	if err != nil {
		return ReadResult{}, false, err
	}
	if epochAfter != startEpoch {
		return ReadResult{}, true, NewAppError("LAYOUT_CHANGED", "block directory state changed during read; retry")
	}
	now := time.Now().UTC()
	if err := s.redis.TouchStripeReads(ctx, obj.Stripes, now); err != nil {
		s.log.Printf("touch read metadata %s: %v", obj.ID, err)
	}
	for i := range degradedStripes {
		details, _ := json.Marshal(map[string]any{
			"start": start, "end": end, "status": degradedStripes[i].Status,
			"missing_blocks": degradedStripes[i].MissingBlocks,
			"reason":         degradedStripes[i].DegradedReason,
		})
		if err := s.db.RecordDegradedRead(ctx, degradedStripes[i].ID, objectID, degradedStripes[i].DegradedReason, string(details)); err != nil {
			s.log.Printf("audit degraded read %s: %v", degradedStripes[i].ID, err)
		}
	}
	result.BytesReturned = len(result.Bytes)
	return result, false, nil
}

func (s *Service) markInvalidBlocks(ctx context.Context, snapshot *model.StripeMeta, dirs model.DirSet, invalidPrimary, invalidBackup []int) {
	if len(invalidPrimary) == 0 && len(invalidBackup) == 0 {
		return
	}
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	latest, err := s.redis.GetStripe(ctx, snapshot.ID)
	if err != nil {
		return
	}
	changed := false
	for _, idx := range invalidPrimary {
		if b := latest.Block(idx); b != nil && b.PrimaryExists {
			b.PrimaryExists = false
			changed = true
		}
	}
	for _, idx := range invalidBackup {
		if b := latest.Block(idx); b != nil && b.BackupState == "committed" {
			b.BackupState = "invalid"
			changed = true
		}
	}
	if !changed {
		return
	}
	latest.Refresh(dirs)
	if err := s.redis.PersistStripe(ctx, &latest); err != nil {
		s.log.Printf("persist invalid block metadata stripe=%s: %v", snapshot.ID, err)
		return
	}
	s.WakeRepair()
}

func (s *Service) readStripe(ctx context.Context, st *model.StripeMeta, dirs model.DirSet) (stripeRead, error) {
	var out stripeRead
	shards := make([][]byte, model.BlockCount)
	present := make([]bool, model.BlockCount)
	for i := 0; i < model.BlockCount; i++ {
		srcs := model.SourcesForBlock(st, dirs, i)
		if len(srcs) == 0 {
			out.missing = append(out.missing, i)
			continue
		}
		for _, src := range srcs {
			data, err := s.readRel(src.Path)
			if err != nil {
				if src.Site == "primary" {
					out.invalidPrimary = append(out.invalidPrimary, i)
				} else {
					out.invalidBackup = append(out.invalidBackup, i)
				}
				continue
			}
			if int64(len(data)) != src.Length || codec.SHA256(data) != src.SHA256 {
				if src.Site == "primary" {
					out.invalidPrimary = append(out.invalidPrimary, i)
				} else {
					out.invalidBackup = append(out.invalidBackup, i)
				}
				continue
			}
			shards[i] = data
			present[i] = true
			if src.Site != "primary" {
				out.usedStandby = append(out.usedStandby, i)
			}
			break
		}
		if !present[i] {
			out.missing = append(out.missing, i)
		}
	}
	available := 0
	for _, ok := range present {
		if ok {
			available++
		}
	}
	out.availableBeforeRead = available
	if available < model.DataBlocks {
		return out, &AppError{
			Code: "STRIPE_UNRECOVERABLE", Message: "fewer than four blocks are available",
			StripeID: st.ID, ObjectID: st.ObjectID, MissingBlocks: out.missing, AvailableBlocks: available,
		}
	}
	if available < model.BlockCount {
		if err := s.codec.Reconstruct(shards, present); err != nil {
			return out, &AppError{
				Code: "STRIPE_RECONSTRUCT_FAILED", Message: err.Error(),
				StripeID: st.ID, ObjectID: st.ObjectID, MissingBlocks: out.missing, AvailableBlocks: available,
			}
		}
		out.reconstructed = append([]int(nil), out.missing...)
		ok, err := s.codec.Verify(shards)
		if err != nil || !ok {
			msg := "reconstructed shards failed parity verification"
			if err != nil {
				msg = err.Error()
			}
			return out, &AppError{
				Code: "STRIPE_INTEGRITY_FAILED", Message: msg,
				StripeID: st.ID, ObjectID: st.ObjectID, MissingBlocks: out.missing, AvailableBlocks: available,
			}
		}
	}
	logical, err := s.codec.LogicalData(shards, st.Length)
	if err != nil {
		return out, &AppError{
			Code: "STRIPE_RECONSTRUCT_FAILED", Message: err.Error(),
			StripeID: st.ID, ObjectID: st.ObjectID, MissingBlocks: out.missing, AvailableBlocks: available,
		}
	}
	if codec.SHA256(logical) != st.DataSHA256 {
		return out, &AppError{
			Code: "STRIPE_INTEGRITY_FAILED", Message: "logical stripe digest does not match metadata",
			StripeID: st.ID, ObjectID: st.ObjectID, MissingBlocks: out.missing, AvailableBlocks: available,
		}
	}
	out.data = logical
	return out, nil
}
