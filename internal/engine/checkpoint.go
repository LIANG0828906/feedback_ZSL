package engine

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"textureworkbench/internal/tex"
)

const checkpointSchema = 1

type checkpointHeader struct {
	Schema            int    `json:"schema"`
	SessionKey        string `json:"session_key"`
	ImageID           string `json:"image_id"`
	ContentHash       string `json:"content_hash"`
	DescriptorVersion string `json:"descriptor_version"`
	ParameterVersion  string `json:"parameter_version"`
	AlgorithmVersion  string `json:"algorithm_version"`
}

func (h checkpointHeader) validate() error {
	if h.Schema != checkpointSchema {
		return fmt.Errorf("unsupported checkpoint schema %d", h.Schema)
	}
	if h.ContentHash == "" || h.DescriptorVersion == "" || h.ParameterVersion == "" || h.AlgorithmVersion == "" {
		return fmt.Errorf("checkpoint header is missing version fields")
	}
	return nil
}

type recordSummary struct {
	PixelCount   int     `json:"pixel_count"`
	ChannelCount int     `json:"channel_count"`
	MeanAbsError float64 `json:"mean_abs_error"`
	MaxAbsError  int64   `json:"max_abs_error"`
	NonZero      int64   `json:"nonzero_errors"`
}

type recordMeta struct {
	Sequence          int           `json:"sequence"`
	BlockIndex        int           `json:"block_index"`
	Row               int           `json:"row"`
	Col               int           `json:"col"`
	Rect              tex.Rect      `json:"rect"`
	ContentHash       string        `json:"content_hash"`
	DescriptorVersion string        `json:"descriptor_version"`
	ParameterVersion  string        `json:"parameter_version"`
	AlgorithmVersion  string        `json:"algorithm_version"`
	OutputHash        string        `json:"output_hash"`
	ErrorHash         string        `json:"error_hash"`
	Summary           recordSummary `json:"summary"`
	File              string        `json:"file"`
}

type blockRecord struct {
	recordMeta
	Output []byte `json:"output"`
	Errors []byte `json:"errors"`
}

type checkpointManifest struct {
	Header  checkpointHeader `json:"header"`
	Records []recordMeta     `json:"records"`
}

type LoadResult struct {
	Invalidated bool
	Reasons     []string
	Records     map[int]blockRecord
	Sequence    int
}

type Store struct {
	mu           sync.Mutex
	root         string
	key          string
	dir          string
	blocksDir    string
	manifestPath string
	header       checkpointHeader
	manifest     checkpointManifest
}

func OpenStore(root, key string, header checkpointHeader) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, tex.ErrPara("checkpoint_root", "required field is missing")
	}
	key = sanitizeKey(key)
	dir := filepath.Join(root, key)
	store := &Store{
		root:         root,
		key:          key,
		dir:          dir,
		blocksDir:    filepath.Join(dir, "blocks"),
		manifestPath: filepath.Join(dir, "checkpoint.json"),
		header:       header,
	}
	if err := os.MkdirAll(store.blocksDir, 0o755); err != nil {
		return nil, fmt.Errorf("create checkpoint directory: %w", err)
	}
	return store, nil
}

func sanitizeKey(key string) string {
	if key == "" {
		return "default"
	}
	var builder strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	result := strings.Trim(builder.String(), "._")
	if result == "" {
		return "default"
	}
	return result
}

