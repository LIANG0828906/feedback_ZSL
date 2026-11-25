package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"lipsync/domain"
	"lipsync/quant"
	"lipsync/truth"
)

type Version string

const (
	V1 Version = "dialogue/v1"
	V2 Version = "dialogue/v2"
	V3 Version = "dialogue/v3"
)

func ParseVersion(s string) (Version, bool) {
	switch Version(s) {
	case V1, V2, V3:
		return Version(s), true
	default:
		return "", false
	}
}

func (v Version) Valid() bool {
	_, ok := ParseVersion(string(v))
	return ok
}

func LoadFile(path string) (*domain.Project, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read project: %w", err)
	}
	return Load(b)
}

// LoadBundle loads a project and its truth manifest. PCM files are checked for
// existence and their declared SHA-256 when present.
func LoadBundle(path string) (*domain.Project, *truth.Manifest, error) {
	p, err := LoadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if p.TruthFile == "" {
		return nil, nil, &domain.Error{
			Code: domain.CodeSchema, Path: "$.truth_file", Version: p.SourceVersion,
			Message: "required truth_file is missing",
		}
	}
	base := filepath.Dir(path)
	tpath := p.TruthFile
	if !filepath.IsAbs(tpath) {
		tpath = filepath.Join(base, tpath)
	}
	m, err := truth.LoadFile(tpath)
	if err != nil {
		return nil, nil, err
	}
	if m.ProjectID != p.ID {
		return nil, nil, &domain.Error{
			Code: domain.CodeSchema, Path: "$.project_id", Version: p.SourceVersion,
			Field: "project_id", Expected: p.ID, Actual: m.ProjectID,
			Message: "project and truth manifest ids differ",
		}
	}
	for _, d := range p.Dialogues {
		if d.PCM.File == "" {
			continue
		}
		audioPath := d.PCM.File
		if !filepath.IsAbs(audioPath) {
			audioPath = filepath.Join(base, audioPath)
		}
		data, err := os.ReadFile(audioPath)
		if err != nil {
			return nil, nil, &domain.Error{
				Code: domain.CodeSchema, Path: fmt.Sprintf("$.dialogues[%d].pcm.file", d.ShardIndex),
				Version: p.SourceVersion, DialogueID: d.ID, ShardIndex: d.ShardIndex,
				Message: "cannot read declared PCM file", Cause: err,
			}
		}
		if d.PCM.SHA256 != "" {
			sum := sha256.Sum256(data)
			if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, d.PCM.SHA256) {
				return nil, nil, &domain.Error{
					Code: domain.CodeSchema, Path: fmt.Sprintf("$.dialogues[%d].pcm.sha256", d.ShardIndex),
					Version: p.SourceVersion, DialogueID: d.ID, ShardIndex: d.ShardIndex,
					Expected: strings.ToLower(d.PCM.SHA256), Actual: got,
					Message: "PCM content hash mismatch",
				}
			}
		}
	}
	return p, m, nil
}

func Load(data []byte) (*domain.Project, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, &domain.Error{
			Code: domain.CodeSchema, Path: "$", Message: "invalid JSON", Cause: err,
		}
	}
	m, err := objectAt(root, "$", "")
	if err != nil {
		return nil, err
	}
	verRaw, err := required(m, "schema", "$", "")
	if err != nil {
		return nil, err
	}
	verStr, err := asString(verRaw, "$.schema", "")
	if err != nil {
		return nil, err
	}
	ver, ok := ParseVersion(verStr)
	if !ok {
		return nil, &domain.Error{
			Code: domain.CodeSchema, Path: "$.schema", Version: verStr, Field: "schema",
			Value: verStr, Message: "unknown dialogue schema version",
		}
	}
	switch ver {
	case V1:
		return loadV1(m)
	case V2:
		return loadV2(m)
	case V3:
		return loadV3(m)
	default:
		panic("unreachable")
	}
}

