// Package store implements the local, append-only block checkpoint store.
// Writes are serialized through a single mutex and each record is a
// self-describing JSON line with a CRC32: a torn tail after a forced kill is
// truncated at load time and only fully durable blocks are recovered.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"

	"vqa/internal/fixed"
	"vqa/internal/metrics"
	"vqa/internal/propagation"
	"vqa/internal/sample"
)

const (
	// MetaName and LogName are the two files of a checkpoint store.
	MetaName = "store_meta.json"
	LogName  = "blocks.log"
)

// Meta pins the inputs and code versions a store was built against.
type Meta struct {
	StoreVersion     string         `json:"store_version"`
	AlgorithmVersion string         `json:"algorithm_version"`
	ParamVersion     int            `json:"param_version"`
	Quality          int            `json:"quality"`
	ManifestHash     string         `json:"manifest_hash"`
	Params           metrics.Params `json:"params"`
	FrameCount       int            `json:"frame_count"`
	BlocksPerFrame   int            `json:"blocks_per_frame"`
}

// BlockRecord is the frozen, fully reproducible result of one block.
type BlockRecord struct {
	Kind         string                       `json:"kind"` // always "block"
	Frame        int                          `json:"frame"`
	BX           int                          `json:"bx"`
	BY           int                          `json:"by"`
	Sequence     int                          `json:"sequence"`
	State        string                       `json:"state"`
	InputHash    string                       `json:"input_hash"`
	ParamHash    string                       `json:"param_hash"`
	Params       metrics.Params               `json:"params"`
	Stats        metrics.BlockStats           `json:"stats"`
	Quantiles    metrics.Quantiles            `json:"quantiles"`
	Windows      []metrics.WindowResult       `json:"windows"`
	Propagation  propagation.BlockPropagation `json:"propagation"`
	EdgeFidelity metrics.EdgeFidelity         `json:"edge_fidelity"`
	Truncations  []fixed.Truncation           `json:"truncations"`
	ReconDigest  string                       `json:"recon_digest"`
	ReconPixels  []uint8                      `json:"recon_pixels"`
	PSNR         float64                      `json:"psnr_db"`
}

// FrameRecord is appended once all blocks of a frame are committed.
type FrameRecord struct {
	Kind        string  `json:"kind"` // always "frame"
	Frame       int     `json:"frame"`
	PSNR        float64 `json:"psnr_db"`
	SSIMQ16     int64   `json:"ssim_q16"`
	PSNRLower   float64 `json:"psnr_lower_bound_db"`
	WindowCount int     `json:"window_count"`
	Sequence    int     `json:"sequence"`
}

// Store is an opened checkpoint directory.
type Store struct {
	dir     string
	mu      sync.Mutex
	file    *os.File
	meta    Meta
	records []BlockRecord
	frames  []FrameRecord
	nextSeq int
}

// HashBytes returns the hex SHA256 of b.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HashFrame returns the input content hash of one frame's raw pixels.
func HashFrame(f *sample.Frame) string {
	return HashBytes(f.Pix)
}

// HashParams deterministically hashes a parameter snapshot.
func HashParams(p metrics.Params) string {
	b, _ := json.Marshal(p)
	return HashBytes(b)
}

// Create writes store metadata and opens a fresh, empty log (full recompute).
func Create(dir string, meta Meta) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	meta.StoreVersion = sample.SchemaVersion
	meta.AlgorithmVersion = sample.AlgorithmVersion
	meta.ParamVersion = metrics.ParamVersion
	if err := writeMetaAtomic(dir, meta); err != nil {
		return nil, err
	}
	logPath := filepath.Join(dir, LogName)
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Store{dir: dir, file: f, meta: meta}, nil
}

// Open loads metadata and replays durable records. A torn final record is
// discarded; a hash/version mismatch invalidates the whole snapshot.
func Open(dir string, manifestHash string, quality int, params metrics.Params) (*Store, error) {
	meta, err := readMeta(dir)
	if err != nil {
		return nil, err
	}
	if meta.AlgorithmVersion != sample.AlgorithmVersion ||
		meta.StoreVersion != sample.SchemaVersion ||
		meta.ParamVersion != metrics.ParamVersion {
		return nil, invalidate(dir, "algorithm or schema version mismatch")
	}
	if meta.ManifestHash != manifestHash || meta.Quality != quality {
		return nil, invalidate(dir, "input manifest or quality mismatch")
	}
	if HashParams(meta.Params) != HashParams(params) {
		return nil, invalidate(dir, "parameter snapshot mismatch")
	}
	blocks, frames, err := readLog(filepath.Join(dir, LogName))
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, LogName), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	st := &Store{
		dir: dir, file: f, meta: meta,
		records: blocks, frames: frames,
		nextSeq: len(blocks) + len(frames),
	}
	return st, nil
}

// AppendBlock serializes and durably appends one block record under the store
// lock, guaranteeing no interleaving and a stable global commit sequence.
func (s *Store) AppendBlock(rec *BlockRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec.Kind = "block"
	rec.Sequence = s.nextSeq
	s.nextSeq++
	if err := appendRecord(s.file, rec); err != nil {
		return err
	}
	s.records = append(s.records, *rec)
	return nil
}

