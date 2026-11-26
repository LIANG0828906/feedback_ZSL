package schemax

import (
	"encoding/json"

	"iconflow/errs"
	"iconflow/model"
)

func loadV3(data []byte) (*LoadResult, *errs.Error) {
	root, err := decodeObject(data, "$")
	if err != nil {
		return nil, err
	}
	result := &LoadResult{}
	project := &model.Project{SourceVersion: model.SchemaV3, SchemaVersion: model.SchemaV3}

	metadataRaw, err := field(root, "metadata", "$")
	if err != nil {
		return nil, err
	}
	metadata, err := asObject(metadataRaw, "$.metadata")
	if err != nil {
		return nil, err
	}
	project.ProjectID, err = getString(metadata, "id", "$.metadata")
	if err != nil {
		return nil, err
	}
	seed, err := getInt(metadata, "seed", "$.metadata")
	if err != nil {
		return nil, err
	}
	project.Seed = int64(seed)
	project.ParamVersion = optionalString(metadata, "param_version")
	if project.ParamVersion == "" {
		project.ParamVersion = DefaultParamVersion
	}

	gridRaw, err := field(root, "grid", "$")
	if err != nil {
		return nil, err
	}
	grid, err := asObject(gridRaw, "$.grid")
	if err != nil {
		return nil, err
	}
	project.BlockWidth, err = getInt(grid, "block_width", "$.grid")
	if err != nil {
		return nil, err
	}
	project.BlockHeight, err = getInt(grid, "block_height", "$.grid")
	if err != nil {
		return nil, err
	}

	scaleRaw, err := field(root, "scale", "$")
	if err != nil {
		return nil, err
	}
	scale, err := asObject(scaleRaw, "$.scale")
	if err != nil {
		return nil, err
	}
	project.ThresholdPermyriad, err = getInt(scale, "threshold_per_10k", "$.scale")
	if err != nil {
		return nil, err
	}

	colorRaw, err := field(root, "color", "$")
	if err != nil {
		return nil, err
	}
	color, err := asObject(colorRaw, "$.color")
	if err != nil {
		return nil, err
	}
	orderRaw, err := field(color, "order", "$.color")
	if err != nil {
		return nil, err
	}
	if uerr := json.Unmarshal(orderRaw, &project.ChannelOrder); uerr != nil || len(project.ChannelOrder) == 0 {
		return nil, errs.Schema("$.color.order", "expected non-empty channel array")
	}
	organization, err := getString(color, "organization", "$.color")
	if err != nil {
		return nil, err
	}
	switch organization {
	case model.LayoutGrouped, "by_channel":
		project.ChannelLayout = model.LayoutGrouped
	case model.LayoutFlat, "interleaved":
		project.ChannelLayout = model.LayoutFlat
	default:
		return nil, errs.Schema("$.color.organization", "unknown organization %q", organization)
	}
	project.ColorTolerance = optionalIntPointer(color, "tolerance")

	qRaw, err := field(root, "quantization", "$")
	if err != nil {
		return nil, err
	}
	q, err := asObject(qRaw, "$.quantization")
	if err != nil {
		return nil, err
	}
	project.Quant.Step, err = getInt(q, "step", "$.quantization")
	if err != nil {
		return nil, err
	}
	project.Quant.Rounding = optionalString(q, "remainder_direction")
	if project.Quant.Rounding == "" {
		project.Quant.Rounding = optionalString(q, "rounding")
	}
	if project.Quant.Rounding == "" {
		project.Quant.Rounding = model.RoundFloor
	}
	project.Quant.ExtentDivisible = optionalBool(q, "extent_divisible", false)

	rulesRaw := root["algorithm"]
	project.Rules, err = rulesFromRaw(rulesRaw, "$.algorithm", project.ThresholdPermyriad)
	if err != nil {
		return nil, err
	}

	imagesRaw, err := field(root, "images", "$")
	if err != nil {
		return nil, err
	}
	images, err := getSlice(imagesRaw, "$.images")
	if err != nil {
		return nil, err
	}
	for i, raw := range images {
		path := "$.images[" + itoa(i) + "]"
		obj, ierr := asObject(raw, path)
		if ierr != nil {
			return nil, ierr
		}
		id, ierr := getString(obj, "id", path)
		if ierr != nil {
			return nil, ierr
		}
		imgPath := optionalString(obj, "path")
		rasterRaw, rerr := field(obj, "raster", path)
		if rerr != nil {
			return nil, rerr
		}
		robj, rerr := asObject(rasterRaw, path+".raster")
		if rerr != nil {
			return nil, rerr
		}
		width, rerr := getInt(robj, "width", path+".raster")
		if rerr != nil {
			return nil, rerr
		}
		height, rerr := getInt(robj, "height", path+".raster")
		if rerr != nil {
			return nil, rerr
		}
		var channels []string
		if rawOrder, ok := robj["channels"]; ok {
			_ = json.Unmarshal(rawOrder, &channels)
		}
		if len(channels) == 0 {
			channels = project.ChannelOrder
		}
		var flat []int
		var planes [][]int
		layout := project.ChannelLayout
		if flatRaw, ok := robj["flat"]; ok {
			flat, rerr = intSlice(flatRaw, path+".raster.flat")
			if rerr != nil {
				return nil, rerr
			}
			layout = model.LayoutFlat
		} else if groupedRaw, ok := robj["grouped"]; ok {
			rawPlanes, perr := getSlice(groupedRaw, path+".raster.grouped")
			if perr != nil {
				return nil, perr
			}
			for j, planeRaw := range rawPlanes {
				values, verr := intSlice(planeRaw, path+".raster.grouped["+itoa(j)+"]")
				if verr != nil {
					return nil, verr
				}
				planes = append(planes, values)
			}
			layout = model.LayoutGrouped
		}
		img, traces, berr := buildRaster(id, imgPath, width, height, channels, layout, flat, planes, path+".raster")
		if berr != nil {
			return nil, berr
		}
		project.Images = append(project.Images, img)
		result.Traces = append(result.Traces, traces...)
	}

	if raw, ok := root["panels"]; ok {
		project.Panels, err = parsePanels(raw, "$.panels")
		if err != nil {
			return nil, err
		}
	}
	project.Dirty = parseDirty(root["dirty"])
	project.Truth, err = parseTruth(root["truth"])
	if err != nil {
		return nil, err
	}
	result.Project = project
	return result, nil
}
