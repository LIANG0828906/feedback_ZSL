package formats

import (
	"encoding/json"
	"fmt"
	"strings"

	"lipsync/domain"
	"lipsync/units"
)

// Load detects the source protocol version and maps it to the canonical model.
func Load(data []byte) (*domain.Project, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, &domain.Error{
			Code:    domain.CodeSchema,
			Path:    "$",
			Message: "document is not a JSON object: " + err.Error(),
		}
	}
	version, versionPath, err := detectVersion(top)
	if err != nil {
		return nil, err
	}
	var project *domain.Project
	switch version {
	case "v1":
		project, err = loadV1(top)
	case "v2":
		project, err = loadV2(top)
	case "v3":
		project, err = loadV3(top)
	default:
		return nil, &domain.Error{
			Code:    domain.CodeSchema,
			Path:    versionPath,
			Message: fmt.Sprintf("unknown schema version %q", version),
		}
	}
	if err != nil {
		return nil, err
	}
	project.SchemaVersion = version
	project.SourceBytes = len(data)
	project.ContentHash = domain.ComputeContentHash(*project)
	for i := range project.Dialogues {
		project.Dialogues[i].ContentHash = domain.ComputeDialogueHash(project.Dialogues[i])
	}
	return project, nil
}

func detectVersion(top map[string]json.RawMessage) (string, string, error) {
	for _, key := range []string{"schema_version", "schemaVersion", "version"} {
		if raw, ok := top[key]; ok {
			var version string
			if err := json.Unmarshal(raw, &version); err != nil {
				return "", key, &domain.Error{
					Code:    domain.CodeSchema,
					Path:    key,
					Message: "schema version must be a string",
				}
			}
			switch version {
			case "v1", "v2", "v3":
				return version, key, nil
			default:
				return "", key, &domain.Error{
					Code:    domain.CodeSchema,
					Path:    key,
					Message: fmt.Sprintf("unknown schema version %q", version),
				}
			}
		}
	}
	return "", "schema_version", domain.NewError(domain.CodeSchema, "schema_version", "required schema version is missing")
}

type object map[string]json.RawMessage
type array []json.RawMessage

func asObject(raw json.RawMessage, path string) (object, error) {
	var obj object
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, &domain.Error{Code: domain.CodeSchema, Path: path, Message: "expected object"}
	}
	return obj, nil
}

func asArray(raw json.RawMessage, path string) (array, error) {
	var items array
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, &domain.Error{Code: domain.CodeSchema, Path: path, Message: "expected array"}
	}
	return items, nil
}

func require(obj object, key, path string) (json.RawMessage, error) {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return nil, &domain.Error{Code: domain.CodeSchema, Path: path, Message: "required field is missing"}
	}
	return raw, nil
}

func requiredString(obj object, key, path string) (string, error) {
	raw, err := require(obj, key, path)
	if err != nil {
		return "", err
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", &domain.Error{Code: domain.CodeSchema, Path: path, Message: "expected non-empty string"}
	}
	return value, nil
}

func optionalString(obj object, key, path, fallback string) (string, error) {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return fallback, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", &domain.Error{Code: domain.CodeSchema, Path: path, Message: "expected string"}
	}
	return value, nil
}

func requiredInt(obj object, key, path string) (int64, error) {
	raw, err := require(obj, key, path)
	if err != nil {
		return 0, err
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, &domain.Error{Code: domain.CodeSchema, Path: path, Message: "expected integer"}
	}
	return value, nil
}

func optionalInt(obj object, key, path string, fallback int64) (int64, bool, error) {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return fallback, false, nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, true, &domain.Error{Code: domain.CodeSchema, Path: path, Message: "expected integer"}
	}
	return value, true, nil
}

func requiredBool(obj object, key, path string) (bool, error) {
	raw, err := require(obj, key, path)
	if err != nil {
		return false, err
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, &domain.Error{Code: domain.CodeSchema, Path: path, Message: "expected boolean"}
	}
	return value, nil
}

func optionalBool(obj object, key, path string, fallback bool) (bool, bool, error) {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return fallback, false, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, true, &domain.Error{Code: domain.CodeSchema, Path: path, Message: "expected boolean"}
	}
	return value, true, nil
}

