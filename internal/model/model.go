package model

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	BlockCount   = 6
	DataBlocks   = 4
	ParityBlocks = 2

	StatusHealthy       = "healthy"
	StatusDegraded      = "degraded"
	StatusUnrecoverable = "unrecoverable"
)

type ObjectMeta struct {
	ID         string    `json:"object_id"`
	Size       int64     `json:"size"`
	StripeSize int64     `json:"stripe_size"`
	DataSHA256 string    `json:"data_sha256"`
	CreatedAt  time.Time `json:"created_at"`
	Seed       int64     `json:"seed,omitempty"`
	Stripes    []string  `json:"stripes"`
}

type BlockMeta struct {
	Index         int    `json:"index"`
	PrimaryPath   string `json:"primary_path"`
	PrimaryLength int64  `json:"primary_length"`
	PrimarySHA256 string `json:"primary_sha256"`
	PrimaryExists bool   `json:"primary_exists"`
	BackupPath    string `json:"backup_path"`
	BackupLength  int64  `json:"backup_length"`
	BackupSHA256  string `json:"backup_sha256"`
	BackupState   string `json:"backup_state"`
	BackupWritten string `json:"backup_written_at,omitempty"`
	BackupJobID   string `json:"backup_job_id,omitempty"`
}

type StripeMeta struct {
	ID              string      `json:"stripe_id"`
	ObjectID        string      `json:"object_id"`
	Index           int         `json:"stripe_index"`
	OffsetStart     int64       `json:"offset_start"`
	OffsetEnd       int64       `json:"offset_end"`
	Length          int64       `json:"length"`
	ShardLength     int64       `json:"shard_length"`
	DataSHA256      string      `json:"data_sha256"`
	Status          string      `json:"status"`
	AvailableBlocks int         `json:"available_blocks"`
	MissingBlocks   []int       `json:"missing_blocks"`
	DegradedReason  string      `json:"degraded_reason,omitempty"`
	CreatedAt       time.Time   `json:"created_at"`
	LastReadAt      time.Time   `json:"last_read_at,omitempty"`
	ReadCount       int64       `json:"read_count"`
	RepairEpoch     int64       `json:"repair_epoch"`
	RepairState     string      `json:"repair_state,omitempty"`
	RepairJobID     string      `json:"repair_job_id,omitempty"`
	CooldownUntil   time.Time   `json:"repair_cooldown_until,omitempty"`
	LastError       string      `json:"last_error,omitempty"`
	Blocks          []BlockMeta `json:"blocks"`
}

func (s *StripeMeta) Block(index int) *BlockMeta {
	for i := range s.Blocks {
		if s.Blocks[i].Index == index {
			return &s.Blocks[i]
		}
	}
	return nil
}

func (s *StripeMeta) NormalizeBlocks() {
	if len(s.Blocks) == BlockCount {
		sort.Slice(s.Blocks, func(i, j int) bool { return s.Blocks[i].Index < s.Blocks[j].Index })
		return
	}
	byIndex := make(map[int]BlockMeta, len(s.Blocks))
	for _, b := range s.Blocks {
		byIndex[b.Index] = b
	}
	s.Blocks = make([]BlockMeta, BlockCount)
	for i := 0; i < BlockCount; i++ {
		b, ok := byIndex[i]
		if !ok {
			b = BlockMeta{Index: i}
		}
		s.Blocks[i] = b
	}
}

type DirState struct {
	Index     int
	Online    bool
	Reason    string
	UpdatedAt time.Time
}

type DirSet struct {
	Epoch int64
	Dirs  [BlockCount]DirState
}

func (d DirSet) IsOnline(index int) bool {
	if index < 0 || index >= BlockCount {
		return false
	}
	return d.Dirs[index].Online
}

func (d DirSet) Labels() []string {
	out := make([]string, 0, BlockCount)
	for i := 0; i < BlockCount; i++ {
		out = append(out, fmt.Sprintf("block-%d", i))
	}
	return out
}

