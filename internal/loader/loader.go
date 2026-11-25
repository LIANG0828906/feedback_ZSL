package loader

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"audiobus/domain"
	"audiobus/errs"
	"audiobus/internal/convert"
)

const FrameSamples = int64(48)

type branchProfile struct {
	timeBasis     string
	durationBasis string
	ldRole        string
	listRole      string
}

var profiles = map[string]branchProfile{
	"alpha": {timeBasis: "samples", durationBasis: "samples", ldRole: "duration", listRole: "types"},
	"beta":  {timeBasis: "frames", durationBasis: "samples", ldRole: "gain", listRole: "routes"},
	"gamma": {timeBasis: "ms", durationBasis: "frames", ldRole: "duration", listRole: "types"},
}

func LoadSampleSet(dir string) (*domain.SampleSet, error) {
	manifestBytes, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, errs.Wrap(errs.EDecode, "read manifest", err)
	}
	var manifest domain.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, errs.Wrap(errs.EDecode, "decode manifest.json", err)
	}
	if err := ValidateManifest(&manifest); err != nil {
		return nil, err
	}

	set := &domain.SampleSet{Manifest: manifest, Branches: map[string]domain.BranchFile{}}
	for _, branch := range []string{"alpha", "beta", "gamma"} {
		path := filepath.Join(dir, "branch_"+branch+".json")
		rawBytes, err := os.ReadFile(path)
		if err != nil {
			return nil, errs.Wrap(errs.EDecode, "read "+path, err)
		}
		bf, events, err := LoadBranchBytes(branch, rawBytes, &manifest)
		if err != nil {
			return nil, err
		}
		set.Branches[branch] = *bf
		set.Events = append(set.Events, events...)
	}
	return set, nil
}

func LoadBranchBytes(expected string, data []byte, m *domain.Manifest) (*domain.BranchFile, []domain.Event, error) {
	var bf domain.BranchFile
	if err := json.Unmarshal(data, &bf); err != nil {
		return nil, nil, &errs.Error{Code: errs.EDecode, Branch: expected, Path: "$", Reason: "invalid JSON", Err: err}
	}
	if bf.Branch == "" {
		return nil, nil, &errs.Error{Code: errs.ESchema, Branch: expected, Path: "$.branch", Reason: "missing branch"}
	}
	if _, ok := profiles[bf.Branch]; !ok {
		return nil, nil, &errs.Error{Code: errs.EVersion, Branch: bf.Branch, Path: "$.branch", Reason: "unknown release branch"}
	}
	if expected != "" && bf.Branch != expected {
		return nil, nil, &errs.Error{Code: errs.EVersion, Branch: bf.Branch, Path: "$.branch", Reason: "branch file/name mismatch"}
	}
	if m != nil && bf.BranchVersion != m.BranchVersion {
		return nil, nil, &errs.Error{Code: errs.EVersion, Branch: bf.Branch, Path: "$.branchVersion", Reason: "branch version does not match capability set"}
	}
	if err := validateHeader(&bf, m); err != nil {
		return nil, nil, err
	}
	events, err := MapBranch(&bf, m)
	if err != nil {
		return nil, nil, err
	}
	if err := ValidateTiming(events); err != nil {
		return nil, nil, err
	}
	if m != nil {
		if err := ValidateRanges(events, m); err != nil {
			return nil, nil, err
		}
	}
	return &bf, events, nil
}

func ValidateManifest(m *domain.Manifest) error {
	if m.Channels <= 0 {
		return capError("$.channels", "must be positive")
	}
	if m.SampleRate <= 0 {
		return capError("$.sampleRate", "must be positive")
	}
	if m.TotalSamples <= 0 {
		return capError("$.totalSamples", "must be positive")
	}
	if m.WindowSamples <= 0 || m.TotalSamples%m.WindowSamples != 0 {
		return capError("$.windowSamples", "must divide totalSamples")
	}
	if m.QuantUnit <= 0 {
		return capError("$.quantUnit", "must be positive")
	}
	if m.MaxSamplesPerPack <= 0 {
		return capError("$.maxSamplesPerPack", "must be positive")
	}
	if m.TimingWindowMs <= 0 {
		return capError("$.timingWindowMs", "must be positive")
	}
	if m.FlagMask <= 0 {
		return capError("$.flagMask", "must be positive")
	}
	if m.TemplateCount <= 0 {
		return capError("$.templateCount", "must be positive")
	}
	if len(m.Templates) != m.Channels {
		return capError("$.templates", fmt.Sprintf("length %d != channels %d", len(m.Templates), m.Channels))
	}
	for ch, templates := range m.Templates {
		if len(templates) != m.TemplateCount {
			return capError(fmt.Sprintf("$.templates[%d]", ch), "template count mismatch")
		}
		for i, t := range templates {
			if t.AttackPermille < 0 || t.ReleasePermille < 0 || t.AttackPermille > 1024 || t.ReleasePermille > 1024 {
				return capError(fmt.Sprintf("$.templates[%d][%d]", ch, i), "envelope coefficient out of declared fixed-point range")
			}
		}
	}
	switch m.TailRule {
	case "pad", "padded", domain.FullDisposition, domain.TruncDisposition, "truncate":
	default:
		return capError("$.tailRule", "must be full, pad, or truncate")
	}
	return nil
}