func requirePositive(value int64, path, channel string, allowZero bool) error {
	if value < 0 || (!allowZero && value == 0) {
		return &domain.Error{
			Code:      domain.CodeSchema,
			Path:      path,
			ChannelID: channel,
			Message:   "value must be positive",
		}
	}
	return nil
}

func parseRounding(obj object, path string) (units.Rounding, error) {
	value, err := optionalString(obj, "rounding", path, "floor")
	if err != nil {
		return "", err
	}
	return units.ParseRounding(value)
}

func parseFraming(top object, sampleRatePath, frameRatePath string) (int64, int64, error) {
	sampleRate, err := requiredInt(top, "sample_rate", sampleRatePath)
	if err != nil {
		if _, ok := top["framing"]; ok {
			return parseCamelFraming(top)
		}
		return 0, 0, err
	}
	frameRate, err := requiredInt(top, "frame_rate", frameRatePath)
	if err != nil {
		return 0, 0, err
	}
	if err := requirePositive(sampleRate, sampleRatePath, "", false); err != nil {
		return 0, 0, err
	}
	if err := requirePositive(frameRate, frameRatePath, "", false); err != nil {
		return 0, 0, err
	}
	return sampleRate, frameRate, nil
}

func parseCamelFraming(top object) (int64, int64, error) {
	raw, err := require(top, "framing", "framing")
	if err != nil {
		return 0, 0, err
	}
	obj, err := asObject(raw, "framing")
	if err != nil {
		return 0, 0, err
	}
	sampleRate, err := requiredInt(obj, "sampleRate", "framing.sampleRate")
	if err != nil {
		return 0, 0, err
	}
	frameRate, err := requiredInt(obj, "frameRate", "framing.frameRate")
	if err != nil {
		return 0, 0, err
	}
	if err := requirePositive(sampleRate, "framing.sampleRate", "", false); err != nil {
		return 0, 0, err
	}
	if err := requirePositive(frameRate, "framing.frameRate", "", false); err != nil {
		return 0, 0, err
	}
	return sampleRate, frameRate, nil
}

func parseChannelsV1(top object, sampleRate, frameRate int64, rounding units.Rounding) ([]domain.Channel, []domain.Truncation, error) {
	raw, err := require(top, "channels", "channels")
	if err != nil {
		return nil, nil, err
	}
	items, err := asArray(raw, "channels")
	if err != nil {
		return nil, nil, err
	}
	var channels []domain.Channel
	var truncations []domain.Truncation
	seen := map[string]bool{}
	for i, item := range items {
		path := fmt.Sprintf("channels[%d]", i)
		obj, err := asObject(item, path)
		if err != nil {
			return nil, nil, err
		}
		id, err := requiredString(obj, "channel_id", path+".channel_id")
		if err != nil {
			return nil, nil, err
		}
		if seen[id] {
			return nil, nil, &domain.Error{Code: domain.CodeSchema, Path: path + ".channel_id", ChannelID: id, Message: "duplicate channel id"}
		}
		seen[id] = true
		scene, err := optionalString(obj, "scene_id", path+".scene_id", "")
		if err != nil {
			return nil, nil, err
		}
		slots, err := requiredInt(obj, "slot_limit", path+".slot_limit")
		if err != nil {
			return nil, nil, withChannel(err, id)
		}
		if slots < 0 {
			return nil, nil, &domain.Error{Code: domain.CodeSchema, Path: path + ".slot_limit", ChannelID: id, Message: "channel slot limit cannot be negative"}
		}
		budget, err := requiredInt(obj, "frame_budget", path+".frame_budget")
		if err != nil {
			return nil, nil, withChannel(err, id)
		}
		if budget < 0 {
			return nil, nil, &domain.Error{Code: domain.CodeSchema, Path: path + ".frame_budget", ChannelID: id, Message: "frame budget cannot be negative"}
		}
		conversion, err := units.PermilleToBasisPoints(budget, rounding, path+".frame_budget")
		if err != nil {
			return nil, nil, err
		}
		units.RecordTruncation(&truncations, path+".frame_budget", domain.BudgetScalePermille, domain.BudgetScaleBasisPoint, conversion, id, "")
		channels = append(channels, domain.Channel{
			ID:                     id,
			Scene:                  scene,
			SlotLimit:              int(slots),
			FrameBudgetBasisPoints: conversion.Output,
			SourceBudgetValue:      budget,
			SourceBudgetScale:      domain.BudgetScalePermille,
			SourcePath:             path,
		})
	}
	if len(channels) == 0 {
		return nil, nil, domain.NewError(domain.CodeSchema, "channels", "at least one channel is required")
	}
	return channels, truncations, nil
}