func loadV1(m map[string]any) (*domain.Project, error) {
	const ver = string(V1)
	projectID, err := reqString(m, "project_id", "$", ver)
	if err != nil {
		return nil, err
	}
	basis, err := reqString(m, "time_basis", "$", ver)
	if err != nil {
		return nil, err
	}
	if basis != "frame" {
		return nil, schemaValueError("$.time_basis", ver, basis, "v1 requires frame time basis")
	}
	scale, err := reqInt(m, "weight_scale", "$", ver)
	if err != nil {
		return nil, err
	}
	if scale != 1000 {
		return nil, schemaValueError("$.weight_scale", ver, strconv.FormatInt(scale, 10), "v1 weight scale must be 1000")
	}
	renderRaw, err := required(m, "render", "$", ver)
	if err != nil {
		return nil, err
	}
	renderMap, err := objectAt(renderRaw, "$.render", ver)
	if err != nil {
		return nil, err
	}
	render, err := parseRenderV1(renderMap, ver)
	if err != nil {
		return nil, err
	}
	catsRaw, err := required(m, "categories", "$", ver)
	if err != nil {
		return nil, err
	}
	categories, err := parseCategoriesFlatStrings(catsRaw, "$.categories", ver)
	if err != nil {
		return nil, err
	}
	dialoguesRaw, err := reqArray(m, "dialogues", "$", ver)
	if err != nil {
		return nil, err
	}
	p := &domain.Project{
		ID: projectID, SourceVersion: ver, TimeBasis: basis, WeightScale: scale,
		CategoryEncoding: "flat", Render: render,
	}
	p.Traces = append(p.Traces,
		domain.Trace{Path: "$.time_basis", SourceVersion: ver, From: basis, To: "frame", Rule: "frame identity"},
		domain.Trace{Path: "$.weight_scale", SourceVersion: ver, From: "1000", To: "10000", Rule: "per-mille -> permyriad"},
		domain.Trace{Path: "$.categories", SourceVersion: ver, From: "flat string table", To: "canonical category", Rule: "code lookup"},
	)
	for i, raw := range dialoguesRaw {
		path := fmt.Sprintf("$.dialogues[%d]", i)
		dm, err := objectAt(raw, path, ver)
		if err != nil {
			return nil, err
		}
		d, traces, defaults, err := parseDialogueV1(dm, path, i, categories, render, scale)
		if err != nil {
			return nil, err
		}
		p.Dialogues = append(p.Dialogues, d)
		p.Traces = append(p.Traces, traces...)
		p.Defaults = append(p.Defaults, defaults...)
	}
	if tf, ok, err := optString(m, "truth_file", "$", ver); err != nil {
		return nil, err
	} else if ok {
		p.TruthFile = tf
	}
	sort.Slice(p.Dialogues, func(i, j int) bool { return p.Dialogues[i].ShardIndex < p.Dialogues[j].ShardIndex })
	return p, nil
}

func loadV2(m map[string]any) (*domain.Project, error) {
	const ver = string(V2)
	projectID, err := reqString(m, "project_id", "$", ver)
	if err != nil {
		return nil, err
	}
	basis, err := reqString(m, "time_basis", "$", ver)
	if err != nil {
		return nil, err
	}
	if basis != "millisecond" {
		return nil, schemaValueError("$.time_basis", ver, basis, "v2 requires millisecond time basis")
	}
	scale, err := reqInt(m, "duration_weight_scale", "$", ver)
	if err != nil {
		return nil, err
	}
	if scale != 10000 {
		return nil, schemaValueError("$.duration_weight_scale", ver, strconv.FormatInt(scale, 10), "v2 duration scale must be 10000")
	}
	renderRaw, err := required(m, "frame_policy", "$", ver)
	if err != nil {
		return nil, err
	}
	renderMap, err := objectAt(renderRaw, "$.frame_policy", ver)
	if err != nil {
		return nil, err
	}
	render, err := parseRenderV2(renderMap, ver)
	if err != nil {
		return nil, err
	}
	catsRaw, err := required(m, "categories", "$", ver)
	if err != nil {
		return nil, err
	}
	categories, err := parseCategoriesNested(catsRaw, "$.categories", ver)
	if err != nil {
		return nil, err
	}
	dialoguesRaw, err := reqArray(m, "dialogues", "$", ver)
	if err != nil {
		return nil, err
	}
	p := &domain.Project{
		ID: projectID, SourceVersion: ver, TimeBasis: basis, WeightScale: scale,
		CategoryEncoding: "flat", Render: render,
	}
	p.Traces = append(p.Traces,
		domain.Trace{Path: "$.time_basis", SourceVersion: ver, From: basis, To: "frame", Rule: "millisecond -> frame quantization"},
		domain.Trace{Path: "$.duration_weight_scale", SourceVersion: ver, From: "10000", To: "10000", Rule: "permyriad identity"},
		domain.Trace{Path: "$.categories", SourceVersion: ver, From: "flat nested table", To: "canonical category", Rule: "code lookup"},
	)
	for i, raw := range dialoguesRaw {
		path := fmt.Sprintf("$.dialogues[%d]", i)
		dm, err := objectAt(raw, path, ver)
		if err != nil {
			return nil, err
		}
		d, traces, defaults, err := parseDialogueV2(dm, path, i, categories, render, scale)
		if err != nil {
			return nil, err
		}
		p.Dialogues = append(p.Dialogues, d)
		p.Traces = append(p.Traces, traces...)
		p.Defaults = append(p.Defaults, defaults...)
	}
	if tf, ok, err := optString(m, "truth_file", "$", ver); err != nil {
		return nil, err
	} else if ok {
		p.TruthFile = tf
	}
	sort.Slice(p.Dialogues, func(i, j int) bool { return p.Dialogues[i].ShardIndex < p.Dialogues[j].ShardIndex })
	return p, nil
}

