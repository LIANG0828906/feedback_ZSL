package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"iconseg/internal/checkpoint"
	"iconseg/internal/core"
	"iconseg/internal/layers"
	"iconseg/internal/pipeline"
	"iconseg/internal/seg"
	"iconseg/internal/workpool"
)

type Options struct {
	PauseAfterCommits int
	WorkerOverride    int
	ForceReset        bool
}

type PreviewReport struct {
	Downsample        int     `json:"downsample"`
	FloodSamples      int     `json:"flood_samples"`
	EquivalentSamples int     `json:"equivalent_samples"`
	ElapsedMS         float64 `json:"elapsed_ms"`
	WithinLatency     bool    `json:"within_latency"`
	Rationale         string  `json:"rationale"`
	RegionCount       int     `json:"region_count"`
	MaskHash          string  `json:"mask_hash"`
}

type ExactReport struct {
	RegionCount      int                 `json:"region_count"`
	Components       []seg.Component     `json:"components"`
	Match            seg.MatchReport     `json:"match"`
	FloodSamples     int                 `json:"flood_samples"`
	MergeCount       int                 `json:"merge_count"`
	ElapsedMS        float64             `json:"elapsed_ms"`
	MaskHash         string              `json:"mask_hash"`
	StatsHash        string              `json:"stats_hash"`
	TruthFingerprint string              `json:"truth_fingerprint"`
	RegionTable      []core.RegionRecord `json:"region_table"`
}

type CommitEntry struct {
	Seq         int64  `json:"seq"`
	Block       int    `json:"block"`
	Layer       string `json:"layer"`
	ContentHash string `json:"content_hash"`
}

type Record struct {
	ImageID         string                     `json:"image_id"`
	Snapshot        core.Snapshot              `json:"snapshot"`
	SnapshotID      string                     `json:"snapshot_id"`
	SnapshotStatus  string                     `json:"snapshot_status"`
	Preview         PreviewReport              `json:"preview"`
	Exact           ExactReport                `json:"exact"`
	Consistency     pipeline.ConsistencyReport `json:"consistency"`
	Workers         int                        `json:"workers"`
	PerThreadPixels int                        `json:"per_thread_pixels"`
	Blocks          int                        `json:"blocks"`
	NewCommits      int                        `json:"new_commits"`
	SkippedCommits  int                        `json:"skipped_commits"`
	Paused          bool                       `json:"paused"`
	Invalidated     bool                       `json:"invalidated_snapshot"`
	Committed       int                        `json:"committed_layers"`
	TotalLayers     int                        `json:"total_layers"`
	CommitSequence  []string                   `json:"commit_sequence"`
	CommitHashes    []CommitEntry              `json:"commit_hashes"`
	PreviewMask     []byte                     `json:"-"`
	ExactMask       []byte                     `json:"-"`
	TruthMask       []byte                     `json:"-"`
	Width           int                        `json:"-"`
	Height          int                        `json:"-"`
}

type layerArtifact struct {
	Layer   layers.Layer
	Summary string
	Content []byte
}

