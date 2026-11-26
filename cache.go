package strokelab

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const BlockCacheFormat = "stroke-block-cache/v1"

type BlockOrderTrace struct {
	BlockID      int                `json:"block_id"`
	WinnerID     string             `json:"winner_id,omitempty"`
	WinnerOrder  int                `json:"winner_order,omitempty"`
	Rationale    string             `json:"rationale"`
	Participants []string           `json:"participants"`
	Suppressed   []SuppressedStroke `json:"suppressed,omitempty"`
}

type SuppressedStroke struct {
	StrokeID string `json:"stroke_id"`
	Order    int    `json:"order"`
	Reason   string `json:"reason"`
}

type BlockCacheEntry struct {
	Format     string            `json:"format"`
	ProjectID  string            `json:"project_id"`
	BlockID    int               `json:"block_id"`
	Rect       Rect              `json:"rect"`
	CacheKey   string            `json:"cache_key"`
	Mask       []byte            `json:"mask"`
	Components map[string][]byte `json:"components"`
	OrderTrace BlockOrderTrace   `json:"order_trace"`
}

type BlockCache struct {
	Format  string                     `json:"format"`
	Entries map[string]BlockCacheEntry `json:"entries"`
}

func NewBlockCache() *BlockCache {
	return &BlockCache{Format: BlockCacheFormat, Entries: make(map[string]BlockCacheEntry)}
}

func LoadBlockCache(path string) (*BlockCache, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewBlockCache(), nil
		}
		return nil, withDetails(CodeCheckpoint, "read block cache: %v", nil, err)
	}
	var cache BlockCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, withDetails(CodeCheckpoint, "decode block cache: %v", nil, err)
	}
	if cache.Format != BlockCacheFormat {
		return nil, coded(CodeCheckpoint, "unknown block cache format %q", cache.Format)
	}
	if cache.Entries == nil {
		cache.Entries = make(map[string]BlockCacheEntry)
	}
	return &cache, nil
}

func (c *BlockCache) Save(path string) error {
	if c == nil {
		return coded(CodeCheckpoint, "block cache is nil")
	}
	c.Format = BlockCacheFormat
	if c.Entries == nil {
		c.Entries = make(map[string]BlockCacheEntry)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return withDetails(CodeCheckpoint, "encode block cache: %v", nil, err)
	}
	return atomicWriteFile(path, append(data, '\n'))
}

func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return withDetails(CodeCheckpoint, "create cache directory: %v", nil, err)
	}
	f, err := os.CreateTemp(dir, ".cache-*.tmp")
	if err != nil {
		return withDetails(CodeCheckpoint, "create temp cache: %v", nil, err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return withDetails(CodeCheckpoint, "write cache: %v", nil, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return withDetails(CodeCheckpoint, "sync cache: %v", nil, err)
	}
	if err := f.Close(); err != nil {
		return withDetails(CodeCheckpoint, "close cache: %v", nil, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return withDetails(CodeCheckpoint, "replace cache: %v", nil, err)
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func ProjectCompositionHash(p *Project) string {
	h := sha256.New()
	fmt.Fprintf(h, "v=%d|w=%d|h=%d|block=%d|round=%s|conv=%s|",
		1, p.Width, p.Height, p.BlockSize, p.Rounding, p.ConversionVersion)
	h.Write(p.CandidateMask)
	for _, g := range p.Groups {
		fmt.Fprintf(h, "g:%s:%d;", g.Name, g.Stage)
	}
	for _, s := range p.Strokes {
		fmt.Fprintf(h, "s:%s;", s.ContentHash())
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ProjectIdentity(p *Project, ordered []OrderedStroke) CheckpointIdentity {
	return CheckpointIdentity{
		ProjectHash:       ProjectCompositionHash(p),
		OrderVersion:      OrderVersion(ordered),
		ConversionVersion: p.ConversionVersion,
		AlgorithmVersion:  AlgorithmVersion,
		ResidualDigest:    ConversionResidualDigest(p.ConversionTrace),
	}
}

func ConversionResidualDigest(traces []ConversionTrace) string {
	h := sha256.New()
	for _, t := range traces {
		if t.Residual == 0 {
			continue
		}
		fmt.Fprintf(h, "%s|%s|%d;", t.Version, t.Path, t.Residual)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func BlockInputHash(p *Project, block Rect) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d,%d,%d,%d|", block.X, block.Y, block.W, block.H)
	for y := block.Y; y < block.Y+block.H; y++ {
		start := y*p.Width + block.X
		h.Write(p.CandidateMask[start : start+block.W])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func BlockStrokeRefs(parts []OrderedStroke) []string {
	refs := make([]string, 0, len(parts))
	for _, os := range parts {
		refs = append(refs, os.Stroke.ID+"="+os.Stroke.ContentHash())
	}
	return refs
}

func LocalOrderVersion(parts []OrderedStroke) string {
	h := sha256.New()
	for _, os := range parts {
		fmt.Fprintf(h, "%s|%d|%d;", os.Stroke.ID, os.Stroke.Stage, os.Stroke.Seq)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ComputeBlockCacheKey combines block content, the exact local stroke set,
// local composition order, conversion parameter version, and algorithm version.
func ComputeBlockCacheKey(p *Project, block Rect, parts []OrderedStroke) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|",
		BlockInputHash(p, block), LocalOrderVersion(parts),
		p.ConversionVersion, AlgorithmVersion)
	for _, ref := range BlockStrokeRefs(parts) {
		fmt.Fprintf(h, "%s;", ref)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (c *BlockCache) Put(key string, entry BlockCacheEntry) {
	if c.Entries == nil {
		c.Entries = make(map[string]BlockCacheEntry)
	}
	entry.Format = BlockCacheFormat
	entry.CacheKey = key
	c.Entries[key] = entry
}

func (c *BlockCache) SortedKeys() []string {
	keys := make([]string, 0, len(c.Entries))
	for k := range c.Entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func buildBlockOrderTrace(blockID int, ordered []OrderedStroke, components map[string][]uint8) BlockOrderTrace {
	trace := BlockOrderTrace{BlockID: blockID, Rationale: "canonical_stage_seq_order"}
	type participant struct {
		id    string
		order int
	}
	var nonzero []participant
	for _, os := range ordered {
		comp := components[os.Stroke.ID]
		any := false
		for _, a := range comp {
			if a != 0 {
				any = true
				break
			}
		}
		trace.Participants = append(trace.Participants, os.Stroke.ID)
		if any {
			nonzero = append(nonzero, participant{os.Stroke.ID, os.Position})
		} else {
			trace.Suppressed = append(trace.Suppressed, SuppressedStroke{
				StrokeID: os.Stroke.ID, Order: os.Position,
				Reason: "zero_coverage_in_block",
			})
		}
	}
	if len(nonzero) == 0 {
		trace.Rationale = "no_nonzero_component"
		sort.SliceStable(trace.Suppressed, func(i, j int) bool {
			return trace.Suppressed[i].Order < trace.Suppressed[j].Order
		})
		return trace
	}
	winner := nonzero[len(nonzero)-1]
	trace.WinnerID = winner.id
	trace.WinnerOrder = winner.order
	trace.Rationale = "last_nonzero_alpha_over_in_canonical_order"
	for _, p := range nonzero[:len(nonzero)-1] {
		trace.Suppressed = append(trace.Suppressed, SuppressedStroke{
			StrokeID: p.id, Order: p.order, Reason: "later_source_over",
		})
	}
	sort.SliceStable(trace.Suppressed, func(i, j int) bool {
		return trace.Suppressed[i].Order < trace.Suppressed[j].Order
	})
	return trace
}
