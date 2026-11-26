package strokelab

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const checkpointHeader = "STROKECKPT1\n"

type CheckpointIdentity struct {
	ProjectHash       string `json:"project_hash"`
	OrderVersion      string `json:"order_version"`
	ConversionVersion string `json:"conversion_version"`
	AlgorithmVersion  string `json:"algorithm_version"`
	ResidualDigest    string `json:"residual_digest"`
}

func (id CheckpointIdentity) equal(other CheckpointIdentity) bool {
	return id == other
}

func (id CheckpointIdentity) String() string {
	return fmt.Sprintf("project=%s order=%s conversion=%s algorithm=%s residual=%s",
		id.ProjectHash, id.OrderVersion, id.ConversionVersion, id.AlgorithmVersion, id.ResidualDigest)
}

type BlockComponentRecord struct {
	BlockID         int    `json:"block_id"`
	Rect            Rect   `json:"rect"`
	CacheKey        string `json:"cache_key"`
	Component       []byte `json:"component"`
	ComponentDigest string `json:"component_digest"`
	ContentHash     string `json:"content_hash"`
	ResidualDigest  string `json:"residual_digest"`
}

type StrokeCommit struct {
	Format            string                 `json:"format"`
	TxID              uint64                 `json:"tx_id"`
	RunID             string                 `json:"run_id"`
	ProjectHash       string                 `json:"project_hash"`
	OrderVersion      string                 `json:"order_version"`
	ConversionVersion string                 `json:"conversion_version"`
	AlgorithmVersion  string                 `json:"algorithm_version"`
	ResidualDigest    string                 `json:"residual_digest"`
	StrokeID          string                 `json:"stroke_id"`
	StrokeOrdinal     int                    `json:"stroke_ordinal"`
	Stage             int                    `json:"stage"`
	Seq               int                    `json:"seq"`
	BlockOrderVersion string                 `json:"block_order_version"`
	Components        []BlockComponentRecord `json:"components"`
	CommittedAt       string                 `json:"committed_at"`
}

func (r StrokeCommit) identity() CheckpointIdentity {
	return CheckpointIdentity{
		ProjectHash: r.ProjectHash, OrderVersion: r.OrderVersion,
		ConversionVersion: r.ConversionVersion, AlgorithmVersion: r.AlgorithmVersion,
		ResidualDigest: r.ResidualDigest,
	}
}

type CheckpointStatus struct {
	Path             string `json:"path"`
	Format           string `json:"format"`
	Records          int    `json:"records"`
	Bytes            int64  `json:"bytes"`
	LastTx           uint64 `json:"last_tx"`
	TailIgnored      bool   `json:"tail_ignored"`
	Identity         string `json:"identity,omitempty"`
	CommittedStrokes int    `json:"committed_strokes"`
	Complete         bool   `json:"complete"`
}

type Checkpoint struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	records []StrokeCommit
	status  CheckpointStatus
}

// OpenCheckpoint opens a standard-library append-only journal. Each record is
// one line with a CRC32 trailer and is fsynced before Commit returns. The
// journal is the local replacement for a SQLite transaction file: the project
// intentionally has no external driver or service dependency.
func OpenCheckpoint(path string) (*Checkpoint, error) {
	if path == "" {
		return nil, coded(CodeCheckpoint, "checkpoint path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, withDetails(CodeCheckpoint, "create checkpoint directory: %v", nil, err)
	}
	existed := true
	info, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, withDetails(CodeCheckpoint, "stat checkpoint: %v", nil, err)
		}
		existed = false
	}
	if existed && info.Size() > 0 {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, withDetails(CodeCheckpoint, "read checkpoint: %v", nil, err)
		}
		records, validLen, tail, err := parseJournal(data)
		if err != nil {
			return nil, err
		}
		if tail && validLen < len(data) {
			// Never append after an uncommitted tail. Truncate back to the last
			// CRC-valid record boundary so the next transaction cannot fuse
			// with crash debris.
			if err := os.Truncate(path, int64(validLen)); err != nil {
				return nil, withDetails(CodeCheckpoint, "truncate checkpoint tail: %v", nil, err)
			}
		}
		f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o644)
		if err != nil {
			return nil, withDetails(CodeCheckpoint, "open checkpoint: %v", nil, err)
		}
		cp := &Checkpoint{path: path, file: f, records: records}
		cp.status = statusFor(path, records, int64(validLen), tail)
		return cp, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, withDetails(CodeCheckpoint, "create checkpoint: %v", nil, err)
	}
	if _, err := f.WriteString(checkpointHeader); err != nil {
		_ = f.Close()
		return nil, withDetails(CodeCheckpoint, "write checkpoint header: %v", nil, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, withDetails(CodeCheckpoint, "sync checkpoint header: %v", nil, err)
	}
	info, _ = f.Stat()
	size := int64(0)
	if info != nil {
		size = info.Size()
	}
	cp := &Checkpoint{path: path, file: f}
	cp.status = statusFor(path, nil, size, false)
	return cp, nil
}