// AppendFrame durably appends one frame summary after its blocks are committed.
func (s *Store) AppendFrame(rec *FrameRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec.Kind = "frame"
	rec.Sequence = s.nextSeq
	s.nextSeq++
	if err := appendRecord(s.file, rec); err != nil {
		return err
	}
	s.frames = append(s.frames, *rec)
	return nil
}

// Close releases the log handle.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		return s.file.Close()
	}
	return nil
}

// Blocks returns the durable block records in commit order.
func (s *Store) Blocks() []BlockRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]BlockRecord, len(s.records))
	copy(out, s.records)
	return out
}

// Frames returns durable frame records.
func (s *Store) Frames() []FrameRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]FrameRecord, len(s.frames))
	copy(out, s.frames)
	return out
}

// Meta returns the store metadata.
func (s *Store) Meta() Meta { return s.meta }

// Progress reports committed blocks over total blocks across the sequence.
func (s *Store) Progress() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := s.meta.FrameCount * s.meta.BlocksPerFrame
	return len(s.records), total
}

// HasBlock reports whether a block is already committed.
func (s *Store) HasBlock(frame, bx, by int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if r.Frame == frame && r.BX == bx && r.BY == by {
			return true
		}
	}
	return false
}

// lineEnvelope wraps one JSON record with its CRC32.
type lineEnvelope struct {
	CRC uint32          `json:"crc"`
	Raw json.RawMessage `json:"raw"`
}

func appendRecord(w io.Writer, rec any) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	env := lineEnvelope{CRC: crcIEEE(payload), Raw: payload}
	line, err := json.Marshal(env)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	// Single Write call per record plus Sync gives an atomic durable append.
	if _, err := w.Write(line); err != nil {
		return err
	}
	if syncer, ok := w.(*os.File); ok {
		return syncer.Sync()
	}
	return nil
}

func readLog(path string) ([]BlockRecord, []FrameRecord, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var blocks []BlockRecord
	var frames []FrameRecord
	lines := splitLines(payload)
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var env lineEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			// Torn tail after a forced kill: drop it and stop replay.
			return truncateLog(path, len(blocks), len(frames), blocks, frames)
		}
		if crcIEEE(env.Raw) != env.CRC {
			return truncateLog(path, len(blocks), len(frames), blocks, frames)
		}
		var kind struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(env.Raw, &kind); err != nil {
			return truncateLog(path, len(blocks), len(frames), blocks, frames)
		}
		switch kind.Kind {
		case "block":
			var br BlockRecord
			if err := json.Unmarshal(env.Raw, &br); err != nil {
				return truncateLog(path, len(blocks), len(frames), blocks, frames)
			}
			blocks = append(blocks, br)
		case "frame":
			var fr FrameRecord
			if err := json.Unmarshal(env.Raw, &fr); err != nil {
				return truncateLog(path, len(blocks), len(frames), blocks, frames)
			}
			frames = append(frames, fr)
		}
	}
	return blocks, frames, nil
}

func truncateLog(path string, nb, nf int, blocks []BlockRecord, frames []FrameRecord) ([]BlockRecord, []FrameRecord, error) {
	// Rewrite the log without the torn tail so subsequent appends stay clean.
	_ = nb
	_ = nf
	if err := rewriteLog(path, blocks, frames); err != nil {
		return nil, nil, err
	}
	return blocks, frames, nil
}

func rewriteLog(path string, blocks []BlockRecord, frames []FrameRecord) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	// Re-emit in original sequence order; block and frame records were appended
	// in that order and are merged back by replaying their stored sequence.
	type seq struct {
		n int
		v any
	}
	var all []seq
	for i := range blocks {
		all = append(all, seq{blocks[i].Sequence, &blocks[i]})
	}
	for i := range frames {
		all = append(all, seq{frames[i].Sequence, &frames[i]})
	}
	// Simple insertion sort (counts are small).
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j-1].n > all[j].n; j-- {
			all[j-1], all[j] = all[j], all[j-1]
		}
	}
	for _, item := range all {
		if err := appendRecord(f, item.v); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	return out
}

func writeMetaAtomic(dir string, meta Meta) error {
	payload, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, MetaName+".tmp")
	final := filepath.Join(dir, MetaName)
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

func readMeta(dir string) (Meta, error) {
	payload, err := os.ReadFile(filepath.Join(dir, MetaName))
	if err != nil {
		return Meta{}, err
	}
	var meta Meta
	if err := json.Unmarshal(payload, &meta); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

// invalidate removes every checkpoint file so the caller must fully recompute;
// partial reuse of old block results is never allowed.
func invalidate(dir, reason string) error {
	_ = os.Remove(filepath.Join(dir, LogName))
	_ = os.Remove(filepath.Join(dir, MetaName))
	return fmt.Errorf("%w: %s", ErrInvalidSnapshot, reason)
}

// ErrInvalidSnapshot marks a checkpoint that must be fully recomputed.
var ErrInvalidSnapshot = errors.New("checkpoint invalidated")

func crcIEEE(b []byte) uint32 {
	return crc32.ChecksumIEEE(b)
}