func parseChannelsV2(top object) ([]domain.Channel, error) {
	raw, err := require(top, "scenes", "scenes")
	if err != nil {
		return nil, err
	}
	scenes, err := asArray(raw, "scenes")
	if err != nil {
		return nil, err
	}
	var channels []domain.Channel
	seen := map[string]bool{}
	for si, sceneRaw := range scenes {
		scenePath := fmt.Sprintf("scenes[%d]", si)
		scene, err := asObject(sceneRaw, scenePath)
		if err != nil {
			return nil, err
		}
		name, err := requiredString(scene, "sceneName", scenePath+".sceneName")
		if err != nil {
			return nil, err
		}
		channelRaw, err := require(scene, "channels", scenePath+".channels")
		if err != nil {
			return nil, err
		}
		items, err := asArray(channelRaw, scenePath+".channels")
		if err != nil {
			return nil, err
		}
		for ci, item := range items {
			path := fmt.Sprintf("%s.channels[%d]", scenePath, ci)
			obj, err := asObject(item, path)
			if err != nil {
				return nil, err
			}
			id, err := requiredString(obj, "id", path+".id")
			if err != nil {
				return nil, err
			}
			if seen[id] {
				return nil, &domain.Error{Code: domain.CodeSchema, Path: path + ".id", ChannelID: id, Message: "duplicate channel id"}
			}
			seen[id] = true
			slots, err := requiredInt(obj, "slots", path+".slots")
			if err != nil {
				return nil, withChannel(err, id)
			}
			if slots < 0 {
				return nil, &domain.Error{Code: domain.CodeSchema, Path: path + ".slots", ChannelID: id, Message: "channel slot limit cannot be negative"}
			}
			budget, err := requiredInt(obj, "frameBudget", path+".frameBudget")
			if err != nil {
				return nil, withChannel(err, id)
			}
			if budget < 0 {
				return nil, &domain.Error{Code: domain.CodeSchema, Path: path + ".frameBudget", ChannelID: id, Message: "frame budget cannot be negative"}
			}
			channels = append(channels, domain.Channel{
				ID:                     id,
				Scene:                  name,
				SlotLimit:              int(slots),
				FrameBudgetBasisPoints: budget,
				SourceBudgetValue:      budget,
				SourceBudgetScale:      domain.BudgetScaleBasisPoint,
				SourcePath:             path,
			})
		}
	}
	if len(channels) == 0 {
		return nil, domain.NewError(domain.CodeSchema, "scenes", "at least one channel is required")
	}
	return channels, nil
}

func parseChannelsV3(top object) ([]domain.Channel, error) {
	raw, err := require(top, "scenes", "scenes")
	if err != nil {
		return nil, err
	}
	scenes, err := asArray(raw, "scenes")
	if err != nil {
		return nil, err
	}
	var channels []domain.Channel
	seen := map[string]bool{}
	for si, sceneRaw := range scenes {
		scenePath := fmt.Sprintf("scenes[%d]", si)
		scene, err := asObject(sceneRaw, scenePath)
		if err != nil {
			return nil, err
		}
		name, err := requiredString(scene, "scene_name", scenePath+".scene_name")
		if err != nil {
			return nil, err
		}
		channelRaw, err := require(scene, "channels", scenePath+".channels")
		if err != nil {
			return nil, err
		}
		items, err := asArray(channelRaw, scenePath+".channels")
		if err != nil {
			return nil, err
		}
		for ci, item := range items {
			path := fmt.Sprintf("%s.channels[%d]", scenePath, ci)
			obj, err := asObject(item, path)
			if err != nil {
				return nil, err
			}
			id, err := requiredString(obj, "channel_id", path+".channel_id")
			if err != nil {
				return nil, err
			}
			if seen[id] {
				return nil, &domain.Error{Code: domain.CodeSchema, Path: path + ".channel_id", ChannelID: id, Message: "duplicate channel id"}
			}
			seen[id] = true
			slots, err := requiredInt(obj, "slot_limit", path+".slot_limit")
			if err != nil {
				return nil, withChannel(err, id)
			}
			if slots < 0 {
				return nil, &domain.Error{Code: domain.CodeSchema, Path: path + ".slot_limit", ChannelID: id, Message: "channel slot limit cannot be negative"}
			}
			budget, err := requiredInt(obj, "frame_budget_basis_points", path+".frame_budget_basis_points")
			if err != nil {
				return nil, withChannel(err, id)
			}
			if budget < 0 {
				return nil, &domain.Error{Code: domain.CodeSchema, Path: path + ".frame_budget_basis_points", ChannelID: id, Message: "frame budget cannot be negative"}
			}
			channels = append(channels, domain.Channel{
				ID:                     id,
				Scene:                  name,
				SlotLimit:              int(slots),
				FrameBudgetBasisPoints: budget,
				SourceBudgetValue:      budget,
				SourceBudgetScale:      domain.BudgetScaleBasisPoint,
				SourcePath:             path,
			})
		}
	}
	if len(channels) == 0 {
		return nil, domain.NewError(domain.CodeSchema, "scenes", "at least one channel is required")
	}
	return channels, nil
}

