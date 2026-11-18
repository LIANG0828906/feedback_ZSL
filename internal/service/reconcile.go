package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"archive/internal/model"
)

type Difference struct {
	StripeID      string `json:"stripe_id"`
	ObjectID      string `json:"object_id"`
	BlockIndex    int    `json:"block_index"`
	PresentSite   string `json:"present_site,omitempty"`
	MissingSite   string `json:"missing_site,omitempty"`
	PrimaryPath   string `json:"primary_path,omitempty"`
	StandbyPath   string `json:"standby_path,omitempty"`
	PrimaryLength int64  `json:"primary_length,omitempty"`
	StandbyLength int64  `json:"standby_length,omitempty"`
	PrimarySHA256 string `json:"primary_sha256,omitempty"`
	StandbySHA256 string `json:"standby_sha256,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

type ReconcileCounts struct {
	Missing        int `json:"missing"`
	LengthMismatch int `json:"length_mismatch"`
	DigestMismatch int `json:"digest_mismatch"`
	Total          int `json:"total"`
}

type ReconcileResult struct {
	ID                  string          `json:"reconcile_id"`
	StartedAt           string          `json:"started_at"`
	FinishedAt          string          `json:"finished_at"`
	State               string          `json:"state"`
	SnapshotEpoch       int64           `json:"snapshot_layout_epoch"`
	ExcludedOfflineDirs []string        `json:"excluded_offline_dirs"`
	StripesCompared     int             `json:"stripes_compared"`
	BlocksCompared      int             `json:"blocks_compared"`
	Counts              ReconcileCounts `json:"counts"`
	Missing             []Difference    `json:"missing"`
	LengthMismatch      []Difference    `json:"length_mismatch"`
	DigestMismatch      []Difference    `json:"digest_mismatch"`
	Error               string          `json:"error,omitempty"`
}

type reconcileBlock struct {
	stripeID      string
	objectID      string
	index         int
	primaryPath   string
	standbyPath   string
	primaryLength int64
	standbyLength int64
	primarySHA    string
	standbySHA    string
}

func NewReconcileID() string {
	return "rec-" + randomID()
}

func (s *Service) Reconcile(ctx context.Context) (ReconcileResult, error) {
	return s.ReconcileWithID(ctx, NewReconcileID())
}

func (s *Service) ReconcileWithID(ctx context.Context, id string) (ReconcileResult, error) {
	started := time.Now().UTC()
	s.mu.RLock()
	dirs, err := s.dirs(ctx)
	if err != nil {
		s.mu.RUnlock()
		return ReconcileResult{}, err
	}
	stripes, err := s.redis.ListAllStripes(ctx)
	if err != nil {
		s.mu.RUnlock()
		return ReconcileResult{}, err
	}
	blocks := make([]reconcileBlock, 0)
	excluded := make([]string, 0)
	for i := 0; i < model.BlockCount; i++ {
		if !dirs.IsOnline(i) {
			excluded = append(excluded, fmt.Sprintf("block-%d", i))
		}
	}
	stripesCompared := 0
	for i := range stripes {
		st := &stripes[i]
		if st.RepairState == "running" {
			continue
		}
		added := false
		for bi := 0; bi < model.BlockCount; bi++ {
			if !dirs.IsOnline(bi) {
				continue
			}
			b := st.Block(bi)
			if b == nil || b.BackupState != "committed" {
				continue
			}
			blocks = append(blocks, reconcileBlock{
				stripeID: st.ID, objectID: st.ObjectID, index: bi,
				primaryPath: b.PrimaryPath, standbyPath: b.BackupPath,
				primaryLength: b.PrimaryLength, standbyLength: b.BackupLength,
				primarySHA: b.PrimarySHA256, standbySHA: b.BackupSHA256,
			})
			added = true
		}
		if added {
			stripesCompared++
		}
	}
	s.mu.RUnlock()
	sort.Slice(blocks, func(i, j int) bool {
		if blocks[i].stripeID != blocks[j].stripeID {
			return blocks[i].stripeID < blocks[j].stripeID
		}
		return blocks[i].index < blocks[j].index
	})
	startDetails, _ := json.Marshal(map[string]any{
		"layout_epoch": dirs.Epoch, "excluded_offline_dirs": excluded,
		"stripes_compared": stripesCompared, "blocks_compared": len(blocks),
	})
	if err := s.db.StartReconciliation(ctx, id, started.Format(time.RFC3339Nano), string(startDetails)); err != nil {
		return ReconcileResult{}, err
	}
	result := ReconcileResult{
		ID:                  id,
		StartedAt:           started.Format(time.RFC3339Nano),
		State:               "running",
		SnapshotEpoch:       dirs.Epoch,
		ExcludedOfflineDirs: excluded,
		StripesCompared:     stripesCompared,
		BlocksCompared:      len(blocks),
		Missing:             []Difference{},
		LengthMismatch:      []Difference{},
		DigestMismatch:      []Difference{},
	}
	for _, b := range blocks {
		d := Difference{
			StripeID: b.stripeID, ObjectID: b.objectID, BlockIndex: b.index,
			PrimaryPath: b.primaryPath, StandbyPath: b.standbyPath,
		}
		pLen, pErr := s.fileLength(b.primaryPath)
		sLen, sErr := s.fileLength(b.standbyPath)
		switch {
		case pErr != nil && sErr != nil:
			d.MissingSite = "primary,standby"
			d.Detail = "both copies are missing or unreadable"
			result.Missing = append(result.Missing, d)
			continue
		case pErr != nil:
			d.PresentSite = "standby"
			d.MissingSite = "primary"
			d.StandbyLength = sLen
			d.Detail = "primary copy is missing"
			result.Missing = append(result.Missing, d)
			continue
		case sErr != nil:
			d.PresentSite = "primary"
			d.MissingSite = "standby"
			d.PrimaryLength = pLen
			d.Detail = "standby copy is missing"
			result.Missing = append(result.Missing, d)
			continue
		}
		d.PrimaryLength = pLen
		d.StandbyLength = sLen
		if pLen != sLen || (b.primaryLength != 0 && pLen != b.primaryLength) || (b.standbyLength != 0 && sLen != b.standbyLength) {
			d.Detail = fmt.Sprintf("primary length=%d standby length=%d", pLen, sLen)
			result.LengthMismatch = append(result.LengthMismatch, d)
			continue
		}
		pHash, err := s.hashRel(b.primaryPath)
		if err != nil {
			d.PresentSite = "standby"
			d.MissingSite = "primary"
			d.Detail = "primary copy could not be hashed"
			result.Missing = append(result.Missing, d)
			continue
		}
		sHash, err := s.hashRel(b.standbyPath)
		if err != nil {
			d.PresentSite = "primary"
			d.MissingSite = "standby"
			d.Detail = "standby copy could not be hashed"
			result.Missing = append(result.Missing, d)
			continue
		}
		d.PrimarySHA256 = pHash
		d.StandbySHA256 = sHash
		if pHash != sHash {
			d.Detail = "primary and standby content digests differ"
			result.DigestMismatch = append(result.DigestMismatch, d)
		}
	}
	result.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	result.State = "completed"
	result.Counts = ReconcileCounts{
		Missing:        len(result.Missing),
		LengthMismatch: len(result.LengthMismatch),
		DigestMismatch: len(result.DigestMismatch),
		Total:          len(result.Missing) + len(result.LengthMismatch) + len(result.DigestMismatch),
	}
	resultJSON, _ := json.Marshal(result)
	finishDetails, _ := json.Marshal(map[string]any{
		"reconcile_id": id, "counts": result.Counts, "stripes_compared": stripesCompared,
		"blocks_compared": len(blocks), "excluded_offline_dirs": excluded,
	})
	if err := s.db.FinishReconciliation(ctx, id, "completed", result.Counts.Missing,
		result.Counts.LengthMismatch, result.Counts.DigestMismatch, string(resultJSON), "", string(finishDetails)); err != nil {
		return result, err
	}
	_ = s.redis.SetLastReconciliation(ctx, resultJSON)
	return result, nil
}

func (s *Service) GetReconciliation(ctx context.Context, id string) (ReconcileResult, error) {
	row, err := s.db.Reconciliation(ctx, id)
	if err != nil {
		return ReconcileResult{}, err
	}
	var result ReconcileResult
	if b, ok := row.Result.(map[string]any); ok {
		raw, _ := json.Marshal(b)
		_ = json.Unmarshal(raw, &result)
	}
	if result.ID == "" {
		result.ID = row.ID
	}
	if result.State == "" {
		result.State = row.State
	}
	if result.Error == "" {
		result.Error = row.Error
	}
	return result, nil
}

func (s *Service) LastReconciliation(ctx context.Context) (ReconcileResult, error) {
	row, err := s.db.LastReconciliation(ctx)
	if err != nil {
		return ReconcileResult{}, err
	}
	var result ReconcileResult
	if b, ok := row.Result.(map[string]any); ok {
		raw, _ := json.Marshal(b)
		_ = json.Unmarshal(raw, &result)
	}
	if result.ID == "" {
		result.ID = row.ID
	}
	if result.State == "" {
		result.State = row.State
	}
	if result.Error == "" {
		result.Error = row.Error
	}
	return result, nil
}

func (s *Service) hashRel(rel string) (string, error) {
	full, err := s.resolveRel(rel)
	if err != nil {
		return "", err
	}
	f, err := os.Open(full)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