type BlockSource struct {
	Index  int
	Site   string
	Path   string
	Length int64
	SHA256 string
}

func SourcesForBlock(s *StripeMeta, dirs DirSet, index int) []BlockSource {
	b := s.Block(index)
	if b == nil {
		return nil
	}
	out := make([]BlockSource, 0, 2)
	if dirs.IsOnline(index) && b.PrimaryExists && b.PrimaryPath != "" {
		out = append(out, BlockSource{Index: index, Site: "primary", Path: b.PrimaryPath, Length: b.PrimaryLength, SHA256: b.PrimarySHA256})
	}
	if b.BackupState == "committed" && b.BackupPath != "" {
		out = append(out, BlockSource{Index: index, Site: "standby", Path: b.BackupPath, Length: b.BackupLength, SHA256: b.BackupSHA256})
	}
	return out
}

func (s *StripeMeta) Refresh(dirs DirSet) {
	s.NormalizeBlocks()
	available := 0
	primaryAvailable := 0
	missing := make([]int, 0, BlockCount)
	primaryMissing := make([]int, 0, BlockCount)
	standbyServed := make([]int, 0, BlockCount)
	for i := 0; i < BlockCount; i++ {
		srcs := SourcesForBlock(s, dirs, i)
		if len(srcs) == 0 {
			missing = append(missing, i)
			primaryMissing = append(primaryMissing, i)
			continue
		}
		available++
		for _, src := range srcs {
			if src.Site == "primary" {
				primaryAvailable++
				break
			}
		}
		if srcs[0].Site != "primary" {
			standbyServed = append(standbyServed, i)
			primaryMissing = append(primaryMissing, i)
		}
	}
	s.AvailableBlocks = available
	s.MissingBlocks = missing
	reasons := make([]string, 0, 4)
	for i := 0; i < BlockCount; i++ {
		if !dirs.IsOnline(i) {
			r := fmt.Sprintf("block_dir_offline:block-%d", i)
			if dirs.Dirs[i].Reason != "" {
				r += ":" + dirs.Dirs[i].Reason
			}
			reasons = append(reasons, r)
		}
	}
	if len(missing) > 0 {
		reasons = append(reasons, "missing_blocks:"+joinInts(missing))
	}
	if len(standbyServed) > 0 {
		reasons = append(reasons, "served_from_standby:"+joinInts(standbyServed))
	}
	if available < DataBlocks {
		s.Status = StatusUnrecoverable
	} else if primaryAvailable < BlockCount || len(missing) > 0 || len(standbyServed) > 0 {
		s.Status = StatusDegraded
	} else {
		s.Status = StatusHealthy
	}
	s.DegradedReason = strings.Join(reasons, ";")
}

func joinInts(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = fmt.Sprintf("%d", n)
	}
	return strings.Join(parts, ",")
}

func (s *StripeMeta) EstimatedRepairBytes(dirs DirSet) int64 {
	s.Refresh(dirs)
	return int64(len(s.MissingBlocks)) * s.ShardLength
}

func (s *StripeMeta) MissingKey() string {
	sort.Ints(s.MissingBlocks)
	return joinInts(s.MissingBlocks)
}

type DegradationRecord struct {
	StripeID        string
	ObjectID        string
	DirIndex        int
	Action          string
	Reason          string
	AvailableBlocks int
	Status          string
	Details         string
}

type RepairJob struct {
	JobID         string
	StripeID      string
	ObjectID      string
	MissingBlocks []int
	State         string
	ReservedBytes int64
	Day           string
	StartedAt     time.Time
	FinishedAt    time.Time
	Error         string
	ResultJSON    string
	MetadataDone  bool
}

type QuotaUsage struct {
	Day            string `json:"day"`
	LimitBytes     int64  `json:"limit_bytes"`
	UsedBytes      int64  `json:"used_bytes"`
	ReservedBytes  int64  `json:"reserved_bytes"`
	CommittedBytes int64  `json:"committed_bytes"`
	RemainingBytes int64  `json:"remaining_bytes"`
}