func loadV3(m map[string]any) (*domain.Project, error) {
	const ver = string(V3)
	projectID, err := reqString(m, "project_id", "$", ver)
	if err != nil {
		return nil, err
	}
	basis, err := reqString(m, "time_basis", "$", ver)
	if err != nil {
		return nil, err
	}
	if basis != "frame" {
		return nil, schemaValueError("$.time_basis", ver, basis, "v3 requires frame time basis")
	}
	scale, err := reqInt(m, "duration_weight_scale", "$", ver)
	if err != nil {
		return nil, err
	}
	if scale != 10000 {
		return nil, schemaValueError("$.duration_weight_scale", ver, strconv.FormatInt(scale, 10), "v3 duration scale must be 10000")
	}
	renderRaw, err := required(m, "render", "$", ver)
	if err != nil {
		return nil, err
	}
	renderMap, err := objectAt(renderRaw, "$.render", ver)
	if err != nil {
		return nil, err
	}
	render, err := parseRenderV3(renderMap, ver)
	if err != nil {
		return nil, err
	}
	groupsRaw, err := required(m, "category_groups", "$", ver)
	if err != nil {
		return nil, err
	}
	categories, groups, err := parseCategoriesGrouped(groupsRaw, "$.category_groups", ver)
	if err != nil {
		return nil, err
	}
	dialoguesRaw, err := reqArray(m, "dialogues", "$", ver)
	if err != nil {
		return nil, err
	}
	p := &domain.Project{
		ID: projectID, SourceVersion: ver, TimeBasis: basis, WeightScale: scale,
		CategoryEncoding: "grouped", CategoryGroups: groups, Render: render,
	}
	p.Traces = append(p.Traces,
		domain.Trace{Path: "$.time_basis", SourceVersion: ver, From: basis, To: "frame", Rule: "frame identity"},
		domain.Trace{Path: "$.duration_weight_scale", SourceVersion: ver, From: "10000", To: "10000", Rule: "permyriad identity"},
		domain.Trace{Path: "$.category_groups", SourceVersion: ver, From: "grouped table", To: "canonical category", Rule: "flatten group membership on load"},
	)
	for i, raw := range dialoguesRaw {
		path := fmt.Sprintf("$.dialogues[%d]", i)
		dm, err := objectAt(raw, path, ver)
		if err != nil {
			return nil, err
		}
		d, traces, defaults, err := parseDialogueV3(dm, path, i, categories, render, scale)
		if err != nil {
			return nil, err
		}
		p.Dialogues = append(p.Dialogues, d)
		p.Traces = append(p.Traces, traces...)
		p.Defaults = append(p.Defaults, defaults...)
	}
	if tf, ok, err := optString(m, "truth_file", "$", ver); err != nil {
		return nil, err
	} else if ok {
		p.TruthFile = tf
	}
	sort.Slice(p.Dialogues, func(i, j int) bool { return p.Dialogues[i].ShardIndex < p.Dialogues[j].ShardIndex })
	return p, nil
}

