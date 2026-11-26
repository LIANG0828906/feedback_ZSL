package model

import (
	"fmt"
	"sort"
)

const (
	SchemaV1 = "icon-project.v1"
	SchemaV2 = "icon-project.v2"
	SchemaV3 = "icon-project.v3"

	LayoutFlat    = "flat_interleaved"
	LayoutGrouped = "channel_grouped"

	RoundFloor   = "floor"
	RoundCeil    = "ceil"
	RoundNearest = "nearest"
)

type BlockCoord struct {
	ImageID string `json:"image_id"`
	Index   int    `json:"index"`
	TX      int    `json:"tx"`
	TY      int    `json:"ty"`
	OriginX int    `json:"origin_x"`
	OriginY int    `json:"origin_y"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
}

func (b BlockCoord) Key() string {
	return fmt.Sprintf("%s#%d,%d", b.ImageID, b.TX, b.TY)
}

func (b BlockCoord) String() string { return b.Key() }

type Centroid struct {
	XMilli int64 `json:"x_milli"`
	YMilli int64 `json:"y_milli"`
}

type ComponentTruth struct {
	Area     int      `json:"area"`
	Centroid Centroid `json:"centroid"`
}

type BlockTruth struct {
	Coord      BlockCoord       `json:"coord"`
	Foreground int              `json:"foreground"`
	Components []ComponentTruth `json:"components"`
}

type DirtyRecord struct {
	PanelID string   `json:"panel_id"`
	Coord   string   `json:"coord"`
	Fields  []string `json:"fields"`
	Reason  string   `json:"reason"`
}

type Panel struct {
	ID     string   `json:"id"`
	Blocks []string `json:"blocks"`
}

type Raster struct {
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	Channels []string `json:"channels"`
	Layout   string   `json:"layout"`
	Flat     []int    `json:"flat,omitempty"`
	Planes   [][]int  `json:"planes,omitempty"`
}

func (r *Raster) ChannelIndex(name string) int {
	for i, candidate := range r.Channels {
		if candidate == name {
			return i
		}
	}
	return -1
}

type Image struct {
	ID     string  `json:"id"`
	Path   string  `json:"path,omitempty"`
	Raster *Raster `json:"raster"`
}

type Tolerances struct {
	AreaAbs           int   `json:"area_abs"`
	CentroidMilliAbs  int64 `json:"centroid_milli_abs"`
	RegionCountStrict bool  `json:"region_count_strict"`
}

type BlockOverride struct {
	ThresholdPermyriad *int     `json:"threshold_permyriad,omitempty"`
	ChannelOrder       []string `json:"channel_order,omitempty"`
	QuantStep          *int     `json:"quant_step,omitempty"`
	OperatorVersion    string   `json:"operator_version,omitempty"`
}

type Rules struct {
	GradientOperator  string     `json:"gradient_operator"`
	OperatorVersion   string     `json:"operator_version"`
	QuantilePermyriad int        `json:"quantile_permyriad"`
	MinContrast       int        `json:"min_contrast"`
	ThresholdMin      int        `json:"threshold_min"`
	ThresholdMax      int        `json:"threshold_max"`
	MinComponentArea  int        `json:"min_component_area"`
	Connectivity      int        `json:"connectivity"`
	FillHoles         bool       `json:"fill_holes"`
	Tolerances        Tolerances `json:"tolerances"`
}

type QuantSpec struct {
	Step            int    `json:"step"`
	Rounding        string `json:"rounding"`
	ExtentDivisible bool   `json:"extent_divisible"`
}

type Project struct {
	SchemaVersion      string                   `json:"schema_version"`
	SourceVersion      string                   `json:"source_version"`
	ProjectID          string                   `json:"project_id"`
	Seed               int64                    `json:"seed"`
	BlockWidth         int                      `json:"block_width"`
	BlockHeight        int                      `json:"block_height"`
	ThresholdPermyriad int                      `json:"threshold_permyriad"`
	ChannelOrder       []string                 `json:"channel_order"`
	ChannelLayout      string                   `json:"channel_layout"`
	Quant              QuantSpec                `json:"quant"`
	ColorTolerance     *int                     `json:"color_tolerance,omitempty"`
	ParamVersion       string                   `json:"param_version"`
	Rules              Rules                    `json:"rules"`
	Images             []*Image                 `json:"images"`
	Panels             []Panel                  `json:"panels"`
	Truth              map[string]*BlockTruth   `json:"truth"`
	Dirty              []DirtyRecord            `json:"dirty"`
	Overrides          map[string]BlockOverride `json:"overrides,omitempty"`
	UpgradeRecords     []ConversionRecord       `json:"upgrade_records,omitempty"`
}

type ConversionRecord struct {
	Field       string `json:"field"`
	FromVersion string `json:"from_version,omitempty"`
	ToVersion   string `json:"to_version,omitempty"`
	Operation   string `json:"operation"`
	OldValue    any    `json:"old_value,omitempty"`
	NewValue    any    `json:"new_value"`
	Source      string `json:"source,omitempty"`
	OperandA    any    `json:"operand_a,omitempty"`
	OperandB    any    `json:"operand_b,omitempty"`
	Quotient    int64  `json:"quotient,omitempty"`
	Remainder   int64  `json:"remainder,omitempty"`
	Rounding    string `json:"rounding,omitempty"`
	TruncatedAt string `json:"truncated_at,omitempty"`
}

func (p *Project) SortedBlockCoords() []BlockCoord {
	var coords []BlockCoord
	for _, img := range p.Images {
		if img.Raster == nil {
			continue
		}
		cols := (img.Raster.Width + p.BlockWidth - 1) / p.BlockWidth
		rows := (img.Raster.Height + p.BlockHeight - 1) / p.BlockHeight
		for ty := 0; ty < rows; ty++ {
			for tx := 0; tx < cols; tx++ {
				bw := p.BlockWidth
				bh := p.BlockHeight
				if ox := tx * p.BlockWidth; ox+bw > img.Raster.Width {
					bw = img.Raster.Width - ox
				}
				if oy := ty * p.BlockHeight; oy+bh > img.Raster.Height {
					bh = img.Raster.Height - oy
				}
				coords = append(coords, BlockCoord{
					ImageID: img.ID,
					Index:   ty*cols + tx,
					TX:      tx, TY: ty,
					OriginX: tx * p.BlockWidth,
					OriginY: ty * p.BlockHeight,
					Width:   bw, Height: bh,
				})
			}
		}
	}
	sort.Slice(coords, func(i, j int) bool {
		if coords[i].ImageID != coords[j].ImageID {
			return coords[i].ImageID < coords[j].ImageID
		}
		if coords[i].TY != coords[j].TY {
			return coords[i].TY < coords[j].TY
		}
		return coords[i].TX < coords[j].TX
	})
	for i := range coords {
		coords[i].Index = i
	}
	return coords
}

func (p *Project) ImageByID(id string) *Image {
	for _, img := range p.Images {
		if img.ID == id {
			return img
		}
	}
	return nil
}

func SortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
