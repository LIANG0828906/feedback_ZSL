// Package codec loads and exports the three dialogue-project schema versions
// into the unified internal model.
package codec

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"dialogueaudio/audioerr"
	"dialogueaudio/fixed"
	"dialogueaudio/model"
)

// LossItem describes one explicitly handled downgrade loss.
type LossItem struct {
	Path        string `json:"path"`
	Kind        string `json:"kind"`
	Value       any    `json:"value"`
	Disposition string `json:"disposition"`
}

// LossReport is non-fatal and uses E_LOSSY as its stable report code.
type LossReport struct {
	Code  audioerr.Code `json:"code"`
	Items []LossItem    `json:"items"`
}

// ExportResult includes bytes plus every lossy conversion or arithmetic
// residual observed while producing the lower-version document.
type ExportResult struct {
	Data   []byte        `json:"-"`
	Losses LossReport    `json:"losses"`
	Traces []fixed.Trace `json:"conversion_traces,omitempty"`
}

// LoadFile reads and loads one project document.
func LoadFile(path string) (*model.Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, audioerr.New(audioerr.CodeIO, path, err.Error())
	}
	return Load(data)
}

// Load parses all three schema versions into one internal model. Every
// conversion and upgrade default is recorded in the returned project.
func Load(data []byte) (*model.Project, error) {
	root, err := decodeObject(data, "$")
	if err != nil {
		return nil, err
	}
	version, versionPath, err := detectVersion(root)
	if err != nil {
		return nil, err
	}
	switch version {
	case 1:
		return loadV1(root, versionPath)
	case 2:
		return loadV2(root, versionPath)
	case 3:
		return loadV3(root, versionPath)
	default:
		return nil, audioerr.Schema(versionPath, fmt.Sprintf("unknown schema version %d", version))
	}
}

// DetectVersion returns the source version and the version field path.
func DetectVersion(data []byte) (int, string, error) {
	root, err := decodeObject(data, "$")
	if err != nil {
		return 0, "", err
	}
	return detectVersion(root)
}

func detectVersion(root map[string]json.RawMessage) (int, string, error) {
	type candidate struct {
		key string
		ver int
	}
	candidates := []candidate{
		{"schema_version", 1},
		{"schemaVersion", 2},
		{"revision", 3},
	}
	found := make([]candidate, 0, 1)
	for _, cand := range candidates {
		if raw, ok := root[cand.key]; ok {
			version, err := decodeInt(raw, cand.key)
			if err != nil {
				return 0, cand.key, err
			}
			found = append(found, candidate{cand.key, int(version)})
		}
	}
	if len(found) == 0 {
		return 0, "schema_version|schemaVersion|revision", audioerr.Schema(
			"$", "missing schema version field")
	}
	if len(found) > 1 {
		return 0, found[1].key, audioerr.Schema(found[1].key, "multiple conflicting schema version fields")
	}
	return found[0].ver, found[0].key, nil
}

type builder struct {
	project *model.Project
}

func newBuilder(version int) *builder {
	return &builder{project: &model.Project{
		SourceVersion:    version,
		ConversionTraces: make([]fixed.Trace, 0, 64),
		Defaults:         make([]model.DefaultTrace, 0, 8),
	}}
}

func (b *builder) defaulted(path string, oldValue, defaultValue any, source string) {
	b.project.Defaults = append(b.project.Defaults, model.DefaultTrace{
		Path:         path,
		OldValue:     oldValue,
		DefaultValue: defaultValue,
		Source:       source,
	})
}

func (b *builder) trace(tr fixed.Trace) {
	b.project.ConversionTraces = append(b.project.ConversionTraces, tr)
}

func (b *builder) rescale(value, fromScale, toScale int64, rounding fixed.Rounding, path, fromName, toName string) (int64, error) {
	out, tr, err := fixed.Rescale(value, fromScale, toScale, rounding, path)
	if err != nil {
		return 0, err
	}
	tr.FromScale = fromName
	tr.ToScale = toName
	b.trace(tr)
	return out, nil
}

func (b *builder) millisToSamples(value int64, sampleRate int, path string) (int64, error) {
	out, tr, err := fixed.MillisToSamples(value, sampleRate, fixed.RoundNearest, path)
	if err != nil {
		return 0, err
	}
	b.trace(tr)
	return out, nil
}

func (b *builder) microsToSamples(value int64, sampleRate int, path string) (int64, error) {
	out, tr, err := fixed.MicrosToSamples(value, sampleRate, fixed.RoundNearest, path)
	if err != nil {
		return 0, err
	}
	b.trace(tr)
	return out, nil
}