func parseDialogueV1(m map[string]any, path string, index int, cats map[string]domain.Category,
	render domain.RenderConfig, scale int64) (domain.Dialogue, []domain.Trace, []domain.DefaultFill, error) {
	const ver = string(V1)
	id, err := reqString(m, "id", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	shard, err := reqInt(m, "shard", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	speaker, err := reqString(m, "speaker", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	channel, err := reqInt(m, "channel", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	pcmRaw, err := required(m, "pcm", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	pcmMap, err := objectAt(pcmRaw, path+".pcm", ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	pcm, err := parsePCMV1(pcmMap, path+".pcm", ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	phonesRaw, err := reqArray(m, "phonemes", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	d := domain.Dialogue{ID: id, ShardIndex: int(shard), Speaker: speaker, Channel: int(channel), PCM: pcm}
	var traces []domain.Trace
	var defaults []domain.DefaultFill
	for i, raw := range phonesRaw {
		pp := fmt.Sprintf("%s.phonemes[%d]", path, i)
		pm, err := objectAt(raw, pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		phID, err := reqString(pm, "id", pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		start, err := reqInt(pm, "start_frame", pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		end, err := reqInt(pm, "end_frame", pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		sampleStart, sampleEnd, sampleTraces, err := parseSampleInterval(
			pm, pp, ver, pcm.SampleRate, render, start, end, "start_sample", "end_sample")
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		catCode, err := reqString(pm, "category", pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		cat, ok := cats[catCode]
		if !ok || !cat.Valid() {
			return domain.Dialogue{}, nil, nil, &domain.Error{
				Code: domain.CodeClass, Path: pp + ".category", Version: ver,
				Field: "category", DialogueID: id, ShardIndex: int(shard), PhonemeID: phID,
				Value: catCode, Message: "category code is not declared",
			}
		}
		weight, err := reqInt(pm, "weight_milli", pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		wc, err := quant.WeightToPermyriad(weight, scale, render.Rounding, pp+".weight_milli")
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		ph := domain.Phoneme{
			ID: phID, StartFrame: start, EndFrame: end, Category: cat, CategoryCode: catCode,
			WeightPermyriad: wc.Value, SourceBasis: "frame",
			SourceStart: start, SourceEnd: end, SourceWeight: weight, SourceWeightScale: scale,
			Affinity: 1, AffinityPresent: false,
			SampleStart: sampleStart, SampleEnd: sampleEnd,
		}
		traces = append(traces, sampleTraces...)
		traces = append(traces,
			conversionTrace(pp+".start_frame", ver, "start_frame", "start_frame", "identity", quant.Conversion{}),
			conversionTrace(pp+".end_frame", ver, "end_frame", "end_frame", "identity", quant.Conversion{}),
			conversionTrace(pp+".weight_milli", ver, strconv.FormatInt(weight, 10), strconv.FormatInt(wc.Value, 10), "per-mille -> permyriad", wc),
			domain.Trace{Path: pp + ".category", SourceVersion: ver, From: catCode, To: string(cat), Rule: "category lookup"},
		)
		def := domain.DefaultFill{
			Path: pp + ".affinity", OldValue: "null", DefaultValue: "1",
			DefaultSource: "schema/v1 declared default",
		}
		defaults = append(defaults, def)
		traces = append(traces, domain.Trace{
			Path: pp + ".affinity", SourceVersion: ver, From: "null", To: "1",
			Rule: "default fill", DefaultSource: def.DefaultSource,
		})
		d.Phonemes = append(d.Phonemes, ph)
	}
	return d, traces, defaults, nil
}

func parseDialogueV2(m map[string]any, path string, index int, cats map[string]domain.Category,
	render domain.RenderConfig, scale int64) (domain.Dialogue, []domain.Trace, []domain.DefaultFill, error) {
	const ver = string(V2)
	id, err := reqString(m, "dialogue_id", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	shard, err := reqInt(m, "shard", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	speaker, err := reqString(m, "speaker", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	channel, err := reqInt(m, "channel", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	pcmRaw, err := required(m, "audio", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	pcmMap, err := objectAt(pcmRaw, path+".audio", ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	pcm, err := parsePCMV2(pcmMap, path+".audio", ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	phonesRaw, err := reqArray(m, "phonemes", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	d := domain.Dialogue{ID: id, ShardIndex: int(shard), Speaker: speaker, Channel: int(channel), PCM: pcm}
	var traces []domain.Trace
	var defaults []domain.DefaultFill
	for i, raw := range phonesRaw {
		pp := fmt.Sprintf("%s.phonemes[%d]", path, i)
		pm, err := objectAt(raw, pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		phID, err := reqString(pm, "phoneme_id", pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		startMS, err := reqInt(pm, "start_ms", pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		endMS, err := reqInt(pm, "end_ms", pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		catCode, err := reqString(pm, "category_code", pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		cat, ok := cats[catCode]
		if !ok || !cat.Valid() {
			return domain.Dialogue{}, nil, nil, &domain.Error{
				Code: domain.CodeClass, Path: pp + ".category_code", Version: ver,
				Field: "category_code", DialogueID: id, ShardIndex: int(shard), PhonemeID: phID,
				Value: catCode, Message: "category code is not declared",
			}
		}
		weight, err := reqInt(pm, "weight_bp", pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		wc, err := quant.WeightToPermyriad(weight, scale, render.Rounding, pp+".weight_bp")
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		sc, err := quant.MillisToFrame(startMS, render.TargetFPS, render.Rounding, pp+".start_ms")
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		ec, err := quant.MillisToFrame(endMS, render.TargetFPS, render.Rounding, pp+".end_ms")
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		sampleStart, sampleEnd, sampleTraces, err := parseSampleInterval(
			pm, pp, ver, pcm.SampleRate, render, sc.Value, ec.Value, "start_sample", "end_sample")
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		affinity := 1.0
		affinityPresent := false
		if rawAff, ok, err := optNumber(pm, "transition_affinity", pp, ver); err != nil {
			return domain.Dialogue{}, nil, nil, err
		} else if ok {
			affinityPresent = true
			affinity, err = asFloat(rawAff, pp+".transition_affinity", ver)
			if err != nil {
				return domain.Dialogue{}, nil, nil, err
			}
		} else {
			def := domain.DefaultFill{
				Path: pp + ".transition_affinity", OldValue: "null", DefaultValue: "1",
				DefaultSource: "schema/v2 optional transition_affinity default",
			}
			defaults = append(defaults, def)
			traces = append(traces, domain.Trace{
				Path: pp + ".transition_affinity", SourceVersion: ver, From: "null", To: "1",
				Rule: "default fill", DefaultSource: def.DefaultSource,
			})
		}
		ph := domain.Phoneme{
			ID: phID, StartFrame: sc.Value, EndFrame: ec.Value, Category: cat, CategoryCode: catCode,
			WeightPermyriad: wc.Value, Affinity: affinity, AffinityPresent: affinityPresent,
			SourceBasis: "millisecond", SourceStart: startMS, SourceEnd: endMS,
			SourceWeight: weight, SourceWeightScale: scale,
			SampleStart: sampleStart, SampleEnd: sampleEnd,
		}
		traces = append(traces, sampleTraces...)
		traces = append(traces,
			conversionTrace(pp+".start_ms", ver, strconv.FormatInt(startMS, 10), strconv.FormatInt(sc.Value, 10), "millisecond -> frame", sc),
			conversionTrace(pp+".end_ms", ver, strconv.FormatInt(endMS, 10), strconv.FormatInt(ec.Value, 10), "millisecond -> frame", ec),
			conversionTrace(pp+".weight_bp", ver, strconv.FormatInt(weight, 10), strconv.FormatInt(wc.Value, 10), "permyriad -> permyriad", wc),
			domain.Trace{Path: pp + ".category_code", SourceVersion: ver, From: catCode, To: string(cat), Rule: "category lookup"},
		)
		d.Phonemes = append(d.Phonemes, ph)
	}
	return d, traces, defaults, nil
}

func parseDialogueV3(m map[string]any, path string, index int, cats map[string]domain.Category,
	render domain.RenderConfig, scale int64) (domain.Dialogue, []domain.Trace, []domain.DefaultFill, error) {
	const ver = string(V3)
	id, err := reqString(m, "id", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	shard, err := reqInt(m, "shard_index", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	speaker, err := reqString(m, "speaker", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	channel, err := reqInt(m, "channel", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	pcmRaw, err := required(m, "pcm_meta", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	pcmMap, err := objectAt(pcmRaw, path+".pcm_meta", ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	pcm, err := parsePCMV3(pcmMap, path+".pcm_meta", ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	phonesRaw, err := reqArray(m, "phonemes", path, ver)
	if err != nil {
		return domain.Dialogue{}, nil, nil, err
	}
	d := domain.Dialogue{ID: id, ShardIndex: int(shard), Speaker: speaker, Channel: int(channel), PCM: pcm}
	var traces []domain.Trace
	var defaults []domain.DefaultFill
	for i, raw := range phonesRaw {
		pp := fmt.Sprintf("%s.phonemes[%d]", path, i)
		pm, err := objectAt(raw, pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		phID, err := reqString(pm, "id", pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		start, err := reqInt(pm, "from_frame", pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		end, err := reqInt(pm, "to_frame", pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		sampleStart, sampleEnd, sampleTraces, err := parseSampleInterval(
			pm, pp, ver, pcm.SampleRate, render, start, end, "sample_start", "sample_end")
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		catCode, err := reqString(pm, "category", pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		cat, ok := cats[catCode]
		if !ok || !cat.Valid() {
			return domain.Dialogue{}, nil, nil, &domain.Error{
				Code: domain.CodeClass, Path: pp + ".category", Version: ver,
				Field: "category", DialogueID: id, ShardIndex: int(shard), PhonemeID: phID,
				Value: catCode, Message: "category code is not declared",
			}
		}
		weight, err := reqInt(pm, "duration_weight", pp, ver)
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		wc, err := quant.WeightToPermyriad(weight, scale, render.Rounding, pp+".duration_weight")
		if err != nil {
			return domain.Dialogue{}, nil, nil, err
		}
		affinity := 1.0
		affinityPresent := false
		if rawAff, ok, err := optNumber(pm, "affinity", pp, ver); err != nil {
			return domain.Dialogue{}, nil, nil, err
		} else if ok {
			affinityPresent = true
			affinity, err = asFloat(rawAff, pp+".affinity", ver)
			if err != nil {
				return domain.Dialogue{}, nil, nil, err
			}
		} else {
			def := domain.DefaultFill{
				Path: pp + ".affinity", OldValue: "null", DefaultValue: "1",
				DefaultSource: "schema/v3 optional affinity default",
			}
			defaults = append(defaults, def)
			traces = append(traces, domain.Trace{
				Path: pp + ".affinity", SourceVersion: ver, From: "null", To: "1",
				Rule: "default fill", DefaultSource: def.DefaultSource,
			})
		}
		ph := domain.Phoneme{
			ID: phID, StartFrame: start, EndFrame: end, Category: cat, CategoryCode: catCode,
			WeightPermyriad: wc.Value, Affinity: affinity, AffinityPresent: affinityPresent,
			SourceBasis: "frame", SourceStart: start, SourceEnd: end,
			SourceWeight: weight, SourceWeightScale: scale,
			SampleStart: sampleStart, SampleEnd: sampleEnd,
		}
		traces = append(traces, sampleTraces...)
		traces = append(traces,
			conversionTrace(pp+".from_frame", ver, strconv.FormatInt(start, 10), strconv.FormatInt(start, 10), "frame identity", quant.Conversion{}),
			conversionTrace(pp+".to_frame", ver, strconv.FormatInt(end, 10), strconv.FormatInt(end, 10), "frame identity", quant.Conversion{}),
			conversionTrace(pp+".duration_weight", ver, strconv.FormatInt(weight, 10), strconv.FormatInt(wc.Value, 10), "permyriad -> permyriad", wc),
			domain.Trace{Path: pp + ".category", SourceVersion: ver, From: catCode, To: string(cat), Rule: "category lookup"},
		)
		d.Phonemes = append(d.Phonemes, ph)
	}
	return d, traces, defaults, nil
}

func conversionTrace(path, ver, from, to, rule string, c quant.Conversion) domain.Trace {
	return domain.Trace{
		Path: path, SourceVersion: ver, From: from, To: to, Rule: rule,
		Truncated: c.Truncated, Remainder: c.Remainder, Divisor: c.Divisor,
	}
}

func parseRenderV1(m map[string]any, ver string) (domain.RenderConfig, error) {
	fps, err := reqInt(m, "fps", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	round, err := reqString(m, "rounding", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	budget, err := reqInt(m, "frame_budget", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	slots, err := reqInt(m, "channel_slots", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	minVisible, err := reqInt(m, "min_visible_frames", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	maxPhones, err := reqInt(m, "max_phonemes_per_package", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	affRaw, err := required(m, "affinity_threshold_permille", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	aff, err := asFloat(affRaw, "$.render.affinity_threshold_permille", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	maxDelay, err := reqInt(m, "max_delay_frames", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	spf, err := reqInt(m, "samples_per_frame", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	r := domain.RenderConfig{
		TargetFPS: fps, Rounding: domain.Rounding(round), FrameBudget: int(budget),
		ChannelSlots: int(slots), MinVisibleFrames: minVisible,
		MaxPhonemesPerPackage: int(maxPhones), AffinityThreshold: aff / 1000,
		MaxDelayFrames: maxDelay, SamplesPerFrame: spf,
	}
	if !r.Rounding.Valid() {
		return domain.RenderConfig{}, schemaValueError("$.render.rounding", ver, round, "invalid rounding mode")
	}
	return r, nil
}

func parseRenderV2(m map[string]any, ver string) (domain.RenderConfig, error) {
	fps, err := reqInt(m, "target_fps", "$.frame_policy", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	round, err := reqString(m, "sample_rounding", "$.frame_policy", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	budget, err := reqInt(m, "frame_budget", "$.frame_policy", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	slots, err := reqInt(m, "channel_slots", "$.frame_policy", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	minVisible, err := reqInt(m, "min_visible_frames", "$.frame_policy", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	maxPhones, err := reqInt(m, "max_phonemes_per_package", "$.frame_policy", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	affRaw, err := required(m, "affinity_threshold", "$.frame_policy", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	aff, err := asFloat(affRaw, "$.frame_policy.affinity_threshold", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	maxDelay, err := reqInt(m, "max_delay_frames", "$.frame_policy", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	spf, err := reqInt(m, "samples_per_frame", "$.frame_policy", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	r := domain.RenderConfig{
		TargetFPS: fps, Rounding: domain.Rounding(round), FrameBudget: int(budget),
		ChannelSlots: int(slots), MinVisibleFrames: minVisible,
		MaxPhonemesPerPackage: int(maxPhones), AffinityThreshold: aff,
		MaxDelayFrames: maxDelay, SamplesPerFrame: spf,
	}
	if !r.Rounding.Valid() {
		return domain.RenderConfig{}, schemaValueError("$.frame_policy.sample_rounding", ver, round, "invalid rounding mode")
	}
	return r, nil
}

func parseRenderV3(m map[string]any, ver string) (domain.RenderConfig, error) {
	fps, err := reqInt(m, "target_fps", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	round, err := reqString(m, "quantize", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	budget, err := reqInt(m, "budget_per_frame", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	slots, err := reqInt(m, "channel_slots", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	minVisible, err := reqInt(m, "min_visible", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	maxPhones, err := reqInt(m, "max_phones_per_pack", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	affRaw, err := required(m, "affinity_floor_permille", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	aff, err := asFloat(affRaw, "$.render.affinity_floor_permille", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	maxDelay, err := reqInt(m, "max_delay_frames", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	spf, err := reqInt(m, "samples_per_frame", "$.render", ver)
	if err != nil {
		return domain.RenderConfig{}, err
	}
	r := domain.RenderConfig{
		TargetFPS: fps, Rounding: domain.Rounding(round), FrameBudget: int(budget),
		ChannelSlots: int(slots), MinVisibleFrames: minVisible,
		MaxPhonemesPerPackage: int(maxPhones), AffinityThreshold: aff / 1000,
		MaxDelayFrames: maxDelay, SamplesPerFrame: spf,
	}
	if !r.Rounding.Valid() {
		return domain.RenderConfig{}, schemaValueError("$.render.quantize", ver, round, "invalid rounding mode")
	}
	return r, nil
}

func parsePCMV1(m map[string]any, path, ver string) (domain.PCMInfo, error) {
	file, err := reqString(m, "file", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	enc, err := reqString(m, "encoding", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	rate, err := reqInt(m, "sample_rate", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	ch, err := reqInt(m, "channels", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	samples, err := reqInt(m, "samples", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	sha, err := reqString(m, "sha256", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	return domain.PCMInfo{File: file, Encoding: enc, SampleRate: rate, Channels: int(ch), Samples: samples, SHA256: sha}, nil
}

func parsePCMV2(m map[string]any, path, ver string) (domain.PCMInfo, error) {
	file, err := reqString(m, "file", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	enc, err := reqString(m, "codec", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	rate, err := reqInt(m, "sample_rate", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	ch, err := reqInt(m, "channels", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	samples, err := reqInt(m, "sample_count", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	sha, err := reqString(m, "content_sha256", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	return domain.PCMInfo{File: file, Encoding: enc, SampleRate: rate, Channels: int(ch), Samples: samples, SHA256: sha}, nil
}

func parsePCMV3(m map[string]any, path, ver string) (domain.PCMInfo, error) {
	file, err := reqString(m, "path", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	enc, err := reqString(m, "encoding", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	rate, err := reqInt(m, "sample_rate", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	ch, err := reqInt(m, "channels", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	samples, err := reqInt(m, "sample_count", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	sha, err := reqString(m, "sha", path, ver)
	if err != nil {
		return domain.PCMInfo{}, err
	}
	return domain.PCMInfo{File: file, Encoding: enc, SampleRate: rate, Channels: int(ch), Samples: samples, SHA256: sha}, nil
}

func parseCategoriesFlatStrings(raw any, path, ver string) (map[string]domain.Category, error) {
	m, err := objectAt(raw, path, ver)
	if err != nil {
		return nil, err
	}
	out := make(map[string]domain.Category, len(m))
	for code, v := range m {
		s, err := asString(v, path+"."+code, ver)
		if err != nil {
			return nil, err
		}
		cat := domain.Category(s)
		if !cat.Valid() {
			return nil, &domain.Error{
				Code: domain.CodeClass, Path: path + "." + code, Version: ver,
				Field: "category", Value: s, Message: "unknown category kind",
			}
		}
		out[code] = cat
	}
	if len(out) == 0 {
		return nil, schemaValueError(path, ver, "{}", "category table must not be empty")
	}
	return out, nil
}

func parseCategoriesNested(raw any, path, ver string) (map[string]domain.Category, error) {
	m, err := objectAt(raw, path, ver)
	if err != nil {
		return nil, err
	}
	out := make(map[string]domain.Category, len(m))
	for code, v := range m {
		obj, err := objectAt(v, path+"."+code, ver)
		if err != nil {
			return nil, err
		}
		kind, err := reqString(obj, "kind", path+"."+code, ver)
		if err != nil {
			return nil, err
		}
		cat := domain.Category(kind)
		if !cat.Valid() {
			return nil, &domain.Error{
				Code: domain.CodeClass, Path: path + "." + code + ".kind", Version: ver,
				Field: "kind", Value: kind, Message: "unknown category kind",
			}
		}
		out[code] = cat
	}
	if len(out) == 0 {
		return nil, schemaValueError(path, ver, "{}", "category table must not be empty")
	}
	return out, nil
}

func parseCategoriesGrouped(raw any, path, ver string) (map[string]domain.Category, []domain.CategoryGroup, error) {
	m, err := objectAt(raw, path, ver)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]domain.Category)
	var groupsOut []domain.CategoryGroup
	groups := make([]string, 0, len(m))
	for g := range m {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, group := range groups {
		items, err := asArray(m[group], path+"."+group, ver)
		if err != nil {
			return nil, nil, err
		}
		var codes []string
		for i, rawItem := range items {
			ip := fmt.Sprintf("%s.%s[%d]", path, group, i)
			item, err := objectAt(rawItem, ip, ver)
			if err != nil {
				return nil, nil, err
			}
			code, err := reqString(item, "code", ip, ver)
			if err != nil {
				return nil, nil, err
			}
			if _, exists := out[code]; exists {
				return nil, nil, schemaValueError(ip+".code", ver, code, "duplicate category code across groups")
			}
			kind, err := reqString(item, "kind", ip, ver)
			if err != nil {
				return nil, nil, err
			}
			cat := domain.Category(kind)
			if !cat.Valid() {
				return nil, nil, &domain.Error{
					Code: domain.CodeClass, Path: ip + ".kind", Version: ver,
					Field: "kind", Value: kind, Message: "unknown category kind",
				}
			}
			out[code] = cat
			codes = append(codes, code)
		}
		groupsOut = append(groupsOut, domain.CategoryGroup{Name: group, Codes: codes})
	}
	if len(out) == 0 {
		return nil, nil, schemaValueError(path, ver, "{}", "category groups must not be empty")
	}
	return out, groupsOut, nil
}

func required(m map[string]any, key, path, ver string) (any, error) {
	v, ok := m[key]
	if !ok || v == nil {
		p := path + "." + key
		if path == "$" {
			p = "$." + key
		}
		return nil, &domain.Error{
			Code: domain.CodeSchema, Path: p, Version: ver, Field: key,
			Message: "required field is missing",
		}
	}
	return v, nil
}

func reqString(m map[string]any, key, path, ver string) (string, error) {
	v, err := required(m, key, path, ver)
	if err != nil {
		return "", err
	}
	p := path + "." + key
	if path == "$" {
		p = "$." + key
	}
	return asString(v, p, ver)
}

func optString(m map[string]any, key, path, ver string) (string, bool, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false, nil
	}
	p := path + "." + key
	if path == "$" {
		p = "$." + key
	}
	s, err := asString(v, p, ver)
	return s, true, err
}

func reqInt(m map[string]any, key, path, ver string) (int64, error) {
	v, err := required(m, key, path, ver)
	if err != nil {
		return 0, err
	}
	p := path + "." + key
	if path == "$" {
		p = "$." + key
	}
	return asInt(v, p, ver)
}

func reqArray(m map[string]any, key, path, ver string) ([]any, error) {
	v, err := required(m, key, path, ver)
	if err != nil {
		return nil, err
	}
	p := path + "." + key
	if path == "$" {
		p = "$." + key
	}
	return asArray(v, p, ver)
}

func optNumber(m map[string]any, key, path, ver string) (any, bool, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, false, nil
	}
	p := path + "." + key
	if path == "$" {
		p = "$." + key
	}
	if _, ok := v.(json.Number); !ok {
		return nil, false, &domain.Error{
			Code: domain.CodeSchema, Path: p, Version: ver, Field: key,
			Message: "expected number",
		}
	}
	return v, true, nil
}

func optInt(m map[string]any, key, path, ver string) (int64, bool, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	p := path + "." + key
	if path == "$" {
		p = "$." + key
	}
	i, err := asInt(v, p, ver)
	return i, true, err
}

func parseSampleInterval(m map[string]any, path, ver string, sampleRate int64,
	render domain.RenderConfig, frameStart, frameEnd int64,
	startKey, endKey string) (int64, int64, []domain.Trace, error) {
	startSample, hasStart, err := optInt(m, startKey, path, ver)
	if err != nil {
		return 0, 0, nil, err
	}
	endSample, hasEnd, err := optInt(m, endKey, path, ver)
	if err != nil {
		return 0, 0, nil, err
	}
	if !hasStart && !hasEnd {
		return 0, 0, nil, nil
	}
	if hasStart != hasEnd {
		missing := endKey
		if !hasStart {
			missing = startKey
		}
		return 0, 0, nil, &domain.Error{
			Code: domain.CodeSchema, Path: path + "." + missing, Version: ver,
			Field: missing, Message: "sample interval fields must be supplied together",
		}
	}
	sc, err := quant.SampleToFrame(startSample, sampleRate, render.TargetFPS,
		render.Rounding, path+"."+startKey)
	if err != nil {
		return 0, 0, nil, err
	}
	ec, err := quant.SampleToFrame(endSample, sampleRate, render.TargetFPS,
		render.Rounding, path+"."+endKey)
	if err != nil {
		return 0, 0, nil, err
	}
	if sc.Value != frameStart || ec.Value != frameEnd {
		return 0, 0, nil, &domain.Error{
			Code: domain.CodeSchema, Path: path + "." + startKey, Version: ver,
			Field: "sample_interval", Expected: fmt.Sprintf("%d..%d", frameStart, frameEnd),
			Actual:  fmt.Sprintf("%d..%d", sc.Value, ec.Value),
			Message: "sample annotation does not quantize to the declared frame interval",
		}
	}
	return startSample, endSample, []domain.Trace{
		conversionTrace(path+"."+startKey, ver, strconv.FormatInt(startSample, 10),
			strconv.FormatInt(sc.Value, 10), "sample -> frame", sc),
		conversionTrace(path+"."+endKey, ver, strconv.FormatInt(endSample, 10),
			strconv.FormatInt(ec.Value, 10), "sample -> frame", ec),
	}, nil
}

func objectAt(v any, path, ver string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &domain.Error{
			Code: domain.CodeSchema, Path: path, Version: ver, Message: "expected object",
		}
	}
	return m, nil
}

func asArray(v any, path, ver string) ([]any, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, &domain.Error{
			Code: domain.CodeSchema, Path: path, Version: ver, Message: "expected array",
		}
	}
	return a, nil
}

func asString(v any, path, ver string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", &domain.Error{
			Code: domain.CodeSchema, Path: path, Version: ver, Message: "expected string",
		}
	}
	return s, nil
}

func asInt(v any, path, ver string) (int64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, &domain.Error{
			Code: domain.CodeSchema, Path: path, Version: ver, Message: "expected integer",
		}
	}
	i, err := n.Int64()
	if err != nil {
		return 0, &domain.Error{
			Code: domain.CodeSchema, Path: path, Version: ver, Value: n.String(),
			Message: "expected int64 integer",
		}
	}
	return i, nil
}

func asFloat(v any, path, ver string) (float64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, &domain.Error{
			Code: domain.CodeSchema, Path: path, Version: ver, Message: "expected number",
		}
	}
	f, err := n.Float64()
	if err != nil {
		return 0, &domain.Error{
			Code: domain.CodeSchema, Path: path, Version: ver, Value: n.String(),
			Message: "invalid number",
		}
	}
	return f, nil
}

func schemaValueError(path, ver, value, message string) error {
	return &domain.Error{
		Code: domain.CodeSchema, Path: path, Version: ver, Value: value, Message: message,
	}
}
