package channelpipe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

const (
	ParamVersion     = "params-v1"
	AlgorithmVersion = "integer-core-v1"
	CacheVersion     = "cache-v1"
)

type Channel string

const (
	ChannelR Channel = "R"
	ChannelG Channel = "G"
	ChannelB Channel = "B"
	ChannelA Channel = "A"
)

type AlphaMode string

const (
	AlphaPassthrough AlphaMode = "passthrough"
	AlphaOpaque      AlphaMode = "opaque"
	AlphaPremultiply AlphaMode = "premultiply"
	AlphaDiscard     AlphaMode = "discard"
)

type Rounding string

const (
	RoundNearest Rounding = "nearest"
	RoundFloor   Rounding = "floor"
	RoundCeil    Rounding = "ceil"
)

type FillKind string

const (
	FillConstant FillKind = "constant"
	FillZero     FillKind = "zero"
	FillOpaque   FillKind = "opaque"
)

type DropPolicy string

const (
	DropRecord DropPolicy = "record"
	DropSilent DropPolicy = "silent"
)

type FillSpec struct {
	Kind  FillKind `json:"kind"`
	Value uint16   `json:"value,omitempty"`
}

type Stage struct {
	Index          int               `json:"index"`
	ID             string            `json:"id"`
	TargetDepth    int               `json:"target_depth"`
	Rounding       Rounding          `json:"rounding"`
	TargetChannels []Channel         `json:"target_channels"`
	Swizzle        []int             `json:"swizzle"`
	Fills          []FillSpec        `json:"fills,omitempty"`
	DropPolicy     DropPolicy        `json:"drop_policy"`
	AlphaMode      AlphaMode         `json:"alpha_mode"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

type Plan struct {
	ID           string    `json:"id"`
	Seed         int64     `json:"seed"`
	Width        int       `json:"width"`
	Height       int       `json:"height"`
	SourceDepth  int       `json:"source_depth"`
	SourceLayout []Channel `json:"source_layout"`
	Stages       []Stage   `json:"stages"`
}

type PlanSet struct {
	ID    string `json:"id"`
	Seed  int64  `json:"seed"`
	Plans []Plan `json:"plans"`
}

type FieldTrace struct {
	PlanID        string `json:"plan_id,omitempty"`
	StageIndex    int    `json:"stage_index,omitempty"`
	FieldPath     string `json:"field_path"`
	OldValue      any    `json:"old_value"`
	SourceDefault any    `json:"source_default"`
	Reason        string `json:"reason"`
}

type LoadResult struct {
	Set           PlanSet      `json:"set"`
	SourceVersion int          `json:"source_version"`
	VersionField  string       `json:"version_field"`
	UpgradeTraces []FieldTrace `json:"upgrade_traces,omitempty"`
}

type LossyItem struct {
	Code        string `json:"code"`
	PlanID      string `json:"plan_id,omitempty"`
	StageIndex  int    `json:"stage_index,omitempty"`
	FieldPath   string `json:"field_path"`
	Feature     string `json:"feature"`
	Message     string `json:"message"`
	Disposition string `json:"disposition"`
}

type LossReport struct {
	TargetVersion int         `json:"target_version"`
	Items         []LossyItem `json:"items,omitempty"`
}

func (r LossReport) HasLoss() bool { return len(r.Items) > 0 }

type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (r Rect) PixelCount() int { return r.Width * r.Height }

func (r Rect) Intersects(other Rect) bool {
	return r.X < other.X+other.Width && r.X+r.Width > other.X &&
		r.Y < other.Y+other.Height && r.Y+r.Height > other.Y
}

type Chunk struct {
	Index int  `json:"index"`
	Rect  Rect `json:"rect"`
}

type Buffer struct {
	Width    int       `json:"width"`
	Height   int       `json:"height"`
	Depth    int       `json:"depth"`
	Channels []Channel `json:"channels"`
	Pixels   []uint16  `json:"-"`
}

func (b *Buffer) ChannelCount() int { return len(b.Channels) }
func (b *Buffer) PixelCount() int   { return b.Width * b.Height }
func (b *Buffer) SampleIndex(x, y, channel int) int {
	return (y*b.Width+x)*b.ChannelCount() + channel
}

func BuildChunks(width, height, tileWidth, tileHeight int) []Chunk {
	if tileWidth <= 0 {
		tileWidth = 128
	}
	if tileHeight <= 0 {
		tileHeight = 128
	}
	chunks := make([]Chunk, 0, ((width+tileWidth-1)/tileWidth)*((height+tileHeight-1)/tileHeight))
	index := 0
	for y := 0; y < height; y += tileHeight {
		h := tileHeight
		if y+h > height {
			h = height - y
		}
		for x := 0; x < width; x += tileWidth {
			w := tileWidth
			if x+w > width {
				w = width - x
			}
			chunks = append(chunks, Chunk{Index: index, Rect: Rect{X: x, Y: y, Width: w, Height: h}})
			index++
		}
	}
	return chunks
}

func CanonicalJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var normalized any
	if err := json.Unmarshal(data, &normalized); err != nil {
		return nil, err
	}
	normalized = sortJSON(normalized)
	return json.Marshal(normalized)
}

func sortJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		result := make(map[string]any, len(keys))
		for _, key := range keys {
			result[key] = sortJSON(typed[key])
		}
		return result
	case []any:
		for i := range typed {
			typed[i] = sortJSON(typed[i])
		}
		return typed
	default:
		return value
	}
}

func SHA256Hex(parts ...[]byte) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write(part)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func Fingerprint(value any) (string, error) {
	data, err := CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	return SHA256Hex(data), nil
}