func parseJournal(data []byte) ([]StrokeCommit, int, bool, error) {
	if !bytes.HasPrefix(data, []byte(checkpointHeader)) {
		return nil, 0, false, coded(CodeCheckpoint, "checkpoint header is missing or invalid")
	}
	body := data[len(checkpointHeader):]
	var records []StrokeCommit
	var expectedTx uint64 = 1
	validLen := len(checkpointHeader)
	for len(body) > 0 {
		idx := bytes.IndexByte(body, '\n')
		if idx < 0 {
			// A crash during append leaves a partial record. It was never
			// acknowledged as committed and is ignored on recovery.
			return records, validLen, true, nil
		}
		line := body[:idx]
		body = body[idx+1:]
		if len(line) == 0 {
			validLen += 1
			continue
		}
		rec, err := decodeJournalLine(line)
		if err != nil {
			if len(body) == 0 {
				return records, validLen, true, nil
			}
			return nil, validLen, false, &CodedError{Code: CodeCheckpoint,
				Message: "corrupt checkpoint record before tail", Err: err}
		}
		if rec.Format != CheckpointFormat {
			return nil, validLen, false, coded(CodeCheckpoint, "unknown checkpoint record format %q", rec.Format)
		}
		if rec.TxID != expectedTx {
			return nil, validLen, false, coded(CodeCheckpoint,
				"non-contiguous transaction sequence: got %d want %d", rec.TxID, expectedTx)
		}
		expectedTx++
		records = append(records, rec)
		validLen += len(line) + 1
	}
	return records, validLen, false, nil
}

func decodeJournalLine(line []byte) (StrokeCommit, error) {
	var rec StrokeCommit
	fields := bytes.Fields(line)
	if len(fields) != 2 {
		return rec, fmt.Errorf("record must be base64 payload + crc32")
	}
	payload, err := base64.RawStdEncoding.DecodeString(string(fields[0]))
	if err != nil {
		return rec, err
	}
	want, err := hex.DecodeString(string(fields[1]))
	if err != nil || len(want) != 4 {
		return rec, fmt.Errorf("invalid crc32 trailer")
	}
	got := crc32.ChecksumIEEE(payload)
	if got != uint32(want[0])<<24|uint32(want[1])<<16|uint32(want[2])<<8|uint32(want[3]) {
		return rec, fmt.Errorf("crc32 mismatch")
	}
	if err := json.Unmarshal(payload, &rec); err != nil {
		return rec, err
	}
	return rec, nil
}

func statusFor(path string, records []StrokeCommit, size int64, tailIgnored bool) CheckpointStatus {
	st := CheckpointStatus{
		Path: path, Format: CheckpointFormat, Records: len(records),
		Bytes: size, TailIgnored: tailIgnored, CommittedStrokes: len(records),
	}
	if len(records) > 0 {
		st.LastTx = records[len(records)-1].TxID
		st.Identity = records[0].identity().String()
	}
	return st
}

func (c *Checkpoint) Status() CheckpointStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *Checkpoint) Records() []StrokeCommit {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]StrokeCommit(nil), c.records...)
}

