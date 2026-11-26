package schemax

import (
	"encoding/json"

	"iconflow/errs"
	"iconflow/model"
)

func loadV2(data []byte) (*LoadResult, *errs.Error) {
	root, err := decodeObject(data, "$")
	if err != nil {
		return nil, err
	}
	result := &LoadResult{}
	project := &model.Project{SourceVersion: model.SchemaV2, SchemaVersion: model.SchemaV2}

	project.ProjectID, err = getString(root, "project_id", "$")
	if err != nil {
		return nil, err
	}
	seed, err := getInt(root, "seed", "$")
	if err != nil {
		return nil, err
	}
	project.Seed = int64(seed)

	sizeRaw, err := field(root, "tile_size", "$")
	if err != nil {
		sizeRaw, err = field(root, "tile", "$")
		if err != nil {
			return nil, err
		}
	}
	size, err := asObject(sizeRaw, "$.tile_size")
	if err != nil {
		return nil, err
	}
	project.BlockWidth = optionalInt(size, "w", optionalInt(size, "width", 0))
	project.BlockHeight = optionalInt(size, "h", optionalInt(size, "height", 0))
	if project.BlockWidth <= 0 || project.BlockHeight <= 0 {
		return nil, errs.Schema("$.tile_size", "w/h must be positive")
	}

	scale, err := getInt(root, "cut_permyriad", "$")
	if err != nil {
		scale, err = getInt(root, "threshold_permyriad", "$")
		if err != nil {
			return nil, err
		}
	}
	project.ThresholdPermyriad = scale
	project.Rules.QuantilePermyriad = scale

	orderRaw, oerr := field(root, "channel_order", "$")
	if oerr != nil {
		return nil, oerr
	}
	if uerr := json.Unmarshal(orderRaw, &project.ChannelOrder); uerr != nil || len(project.ChannelOrder) == 0 {
		return nil, errs.Schema("$.channel_order", "expected non-empty channel name array")
	}
	organization := optionalString(root, "pixel_organization")
	if organization == "" {
		organization = optionalString(root, "channel_layout")
	}
	switch organization {
	case "by_channel", model.LayoutGrouped:
		project.ChannelLayout = model.LayoutGrouped
	case "interleaved", model.LayoutFlat:
		project.ChannelLayout = model.LayoutFlat
	default:
		return nil, errs.Schema("$.pixel_organization", "unknown organization %q", organization)
	}

	qRaw, qerr := field(root, "quantization", "$")
	if qerr != nil {
		return nil, qerr
	}
	q, qerr2 := asObject(qRaw, "$.quantization")
	if qerr2 != nil {
		return nil, qerr2
	}
	project.Quant.Step, qerr2 = getInt(q, "step", "$.quantization")
	if qerr2 != nil {
		return nil, qerr2
	}
	project.Quant.Rounding = optionalString(q, "round")
	if project.Quant.Rounding == "" {
		project.Quant.Rounding = optionalString(q, "rounding")
	}
	if project.Quant.Rounding == "" {
		project.Quant.Rounding = model.RoundFloor
	}
	project.Quant.ExtentDivisible = optionalBool(q, "divides_extent", optionalBool(q, "extent_divisible", false))
	project.ColorTolerance = optionalIntPointer(root, "color_tolerance")
	project.ParamVersion = optionalString(root, "param_revision")
	if project.ParamVersion == "" {
		project.ParamVersion = optionalString(root, "param_version")
	}
	if project.ParamVersion == "" {
		project.ParamVersion = DefaultParamVersion
	}

	rulesRaw := root["segmentation"]
	project.Rules, err = rulesFromRaw(rulesRaw, "$.segmentation", project.ThresholdPermyriad)
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
		width, ierr := getInt(obj, "width", path)
		if ierr != nil {
			return nil, ierr
		}
		height, ierr := getInt(obj, "height", path)
		if ierr != nil {
			return nil, ierr
		}
		imgPath := optionalString(obj, "path")
		var flat []int
		var planes [][]int
		layout := project.ChannelLayout
		if planesRaw, ok := obj["channel_planes"]; ok {
			layout = model.LayoutGrouped
			planeItems, perr := getSlice(planesRaw, path+".channel_planes")
			if perr != nil {
				return nil, perr
			}
			channels := []string{}
			for j, planeRaw := range planeItems {
				pobj, perr := asObject(planeRaw, path+".channel_planes["+itoa(j)+"]")
				if perr != nil {
					return nil, perr
				}
				name, nerr := getString(pobj, "channel", path+".channel_planes["+itoa(j)+"]")
				if nerr != nil {
					return nil, nerr
				}
				valuesRaw, verr := field(pobj, "values", path+".channel_planes["+itoa(j)+"]")
				if verr != nil {
					return nil, verr
				}
				values, verr := intSlice(valuesRaw, path+".channel_planes["+itoa(j)+"].values")
				if verr != nil {
					return nil, verr
				}
				channels = append(channels, name)
				planes = append(planes, values)
			}
			project.ChannelOrder = channels
			img, traces, berr := buildRaster(id, imgPath, width, height, channels, layout, nil, planes, path)
			if berr != nil {
				return nil, berr
			}
			project.Images = append(project.Images, img)
			result.Traces = append(result.Traces, traces...)
			continue
		}
		if flatRaw, ok := obj["pixels"]; ok {
			flat, ierr = intSlice(flatRaw, path+".pixels")
			if ierr != nil {
				return nil, ierr
			}
			layout = model.LayoutFlat
		}
		img, traces, berr := buildRaster(id, imgPath, width, height, project.ChannelOrder, layout, flat, planes, path)
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
	project.Dirty = parseDirty(root["dirty_blocks"])
	project.Truth, err = parseTruth(root["expected_blocks"])
	if err != nil {
		return nil, err
	}
	result.Project = project
	return result, nil
}

func optionalIntPointer(obj rawObject, key string) *int {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return nil
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return &value
}
