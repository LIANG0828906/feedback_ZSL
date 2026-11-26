package schemax

import (
	"encoding/json"

	"iconflow/errs"
	"iconflow/model"
)

func loadV1(data []byte) (*LoadResult, *errs.Error) {
	root, err := decodeObject(data, "$")
	if err != nil {
		return nil, err
	}
	result := &LoadResult{}
	project := &model.Project{SourceVersion: model.SchemaV1, SchemaVersion: model.SchemaV1}

	project.ProjectID, err = getString(root, "project_id", "$")
	if err != nil {
		return nil, err
	}
	seed, serr := getInt(root, "seed", "$")
	if serr != nil {
		return nil, serr
	}
	project.Seed = int64(seed)

	tileRaw, terr := field(root, "tile", "$")
	if terr != nil {
		return nil, terr
	}
	tile, terr := asObject(tileRaw, "$.tile")
	if terr != nil {
		return nil, terr
	}
	project.BlockWidth, terr = getInt(tile, "width", "$.tile")
	if terr != nil {
		return nil, terr
	}
	project.BlockHeight, terr = getInt(tile, "height", "$.tile")
	if terr != nil {
		return nil, terr
	}

	permille, terr := getInt(root, "threshold_permille", "$")
	if terr != nil {
		return nil, terr
	}
	permyriad, trace, cerr := permilleToPermyriad(permille, "$.threshold_permille")
	if cerr != nil {
		return nil, cerr
	}
	project.ThresholdPermyriad = permyriad
	result.Traces = append(result.Traces, trace)

	if raw, ok := root["channel_sequence"]; ok {
		var sequence string
		if uerr := json.Unmarshal(raw, &sequence); uerr != nil {
			return nil, errs.Schema("$.channel_sequence", "expected string: %v", uerr)
		}
		project.ChannelOrder = parseChannelOrder(sequence)
	}
	if len(project.ChannelOrder) == 0 {
		project.ChannelOrder = defaultChannelOrder()
		result.UpgradeRecords = append(result.UpgradeRecords, model.ConversionRecord{
			Field: "$.channel_sequence", FromVersion: model.SchemaV1, ToVersion: model.SchemaV3,
			Operation: "fill_default", OldValue: nil, NewValue: project.ChannelOrder,
			Source: "schema.v1.defaults.channel_sequence=RGBA",
		})
	}
	project.ChannelLayout = model.LayoutFlat

	project.Quant = model.QuantSpec{Rounding: model.RoundFloor}
	if step := optionalInt(root, "quant_step", 0); step > 0 {
		project.Quant.Step = step
	} else if qRaw, ok := root["quantization"]; ok {
		qObj, qerr := asObject(qRaw, "$.quantization")
		if qerr != nil {
			return nil, qerr
		}
		project.Quant.Step = optionalInt(qObj, "step", 0)
		project.Quant.Rounding = optionalString(qObj, "rounding")
		project.Quant.ExtentDivisible = optionalBool(qObj, "extent_divisible", false)
	}
	if project.Quant.Step == 0 {
		project.Quant.Step = DefaultV1QuantStep
		result.UpgradeRecords = append(result.UpgradeRecords, model.ConversionRecord{
			Field: "$.quant_step", FromVersion: model.SchemaV1, ToVersion: model.SchemaV3,
			Operation: "fill_default", OldValue: nil, NewValue: DefaultV1QuantStep,
			Source: "schema.v1.compatibility_defaults.quant_step=6",
		})
	}
	if project.Quant.Rounding == "" {
		project.Quant.Rounding = model.RoundFloor
	}
	if raw, ok := root["extent_divisible"]; ok {
		_ = json.Unmarshal(raw, &project.Quant.ExtentDivisible)
	}
	project.ParamVersion = optionalString(root, "param_version")
	if project.ParamVersion == "" {
		project.ParamVersion = DefaultParamVersion
		result.UpgradeRecords = append(result.UpgradeRecords, model.ConversionRecord{
			Field: "$.param_version", Operation: "fill_default", OldValue: nil,
			NewValue: DefaultParamVersion, Source: "schema.defaults.param_version",
		})
	}

	rulesRaw := root["rules"]
	project.Rules, terr = rulesFromRaw(rulesRaw, "$.rules", project.ThresholdPermyriad)
	if terr != nil {
		return nil, terr
	}

	imagesRaw, ierr := field(root, "images", "$")
	if ierr != nil {
		return nil, ierr
	}
	images, ierr := getSlice(imagesRaw, "$.images")
	if ierr != nil {
		return nil, ierr
	}
	for i, raw := range images {
		obj, oerr := asObject(raw, "$.images["+itoa(i)+"]")
		if oerr != nil {
			return nil, oerr
		}
		id, iderr := getString(obj, "id", "$.images["+itoa(i)+"]")
		if iderr != nil {
			return nil, iderr
		}
		width, werr := getInt(obj, "width", "$.images["+itoa(i)+"]")
		if werr != nil {
			return nil, werr
		}
		height, herr := getInt(obj, "height", "$.images["+itoa(i)+"]")
		if herr != nil {
			return nil, herr
		}
		path := optionalString(obj, "path")
		channels := parseChannelOrder(optionalString(obj, "channels"))
		if len(channels) == 0 {
			channels = project.ChannelOrder
		}
		pixelsRaw, perr := field(obj, "pixels_flat", "$.images["+itoa(i)+"]")
		if perr != nil {
			pixelsRaw, perr = field(obj, "pixels", "$.images["+itoa(i)+"]")
			if perr != nil {
				return nil, perr
			}
		}
		flat, perr := intSlice(pixelsRaw, "$.images["+itoa(i)+"].pixels")
		if perr != nil {
			return nil, perr
		}
		img, traces, berr := buildRaster(id, path, width, height, channels, model.LayoutFlat, flat, nil, "$.images["+itoa(i)+"]")
		if berr != nil {
			return nil, berr
		}
		project.Images = append(project.Images, img)
		result.Traces = append(result.Traces, traces...)
	}

	if raw, ok := root["panels"]; ok {
		project.Panels, terr = parsePanels(raw, "$.panels")
		if terr != nil {
			return nil, terr
		}
	} else if raw, ok := root["windows"]; ok {
		project.Panels, terr = parsePanels(raw, "$.windows")
		if terr != nil {
			return nil, terr
		}
	}
	project.Dirty = parseDirty(root["dirty_blocks"])
	if project.Dirty == nil {
		project.Dirty = parseDirty(root["dirty"])
	}
	project.Truth, terr = parseTruth(root["expected_blocks"])
	if terr != nil {
		return nil, terr
	}
	if len(root["truth"]) > 0 {
		project.Truth, terr = parseTruth(root["truth"])
		if terr != nil {
			return nil, terr
		}
	}

	result.Project = project
	return result, nil
}
