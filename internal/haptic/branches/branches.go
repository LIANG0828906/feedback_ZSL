package branches

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"

	"github.com/liang/hapticfeedback/internal/haptic/errs"
	"github.com/liang/hapticfeedback/internal/haptic/model"
)

const (
	SemStrength = "strength"
	SemDuration = "duration_samples"
	SemTypes    = "type_sequence"
	SemChannels = "channel_sequence"
)

type TimeRule struct {
	Basis        string `json:"basis"`
	UnitMS       int64  `json:"unit_ms"`
	Rounding     string `json:"rounding"`
	DisplayField string `json:"display_field"`
}

type FlagRule struct {
	Encoding string `json:"encoding"`
	TruthBit int64  `json:"truth_bit,omitempty"`
	ValidBit int64  `json:"valid_bit,omitempty"`
	Polarity string `json:"polarity,omitempty"`
}

type Rule struct {
	Branch int      `json:"branch"`
	Name   string   `json:"name"`
	P      string   `json:"p_semantic"`
	Q      string   `json:"q_semantic"`
	Seq    string   `json:"seq_semantic"`
	Aux    string   `json:"aux_semantic"`
	Time   TimeRule `json:"time"`
	Flag   FlagRule `json:"flag"`
}

func Rules() map[int]Rule {
	return map[int]Rule{
		1: {
			Branch: 1, Name: "mainline-tick",
			P: SemStrength, Q: SemDuration, Seq: SemTypes, Aux: SemChannels,
			Time: TimeRule{Basis: "tick_10ms", UnitMS: 10, Rounding: "floor", DisplayField: "at"},
			Flag: FlagRule{Encoding: "direct_0_1", Polarity: "one_is_true"},
		},
		2: {
			Branch: 2, Name: "release-b-ms-swapped",
			P: SemDuration, Q: SemStrength, Seq: SemChannels, Aux: SemTypes,
			Time: TimeRule{Basis: "millisecond", UnitMS: 1, Rounding: "floor", DisplayField: "at"},
			Flag: FlagRule{Encoding: "validated_bits", TruthBit: 0x1, ValidBit: 0x4},
		},
		3: {
			Branch: 3, Name: "release-c-fast-tick-inverted",
			P: SemStrength, Q: SemDuration, Seq: SemTypes, Aux: SemChannels,
			Time: TimeRule{Basis: "fast_tick_5ms", UnitMS: 5, Rounding: "floor", DisplayField: "at"},
			Flag: FlagRule{Encoding: "inverted_0_1", Polarity: "zero_is_true"},
		},
	}
}

func ChannelCodeToName() map[int]string {
	return map[int]string{10: "ch_left", 20: "ch_right"}
}

func ChannelNameToCode() map[string]int {
	return map[string]int{"ch_left": 10, "ch_right": 20}
}

type rawFile struct {
	Branch   *int              `json:"branch"`
	StreamID *string           `json:"stream_id"`
	Seed     *int64            `json:"seed"`
	Frames   []json.RawMessage `json:"frames"`
}

type rawFrame struct {
	FrameNo         *int            `json:"frame_no"`
	EventID         *string         `json:"event_id"`
	ArrivalOrder    *int            `json:"arrival_order"`
	At              *int64          `json:"at"`
	P               *int64          `json:"p"`
	Q               *int64          `json:"q"`
	Seq             json.RawMessage `json:"seq"`
	Aux             json.RawMessage `json:"aux"`
	Flag            json.RawMessage `json:"flag"`
	Priority        *int            `json:"priority"`
	NoInterrupt     *bool           `json:"no_interrupt"`
	Conditional     *bool           `json:"conditional"`
	Template        *string         `json:"template"`
	JitterAmplitude *int            `json:"jitter_amplitude"`
	MergeKey        *string         `json:"merge_key"`
}

func decodeJSON(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		wrapped := errs.Wrap(errs.EDecode, "JSON decode failed", err)
		wrapped.Path = "$"
		return wrapped
	}
	return nil
}

