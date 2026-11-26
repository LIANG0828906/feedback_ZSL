package schemax

import (
	"encoding/json"
	"strings"

	"iconflow/convert"
	"iconflow/errs"
	"iconflow/model"
)

const (
	DefaultV1QuantStep  = 6
	DefaultParamVersion = "param-compat-v1"
	DefaultAlgoVersion  = "sobel-v1"
)

type LoadResult struct {
	Project        *model.Project           `json:"project"`
	UpgradeRecords []model.ConversionRecord `json:"upgrade_records"`
	Traces         []model.ConversionRecord `json:"traces"`
}

func LoadDocument(data []byte) (*LoadResult, *errs.Error) {
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, errs.Schema("$.schema", "invalid JSON: %v", err)
	}
	if head.Schema == "" {
		return nil, errs.Schema("$.schema", "missing required field %q", "schema")
	}
	switch head.Schema {
	case model.SchemaV1:
		return loadV1(data)
	case model.SchemaV2:
		return loadV2(data)
	case model.SchemaV3:
		return loadV3(data)
	default:
		return nil, errs.Schema("$.schema", "unknown schema version %q", head.Schema)
	}
}

func LoadDocumentOrError(data []byte) (*LoadResult, error) {
	result, err := LoadDocument(data)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func defaultChannelOrder() []string { return []string{"R", "G", "B", "A"} }

func defaultRules() model.Rules {
	return model.Rules{
		GradientOperator:  "sobel",
		OperatorVersion:   DefaultAlgoVersion,
		QuantilePermyriad: 8500,
		MinContrast:       96,
		ThresholdMin:      24,
		ThresholdMax:      1200,
		MinComponentArea:  6,
		Connectivity:      8,
		FillHoles:         true,
		Tolerances: model.Tolerances{
			AreaAbs:          72,
			CentroidMilliAbs: 1750,
		},
	}
}

func parseChannelOrder(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.Contains(value, ",") {
		parts := strings.Split(value, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts
	}
	return strings.Split(value, "")
}

func buildRaster(id, path string, width, height int, channels []string, layout string, flat []int, planes [][]int, fieldPath string) (*model.Image, []model.ConversionRecord, *errs.Error) {
	traces := []model.ConversionRecord{}
	if width <= 0 || height <= 0 {
		return nil, nil, errs.Schema(fieldPath, "width and height must be positive")
	}
	pixelCount := width * height
	if len(channels) == 0 {
		channels = defaultChannelOrder()
	}
	if layout == "" {
		layout = model.LayoutFlat
	}
	raster := &model.Raster{Width: width, Height: height, Channels: channels, Layout: layout}

	switch layout {
	case model.LayoutFlat:
		if len(flat) == 0 {
			return nil, nil, errs.Schema(fieldPath+".pixels", "flat pixel array is required")
		}
		if want := pixelCount * len(channels); len(flat) != want {
			return nil, nil, errs.Schema(fieldPath+".pixels", "flat array length %d != %d", len(flat), want)
		}
		raster.Flat = flat
		raster.Planes = make([][]int, len(channels))
		for ch := range channels {
			raster.Planes[ch] = make([]int, pixelCount)
			for p := 0; p < pixelCount; p++ {
				raster.Planes[ch][p] = flat[p*len(channels)+ch]
			}
		}
		if len(channels) > 0 {
			_, tr, err := convert.ChannelOffset(layout, 0, 0, width, height, 0, len(channels), fieldPath+".channel_offset")
			if err != nil {
				return nil, nil, err
			}
			traces = append(traces, tr)
		}
	case model.LayoutGrouped:
		if len(planes) != len(channels) {
			return nil, nil, errs.Schema(fieldPath+".planes", "plane count %d != channel count %d", len(planes), len(channels))
		}
		raster.Planes = planes
		raster.Flat = make([]int, pixelCount*len(channels))
		for ch := range channels {
			if len(planes[ch]) != pixelCount {
				return nil, nil, errs.Schema(fieldPath+".planes", "plane %d length %d != %d", ch, len(planes[ch]), pixelCount)
			}
			for p := 0; p < pixelCount; p++ {
				raster.Flat[p*len(channels)+ch] = planes[ch][p]
			}
		}
		if len(channels) > 0 {
			_, tr, err := convert.ChannelOffset(layout, 0, 0, width, height, 0, len(channels), fieldPath+".channel_offset")
			if err != nil {
				return nil, nil, err
			}
			traces = append(traces, tr)
		}
	default:
		return nil, nil, errs.Schema(fieldPath+".layout", "unknown layout %q", layout)
	}
	return &model.Image{ID: id, Path: path, Raster: raster}, traces, nil
}

func parseTruth(raw json.RawMessage) (map[string]*model.BlockTruth, *errs.Error) {
	if len(raw) == 0 {
		return map[string]*model.BlockTruth{}, nil
	}
	var truth map[string]*model.BlockTruth
	if err := json.Unmarshal(raw, &truth); err != nil {
		return nil, errs.Schema("$.truth", "invalid truth map: %v", err)
	}
	if truth == nil {
		truth = map[string]*model.BlockTruth{}
	}
	return truth, nil
}

func parsePanels(raw json.RawMessage, path string) ([]model.Panel, *errs.Error) {
	if len(raw) == 0 {
		return nil, nil
	}
	items, err := getSlice(raw, path)
	if err != nil {
		return nil, err
	}
	panels := make([]model.Panel, 0, len(items))
	for i, item := range items {
		obj, oerr := asObject(item, path)
		if oerr != nil {
			return nil, oerr
		}
		id, ierr := getString(obj, "id", path)
		if ierr != nil {
			return nil, ierr
		}
		blockRaw, berr := field(obj, "blocks", path+"["+itoa(i)+"]")
		if berr != nil {
			return nil, berr
		}
		var blocks []string
		if uerr := json.Unmarshal(blockRaw, &blocks); uerr != nil {
			return nil, errs.Schema(path+"["+itoa(i)+"].blocks", "expected string array: %v", uerr)
		}
		panels = append(panels, model.Panel{ID: id, Blocks: blocks})
	}
	return panels, nil
}

func parseDirty(raw json.RawMessage) []model.DirtyRecord {
	if len(raw) == 0 {
		return nil
	}
	var dirty []model.DirtyRecord
	_ = json.Unmarshal(raw, &dirty)
	return dirty
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