func capError(path, reason string) error {
	return &errs.Error{Code: errs.ECap, Path: path, Reason: reason}
}

func validateHeader(bf *domain.BranchFile, m *domain.Manifest) error {
	if bf.Header.Channels <= 0 {
		return &errs.Error{Code: errs.ESchema, Branch: bf.Branch, Path: "$.header.channels", Reason: "missing or invalid channels"}
	}
	if bf.Header.TotalSamples <= 0 {
		return &errs.Error{Code: errs.ESchema, Branch: bf.Branch, Path: "$.header.totalSamples", Reason: "missing totalSamples"}
	}
	if bf.Header.SampleRate <= 0 {
		return &errs.Error{Code: errs.ESchema, Branch: bf.Branch, Path: "$.header.sampleRate", Reason: "missing sampleRate"}
	}
	if m != nil {
		if bf.Header.Channels != m.Channels {
			return capError("$.header.channels", "target channel count contradicts capability list")
		}
		if bf.Header.TotalSamples != m.TotalSamples {
			return capError("$.header.totalSamples", "event range contradicts capability list")
		}
		if bf.Header.SampleRate != m.SampleRate {
			return capError("$.header.sampleRate", "sample rate contradicts capability list")
		}
	}
	if len(bf.Events) == 0 {
		return &errs.Error{Code: errs.ESchema, Branch: bf.Branch, Path: "$.events", Reason: "at least one event is required"}
	}
	return nil
}

func MapBranch(bf *domain.BranchFile, m *domain.Manifest) ([]domain.Event, error) {
	p := profiles[bf.Branch]
	out := make([]domain.Event, 0, len(bf.Events))
	for ordinal, raw := range bf.Events {
		ev, err := mapOne(bf.Branch, p, ordinal, raw, m)
		if err != nil {
			return nil, err
		}
		out = append(out, ev...)
	}
	return out, nil
}

func requireInt(v *int64, branch, path string) (int64, error) {
	if v == nil {
		return 0, &errs.Error{Code: errs.ESchema, Branch: branch, Path: path, Reason: "missing required integer field"}
	}
	return *v, nil
}

func mapOne(branch string, p branchProfile, ordinal int, raw domain.RawEvent, m *domain.Manifest) ([]domain.Event, error) {
	base := fmt.Sprintf("$.events[%d]", ordinal)
	if raw.ID == 0 {
		return nil, &errs.Error{Code: errs.ESchema, Branch: branch, Path: base + ".id", Reason: "missing event id"}
	}
	if raw.Ts == nil {
		return nil, &errs.Error{Code: errs.ESchema, Branch: branch, Path: base + ".ts", Reason: "missing timestamp"}
	}
	if raw.Ld == nil {
		return nil, &errs.Error{Code: errs.ESchema, Branch: branch, Path: base + ".ld", Reason: "missing ld field"}
	}
	if raw.Tg == nil {
		return nil, &errs.Error{Code: errs.ESchema, Branch: branch, Path: base + ".tg", Reason: "missing tg field"}
	}
	if raw.Pr == nil {
		return nil, &errs.Error{Code: errs.ESchema, Branch: branch, Path: base + ".pr", Reason: "missing priority"}
	}
	if raw.Ni == nil {
		return nil, &errs.Error{Code: errs.ESchema, Branch: branch, Path: base + ".ni", Reason: "missing no-interrupt flag"}
	}
	if raw.Cd == nil {
		return nil, &errs.Error{Code: errs.ESchema, Branch: branch, Path: base + ".cd", Reason: "missing conditional marker"}
	}
	if raw.Fl == nil {
		return nil, &errs.Error{Code: errs.ESchema, Branch: branch, Path: base + ".fl", Reason: "missing condition flag"}
	}
	if len(raw.Lst) == 0 {
		return nil, &errs.Error{Code: errs.ESchema, Branch: branch, Path: base + ".lst", Reason: "missing sequence"}
	}

	tsRaw := *raw.Ts
	ldRaw := *raw.Ld
	tgRaw := *raw.Tg
	var start int64
	var err *errs.Error
	switch p.timeBasis {
	case "samples":
		start = tsRaw
	case "frames":
		start, err = convert.FrameToSamples(tsRaw, FrameSamples)
	case "ms":
		start, err = convert.MsToSamples(tsRaw, m.SampleRate)
	}
	if err != nil {
		err.Branch = branch
		err.Path = base + ".ts"
		err.EventID = raw.ID
		err.RawValue = tsRaw
		return nil, err
	}

	var durationRaw int64
	var gain int64
	if p.ldRole == "duration" {
		durationRaw = ldRaw
		gain = tgRaw
	} else {
		durationRaw = tgRaw
		gain = ldRaw
	}
	var duration int64
	switch p.durationBasis {
	case "samples":
		duration = durationRaw
	case "frames":
		duration, err = convert.FrameToSamples(durationRaw, FrameSamples)
	}
	if err != nil {
		err.Branch = branch
		err.Path = base + ".ld/tg"
		err.EventID = raw.ID
		err.RawValue = durationRaw
		return nil, err
	}

	channels := []int{}
	typeSeq := []int{}
	if p.listRole == "routes" {
		for _, route := range raw.Lst {
			if route < 0 || route >= m.Channels {
				return nil, &errs.Error{Code: errs.ECap, Branch: branch, Path: base + ".lst", Reason: "route outside target channels", EventID: raw.ID}
			}
			channels = append(channels, route)
		}
	} else {
		typeSeq = append(typeSeq, raw.Lst...)
		if raw.Ch == nil {
			return nil, &errs.Error{Code: errs.ESchema, Branch: branch, Path: base + ".ch", Reason: "missing channel"}
		}
		if *raw.Ch < 0 || *raw.Ch >= m.Channels {
			return nil, &errs.Error{Code: errs.ECap, Branch: branch, Path: base + ".ch", Reason: "channel outside capability list", EventID: raw.ID}
		}
		channels = []int{*raw.Ch}
	}

	out := make([]domain.Event, 0, len(channels))
	for _, ch := range channels {
		seq := append([]int(nil), typeSeq...)
		if p.listRole == "routes" {
			seq = []int{ch % m.TemplateCount}
		} else if len(seq) == 0 {
			seq = []int{0}
		}
		e := domain.Event{
			ID: raw.ID, SourceBranch: branch, SourceOrdinal: ordinal,
			SourcePath: fmt.Sprintf("%s.events[%d]", branch, ordinal),
			Channel:    ch, Start: start, Duration: duration, Gain: gain,
			Priority: *raw.Pr, NoInterrupt: *raw.Ni != 0,
			Conditional: *raw.Cd != 0, Flag: *raw.Fl,
			FlagTrue:     (*raw.Fl & m.FlagMask) == m.FlagMask,
			TypeSequence: seq, RouteSequence: append([]int(nil), raw.Lst...),
			ArrivalOrder: ordinal, StartRaw: tsRaw, DurationRaw: durationRaw,
			GainRaw: gain, StartBasis: p.timeBasis, DurationBasis: p.durationBasis,
			EffectiveEnd: start + duration,
		}
		if p.listRole == "types" {
			e.RouteSequence = []int{ch}
		}
		out = append(out, e)
	}
	return out, nil
}