func Load(data []byte) (*model.Stream, error) {
	var top rawFile
	if err := decodeJSON(data, &top); err != nil {
		return nil, err
	}
	if top.Branch == nil {
		return nil, errs.New(errs.EVersion, "missing branch number").WithPath("$.branch")
	}
	rule, ok := Rules()[*top.Branch]
	if !ok {
		return nil, (&errs.Error{Code: errs.EVersion, Message: "unknown release branch number", Path: "$.branch",
			Details: map[string]any{"branch": *top.Branch, "supported": []int{1, 2, 3}}})
	}
	if top.StreamID == nil || *top.StreamID == "" {
		return nil, errs.New(errs.EVersion, "missing required stream_id").WithPath("$.stream_id")
	}
	if top.Seed == nil {
		return nil, errs.New(errs.EVersion, "missing required seed").WithPath("$.seed")
	}
	if top.Frames == nil {
		return nil, errs.New(errs.EVersion, "missing required frames").WithPath("$.frames")
	}

	stream := &model.Stream{
		StreamID: *top.StreamID, Seed: *top.Seed, InternalUnit: "device_sample",
		SamplePeriodMS: 10, SourceBranch: rule.Branch,
	}
	for i, raw := range top.Frames {
		ev, err := loadFrame(raw, i, rule, stream.SamplePeriodMS)
		if err != nil {
			return nil, err
		}
		stream.Events = append(stream.Events, ev)
	}
	return stream, nil
}

func loadFrame(raw json.RawMessage, index int, rule Rule, samplePeriodMS int) (model.Event, error) {
	prefix := fmt.Sprintf("$.frames[%d]", index)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return model.Event{}, decodeError(prefix, 0, "", "frame object", err)
	}

	frameNo, err := requiredInt(obj, "frame_no", prefix)
	if err != nil {
		return model.Event{}, err
	}
	channelHint := contextChannel(obj, rule)

	eventID, err := requiredString(obj, "event_id", prefix, frameNo, channelHint)
	if err != nil {
		return model.Event{}, err
	}
	order, err := requiredIntContext(obj, "arrival_order", prefix, frameNo, channelHint)
	if err != nil {
		return model.Event{}, err
	}
	at, err := requiredInt64Context(obj, "at", prefix, frameNo, channelHint)
	if err != nil {
		return model.Event{}, err
	}
	pVal, err := requiredInt64Context(obj, "p", prefix, frameNo, channelHint)
	if err != nil {
		return model.Event{}, err
	}
	qVal, err := requiredInt64Context(obj, "q", prefix, frameNo, channelHint)
	if err != nil {
		return model.Event{}, err
	}
	seqRaw, err := requiredRaw(obj, "seq", prefix)
	if err != nil {
		return model.Event{}, err
	}
	auxRaw, err := requiredRaw(obj, "aux", prefix)
	if err != nil {
		return model.Event{}, err
	}
	seq, err := decodeIntList(seqRaw, prefix+".seq", frameNo, channelHint)
	if err != nil {
		return model.Event{}, err
	}
	aux, err := decodeIntList(auxRaw, prefix+".aux", frameNo, channelHint)
	if err != nil {
		return model.Event{}, err
	}
	priority, err := requiredIntContext(obj, "priority", prefix, frameNo, channelHint)
	if err != nil {
		return model.Event{}, err
	}
	conditional, err := requiredBoolContext(obj, "conditional", prefix, frameNo, channelHint)
	if err != nil {
		return model.Event{}, err
	}
	template, err := requiredStringContext(obj, "template", prefix, frameNo, channelHint)
	if err != nil {
		return model.Event{}, err
	}
	jitter, err := requiredIntContext(obj, "jitter_amplitude", prefix, frameNo, channelHint)
	if err != nil {
		return model.Event{}, err
	}

	noInterrupt := false
	if rawVal, ok := obj["no_interrupt"]; ok && string(rawVal) != "null" {
		if err := json.Unmarshal(rawVal, &noInterrupt); err != nil {
			return model.Event{}, decodeError(prefix+".no_interrupt", frameNo, channelHint, "boolean", err)
		}
	}
	mergeKey := ""
	if rawVal, ok := obj["merge_key"]; ok && string(rawVal) != "null" {
		if err := json.Unmarshal(rawVal, &mergeKey); err != nil {
			return model.Event{}, decodeError(prefix+".merge_key", frameNo, channelHint, "string", err)
		}
	}

	flag, flagRaw, err := decodeFlag(obj, prefix, frameNo, channelHint, rule)
	if err != nil {
		return model.Event{}, err
	}
	startSample, err := convertTime(at, rule.Time, samplePeriodMS, prefix+".at", frameNo, channelHint)
	if err != nil {
		return model.Event{}, err
	}

	values := map[string]any{"p": pVal, "q": qVal}
	strength := int(pVal)
	duration := int(qVal)
	if rule.P == SemDuration {
		duration = int(pVal)
		strength = int(qVal)
	}

	typeSeqRaw, channelCodes := seq, aux
	if rule.Seq == SemChannels {
		channelCodes, typeSeqRaw = seq, aux
	}
	channelNames := codesToChannels(channelCodes)
	channel := channelHint
	if len(channelNames) > 0 {
		channel = channelNames[0]
	}
	if channel == "" {
		err := errs.New(errs.EVersion, "channel allocation sequence is empty").
			WithPath(prefix + "." + listFieldFor(rule, SemChannels))
		return model.Event{}, errs.WithContext(err, frameNo, "")
	}

	ev := model.Event{
		FrameNo: frameNo, ArrivalOrder: order,
		EventID: eventID, Channel: channel, TypeCode: firstOrZero(typeSeqRaw),
		TypeSequence: append([]int(nil), typeSeqRaw...), ChannelSequence: channelNames,
		Strength: strength, DurationSamples: duration, StartSample: startSample,
		Priority: priority, NoInterrupt: noInterrupt, Conditional: conditional, Flag: flag,
		Template: template, JitterAmplitude: jitter, MergeKey: mergeKey,
		SourceBranch: rule.Branch, TimeSourceValue: at, TimeSourceBasis: rule.Time.Basis,
		FlagSourceValue: flagRaw,
	}
	ev.Traces = []model.Trace{
		trace(frameNo, rule, SemStrength, prefix+"."+intFieldFor(rule, SemStrength), values[intFieldFor(rule, SemStrength)], strength, "branch integer semantic table"),
		trace(frameNo, rule, SemDuration, prefix+"."+intFieldFor(rule, SemDuration), values[intFieldFor(rule, SemDuration)], duration, "branch integer semantic table"),
		trace(frameNo, rule, SemTypes, prefix+"."+listFieldFor(rule, SemTypes), typeSeqRaw, typeSeqRaw, "branch list semantic table"),
		trace(frameNo, rule, SemChannels, prefix+"."+listFieldFor(rule, SemChannels), channelCodes, channelNames, "channel code table 10=left,20=right"),
		trace(frameNo, rule, "start_sample", prefix+".at", at, startSample, rule.Time.Basis+" -> sample; rounding="+rule.Time.Rounding),
		trace(frameNo, rule, "flag", prefix+".flag", flagRaw, flag, rule.Flag.Encoding),
	}
	return ev, nil
}