func (s *Store) Load() (LoadResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.manifestPath)
	if os.IsNotExist(err) {
		return LoadResult{Records: make(map[int]blockRecord)}, nil
	}
	if err != nil {
		return LoadResult{}, fmt.Errorf("read checkpoint manifest: %w", err)
	}
	var manifest checkpointManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		reasons := []string{"checkpoint_manifest_corrupt"}
		if invalidateErr := s.invalidateLocked(reasons); invalidateErr != nil {
			return LoadResult{}, invalidateErr
		}
		return LoadResult{Invalidated: true, Reasons: reasons, Records: make(map[int]blockRecord)}, nil
	}
	var reasons []string
	if manifest.Header.Schema != s.header.Schema {
		reasons = append(reasons, "schema_version_mismatch")
	}
	if manifest.Header.SessionKey != s.header.SessionKey {
		reasons = append(reasons, "session_key_mismatch")
	}
	if manifest.Header.ImageID != s.header.ImageID {
		reasons = append(reasons, "image_id_mismatch")
	}
	if manifest.Header.ContentHash != s.header.ContentHash {
		reasons = append(reasons, "content_hash_mismatch")
	}
	if manifest.Header.DescriptorVersion != s.header.DescriptorVersion {
		reasons = append(reasons, "target_descriptor_version_mismatch")
	}
	if manifest.Header.ParameterVersion != s.header.ParameterVersion {
		reasons = append(reasons, "parameter_version_mismatch")
	}
	if manifest.Header.AlgorithmVersion != s.header.AlgorithmVersion {
		reasons = append(reasons, "algorithm_version_mismatch")
	}
	if len(reasons) > 0 {
		s.manifest = manifest
		if err := s.invalidateLocked(reasons); err != nil {
			return LoadResult{}, err
		}
		return LoadResult{Invalidated: true, Reasons: reasons, Records: make(map[int]blockRecord)}, nil
	}
	sort.Slice(manifest.Records, func(i, j int) bool {
		return manifest.Records[i].Sequence < manifest.Records[j].Sequence
	})
	s.manifest = manifest
	records := make(map[int]blockRecord, len(manifest.Records))
	seenSequence := -1
	seenBlocks := make(map[int]bool, len(manifest.Records))
	for _, meta := range manifest.Records {
		if meta.Sequence != seenSequence+1 {
			reasons = append(reasons, "checkpoint_sequence_has_gap_or_duplicate")
			break
		}
		if meta.BlockIndex < 0 || seenBlocks[meta.BlockIndex] {
			reasons = append(reasons, fmt.Sprintf("block_%d_duplicate_or_invalid_index", meta.BlockIndex))
			break
		}
		if meta.File != fmt.Sprintf("blocks/%08d.json", meta.Sequence) {
			reasons = append(reasons, fmt.Sprintf("block_%d_invalid_record_path", meta.BlockIndex))
			break
		}
		seenSequence = meta.Sequence
		seenBlocks[meta.BlockIndex] = true
		if meta.ContentHash != s.header.ContentHash ||
			meta.DescriptorVersion != s.header.DescriptorVersion ||
			meta.ParameterVersion != s.header.ParameterVersion ||
			meta.AlgorithmVersion != s.header.AlgorithmVersion {
			reasons = append(reasons, fmt.Sprintf("block_%d_version_mismatch", meta.BlockIndex))
			break
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, meta.File))
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("block_%d_missing", meta.BlockIndex))
			break
		}
		var record blockRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			reasons = append(reasons, fmt.Sprintf("block_%d_corrupt", meta.BlockIndex))
			break
		}
		if record.Sequence != meta.Sequence ||
			record.BlockIndex != meta.BlockIndex ||
			record.Row != meta.Row ||
			record.Col != meta.Col ||
			record.Rect != meta.Rect ||
			record.ContentHash != meta.ContentHash ||
			record.DescriptorVersion != meta.DescriptorVersion ||
			record.ParameterVersion != meta.ParameterVersion ||
			record.AlgorithmVersion != meta.AlgorithmVersion ||
			record.OutputHash != meta.OutputHash ||
			record.ErrorHash != meta.ErrorHash ||
			record.File != meta.File {
			reasons = append(reasons, fmt.Sprintf("block_%d_metadata_mismatch", meta.BlockIndex))
			break
		}
		if tex.HashBytes(record.Output) != record.OutputHash {
			reasons = append(reasons, fmt.Sprintf("block_%d_output_hash_mismatch", meta.BlockIndex))
			break
		}
		if tex.HashBytes(record.Errors) != record.ErrorHash {
			reasons = append(reasons, fmt.Sprintf("block_%d_error_hash_mismatch", meta.BlockIndex))
			break
		}
		records[record.BlockIndex] = record
	}
	if len(reasons) > 0 {
		if err := s.invalidateLocked(reasons); err != nil {
			return LoadResult{}, err
		}
		return LoadResult{Invalidated: true, Reasons: reasons, Records: make(map[int]blockRecord)}, nil
	}
	s.manifest = manifest
	return LoadResult{Records: records, Sequence: seenSequence}, nil
}