func ValidateTiming(events []domain.Event) error {
	byChannel := map[int][]domain.Event{}
	for _, e := range events {
		if e.Start < 0 {
			return orderError(e, "$.ts", "negative start")
		}
		if e.Duration <= 0 {
			return orderError(e, "$.ld/tg", "duration must be positive")
		}
		end, ok := convert.MulChecked(e.Duration, 1)
		if !ok {
			return &errs.Error{Code: errs.EOvf, EventID: e.ID, Branch: e.SourceBranch, RawValue: e.Duration}
		}
		end = e.Start + e.Duration
		if e.Start > int64(1<<62) || end > int64(1<<62) {
			return &errs.Error{Code: errs.EOvf, EventID: e.ID, Branch: e.SourceBranch, RawValue: e.Start}
		}
		byChannel[e.Channel] = append(byChannel[e.Channel], e)
	}
	for ch, list := range byChannel {
		sort.SliceStable(list, func(i, j int) bool { return list[i].SourceOrdinal < list[j].SourceOrdinal })
		var previous domain.Event
		have := false
		for _, e := range list {
			if have && e.Start < previous.Start {
				ae := orderError(e, "$.ts", "frame timing inversion in source arrival order")
				ae.Channel = ch
				ae.Frame = e.Start
				return ae
			}
			if have && e.Start+e.Duration < previous.Start {
				ae := orderError(e, "$.ld/tg", "event ends before previous start")
				ae.Channel = ch
				return ae
			}
			previous = e
			have = true
		}
	}
	return nil
}

func orderError(e domain.Event, path, reason string) *errs.Error {
	return &errs.Error{Code: errs.EOrder, Branch: e.SourceBranch, Path: path, Reason: reason, Channel: e.Channel, EventID: e.ID, Frame: e.Start}
}

func ValidateRanges(events []domain.Event, m *domain.Manifest) error {
	for _, e := range events {
		end := e.Start + e.Duration
		if e.Start < 0 || e.Start >= int64(m.TotalSamples) || end > int64(m.TotalSamples) {
			return &errs.Error{Code: errs.EOrder, Branch: e.SourceBranch, Path: e.SourcePath + ".ts/ld", Reason: "event start/end outside declared sample range", Channel: e.Channel, EventID: e.ID, Frame: e.Start}
		}
	}
	return nil
}

func CanonicalEvents(set *domain.SampleSet) []domain.Event {
	seen := map[int64]domain.Event{}
	for _, e := range set.Events {
		if e.SourceBranch == "alpha" {
			seen[e.ID] = e
		}
	}
	out := make([]domain.Event, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Channel != out[j].Channel {
			return out[i].Channel < out[j].Channel
		}
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return out[i].ID < out[j].ID
	})
	return out
}