func decodeError(path string, frame int, channel, expected string, cause error) error {
	e := errs.Wrap(errs.EDecode, "invalid field, expected "+expected, cause)
	e.Path = path
	e.FrameNo = frame
	e.Channel = channel
	return e
}

func requiredRaw(obj map[string]json.RawMessage, name, prefix string) (json.RawMessage, error) {
	v, ok := obj[name]
	if !ok || string(v) == "null" {
		return nil, errs.New(errs.EVersion, "missing required field").WithPath(prefix + "." + name)
	}
	return v, nil
}

func requiredInt(obj map[string]json.RawMessage, name, prefix string) (int, error) {
	raw, err := requiredRaw(obj, name, prefix)
	if err != nil {
		return 0, err
	}
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, decodeError(prefix+"."+name, 0, "", "integer", err)
	}
	return v, nil
}

func requiredIntContext(obj map[string]json.RawMessage, name, prefix string, frame int, channel string) (int, error) {
	v, err := requiredInt(obj, name, prefix)
	if err != nil {
		err = errs.WithContext(err, frame, channel)
	}
	return v, err
}

func requiredInt64Context(obj map[string]json.RawMessage, name, prefix string, frame int, channel string) (int64, error) {
	raw, err := requiredRaw(obj, name, prefix)
	if err != nil {
		return 0, err
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, decodeError(prefix+"."+name, frame, channel, "int64", err)
	}
	return v, nil
}

func requiredString(obj map[string]json.RawMessage, name, prefix string, frame int, channel string) (string, error) {
	raw, err := requiredRaw(obj, name, prefix)
	if err != nil {
		return "", err
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", decodeError(prefix+"."+name, frame, channel, "string", err)
	}
	if v == "" {
		e := errs.New(errs.EVersion, "required field is empty").WithPath(prefix + "." + name)
		return "", errs.WithContext(e, frame, channel)
	}
	return v, nil
}