func Run(ctx context.Context, store *checkpoint.Store, icon core.Icon, snap core.Snapshot, opts Options) (*Record, error) {
	if err := snap.ValidateFor(icon.Manifest.ImageID, icon.Manifest.Seed); err != nil {
		return nil, err
	}
	preview, err := pipeline.Preview(icon, snap)
	if err != nil {
		return nil, err
	}
	exact, err := pipeline.Exact(icon, snap)
	if err != nil {
		return nil, err
	}
	consistency, err := pipeline.CheckConsistency(preview, exact, icon.Manifest)
	if err != nil {
		return nil, err
	}
	blocks := layers.Split(icon.Manifest.Width, icon.Manifest.Height, icon.Manifest.BlockSize)
	if opts.ForceReset {
		id := checkpoint.SnapshotID(snap, icon.Manifest.ReferenceFingerprint)
		_ = store.InvalidateSnapshot(id, "forced reset")
	}
	invalidated := false
	record, err := store.EnsureSnapshot(snap, icon.Manifest.ReferenceFingerprint, blocks)
	if err != nil {
		app := core.AsAppError(err)
		if app.Code != core.CodeVersion {
			return nil, err
		}
		if err := store.ResetSnapshot(checkpoint.SnapshotID(snap, icon.Manifest.ReferenceFingerprint)); err != nil {
			return nil, err
		}
		invalidated = true
		record, err = store.EnsureSnapshot(snap, icon.Manifest.ReferenceFingerprint, blocks)
		if err != nil {
			return nil, err
		}
	}
	if err := store.ValidateSnapshot(record.ID); err != nil {
		app := core.AsAppError(err)
		if app.Code != core.CodeVersion {
			return nil, err
		}
		invalidated = true
		if err := store.ResetSnapshot(record.ID); err != nil {
			return nil, err
		}
		record, err = store.EnsureSnapshot(snap, icon.Manifest.ReferenceFingerprint, blocks)
		if err != nil {
			return nil, err
		}
	}

	workers := icon.Manifest.WorkerBudget
	if opts.WorkerOverride > 0 {
		workers = min(workers, opts.WorkerOverride)
	}
	pool, err := workpool.New(workers, icon.Manifest.PerThreadPixelBuffer)
	if err != nil {
		return nil, err
	}
	tasks := make([]workpool.Task, 0, len(blocks)*len(layers.Order))
	taskKeys := make([]string, 0, len(tasks))
	for _, block := range blocks {
		block := block
		for _, layer := range layers.Order {
			layer := layer
			taskKeys = append(taskKeys, strconv.Itoa(block.Index)+":"+string(layer))
			tasks = append(tasks, workpool.Task{
				Pixels: block.W * block.H,
				Run: func(_ []byte) (any, error) {
					var content []byte
					switch layer {
					case layers.Gradient:
						content = layers.EncodeGradient(layers.CropFloat(exact.Gradient, icon.Manifest.Width, block))
					case layers.Marker:
						content = layers.EncodeInt32(layers.CropInt32(exact.InitialMarkers, icon.Manifest.Width, block))
					case layers.Flood:
						content = layers.EncodeInt32(layers.CropInt32(exact.Labels, icon.Manifest.Width, block))
					case layers.Stats:
						content = layers.CropMask(exact.Mask, icon.Manifest.Width, block)
					default:
						return nil, core.ParaError("layer", "{gradient,marker,flood_merge,stats}", "unknown layer")
					}
					return layerArtifact{Layer: layer, Summary: layers.Summary(layer, block), Content: content}, nil
				},
			})
		}
	}
	results, err := pool.Run(ctx, tasks)
	if err != nil {
		return nil, err
	}
	artifacts := make([]layerArtifact, len(results))
	for i, result := range results {
		if result.Err != nil {
			return nil, result.Err
		}
		artifact, ok := result.Value.(layerArtifact)
		if !ok {
			return nil, fmt.Errorf("worker %s returned an invalid artifact", taskKeys[i])
		}
		artifacts[i] = artifact
	}

	existingRows, err := store.LoadRows(record.ID)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]checkpoint.Row, len(existingRows))
	for _, row := range existingRows {
		existing[strconv.Itoa(row.BlockIndex)+":"+row.Layer] = row
	}
	for i, artifact := range artifacts {
		row, ok := existing[taskKeys[i]]
		if !ok || row.State != checkpoint.Committed {
			continue
		}
		if row.ContentHash == core.BytesHash(artifact.Content) &&
			row.ParamVersion == snap.ParamVersion && row.AlgorithmVersion == snap.AlgorithmVersion {
			continue
		}
		invalidated = true
		if err := store.InvalidateSnapshot(record.ID, "recomputed layer disagrees with checkpoint"); err != nil {
			return nil, err
		}
		if err := store.ResetSnapshot(record.ID); err != nil {
			return nil, err
		}
		if _, err := store.EnsureSnapshot(snap, icon.Manifest.ReferenceFingerprint, blocks); err != nil {
			return nil, err
		}
		existing = map[string]checkpoint.Row{}
		break
	}
	if err := store.SetSnapshotResult(record.ID, exact.RegionTable, exact.MaskHash, exact.StatsHash); err != nil {
		return nil, err
	}
	newCommits := 0
	skipped := 0
	paused := false
	for i, artifact := range artifacts {
		key := taskKeys[i]
		blockIndex := blockIndexFromKey(key)
		row, ok := existing[key]
		if ok && row.State == checkpoint.Committed {
			skipped++
			continue
		}
		if err := store.Transition(record.ID, blockIndex, string(artifact.Layer), checkpoint.Processing); err != nil {
			return nil, err
		}
		if _, err := store.CommitLayer(record.ID, blockIndex, string(artifact.Layer),
			artifact.Summary, artifact.Content); err != nil {
			return nil, err
		}
		newCommits++
		if opts.PauseAfterCommits > 0 && newCommits >= opts.PauseAfterCommits {
			paused = true
			break
		}
	}
	if !paused {
		if err := store.FinalizeSnapshot(record.ID); err != nil {
			return nil, err
		}
	}
	committed, total, err := store.SnapshotProgress(record.ID)
	if err != nil {
		return nil, err
	}
	history, err := store.CommitHistory(record.ID)
	if err != nil {
		return nil, err
	}
	commitSequence := make([]string, 0, len(history))
	commitHashes := make([]CommitEntry, 0, len(history))
	for _, row := range history {
		commitSequence = append(commitSequence, strconv.Itoa(row.BlockIndex)+":"+row.Layer)
		commitHashes = append(commitHashes, CommitEntry{
			Seq: row.Seq, Block: row.BlockIndex, Layer: row.Layer, ContentHash: row.ContentHash,
		})
	}
	snapshot, err := store.GetSnapshot(record.ID)
	if err != nil {
		return nil, err
	}
	return &Record{
		ImageID: icon.Manifest.ImageID, Snapshot: snap, SnapshotID: record.ID, SnapshotStatus: snapshot.Status,
		Preview: PreviewReport{
			Downsample: preview.Downsample, FloodSamples: preview.FloodSamples,
			EquivalentSamples: preview.EquivalentSamples, ElapsedMS: durationMS(preview.Elapsed),
			WithinLatency: preview.WithinLatency, Rationale: preview.Rationale,
			RegionCount: len(preview.Components), MaskHash: preview.MaskHash,
		},
		Exact: ExactReport{
			RegionCount: len(exact.Components), Components: exact.Components, Match: exact.Match,
			FloodSamples: exact.FloodSamples, MergeCount: exact.MergeCount, ElapsedMS: durationMS(exact.Elapsed),
			MaskHash: exact.MaskHash, StatsHash: exact.StatsHash, TruthFingerprint: exact.TruthFingerprint,
			RegionTable: exact.RegionTable,
		},
		Consistency: consistency, Workers: workers, PerThreadPixels: icon.Manifest.PerThreadPixelBuffer,
		Blocks: len(blocks), NewCommits: newCommits, SkippedCommits: skipped, Paused: paused,
		Invalidated: invalidated, Committed: committed, TotalLayers: total,
		CommitSequence: commitSequence, CommitHashes: commitHashes,
		PreviewMask: preview.Mask, ExactMask: exact.Mask, TruthMask: icon.Manifest.ReferenceMask,
		Width: icon.Manifest.Width, Height: icon.Manifest.Height,
	}, nil
}

func blockIndexFromKey(key string) int {
	for i := 0; i < len(key); i++ {
		if key[i] == ':' {
			value, _ := strconv.Atoi(key[:i])
			return value
		}
	}
	return 0
}

func durationMS(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

func MarshalRecord(record *Record) ([]byte, error) {
	return json.MarshalIndent(record, "", "  ")
}

func CompareCommitSequences(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func SortCommitEntries(entries []CommitEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Seq < entries[j].Seq })
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
