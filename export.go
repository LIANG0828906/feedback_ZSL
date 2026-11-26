package dialoguecal

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
)

type ExportResult struct {
	Version     SchemaVersion
	Data        []byte
	Conversions []ConversionTrace
	Lossy       []LossyIssue
	Fingerprint string
}

type FieldResidual struct {
	FieldPath string
	Before    int64
	After     int64
	Residual  int64
}

type RoundTripResult struct {
	Export            *ExportResult
	Loaded            *Project
	Upgraded          *Project
	Residuals         []FieldResidual
	FingerprintBefore string
	FingerprintAfter  string
}

func encodePCMBase64(pcm PCM) string {
	raw := make([]byte, len(pcm.Samples)*2)
	for i, s := range pcm.Samples {
		binary.LittleEndian.PutUint16(raw[i*2:], uint16(s))
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// CanonicalSemantic builds the source-independent semantic fingerprint.
// Presentation organization, source revision, traces and designer-only
// calibration markers are intentionally excluded so a lossy-but-equivalent
// flattening can still round trip.
func CanonicalSemantic(p *Project) string {
	if p == nil {
		return HashStrings("nil")
	}
	cp := p.Clone()
	cp.Sort()
	parts := []string{
		"dialogue-semantic-v1", cp.ID, fmt.Sprint(cp.SampleRate), fmt.Sprint(cp.FrameRateHz),
		fmt.Sprint(cp.TargetLoudness), fmt.Sprint(cp.SessionGain), fmt.Sprint(cp.GainUpperBound),
		fmt.Sprint(cp.Tolerance), fmt.Sprint(cp.FrameEnergyLimit), fmt.Sprint(cp.MaxParallelism),
		fmt.Sprint(cp.WorkerBufferSamples), fmt.Sprint(cp.QueueBudget),
	}
	for _, d := range cp.Dialogues {
		parts = append(parts, "dialogue", d.ID, d.PCM.Encoding, fmt.Sprint(d.PCM.SampleRate),
			fmt.Sprint(d.PCM.Channels), PCMSHA256(d.PCM))
		for _, s := range d.Segments {
			parts = append(parts, "segment", s.ID, fmt.Sprint(s.StartSample), fmt.Sprint(s.EndSample), fmt.Sprint(s.Gain))
			for _, ph := range s.Phonemes {
				parts = append(parts, "phoneme", ph.ID, fmt.Sprint(ph.StartSample), fmt.Sprint(ph.EndSample),
					ph.Class, fmt.Sprint(ph.Weight))
			}
		}
	}
	return HashStrings(parts...)
}

func (p *Project) SemanticFingerprint() string { return CanonicalSemantic(p) }

func traceExport(p *Project, tr ConversionTrace, target SchemaVersion) {
	tr.SourceVersion = VersionV3
	tr.TargetVersion = target
	p.Conversions = append(p.Conversions, tr)
}

func exportSamples(samples, sampleRate int64, path string, target SchemaVersion, out *Project) (int64, error) {
	ms, tr, err := SamplesToMillis(samples, sampleRate, path, RoundHalfAway)
	if err != nil {
		return 0, err
	}
	back, _, err := MillisToSamples(ms, sampleRate, path, RoundHalfAway)
	if err != nil {
		return 0, err
	}
	tr.Residual = back - samples
	traceExport(out, tr, target)
	if tr.Residual != 0 {
		out.Lossy = append(out.Lossy, LossyIssue{
			Code: CodeLossy, FieldPath: path, Original: fmt.Sprintf("%d samples", samples),
			Disposition: fmt.Sprintf("quantized to %d ms, residual %d samples", ms, tr.Residual),
		})
	}
	return ms, nil
}

func exportScale(value, targetScale int64, path string, target SchemaVersion, out *Project) (int64, error) {
	q, tr, err := PerMyriadToScale(value, targetScale, path, RoundHalfAway)
	if err != nil {
		return 0, err
	}
	back, _, err := ScaleToPerMyriad(q, targetScale, path, RoundHalfAway)
	if err != nil {
		return 0, err
	}
	tr.Residual = back - value
	traceExport(out, tr, target)
	if tr.Residual != 0 {
		out.Lossy = append(out.Lossy, LossyIssue{
			Code: CodeLossy, FieldPath: path, Original: fmt.Sprint(value),
			Disposition: fmt.Sprintf("quantized to %d at 1/%d, residual %d", q, targetScale, tr.Residual),
		})
	}
	return q, nil
}

func identityScale(value int64, path string, target SchemaVersion, out *Project) int64 {
	traceExport(out, ConversionTrace{
		FieldPath: path, Stage: "identity", FromUnit: "1/10000", ToUnit: "1/10000",
		Rounding: string(RoundHalfAway), OperandA: value, OperandB: ScalePerMyriad,
		Result: value, TruncationSite: "none",
	}, target)
	return value
}

func identitySamples(value int64, path string, target SchemaVersion, out *Project) int64 {
	traceExport(out, ConversionTrace{
		FieldPath: path, Stage: "identity", FromUnit: "sample", ToUnit: "sample",
		Rounding: string(RoundHalfAway), OperandA: value, Result: value, TruncationSite: "none",
	}, target)
	return value
}

func appendMarkerLoss(out *Project, markers []CalibrationMarker, target SchemaVersion) {
	if target >= VersionV3 {
		return
	}
	for _, m := range markers {
		out.Lossy = append(out.Lossy, LossyIssue{
			Code: CodeLossy, FieldPath: m.Path, Original: m.Kind + "=" + m.Value,
			Disposition: fmt.Sprintf("omitted by schema v%d; designer marker is not part of the semantic fingerprint", target),
		})
	}
}

func appendOrganizationLoss(out *Project, target SchemaVersion) {
	if out.Organization == "scene-grouped" && target < VersionV3 {
		out.Lossy = append(out.Lossy, LossyIssue{
			Code: CodeLossy, FieldPath: "$.sceneGroups", Original: fmt.Sprintf("%d scene groups", len(out.SceneGroups)),
			Disposition: fmt.Sprintf("flattened into schema v%d dialogue/segment tables; dialogue and segment identities are preserved", target),
		})
	}
	if out.Organization == "dialogue-nested" && target == VersionV1 {
		out.Lossy = append(out.Lossy, LossyIssue{
			Code: CodeLossy, FieldPath: "$.dialogues[].segments", Original: out.Organization,
			Disposition: "flattened into $.segments with an explicit dialogue_id",
		})
	}
}

type v1AudioTrack struct {
	DialogueID  string `json:"dialogue_id"`
	Encoding    string `json:"encoding"`
	Channels    int    `json:"channels"`
	SampleRate  int    `json:"sample_rate"`
	SampleCount int64  `json:"sample_count"`
	PCMBase64   string `json:"pcm_base64"`
}

type v1Phoneme struct {
	PhonemeID      string `json:"phoneme_id"`
	StartMS        int64  `json:"start_ms"`
	EndMS          int64  `json:"end_ms"`
	Class          string `json:"class"`
	WeightPermille int64  `json:"weight_permille"`
}

type v1Segment struct {
	DialogueID   string      `json:"dialogue_id"`
	SegmentID    string      `json:"segment_id"`
	StartMS      int64       `json:"start_ms"`
	EndMS        int64       `json:"end_ms"`
	GainPermille int64       `json:"gain_permille"`
	Phonemes     []v1Phoneme `json:"phonemes"`
}

type v1Document struct {
	Schema                 string         `json:"schema"`
	ProjectID              string         `json:"project_id"`
	FrameRateHz            int            `json:"frame_rate_hz"`
	SampleRate             int            `json:"sample_rate"`
	TargetLoudnessPermille int64          `json:"target_loudness_permille"`
	SessionGainPermille    int64          `json:"session_gain_permille"`
	GainUpperBoundPermille int64          `json:"gain_upper_bound_permille"`
	TolerancePermille      int64          `json:"tolerance_permille"`
	FrameEnergyLimit       int64          `json:"frame_energy_limit"`
	MaxParallelism         int            `json:"max_parallelism"`
	WorkerBufferSamples    int            `json:"worker_buffer_samples"`
	QueueBudget            int            `json:"queue_budget"`
	AudioTracks            []v1AudioTrack `json:"audio_tracks"`
	Segments               []v1Segment    `json:"segments"`
}

type v2PCM struct {
	Encoding    string `json:"encoding"`
	SampleRate  int    `json:"sampleRate"`
	Channels    int    `json:"channels"`
	SampleCount int64  `json:"sampleCount"`
	DataBase64  string `json:"dataBase64"`
}

type v2Phoneme struct {
	ID                string `json:"id"`
	StartSample       int64  `json:"startSample"`
	EndSample         int64  `json:"endSample"`
	Class             string `json:"class"`
	WeightBasisPoints int64  `json:"weightBasisPoints"`
}

type v2Segment struct {
	ID              string      `json:"id"`
	StartSample     int64       `json:"startSample"`
	EndSample       int64       `json:"endSample"`
	GainBasisPoints int64       `json:"gainBasisPoints"`
	Phonemes        []v2Phoneme `json:"phonemes"`
}

type v2Dialogue struct {
	ID       string      `json:"id"`
	PCM      v2PCM       `json:"pcm"`
	Segments []v2Segment `json:"segments"`
}

type v2Document struct {
	SchemaVersion             int          `json:"schemaVersion"`
	ProjectID                 string       `json:"projectId"`
	FrameRateHz               int          `json:"frameRateHz"`
	SampleRate                int          `json:"sampleRate"`
	TargetLoudnessBasisPoints int64        `json:"targetLoudnessBasisPoints"`
	SessionGainBasisPoints    int64        `json:"sessionGainBasisPoints"`
	GainUpperBoundBasisPoints int64        `json:"gainUpperBoundBasisPoints"`
	ToleranceBasisPoints      int64        `json:"toleranceBasisPoints"`
	FrameEnergyLimit          int64        `json:"frameEnergyLimit"`
	Parallel                  v2Parallel   `json:"parallel"`
	Quantization              v2Named      `json:"quantization"`
	Conversion                v2Named      `json:"conversion"`
	AlgorithmVersion          string       `json:"algorithmVersion"`
	Dialogues                 []v2Dialogue `json:"dialogues"`
}

type v2Parallel struct {
	MaxParallelism      int `json:"maxParallelism"`
	WorkerBufferSamples int `json:"workerBufferSamples"`
	QueueBudget         int `json:"queueBudget"`
}

type v2Named struct {
	Version string `json:"version"`
}

type v3PCM struct {
	Encoding      string `json:"encoding"`
	SampleRate    int    `json:"sampleRate"`
	Channels      int    `json:"channels"`
	SampleCount   int64  `json:"sampleCount"`
	SamplesBase64 string `json:"samplesBase64"`
}

type v3Marker struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type v3Phoneme struct {
	ID              string `json:"id"`
	StartSample     int64  `json:"startSample"`
	EndSample       int64  `json:"endSample"`
	Class           string `json:"class"`
	WeightPermyriad int64  `json:"weightPermyriad"`
}

type v3Segment struct {
	ID                 string      `json:"id"`
	StartSample        int64       `json:"startSample"`
	EndSample          int64       `json:"endSample"`
	GainPermyriad      int64       `json:"gainPermyriad"`
	Phonemes           []v3Phoneme `json:"phonemes"`
	CalibrationMarkers []v3Marker  `json:"calibrationMarkers,omitempty"`
}

type v3Dialogue struct {
	ID       string      `json:"id"`
	PCM      v3PCM       `json:"pcm"`
	Segments []v3Segment `json:"segments"`
}

type v3SceneGroup struct {
	ID        string       `json:"id"`
	Dialogues []v3Dialogue `json:"dialogues"`
}

type v3Calibration struct {
	TargetLoudnessPermyriad int64      `json:"targetLoudnessPermyriad"`
	SessionGainPermyriad    int64      `json:"sessionGainPermyriad"`
	GainUpperBoundPermyriad int64      `json:"gainUpperBoundPermyriad"`
	TolerancePermyriad      int64      `json:"tolerancePermyriad"`
	Markers                 []v3Marker `json:"markers"`
}

type v3Versions struct {
	Quantization   string `json:"quantization"`
	ParallelBudget string `json:"parallelBudget"`
	Conversion     string `json:"conversion"`
	Algorithm      string `json:"algorithm"`
}

type v3Project struct {
	ID               string        `json:"id"`
	SampleRate       int           `json:"sampleRate"`
	FrameRateHz      int           `json:"frameRateHz"`
	FrameEnergyLimit int64         `json:"frameEnergyLimit"`
	Calibration      v3Calibration `json:"calibration"`
	LimitedParallel  v2Parallel    `json:"limitedParallel"`
	Versions         v3Versions    `json:"versions"`
}

type v3Document struct {
	Schema        string         `json:"schema"`
	SchemaVersion int            `json:"schemaVersion"`
	Project       v3Project      `json:"project"`
	SceneGroups   []v3SceneGroup `json:"sceneGroups"`
}

func ExportProject(p *Project, target SchemaVersion) (*ExportResult, error) {
	if p == nil {
		return nil, &Error{Code: CodeSchema, Path: "$", Message: "nil project"}
	}
	if _, ok := ParseSchemaVersion(int(target)); !ok {
		return nil, &Error{Code: CodeSchema, Path: "$.schemaVersion", Message: "unknown target schema version"}
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	cp := p.Clone()
	cp.Sort()
	cp.Lossy = nil
	cp.Conversions = nil
	cp.Organization = p.Organization
	if cp.Organization == "" {
		cp.Organization = "dialogue-nested"
	}
	appendOrganizationLoss(cp, target)
	appendMarkerLoss(cp, cp.CalibrationMarkers, target)
	for _, d := range cp.Dialogues {
		for _, s := range d.Segments {
			appendMarkerLoss(cp, s.Markers, target)
		}
	}
	var doc any
	var err error
	switch target {
	case VersionV1:
		doc, err = buildV1(cp)
	case VersionV2:
		doc, err = buildV2(cp)
	case VersionV3:
		doc, err = buildV3(cp)
	}
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, &Error{Code: CodeDecode, Path: "$", Message: err.Error()}
	}
	data = append(data, '\n')
	return &ExportResult{
		Version: target, Data: data, Conversions: append([]ConversionTrace(nil), cp.Conversions...),
		Lossy: append([]LossyIssue(nil), cp.Lossy...), Fingerprint: CanonicalSemantic(p),
	}, nil
}

func buildV1(p *Project) (*v1Document, error) {
	target, err := exportScale(p.TargetLoudness, ScalePermille, "$.target_loudness_permille", VersionV1, p)
	if err != nil {
		return nil, err
	}
	session, err := exportScale(p.SessionGain, ScalePermille, "$.session_gain_permille", VersionV1, p)
	if err != nil {
		return nil, err
	}
	upper, err := exportScale(p.GainUpperBound, ScalePermille, "$.gain_upper_bound_permille", VersionV1, p)
	if err != nil {
		return nil, err
	}
	tolerance, err := exportScale(p.Tolerance, ScalePermille, "$.tolerance_permille", VersionV1, p)
	if err != nil {
		return nil, err
	}
	doc := &v1Document{
		Schema: "dialogue.v1", ProjectID: p.ID, FrameRateHz: p.FrameRateHz,
		SampleRate: p.SampleRate, TargetLoudnessPermille: target,
		SessionGainPermille: session, GainUpperBoundPermille: upper,
		TolerancePermille: tolerance, FrameEnergyLimit: p.FrameEnergyLimit,
		MaxParallelism: p.MaxParallelism, WorkerBufferSamples: p.WorkerBufferSamples,
		QueueBudget: p.QueueBudget, AudioTracks: []v1AudioTrack{}, Segments: []v1Segment{},
	}
	for _, d := range p.Dialogues {
		doc.AudioTracks = append(doc.AudioTracks, v1AudioTrack{
			DialogueID: d.ID, Encoding: d.PCM.Encoding, Channels: d.PCM.Channels,
			SampleRate: d.PCM.SampleRate, SampleCount: int64(len(d.PCM.Samples)),
			PCMBase64: encodePCMBase64(d.PCM),
		})
		for _, s := range d.Segments {
			start, err := exportSamples(s.StartSample, int64(d.PCM.SampleRate), fmt.Sprintf("$.segments.%s.start_ms", s.ID), VersionV1, p)
			if err != nil {
				return nil, err
			}
			end, err := exportSamples(s.EndSample, int64(d.PCM.SampleRate), fmt.Sprintf("$.segments.%s.end_ms", s.ID), VersionV1, p)
			if err != nil {
				return nil, err
			}
			gain, err := exportScale(s.Gain, ScalePermille, fmt.Sprintf("$.segments.%s.gain_permille", s.ID), VersionV1, p)
			if err != nil {
				return nil, err
			}
			vs := v1Segment{DialogueID: d.ID, SegmentID: s.ID, StartMS: start, EndMS: end, GainPermille: gain}
			for _, ph := range s.Phonemes {
				ps, err := exportSamples(ph.StartSample, int64(d.PCM.SampleRate), fmt.Sprintf("$.segments.%s.phonemes.%s.start_ms", s.ID, ph.ID), VersionV1, p)
				if err != nil {
					return nil, err
				}
				pe, err := exportSamples(ph.EndSample, int64(d.PCM.SampleRate), fmt.Sprintf("$.segments.%s.phonemes.%s.end_ms", s.ID, ph.ID), VersionV1, p)
				if err != nil {
					return nil, err
				}
				weight, err := exportScale(int64(ph.Weight), ScalePermille, fmt.Sprintf("$.segments.%s.phonemes.%s.weight_permille", s.ID, ph.ID), VersionV1, p)
				if err != nil {
					return nil, err
				}
				vs.Phonemes = append(vs.Phonemes, v1Phoneme{
					PhonemeID: ph.ID, StartMS: ps, EndMS: pe, Class: ph.Class, WeightPermille: weight,
				})
			}
			doc.Segments = append(doc.Segments, vs)
		}
	}
	return doc, nil
}

func buildV2(p *Project) (*v2Document, error) {
	doc := &v2Document{
		SchemaVersion: 2, ProjectID: p.ID, FrameRateHz: p.FrameRateHz, SampleRate: p.SampleRate,
		TargetLoudnessBasisPoints: identityScale(p.TargetLoudness, "$.targetLoudnessBasisPoints", VersionV2, p),
		SessionGainBasisPoints:    identityScale(p.SessionGain, "$.sessionGainBasisPoints", VersionV2, p),
		GainUpperBoundBasisPoints: identityScale(p.GainUpperBound, "$.gainUpperBoundBasisPoints", VersionV2, p),
		ToleranceBasisPoints:      identityScale(p.Tolerance, "$.toleranceBasisPoints", VersionV2, p),
		FrameEnergyLimit:          p.FrameEnergyLimit,
		Parallel:                  v2Parallel{MaxParallelism: p.MaxParallelism, WorkerBufferSamples: p.WorkerBufferSamples, QueueBudget: p.QueueBudget},
		Quantization:              v2Named{Version: p.QuantizationVersion},
		Conversion:                v2Named{Version: p.ConversionVersion},
		AlgorithmVersion:          p.AlgorithmVersion, Dialogues: []v2Dialogue{},
	}
	for _, d := range p.Dialogues {
		vd := v2Dialogue{
			ID: d.ID,
			PCM: v2PCM{Encoding: d.PCM.Encoding, SampleRate: d.PCM.SampleRate, Channels: d.PCM.Channels,
				SampleCount: int64(len(d.PCM.Samples)), DataBase64: encodePCMBase64(d.PCM)},
			Segments: []v2Segment{},
		}
		for _, s := range d.Segments {
			vs := v2Segment{
				ID: s.ID, StartSample: identitySamples(s.StartSample, fmt.Sprintf("$.dialogues.%s.segments.%s.startSample", d.ID, s.ID), VersionV2, p),
				EndSample:       identitySamples(s.EndSample, fmt.Sprintf("$.dialogues.%s.segments.%s.endSample", d.ID, s.ID), VersionV2, p),
				GainBasisPoints: identityScale(s.Gain, fmt.Sprintf("$.dialogues.%s.segments.%s.gainBasisPoints", d.ID, s.ID), VersionV2, p),
			}
			for _, ph := range s.Phonemes {
				vs.Phonemes = append(vs.Phonemes, v2Phoneme{
					ID:                ph.ID,
					StartSample:       identitySamples(ph.StartSample, fmt.Sprintf("$.dialogues.%s.segments.%s.phonemes.%s.startSample", d.ID, s.ID, ph.ID), VersionV2, p),
					EndSample:         identitySamples(ph.EndSample, fmt.Sprintf("$.dialogues.%s.segments.%s.phonemes.%s.endSample", d.ID, s.ID, ph.ID), VersionV2, p),
					Class:             ph.Class,
					WeightBasisPoints: identityScale(int64(ph.Weight), fmt.Sprintf("$.dialogues.%s.segments.%s.phonemes.%s.weightBasisPoints", d.ID, s.ID, ph.ID), VersionV2, p),
				})
			}
			vd.Segments = append(vd.Segments, vs)
		}
		doc.Dialogues = append(doc.Dialogues, vd)
	}
	return doc, nil
}

func buildV3(p *Project) (*v3Document, error) {
	doc := &v3Document{
		Schema: "dialogue.v3", SchemaVersion: 3,
		Project: v3Project{
			ID: p.ID, SampleRate: p.SampleRate, FrameRateHz: p.FrameRateHz,
			FrameEnergyLimit: p.FrameEnergyLimit,
			Calibration: v3Calibration{
				TargetLoudnessPermyriad: identityScale(p.TargetLoudness, "$.project.calibration.targetLoudnessPermyriad", VersionV3, p),
				SessionGainPermyriad:    identityScale(p.SessionGain, "$.project.calibration.sessionGainPermyriad", VersionV3, p),
				GainUpperBoundPermyriad: identityScale(p.GainUpperBound, "$.project.calibration.gainUpperBoundPermyriad", VersionV3, p),
				TolerancePermyriad:      identityScale(p.Tolerance, "$.project.calibration.tolerancePermyriad", VersionV3, p),
				Markers:                 []v3Marker{},
			},
			LimitedParallel: v2Parallel{MaxParallelism: p.MaxParallelism, WorkerBufferSamples: p.WorkerBufferSamples, QueueBudget: p.QueueBudget},
			Versions: v3Versions{Quantization: p.QuantizationVersion, ParallelBudget: p.ParallelVersion,
				Conversion: p.ConversionVersion, Algorithm: p.AlgorithmVersion},
		},
		SceneGroups: []v3SceneGroup{},
	}
	for _, m := range p.CalibrationMarkers {
		doc.Project.Calibration.Markers = append(doc.Project.Calibration.Markers, v3Marker{Kind: m.Kind, Value: m.Value})
	}
	dialogues := make(map[string]v3Dialogue, len(p.Dialogues))
	for _, d := range p.Dialogues {
		vd := v3Dialogue{
			ID: d.ID,
			PCM: v3PCM{Encoding: d.PCM.Encoding, SampleRate: d.PCM.SampleRate, Channels: d.PCM.Channels,
				SampleCount: int64(len(d.PCM.Samples)), SamplesBase64: encodePCMBase64(d.PCM)},
			Segments: []v3Segment{},
		}
		for _, s := range d.Segments {
			vs := v3Segment{
				ID: s.ID, StartSample: identitySamples(s.StartSample, fmt.Sprintf("$.sceneGroups.dialogues.%s.segments.%s.startSample", d.ID, s.ID), VersionV3, p),
				EndSample:     identitySamples(s.EndSample, fmt.Sprintf("$.sceneGroups.dialogues.%s.segments.%s.endSample", d.ID, s.ID), VersionV3, p),
				GainPermyriad: identityScale(s.Gain, fmt.Sprintf("$.sceneGroups.dialogues.%s.segments.%s.gainPermyriad", d.ID, s.ID), VersionV3, p),
			}
			for _, ph := range s.Phonemes {
				vs.Phonemes = append(vs.Phonemes, v3Phoneme{
					ID:              ph.ID,
					StartSample:     identitySamples(ph.StartSample, fmt.Sprintf("$.sceneGroups.dialogues.%s.segments.%s.phonemes.%s.startSample", d.ID, s.ID, ph.ID), VersionV3, p),
					EndSample:       identitySamples(ph.EndSample, fmt.Sprintf("$.sceneGroups.dialogues.%s.segments.%s.phonemes.%s.endSample", d.ID, s.ID, ph.ID), VersionV3, p),
					Class:           ph.Class,
					WeightPermyriad: identityScale(int64(ph.Weight), fmt.Sprintf("$.sceneGroups.dialogues.%s.segments.%s.phonemes.%s.weightPermyriad", d.ID, s.ID, ph.ID), VersionV3, p),
				})
			}
			for _, m := range s.Markers {
				vs.CalibrationMarkers = append(vs.CalibrationMarkers, v3Marker{Kind: m.Kind, Value: m.Value})
			}
			vd.Segments = append(vd.Segments, vs)
		}
		dialogues[d.ID] = vd
	}
	if len(p.SceneGroups) == 0 {
		g := v3SceneGroup{ID: "scene-01"}
		for _, d := range p.Dialogues {
			g.Dialogues = append(g.Dialogues, dialogues[d.ID])
		}
		doc.SceneGroups = append(doc.SceneGroups, g)
	} else {
		seen := make(map[string]bool)
		for _, sg := range p.SceneGroups {
			g := v3SceneGroup{ID: sg.ID}
			for _, id := range sg.DialogueIDs {
				if d, ok := dialogues[id]; ok {
					g.Dialogues = append(g.Dialogues, d)
					seen[id] = true
				}
			}
			doc.SceneGroups = append(doc.SceneGroups, g)
		}
		for _, d := range p.Dialogues {
			if !seen[d.ID] {
				if len(doc.SceneGroups) == 0 {
					doc.SceneGroups = append(doc.SceneGroups, v3SceneGroup{ID: "scene-orphans"})
				}
				doc.SceneGroups[len(doc.SceneGroups)-1].Dialogues = append(doc.SceneGroups[len(doc.SceneGroups)-1].Dialogues, dialogues[d.ID])
			}
		}
	}
	return doc, nil
}

// UpgradeProject applies declared defaults while moving a source revision up
// to target. Every added field is recorded with old/default/source values.
func UpgradeProject(p *Project, target SchemaVersion) (*Project, error) {
	if p == nil {
		return nil, &Error{Code: CodeSchema, Path: "$", Message: "nil project"}
	}
	if _, ok := ParseSchemaVersion(int(target)); !ok {
		return nil, &Error{Code: CodeSchema, Path: "$.schemaVersion", Message: "unknown target schema version"}
	}
	if target < p.SourceVersion {
		return nil, &Error{Code: CodeState, Entity: "project", From: p.SourceVersion.String(), To: target.String(), Message: "use ExportProject for downgrade"}
	}
	cp := p.Clone()
	if cp.QuantizationVersion == "" {
		cp.QuantizationVersion = DefaultQuantizationVersion
		addDefault(cp, "$.quantization", "<missing>", cp.QuantizationVersion, "upgrade default")
	}
	if cp.ParallelVersion == "" {
		cp.ParallelVersion = DefaultParallelVersion
		addDefault(cp, "$.parallelBudget", "<missing>", cp.ParallelVersion, "upgrade default")
	}
	if cp.ConversionVersion == "" {
		cp.ConversionVersion = DefaultConversionVersion
		addDefault(cp, "$.conversion", "<missing>", cp.ConversionVersion, "upgrade default")
	}
	if cp.AlgorithmVersion == "" {
		cp.AlgorithmVersion = DefaultAlgorithmVersion
		addDefault(cp, "$.algorithm", "<missing>", cp.AlgorithmVersion, "upgrade default")
	}
	if target >= VersionV3 && len(cp.CalibrationMarkers) == 0 {
		addDefault(cp, "$.project.calibration.markers", "<missing>", "[]", "v3 empty-marker default")
	}
	cp.SourceVersion = target
	return cp, nil
}

func RoundTripProject(p *Project, target SchemaVersion) (*RoundTripResult, error) {
	before := CanonicalSemantic(p)
	exp, err := ExportProject(p, target)
	if err != nil {
		return nil, err
	}
	loaded, err := LoadProject(exp.Data)
	if err != nil {
		return nil, err
	}
	upgraded, err := UpgradeProject(loaded, VersionV3)
	if err != nil {
		return nil, err
	}
	return &RoundTripResult{
		Export: exp, Loaded: loaded, Upgraded: upgraded,
		Residuals:         CompareProjectResidual(p, upgraded),
		FingerprintBefore: before, FingerprintAfter: CanonicalSemantic(upgraded),
	}, nil
}

func CompareProjectResidual(before, after *Project) []FieldResidual {
	var out []FieldResidual
	compare := func(path string, a, b int64) {
		if a != b {
			out = append(out, FieldResidual{FieldPath: path, Before: a, After: b, Residual: b - a})
		}
	}
	compare("$.targetLoudness", before.TargetLoudness, after.TargetLoudness)
	compare("$.sessionGain", before.SessionGain, after.SessionGain)
	compare("$.gainUpperBound", before.GainUpperBound, after.GainUpperBound)
	compare("$.tolerance", before.Tolerance, after.Tolerance)
	am := make(map[string]Segment)
	for _, d := range before.Dialogues {
		for _, s := range d.Segments {
			am[d.ID+"/"+s.ID] = s
		}
	}
	for _, d := range after.Dialogues {
		for _, s := range d.Segments {
			a, ok := am[d.ID+"/"+s.ID]
			if !ok {
				out = append(out, FieldResidual{FieldPath: "$.segments." + s.ID, Before: -1, After: 1, Residual: 1})
				continue
			}
			prefix := "$.dialogues." + d.ID + ".segments." + s.ID
			compare(prefix+".startSample", a.StartSample, s.StartSample)
			compare(prefix+".endSample", a.EndSample, s.EndSample)
			compare(prefix+".gain", a.Gain, s.Gain)
			pm := make(map[string]Phoneme)
			for _, ph := range a.Phonemes {
				pm[ph.ID] = ph
			}
			for _, ph := range s.Phonemes {
				b, ok := pm[ph.ID]
				if !ok {
					out = append(out, FieldResidual{FieldPath: prefix + ".phonemes." + ph.ID, Before: -1, After: 1, Residual: 1})
					continue
				}
				pp := prefix + ".phonemes." + ph.ID
				compare(pp+".startSample", b.StartSample, ph.StartSample)
				compare(pp+".endSample", b.EndSample, ph.EndSample)
				compare(pp+".weight", int64(b.Weight), int64(ph.Weight))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FieldPath < out[j].FieldPath })
	return out
}