func loadV1(root map[string]json.RawMessage, versionPath string) (*model.Project, error) {
	b := newBuilder(1)
	p := b.project
	projectObj, err := requiredObject(root, "project", "project")
	if err != nil {
		return nil, err
	}
	p.ProjectID, err = requiredString(projectObj, "id", "project.id")
	if err != nil {
		return nil, err
	}
	sampleRate, err := requiredInt(projectObj, "sample_rate", "project.sample_rate")
	if err != nil {
		return nil, err
	}
	if sampleRate <= 0 {
		return nil, audioerr.Schema("project.sample_rate", "must be positive")
	}
	p.SampleRate = int(sampleRate)
	frameMS, err := requiredInt(projectObj, "frame_ms", "project.frame_ms")
	if err != nil {
		return nil, err
	}
	hopMS, err := requiredInt(projectObj, "hop_ms", "project.hop_ms")
	if err != nil {
		return nil, err
	}
	p.FrameLenSamples, err = b.millisToSamples(frameMS, p.SampleRate, "project.frame_ms")
	if err != nil {
		return nil, err
	}
	p.HopSamples, err = b.millisToSamples(hopMS, p.SampleRate, "project.hop_ms")
	if err != nil {
		return nil, err
	}
	target, err := requiredInt(projectObj, "target_lufs_q100", "project.target_lufs_q100")
	if err != nil {
		return nil, err
	}
	p.TargetLoudnessQ16, err = b.rescale(target, 100, fixed.Q16, fixed.RoundNearest,
		"project.target_lufs_q100", "dB_q100", "dB_q16")
	if err != nil {
		return nil, err
	}
	sessionGain, err := requiredInt(projectObj, "session_gain_q1000", "project.session_gain_q1000")
	if err != nil {
		return nil, err
	}
	p.SessionGainQ16, err = b.rescale(sessionGain, 1000, fixed.Q16, fixed.RoundNearest,
		"project.session_gain_q1000", "linear_q1000", "linear_q16")
	if err != nil {
		return nil, err
	}
	maxAdj, err := requiredInt(projectObj, "max_adj_db_q100", "project.max_adj_db_q100")
	if err != nil {
		return nil, err
	}
	p.MaxAdjustmentDBQ16, err = b.rescale(maxAdj, 100, fixed.Q16, fixed.RoundNearest,
		"project.max_adj_db_q100", "dB_q100", "dB_q16")
	if err != nil {
		return nil, err
	}
	p.CalibrationEnabled = true
	b.defaulted("project.calibration_enabled", nil, true, "schema-v1.defaults")

	clipsRaw, err := requiredArray(root, "clips", "clips")
	if err != nil {
		return nil, err
	}
	seenScenes := make(map[string]int)
	for i, raw := range clipsRaw {
		base := fmt.Sprintf("clips[%d]", i)
		clipObj, err := rawObject(raw, base)
		if err != nil {
			return nil, err
		}
		clip, err := loadV1Clip(b, clipObj, base)
		if err != nil {
			return nil, err
		}
		if _, ok := seenScenes[clip.SceneID]; !ok {
			seenScenes[clip.SceneID] = len(p.Scenes)
			p.Scenes = append(p.Scenes, model.Scene{ID: clip.SceneID, Name: clip.SceneID})
			b.defaulted(fmt.Sprintf("scenes[%d].name", len(p.Scenes)-1), nil, clip.SceneID, "schema-v1.defaults")
		}
		clip.SceneName = clip.SceneID
		p.Clips = append(p.Clips, clip)
	}
	p.SortClips()
	if err := finalizeProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV1Clip(b *builder, obj map[string]json.RawMessage, base string) (model.Clip, error) {
	var clip model.Clip
	var err error
	clip.ID, err = requiredString(obj, "id", base+".id")
	if err != nil {
		return clip, err
	}
	clip.SceneID, err = requiredString(obj, "scene", base+".scene")
	if err != nil {
		return clip, err
	}
	startMS, err := requiredInt(obj, "start_ms", base+".start_ms")
	if err != nil {
		return clip, err
	}
	endMS, err := requiredInt(obj, "end_ms", base+".end_ms")
	if err != nil {
		return clip, err
	}
	clip.StartSample, err = b.millisToSamples(startMS, b.project.SampleRate, base+".start_ms")
	if err != nil {
		return clip, err
	}
	clip.EndSample, err = b.millisToSamples(endMS, b.project.SampleRate, base+".end_ms")
	if err != nil {
		return clip, err
	}
	if clip.EndSample <= clip.StartSample {
		return clip, audioerr.Schema(base+".end_ms", "must be greater than start_ms")
	}
	gain, err := requiredInt(obj, "gain_milli", base+".gain_milli")
	if err != nil {
		return clip, err
	}
	clip.GainQ16, err = b.rescale(gain, 1000, fixed.Q16, fixed.RoundNearest, base+".gain_milli", "linear_q1000", "linear_q16")
	if err != nil {
		return clip, err
	}
	if clip.GainQ16 <= 0 {
		return clip, audioerr.Schema(base+".gain_milli", "must be positive")
	}
	weight, err := requiredInt(obj, "weight_permille", base+".weight_permille")
	if err != nil {
		return clip, err
	}
	clip.LoudnessWeightQ16, err = b.rescale(weight, 1000, fixed.Q16, fixed.RoundNearest, base+".weight_permille", "weight_q1000", "weight_q16")
	if err != nil {
		return clip, err
	}
	if clip.LoudnessWeightQ16 <= 0 {
		return clip, audioerr.Schema(base+".weight_permille", "must be positive")
	}
	clip.Category, err = requiredString(obj, "voice_type", base+".voice_type")
	if err != nil {
		return clip, err
	}
	label, ok, err := optionalString(obj, "label", base+".label")
	if err != nil {
		return clip, err
	}
	if !ok || label == "" {
		label = clip.ID
		b.defaulted(base+".label", nil, clip.ID, "schema-v1.defaults")
	}
	clip.Label = label
	pcm, err := requiredString(obj, "pcm_b64", base+".pcm_b64")
	if err != nil {
		return clip, err
	}
	clip.PCM, err = decodePCM(pcm, base+".pcm_b64")
	if err != nil {
		return clip, err
	}
	clip.GroupPath = []string{}
	b.defaulted(base+".group_path", nil, []string{}, "schema-v1.defaults")
	clip.CalibrationMark = ""
	b.defaulted(base+".calibration_mark", nil, "", "schema-v1.defaults")
	return clip, nil
}

func loadV2(root map[string]json.RawMessage, versionPath string) (*model.Project, error) {
	b := newBuilder(2)
	p := b.project
	var err error
	p.ProjectID, err = requiredString(root, "projId", "projId")
	if err != nil {
		return nil, err
	}
	audio, err := requiredObject(root, "audio", "audio")
	if err != nil {
		return nil, err
	}
	sampleRate, err := requiredInt(audio, "sampleRateHz", "audio.sampleRateHz")
	if err != nil {
		return nil, err
	}
	if sampleRate <= 0 {
		return nil, audioerr.Schema("audio.sampleRateHz", "must be positive")
	}
	p.SampleRate = int(sampleRate)
	frameUS, err := requiredInt(audio, "frameDurationUs", "audio.frameDurationUs")
	if err != nil {
		return nil, err
	}
	hopUS, err := requiredInt(audio, "hopDurationUs", "audio.hopDurationUs")
	if err != nil {
		return nil, err
	}
	p.FrameLenSamples, err = b.microsToSamples(frameUS, p.SampleRate, "audio.frameDurationUs")
	if err != nil {
		return nil, err
	}
	p.HopSamples, err = b.microsToSamples(hopUS, p.SampleRate, "audio.hopDurationUs")
	if err != nil {
		return nil, err
	}
	calibration, err := requiredObject(root, "calibration", "calibration")
	if err != nil {
		return nil, err
	}
	target, err := requiredInt(calibration, "targetLufsQ10000", "calibration.targetLufsQ10000")
	if err != nil {
		return nil, err
	}
	p.TargetLoudnessQ16, err = b.rescale(target, 10000, fixed.Q16, fixed.RoundNearest,
		"calibration.targetLufsQ10000", "dB_q10000", "dB_q16")
	if err != nil {
		return nil, err
	}
	sessionGain, err := requiredInt(calibration, "sessionGainQ10000", "calibration.sessionGainQ10000")
	if err != nil {
		return nil, err
	}
	p.SessionGainQ16, err = b.rescale(sessionGain, 10000, fixed.Q16, fixed.RoundNearest,
		"calibration.sessionGainQ10000", "linear_q10000", "linear_q16")
	if err != nil {
		return nil, err
	}
	maxAdj, err := requiredInt(calibration, "maxAdjDbQ10000", "calibration.maxAdjDbQ10000")
	if err != nil {
		return nil, err
	}
	p.MaxAdjustmentDBQ16, err = b.rescale(maxAdj, 10000, fixed.Q16, fixed.RoundNearest,
		"calibration.maxAdjDbQ10000", "dB_q10000", "dB_q16")
	if err != nil {
		return nil, err
	}
	p.CalibrationEnabled, err = requiredBool(calibration, "enabled", "calibration.enabled")
	if err != nil {
		return nil, err
	}
	scenesRaw, err := requiredArray(root, "scenes", "scenes")
	if err != nil {
		return nil, err
	}
	for si, sceneRaw := range scenesRaw {
		sceneBase := fmt.Sprintf("scenes[%d]", si)
		sceneObj, err := rawObject(sceneRaw, sceneBase)
		if err != nil {
			return nil, err
		}
		sceneID, err := requiredString(sceneObj, "sceneId", sceneBase+".sceneId")
		if err != nil {
			return nil, err
		}
		sceneName, err := requiredString(sceneObj, "sceneName", sceneBase+".sceneName")
		if err != nil {
			return nil, err
		}
		p.Scenes = append(p.Scenes, model.Scene{ID: sceneID, Name: sceneName, GroupPath: []string{}})
		clipsRaw, err := requiredArray(sceneObj, "clips", sceneBase+".clips")
		if err != nil {
			return nil, err
		}
		for ci, clipRaw := range clipsRaw {
			clipBase := fmt.Sprintf("%s.clips[%d]", sceneBase, ci)
			clipObj, err := rawObject(clipRaw, clipBase)
			if err != nil {
				return nil, err
			}
			clip, err := loadV2Clip(b, clipObj, clipBase, sceneID, sceneName)
			if err != nil {
				return nil, err
			}
			p.Clips = append(p.Clips, clip)
		}
	}
	p.SortClips()
	if err := finalizeProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV2Clip(b *builder, obj map[string]json.RawMessage, base, sceneID, sceneName string) (model.Clip, error) {
	var clip model.Clip
	clip.SceneID = sceneID
	clip.SceneName = sceneName
	var err error
	clip.ID, err = requiredString(obj, "clipId", base+".clipId")
	if err != nil {
		return clip, err
	}
	rng, err := requiredObject(obj, "range", base+".range")
	if err != nil {
		return clip, err
	}
	startUS, err := requiredInt(rng, "startUs", base+".range.startUs")
	if err != nil {
		return clip, err
	}
	endUS, err := requiredInt(rng, "endUs", base+".range.endUs")
	if err != nil {
		return clip, err
	}
	clip.StartSample, err = b.microsToSamples(startUS, b.project.SampleRate, base+".range.startUs")
	if err != nil {
		return clip, err
	}
	clip.EndSample, err = b.microsToSamples(endUS, b.project.SampleRate, base+".range.endUs")
	if err != nil {
		return clip, err
	}
	if clip.EndSample <= clip.StartSample {
		return clip, audioerr.Schema(base+".range.endUs", "must be greater than startUs")
	}
	gain, err := requiredInt(obj, "gainQ10000", base+".gainQ10000")
	if err != nil {
		return clip, err
	}
	clip.GainQ16, err = b.rescale(gain, 10000, fixed.Q16, fixed.RoundNearest, base+".gainQ10000", "linear_q10000", "linear_q16")
	if err != nil {
		return clip, err
	}
	if clip.GainQ16 <= 0 {
		return clip, audioerr.Schema(base+".gainQ10000", "must be positive")
	}
	weight, err := requiredInt(obj, "loudnessWeightQ10000", base+".loudnessWeightQ10000")
	if err != nil {
		return clip, err
	}
	clip.LoudnessWeightQ16, err = b.rescale(weight, 10000, fixed.Q16, fixed.RoundNearest,
		base+".loudnessWeightQ10000", "weight_q10000", "weight_q16")
	if err != nil {
		return clip, err
	}
	if clip.LoudnessWeightQ16 <= 0 {
		return clip, audioerr.Schema(base+".loudnessWeightQ10000", "must be positive")
	}
	clip.Category, err = requiredString(obj, "kind", base+".kind")
	if err != nil {
		return clip, err
	}
	clip.Label, err = requiredString(obj, "label", base+".label")
	if err != nil {
		return clip, err
	}
	pcm, err := requiredString(obj, "pcmBase64", base+".pcmBase64")
	if err != nil {
		return clip, err
	}
	clip.PCM, err = decodePCM(pcm, base+".pcmBase64")
	if err != nil {
		return clip, err
	}
	clip.GroupPath = []string{}
	b.defaulted(base+".groupPath", nil, []string{}, "schema-v2.defaults")
	clip.CalibrationMark = ""
	b.defaulted(base+".calibrationMark", nil, "", "schema-v2.defaults")
	return clip, nil
}

func loadV3(root map[string]json.RawMessage, versionPath string) (*model.Project, error) {
	b := newBuilder(3)
	p := b.project
	project, err := requiredObject(root, "project", "project")
	if err != nil {
		return nil, err
	}
	p.ProjectID, err = requiredString(project, "id", "project.id")
	if err != nil {
		return nil, err
	}
	session, err := requiredObject(root, "session", "session")
	if err != nil {
		return nil, err
	}
	sampleRate, err := requiredInt(session, "sample_rate", "session.sample_rate")
	if err != nil {
		return nil, err
	}
	if sampleRate <= 0 {
		return nil, audioerr.Schema("session.sample_rate", "must be positive")
	}
	p.SampleRate = int(sampleRate)
	p.FrameLenSamples, err = requiredInt(session, "frame_samples", "session.frame_samples")
	if err != nil {
		return nil, err
	}
	p.HopSamples, err = requiredInt(session, "hop_samples", "session.hop_samples")
	if err != nil {
		return nil, err
	}
	target, err := requiredInt(session, "target_db_u", "session.target_db_u")
	if err != nil {
		return nil, err
	}
	p.TargetLoudnessQ16, err = b.rescale(target, 1_000_000, fixed.Q16, fixed.RoundNearest,
		"session.target_db_u", "dB_u", "dB_q16")
	if err != nil {
		return nil, err
	}
	sessionGain, err := requiredInt(session, "session_gain_u", "session.session_gain_u")
	if err != nil {
		return nil, err
	}
	p.SessionGainQ16, err = b.rescale(sessionGain, 1_000_000, fixed.Q16, fixed.RoundNearest,
		"session.session_gain_u", "linear_u", "linear_q16")
	if err != nil {
		return nil, err
	}
	maxAdj, err := requiredInt(session, "max_adj_db_u", "session.max_adj_db_u")
	if err != nil {
		return nil, err
	}
	p.MaxAdjustmentDBQ16, err = b.rescale(maxAdj, 1_000_000, fixed.Q16, fixed.RoundNearest,
		"session.max_adj_db_u", "dB_u", "dB_q16")
	if err != nil {
		return nil, err
	}
	calibration, err := requiredObject(root, "calibration", "calibration")
	if err != nil {
		return nil, err
	}
	p.CalibrationEnabled, err = requiredBool(calibration, "enabled", "calibration.enabled")
	if err != nil {
		return nil, err
	}
	scenesRaw, err := requiredArray(root, "scenes", "scenes")
	if err != nil {
		return nil, err
	}
	for si, sceneRaw := range scenesRaw {
		sceneBase := fmt.Sprintf("scenes[%d]", si)
		sceneObj, err := rawObject(sceneRaw, sceneBase)
		if err != nil {
			return nil, err
		}
		sceneID, err := requiredString(sceneObj, "scene_id", sceneBase+".scene_id")
		if err != nil {
			return nil, err
		}
		sceneName, err := requiredString(sceneObj, "scene_name", sceneBase+".scene_name")
		if err != nil {
			return nil, err
		}
		p.Scenes = append(p.Scenes, model.Scene{ID: sceneID, Name: sceneName, GroupPath: []string{}})
		if groupsRaw, ok := sceneObj["groups"]; ok {
			groups, err := rawArray(groupsRaw, sceneBase+".groups")
			if err != nil {
				return nil, err
			}
			if err := loadV3Groups(b, groups, sceneBase+".groups", sceneID, sceneName, nil); err != nil {
				return nil, err
			}
		}
		if directRaw, ok := sceneObj["clips"]; ok {
			directClips, err := rawArray(directRaw, sceneBase+".clips")
			if err != nil {
				return nil, err
			}
			if err := loadV3ClipArray(b, directClips, sceneBase+".clips", sceneID, sceneName, []string{}); err != nil {
				return nil, err
			}
		}
	}
	p.SortClips()
	if err := finalizeProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV3Groups(b *builder, groups []json.RawMessage, base, sceneID, sceneName string, parentPath []string) error {
	for gi, raw := range groups {
		groupBase := fmt.Sprintf("%s[%d]", base, gi)
		obj, err := rawObject(raw, groupBase)
		if err != nil {
			return err
		}
		if _, err := requiredString(obj, "group_id", groupBase+".group_id"); err != nil {
			return err
		}
		name, err := requiredString(obj, "group_name", groupBase+".group_name")
		if err != nil {
			return err
		}
		path := append(append([]string(nil), parentPath...), name)
		if clipsRaw, ok := obj["clips"]; ok {
			clips, err := rawArray(clipsRaw, groupBase+".clips")
			if err != nil {
				return err
			}
			if err := loadV3ClipArray(b, clips, groupBase+".clips", sceneID, sceneName, path); err != nil {
				return err
			}
		}
		if subgroupsRaw, ok := obj["subgroups"]; ok {
			subgroups, err := rawArray(subgroupsRaw, groupBase+".subgroups")
			if err != nil {
				return err
			}
			if err := loadV3Groups(b, subgroups, groupBase+".subgroups", sceneID, sceneName, path); err != nil {
				return err
			}
		}
	}
	return nil
}

func loadV3ClipArray(b *builder, clips []json.RawMessage, base, sceneID, sceneName string, groupPath []string) error {
	for ci, raw := range clips {
		clipBase := fmt.Sprintf("%s[%d]", base, ci)
		obj, err := rawObject(raw, clipBase)
		if err != nil {
			return err
		}
		clip, err := loadV3Clip(b, obj, clipBase, sceneID, sceneName, groupPath)
		if err != nil {
			return err
		}
		b.project.Clips = append(b.project.Clips, clip)
	}
	return nil
}

func loadV3Clip(b *builder, obj map[string]json.RawMessage, base, sceneID, sceneName string, groupPath []string) (model.Clip, error) {
	var clip model.Clip
	clip.SceneID = sceneID
	clip.SceneName = sceneName
	clip.GroupPath = append([]string(nil), groupPath...)
	var err error
	clip.ID, err = requiredString(obj, "clip_id", base+".clip_id")
	if err != nil {
		return clip, err
	}
	clip.Label, err = requiredString(obj, "label", base+".label")
	if err != nil {
		return clip, err
	}
	clip.Category, err = requiredString(obj, "category", base+".category")
	if err != nil {
		return clip, err
	}
	clip.StartSample, err = requiredInt(obj, "start_sample", base+".start_sample")
	if err != nil {
		return clip, err
	}
	clip.EndSample, err = requiredInt(obj, "end_sample", base+".end_sample")
	if err != nil {
		return clip, err
	}
	if clip.EndSample <= clip.StartSample {
		return clip, audioerr.Schema(base+".end_sample", "must be greater than start_sample")
	}
	gain, err := requiredInt(obj, "gain_u", base+".gain_u")
	if err != nil {
		return clip, err
	}
	clip.GainQ16, err = b.rescale(gain, 1_000_000, fixed.Q16, fixed.RoundNearest,
		base+".gain_u", "linear_u", "linear_q16")
	if err != nil {
		return clip, err
	}
	if clip.GainQ16 <= 0 {
		return clip, audioerr.Schema(base+".gain_u", "must be positive")
	}
	weight, err := requiredInt(obj, "weight_u", base+".weight_u")
	if err != nil {
		return clip, err
	}
	clip.LoudnessWeightQ16, err = b.rescale(weight, 1_000_000, fixed.Q16, fixed.RoundNearest,
		base+".weight_u", "weight_u", "weight_q16")
	if err != nil {
		return clip, err
	}
	if clip.LoudnessWeightQ16 <= 0 {
		return clip, audioerr.Schema(base+".weight_u", "must be positive")
	}
	mark, ok, err := optionalString(obj, "calibration_mark", base+".calibration_mark")
	if err != nil {
		return clip, err
	}
	if !ok {
		mark = ""
		b.defaulted(base+".calibration_mark", nil, "", "schema-v3.defaults")
	}
	clip.CalibrationMark = mark
	pcm, err := requiredString(obj, "pcm_data", base+".pcm_data")
	if err != nil {
		return clip, err
	}
	clip.PCM, err = decodePCM(pcm, base+".pcm_data")
	if err != nil {
		return clip, err
	}
	return clip, nil
}

func finalizeProject(p *model.Project) error {
	if len(p.Clips) == 0 {
		return audioerr.Schema("clips", "project contains no clips")
	}
	for i := range p.Clips {
		if p.Clips[i].EndSample <= p.Clips[i].StartSample {
			return audioerr.Schema(fmt.Sprintf("clips[%d].end_sample", i), "must be greater than start")
		}
		if p.Clips[i].GainQ16 <= 0 || p.Clips[i].LoudnessWeightQ16 <= 0 {
			return audioerr.Schema(fmt.Sprintf("clips[%d].gain/weight", i), "must be positive")
		}
		p.Clips[i].ContentHash = model.ContentHashForPCM(p.Clips[i].PCM, p.SampleRate)
	}
	if p.FrameLenSamples <= 0 || p.HopSamples <= 0 {
		return audioerr.Schema("session.frame_samples", "frame length and hop must be positive")
	}
	if p.MaxAdjustmentDBQ16 < 0 {
		return audioerr.Schema("session.max_adj_db_q16", "must not be negative")
	}
	return nil
}

// Export converts the internal model into a lower or equal schema version.
// Downgrade losses are returned in LossReport rather than as a fatal error.
func Export(p model.Project, targetVersion int) (ExportResult, error) {
	switch targetVersion {
	case 1:
		return exportV1(p)
	case 2:
		return exportV2(p)
	case 3:
		return exportV3(p)
	default:
		return ExportResult{}, audioerr.Schema("target_version", fmt.Sprintf("unknown schema version %d", targetVersion))
	}
}

func exportV1(p model.Project) (ExportResult, error) {
	losses := LossReport{Code: audioerr.CodeLossy, Items: []LossItem{}}
	traces := make([]fixed.Trace, 0, len(p.Clips)*4)
	if !p.CalibrationEnabled {
		losses.Items = append(losses.Items, LossItem{
			Path: "calibration.enabled", Kind: "calibration_flag",
			Value: p.CalibrationEnabled, Disposition: "drop_with_schema_v1_default_true",
		})
	}
	for i, scene := range p.Scenes {
		if scene.Name != scene.ID {
			losses.Items = append(losses.Items, LossItem{
				Path: fmt.Sprintf("scenes[%d].name", i), Kind: "scene_name",
				Value: scene.Name, Disposition: "retain_scene_id_only",
			})
		}
	}
	project := map[string]any{
		"id":          p.ProjectID,
		"sample_rate": p.SampleRate,
	}
	frameMS, tr, err := fixed.SamplesToMillis(p.FrameLenSamples, p.SampleRate, fixed.RoundNearest, "frame_len_samples")
	if err != nil {
		return ExportResult{}, err
	}
	traces = append(traces, tr)
	hopMS, tr, err := fixed.SamplesToMillis(p.HopSamples, p.SampleRate, fixed.RoundNearest, "hop_samples")
	if err != nil {
		return ExportResult{}, err
	}
	traces = append(traces, tr)
	project["frame_ms"] = frameMS
	project["hop_ms"] = hopMS
	target, tr, err := fixed.Rescale(p.TargetLoudnessQ16, fixed.Q16, 100, fixed.RoundNearest, "target_loudness_q16")
	if err != nil {
		return ExportResult{}, err
	}
	traces = append(traces, tr)
	project["target_lufs_q100"] = target
	sessionGain, tr, err := fixed.Rescale(p.SessionGainQ16, fixed.Q16, 1000, fixed.RoundNearest, "session_gain_q16")
	if err != nil {
		return ExportResult{}, err
	}
	traces = append(traces, tr)
	project["session_gain_q1000"] = sessionGain
	maxAdj, tr, err := fixed.Rescale(p.MaxAdjustmentDBQ16, fixed.Q16, 100, fixed.RoundNearest, "max_adjustment_db_q16")
	if err != nil {
		return ExportResult{}, err
	}
	traces = append(traces, tr)
	project["max_adj_db_q100"] = maxAdj
	clips := make([]any, 0, len(p.Clips))
	for i, clip := range p.Clips {
		if len(clip.GroupPath) > 0 {
			losses.Items = append(losses.Items, LossItem{
				Path: fmt.Sprintf("clips[%d].group_path", i), Kind: "nested_scene_group",
				Value: clip.GroupPath, Disposition: "flatten_to_scene_id",
			})
		}
		if clip.CalibrationMark != "" {
			losses.Items = append(losses.Items, LossItem{
				Path: fmt.Sprintf("clips[%d].calibration_mark", i), Kind: "calibration_mark",
				Value: clip.CalibrationMark, Disposition: "drop_with_empty_default",
			})
		}
		startMS, tr, err := fixed.SamplesToMillis(clip.StartSample, p.SampleRate, fixed.RoundNearest,
			fmt.Sprintf("clips[%d].start_sample", i))
		if err != nil {
			return ExportResult{}, err
		}
		traces = append(traces, tr)
		endMS, tr, err := fixed.SamplesToMillis(clip.EndSample, p.SampleRate, fixed.RoundNearest,
			fmt.Sprintf("clips[%d].end_sample", i))
		if err != nil {
			return ExportResult{}, err
		}
		traces = append(traces, tr)
		gain, tr, err := fixed.Rescale(clip.GainQ16, fixed.Q16, 1000, fixed.RoundNearest,
			fmt.Sprintf("clips[%d].gain_q16", i))
		if err != nil {
			return ExportResult{}, err
		}
		traces = append(traces, tr)
		weight, tr, err := fixed.Rescale(clip.LoudnessWeightQ16, fixed.Q16, 1000, fixed.RoundNearest,
			fmt.Sprintf("clips[%d].weight_q16", i))
		if err != nil {
			return ExportResult{}, err
		}
		traces = append(traces, tr)
		clips = append(clips, map[string]any{
			"id":              clip.ID,
			"scene":           clip.SceneID,
			"label":           clip.Label,
			"start_ms":        startMS,
			"end_ms":          endMS,
			"gain_milli":      gain,
			"weight_permille": weight,
			"voice_type":      clip.Category,
			"pcm_b64":         encodePCM(clip.PCM),
		})
	}
	doc := map[string]any{
		"schema_version": 1,
		"project":        project,
		"clips":          clips,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return ExportResult{}, err
	}
	return ExportResult{Data: append(data, '\n'), Losses: losses, Traces: traces}, nil
}

func exportV2(p model.Project) (ExportResult, error) {
	losses := LossReport{Code: audioerr.CodeLossy, Items: []LossItem{}}
	traces := make([]fixed.Trace, 0, len(p.Clips)*4+8)
	frameUS, tr, err := fixed.SamplesToMicros(p.FrameLenSamples, p.SampleRate, fixed.RoundNearest, "frame_len_samples")
	if err != nil {
		return ExportResult{}, err
	}
	traces = append(traces, tr)
	hopUS, tr, err := fixed.SamplesToMicros(p.HopSamples, p.SampleRate, fixed.RoundNearest, "hop_samples")
	if err != nil {
		return ExportResult{}, err
	}
	traces = append(traces, tr)
	target, tr, err := fixed.Rescale(p.TargetLoudnessQ16, fixed.Q16, 10000, fixed.RoundNearest, "target_loudness_q16")
	if err != nil {
		return ExportResult{}, err
	}
	traces = append(traces, tr)
	sessionGain, tr, err := fixed.Rescale(p.SessionGainQ16, fixed.Q16, 10000, fixed.RoundNearest, "session_gain_q16")
	if err != nil {
		return ExportResult{}, err
	}
	traces = append(traces, tr)
	maxAdj, tr, err := fixed.Rescale(p.MaxAdjustmentDBQ16, fixed.Q16, 10000, fixed.RoundNearest, "max_adjustment_db_q16")
	if err != nil {
		return ExportResult{}, err
	}
	traces = append(traces, tr)
	byScene := make(map[string][]model.Clip)
	for _, clip := range p.Clips {
		byScene[clip.SceneID] = append(byScene[clip.SceneID], clip)
	}
	scenes := make([]any, 0, len(p.Scenes))
	sceneSeen := make(map[string]bool)
	for _, scene := range p.Scenes {
		sceneSeen[scene.ID] = true
		clips := make([]any, 0, len(byScene[scene.ID]))
		for _, clip := range byScene[scene.ID] {
			clipJSON, clipTraces, err := exportV2Clip(p, clip)
			if err != nil {
				return ExportResult{}, err
			}
			traces = append(traces, clipTraces...)
			if len(clip.GroupPath) > 0 {
				losses.Items = append(losses.Items, LossItem{
					Path: "clip." + clip.ID + ".group_path", Kind: "nested_scene_group",
					Value: clip.GroupPath, Disposition: "flatten_to_scene",
				})
			}
			if clip.CalibrationMark != "" {
				losses.Items = append(losses.Items, LossItem{
					Path: "clip." + clip.ID + ".calibration_mark", Kind: "calibration_mark",
					Value: clip.CalibrationMark, Disposition: "drop_with_empty_default",
				})
			}
			clips = append(clips, clipJSON)
		}
		scenes = append(scenes, map[string]any{
			"sceneId":   scene.ID,
			"sceneName": scene.Name,
			"clips":     clips,
		})
	}
	for _, clip := range p.Clips {
		if sceneSeen[clip.SceneID] {
			continue
		}
		clipJSON, clipTraces, err := exportV2Clip(p, clip)
		if err != nil {
			return ExportResult{}, err
		}
		traces = append(traces, clipTraces...)
		scenes = append(scenes, map[string]any{
			"sceneId":   clip.SceneID,
			"sceneName": clip.SceneName,
			"clips":     []any{clipJSON},
		})
	}
	doc := map[string]any{
		"schemaVersion": 2,
		"projId":        p.ProjectID,
		"audio": map[string]any{
			"sampleRateHz":    p.SampleRate,
			"frameDurationUs": frameUS,
			"hopDurationUs":   hopUS,
		},
		"calibration": map[string]any{
			"targetLufsQ10000":  target,
			"sessionGainQ10000": sessionGain,
			"maxAdjDbQ10000":    maxAdj,
			"enabled":           p.CalibrationEnabled,
		},
		"scenes": scenes,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return ExportResult{}, err
	}
	return ExportResult{Data: append(data, '\n'), Losses: losses, Traces: traces}, nil
}

func exportV2Clip(p model.Project, clip model.Clip) (any, []fixed.Trace, error) {
	traces := make([]fixed.Trace, 0, 4)
	startUS, tr, err := fixed.SamplesToMicros(clip.StartSample, p.SampleRate, fixed.RoundNearest, "clip."+clip.ID+".start_sample")
	if err != nil {
		return nil, nil, err
	}
	traces = append(traces, tr)
	endUS, tr, err := fixed.SamplesToMicros(clip.EndSample, p.SampleRate, fixed.RoundNearest, "clip."+clip.ID+".end_sample")
	if err != nil {
		return nil, nil, err
	}
	traces = append(traces, tr)
	gain, tr, err := fixed.Rescale(clip.GainQ16, fixed.Q16, 10000, fixed.RoundNearest, "clip."+clip.ID+".gain_q16")
	if err != nil {
		return nil, nil, err
	}
	traces = append(traces, tr)
	weight, tr, err := fixed.Rescale(clip.LoudnessWeightQ16, fixed.Q16, 10000, fixed.RoundNearest, "clip."+clip.ID+".weight_q16")
	if err != nil {
		return nil, nil, err
	}
	traces = append(traces, tr)
	return map[string]any{
		"clipId":               clip.ID,
		"label":                clip.Label,
		"kind":                 clip.Category,
		"range":                map[string]any{"startUs": startUS, "endUs": endUS},
		"gainQ10000":           gain,
		"loudnessWeightQ10000": weight,
		"pcmBase64":            encodePCM(clip.PCM),
	}, traces, nil
}

type v3Group struct {
	ID       string
	Name     string
	Clips    []model.Clip
	Children []*v3Group
}

func exportV3(p model.Project) (ExportResult, error) {
	losses := LossReport{Code: audioerr.CodeLossy, Items: []LossItem{}}
	traces := make([]fixed.Trace, 0, len(p.Clips)*4)
	target, tr, err := fixed.Rescale(p.TargetLoudnessQ16, fixed.Q16, 1_000_000, fixed.RoundNearest, "target_loudness_q16")
	if err != nil {
		return ExportResult{}, err
	}
	traces = append(traces, tr)
	sessionGain, tr, err := fixed.Rescale(p.SessionGainQ16, fixed.Q16, 1_000_000, fixed.RoundNearest, "session_gain_q16")
	if err != nil {
		return ExportResult{}, err
	}
	traces = append(traces, tr)
	maxAdj, tr, err := fixed.Rescale(p.MaxAdjustmentDBQ16, fixed.Q16, 1_000_000, fixed.RoundNearest, "max_adjustment_db_q16")
	if err != nil {
		return ExportResult{}, err
	}
	traces = append(traces, tr)
	byScene := make(map[string][]model.Clip)
	for _, clip := range p.Clips {
		byScene[clip.SceneID] = append(byScene[clip.SceneID], clip)
	}
	scenes := make([]any, 0, len(p.Scenes))
	for _, scene := range p.Scenes {
		rootGroups := make(map[string]*v3Group)
		for _, clip := range byScene[scene.ID] {
			if len(clip.GroupPath) == 0 {
				g := ensureGroup(rootGroups, "", "ungrouped")
				g.Clips = append(g.Clips, clip)
				continue
			}
			g := ensureGroup(rootGroups, clip.GroupPath[0], clip.GroupPath[0])
			for _, name := range clip.GroupPath[1:] {
				g = ensureChild(g, name)
			}
			g.Clips = append(g.Clips, clip)
		}
		groups := make([]any, 0, len(rootGroups))
		groupNames := make([]string, 0, len(rootGroups))
		for name := range rootGroups {
			groupNames = append(groupNames, name)
		}
		sort.Strings(groupNames)
		for _, name := range groupNames {
			groupJSON, groupTraces, err := exportV3Group(p, rootGroups[name])
			if err != nil {
				return ExportResult{}, err
			}
			traces = append(traces, groupTraces...)
			groups = append(groups, groupJSON)
		}
		scenes = append(scenes, map[string]any{
			"scene_id":   scene.ID,
			"scene_name": scene.Name,
			"groups":     groups,
		})
	}
	doc := map[string]any{
		"revision": 3,
		"project":  map[string]any{"id": p.ProjectID},
		"session": map[string]any{
			"sample_rate":    p.SampleRate,
			"frame_samples":  p.FrameLenSamples,
			"hop_samples":    p.HopSamples,
			"target_db_u":    target,
			"session_gain_u": sessionGain,
			"max_adj_db_u":   maxAdj,
		},
		"calibration": map[string]any{"enabled": p.CalibrationEnabled},
		"scenes":      scenes,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return ExportResult{}, err
	}
	return ExportResult{Data: append(data, '\n'), Losses: losses, Traces: traces}, nil
}

func ensureGroup(groups map[string]*v3Group, id, name string) *v3Group {
	if id == "" {
		id = "ungrouped"
	}
	if g, ok := groups[id]; ok {
		return g
	}
	g := &v3Group{ID: id, Name: name}
	groups[id] = g
	return g
}

func ensureChild(parent *v3Group, name string) *v3Group {
	id := parent.ID + "/" + name
	for _, child := range parent.Children {
		if child.ID == id {
			return child
		}
	}
	child := &v3Group{ID: id, Name: name}
	parent.Children = append(parent.Children, child)
	return child
}

func exportV3Group(p model.Project, group *v3Group) (any, []fixed.Trace, error) {
	traces := make([]fixed.Trace, 0, len(group.Clips)*4)
	clips := make([]any, 0, len(group.Clips))
	for _, clip := range group.Clips {
		gain, tr, err := fixed.Rescale(clip.GainQ16, fixed.Q16, 1_000_000, fixed.RoundNearest, "clip."+clip.ID+".gain_q16")
		if err != nil {
			return nil, nil, err
		}
		traces = append(traces, tr)
		weight, tr, err := fixed.Rescale(clip.LoudnessWeightQ16, fixed.Q16, 1_000_000, fixed.RoundNearest, "clip."+clip.ID+".weight_q16")
		if err != nil {
			return nil, nil, err
		}
		traces = append(traces, tr)
		clips = append(clips, map[string]any{
			"clip_id":          clip.ID,
			"label":            clip.Label,
			"category":         clip.Category,
			"start_sample":     clip.StartSample,
			"end_sample":       clip.EndSample,
			"gain_u":           gain,
			"weight_u":         weight,
			"calibration_mark": clip.CalibrationMark,
			"pcm_data":         encodePCM(clip.PCM),
		})
	}
	sort.Slice(group.Children, func(i, j int) bool { return group.Children[i].ID < group.Children[j].ID })
	children := make([]any, 0, len(group.Children))
	for _, child := range group.Children {
		childJSON, childTraces, err := exportV3Group(p, child)
		if err != nil {
			return nil, nil, err
		}
		traces = append(traces, childTraces...)
		children = append(children, childJSON)
	}
	return map[string]any{
		"group_id":   group.ID,
		"group_name": group.Name,
		"clips":      clips,
		"subgroups":  children,
	}, traces, nil
}

func decodeObject(data []byte, path string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, audioerr.Schema(path, "invalid JSON object: "+err.Error())
	}
	if obj == nil {
		return nil, audioerr.Schema(path, "expected object")
	}
	return obj, nil
}

func rawObject(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	return decodeObject(raw, path)
}

func rawArray(raw json.RawMessage, path string) ([]json.RawMessage, error) {
	var array []json.RawMessage
	if err := json.Unmarshal(raw, &array); err != nil {
		return nil, audioerr.Schema(path, "expected array: "+err.Error())
	}
	return array, nil
}

func requiredObject(obj map[string]json.RawMessage, key, path string) (map[string]json.RawMessage, error) {
	raw, ok := obj[key]
	if !ok {
		return nil, audioerr.Schema(path, "required field is missing")
	}
	return rawObject(raw, path)
}

func requiredArray(obj map[string]json.RawMessage, key, path string) ([]json.RawMessage, error) {
	raw, ok := obj[key]
	if !ok {
		return nil, audioerr.Schema(path, "required field is missing")
	}
	return rawArray(raw, path)
}

func requiredString(obj map[string]json.RawMessage, key, path string) (string, error) {
	raw, ok := obj[key]
	if !ok {
		return "", audioerr.Schema(path, "required field is missing")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", audioerr.Schema(path, "expected string")
	}
	if strings.TrimSpace(value) == "" {
		return "", audioerr.Schema(path, "must not be empty")
	}
	return value, nil
}

func optionalString(obj map[string]json.RawMessage, key, path string) (string, bool, error) {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false, audioerr.Schema(path, "expected string")
	}
	return value, true, nil
}

func requiredInt(obj map[string]json.RawMessage, key, path string) (int64, error) {
	raw, ok := obj[key]
	if !ok {
		return 0, audioerr.Schema(path, "required field is missing")
	}
	return decodeInt(raw, path)
}

func decodeInt(raw json.RawMessage, path string) (int64, error) {
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, audioerr.Schema(path, "expected integer")
	}
	return value, nil
}

func requiredBool(obj map[string]json.RawMessage, key, path string) (bool, error) {
	raw, ok := obj[key]
	if !ok {
		return false, audioerr.Schema(path, "required field is missing")
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, audioerr.Schema(path, "expected boolean")
	}
	return value, nil
}

func decodePCM(value, path string) ([]int16, error) {
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, audioerr.Schema(path, "invalid base64 PCM: "+err.Error())
	}
	if len(data)%2 != 0 {
		return nil, audioerr.Schema(path, "PCM byte count must be even")
	}
	samples := make([]int16, len(data)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	return samples, nil
}

func encodePCM(samples []int16) string {
	data := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(sample))
	}
	return base64.StdEncoding.EncodeToString(data)
}