func (s *Store) Commit(sequence int, blockIndex, row, col int, rect tex.Rect, converted *tex.Converted) (recordMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sequence != len(s.manifest.Records) {
		return recordMeta{}, fmt.Errorf("checkpoint sequence gap: got %d, want %d", sequence, len(s.manifest.Records))
	}
	outputBytes := encodeUint16(converted.Pixels)
	errorBytes := encodeInt32(converted.Errors)
	meta := recordMeta{
		Sequence:          sequence,
		BlockIndex:        blockIndex,
		Row:               row,
		Col:               col,
		Rect:              rect,
		ContentHash:       s.header.ContentHash,
		DescriptorVersion: s.header.DescriptorVersion,
		ParameterVersion:  s.header.ParameterVersion,
		AlgorithmVersion:  s.header.AlgorithmVersion,
		OutputHash:        tex.HashBytes(outputBytes),
		ErrorHash:         tex.HashBytes(errorBytes),
		Summary: recordSummary{
			PixelCount:   rect.Pixels(),
			ChannelCount: converted.Layout.ChannelCount(),
			MeanAbsError: converted.QuantStats.MeanAbs,
			MaxAbsError:  converted.QuantStats.MaxAbs,
			NonZero:      converted.QuantStats.NonZero,
		},
		File: fmt.Sprintf("blocks/%08d.json", sequence),
	}
	record := blockRecord{recordMeta: meta, Output: outputBytes, Errors: errorBytes}
	payload, err := json.Marshal(record)
	if err != nil {
		return recordMeta{}, err
	}
	recordPath := filepath.Join(s.dir, meta.File)
	if err := atomicWrite(recordPath, payload); err != nil {
		return recordMeta{}, fmt.Errorf("write block checkpoint: %w", err)
	}
	if len(s.manifest.Records) == 0 {
		s.manifest.Header = s.header
	}
	s.manifest.Records = append(s.manifest.Records, meta)
	manifestData, err := json.MarshalIndent(s.manifest, "", "  ")
	if err != nil {
		return recordMeta{}, err
	}
	if err := atomicWrite(s.manifestPath, manifestData); err != nil {
		return recordMeta{}, fmt.Errorf("write checkpoint manifest: %w", err)
	}
	return meta, nil
}

func (s *Store) Invalidate(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalidateLocked([]string{reason})
}

func (s *Store) invalidateLocked(reasons []string) error {
	auditDir := filepath.Join(s.dir, "invalidations")
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		return err
	}
	audit := map[string]any{
		"invalidated_at":   time.Now().UTC().Format(time.RFC3339Nano),
		"reasons":          reasons,
		"expected_header":  s.header,
		"persisted_header": s.manifest.Header,
		"record_count":     len(s.manifest.Records),
	}
	data, _ := json.MarshalIndent(audit, "", "  ")
	name := fmt.Sprintf("invalidated-%d.json", time.Now().UTC().UnixNano())
	if err := atomicWrite(filepath.Join(auditDir, name), data); err != nil {
		return err
	}
	_ = os.Remove(s.manifestPath)
	if err := os.RemoveAll(s.blocksDir); err != nil {
		return err
	}
	if err := os.MkdirAll(s.blocksDir, 0o755); err != nil {
		return err
	}
	s.manifest = checkpointManifest{}
	return nil
}

func (s *Store) Sequence() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.manifest.Records)
}

func (s *Store) Header() checkpointHeader {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.header
}

func encodeUint16(values []uint16) []byte {
	data := make([]byte, len(values)*2)
	for i, value := range values {
		binary.BigEndian.PutUint16(data[i*2:i*2+2], value)
	}
	return data
}

func decodeUint16(data []byte, count int) ([]uint16, error) {
	if len(data) != count*2 {
		return nil, fmt.Errorf("uint16 payload has %d bytes, want %d", len(data), count*2)
	}
	values := make([]uint16, count)
	for i := range values {
		values[i] = binary.BigEndian.Uint16(data[i*2 : i*2+2])
	}
	return values, nil
}

func encodeInt32(values []int32) []byte {
	data := make([]byte, len(values)*4)
	for i, value := range values {
		binary.BigEndian.PutUint32(data[i*4:i*4+4], uint32(value))
	}
	return data
}

func decodeInt32(data []byte, count int) ([]int32, error) {
	if len(data) != count*4 {
		return nil, fmt.Errorf("int32 payload has %d bytes, want %d", len(data), count*4)
	}
	values := make([]int32, count)
	for i := range values {
		values[i] = int32(binary.BigEndian.Uint32(data[i*4 : i*4+4]))
	}
	return values, nil
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".atomic-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	cleanup := func() {
		temp.Close()
		os.Remove(name)
	}
	if _, err := temp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}