func requiredStringContext(obj map[string]json.RawMessage, name, prefix string, frame int, channel string) (string, error) {
	return requiredString(obj, name, prefix, frame, channel)
}

func requiredBoolContext(obj map[string]json.RawMessage, name, prefix string, frame int, channel string) (bool, error) {
	raw, err := requiredRaw(obj, name, prefix)
	if err != nil {
		return false, err
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, decodeError(prefix+"."+name, frame, channel, "boolean", err)
	}
	return v, nil
}

func decodeIntList(raw json.RawMessage, path string, frame int, channel string) ([]int, error) {
	var v []int
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, decodeError(path, frame, channel, "integer array", err)
	}
	if len(v) == 0 {
		e := errs.New(errs.EVersion, "required list is empty").WithPath(path)
		return nil, errs.WithContext(e, frame, channel)
	}
	return v, nil
}

func contextChannel(obj map[string]json.RawMessage, rule Rule) string {
	field := "aux"
	if rule.Aux == SemChannels {
		field = "aux"
	} else if rule.Seq == SemChannels {
		field = "seq"
	}
	raw, ok := obj[field]
	if !ok {
		return ""
	}
	codes, err := decodeIntList(raw, "$.context."+field, 0, "")
	if err != nil {
		return ""
	}
	names := codesToChannels(codes)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func codesToChannels(codes []int) []string {
	table := ChannelCodeToName()
	out := make([]string, 0, len(codes))
	for _, code := range codes {
		if name, ok := table[code]; ok {
			out = append(out, name)
		} else {
			out = append(out, fmt.Sprintf("ch_code_%d", code))
		}
	}
	return out
}

func channelsToCodes(names []string) []int {
	table := ChannelNameToCode()
	out := make([]int, 0, len(names))
	for _, name := range names {
		if code, ok := table[name]; ok {
			out = append(out, code)
		} else {
			out = append(out, -1)
		}
	}
	return out
}

func firstOrZero(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	return xs[0]
}

func intFieldFor(rule Rule, semantic string) string {
	if rule.P == semantic {
		return "p"
	}
	return "q"
}

func listFieldFor(rule Rule, semantic string) string {
	if rule.Seq == semantic {
		return "seq"
	}
	return "aux"
}

func trace(frame int, rule Rule, internal, path string, raw, converted any, mapping string) model.Trace {
	return model.Trace{
		FrameNo: frame, Branch: rule.Branch, BranchName: rule.Name, InternalName: internal,
		SourcePath: path, SourceField: path[stringsLastDot(path)+1:], Rule: mapping,
		RawValue: raw, ConvertedValue: converted,
	}
}

func stringsLastDot(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

func decodeFlag(obj map[string]json.RawMessage, prefix string, frame int, channel string, rule Rule) (bool, any, error) {
	rawMsg, ok := obj["flag"]
	path := prefix + ".flag"
	if !ok || string(rawMsg) == "null" {
		e := errs.New(errs.EFlag, "conditional flag is missing")
		e.Path = path
		e.FrameNo = frame
		e.Channel = channel
		e.Details = map[string]any{"required_encoding": rule.Flag.Encoding}
		return false, nil, e
	}
	var rawInt int64
	if err := json.Unmarshal(rawMsg, &rawInt); err != nil {
		e := errs.Wrap(errs.EFlag, "flag must be branch-encoded integer", err)
		e.Path = path
		e.FrameNo = frame
		e.Channel = channel
		return false, string(rawMsg), e
	}
	switch rule.Flag.Encoding {
	case "direct_0_1":
		if rawInt == 0 {
			return false, rawInt, nil
		}
		if rawInt == 1 {
			return true, rawInt, nil
		}
	case "inverted_0_1":
		if rawInt == 0 {
			return true, rawInt, nil
		}
		if rawInt == 1 {
			return false, rawInt, nil
		}
	case "validated_bits":
		if rawInt&rule.Flag.ValidBit == 0 {
			e := errs.New(errs.EFlag, "conditional flag contradicts branch validity bit")
			e.Path = path
			e.FrameNo = frame
			e.Channel = channel
			e.Field = "flag"
			e.Details = map[string]any{"raw": rawInt, "valid_bit": rule.Flag.ValidBit, "truth_bit": rule.Flag.TruthBit}
			return false, rawInt, e
		}
		return rawInt&rule.Flag.TruthBit != 0, rawInt, nil
	}
	e := errs.New(errs.EFlag, "invalid branch flag encoding")
	e.Path = path
	e.FrameNo = frame
	e.Channel = channel
	e.Details = map[string]any{"raw": rawInt, "encoding": rule.Flag.Encoding}
	return false, rawInt, e
}

func convertTime(value int64, rule TimeRule, samplePeriodMS int, path string, frame int, channel string) (int, error) {
	if value < 0 {
		e := errs.New(errs.EOver, "negative time basis is not representable")
		e.Path = path
		e.FrameNo = frame
		e.Channel = channel
		e.Field = "at"
		e.Details = map[string]any{"source_value": value, "unit_ms": rule.UnitMS, "side_a": value, "side_b": "non-negative sample axis"}
		return 0, e
	}
	if value > (math.MaxInt64-1)/rule.UnitMS {
		e := errs.New(errs.EOver, "time conversion overflows int64 milliseconds")
		e.Path = path
		e.FrameNo = frame
		e.Channel = channel
		e.Field = "at"
		e.Details = map[string]any{
			"source_value": value, "source_unit_ms": rule.UnitMS,
			"side_a":     fmt.Sprintf("%d %s", value, rule.Basis),
			"side_b":     fmt.Sprintf("multiply by %d ms/tick", rule.UnitMS),
			"left_side":  map[string]any{"semantic": "source_time", "basis": rule.Basis, "value": value, "unit_ms": rule.UnitMS},
			"right_side": map[string]any{"semantic": "milliseconds_before_internal_division", "operation": "checked_multiply", "limit_int64": math.MaxInt64},
			"limit":      math.MaxInt64,
		}
		return 0, e
	}
	ms := value * rule.UnitMS
	var samples int64
	switch rule.Rounding {
	case "ceil":
		samples = (ms + int64(samplePeriodMS) - 1) / int64(samplePeriodMS)
	default:
		samples = ms / int64(samplePeriodMS)
	}
	if samples > math.MaxInt32 {
		e := errs.New(errs.EOver, "converted sample index exceeds internal int range")
		e.Path = path
		e.FrameNo = frame
		e.Channel = channel
		e.Field = "at"
		e.Details = map[string]any{"source_value": value, "milliseconds": ms, "sample_period_ms": samplePeriodMS}
		return 0, e
	}
	return int(samples), nil
}

type ExportResult struct {
	Branch int              `json:"branch"`
	Data   json.RawMessage  `json:"data"`
	Losses []model.LossItem `json:"losses"`
}

func Export(stream model.Stream, branch int) (*ExportResult, error) {
	rule, ok := Rules()[branch]
	if !ok {
		return nil, errs.New(errs.EVersion, "unknown export branch").WithPath("branch")
	}
	losses := []model.LossItem{}
	frames := make([]map[string]any, 0, len(stream.Events))
	for _, ev := range stream.Events {
		pVal, qVal := ev.Strength, ev.DurationSamples
		if rule.P == SemDuration {
			pVal, qVal = ev.DurationSamples, ev.Strength
		}
		seqVal, auxVal := ev.TypeSequence, channelNamesToCodes(ev.ChannelSequence)
		if rule.Seq == SemChannels {
			seqVal, auxVal = channelNamesToCodes(ev.ChannelSequence), ev.TypeSequence
		}
		at, err := inverseTime(ev.StartSample, rule.Time, stream.SamplePeriodMS)
		if err != nil {
			return nil, err
		}
		flag, err := encodeFlag(ev.Flag, rule.Flag)
		if err != nil {
			return nil, err
		}
		if ev.DesignerNote != "" {
			losses = append(losses, model.LossItem{
				Code: string(errs.ELossy), Branch: branch, Path: fmt.Sprintf("$.frames[%d].designer_note", ev.FrameNo-1),
				InternalName: "designer_note", Original: ev.DesignerNote,
				Disposition: "omit", Detail: "branch wire schema has no designer_note field",
			})
		}
		if ev.Priority > 9 {
			losses = append(losses, model.LossItem{
				Code: string(errs.ELossy), Branch: branch, Path: fmt.Sprintf("$.frames[%d].priority", ev.FrameNo-1),
				InternalName: "priority", Original: ev.Priority,
				Disposition: "clamp_to_9", Detail: "branch priority upper bound",
			})
			ev.Priority = 9
		}
		frame := map[string]any{
			"frame_no": ev.FrameNo, "event_id": ev.EventID, "arrival_order": ev.ArrivalOrder,
			"at": at, "p": pVal, "q": qVal, "seq": seqVal, "aux": auxVal, "flag": flag,
			"priority": ev.Priority, "no_interrupt": ev.NoInterrupt, "conditional": ev.Conditional,
			"template": ev.Template, "jitter_amplitude": ev.JitterAmplitude,
		}
		if ev.MergeKey != "" {
			frame["merge_key"] = ev.MergeKey
		}
		frames = append(frames, frame)
	}
	file := map[string]any{
		"schema": "haptic.branch-stream/v1", "branch": branch, "stream_id": stream.StreamID,
		"seed": stream.Seed, "frames": frames,
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return nil, err
	}
	return &ExportResult{Branch: branch, Data: data, Losses: losses}, nil
}

func channelNamesToCodes(names []string) []int {
	return channelsToCodes(names)
}

func inverseTime(startSample int, rule TimeRule, samplePeriodMS int) (int64, error) {
	ms := int64(startSample) * int64(samplePeriodMS)
	if ms%rule.UnitMS != 0 {
		e := errs.New(errs.ELossy, "time conversion cannot preserve exact source tick")
		e.Field = "at"
		e.Details = map[string]any{"start_sample": startSample, "milliseconds": ms, "unit_ms": rule.UnitMS}
		return 0, e
	}
	return ms / rule.UnitMS, nil
}

func encodeFlag(flag bool, rule FlagRule) (int64, error) {
	switch rule.Encoding {
	case "direct_0_1":
		if flag {
			return 1, nil
		}
		return 0, nil
	case "inverted_0_1":
		if flag {
			return 0, nil
		}
		return 1, nil
	case "validated_bits":
		v := rule.ValidBit
		if flag {
			v |= rule.TruthBit
		}
		return v, nil
	}
	return 0, errs.New(errs.EFlag, "unknown flag encoding")
}

type Diff struct {
	Equal        bool   `json:"equal"`
	SemanticName string `json:"semantic_name,omitempty"`
	EventIndex   int    `json:"event_index,omitempty"`
	FrameNo      int    `json:"frame_no,omitempty"`
	Left         any    `json:"left,omitempty"`
	Right        any    `json:"right,omitempty"`
	Message      string `json:"message,omitempty"`
}

func CompareStreams(left, right *model.Stream) Diff {
	if left == nil || right == nil {
		return Diff{Equal: false, SemanticName: "stream", Message: "nil stream"}
	}
	if left.StreamID != right.StreamID {
		return Diff{false, "stream_id", -1, 0, left.StreamID, right.StreamID, "stream_id differs"}
	}
	if left.Seed != right.Seed {
		return Diff{false, "seed", -1, 0, left.Seed, right.Seed, "seed differs"}
	}
	if len(left.Events) != len(right.Events) {
		return Diff{false, "events.length", -1, 0, len(left.Events), len(right.Events), "event count differs"}
	}
	names := []string{
		"FrameNo", "ArrivalOrder", "EventID", "Channel", "TypeCode", "TypeSequence", "ChannelSequence",
		"Strength", "DurationSamples", "StartSample", "Priority", "NoInterrupt", "Conditional", "Flag",
		"Template", "JitterAmplitude", "MergeKey", "DesignerNote",
	}
	for i := range left.Events {
		a, b := left.Events[i], right.Events[i]
		va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
		for _, name := range names {
			x, y := va.FieldByName(name).Interface(), vb.FieldByName(name).Interface()
			if !reflect.DeepEqual(x, y) {
				return Diff{
					Equal: false, SemanticName: snake(name), EventIndex: i, FrameNo: a.FrameNo,
					Left: x, Right: y, Message: "first inequivalent internal semantic field",
				}
			}
		}
	}
	return Diff{Equal: true, Message: "semantic streams equal"}
}

func snake(s string) string {
	var out []byte
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				out = append(out, '_')
			}
			out = append(out, byte(r-'A'+'a'))
		} else {
			out = append(out, byte(r))
		}
	}
	return string(out)
}