func parseOrder(top object, channels []domain.Channel) ([]string, error) {
	raw, err := require(top, "arbitration_order", "arbitration_order")
	if err != nil {
		if _, ok := top["arbitrationOrder"]; ok {
			raw, err = require(top, "arbitrationOrder", "arbitrationOrder")
		}
		if err != nil {
			return nil, err
		}
	}
	path := "arbitration_order"
	if _, ok := top["arbitrationOrder"]; ok {
		path = "arbitrationOrder"
	}
	items, err := asArray(raw, path)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, channel := range channels {
		known[channel.ID] = true
	}
	seen := map[string]bool{}
	var order []string
	for i, item := range items {
		var id string
		if err := json.Unmarshal(item, &id); err != nil || id == "" {
			return nil, &domain.Error{Code: domain.CodeSchema, Path: fmt.Sprintf("%s[%d]", path, i), Message: "expected channel id"}
		}
		if !known[id] {
			return nil, &domain.Error{Code: domain.CodeSchema, Path: fmt.Sprintf("%s[%d]", path, i), ChannelID: id, Message: "arbitration order references unknown channel"}
		}
		if seen[id] {
			return nil, &domain.Error{Code: domain.CodeSchema, Path: fmt.Sprintf("%s[%d]", path, i), ChannelID: id, Message: "duplicate channel in arbitration order"}
		}
		seen[id] = true
		order = append(order, id)
	}
	if len(order) != len(channels) {
		return nil, &domain.Error{Code: domain.CodeSchema, Path: path, Message: "arbitration order must list every channel exactly once"}
	}
	return order, nil
}

func withChannel(err error, channel string) error {
	if domainErr, ok := err.(*domain.Error); ok {
		copy := *domainErr
		copy.ChannelID = channel
		return &copy
	}
	return err
}

func validateDialogueChannel(channelID string, channelSet map[string]bool, path string) error {
	if !channelSet[channelID] {
		return &domain.Error{Code: domain.CodeSchema, Path: path, ChannelID: channelID, Message: "dialogue references unknown channel"}
	}
	return nil
}

func validateRecovery(policy domain.RecoveryPolicy, path string) error {
	switch policy.Strategy {
	case domain.StrategyNextFrame, domain.StrategyNextWindow, domain.StrategyFixedDelay:
	default:
		return &domain.Error{Code: domain.CodeSchema, Path: path + ".strategy", Message: "unsupported recovery strategy"}
	}
	if policy.RecoveryOffsetFrame < 0 {
		return &domain.Error{Code: domain.CodeSchema, Path: path + ".recovery_offset_frames", Message: "recovery offset cannot be negative"}
	}
	if policy.MinContinuousFrames < 1 {
		return &domain.Error{Code: domain.CodeSchema, Path: path + ".min_continuous_frames", Message: "minimum continuous frames must be at least one"}
	}
	if policy.WindowFrames < 1 {
		return &domain.Error{Code: domain.CodeSchema, Path: path + ".window_frames", Message: "recovery window must be at least one frame"}
	}
	return nil
}