// AppendStroke commits one stroke record across all affected blocks in one
// syscall, then fsyncs. Concurrent commits are serialized and transaction IDs
// are strictly increasing.
func (c *Checkpoint) AppendStroke(rec StrokeCommit) (StrokeCommit, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec.Format = CheckpointFormat
	rec.TxID = c.status.LastTx + 1
	rec.CommittedAt = nowRFC3339()
	if err := validateCommit(rec); err != nil {
		return rec, err
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return rec, withDetails(CodeCheckpoint, "encode checkpoint record: %v", nil, err)
	}
	crc := crc32.ChecksumIEEE(payload)
	line := base64.RawStdEncoding.EncodeToString(payload) + " " + fmt.Sprintf("%08x", crc) + "\n"
	if _, err := c.file.WriteString(line); err != nil {
		return rec, withDetails(CodeCheckpoint, "append checkpoint record: %v", nil, err)
	}
	if err := c.file.Sync(); err != nil {
		return rec, withDetails(CodeCheckpoint, "fsync checkpoint record: %v", nil, err)
	}
	c.records = append(c.records, rec)
	info, _ := c.file.Stat()
	size := c.status.Bytes
	if info != nil {
		size = info.Size()
	}
	c.status = statusFor(c.path, c.records, size, c.status.TailIgnored)
	return rec, nil
}

func validateCommit(rec StrokeCommit) error {
	if rec.RunID == "" || rec.ProjectHash == "" || rec.StrokeID == "" {
		return coded(CodeCheckpoint, "checkpoint record identity fields are required")
	}
	if rec.ConversionVersion == "" || rec.AlgorithmVersion == "" {
		return coded(CodeCheckpoint, "checkpoint version fields are required")
	}
	if rec.StrokeOrdinal < 0 {
		return coded(CodeCheckpoint, "stroke ordinal must be non-negative")
	}
	for i := range rec.Components {
		comp := &rec.Components[i]
		if comp.CacheKey == "" || comp.ContentHash == "" || comp.ComponentDigest == "" {
			return coded(CodeCheckpoint, "block %d component identity fields are required", comp.BlockID)
		}
		if comp.Rect.PixelCount() != len(comp.Component) {
			return coded(CodeCheckpoint, "block %d component length mismatch", comp.BlockID)
		}
	}
	return nil
}

// ResumePrefix returns committed records only when every identity field
// matches. Any mismatch is E_CKPT; stale records are never applied silently.
func (c *Checkpoint) ResumePrefix(identity CheckpointIdentity) ([]StrokeCommit, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	records := append([]StrokeCommit(nil), c.records...)
	for i, rec := range records {
		if !rec.identity().equal(identity) {
			return nil, withDetails(CodeCheckpoint,
				"checkpoint record %d identity does not match this project/version",
				map[string]any{"tx_id": rec.TxID, "got": rec.identity().String(),
					"want": identity.String()},
				"stale checkpoint at tx %d", rec.TxID)
		}
		if rec.StrokeOrdinal != i {
			return nil, coded(CodeCheckpoint,
				"checkpoint stroke ordinal %d at tx %d, want %d", rec.StrokeOrdinal, rec.TxID, i)
		}
	}
	return records, nil
}

func (c *Checkpoint) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		return nil
	}
	err := c.file.Close()
	c.file = nil
	return err
}

func nowRFC3339() string {
	// Kept in a tiny helper so journal tests can focus on stable fields. Time
	// is metadata only and never participates in any composition decision.
	return timeNowUTC().Format("2006-01-02T15:04:05.000000000Z")
}

func digestBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func componentDigest(component []byte) string {
	return digestBytes(component)
}

func contentHash(mask []byte) string {
	return digestBytes(mask)
}

func BlockOrderVersionForCommit(records []BlockComponentRecord) string {
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, fmt.Sprintf("%d:%s", r.BlockID, r.CacheKey))
	}
	sort.Strings(ids)
	return digestBytes([]byte(strings.Join(ids, "|")))
}

var ErrCheckpointClosed = errors.New("checkpoint closed")
